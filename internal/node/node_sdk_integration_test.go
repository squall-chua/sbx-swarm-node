//go:build integration

package node

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/squall-chua/sbx-swarm-node/internal/config"
	"github.com/squall-chua/sbx-swarm-node/internal/obs"
	"github.com/stretchr/testify/require"
)

// TestNode_SDKBackend_CreateExecStop drives the WHOLE node with backend:sdk against
// a live sbx daemon over the real REST API: boot -> POST /v1/sandboxes -> poll the
// list to "running" -> exec -> stop -> delete. The SDKBackend adapter is already
// covered directly (internal/sandbox/sdkbackend_integration_test.go); this is the
// only test that exercises the full REST -> gateway -> loopback-gRPC -> coordinator
// -> manager -> SDKBackend -> daemon wiring that production backend:sdk uses.
//
// Daemon contract (sbx v0.32.0): create needs an agent ("shell" = no AI agent) and
// at least one workspace; the box must have a default network policy (it does:
// `sbx policy ls` shows default-ai-services). Env-gated behind the `integration`
// build tag — no sbx/docker in CI, so red-by-default there. Run:
//   go test -tags integration ./internal/node/ -run TestNode_SDKBackend_CreateExecStop
func TestNode_SDKBackend_CreateExecStop(t *testing.T) {
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

	// Create (async op).
	var op struct {
		SandboxID string `json:"sandbox_id"`
		Error     string `json:"error"`
	}
	c.do(http.MethodPost, "/v1/sandboxes", map[string]any{
		"agent":        "shell",
		"cpus":         1,
		"memory_bytes": 1 << 30,
		"workspaces":   []map[string]any{{"name": "ws"}},
	}, &op)

	// Poll the list to running; capture the id. Register deletion before asserting
	// anything else so a real sandbox is never leaked on failure.
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
	// Delete is async (Operation) with no poll endpoint; wait for the sandbox to
	// leave the list so the daemon actually frees it before node.Stop tears the
	// backend down. Best-effort: never fail an otherwise-passing test on teardown.
	t.Cleanup(func() { c.deleteAndWait(id) })

	// Exec: stdout must round-trip through the full chain.
	var ex struct {
		ExitCode int    `json:"exit_code"`
		Stdout   []byte `json:"stdout"`
	}
	c.do(http.MethodPost, "/v1/sandboxes/"+id+"/exec", map[string]any{"cmd": []string{"echo", "smoke-ok"}}, &ex)
	require.Equal(t, 0, ex.ExitCode)
	require.Contains(t, string(ex.Stdout), "smoke-ok")

	// Stop.
	var stopped struct {
		Status string `json:"status"`
	}
	c.do(http.MethodPost, "/v1/sandboxes/"+id+"/stop", nil, &stopped)
	require.Equal(t, "stopped", stopped.Status)
}

// deleteAndWait issues an async DELETE and polls the list until the sandbox is
// gone (or a short deadline passes). Best-effort: logs rather than failing.
func (c *nodeClient) deleteAndWait(id string) {
	c.do(http.MethodDelete, "/v1/sandboxes/"+id, nil, nil)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var list struct {
			Sandboxes []struct {
				ID string `json:"id"`
			} `json:"sandboxes"`
		}
		c.do(http.MethodGet, "/v1/sandboxes", nil, &list)
		found := false
		for _, s := range list.Sandboxes {
			if s.ID == id {
				found = true
			}
		}
		if !found {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	c.t.Logf("cleanup: sandbox %s still present after delete; remove manually with `sbx rm --force %s`", id, id)
}

// nodeClient is a tiny admin-authed JSON REST client for the node under test.
type nodeClient struct {
	t    *testing.T
	base string
	http *http.Client
}

// do issues req with the admin bearer; on 2xx it decodes the body into out (nil to
// ignore). Any transport error or non-2xx fails the test.
func (c *nodeClient) do(method, path string, body, out any) {
	c.t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(c.t, err)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, rdr)
	require.NoError(c.t, err)
	req.Header.Set("Authorization", "Bearer adm")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	require.NoError(c.t, err)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	require.Truef(c.t, resp.StatusCode >= 200 && resp.StatusCode < 300, "%s %s -> %d: %s", method, path, resp.StatusCode, raw)
	if out != nil {
		require.NoErrorf(c.t, json.Unmarshal(raw, out), "decode %s %s body: %s", method, path, raw)
	}
}
