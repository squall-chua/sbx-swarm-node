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
}

// NetLogCollector polls BlockedEgress and accumulates distinct blocked pairs.
type NetLogCollector struct {
	backend sandbox.Backend
	resolve func(vmName string) (sandboxID string, ok bool)
	now     func() time.Time

	mu    sync.RWMutex
	pairs map[string]*BlockedPair // key: host|sandboxID
}

// NewNetLogCollector builds the collector. resolve maps a backend VM name to a
// swarm sandbox ID.
func NewNetLogCollector(b sandbox.Backend, resolve func(string) (string, bool)) *NetLogCollector {
	return &NetLogCollector{
		backend: b,
		resolve: resolve,
		now:     time.Now,
		pairs:   map[string]*BlockedPair{},
	}
}

// PollOnce reads the current blocked set and merges it (first/last seen).
func (c *NetLogCollector) PollOnce(ctx context.Context) error {
	cur, err := c.backend.BlockedEgress(ctx)
	if err != nil {
		return err
	}
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, b := range cur {
		sbID, ok := c.resolve(b.VMName)
		if !ok {
			sbID = b.VMName
		}
		key := b.Host + "|" + sbID
		if p, exists := c.pairs[key]; exists {
			p.LastSeen = now
		} else {
			c.pairs[key] = &BlockedPair{Host: b.Host, SandboxID: sbID, FirstSeen: now, LastSeen: now}
		}
	}
	return nil
}

// ForSandbox returns the blocked pairs for a sandbox.
func (c *NetLogCollector) ForSandbox(sandboxID string) []BlockedPair {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var out []BlockedPair
	for _, p := range c.pairs {
		if p.SandboxID == sandboxID {
			out = append(out, *p)
		}
	}
	return out
}

// DistinctCount returns the number of distinct (host, sandbox) pairs.
func (c *NetLogCollector) DistinctCount() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.pairs)
}
