//go:build integration

package node

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/squall-chua/sbx-swarm-node/internal/config"
	"github.com/squall-chua/sbx-swarm-node/internal/obs"
	"github.com/stretchr/testify/require"
)

// TestNode_PerSandboxEgress_Enforced asserts that a per-sandbox egress allow set
// through the node's real path (PUT /v1/sandboxes/{swarmID}/policy) actually GATES
// the swarm-managed sandbox's egress — not merely that the rule is listed back.
//
// This closes the coverage gap in apiserver.TestPolicyService_PerSandboxScope,
// which only checks registration against a fake backend. Enforcement lives in the
// closed sbx daemon and cannot be unit-tested, so this is env-gated behind the
// `integration` build tag (no sbx/daemon/internet in CI -> red-by-default there).
//
// It reproduces the exact chain the Agency's credential injector uses: scope is
// the sandbox's swarm ID, decision "allow". The assertion is behavioural — a fresh
// request from inside the sandbox flips 403 (blocked) -> reachable when the scoped
// rule is present, and reverts to 403 when it is removed (causal proof the rule,
// not something ambient, is what permits egress). Least-privilege: never a
// node-global rule.
//
//	go test -tags integration ./internal/node/ -run TestNode_PerSandboxEgress_Enforced
func TestNode_PerSandboxEgress_Enforced(t *testing.T) {
	const egressHost = "example.com" // stable, returns 200 to `GET /` when reachable

	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.ListenAddr = "127.0.0.1:0"
	cfg.Backend = "sdk"
	cfg.APIKeys = []config.APIKey{{Key: "adm", Role: "admin"}}
	cfg.Workspaces = []config.WorkspaceConfig{{Name: "ws", HostPath: t.TempDir()}}

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

	// Create + poll to running.
	c.do(http.MethodPost, "/v1/sandboxes", map[string]any{
		"agent":        "shell",
		"cpus":         1,
		"memory_bytes": 1 << 30,
		"workspaces":   []map[string]any{{"name": "ws"}},
	}, nil)
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
	}, 90*time.Second, time.Second, "sandbox never reached running")
	t.Cleanup(func() { c.deleteAndWait(id) })

	// 1. Baseline: default policy blocks the host (sbx proxy answers 403).
	require.Equal(t, "403", c.curlCode(id, egressHost),
		"precondition: host must be blocked by default policy before the allow")

	// 2. Per-sandbox allow via the real node path; scope = swarm ID (NOT global).
	c.do(http.MethodPut, "/v1/sandboxes/"+id+"/policy",
		map[string]any{"decision": "allow", "host": egressHost}, nil)

	// 3. Enforcement: a fresh request now reaches the origin (any non-403 == the
	//    proxy let it through; example.com returns 200).
	require.Eventually(t, func() bool { return c.curlCode(id, egressHost) != "403" },
		15*time.Second, time.Second,
		"per-sandbox allow did not enable egress — the scoped rule registers but does not gate")

	// 4. Causal proof: removing the scoped rule reverts egress to blocked.
	c.do(http.MethodDelete, "/v1/sandboxes/"+id+"/policy/"+egressHost, nil, nil)
	require.Eventually(t, func() bool { return c.curlCode(id, egressHost) == "403" },
		15*time.Second, time.Second,
		"egress still permitted after removing the scoped allow — rule was not the cause")
}

// curlCode execs `curl` inside the sandbox against https://host and returns the
// HTTP status code as a string ("403" when the sbx egress proxy blocks it), or
// "FAIL" if curl could not complete (e.g. a blackholed connection timing out).
func (c *nodeClient) curlCode(id, host string) string {
	c.t.Helper()
	var res struct {
		Stdout []byte `json:"stdout"` // JSON base64 -> bytes
	}
	c.do(http.MethodPost, "/v1/sandboxes/"+id+"/exec", map[string]any{
		"cmd": []string{"sh", "-c",
			"curl -sS -m10 -o /dev/null -w '%{http_code}' https://" + host + " || echo FAIL"},
	}, &res)
	return strings.TrimSpace(string(res.Stdout))
}
