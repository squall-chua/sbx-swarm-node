// Package apiserver builds the node's one-port gRPC + REST + static server.
package apiserver

import (
	"context"
	"sync/atomic"

	"github.com/squall-chua/sbx-swarm-node/internal/audit"
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
	removeTemplate            func(ctx context.Context, ref string) error           // optional; nil until wired by node.go
	audit                     *audit.Log                                            // optional; nil until wired by node.go
	draining                  atomic.Bool
}

// NewNodeService returns a NodeService reporting the given identity.
func NewNodeService(nodeID, nodeName, version string) *NodeService {
	return &NodeService{nodeID: nodeID, nodeName: nodeName, version: version}
}

// SetCordoner wires the cluster's cordon controller. Called from node.New after
// the cluster is built; nil-safe so existing NodeService tests pass unchanged.
func (s *NodeService) SetCordoner(c Cordoner) { s.cordoner = c }

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
	if s.cordoner != nil {
		s.cordoner.SetCordoned(true)
	}
	return &sbxv1.NodeInfo{
		NodeId:   s.nodeID,
		NodeName: s.nodeName,
		Version:  s.version,
		Cordoned: true,
		Draining: s.draining.Load(),
	}, nil
}

// Uncordon removes the cordon so the node can accept new sandboxes again.
func (s *NodeService) Uncordon(_ context.Context, _ *sbxv1.CordonRequest) (*sbxv1.NodeInfo, error) {
	if s.cordoner != nil {
		s.cordoner.SetCordoned(false)
	}
	s.draining.Store(false)
	return &sbxv1.NodeInfo{
		NodeId:   s.nodeID,
		NodeName: s.nodeName,
		Version:  s.version,
		Cordoned: false,
		Draining: false,
	}, nil
}

// Drain cordons the node and sets a draining flag so the M5 scheduler can
// gracefully migrate sandboxes away. The draining flag is visible via
// routing.Table.IsCordoned (both cordon and drain block new placements).
func (s *NodeService) Drain(_ context.Context, _ *sbxv1.DrainRequest) (*sbxv1.NodeInfo, error) {
	s.draining.Store(true)
	if s.cordoner != nil {
		s.cordoner.SetCordoned(true)
	}
	return &sbxv1.NodeInfo{
		NodeId:   s.nodeID,
		NodeName: s.nodeName,
		Version:  s.version,
		Cordoned: true,
		Draining: true,
	}, nil
}

// SetNodeLister wires the swarm-node snapshot source (node.go). nil-safe:
// without it, ListNodes reports self identity only.
func (s *NodeService) SetNodeLister(fn func() []NodeRow) { s.nodeLister = fn }

// SetTemplateLister wires the local backend's template source (node.go).
func (s *NodeService) SetTemplateLister(fn func(context.Context) ([]sandbox.TemplateInfo, error)) {
	s.templateLister = fn
}

// SetTemplateRemover wires template deletion (node.go). Optional: without it,
// RemoveTemplate answers Unavailable rather than pretending to delete.
func (s *NodeService) SetTemplateRemover(fn func(ctx context.Context, ref string) error) {
	s.removeTemplate = fn
}

// SetAudit wires the audit log (node.go). Optional: without it, RemoveTemplate
// still deletes but records nothing.
func (s *NodeService) SetAudit(a *audit.Log) { s.audit = a }

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

// RemoveTemplate deletes one template image from this node's image store and
// returns what is left. Cross-node calls arrive here already forwarded by
// node_id (ADR-0018), so this only ever removes locally.
//
// A node_id naming some other node is refused here rather than trusted to the
// interceptor: the interceptor forwards to the owning peer only when it has an
// address for that node, and otherwise falls through to this local handler
// (unknown or departed peer, or a standalone node with no forwarder at all).
// Without this guard that fallback would delete the wrong node's image and
// report success, with no node identity in the reply to reveal the mistake.
func (s *NodeService) RemoveTemplate(ctx context.Context, r *sbxv1.RemoveTemplateRequest) (*sbxv1.ListTemplatesResponse, error) {
	if id := r.GetNodeId(); id != "" && id != s.nodeID {
		return nil, status.Error(codes.NotFound, "unknown node")
	}
	if r.GetRef() == "" {
		return nil, status.Error(codes.InvalidArgument, "ref is required")
	}
	if s.removeTemplate == nil {
		return nil, status.Error(codes.Unavailable, "no sandbox backend on this node")
	}
	err := s.removeTemplate(ctx, r.GetRef())
	if s.audit != nil {
		_ = s.audit.Record(audit.Entry{
			Actor:   actor(ctx),
			Action:  "template.remove",
			Target:  r.GetRef(),
			Outcome: outcomeOf(err),
		})
	}
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return s.ListTemplates(ctx, &sbxv1.ListTemplatesRequest{})
}

// Draining reports this node's drain flag (self-only; not gossiped).
func (s *NodeService) Draining() bool { return s.draining.Load() }

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
