package peer

import (
	"context"
	"net"
	"testing"

	sbxv1 "github.com/squall-chua/sbx-swarm-node/internal/gen/sbxswarm/v1"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

type infoSvc struct{ sbxv1.UnimplementedNodeServiceServer }

func (infoSvc) GetNodeInfo(context.Context, *sbxv1.GetNodeInfoRequest) (*sbxv1.NodeInfo, error) {
	return &sbxv1.NodeInfo{NodeId: "peer"}, nil
}

func TestPool_DialAndReuse(t *testing.T) {
	lis := bufconn.Listen(1 << 20)
	s := grpc.NewServer()
	sbxv1.RegisterNodeServiceServer(s, infoSvc{})
	go s.Serve(lis)
	defer s.Stop()

	dial := func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }
	p := NewPool(WithContextDialer(dial), WithCreds(insecure.NewCredentials()))

	c1, err := p.Conn("bufnet", "peer")
	require.NoError(t, err)
	out, err := sbxv1.NewNodeServiceClient(c1).GetNodeInfo(context.Background(), &sbxv1.GetNodeInfoRequest{})
	require.NoError(t, err)
	require.Equal(t, "peer", out.NodeId)

	c2, err := p.Conn("bufnet", "peer")
	require.NoError(t, err)
	require.Same(t, c1, c2) // cached
}
