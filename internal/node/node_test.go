package node

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/squall-chua/sbx-swarm-node/internal/config"
	"github.com/squall-chua/sbx-swarm-node/internal/obs"
	"github.com/stretchr/testify/require"
)

func TestNode_BootServeStop(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.ListenAddr = "127.0.0.1:0" // random free port

	n, err := New(cfg, obs.NewLogger("error", io.Discard), "test")
	require.NoError(t, err)

	require.NoError(t, n.Start())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = n.Stop(ctx)
	})

	require.NotEmpty(t, n.NodeID())

	resp, err := http.Get("http://" + n.Addr() + "/healthz")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	resp, err = http.Get("http://" + n.Addr() + "/readyz")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode) // Start sets ready
	resp.Body.Close()
}
