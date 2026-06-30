package node

import (
	"context"
	"crypto/ecdsa"
	"crypto/tls"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/squall-chua/sbx-swarm-node/internal/config"
	"github.com/squall-chua/sbx-swarm-node/internal/obs"
	"github.com/stretchr/testify/require"
)

// The node's main listener serves an Ed25519 identity cert (node-to-node
// pinning, ADR-0004) that browsers reject (ERR_SSL_VERSION_OR_CIPHER_MISMATCH).
// When console_addr is set, the node serves the SPA + REST on a SECOND listener
// with a browser-compatible ECDSA cert, leaving the pinned main port untouched.
func TestNode_ConsoleListener_BrowserCompatCert(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.ListenAddr = "127.0.0.1:0"
	cfg.ConsoleAddr = "127.0.0.1:0"
	cfg.APIKeys = []config.APIKey{{Key: "adm", Role: "admin"}}

	n, err := New(cfg, obs.NewLogger("error", io.Discard), "test")
	require.NoError(t, err)
	require.NoError(t, n.Start())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = n.Stop(ctx)
	})

	require.NotEmpty(t, n.ConsoleAddr())
	require.NotEqual(t, n.Addr(), n.ConsoleAddr(), "console must be a separate listener from the pinned main port")

	// Browser compatibility hinges on the cert key algorithm: ECDSA is accepted,
	// Ed25519 is not. Capture the console leaf cert and assert it is ECDSA.
	conn, err := tls.Dial("tcp", n.ConsoleAddr(), &tls.Config{InsecureSkipVerify: true})
	require.NoError(t, err)
	leaf := conn.ConnectionState().PeerCertificates[0]
	conn.Close()
	require.IsType(t, &ecdsa.PublicKey{}, leaf.PublicKey, "console cert must be ECDSA (browsers reject Ed25519 server certs)")

	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}

	// The embedded SPA is served at / on the console port.
	resp, err := client.Get("https://" + n.ConsoleAddr() + "/")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	// The authed REST surface works on the console port too (same handlers).
	req, _ := http.NewRequest(http.MethodGet, "https://"+n.ConsoleAddr()+"/v1/node", nil)
	req.Header.Set("Authorization", "Bearer adm")
	resp, err = client.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()
}

// With console_tls=false the console listener speaks plain HTTP (front it with a
// TLS proxy or keep it on a trusted network). The login cookie also drops its
// Secure flag so browsers will store it over cleartext.
func TestNode_ConsoleListener_PlainHTTP(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.ListenAddr = "127.0.0.1:0"
	cfg.ConsoleAddr = "127.0.0.1:0"
	cfg.ConsoleTLS = false
	cfg.APIKeys = []config.APIKey{{Key: "adm", Role: "admin"}}

	n, err := New(cfg, obs.NewLogger("error", io.Discard), "test")
	require.NoError(t, err)
	require.NoError(t, n.Start())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = n.Stop(ctx)
	})

	// The SPA is served over plain HTTP — no TLS handshake.
	resp, err := http.Get("http://" + n.ConsoleAddr() + "/")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	// Login over HTTP mints cookies without the Secure flag (else browsers drop them).
	req, _ := http.NewRequest(http.MethodPost, "http://"+n.ConsoleAddr()+"/v1/auth/session", nil)
	req.Header.Set("Authorization", "Bearer adm")
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
	for _, c := range resp.Cookies() {
		require.False(t, c.Secure, "cookie %q must not be Secure over plain HTTP", c.Name)
	}
	resp.Body.Close()
}
