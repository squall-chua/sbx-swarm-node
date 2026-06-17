# M5 — Scheduling / Placement Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Constraint-based placement — filter candidate nodes by workspace/template/capability/label/capacity, score survivors by dominant-resource ratio over CPU/mem/disk with a hash tie-break (ADR-0007), then provision on the chosen node with target-authoritative admission and NACK retry.

**Architecture:** Three pure-ish units (`scheduler` filter→score→tiebreak; `sandbox.Capacity` atomic soft-reservation accounting; `coordinator` schedule+attempt+retry) plus an internal node→node `Provision` RPC whose target re-checks real local capacity. Node-gated authz (ADR-0011): admin is enforced once at the entry node's `CreateSandbox`; the internal hop is authorized by node identity. Spec: `docs/superpowers/specs/2026-06-17-m5-scheduling-design.md`.

**Tech Stack:** Go 1.25, M1–M4 stack (memberlist gossip, grpc + grpc-gateway, buf, bbolt store, sbx-go-sdk v0.1.2), `golang.org/x/sys/unix` (already an indirect dep) for host detection.

---

## Units & units-of-measure

- **CPU = cores (float64)**, **memory = KB (float64)** (matches gossip `*MemKB`), **disk = GB (float64)** (matches `Usage.Disk*GB`).
- Module path: `github.com/squall-chua/sbx-swarm-node`. Generated proto alias: `sbxv1 "github.com/squall-chua/sbx-swarm-node/internal/gen/sbxswarm/v1"`.
- **Reconcile-before-coding note:** every signature below was checked against the repo on 2026-06-17. Ignore gopls "undefined/redeclared" noise; trust the real `go` toolchain.

## File Structure

| File | Responsibility |
|---|---|
| `internal/scheduler/scheduler.go` | pure filter → dominant-resource score → hash tiebreak |
| `internal/sandbox/capacity.go` | atomic soft-reservation accounting (TryReserve/Commit/Release/SetBase/Snapshot) |
| `internal/sandbox/hostlimits.go` (+ `_other.go`) | host CPU/mem/disk auto-detect (Linux) + non-Linux fallback |
| `internal/coordinator/coordinator.go` | schedule + attempt-in-order + NACK retry |
| `internal/sandbox/backend.go`, `fake.go`, `sdkbackend.go` | `ListTemplates`; `CreateSpec.DiskGB`; `Resources` type |
| `internal/sandbox/manager.go` | `*Capacity`, `AdmitAndCreate`, `ErrNoCapacity`, `Reconcile`→base recompute |
| `internal/membership/state.go` | `NodeState.Workspaces/Templates/LimitDiskGB/AllocDiskGB` |
| `internal/config/config.go` | `Workspaces`, `DefaultStrategy`, `DefaultSandboxResources`, `ProvisionLimits.DiskGB` |
| `proto/sbxswarm/v1/sandbox.proto` | `CreateSandboxRequest.disk_gb`, `.strategy` |
| `proto/sbxswarm/v1/internal.proto` | `InternalService.Provision` |
| `internal/apiserver/provision.go` | target admission handler |
| `internal/apiserver/server.go` | register `InternalService` (grpc-only); `Options.Internal` |
| `internal/apiserver/authz.go` (+ test) | `internalMethods` node-gated bucket + drift-guard |
| `internal/apiserver/sandboxservice.go` | effective sizing, strategy validation, `PlaceFunc`, requestID=op.ID |
| `internal/node/node.go` | build Capacity + coordinator + candidates + attempt; advertise NodeState; register Internal |
| `internal/membership/scheduling_integration_test.go` | 2-node placement integration test |

---

## Task 1: Scheduler (pure filter → score → tiebreak)

**Files:**
- Create: `internal/scheduler/scheduler.go`
- Test: `internal/scheduler/scheduler_test.go`

- [ ] **Step 1: Write the failing test**

```go
package scheduler

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func cand(id string, cpuLim, cpuAlloc, memLim, memAlloc, diskLim, diskAlloc float64, ws ...string) Candidate {
	m := map[string]bool{}
	for _, w := range ws {
		m[w] = true
	}
	return Candidate{
		NodeID: id, Workspaces: m,
		LimitCPU: cpuLim, AllocCPU: cpuAlloc,
		LimitMem: memLim, AllocMem: memAlloc,
		LimitDisk: diskLim, AllocDisk: diskAlloc,
	}
}

func TestSchedule_FiltersWorkspaceAndCapacity(t *testing.T) {
	req := Request{CPU: 2, Mem: 4, Disk: 1, Workspaces: []string{"repo-foo"}, Strategy: "least-loaded", RequestID: "r1"}
	cands := []Candidate{
		cand("A", 8, 6, 16, 11, 100, 10, "repo-foo", "data"), // eligible, loaded
		cand("B", 16, 1, 32, 1, 100, 1, "repo-bar"),          // missing workspace -> filtered
		cand("C", 16, 4, 32, 6, 100, 5, "repo-foo"),          // eligible, light
	}
	order, err := Schedule(req, cands)
	require.NoError(t, err)
	require.Equal(t, []string{"C", "A"}, order) // least-loaded: C before A; B excluded
}

func TestSchedule_DiskIsDominant(t *testing.T) {
	// A is light on cpu/mem but nearly full on disk; B is the opposite. The
	// dominant-resource max() must pick A as the more-loaded node.
	req := Request{CPU: 1, Mem: 1, Disk: 1, Strategy: "least-loaded", RequestID: "r"}
	cands := []Candidate{
		cand("A", 100, 1, 100, 1, 10, 9), // disk ratio (9+1)/10 = 1.0  -> dominant
		cand("B", 100, 50, 100, 50, 100, 1),
	}
	order, err := Schedule(req, cands)
	require.NoError(t, err)
	require.Equal(t, "B", order[0]) // B less dominant-loaded
}

func TestSchedule_NoEligibleNode(t *testing.T) {
	req := Request{CPU: 100, Mem: 1, Disk: 1, Strategy: "least-loaded", RequestID: "r"}
	_, err := Schedule(req, []Candidate{cand("A", 8, 0, 16, 0, 100, 0)})
	require.ErrorIs(t, err, ErrNoEligibleNode)
}

func TestSchedule_CordonedExcluded(t *testing.T) {
	c := cand("A", 8, 0, 16, 0, 100, 0)
	c.Cordoned = true
	_, err := Schedule(Request{CPU: 1, Mem: 1, Disk: 1, RequestID: "r"}, []Candidate{c})
	require.ErrorIs(t, err, ErrNoEligibleNode)
}

func TestSchedule_BinPackPrefersFuller(t *testing.T) {
	req := Request{CPU: 1, Mem: 1, Disk: 1, Strategy: "bin-pack", RequestID: "r"}
	cands := []Candidate{cand("A", 4, 3, 4, 3, 4, 3), cand("C", 4, 0, 4, 0, 4, 0)}
	order, err := Schedule(req, cands)
	require.NoError(t, err)
	require.Equal(t, "A", order[0]) // fuller node first
}

func TestSchedule_CapabilityAndTemplateFilter(t *testing.T) {
	c := Candidate{NodeID: "A", LimitCPU: 8, LimitMem: 8, LimitDisk: 8,
		Capabilities: map[string]bool{"clone": true}, Templates: map[string]bool{"base:1": true}}
	// needs a template the node lacks
	_, err := Schedule(Request{CPU: 1, Mem: 1, Disk: 1, Template: "other:1", RequestID: "r"}, []Candidate{c})
	require.ErrorIs(t, err, ErrNoEligibleNode)
	// needs a capability the node lacks
	_, err = Schedule(Request{CPU: 1, Mem: 1, Disk: 1, Capabilities: []string{"gpu"}, RequestID: "r"}, []Candidate{c})
	require.ErrorIs(t, err, ErrNoEligibleNode)
	// both satisfied
	order, err := Schedule(Request{CPU: 1, Mem: 1, Disk: 1, Template: "base:1", Capabilities: []string{"clone"}, RequestID: "r"}, []Candidate{c})
	require.NoError(t, err)
	require.Equal(t, []string{"A"}, order)
}

func TestSchedule_TieBreakDeterministicAcrossCalls(t *testing.T) {
	req := Request{CPU: 1, Mem: 1, Disk: 1, Strategy: "least-loaded", RequestID: "same"}
	cands := []Candidate{cand("A", 10, 0, 10, 0, 10, 0), cand("B", 10, 0, 10, 0, 10, 0)}
	o1, _ := Schedule(req, cands)
	o2, _ := Schedule(req, cands)
	require.Equal(t, o1, o2) // hash(requestID ⊕ nodeID) is stable
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/scheduler/ -v`
Expected: FAIL (package/types undefined).

