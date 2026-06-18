# Operability Hardening Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop a node restart from stranding in-flight operations, and realise ADR-0004's gossiped per-node-key denylist (revocation).

**Architecture:** Two independent, additive features. (A) `ops.Manager` gains a boot-time sweep that marks non-terminal operations as `error`. (B) A grow-only, store-persisted, gossiped union of revoked `node_id`s owned by `membership.Cluster`; an admin `RevokeNode` RPC writes it, `IsRevoked` gates `nodekey.Verify` (a revoked node's node-auth is rejected swarm-wide).

**Tech Stack:** Go 1.25, bbolt (`internal/store`), hashicorp/memberlist (`internal/membership`), grpc + grpc-gateway (buf codegen), testify.

**Spec:** `docs/superpowers/specs/2026-06-17-operability-hardening-design.md` · **ADRs:** ADR-0004, **ADR-0013**.

## Global Constraints

- **Go 1.25**; run `gofmt -w` on every changed `.go` file before commit.
- **Standalone must keep working** — a node with no cluster (`clusterInstance == nil`) must build and run; revocation degrades to `FailedPrecondition`/empty, recovery is unaffected.
- **Every new gRPC method must be classified** in `internal/apiserver/authz.go` (`mutatingMethods`/`readMethods`/`internalMethods`) or `TestAuthz_AllMethodsClassified` fails.
- **Codegen:** edit `.proto` → `buf generate` (from repo root; buf 1.66 + remote BSR plugins, needs network) → `go build ./...` → commit the regenerated `internal/gen/sbxswarm/v1/*` (git-tracked). `go build` does NOT compile tests — run `go vet`/`go test` to catch test breakage. **Ignore gopls "undefined/redeclared/MissingLitField" diagnostics after codegen — trust the `go` toolchain.**
- **Secrets invariant (spec §11):** never log/persist/gossip secret values, keys, or tokens. `node_id`s are non-sensitive (already gossiped). The `Revoked` field and `revoked` bucket carry only `node_id`s — clean.
- **Verify with the real toolchain:** `go build ./... && go vet ./... && go test ./...`; `-race` on `internal/membership`; run the integration suite (Task 5) when cross-node behavior changes.

---

### Task 1: Ops crash-recovery sweep

**Files:**
- Modify: `internal/ops/ops.go` (add `RecoverInterrupted`)
- Modify: `internal/node/node.go:266-269` (call it at boot)
- Test: `internal/ops/ops_test.go`

**Interfaces:**
- Produces: `func (m *Manager) RecoverInterrupted() (int, error)` — marks every non-terminal op `error`, returns the count swept.

- [x] **Step 1: Write the failing test**

Add to `internal/ops/ops_test.go` (the package already imports `store`, `context`, `time`, `require`):

```go
func TestOps_RecoverInterrupted(t *testing.T) {
	m := newMgr(t)

	// Seed three ops directly in the store: pending, running, done.
	pending := &Operation{ID: "op-pending", Type: "provision", State: "pending", CreatedAt: m.now()}
	running := &Operation{ID: "op-running", Type: "agent-run", State: "running", CreatedAt: m.now()}
	done := &Operation{ID: "op-done", Type: "remove", State: "done", SandboxID: "sb1", CreatedAt: m.now()}
	for _, op := range []*Operation{pending, running, done} {
		require.NoError(t, m.put(op))
	}
	// An idempotency key mapped to the pending op must still resolve afterwards.
	require.NoError(t, m.store.Put(idemBucket, "key-1", []byte("op-pending")))

	n, err := m.RecoverInterrupted()
	require.NoError(t, err)
	require.Equal(t, 2, n, "pending + running swept; done left alone")

	gotPending, _ := m.Get("op-pending")
	require.Equal(t, "error", gotPending.State)
	require.Contains(t, gotPending.Error, "interrupted")

	gotRunning, _ := m.Get("op-running")
	require.Equal(t, "error", gotRunning.State)

	gotDone, _ := m.Get("op-done")
	require.Equal(t, "done", gotDone.State, "terminal ops are untouched")
	require.Empty(t, gotDone.Error)

	raw, ok, err := m.store.Get(idemBucket, "key-1")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "op-pending", string(raw), "idempotency mapping survives recovery")
}
```

- [x] **Step 2: Run test to verify it fails**

Run: `go test ./internal/ops/ -run TestOps_RecoverInterrupted -v`
Expected: FAIL — build error `m.RecoverInterrupted undefined`.

- [x] **Step 3: Write minimal implementation**

Add to `internal/ops/ops.go` (after `Get`):

```go
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
```

- [x] **Step 4: Run test to verify it passes**

Run: `go test ./internal/ops/ -run TestOps_RecoverInterrupted -v`
Expected: PASS.

- [x] **Step 5: Wire the sweep into boot**

In `internal/node/node.go`, the boot reconcile block currently reads:

```go
	// Best-effort reconcile of persisted records against backend truth at boot.
	if err := mgr.Reconcile(context.Background()); err != nil {
		log.Warn("initial reconcile failed", "err", err)
	}
```

Append immediately after it:

```go
	// Unstick operations left non-terminal by a previous crash (ops crash-recovery).
	if n, rerr := opsM.RecoverInterrupted(); rerr != nil {
		log.Warn("op recovery failed", "err", rerr)
	} else if n > 0 {
		log.Info("recovered interrupted operations", "count", n)
	}
```

- [x] **Step 6: Verify build + full ops suite, then commit**

Run: `gofmt -w internal/ops/ops.go internal/node/node.go && go build ./... && go vet ./internal/ops/ ./internal/node/ && go test ./internal/ops/`
Expected: all PASS.

```bash
git add internal/ops/ops.go internal/ops/ops_test.go internal/node/node.go
git commit -m "feat(ops): mark interrupted operations as error at boot (crash-recovery)"
```

---

### Task 2: Revocation proto surface + codegen + authz classification

**Files:**
- Modify: `proto/sbxswarm/v1/node.proto`
- Regenerate: `internal/gen/sbxswarm/v1/*` (via `buf generate`)
- Modify: `internal/apiserver/authz.go:23-37,47-57`

**Interfaces:**
- Produces (generated Go types/methods on `NodeServiceServer`): `RevokeNode(ctx, *RevokeNodeRequest) (*RevokedList, error)`, `ListRevoked(ctx, *ListRevokedRequest) (*RevokedList, error)`; messages `RevokeNodeRequest{NodeId string}`, `ListRevokedRequest{}`, `RevokedList{NodeIds []string}`.

- [x] **Step 1: Add the RPCs and messages to the proto**

In `proto/sbxswarm/v1/node.proto`, inside `service NodeService { ... }` (after the `Drain` rpc, before the closing `}`):

```proto
  rpc RevokeNode(RevokeNodeRequest) returns (RevokedList) {
    option (google.api.http) = {
      post: "/v1/node/revoke"
      body: "*"
    };
  }
  rpc ListRevoked(ListRevokedRequest) returns (RevokedList) {
    option (google.api.http) = {get: "/v1/node/revoked"};
  }
```

And after the `DrainRequest` message (top level):

```proto
message RevokeNodeRequest {
  string node_id = 1;
}

message ListRevokedRequest {}

message RevokedList {
  repeated string node_ids = 1;
}
```

- [x] **Step 2: Regenerate and verify it compiles**

Run (from repo root): `buf generate && go build ./...`
Expected: PASS. `internal/gen/sbxswarm/v1/node.pb.go` and `node.pb.gw.go` now contain the new types/handlers; `NodeService` still satisfies the server interface via the embedded `UnimplementedNodeServiceServer` (the methods return `codes.Unimplemented` until Task 4).

- [x] **Step 3: Run the drift guard to confirm it now fails (methods unclassified)**

Run: `go test ./internal/apiserver/ -run TestAuthz_AllMethodsClassified -v`
Expected: FAIL — `method /sbxswarm.v1.NodeService/RevokeNode is not classified` (and `ListRevoked`). This confirms the new methods are visible to the guard.

- [x] **Step 4: Classify the methods**

In `internal/apiserver/authz.go`, add to `mutatingMethods` (after the `Drain` entry):

```go
	"/sbxswarm.v1.NodeService/RevokeNode":        true,
```

And add to `readMethods` (after the `GetNodeInfo` entry):

```go
	"/sbxswarm.v1.NodeService/ListRevoked":       true,
```

- [x] **Step 5: Verify build + drift guard pass**

Run: `gofmt -w internal/apiserver/authz.go && go build ./... && go test ./internal/apiserver/ -run TestAuthz_AllMethodsClassified -v`
Expected: PASS.

- [x] **Step 6: Commit**

```bash
git add proto/sbxswarm/v1/node.proto internal/gen/sbxswarm/v1/ internal/apiserver/authz.go
git commit -m "feat(proto): RevokeNode/ListRevoked RPCs + authz classification"
```

---

### Task 3: Membership revoked union (gossiped + persisted)

**Files:**
- Modify: `internal/membership/state.go:29-31` (add `Revoked` bulk field)
- Modify: `internal/store/store.go:19` (add the `revoked` bucket)
- Create: `internal/membership/revocation.go`
- Modify: `internal/membership/cluster.go` (struct fields; `NewCluster` `st` param + seed; `MergeRemoteState` fold)
- Modify: `internal/node/node.go:195` (pass `st` to `NewCluster`)
- Modify: `internal/membership/cluster_test.go` (init `revoked` map in `newTestDelegate`)
- Create: `internal/membership/revocation_test.go`

**Interfaces:**
- Consumes: `store.Store.Put/ForEach/Get`, `routing.NewTable`.
- Produces: `func (c *Cluster) Revoke(nodeID string) error`; `func (c *Cluster) IsRevoked(nodeID string) bool`; `func (c *Cluster) RevokedList() []string`; `NodeState.Revoked []string`; new `NewCluster(cfg, local, tbl, si, siPath, st *store.Store, onNodeDead, log)` signature; package const `revokedBucket = "revoked"`.

- [x] **Step 1: Write the failing tests**

Create `internal/membership/revocation_test.go`:

```go
package membership

import (
	"io"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/squall-chua/sbx-swarm-node/internal/routing"
	"github.com/squall-chua/sbx-swarm-node/internal/store"
	"github.com/stretchr/testify/require"
)

// newRevCluster builds a bare Cluster (no live memberlist; ml nil so UpdateNode
// no-ops) backed by a temp store — enough to exercise the revoked union.
func newRevCluster(t *testing.T, self string) *Cluster {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "n.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	return &Cluster{
		local:      NodeState{NodeID: self, ProtocolVersion: ProtocolVersion},
		peerStates: map[string]NodeState{},
		tbl:        routing.NewTable(self),
		st:         st,
		revoked:    map[string]struct{}{},
		log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestRevoke_AddsAndAdvertises(t *testing.T) {
	c := newRevCluster(t, "nA")
	before := c.LocalNodeState().StateVersion
	require.NoError(t, c.Revoke("nB"))
	require.True(t, c.IsRevoked("nB"))
	require.Equal(t, []string{"nB"}, c.RevokedList())
	require.Equal(t, []string{"nB"}, c.LocalNodeState().Revoked, "revoked set is advertised")
	require.Equal(t, before+1, c.LocalNodeState().StateVersion)
}

func TestRevoke_RejectsSelfAndEmpty(t *testing.T) {
	c := newRevCluster(t, "nA")
	require.Error(t, c.Revoke("nA"))
	require.Error(t, c.Revoke(""))
	require.Empty(t, c.RevokedList())
}

func TestRevoke_IdempotentNoSecondBump(t *testing.T) {
	c := newRevCluster(t, "nA")
	require.NoError(t, c.Revoke("nB"))
	v := c.LocalNodeState().StateVersion
	require.NoError(t, c.Revoke("nB"))
	require.Equal(t, v, c.LocalNodeState().StateVersion, "re-revoking must not bump version")
	require.Len(t, c.RevokedList(), 1)
}

func TestRevoke_PersistsAndReloads(t *testing.T) {
	c := newRevCluster(t, "nA")
	require.NoError(t, c.Revoke("nB"))
	require.NoError(t, c.Revoke("nC"))

	// Simulate restart: a fresh Cluster over the SAME store re-seeds the union.
	c2 := &Cluster{
		local:      NodeState{NodeID: "nA", ProtocolVersion: ProtocolVersion},
		peerStates: map[string]NodeState{},
		tbl:        routing.NewTable("nA"),
		st:         c.st,
		revoked:    map[string]struct{}{},
		log:        c.log,
	}
	c2.loadRevoked()
	require.True(t, c2.IsRevoked("nB"))
	require.True(t, c2.IsRevoked("nC"))
	require.Equal(t, []string{"nB", "nC"}, c2.LocalNodeState().Revoked)
}

func TestMergeRemoteState_FoldsRemoteRevoked(t *testing.T) {
	c := newRevCluster(t, "nA")
	c.si = &SwarmIdentity{SwarmID: "swarm-A", Mode: ModeRejoin}
	d := &delegate{c: c}

	remote := NodeState{
		NodeID:          "nB",
		Addr:            "10.0.0.2:8443",
		ProtocolVersion: ProtocolVersion,
		SwarmID:         "swarm-A",
		Revoked:         []string{"nX"}, // B has revoked nX
	}
	d.MergeRemoteState(remote.EncodeBulk(), false)

	require.True(t, c.IsRevoked("nX"), "a peer's revocation is folded into our union")
	raw, ok, _ := c.st.Get(revokedBucket, "nX")
	require.True(t, ok)
	require.Equal(t, []byte{1}, raw, "folded revocation is persisted")
}
```

- [x] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/membership/ -run 'Revoke|FoldsRemoteRevoked' -v`
Expected: FAIL — build errors (`c.st`, `c.revoked`, `Revoke`, `IsRevoked`, `RevokedList`, `loadRevoked`, `revokedBucket`, `NodeState.Revoked` all undefined).

- [x] **Step 3a: Add the gossip field**

In `internal/membership/state.go`, after the `ActualMem` field (the last bulk field):

```go
	Revoked         []string          `json:"revoked,omitempty"` // grow-only denylist of revoked node ids (ADR-0013)
```

- [x] **Step 3b: Add the store bucket**

In `internal/store/store.go`, extend `bucketNames`:

```go
	bucketNames = []string{"meta", "sandboxes", "operations", "idempotency", "blocked_egress", "audit", "revoked"}
```

(No schema bump — `CreateBucketIfNotExists` creates it on the next `Open`.)

- [x] **Step 3c: Add the Cluster fields + store import**

In `internal/membership/cluster.go`, add to the import block:

```go
	"github.com/squall-chua/sbx-swarm-node/internal/store"
```

Add to the `Cluster` struct (after `siPath`):

```go
	st         *store.Store        // durable home for the revoked union
	revoked    map[string]struct{} // grow-only union of revoked node ids (ADR-0013)
```

Also keep the struct invariant "`revoked` is never nil" for the existing bare-Cluster test helper: in `internal/membership/cluster_test.go`, add `revoked: map[string]struct{}{},` to the `newTestDelegate` `&Cluster{...}` literal (so a future merge test that sets `remote.Revoked` cannot hit a nil-map write).

- [x] **Step 3d: Thread `st` through `NewCluster` and seed the union**

Change the `NewCluster` signature to insert `st *store.Store` after `siPath`:

```go
func NewCluster(
	cfg *config.Config,
	local NodeState,
	tbl *routing.Table,
	si *SwarmIdentity,
	siPath string,
	st *store.Store,
	onNodeDead func(string),
	log *slog.Logger,
) (*Cluster, error) {
```

In the `c := &Cluster{...}` literal, add:

```go
		st:         st,
		revoked:    map[string]struct{}{},
```

Immediately after the literal (before `mlCfg := memberlist.DefaultLANConfig()`), seed from the store:

```go
	c.loadRevoked()
```

- [x] **Step 3e: Fold a peer's revocations in `MergeRemoteState`**

In `internal/membership/cluster.go`, the merge currently has:

```go
	// Update peer map.
	d.c.mu.Lock()
	d.c.peerStates[remote.NodeID] = remote
	isPending := d.c.si.Mode == ModePendingJoin
	d.c.mu.Unlock()

	// Update routing table.
	d.c.tbl.Upsert(remote.NodeID, remote.Addr, remote.Cordoned, remote.PubKey)
```

Replace that block with:

```go
	// Update peer map + fold any revocations the peer carries into our union.
	d.c.mu.Lock()
	d.c.peerStates[remote.NodeID] = remote
	isPending := d.c.si.Mode == ModePendingJoin
	grew := d.c.addRevokedLocked(remote.Revoked...)
	ml := d.c.ml
	d.c.mu.Unlock()

	if grew && ml != nil {
		_ = ml.UpdateNode(5 * time.Second) // propagate the learned revocation onward
	}

	// Update routing table.
	d.c.tbl.Upsert(remote.NodeID, remote.Addr, remote.Cordoned, remote.PubKey)
```

- [x] **Step 3f: Create the revocation methods**

Create `internal/membership/revocation.go`:

```go
package membership

import (
	"errors"
	"sort"
	"time"
)

// revokedBucket persists the grow-only denylist of revoked node ids (ADR-0013).
const revokedBucket = "revoked"

// Revoke adds nodeID to this node's denylist, persists it, and re-advertises so
// the revocation propagates over gossip. Grow-only and permanent (ADR-0013): a
// revoked node returns only by generating a new key. Rejects the empty id and
// self-revocation (which would brick this node's own node-auth to peers).
func (c *Cluster) Revoke(nodeID string) error {
	if nodeID == "" {
		return errors.New("revoke: empty node id")
	}
	c.mu.Lock()
	if nodeID == c.local.NodeID {
		c.mu.Unlock()
		return errors.New("revoke: cannot revoke self")
	}
	grew := c.addRevokedLocked(nodeID)
	ml := c.ml
	c.mu.Unlock()
	if grew && ml != nil {
		_ = ml.UpdateNode(5 * time.Second)
	}
	return nil
}

// IsRevoked reports whether nodeID is on the denylist. Wired as the nodekey
// `denied` predicate so a revoked node's node-auth is rejected swarm-wide.
func (c *Cluster) IsRevoked(nodeID string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.revoked[nodeID]
	return ok
}

// RevokedList returns the sorted denylist snapshot.
func (c *Cluster) RevokedList() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return sortedKeys(c.revoked)
}

// addRevokedLocked folds ids into the union, persisting new ones and refreshing
// the advertised NodeState. Returns whether the union grew. Caller MUST hold
// c.mu (write); if it returns true, call ml.UpdateNode after unlocking.
func (c *Cluster) addRevokedLocked(ids ...string) bool {
	grew := false
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, ok := c.revoked[id]; ok {
			continue
		}
		c.revoked[id] = struct{}{}
		if c.st != nil {
			_ = c.st.Put(revokedBucket, id, []byte{1})
		}
		grew = true
	}
	if grew {
		c.local.Revoked = sortedKeys(c.revoked)
		c.local.StateVersion++
	}
	return grew
}

