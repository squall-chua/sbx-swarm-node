// Package ops tracks asynchronous operations and provision idempotency.
package ops

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
	"time"

	"github.com/squall-chua/sbx-swarm-node/internal/events"
	"github.com/squall-chua/sbx-swarm-node/internal/ids"
	"github.com/squall-chua/sbx-swarm-node/internal/store"
)

const (
	opBucket        = "operations"
	idemBucket      = "idempotency"
	defaultListLimit = 200
)

// Operation is a tracked async unit of work.
type Operation struct {
	ID        string    `json:"id"`
	Type      string    `json:"type"`
	State     string    `json:"state"` // pending|running|done|error
	SandboxID string    `json:"sandbox_id,omitempty"`
	Error     string    `json:"error,omitempty"`
	IdempKey  string    `json:"idempotency_key,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// opCounter counts operations by type and final state. obs.Metrics satisfies
// it; declared here to avoid an ops->obs import.
type opCounter interface {
	IncOp(opType, state string)
}

// Manager creates, runs, and persists operations.
type Manager struct {
	store   *store.Store
	ids     *ids.Gen
	pub     events.Publisher
	metrics opCounter
	mu      sync.Mutex
	now     func() time.Time
}

// NewManager builds an ops manager.
func NewManager(st *store.Store, gen *ids.Gen) *Manager {
	return &Manager{store: st, ids: gen, now: time.Now}
}

// SetPublisher wires an event publisher (optional).
func (m *Manager) SetPublisher(p events.Publisher) { m.pub = p }

// SetMetrics wires an operation counter, incremented when an op reaches a
// terminal state (optional; nil disables counting).
func (m *Manager) SetMetrics(c opCounter) { m.metrics = c }

func (m *Manager) emit(op *Operation) {
	if m.pub != nil {
		m.pub.Publish("operation."+op.State, op.SandboxID, map[string]string{"op_id": op.ID, "type": op.Type})
	}
}

func (m *Manager) put(op *Operation) error {
	op.UpdatedAt = m.now()
	raw, err := json.Marshal(op)
	if err != nil {
		return err
	}
	return m.store.Put(opBucket, op.ID, raw)
}

// Start creates a pending operation. If idempKey is non-empty and already
// mapped to an op, that op is returned with existed=true.
func (m *Manager) Start(_ context.Context, opType, idempKey string) (*Operation, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if idempKey != "" {
		if raw, ok, err := m.store.Get(idemBucket, idempKey); err != nil {
			return nil, false, err
		} else if ok {
			existing, gerr := m.Get(string(raw))
			if gerr == nil && existing != nil {
				return existing, true, nil
			}
		}
	}

	op := &Operation{ID: m.ids.Op(), Type: opType, State: "pending", IdempKey: idempKey, CreatedAt: m.now()}
	if err := m.put(op); err != nil {
		return nil, false, err
	}
	if idempKey != "" {
		if err := m.store.Put(idemBucket, idempKey, []byte(op.ID)); err != nil {
			return nil, false, err
		}
	}
	return op, false, nil
}

// Run executes fn in the background, recording the result on the operation.
func (m *Manager) Run(opID string, fn func() (sandboxID string, err error)) {
	go func() {
		op, err := m.Get(opID)
		if err != nil || op == nil {
			return
		}
		op.State = "running"
		_ = m.put(op)
		m.emit(op)

		sbID, runErr := fn()
		if runErr != nil {
			op.State, op.Error = "error", runErr.Error()
		} else {
			op.State, op.SandboxID = "done", sbID
		}
		_ = m.put(op)
		m.emit(op)
		if m.metrics != nil {
			m.metrics.IncOp(op.Type, op.State)
		}
	}()
}

// RecoverInterrupted marks every operation left in a non-terminal state
// (pending/running) as error, so a node restart does not strand an in-flight
// operation forever (a polling client would hang; a same-idempotency-key retry
// returns the stuck op). Call once at boot, before serving. Returns the number
// of operations swept. Terminal states (done/error) are left untouched.
//
// Log-only: it does NOT emit events (would pollute the SSE replay ring with
// phantom failures from a previous process) or IncOp (would conflate restart
// interruptions with genuine op errors). See spec §1.
func (m *Manager) RecoverInterrupted() (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Collect first — do not mutate the bucket inside its own read iterator.
	var stranded []*Operation
	err := m.store.ForEach(opBucket, func(_, v []byte) error {
		var op Operation
		if uerr := json.Unmarshal(v, &op); uerr != nil {
			return uerr
		}
		if op.State != "done" && op.State != "error" {
			stranded = append(stranded, &op)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	for _, op := range stranded {
		op.State = "error"
		op.Error = "interrupted: node restarted during operation"
		if perr := m.put(op); perr != nil {
			return 0, perr
		}
	}
	return len(stranded), nil
}

// List returns operations newest-first (by CreatedAt), capped at limit. limit
// <= 0 or > defaultListLimit uses defaultListLimit. Reads the durable
// operations bucket — this is the operation history, distinct from the
// best-effort event firehose (ADR-0008).
func (m *Manager) List(limit int) ([]*Operation, error) {
	if limit <= 0 || limit > defaultListLimit {
		limit = defaultListLimit
	}
	var ops []*Operation
	err := m.store.ForEach(opBucket, func(_, v []byte) error {
		var op Operation
		if uerr := json.Unmarshal(v, &op); uerr != nil {
			return uerr
		}
		ops = append(ops, &op)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(ops, func(i, j int) bool { return ops[i].CreatedAt.After(ops[j].CreatedAt) })
	if len(ops) > limit {
		ops = ops[:limit]
	}
	return ops, nil
}

// Get returns an operation by ID.
func (m *Manager) Get(opID string) (*Operation, error) {
	raw, ok, err := m.store.Get(opBucket, opID)
	if err != nil || !ok {
		return nil, err
	}
	var op Operation
	if err := json.Unmarshal(raw, &op); err != nil {
		return nil, err
	}
	return &op, nil
}
