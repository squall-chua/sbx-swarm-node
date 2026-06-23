package apiserver

import (
	"context"
	"errors"
	"testing"

	sbxv1 "github.com/squall-chua/sbx-swarm-node/internal/gen/sbxswarm/v1"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestNodeService_GetNodeInfo(t *testing.T) {
	svc := NewNodeService("node-abc", "alpha", "v9")
	out, err := svc.GetNodeInfo(context.Background(), &sbxv1.GetNodeInfoRequest{})
	require.NoError(t, err)
	require.Equal(t, "node-abc", out.NodeId)
	require.Equal(t, "alpha", out.NodeName)
	require.Equal(t, "v9", out.Version)
}

type fakeRevoker struct {
	revoked []string
	err     error
}

func (f *fakeRevoker) Revoke(id string) error {
	if f.err != nil {
		return f.err
	}
	f.revoked = append(f.revoked, id)
	return nil
}
func (f *fakeRevoker) RevokedList() []string { return f.revoked }

func TestNodeService_RevokeNode(t *testing.T) {
	s := NewNodeService("nA", "name", "v")

	// Standalone (no revoker) -> FailedPrecondition.
	_, err := s.RevokeNode(context.Background(), &sbxv1.RevokeNodeRequest{NodeId: "nB"})
	require.Equal(t, codes.FailedPrecondition, status.Code(err))

	fr := &fakeRevoker{}
	s.SetRevoker(fr)
	reply, err := s.RevokeNode(context.Background(), &sbxv1.RevokeNodeRequest{NodeId: "nB"})
	require.NoError(t, err)
	require.Equal(t, []string{"nB"}, reply.NodeIds)
	require.Equal(t, []string{"nB"}, fr.revoked)
}

func TestNodeService_RevokeNode_InvalidArg(t *testing.T) {
	s := NewNodeService("nA", "name", "v")
	s.SetRevoker(&fakeRevoker{err: errors.New("revoke: cannot revoke self")})
	_, err := s.RevokeNode(context.Background(), &sbxv1.RevokeNodeRequest{NodeId: "nA"})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestNodeService_ListRevoked(t *testing.T) {
	s := NewNodeService("nA", "name", "v")

	reply, err := s.ListRevoked(context.Background(), &sbxv1.ListRevokedRequest{})
	require.NoError(t, err)
	require.Empty(t, reply.NodeIds, "standalone returns an empty list, not an error")

	s.SetRevoker(&fakeRevoker{revoked: []string{"nB", "nC"}})
	reply, err = s.ListRevoked(context.Background(), &sbxv1.ListRevokedRequest{})
	require.NoError(t, err)
	require.Equal(t, []string{"nB", "nC"}, reply.NodeIds)
}

func TestNodeService_ListNodes(t *testing.T) {
	svc := NewNodeService("n1", "node-one", "test")

	// No lister: self identity only.
	resp, err := svc.ListNodes(context.Background(), &sbxv1.ListNodesRequest{})
	require.NoError(t, err)
	require.Len(t, resp.Nodes, 1)
	require.Equal(t, "n1", resp.Nodes[0].NodeId)

	// With a lister returning self + one peer.
	svc.SetNodeLister(func() []NodeRow {
		return []NodeRow{
			{NodeID: "n1", NodeName: "node-one", LimitCPU: 2},
			{NodeID: "n2", Cordoned: true, Labels: map[string]string{"zone": "b"}},
		}
	})
	resp, err = svc.ListNodes(context.Background(), &sbxv1.ListNodesRequest{})
	require.NoError(t, err)
	require.Len(t, resp.Nodes, 2)
	require.Equal(t, float64(2), resp.Nodes[0].LimitCpu)
	require.True(t, resp.Nodes[1].Cordoned)
	require.Equal(t, "b", resp.Nodes[1].Labels["zone"])
}