// loadRevoked seeds the in-memory union from the store at construction so a
// restarted node keeps (and re-advertises) what it has revoked or learned.
func (c *Cluster) loadRevoked() {
	if c.st == nil {
		return
	}
	_ = c.st.ForEach(revokedBucket, func(k, _ []byte) error {
		c.revoked[string(k)] = struct{}{}
		return nil
	})
	if len(c.revoked) > 0 {
		c.local.Revoked = sortedKeys(c.revoked)
	}
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
```

- [x] **Step 3g: Update the `NewCluster` call site**

In `internal/node/node.go`, the cluster construction currently reads `membership.NewCluster(cfg, localNS, tbl, si, siPath,`. Insert `st` after `siPath`:

```go
		cl, clErr := membership.NewCluster(cfg, localNS, tbl, si, siPath, st,
			func(deadNodeID string) {
				mgr.MarkUnreachable(deadNodeID)
			},
			log,
		)
```

- [x] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/membership/ ./internal/store/ -run 'Revoke|FoldsRemoteRevoked' -v && go build ./...`
Expected: PASS. Then the full membership suite + race:
Run: `go test ./internal/membership/ ./internal/store/ ./internal/node/ && go test -race ./internal/membership/`
Expected: PASS (the existing `MergeRemoteState`/`UpdateLocalLoad` tests still green).

- [x] **Step 5: Format + commit**

Run: `gofmt -w internal/membership/state.go internal/membership/cluster.go internal/membership/cluster_test.go internal/membership/revocation.go internal/membership/revocation_test.go internal/store/store.go internal/node/node.go`

```bash
git add internal/membership/ internal/store/store.go internal/node/node.go
git commit -m "feat(membership): grow-only gossiped + persisted node revocation union"
```

---

### Task 4: NodeService RevokeNode/ListRevoked + node wiring

**Files:**
- Modify: `internal/apiserver/nodeservice.go`
- Modify: `internal/node/node.go` (`nodeSvc.SetRevoker(cl)` + `Denylist` option)
- Test: `internal/apiserver/nodeservice_test.go`

**Interfaces:**
- Consumes: `Cluster.Revoke`/`RevokedList` (Task 3); generated `RevokeNodeRequest`/`ListRevokedRequest`/`RevokedList` (Task 2).
- Produces: `Revoker` interface (`Revoke(string) error`, `RevokedList() []string`); `NodeService.SetRevoker`, `RevokeNode`, `ListRevoked`.

- [x] **Step 1: Write the failing test**

Create `internal/apiserver/nodeservice_test.go` (or append if it exists):

```go
package apiserver

import (
	"context"
	"errors"
	"testing"

	sbxv1 "github.com/squall-chua/sbx-swarm-node/internal/gen/sbxswarm/v1"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type fakeRevoker struct {
	revoked []string
	err     error
}

func (f *fakeRevoker) Revoke(id string) error {
	if f.err != nil {
		return f.err
	}
	f.revoked = append(f.revoked, id)
	return nil
}
func (f *fakeRevoker) RevokedList() []string { return f.revoked }

func TestNodeService_RevokeNode(t *testing.T) {
	s := NewNodeService("nA", "name", "v")

	// Standalone (no revoker) -> FailedPrecondition.
	_, err := s.RevokeNode(context.Background(), &sbxv1.RevokeNodeRequest{NodeId: "nB"})
	require.Equal(t, codes.FailedPrecondition, status.Code(err))

	fr := &fakeRevoker{}
	s.SetRevoker(fr)
	reply, err := s.RevokeNode(context.Background(), &sbxv1.RevokeNodeRequest{NodeId: "nB"})
	require.NoError(t, err)
	require.Equal(t, []string{"nB"}, reply.NodeIds)
	require.Equal(t, []string{"nB"}, fr.revoked)
}

func TestNodeService_RevokeNode_InvalidArg(t *testing.T) {
	s := NewNodeService("nA", "name", "v")
	s.SetRevoker(&fakeRevoker{err: errors.New("revoke: cannot revoke self")})
	_, err := s.RevokeNode(context.Background(), &sbxv1.RevokeNodeRequest{NodeId: "nA"})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestNodeService_ListRevoked(t *testing.T) {
	s := NewNodeService("nA", "name", "v")

	reply, err := s.ListRevoked(context.Background(), &sbxv1.ListRevokedRequest{})
	require.NoError(t, err)
	require.Empty(t, reply.NodeIds, "standalone returns an empty list, not an error")

	s.SetRevoker(&fakeRevoker{revoked: []string{"nB", "nC"}})
	reply, err = s.ListRevoked(context.Background(), &sbxv1.ListRevokedRequest{})
	require.NoError(t, err)
	require.Equal(t, []string{"nB", "nC"}, reply.NodeIds)
}
```

- [x] **Step 2: Run test to verify it fails**

Run: `go test ./internal/apiserver/ -run TestNodeService_ -v`
Expected: FAIL — `s.RevokeNode`/`s.ListRevoked`/`s.SetRevoker` undefined.

- [x] **Step 3: Implement the service methods**

In `internal/apiserver/nodeservice.go`, add `codes` and `status` to imports:

```go
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
```

Add the `revoker` field to the `NodeService` struct (after `cordoner`):

```go
	revoker  Revoker // optional; nil when not in cluster mode
```

Add (near `Cordoner`):

```go
// Revoker is implemented by membership.Cluster. Minimal interface so NodeService
// does not import membership (avoiding a cycle), mirroring Cordoner.
type Revoker interface {
	Revoke(nodeID string) error
	RevokedList() []string
}

// SetRevoker wires the cluster's revocation controller. nil-safe; standalone
// leaves it nil so revocation degrades to FailedPrecondition/empty.
func (s *NodeService) SetRevoker(r Revoker) { s.revoker = r }

// RevokeNode places a node id on the swarm-wide denylist (admin; ADR-0013).
func (s *NodeService) RevokeNode(_ context.Context, r *sbxv1.RevokeNodeRequest) (*sbxv1.RevokedList, error) {
	if s.revoker == nil {
		return nil, status.Error(codes.FailedPrecondition, "revocation requires clustering")
	}
	if err := s.revoker.Revoke(r.NodeId); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return &sbxv1.RevokedList{NodeIds: s.revoker.RevokedList()}, nil
}

// ListRevoked returns the node ids on this node's denylist.
func (s *NodeService) ListRevoked(_ context.Context, _ *sbxv1.ListRevokedRequest) (*sbxv1.RevokedList, error) {
	if s.revoker == nil {
		return &sbxv1.RevokedList{}, nil
	}
	return &sbxv1.RevokedList{NodeIds: s.revoker.RevokedList()}, nil
}
```

- [x] **Step 4: Run test to verify it passes**

Run: `go test ./internal/apiserver/ -run TestNodeService_ -v`
Expected: PASS.

- [x] **Step 5: Wire the cluster into the node**

In `internal/node/node.go`, inside the `if cfg.GossipAddr != "" && cfg.ClusterSecret != ""` block, next to `nodeSvc.SetCordoner(cl)`:

```go
		nodeSvc.SetRevoker(cl)
```

And in the `apiserver.Build(apiserver.Options{...})` call, replace the line
`// Denylist: nil for v1 (local-only hook; gossiped revocation is vNext).` with:

```go
		Denylist: func(nodeID string) bool { return clusterInstance != nil && clusterInstance.IsRevoked(nodeID) },
```

(Use `clusterInstance`, the outer `*membership.Cluster` var — not the block-local `cl`. The closure is evaluated per-request, after `clusterInstance` is assigned.)

- [x] **Step 6: Verify build + full apiserver/node suites, then commit**

Run: `gofmt -w internal/apiserver/nodeservice.go internal/apiserver/nodeservice_test.go internal/node/node.go && go build ./... && go vet ./internal/apiserver/ ./internal/node/ && go test ./internal/apiserver/ ./internal/node/`
Expected: all PASS (incl. `TestAuthz_AllMethodsClassified`).

```bash
git add internal/apiserver/nodeservice.go internal/apiserver/nodeservice_test.go internal/node/node.go
git commit -m "feat(apiserver): RevokeNode/ListRevoked service + wire IsRevoked denylist"
```

---

### Task 5: Integration test — revocation propagates over gossip

**Files:**
- Create: `internal/membership/revocation_integration_test.go`

**Interfaces:**
- Consumes existing integration helpers in `internal/membership/cluster_integration_test.go`: `startNode(t, listenAddr, gossipAddr, seeds)`, `waitForPeer(t, node, peerID, timeout)`, `tlsClient()`. Admin bearer key is `"adm"`.

- [x] **Step 1: Write the integration test**

Create `internal/membership/revocation_integration_test.go`:

```go
//go:build integration

package membership_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestRevocation_PropagatesOverGossip: node A revokes B's id; the revocation
// must reach B's own denylist via gossip (B folds A's gossiped Revoked set into
// its union). Proven by polling B's /v1/node/revoked until B's id appears.
func TestRevocation_PropagatesOverGossip(t *testing.T) {
	nodeA := startNode(t, "127.0.0.1:19847", "127.0.0.1:17990", nil)
	nodeB := startNode(t, "127.0.0.1:19848", "127.0.0.1:17991", []string{"127.0.0.1:17990"})
	waitForPeer(t, nodeA, nodeB.NodeID(), 10*time.Second)
	waitForPeer(t, nodeB, nodeA.NodeID(), 10*time.Second)

	client := tlsClient()

	// A revokes B (admin bearer).
	body := fmt.Sprintf(`{"node_id":%q}`, nodeB.NodeID())
	req, err := http.NewRequest(http.MethodPost,
		fmt.Sprintf("https://%s/v1/node/revoke", nodeA.Addr()), strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer adm")
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// Poll B's denylist until B's own id propagates in.
	revokedURL := fmt.Sprintf("https://%s/v1/node/revoked", nodeB.Addr())
	require.Eventually(t, func() bool {
		greq, _ := http.NewRequest(http.MethodGet, revokedURL, nil)
		greq.Header.Set("Authorization", "Bearer adm")
		gresp, gerr := client.Do(greq)
		if gerr != nil {
			return false
		}
		defer gresp.Body.Close()
		var out struct {
			NodeIds []string `json:"node_ids"`
		}
		if json.NewDecoder(gresp.Body).Decode(&out) != nil {
			return false
		}
		for _, id := range out.NodeIds {
			if id == nodeB.NodeID() {
				return true
			}
		}
		return false
	}, 30*time.Second, 500*time.Millisecond, "B must learn its own revocation via gossip")
}
```

- [x] **Step 2: Run the integration test**

Run: `go test -tags integration ./internal/membership/ -run TestRevocation_PropagatesOverGossip -timeout 180s -count=1 -v`
Expected: PASS (propagation typically within a few push/pull rounds, well under 30s).

- [x] **Step 3: Commit**

```bash
git add internal/membership/revocation_integration_test.go
git commit -m "test(membership): integration — revocation propagates over gossip"
```

---

## Definition of done

- [x] `go build ./... && go vet ./... && go test ./...` all green.
- [x] `go test -race ./internal/membership/` green.
- [x] `go test -tags integration ./internal/membership/ -timeout 180s -count=1` green (full integration suite, not just Task 5 — confirms no regression from the `NewCluster` signature change).
- [x] Standalone smoke: a node with no `gossip_addr`/`cluster_secret` still boots; `RevokeNode` returns `FailedPrecondition`, `ListRevoked` returns empty.
- [x] Plan checkboxes flipped; ready for the independent Opus whole-branch review (`scripts/review-package $(git merge-base main HEAD) HEAD`).

## Self-review notes (spec coverage)

- Spec §1 (ops recovery, log-only) → Task 1. ✓
- Spec §2.1 (union, Revoke/IsRevoked/RevokedList, self/empty guard, persist, merge fold) → Task 3. ✓
- Spec §2.2 (`NodeState.Revoked` bulk field) → Task 3 (3a). ✓
- Spec §2.3 (proto RevokeNode/ListRevoked + REST) → Task 2; service impl → Task 4. ✓
- Spec §2.4 (Denylist wiring + `NewCluster` `st`) → Tasks 3 (3g) + 4 (5). ✓
- Spec §2.5 (authz classification) → Task 2. ✓
- Spec §2.6 (auth-layer enforcement; per-RPC) → covered by the `Denylist` wiring (Task 4) + pre-existing `nodekey.Verify(denied)` coverage (`nodekey_test.go:79`); no new nodekey test needed. ✓
- Spec §2.7 (tests) → Tasks 3 (unit) + 5 (integration). ✓
- Spec §3 (secrets invariant) → only `node_id`s persisted/gossiped; no key material. ✓
