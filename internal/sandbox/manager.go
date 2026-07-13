package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/squall-chua/sbx-swarm-node/internal/events"
	"github.com/squall-chua/sbx-swarm-node/internal/ids"
	"github.com/squall-chua/sbx-swarm-node/internal/store"
)

// ErrNoCapacity means the node cannot admit the request within its provision limit.
var ErrNoCapacity = errors.New("insufficient capacity")

const bucket = "sandboxes"

// OwnedIDsNotifier is notified when this node's owned-sandbox set changes, so
// the cluster can re-gossip OwnedSandboxIDs. Implemented by membership.Cluster.
type OwnedIDsNotifier interface {
	UpdateLocalSandboxIDs(ids []string)
}

// Manager owns this node's sandbox records and drives the Backend.
type Manager struct {
	nodeID   string
	backend  Backend
	store    *store.Store
	ids      *ids.Gen
	pub      events.Publisher
	ownedSub OwnedIDsNotifier
	now      func() time.Time
	capacity *Capacity
	mu       sync.Mutex // serializes node-local record read-modify-write (BumpActivity vs Stop/Reconcile)
}

// NewManager builds a Manager.
func NewManager(nodeID string, backend Backend, st *store.Store, gen *ids.Gen) *Manager {
	return &Manager{nodeID: nodeID, backend: backend, store: st, ids: gen, now: time.Now, capacity: NewCapacity(0, 0, 0)}
}

// SetCapacity wires a capacity tracker (node.go passes resolved limits).
func (m *Manager) SetCapacity(c *Capacity) { m.capacity = c }

// Capacity returns the capacity tracker.
func (m *Manager) Capacity() *Capacity { return m.capacity }

// costOf is a spec's resource cost (cores / KB / GB).
func costOf(spec CreateSpec) (cpu, mem, disk float64) {
	return float64(spec.CPUs), float64(spec.MemoryBytes) / 1024, spec.DiskGB
}

// costSum totals the resource cost (cores/KB/GB) of all non-terminal records.
func costSum(recs []*Record) (cpu, mem, disk float64) {
	for _, rec := range recs {
		if rec.Status == "lost" {
			continue
		}
		c, m, d := costOf(rec.Spec)
		cpu += c
		mem += m
		disk += d
	}
	return
}

// AdmitAndCreate reserves capacity (atomic), creates, then commits the
// reservation into the base on success (or releases it on failure). Returns
// ErrNoCapacity when admission fails.
func (m *Manager) AdmitAndCreate(ctx context.Context, spec CreateSpec) (*Record, error) {
	cpu, mem, disk := costOf(spec)
	id, ok := m.capacity.TryReserve(cpu, mem, disk)
	if !ok {
		return nil, ErrNoCapacity
	}
	rec, err := m.Create(ctx, spec)
	if err != nil {
		m.capacity.Release(id)
		return nil, err
	}
	// Resync base absolutely from durable records (now incl. rec), then drop the
	// reservation — consistent with Reconcile, so no double-count. If the list
	// fails, fall back to the incremental commit (base += reserved cost).
	if recs, lerr := m.List(ctx); lerr == nil {
		bc, bm, bd := costSum(recs)
		m.capacity.CommitBase(bc, bm, bd, id)
	} else {
		m.capacity.Commit(id)
	}
	return rec, nil
}

// SetPublisher wires an event publisher (optional; nil disables events).
func (m *Manager) SetPublisher(p events.Publisher) { m.pub = p }

// SetOwnedIDsNotifier wires the cluster notifier (optional; nil disables
// owned-id re-gossip for non-cluster nodes).
func (m *Manager) SetOwnedIDsNotifier(n OwnedIDsNotifier) { m.ownedSub = n }

func (m *Manager) emit(eventType, sandboxID string, payload any) {
	if m.pub != nil {
		m.pub.Publish(eventType, sandboxID, payload)
	}
}

// notifyOwnedChanged recomputes the owned-id set and pushes it to the cluster
// notifier (if wired) so peers receive the updated OwnedSandboxIDs via gossip.
func (m *Manager) notifyOwnedChanged() {
	if m.ownedSub == nil {
		return
	}
	recs, err := m.List(context.Background())
	if err != nil {
		return
	}
	ids := make([]string, 0, len(recs))
	for _, r := range recs {
		ids = append(ids, r.ID)
	}
	m.ownedSub.UpdateLocalSandboxIDs(ids)
}

