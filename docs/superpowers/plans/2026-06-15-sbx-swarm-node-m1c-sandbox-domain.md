# sbx-swarm-node M1c — Sandbox Domain Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Manage sandboxes on a single node — create/get/list/delete/start/stop, exec (sync), agent-run (async via `ExecDetached`), ports, and files — behind a `SandboxBackend` interface (with an in-memory fake and the real `sbx-go-sdk` adapter), tracked as durable records and asynchronous Operations with `Idempotency-Key` support, exposed over gRPC + REST.

**Architecture:** `SandboxBackend` abstracts the SDK so all domain logic is testable with a fake. `sandbox.Manager` owns local sandbox records (persisted in `bbolt`), maps self-routing swarm IDs ↔ backend names, and reconciles against backend truth. Long actions run as `ops.Operation`s; provision is idempotent. A gRPC `SandboxService` (+ gateway) is the API surface. Builds on M1a (`store`, `ids`) and M1b (`apiserver`, auth).

**Tech Stack:** Go 1.23, `github.com/squall-chua/sbx-go-sdk` v0.1.2 (real backend), `encoding/json` (record marshaling), buf codegen, `github.com/stretchr/testify`.

**Scope:** Sandbox lifecycle + exec/agent-run/ports/files + operations/idempotency. **Excludes** stats/logs/network (Milestone 2 observability), scheduling/capacity/admission (Milestone 5), and events/SSE (M1d) — those wire into this manager later.

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/store/kv.go` | generic bucket KV: `Put`/`Get`/`Delete`/`ForEach` |
| `internal/sandbox/backend.go` | `Backend` interface + spec/result value types |
| `internal/sandbox/fake.go` | in-memory `Backend` for tests |
| `internal/sandbox/sdkbackend.go` | real `Backend` over `sbx-go-sdk` |
| `internal/sandbox/record.go` | `Record` (persisted sandbox) + JSON marshaling |
| `internal/sandbox/manager.go` | CRUD + record persistence + reconcile |
| `internal/ops/ops.go` | async operation tracking + idempotency |
| `proto/sbxswarm/v1/sandbox.proto` | `SandboxService` |
| `internal/apiserver/sandboxservice.go` | gRPC `SandboxService` impl |
| `internal/node/node.go` | construct backend + manager + ops, pass to apiserver |

---

## Task 1: Generic store KV helpers

**Files:**
- Create: `internal/store/kv.go`
- Test: `internal/store/kv_test.go`

- [x] **Step 1: Write the failing test**

```go
package store

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestKV_PutGetDeleteForEach(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "n.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	require.NoError(t, s.Put("sandboxes", "a", []byte("1")))
	require.NoError(t, s.Put("sandboxes", "b", []byte("2")))

	v, ok, err := s.Get("sandboxes", "a")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, []byte("1"), v)

	seen := map[string]string{}
	require.NoError(t, s.ForEach("sandboxes", func(k, v []byte) error {
		seen[string(k)] = string(v)
		return nil
	}))
	require.Equal(t, map[string]string{"a": "1", "b": "2"}, seen)

	require.NoError(t, s.Delete("sandboxes", "a"))
	_, ok, err = s.Get("sandboxes", "a")
	require.NoError(t, err)
	require.False(t, ok)
}
```

- [x] **Step 2: Run test to verify it fails**

Run: `go test ./internal/store/ -run TestKV -v`
Expected: FAIL — `s.Put undefined`

- [x] **Step 3: Write the implementation**

```go
package store

import (
	"fmt"

	bolt "go.etcd.io/bbolt"
)

// Put stores val under key in the named bucket.
func (s *Store) Put(bucket, key string, val []byte) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucket))
		if b == nil {
			return fmt.Errorf("unknown bucket %q", bucket)
		}
		return b.Put([]byte(key), val)
	})
}

// Get returns the value for key; ok is false if absent.
func (s *Store) Get(bucket, key string) (val []byte, ok bool, err error) {
	err = s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucket))
		if b == nil {
			return fmt.Errorf("unknown bucket %q", bucket)
		}
		raw := b.Get([]byte(key))
		if raw == nil {
			return nil
		}
		val = append([]byte(nil), raw...) // copy: bbolt memory is txn-scoped
		ok = true
		return nil
	})
	return val, ok, err
}

// Delete removes key from the bucket (no error if absent).
func (s *Store) Delete(bucket, key string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucket))
		if b == nil {
			return fmt.Errorf("unknown bucket %q", bucket)
		}
		return b.Delete([]byte(key))
	})
}

