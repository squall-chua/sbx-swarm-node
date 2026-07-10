package node

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/squall-chua/sbx-swarm-node/internal/auth"
	"github.com/squall-chua/sbx-swarm-node/internal/config"
	"github.com/squall-chua/sbx-swarm-node/internal/coordinator"
	sbxv1 "github.com/squall-chua/sbx-swarm-node/internal/gen/sbxswarm/v1"
	"github.com/squall-chua/sbx-swarm-node/internal/obs"
	"github.com/squall-chua/sbx-swarm-node/internal/peer"
	"github.com/squall-chua/sbx-swarm-node/internal/routing"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

func TestNode_BootServeStop(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.ListenAddr = "127.0.0.1:0"
	cfg.APIKeys = []config.APIKey{{Key: "adm", Role: "admin"}}

	n, err := New(cfg, obs.NewLogger("error", io.Discard), "test")
	require.NoError(t, err)
	require.NoError(t, n.Start())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = n.Stop(ctx)
	})

	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}

	// health is unauthenticated
	resp, err := client.Get("https://" + n.Addr() + "/healthz")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	// /v1/node needs auth
	req, _ := http.NewRequest(http.MethodGet, "https://"+n.Addr()+"/v1/node", nil)
	req.Header.Set("Authorization", "Bearer adm")
	resp, err = client.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()
}

func TestWorkspaceResolver(t *testing.T) {
	resolve := workspaceResolver([]config.WorkspaceConfig{
		{Name: "data", HostPath: "/srv/data", ReadOnly: false},
		{Name: "ro", HostPath: "/srv/ro", ReadOnly: true},
		{Name: "repo", HostPath: "/srv/repo.git", ReadOnly: false, Git: &config.GitConfig{}},
	}, "")

	host, ro, ok := resolve("data")
	require.True(t, ok)
	require.Equal(t, "/srv/data", host)
	require.False(t, ro)

	_, ro, ok = resolve("ro")
	require.True(t, ok)
	require.True(t, ro)

	// git-backed mounts are always read-only (ADR-0015), even with read_only:false.
	host, ro, ok = resolve("repo")
	require.True(t, ok)
	require.Equal(t, "/srv/repo.git", host)
	require.True(t, ro)

	_, _, ok = resolve("missing")
	require.False(t, ok)
}

// TestEffectiveGitBase_ProviderVsHostPath asserts buildGitWorkspaces and
// workspaceResolver resolve a workspace's host-side base identically, for both
// the operator-set host_path case and the provider-workspace (remote_url, no
// host_path) auto-mirror case (ADR-0020).
func TestEffectiveGitBase_ProviderVsHostPath(t *testing.T) {
	t.Setenv("SBX_GIT_WORKSPACE_DIR", "")
	dataDir := t.TempDir()

	t.Run("provider workspace with empty host_path", func(t *testing.T) {
		ws := []config.WorkspaceConfig{
			{Name: "acme", HostPath: "", Git: &config.GitConfig{RemoteURL: "https://github.com/acme/app"}},
		}
		want := filepath.Join(dataDir, "git-workspaces", "acme.git")

		gw := buildGitWorkspaces(ws, dataDir)
		require.Equal(t, want, gw["acme"].Base())

		host, ro, ok := workspaceResolver(ws, dataDir)("acme")
		require.True(t, ok)
		require.True(t, ro)
		require.Equal(t, want, host)
	})

	t.Run("operator host_path set", func(t *testing.T) {
		ws := []config.WorkspaceConfig{
			{Name: "repo", HostPath: "/srv/repo.git", Git: &config.GitConfig{RemoteURL: "https://github.com/acme/app"}},
		}

		gw := buildGitWorkspaces(ws, dataDir)
		require.Equal(t, "/srv/repo.git", gw["repo"].Base())

		host, ro, ok := workspaceResolver(ws, dataDir)("repo")
		require.True(t, ok)
		require.True(t, ro)
		require.Equal(t, "/srv/repo.git", host)
	})
}

func TestNode_SSEEndpointAuthed(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.ListenAddr = "127.0.0.1:0"
	cfg.APIKeys = []config.APIKey{{Key: "adm", Role: "admin"}}

	n, err := New(cfg, obs.NewLogger("error", io.Discard), "test")
	require.NoError(t, err)
	require.NoError(t, n.Start())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = n.Stop(ctx)
	})

	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}

	// unauthenticated SSE -> 401
	resp, err := client.Get("https://" + n.Addr() + "/v1/events")
	require.NoError(t, err)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	resp.Body.Close()
}

