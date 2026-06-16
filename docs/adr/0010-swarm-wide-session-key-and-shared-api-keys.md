# User credentials are swarm-wide: shared api_keys + a cluster-secret-derived session key

User authentication is a **swarm-wide** concern, distinct from node membership. Operators configure
the **same `api_keys` on every node** (the leaderless swarm has no shared user store), and the
**session-cookie signing key is derived from the `cluster_secret`** (`HKDF-SHA256(cluster_secret,
"sbx-session-v1")`) rather than from the per-node key. A standalone node (no `cluster_secret`) falls
back to its per-node key seed. Both choices make a credential minted by any node verifiable on every
node, so cross-node REST forwarding (OwnerProxy re-auth at the owner) works for both bearer and browser
clients. CSRF is unaffected — it is stateless double-submit (cookie value == header), never a signature.

Why: a forwarded mutating call is authorized at the **owning** node (ADR-0002 routing), which must first
authenticate the forwarded credential. With a per-node session key, a browser session minted on node A
got a 401 at node B; with swarm-shared `api_keys` and a swarm-wide session key, the owner verifies the
forwarded cookie/bearer directly. This keeps `cluster_secret` as the single swarm-wide secret boundary
(it already gates join + encrypts gossip, ADR-0001/0004) and avoids inventing a second cross-node
user-delegation credential.

Trade-offs: every node can mint sessions for any role, so a compromised node yields swarm-wide user
access. This adds no *new* exposure class — once `api_keys` (including `admin`) are replicated to every
node, a compromised node already holds swarm-admin, and `cluster_secret` is already the swarm-wide auth
boundary. Per-node session isolation is therefore a benefit already largely spent by the shared-key
model; we trade it for cross-node session verifiability with zero new mechanism.

Considered: (1) **node-key-vouched delegation** — keep the per-node session key; the entry node mints a
short-lived assertion signed by its Ed25519 node key and the owner trusts it against the gossiped
pubkey. Preserves per-node isolation and is attributable, but adds a whole new credential type +
verification path + OwnerProxy minting, for a benefit the shared-`api_keys` decision has already mostly
spent. (2) **A real shared user store / IdP** — correct long-term, but a central dependency the swarm is
explicitly designed to avoid (v1).
