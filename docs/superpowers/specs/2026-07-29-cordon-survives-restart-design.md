# Design — make the out-of-service switch tell the truth

(Originally: make a restart safe, and declare config restart-only. Widened by the
amendment below to cover the standalone cordon and what `Drain` means.)

Date: 2026-07-29
Base: `main` @ `1654521`
Amended: 2026-07-29, same day, after a grilling session. See "Amendment" below.
Probed against: `sbx` v0.37.0 (api `0.24.0`), sbx-go-sdk v0.1.9

This is the scoping result for candidate **item 2, config reload**. The answer is
not config reload.

## Amendment

The design now covers two more items that were scoped after it was written. Both
touch the same three files, so they are built on the same branch.

- **Cordon lied on a standalone node.** The RPC returned `Cordoned: true` while
  nothing was cordoned. The fix makes the cordon flag local and authoritative, so
  it works with or without a cluster. This reverses the original "Reading" and
  "Standalone nodes" sections; both are rewritten below.
- **`Drain` did not drain.** It now cordons the node and then publishes and stops
  every sandbox running on it. See "Change 3".

Re-probe note: local `main` sits 6 commits behind `origin/main`, because the
small-gaps work merged as `cb02a51`. The branch needs a rebase before it is
built.

## The problem

### The premise holds

Config is read once and consumed only inside `node.New()`, at about thirty
sites, all of them snapshotting into constructors and closures.
`workspaceResolver` (`internal/node/node.go:661-674`) builds its name-to-path map
at boot and never re-reads it. So adding a workspace, a kit, a template
constraint or a git provider does require a restart.

### A restart is normally cheap

Sandboxes survive: the sbx daemon owns them, and `Manager.Reconcile` diffs
persisted records against backend truth at boot
(`internal/node/node.go:326-329`). What is lost is in-flight terminal, SSE and
file-transfer connections, and the tracking of in-flight operations, which the
ops crash-recovery sweep unsticks. For a planned operator action that is an
acceptable price.

### Except a restart silently un-cordons the node, swarm-wide

This is a real bug and it is the reason item 2 looked like it needed reload.

- `Cordoned` lives only in `Cluster.local` and on the gossip wire
  (`internal/membership/state.go:11`, `cluster.go:148-153`). None of the seven
  store buckets — `meta`, `sandboxes`, `operations`, `idempotency`,
  `blocked_egress`, `audit`, `revoked` — holds it.
- The boot `localNS` (`internal/node/node.go:192-212`) never sets `Cordoned`, so
  it defaults to `false`.
- `routing.Table.Upsert` (`internal/routing/table.go:29-39`) is last-writer-wins:
  it assigns `e.cordoned = cordoned` with no version check, even though
  `StateVersion` is carried on the wire and bumped by `SetCordoned`.

So a node that was cordoned or drained, then restarted to pick up a config
change, rejoins advertising `Cordoned: false` and **overwrites every peer's
view**. It starts taking work again with no signal.

The `draining` marker is lost the same way — it is an `atomic.Bool` on
`NodeService` (`internal/apiserver/nodeservice.go:50`) — but that one is display
only. `Drain` sets it *and* calls `SetCordoned(true)`, so the cordon is the part
with teeth.

This happens on any restart, planned or not. A crash un-cordons the node exactly
the same way, which config reload would never have fixed.

## The decision

Make a restart safe, then say plainly that restart-only is the permanent
position. That is smaller than config reload and it fixes the crash case too.

## Change 1 — persist the cordon and drain flags

### Storage

Add `"node"` to `bucketNames` in `internal/store/store.go:19`.

No schema-version bump is needed. `migrate()` runs `CreateBucketIfNotExists`
over every name in `bucketNames` on each open, so an existing database gains the
bucket on its next start.

Store both flags as one small JSON value under a single key. One value keeps the
two flags consistent: they are always written together and always read together.

### Writing

`Cordon`, `Uncordon` and `Drain` in `internal/apiserver/nodeservice.go` are the
only three mutation points. `Uncordon` already clears `draining`
(`nodeservice.go:115`), so there is no fourth case to cover.

Add an optional persist hook, following the file's existing wiring pattern
(`SetCordoner`, `SetNodeLister`, `SetTemplateLister`):

