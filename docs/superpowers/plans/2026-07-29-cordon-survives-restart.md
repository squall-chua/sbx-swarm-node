# Cordon Survives a Restart Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the node's out-of-service switch tell the truth. A restart must not
silently un-cordon the node, a cordon must work without a cluster, and `Drain`
must actually empty the node.

**Architecture:** Persist the cordon and drain flags to the existing bbolt store.
Make the cordon a local flag on `NodeService` and the single source of truth for
this node, so it works standalone and the routing table can drop its copy. Give
`Drain` a background sweep that publishes and stops everything running. Six
tasks: storage helpers, the `NodeService` write hook, the local flag and the
table deletion, the boot restore, the drain sweep, then the docs.

**Tech Stack:** Go 1.x, bbolt via `internal/store`, memberlist via `internal/membership`.

**Spec:** `docs/superpowers/specs/2026-07-29-cordon-survives-restart-design.md`
**ADR:** `docs/adr/0023-a-cordon-survives-a-restart.md` (already committed, and
amended by the same session that amended the spec)

## Global Constraints

- **Rebase first.** This plan was written against local `main` @ `f74f347`, which
  is now 6 commits behind `origin/main` — `feat/small-gaps` merged as `cb02a51`.
  Start from `origin/main` and re-check every line number in this plan; they were
  taken before that merge.
- The repo is gofmt-dirty but does not enforce it. Format only files you touch.
- `go vet ./...` does not apply the `integration` tag. Also run
  `go vet -tags integration ./internal/...`.
- `TestNode_Gerrit_Publish` is red unless the local Gerrit stack is running (see
  `dev/gerrit/README.md`). Environmental, not yours.
- Plain, short English in comments and commit messages. One idea per sentence.
- **Cordon is inert on a standalone node until Task 3.** Before that task, no
  `cluster_secret` means no cluster (`internal/node/node.go:217`), `cordoner` is
  nil and `SetCordoned` is never reached, so a test that must observe a cordon
  needs both `GossipAddr` and `ClusterSecret`. Task 3 removes that requirement,
  and later tasks should not carry it forward out of habit.
- Do not add `StateVersion` arbitration to `routing.Table.Upsert`. The spec
  rejects it with reasons.
- Do not build sandbox migration. The spec explains why it cannot work without a
  stable sandbox handle.

## File Structure

| File | Task | Responsibility |
|---|---|---|
| `internal/store/store.go` | 1 | Declare the `node` bucket |
| `internal/node/flags.go` (new) | 1 | Load and save the operator flags |
| `internal/node/flags_test.go` (new) | 1 | Round trip, including the absent case |
| `internal/apiserver/nodeservice.go` | 2, 3, 5, 6 | Persist hook; the local cordon flag; the drain hook; honest `Drain` comment |
| `internal/apiserver/nodeservice_test.go` | 2, 3, 5 | Hook fires; a cordon without a cluster is real; drain calls the sweep |
| `internal/routing/table.go` | 3 | Delete the cordon field, parameter and getter |
| `internal/routing/table_test.go` | 3 | Drop the cordon assertions |
| `internal/membership/cluster.go` | 3 | Shrink `SetCordoned`; fix four `Upsert` calls |
| `internal/node/node.go` | 3, 4, 5 | Three self reads; boot restore; wire the sweep |
| `internal/node/node_test.go` | 3, 4 | A standalone cordon bites; a cordoned node boots cordoned |
| `internal/apiserver/sandboxservice.go` | 5 | The drain sweep |
| `internal/apiserver/sandboxservice_test.go` | 5 | Sweep stops everything, and cancels |
| `CONTEXT.md` | 6 | Define **Cordon** and **Drain** |
| `README.md` | 6 | State that config is restart-only |

---

### Task 1: Store the flags

**Files:**
- Modify: `internal/store/store.go:19`
- Create: `internal/node/flags.go`
- Test: `internal/node/flags_test.go`

**Interfaces:**
- Consumes: `(*store.Store).Put(bucket, key string, val []byte) error` and
  `(*store.Store).Get(bucket, key string) ([]byte, bool, error)` from
  `internal/store/kv.go`.
- Produces, used by Tasks 2 and 3:
  - `type nodeFlags struct { Cordoned bool; Draining bool }`
  - `func loadNodeFlags(st *store.Store, log *slog.Logger) nodeFlags`
  - `func saveNodeFlags(st *store.Store, log *slog.Logger, f nodeFlags)`

- [ ] **Step 1: Add the bucket**

