# sbx-swarm-node M7 — TTL / Idle Reaper Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development or superpowers:executing-plans. Checkbox steps.
>
> **Forward-looking:** depends on M1c (`Manager`, `Record`), M1c/M1d ops, M6 (publish-on-stop). Reconcile signatures against real code.

**Goal:** Reclaim abandoned sandboxes safely — enforce an absolute **TTL**, and an **opt-in, activity-based idle** timeout that **never** fires on a sandbox with an in-flight operation and **never** uses CPU; idle → **stop** (which triggers publish-on-stop), with stopped sandboxes removed after a retention window (spec §7 reaper).

**Architecture:** `reaper.Reaper.Sweep()` is pure decision logic over injected dependencies (clock, record lister, active-op check, stop/remove funcs) — fully unit-tested with a fake clock. `Manager` gains `last_activity_at` (bumped on any swarm-side touch) and `ops.Manager` gains `HasActive(sandboxID)`. A ticker drives `Sweep` in `node`.

**Tech Stack:** Go 1.23, M1/M6 stack.

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/sandbox/manager.go` | add `LastActivityAt` to `Record`, `Touch(id)` |
| `internal/ops/ops.go` | `HasActive(sandboxID) bool` |
| `internal/reaper/reaper.go` | `Sweep` decision logic |
| `internal/config/config.go` | reaper interval + retention |
| `internal/node/node.go` | ticker loop; bump activity on exec/terminal/publish |

---

## Task 1: Activity tracking + active-op check

**Files:** `internal/sandbox/manager.go`, `internal/ops/ops.go`, tests alongside

- [ ] **Step 1: Failing tests**

`internal/sandbox/manager_activity_test.go`:

```go
package sandbox

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/squall-chua/sbx-swarm-node/internal/ids"
	"github.com/squall-chua/sbx-swarm-node/internal/store"
	"github.com/stretchr/testify/require"
)

func TestManager_TouchBumpsLastActivity(t *testing.T) {
	st, _ := store.Open(filepath.Join(t.TempDir(), "n.db"))
	t.Cleanup(func() { _ = st.Close() })
	m := NewManager("n1", NewFake(), st, ids.NewGen("n1"))

	rec, err := m.Create(context.Background(), CreateSpec{})
	require.NoError(t, err)
	before := rec.LastActivityAt

	time.Sleep(2 * time.Millisecond)
	require.NoError(t, m.Touch(rec.ID))

	got, _ := m.Get(context.Background(), rec.ID)
	require.True(t, got.LastActivityAt.After(before))
}
```

`internal/ops/ops_active_test.go`:

```go
package ops

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOps_HasActive(t *testing.T) {
	m := newMgr(t)
	op, _, _ := m.Start(context.Background(), "agent-run", "")
	// associate the op with a sandbox by setting SandboxID then persisting
	op.SandboxID = "sb1"
	op.State = "running"
	require.NoError(t, m.put(op))

	require.True(t, m.HasActive("sb1"))
	require.False(t, m.HasActive("other"))
}
```

- [ ] **Step 2: Run → FAIL**: `go test ./internal/sandbox/ -run TestManager_Touch -v` and `go test ./internal/ops/ -run TestOps_HasActive -v`

- [ ] **Step 3: Implement**

In `record.go`, add to `Record`:

```go
	LastActivityAt time.Time `json:"last_activity_at"`
```

In `manager.go` `Create`, set `rec.LastActivityAt = m.now()` before save. Add:

```go
// Touch updates a sandbox's last-activity timestamp (called on any swarm-side
// interaction: exec, terminal, publish, lifecycle).
func (m *Manager) Touch(id string) error {
	rec, err := m.Get(context.Background(), id)
	if err != nil {
		return err
	}
	rec.LastActivityAt = m.now()
	return m.save(rec)
}
```

In `ops.go`, add:

```go
// HasActive reports whether a non-terminal operation references sandboxID.
func (m *Manager) HasActive(sandboxID string) bool {
	active := false
	_ = m.store.ForEach(opBucket, func(_, v []byte) error {
		var op Operation
		if err := json.Unmarshal(v, &op); err != nil {
			return nil
		}
		if op.SandboxID == sandboxID && (op.State == "pending" || op.State == "running") {
			active = true
		}
		return nil
	})
	return active
}
```

- [ ] **Step 4: Run → PASS, commit**

```bash
go test ./internal/sandbox/ ./internal/ops/ -v
git add internal/sandbox/ internal/ops/
git commit -m "feat(sandbox,ops): last-activity tracking + active-op check"
```

---

## Task 2: Reaper sweep logic

**Files:** `internal/reaper/reaper.go`, test `internal/reaper/reaper_test.go`

- [ ] **Step 1: Failing test**

```go
package reaper

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSweep_TTLIdleAndRetention(t *testing.T) {
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	recs := []Record{
		{ID: "ttl", Status: "running", CreatedAt: now.Add(-2 * time.Hour), TTL: time.Hour},
		{ID: "idle", Status: "running", IdleReap: true, IdleTimeout: 30 * time.Minute, LastActivity: now.Add(-time.Hour)},
		{ID: "idle-busy", Status: "running", IdleReap: true, IdleTimeout: 30 * time.Minute, LastActivity: now.Add(-time.Hour)},
		{ID: "active", Status: "running", LastActivity: now},
		{ID: "stopped-old", Status: "stopped", StoppedAt: now.Add(-2 * time.Hour)},
	}
	d := Deps{
		Now:         func() time.Time { return now },
		List:        func() []Record { return recs },
		HasActiveOp: func(id string) bool { return id == "idle-busy" },
		Retention:   time.Hour,
	}
	var stopped, removed []string
	d.Stop = func(id string) error { stopped = append(stopped, id); return nil }
	d.Remove = func(id string) error { removed = append(removed, id); return nil }

	NewReaper(d).Sweep()

	require.ElementsMatch(t, []string{"idle"}, stopped)       // idle stopped; idle-busy skipped (active op); active untouched
	require.ElementsMatch(t, []string{"ttl", "stopped-old"}, removed) // ttl expired + stopped past retention
}
```

- [ ] **Step 2: Run → FAIL**: `go test ./internal/reaper/ -v`

- [ ] **Step 3: Implement `reaper.go`**

```go
// Package reaper enforces absolute TTL and opt-in activity-based idle timeouts.
// Idle never uses CPU and never fires while an operation is active (spec §7).
package reaper

