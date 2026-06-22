//go:build integration

package sandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// These integration tests need a running, version-compatible sbx daemon
// (NewSDKBackend uses WithAutoStart + WithStrictVersion). There is no sbx/docker
// in CI, so they are gated behind the `integration` build tag and are the only
// place the REAL backend translation is exercised — everything else runs against
// the in-memory Fake. Run: go test -tags integration ./internal/sandbox/
//
// They are deliberately NOT parallel: they share one daemon and touch global
// state (ports, policy, secrets). Each test removes the sandboxes it creates.
//
// Observed daemon contract (sbx v0.32.0), discovered by running these:
//   - Create REQUIRES an agent (WithAgent) — "shell" = a sandbox with no AI agent.
//   - Create REQUIRES at least one workspace (WithWorkspace).
//   - In --clone mode the primary workspace must be read/WRITE (NO ":ro"): sbx
//     mounts the clone read-only itself. SDKBackend.Create now drops ":ro" on the
//     primary clone workspace accordingly.
//   - Unpublish needs a HOST_PORT:SANDBOX_PORT spec, not the bare sandbox port.

func noWorkspaces(string) (string, bool, bool) { return "", false, false }

// dial connects to the local daemon. It FAILS (not skips) on a connect/version
// error: that failure is the signal this scaffolding exists to surface — an
// absent or version-incompatible daemon (the long-standing post-M7 gap).
func dial(t *testing.T, resolve WorkspaceResolver) *SDKBackend {
	t.Helper()
	b, err := NewSDKBackend(context.Background(), resolve)
	require.NoError(t, err, "connect daemon: need a version-compatible sbx daemon")
	return b
}

// backendWS dials a backend and returns the workspace mount to attach. The
// daemon requires at least one workspace on Create; "ws" maps to a fresh RW
// temp dir.
func backendWS(t *testing.T) (*SDKBackend, []WorkspaceMount) {
	t.Helper()
	dir := t.TempDir()
	b := dial(t, func(name string) (string, bool, bool) {
		if name == "ws" {
			return dir, false, true
		}
		return "", false, false
	})
	return b, []WorkspaceMount{{Name: "ws"}}
}

// mkSandbox creates a sandbox and schedules its removal.
func mkSandbox(t *testing.T, b *SDKBackend, spec CreateSpec) BackendSandbox {
	t.Helper()
	if spec.Agent == "" {
		spec.Agent = "shell" // daemon v0.32.0 requires an agent; shell = no AI agent
	}
	sb, err := b.Create(context.Background(), spec)
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.Remove(context.Background(), sb.Name) })
	return sb
}

func containsName(list []BackendSandbox, name string) bool {
	for _, s := range list {
		if s.Name == name {
			return true
		}
	}
	return false
}

func hasContainerPort(ports []PublishedPort, cp int) bool {
	for _, p := range ports {
		if p.ContainerPort == cp {
			return true
		}
	}
	return false
}

func hasAllow(rules []PolicyRule, host string) bool {
	for _, r := range rules {
		// `sbx policy ls` puts the allowed host in the RESOURCES column (not the
		// rule name); match either to be robust across SDK rule shapes.
		if r.Decision == "allow" && (strings.Contains(r.Resources, host) || strings.Contains(r.Rule, host)) {
			return true
		}
	}
	return false
}

// ---- Area 1: SDKBackend adapter coverage (was: Create/Exec/Remove only) ----

// TestSDKBackend_CreateExecRemove is the original smoke: Create → Exec → Remove.
func TestSDKBackend_CreateExecRemove(t *testing.T) {
	ctx := context.Background()
	b, ws := backendWS(t)

	sb := mkSandbox(t, b, CreateSpec{Name: "it-create-exec", CPUs: 1, MemoryBytes: 1 << 30, Workspaces: ws})

	res, err := b.Exec(ctx, sb.Name, []string{"true"}, ExecOpts{})
	require.NoError(t, err)
	require.Equal(t, 0, res.ExitCode)
}

