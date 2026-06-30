package obsd

import (
	"context"
	"sync"
	"time"

	"github.com/squall-chua/sbx-swarm-node/internal/sandbox"
)

// BlockedPair is a distinct (host, sandbox) egress denial with synthesized times.
type BlockedPair struct {
	Host      string    `json:"host"`
	SandboxID string    `json:"sandbox_id"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
	Count     int       `json:"count"` // proxy hit count (latest poll)
}

// NetLogCollector polls the egress log and accumulates distinct blocked and
// allowed (host, sandbox) pairs.
type NetLogCollector struct {
	backend sandbox.Backend
	resolve func(vmName string) (sandboxID string, ok bool)
	now     func() time.Time

	mu      sync.RWMutex
	pairs   map[string]*BlockedPair // blocked, key: host|sandboxID
	allowed map[string]*BlockedPair // allowed, key: host|sandboxID
}

// NewNetLogCollector builds the collector. resolve maps a backend VM name to a
// swarm sandbox ID.
func NewNetLogCollector(b sandbox.Backend, resolve func(string) (string, bool)) *NetLogCollector {
	return &NetLogCollector{
		backend: b,
		resolve: resolve,
		now:     time.Now,
		pairs:   map[string]*BlockedPair{},
		allowed: map[string]*BlockedPair{},
	}
}

// PollOnce reads the current blocked + allowed sets and merges them (first/last seen).
func (c *NetLogCollector) PollOnce(ctx context.Context) error {
	blocked, err := c.backend.BlockedEgress(ctx)
	if err != nil {
		return err
	}
	allowed, err := c.backend.AllowedEgress(ctx)
	if err != nil {
		return err
	}
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.merge(c.pairs, blocked, now)
	c.merge(c.allowed, allowed, now)
	return nil
}

// merge folds the current host set into dst, synthesizing first/last seen. Caller holds c.mu.
func (c *NetLogCollector) merge(dst map[string]*BlockedPair, cur []sandbox.BlockedHost, now time.Time) {
	for _, b := range cur {
		sbID, ok := c.resolve(b.VMName)
		if !ok {
			sbID = b.VMName
		}
		key := b.Host + "|" + sbID
		if p, exists := dst[key]; exists {
			p.LastSeen = now
			p.Count = b.Count // count_since is cumulative; keep the latest
		} else {
			dst[key] = &BlockedPair{Host: b.Host, SandboxID: sbID, FirstSeen: now, LastSeen: now, Count: b.Count}
		}
	}
}

func forSandbox(m map[string]*BlockedPair, sandboxID string) []BlockedPair {
	var out []BlockedPair
	for _, p := range m {
		if p.SandboxID == sandboxID {
			out = append(out, *p)
		}
	}
	return out
}

// ForSandbox returns the blocked pairs for a sandbox.
func (c *NetLogCollector) ForSandbox(sandboxID string) []BlockedPair {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return forSandbox(c.pairs, sandboxID)
}

// AllowedForSandbox returns the allowed pairs for a sandbox.
func (c *NetLogCollector) AllowedForSandbox(sandboxID string) []BlockedPair {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return forSandbox(c.allowed, sandboxID)
}

// DistinctCount returns the number of distinct blocked (host, sandbox) pairs.
func (c *NetLogCollector) DistinctCount() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.pairs)
}

// AllowedDistinctCount returns the number of distinct allowed (host, sandbox) pairs.
func (c *NetLogCollector) AllowedDistinctCount() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.allowed)
}
