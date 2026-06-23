package apiserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/squall-chua/sbx-swarm-node/internal/sandbox"
	"github.com/stretchr/testify/require"
)

func TestTerminalHandler_EchoesOverWebSocket(t *testing.T) {
	svc := newSandboxSvc(t) // Fake backend
	ctx := context.Background()
	// Create a sandbox directly via the manager so Resolve succeeds immediately.
	rec, err := svc.mgr.Create(ctx, sandbox.CreateSpec{})
	require.NoError(t, err)

	srv := httptest.NewServer(terminalMux(svc.TerminalHandler(), http.NotFoundHandler()))
	t.Cleanup(srv.Close)

	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/sandboxes/" + rec.ID + "/terminal"
	dialCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(dialCtx, url, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close(websocket.StatusNormalClosure, "") })

	require.NoError(t, c.Write(dialCtx, websocket.MessageBinary, []byte("hello")))
	typ, data, err := c.Read(dialCtx)
	require.NoError(t, err)
	require.Equal(t, websocket.MessageBinary, typ)
	require.Equal(t, "hello", string(data))
}