func (m *Manager) save(rec *Record) error {
	rec.UpdatedAt = m.now()
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return m.store.Put(bucket, rec.ID, raw)
}

// mutate applies fn to a freshly-read record under m.mu, then persists it.
// Re-reading inside the lock prevents lost updates when control-plane activity
// (BumpActivity) races a status transition (Stop/Reconcile/Delete). The lock is
// NEVER held across a backend RPC — callers do the RPC first, then mutate.
func (m *Manager) mutate(ctx context.Context, id string, fn func(*Record)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, err := m.Get(ctx, id)
	if err != nil {
		return err
	}
	fn(rec)
	return m.save(rec)
}

// Create provisions a sandbox and persists its record.
func (m *Manager) Create(ctx context.Context, spec CreateSpec) (*Record, error) {
	id := m.ids.Sandbox()
	backendName := id // use the swarm id as the SDK name for a 1:1 mapping
	spec.Name = backendName

	bs, err := m.backend.Create(ctx, spec)
	if err != nil {
		return nil, fmt.Errorf("backend create: %w", err)
	}
	now := m.now()
	rec := &Record{
		ID: id, BackendName: backendName, OwnerNode: m.nodeID,
		Spec: spec, Status: bs.Status, CreatedAt: now, LastActivity: now, Labels: spec.Labels,
	}
	if err := m.save(rec); err != nil {
		return nil, err
	}
	m.emit("sandbox.created", rec.ID, map[string]string{"status": rec.Status})
	m.notifyOwnedChanged()
	return rec, nil
}

// Get returns a record by swarm ID.
func (m *Manager) Get(_ context.Context, id string) (*Record, error) {
	raw, ok, err := m.store.Get(bucket, id)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNotFound
	}
	var rec Record
	if err := json.Unmarshal(raw, &rec); err != nil {
		return nil, err
	}
	return &rec, nil
}

// List returns all records on this node.
func (m *Manager) List(_ context.Context) ([]*Record, error) {
	var out []*Record
	err := m.store.ForEach(bucket, func(_, v []byte) error {
		var rec Record
		if err := json.Unmarshal(v, &rec); err != nil {
			return err
		}
		out = append(out, &rec)
		return nil
	})
	return out, err
}

func (m *Manager) lifecycle(ctx context.Context, id string, fn func(name string) error, status string) error {
	rec, err := m.Get(ctx, id)
	if err != nil {
		return err
	}
	if err := fn(rec.BackendName); err != nil {
		return err
	}
	if err := m.mutate(ctx, id, func(r *Record) { r.Status = status }); err != nil {
		return err
	}
	m.emit("sandbox."+status, rec.ID, nil)
	return nil
}

// BumpActivity records that the sandbox was just used (control-plane Activity),
// resetting its idle clock. Returns ErrNotFound if the sandbox is gone.
func (m *Manager) BumpActivity(ctx context.Context, id string) error {
	return m.mutate(ctx, id, func(r *Record) { r.LastActivity = m.now() })
}

// Start/Stop transition the backend and record.
func (m *Manager) Start(ctx context.Context, id string) error {
	if err := m.lifecycle(ctx, id, func(n string) error { return m.backend.Start(ctx, n) }, "running"); err != nil {
		return err
	}
	return m.BumpActivity(ctx, id) // Start is Activity (prevents immediate re-reap)
}
func (m *Manager) Stop(ctx context.Context, id string) error {
	return m.lifecycle(ctx, id, func(n string) error { return m.backend.Stop(ctx, n) }, "stopped")
}

// Delete removes the sandbox from the backend and the store.
func (m *Manager) Delete(ctx context.Context, id string) error {
	rec, err := m.Get(ctx, id)
	if err != nil {
		return err
	}
	if err := m.backend.Remove(ctx, rec.BackendName); err != nil && err != ErrNotFound {
		return err
	}
	// GC the sandbox's proxy-injected custom secrets. The daemon keys them by
	// scope (BackendName) and does NOT drop them when the sandbox is removed, so
	// they orphan in `secret ls`. Best-effort: a cleanup failure must not fail
	// the delete — the sandbox is already gone.
	m.removeCustomSecrets(ctx, rec.BackendName)
	m.mu.Lock()
	err = m.store.Delete(bucket, id)
	m.mu.Unlock()
	if err != nil {
		return err
	}
	m.emit("sandbox.deleted", id, nil)
	m.notifyOwnedChanged()
	return nil
}