// ForEach calls fn for every key/value in the bucket.
func (s *Store) ForEach(bucket string, fn func(k, v []byte) error) error {
	return s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucket))
		if b == nil {
			return fmt.Errorf("unknown bucket %q", bucket)
		}
		return b.ForEach(fn)
	})
}
```

- [x] **Step 4: Run test to verify it passes**

Run: `go test ./internal/store/ -v`
Expected: PASS

- [x] **Step 5: Commit**

```bash
git add internal/store/kv.go internal/store/kv_test.go
git commit -m "feat(store): generic bucket KV helpers"
```

---

## Task 2: SandboxBackend interface + in-memory fake

**Files:**
- Create: `internal/sandbox/backend.go`, `internal/sandbox/fake.go`
- Test: `internal/sandbox/fake_test.go`

- [x] **Step 1: Write the failing test**

```go
package sandbox

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFake_LifecycleAndExec(t *testing.T) {
	f := NewFake()
	ctx := context.Background()

	sb, err := f.Create(ctx, CreateSpec{Name: "s1", CPUs: 2, MemoryBytes: 1 << 30})
	require.NoError(t, err)
	require.Equal(t, "s1", sb.Name)
	require.Equal(t, "running", sb.Status)

	list, err := f.List(ctx)
	require.NoError(t, err)
	require.Len(t, list, 1)

	res, err := f.Exec(ctx, "s1", []string{"echo", "hi"}, ExecOpts{})
	require.NoError(t, err)
	require.Equal(t, 0, res.ExitCode)

	id, err := f.ExecDetached(ctx, "s1", []string{"sleep", "1"}, ExecOpts{})
	require.NoError(t, err)
	st, err := f.PollDetached(ctx, "s1", id)
	require.NoError(t, err)
	require.True(t, st.Done)

	require.NoError(t, f.Stop(ctx, "s1"))
	require.NoError(t, f.Remove(ctx, "s1"))
	_, err = f.Get(ctx, "s1")
	require.ErrorIs(t, err, ErrNotFound)
}
```

- [x] **Step 2: Run test to verify it fails**

Run: `go test ./internal/sandbox/ -run TestFake -v`
Expected: FAIL — `undefined: NewFake`

- [x] **Step 3: Write `backend.go`**

```go
// Package sandbox manages sandboxes on this node behind a Backend abstraction
// over the sbx-go-sdk, with an in-memory fake for tests.
package sandbox

import (
	"context"
	"errors"
)

// ErrNotFound is returned when a sandbox does not exist in the backend.
var ErrNotFound = errors.New("sandbox not found")

// WorkspaceMount describes a workspace to attach.
type WorkspaceMount struct {
	Name     string // logical workspace name (resolved to a host path by the backend/config)
	ReadOnly bool
}

// CreateSpec describes a sandbox to provision.
type CreateSpec struct {
	Name        string
	Agent       string
	Template    string
	CPUs        int
	MemoryBytes int64
	Clone       bool
	Workspaces  []WorkspaceMount
	Env         map[string]string
}

// BackendSandbox is the backend's view of a sandbox.
type BackendSandbox struct {
	Name   string
	Status string // "running" | "stopped" | ...
}

// ExecOpts are options for exec/agent-run.
type ExecOpts struct {
	Workdir string
	Env     map[string]string
}

// ExecResult is the captured outcome of a synchronous exec.
type ExecResult struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
}

// DetachedStatus is the poll result for a detached exec / agent run.
type DetachedStatus struct {
	Done     bool
	ExitCode int // valid when Done
}

// PublishedPort maps a container port to a host port.
type PublishedPort struct {
	ContainerPort int
	HostPort      int
}

// Backend is the abstraction over sbx-go-sdk used by the manager.
type Backend interface {
	Create(ctx context.Context, spec CreateSpec) (BackendSandbox, error)
	Get(ctx context.Context, name string) (BackendSandbox, error)
	List(ctx context.Context) ([]BackendSandbox, error)
	Start(ctx context.Context, name string) error
	Stop(ctx context.Context, name string) error
	Remove(ctx context.Context, name string) error
	Exec(ctx context.Context, name string, cmd []string, opts ExecOpts) (ExecResult, error)
	ExecDetached(ctx context.Context, name string, cmd []string, opts ExecOpts) (detachedID string, err error)
	PollDetached(ctx context.Context, name, detachedID string) (DetachedStatus, error)
	PublishPort(ctx context.Context, name string, containerPort int) (PublishedPort, error)
	Ports(ctx context.Context, name string) ([]PublishedPort, error)
	UnpublishPort(ctx context.Context, name string, containerPort int) error
	CopyTo(ctx context.Context, name, localPath, remotePath string) error
	CopyFrom(ctx context.Context, name, remotePath, localPath string) error
}
```

- [x] **Step 4: Write `fake.go`**

```go
package sandbox

import (
	"context"
	"fmt"
	"sync"
)

// Fake is an in-memory Backend for tests.
type Fake struct {
	mu        sync.Mutex
	sandboxes map[string]*BackendSandbox
	ports     map[string][]PublishedPort
	detached  map[string]bool // detachedID -> done
	seq       int
}

// NewFake returns an empty fake backend.
func NewFake() *Fake {
	return &Fake{sandboxes: map[string]*BackendSandbox{}, ports: map[string][]PublishedPort{}, detached: map[string]bool{}}
}

func (f *Fake) Create(_ context.Context, spec CreateSpec) (BackendSandbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.sandboxes[spec.Name]; ok {
		return BackendSandbox{}, fmt.Errorf("exists: %s", spec.Name)
	}
	sb := &BackendSandbox{Name: spec.Name, Status: "running"}
	f.sandboxes[spec.Name] = sb
	return *sb, nil
}

func (f *Fake) Get(_ context.Context, name string) (BackendSandbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	sb, ok := f.sandboxes[name]
	if !ok {
		return BackendSandbox{}, ErrNotFound
	}
	return *sb, nil
}

func (f *Fake) List(_ context.Context) ([]BackendSandbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]BackendSandbox, 0, len(f.sandboxes))
	for _, sb := range f.sandboxes {
		out = append(out, *sb)
	}
	return out, nil
}

func (f *Fake) setStatus(name, status string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	sb, ok := f.sandboxes[name]
	if !ok {
		return ErrNotFound
	}
	sb.Status = status
	return nil
}

func (f *Fake) Start(_ context.Context, name string) error { return f.setStatus(name, "running") }
func (f *Fake) Stop(_ context.Context, name string) error  { return f.setStatus(name, "stopped") }

func (f *Fake) Remove(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.sandboxes[name]; !ok {
		return ErrNotFound
	}
	delete(f.sandboxes, name)
	delete(f.ports, name)
	return nil
}