- [ ] **Step 3: Write minimal implementation**

```go
// Package scheduler performs constraint-based placement: filter by hard
// predicates (Placement constraints), score survivors by dominant-resource
// ratio over CPU/mem/disk, break ties by hash(requestID ⊕ nodeID) (ADR-0007).
package scheduler

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"sort"
)

// ErrNoEligibleNode means no candidate passed every Placement constraint.
var ErrNoEligibleNode = errors.New("no eligible node")

// Candidate is a node's schedulable view (self from local capacity, peers from gossip).
// Units: CPU cores, memory KB, disk GB.
type Candidate struct {
	NodeID       string
	Workspaces   map[string]bool
	Templates    map[string]bool
	Capabilities map[string]bool
	Labels       map[string]string
	LimitCPU     float64
	LimitMem     float64
	LimitDisk    float64
	AllocCPU     float64
	AllocMem     float64
	AllocDisk    float64
	Sandboxes    int
	Cordoned     bool
}

// Request is a provision request's scheduling constraints. Units match Candidate.
type Request struct {
	CPU, Mem, Disk float64
	Workspaces     []string
	Template       string
	Capabilities   []string
	Affinity       map[string]string
	AntiAffinity   map[string]string
	Strategy       string // least-loaded(default)|bin-pack|spread
	RequestID      string
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
	return capFits(c.AllocCPU+req.CPU, c.LimitCPU) &&
		capFits(c.AllocMem+req.Mem, c.LimitMem) &&
		capFits(c.AllocDisk+req.Disk, c.LimitDisk)
}

// capFits reports whether used ≤ limit; a 0 limit is non-binding (unknown/unlimited).
func capFits(used, limit float64) bool { return limit == 0 || used <= limit }

// score is the post-placement dominant-resource ratio (or sandbox count for spread).
func score(req Request, c Candidate) float64 {
	if req.Strategy == "spread" {
		return float64(c.Sandboxes)
	}
	return max3(
		ratio(c.AllocCPU+req.CPU, c.LimitCPU),
		ratio(c.AllocMem+req.Mem, c.LimitMem),
		ratio(c.AllocDisk+req.Disk, c.LimitDisk),
	)
}

func max3(a, b, c float64) float64 {
	m := a
	if b > m {
		m = b
	}
	if c > m {
		m = c
	}
	return m
}

// ratio is used/limit; an unknown (0) limit sorts as fully loaded.
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

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/scheduler/ -v`
Expected: PASS (all cases).

- [ ] **Step 5: Commit**

```bash
git add internal/scheduler/
git commit -m "feat(scheduler): filter/score/tiebreak placement over cpu/mem/disk (ADR-0007)"
```

---

## Task 2: Capacity (atomic soft-reservation accounting)

**Files:**
- Create: `internal/sandbox/capacity.go`
- Test: `internal/sandbox/capacity_test.go`

- [ ] **Step 1: Write the failing test**

```go
package sandbox

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCapacity_TryReserveAdmitRelease(t *testing.T) {
	c := NewCapacity(4, 8, 10) // 4 cores, 8 KB, 10 GB

	id1, ok := c.TryReserve(2, 4, 5)
	require.True(t, ok)
	_, ok = c.TryReserve(3, 1, 1) // only 2 cores left
	require.False(t, ok)
	id2, ok := c.TryReserve(2, 4, 5)
	require.True(t, ok)

	c.Release(id1)
	c.Release(id2)
	_, ok = c.TryReserve(4, 8, 10)
	require.True(t, ok)
}

func TestCapacity_CommitMovesReservationToBase(t *testing.T) {
	c := NewCapacity(4, 8, 10)
	id, ok := c.TryReserve(2, 4, 5)
	require.True(t, ok)
	c.Commit(id) // create succeeded: reservation becomes base

	cpu, mem, disk := c.Snapshot()
	require.Equal(t, 2.0, cpu)
	require.Equal(t, 4.0, mem)
	require.Equal(t, 5.0, disk)
	// committed load still counts against the limit
	_, ok = c.TryReserve(3, 1, 1)
	require.False(t, ok)
}

func TestCapacity_SetBaseFromRecords(t *testing.T) {
	c := NewCapacity(4, 8, 10)
	c.SetBase(3, 6, 9) // reconciled from List()
	_, ok := c.TryReserve(2, 1, 1)
	require.False(t, ok)
	_, ok = c.TryReserve(1, 2, 1)
	require.True(t, ok)
}

func TestCapacity_ZeroLimitIsUnlimited(t *testing.T) {
	c := NewCapacity(0, 0, 0) // detection-failed / standalone
	_, ok := c.TryReserve(1e9, 1e9, 1e9)
	require.True(t, ok)
}

func TestCapacity_TryReserveAtomicUnderRace(t *testing.T) {
	c := NewCapacity(10, 1e9, 1e9) // exactly 5 of size-2 cpu fit
	var wg sync.WaitGroup
	var mu sync.Mutex
	got := 0
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, ok := c.TryReserve(2, 0, 0); ok {
				mu.Lock()
				got++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	require.Equal(t, 5, got) // never over-admits
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/sandbox/ -run TestCapacity -v`
Expected: FAIL (`NewCapacity` undefined).

- [ ] **Step 3: Write minimal implementation**

```go
package sandbox

import "sync"

// Capacity tracks soft, in-memory CPU/mem/disk accounting against a per-node
// provision limit. Units: CPU cores, memory KB, disk GB. The durable truth is
// Manager.List(); SetBase is called by reconcile. A 0 limit is non-binding
// (unlimited / detection-failed). Admission is a single atomic op (TryReserve)
// to avoid a check-then-reserve TOCTOU race.
type Capacity struct {
	mu                          sync.Mutex
	limitCPU, limitMem, limitDisk float64
	baseCPU, baseMem, baseDisk    float64
	resv                        map[int]reservation
	next                        int
}

type reservation struct{ cpu, mem, disk float64 }

// NewCapacity builds a tracker with the given (already-resolved) limits.
func NewCapacity(limitCPU, limitMem, limitDisk float64) *Capacity {
	return &Capacity{limitCPU: limitCPU, limitMem: limitMem, limitDisk: limitDisk, resv: map[int]reservation{}}
}

func (c *Capacity) usedLocked() (cpu, mem, disk float64) {
	cpu, mem, disk = c.baseCPU, c.baseMem, c.baseDisk
	for _, r := range c.resv {
		cpu += r.cpu
		mem += r.mem
		disk += r.disk
	}
	return
}

func fitsLimit(used, limit float64) bool { return limit == 0 || used <= limit }

// TryReserve atomically checks used+req ≤ limit (all three; 0 limit non-binding)
// and reserves. Returns (id, true) on success or (0, false) if it does not fit.
func (c *Capacity) TryReserve(cpu, mem, disk float64) (int, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	uc, um, ud := c.usedLocked()
	if !fitsLimit(uc+cpu, c.limitCPU) || !fitsLimit(um+mem, c.limitMem) || !fitsLimit(ud+disk, c.limitDisk) {
		return 0, false
	}
	id := c.next
	c.next++
	c.resv[id] = reservation{cpu: cpu, mem: mem, disk: disk}
	return id, true
}

// Commit promotes a reservation into the base (create succeeded) and drops it,
// atomically — no double-count, no gap.
func (c *Capacity) Commit(id int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, ok := c.resv[id]
	if !ok {
		return
	}
	c.baseCPU += r.cpu
	c.baseMem += r.mem
	c.baseDisk += r.disk
	delete(c.resv, id)
}

// Release frees a reservation (create failed).
func (c *Capacity) Release(id int) { c.mu.Lock(); delete(c.resv, id); c.mu.Unlock() }

// SetBase sets the reconciled allocation from durable records.
func (c *Capacity) SetBase(cpu, mem, disk float64) {
	c.mu.Lock()
	c.baseCPU, c.baseMem, c.baseDisk = cpu, mem, disk
	c.mu.Unlock()
}

// Limits returns the resolved limits (for gossip advertisement).
func (c *Capacity) Limits() (cpu, mem, disk float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.limitCPU, c.limitMem, c.limitDisk
}

// Snapshot returns current allocated cpu/mem/disk (base + reservations).
func (c *Capacity) Snapshot() (cpu, mem, disk float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.usedLocked()
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/sandbox/ -run TestCapacity -race -v`
Expected: PASS, no race.

- [ ] **Step 5: Commit**

```bash
git add internal/sandbox/capacity.go internal/sandbox/capacity_test.go
git commit -m "feat(sandbox): atomic soft-reservation capacity accounting (cpu/mem/disk)"
```

---

## Task 3: Host limit auto-detection