// TestSDKBackend_Lifecycle covers Get/Stop/Start/List/Remove status transitions.
func TestSDKBackend_Lifecycle(t *testing.T) {
	ctx := context.Background()
	b, ws := backendWS(t)
	sb := mkSandbox(t, b, CreateSpec{Name: "it-lifecycle", CPUs: 1, MemoryBytes: 1 << 30, Workspaces: ws})

	got, err := b.Get(ctx, sb.Name)
	require.NoError(t, err)
	require.Equal(t, "running", got.Status)

	require.NoError(t, b.Stop(ctx, sb.Name))
	got, err = b.Get(ctx, sb.Name)
	require.NoError(t, err)
	require.Equal(t, "stopped", got.Status)

	require.NoError(t, b.Start(ctx, sb.Name))
	got, err = b.Get(ctx, sb.Name)
	require.NoError(t, err)
	require.Equal(t, "running", got.Status)

	list, err := b.List(ctx)
	require.NoError(t, err)
	require.True(t, containsName(list, sb.Name))

	require.NoError(t, b.Remove(ctx, sb.Name))
	_, err = b.Get(ctx, sb.Name)
	require.ErrorIs(t, err, ErrNotFound)
}

// TestSDKBackend_ExecDetached covers ExecDetached + PollDetached (exit-code
// propagation). The command lingers (sleep) so polls can observe Running before
// it exits, then asserts the completed state carries the exit code — exactly what
// production AgentRun (sandboxservice.go) relies on to gate exit-0 auto-publish.
//
// The poll loop logs each transition so a 404 (daemon discarding a finished exec)
// is visible in -v output rather than just a timeout.
func TestSDKBackend_ExecDetached(t *testing.T) {
	ctx := context.Background()
	b, ws := backendWS(t)
	sb := mkSandbox(t, b, CreateSpec{Name: "it-detached", CPUs: 1, MemoryBytes: 1 << 30, Workspaces: ws})

	id, err := b.ExecDetached(ctx, sb.Name, []string{"sh", "-c", "sleep 3; exit 7"}, ExecOpts{})
	require.NoError(t, err)
	require.NotEmpty(t, id)

	sawRunning := false
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		st, perr := b.PollDetached(ctx, sb.Name, id)
		t.Logf("poll: done=%v exit=%d err=%v", st.Done, st.ExitCode, perr)
		if perr != nil {
			t.Fatalf("PollDetached returned an error (daemon discarded the exec?): %v", perr)
		}
		if !st.Done {
			sawRunning = true
		}
		if st.Done {
			require.True(t, sawRunning, "exec reported Done before ever observed Running")
			require.Equal(t, 7, st.ExitCode)
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatal("detached exec never reported Done within 20s")
}

// TestSDKBackend_Ports covers PublishPort/Ports/UnpublishPort. UnpublishPort takes
// the container port but the daemon requires a HOST_PORT:SANDBOX_PORT spec, so the
// adapter resolves the host port via Ports first (sdkbackend.go).
func TestSDKBackend_Ports(t *testing.T) {
	ctx := context.Background()
	b, ws := backendWS(t)
	sb := mkSandbox(t, b, CreateSpec{Name: "it-ports", CPUs: 1, MemoryBytes: 1 << 30, Workspaces: ws})

	p, err := b.PublishPort(ctx, sb.Name, 8080)
	require.NoError(t, err)
	require.Equal(t, 8080, p.ContainerPort)
	require.Greater(t, p.HostPort, 0)

	ports, err := b.Ports(ctx, sb.Name)
	require.NoError(t, err)
	require.True(t, hasContainerPort(ports, 8080))

	require.NoError(t, b.UnpublishPort(ctx, sb.Name, 8080))
	ports, err = b.Ports(ctx, sb.Name)
	require.NoError(t, err)
	require.False(t, hasContainerPort(ports, 8080))
}

// TestSDKBackend_CopyRoundTrip covers CopyTo + CopyFrom (byte-for-byte).
func TestSDKBackend_CopyRoundTrip(t *testing.T) {
	ctx := context.Background()
	b, ws := backendWS(t)
	sb := mkSandbox(t, b, CreateSpec{Name: "it-copy", CPUs: 1, MemoryBytes: 1 << 30, Workspaces: ws})

	dir := t.TempDir()
	src := filepath.Join(dir, "up.txt")
	want := []byte("payload-" + t.Name())
	require.NoError(t, os.WriteFile(src, want, 0o644))

	require.NoError(t, b.CopyTo(ctx, sb.Name, src, "/tmp/up.txt"))

	dst := filepath.Join(dir, "down.txt")
	require.NoError(t, b.CopyFrom(ctx, sb.Name, "/tmp/up.txt", dst))

	got, err := os.ReadFile(dst)
	require.NoError(t, err)
	require.Equal(t, want, got)
}

// TestSDKBackend_ListTemplates is a smoke check (templates may be empty).
func TestSDKBackend_ListTemplates(t *testing.T) {
	_, err := dial(t, noWorkspaces).ListTemplates(context.Background())
	require.NoError(t, err)
}

// TestSDKBackend_PolicyRoundTrip covers Profiles/Allow/List. The allowed host
// appears in the RESOURCES column of `sbx policy ls`, which hasAllow matches.
//
// PolicyRemoveRule is NOT exercised here: SDK v0.1.2's RemoveRule(scope) runs
// `policy rm network --sandbox <scope>` with no selector, but sbx v0.32.0 requires
// --id or --resource ("at least one selector is required"). That is an SDK-vs-daemon
// gap that needs an SDK bump (or a Backend.PolicyRemoveRule signature that carries a
// resource) — it can't be fixed in the adapter alone. Cleanup uses the CLI directly.
func TestSDKBackend_PolicyRoundTrip(t *testing.T) {
	ctx := context.Background()
	b, ws := backendWS(t)

	profiles, err := b.PolicyProfiles(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, profiles)

	sb := mkSandbox(t, b, CreateSpec{Name: "it-policy", CPUs: 1, MemoryBytes: 1 << 30, Workspaces: ws})
	scope := sb.Name

	require.NoError(t, b.PolicyAllow(ctx, scope, "example.com"))
	t.Cleanup(func() {
		_ = exec.Command("sbx", "policy", "rm", "network", "--sandbox", scope, "--resource", "example.com").Run()
	})

	rules, err := b.PolicyList(ctx, scope)
	require.NoError(t, err)
	require.True(t, hasAllow(rules, "example.com"))
}

// TestSDKBackend_SecretRoundTrip covers SecretSet/List/Remove and that reads MASK
// the value. TODO: confirm secret SCOPE semantics for SDK v0.1.2 (see policy note).
func TestSDKBackend_SecretRoundTrip(t *testing.T) {
	ctx := context.Background()
	b, ws := backendWS(t)
	sb := mkSandbox(t, b, CreateSpec{Name: "it-secret", CPUs: 1, MemoryBytes: 1 << 30, Workspaces: ws})
	scope := sb.Name

	require.NoError(t, b.SecretSet(ctx, scope, CustomSecret{Host: "api.example.com", Env: "API_TOKEN", Value: "s3cr3t"}))

	got, err := b.SecretList(ctx, scope)
	require.NoError(t, err)
	var found *CustomSecret
	for i := range got.Custom {
		if got.Custom[i].Host == "api.example.com" {
			found = &got.Custom[i]
		}
	}
	require.NotNil(t, found, "secret not listed after set")
	require.Equal(t, "API_TOKEN", found.Env)
	require.Empty(t, found.Value, "secret value must be masked on read")

	require.NoError(t, b.SecretRemove(ctx, scope, "api.example.com"))
}

// ---- Area 3: M7 idle-stop — real CPU drives the activity signal ----

// cpuActiveThreshold mirrors node.cpuActiveThreshold (ADR-0016). It is duplicated
// by hand because package node can't be imported from package sandbox. The Fake
// reports a CONSTANT 10% and so can never exercise this branch — only a real
// daemon produces variable CPU.
const cpuActiveThreshold = 5.0

// TestSDKBackend_StatsReflectsRealCPU proves Stats().CPUPercent tracks real load:
// an idle sandbox settles BELOW the threshold (the Fake's fixed 10% would wrongly
// read "active"), and a spin-loop drives a busy one AT/ABOVE it — the exact signal
// the node reaper's CPU-as-activity bump depends on (node.go).
func TestSDKBackend_StatsReflectsRealCPU(t *testing.T) {
	ctx := context.Background()

	bIdle, wsIdle := backendWS(t)
	idle := mkSandbox(t, bIdle, CreateSpec{Name: "it-stats-idle", CPUs: 1, MemoryBytes: 1 << 30, Workspaces: wsIdle})
	require.Eventually(t, func() bool {
		u, err := bIdle.Stats(ctx, idle.Name)
		return err == nil && u.CPUPercent < cpuActiveThreshold
	}, 20*time.Second, 500*time.Millisecond, "idle sandbox never settled below threshold")

	bBusy, wsBusy := backendWS(t)
	busy := mkSandbox(t, bBusy, CreateSpec{Name: "it-stats-busy", CPUs: 1, MemoryBytes: 1 << 30, Workspaces: wsBusy})
	_, err := bBusy.ExecDetached(ctx, busy.Name, []string{"sh", "-c", "while true; do :; done"}, ExecOpts{})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		u, err := bBusy.Stats(ctx, busy.Name)
		return err == nil && u.CPUPercent >= cpuActiveThreshold
	}, 20*time.Second, 500*time.Millisecond, "busy sandbox CPU never crossed threshold")
}

