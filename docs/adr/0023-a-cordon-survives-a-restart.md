# A cordon survives a restart

A cordon is persisted to the node's store and restored at startup. Restarting a cordoned node does
not put it back into service; only an explicit uncordon does. The drain marker is persisted the same
way, so the console keeps showing why a node is out of service.

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

The restore reuses `Cluster.SetCordoned` rather than seeding the boot `NodeState`. Enforcement reads
the routing table, and `NewCluster` never seeds a self entry — the only self-upsert is inside
`SetCordoned`. Setting the boot state alone would tell every peer the node was cordoned while leaving
its own table entry absent, so it would place sandboxes on itself while advertising that it would
not. Relying on memberlist to deliver a self `NotifyJoin` would also work today, but pins correctness
to a dependency's internals.

Scope: cordon is inert on a standalone node, which builds no cluster at all, and this ADR does not
change that. Teaching `routing.Table.Upsert` to arbitrate on `StateVersion` was considered and
rejected as unnecessary — once a node advertises the correct value on rejoin, last-writer-wins is
right.