In `internal/store/store.go:19`, add `"node"` to the end of `bucketNames`:

```go
bucketNames = []string{"meta", "sandboxes", "operations", "idempotency", "blocked_egress", "audit", "revoked", "node"}
```

No schema-version bump. `migrate()` runs `CreateBucketIfNotExists` over every
name on each open (`store.go:56`), so an existing database gains the bucket on
its next start.

- [ ] **Step 2: Write the failing test**

Create `internal/node/flags_test.go`:

```go
package node

import (
	"io"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/squall-chua/sbx-swarm-node/internal/obs"
	"github.com/squall-chua/sbx-swarm-node/internal/store"
)

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "node.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestNodeFlags_AbsentReadsFalse(t *testing.T) {
	log := obs.NewLogger("error", io.Discard)
	require.Equal(t, nodeFlags{}, loadNodeFlags(openTestStore(t), log))
}

func TestNodeFlags_RoundTrip(t *testing.T) {
	log := obs.NewLogger("error", io.Discard)
	st := openTestStore(t)
	saveNodeFlags(st, log, nodeFlags{Cordoned: true, Draining: true})
	require.Equal(t, nodeFlags{Cordoned: true, Draining: true}, loadNodeFlags(st, log))

	// Uncordon writes both back to false, not just the cordon.
	saveNodeFlags(st, log, nodeFlags{})
	require.Equal(t, nodeFlags{}, loadNodeFlags(st, log))
}
```

Check the module path in an existing file's imports before you rely on the one
written above; use whatever `internal/node/node.go` already uses.

- [ ] **Step 3: Run the test to verify it fails**

Run: `go test ./internal/node/ -run TestNodeFlags -v`

Expected: FAIL to compile, `undefined: nodeFlags`.

- [ ] **Step 4: Write the implementation**

Create `internal/node/flags.go`:

```go
package node

import (
	"encoding/json"
	"log/slog"

	"github.com/squall-chua/sbx-swarm-node/internal/store"
)

const (
	flagsBucket = "node"
	flagsKey    = "flags"
)

// nodeFlags is the operator state that must outlive the process. A restart used
// to discard it, which silently put a cordoned node back into service across the
// whole swarm (ADR-0023).
type nodeFlags struct {
	Cordoned bool `json:"cordoned"`
	Draining bool `json:"draining"`
}

// loadNodeFlags reads the stored flags. A missing value is the normal case on a
// fresh node and reads as all-false.
//
// A read error also falls back to all-false, which un-cordons the node — the
// very failure this feature exists to stop. It is logged at error level rather
// than failing the boot: a node that refuses to start is worse than one that
// starts uncordoned and says so loudly.
func loadNodeFlags(st *store.Store, log *slog.Logger) nodeFlags {
	raw, ok, err := st.Get(flagsBucket, flagsKey)
	if err != nil {
		log.Error("node flags unreadable, starting UNCORDONED", "err", err)
		return nodeFlags{}
	}
	if !ok {
		return nodeFlags{}
	}
	var f nodeFlags
	if err := json.Unmarshal(raw, &f); err != nil {
		log.Error("node flags corrupt, starting UNCORDONED", "err", err)
		return nodeFlags{}
	}
	return f
}

// saveNodeFlags persists the flags. Best effort: a write failure is logged and
// the RPC still succeeds, because refusing an operator's cordon because the disk
// is unhappy would be worse than losing it on the next restart.
func saveNodeFlags(st *store.Store, log *slog.Logger, f nodeFlags) {
	raw, err := json.Marshal(f)
	if err != nil { // unreachable for two bools; kept so the error is never dropped silently
		log.Error("node flags marshal failed", "err", err)
		return
	}
	if err := st.Put(flagsBucket, flagsKey, raw); err != nil {
		log.Error("node flags not saved; a restart will lose this state", "err", err)
	}
}
```

Both flags live in one value on purpose. They are always written together and
always read together, so they cannot drift apart.

- [ ] **Step 5: Run the test to verify it passes**

Run: `go test ./internal/node/ -run TestNodeFlags -v`

Expected: PASS, both tests.

- [ ] **Step 6: Verify the build**

```bash
go build ./...
go vet ./...
go vet -tags integration ./internal/...
go test ./internal/store/ ./internal/node/
```

Expected: no output from the first three; PASS from the fourth.

- [ ] **Step 7: Commit**

```bash
git add internal/store/store.go internal/node/flags.go internal/node/flags_test.go
git commit -m "feat(node): store the cordon and drain flags

Adds the node bucket and a load/save pair. Nothing writes them yet.
Both flags share one value so they cannot drift apart."
```

