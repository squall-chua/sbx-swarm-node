package node

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/squall-chua/sbx-swarm-node/internal/auth"
	"github.com/squall-chua/sbx-swarm-node/internal/config"
	"github.com/squall-chua/sbx-swarm-node/internal/coordinator"
	sbxv1 "github.com/squall-chua/sbx-swarm-node/internal/gen/sbxswarm/v1"
	"github.com/squall-chua/sbx-swarm-node/internal/ids"
	"github.com/squall-chua/sbx-swarm-node/internal/obs"
	"github.com/squall-chua/sbx-swarm-node/internal/peer"
	"github.com/squall-chua/sbx-swarm-node/internal/routing"
	"github.com/squall-chua/sbx-swarm-node/internal/sandbox"
	"github.com/squall-chua/sbx-swarm-node/internal/scheduler"
	"github.com/squall-chua/sbx-swarm-node/internal/store"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
)

// TestBuildBackend_FakeWithKitsWarns proves a fake-backend node with kits
// declared in config gets a boot-time warning that the kits are advertised
// but will not be applied (review Minor #7): the fake admits every configured
// name unchecked and Fake.Create accepts a known name while doing nothing
// with it, so such a node is a silent kit-less-create trap for a
// kit-constrained placement.
func TestBuildBackend_FakeWithKitsWarns(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))

	cfg := config.Default()
	cfg.Kits = []config.KitConfig{{Name: "tools", Ref: "/opt/kits/tools"}}

	_, err := buildBackend(cfg, log)
	require.NoError(t, err)
	require.Contains(t, buf.String(), "kits", "expected a boot warning naming the unapplied kits")
	require.Contains(t, buf.String(), "level=WARN")
}

func TestBuildBackend_FakeWithNoKitsIsQuiet(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))

	cfg := config.Default()

	_, err := buildBackend(cfg, log)
	require.NoError(t, err)
	require.Empty(t, buf.String())
}

func TestKitMap(t *testing.T) {
	got := kitMap([]config.KitConfig{
		{Name: "tools", Ref: "/opt/kits/tools"},
		{Name: "extras", Ref: "ghcr.io/acme/extras:v1"},
	})
	want := map[string]string{
		"tools":  "/opt/kits/tools",
		"extras": "ghcr.io/acme/extras:v1",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("want %v, got %v", want, got)
	}
}

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

// TestNode_BootRestoresCordon proves a stored cordon is restored on both
// halves: the local flag, which is what enforcement reads, and the cluster's
// gossiped state, which is what peers read.
func TestNode_BootRestoresCordon(t *testing.T) {
	dir := t.TempDir()

	// Pre-seed the flags as if this node had been cordoned before a restart.
	st, err := store.Open(filepath.Join(dir, "node.db"))
	require.NoError(t, err)
	saveNodeFlags(st, obs.NewLogger("error", io.Discard), nodeFlags{Cordoned: true})
	require.NoError(t, st.Close())

	cfg := config.Default()
	cfg.DataDir = dir
	cfg.ListenAddr = "127.0.0.1:0"
	// A cluster is no longer needed to observe a cordon (Task 3), but keep one
	// here so the mirror-to-cluster half is covered too.
	cfg.GossipAddr = "127.0.0.1:0"
	cfg.ClusterSecret = "test-secret"

	n, err := New(cfg, obs.NewLogger("error", io.Discard), "test")
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = n.Stop(ctx)
	})

	require.True(t, n.nodeSvc.Cordoned(), "a stored cordon must be restored at boot")
	require.True(t, n.cluster.LocalNodeState().Cordoned, "and it must reach the peers")
}

// TestNode_BootRestoresCordon_Standalone proves the restore and the persister
// wiring do not depend on a cluster: a standalone node's cordon must survive a
// restart too.
func TestNode_BootRestoresCordon_Standalone(t *testing.T) {
	dir := t.TempDir()

	st, err := store.Open(filepath.Join(dir, "node.db"))
	require.NoError(t, err)
	saveNodeFlags(st, obs.NewLogger("error", io.Discard), nodeFlags{Cordoned: true})
	require.NoError(t, st.Close())

	cfg := config.Default()
	cfg.DataDir = dir
	cfg.ListenAddr = "127.0.0.1:0"

	n, err := New(cfg, obs.NewLogger("error", io.Discard), "test")
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = n.Stop(ctx)
	})

	require.True(t, n.nodeSvc.Cordoned(), "a standalone node's stored cordon must be restored too")
}

