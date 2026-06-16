package apiserver

import (
	"context"
	"path/filepath"
	"testing"

	sbxv1 "github.com/squall-chua/sbx-swarm-node/internal/gen/sbxswarm/v1"
	"github.com/squall-chua/sbx-swarm-node/internal/ids"
	"github.com/squall-chua/sbx-swarm-node/internal/obsd"
	"github.com/squall-chua/sbx-swarm-node/internal/ops"
	"github.com/squall-chua/sbx-swarm-node/internal/sandbox"
	"github.com/squall-chua/sbx-swarm-node/internal/store"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func newObserveSvc(t *testing.T) (*SandboxService, *sandbox.Fake, *sandbox.Manager, *obsd.StatsCollector, *obsd.NetLogCollector) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "n.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	f := sandbox.NewFake()
	gen := ids.NewGen("n1")
	mgr := sandbox.NewManager("n1", f, st, gen)
	opsM := ops.NewManager(st, gen)

	statsC := obsd.NewStatsCollector(f, func(ctx context.Context) ([]string, error) {
		bs, err := f.List(ctx)
		if err != nil {
			return nil, err
		}
		names := make([]string, len(bs))
		for i, b := range bs {
			names[i] = b.Name
		}
		return names, nil
	}, obsd.ProvisionLimit{CPU: 4, MemKB: 1 << 21}, 4)

	netC := obsd.NewNetLogCollector(f, func(vm string) (string, bool) { return vm, true })

	svc := NewSandboxService(mgr, opsM)
	svc.WithObserve(ObserveDeps{Stats: statsC, NetLog: netC})
	return svc, f, mgr, statsC, netC
}

func TestGetStats_ReturnsUnimplementedWhenObsNotConfigured(t *testing.T) {
	svc := NewSandboxService(nil, nil)
	_, err := svc.GetStats(context.Background(), &sbxv1.IdRequest{Id: "x"})
	require.Equal(t, codes.Unimplemented, status.Code(err))
}

func TestGetStats_ReturnsCachedUsage(t *testing.T) {
	svc, _, mgr, statsC, _ := newObserveSvc(t)
	ctx := context.Background()

	rec, _ := mgr.Create(ctx, sandbox.CreateSpec{Name: "s1"})
	require.NoError(t, statsC.PollOnce(ctx))

	resp, err := svc.GetStats(ctx, &sbxv1.IdRequest{Id: rec.ID})
	require.NoError(t, err)
	require.Equal(t, int32(2), resp.Cores)
}

func TestListBlocked_ReturnsUnimplementedWhenObsNotConfigured(t *testing.T) {
	svc := NewSandboxService(nil, nil)
	_, err := svc.ListBlocked(context.Background(), &sbxv1.IdRequest{Id: "x"})
	require.Equal(t, codes.Unimplemented, status.Code(err))
}

func TestListBlocked_ReturnsDistinctPairs(t *testing.T) {
	_, f, mgr, statsC, _ := newObserveSvc(t)
	ctx := context.Background()

	st2, err := store.Open(filepath.Join(t.TempDir(), "n2.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st2.Close() })

	rec, _ := mgr.Create(ctx, sandbox.CreateSpec{Name: "s1"})
	f.SetBlocked([]sandbox.BlockedHost{{Host: "evil.com", VMName: rec.BackendName}})

	// Wire a fresh NetLogCollector that resolves VM name → sandbox ID.
	netC2 := obsd.NewNetLogCollector(f, func(vm string) (string, bool) {
		return rec.ID, true
	})
	require.NoError(t, netC2.PollOnce(ctx))

	gen2 := ids.NewGen("n1")
	opsM2 := ops.NewManager(st2, gen2)
	svc2 := NewSandboxService(mgr, opsM2)
	svc2.WithObserve(ObserveDeps{Stats: statsC, NetLog: netC2})

	resp, err := svc2.ListBlocked(ctx, &sbxv1.IdRequest{Id: rec.ID})
	require.NoError(t, err)
	require.Len(t, resp.Blocked, 1)
	require.Equal(t, "evil.com", resp.Blocked[0].Host)
	require.Equal(t, int32(1), resp.DistinctCount)
}
