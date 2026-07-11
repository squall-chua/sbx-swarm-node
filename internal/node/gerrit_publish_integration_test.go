//go:build integration

package node

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/squall-chua/sbx-swarm-node/internal/config"
	"github.com/squall-chua/sbx-swarm-node/internal/obs"
	"github.com/stretchr/testify/require"
)

// TestNode_Gerrit_Publish drives the WHOLE node with backend:sdk through a real
// gerrit_change publish: boot -> create a clone-mode sandbox on a git-backed
// gerrit workspace -> commit inside the sandbox -> POST git/publish-work -> assert
// the node returned a Gerrit change (refs/for/master + a real Change-Id + change URL).
//
// Prereqs (see dev/gerrit/README.md): a Gerrit at ssh://admin@localhost:29418 with
// a "demo" project (master has a commit) and dev/gerrit/id_gerrit registered, plus
// a live sbx daemon. Env-gated behind `integration` — red-by-default in CI. Run:
//
//	go test -tags integration ./internal/node/ -run TestNode_Gerrit_Publish -v
func TestNode_Gerrit_Publish(t *testing.T) {
	key, err := filepath.Abs("../../dev/gerrit/id_gerrit")
	require.NoError(t, err)
	knownHosts, err := filepath.Abs("../../dev/gerrit/known_hosts")
	require.NoError(t, err)

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

	// Create a clone-mode sandbox on the git-backed gerrit workspace, branch "feature".
	var op struct {
		SandboxID string `json:"sandbox_id"`
		Error     string `json:"error"`
	}
	c.do(http.MethodPost, "/v1/sandboxes", map[string]any{
		"agent":        "shell",
		"cpus":         1,
		"memory_bytes": 1 << 30,
		"clone":        true,
		"branch":       "feature",
		"workspaces":   []map[string]any{{"name": "gerrit-demo"}},
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
	}, 90*time.Second, time.Second, "sandbox never reached running (create op may have failed)")
	t.Cleanup(func() { c.deleteAndWait(id) })

	// Do work inside the sandbox: switch to the branch and commit a file. The publish
	// source is the sandbox's own HEAD, so this branch/commit is what gets published.
	var ex struct {
		ExitCode int    `json:"exit_code"`
		Stdout   []byte `json:"stdout"`
	}
	c.do(http.MethodPost, "/v1/sandboxes/"+id+"/exec", map[string]any{
		"cmd": []string{"sh", "-c",
			"git checkout -b feature && echo 'from sbx-swarm' > NOTES.md && git add NOTES.md && " +
				"git -c user.email=dev@example.com -c user.name=dev commit -m 'add notes'"},
	}, &ex)
	require.Equalf(t, 0, ex.ExitCode, "commit failed: %s", ex.Stdout)

	// Publish via gerrit_change against master. Node derives the Change-Id and pushes
	// to refs/for/master; the response carries the change URL.
	var res struct {
		Ref         string `json:"ref"`
		DeliveryURL string `json:"delivery_url"`
		ChangeID    string `json:"change_id"`
	}
	c.do(http.MethodPost, "/v1/sandboxes/"+id+"/git/publish-work", map[string]any{
		"strategy": "gerrit_change",
		"target":   "master",
		"title":    "sbx-swarm gerrit_change smoke",
	}, &res)

	require.Equal(t, "refs/for/master", res.Ref)
	require.Regexp(t, `^I[0-9a-f]{40}$`, res.ChangeID, "node must derive a valid Gerrit Change-Id")
	require.Contains(t, res.DeliveryURL, "/c/demo/+/", "delivery URL should point at the Gerrit change")
	t.Logf("published Gerrit change: %s (Change-Id %s)", res.DeliveryURL, res.ChangeID)
}
