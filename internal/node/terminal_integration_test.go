//go:build integration

package node

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/squall-chua/sbx-swarm-node/internal/config"
	"github.com/squall-chua/sbx-swarm-node/internal/obs"
	"github.com/stretchr/testify/require"
)

// TestNode_Terminal_EchoesAllInput is the regression guard for the terminal echo
// bug: bridgeTerminal MUST be the sole reader of the exec session's stdout. The SDK
// stdout is an io.Pipe, so a second concurrent reader (the old sess.Wait() drain)
// stole every other byte — typing "echo HELLO" echoed back as "coHLO". This drives
// the real terminal WebSocket end-to-end and asserts the whole line echoes.
// Env-gated (needs a live, version-compatible sbx daemon); no sbx/docker in CI. Run:
//
//	go test -tags integration ./internal/node/ -run TestNode_Terminal_EchoesAllInput
func TestNode_Terminal_EchoesAllInput(t *testing.T) {
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

	c.do(http.MethodPost, "/v1/sandboxes", map[string]any{
		"agent": "shell", "cpus": 1, "memory_bytes": 1 << 30,
		"workspaces": []map[string]any{{"name": "ws"}},
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

	// Dial the real terminal WebSocket (admin bearer + matching Origin for same-origin).
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer adm")
	hdr.Set("Origin", "https://"+n.Addr())
	ws, _, err := websocket.Dial(ctx, "wss://"+n.Addr()+"/v1/sandboxes/"+id+"/terminal",
		&websocket.DialOptions{HTTPClient: c.http, HTTPHeader: hdr})
	require.NoError(t, err, "dial terminal websocket")
	defer ws.Close(websocket.StatusNormalClosure, "")

	_ = ws.Write(ctx, websocket.MessageText, []byte(`{"type":"resize","cols":80,"rows":24}`))

	var mu sync.Mutex
	var buf []byte
	go func() {
		for {
			typ, data, rerr := ws.Read(ctx)
			if rerr != nil {
				return
			}
			if typ == websocket.MessageBinary {
				mu.Lock()
				buf = append(buf, data...)
				mu.Unlock()
			}
		}
	}()

	time.Sleep(2 * time.Second) // let the shell print its first prompt
	for _, ch := range []byte("echo HELLO\r") {
		require.NoError(t, ws.Write(ctx, websocket.MessageBinary, []byte{ch}))
		time.Sleep(40 * time.Millisecond)
	}
	time.Sleep(time.Second)

	mu.Lock()
	got := string(buf)
	mu.Unlock()
	// The dual-reader bug echoed only every other char ("coHLO"); require the full line.
	require.Contains(t, got, "echo HELLO", "terminal must echo the full typed line; got %q", got)
}