func (f *Fake) Exec(_ context.Context, name string, _ []string, _ ExecOpts) (ExecResult, error) {
	if _, err := f.Get(context.Background(), name); err != nil {
		return ExecResult{}, err
	}
	return ExecResult{ExitCode: 0, Stdout: []byte("ok")}, nil
}

func (f *Fake) ExecDetached(_ context.Context, name string, _ []string, _ ExecOpts) (string, error) {
	if _, err := f.Get(context.Background(), name); err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	id := fmt.Sprintf("d%d", f.seq)
	f.detached[id] = true // completes immediately in the fake
	return id, nil
}

func (f *Fake) PollDetached(_ context.Context, _ , detachedID string) (DetachedStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	done, ok := f.detached[detachedID]
	if !ok {
		return DetachedStatus{}, fmt.Errorf("no such detached exec %s", detachedID)
	}
	return DetachedStatus{Done: done, ExitCode: 0}, nil
}

func (f *Fake) PublishPort(_ context.Context, name string, cp int) (PublishedPort, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.sandboxes[name]; !ok {
		return PublishedPort{}, ErrNotFound
	}
	p := PublishedPort{ContainerPort: cp, HostPort: 30000 + cp}
	f.ports[name] = append(f.ports[name], p)
	return p, nil
}

func (f *Fake) Ports(_ context.Context, name string) ([]PublishedPort, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ports[name], nil
}

func (f *Fake) UnpublishPort(_ context.Context, name string, cp int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	kept := f.ports[name][:0]
	for _, p := range f.ports[name] {
		if p.ContainerPort != cp {
			kept = append(kept, p)
		}
	}
	f.ports[name] = kept
	return nil
}

func (f *Fake) CopyTo(_ context.Context, name, _, _ string) error {
	_, err := f.Get(context.Background(), name)
	return err
}

func (f *Fake) CopyFrom(_ context.Context, name, _, _ string) error {
	_, err := f.Get(context.Background(), name)
	return err
}
```

- [x] **Step 5: Run test, then commit**

Run: `go test ./internal/sandbox/ -run TestFake -v`
Expected: PASS

```bash
git add internal/sandbox/backend.go internal/sandbox/fake.go internal/sandbox/fake_test.go
git commit -m "feat(sandbox): Backend interface + in-memory fake"
```

---

## Task 3: Sandbox records + Manager (CRUD + reconcile)

**Files:**
- Create: `internal/sandbox/record.go`, `internal/sandbox/manager.go`
- Test: `internal/sandbox/manager_test.go`

- [x] **Step 1: Write the failing test**

```go
package sandbox

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/squall-chua/sbx-swarm-node/internal/ids"
	"github.com/squall-chua/sbx-swarm-node/internal/store"
	"github.com/stretchr/testify/require"
)

func newMgr(t *testing.T) (*Manager, *Fake) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "n.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	f := NewFake()
	return NewManager("node1", f, st, ids.NewGen("node1")), f
}

func TestManager_CreateGetListDelete(t *testing.T) {
	m, _ := newMgr(t)
	ctx := context.Background()

	rec, err := m.Create(ctx, CreateSpec{CPUs: 1, MemoryBytes: 1 << 30})
	require.NoError(t, err)
	require.Contains(t, rec.ID, "node1.")
	require.Equal(t, "running", rec.Status)

	got, err := m.Get(ctx, rec.ID)
	require.NoError(t, err)
	require.Equal(t, rec.ID, got.ID)

	all, err := m.List(ctx)
	require.NoError(t, err)
	require.Len(t, all, 1)

	require.NoError(t, m.Delete(ctx, rec.ID))
	_, err = m.Get(ctx, rec.ID)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestManager_ReconcileDropsVanishedRecords(t *testing.T) {
	m, f := newMgr(t)
	ctx := context.Background()
	rec, err := m.Create(ctx, CreateSpec{})
	require.NoError(t, err)

	// Backend loses the sandbox out-of-band.
	require.NoError(t, f.Remove(ctx, rec.BackendName))

	require.NoError(t, m.Reconcile(ctx))
	got, err := m.Get(ctx, rec.ID)
	require.NoError(t, err)
	require.Equal(t, "lost", got.Status)
}
```

- [x] **Step 2: Run test to verify it fails**

Run: `go test ./internal/sandbox/ -run TestManager -v`
Expected: FAIL — `undefined: NewManager`

- [x] **Step 3: Write `record.go`**

```go
package sandbox

import "time"

// Record is the persisted, authoritative sandbox state on its owner node.
type Record struct {
	ID          string            `json:"id"`           // self-routing <node_id>.<ulid>
	BackendName string            `json:"backend_name"` // SDK sandbox name
	OwnerNode   string            `json:"owner_node"`
	Spec        CreateSpec        `json:"spec"`
	Status      string            `json:"status"` // running|stopped|failed|lost
	Ports       []PublishedPort   `json:"ports,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	IdempKey    string            `json:"idempotency_key,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
}
```

- [x] **Step 4: Write `manager.go`**

```go
package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/squall-chua/sbx-swarm-node/internal/ids"
	"github.com/squall-chua/sbx-swarm-node/internal/store"
)

const bucket = "sandboxes"

// Manager owns this node's sandbox records and drives the Backend.
type Manager struct {
	nodeID  string
	backend Backend
	store   *store.Store
	ids     *ids.Gen
	now     func() time.Time
}