// removeCustomSecrets best-effort deletes every proxy-injected custom secret
// scoped to a sandbox (keyed on its BackendName). Errors are swallowed:
// orphaned secrets are a minor leak, but a failed cleanup must never block
// sandbox deletion.
func (m *Manager) removeCustomSecrets(ctx context.Context, scope string) {
	secs, err := m.backend.SecretList(ctx, scope)
	if err != nil {
		return
	}
	for _, c := range secs.Custom {
		_ = m.backend.SecretRemove(ctx, scope, c.Host)
	}
}

// SetLastPublish records a successful publish time on the sandbox record.
func (m *Manager) SetLastPublish(ctx context.Context, id string, t time.Time) error {
	return m.mutate(ctx, id, func(r *Record) { r.LastPublish = t })
}

// IdleRunning returns running, non-exempt records whose last Activity precedes
// now-timeout (strict >). A record labeled idle-stop:off is exempt. timeout<=0
// returns nothing. now is a parameter so the boundary is deterministically testable.
func (m *Manager) IdleRunning(ctx context.Context, now time.Time, timeout time.Duration) ([]*Record, error) {
	if timeout <= 0 {
		return nil, nil
	}
	recs, err := m.List(ctx)
	if err != nil {
		return nil, err
	}
	var out []*Record
	for _, rec := range recs {
		if rec.Status != "running" || rec.Labels["idle-stop"] == "off" {
			continue
		}
		if now.Sub(rec.LastActivity) > timeout {
			out = append(out, rec)
		}
	}
	return out, nil
}

// Backend returns the underlying backend (for exec/ports/files handlers).
func (m *Manager) Backend() Backend { return m.backend }

// Resolve maps a swarm ID to its backend name.
func (m *Manager) Resolve(ctx context.Context, id string) (string, error) {
	rec, err := m.Get(ctx, id)
	if err != nil {
		return "", err
	}
	return rec.BackendName, nil
}

// ResolveVMToID is the reverse of Resolve: it scans all records and returns
// the swarm sandbox ID whose BackendName matches vm.
func (m *Manager) ResolveVMToID(vm string) (string, bool) {
	recs, err := m.List(context.Background())
	if err != nil {
		return "", false
	}
	for _, rec := range recs {
		if rec.BackendName == vm {
			return rec.ID, true
		}
	}
	return "", false
}

// MarkUnreachable records that a remote peer node is considered dead so callers
// can surface it. In v1 this is a no-op on the local Manager because a node
// only owns its own records — the dead node's records remain in their last
// known status. The authoritative "lost" transition happens on the dead node's
// own Reconcile when it rejoins. This method exists so the cluster's
// onNodeDead callback has a concrete call target, and tests can verify the
// wiring without triggering cross-node writes.
//
// NOTE for holistic reviewer: the chosen semantic is "view-level noop". The
// local node does not mutate records owned by the dead peer because it lacks
// truth about whether the peer's backend is actually gone. When the dead node
// rejoins it calls Reconcile and transitions its own records to "lost" if the
// backend confirms they are gone. This avoids split-brain writes.
func (m *Manager) MarkUnreachable(deadNodeID string) {
	// No-op in v1: log the event; do not mutate remote-owned records.
	_ = deadNodeID
}

// Reconcile diffs backend truth against stored records: records whose backend
// sandbox is gone are marked "lost" (spec §7).
func (m *Manager) Reconcile(ctx context.Context) error {
	live, err := m.backend.List(ctx)
	if err != nil {
		return err
	}
	alive := map[string]bool{}
	for _, b := range live {
		alive[b.Name] = true
	}
	recs, err := m.List(ctx)
	if err != nil {
		return err
	}
	for _, rec := range recs {
		if rec.Status == "lost" {
			continue
		}
		if !alive[rec.BackendName] {
			if err := m.mutate(ctx, rec.ID, func(r *Record) { r.Status = "lost" }); err != nil {
				if err == ErrNotFound {
					continue // deleted concurrently; nothing to mark
				}
				return err
			}
			rec.Status = "lost" // reflect in the local snapshot for costSum below
			m.emit("sandbox.lost", rec.ID, nil)
		}
	}
	bc, bm, bd := costSum(recs)
	m.capacity.SetBase(bc, bm, bd)
	return nil
}