---

### Task 2: Persist on every cordon change

**Files:**
- Modify: `internal/apiserver/nodeservice.go` — struct field near line 50, a setter
  near line 60, and the three RPCs at lines 97, 111 and 128
- Test: `internal/apiserver/nodeservice_test.go`

**Interfaces:**
- Consumes: nothing from Task 1. This task deliberately knows nothing about the
  store; it takes a function.
- Produces, used by Task 3:
  - `func (s *NodeService) SetFlagPersister(fn func(cordoned, draining bool))`
  - `func (s *NodeService) SetDraining(v bool)`

`Cordon`, `Uncordon` and `Drain` are the only three mutation points. `Uncordon`
already clears `draining` (`nodeservice.go:115`), so there is no fourth case.

The hook takes both flags rather than wrapping the `Cordoner` interface, because
only `NodeService` knows both. A `Cordoner` wrapper would see the cordon and miss
the drain marker.

- [ ] **Step 1: Write the failing test**

Add to `internal/apiserver/nodeservice_test.go`. Read the file first and match
its existing construction style for `NewNodeService`.

```go
func TestNodeService_FlagPersisterSeesEveryChange(t *testing.T) {
	type call struct{ cordoned, draining bool }
	var got []call

	s := NewNodeService("n1", "node-1", "test")
	s.SetFlagPersister(func(c, d bool) { got = append(got, call{c, d}) })

	_, err := s.Cordon(context.Background(), &sbxv1.CordonRequest{})
	require.NoError(t, err)
	_, err = s.Drain(context.Background(), &sbxv1.DrainRequest{})
	require.NoError(t, err)
	_, err = s.Uncordon(context.Background(), &sbxv1.CordonRequest{})
	require.NoError(t, err)

	require.Equal(t, []call{
		{cordoned: true, draining: false}, // Cordon
		{cordoned: true, draining: true},  // Drain also sets the marker
		{cordoned: false, draining: false}, // Uncordon clears both
	}, got)
}

func TestNodeService_NilFlagPersisterDoesNotPanic(t *testing.T) {
	s := NewNodeService("n1", "node-1", "test")
	_, err := s.Cordon(context.Background(), &sbxv1.CordonRequest{})
	require.NoError(t, err)
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/apiserver/ -run TestNodeService_ -v`

Expected: FAIL to compile, `s.SetFlagPersister undefined`.

- [ ] **Step 3: Add the field and setter**

In `internal/apiserver/nodeservice.go`, add to the struct beside `draining` (line 50):

```go
	persistFlags              func(cordoned, draining bool)
```

And beside the other wiring setters (near `SetCordoner`, line 60):

```go
// SetFlagPersister wires the store-backed save of the cordon and drain flags
// (node.go). Optional and nil-safe, so existing tests need no change.
func (s *NodeService) SetFlagPersister(fn func(cordoned, draining bool)) { s.persistFlags = fn }

// SetDraining restores the drain marker at boot (node.go). Display only: the
// cordon is what blocks placement.
func (s *NodeService) SetDraining(v bool) { s.draining.Store(v) }

// saveFlags persists the current flags if a persister is wired.
func (s *NodeService) saveFlags(cordoned bool) {
	if s.persistFlags != nil {
		s.persistFlags(cordoned, s.draining.Load())
	}
}
```

- [ ] **Step 4: Call it from the three RPCs**

In `Cordon`, after the `if s.cordoner != nil { s.cordoner.SetCordoned(true) }`
block and before the `return`:

```go
	s.saveFlags(true)
```

In `Uncordon`, after `s.draining.Store(false)` and before the `return`:

```go
	s.saveFlags(false)
```

In `Drain`, after the `if s.cordoner != nil { s.cordoner.SetCordoned(true) }`
block and before the `return`:

```go
	s.saveFlags(true)
```

Order matters in `Uncordon` and `Drain`: `saveFlags` reads `s.draining`, so it
must run after that field is set, never before.

- [ ] **Step 5: Run the test to verify it passes**

Run: `go test ./internal/apiserver/ -run TestNodeService_ -v`

Expected: PASS.

- [ ] **Step 6: Verify nothing else broke**

```bash
go build ./...
go vet ./...
go test ./internal/apiserver/
```

Expected: no output from the first two; PASS from the third.

- [ ] **Step 7: Commit**

