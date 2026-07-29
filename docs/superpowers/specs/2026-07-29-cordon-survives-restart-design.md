# Design — make a restart safe, and declare config restart-only

Date: 2026-07-29
Base: `main` @ `1654521`
Probed against: `sbx` v0.37.0 (api `0.24.0`), sbx-go-sdk v0.1.9

This is the scoping result for candidate **item 2, config reload**. The answer is
not config reload.

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

In `internal/node/node.go`, after the cluster is built (the
`cfg.GossipAddr != "" && cfg.ClusterSecret != ""` block at line 217), if the
stored cordon flag is set, call `cl.SetCordoned(true)`. Restore the drain marker
onto `nodeSvc` at the same point, and wire the persister.

**Do not restore by setting `localNS.Cordoned` before construction.** That was
the first draft of this design and it is wrong. Enforcement reads the *routing
table* — `tbl.IsCordoned(id.NodeID)` at `node.go:304`, and the scheduler's
candidate filter — and `NewCluster` (`cluster.go:42-55`) never seeds a self
entry. The only self-upsert in the file is inside `SetCordoned` itself
(`cluster.go:156`). Seeding `localNS` alone would put the restored value on the
gossip wire, so every peer would see the node as cordoned, while the node's own
table entry stayed absent and `IsCordoned` returned the zero value. The node
would place sandboxes on itself while telling the swarm it was cordoned.

Reusing `SetCordoned` avoids that. It already sets `local.Cordoned`, bumps
`StateVersion`, upserts the self entry, and re-advertises. It is nil-safe on the
memberlist handle (`cluster.go:159`), so calling it before gossip is up is safe:
the table is seeded immediately and the flag rides the meta once memberlist
starts.

It would also be possible to rely on memberlist delivering a `NotifyJoin` for the
local node, since that handler upserts from decoded meta. Rejected: that pins our
correctness to a dependency's internal behaviour, which a version bump could
change.

### Standalone nodes

A node with no `cluster_secret` builds no cluster (`node.go:217`), so `cordoner`
is nil and `SetCordoned` is never reached. **Cordon is already inert in
standalone mode**, before and after this change. The `Cordon` RPC nonetheless
returns `Cordoned: true`, which is a pre-existing inaccuracy and is not in scope
here.

Do not try to verify this work on a standalone node. It cannot show anything.

### The cordon is sticky

A restored cordon is **not** cleared by a restart. This is deliberate. A cordon
is an operator decision and only an operator should undo it. An operator who
repairs a host and restarts the node must now explicitly uncordon it.

The failure direction is the safe one: a node sitting idle when it could work,
rather than a node quietly taking jobs it should not have.

## Change 2 — document it

- `README.md`, under `## Configuration reference`: config is read once at
  startup, every change needs a restart, and a restart is safe — the daemon owns
  the sandboxes, records reconcile at boot, and the cordon survives.
- `CONTEXT.md`: define **Cordon**, including that it survives a restart. The
  **Revoke / Revoked** term already says "Distinct from Cordon, which merely
  stops new placements on a still-trusted node", pointing at a term the glossary
  never defines. This is the same dangling cross-reference the `Custom secret`
  work found, and it is fixed the same way.
- `CONTEXT.md`: define **Drain** honestly, as a Cordon that records why it was
  applied. See below.
- `internal/apiserver/nodeservice.go:141-143`: correct the `Drain` comment.

`CONTEXT.md` gets the terms only. The restart-only property is operational
guidance, not vocabulary, so it belongs in `README.md`.

### Drain does not drain

The `Drain` comment claims it "sets a draining flag so the M5 scheduler can
gracefully migrate sandboxes away". Nothing migrates. `internal/scheduler` and
`internal/apiserver/provision.go` contain no reference to drain at all, and
`Draining` is consumed in exactly one place — `node.go:247`, building the display
row for `ListNodes`.

What `Drain` actually does is call `SetCordoned(true)` and set a marker recording
that the cordon came from a drain. That is useful, and it is all it is.

Defining **Cordon** truthfully while leaving that comment in place would produce
a glossary nobody trusts, so the comment is corrected in the same change. The
marker is persisted alongside the cordon so the console keeps showing the right
reason a node is out of service.

Building actual migration, or removing the now-redundant `Drain` RPC, are both
out of scope. `Drain` is shipped API with a console control.

## ADR

`docs/adr/0023-a-cordon-survives-a-restart.md` records the sticky-cordon
decision. It meets all three bars: hard to reverse once operators depend on it,
surprising without context because the naive expectation is that a restart
clears a cordon, and the result of a real trade-off against clear-on-restart and
crash-only-restore.

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
- A test that the restored cordon reaches the **routing table**, not only the
  gossiped state. This is the bug the first draft of this design would have
  shipped, so it is the one check that must not be skipped: assert
  `tbl.IsCordoned(selfID)` is true after a restore.
- Manual, on a node **with a `cluster_secret` and at least one peer**: cordon it,
  restart it, and confirm both that `GET /v1/nodes` still reports it cordoned and
  that a provision request is not placed on it. A standalone node cannot verify
  any of this, because cordon is inert there.

## Out of scope

- Config reload in any form.
- Changing gossip merge semantics.
- Persisting anything else that lives only in memory. If another such flag is
  found, it is its own item.
