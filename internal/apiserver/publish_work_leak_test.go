package apiserver

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/squall-chua/sbx-swarm-node/internal/audit"
	"github.com/squall-chua/sbx-swarm-node/internal/events"
	sbxv1 "github.com/squall-chua/sbx-swarm-node/internal/gen/sbxswarm/v1"
	"github.com/squall-chua/sbx-swarm-node/internal/git"
	"github.com/squall-chua/sbx-swarm-node/internal/sandbox"
	"github.com/squall-chua/sbx-swarm-node/internal/store"
	"github.com/stretchr/testify/require"
)

// capturePub is an events.Publisher that records every call instead of
// broadcasting, so the leak test can inspect emitted payloads directly.
type capturePub struct {
	mu    sync.Mutex
	calls []string
}

func (c *capturePub) Publish(t, id string, payload any) events.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, fmt.Sprintf("%s %s %v", t, id, payload))
	return events.Event{}
}

// credLeakFixture mirrors gitPublishFixture but registers the workspace with a
// SENTINEL credential (token + CA path) and a RemoteURL, so credEnv genuinely
// injects the token into the git child env during branch/patch publish — the
// exact leak surface this test must prove is never exposed outward.
func credLeakFixture(t *testing.T, tok string) (svc *SandboxService, rec *sandbox.Record, al *audit.Log, cp *capturePub) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	upstream := filepath.Join(root, "up.git")
	const name = "repo"
	base := filepath.Join(root, "base", name)
	sbx := filepath.Join(root, "srv", name) // the agent's clone
	for _, c := range [][]string{
		{"git", "init", "--bare", upstream},
		{"git", "clone", upstream, sbx},
	} {
		out, err := exec.Command(c[0], c[1:]...).CombinedOutput()
		require.NoError(t, err, string(out))
	}
	run := func(dir string, a ...string) {
		c := exec.Command("git", a...)
		c.Dir = dir
		out, err := c.CombinedOutput()
		require.NoError(t, err, string(out))
	}
	run(sbx, "-c", "user.email=a@b.c", "-c", "user.name=t", "commit", "--allow-empty", "-m", "init")
	run(sbx, "push", "origin", "HEAD:main")
	out, err := exec.Command("git", "clone", "--bare", upstream, base).CombinedOutput()
	require.NoError(t, err, string(out))
	run(sbx, "checkout", "-b", "agent/x")
	run(sbx, "-c", "user.email=a@b.c", "-c", "user.name=t", "commit", "--allow-empty", "-m", "work")

	ws := git.New(git.Spec{
		Name: name, Base: base, Remote: "origin", DefaultBranch: "main", AllowPush: true,
		RemoteURL: upstream, // credEnv builds the extraheader for this url
		Cred:      git.Credential{Token: tok, CAPath: "/SENTINEL-CA-PATH.pem"},
		PublishSteps: [][]string{
			{"git", "fetch", "{sandbox_remote}", "+refs/heads/{branch}:refs/heads/{branch}"},
			{"git", "push", "{remote}", "{branch}"},
		},
		Allowlist: []string{"git"},
	})

	mgr := newTestManager(t)
	fake := mgr.Backend().(*sandbox.Fake)
	fake.ExecFunc = func(_ string, cmd []string) (sandbox.ExecResult, error) {
		if len(cmd) == 0 || cmd[0] != "git" {
			return sandbox.ExecResult{ExitCode: 0}, nil // e.g. `rm -f` cleanup
		}
		out, err := exec.Command("git", append([]string{"-C", sbx}, cmd[1:]...)...).Output()
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				return sandbox.ExecResult{ExitCode: ee.ExitCode(), Stderr: ee.Stderr}, nil
			}
			return sandbox.ExecResult{ExitCode: 1, Stderr: []byte(err.Error())}, nil
		}
		return sandbox.ExecResult{ExitCode: 0, Stdout: out}, nil
	}
	fake.CopyFromFunc = func(_, remote, local string) error { return copyFile(remote, local) }
	rec, err = mgr.AdmitAndCreate(context.Background(), sandbox.CreateSpec{
		Agent: "shell", Clone: true, Branch: "agent/x", Workspaces: []sandbox.WorkspaceMount{{Name: name}},
	})
	require.NoError(t, err)

	ast, err := store.Open(filepath.Join(root, "audit.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = ast.Close() })
	al = audit.New(ast, func() int64 { return 1 })

	cp = &capturePub{}
	svc = NewSandboxService(mgr, newTestOps(t))
	svc.SetGit(map[string]*git.Workspace{name: ws})
	svc.SetAudit(al)
	svc.SetEvents(cp)
	svc.bundleDir = t.TempDir()
	return svc, rec, al, cp
}

// copyFile is defined in sandboxservice_test.go (same package).

// TestPublishWork_NoCredentialLeak is the security bar for this feature: it runs
// the full PublishWork path (branch success + patch success + one forced
// failure) against a workspace configured with sentinel credential values, and
// asserts the sentinel token appears in none of the outward surfaces —
// PublishResult, the error string, slog logs, emitted events, audit records, or
// the persisted sandbox record. If the token leaks into any of these, that is a
// real bug and the assertion must not be weakened.
func TestPublishWork_NoCredentialLeak(t *testing.T) {
	const tok = "SENTINEL-TOKEN-9f3a2b"
	svc, rec, al, cp := credLeakFixture(t, tok)
	svc.publishTimeout = 10 * time.Second

	var logs bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	ctx := context.Background()

	rb, err := svc.PublishWork(ctx, &sbxv1.PublishWorkRequest{Id: rec.ID, Strategy: "branch", Target: "x"})
	require.NoError(t, err)
	rp, err := svc.PublishWork(ctx, &sbxv1.PublishWorkRequest{Id: rec.ID, Strategy: "patch", Target: "main"})
	require.NoError(t, err)
	// forced failure: an invalid ref path segment makes the underlying git push
	// fail, surfacing a git error string that must not carry the credential.
	_, ferr := svc.PublishWork(ctx, &sbxv1.PublishWorkRequest{Id: rec.ID, Strategy: "branch", Target: "refs/heads/../evil"})
	require.Error(t, ferr)

	auditList, err := al.List()
	require.NoError(t, err)

	mustGet, err := svc.mgr.Get(ctx, rec.ID)
	require.NoError(t, err)

	surfaces := map[string]string{
		"PublishResult(branch)": rb.String(),
		"PublishResult(patch)":  rp.String(),
		"Patch bytes":           string(rp.Patch),
		"error string":          ferr.Error(),
		"slog logs":             logs.String(),
		"captured events":       fmt.Sprint(cp.calls),
		"audit entries":         fmt.Sprint(auditList),
		"persisted record":      fmt.Sprint(mustGet),
	}
	for name, sfc := range surfaces {
		require.NotContains(t, sfc, tok, "credential leaked into outward surface: %s", name)
	}
}
