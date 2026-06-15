# sbx-swarm-node M5 — Scheduling Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development or superpowers:executing-plans. Checkbox steps.
>
> **Forward-looking:** depends on M4 (`routing.Table`, gossiped peer `NodeState` with caps/limits/alloc/workspaces/templates), M1c (`Manager`), M2 (`actual_util`). Reconcile signatures against real code.

**Goal:** Constraint-based placement — filter candidate nodes by workspace/template/capability/capacity/labels, score survivors by dominant-resource ratio with a hash tie-break (ADR-0007), then provision on the chosen node with **target-authoritative admission** and retry; soft in-memory reservations with `List()`-derived reconcile (spec §6).

**Architecture:** `scheduler.Schedule(req, candidates) → ordered node ids` is pure and exhaustively unit-tested. A `Coordinator` runs it over the gossiped peer view and attempts provision on each candidate in order; the **target** re-checks its *real* local capacity (`admission`) and either creates or NACKs. Capacity is a soft in-memory reservation reconciled from backend truth.

**Tech Stack:** Go 1.23, M1–M4 stack.

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/scheduler/scheduler.go` | pure filter→score→tiebreak |
| `internal/sandbox/capacity.go` | soft reservations + admission + alloc-from-records |
| `internal/coordinator/coordinator.go` | run scheduler, attempt candidates, retry |
| `proto/sbxswarm/v1/internal.proto` | internal `Provision` RPC (node→node) |
| `internal/apiserver/provision.go` | target-side admission + create |
| `internal/config/config.go` | `Workspaces` (name→path), `ProvisionLimits`, default strategy |
| `internal/node/node.go` | advertise workspaces/templates/caps; wire coordinator into CreateSandbox |

---

## Task 1: Scheduler (filter → score → tiebreak)

**Files:** `internal/scheduler/scheduler.go`, test `internal/scheduler/scheduler_test.go`

- [ ] **Step 1: Failing test**

```go
package scheduler

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func cand(id string, cpuLimit, cpuAlloc, memLimit, memAlloc float64, ws ...string) Candidate {
	m := map[string]bool{}
	for _, w := range ws {
		m[w] = true
	}
	return Candidate{NodeID: id, LimitCPU: cpuLimit, AllocCPU: cpuAlloc, LimitMem: memLimit, AllocMem: memAlloc, Workspaces: m}
}

func TestSchedule_FiltersWorkspaceAndCapacity(t *testing.T) {
	req := Request{CPU: 2, Mem: 4, Workspaces: []string{"repo-foo"}, Strategy: "least-loaded", RequestID: "r1"}
	cands := []Candidate{
		cand("A", 8, 6, 16, 11, "repo-foo", "data"), // eligible, loaded
		cand("B", 16, 1, 32, 1, "repo-bar"),          // missing workspace -> filtered
		cand("C", 16, 4, 32, 6, "repo-foo"),          // eligible, light
	}
	order, err := Schedule(req, cands)
	require.NoError(t, err)
	require.Equal(t, []string{"C", "A"}, order) // least-loaded: C before A; B excluded
}

func TestSchedule_NoEligibleNode(t *testing.T) {
	req := Request{CPU: 100, Mem: 1, Strategy: "least-loaded", RequestID: "r"}
	_, err := Schedule(req, []Candidate{cand("A", 8, 0, 16, 0)})
	require.ErrorIs(t, err, ErrNoEligibleNode)
}

func TestSchedule_BinPackPrefersFuller(t *testing.T) {
	req := Request{CPU: 1, Mem: 1, Strategy: "bin-pack", RequestID: "r"}
	cands := []Candidate{cand("A", 4, 3, 4, 3), cand("C", 4, 0, 4, 0)}
	order, err := Schedule(req, cands)
	require.NoError(t, err)
	require.Equal(t, "A", order[0]) // fuller node first
}

func TestSchedule_TieBreakDeterministicAcrossCalls(t *testing.T) {
	req := Request{CPU: 1, Mem: 1, Strategy: "least-loaded", RequestID: "same"}
	cands := []Candidate{cand("A", 10, 0, 10, 0), cand("B", 10, 0, 10, 0)} // identical load -> tie
	o1, _ := Schedule(req, cands)
	o2, _ := Schedule(req, cands)
	require.Equal(t, o1, o2) // hash(requestID âŠ• nodeID) is stable
}
```

- [ ] **Step 2: Run → FAIL**: `go test ./internal/scheduler/ -v`

- [ ] **Step 3: Implement `scheduler.go`**

```go
// Package scheduler performs constraint-based placement: filter by hard
// predicates, score survivors by dominant-resource ratio, break ties by
// hash(requestID âŠ• nodeID) (ADR-0007).
package scheduler

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"sort"
)

