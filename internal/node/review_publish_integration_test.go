//go:build integration

package node

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/squall-chua/sbx-swarm-node/internal/config"
	"github.com/squall-chua/sbx-swarm-node/internal/obs"
	"github.com/stretchr/testify/require"
)

// TestNode_Gerrit_ReviewHeadRepublish drives the #23-b Sandbox path end to end:
// create a clone-mode sandbox with review_ref -> the node checks out the Change's
// current Patchset (branch review/<n>) -> commit a fix in the sandbox ->
// PublishWork("gerrit_change") -> the node REUSES the checked-out commit's
// Change-Id so a NEW Patchset lands on the SAME Change (no duplicate).
//
// Prereqs (see dev/gerrit/README.md) plus a Gerrit HTTP password and an existing
// open change on the demo project:
//
//	ssh -p 29418 -i dev/gerrit/id_gerrit admin@localhost \
//	    gerrit set-account --http-password <pw> admin
//	GERRIT_HTTP_PASSWORD=<pw> GERRIT_REVIEW_CHANGE=2 \
//	go test -tags integration ./internal/node/ -run TestNode_Gerrit_ReviewHeadRepublish -v
func TestNode_Gerrit_ReviewHeadRepublish(t *testing.T) {
	httpPW := os.Getenv("GERRIT_HTTP_PASSWORD")
	change := os.Getenv("GERRIT_REVIEW_CHANGE")
	if httpPW == "" || change == "" {
		t.Skip("set GERRIT_HTTP_PASSWORD and GERRIT_REVIEW_CHANGE (an open change number on the demo project)")
	}
	const apiBase = "http://localhost:8080"

	// The Change-Id we expect the re-push to reuse.
	wantChangeID := gerritChangeIDField(t, apiBase, httpPW, change)
	t.Logf("target change %s Change-Id=%s", change, wantChangeID)

	key, err := filepath.Abs("../../dev/gerrit/id_gerrit")
	require.NoError(t, err)
	knownHosts, err := filepath.Abs("../../dev/gerrit/known_hosts")
	require.NoError(t, err)
	t.Setenv("GERRIT_HTTP_PASSWORD", httpPW)

	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.ListenAddr = "127.0.0.1:0"
	cfg.Backend = "sdk"
	cfg.APIKeys = []config.APIKey{{Key: "adm", Role: "admin"}}
	cfg.Workspaces = []config.WorkspaceConfig{{
		Name: "gerrit-demo",
		Git: &config.GitConfig{
			Provider:          "gerrit",
			RemoteURL:         "ssh://admin@localhost:29418/demo",
			SSHKeyPath:        key,
			SSHKnownHostsPath: knownHosts,
			DefaultBranch:     "master",
			AllowPush:         true,
			APIBaseURL:        apiBase,             // Gerrit REST (review-head resolution)
			TokenEnv:          "GERRIT_HTTP_PASSWORD", // HTTP password for REST basic auth
		},
	}}

	n, err := New(cfg, obs.NewLogger("error", io.Discard), "test")
	require.NoError(t, err, "node.New with backend:sdk (needs a version-compatible sbx daemon)")
	require.NoError(t, n.Start())
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

	// Create a clone-mode sandbox on the Review head (no branch — review_ref wins).
	var op struct {
		SandboxID string `json:"sandbox_id"`
		Error     string `json:"error"`
	}
	c.do(http.MethodPost, "/v1/sandboxes", map[string]any{
		"agent":        "shell",
		"cpus":         1,
		"memory_bytes": 1 << 30,
		"clone":        true,
		"workspaces":   []map[string]any{{"name": "gerrit-demo"}},
		"review_ref":   map[string]any{"workspace": "gerrit-demo", "id": change},
	}, &op)

	var id, branch string
	require.Eventually(t, func() bool {
		var list struct {
			Sandboxes []struct {
				ID     string `json:"id"`
				Status string `json:"status"`
				Branch string `json:"branch"`
			} `json:"sandboxes"`
		}
		c.do(http.MethodGet, "/v1/sandboxes", nil, &list)
		for _, s := range list.Sandboxes {
			if s.Status == "running" {
				id, branch = s.ID, s.Branch
				return true
			}
		}
		return false
	}, 90*time.Second, time.Second, "sandbox never reached running (review-head checkout may have failed)")
	t.Cleanup(func() { c.deleteAndWait(id) })

	require.Equal(t, "review/"+change, branch, "sandbox checked out the Review head branch")

	// Check out the Review head branch (present in the clone since the base fetched
	// it) and commit a fix on it, so the publish source is the patchset branch.
	var ex struct {
		ExitCode int    `json:"exit_code"`
		Stdout   []byte `json:"stdout"`
		Stderr   []byte `json:"stderr"`
	}
	c.do(http.MethodPost, "/v1/sandboxes/"+id+"/exec", map[string]any{
		"cmd": []string{"sh", "-c",
			"set -ex; git checkout review/" + change + "; echo 'clarified' >> NOTES.md; git add -A; " +
				"git -c user.email=dev@example.com -c user.name=dev commit -m 'address review'"},
	}, &ex)
	require.Equalf(t, 0, ex.ExitCode, "fix commit failed: stdout=%s stderr=%s", ex.Stdout, ex.Stderr)

	// Re-publish: a new Patchset on the SAME Change (reused Change-Id).
	var res struct {
		Ref      string `json:"ref"`
		ChangeID string `json:"change_id"`
		NoChange bool   `json:"no_change"`
	}
	c.do(http.MethodPost, "/v1/sandboxes/"+id+"/git/publish-work", map[string]any{
		"strategy": "gerrit_change",
		"target":   "master",
	}, &res)
	require.Equal(t, "refs/for/master", res.Ref)
	require.Equal(t, wantChangeID, res.ChangeID, "re-push must reuse the Change-Id (new patchset, not a duplicate change)")
	require.False(t, res.NoChange, "a real fix commit is a change")

	// Exactly one change carries that Change-Id — no duplicate was created.
	require.Equal(t, 1, gerritChangeCount(t, apiBase, httpPW, wantChangeID), "no duplicate change")
	t.Logf("re-published patchset on change %s (Change-Id %s)", change, wantChangeID)
}

