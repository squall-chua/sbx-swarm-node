package node

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/squall-chua/sbx-swarm-node/internal/auth"
	"github.com/squall-chua/sbx-swarm-node/internal/config"
	"github.com/squall-chua/sbx-swarm-node/internal/coordinator"
	sbxv1 "github.com/squall-chua/sbx-swarm-node/internal/gen/sbxswarm/v1"
	"github.com/squall-chua/sbx-swarm-node/internal/obs"
	"github.com/squall-chua/sbx-swarm-node/internal/peer"
	"github.com/squall-chua/sbx-swarm-node/internal/routing"
	"github.com/stretchr/testify/require"
)

func TestNode_BootServeStop(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.ListenAddr = "127.0.0.1:0"
	cfg.APIKeys = []config.APIKey{{Key: "adm", Role: "admin"}}

	n, err := New(cfg, obs.NewLogger("error", io.Discard), "test")
	require.NoError(t, err)
	require.NoError(t, n.Start())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = n.Stop(ctx)
	})

	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}

	// health is unauthenticated
	resp, err := client.Get("https://" + n.Addr() + "/healthz")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	// /v1/node needs auth
	req, _ := http.NewRequest(http.MethodGet, "https://"+n.Addr()+"/v1/node", nil)
	req.Header.Set("Authorization", "Bearer adm")
	resp, err = client.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()
}

func TestNode_SSEEndpointAuthed(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.ListenAddr = "127.0.0.1:0"
	cfg.APIKeys = []config.APIKey{{Key: "adm", Role: "admin"}}

	n, err := New(cfg, obs.NewLogger("error", io.Discard), "test")
	require.NoError(t, err)
	require.NoError(t, n.Start())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = n.Stop(ctx)
	})

	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}

	// unauthenticated SSE -> 401
	resp, err := client.Get("https://" + n.Addr() + "/v1/events")
	require.NoError(t, err)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	resp.Body.Close()
}

func TestAttemptFor_DialFailureNacks(t *testing.T) {
	// A peer in the routing table whose pin is unknown makes pool.Conn fail-closed.
	// The attempt must NACK so the coordinator falls through to the next candidate,
	// rather than surfacing a hard error that aborts the whole placement.
	_, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	pool := peer.NewPool(
		peer.WithNodeKey("self", priv),
		peer.WithPinResolver(func(string) ([]byte, bool) { return nil, false }),
	)
	tbl := routing.NewTable("self")
	tbl.Upsert("peerB", "127.0.0.1:1", false, nil)

	attempt := attemptFor("self", &sbxv1.CreateSandboxRequest{Cpus: 1, MemoryBytes: 1},
		nil, tbl, pool, obs.NewLogger("error", io.Discard))
	_, err = attempt(context.Background(), "peerB")
	require.ErrorIs(t, err, coordinator.ErrNack)
}

func TestNode_SessionKeyIsSwarmWideWhenClustered(t *testing.T) {
	// Two nodes with the same cluster secret derive the same session signer, so a
	// token minted by one verifies on the other (cross-node sessions, ADR-0010).
	seedA := bytes.Repeat([]byte{1}, ed25519.SeedSize)
	seedB := bytes.Repeat([]byte{2}, ed25519.SeedSize)
	kA := auth.DeriveSessionKey("shared-secret", ed25519.NewKeyFromSeed(seedA).Seed())
	kB := auth.DeriveSessionKey("shared-secret", ed25519.NewKeyFromSeed(seedB).Seed())
	require.Equal(t, kA, kB)
}
