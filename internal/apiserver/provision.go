package apiserver

import (
	"context"
	"sync"
	"time"

	sbxv1 "github.com/squall-chua/sbx-swarm-node/internal/gen/sbxswarm/v1"
	"github.com/squall-chua/sbx-swarm-node/internal/sandbox"
)

// dedup is a bounded TTL cache of request_id -> sandbox_id for cross-node
// Provision idempotency: a re-sent request returns the original sandbox.
// ponytail: O(n) oldest-eviction, fine at max<=1024; swap for an LRU if it grows.
type dedup struct {
	mu  sync.Mutex
	m   map[string]dedupEntry
	ttl time.Duration
	max int
}

type dedupEntry struct {
	sandboxID string
	at        time.Time
}

func newDedup(ttl time.Duration, max int) *dedup {
	return &dedup{m: map[string]dedupEntry{}, ttl: ttl, max: max}
}

func (d *dedup) get(id string) (string, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	e, ok := d.m[id]
	if !ok {
		return "", false
	}
	if time.Since(e.at) > d.ttl {
		delete(d.m, id)
		return "", false
	}
	return e.sandboxID, true
}

func (d *dedup) put(id, sandboxID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now()
	for k, e := range d.m { // sweep expired
		if now.Sub(e.at) > d.ttl {
			delete(d.m, k)
		}
	}
	if len(d.m) >= d.max { // evict oldest
		var oldestKey string
		var oldestAt time.Time
		first := true
		for k, e := range d.m {
			if first || e.at.Before(oldestAt) {
				oldestKey, oldestAt, first = k, e.at, false
			}
		}
		delete(d.m, oldestKey)
	}
	d.m[id] = dedupEntry{sandboxID: sandboxID, at: now}
}

// InternalService handles node->node RPCs (provision admission). Authorized by
// node identity (ADR-0011); never exposed over REST.
type InternalService struct {
	sbxv1.UnimplementedInternalServiceServer
	mgr      *sandbox.Manager
	cordoned func() bool // self-cordon check; nil when standalone (never cordoned)
	dedup    *dedup
}

// NewInternalService builds the internal service. cordoned reports this node's
// own cordon state (nil = standalone, never cordoned).
func NewInternalService(mgr *sandbox.Manager, cordoned func() bool) *InternalService {
	return &InternalService{mgr: mgr, cordoned: cordoned, dedup: newDedup(5*time.Minute, 1024)}
}

// Provision performs target-authoritative admission against real local capacity,
// then creates. A capacity miss returns accepted=false (the coordinator's NACK).
func (s *InternalService) Provision(ctx context.Context, r *sbxv1.ProvisionRequest) (*sbxv1.ProvisionReply, error) {
	// Self-cordon recheck: a node cordoned after the entry node's candidate
	// snapshot must refuse a forwarded provision until gossip propagates (the
	// coordinator treats this NACK as a retry on the next candidate). The cordon
	// recheck deliberately precedes the dedup lookup (spec §2.2): a node cordoned
	// after recording a request_id NACKs a re-send rather than returning the
	// cached id. That's within the accepted double-fault window (§2.1) and only
	// reachable if the cordon flips inside the same-target retry's microsecond gap.
	if s.cordoned != nil && s.cordoned() {
		return &sbxv1.ProvisionReply{Accepted: false, Reason: "cordoned"}, nil
	}
	if r.RequestId != "" {
		if sbID, ok := s.dedup.get(r.RequestId); ok {
			return &sbxv1.ProvisionReply{Accepted: true, SandboxId: sbID}, nil
		}
	}
	in := r.Spec
	if in == nil {
		in = &sbxv1.CreateSandboxRequest{}
	}
	// Defensive: re-apply the built-in floor at the node->node trust boundary so a
	// peer that sends an unsized spec cannot bypass capacity accounting (ADR-0011).
	spec := toSpec(effectiveSpec(in, sandbox.Resources{}))
	rec, err := s.mgr.AdmitAndCreate(ctx, spec)
	if err == sandbox.ErrNoCapacity {
		return &sbxv1.ProvisionReply{Accepted: false, Reason: "no capacity"}, nil
	}
	if err != nil {
		return nil, err
	}
	if r.RequestId != "" {
		s.dedup.put(r.RequestId, rec.ID)
	}
	return &sbxv1.ProvisionReply{Accepted: true, SandboxId: rec.ID}, nil
}
