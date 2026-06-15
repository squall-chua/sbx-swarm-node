# Gossip dissemination: tiny meta + TCP push/pull + delta broadcasts

Per-node state is split across `memberlist`'s channels by size and volatility, rather than stuffed into
UDP gossip metadata (which is ~512-byte capped):

- **`NodeMeta` (UDP gossip)** — only tiny routing essentials: `node_id` (= pubkey hash), REST address,
  `cordoned`, a monotonic `state_version`.
- **TCP push/pull state sync** (`LocalState` / `MergeRemoteState`) — bulky/dynamic state: capacity,
  `allocated`, `actual_util`, workspace names, template names, full pubkey, owned sandbox IDs,
  `blocked_egress_distinct_count`.
- **Delta broadcasts** (delegate broadcast queue) — per-change events (sandbox created/removed) for
  faster-than-sync propagation, reconciled via `state_version`.

Why: the owned-sandbox-ID list and pubkeys exceed `NodeMeta` limits — naive gossip would silently
truncate or fail. Push/pull over TCP is built for larger state.

Consequence: a peer's full view converges on the push/pull interval (seconds), not instantly. Accepted
because placement is target-authoritative (Approach A) — stale views self-heal at admission, so the
scheduler tolerates lag.