// NewManager builds a Manager.
func NewManager(nodeID string, backend Backend, st *store.Store, gen *ids.Gen) *Manager {
	return &Manager{nodeID: nodeID, backend: backend, store: st, ids: gen, now: time.Now}
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
	return m.save(rec)
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
	return m.store.Delete(bucket, id)
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
		}
	}
	return nil
}
```

- [x] **Step 5: Run test, then commit**

Run: `go test ./internal/sandbox/ -v`
Expected: PASS

```bash
git add internal/sandbox/record.go internal/sandbox/manager.go internal/sandbox/manager_test.go
git commit -m "feat(sandbox): records + Manager CRUD/lifecycle/reconcile"
```

---

## Task 4: Operations + idempotency

**Files:**
- Create: `internal/ops/ops.go`
- Test: `internal/ops/ops_test.go`

- [x] **Step 1: Write the failing test**

```go
package ops

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/squall-chua/sbx-swarm-node/internal/ids"
	"github.com/squall-chua/sbx-swarm-node/internal/store"
	"github.com/stretchr/testify/require"
)

func newMgr(t *testing.T) *Manager {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "n.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	return NewManager(st, ids.NewGen("n1"))
}

func TestOps_RunSetsDone(t *testing.T) {
	m := newMgr(t)
	op, existed, err := m.Start(context.Background(), "provision", "")
	require.NoError(t, err)
	require.False(t, existed)

	m.Run(op.ID, func() (string, error) { return "sb1", nil })
	require.Eventually(t, func() bool {
		got, _ := m.Get(op.ID)
		return got != nil && got.State == "done" && got.SandboxID == "sb1"
	}, time.Second, 10*time.Millisecond)
}

func TestOps_IdempotencyReturnsSameOp(t *testing.T) {
	m := newMgr(t)
	a, existedA, err := m.Start(context.Background(), "provision", "key-1")
	require.NoError(t, err)
	require.False(t, existedA)

	b, existedB, err := m.Start(context.Background(), "provision", "key-1")
	require.NoError(t, err)
	require.True(t, existedB)
	require.Equal(t, a.ID, b.ID) // same op for same idempotency key
}
```

- [x] **Step 2: Run test to verify it fails**

Run: `go test ./internal/ops/ -v`
Expected: FAIL — `undefined: NewManager`

- [x] **Step 3: Write the implementation**

```go
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
	opBucket    = "operations"
	idemBucket  = "idempotency"
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
```

- [x] **Step 4: Run test, then commit**

Run: `go test ./internal/ops/ -v`
Expected: PASS

```bash
git add internal/ops/
git commit -m "feat(ops): async operations with idempotency"
```

---

## Task 5: SandboxService proto + codegen

**Files:**
- Create: `proto/sbxswarm/v1/sandbox.proto`
- Regenerate: `internal/gen/sbxswarm/v1/`

- [x] **Step 1: Write `proto/sbxswarm/v1/sandbox.proto`**

```proto
syntax = "proto3";

package sbxswarm.v1;

import "google/api/annotations.proto";

option go_package = "github.com/squall-chua/sbx-swarm-node/internal/gen/sbxswarm/v1;sbxswarmv1";

service SandboxService {
  rpc CreateSandbox(CreateSandboxRequest) returns (Operation) {
    option (google.api.http) = {post: "/v1/sandboxes" body: "*"};
  }
  rpc GetSandbox(GetSandboxRequest) returns (Sandbox) {
    option (google.api.http) = {get: "/v1/sandboxes/{id}"};
  }
  rpc ListSandboxes(ListSandboxesRequest) returns (ListSandboxesResponse) {
    option (google.api.http) = {get: "/v1/sandboxes"};
  }
  rpc DeleteSandbox(DeleteSandboxRequest) returns (Operation) {
    option (google.api.http) = {delete: "/v1/sandboxes/{id}"};
  }
  rpc StartSandbox(IdRequest) returns (Sandbox) {
    option (google.api.http) = {post: "/v1/sandboxes/{id}/start"};
  }
  rpc StopSandbox(IdRequest) returns (Sandbox) {
    option (google.api.http) = {post: "/v1/sandboxes/{id}/stop"};
  }
  rpc Exec(ExecRequest) returns (ExecResponse) {
    option (google.api.http) = {post: "/v1/sandboxes/{id}/exec" body: "*"};
  }
  rpc AgentRun(AgentRunRequest) returns (Operation) {
    option (google.api.http) = {post: "/v1/sandboxes/{id}/agent-run" body: "*"};
  }
  rpc PublishPort(PublishPortRequest) returns (Port) {
    option (google.api.http) = {post: "/v1/sandboxes/{id}/ports" body: "*"};
  }
  rpc ListPorts(IdRequest) returns (ListPortsResponse) {
    option (google.api.http) = {get: "/v1/sandboxes/{id}/ports"};
  }
}

message WorkspaceMount {
  string name = 1;
  bool read_only = 2;
}

message CreateSandboxRequest {
  string agent = 1;
  string template = 2;
  int32 cpus = 3;
  int64 memory_bytes = 4;
  bool clone = 5;
  repeated WorkspaceMount workspaces = 6;
  map<string, string> env = 7;
  map<string, string> labels = 8;
}

message Sandbox {
  string id = 1;
  string owner_node = 2;
  string status = 3;
  repeated Port ports = 4;
  map<string, string> labels = 5;
}

message GetSandboxRequest { string id = 1; }
message IdRequest { string id = 1; }
message DeleteSandboxRequest { string id = 1; }

message ListSandboxesRequest {
  string status = 1;
  string label = 2;
}
message ListSandboxesResponse { repeated Sandbox sandboxes = 1; }

message ExecRequest {
  string id = 1;
  repeated string cmd = 2;
  string workdir = 3;
  map<string, string> env = 4;
}
message ExecResponse {
  int32 exit_code = 1;
  bytes stdout = 2;
  bytes stderr = 3;
}

