package apiserver

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/squall-chua/sbx-swarm-node/internal/ids"
	"github.com/squall-chua/sbx-swarm-node/internal/ops"
	"github.com/squall-chua/sbx-swarm-node/internal/sandbox"
	"github.com/squall-chua/sbx-swarm-node/internal/store"
	"github.com/stretchr/testify/require"
)

func startRoleGateServer(t *testing.T) (string, func()) {
	t.Helper()
	cert := mustSelfSigned(t)
	st, err := store.Open(filepath.Join(t.TempDir(), "n.db"))
	require.NoError(t, err)
	gen := ids.NewGen("n1")
	mgr := sandbox.NewManager("n1", sandbox.NewFake(), st, gen)
	svc := NewSandboxService(mgr, ops.NewManager(st, gen))
	h, grpcSrv, err := Build(Options{
		NodeID: "n1", NodeName: "n", Version: "v0",
		Keys:      keyMap{"adm": "admin", "ro": "read-only"},
		Signer:    testSigner(),
		Cert:      cert,
		Sandboxes: svc,
	})
	require.NoError(t, err)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	srv := &http.Server{Handler: h, TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"h2", "http/1.1"}}}
	go srv.ServeTLS(ln, "", "")
	return ln.Addr().String(), func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		grpcSrv.Stop()
		_ = st.Close()
	}
}

func TestRoleGate_ReadOnlyCannotCreateOverREST(t *testing.T) {
	addr, cleanup := startRoleGateServer(t)
	defer cleanup()
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}

	post := func(key string) int {
		req, _ := http.NewRequest(http.MethodPost, "https://"+addr+"/v1/sandboxes", strings.NewReader(`{"cpus":1}`))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		require.NoError(t, err)
		resp.Body.Close()
		return resp.StatusCode
	}
	require.Equal(t, http.StatusForbidden, post("ro")) // read-only blocked
	require.Equal(t, http.StatusOK, post("adm"))       // admin allowed
}

func TestRoleGate_ReadOnlyCanListOverREST(t *testing.T) {
	addr, cleanup := startRoleGateServer(t)
	defer cleanup()
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	req, _ := http.NewRequest(http.MethodGet, "https://"+addr+"/v1/sandboxes", nil)
	req.Header.Set("Authorization", "Bearer ro")
	resp, err := client.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
}
