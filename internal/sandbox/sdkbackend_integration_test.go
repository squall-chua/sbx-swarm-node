//go:build integration

package sandbox

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdksecret "github.com/squall-chua/sbx-go-sdk/secret"
	"github.com/stretchr/testify/require"

	sdksandbox "github.com/squall-chua/sbx-go-sdk/sandbox"
	"github.com/squall-chua/sbx-swarm-node/internal/ids"
	"github.com/squall-chua/sbx-swarm-node/internal/store"
)

// These integration tests need a running sbx daemon (NewSDKBackend uses
// WithAutoStart; a version mismatch only warns). There is no sbx/docker
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

// dial connects to the local daemon. It FAILS (not skips) on a connect error:
// that failure is the signal this scaffolding exists to surface — an absent
// daemon (the long-standing post-M7 gap).
func dial(t *testing.T, resolve WorkspaceResolver) *SDKBackend {
	t.Helper()
	return dialKits(t, resolve, nil)
}

// dialKits is dial with configured kits, for the kit tests.
func dialKits(t *testing.T, resolve WorkspaceResolver, kits map[string]string) *SDKBackend {
	t.Helper()
	b, err := NewSDKBackend(context.Background(), resolve, kits, slog.Default())
	require.NoError(t, err, "connect daemon: need a running sbx daemon")
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

func TestSDKBackend_ListTemplateInfo(t *testing.T) {
	infos, err := dial(t, noWorkspaces).ListTemplateInfo(context.Background())
	require.NoError(t, err)
	require.NotEmpty(t, infos, "daemon should hold at least the shell-docker template")
	require.NotEmpty(t, infos[0].Repository)
}

// TestSDKBackend_PolicyRoundTrip covers Profiles/Allow/List/RemoveRule. The allowed
// host appears in the RESOURCES column of `sbx policy ls`, which hasAllow matches.
// RemoveRule takes the resource selector that sbx requires (--resource).
func TestSDKBackend_PolicyRoundTrip(t *testing.T) {
	ctx := context.Background()
	b, ws := backendWS(t)

	profiles, err := b.PolicyProfiles(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, profiles)

	sb := mkSandbox(t, b, CreateSpec{Name: "it-policy", CPUs: 1, MemoryBytes: 1 << 30, Workspaces: ws})
	scope := sb.Name

	require.NoError(t, b.PolicyAllow(ctx, scope, "example.com"))
	rules, err := b.PolicyList(ctx, scope)
	require.NoError(t, err)
	require.True(t, hasAllow(rules, "example.com"))

	require.NoError(t, b.PolicyRemoveRule(ctx, scope, "example.com"))
	rules, err = b.PolicyList(ctx, scope)
	require.NoError(t, err)
	require.False(t, hasAllow(rules, "example.com"), "rule should be gone after RemoveRule")
}

// TestSDKBackend_SecretRoundTrip covers SecretSet/List/Remove and that reads MASK
// the value. TODO: confirm secret SCOPE semantics for SDK v0.1.2 (see policy note).
func TestSDKBackend_SecretRoundTrip(t *testing.T) {
	ctx := context.Background()
	b, ws := backendWS(t)
	sb := mkSandbox(t, b, CreateSpec{Name: "it-secret", CPUs: 1, MemoryBytes: 1 << 30, Workspaces: ws})
	scope := sb.Name

	require.NoError(t, b.SecretSet(ctx, scope, CustomSecret{Host: "api.example.com", Env: "API_TOKEN", Value: "AAAAAAAA-first-0001"}))

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

	// Rotation: a second write to the same (scope, env) must land. The daemon
	// rejects it unless the existing placeholder is re-supplied, which SecretSet
	// looks up for us. The placeholder must NOT change — it is the value the
	// sandbox already holds in its env var, so changing it would break the
	// sandbox rather than rotate the secret.
	firstPlaceholder := found.Placeholder
	require.NotEmpty(t, firstPlaceholder, "daemon did not report a placeholder")

	// The daemon's masked value column reveals a prefix (and sometimes a suffix)
	// of the real value, so it changes when the value really changes. Read it
	// through the SDK's own List directly: SDKBackend.SecretList deliberately
	// drops ValueMasked (spec §11 — the node API never returns even a masked
	// value), so this is the only way to observe a real rotation from this test.
	beforeList, err := sdksecret.List(ctx, b.cl, scope)
	require.NoError(t, err)
	var beforeMasked string
	for _, c := range beforeList.Custom {
		if c.Env == "API_TOKEN" {
			beforeMasked = c.ValueMasked
		}
	}
	require.NotEmpty(t, beforeMasked, "masked value not found before rotation")

	require.NoError(t,
		b.SecretSet(ctx, scope, CustomSecret{Host: "api.example.com", Env: "API_TOKEN", Value: "ZZZZZZZZ-second-9999"}),
		"second write to the same env must succeed")

	after, err := b.SecretList(ctx, scope)
	require.NoError(t, err)
	var rotated *CustomSecret
	for i := range after.Custom {
		if after.Custom[i].Env == "API_TOKEN" {
			rotated = &after.Custom[i]
		}
	}
	require.NotNil(t, rotated, "secret not listed after rotation")
	require.Equal(t, firstPlaceholder, rotated.Placeholder, "rotation must reuse the placeholder")
	require.Empty(t, rotated.Value, "secret value must stay masked after rotation")

	// Exit 0 alone cannot tell a real value replacement from a silent cancel
	// (the SDK warns set-custom may hit exactly that shape). The masked value
	// changing is the only observable proof the write actually replaced the
	// value rather than being a no-op.
	afterList, err := sdksecret.List(ctx, b.cl, scope)
	require.NoError(t, err)
	var afterMasked string
	for _, c := range afterList.Custom {
		if c.Env == "API_TOKEN" {
			afterMasked = c.ValueMasked
		}
	}
	require.NotEmpty(t, afterMasked, "masked value not found after rotation")
	require.NotEqual(t, beforeMasked, afterMasked, "masked value must change after a real rotation")

	// A global list must NOT carry this sandbox's secret. Bare `sbx secret ls`
	// lists every scope, so SecretList filters rows to the requested scope.
	global, err := b.SecretList(ctx, "")
	require.NoError(t, err)
	for _, c := range global.Custom {
		require.NotEqual(t, "api.example.com", c.Host, "per-sandbox secret leaked into the global listing")
	}
	for _, st := range global.Stored {
		require.Empty(t, st.Scope, "global listing must only carry global-scope rows")
	}

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

func TestSDKBackend_ExecInteractive(t *testing.T) {
	ctx := context.Background()
	b, ws := backendWS(t)
	sb := mkSandbox(t, b, CreateSpec{Name: "it-terminal", CPUs: 1, MemoryBytes: 1 << 30, Workspaces: ws})

	sess, err := b.ExecInteractive(ctx, sb.Name, []string{"/bin/sh"}, true)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sess.Close() })

	_, err = sess.Stdin().Write([]byte("echo it-terminal-ok; exit\n"))
	require.NoError(t, err)

	out := make([]byte, 0, 256)
	buf := make([]byte, 256)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		n, rerr := sess.Stdout().Read(buf)
		out = append(out, buf[:n]...)
		if strings.Contains(string(out), "it-terminal-ok") {
			return
		}
		if rerr != nil {
			break
		}
	}
	t.Fatalf("did not see terminal echo; got: %q", out)
}

