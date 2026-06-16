package apiserver

import (
	"context"

	sbxv1 "github.com/squall-chua/sbx-swarm-node/internal/gen/sbxswarm/v1"
	"github.com/squall-chua/sbx-swarm-node/internal/obsd"
	"github.com/squall-chua/sbx-swarm-node/internal/sandbox"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ObserveDeps are the observability collectors and handles the observe RPCs and
// SSE handlers read from.
type ObserveDeps struct {
	Stats   *obsd.StatsCollector
	NetLog  *obsd.NetLogCollector
	Backend sandbox.Backend  // for log streaming (SSE /logs)
	Mgr     *sandbox.Manager // for resolving id -> backend name (SSE)
}

// WithObserve wires the observability collectors into the service.
func (s *SandboxService) WithObserve(o ObserveDeps) { s.obs = o }

// observeStreamReady reports whether the SSE log/stats handlers can run (backend
// + manager + stats collector are all wired).
func (s *SandboxService) observeStreamReady() bool {
	return s.obs.Backend != nil && s.obs.Mgr != nil && s.obs.Stats != nil
}

// GetStats returns the latest cached usage for a sandbox.
func (s *SandboxService) GetStats(ctx context.Context, r *sbxv1.IdRequest) (*sbxv1.Stats, error) {
	if s.obs.Stats == nil {
		return nil, status.Error(codes.Unimplemented, "observe not configured")
	}
	name, err := s.mgr.Resolve(ctx, r.Id)
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	u, ok := s.obs.Stats.Latest(name)
	if !ok {
		return &sbxv1.Stats{}, nil
	}
	return &sbxv1.Stats{
		Cores:      int32(u.Cores),
		CpuPercent: u.CPUPercent,
		MemTotalKb: u.MemTotalKB,
		MemUsedKb:  u.MemUsedKB,
	}, nil
}

// ListBlocked returns the distinct blocked-egress pairs for a sandbox.
func (s *SandboxService) ListBlocked(_ context.Context, r *sbxv1.IdRequest) (*sbxv1.ListBlockedResponse, error) {
	if s.obs.NetLog == nil {
		return nil, status.Error(codes.Unimplemented, "observe not configured")
	}
	pairs := s.obs.NetLog.ForSandbox(r.Id)
	out := &sbxv1.ListBlockedResponse{DistinctCount: int32(s.obs.NetLog.DistinctCount())}
	for _, p := range pairs {
		out.Blocked = append(out.Blocked, &sbxv1.Blocked{
			Host:      p.Host,
			FirstSeen: p.FirstSeen.Format("2006-01-02T15:04:05Z07:00"),
			LastSeen:  p.LastSeen.Format("2006-01-02T15:04:05Z07:00"),
		})
	}
	return out, nil
}