// TestNode_GitHub_ReviewHeadPush drives the #23-b Sandbox path for a forge PR:
// create a clone-mode sandbox with review_ref -> the node checks out the PR head
// branch -> commit a fix -> PublishWork("branch") pushes it to the PR head branch
// IN PLACE (new commits on the same PR, no new PR).
//
//	GITHUB_TOKEN=$(gh auth token) GH_REVIEW_REPO=squall-chua/test GH_REVIEW_PR=2 \
//	go test -tags integration ./internal/node/ -run TestNode_GitHub_ReviewHeadPush -v
func TestNode_GitHub_ReviewHeadPush(t *testing.T) {
	token := os.Getenv("GITHUB_TOKEN")
	repo := os.Getenv("GH_REVIEW_REPO")
	pr := os.Getenv("GH_REVIEW_PR")
	if token == "" || repo == "" || pr == "" {
		t.Skip("set GITHUB_TOKEN, GH_REVIEW_REPO, GH_REVIEW_PR")
	}
	headBranch := githubPRHeadBranch(t, token, repo, pr)
	beforeSHA := githubBranchSHA(t, token, repo, headBranch)
	t.Logf("PR %s head branch=%s sha=%s", pr, headBranch, beforeSHA)
	t.Setenv("GITHUB_TOKEN", token)

	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.ListenAddr = "127.0.0.1:0"
	cfg.Backend = "sdk"
	cfg.APIKeys = []config.APIKey{{Key: "adm", Role: "admin"}}
	cfg.Workspaces = []config.WorkspaceConfig{{
		Name: "gh",
		Git: &config.GitConfig{
			Provider:      "github",
			RemoteURL:     "https://github.com/" + repo + ".git",
			TokenEnv:      "GITHUB_TOKEN",
			DefaultBranch: "main",
			AllowPush:     true,
		},
	}}

	n, err := New(cfg, obs.NewLogger("error", io.Discard), "test")
	require.NoError(t, err, "node.New with backend:sdk (needs a version-compatible sbx daemon)")
	require.NoError(t, n.Start())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = n.Stop(ctx)
	})
	c := &nodeClient{t: t, base: "https://" + n.Addr(),
		http: &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}}

	var op struct{ SandboxID string `json:"sandbox_id"` }
	c.do(http.MethodPost, "/v1/sandboxes", map[string]any{
		"agent": "shell", "cpus": 1, "memory_bytes": 1 << 30, "clone": true,
		"workspaces": []map[string]any{{"name": "gh"}},
		"review_ref": map[string]any{"workspace": "gh", "id": pr},
	}, &op)

	var id, branch string
	require.Eventually(t, func() bool {
		var list struct {
			Sandboxes []struct{ ID, Status, Branch string }
		}
		c.do(http.MethodGet, "/v1/sandboxes", nil, &list)
		for _, s := range list.Sandboxes {
			if s.Status == "running" {
				id, branch = s.ID, s.Branch
				return true
			}
		}
		return false
	}, 90*time.Second, time.Second, "sandbox never reached running")
	t.Cleanup(func() { c.deleteAndWait(id) })
	require.Equal(t, headBranch, branch, "sandbox checked out the PR head branch")

	var ex struct {
		ExitCode int    `json:"exit_code"`
		Stdout   []byte `json:"stdout"`
		Stderr   []byte `json:"stderr"`
	}
	c.do(http.MethodPost, "/v1/sandboxes/"+id+"/exec", map[string]any{
		"cmd": []string{"sh", "-c",
			"set -ex; git checkout " + headBranch + "; echo '# addressed review' >> NOTES.md; git add -A; " +
				"git -c user.email=dev@example.com -c user.name=dev commit -m 'address review'"},
	}, &ex)
	require.Equalf(t, 0, ex.ExitCode, "fix commit failed: stdout=%s stderr=%s", ex.Stdout, ex.Stderr)

	var res struct {
		Ref      string `json:"ref"`
		NoChange bool   `json:"no_change"`
	}
	c.do(http.MethodPost, "/v1/sandboxes/"+id+"/git/publish-work", map[string]any{
		"strategy": "branch", "target": headBranch,
	}, &res)
	require.Equal(t, "refs/heads/"+headBranch, res.Ref)
	require.False(t, res.NoChange, "a real fix commit advances the branch")

	// Verify against the branch ref, which updates immediately (the PR object's
	// head.sha is eventually consistent and lags the push).
	afterSHA := githubBranchSHA(t, token, repo, headBranch)
	require.NotEqual(t, beforeSHA, afterSHA, "PR head branch advanced in place (new commits, same PR)")
	t.Logf("PR %s head branch %s advanced %s -> %s (in place, no new PR)", pr, headBranch, beforeSHA, afterSHA)

	// Re-publish with no new commit: the branch is already up to date, so the
	// node reports no_change=true (the #23-d seam the Agency uses to skip the reply).
	var res2 struct {
		NoChange bool `json:"no_change"`
	}
	c.do(http.MethodPost, "/v1/sandboxes/"+id+"/git/publish-work", map[string]any{
		"strategy": "branch", "target": headBranch,
	}, &res2)
	require.True(t, res2.NoChange, "idempotent re-publish with no new commit reports no_change")
	require.Equal(t, afterSHA, githubBranchSHA(t, token, repo, headBranch), "no-op re-publish left the branch unchanged")
}

