package apiserver

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/squall-chua/sbx-swarm-node/internal/audit"
	sbxv1 "github.com/squall-chua/sbx-swarm-node/internal/gen/sbxswarm/v1"
	"github.com/squall-chua/sbx-swarm-node/internal/sandbox"
	"github.com/squall-chua/sbx-swarm-node/internal/store"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func newTestAudit(t *testing.T) *audit.Log {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "n.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	return audit.New(st, func() int64 { return 1 })
}

func TestNodeService_RemoveTemplate(t *testing.T) {
	f := sandbox.NewFake()
	require.NoError(t, f.SaveTemplate(context.Background(), "sb-1", "myimage:v1"))

	s := NewNodeService("n1", "node-1", "test")
	s.SetTemplateLister(f.ListTemplateInfo)
	s.SetTemplateRemover(f.RemoveTemplate)

	resp, err := s.RemoveTemplate(context.Background(), &sbxv1.RemoveTemplateRequest{Ref: "myimage:v1"})
	require.NoError(t, err)
	for _, tm := range resp.Templates {
		require.NotEqual(t, "myimage:v1", tm.Repository+":"+tm.Tag)
	}
}

func TestNodeService_RemoveTemplateWritesAudit(t *testing.T) {
	f := sandbox.NewFake()
	require.NoError(t, f.SaveTemplate(context.Background(), "sb-1", "myimage:v1"))
	a := newTestAudit(t)

	s := NewNodeService("n1", "node-1", "test")
	s.SetTemplateLister(f.ListTemplateInfo)
	s.SetTemplateRemover(f.RemoveTemplate)
	s.SetAudit(a)

	_, err := s.RemoveTemplate(context.Background(), &sbxv1.RemoveTemplateRequest{Ref: "myimage:v1"})
	require.NoError(t, err)

	entries, err := a.List()
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "template.remove", entries[0].Action)
	require.Equal(t, "myimage:v1", entries[0].Target)
	require.Equal(t, "ok", entries[0].Outcome)
}

func TestNodeService_RemoveTemplateWritesAuditOnFailure(t *testing.T) {
	a := newTestAudit(t)

	s := NewNodeService("n1", "node-1", "test")
	s.SetTemplateRemover(func(context.Context, string) error { return errors.New("boom") })
	s.SetAudit(a)

	_, err := s.RemoveTemplate(context.Background(), &sbxv1.RemoveTemplateRequest{Ref: "myimage:v1"})
	require.Error(t, err)

	entries, err := a.List()
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "template.remove", entries[0].Action)
	require.Equal(t, "myimage:v1", entries[0].Target)
	require.Equal(t, "error", entries[0].Outcome)
}

func TestNodeService_RemoveTemplateNeedsARef(t *testing.T) {
	s := NewNodeService("n1", "node-1", "test")
	_, err := s.RemoveTemplate(context.Background(), &sbxv1.RemoveTemplateRequest{})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestNodeService_RemoveTemplateWithoutABackend(t *testing.T) {
	s := NewNodeService("n1", "node-1", "test") // no remover wired
	_, err := s.RemoveTemplate(context.Background(), &sbxv1.RemoveTemplateRequest{Ref: "myimage:v1"})
	require.Equal(t, codes.Unavailable, status.Code(err))
}

func TestNodeService_RemoveTemplateRefusesAnotherNode(t *testing.T) {
	f := sandbox.NewFake()
	require.NoError(t, f.SaveTemplate(context.Background(), "sb-1", "myimage:v1"))

	called := false
	s := NewNodeService("n1", "node-1", "test")
	s.SetTemplateLister(f.ListTemplateInfo)
	s.SetTemplateRemover(func(ctx context.Context, ref string) error {
		called = true
		return f.RemoveTemplate(ctx, ref)
	})

	_, err := s.RemoveTemplate(context.Background(), &sbxv1.RemoveTemplateRequest{NodeId: "n2", Ref: "myimage:v1"})
	require.Equal(t, codes.NotFound, status.Code(err))
	require.False(t, called, "the backend remover must not run for another node's id")
}

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

func TestNodeService_ListTemplates(t *testing.T) {
	svc := NewNodeService("n1", "node-one", "test")

	// No lister: empty response, not error.
	resp, err := svc.ListTemplates(context.Background(), &sbxv1.ListTemplatesRequest{})
	require.NoError(t, err)
	require.Empty(t, resp.Templates)

	svc.SetTemplateLister(func(context.Context) ([]sandbox.TemplateInfo, error) {
		return []sandbox.TemplateInfo{{Repository: "r", Tag: "t", ID: "i"}}, nil
	})
	resp, err = svc.ListTemplates(context.Background(), &sbxv1.ListTemplatesRequest{})
	require.NoError(t, err)
	require.Len(t, resp.Templates, 1)
	require.Equal(t, "r", resp.Templates[0].Repository)
	require.Equal(t, "i", resp.Templates[0].Id)
}

func TestGetNodeInfo_ReturnsCallerRole(t *testing.T) {
	svc := NewNodeService("n1", "node-one", "v1.2.3")

	adminCtx := context.WithValue(context.Background(), principalCtxKey{}, principal{userRole: "admin"})
	info, err := svc.GetNodeInfo(adminCtx, &sbxv1.GetNodeInfoRequest{})
	require.NoError(t, err)
	require.Equal(t, "admin", info.Role)

	roCtx := context.WithValue(context.Background(), principalCtxKey{}, principal{userRole: "read-only"})
	info, err = svc.GetNodeInfo(roCtx, &sbxv1.GetNodeInfoRequest{})
	require.NoError(t, err)
	require.Equal(t, "read-only", info.Role)
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

func TestNodeService_ListNodes_GitWorkspaces(t *testing.T) {
	svc := NewNodeService("n1", "node-one", "test")
	svc.SetNodeLister(func() []NodeRow {
		return []NodeRow{
			{NodeID: "n1", Workspaces: []string{"repo", "plain"}, GitWorkspaces: []string{"repo"}},
		}
	})
	resp, err := svc.ListNodes(context.Background(), &sbxv1.ListNodesRequest{})
	require.NoError(t, err)
	require.Len(t, resp.Nodes, 1)
	require.Equal(t, []string{"repo", "plain"}, resp.Nodes[0].Workspaces)
	require.Equal(t, []string{"repo"}, resp.Nodes[0].GitWorkspaces)
}

func TestNodeService_ListNodes_Kits(t *testing.T) {
	svc := NewNodeService("n1", "node-one", "test")
	svc.SetNodeLister(func() []NodeRow {
		return []NodeRow{
			{NodeID: "n1", Kits: []string{"extras", "tools"}},
		}
	})
	resp, err := svc.ListNodes(context.Background(), &sbxv1.ListNodesRequest{})
	require.NoError(t, err)
	require.Len(t, resp.Nodes, 1)
	require.Equal(t, []string{"extras", "tools"}, resp.Nodes[0].Kits)
}