**Files:**
- Create: `internal/sandbox/hostlimits.go` (`//go:build linux`)
- Create: `internal/sandbox/hostlimits_other.go` (`//go:build !linux`)
- Test: `internal/sandbox/hostlimits_test.go`

- [ ] **Step 1: Write the failing test**

```go
package sandbox

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResolveLimit(t *testing.T) {
	require.Equal(t, 5.0, resolveLimit(5, 99)) // explicit config wins
	require.Equal(t, 12.0, resolveLimit(0, 12)) // 0 -> detected
	require.Equal(t, 0.0, resolveLimit(0, 0))   // detection failed -> unlimited
}

func TestDetectHostLimits_PositiveOnThisHost(t *testing.T) {
	cpu, memKB, diskGB := detectHostLimits(".")
	require.Greater(t, cpu, 0.0)
	// memKB/diskGB are best-effort; on the linux CI host they are > 0, but the
	// contract only guarantees ≥ 0 (0 == unknown → unlimited).
	require.GreaterOrEqual(t, memKB, 0.0)
	require.GreaterOrEqual(t, diskGB, 0.0)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/sandbox/ -run 'TestResolveLimit|TestDetectHostLimits' -v`
Expected: FAIL (`resolveLimit`/`detectHostLimits` undefined).

- [ ] **Step 3: Write minimal implementation**

`internal/sandbox/hostlimits.go`:

```go
//go:build linux

package sandbox

import "golang.org/x/sys/unix"

// resolveLimit returns the explicit config limit when set (>0), else the
// auto-detected host value (which may be 0 == unlimited on detection failure).
func resolveLimit(configured, detected float64) float64 {
	if configured > 0 {
		return configured
	}
	return detected
}

// detectHostLimits best-effort reads host CPU (cores), memory (KB), and the
// filesystem total at dataDir (GB). Any failed probe yields 0 (== unlimited).
// Linux-only (the deploy target); see hostlimits_other.go for the fallback.
func detectHostLimits(dataDir string) (cpuCores, memKB, diskGB float64) {
	cpuCores = float64(numCPU())

	var si unix.Sysinfo_t
	if err := unix.Sysinfo(&si); err == nil {
		// Totalram is in units of si.Unit bytes.
		totalBytes := uint64(si.Totalram) * uint64(si.Unit)
		memKB = float64(totalBytes) / 1024
	}

	var st unix.Statfs_t
	if err := unix.Statfs(dataDir, &st); err == nil {
		totalBytes := uint64(st.Bsize) * st.Blocks
		diskGB = float64(totalBytes) / (1 << 30)
	}
	return cpuCores, memKB, diskGB
}
```

`internal/sandbox/hostlimits_other.go`:

```go
//go:build !linux

package sandbox

// resolveLimit returns the explicit config limit; host auto-detect is
// Linux-only, so a 0 config means unlimited on other platforms.
func resolveLimit(configured, _ float64) float64 { return configured }

// detectHostLimits is a no-op (0 == unlimited) on non-Linux platforms.
func detectHostLimits(string) (cpuCores, memKB, diskGB float64) { return float64(numCPU()), 0, 0 }
```

Add `numCPU` once in a shared (build-tag-free) file. Append to `internal/sandbox/capacity.go`:

```go
import "runtime" // add to capacity.go's import block

// numCPU is wrapped for test/stub clarity.
func numCPU() int { return runtime.NumCPU() }
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/sandbox/ -run 'TestResolveLimit|TestDetectHostLimits' -v && go vet ./internal/sandbox/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/sandbox/hostlimits.go internal/sandbox/hostlimits_other.go internal/sandbox/hostlimits_test.go internal/sandbox/capacity.go
git commit -m "feat(sandbox): host cpu/mem/disk limit auto-detection (linux + fallback)"
```

---

## Task 4: Coordinator (schedule + attempt + NACK retry)

**Files:**
- Create: `internal/coordinator/coordinator.go`
- Test: `internal/coordinator/coordinator_test.go`

- [ ] **Step 1: Write the failing test**

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
		{NodeID: "C", LimitCPU: 10, LimitMem: 10, LimitDisk: 10},
		{NodeID: "A", LimitCPU: 10, AllocCPU: 5, LimitMem: 10, LimitDisk: 10},
	}
	var attempts []string
	attempt := func(_ context.Context, nodeID string) (string, error) {
		attempts = append(attempts, nodeID)
		if nodeID == "C" {
			return "", ErrNack // C rejects (admission)
		}
		return nodeID + ".sb", nil
	}
	co := New(func() []scheduler.Candidate { return cands })
	sbID, err := co.Provision(context.Background(), scheduler.Request{CPU: 1, Mem: 1, Disk: 1, Strategy: "least-loaded", RequestID: "r"}, attempt)
	require.NoError(t, err)
	require.Equal(t, "A.sb", sbID)
	require.Equal(t, []string{"C", "A"}, attempts) // C first (lighter), retried A
}

func TestCoordinator_AllNack(t *testing.T) {
	co := New(func() []scheduler.Candidate {
		return []scheduler.Candidate{{NodeID: "A", LimitCPU: 10, LimitMem: 10, LimitDisk: 10}}
	})
	_, err := co.Provision(context.Background(),
		scheduler.Request{CPU: 1, Mem: 1, Disk: 1, RequestID: "r"},
		func(context.Context, string) (string, error) { return "", ErrNack })
	require.True(t, errors.Is(err, ErrNoCapacity))
}

func TestCoordinator_NoEligibleNode(t *testing.T) {
	co := New(func() []scheduler.Candidate { return nil })
	_, err := co.Provision(context.Background(),
		scheduler.Request{CPU: 1, Mem: 1, Disk: 1, RequestID: "r"},
		func(context.Context, string) (string, error) { return "x", nil })
	require.True(t, errors.Is(err, scheduler.ErrNoEligibleNode))
}

