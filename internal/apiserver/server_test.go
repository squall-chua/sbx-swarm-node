package apiserver

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/squall-chua/sbx-swarm-node/internal/auth"
	sbxv1 "github.com/squall-chua/sbx-swarm-node/internal/gen/sbxswarm/v1"
	"github.com/squall-chua/sbx-swarm-node/internal/ids"
	"github.com/squall-chua/sbx-swarm-node/internal/ops"
	"github.com/squall-chua/sbx-swarm-node/internal/sandbox"
	"github.com/squall-chua/sbx-swarm-node/internal/store"
	"github.com/squall-chua/sbx-swarm-node/internal/tlsutil"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type keyMap map[string]string

func (k keyMap) RoleForKey(key string) (string, bool) { r, ok := k[key]; return r, ok }

func mustSelfSigned(t *testing.T) tls.Certificate {
	t.Helper()
	cert, err := tlsutil.LoadOrGenerate("", "", t.TempDir())
	require.NoError(t, err)
	return cert
}

func testSigner() *auth.Signer { return auth.NewSigner([]byte("test-secret")) }

func startTestServer(t *testing.T) (addr string, cleanup func()) {
	t.Helper()
	cert := mustSelfSigned(t)
	opts := Options{
		NodeID: "n1", NodeName: "n", Version: "v0",
		Keys:   keyMap{"adm": "admin"},
		Signer: testSigner(),
		Cert:   cert,
	}
	h, _, grpcSrv, err := Build(opts)
	require.NoError(t, err)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := &http.Server{Handler: h, TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"h2", "http/1.1"}}}
	go srv.ServeTLS(ln, "", "")

	return ln.Addr().String(), func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		grpcSrv.Stop()
	}
}

func TestServer_RESTRequiresAuth_AndReturnsNodeInfo(t *testing.T) {
	addr, cleanup := startTestServer(t)
	defer cleanup()
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}

	// no auth -> 401
	resp, err := client.Get("https://" + addr + "/v1/node")
	require.NoError(t, err)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	resp.Body.Close()

	// bearer -> 200 + node id
	req, _ := http.NewRequest(http.MethodGet, "https://"+addr+"/v1/node", nil)
	req.Header.Set("Authorization", "Bearer adm")
	resp, err = client.Do(req)
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, string(body), `"node_id":"n1"`)
}

func TestServer_GRPCGetNodeInfo(t *testing.T) {
	addr, cleanup := startTestServer(t)
	defer cleanup()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{InsecureSkipVerify: true})))
	require.NoError(t, err)
	defer conn.Close()

	// no creds -> Unauthenticated
	_, err = sbxv1.NewNodeServiceClient(conn).GetNodeInfo(context.Background(), &sbxv1.GetNodeInfoRequest{})
	require.Equal(t, codes.Unauthenticated, status.Code(err))

	// bearer in metadata -> ok
	ctx := metadata.NewOutgoingContext(context.Background(), metadata.Pairs("authorization", "Bearer adm"))
	out, err := sbxv1.NewNodeServiceClient(conn).GetNodeInfo(ctx, &sbxv1.GetNodeInfoRequest{})
	require.NoError(t, err)
	require.Equal(t, "n1", out.NodeId)
}

func startTestServerWithSandboxes(t *testing.T) (addr string, cleanup func()) {
	t.Helper()
	cert := mustSelfSigned(t)
	st, err := store.Open(filepath.Join(t.TempDir(), "n.db"))
	require.NoError(t, err)
	gen := ids.NewGen("n1")
	mgr := sandbox.NewManager("n1", sandbox.NewFake(), st, gen)
	svc := NewSandboxService(mgr, ops.NewManager(st, gen))
	opts := Options{
		NodeID: "n1", NodeName: "n", Version: "v0",
		Keys:      keyMap{"adm": "admin"},
		Signer:    testSigner(),
		Cert:      cert,
		Sandboxes: svc,
	}
	h, _, grpcSrv, err := Build(opts)
	require.NoError(t, err)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
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

func TestServer_CreateSandboxOverREST(t *testing.T) {
	addr, cleanup := startTestServerWithSandboxes(t)
	defer cleanup()
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}

	req, _ := http.NewRequest(http.MethodPost, "https://"+addr+"/v1/sandboxes", strings.NewReader(`{"cpus":1,"memory_bytes":1073741824}`))
	req.Header.Set("Authorization", "Bearer adm")
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, string(body), `"id"`) // operation id
}

func TestServer_CreateSandboxIdempotentOverREST(t *testing.T) {
	addr, cleanup := startTestServerWithSandboxes(t)
	defer cleanup()
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}

	do := func() string {
		req, _ := http.NewRequest(http.MethodPost, "https://"+addr+"/v1/sandboxes", strings.NewReader(`{"cpus":1}`))
		req.Header.Set("Authorization", "Bearer adm")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "key-xyz")
		resp, err := client.Do(req)
		require.NoError(t, err)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		var op struct {
			Id string `json:"id"`
		}
		require.NoError(t, json.Unmarshal(body, &op))
		require.NotEmpty(t, op.Id)
		return op.Id
	}

	require.Equal(t, do(), do()) // same Idempotency-Key -> same operation
}
