package apiserver

import (
	"context"
	"net"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

const loopbackBuf = 1 << 20

// loopback serves grpcSrv over an in-memory listener and dials it. The gateway
// uses the returned conn so REST traffic traverses the same interceptor chain as
// native gRPC. The listener is in-memory only (never network-reachable).
func loopback(grpcSrv *grpc.Server) (*grpc.ClientConn, *bufconn.Listener, error) {
	lis := bufconn.Listen(loopbackBuf)
	go func() { _ = grpcSrv.Serve(lis) }()
	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		lis.Close()
		return nil, nil, err
	}
	return conn, lis, nil
}
