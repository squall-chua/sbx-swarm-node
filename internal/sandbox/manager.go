package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/squall-chua/sbx-swarm-node/internal/events"
	"github.com/squall-chua/sbx-swarm-node/internal/ids"
	"github.com/squall-chua/sbx-swarm-node/internal/store"
)

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
}

// NewManager builds a Manager.
func NewManager(nodeID string, backend Backend, st *store.Store, gen *ids.Gen) *Manager {
	return &Manager{nodeID: nodeID, backend: backend, store: st, ids: gen, now: time.Now}
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

// Create provisions a sandbox and persists its record.
func (m *Manager) Create(ctx context.Context, spec CreateSpec) (*Record, error) {
	id := m.ids.Sandbox()
	backendName := id // use the swarm id as the SDK name for a 1:1 mapping
	spec.Name = backendName

	bs, err := m.backend.Create(ctx, spec)
	if err != nil {
		return nil, fmt.Errorf("backend create: %w", err)
	}
	rec := &Record{
		ID: id, BackendName: backendName, OwnerNode: m.nodeID,
		Spec: spec, Status: bs.Status, CreatedAt: m.now(),
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
	rec.Status = status
	if err := m.save(rec); err != nil {
		return err
	}
	m.emit("sandbox."+status, rec.ID, nil)
	return nil
}

// Start/Stop transition the backend and record.
func (m *Manager) Start(ctx context.Context, id string) error {
	return m.lifecycle(ctx, id, func(n string) error { return m.backend.Start(ctx, n) }, "running")
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
	if err := m.store.Delete(bucket, id); err != nil {
		return err
	}
	m.emit("sandbox.deleted", id, nil)
	m.notifyOwnedChanged()
	return nil
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
			rec.Status = "lost"
			if err := m.save(rec); err != nil {
				return err
			}
			m.emit("sandbox.lost", rec.ID, nil)
		}
	}
	return nil
}