type flakyProvisionClient struct {
	calls     int
	sandboxID string
}

func (f *flakyProvisionClient) Provision(_ context.Context, _ *sbxv1.ProvisionRequest, _ ...grpc.CallOption) (*sbxv1.ProvisionReply, error) {
	f.calls++
	if f.calls == 1 {
		return nil, errors.New("transport reset")
	}
	return &sbxv1.ProvisionReply{Accepted: true, SandboxId: f.sandboxID}, nil
}

func TestCallProvisionWithRetry_RetriesOnceWhenIdempotent(t *testing.T) {
	c := &flakyProvisionClient{sandboxID: "sb-1"}
	reply, err := callProvisionWithRetry(context.Background(), c,
		&sbxv1.ProvisionRequest{RequestId: "op-1", Spec: &sbxv1.CreateSandboxRequest{Cpus: 1}})
	require.NoError(t, err)
	require.Equal(t, 2, c.calls, "must retry the same target once")
	require.Equal(t, "sb-1", reply.SandboxId)
}

func TestCallProvisionWithRetry_NoRetryWithoutRequestID(t *testing.T) {
	c := &flakyProvisionClient{sandboxID: "sb-1"}
	_, err := callProvisionWithRetry(context.Background(), c,
		&sbxv1.ProvisionRequest{Spec: &sbxv1.CreateSandboxRequest{Cpus: 1}}) // empty RequestId
	require.Error(t, err, "no idempotency key => must not retry (duplicate risk)")
	require.Equal(t, 1, c.calls)
}

func TestAttemptFor_DialFailureNacks(t *testing.T) {
	// A peer in the routing table whose pin is unknown makes pool.Conn fail-closed.
	// The attempt must NACK so the coordinator falls through to the next candidate,
	// rather than surfacing a hard error that aborts the whole placement.
	_, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	pool := peer.NewPool(
		peer.WithNodeKey("self", priv),
		peer.WithPinResolver(func(string) ([]byte, bool) { return nil, false }),
	)
	tbl := routing.NewTable("self")
	tbl.Upsert("peerB", "127.0.0.1:1", false, nil)

	attempt := attemptFor("self", &sbxv1.CreateSandboxRequest{Cpus: 1, MemoryBytes: 1},
		"op-x", nil, nil, tbl, pool, obs.NewLogger("error", io.Discard))
	_, err = attempt(context.Background(), "peerB")
	require.ErrorIs(t, err, coordinator.ErrNack)
}

func TestReapInterval(t *testing.T) {
	require.Equal(t, 30*time.Second, reapInterval(30*time.Second))
	require.Equal(t, time.Minute, reapInterval(10*time.Minute))
	require.Equal(t, time.Minute, reapInterval(time.Minute))
}

func TestNode_BootWithIdleTimeout(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.ListenAddr = "127.0.0.1:0"
	cfg.IdleTimeout = "50ms" // reaper enabled; fast sweep
	require.NoError(t, cfg.Validate())

	n, err := New(cfg, obs.NewLogger("error", io.Discard), "test")
	require.NoError(t, err)
	require.NoError(t, n.Start())
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, n.Stop(ctx))
}

func TestNode_SessionKeyIsSwarmWideWhenClustered(t *testing.T) {
	// Two nodes with the same cluster secret derive the same session signer, so a
	// token minted by one verifies on the other (cross-node sessions, ADR-0010).
	seedA := bytes.Repeat([]byte{1}, ed25519.SeedSize)
	seedB := bytes.Repeat([]byte{2}, ed25519.SeedSize)
	kA := auth.DeriveSessionKey("shared-secret", ed25519.NewKeyFromSeed(seedA).Seed())
	kB := auth.DeriveSessionKey("shared-secret", ed25519.NewKeyFromSeed(seedB).Seed())
	require.Equal(t, kA, kB)
}

func TestGitWorkspaceNames(t *testing.T) {
	ws := []config.WorkspaceConfig{
		{Name: "repo", Git: &config.GitConfig{}},
		{Name: "plain"},
		{Name: "repo2", Git: &config.GitConfig{Remote: "git@x:y.git"}},
	}
	require.Equal(t, []string{"repo", "repo2"}, gitWorkspaceNames(ws))
	require.Empty(t, gitWorkspaceNames([]config.WorkspaceConfig{{Name: "plain"}}))
}