// TestNode_StandaloneCordonBlocksPlacement proves the point of this task: a
// node with no GossipAddr and no ClusterSecret builds no cluster at all, and a
// cordon on it must still block placement. It drives the real HTTP path —
// cordon, then request a sandbox — because node_test.go has no in-process
// handle to the sandbox/ops manager to poll directly; there is also no
// GetOperation RPC, so this polls ListOperations, exactly as a real client
// would.
func TestNode_StandaloneCordonBlocksPlacement(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.ListenAddr = "127.0.0.1:0"
	cfg.APIKeys = []config.APIKey{{Key: "adm", Role: "admin"}}
	// No GossipAddr, no ClusterSecret: standalone.

	n, err := New(cfg, obs.NewLogger("error", io.Discard), "test")
	require.NoError(t, err)
	require.NoError(t, n.Start())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = n.Stop(ctx)
	})

	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	authed := func(req *http.Request) *http.Request {
		req.Header.Set("Authorization", "Bearer adm")
		return req
	}

	req, _ := http.NewRequest(http.MethodPost, "https://"+n.Addr()+"/v1/node/cordon", nil)
	resp, err := client.Do(authed(req))
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	req, _ = http.NewRequest(http.MethodPost, "https://"+n.Addr()+"/v1/sandboxes",
		strings.NewReader(`{"cpus":1,"memory_bytes":1}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err = client.Do(authed(req))
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var op struct {
		Id string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(body, &op))
	require.NotEmpty(t, op.Id)

	// The provision runs in a background goroutine (internal/ops.Manager.Run).
	// Poll ListOperations until it reaches a terminal state.
	var found struct {
		State string `json:"state"`
		Error string `json:"error"`
	}
	require.Eventually(t, func() bool {
		req, _ := http.NewRequest(http.MethodGet, "https://"+n.Addr()+"/v1/operations", nil)
		resp, err := client.Do(authed(req))
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		var out struct {
			Operations []struct {
				Id    string `json:"id"`
				State string `json:"state"`
				Error string `json:"error"`
			} `json:"operations"`
		}
		if json.NewDecoder(resp.Body).Decode(&out) != nil {
			return false
		}
		for _, o := range out.Operations {
			if o.Id == op.Id && o.State != "" && o.State != "pending" && o.State != "running" {
				found.State, found.Error = o.State, o.Error
				return true
			}
		}
		return false
	}, 2*time.Second, 20*time.Millisecond, "provision did not reach a terminal state")

	require.Equal(t, "error", found.State)
	require.Contains(t, found.Error, scheduler.ErrNoEligibleNode.Error())
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
	tbl.Upsert("peerB", "127.0.0.1:1", nil)

	attempt := attemptFor("self", &sbxv1.CreateSandboxRequest{Cpus: 1, MemoryBytes: 1},
		"op-x", nil, nil, tbl, pool, obs.NewLogger("error", io.Discard))
	_, err = attempt(context.Background(), "peerB")
	require.ErrorIs(t, err, coordinator.ErrNack)
}

// TestAttemptFor_LocalUnknownKitNacks proves the LOCAL branch of attemptFor
// (self == nodeID) NACKs on sandbox.ErrUnknownKit exactly like it already does
// on sandbox.ErrNoCapacity, instead of surfacing a hard error that aborts the
// whole placement. Combined with the coordinator package's existing proof that
// ErrNack causes a retry against the next candidate, this shows a stale-gossip
// unknown kit does not abort placement when another node could serve it.
func TestAttemptFor_LocalUnknownKitNacks(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "n.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	mgr := sandbox.NewManager("self", sandbox.NewFake(), st, ids.NewGen("self")) // no kits admitted
	mgr.SetCapacity(sandbox.NewCapacity(4, 1e9, 1e9))

	attempt := attemptFor("self", &sbxv1.CreateSandboxRequest{Cpus: 1, MemoryBytes: 1, Kits: []string{"nope"}},
		"op-x", mgr, nil, nil, nil, obs.NewLogger("error", io.Discard))
	_, err = attempt(context.Background(), "self")
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

// TestNode_BootAdvertisesConfiguredKits boots a node (default backend: fake,
// no daemon needed) with a kit declared in config and checks the name reaches
// the ListNodes-advertised set, i.e. the full config.Kits -> NodeSummary.kits
// wiring at the three AdmittedKits() call sites in node.go, not just the
// kitMap helper (TestKitMap).
func TestNode_BootAdvertisesConfiguredKits(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.ListenAddr = "127.0.0.1:0"
	cfg.APIKeys = []config.APIKey{{Key: "adm", Role: "admin"}}
	cfg.Kits = []config.KitConfig{{Name: "tools", Ref: "/opt/kits/tools"}}
	require.NoError(t, cfg.Validate())

	n, err := New(cfg, obs.NewLogger("error", io.Discard), "test")
	require.NoError(t, err)
	require.NoError(t, n.Start())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = n.Stop(ctx)
	})

	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	req, _ := http.NewRequest(http.MethodGet, "https://"+n.Addr()+"/v1/nodes", nil)
	req.Header.Set("Authorization", "Bearer adm")
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var body struct {
		Nodes []struct {
			Kits []string `json:"kits"`
		} `json:"nodes"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	require.Len(t, body.Nodes, 1)
	require.Equal(t, []string{"tools"}, body.Nodes[0].Kits)
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
