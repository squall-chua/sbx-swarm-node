package apiserver

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
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
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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
		// Nothing on the PublishWork path logs via slog today; this is a forward
		// regression-guard that catches a future addition that logs the credential.
		"slog logs":        logs.String(),
		"captured events":  fmt.Sprint(cp.calls),
		"audit entries":    fmt.Sprint(auditList),
		"persisted record": fmt.Sprint(mustGet),
	}
	// Assert BOTH the raw token and its wire form (the base64 Basic-auth header the
	// token is actually injected as) are absent — a surface that dumped the git env
	// value would leak the encoded form, not the raw token.
	b64 := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + tok))
	for name, sfc := range surfaces {
		require.NotContains(t, sfc, tok, "raw credential leaked into outward surface: %s", name)
		require.NotContains(t, sfc, b64, "base64 credential header leaked into outward surface: %s", name)
	}
}

// gitBaseOf returns the base dir of the single workspace registered on svc
// (field is s.gitWS), so the REST-leak test can re-register a github provider
// over the same base.
func gitBaseOf(t *testing.T, svc *SandboxService) string {
	t.Helper()
	for _, w := range svc.gitWS {
		return w.Base()
	}
	t.Fatal("no workspace registered")
	return ""
}

// TestPublishWork_PullRequest_NoCredentialLeak extends the credential-leak bar
// to a REST strategy: the token must be used to talk to the (fake) provider
// API and to push the head branch, but must never leak into the delivery
// result, the backend call log, or the audit list.
func TestPublishWork_PullRequest_NoCredentialLeak(t *testing.T) {
	const tok = "SENTINEL-PR-TOKEN-7c1d"
	// fake GitHub PR API (create then found).
	var created bool
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app/pulls", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			if created {
				_, _ = w.Write([]byte(`[{"number":1,"html_url":"https://github.com/acme/app/pull/1"}]`))
			} else {
				_, _ = w.Write([]byte(`[]`))
			}
			return
		}
		created = true
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"number":1,"html_url":"https://github.com/acme/app/pull/1"}`))
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(caPath, caPEM, 0o600))

	svc, rec, al, cp := credLeakFixture(t, tok)
	svc.publishTimeout = 10 * time.Second
	// re-register the workspace as a github provider pointing at the fake.
	ws := git.New(git.Spec{
		Name: "repo", Base: gitBaseOf(t, svc), Remote: "origin", DefaultBranch: "main", AllowPush: true,
		Provider: "github", RemoteURL: "https://github.com/acme/app", APIBaseURL: srv.URL,
		Cred:      git.Credential{Token: tok, CAPath: caPath},
		Allowlist: []string{"git"},
	})
	svc.SetGit(map[string]*git.Workspace{"repo": ws})

	res, err := svc.PublishWork(context.Background(), &sbxv1.PublishWorkRequest{Id: rec.ID, Strategy: "pull_request", Target: "main", Title: "t"})
	require.NoError(t, err)
	require.Equal(t, "https://github.com/acme/app/pull/1", res.DeliveryUrl)

	auditList, err := al.List()
	require.NoError(t, err)
	b64 := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + tok))
	surfaces := []string{res.String(), fmt.Sprint(cp.calls), fmt.Sprint(auditList)}
	for _, s := range surfaces {
		require.NotContains(t, s, tok)
		require.NotContains(t, s, b64)
		require.NotContains(t, s, "Bearer "+tok)
	}
}

// TestPublishWork_PullRequest_PropagatesForgeStatus proves the handler no longer
// flattens every strategy error to codes.Internal: a forge rejecting the token
// (GitHub 403 on the PR-list GET) must surface as codes.PermissionDenied to the
// gRPC client, not a generic Internal, while still not leaking the credential.
func TestPublishWork_PullRequest_PropagatesForgeStatus(t *testing.T) {
	const tok = "SENTINEL-PR-TOKEN-403"
	// fake GitHub: PR list GET is forbidden (token lacks repo scope).
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app/pulls", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"Resource not accessible by personal access token"}`))
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(caPath, caPEM, 0o600))

	svc, rec, _, _ := credLeakFixture(t, tok)
	svc.publishTimeout = 10 * time.Second
	// re-register the workspace as a github provider pointing at the fake.
	ws := git.New(git.Spec{
		Name: "repo", Base: gitBaseOf(t, svc), Remote: "origin", DefaultBranch: "main", AllowPush: true,
		Provider: "github", RemoteURL: "https://github.com/acme/app", APIBaseURL: srv.URL,
		Cred:      git.Credential{Token: tok, CAPath: caPath},
		Allowlist: []string{"git"},
	})
	svc.SetGit(map[string]*git.Workspace{"repo": ws})

	_, err := svc.PublishWork(context.Background(), &sbxv1.PublishWorkRequest{Id: rec.ID, Strategy: "pull_request", Target: "main", Title: "t"})
	require.Error(t, err)
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	require.NotContains(t, err.Error(), tok)
}
