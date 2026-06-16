// Package apiserver builds the node's one-port gRPC + REST + static server.
package apiserver

import (
	"context"
	"sync/atomic"

	sbxv1 "github.com/squall-chua/sbx-swarm-node/internal/gen/sbxswarm/v1"
)

// Cordoner is implemented by membership.Cluster. It is a minimal interface so
// NodeService does not import the membership package (avoiding a cycle).
type Cordoner interface {
	SetCordoned(bool)
}

// NodeService implements sbxv1.NodeServiceServer.
type NodeService struct {
	sbxv1.UnimplementedNodeServiceServer
	nodeID, nodeName, version string
	cordoner                  Cordoner   // optional; nil when not in cluster mode
	draining                  atomic.Bool
}

// NewNodeService returns a NodeService reporting the given identity.
func NewNodeService(nodeID, nodeName, version string) *NodeService {
	return &NodeService{nodeID: nodeID, nodeName: nodeName, version: version}
}

// SetCordoner wires the cluster's cordon controller. Called from node.New after
// the cluster is built; nil-safe so existing NodeService tests pass unchanged.
func (s *NodeService) SetCordoner(c Cordoner) { s.cordoner = c }

// GetNodeInfo returns static node identity.
func (s *NodeService) GetNodeInfo(_ context.Context, _ *sbxv1.GetNodeInfoRequest) (*sbxv1.NodeInfo, error) {
	return &sbxv1.NodeInfo{
		NodeId:   s.nodeID,
		NodeName: s.nodeName,
		Version:  s.version,
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