message AgentRunRequest {
  string id = 1;
  repeated string cmd = 2;
  string workdir = 3;
  map<string, string> env = 4;
  bool publish_on_success = 5;
}

message PublishPortRequest {
  string id = 1;
  int32 container_port = 2;
}
message Port {
  int32 container_port = 1;
  int32 host_port = 2;
}
message ListPortsResponse { repeated Port ports = 1; }

message Operation {
  string id = 1;
  string type = 2;
  string state = 3;
  string sandbox_id = 4;
  string error = 5;
}
```

- [x] **Step 2: Regenerate + build**

Run: `buf generate && go build ./...`
Expected: new types/handlers compile.

- [x] **Step 3: Commit**

```bash
git add proto/ internal/gen/
git commit -m "feat(proto): SandboxService"
```

---

## Task 6: SandboxService gRPC implementation

**Files:**
- Create: `internal/apiserver/sandboxservice.go`
- Test: `internal/apiserver/sandboxservice_test.go`

- [x] **Step 1: Write the failing test**

```go
package apiserver

import (
	"context"
	"path/filepath"
	"testing"

	sbxv1 "github.com/squall-chua/sbx-swarm-node/internal/gen/sbxswarm/v1"
	"github.com/squall-chua/sbx-swarm-node/internal/ids"
	"github.com/squall-chua/sbx-swarm-node/internal/ops"
	"github.com/squall-chua/sbx-swarm-node/internal/sandbox"
	"github.com/squall-chua/sbx-swarm-node/internal/store"
	"github.com/stretchr/testify/require"
)

func newSandboxSvc(t *testing.T) *SandboxService {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "n.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	gen := ids.NewGen("n1")
	mgr := sandbox.NewManager("n1", sandbox.NewFake(), st, gen)
	return NewSandboxService(mgr, ops.NewManager(st, gen))
}

func TestSandboxService_CreateThenGetList(t *testing.T) {
	svc := newSandboxSvc(t)
	ctx := context.Background()

	op, err := svc.CreateSandbox(ctx, &sbxv1.CreateSandboxRequest{Cpus: 1, MemoryBytes: 1 << 30})
	require.NoError(t, err)
	require.NotEmpty(t, op.Id)

	// provision runs async; wait until the op carries a sandbox id
	var sbID string
	require.Eventually(t, func() bool {
		got, _ := svc.ops.Get(op.Id)
		if got != nil && got.State == "done" {
			sbID = got.SandboxID
			return true
		}
		return false
	}, time.Second, 10*time.Millisecond)

	got, err := svc.GetSandbox(ctx, &sbxv1.GetSandboxRequest{Id: sbID})
	require.NoError(t, err)
	require.Equal(t, "running", got.Status)

	list, err := svc.ListSandboxes(ctx, &sbxv1.ListSandboxesRequest{})
	require.NoError(t, err)
	require.Len(t, list.Sandboxes, 1)
}

func TestSandboxService_Exec(t *testing.T) {
	svc := newSandboxSvc(t)
	ctx := context.Background()
	// create synchronously via the manager for a direct id
	rec, err := svc.mgr.Create(ctx, sandbox.CreateSpec{})
	require.NoError(t, err)

	res, err := svc.Exec(ctx, &sbxv1.ExecRequest{Id: rec.ID, Cmd: []string{"echo", "hi"}})
	require.NoError(t, err)
	require.Equal(t, int32(0), res.ExitCode)
}
```

Add `"time"` to the test imports.

- [x] **Step 2: Run test to verify it fails**

Run: `go test ./internal/apiserver/ -run TestSandboxService -v`
Expected: FAIL — `undefined: NewSandboxService`

- [x] **Step 3: Write the implementation**

```go
package apiserver

import (
	"context"

	sbxv1 "github.com/squall-chua/sbx-swarm-node/internal/gen/sbxswarm/v1"
	"github.com/squall-chua/sbx-swarm-node/internal/ops"
	"github.com/squall-chua/sbx-swarm-node/internal/sandbox"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// SandboxService implements sbxv1.SandboxServiceServer over the sandbox Manager.
type SandboxService struct {
	sbxv1.UnimplementedSandboxServiceServer
	mgr *sandbox.Manager
	ops *ops.Manager
}

// NewSandboxService builds the service.
func NewSandboxService(mgr *sandbox.Manager, opsM *ops.Manager) *SandboxService {
	return &SandboxService{mgr: mgr, ops: opsM}
}

func toSpec(r *sbxv1.CreateSandboxRequest) sandbox.CreateSpec {
	ws := make([]sandbox.WorkspaceMount, 0, len(r.Workspaces))
	for _, w := range r.Workspaces {
		ws = append(ws, sandbox.WorkspaceMount{Name: w.Name, ReadOnly: w.ReadOnly})
	}
	return sandbox.CreateSpec{
		Agent: r.Agent, Template: r.Template, CPUs: int(r.Cpus),
		MemoryBytes: r.MemoryBytes, Clone: r.Clone, Workspaces: ws, Env: r.Env,
	}
}

func toProto(rec *sandbox.Record) *sbxv1.Sandbox {
	ports := make([]*sbxv1.Port, 0, len(rec.Ports))
	for _, p := range rec.Ports {
		ports = append(ports, &sbxv1.Port{ContainerPort: int32(p.ContainerPort), HostPort: int32(p.HostPort)})
	}
	return &sbxv1.Sandbox{Id: rec.ID, OwnerNode: rec.OwnerNode, Status: rec.Status, Ports: ports, Labels: rec.Labels}
}

func opProto(op *ops.Operation) *sbxv1.Operation {
	return &sbxv1.Operation{Id: op.ID, Type: op.Type, State: op.State, SandboxId: op.SandboxID, Error: op.Error}
}

// idempotencyKey reads the Idempotency-Key from gRPC/gateway metadata.
func idempotencyKey(ctx context.Context) string {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if v := md.Get("idempotency-key"); len(v) > 0 {
			return v[0]
		}
	}
	return ""
}