// ErrNoEligibleNode means no candidate passed the filter.
var ErrNoEligibleNode = errors.New("no eligible node")

// Candidate is a node's schedulable view (from gossip).
type Candidate struct {
	NodeID       string
	Workspaces   map[string]bool
	Templates    map[string]bool
	Capabilities map[string]bool
	Labels       map[string]string
	LimitCPU     float64
	LimitMem     float64
	AllocCPU     float64
	AllocMem     float64
	Sandboxes    int
	Cordoned     bool
}

// Request is a provision request's scheduling constraints.
type Request struct {
	CPU, Mem     float64
	Workspaces   []string
	Template     string
	Capabilities []string
	Affinity     map[string]string
	AntiAffinity map[string]string
	Strategy     string // least-loaded(default)|bin-pack|spread
	RequestID    string
}

// Schedule returns eligible node ids best-first.
func Schedule(req Request, cands []Candidate) ([]string, error) {
	var ok []Candidate
	for _, c := range cands {
		if fits(req, c) {
			ok = append(ok, c)
		}
	}
	if len(ok) == 0 {
		return nil, ErrNoEligibleNode
	}
	sort.SliceStable(ok, func(i, j int) bool {
		si, sj := score(req, ok[i]), score(req, ok[j])
		if si != sj {
			if req.Strategy == "bin-pack" {
				return si > sj // fuller first
			}
			return si < sj // least-loaded / spread: lighter first
		}
		return tie(req.RequestID, ok[i].NodeID) < tie(req.RequestID, ok[j].NodeID)
	})
	out := make([]string, len(ok))
	for i, c := range ok {
		out[i] = c.NodeID
	}
	return out, nil
}

func fits(req Request, c Candidate) bool {
	if c.Cordoned {
		return false
	}
	for _, w := range req.Workspaces {
		if !c.Workspaces[w] {
			return false
		}
	}
	if req.Template != "" && !c.Templates[req.Template] {
		return false
	}
	for _, cap := range req.Capabilities {
		if !c.Capabilities[cap] {
			return false
		}
	}
	for k, v := range req.Affinity {
		if c.Labels[k] != v {
			return false
		}
	}
	for k, v := range req.AntiAffinity {
		if c.Labels[k] == v {
			return false
		}
	}
	return c.AllocCPU+req.CPU <= c.LimitCPU && c.AllocMem+req.Mem <= c.LimitMem
}

// score is the post-placement dominant-resource ratio (or sandbox count for spread).
func score(req Request, c Candidate) float64 {
	if req.Strategy == "spread" {
		return float64(c.Sandboxes)
	}
	cpu := ratio(c.AllocCPU+req.CPU, c.LimitCPU)
	mem := ratio(c.AllocMem+req.Mem, c.LimitMem)
	if cpu > mem {
		return cpu
	}
	return mem
}

func ratio(a, b float64) float64 {
	if b == 0 {
		return 1
	}
	return a / b
}

func tie(requestID, nodeID string) uint64 {
	h := sha256.Sum256([]byte(requestID + "\x00" + nodeID))
	return binary.BigEndian.Uint64(h[:8])
}
```

- [ ] **Step 4: Run → PASS, commit**

```bash
go test ./internal/scheduler/ -v
git add internal/scheduler/ && git commit -m "feat(scheduler): filter/score/tiebreak placement (ADR-0007)"
```

---

## Task 2: Capacity accounting (soft reservations + admission)

**Files:** `internal/sandbox/capacity.go`, test `internal/sandbox/capacity_test.go`

- [ ] **Step 1: Failing test**

```go
package sandbox

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCapacity_ReserveAdmitRelease(t *testing.T) {
	cap := NewCapacity(4, 8) // 4 cpu, 8 mem units

	require.True(t, cap.CanAdmit(2, 4))
	rid := cap.Reserve(2, 4)
	require.False(t, cap.CanAdmit(3, 1)) // 2 already reserved, only 2 cpu left
	require.True(t, cap.CanAdmit(2, 4))

	cap.Release(rid)
	require.True(t, cap.CanAdmit(4, 8))
}

