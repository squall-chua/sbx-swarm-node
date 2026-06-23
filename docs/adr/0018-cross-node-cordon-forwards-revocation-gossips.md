# Cross-node cordon forwards to the target; revocation gossips

Cordoning, uncordoning, or draining a **peer** node is done by forwarding the request to that node
(routed by `node_id` over the existing node-key peer path), which then updates its **own** gossiped
`NodeState.Cordoned`. Schedulers read that flag from gossip as they already do. An empty `node_id` means
"self" (today's behavior, unchanged).

Why forward rather than gossip a directive: a cordoned node is **still trusted** and remains the single
authority over its own `NodeState`, so the operator's intent is applied at the source and disseminated by
the node itself — no other node fabricates a peer's cordon state. This is the deliberate opposite of node
**revocation** (ADR-0013), which *is* a gossiped grow-only denylist precisely because a revoked node is
**untrusted** and cannot be asked to self-revoke. The trust status of the target is what decides the
mechanism.

Consequences: a peer must be reachable to be cordoned/drained — acceptable, since an unreachable node is
not accepting placements and a dead node has nothing to drain. Reusing the per-node-key forward path
keeps cross-node control authenticated exactly like sandbox forwarding.
