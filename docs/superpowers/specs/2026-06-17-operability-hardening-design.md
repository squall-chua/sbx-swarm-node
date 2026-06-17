# Operability hardening — ops crash-recovery + gossiped denylist revocation

**Date:** 2026-06-17 · **Status:** approved + grilled, pre-plan · **Builds on:**
**ADR-0004** (node trust: shared secret + per-node key; "Revocation is a gossiped
per-node key denylist", eventually-consistent), **ADR-0013** (the denylist trusts
any cluster-secret holder; remedy for a compromised secret is rotation), ADR-0005
(gossip tiers), ADR-0011 (node-authorized internal Provision). Glossary:
CONTEXT.md gained **Revoke / Revoked (node)** (distinct from Cordon).

Two independent, additive operability gaps left after M5. Each preserves the
node's invariants: node-gated `Provision`, additive wire evolution, no secret
leakage (spec §11). Disk *enforcement* (the third operability candidate) is
**deferred** — blocked on an sbx-go-sdk gap (v0.1.2 `Create` has no disk option).

## 0. Reconciliation (verified against code, not assumptions)

- `ops.Manager` persists operations to bbolt bucket `operations` (`internal/ops/ops.go`).
  `Start`→state `pending`; `Run` spawns a goroutine → `running` → `done`/`error`.
  A crash leaves a `pending`/`running` record with no goroutine to finish it.
- `store.Store` exposes `ForEach(bucket, fn)`, `Put`, `Get`, `Delete`
  (`internal/store/kv.go`) — enough for the boot sweep and the revoked-set persistence.
- `node.New` already runs a best-effort `mgr.Reconcile(ctx)` at boot (node.go:267)
  to resync sandbox records against backend truth. The op sweep slots in next to it.
- `nodekey.Verify(token, target, pubkeyFor, now, skew, denied func(id) bool)`
  already accepts a `denied` predicate (`internal/nodekey/nodekey.go`). It is the
  enforcement hook; `node.go` passes `Denylist: nil` today (node.go:254).
- `membership.NodeState` bulk fields (TCP push/pull, ADR-0005) gossip like
  `OwnedSandboxIDs` (`internal/membership/state.go`). `NodeMeta` (UDP) carries only
  routing fields, so a new bulk field adds **no UDP pressure**.
- `Cluster.MergeRemoteState` (cluster.go:238) is the single merge point for a peer's
  bulk state. `SetCordoned`/`UpdateLocalSandboxIDs`/`UpdateLocalLoad` are the existing
  "mutate local NodeState → bump StateVersion → `ml.UpdateNode`" re-advertise pattern.
- `NodeService` uses a minimal `Cordoner` interface to drive cluster control without
  importing `membership` (avoids a cycle). Revocation reuses this pattern (`Revoker`).
- `authz.go` classifies every gRPC method into `mutatingMethods` (admin user),
  `readMethods` (any authed principal), or `internalMethods` (node-key). The
  `TestAuthz_AllMethodsClassified` drift-guard fails on any unclassified method.

---

## 1. Ops crash-recovery

**Goal:** a node restart must not strand an in-flight operation in a non-terminal
state forever (clients polling `GetOperation` would hang; a same-idempotency-key
retry returns the stuck op via `existed=true` and can never progress).

**Semantics (chosen): mark-as-error.** Reconcile/reattach was rejected as YAGNI —
the op record carries no link to the in-flight provision target/spec, and orphan
*sandboxes* are already reconciled by `mgr.Reconcile`. The op ledger just needs to
reach a terminal state.