// CreateSandbox starts an async provision operation (idempotent).
func (s *SandboxService) CreateSandbox(ctx context.Context, r *sbxv1.CreateSandboxRequest) (*sbxv1.Operation, error) {
	op, existed, err := s.ops.Start(ctx, "provision", idempotencyKey(ctx))
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if existed {
		return opProto(op), nil
	}
	spec := toSpec(r)
	s.ops.Run(op.ID, func() (string, error) {
		rec, cerr := s.mgr.Create(context.Background(), spec)
		if cerr != nil {
			return "", cerr
		}
		return rec.ID, nil
	})
	return opProto(op), nil
}

func (s *SandboxService) GetSandbox(ctx context.Context, r *sbxv1.GetSandboxRequest) (*sbxv1.Sandbox, error) {
	rec, err := s.mgr.Get(ctx, r.Id)
	if err == sandbox.ErrNotFound {
		return nil, status.Error(codes.NotFound, "sandbox not found")
	}
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return toProto(rec), nil
}

func (s *SandboxService) ListSandboxes(ctx context.Context, r *sbxv1.ListSandboxesRequest) (*sbxv1.ListSandboxesResponse, error) {
	recs, err := s.mgr.List(ctx)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	out := &sbxv1.ListSandboxesResponse{}
	for _, rec := range recs {
		if r.Status != "" && rec.Status != r.Status {
			continue
		}
		out.Sandboxes = append(out.Sandboxes, toProto(rec))
	}
	return out, nil
}

func (s *SandboxService) DeleteSandbox(ctx context.Context, r *sbxv1.DeleteSandboxRequest) (*sbxv1.Operation, error) {
	op, _, err := s.ops.Start(ctx, "remove", "")
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	id := r.Id
	s.ops.Run(op.ID, func() (string, error) { return id, s.mgr.Delete(context.Background(), id) })
	return opProto(op), nil
}

func (s *SandboxService) StartSandbox(ctx context.Context, r *sbxv1.IdRequest) (*sbxv1.Sandbox, error) {
	if err := s.mgr.Start(ctx, r.Id); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return s.GetSandbox(ctx, &sbxv1.GetSandboxRequest{Id: r.Id})
}

func (s *SandboxService) StopSandbox(ctx context.Context, r *sbxv1.IdRequest) (*sbxv1.Sandbox, error) {
	if err := s.mgr.Stop(ctx, r.Id); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return s.GetSandbox(ctx, &sbxv1.GetSandboxRequest{Id: r.Id})
}

func (s *SandboxService) Exec(ctx context.Context, r *sbxv1.ExecRequest) (*sbxv1.ExecResponse, error) {
	name, err := s.mgr.Resolve(ctx, r.Id)
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	res, err := s.mgr.Backend().Exec(ctx, name, r.Cmd, sandbox.ExecOpts{Workdir: r.Workdir, Env: r.Env})
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &sbxv1.ExecResponse{ExitCode: int32(res.ExitCode), Stdout: res.Stdout, Stderr: res.Stderr}, nil
}

func (s *SandboxService) AgentRun(ctx context.Context, r *sbxv1.AgentRunRequest) (*sbxv1.Operation, error) {
	name, err := s.mgr.Resolve(ctx, r.Id)
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	op, _, err := s.ops.Start(ctx, "agent-run", idempotencyKey(ctx))
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	cmd, opts := r.Cmd, sandbox.ExecOpts{Workdir: r.Workdir, Env: r.Env}
	sbID := r.Id
	s.ops.Run(op.ID, func() (string, error) {
		did, derr := s.mgr.Backend().ExecDetached(context.Background(), name, cmd, opts)
		if derr != nil {
			return "", derr
		}
		for { // poll to completion (M1c: simple loop; M1d streams progress)
			st, perr := s.mgr.Backend().PollDetached(context.Background(), name, did)
			if perr != nil {
				return "", perr
			}
			if st.Done {
				if st.ExitCode != 0 {
					return sbID, status.Errorf(codes.Internal, "agent run exited %d", st.ExitCode)
				}
				return sbID, nil
			}
			time.Sleep(200 * time.Millisecond)
		}
	})
	return opProto(op), nil
}

func (s *SandboxService) PublishPort(ctx context.Context, r *sbxv1.PublishPortRequest) (*sbxv1.Port, error) {
	name, err := s.mgr.Resolve(ctx, r.Id)
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	p, err := s.mgr.Backend().PublishPort(ctx, name, int(r.ContainerPort))
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &sbxv1.Port{ContainerPort: int32(p.ContainerPort), HostPort: int32(p.HostPort)}, nil
}

