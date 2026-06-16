package peer

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"net"
	"testing"
	"time"

	sbxv1 "github.com/squall-chua/sbx-swarm-node/internal/gen/sbxswarm/v1"
	"github.com/squall-chua/sbx-swarm-node/internal/identity"
	"github.com/squall-chua/sbx-swarm-node/internal/nodekey"
	"github.com/squall-chua/sbx-swarm-node/internal/tlsutil"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
)

// echoNodeService records the node-key metadata it receives and returns a node id.
type echoNodeService struct {
	sbxv1.UnimplementedNodeServiceServer
	gotNodeAuth chan string
}

func (s *echoNodeService) GetNodeInfo(ctx context.Context, _ *sbxv1.GetNodeInfoRequest) (*sbxv1.NodeInfo, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	v := md.Get(nodekey.MetadataKey)
	if len(v) > 0 {
		s.gotNodeAuth <- v[0]
	} else {
		s.gotNodeAuth <- ""
	}
	return &sbxv1.NodeInfo{NodeId: "server"}, nil
}

func startTLSGRPC(t *testing.T, priv ed25519.PrivateKey) string {
	t.Helper()
	cert, err := tlsutil.GenerateForKey(priv)
	require.NoError(t, err)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{cert}})))
	sbxv1.RegisterNodeServiceServer(srv, &echoNodeService{gotNodeAuth: make(chan string, 1)})
	go srv.Serve(ln)
	t.Cleanup(srv.Stop)
	return ln.Addr().String()
}

func TestPool_PinnedDial_AcceptsMatching(t *testing.T) {
	srvPub, srvPriv, _ := ed25519.GenerateKey(rand.Reader)
	srvID := identity.DeriveNodeID(srvPub)
	addr := startTLSGRPC(t, srvPriv)

	callerPub, callerPriv, _ := ed25519.GenerateKey(rand.Reader)
	callerID := identity.DeriveNodeID(callerPub)

	pins := map[string][]byte{srvID: srvPub}
	pool := NewPool(
		WithNodeKey(callerID, callerPriv),
		WithPinResolver(func(id string) ([]byte, bool) { p, ok := pins[id]; return p, ok }),
	)
	defer pool.Close()

	conn, err := pool.Conn(addr, srvID)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := sbxv1.NewNodeServiceClient(conn).GetNodeInfo(ctx, &sbxv1.GetNodeInfoRequest{})
	require.NoError(t, err)
	require.Equal(t, "server", out.NodeId)
}

func TestPool_PinnedDial_RejectsMismatch(t *testing.T) {
	srvPub, srvPriv, _ := ed25519.GenerateKey(rand.Reader)
	srvID := identity.DeriveNodeID(srvPub)
	addr := startTLSGRPC(t, srvPriv)

	wrongPub, _, _ := ed25519.GenerateKey(rand.Reader) // gossiped pin is wrong
	_, callerPriv, _ := ed25519.GenerateKey(rand.Reader)
	pool := NewPool(
		WithNodeKey("caller", callerPriv),
		WithPinResolver(func(string) ([]byte, bool) { return wrongPub, true }),
	)
	defer pool.Close()

	conn, err := pool.Conn(addr, srvID)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err = sbxv1.NewNodeServiceClient(conn).GetNodeInfo(ctx, &sbxv1.GetNodeInfoRequest{})
	require.Error(t, err) // TLS handshake fails the pin
}
