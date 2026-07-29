# Cordon Survives a Restart Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop a node restart from silently un-cordoning the node across the whole swarm.

**Architecture:** Persist the cordon and drain flags to the existing bbolt store,
and restore them at boot by calling the cluster's existing `SetCordoned`. Four
tasks: storage helpers, the `NodeService` write hook, the `node.go` wiring, then
the docs.

**Tech Stack:** Go 1.x, bbolt via `internal/store`, memberlist via `internal/membership`.

**Spec:** `docs/superpowers/specs/2026-07-29-cordon-survives-restart-design.md`
**ADR:** `docs/adr/0023-a-cordon-survives-a-restart.md` (already committed)

## Global Constraints

- Base is `main` @ `f74f347` or later.
- **`feat/small-gaps` is in flight** in a second worktree at
  `/home/mwchua/sbx-swarm-node-small-gaps`. It also edits `CONTEXT.md`, adding a
  **Custom secret** term after **Workspace credential** (around line 110). Task 4
  adds terms after **Revoke / Revoked** (around line 24), far away, so the two
  should merge cleanly. Do not touch any other file that branch changes:
  `internal/sandbox/sdkbackend.go`, `internal/apiserver/sandboxservice.go`,
  `proto/sbxswarm/v1/sandbox.proto`, `internal/gen/sbxswarm/v1/sandbox.pb.go`,
  `web/app/components/drawer/SecretsTab.vue`, `web/tests/drawer-secrets.spec.ts`.
- The repo is gofmt-dirty but does not enforce it. Format only files you touch.
- `go vet ./...` does not apply the `integration` tag. Also run
  `go vet -tags integration ./internal/...`.
- `TestNode_Gerrit_Publish` is red unless the local Gerrit stack is running (see
  `dev/gerrit/README.md`). Environmental, not yours.
- Plain, short English in comments and commit messages. One idea per sentence.
- **Cordon is inert on a standalone node.** No `cluster_secret` means no cluster
  (`internal/node/node.go:217`), so `cordoner` is nil and `SetCordoned` is never
  reached. Every test that must observe a cordon has to set both `GossipAddr` and
  `ClusterSecret`.
- Do not add `StateVersion` arbitration to `routing.Table.Upsert`. The spec
  rejects it with reasons.

## File Structure

| File | Task | Responsibility |
|---|---|---|
| `internal/store/store.go` | 1 | Declare the `node` bucket |
| `internal/node/flags.go` (new) | 1 | Load and save the operator flags |
| `internal/node/flags_test.go` (new) | 1 | Round trip, including the absent case |
| `internal/apiserver/nodeservice.go` | 2, 4 | Persist hook; honest `Drain` comment |
| `internal/apiserver/nodeservice_test.go` | 2 | The hook fires with the right pair |
| `internal/node/node.go` | 3 | Restore at boot, wire the persister |
| `internal/node/node_test.go` | 3 | A cordoned node boots cordoned |
| `CONTEXT.md` | 4 | Define **Cordon** and **Drain** |
| `README.md` | 4 | State that config is restart-only |

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

### Task 3: Restore at boot, and save on change

**Files:**
- Modify: `internal/node/node.go` — inside the cluster block that starts at line
  217, next to `nodeSvc.SetCordoner(cl)` at line 233
- Test: `internal/node/node_test.go`

**Interfaces:**
- Consumes: `loadNodeFlags` / `saveNodeFlags` / `nodeFlags` from Task 1, and
  `(*NodeService).SetFlagPersister` from Task 2.
- Produces: nothing.

**Restore by calling `cl.SetCordoned(true)`. Do not set `localNS.Cordoned`.**
This is the whole point of the task. Enforcement reads the *routing table* —
`tbl.IsCordoned(id.NodeID)` at `node.go:304`, and the scheduler's candidate
filter — and `NewCluster` (`internal/membership/cluster.go:42-55`) never seeds a
self entry. The only self-upsert in that file is inside `SetCordoned`
(`cluster.go:156`). Seeding `localNS` alone would tell every peer the node is
cordoned while leaving its own table entry absent, so it would place sandboxes on
itself while advertising that it would not.

`SetCordoned` is nil-safe on the memberlist handle (`cluster.go:159`), so calling
it here, before gossip is up, is safe.

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
	// Cordon is inert without a cluster, so both of these are required.
	cfg.GossipAddr = "127.0.0.1:0"
	cfg.ClusterSecret = "test-secret"

	n, err := New(cfg, obs.NewLogger("error", io.Discard), "test")
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = n.Stop(ctx)
	})

	require.True(t, n.cluster.LocalNodeState().Cordoned,
		"a stored cordon must be restored at boot")
}
```

Two things to sort out while writing it, both by reading `node.go` rather than
guessing:

- The `Node` field holding the cluster may not be named `cluster`, and may not be
  reachable from the test. Use whatever `node.go` actually declares; the test is
  in the same package, so an unexported field is fine.
- If `config.Default()` does not exist or the field names differ, copy exactly
  what `TestNode_BootServeStop` uses.

**Why asserting `LocalNodeState().Cordoned` is enough.** It looks like it only
proves the gossiped state, not the routing table. It proves both, because after
this task nothing else sets `local.Cordoned` at boot — `localNS` is left alone
deliberately — so the only way it can be true is if `SetCordoned` ran, and
`SetCordoned` upserts the routing table in the same function. If a later change
starts seeding `localNS.Cordoned` again, this test stops being sufficient; say so
in a comment on the assertion.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/node/ -run TestNode_BootRestoresCordon -v`

