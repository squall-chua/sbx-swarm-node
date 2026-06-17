# Gossiped denylist trusts any cluster-secret holder

ADR-0004 states revocation is "a gossiped per-node key denylist," eventually
consistent. This ADR records *who is trusted to add to it*. The denylist is a
grow-only set of revoked `node_id`s, replicated to and persisted on every node,
advertised as a bulk `NodeState.Revoked` field. A node folds **any** peer's
gossiped `Revoked` set into its local union (`MergeRemoteState`) and trusts it.

That means the denylist rides ADR-0004's existing trust boundary unchanged: the
**`cluster_secret` is the auth boundary**. Anyone holding it can already join,
gossip, and provision — and can therefore also gossip a `Revoked` list naming
**healthy** nodes. One compromised secret-holder can revoke the whole swarm, and
the set is grow-only (no un-revoke), so a poisoned entry is permanent.

We accept this. The denylist does not raise the trust bar above the secret; it
adds a *capability* for secret-holders. Its intended use is a node whose **key**
is compromised but whose holder is not an active in-swarm attacker — a
decommissioned node, or a key leaked from an offline box. For an actively
malicious secret-holder, revocation is the wrong tool: the remedy for a
compromised `cluster_secret` is **rotating the secret**, which re-gates the whole
swarm.

## Scope

- **Authorization = possession of the `cluster_secret`** (via the gossip channel),
  same as join/provision. There is no separate admin signing key in the gossip
  layer; admin auth lives at the API (`api_keys`) and gates the `RevokeNode` RPC,
  but the *propagated* revocation carries no further proof.
- **Enforcement is auth-layer only.** `IsRevoked` gates `nodekey.Verify`, so a
  revoked node's per-RPC node-auth is rejected on its next call (even over an
  existing pooled connection). The node stays in gossip/routing but is auth-dead.
  Routing/memberlist eviction is deferred.
- **Grow-only and durable.** Revocations never expire and are persisted on every
  node, so they survive the departure of the node that issued them. A revoked
  node returns only by generating a new key (new `node_id`).
- **Unbounded but rare.** No size cap in v1; real revocations are a handful of
  ~20-byte ids. Pathological growth is only reachable via the poisoning vector
  above, whose remedy is secret rotation, not a cap.

## Consequences

- A future reader sees a security feature that trusts every peer's claims and
  might assume that's a bug; it is a deliberate inheritance of ADR-0004's
  secret-only trust model, not an oversight.
- Compromised-secret incidents are handled by rotation, not revocation; operators
  must not rely on the denylist to contain an attacker who still holds the secret.
- A typo'd `node_id` is permanent denylist cruft (harmless — it denies an id no
  one holds). `RevokeNode` accepts any non-empty, non-self id (revoking
  already-departed nodes is a core use case), so it cannot validate against
  current membership.

## Considered

- **Admin-signed revocations** (only entries signed by an admin key are folded
  into the union) — would raise the bar above the secret, but requires
  distributing and rotating an admin signing key across nodes, reintroducing the
  PKI hierarchy ADR-0004 deliberately avoids. Deferred to vNext if the threat
  model ever needs to survive a compromised secret-holder.
- **Per-node-authored revocations only** (a node may only revoke ids it can prove
  it learned about) — no coherent proof exists in a leaderless swarm; rejected.
- **A size cap with log-and-drop** — risks silently dropping a legitimate
  revocation; the rare-by-design size made it unnecessary. Rejected for v1.