```bash
git add internal/apiserver/nodeservice.go internal/apiserver/nodeservice_test.go
git commit -m "feat(apiserver): let NodeService persist cordon changes

Adds an optional, nil-safe persist hook called by Cordon, Uncordon and
Drain. It takes both flags because only NodeService knows the drain
marker. Nothing wires it yet."
```

---

### Task 3: The cordon becomes a local flag

**Files:**
- Modify: `internal/apiserver/nodeservice.go` — the struct near line 50, the three
  RPCs at lines 97, 111 and 128
- Modify: `internal/node/node.go` — three self reads, at lines 246, 304 and 738
- Modify: `internal/routing/table.go` — delete the cordon
- Modify: `internal/membership/cluster.go` — `SetCordoned` and four `Upsert` calls
- Test: `internal/apiserver/nodeservice_test.go`, `internal/node/node_test.go`,
  `internal/routing/table_test.go`

**Interfaces:**
- Produces, used by Tasks 4 and 5: `func (s *NodeService) Cordoned() bool`
- Removes: `func (t *Table) IsCordoned(nodeID string) bool`, and the `cordoned`
  parameter of `Table.Upsert`

**Why.** `Cordon` returns `Cordoned: true` whether or not anything was cordoned.
On a standalone node nothing is, so the console shows a cordon that does not
exist and the node keeps taking work. The fix is not to refuse the RPC; it is to
make the flag local, so it works with or without a cluster.

**The deletion is safe, and here is the proof to re-run before you rely on it:**

```bash
grep -rn "IsCordoned" --include=*.go . | grep -v gen/
```

Every non-test caller asks about **self** (`node.go:304`, `node.go:738`). A
peer's cordon reaches the scheduler through gossip — `ns.Cordoned` in
`buildCandidates` and `rowFromState` — never through the table. Once self reads
the flag, the table's copy has no readers left.

- [ ] **Step 1: Write the failing test**

Add to `internal/apiserver/nodeservice_test.go`:

```go
func TestNodeService_CordonWithoutClusterIsReal(t *testing.T) {
	s := NewNodeService("n1", "node-1", "test") // no cordoner: standalone
	require.False(t, s.Cordoned())

	info, err := s.Cordon(context.Background(), &sbxv1.CordonRequest{})
	require.NoError(t, err)
	require.True(t, info.Cordoned)
	require.True(t, s.Cordoned(), "the reply must not claim a cordon the node did not take")

	_, err = s.Uncordon(context.Background(), &sbxv1.CordonRequest{})
	require.NoError(t, err)
	require.False(t, s.Cordoned())
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/apiserver/ -run TestNodeService_CordonWithoutCluster -v`

Expected: FAIL to compile, `s.Cordoned undefined`.

- [ ] **Step 3: Add the flag**

In `internal/apiserver/nodeservice.go`, beside `draining` (line 50):

```go
	cordoned                  atomic.Bool
```

And a getter beside `Draining()`:

```go
// Cordoned reports this node's cordon. This flag is the single source of truth
// about self: the cluster publishes it to peers but does not own it, so a
// standalone node can be cordoned like any other.
func (s *NodeService) Cordoned() bool { return s.cordoned.Load() }
```

In `Cordon` set `s.cordoned.Store(true)` before the `s.cordoner` call, in
`Uncordon` `s.cordoned.Store(false)`, and in `Drain` `s.cordoned.Store(true)`.
Each reply reports `s.cordoned.Load()` rather than a literal.

Simplify `saveFlags` from Task 2 while you are here: it can read both flags off
the struct now, so drop its parameter and update the three call sites.

- [ ] **Step 4: Switch the three self reads**

In `internal/node/node.go`:

| Line | From | To |
|---|---|---|
| 246 | `Cordoned: clusterInstance != nil && clusterInstance.LocalNodeState().Cordoned` | `Cordoned: nodeSvc.Cordoned()` |
| 304 | `func() bool { return tbl.IsCordoned(id.NodeID) }` | `nodeSvc.Cordoned` |
| 738 | `Cordoned: tbl.IsCordoned(self)` | see below |

`buildCandidates` is a package-level function that does not have `nodeSvc`. Give
it a `selfCordoned bool` parameter and pass `nodeSvc.Cordoned()` at the call site
in the `coordinator.New` closure (line 273), so the value is read fresh on each
placement. Do not pass a captured `bool` from outside the closure; it would go
stale the moment the operator cordons.

- [ ] **Step 5: Delete the cordon from the routing table**

In `internal/routing/table.go`: remove the `cordoned` field from the entry, the
`cordoned` parameter from `Upsert`, and `IsCordoned` entirely.