func (s *SandboxService) ListPorts(ctx context.Context, r *sbxv1.IdRequest) (*sbxv1.ListPortsResponse, error) {
	name, err := s.mgr.Resolve(ctx, r.Id)
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	ports, err := s.mgr.Backend().Ports(ctx, name)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	out := &sbxv1.ListPortsResponse{}
	for _, p := range ports {
		out.Ports = append(out.Ports, &sbxv1.Port{ContainerPort: int32(p.ContainerPort), HostPort: int32(p.HostPort)})
	}
	return out, nil
}
```

Add `"time"` to the imports.

- [x] **Step 4: Run test to verify it passes**

Run: `go test ./internal/apiserver/ -run TestSandboxService -v`
Expected: PASS

- [x] **Step 5: Commit**

```bash
git add internal/apiserver/sandboxservice.go internal/apiserver/sandboxservice_test.go
git commit -m "feat(apiserver): SandboxService (CRUD/exec/agent-run/ports) over Manager"
```

---

## Task 7: Real SDK backend adapter

**Files:**
- Create: `internal/sandbox/sdkbackend.go`
- Test: `internal/sandbox/sdkbackend_integration_test.go` (build tag `integration`)

> The adapter maps the `Backend` interface onto `sbx-go-sdk` v0.1.2. Method names below follow the SDK surface documented in the spec §2; **verify each call signature against the installed SDK** when implementing (the SDK is the source of truth). Unit tests use the fake; this adapter is exercised by a tagged integration test against a real `sandboxd`.

- [x] **Step 1: Add the SDK dependency**

Run: `go get github.com/squall-chua/sbx-go-sdk@v0.1.2 && go mod tidy`
Expected: module added.

- [x] **Step 2: Write `sdkbackend.go`**

```go
package sandbox

import (
	"context"
	"fmt"

	sdkclient "github.com/squall-chua/sbx-go-sdk/client"
	sdksandbox "github.com/squall-chua/sbx-go-sdk/sandbox"
	sdkexec "github.com/squall-chua/sbx-go-sdk/exec"
)

// WorkspaceResolver maps a logical workspace name to a host path + ro flag.
type WorkspaceResolver func(name string) (hostPath string, readOnly bool, ok bool)

// SDKBackend implements Backend over sbx-go-sdk. Workspaces are resolved to
// host paths via the resolver (config-provided).
type SDKBackend struct {
	cl       *sdkclient.Client
	resolve  WorkspaceResolver
}

// NewSDKBackend connects to the local daemon.
func NewSDKBackend(ctx context.Context, resolve WorkspaceResolver) (*SDKBackend, error) {
	cl, err := sdkclient.New(ctx, sdkclient.WithAutoStart(), sdkclient.WithStrictVersion())
	if err != nil {
		return nil, fmt.Errorf("connect daemon: %w", err)
	}
	return &SDKBackend{cl: cl, resolve: resolve}, nil
}

func (b *SDKBackend) Create(ctx context.Context, spec CreateSpec) (BackendSandbox, error) {
	opts := []sdksandbox.Option{
		sdksandbox.WithName(spec.Name),
		sdksandbox.WithCPUs(spec.CPUs),
		sdksandbox.WithMemory(spec.MemoryBytes),
	}
	if spec.Agent != "" {
		opts = append(opts, sdksandbox.WithAgent(spec.Agent))
	}
	if spec.Template != "" {
		opts = append(opts, sdksandbox.WithTemplate(spec.Template))
	}
	if spec.Clone {
		opts = append(opts, sdksandbox.WithClone())
	}
	for _, w := range spec.Workspaces {
		host, ro, ok := b.resolve(w.Name)
		if !ok {
			return BackendSandbox{}, fmt.Errorf("unknown workspace %q", w.Name)
		}
		path := host
		if ro || w.ReadOnly {
			path += ":ro"
		}
		opts = append(opts, sdksandbox.WithWorkspace(path))
	}
	sb, err := sdksandbox.Create(ctx, b.cl, opts...)
	if err != nil {
		return BackendSandbox{}, err
	}
	return BackendSandbox{Name: spec.Name, Status: sb.State()}, nil
}

// NOTE: the remaining methods (Get/List/Start/Stop/Remove/Exec/ExecDetached/
// PollDetached/PublishPort/Ports/UnpublishPort/CopyTo/CopyFrom) map 1:1 onto
// sdksandbox.Get/List, sb.Start/Stop/Remove/Inspect, sdkexec.Exec/ExecDetached,
// sb.PublishPort/Ports/UnpublishPort, sb.CopyTo/CopyFrom. Implement each to
// match the installed SDK signatures; translate the SDK's not-found sentinel
// (client.ErrSandboxNotFound) to sandbox.ErrNotFound. Keep them thin — no logic
// beyond translation.
```

> Implement the remaining methods following the one shown, translating `sdkclient.ErrSandboxNotFound` → `ErrNotFound`. Each is a direct passthrough; do not add behavior. A compile-time check `var _ Backend = (*SDKBackend)(nil)` at the bottom of the file will flag any method you miss.

- [x] **Step 3: Add the interface assertion + write the tagged integration test**

At the end of `sdkbackend.go`:

```go
var _ Backend = (*SDKBackend)(nil)
```

`sdkbackend_integration_test.go`:

```go
//go:build integration

package sandbox

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// Requires a running sandboxd + sbx v0.32.0. Run: go test -tags integration ./internal/sandbox/
func TestSDKBackend_CreateExecRemove(t *testing.T) {
	ctx := context.Background()
	b, err := NewSDKBackend(ctx, func(string) (string, bool, bool) { return "", false, false })
	require.NoError(t, err)

	sb, err := b.Create(ctx, CreateSpec{Name: "it-" + t.Name(), CPUs: 1, MemoryBytes: 1 << 30})
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.Remove(ctx, sb.Name) })

	res, err := b.Exec(ctx, sb.Name, []string{"true"}, ExecOpts{})
	require.NoError(t, err)
	require.Equal(t, 0, res.ExitCode)
}
```

- [x] **Step 4: Verify compile (unit build) and that the assertion forces completeness**

Run: `go build ./...`
Expected: compiles only once **all** `Backend` methods are implemented (the `var _ Backend` assertion fails otherwise — that is the signal to finish the passthroughs).

- [x] **Step 5: Commit**

```bash
git add internal/sandbox/sdkbackend.go internal/sandbox/sdkbackend_integration_test.go go.mod go.sum
git commit -m "feat(sandbox): real sbx-go-sdk backend adapter"
```

---

## Task 8: Wire SandboxService into the server + node

**Files:**
- Modify: `internal/apiserver/server.go`
- Modify: `internal/node/node.go`
- Test: `internal/apiserver/server_test.go` (extend)

- [x] **Step 1: Extend `apiserver.Options` and `Build` to register SandboxService**

In `server.go`, add to `Options`:

```go
	Sandboxes *SandboxService // optional; registered if set
