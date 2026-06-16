package apiserver

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/squall-chua/sbx-swarm-node/internal/auth"
	sbxv1 "github.com/squall-chua/sbx-swarm-node/internal/gen/sbxswarm/v1"
	"github.com/squall-chua/sbx-swarm-node/internal/tlsutil"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
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
	h, grpcSrv, err := Build(opts)
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

	out, err := sbxv1.NewNodeServiceClient(conn).GetNodeInfo(context.Background(), &sbxv1.GetNodeInfoRequest{})
	require.NoError(t, err)
	require.Equal(t, "n1", out.NodeId)
}
