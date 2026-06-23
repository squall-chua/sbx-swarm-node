package apiserver

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/squall-chua/sbx-swarm-node/internal/audit"
	sbxv1 "github.com/squall-chua/sbx-swarm-node/internal/gen/sbxswarm/v1"
	"github.com/squall-chua/sbx-swarm-node/internal/ids"
	"github.com/squall-chua/sbx-swarm-node/internal/sandbox"
	"github.com/squall-chua/sbx-swarm-node/internal/store"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func buildPolicySvcMgr(t *testing.T) (*PolicyService, *audit.Log, *sandbox.Manager) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "n.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	fake := sandbox.NewFake()
	mgr := sandbox.NewManager("node1", fake, st, ids.NewGen("node1"))
	a := audit.New(st, func() int64 { return 1 })
	svc := NewPolicyService(mgr, a)
	return svc, a, mgr
}

func buildPolicySvc(t *testing.T) (*PolicyService, *audit.Log) {
	t.Helper()
	svc, a, _ := buildPolicySvcMgr(t)
	return svc, a
}

func TestPolicyService_SetSecretDoesNotLeakValue(t *testing.T) {
	svc, a := buildPolicySvc(t)
	ctx := context.Background()

	// scope="" = node-global; no sandbox resolution needed.
	_, err := svc.SetSecret(ctx, &sbxv1.SetSecretRequest{Scope: "", Host: "api.x", Env: "TOKEN", Value: "shh"})
	require.NoError(t, err)

	resp, err := svc.ListSecrets(ctx, &sbxv1.ScopeRequest{Scope: ""})
	require.NoError(t, err)
	require.Len(t, resp.Custom, 1)
	require.Equal(t, "api.x", resp.Custom[0].Host)
	require.Equal(t, "TOKEN", resp.Custom[0].Env)
	// SecretMsg has no value field by design; verify env is the env name, not the secret value.
	require.NotEqual(t, "shh", resp.Custom[0].Env)

	// audit record must reference host only — never the value.
	entries, err := a.List()
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "secret.set", entries[0].Action)
	require.Equal(t, "api.x", entries[0].Target)
	require.NotEqual(t, "shh", entries[0].Target)
}

func TestPolicyService_SetPolicyWritesAudit(t *testing.T) {
	svc, a := buildPolicySvc(t)
	ctx := context.Background()

	_, err := svc.SetPolicy(ctx, &sbxv1.SetPolicyRequest{Scope: "", Decision: "deny", Host: "evil.example"})
	require.NoError(t, err)

	entries, err := a.List()
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "policy.deny", entries[0].Action)
	require.Equal(t, "evil.example", entries[0].Target)
}

func TestPolicyService_AuditActorFromGRPCPrincipal(t *testing.T) {
	svc, a := buildPolicySvc(t)
	// Context carrying the gRPC principal as the authn interceptor attaches it.
	ctx := context.WithValue(context.Background(), principalCtxKey{}, principal{userRole: "admin"})

	_, err := svc.SetPolicy(ctx, &sbxv1.SetPolicyRequest{Scope: "", Decision: "deny", Host: "evil.example"})
	require.NoError(t, err)
	_, err = svc.SetSecret(ctx, &sbxv1.SetSecretRequest{Scope: "", Host: "api.x", Env: "TOKEN", Value: "shh"})
	require.NoError(t, err)

	entries, err := a.List()
	require.NoError(t, err)
	require.Len(t, entries, 2)
	for _, e := range entries {
		require.Equal(t, "admin", e.Actor, "audit must attribute the gRPC principal, action=%s", e.Action)
	}
}

func TestPolicyService_PerSandboxScope(t *testing.T) {
	svc, _, mgr := buildPolicySvcMgr(t)
	ctx := context.Background()

	// Create a real sandbox so scopeName -> mgr.Resolve succeeds for its id.
	rec, err := mgr.Create(ctx, sandbox.CreateSpec{})
	require.NoError(t, err)
	id := rec.ID

	// (a) per-sandbox SetSecret/SetPolicy succeed (exercises Resolve).
	_, err = svc.SetSecret(ctx, &sbxv1.SetSecretRequest{Scope: id, Host: "api.x", Env: "TOKEN", Value: "shh"})
	require.NoError(t, err)
	_, err = svc.SetPolicy(ctx, &sbxv1.SetPolicyRequest{Scope: id, Decision: "deny", Host: "evil.example"})
	require.NoError(t, err)

	// (b) ListSecrets on the per-sandbox path returns host/env but NO value.
	resp, err := svc.ListSecrets(ctx, &sbxv1.ScopeRequest{Scope: id})
	require.NoError(t, err)
	require.Len(t, resp.Custom, 1)
	require.Equal(t, "api.x", resp.Custom[0].Host)
	require.Equal(t, "TOKEN", resp.Custom[0].Env)
	require.NotEqual(t, "shh", resp.Custom[0].Env)

	// (c) an unknown sandbox id resolves to NotFound (not Internal).
	_, err = svc.ListSecrets(ctx, &sbxv1.ScopeRequest{Scope: "does-not-exist"})
	require.Error(t, err)
	require.Equal(t, codes.NotFound, status.Code(err))
}