In `internal/membership/cluster.go`: fix the four `Upsert` calls (lines 156, 297,
329, 339) and delete the self-upsert inside `SetCordoned` with them —
`cluster.go:156` existed only to publish the cordon locally, and nothing reads
self's address or public key out of the table. `SetCordoned` is then three lines:
set `local.Cordoned`, bump `StateVersion`, call `UpdateNode`.

Update `internal/routing/table_test.go`, which asserts `IsCordoned` in two places
(lines 26 and 42). Delete those assertions rather than reworking them; the
behaviour they covered no longer exists.

- [ ] **Step 6: Add the standalone end-to-end test**

Add to `internal/node/node_test.go`. This is the test that proves the point, so
do not skip it if it is fiddly:

```go
func TestNode_StandaloneCordonBlocksPlacement(t *testing.T) {
	// No GossipAddr and no ClusterSecret: this node builds no cluster at all.
	// ... boot as TestNode_BootServeStop does ...
	// cordon through the NodeService, then ask for a sandbox and expect the
	// placement to fail with scheduler.ErrNoEligibleNode.
}
```

Reach the placement the way the node itself does. If driving `CreateSandbox` and
polling its operation proves awkward, the acceptable smaller version is to assert
that the self candidate from `buildCandidates` comes back `Cordoned: true` after
a cordon — but say in a comment which one you did and why.

- [ ] **Step 7: Full verification**

```bash
go build ./...
go vet ./...
go vet -tags integration ./internal/...
go test ./...
```

Expected: PASS, Gerrit aside. The compiler finds every `Upsert` and `IsCordoned`
call site for you; if anything outside the files listed above needs changing,
stop and say so rather than widening the change quietly.

- [ ] **Step 8: Commit**

```bash
git add internal/apiserver/nodeservice.go internal/apiserver/nodeservice_test.go \
        internal/node/node.go internal/node/node_test.go \
        internal/routing/table.go internal/routing/table_test.go \
        internal/membership/cluster.go
git commit -m "feat(node): a cordon works without a cluster

The cordon flag moves onto NodeService and becomes the single source of
truth about this node. Cordon used to answer true while doing nothing on
a standalone node, which builds no cluster.

The routing table drops its cordon copy. Both callers asked about self,
and a peer's cordon has always come from gossip, so nothing read it."
```

---

### Task 4: Restore at boot, and save on change

**Files:**
- Modify: `internal/node/node.go` — before and inside the cluster block that
  starts at line 217
- Test: `internal/node/node_test.go`

**Interfaces:**
- Consumes: `loadNodeFlags` / `saveNodeFlags` / `nodeFlags` from Task 1,
  `(*NodeService).SetFlagPersister` and `SetDraining` from Task 2, and
  `(*NodeService).Cordoned` from Task 3.
- Produces: nothing.

**Restore the local flag first, then mirror to the cluster.** An earlier draft of
this plan said the opposite — restore only through `cl.SetCordoned(true)`,
because enforcement read the routing table. Task 3 deleted the table's copy, so
that reasoning is gone. Setting `localNS.Cordoned` before construction is still
wrong: it would put the value on the gossip wire while the local flag stayed
false, so the node and its peers would disagree.

`SetCordoned` is nil-safe on the memberlist handle (`cluster.go:159`), so calling
it before gossip is up is safe.

- [ ] **Step 1: Write the failing test**

Add to `internal/node/node_test.go`. Model the config on
`TestNode_BootServeStop` (line 76).

```go
func TestNode_BootRestoresCordon(t *testing.T) {
	dir := t.TempDir()

	// Pre-seed the flags as if this node had been cordoned before a restart.
	st, err := store.Open(filepath.Join(dir, "node.db"))
	require.NoError(t, err)
	saveNodeFlags(st, obs.NewLogger("error", io.Discard), nodeFlags{Cordoned: true})
	require.NoError(t, st.Close())

	cfg := config.Default()
	cfg.DataDir = dir
	cfg.ListenAddr = "127.0.0.1:0"
	// A cluster is no longer needed to observe a cordon (Task 3), but keep one
	// here so the mirror-to-cluster half is covered too.
	cfg.GossipAddr = "127.0.0.1:0"
	cfg.ClusterSecret = "test-secret"

	n, err := New(cfg, obs.NewLogger("error", io.Discard), "test")
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = n.Stop(ctx)
	})

	require.True(t, n.nodeSvc.Cordoned(), "a stored cordon must be restored at boot")
	require.True(t, n.cluster.LocalNodeState().Cordoned, "and it must reach the peers")
}
```