Expected: FAIL, the cordon is false because nothing restores it yet.

- [ ] **Step 3: Restore and wire**

In `internal/node/node.go`, inside the
`if cfg.GossipAddr != "" && cfg.ClusterSecret != ""` block, immediately after
`nodeSvc.SetCordoner(cl)` (line 233):

```go
		// A restart must not put a cordoned node back into service (ADR-0023).
		// Restore through SetCordoned, not localNS: enforcement reads the routing
		// table and NewCluster never seeds a self entry, so seeding localNS alone
		// would advertise the cordon to peers while this node kept placing
		// sandboxes on itself.
		flags := loadNodeFlags(st, log)
		if flags.Cordoned {
			cl.SetCordoned(true)
			log.Info("restored cordon from a previous run", "draining", flags.Draining)
		}
		nodeSvc.SetDraining(flags.Draining)
		nodeSvc.SetFlagPersister(func(cordoned, draining bool) {
			saveNodeFlags(st, log, nodeFlags{Cordoned: cordoned, Draining: draining})
		})
```

`SetDraining` and `SetFlagPersister` both come from Task 2. This task adds no
code to `internal/apiserver/`.

The persister is wired only inside the cluster block. That is correct: without a
cluster there is no cordon to persist, and the spec records that cordon is inert
on a standalone node.

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

Restores the stored cordon through Cluster.SetCordoned, which also
upserts this node's own routing entry. Seeding the boot NodeState would
have told peers the node was cordoned while it kept placing sandboxes on
itself, because NewCluster never seeds a self entry.

The cordon is sticky: only an explicit uncordon clears it (ADR-0023)."
```

---

### Task 4: Say what cordon and drain actually mean

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

Nothing migrates. `internal/scheduler` and `internal/apiserver/provision.go`
contain no reference to drain, and `draining` is consumed in exactly one place —
`node.go:247`, building the display row for `ListNodes`.

Replace the comment with:

```go
// Drain cordons the node and records that the cordon came from a drain. The
// marker is display only: the cordon is what blocks new placements. Nothing
// migrates existing sandboxes away — that is not built.
```

- [ ] **Step 2: Add the two glossary terms**

`CONTEXT.md`'s **Revoke / Revoked** entry ends at line 24 with its `_Avoid_`
line, and already says "Distinct from Cordon, which merely stops new placements
on a still-trusted node" — pointing at a term the glossary never defines.

Insert after that entry's trailing blank line, before `**Swarm**:`

```markdown
**Cordon**:
An operator action that stops new placements on a Node while leaving it trusted and its existing
Sandboxes running. It survives a restart: only an explicit uncordon clears it, so a repaired host must
be uncordoned deliberately (ADR-0023). Distinct from Revoke, which ejects the Node's identity entirely.
_Avoid_: disable, pause, quarantine

**Drain**:
A Cordon that also records why it was applied, so an operator can tell a drained Node from a plainly
cordoned one. The marker is display only — the Cordon is what blocks placement. Draining does not move
existing Sandboxes off the Node; nothing migrates them.
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
- Drain is defined as what it is: a cordon that records why. Nothing
  migrates sandboxes away, so the misleading comment is corrected too.
- README states that config is read once and a restart is the way to
  change it."
```

---

## Done when

- [ ] `go build ./...`, `go vet ./...` and `go vet -tags integration ./internal/...` are clean
- [ ] `go test ./...` passes, Gerrit aside
- [ ] A node booted with a stored cordon reports itself cordoned
- [ ] `git diff --stat main` shows exactly these nine files:
      `internal/store/store.go`, `internal/node/flags.go`,
      `internal/node/flags_test.go`, `internal/node/node.go`,
      `internal/node/node_test.go`, `internal/apiserver/nodeservice.go`,
      `internal/apiserver/nodeservice_test.go`, `CONTEXT.md`, `README.md`
- [ ] Manual, on a node with a `cluster_secret` and at least one peer: cordon it,
      restart it, confirm `GET /v1/nodes` still reports it cordoned and that a
      provision is not placed on it. A standalone node cannot show any of this.

## Explicitly out of scope

Decided in the spec. Do not add these, and do not treat them as oversights:

- Config reload in any form.
- `StateVersion` arbitration in `routing.Table.Upsert`.
- Making `Drain` actually migrate sandboxes, or removing the now-redundant
  `Drain` RPC. It is shipped API with a console control.
- Fixing the `Cordon` RPC returning `Cordoned: true` on a standalone node where
  nothing was cordoned. Pre-existing, and its own item.
- Persisting any other in-memory flag. If one is found, it is its own item.