func TestCapacity_SetAllocatedFromRecords(t *testing.T) {
	cap := NewCapacity(4, 8)
	cap.SetBase(3, 6) // reconciled from List(): 3 cpu / 6 mem already used
	require.False(t, cap.CanAdmit(2, 1))
	require.True(t, cap.CanAdmit(1, 2))
}
```

- [ ] **Step 2: Run → FAIL**: `go test ./internal/sandbox/ -run TestCapacity -v`

- [ ] **Step 3: Implement `capacity.go`**

```go
package sandbox

import "sync"

// Capacity tracks soft, in-memory CPU/mem accounting against a provision limit.
// The durable truth is SandboxBackend.List(); SetBase is called by reconcile.
type Capacity struct {
	mu              sync.Mutex
	limitCPU        float64
	limitMem        float64
	baseCPU, baseMem float64           // from reconciled records
	resv            map[int]reservation // soft reservations during create
	next            int
}

type reservation struct{ cpu, mem float64 }

// NewCapacity builds a capacity tracker with the given provision limits.
func NewCapacity(limitCPU, limitMem float64) *Capacity {
	return &Capacity{limitCPU: limitCPU, limitMem: limitMem, resv: map[int]reservation{}}
}

func (c *Capacity) usedLocked() (cpu, mem float64) {
	cpu, mem = c.baseCPU, c.baseMem
	for _, r := range c.resv {
		cpu += r.cpu
		mem += r.mem
	}
	return
}

// SetBase sets the reconciled allocation from durable records.
func (c *Capacity) SetBase(cpu, mem float64) { c.mu.Lock(); c.baseCPU, c.baseMem = cpu, mem; c.mu.Unlock() }

// CanAdmit reports whether a request fits within the limit right now.
func (c *Capacity) CanAdmit(cpu, mem float64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	uc, um := c.usedLocked()
	return uc+cpu <= c.limitCPU && um+mem <= c.limitMem
}

// Reserve holds capacity for an in-flight create; returns a reservation id.
func (c *Capacity) Reserve(cpu, mem float64) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := c.next
	c.next++
	c.resv[id] = reservation{cpu: cpu, mem: mem}
	return id
}

// Release frees a reservation (on create success the base is updated by
// reconcile; on failure the reservation simply disappears).
func (c *Capacity) Release(id int) { c.mu.Lock(); delete(c.resv, id); c.mu.Unlock() }

// Snapshot returns current allocated cpu/mem (base + reservations).
func (c *Capacity) Snapshot() (cpu, mem float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.usedLocked()
}
```

- [ ] **Step 4: Run → PASS, commit**

```bash
go test ./internal/sandbox/ -run TestCapacity -v
git add internal/sandbox/capacity.go internal/sandbox/capacity_test.go
git commit -m "feat(sandbox): soft reservation capacity accounting"
```

> Wire into `Manager`: hold a `*Capacity`; `Reconcile` computes base from non-terminal records' `Spec.CPUs`/`MemoryBytes` and calls `SetBase`. Add `AdmitAndCreate(ctx, spec)` that `CanAdmit`→`Reserve`→`backend.Create`→on success leaves reservation until next reconcile (or converts), on failure `Release`. Expose `Capacity().Snapshot()` for the gossiped `AllocCPU/AllocMem`.

---

## Task 3: Coordinator (scheduler + attempt + retry)

**Files:** `internal/coordinator/coordinator.go`, test `internal/coordinator/coordinator_test.go`

- [ ] **Step 1: Failing test** (inject an `attempt` func so we test ordering + retry without real RPC)

```go
package coordinator

import (
	"context"
	"errors"
	"testing"

	"github.com/squall-chua/sbx-swarm-node/internal/scheduler"
	"github.com/stretchr/testify/require"
)