Two things to sort out while writing it, both by reading `node.go` rather than
guessing:

- The `Node` fields holding the cluster and the `NodeService` may not be named
  `cluster` and `nodeSvc`, and may not be reachable from the test. Use whatever
  `node.go` actually declares; the test is in the same package, so unexported
  fields are fine. If neither is reachable, add the field rather than asserting
  something weaker.
- If `config.Default()` does not exist or the field names differ, copy exactly
  what `TestNode_BootServeStop` uses.

Both assertions matter. The first proves enforcement, which after Task 3 reads
the local flag. The second proves the peers are told. A restore that does one and
not the other is the bug this whole branch exists to prevent.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/node/ -run TestNode_BootRestoresCordon -v`

Expected: FAIL, the cordon is false because nothing restores it yet.

- [ ] **Step 3: Restore and wire**

In `internal/node/node.go`, **before** the
`if cfg.GossipAddr != "" && cfg.ClusterSecret != ""` block, right after
`nodeSvc` is built (line 215):

```go
	// A restart must not put a cordoned node back into service (ADR-0023). The
	// flag is restored locally and unconditionally: it is what enforcement reads,
	// with or without a cluster.
	flags := loadNodeFlags(st, log)
	nodeSvc.SetCordonedFlag(flags.Cordoned)
	nodeSvc.SetDraining(flags.Draining)
	nodeSvc.SetFlagPersister(func(cordoned, draining bool) {
		saveNodeFlags(st, log, nodeFlags{Cordoned: cordoned, Draining: draining})
	})
	if flags.Cordoned {
		log.Info("restored cordon from a previous run", "draining", flags.Draining)
	}
```

Then inside the cluster block, after `nodeSvc.SetCordoner(cl)` (line 233), mirror
it to the peers:

```go
		if flags.Cordoned {
			cl.SetCordoned(true) // tell the peers what we already know
		}
```

`SetCordonedFlag` is a boot-time setter in the same family as `SetDraining` from
Task 2; add it beside that one, in `internal/apiserver/nodeservice.go`. It is the
only apiserver code this task adds.

The persister is wired unconditionally now. A standalone node has a real cordon
to persist.

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/node/ -run TestNode_BootRestoresCordon -v`

Expected: PASS.

- [ ] **Step 5: Run the full Go suite**

```bash
go build ./...
go vet ./...
go vet -tags integration ./internal/...
go test ./...
```

Expected: PASS, `TestNode_Gerrit_Publish` aside.

- [ ] **Step 6: Commit**

```bash
git add internal/node/node.go internal/node/node_test.go
git commit -m "feat(node): a cordon now survives a restart

Restores the stored flags onto NodeService before the cluster is built,
then mirrors the cordon to the peers when there is a cluster. Seeding the
boot NodeState instead would tell peers the node was cordoned while the
node's own flag stayed false.

The cordon is sticky: only an explicit uncordon clears it (ADR-0023)."
```

---

### Task 5: Drain publishes and stops everything here

**Files:**
- Modify: `internal/apiserver/sandboxservice.go` — the sweep, beside `ReapIdle`
  (line 388)
- Modify: `internal/apiserver/nodeservice.go` — a `drainer` hook, called by `Drain`
- Modify: `internal/node/node.go` — wire the hook
- Test: `internal/apiserver/sandboxservice_test.go`,
  `internal/apiserver/nodeservice_test.go`

**Interfaces:**
- Produces: `func (s *SandboxService) DrainAll(ctx context.Context, actor string, keepGoing func() bool) int`
- Produces: `func (s *NodeService) SetDrainer(fn func(actor string, keepGoing func() bool))`

**What it does.** `Drain` sets both flags, returns at once, and starts a
goroutine that publishes and stops every record with `Status == "running"`. It is
the `ReapIdle` loop over a different list. `Drain` returns `NodeInfo`, which is
shipped API with a console control, so it cannot wait for a sweep that takes
minutes; the per-sandbox events already tell the console what is happening.

- [ ] **Step 1: Write the failing test**

Add to `internal/apiserver/sandboxservice_test.go`. Match how the existing
`ReapIdle` tests build a service over the sandbox fake.