func TestCoordinator_HardErrorSurfaces(t *testing.T) {
	boom := errors.New("transport down")
	co := New(func() []scheduler.Candidate {
		return []scheduler.Candidate{{NodeID: "A", LimitCPU: 10, LimitMem: 10, LimitDisk: 10}}
	})
	_, err := co.Provision(context.Background(),
		scheduler.Request{CPU: 1, Mem: 1, Disk: 1, RequestID: "r"},
		func(context.Context, string) (string, error) { return "", boom })
	require.ErrorIs(t, err, boom)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/coordinator/ -v`
Expected: FAIL (undefined).

- [ ] **Step 3: Write minimal implementation**

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

// ErrNoCapacity means every eligible candidate nacked.
var ErrNoCapacity = errors.New("no node accepted the provision")

// AttemptFunc provisions on a node, returning the new sandbox id or ErrNack.
// It is supplied per request so it can capture that request's create spec.
type AttemptFunc func(ctx context.Context, nodeID string) (sandboxID string, err error)

// Coordinator places provisions over the current candidate view.
type Coordinator struct {
	candidates func() []scheduler.Candidate
}

// New builds a coordinator over a candidate-view function.
func New(candidates func() []scheduler.Candidate) *Coordinator {
	return &Coordinator{candidates: candidates}
}

// Provision runs the scheduler and tries candidates best-first until one
// accepts, returning the sandbox id. ErrNoEligibleNode passes through; all-NACK
// becomes ErrNoCapacity; any non-NACK attempt error is surfaced immediately.
func (c *Coordinator) Provision(ctx context.Context, req scheduler.Request, attempt AttemptFunc) (string, error) {
	order, err := scheduler.Schedule(req, c.candidates())
	if err != nil {
		return "", err
	}
	for _, nodeID := range order {
		sbID, aerr := attempt(ctx, nodeID)
		if aerr == nil {
			return sbID, nil
		}
		if !errors.Is(aerr, ErrNack) {
			return "", aerr
		}
	}
	return "", ErrNoCapacity
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/coordinator/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/coordinator/
git commit -m "feat(coordinator): scheduler-driven placement with NACK retry"
```

---

## Task 5: Data-model additions (CreateSpec.DiskGB + Resources, NodeState fields, config fields)

**Files:**
- Modify: `internal/sandbox/backend.go` (add `DiskGB` to `CreateSpec`; add `Resources`)
- Modify: `internal/membership/state.go` (4 new fields)
- Modify: `internal/config/config.go` (config fields + validate)
- Test: `internal/membership/state_test.go` (round-trip), `internal/config/config_test.go` (validate)

- [ ] **Step 1: Write the failing test**

Add to `internal/membership/state_test.go` (create if absent — package `membership`):

```go
func TestNodeState_BulkRoundTripSchedulingFields(t *testing.T) {
	in := NodeState{
		NodeID: "n1", ProtocolVersion: ProtocolVersion,
		Workspaces:  []string{"repo-foo"},
		Templates:   []string{"base:1"},
		LimitDiskGB: 100, AllocDiskGB: 12,
	}
	out, err := DecodeBulk(in.EncodeBulk())
	require.NoError(t, err)
	require.Equal(t, []string{"repo-foo"}, out.Workspaces)
	require.Equal(t, []string{"base:1"}, out.Templates)
	require.Equal(t, 100.0, out.LimitDiskGB)
	require.Equal(t, 12.0, out.AllocDiskGB)
}
```

(Ensure imports include `"testing"` and `"github.com/stretchr/testify/require"`.)

Add to `internal/config/config_test.go`:

```go
func TestValidate_NegativeDiskLimitRejected(t *testing.T) {
	c := Default()
	c.ProvisionLimits.DiskGB = -1
	require.Error(t, c.Validate())
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/membership/ -run TestNodeState_BulkRoundTrip ./internal/config/ -run TestValidate_NegativeDisk -v`
Expected: FAIL (unknown fields).

- [ ] **Step 3: Write minimal implementation**

In `internal/sandbox/backend.go`, add `DiskGB` to `CreateSpec` and a `Resources` type:

```go
// CreateSpec describes a sandbox to provision.
type CreateSpec struct {
	Name        string
	Agent       string
	Template    string
	CPUs        int
	MemoryBytes int64
	DiskGB      float64 // requested disk (GB); scheduling-only in v1 (no SDK create option)
	Clone       bool
	Workspaces  []WorkspaceMount
	Env         map[string]string
}

// Resources is a per-sandbox resource triple (cores / bytes / GB). Used for
// the configured default applied to unsized requests.
type Resources struct {
	CPUCores    float64
	MemoryBytes int64
	DiskGB      float64
}
```

In `internal/membership/state.go`, add to the bulk section of `NodeState`:

```go
	Workspaces      []string          `json:"workspaces,omitempty"`
	Templates       []string          `json:"templates,omitempty"`
	LimitDiskGB     float64           `json:"limit_disk_gb,omitempty"`
	AllocDiskGB     float64           `json:"alloc_disk_gb,omitempty"`
```

In `internal/config/config.go`:
- Add a `WorkspaceConfig` type and config fields:

```go
// WorkspaceConfig is a named host directory advertised for mounting/cloning.
type WorkspaceConfig struct {
	Name     string `yaml:"name"`
	HostPath string `yaml:"host_path"`
	ReadOnly bool   `yaml:"read_only"`
}
```

- Extend `Config` (in the M4 cluster section):

```go
	Workspaces       []WorkspaceConfig `yaml:"workspaces"`
	DefaultStrategy  string            `yaml:"default_strategy"`
	DefaultSandboxResources SandboxResources `yaml:"default_sandbox_resources"`
```

- Add `SandboxResources` and `DiskGB` to `ProvisionLimits`:

```go
// SandboxResources is the per-sandbox default applied when a request omits a resource.
type SandboxResources struct {
	CPUCores    float64 `yaml:"cpu_cores"`
	MemoryBytes int64   `yaml:"memory_bytes"`
	DiskGB      float64 `yaml:"disk_gb"`
}
```

```go
type ProvisionLimits struct {
	CPUCores    float64 `yaml:"cpu_cores"`
	MemoryBytes int64   `yaml:"memory_bytes"`
	DiskGB      float64 `yaml:"disk_gb"`
}
```

- In `Validate()`, before the final `return nil`, reject negatives and a bad default strategy:

```go
	if c.ProvisionLimits.CPUCores < 0 || c.ProvisionLimits.MemoryBytes < 0 || c.ProvisionLimits.DiskGB < 0 {
		return fmt.Errorf("provision_limits must not be negative")
	}
	switch c.DefaultStrategy {
	case "", "least-loaded", "bin-pack", "spread":
	default:
		return fmt.Errorf("default_strategy must be one of least-loaded|bin-pack|spread, got %q", c.DefaultStrategy)
	}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/membership/ ./internal/config/ -v && go build ./...`
Expected: PASS, build OK.

- [ ] **Step 5: Commit**

```bash
git add internal/sandbox/backend.go internal/membership/state.go internal/membership/state_test.go internal/config/config.go internal/config/config_test.go
git commit -m "feat(model): scheduling fields on CreateSpec/NodeState/Config (+disk)"
```

---

## Task 6: Backend.ListTemplates

**Files:**
- Modify: `internal/sandbox/backend.go` (interface method)
- Modify: `internal/sandbox/fake.go` (settable templates)
- Modify: `internal/sandbox/sdkbackend.go` (wrap `template.List`)
- Test: `internal/sandbox/fake_test.go` (or existing fake test file)

- [ ] **Step 1: Write the failing test**

Add to `internal/sandbox/fake_test.go` (create if absent — package `sandbox`):

```go
func TestFake_ListTemplates(t *testing.T) {
	f := NewFake()
	got, err := f.ListTemplates(context.Background())
	require.NoError(t, err)
	require.Empty(t, got)

	f.SetTemplates([]string{"base:1", "gpu:2"})
	got, err = f.ListTemplates(context.Background())
	require.NoError(t, err)
	require.Equal(t, []string{"base:1", "gpu:2"}, got)
}
```

(Imports: `"context"`, `"testing"`, `"github.com/stretchr/testify/require"`.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/sandbox/ -run TestFake_ListTemplates -v`
Expected: FAIL (`ListTemplates`/`SetTemplates` undefined).

- [ ] **Step 3: Write minimal implementation**

In `internal/sandbox/backend.go`, add to the `Backend` interface (near `List`):

```go
	// ListTemplates returns the template refs this node's daemon holds.
	ListTemplates(ctx context.Context) ([]string, error)
```

In `internal/sandbox/fake.go`, add a field to the `Fake` struct and methods:

```go
// add to the Fake struct:
//   templates []string

// SetTemplates sets the advertised template refs (tests).
func (f *Fake) SetTemplates(t []string) { f.mu.Lock(); f.templates = append([]string(nil), t...); f.mu.Unlock() }

// ListTemplates returns the configured template refs.
func (f *Fake) ListTemplates(_ context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.templates...), nil
}
```

In `internal/sandbox/sdkbackend.go`, add the import `sdktemplate "github.com/squall-chua/sbx-go-sdk/template"` and:

```go
// ListTemplates returns the template refs the daemon holds (repository:tag).
// ponytail: ref format assumed repository:tag to match WithTemplate; confirm
// against a live daemon (integration-only) before relying on exact matching.
func (b *SDKBackend) ListTemplates(ctx context.Context) ([]string, error) {
	imgs, err := sdktemplate.List(ctx, b.cl)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(imgs))
	for _, im := range imgs {
		ref := im.Repository
		if im.Tag != "" {
			ref += ":" + im.Tag
		}
		out = append(out, ref)
	}
	return out, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/sandbox/ -run TestFake_ListTemplates -v && go build ./...`
Expected: PASS, build OK (both backends satisfy `Backend`).

- [ ] **Step 5: Commit**

```bash
git add internal/sandbox/backend.go internal/sandbox/fake.go internal/sandbox/sdkbackend.go internal/sandbox/fake_test.go
git commit -m "feat(sandbox): Backend.ListTemplates (daemon-derived template advertisement)"
```

---

## Task 7: Manager capacity wiring (AdmitAndCreate, ErrNoCapacity, Reconcile→base)

**Files:**
- Modify: `internal/sandbox/manager.go`
- Test: `internal/sandbox/manager_test.go` (append)

- [ ] **Step 1: Write the failing test**

```go
func TestManager_AdmitAndCreate_NacksOverLimit(t *testing.T) {
	st := newTestStore(t) // reuse the existing manager-test store helper
	mgr := NewManager("n1", NewFake(), st, ids.NewGen("n1"))
	mgr.SetCapacity(NewCapacity(2, 1e9, 1e9)) // 2 cores

	_, err := mgr.AdmitAndCreate(context.Background(), CreateSpec{CPUs: 2, MemoryBytes: 1, DiskGB: 0})
	require.NoError(t, err)

	_, err = mgr.AdmitAndCreate(context.Background(), CreateSpec{CPUs: 1, MemoryBytes: 1, DiskGB: 0})
	require.ErrorIs(t, err, ErrNoCapacity)
}

func TestManager_ReconcileSetsBaseFromRecords(t *testing.T) {
	st := newTestStore(t)
	mgr := NewManager("n1", NewFake(), st, ids.NewGen("n1"))
	cap := NewCapacity(0, 0, 0)
	mgr.SetCapacity(cap)

	_, err := mgr.AdmitAndCreate(context.Background(), CreateSpec{CPUs: 3, MemoryBytes: 2048, DiskGB: 4})
	require.NoError(t, err)
	require.NoError(t, mgr.Reconcile(context.Background()))

	cpu, mem, disk := cap.Snapshot()
	require.Equal(t, 3.0, cpu)
	require.Equal(t, 2.0, mem) // 2048 bytes -> 2 KB
	require.Equal(t, 4.0, disk)
}
```

> If `newTestStore`/`ids` import don't already exist in `manager_test.go`, mirror the existing test setup in that file (it already constructs a `Manager` with a `store.Store` and `ids.Gen`). Match the helper actually present.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/sandbox/ -run 'TestManager_AdmitAndCreate|TestManager_Reconcile' -v`
Expected: FAIL (`SetCapacity`/`AdmitAndCreate`/`ErrNoCapacity` undefined).

- [ ] **Step 3: Write minimal implementation**

In `internal/sandbox/manager.go`:

- Add `"errors"` to imports; add the sentinel and a `cap` field:

```go
// ErrNoCapacity means the node cannot admit the request within its provision limit.
var ErrNoCapacity = errors.New("insufficient capacity")
```

```go
// add to Manager struct:
//   cap *Capacity
```

- Default the capacity in `NewManager` (unlimited) so existing callers are unaffected:

```go
func NewManager(nodeID string, backend Backend, st *store.Store, gen *ids.Gen) *Manager {
	return &Manager{nodeID: nodeID, backend: backend, store: st, ids: gen, now: time.Now, cap: NewCapacity(0, 0, 0)}
}
```

- Add accessors + admission:

```go
// SetCapacity wires a capacity tracker (node.go passes resolved limits).
func (m *Manager) SetCapacity(c *Capacity) { m.cap = c }

// Capacity returns the capacity tracker.
func (m *Manager) Capacity() *Capacity { return m.cap }

// costOf is a spec's resource cost (cores / KB / GB).
func costOf(spec CreateSpec) (cpu, mem, disk float64) {
	return float64(spec.CPUs), float64(spec.MemoryBytes) / 1024, spec.DiskGB
}

// AdmitAndCreate reserves capacity (atomic), creates, then commits the
// reservation into the base on success (or releases it on failure). Returns
// ErrNoCapacity when admission fails.
func (m *Manager) AdmitAndCreate(ctx context.Context, spec CreateSpec) (*Record, error) {
	cpu, mem, disk := costOf(spec)
	id, ok := m.cap.TryReserve(cpu, mem, disk)
	if !ok {
		return nil, ErrNoCapacity
	}
	rec, err := m.Create(ctx, spec)
	if err != nil {
		m.cap.Release(id)
		return nil, err
	}
	m.cap.Commit(id)
	return rec, nil
}
```

- At the end of `Reconcile` (after the lost-marking loop, before `return nil`), recompute base from non-terminal records:

```go
	var bc, bm, bd float64
	for _, rec := range recs {
		if rec.Status == "lost" {
			continue
		}
		c, mm, d := costOf(rec.Spec)
		bc += c
		bm += mm
		bd += d
	}
	m.cap.SetBase(bc, bm, bd)
```

(`recs` is already in scope from the reconcile loop; reuse it.)

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/sandbox/ -v && go build ./...`
Expected: PASS (existing manager tests unaffected — default capacity is unlimited).

- [ ] **Step 5: Commit**

```bash
git add internal/sandbox/manager.go internal/sandbox/manager_test.go
git commit -m "feat(sandbox): Manager.AdmitAndCreate + reconcile-derived capacity base"
```

---

## Task 8: Proto — disk_gb/strategy + internal Provision RPC

**Files:**
- Modify: `proto/sbxswarm/v1/sandbox.proto`
- Create: `proto/sbxswarm/v1/internal.proto`
- Generated (committed): `internal/gen/sbxswarm/v1/*`

- [ ] **Step 1: Edit `sandbox.proto`** — add two fields to `CreateSandboxRequest` (next free numbers after `labels = 8`):

```proto
  double disk_gb = 9;
  string strategy = 10; // optional: least-loaded|bin-pack|spread (empty = node default)
```

- [ ] **Step 2: Create `proto/sbxswarm/v1/internal.proto`**:

```proto
syntax = "proto3";

package sbxswarm.v1;

import "sbxswarm/v1/sandbox.proto";

option go_package = "github.com/squall-chua/sbx-swarm-node/internal/gen/sbxswarm/v1;sbxswarmv1";

// InternalService carries node→node RPCs. No google.api.http annotations: it is
// never exposed over REST, and is authorized by node identity (ADR-0011).
service InternalService {
  rpc Provision(ProvisionRequest) returns (ProvisionReply);
}

message ProvisionRequest {
  CreateSandboxRequest spec = 1; // already-sized create spec (effective sizing applied at the entry node)
  string request_id = 2;
}

message ProvisionReply {
  bool accepted = 1;
  string sandbox_id = 2;
  string reason = 3;
}
```

- [ ] **Step 3: Regenerate + build**

Run:
```bash
buf generate && go build ./...
```
Expected: regenerates `internal/gen/sbxswarm/v1/*` (new `internal.pb.go`, `internal_grpc.pb.go`; `sandbox.pb.go` gains `DiskGb`/`Strategy`); build OK. No gateway file for internal (no http annotation).

- [ ] **Step 4: Verify the generated symbols exist**

Run:
```bash
grep -l "InternalService_ServiceDesc" internal/gen/sbxswarm/v1/*.go
grep -n "DiskGb\|Strategy" internal/gen/sbxswarm/v1/sandbox.pb.go | head
```
Expected: a grpc file lists `InternalService_ServiceDesc`; `CreateSandboxRequest` has `GetDiskGb()`/`GetStrategy()`.

- [ ] **Step 5: Commit**

```bash
git add proto/ internal/gen/
git commit -m "feat(proto): CreateSandboxRequest disk_gb/strategy; InternalService.Provision"
```

---

## Task 9: InternalService.Provision handler + registration

**Files:**
- Create: `internal/apiserver/provision.go`
- Modify: `internal/apiserver/server.go` (`Options.Internal`, register on grpcSrv only)
- Test: `internal/apiserver/provision_test.go`

- [ ] **Step 1: Write the failing test**

```go
package apiserver

import (
	"context"
	"testing"

	sbxv1 "github.com/squall-chua/sbx-swarm-node/internal/gen/sbxswarm/v1"
	"github.com/squall-chua/sbx-swarm-node/internal/ids"
	"github.com/squall-chua/sbx-swarm-node/internal/sandbox"
	"github.com/stretchr/testify/require"
)

func TestInternalProvision_AdmitsThenNacks(t *testing.T) {
	st := newProvisionTestStore(t) // a bbolt store; mirror an existing apiserver test helper
	mgr := sandbox.NewManager("n1", sandbox.NewFake(), st, ids.NewGen("n1"))
	mgr.SetCapacity(sandbox.NewCapacity(2, 1e9, 1e9)) // 2 cores
	svc := NewInternalService(mgr)

	r1, err := svc.Provision(context.Background(), &sbxv1.ProvisionRequest{
		Spec: &sbxv1.CreateSandboxRequest{Cpus: 2, MemoryBytes: 1},
	})
	require.NoError(t, err)
	require.True(t, r1.Accepted)
	require.NotEmpty(t, r1.SandboxId)

	r2, err := svc.Provision(context.Background(), &sbxv1.ProvisionRequest{
		Spec: &sbxv1.CreateSandboxRequest{Cpus: 1, MemoryBytes: 1},
	})
	require.NoError(t, err)
	require.False(t, r2.Accepted)
	require.Equal(t, "no capacity", r2.Reason)
}
```

> Provide `newProvisionTestStore` by mirroring how other `internal/apiserver` tests open a temp `store.Store` (e.g. via `store.Open(filepath.Join(t.TempDir(), "x.db"))`). Match the existing pattern in the package.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/apiserver/ -run TestInternalProvision -v`
Expected: FAIL (`NewInternalService` undefined).

- [ ] **Step 3: Write `provision.go`**

```go
package apiserver

import (
	"context"

	sbxv1 "github.com/squall-chua/sbx-swarm-node/internal/gen/sbxswarm/v1"
	"github.com/squall-chua/sbx-swarm-node/internal/sandbox"
)

// InternalService handles node→node RPCs (provision admission). Authorized by
// node identity (ADR-0011); never exposed over REST.
type InternalService struct {
	sbxv1.UnimplementedInternalServiceServer
	mgr *sandbox.Manager
}

// NewInternalService builds the internal service.
func NewInternalService(mgr *sandbox.Manager) *InternalService { return &InternalService{mgr: mgr} }

// Provision performs target-authoritative admission against real local capacity,
// then creates. A capacity miss returns accepted=false (the coordinator's NACK).
func (s *InternalService) Provision(ctx context.Context, r *sbxv1.ProvisionRequest) (*sbxv1.ProvisionReply, error) {
	spec := toSpec(r.Spec)
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

(`toSpec(*sbxv1.CreateSandboxRequest) sandbox.CreateSpec` already exists in `sandboxservice.go`; Task 11 extends it to also map `DiskGB`.)

- [ ] **Step 4: Register on the gRPC server (grpc-only, no gateway)**

In `internal/apiserver/server.go`:
- Add to `Options`: `Internal *InternalService // optional; node→node provision RPC`
- After the other `Register…Server` calls in `Build`:

```go
	if opts.Internal != nil {
		sbxv1.RegisterInternalServiceServer(grpcSrv, opts.Internal)
	}
```

Do **not** add a gateway handler for it.

- [ ] **Step 5: Run + commit**

Run: `go test ./internal/apiserver/ -run TestInternalProvision -v && go build ./...`
Expected: PASS, build OK.

```bash
git add internal/apiserver/provision.go internal/apiserver/provision_test.go internal/apiserver/server.go
git commit -m "feat(apiserver): InternalService.Provision target admission + registration"
```

---

## Task 10: authz — node-gated internal bucket + drift-guard

**Files:**
- Modify: `internal/apiserver/authz.go`
- Modify: `internal/apiserver/authz_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/apiserver/authz_test.go`:

```go
func TestAuthorize_InternalProvisionNodeGated(t *testing.T) {
	const m = "/sbxswarm.v1.InternalService/Provision"
	require.NoError(t, authorize(m, principal{node: true}))                       // verified peer: allowed
	require.Error(t, authorize(m, principal{userRole: "admin"}))                  // user (even admin): denied
	require.Error(t, authorize(m, principal{}))                                   // unauthenticated: denied
}
```

And add `sbxv1.InternalService_ServiceDesc` to the `descs` slice in `TestAuthz_AllMethodsClassified`:

```go
	descs := []grpc.ServiceDesc{
		sbxv1.SandboxService_ServiceDesc,
		sbxv1.NodeService_ServiceDesc,
		sbxv1.PolicyService_ServiceDesc,
		sbxv1.EventService_ServiceDesc,
		sbxv1.InternalService_ServiceDesc,
	}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/apiserver/ -run TestAuthorize_InternalProvision -v`
Expected: FAIL (Provision unclassified / not node-gated). `TestAuthz_AllMethodsClassified` also fails until Step 3.

- [ ] **Step 3: Write the implementation**

In `internal/apiserver/authz.go`:
- Add the bucket:

```go
// internalMethods are node→node RPCs authorized by node identity alone
// (a verified swarm peer). A user principal cannot call them. Admin is enforced
// once at the request's entry node before the async op (ADR-0011).
var internalMethods = map[string]bool{
	"/sbxswarm.v1.InternalService/Provision": true,
}
```

- Update `classified`:

```go
func classified(fullMethod string) bool {
	return mutatingMethods[fullMethod] || readMethods[fullMethod] || internalMethods[fullMethod]
}
```

- Update `authorize` (insert before the read/admin handling, after the authenticated check):

```go
func authorize(fullMethod string, p principal) error {
	if !p.authenticated() {
		return status.Error(codes.Unauthenticated, "authentication required")
	}
	if internalMethods[fullMethod] {
		if p.node {
			return nil
		}
		return status.Errorf(codes.PermissionDenied, "method %s requires a swarm node", fullMethod)
	}
	if readMethods[fullMethod] {
		return nil
	}
	if p.userRole == "admin" {
		return nil
	}
	return status.Errorf(codes.PermissionDenied, "method %s requires admin", fullMethod)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/apiserver/ -v`
Expected: PASS (including `TestAuthz_AllMethodsClassified`).

- [ ] **Step 5: Commit**

```bash
git add internal/apiserver/authz.go internal/apiserver/authz_test.go
git commit -m "feat(apiserver): node-gated internalMethods authz for Provision (ADR-0011)"
```

---

## Task 11: SandboxService — effective sizing, strategy, placement seam

**Files:**
- Modify: `internal/apiserver/sandboxservice.go`
- Test: `internal/apiserver/sandboxservice_test.go` (append)

- [ ] **Step 1: Write the failing test**

```go
func TestCreateSandbox_RejectsBadStrategy(t *testing.T) {
	svc := newSandboxServiceForTest(t) // mirror existing helper that builds a SandboxService with fake mgr+ops
	_, err := svc.CreateSandbox(context.Background(), &sbxv1.CreateSandboxRequest{Strategy: "bogus"})
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestEffectiveSizing_FillsUnsized(t *testing.T) {
	defaults := sandbox.Resources{CPUCores: 2, MemoryBytes: 1024, DiskGB: 3}
	got := effectiveSpec(&sbxv1.CreateSandboxRequest{}, defaults)
	require.Equal(t, int32(2), got.Cpus)
	require.Equal(t, int64(1024), got.MemoryBytes)
	require.Equal(t, 3.0, got.DiskGb)

	// explicit values win
	got = effectiveSpec(&sbxv1.CreateSandboxRequest{Cpus: 8, MemoryBytes: 4096, DiskGb: 9}, defaults)
	require.Equal(t, int32(8), got.Cpus)
	require.Equal(t, int64(4096), got.MemoryBytes)
	require.Equal(t, 9.0, got.DiskGb)
}

func TestEffectiveSizing_BuiltinFloorWhenNoDefault(t *testing.T) {
	got := effectiveSpec(&sbxv1.CreateSandboxRequest{}, sandbox.Resources{})
	require.Equal(t, floorCPUCores, got.Cpus)
	require.Equal(t, floorMemoryBytes, got.MemoryBytes)
	require.Equal(t, floorDiskGB, got.DiskGb)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/apiserver/ -run 'TestCreateSandbox_RejectsBadStrategy|TestEffectiveSizing' -v`
Expected: FAIL (undefined).

- [ ] **Step 3: Write the implementation**

In `internal/apiserver/sandboxservice.go`:

- Imports: add `"github.com/squall-chua/sbx-swarm-node/internal/scheduler"`, `"google.golang.org/protobuf/proto"`.

- Add the placement seam + config to the struct and a setter:

```go
// PlaceFunc places a sized request and returns the created sandbox id. Injected
// by node.go (coordinator-backed); nil falls back to a local admit+create.
type PlaceFunc func(ctx context.Context, req scheduler.Request, spec *sbxv1.CreateSandboxRequest) (sandboxID string, err error)

// add to SandboxService struct:
//   place            PlaceFunc
//   defaultStrategy  string
//   defaultResources sandbox.Resources

// WithPlacement wires placement (coordinator) + sizing defaults.
func (s *SandboxService) WithPlacement(place PlaceFunc, defaultStrategy string, defaults sandbox.Resources) {
	s.place = place
	s.defaultStrategy = defaultStrategy
	s.defaultResources = defaults
}
```

- Built-in floor constants + helpers:

```go
const (
	floorCPUCores    int32 = 1
	floorMemoryBytes int64 = 512 << 20 // 512 MiB
	floorDiskGB            = 1.0
)

// effectiveSpec returns a copy of r with each unset resource filled from the
// configured default, else the built-in floor (no untracked sandboxes).
// ponytail: floor approximates the daemon's hidden default; source it from the
// daemon once the SDK exposes it.
func effectiveSpec(r *sbxv1.CreateSandboxRequest, defaults sandbox.Resources) *sbxv1.CreateSandboxRequest {
	out := proto.Clone(r).(*sbxv1.CreateSandboxRequest)
	if out.Cpus <= 0 {
		if defaults.CPUCores > 0 {
			out.Cpus = int32(defaults.CPUCores)
		} else {
			out.Cpus = floorCPUCores
		}
	}
	if out.MemoryBytes <= 0 {
		if defaults.MemoryBytes > 0 {
			out.MemoryBytes = defaults.MemoryBytes
		} else {
			out.MemoryBytes = floorMemoryBytes
		}
	}
	if out.DiskGb <= 0 {
		if defaults.DiskGB > 0 {
			out.DiskGb = defaults.DiskGB
		} else {
			out.DiskGb = floorDiskGB
		}
	}
	return out
}

// resolveStrategy applies precedence request → config default → least-loaded and
// validates the result.
func resolveStrategy(reqStrategy, defaultStrategy string) (string, error) {
	s := reqStrategy
	if s == "" {
		s = defaultStrategy
	}
	if s == "" {
		s = "least-loaded"
	}
	switch s {
	case "least-loaded", "bin-pack", "spread":
		return s, nil
	default:
		return "", status.Errorf(codes.InvalidArgument, "unknown strategy %q", reqStrategy)
	}
}

// requestFromSpec builds the scheduler Request from a sized spec.
func requestFromSpec(spec *sbxv1.CreateSandboxRequest, strategy, requestID string) scheduler.Request {
	ws := make([]string, 0, len(spec.Workspaces))
	for _, w := range spec.Workspaces {
		ws = append(ws, w.Name)
	}
	var caps []string
	if spec.Clone {
		caps = append(caps, "clone") // ADR-0009 capability predicate
	}
	return scheduler.Request{
		CPU: float64(spec.Cpus), Mem: float64(spec.MemoryBytes) / 1024, Disk: spec.DiskGb,
		Workspaces: ws, Template: spec.Template, Capabilities: caps,
		Strategy: strategy, RequestID: requestID,
	}
}
```

- Extend `toSpec` to map disk:

```go
// in toSpec(...), add to the returned CreateSpec literal:
//   DiskGB: r.DiskGb,
```

- Rewrite `CreateSandbox`:

```go
func (s *SandboxService) CreateSandbox(ctx context.Context, r *sbxv1.CreateSandboxRequest) (*sbxv1.Operation, error) {
	strategy, err := resolveStrategy(r.Strategy, s.defaultStrategy)
	if err != nil {
		return nil, err
	}
	op, existed, err := s.ops.Start(ctx, "provision", idempotencyKey(ctx))
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if existed {
		return opProto(op), nil
	}
	sized := effectiveSpec(r, s.defaultResources)
	req := requestFromSpec(sized, strategy, op.ID)
	s.ops.Run(op.ID, func() (string, error) {
		if s.place != nil {
			return s.place(context.Background(), req, sized)
		}
		// Fallback (no coordinator wired, e.g. unit tests): local admit+create.
		rec, cerr := s.mgr.AdmitAndCreate(context.Background(), toSpec(sized))
		if cerr != nil {
			return "", cerr
		}
		return rec.ID, nil
	})
	return opProto(op), nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/apiserver/ -v && go build ./...`
Expected: PASS (existing CreateSandbox tests still pass via the fallback path; an unsized create now produces a floored, admitted sandbox).

- [ ] **Step 5: Commit**

```bash
git add internal/apiserver/sandboxservice.go internal/apiserver/sandboxservice_test.go
git commit -m "feat(apiserver): effective sizing, strategy validation, placement seam"
```

---

## Task 12: node.go wiring — capacity, coordinator, advertisement, registration

**Files:**
- Modify: `internal/node/node.go`

This task has no new unit test (it is pure wiring); verification is `build`/`vet`/the full suite, plus Task 13's integration test. Follow the existing structure in `node.New`.

- [ ] **Step 1: Resolve capacity limits + wire into the Manager**

After `backend`/`mgr` are created (around `mgr := sandbox.NewManager(...)`), add:

```go
	dc, dm, dd := detectHostLimitsForNode(cfg.DataDir) // see Step 5 helper
	capt := sandbox.NewCapacity(
		resolveCfgLimit(cfg.ProvisionLimits.CPUCores, dc),
		resolveCfgLimit(float64(cfg.ProvisionLimits.MemoryBytes)/1024, dm),
		resolveCfgLimit(cfg.ProvisionLimits.DiskGB, dd),
	)
	mgr.SetCapacity(capt)
```

- [ ] **Step 2: Advertise scheduling fields in `localNS`**

Where `localNS := membership.NodeState{...}` is built, set limits from the resolved capacity and add workspaces/templates/alloc:

```go
	lc, lm, ld := capt.Limits()
	ac, am, ad := capt.Snapshot()
	wsNames := workspaceNames(cfg.Workspaces)
	tmpls, _ := backend.ListTemplates(context.Background())
```

and in the struct literal replace the `LimitCPU`/`LimitMemKB` lines and add:

```go
		LimitCPU:    lc,
		LimitMemKB:  lm,
		LimitDiskGB: ld,
		AllocCPU:    ac,
		AllocMemKB:  am,
		AllocDiskGB: ad,
		Workspaces:  wsNames,
		Templates:   tmpls,
```

- [ ] **Step 3: Build the coordinator + placement, inject into `sandboxes`**

After `tbl`, `pool`, `fwd` are constructed, add:

```go
	coord := coordinator.New(func() []scheduler.Candidate {
		return buildCandidates(id.NodeID, cfg, capt, mgr, clusterInstance, tbl)
	})
	sandboxes.WithPlacement(
		func(ctx context.Context, req scheduler.Request, spec *sbxv1.CreateSandboxRequest) (string, error) {
			return coord.Provision(ctx, req, attemptFor(ctx, id.NodeID, spec, mgr, tbl, pool))
		},
		cfg.DefaultStrategy,
		sandbox.Resources{
			CPUCores:    cfg.DefaultSandboxResources.CPUCores,
			MemoryBytes: cfg.DefaultSandboxResources.MemoryBytes,
			DiskGB:      cfg.DefaultSandboxResources.DiskGB,
		},
	)
```

> `clusterInstance` is declared further down today; hoist its `var clusterInstance *membership.Cluster` declaration above this block (it is already a `var` — move the declaration up, keep the assignment where it is). `buildCandidates` reads `clusterInstance` (nil-safe).

- [ ] **Step 4: Register `InternalService` in the `apiserver.Build` Options**

Add to the `apiserver.Options{...}` literal:

```go
		Internal: apiserver.NewInternalService(mgr),
```

- [ ] **Step 5: Add the node-local helpers (bottom of `node.go`)**

```go
// detectHostLimitsForNode exposes sandbox.detectHostLimits via a tiny wrapper so
// node.go does not need build tags. It calls the package-internal detector
// through an exported shim.
//
// Implemented in internal/sandbox via DetectHostLimits (add it in Task 3 follow-up).
func detectHostLimitsForNode(dataDir string) (cpu, memKB, diskGB float64) {
	return sandbox.DetectHostLimits(dataDir)
}

func resolveCfgLimit(configured, detected float64) float64 {
	if configured > 0 {
		return configured
	}
	return detected
}

func workspaceNames(ws []config.WorkspaceConfig) []string {
	out := make([]string, 0, len(ws))
	for _, w := range ws {
		out = append(out, w.Name)
	}
	return out
}

// buildCandidates assembles the self candidate (live local capacity) + gossiped peers.
func buildCandidates(self string, cfg *config.Config, capt *sandbox.Capacity, mgr *sandbox.Manager, cl *membership.Cluster, tbl *routing.Table) []scheduler.Candidate {
	lc, lm, ld := capt.Limits()
	ac, am, ad := capt.Snapshot()
	recs, _ := mgr.List(context.Background())
	caps := map[string]bool{"clone": true, "stats": true, "exec": true}
	selfTmpls, _ := mgr.Backend().ListTemplates(context.Background())
	out := []scheduler.Candidate{{
		NodeID: self, Workspaces: nameSet(workspaceNames(cfg.Workspaces)), Templates: nameSet(selfTmpls),
		Capabilities: caps, Labels: cfg.Labels,
		LimitCPU: lc, LimitMem: lm, LimitDisk: ld,
		AllocCPU: ac, AllocMem: am, AllocDisk: ad,
		Sandboxes: len(recs), Cordoned: tbl.IsCordoned(self),
	}}
	if cl == nil {
		return out
	}
	for _, ns := range cl.PeerStates() {
		out = append(out, scheduler.Candidate{
			NodeID: ns.NodeID, Workspaces: nameSet(ns.Workspaces), Templates: nameSet(ns.Templates),
			Capabilities: nameSet(ns.Capabilities), Labels: ns.Labels,
			LimitCPU: ns.LimitCPU, LimitMem: ns.LimitMemKB, LimitDisk: ns.LimitDiskGB,
			AllocCPU: ns.AllocCPU, AllocMem: ns.AllocMemKB, AllocDisk: ns.AllocDiskGB,
			Sandboxes: len(ns.OwnedSandboxIDs), Cordoned: ns.Cordoned,
		})
	}
	return out
}

func nameSet(ss []string) map[string]bool {
	m := make(map[string]bool, len(ss))
	for _, s := range ss {
		m[s] = true
	}
	return m
}

// attemptFor builds the per-request attempt closure: local admit+create, or a
// remote Provision RPC over the pinned peer pool.
func attemptFor(_ context.Context, self string, spec *sbxv1.CreateSandboxRequest, mgr *sandbox.Manager, tbl *routing.Table, pool *peer.Pool) coordinator.AttemptFunc {
	return func(ctx context.Context, nodeID string) (string, error) {
		if nodeID == self {
			rec, err := mgr.AdmitAndCreate(ctx, apiserver.ToSpecForProvision(spec))
			if err == sandbox.ErrNoCapacity {
				return "", coordinator.ErrNack
			}
			if err != nil {
				return "", err
			}
			return rec.ID, nil
		}
		addr, ok := tbl.Addr(nodeID)
		if !ok {
			return "", coordinator.ErrNack
		}
		conn, err := pool.Conn(addr, nodeID)
		if err != nil {
			return "", err
		}
		reply, err := sbxv1.NewInternalServiceClient(conn).Provision(ctx, &sbxv1.ProvisionRequest{Spec: spec})
		if err != nil {
			return "", err
		}
		if !reply.Accepted {
			return "", coordinator.ErrNack
		}
		return reply.SandboxId, nil
	}
}
```

- [ ] **Step 6: Export the two `sandbox`/`apiserver` shims used above**

- In `internal/sandbox/hostlimits.go` and `hostlimits_other.go`, add an exported wrapper so `node.go` (no build tags) can call it:

```go
// DetectHostLimits is the exported entry point for host capacity probing.
func DetectHostLimits(dataDir string) (cpuCores, memKB, diskGB float64) { return detectHostLimits(dataDir) }
```

(Add to **both** build-tagged files so it exists on every platform.)

- In `internal/apiserver/sandboxservice.go`, export the spec mapper for node.go's local attempt:

```go
// ToSpecForProvision maps a proto create request to a sandbox.CreateSpec.
func ToSpecForProvision(r *sbxv1.CreateSandboxRequest) sandbox.CreateSpec { return toSpec(r) }
```

- [ ] **Step 7: Periodic capacity/template refresh on the existing 10s ticker**

In the existing `go runTicker(nctx, 10*time.Second, func() {...})` block, add a periodic reconcile so capacity base + advertised state stay fresh:

```go
		_ = mgr.Reconcile(nctx)
		if clusterInstance != nil {
			ac, am, ad := mgr.Capacity().Snapshot()
			clusterInstance.UpdateLocalAlloc(ac, am, ad) // see Step 8
		}
```

- [ ] **Step 8: Add `Cluster.UpdateLocalAlloc`**

In `internal/membership/cluster.go`, mirroring `UpdateLocalSandboxIDs`:

```go
// UpdateLocalAlloc refreshes the gossiped allocation snapshot and re-advertises.
func (c *Cluster) UpdateLocalAlloc(cpu, memKB, diskGB float64) {
	c.mu.Lock()
	c.local.AllocCPU = cpu
	c.local.AllocMemKB = memKB
	c.local.AllocDiskGB = diskGB
	c.local.StateVersion++
	ml := c.ml
	c.mu.Unlock()
	if ml != nil {
		_ = ml.UpdateNode(5 * time.Second)
	}
}
```

- [ ] **Step 9: Add imports to node.go**

Add `"github.com/squall-chua/sbx-swarm-node/internal/coordinator"` and `"github.com/squall-chua/sbx-swarm-node/internal/scheduler"` and `sbxv1 "github.com/squall-chua/sbx-swarm-node/internal/gen/sbxswarm/v1"` to `node.go`'s import block.

- [ ] **Step 10: Build + full suite**

Run:
```bash
go build ./... && go vet ./... && go test ./...
```
Expected: build/vet clean; all existing tests pass.

- [ ] **Step 11: Commit**

```bash
git add internal/node/node.go internal/membership/cluster.go internal/sandbox/hostlimits.go internal/sandbox/hostlimits_other.go internal/apiserver/sandboxservice.go
git commit -m "feat(node): wire capacity + coordinator placement; advertise scheduling state"
```

---

## Task 13: Integration test — cross-node placement (2 nodes)

**Files:**
- Create: `internal/membership/scheduling_integration_test.go` (`//go:build integration`)

Mirror the existing 2-node integration test harness in `internal/membership/` (same cluster bootstrap, **fresh ports** — use REST `19843`/`19853` and gossip `17996`/`18006`, none of which the existing suite uses). Each node is a real `node.New(...)` + `Start()` with a shared `cluster_secret`; one node configures a workspace the other lacks.

- [ ] **Step 1: Write the integration test**

```go
//go:build integration

package membership_test

// Bring up two clustered nodes A and B (real TLS, node-key auth). Configure
// workspace "repo-only-b" on B only, and tiny provision limits on both.
//
// 1. CreateSandbox on A requesting workspace "repo-only-b" must land on B:
//    poll the op to completion, then assert the returned sandbox id has B's
//    node-id prefix (ADR-0002 self-routing id).
// 2. CreateSandbox on A requesting cpus beyond BOTH nodes' limits must fail the
//    op with a "no capacity"/no-eligible reason.
//
// Use the same client/bootstrap helpers as the existing membership integration
// tests (TLS dialer pinned to the node cert, admin bearer key, JSON REST POST
// to /v1/sandboxes, GET /v1/operations/{id} to poll).
```

Implement against the existing harness: build configs for A and B with `Workspaces`, `ProvisionLimits`, shared `ClusterSecret`, distinct ports; wait for gossip convergence (peer states non-empty) as the existing tests do; POST create; poll the op; assert the owning prefix / failure reason.

- [ ] **Step 2: Run it**

Run: `go test -tags integration ./internal/membership/ -run TestScheduling -timeout 120s -v`
Expected: PASS (workspace-targeted create lands on B; over-limit create fails with a clear reason).

- [ ] **Step 3: Full verification sweep**

Run:
```bash
go build ./... && go vet ./...
go test ./...
go test -race ./internal/scheduler/ ./internal/sandbox/ ./internal/coordinator/
go test -tags integration ./internal/membership/ -timeout 120s
```
Expected: all green.

- [ ] **Step 4: Commit**

```bash
git add internal/membership/scheduling_integration_test.go
git commit -m "test(scheduling): 2-node cross-node placement + no-capacity integration"
```

---

## Self-Review

**Spec coverage (design doc §-by-§):**
- §2 flow (entry→op→coordinator→attempt→Provision/local) → Tasks 4, 9, 11, 12 ✓
- §3 scheduler (3-resource filter/score/tiebreak, strategies) → Task 1 ✓
- §4 capacity (auto-detect limits, atomic TryReserve, convert-on-success, SetBase) → Tasks 2, 3, 7 ✓
- §5 coordinator → Task 4 ✓
- §6 Provision RPC + node-gated authz + effective sizing + strategy + op-id requestID → Tasks 8, 9, 10, 11 ✓
- §7 NodeState/config additions + backend-derived templates + resolved-limit advertisement → Tasks 5, 6, 12 ✓
- §8 testing (unit incl. race; integration) → every task + Task 13 ✓
- §9 deferred (disk enforcement, least-actual-load, gossiped reservations) → not built (intentional) ✓
- §10 invariants (no secret leakage; all methods classified; additive wire; standalone keeps working) → Task 10 drift-guard; Task 7 default-unlimited capacity preserves standalone ✓

**Placeholder scan:** No "TBD"/"handle errors"/"similar to". Two honest deferrals are marked with `// ponytail:` (SDK template ref format; built-in size floor) and one prose note where a test must mirror an existing helper (`newTestStore`/`newProvisionTestStore`/`newSandboxServiceForTest`) — these reference real, present patterns rather than inventing APIs.

**Type consistency:**
- `Candidate`/`Request` field names (`LimitCPU/LimitMem/LimitDisk`, `AllocCPU/AllocMem/AllocDisk`, `CPU/Mem/Disk`) identical across Tasks 1, 4, 12.
- `Capacity` API (`NewCapacity(cpu,mem,disk)`, `TryReserve`, `Commit`, `Release`, `SetBase`, `Snapshot`, `Limits`) consistent across Tasks 2, 7, 9, 12.
- `coordinator.New(candidatesFn)` + `Provision(ctx, req, attempt)` + `AttemptFunc`/`ErrNack`/`ErrNoCapacity` consistent across Tasks 4, 12.
- `Manager.AdmitAndCreate`/`Capacity()`/`SetCapacity`/`ErrNoCapacity` consistent across Tasks 7, 9, 12.
- `sbxv1.ProvisionRequest{Spec, RequestId}` / `ProvisionReply{Accepted, SandboxId, Reason}` consistent across Tasks 8, 9, 12.
- `effectiveSpec`/`resolveStrategy`/`requestFromSpec`/`PlaceFunc` consistent across Tasks 11, 12.
- `sandbox.DetectHostLimits` (exported) used by Task 12; defined in Task 3 follow-up (Task 12 Step 6).