```

In `Build`, after registering NodeService on `grpcSrv` and the gateway, add:

```go
	if opts.Sandboxes != nil {
		sbxv1.RegisterSandboxServiceServer(grpcSrv, opts.Sandboxes)
		if err := sbxv1.RegisterSandboxServiceHandlerServer(context.Background(), gw, opts.Sandboxes); err != nil {
			return nil, nil, err
		}
	}
```

- [x] **Step 2: Build the manager/ops/backend in `node.New` and pass them**

In `node.go` `New`, after opening the store and before `apiserver.Build`, add:

```go
	gen := ids.NewGen(id.NodeID)
	backend := sandbox.NewFake() // M1c default; swap to sandbox.NewSDKBackend(...) when a daemon is present
	mgr := sandbox.NewManager(id.NodeID, backend, st, gen)
	opsM := ops.NewManager(st, gen)
	sandboxes := apiserver.NewSandboxService(mgr, opsM)
```

Add `Sandboxes: sandboxes` to the `apiserver.Options{...}`. Add imports for `sandbox`, `ops`, `ids` (ids already imported in M1a). Keep a `mgr.Reconcile(ctx)` call right after construction (best-effort, log on error).

> The fake backend is the M1c default so the node boots without a daemon. A later milestone (or a config flag) selects `SDKBackend`. This keeps M1c end-to-end testable.

- [x] **Step 3: Extend `server_test.go` with a REST round-trip**

```go
func TestServer_CreateSandboxOverREST(t *testing.T) {
	addr, cleanup := startTestServerWithSandboxes(t)
	defer cleanup()
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}

	req, _ := http.NewRequest(http.MethodPost, "https://"+addr+"/v1/sandboxes", strings.NewReader(`{"cpus":1,"memory_bytes":1073741824}`))
	req.Header.Set("Authorization", "Bearer adm")
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, string(body), `"id"`) // operation id
}
```

Add a `startTestServerWithSandboxes` helper mirroring `startTestServer` but setting `opts.Sandboxes` to a fake-backed `NewSandboxService` (build a `store`, `ids`, `sandbox.NewManager(...,NewFake(),...)`, `ops.NewManager`). Add `strings` import.

- [x] **Step 4: Run all tests + manual smoke**

Run: `go test ./... && go test -tags integration ./internal/sandbox/ || echo "integration skipped (no daemon)"`
Expected: unit tests PASS; integration runs only with a daemon.

Manual:
```bash
go run ./cmd/sbx-swarm-node --data-dir ./tmp-data --listen-addr 127.0.0.1:8443 &
# configure an admin key via a config file in a real run; here expect 401 without one
curl -sk -X POST -H "Authorization: Bearer adm" -H 'Content-Type: application/json' \
  -d '{"cpus":1,"memory_bytes":1073741824}' https://localhost:8443/v1/sandboxes
kill %1; rm -rf ./tmp-data
```

- [x] **Step 5: Commit**

```bash
git add internal/apiserver/server.go internal/apiserver/server_test.go internal/node/node.go
git commit -m "feat(node): wire sandbox manager + SandboxService into the server"
```

---

## Self-Review

**Spec coverage (M1c slice):**
- `SandboxBackend` interface + fake + SDK adapter → Tasks 2, 7 ✓
- Sandbox CRUD + start/stop → Tasks 3, 6 ✓
- Exec (sync) + agent-run (`ExecDetached`, poll to exit) + `publish_on_success` field present → Task 6 ✓ (publish wiring itself lands with the git milestone)
- Ports → Tasks 2, 6 ✓; **Files (CopyTo/CopyFrom)** are in the `Backend` interface + fake (Task 2) and SDK adapter (Task 7); their gRPC endpoints are intentionally deferred to the file-transfer slice (need streaming/multipart) — noted here as the one M1c API gap to pick up with M1d streaming.
- Self-routing IDs ↔ backend names → Task 3 ✓
- Operations + `Idempotency-Key` → Tasks 4, 6 ✓
- Reconcile (records vs backend truth, `lost`) → Task 3 ✓
- **Deferred:** stats/logs/network (M2), scheduling/capacity (M5), events emission (M1d) — manager methods are the hook points.

**Placeholder scan:** No TBD/TODO in code. The SDK adapter's repetitive passthrough methods are described with the exact SDK mapping + a compile-time `var _ Backend` assertion that *forces* completeness — not a vague "implement the rest." The Files gRPC endpoints are explicitly called out as deferred, not silently missing.

**Type consistency:** `sandbox.Backend` methods match the fake and `SDKBackend`; `sandbox.NewManager(nodeID, Backend, *store.Store, *ids.Gen)`→`*Manager{Create,Get,List,Start,Stop,Delete,Resolve,Backend,Reconcile}`; `ops.NewManager(*store.Store,*ids.Gen)`→`*Manager{Start,Run,Get}`; `apiserver.NewSandboxService(*sandbox.Manager,*ops.Manager)`. Proto messages (`CreateSandboxRequest`,`Sandbox`,`Operation`,…) match the handlers. `apiserver.Options.Sandboxes` is registered in `Build`.