// TestSDKBackend_CreateWithKit proves the whole kit path against a live daemon: a
// configured mixin is admitted, resolved by name, and applied at create. The
// environment variable read from inside the sandbox is the proof it was applied,
// not merely accepted.
func TestSDKBackend_CreateWithKit(t *testing.T) {
	ref, err := filepath.Abs("testdata/mixin-kit")
	require.NoError(t, err)

	dir := t.TempDir()
	b := dialKits(t, func(name string) (string, bool, bool) {
		if name == "ws" {
			return dir, false, true
		}
		return "", false, false
	}, map[string]string{"testkit": ref})

	require.Equal(t, []string{"testkit"}, b.AdmittedKits(), "the fixture mixin must be admitted")

	sb := mkSandbox(t, b, CreateSpec{
		Name:       "swarmkit-create",
		Workspaces: []WorkspaceMount{{Name: "ws"}},
		Kits:       []string{"testkit"},
	})

	ctx := context.Background()

	// The daemon records the kit list on the sandbox, as absolute references.
	h, err := sdksandbox.Get(ctx, b.cl, sb.Name)
	require.NoError(t, err)
	kits, err := h.Kits(ctx)
	require.NoError(t, err)
	require.Contains(t, kits, ref, "the recorded reference must be the absolute one")

	out, err := b.Exec(ctx, sb.Name, []string{"printenv", "SWARM_KIT_TEST"}, ExecOpts{})
	require.NoError(t, err)
	require.Equal(t, "1", strings.TrimSpace(string(out.Stdout)), "the kit's environment variable must be applied")
}

// TestSDKBackend_RefusesASandboxKindKit proves the admission gate against real
// daemon output. A kind: sandbox kit supplies the base image, which would make
// the scheduler's template constraint a lie, so it must never be advertised.
func TestSDKBackend_RefusesASandboxKindKit(t *testing.T) {
	dir := t.TempDir()
	spec := "schemaVersion: \"2\"\nkind: sandbox\nname: swarm-node-bad-kit\nversion: 0.0.1\nsandbox:\n  image: alpine:3\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "spec.yaml"), []byte(spec), 0o644))

	b := dialKits(t, noWorkspaces, map[string]string{"badkit": dir})
	require.Empty(t, b.AdmittedKits(), "a sandbox-kind kit must not be advertised")
}

