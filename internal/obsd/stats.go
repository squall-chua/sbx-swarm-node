// Package obsd holds background observability collectors (stats, network log).
package obsd

import (
	"context"
	"sync"

	"github.com/squall-chua/sbx-swarm-node/internal/sandbox"
	"golang.org/x/sync/errgroup"
)

type provisionLimit struct {
	CPU   float64
	MemKB float64
}

// Util is normalized actual utilization (0..1+) vs the provision limit.
type Util struct{ CPU, Mem float64 }

// StatsCollector polls running sandboxes for usage and caches the latest.
type StatsCollector struct {
	backend     sandbox.Backend
	list        func(context.Context) ([]string, error)
	limit       provisionLimit
	concurrency int

	mu     sync.RWMutex
	latest map[string]sandbox.Usage
	util   Util
}

// NewStatsCollector builds a collector polling at most concurrency sandboxes at once.
func NewStatsCollector(b sandbox.Backend, list func(context.Context) ([]string, error), limit provisionLimit, concurrency int) *StatsCollector {
	if concurrency <= 0 {
		concurrency = 4
	}
	return &StatsCollector{
		backend:     b,
		list:        list,
		limit:       limit,
		concurrency: concurrency,
		latest:      map[string]sandbox.Usage{},
	}
}

// PollOnce polls all sandboxes once (bounded concurrency), updating the cache.
func (c *StatsCollector) PollOnce(ctx context.Context) error {
	names, err := c.list(ctx)
	if err != nil {
		return err
	}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(c.concurrency)
	var mu sync.Mutex
	got := map[string]sandbox.Usage{}
	for _, name := range names {
		name := name
		g.Go(func() error {
			u, err := c.backend.Stats(gctx, name)
			if err != nil {
				return nil // skip this sandbox; don't fail the whole poll
			}
			mu.Lock()
			got[name] = u
			mu.Unlock()
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}

	var sumCPU, sumMem float64
	for _, u := range got {
		sumCPU += (u.CPUPercent / 100.0) * float64(u.Cores)
		sumMem += float64(u.MemUsedKB)
	}
	c.mu.Lock()
	c.latest = got
	c.util = Util{CPU: safeDiv(sumCPU, c.limit.CPU), Mem: safeDiv(sumMem, c.limit.MemKB)}
	c.mu.Unlock()
	return nil
}

// Latest returns the cached usage for a sandbox.
func (c *StatsCollector) Latest(name string) (sandbox.Usage, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	u, ok := c.latest[name]
	return u, ok
}

// ActualUtil returns the aggregate normalized utilization.
func (c *StatsCollector) ActualUtil() Util {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.util
}

func safeDiv(a, b float64) float64 {
	if b == 0 {
		return 0
	}
	return a / b
}
