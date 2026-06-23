package apiserver

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	sbxv1 "github.com/squall-chua/sbx-swarm-node/internal/gen/sbxswarm/v1"
	"github.com/squall-chua/sbx-swarm-node/internal/ids"
	"github.com/squall-chua/sbx-swarm-node/internal/ops"
	"github.com/squall-chua/sbx-swarm-node/internal/peer"
	"github.com/squall-chua/sbx-swarm-node/internal/routing"
	"github.com/squall-chua/sbx-swarm-node/internal/sandbox"
	"github.com/squall-chua/sbx-swarm-node/internal/store"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

// TestForward_UnaryGetSandbox: server A (with forwarding interceptor) routes
// GetSandbox for a nB-owned sandbox to server B.
func TestForward_UnaryGetSandbox(t *testing.T) {
	const bufSize = 1 << 20

	// --- Server B: holds the sandbox owned by "nB" ---
	lisB := bufconn.Listen(bufSize)
	stB, err := store.Open(filepath.Join(t.TempDir(), "b.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = stB.Close() })
	genB := ids.NewGen("nB")
	mgrB := sandbox.NewManager("nB", sandbox.NewFake(), stB, genB)
	svcB := NewSandboxService(mgrB, ops.NewManager(stB, genB))

	// Create a sandbox directly so we have a known ID.
	rec, err := mgrB.Create(context.Background(), sandbox.CreateSpec{})
	require.NoError(t, err)
	sbxID := rec.ID // "nB.<ulid>"

	grpcB := grpc.NewServer()
	sbxv1.RegisterSandboxServiceServer(grpcB, svcB)
	go grpcB.Serve(lisB) //nolint:errcheck
	t.Cleanup(grpcB.Stop)

	// --- Routing table + peer pool for server A ---
	const addrB = "nB-bufnet"
	tbl := routing.NewTable("nA")
	tbl.Upsert("nB", addrB, false, nil)

	dialB := func(ctx context.Context, _ string) (net.Conn, error) {
		return lisB.DialContext(ctx)
	}
	pool := peer.NewPool(
		peer.WithContextDialer(dialB),
		peer.WithCreds(insecure.NewCredentials()),
	)
	t.Cleanup(pool.Close)

	// --- Server A: forwarding interceptor only, no real sandbox service ---
	lisA := bufconn.Listen(bufSize)
	fwd := NewForwarder(tbl, pool, "nA")
	grpcA := grpc.NewServer(grpc.UnaryInterceptor(fwd.UnaryInterceptor()))
	// Register sandbox service on A too (so the method is known to gRPC), but
	// with an empty manager — it should never be called for nB's sandbox.
	stA, err := store.Open(filepath.Join(t.TempDir(), "a.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = stA.Close() })
	genA := ids.NewGen("nA")
	mgrA := sandbox.NewManager("nA", sandbox.NewFake(), stA, genA)
	svcA := NewSandboxService(mgrA, ops.NewManager(stA, genA))
	sbxv1.RegisterSandboxServiceServer(grpcA, svcA)
	go grpcA.Serve(lisA) //nolint:errcheck
	t.Cleanup(grpcA.Stop)

	// --- Client dials A ---
	connA, err := grpc.NewClient("passthrough:///a",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lisA.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = connA.Close() })

	got, err := sbxv1.NewSandboxServiceClient(connA).GetSandbox(context.Background(), &sbxv1.GetSandboxRequest{Id: sbxID})
	require.NoError(t, err)
	require.Equal(t, sbxID, got.Id)
	require.Equal(t, "nB", got.OwnerNode)
}

// TestForwarder_RoutesCordonByNodeID: a Cordon request with an empty node_id is handled
// locally; one with a peer node_id is forwarded (and errors when the peer is unreachable).
func TestForwarder_RoutesCordonByNodeID(t *testing.T) {
	tbl := routing.NewTable("self")
	tbl.Upsert("peer2", "127.0.0.1:65501", false, nil) // unreachable addr; we assert dial is attempted

	fwd := NewForwarder(tbl, peer.NewPool(peer.WithCreds(insecure.NewCredentials())), "self")
	interceptor := fwd.UnaryInterceptor()

	called := false
	handler := func(ctx context.Context, req any) (any, error) { called = true; return &sbxv1.NodeInfo{}, nil }

	// Empty node_id -> handled locally.
	_, err := interceptor(context.Background(), &sbxv1.CordonRequest{}, &grpc.UnaryServerInfo{FullMethod: "/sbxswarm.v1.NodeService/Cordon"}, handler)
	require.NoError(t, err)
	require.True(t, called, "empty node_id must run locally")

	// Peer node_id -> forwarded (dial fails to the bogus addr => error, NOT local handling).
	// Short deadline so the bogus-addr dial fails fast instead of waiting gRPC's default timeout.
	called = false
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = interceptor(ctx, &sbxv1.CordonRequest{NodeId: "peer2"}, &grpc.UnaryServerInfo{FullMethod: "/sbxswarm.v1.NodeService/Cordon"}, handler)
	require.Error(t, err, "forwarding to an unreachable peer should error")
	require.False(t, called, "peer-targeted cordon must not run the local handler")
}

// TestForward_LocalPassthrough: a request for a local sandbox goes to the local handler.
func TestForward_LocalPassthrough(t *testing.T) {
	const bufSize = 1 << 20

	lisA := bufconn.Listen(bufSize)
	stA, err := store.Open(filepath.Join(t.TempDir(), "a.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = stA.Close() })
	genA := ids.NewGen("nA")
	mgrA := sandbox.NewManager("nA", sandbox.NewFake(), stA, genA)
	svcA := NewSandboxService(mgrA, ops.NewManager(stA, genA))

	rec, err := mgrA.Create(context.Background(), sandbox.CreateSpec{})
	require.NoError(t, err)

	tbl := routing.NewTable("nA")
	pool := peer.NewPool(peer.WithCreds(insecure.NewCredentials()))
	t.Cleanup(pool.Close)

	fwd := NewForwarder(tbl, pool, "nA")
	grpcA := grpc.NewServer(grpc.UnaryInterceptor(fwd.UnaryInterceptor()))
	sbxv1.RegisterSandboxServiceServer(grpcA, svcA)
	go grpcA.Serve(lisA) //nolint:errcheck
	t.Cleanup(grpcA.Stop)

	connA, err := grpc.NewClient("passthrough:///a",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lisA.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = connA.Close() })

	got, err := sbxv1.NewSandboxServiceClient(connA).GetSandbox(context.Background(), &sbxv1.GetSandboxRequest{Id: rec.ID})
	require.NoError(t, err)
	require.Equal(t, rec.ID, got.Id)
	require.Equal(t, "nA", got.OwnerNode)
}