```go
func (s *NodeService) SetFlagPersister(fn func(cordoned, draining bool))
```

Call it at the end of each of the three RPCs. Optional and nil-safe, so existing
`NodeService` tests keep passing untouched.

The hook takes both flags rather than wrapping the `Cordoner` interface, because
only `NodeService` knows both. A `Cordoner` wrapper would see the cordon and not
the drain marker.

### Reading

In `internal/node/node.go`, restore the stored flags onto `nodeSvc` before the
cluster block at line 217, and wire the persister. Inside the cluster block, if
the restored cordon is set, also call `cl.SetCordoned(true)` so peers learn it.

The local flag is restored first and unconditionally. The cluster call is only a
mirror, and it is skipped on a standalone node, which has no cluster.

An earlier draft restored **only** through `cl.SetCordoned(true)`, on the grounds
that enforcement read the routing table. Change 2 removes cordon from the routing
table, so that reasoning no longer holds. Setting `localNS.Cordoned` before
construction is still wrong, for the reason it always was: it would put the value
on the gossip wire without the local flag being set, so peers and the node itself
would disagree.

### Standalone nodes

A node with no `cluster_secret` builds no cluster (`node.go:217`), so `cordoner`
is nil. After Change 2 that no longer matters: the flag lives on `NodeService`
and the scheduler reads it directly, so **a standalone node can be cordoned, and
the cordon is enforced.** A provision on a cordoned standalone node fails with
`scheduler.ErrNoEligibleNode`.

That message is not improved. On a one-node swarm "no eligible node" reads
oddly, but the console shows the node as cordoned, and a better message would
mean teaching the scheduler to report which constraint rejected each candidate.
That machinery is not worth one string.

### The cordon is sticky

A restored cordon is **not** cleared by a restart. This is deliberate. A cordon
is an operator decision and only an operator should undo it. An operator who
repairs a host and restarts the node must now explicitly uncordon it.

The failure direction is the safe one: a node sitting idle when it could work,
rather than a node quietly taking jobs it should not have.

## Change 2 — the cordon flag is local, and the routing table drops it

### The lie

`Cordon` returns `Cordoned: true` whether or not anything was cordoned
(`nodeservice.go:97-107`). On a standalone node nothing is: `cordoner` is nil, so
`SetCordoned` is never called, and the console shows a cordon that does not
exist. `Revoke` already handles the same situation honestly — it returns
`FailedPrecondition, "revocation requires clustering"` (`nodeservice.go:69`) —
so `Cordon` is the odd one out.

Standalone is real use here, not only a test shape, so the answer is to make
cordon work rather than to make the RPC refuse.

### The flag

Hold the cordon in an `atomic.Bool` on `NodeService`, beside `draining`.
`Cordon`, `Uncordon` and `Drain` set it, then call `cordoner` if there is one.
Each reply reports the flag, so it can no longer overstate what happened.

`NodeService` is the home because `draining` already lives there and is already
read from outside through `nodeSvc.Draining()` (`node.go:245`). A separate holder
type in `internal/node` would be tidier ownership, and it buys nothing we are
paying for anywhere else.

### The three self reads

All three switch from the routing table and the cluster to the flag:

| Site | Today | After |
|---|---|---|
| `node.go:738` scheduler self-candidate | `tbl.IsCordoned(self)` | `nodeSvc.Cordoned()` |
| `node.go:304` `InternalService` admission | `tbl.IsCordoned(id.NodeID)` | `nodeSvc.Cordoned()` |
| `node.go:246` self `NodeRow` | `clusterInstance != nil && ...LocalNodeState().Cordoned` | `nodeSvc.Cordoned()` |

### The deletion

Those were the only two non-test callers of `routing.Table.IsCordoned`, and both
asked about **self**. A peer's cordon reaches the scheduler through gossip —
`ns.Cordoned` in `buildCandidates` and in `rowFromState` — never through the
table. So once self moves, nothing reads the table's copy at all.

Delete it: the `cordoned` field on the table entry, the `cordoned` parameter of
`Upsert` (four call sites, all in `internal/membership/cluster.go`), and
`IsCordoned` with its tests.