```go
func TestSandboxService_DrainAllStopsEverythingRunning(t *testing.T) {
	// Two running sandboxes, one labelled idle-stop: off.
	// DrainAll stops both: that label protects against the idle timer, not
	// against an operator.
}

func TestSandboxService_DrainAllStopsWhenCancelled(t *testing.T) {
	// keepGoing returns false after the first sandbox. The second is left
	// running, because Uncordon during a sweep must cancel it.
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/apiserver/ -run TestSandboxService_DrainAll -v`

Expected: FAIL to compile, `s.DrainAll undefined`.

- [ ] **Step 3: Write the sweep**

Beside `ReapIdle` in `internal/apiserver/sandboxservice.go`:

```go
// DrainAll publishes and stops every running sandbox on this node. It is what
// Drain does after cordoning: the node ends up empty and git-backed work is
// saved on the way out. Nothing is migrated — a sandbox id names its owner node,
// so a sandbox cannot move without changing identity.
//
// keepGoing is re-checked before each sandbox so an uncordon cancels the sweep.
// actor is carried in because the caller's context dies with its RPC, and the
// audit should not record a bulk operator action as "system".
func (s *SandboxService) DrainAll(ctx context.Context, actor string, keepGoing func() bool) int {
```

List with `s.mgr.List(ctx)`, skip anything whose `Status` is not `"running"`,
and for each: check `keepGoing()`, publish, then `s.mgr.Stop`. Reuse the
publish-then-stop order and the "a publish failure does not skip the stop" rule
from `ReapIdle`; say so in a comment rather than repeating the reasoning.

`maybeAutoPublish` reads the actor from the context (`sandboxservice.go:374`).
Give it an explicit actor instead, either by adding a parameter or by lifting the
publish call out; keep the existing behaviour for its two current callers.

- [ ] **Step 4: Call it from Drain**

In `internal/apiserver/nodeservice.go`, add the hook beside the other setters and
call it at the end of `Drain`:

```go
	if s.drainer != nil {
		go s.drainer(principalOrSystem(ctx), s.draining.Load)
	}
```

`s.draining.Load` is a method value, so the sweep reads the live flag rather than
a copy. Reuse whatever helper already turns a context into an actor string; if
there is none, `principalFromContext(ctx).userRole` with a `"system"` fallback
matches `maybeAutoPublish` exactly. Note that this actor is a role, not a person
— that is what the audit records everywhere else.

Add a `NodeService` test that `Drain` calls the hook and `Cordon` does not.

- [ ] **Step 5: Wire it**

In `internal/node/node.go`, beside the other `nodeSvc.Set*` calls:

```go
	nodeSvc.SetDrainer(func(actor string, keepGoing func() bool) {
		n := sandboxes.DrainAll(nctx, actor, keepGoing)
		log.Info("drain finished", "stopped", n)
	})
```

Use the node's own long-lived context, not a request context. Check the name of
the context in scope there; the reaper ticker at line 149 uses `nctx`.

- [ ] **Step 6: Verify**

```bash
go build ./...
go vet ./...
go vet -tags integration ./internal/...
go test ./...
```

Expected: PASS, Gerrit aside.

- [ ] **Step 7: Commit**

```bash
git add internal/apiserver/sandboxservice.go internal/apiserver/sandboxservice_test.go \
        internal/apiserver/nodeservice.go internal/apiserver/nodeservice_test.go \
        internal/node/node.go
git commit -m "feat(apiserver): Drain now empties the node

Drain cordons the node, then publishes and stops every running sandbox in
the background. It used to set a flag nothing read.

The sweep stops sandboxes labelled idle-stop: off as well. That label
protects against the idle timer, not against an operator. An uncordon
during a sweep cancels it."
```

---

### Task 6: Say what cordon and drain actually mean

**Files:**
- Modify: `CONTEXT.md` — insert after the **Revoke / Revoked** block, which ends
  at line 24
- Modify: `README.md` — under `## Configuration reference` (line 388)
- Modify: `internal/apiserver/nodeservice.go:141-143` — the `Drain` comment

**Interfaces:**
- Consumes: nothing. Documentation only, plus one comment correction.
- Produces: nothing.

- [ ] **Step 1: Correct the Drain comment**

`internal/apiserver/nodeservice.go:141-143` currently claims Drain "sets a
draining flag so the M5 scheduler can gracefully migrate sandboxes away".

Nothing migrates, before this branch or after it. Replace the comment with:

```go
// Drain cordons the node, then publishes and stops every sandbox running on it,
// in the background. The node ends up empty and git-backed work is saved. The
// draining marker records why the node is out of service and survives a restart.
// Nothing is migrated: a sandbox id names its owner node, so a sandbox cannot
// move without changing identity.
```

