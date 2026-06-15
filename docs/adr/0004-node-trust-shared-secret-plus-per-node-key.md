# Node-to-node trust: shared secret + per-node key, no CA

Node↔node gRPC uses TLS for encryption and a two-layer, CA-free trust model. The shared
`cluster_secret` gates joining/gossip and bootstraps trust. Each node generates its own keypair on
first run and derives `node_id = short-hash(pubkey)`, making the ID self-certifying. Public keys are
distributed and pinned via the secret-gated gossip channel (trust-on-first-use). A peer authenticates
by proving possession of the private key bound to its `node_id` (signed challenge, or a pubkey-pinned
self-signed client cert). Revocation is a gossiped per-node key denylist.

Why: spoof-resistant per-node identity, action attribution, and per-node revocation, while keeping
CA-free, secret-only bootstrapping (a node needs only seeds + `cluster_secret`; its key is
auto-generated). It fits the self-routing IDs (ADR-0002) — the node-id prefix becomes
cryptographically meaningful.

Trade-offs: a trust-on-first-use window (a node's key is trusted the first time it appears with a valid
secret), and eventually-consistent revocation over gossip rather than instant like a CRL. Full
CA-issued mTLS (hierarchical trust, instant revocation) is deferred to vNext; gRPC auth sits behind an
interface so it can drop in without reworking call sites.

Considered: plain shared-token (simplest, but no per-node identity/attribution, only whole-secret
rotation); CA mTLS (strongest, but a CA to operate and a chicken-and-egg bootstrap).