`Cluster.SetCordoned` then shrinks to setting `local.Cordoned`, bumping
`StateVersion` and calling `UpdateNode`. Its self-upsert (`cluster.go:156`) goes
with the parameter: it existed only to publish the cordon, and nothing reads
self's address or public key out of the table — `Forwarder` looks up peers only
(`forward.go:58-61`), and the pin resolvers are for incoming peer certificates.

The cluster keeps its real job, which is telling peers and holding peer state. It
stops being the source of truth about this node.

## Change 3 — `Drain` publishes and stops everything here

### What it does

`Drain` sets both flags, returns immediately, and a goroutine sweeps every record
with `Status == "running"`: publish first, then stop. This is the `ReapIdle` loop
(`sandboxservice.go:388-405`), over a different list.

The sweep lives on `SandboxService`, which already owns `maybeAutoPublish` and
the manager. `NodeService` gets a `drainer` hook wired in `node.go`, the same
shape as every other optional dependency on that struct.

### The decisions inside it

- **Everything running, including `idle-stop: off`.** That label protects a
  sandbox from a background timer, not from an operator pressing Drain. Skipping
  those would mean a node with one long-lived sandbox could never be emptied —
  the same class of quiet under-delivery this branch is fixing in `Cordon`.
- **Fire and forget.** `Drain` returns `NodeInfo`; that is shipped API with a
  console control, and a sweep with a git publish per sandbox runs far past any
  client timeout. `Manager.Stop` publishes an event per sandbox, so the console
  watches the node empty out. Failures are logged, as the reaper's are. The
  alternative — returning an Operation id — changes a shipped reply type and
  needs console work to follow it.
- **The audit records the caller, not `"system"`.** `maybeAutoPublish` reads the
  actor from the context and falls back to `"system"`
  (`sandboxservice.go:374-377`). A goroutine cannot hold the RPC context, so
  capture the principal from the `Drain` context and carry it into the sweep.
  Note what "actor" means here: `principal.userRole` (`authz.go:12-14`), so the
  audit gains `"admin"` rather than a person's name. That is the same actor
  string the synchronous stop path already writes, and this keeps the two paths
  consistent. Per-user attribution would need identities the API keys do not
  carry.
- **Uncordon cancels.** The sweep re-checks the drain flag before each sandbox
  and stops early once it is cleared. Two lines, and it is the only way to halt a
  long sweep short of killing the process.
- **A second `Drain` during a sweep is allowed.** Both loops call `Stop` on
  records that are already stopped, which is harmless. No lock.
- **One-shot, not an invariant.** A restored `draining` flag after a restart
  re-blocks placement and shows the marker; it sweeps nothing. Stopped containers
  stay stopped across a node restart, so a node that finished draining comes back
  empty on its own. Making drain continuous — the existing reaper ticker stopping
  anything running while the flag is set — was considered and rejected: it lets a
  node quietly kill a sandbox somebody started on purpose.

### Why nothing migrates

A sandbox id is `<node_id>.<ulid>` (`record.go:7`), self-routing by ADR-0002. A
sandbox that moves to another node gets a **new id**, so every client handle to it
breaks. `Record.Spec` does hold the full `CreateSpec`, so replaying a sandbox on a
peer is mechanically possible — but anything on the container's disk outside the
git workspace is lost, and the identity problem is not a drain problem. Migration
needs a stable sandbox handle that the domain does not have. That is its own
design, if it is ever wanted.

So drain empties the node and saves the git work. It does not move anything, and
the glossary says so.

## Change 4 — document it

- `README.md`, under `## Configuration reference`: config is read once at
  startup, every change needs a restart, and a restart is safe — the daemon owns
  the sandboxes, records reconcile at boot, and the cordon survives.
- `CONTEXT.md`: define **Cordon**, including that it survives a restart. The
  **Revoke / Revoked** term already says "Distinct from Cordon, which merely
  stops new placements on a still-trusted node", pointing at a term the glossary
  never defines. This is the same dangling cross-reference the `Custom secret`
  work found, and it is fixed the same way.
- `CONTEXT.md`: define **Drain** as a Cordon followed by publishing and stopping
  every sandbox on the node, and say plainly that the swarm does not move
  sandboxes between nodes.
