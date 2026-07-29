//go:build integration

package node

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/squall-chua/sbx-swarm-node/internal/config"
	"github.com/squall-chua/sbx-swarm-node/internal/obs"
	"github.com/stretchr/testify/require"
)

// gitLocal runs a git command in dir and fails the test on error. Used to build
// and inspect the local upstream/base fixtures — no network, no provider.
func gitLocal(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "git %v: %s", args, out)
	return string(out)
}

// TestNode_DrainPublishesGitBackedSandbox is the "Drain publishes before it
// stops" check the spec left manual. Both existing drain tests (apiserver unit
// tests) run without a git workspace, so maybeAutoPublish returns early at its
// s.gitWS == nil guard and never actually exercises a publish — this is the
// only test that does.
//
// It builds a real, local, provider-free git-backed workspace (an upstream
// bare repo + a node-managed base that already has "origin" pointed at it,
// ADR-0014's legacy/operator-prepared mode — no GitHub/Gerrit credentials
// needed), creates a real clone-mode sandbox against the live sbx daemon,
// commits inside it, then Drains the node and asserts BOTH that the sandbox
// stopped AND that the commit actually reached the upstream repo — the git
// state a publish produces, not just the absence of a logged error.
func TestNode_DrainPublishesGitBackedSandbox(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}

	root := t.TempDir()
	upstream := filepath.Join(root, "upstream.git")
	work := filepath.Join(root, "work")
	base := filepath.Join(root, "base")

	gitLocal(t, root, "init", "--bare", upstream)
	gitLocal(t, root, "clone", upstream, work)
	gitLocal(t, work, "-c", "user.email=t@t.com", "-c", "user.name=t", "commit", "--allow-empty", "-m", "init")
	gitLocal(t, work, "push", "origin", "HEAD:main")
	gitLocal(t, root, "-C", upstream, "symbolic-ref", "HEAD", "refs/heads/main") // like a real remote: default branch = main
	gitLocal(t, root, "clone", upstream, base)                                   // node-managed base: non-bare, "origin" -> upstream
	// Detach HEAD, same as EnsureBase does for a node-managed base (ADR-0020): the
	// PRE pipeline's wildcard fetch (+refs/heads/*:refs/heads/*) touches
	// refs/heads/main, which git refuses to update while it is checked out.
	gitLocal(t, base, "checkout", "--detach")

	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.ListenAddr = "127.0.0.1:0"
	cfg.Backend = "sdk"
	cfg.APIKeys = []config.APIKey{{Key: "adm", Role: "admin"}}
	cfg.Workspaces = []config.WorkspaceConfig{{
		Name:     "ws",
		HostPath: base,
		Git: &config.GitConfig{
			AllowPush:     true,
			DefaultBranch: "main",
		},
	}}

	n, err := New(cfg, obs.NewLogger("error", io.Discard), "test")
	if err != nil {
		t.Skipf("skipping: could not build a backend:sdk node (daemon unavailable?): %v", err)
	}
	if err := n.Start(); err != nil {
		t.Skipf("skipping: could not start a backend:sdk node (daemon unavailable?): %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = n.Stop(ctx)
	})

	c := &nodeClient{
		t:    t,
		base: "https://" + n.Addr(),
		http: &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}},
	}

	// Create a clone-mode sandbox on the "ws" workspace, recording "agent/work" as
	// the auto-publish target branch (maybeAutoPublish only needs this non-empty;
	// it does not steer what gets checked out). The clone starts on base's
	// detached HEAD; we check out agent/work ourselves below, same as a real
	// agent would.
	var op struct {
		SandboxID string `json:"sandbox_id"`
		Error     string `json:"error"`
	}
	c.do(http.MethodPost, "/v1/sandboxes", map[string]any{
		"agent":        "shell",
		"cpus":         1,
		"memory_bytes": 1 << 30,
		"clone":        true,
		"workspaces":   []map[string]any{{"name": "ws"}},
		"branch":       "agent/work",
	}, &op)

	var id string
	require.Eventually(t, func() bool {
		var list struct {
			Sandboxes []struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			} `json:"sandboxes"`
		}
		c.do(http.MethodGet, "/v1/sandboxes", nil, &list)
		for _, s := range list.Sandboxes {
			if s.Status == "running" {
				id = s.ID
				return true
			}
		}
		return false
	}, 90*time.Second, time.Second, "sandbox never reached running (clone may have failed)")
	// Register cleanup before asserting anything else so a real sandbox is never
	// leaked on failure. Drain already stops it; delete is still needed to free it.
	t.Cleanup(func() { c.deleteAndWait(id) })

	// Commit inside the sandbox on a new branch, mirroring what a real agent does
	// before the node auto-publishes on stop.
	var ex struct {
		ExitCode int    `json:"exit_code"`
		Stdout   []byte `json:"stdout"`
		Stderr   []byte `json:"stderr"`
	}
	c.do(http.MethodPost, "/v1/sandboxes/"+id+"/exec", map[string]any{
		"cmd": []string{"sh", "-c",
			"set -ex; git checkout -b agent/work; echo 'drain-publish-check' > NOTES.md; git add -A; " +
				"git -c user.email=dev@example.com -c user.name=dev commit -m 'drain test commit'"},
	}, &ex)
	require.Equalf(t, 0, ex.ExitCode, "commit failed: stdout=%s stderr=%s", ex.Stdout, ex.Stderr)

	// Drain: cordons the node and starts the publish-then-stop sweep in the
	// background.
	var nodeInfo struct {
		Cordoned bool `json:"cordoned"`
		Draining bool `json:"draining"`
	}
	c.do(http.MethodPost, "/v1/node/drain", nil, &nodeInfo)
	require.True(t, nodeInfo.Cordoned)
	require.True(t, nodeInfo.Draining)

	// Wait for the sweep to finish processing our sandbox: maybeAutoPublish runs
	// BEFORE Stop in the same DrainAll iteration, so once the sandbox reports
	// stopped, the publish attempt for it has already completed (success or
	// failure — that is why the upstream check below is load-bearing, not the
	// "stopped" status alone).
	require.Eventually(t, func() bool {
		var got struct {
			Status string `json:"status"`
		}
		c.do(http.MethodGet, "/v1/sandboxes/"+id, nil, &got)
		return got.Status == "stopped"
	}, 90*time.Second, time.Second, "drain sweep never stopped the sandbox")

	// The publish half: the commit must actually have reached the upstream repo
	// on branch agent/work, with the content we wrote.
	out := gitLocal(t, upstream, "rev-parse", "--verify", "refs/heads/agent/work")
	require.NotEmpty(t, out, "drain must have published agent/work to upstream")

	show := gitLocal(t, upstream, "show", "refs/heads/agent/work:NOTES.md")
	require.Contains(t, show, "drain-publish-check", "the published branch must carry the commit made inside the sandbox")
}
