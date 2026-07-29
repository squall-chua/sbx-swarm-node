# A cordon survives a restart

A cordon is persisted to the node's store and restored at startup. Restarting a cordoned node does
not put it back into service; only an explicit uncordon does. The drain marker is persisted the same
way, so the console keeps showing why a node is out of service. A restored drain marker re-blocks
placement and explains itself; it does not re-run the drain sweep, which is a one-shot operator
action, not a standing rule.

Why: the flag lived only in memory. `Cordoned` was held in `Cluster.local` and on the gossip wire,
and the boot `NodeState` never set it, so it defaulted to false. `routing.Table.Upsert` is
last-writer-wins with no version check, even though `StateVersion` is carried on the wire. A node
that was cordoned, then restarted — to pick up a config change, or because it crashed — rejoined
advertising `Cordoned: false` and **overwrote every peer's view**. It silently began taking work
again. Nothing warned anyone.

This is also the reason config reload was not built. Config is read once in `node.New`, so every
change needs a restart, and the instinct was to remove the restart. The restart was not the problem:
sandboxes are owned by the daemon and records reconcile at boot, so a restart is otherwise cheap. The
problem was that a restart quietly discarded an operator decision — which reload would not have
fixed, because a crash discards it identically.

Trade-off: a cordon is now sticky, so an operator who repairs a host and restarts the node must
explicitly uncordon it. That direction is deliberate. The failure mode becomes a node sitting idle
when it could work, which is visible in the console and harmless, rather than a node the operator
took out of service quietly accepting jobs again. Restoring only after an unclean shutdown was
considered and rejected: it needs a shutdown marker and gets the answer wrong whenever the node is
killed hard.

The cordon itself becomes a local flag on `NodeService`, beside the drain marker, and that flag is
the single source of truth about this node. Everything that asks whether *this* node is cordoned —
the scheduler's self candidate, internal provision admission, and the console's own row — reads it.
The cluster keeps telling peers and holding peer state; it stops owning the answer for self. The
restore therefore sets the local flag first and unconditionally, then mirrors it into the cluster
when there is one. Seeding the boot `NodeState` instead is still wrong: it would put the value on the
gossip wire while the node's own flag stayed false, so the node and its peers would disagree.

Because self now reads the flag, `routing.Table` no longer carries a cordon at all. Its two callers
both asked about self, and a peer's cordon has always reached the scheduler through gossip rather
than the table, so the field, the `Upsert` parameter and `IsCordoned` are removed.

Scope: cordon works on a standalone node, which builds no cluster at all. It used to be inert there
while the RPC still answered `Cordoned: true` — a node that reported itself out of service and kept
taking work. The local flag closes that. Teaching `routing.Table.Upsert` to arbitrate on
`StateVersion` was considered and rejected as unnecessary — once a node advertises the correct value
on rejoin, last-writer-wins is right.

An older binary that does not know about the `node` bucket opens the database without error and simply
leaves it untouched. This means an operator who downgrades the binary, uncordons the node, and then
upgrades again will find the node cordoned once more — the downgrade uncordoned it on the operator's
request, but the old binary never wrote the `Cordoned: false` value to the new bucket, so the previous
cordoned state remains on disk. The cordon is restored when the newer binary starts and reads the
persisted flag. This asymmetry is the safer failure mode: a rolled-back and re-upgraded node ends up
idle rather than quietly returning to service, and it is inherent to the schema change that carries no
version bump. An operator performing a rollback should account for this during recovery.