- `internal/apiserver/nodeservice.go:141-143`: correct the `Drain` comment, which
  claims the M5 scheduler migrates sandboxes away. Nothing migrates, before or
  after this change.

`CONTEXT.md` gets the terms only. The restart-only property is operational
guidance, not vocabulary, so it belongs in `README.md`.

## ADR

`docs/adr/0023-a-cordon-survives-a-restart.md` records the sticky-cordon
decision. It meets all three bars: hard to reverse once operators depend on it,
surprising without context because the naive expectation is that a restart
clears a cordon, and the result of a real trade-off against clear-on-restart and
crash-only-restore.

**0023 is edited in place, not superseded.** Two of its paragraphs are now wrong:
the restore argument rests on "enforcement reads the routing table", and the
scope line says cordon is inert on a standalone node and that the ADR does not
change that. The commit holding 0023 is still unpushed on local `main`, so
nothing outside this machine has read it. Rewriting is honest; a superseding
document for an unreleased one is ceremony.

No new ADR for Change 3. It was considered — drain meaning "stop", not "move", is
surprising to anyone who knows Kubernetes — and rejected to keep the decision in
one place. The reasoning lives in "Why nothing migrates" above, and the glossary
carries the result.

## Rejected alternatives

### Config reload

The original framing of item 2. Milestone-sized: each of the ~30 consumption
points would have to tolerate its value changing underneath, and each field has
its own consumers — a workspace change alone needs the resolver rebuilt, the git
workspace map rebuilt, and the gossiped `NodeState` updated, which is a bulk
field and so needs a push/pull to propagate promptly.

It is also the wrong shape: it exists to avoid restarting, when the cheaper move
is to make restarting safe. And it would not fix a crash un-cordoning the node.

### Version arbitration in `routing.Table.Upsert`

Teaching `Upsert` to compare `StateVersion` and reject an older state is the
deeper fix for the overwrite. Rejected: once a node advertises the correct value
on rejoin, last-writer-wins is the right rule. Adding version arbitration to the
gossip merge path is a much larger change to reason about, for no additional
benefit here.

### Restoring only after a crash

Distinguishing a clean shutdown from a crash and restoring only in the crash
case is more faithful to intent, but it needs a shutdown marker and gets the
answer wrong whenever the node is killed hard. Sticky is simpler and errs safe.

### A boot log line naming the restored state

Considered and not taken. It would remove the one downside of stickiness, which
is an operator wondering why a healthy node takes no work. The console already
shows cordon state per node.

## How to verify

- A unit test on `NodeService`: `Cordon` then `Drain` then `Uncordon` each invoke
  the persister with the expected pair, and a nil persister does not panic.
- A unit test on the node-level save and load round trip, including the absent
  case on a fresh database, which must read as `false, false`.
- `go build ./... && go vet ./...`, plus `go vet -tags integration ./internal/...`
- `go test ./...` green, `TestNode_Gerrit_Publish` aside.
- A test that a restored cordon reaches **enforcement**, not only the gossiped
  state: the scheduler must refuse to place on the node after a restore. The
  first draft of this design would have shipped that bug, so it is the one check
  that must not be skipped. It is now a test on `nodeSvc.Cordoned()` and the
  self candidate, not on `tbl.IsCordoned`, which no longer exists.
- A test that a cordon on a **standalone** node — no `cluster_secret` — is
  enforced, and that the RPC reply matches what happened.
- A test that the drain sweep stops a running sandbox labelled `idle-stop: off`,
  and that clearing the flag mid-sweep stops it early.
- Manual, on a node **with a `cluster_secret` and at least one peer**: cordon it,
  restart it, and confirm both that `GET /v1/nodes` still reports it cordoned and
  that a provision request is not placed on it. Unlike the earlier draft of this
  design, the single-node parts can now be checked standalone.
- Manual drain: with two running sandboxes, one git-backed, press Drain and
  confirm the node empties, the git work is published, and the audit names the
  operator rather than `system`.

## Out of scope

- Config reload in any form.
- Changing gossip merge semantics.
- Migrating sandboxes between nodes, and the stable sandbox handle it would need.
- Persisting anything else that lives only in memory. If another such flag is
  found, it is its own item.