- **`ops`** — new method:
  ```go
  // RecoverInterrupted marks every non-terminal (pending/running) operation as
  // error ("interrupted by restart"). Call once at boot, before serving. Returns
  // the number of operations swept.
  func (m *Manager) RecoverInterrupted() (int, error)
  ```
  Implementation: `m.mu.Lock()`; `store.ForEach(opBucket, …)` collecting the ids of
  non-terminal ops (don't mutate the bucket inside its own iterator); then for each,
  set `State="error"`, `Error="interrupted: node restarted during operation"`, and
  `put(op)` (which stamps `UpdatedAt`). Return the count.

  Terminal states are exactly `done` and `error`; treat anything else
  (`pending`/`running`, or an unknown legacy value) as interrupted.

  **Log-only, no event/metric (grill finding).** `RecoverInterrupted` does **not**
  `emit` or `IncOp`. The event bus replays its ring to every new subscriber
  (`events/bus.go`), so emitting at boot would replay `operation.error` events for
  ops that died in a *previous* process — a phantom live failure to the first SSE
  client. And `IncOp(type,"error")` would conflate restart-interruptions with
  genuine op errors in `op_total`. The boot count is surfaced via the `node.New`
  log line below; a dedicated `interrupted_ops_recovered_total` metric is
  carry-forward if alerting is ever wanted.

- **`node.New`** — after the existing boot reconcile (node.go:267):
  ```go
  if n, err := opsM.RecoverInterrupted(); err != nil {
      log.Warn("op recovery failed", "err", err)
  } else if n > 0 {
      log.Info("recovered interrupted operations", "count", n)
  }
  ```

**Idempotency-key contract (unchanged, documented):** the `idempotency` bucket
still maps `idempKey → opID`. A retry with the same key returns the now-errored op
(`existed=true`); the caller must use a fresh key to retry. This is the intended
idempotency semantics (same key = same result), not a regression.

**Test (unit, `ops_test.go`):** seed three ops directly in the store —
`pending`, `running`, `done`; call `RecoverInterrupted`; assert it returns 2, the
two non-terminal ops are now `error` with the interrupted message, and the `done`
op is untouched. Assert the idempotency mapping still resolves to the (now-errored)
op id.

---

## 2. Gossiped denylist revocation

**Goal:** realise ADR-0004's "gossiped per-node key denylist" — an admin revokes a
compromised `node_id`; the revocation propagates over gossip; every node then
rejects that node's `x-sbx-node-auth` tokens (eventually consistent).

**Model: grow-only, durable, replicated union.** Revocation is permanent — a
revoked node returns by generating a new keypair (new `node_id`). No tombstones, no
un-revoke. The set is replicated to **every** node and persisted on each, so a
revocation survives the departure of the node that issued it (a security denylist
must not evaporate when the revoker leaves).

### 2.1 The replicated set (owned by `membership.Cluster`)

- **Persistence:** new bbolt bucket `revoked` (key = `node_id`, value = `[]byte{1}`).
- **`Cluster`** gains a `*store.Store` (threaded through `NewCluster`) and an
  in-memory `revoked map[string]struct{}` seeded from the bucket at construction;
  `local.Revoked` is set to the sorted union at seed time so a restarted node
  re-advertises what it knows.
- **Methods:**
  ```go
  func (c *Cluster) Revoke(nodeID string) error   // admin action
  func (c *Cluster) IsRevoked(nodeID string) bool  // the denied predicate
  func (c *Cluster) RevokedList() []string         // sorted snapshot
  ```
  - `Revoke`: reject `nodeID == ""` (`errors.New`, mapped to `InvalidArgument`) and
    `nodeID == self` (reject self-revoke — it would brick the node's own node-auth
    to peers). Accepts **any other** non-empty id — does *not* require it to be a
    current member, since revoking a departed/offline/partitioned node is a core
    use case (a typo'd id becomes permanent but harmless cruft, denying an id no
    one holds). Idempotent (already-present → return nil, no churn). On a new id:
    add to map → `store.Put(revokedBucket, id, []byte{1})` → `local.Revoked =
    sortedUnion` → `StateVersion++` → `ml.UpdateNode` (nil-safe).
  - `IsRevoked`: `RLock` map membership, O(1).
  - `RevokedList`: `RLock` sorted snapshot.

- **`MergeRemoteState`** (cluster.go, after the existing `peerStates` update): fold
  each id in `remote.Revoked` into the local union (add + persist if new). If the
  union grew, set `local.Revoked = sortedUnion`, `StateVersion++`, and
  `ml.UpdateNode` so the revocation keeps propagating onward. Mind lock ordering —
  the merge already holds `c.mu.Lock` for the peerStates write; do the union fold in
  the same critical section, persist, and call `UpdateNode` after releasing the lock.

### 2.2 Gossip field

- **`membership.NodeState`** (bulk): `Revoked []string \`json:"revoked,omitempty"\``.
  Rides `EncodeBulk`/`DecodeBulk` (TCP push/pull) automatically; **not** in
  `metaWire`/`EncodeMeta` (UDP) — no meta-tier size impact. Additive (ADR-0009:
  unknown fields are ignored by older peers).

### 2.3 API

**`proto/sbxswarm/v1/node.proto`** (then `buf generate`, commit regenerated gen):
```proto
rpc RevokeNode(RevokeNodeRequest) returns (RevokedList) {
  option (google.api.http) = {post: "/v1/node/revoke" body: "*"};
}
rpc ListRevoked(ListRevokedRequest) returns (RevokedList) {
  option (google.api.http) = {get: "/v1/node/revoked"};
}

message RevokeNodeRequest { string node_id = 1; }
message ListRevokedRequest {}
message RevokedList { repeated string node_ids = 1; }
```
`RevokeNode` returns the updated `RevokedList` (not self's `NodeInfo`) — it acts on
a *different* node, so the caller wants confirmation the set took effect.

**`NodeService`** (`nodeservice.go`):
```go
type Revoker interface {
    Revoke(nodeID string) error
    RevokedList() []string
}
func (s *NodeService) SetRevoker(r Revoker) { s.revoker = r }   // nil-safe, like SetCordoner
```
- `RevokeNode`: `revoker == nil` → `codes.FailedPrecondition` ("revocation requires
  clustering"); else `revoker.Revoke(req.NodeId)` (map its `""`/self error to
  `codes.InvalidArgument`); return `&RevokedList{NodeIds: revoker.RevokedList()}`.
- `ListRevoked`: `revoker == nil` → empty `RevokedList`; else the snapshot.

### 2.4 Wiring (`node.go`)

- In the cluster-built block (next to `SetCordoner`/`SetOwnedIDsNotifier`):
  `nodeSvc.SetRevoker(cl)`.
- `NewCluster(...)` gains the `*store.Store` argument (`st`). Single production
  caller (`node.go:195`); no `membership` test constructs it directly, so the
  signature change ripple is one line.
- The `apiserver.Build` options:
  ```go
  Denylist: func(id string) bool { return clusterInstance != nil && clusterInstance.IsRevoked(id) },
  ```
  This replaces the `nil` at node.go:254. `nodekey.Verify` now rejects a revoked
  caller before checking its pin/signature.

### 2.5 Authz

`authz.go`: add `/sbxswarm.v1.NodeService/RevokeNode` → `mutatingMethods` (admin
user), `/sbxswarm.v1.NodeService/ListRevoked` → `readMethods`. The
`TestAuthz_AllMethodsClassified` drift-guard enforces this.

### 2.6 Enforcement scope (chosen: auth-layer only)

`IsRevoked` gates `nodekey.Verify`, so a revoked node cannot (a) make node→node
RPCs (e.g. `Provision`) or (b) authenticate as a node-key read peer. It is **not**
evicted from routing/memberlist (it stays in gossip but is auth-dead). Routing
eviction is a bigger hammer (memberlist re-adds on gossip) and beyond ADR-0004's
stated mechanism — **carry-forward**.

Node-auth is `PerRPCCredentials` (`internal/peer/nodekey_creds.go`), sent and
verified on **every** RPC, so revocation takes effect on the revoked node's **next
call even over an existing pooled connection** — no connection teardown needed.

**Trust boundary (ADR-0013).** The union folds any peer's gossiped `Revoked` set
and trusts it, so a `cluster_secret`-holder can revoke healthy nodes; the denylist
inherits ADR-0004's secret-only trust boundary and does not raise it. The remedy
for a compromised secret is rotation, not revocation. See ADR-0013.

### 2.7 Tests

- **Unit (`membership`):** `Revoke`/`IsRevoked`/`RevokedList` happy path;
  persistence reload (seed bucket → `NewCluster` → `IsRevoked` true); self-revoke
  and empty-id rejected; idempotent re-revoke (no second `StateVersion` bump);
  `MergeRemoteState` folds a remote `Revoked` id into the local union + persists it.
- **Unit (`nodekey` or `authn`):** the `denied` predicate rejects a revoked caller
  (token from a revoked id fails `Verify` with the denied error) — confirms the wire
  hook works independent of gossip timing.
- **Unit (`apiserver`):** `RevokeNode`/`ListRevoked` with a fake `Revoker`; nil
  revoker → `FailedPrecondition`/empty; `TestAuthz_AllMethodsClassified` green with
  the two new methods.
- **Integration (2-node, fresh ports — gossip `17946`+, REST `19443`+):** node A
  `RevokeNode(B)`; poll **B**'s `ListRevoked` until B's own id appears — deterministic
  proof the revocation propagated over gossip and persisted into B's union. (The
  end-to-end auth rejection of B→A is unit-tested at the nodekey layer; the harness
  can't deterministically induce a B→A `Provision` to observe the rejection in situ —
  same limitation documented for m5-latents.)

---

## 3. Secrets invariant (spec §11)

`node_id`s are non-sensitive (already gossiped as `OwnedSandboxIDs` and pins). The
denylist carries no key material, env values, or tokens. The new `Revoked` gossip
field and the `revoked` bucket are clean. Ops recovery only rewrites `state`/`error`
on existing records — no new persisted data.

## 4. Out of scope / carry-forward

- **Routing/memberlist eviction** of a revoked node (auth-layer denylist is the
  ADR-0004 mechanism; the node stays in gossip but is auth-dead).
- **Un-revoke / reinstatement** of the same `node_id` (grow-only by design; return
  via a new keypair).
- **Admin-signed revocations** (the ADR-0013 upgrade path — only worth it if the
  threat model ever needs to survive a compromised `cluster_secret`-holder).
- **Denylist size cap** (unbounded but rare by design; revisit only if poisoning
  becomes a concern, which secret rotation already addresses).
- **`interrupted_ops_recovered_total` metric** (recovery is log-only in v1).
- **Ops reconcile-and-reattach** (mark-as-error is the chosen v1 semantics).
- **Per-sandbox disk enforcement** (unblocked only by an sbx-go-sdk change).