func TestCoordinator_TriesInOrderAndRetriesOnNack(t *testing.T) {
	cands := []scheduler.Candidate{
		{NodeID: "C", LimitCPU: 10, LimitMem: 10, Workspaces: map[string]bool{}},
		{NodeID: "A", LimitCPU: 10, AllocCPU: 5, LimitMem: 10, Workspaces: map[string]bool{}},
	}
	attempts := []string{}
	attempt := func(_ context.Context, nodeID string) (string, error) {
		attempts = append(attempts, nodeID)
		if nodeID == "C" {
			return "", ErrNack // C rejects (admission)
		}
		return nodeID + ".sb", nil
	}

	co := New(func() []scheduler.Candidate { return cands }, attempt)
	sbID, err := co.Provision(context.Background(), scheduler.Request{CPU: 1, Mem: 1, Strategy: "least-loaded", RequestID: "r"})
	require.NoError(t, err)
	require.Equal(t, "A.sb", sbID)
	require.Equal(t, []string{"C", "A"}, attempts) // tried C first (lighter), retried A
}

func TestCoordinator_AllNack(t *testing.T) {
	co := New(
		func() []scheduler.Candidate { return []scheduler.Candidate{{NodeID: "A", LimitCPU: 10, LimitMem: 10}} },
		func(context.Context, string) (string, error) { return "", ErrNack },
	)
	_, err := co.Provision(context.Background(), scheduler.Request{CPU: 1, Mem: 1, RequestID: "r"})
	require.True(t, errors.Is(err, ErrNoCapacity))
}
```

- [ ] **Step 2: Run → FAIL**: `go test ./internal/coordinator/ -v`

- [ ] **Step 3: Implement `coordinator.go`**

```go
// Package coordinator places provision requests: it scores candidates and
// attempts them in order, retrying on a target NACK (admission failure).
package coordinator

import (
	"context"
	"errors"

	"github.com/squall-chua/sbx-swarm-node/internal/scheduler"
)

// ErrNack is returned by an attempt when the target refuses admission.
var ErrNack = errors.New("target nacked")

// ErrNoCapacity means no candidate accepted (all filtered or nacked).
var ErrNoCapacity = errors.New("no node accepted the provision")

// AttemptFunc provisions on a node, returning the new sandbox id or ErrNack.
type AttemptFunc func(ctx context.Context, nodeID string) (sandboxID string, err error)

// Coordinator places provisions over the current candidate view.
type Coordinator struct {
	candidates func() []scheduler.Candidate
	attempt    AttemptFunc
}

// New builds a coordinator.
func New(candidates func() []scheduler.Candidate, attempt AttemptFunc) *Coordinator {
	return &Coordinator{candidates: candidates, attempt: attempt}
}

// Provision runs the scheduler and tries candidates best-first until one
// accepts, returning the sandbox id.
func (c *Coordinator) Provision(ctx context.Context, req scheduler.Request) (string, error) {
	order, err := scheduler.Schedule(req, c.candidates())
	if err != nil {
		return "", err // ErrNoEligibleNode
	}
	for _, nodeID := range order {
		sbID, aerr := c.attempt(ctx, nodeID)
		if aerr == nil {
			return sbID, nil
		}
		if !errors.Is(aerr, ErrNack) {
			return "", aerr // hard error (e.g. transport) — surface it
		}
	}
	return "", ErrNoCapacity
}
```

- [ ] **Step 4: Run → PASS, commit**

```bash
go test ./internal/coordinator/ -v
git add internal/coordinator/ && git commit -m "feat(coordinator): scheduler-driven placement with NACK retry"
```

---

## Task 4: Internal Provision RPC (target admission) + wiring

**Files:** `proto/sbxswarm/v1/internal.proto`, `internal/apiserver/provision.go`, `internal/config/config.go`, `internal/node/node.go`

- [ ] **Step 1: Proto** — an internal `InternalService.Provision(ProvisionRequest) returns (ProvisionReply)` (no gateway annotation; node→node only). `ProvisionRequest` carries the full `CreateSpec` + cpu/mem; `ProvisionReply{accepted bool, sandbox_id, reason}`. Regenerate.

- [ ] **Step 2: Target admission handler `provision.go`** (TDD: a fake-backed manager with a tiny limit; first provision accepted, second over-limit NACKed)

```go
package apiserver

import (
	"context"

	sbxv1 "github.com/squall-chua/sbx-swarm-node/internal/gen/sbxswarm/v1"
	"github.com/squall-chua/sbx-swarm-node/internal/sandbox"
)

// InternalService handles node→node RPCs (provision admission).
type InternalService struct {
	sbxv1.UnimplementedInternalServiceServer
	mgr *sandbox.Manager
}

// NewInternalService builds the internal service.
func NewInternalService(mgr *sandbox.Manager) *InternalService { return &InternalService{mgr: mgr} }