func githubGet(t *testing.T, token, url string, out any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	require.Equalf(t, 200, resp.StatusCode, "GET %s: %s", url, raw)
	require.NoError(t, json.Unmarshal(raw, out))
}

func githubPRHeadBranch(t *testing.T, token, repo, pr string) string {
	var p struct {
		Head struct {
			Ref string `json:"ref"`
		} `json:"head"`
	}
	githubGet(t, token, "https://api.github.com/repos/"+repo+"/pulls/"+pr, &p)
	require.NotEmpty(t, p.Head.Ref)
	return p.Head.Ref
}

func githubBranchSHA(t *testing.T, token, repo, branch string) string {
	var r struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	githubGet(t, token, "https://api.github.com/repos/"+repo+"/git/ref/heads/"+branch, &r)
	return r.Object.SHA
}

// gerritGet issues an XSSI-stripped authenticated GET and decodes into out.
func gerritGet(t *testing.T, apiBase, pw, path string, out any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, apiBase+"/a"+path, nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("admin:"+pw)))
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	require.Truef(t, resp.StatusCode == 200, "gerrit GET %s -> %d: %s", path, resp.StatusCode, raw)
	if i := bytes.IndexByte(raw, '\n'); bytes.HasPrefix(raw, []byte(")]}'")) && i >= 0 {
		raw = raw[i+1:] // strip Gerrit's XSSI prefix
	}
	require.NoError(t, json.Unmarshal(raw, out))
}

func gerritChangeIDField(t *testing.T, apiBase, pw, change string) string {
	var ch struct {
		ChangeID string `json:"change_id"`
	}
	gerritGet(t, apiBase, pw, "/changes/"+change, &ch)
	require.NotEmpty(t, ch.ChangeID)
	return ch.ChangeID
}

func gerritChangeCount(t *testing.T, apiBase, pw, changeID string) int {
	var changes []struct {
		ChangeID string `json:"change_id"`
	}
	gerritGet(t, apiBase, pw, "/changes/?q=change:"+changeID, &changes)
	return len(changes)
}