import "time"

// Record is the reaper's view of a sandbox.
type Record struct {
	ID           string
	Status       string
	CreatedAt    time.Time
	TTL          time.Duration // 0 = no TTL
	IdleReap     bool
	IdleTimeout  time.Duration
	LastActivity time.Time
	StoppedAt    time.Time
}

// Deps are the reaper's injected dependencies.
type Deps struct {
	Now         func() time.Time
	List        func() []Record
	HasActiveOp func(id string) bool
	Stop        func(id string) error // triggers publish-on-stop in the Manager
	Remove      func(id string) error
	Retention   time.Duration // remove stopped sandboxes older than this (0 = never)
}

// Reaper runs sweeps.
type Reaper struct{ d Deps }

// NewReaper builds a reaper.
func NewReaper(d Deps) *Reaper { return &Reaper{d: d} }

// Sweep performs one reaping pass.
func (r *Reaper) Sweep() {
	now := r.d.Now()
	for _, rec := range r.d.List() {
		switch rec.Status {
		case "running":
			// Absolute TTL (always available): remove outright.
			if rec.TTL > 0 && now.Sub(rec.CreatedAt) >= rec.TTL {
				_ = r.d.Remove(rec.ID)
				continue
			}
			// Activity-based idle (opt-in): stop (never while an op is active).
			if rec.IdleReap && rec.IdleTimeout > 0 && now.Sub(rec.LastActivity) >= rec.IdleTimeout {
				if r.d.HasActiveOp != nil && r.d.HasActiveOp(rec.ID) {
					continue // active op => not idle
				}
				_ = r.d.Stop(rec.ID) // Manager.Stop triggers publish-on-stop (M6)
			}
		case "stopped":
			if r.d.Retention > 0 && !rec.StoppedAt.IsZero() && now.Sub(rec.StoppedAt) >= r.d.Retention {
				_ = r.d.Remove(rec.ID)
			}
		}
	}
}
```

- [ ] **Step 4: Run → PASS, commit**

```bash
go test ./internal/reaper/ -v
git add internal/reaper/ && git commit -m "feat(reaper): TTL + activity-based idle + retention sweep"
```

---

## Task 3: Wire reaper into the node + activity bumps

**Files:** `internal/config/config.go`, `internal/node/node.go`, `internal/sandbox/manager.go`

- [ ] **Step 1: Config** — add `ReaperInterval time.Duration` (default 60s) and `StoppedRetention time.Duration` (default 24h). Add `StoppedAt time.Time` to `Record`, set in `Manager.Stop`.

- [ ] **Step 2: Bump activity** — call `mgr.Touch(id)` in `SandboxService.Exec`, `AgentRun`, the terminal/logs SSE handlers, and `PublishSandbox`. (One-line each; covered by the M7 Task 1 test for `Touch` itself.)

- [ ] **Step 3: Wire the loop** — in `node.New`/`Start`, build:

```go
	rp := reaper.NewReaper(reaper.Deps{
		Now:  time.Now,
		List: func() []reaper.Record { return mgr.ReaperView() }, // maps records → reaper.Record
		HasActiveOp: opsM.HasActive,
		Stop:        func(id string) error { return mgr.Stop(n.ctx, id) },
		Remove:      func(id string) error { return mgr.Delete(n.ctx, id) },
		Retention:   cfg.StoppedRetention,
	})
	go runTicker(n.ctx, cfg.ReaperInterval, rp.Sweep)
```

Add `Manager.ReaperView()` returning `[]reaper.Record` built from records (id/status/createdAt/ttl/idleReap/idleTimeout/lastActivity/stoppedAt from `Record` + `Spec`). (`runTicker`/`n.ctx` from M2.)

- [ ] **Step 4: Run all + commit**

```bash
go test ./...
git add internal/config/ internal/node/ internal/sandbox/
git commit -m "feat(reaper): wire sweep loop + activity bumps + retention"
```

---

## Self-Review

**Spec coverage (M7):** absolute TTL → Task 2 ✓; opt-in activity-based idle (never CPU, never while op active) → Tasks 1,2 ✓; idle → stop (triggers publish-on-stop from M6) → Task 2/3 ✓; retention removal of stopped sandboxes → Task 2 ✓; `last_activity_at` bumped on swarm-side touch → Tasks 1,3 ✓. Matches the M10/idle decision recorded in the spec §7 / the reaper component row.

**Placeholder scan:** Sweep logic fully coded + unit-TDD'd with a fake clock covering TTL/idle/idle-busy/active/retention cases. `ReaperView`/activity-bump call sites are specified concretely (one-liners). No TBD/TODO.

**Type consistency:** `sandbox.Manager.{Touch,Stop(sets StoppedAt),ReaperView}`, `Record.{LastActivityAt,StoppedAt}`; `ops.Manager.HasActive(id)`; `reaper.NewReaper(Deps).Sweep()` with `Deps` fields matching the wiring.