- [ ] **Step 2: Add the two glossary terms**

`CONTEXT.md`'s **Revoke / Revoked** entry ends at line 24 with its `_Avoid_`
line, and already says "Distinct from Cordon, which merely stops new placements
on a still-trusted node" — pointing at a term the glossary never defines.

Insert after that entry's trailing blank line, before `**Swarm**:`

```markdown
**Cordon**:
An operator action that stops new placements on a Node while leaving it trusted and its existing
Sandboxes running. It applies to any Node, clustered or standalone, and it survives a restart: only an
explicit uncordon clears it, so a repaired host must be uncordoned deliberately (ADR-0023). Distinct
from Revoke, which ejects the Node's identity entirely.
_Avoid_: disable, pause, quarantine

**Drain**:
A Cordon followed by publishing and stopping every Sandbox on the Node, so the Node ends up empty and
git-backed work is saved. It also records why the Node is out of service. Draining does not move
Sandboxes to other Nodes — the swarm never does, because a Sandbox id names its owner Node.
_Avoid_: evacuate, migrate (neither happens)
```

Match the file's shape: `**Term**:`, description lines wrapped at about 110
characters, then one `_Avoid_:` line. `CONTEXT.md` is a glossary and nothing
else — no implementation detail.

- [ ] **Step 3: State that config is restart-only**

Under `## Configuration reference` in `README.md` (line 388), add a short
paragraph near the top of the section:

```markdown
Configuration is read once, at startup. There is no reload: changing a workspace,
a kit, a template constraint or a git provider means restarting the node. That is
the intended design, not a gap. A restart is cheap — the sandbox daemon owns the
sandboxes and the node reconciles its records against them at boot — and a cordon
now survives a restart, so restarting a node you deliberately took out of service
will not put it back into service.
```

- [ ] **Step 4: Verify**

```bash
go build ./...
go vet ./...
go test ./internal/apiserver/
git diff --stat
```

Expected: no output from the first two, PASS from the third, and exactly three
files changed.

- [ ] **Step 5: Commit**

```bash
git add CONTEXT.md README.md internal/apiserver/nodeservice.go
git commit -m "docs: define Cordon and Drain, and say config is restart-only

- CONTEXT.md defines Cordon, which the Revoke entry already pointed at
  but nothing defined, and Drain.
- Drain is defined as what it now does: cordon, then publish and stop
  everything here. The misleading migration comment is corrected too.
- README states that config is read once and a restart is the way to
  change it."
```

---

## Done when

- [ ] `go build ./...`, `go vet ./...` and `go vet -tags integration ./internal/...` are clean
- [ ] `go test ./...` passes, Gerrit aside
- [ ] A node booted with a stored cordon reports itself cordoned
- [ ] A **standalone** node — no `cluster_secret` — can be cordoned, and the
      cordon blocks placement
- [ ] `grep -rn "IsCordoned" --include=*.go . | grep -v gen/` returns nothing
- [ ] `git diff --stat origin/main` shows exactly these fourteen files:
      `internal/store/store.go`, `internal/node/flags.go`,
      `internal/node/flags_test.go`, `internal/node/node.go`,
      `internal/node/node_test.go`, `internal/apiserver/nodeservice.go`,
      `internal/apiserver/nodeservice_test.go`,
      `internal/apiserver/sandboxservice.go`,
      `internal/apiserver/sandboxservice_test.go`, `internal/routing/table.go`,
      `internal/routing/table_test.go`, `internal/membership/cluster.go`,
      `CONTEXT.md`, `README.md`
- [ ] Manual, on a node with a `cluster_secret` and at least one peer: cordon it,
      restart it, confirm `GET /v1/nodes` still reports it cordoned and that a
      provision is not placed on it.
- [ ] Manual drain: with two running sandboxes, one git-backed, press Drain and
      confirm the node empties, the git work is published, and the audit records
      the caller rather than `system`.

## Explicitly out of scope

Decided in the spec. Do not add these, and do not treat them as oversights:

- Config reload in any form.
- `StateVersion` arbitration in `routing.Table.Upsert`.
- Migrating Sandboxes to another Node, and the stable sandbox handle it would
  need. Removing the `Drain` RPC is also out: it is shipped API with a console
  control, and after Task 5 it does real work.
- A better message than `no eligible node` when a cordoned standalone node
  refuses a placement. It would need the scheduler to report which constraint
  rejected each candidate.
- Persisting any other in-memory flag. If one is found, it is its own item.