// Provision performs target-authoritative admission against real local capacity,
// then creates. A capacity miss returns accepted=false (the coordinator's NACK).
func (s *InternalService) Provision(ctx context.Context, r *sbxv1.ProvisionRequest) (*sbxv1.ProvisionReply, error) {
	spec := sandbox.CreateSpec{
		Agent: r.Agent, Template: r.Template, CPUs: int(r.Cpus), MemoryBytes: r.MemoryBytes, Clone: r.Clone,
	}
	for _, w := range r.Workspaces {
		spec.Workspaces = append(spec.Workspaces, sandbox.WorkspaceMount{Name: w.Name, ReadOnly: w.ReadOnly})
	}
	rec, err := s.mgr.AdmitAndCreate(ctx, spec)
	if err == sandbox.ErrNoCapacity {
		return &sbxv1.ProvisionReply{Accepted: false, Reason: "no capacity"}, nil
	}
	if err != nil {
		return nil, err
	}
	return &sbxv1.ProvisionReply{Accepted: true, SandboxId: rec.ID}, nil
}
```

(`Manager.AdmitAndCreate` returns `sandbox.ErrNoCapacity` when `Capacity.CanAdmit` is false — add that sentinel.)

- [ ] **Step 3: Config + advertisement** — add `Workspaces []WorkspaceConfig{Name,HostPath,ReadOnly}`, `ProvisionLimits{CPUCores,MemoryBytes}`, `DefaultStrategy`. The node advertises workspace names, template names (from `backend`/`template.List`), capabilities, and `Capacity().Snapshot()` in its `membership.NodeState` (M4 Task 2/7).

- [ ] **Step 4: Wire coordinator into CreateSandbox** — in `node.New`, build the coordinator:
  - `candidates()` builds `[]scheduler.Candidate` from the local node + gossiped peer states (workspaces/templates/caps/limits/alloc/cordoned/sandbox-count).
  - `attempt(ctx, nodeID)`: if local, call `mgr.AdmitAndCreate`; else dial the owner via `peer.Pool` and call `InternalService.Provision`, mapping `accepted=false`→`coordinator.ErrNack`.
  - In `SandboxService.CreateSandbox` (M1c), replace the direct `mgr.Create` inside the op with `coordinator.Provision(ctx, reqFromSpec(spec))`; map `ErrNoEligibleNode`/`ErrNoCapacity` to a failed op with a clear reason.

- [ ] **Step 5: Integration test** (tag `integration`, 2 nodes) — node A coordinates; a request needing workspace only present on B lands on B (forwarded provision); a request exceeding both nodes' limits fails with `no capacity`.

- [ ] **Step 6: Run all + commit**

```bash
go test ./... && buf generate
git add proto/ internal/gen/ internal/apiserver/provision.go internal/config/ internal/node/
git commit -m "feat(scheduling): internal Provision RPC, target admission, coordinator wiring"
```

---

## Self-Review

**Spec coverage (M5):** filter (workspace+template+capability+capacity+labels) + dominant-resource score + hash tiebreak (ADR-0007) → Task 1 ✓; soft reservation + admission + List-derived base (spec §6) → Task 2 ✓; coordinator + NACK retry, target-authoritative admission → Tasks 3,4 ✓; templates/workspaces advertised + filtered (D model) → Task 4 ✓; provision limits config → Task 4 ✓. **Deferred:** `least-actual-load` strategy variant (additive — feed `actual_util` into a `score` branch); placement events already flow via M1d/M4.

**Placeholder scan:** Pure scheduler + capacity + coordinator are fully coded and exhaustively unit-TDD'd. Task 4 wiring is specified by precise behavior + an integration test (the honest level for cross-node provisioning). Helper `Manager.AdmitAndCreate`/`ErrNoCapacity` and `reqFromSpec` are specified concretely.

**Type consistency:** `scheduler.Schedule(Request,[]Candidate)→([]string,error)`+`ErrNoEligibleNode`; `sandbox.NewCapacity(cpu,mem).{CanAdmit,Reserve,Release,SetBase,Snapshot}`; `coordinator.New(candidatesFn,attemptFn).Provision`+`ErrNack`/`ErrNoCapacity`; `apiserver.NewInternalService(*sandbox.Manager).Provision`. Manager gains `AdmitAndCreate`/`Capacity()`/`ErrNoCapacity`.