// TestSDKBackend_KitCredentialsNotInSecretList settles the credentials-in-
// SecretList question the design doc (and this file, before this test existed)
// left open. A `credentials` entry declares a NEED ({service, description,
// required}) — there is no value/token/env field on it — so there is nothing
// for the daemon to materialise into the secret store, at any scope. required:
// true with no matching secret present must also not block the create; a
// credentials block is informational, not enforced. Verified against sbx
// v0.37.0.
//
// This fixture declares no environment.variables (unlike mixin-kit), so it has
// no side effect to observe as proof the kit was actually applied — the test
// checks h.Kits(ctx) for that BEFORE asserting SecretList is empty, so it reads
// "the kit really was applied — and even so, no secret appeared" rather than a
// tautology that would also pass against a daemon that silently no-ops --kit.
func TestSDKBackend_KitCredentialsNotInSecretList(t *testing.T) {
	ref, err := filepath.Abs("testdata/credential-kit")
	require.NoError(t, err)

	dir := t.TempDir()
	b := dialKits(t, func(name string) (string, bool, bool) {
		if name == "ws" {
			return dir, false, true
		}
		return "", false, false
	}, map[string]string{"credkit": ref})

	require.Equal(t, []string{"credkit"}, b.AdmittedKits(), "a credentials block must not block admission")

	sb := mkSandbox(t, b, CreateSpec{
		Name:       "it-kit-cred",
		Workspaces: []WorkspaceMount{{Name: "ws"}},
		Kits:       []string{"credkit"},
	})
	ctx := context.Background()

	// Proof of application, not just acceptance: the daemon records the kit list
	// on the sandbox, as absolute references.
	h, err := sdksandbox.Get(ctx, b.cl, sb.Name)
	require.NoError(t, err)
	kits, err := h.Kits(ctx)
	require.NoError(t, err)
	require.Contains(t, kits, ref, "the credentials kit must actually be applied to the sandbox")

	scoped, err := b.SecretList(ctx, sb.Name)
	require.NoError(t, err)
	require.Empty(t, scoped.Stored, "a credentials entry declares a need, not a value; nothing to store")
	require.Empty(t, scoped.Custom, "a credentials entry declares a need, not a value; nothing to store")

	global, err := b.SecretList(ctx, "")
	require.NoError(t, err)
	require.Empty(t, global.Stored, "a credentials entry declares a need, not a value; nothing to store")
	require.Empty(t, global.Custom, "a credentials entry declares a need, not a value; nothing to store")
}

// TestSDKBackend_KitPortsMatchLiveRead settles the Record.Ports-vs-live-read
// question the design doc left open, against a kit that really publishes a
// port. It goes through Manager.Create, not just the backend, because the
// actual claim is that the node's best-effort post-create snapshot
// (Record.Ports) does not lag a live ListPorts read. Verified against sbx
// v0.37.0: no timing gap observed.
func TestSDKBackend_KitPortsMatchLiveRead(t *testing.T) {
	ref, err := filepath.Abs("testdata/ports-kit")
	require.NoError(t, err)

	dir := t.TempDir()
	b := dialKits(t, func(name string) (string, bool, bool) {
		if name == "ws" {
			return dir, false, true
		}
		return "", false, false
	}, map[string]string{"portkit": ref})

	require.Equal(t, []string{"portkit"}, b.AdmittedKits(), "a publishedPorts block must not block admission")

	st, err := store.Open(filepath.Join(t.TempDir(), "n.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	m := NewManager("node1", b, st, ids.NewGen("node1"))

	ctx := context.Background()
	rec, err := m.Create(ctx, CreateSpec{
		Agent:      "shell",
		Workspaces: []WorkspaceMount{{Name: "ws"}},
		Kits:       []string{"portkit"},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.Remove(context.Background(), rec.BackendName) })

	require.True(t, hasContainerPort(rec.Ports, 18081), "Manager.Create must snapshot the kit's published port")

	live, err := b.Ports(ctx, rec.BackendName)
	require.NoError(t, err)
	require.Equal(t, rec.Ports, live, "the snapshot taken at create must agree with a live ListPorts read")
}

// TestSDKBackend_ReadOnlyWorkspaceRules pins sbx's positional read-only rule
// (regression for the read-only primary-workspace bug). The daemon requires the
// primary (first) workspace be read/write, so a read-only primary in non-clone
// mode is rejected by SDKBackend before any daemon round-trip; a read-only
// SECONDARY workspace is accepted and mounted.
func TestSDKBackend_ReadOnlyWorkspaceRules(t *testing.T) {
	ctx := context.Background()
	d1, d2 := t.TempDir(), t.TempDir()
	b := dial(t, func(n string) (string, bool, bool) {
		switch n {
		case "a":
			return d1, false, true
		case "b":
			return d2, false, true
		}
		return "", false, false
	})

	// Read-only PRIMARY (non-clone) is rejected locally — no daemon round-trip.
	_, err := b.Create(ctx, CreateSpec{Agent: "shell",
		Workspaces: []WorkspaceMount{{Name: "a", ReadOnly: true}}})
	require.ErrorContains(t, err, "cannot be read-only")

	// Read/write primary + read-only SECONDARY is accepted by the daemon.
	sb := mkSandbox(t, b, CreateSpec{Name: "it-ro-secondary",
		Workspaces: []WorkspaceMount{{Name: "a"}, {Name: "b", ReadOnly: true}}})
	require.Equal(t, "it-ro-secondary", sb.Name)
}
