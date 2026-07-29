// Package apiserver builds the node's one-port gRPC + REST + static server.
package apiserver

import (
	"context"
	"sync/atomic"

	sbxv1 "github.com/squall-chua/sbx-swarm-node/internal/gen/sbxswarm/v1"
	"github.com/squall-chua/sbx-swarm-node/internal/sandbox"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// NodeRow is one node's summary for ListNodes, assembled by the wiring layer
// (node.go) so apiserver need not import membership. Field names/units mirror
// membership.NodeState.
type NodeRow struct {
	NodeID, NodeName                    string
	Cordoned, Draining                  bool
	Labels                              map[string]string
	Capabilities, Workspaces, Templates []string
	GitWorkspaces                       []string
	Kits                                []string
	LimitCPU, LimitMemKB, LimitDiskGB   float64
	AllocCPU, AllocMemKB, AllocDiskGB   float64
	ActualCPU, ActualMem                float64
}

// Cordoner is implemented by membership.Cluster. It is a minimal interface so
// NodeService does not import the membership package (avoiding a cycle).
type Cordoner interface {
	SetCordoned(bool)
}

// Revoker is implemented by membership.Cluster. Minimal interface so NodeService
// does not import membership (avoiding a cycle), mirroring Cordoner.
type Revoker interface {
	Revoke(nodeID string) error
	RevokedList() []string
}

// NodeService implements sbxv1.NodeServiceServer.
type NodeService struct {
	sbxv1.UnimplementedNodeServiceServer
	nodeID, nodeName, version string
	cordoner                  Cordoner                                              // optional; nil when not in cluster mode
	revoker                   Revoker                                               // optional; nil when not in cluster mode
	nodeLister                func() []NodeRow                                      // optional; nil until wired by node.go
	templateLister            func(context.Context) ([]sandbox.TemplateInfo, error) // optional; nil until wired by node.go
	persistFlags              func(cordoned, draining bool)                         // optional; nil until wired by node.go
	drainer                   func(actor string, keepGoing func() bool)             // optional; nil until wired by node.go
	draining                  atomic.Bool
	cordoned                  atomic.Bool
}

// NewNodeService returns a NodeService reporting the given identity.
func NewNodeService(nodeID, nodeName, version string) *NodeService {
	return &NodeService{nodeID: nodeID, nodeName: nodeName, version: version}
}

// SetCordoner wires the cluster's cordon controller. Called from node.New after
// the cluster is built; nil-safe so existing NodeService tests pass unchanged.
func (s *NodeService) SetCordoner(c Cordoner) { s.cordoner = c }

// SetFlagPersister wires the store-backed save of the cordon and drain flags
// (node.go). Optional and nil-safe, so existing tests need no change.
func (s *NodeService) SetFlagPersister(fn func(cordoned, draining bool)) { s.persistFlags = fn }

// SetDraining restores the drain marker at boot (node.go). Display only: the
// cordon is what blocks placement.
func (s *NodeService) SetDraining(v bool) { s.draining.Store(v) }

// SetCordonedFlag restores the cordon at boot (node.go). Local only: it does
// not touch the cluster, so callers must mirror it to the Cordoner themselves
// once one exists.
func (s *NodeService) SetCordonedFlag(v bool) { s.cordoned.Store(v) }

// SetDrainer wires the background sweep run by Drain (node.go). Optional and
// nil-safe, so existing tests need no change.
func (s *NodeService) SetDrainer(fn func(actor string, keepGoing func() bool)) { s.drainer = fn }

// saveFlags persists the current flags if a persister is wired.
func (s *NodeService) saveFlags() {
	if s.persistFlags != nil {
		s.persistFlags(s.cordoned.Load(), s.draining.Load())
	}
}

// SetRevoker wires the cluster's revocation controller. nil-safe; standalone
// leaves it nil so revocation degrades to FailedPrecondition/empty.
func (s *NodeService) SetRevoker(r Revoker) { s.revoker = r }

// RevokeNode places a node id on the swarm-wide denylist (admin; ADR-0013).
func (s *NodeService) RevokeNode(_ context.Context, r *sbxv1.RevokeNodeRequest) (*sbxv1.RevokedList, error) {
	if s.revoker == nil {
		return nil, status.Error(codes.FailedPrecondition, "revocation requires clustering")
	}
	if err := s.revoker.Revoke(r.NodeId); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return &sbxv1.RevokedList{NodeIds: s.revoker.RevokedList()}, nil
}

// ListRevoked returns the node ids on this node's denylist.
func (s *NodeService) ListRevoked(_ context.Context, _ *sbxv1.ListRevokedRequest) (*sbxv1.RevokedList, error) {
	if s.revoker == nil {
		return &sbxv1.RevokedList{}, nil
	}
	return &sbxv1.RevokedList{NodeIds: s.revoker.RevokedList()}, nil
}

// GetNodeInfo returns static node identity.
func (s *NodeService) GetNodeInfo(ctx context.Context, _ *sbxv1.GetNodeInfoRequest) (*sbxv1.NodeInfo, error) {
	return &sbxv1.NodeInfo{
		NodeId:   s.nodeID,
		NodeName: s.nodeName,
		Version:  s.version,
		Role:     principalFromContext(ctx).userRole,
	}, nil
}

// Cordon marks the node as cordoned: the scheduler will not place new sandboxes
// here. Existing sandboxes continue running.
func (s *NodeService) Cordon(_ context.Context, _ *sbxv1.CordonRequest) (*sbxv1.NodeInfo, error) {
	s.cordoned.Store(true)
	if s.cordoner != nil {
		s.cordoner.SetCordoned(true)
	}
	s.saveFlags()
	return &sbxv1.NodeInfo{
		NodeId:   s.nodeID,
		NodeName: s.nodeName,
		Version:  s.version,
		Cordoned: s.cordoned.Load(),
		Draining: s.draining.Load(),
	}, nil
}

// Uncordon removes the cordon so the node can accept new sandboxes again.
func (s *NodeService) Uncordon(_ context.Context, _ *sbxv1.CordonRequest) (*sbxv1.NodeInfo, error) {
	s.cordoned.Store(false)
	if s.cordoner != nil {
		s.cordoner.SetCordoned(false)
	}
	s.draining.Store(false)
	s.saveFlags()
	return &sbxv1.NodeInfo{
		NodeId:   s.nodeID,
		NodeName: s.nodeName,
		Version:  s.version,
		Cordoned: s.cordoned.Load(),
		Draining: s.draining.Load(),
	}, nil
}

// actorOrSystem returns the authenticated role from ctx, or "system" if there
// is none. Used for a bulk operator action started here but finished by a
// background goroutine, whose own context does not survive past this RPC.
func actorOrSystem(ctx context.Context) string {
	if a := principalFromContext(ctx).userRole; a != "" {
		return a
	}
	return "system"
}

// Drain cordons the node, then publishes and stops every sandbox running on it,
// in the background. The node ends up empty and git-backed work is saved. The
// draining marker records why the node is out of service and survives a restart.
// Nothing is migrated: a sandbox id names its owner node, so a sandbox cannot
// move without changing identity.
func (s *NodeService) Drain(ctx context.Context, _ *sbxv1.DrainRequest) (*sbxv1.NodeInfo, error) {
	s.draining.Store(true)
	s.cordoned.Store(true)
	if s.cordoner != nil {
		s.cordoner.SetCordoned(true)
	}
	s.saveFlags()
	if s.drainer != nil {
		go s.drainer(actorOrSystem(ctx), s.draining.Load)
	}
	return &sbxv1.NodeInfo{
		NodeId:   s.nodeID,
		NodeName: s.nodeName,
		Version:  s.version,
		Cordoned: s.cordoned.Load(),
		Draining: s.draining.Load(),
	}, nil
}

// SetNodeLister wires the swarm-node snapshot source (node.go). nil-safe:
// without it, ListNodes reports self identity only.
func (s *NodeService) SetNodeLister(fn func() []NodeRow) { s.nodeLister = fn }

// SetTemplateLister wires the local backend's template source (node.go).
func (s *NodeService) SetTemplateLister(fn func(context.Context) ([]sandbox.TemplateInfo, error)) {
	s.templateLister = fn
}

// ListTemplates returns the local node's templates with metadata.
func (s *NodeService) ListTemplates(ctx context.Context, _ *sbxv1.ListTemplatesRequest) (*sbxv1.ListTemplatesResponse, error) {
	out := &sbxv1.ListTemplatesResponse{}
	if s.templateLister == nil {
		return out, nil
	}
	infos, err := s.templateLister(ctx)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	for _, t := range infos {
		out.Templates = append(out.Templates, &sbxv1.TemplateInfo{
			Repository: t.Repository, Tag: t.Tag, Id: t.ID, Agent: t.Agent, CreatedAt: t.CreatedAt,
		})
	}
	return out, nil
}

// Draining reports this node's drain flag (self-only; not gossiped).
func (s *NodeService) Draining() bool { return s.draining.Load() }

// Cordoned reports this node's cordon. This flag is the single source of truth
// about self: the cluster publishes it to peers but does not own it, so a
// standalone node can be cordoned like any other.
func (s *NodeService) Cordoned() bool { return s.cordoned.Load() }

// ListNodes returns self plus gossiped peers (a node present here is alive by
// construction — dead nodes are removed from routing).
func (s *NodeService) ListNodes(_ context.Context, _ *sbxv1.ListNodesRequest) (*sbxv1.ListNodesResponse, error) {
	out := &sbxv1.ListNodesResponse{}
	if s.nodeLister == nil {
		out.Nodes = append(out.Nodes, &sbxv1.NodeSummary{
			NodeId: s.nodeID, NodeName: s.nodeName, Draining: s.draining.Load(),
		})
		return out, nil
	}
	for _, r := range s.nodeLister() {
		out.Nodes = append(out.Nodes, &sbxv1.NodeSummary{
			NodeId: r.NodeID, NodeName: r.NodeName, Cordoned: r.Cordoned, Draining: r.Draining,
			Labels: r.Labels, Capabilities: r.Capabilities, Workspaces: r.Workspaces, Templates: r.Templates, GitWorkspaces: r.GitWorkspaces,
			Kits:     r.Kits,
			LimitCpu: r.LimitCPU, LimitMemKb: r.LimitMemKB, LimitDiskGb: r.LimitDiskGB,
			AllocCpu: r.AllocCPU, AllocMemKb: r.AllocMemKB, AllocDiskGb: r.AllocDiskGB,
			ActualCpu: r.ActualCPU, ActualMem: r.ActualMem,
		})
	}
	return out, nil
}
