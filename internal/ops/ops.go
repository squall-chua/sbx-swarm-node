// Package ops tracks asynchronous operations and provision idempotency.
package ops

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/squall-chua/sbx-swarm-node/internal/ids"
	"github.com/squall-chua/sbx-swarm-node/internal/store"
)

const (
	opBucket   = "operations"
	idemBucket = "idempotency"
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

// Manager creates, runs, and persists operations.
type Manager struct {
	store *store.Store
	ids   *ids.Gen
	mu    sync.Mutex
	now   func() time.Time
}

// NewManager builds an ops manager.
func NewManager(st *store.Store, gen *ids.Gen) *Manager {
	return &Manager{store: st, ids: gen, now: time.Now}
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

		sbID, runErr := fn()
		if runErr != nil {
			op.State, op.Error = "error", runErr.Error()
		} else {
			op.State, op.SandboxID = "done", sbID
		}
		_ = m.put(op)
	}()
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