// ---- Area 2: M6 git publish — `sbx --clone` registers the sandbox remote ----

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "git %s: %s", strings.Join(args, " "), out)
	return string(out)
}

// TestSDKBackend_CloneRegistersSandboxRemote confirms the M6 carry-forward crux:
// `sbx --clone` registers a remote named "sandbox-"+name on the host repo (its
// --help: "commits are accessible via the sandbox-<name> git remote on the host").
// doPublish runs `git fetch sandbox-<BackendName> ...` against that remote
// (apiserver sandboxservice.go:438), so the name must match exactly. The dotted
// swarm-id name shape (<node>.<ULID>) is used on purpose.
//
// This mounts the workspace read-only at the resolver level (ro=true) — exactly
// what the production workspaceResolver does for git-backed workspaces (ADR-0015).
// The fix in SDKBackend.Create (don't append ":ro" to the primary in --clone mode)
// is what lets this succeed; before it, sbx rejected "primary workspace must be
// read/write".
func TestSDKBackend_CloneRegistersSandboxRemote(t *testing.T) {
	requireGit(t)

	// A real git repo with a commit on main, so the clone has something to clone.
	base := t.TempDir()
	runGit(t, base, "init", "-b", "main")
	runGit(t, base, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "--allow-empty", "-m", "init")

	const wsName = "ws"
	b := dial(t, func(name string) (string, bool, bool) {
		if name == wsName {
			return base, true, true // ro=true: mirrors the git-backed resolver (ADR-0015)
		}
		return "", false, false
	})

	sb := mkSandbox(t, b, CreateSpec{
		Name:        "node.01HXCLONEREMOTE0000000000",
		CPUs:        1,
		MemoryBytes: 1 << 30,
		Clone:       true,
		Branch:      "main",
		Workspaces:  []WorkspaceMount{{Name: wsName}},
	})

	want := "sandbox-" + sb.Name
	out := runGit(t, base, "remote")
	require.Contains(t, strings.Fields(out), want,
		"clone-mode create did not register the expected sandbox remote on the base")
}
