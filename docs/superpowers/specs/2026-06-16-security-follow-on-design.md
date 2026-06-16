# Security follow-on — design

**Date:** 2026-06-16 · **Status:** approved + grilled, pending implementation plan
**Branch:** `security-node-trust` · **Related ADRs:** 0004 (node trust), 0006 (sessions), 0010 (swarm-wide user creds)

## Why

M1–M4 deliberately deferred the auth/trust work; M4 made it load-bearing by adding
node↔node gRPC/REST forwarding with no peer authentication. Three gaps must close
before the node is safe on an untrusted network:

1. **No role-gate.** A `read-only` principal can call every mutating RPC
   (Create/Delete/Start/Stop/Exec/AgentRun, SetPolicy/SetSecret/DeleteSecret,
   Cordon/Uncordon/Drain). `auth.RequireRole` exists but is unwired.
2. **No gRPC app-layer auth.** The multiplexed native-gRPC port is callable with
   no identity (`auth.RoleFromContext` is only set by the HTTP middleware).
3. **`InsecureSkipVerify` peer dials.** Peers dial each other with
   `tls.Config{InsecureSkipVerify:true}` at two sites (`peer.Pool` in
   `internal/node/node.go`; the `OwnerProxy` transport in
   `internal/apiserver/forward_http.go`) — MITM-able.

All three ship in **one milestone / one branch**.

## Threat model

- **Over-privileged caller:** an authenticated `read-only` principal mutates state. → role-gate.
- **Unauthenticated network access** to the raw gRPC port. → gRPC authn interceptor that
  rejects un-credentialed calls.
- **Network MITM** on A→B forwards, or an untrusted host impersonating a peer. → Ed25519
  node-key proof (audience-bound) + peer TLS pinned to the node key.

**Out of scope (vNext):** *gossiped* revocation denylist propagation. We add a **local**
denylist hook in the interceptor; distributing revocations over gossip is a later,
eventually-consistent layer (ADR-0004 treats it as such). Cross-node browser sessions ARE
in scope (see Cross-node credentials).

## The design crux (resolved)

The in-process grpc-gateway (`RegisterSandboxServiceHandlerServer`) calls service handlers
**directly** — REST traffic never traverses gRPC server interceptors (M4's "C1"). So a
single gRPC interceptor cannot, as-is, gate both paths.

**Resolution: loopback-gRPC unification.** Re-wire the gateway to dial an **in-memory
loopback gRPC connection** to the same `*grpc.Server` (one server can `Serve` an in-memory
listener *and* handle external h2 via the multiplexer's `ServeHTTP`; services + interceptor
chain are shared):

```
REST        : client → HTTP mw.Authenticate → OwnerProxy → gateway
              → [in-memory loopback gRPC conn] → grpcSrv → interceptors → handler
native gRPC : client → multiplexer (h2 + application/grpc) → grpcSrv → interceptors → handler
```

One interceptor chain = one authorization source of truth, covering both paths.

- **Loopback listener:** a small `net.Pipe`-backed in-memory `net.Listener` (NOT a
  `localhost:port` TCP dial — that would be network-reachable; the pipe is reachable only by
  the in-process gateway). The loopback conn uses **insecure** transport (in-process); it
  carries the HMAC-signed `x-sbx-authz` token (below). External peers hit the TLS port with
  node-key/bearer. The interceptor verifies every credential cryptographically, so it needn't
  distinguish loopback from external.
- **OwnerProxy stays before the gateway**, short-circuiting remote ids; the loopback only ever
  carries **local** calls into the interceptor. M4 forwarding is untouched — no double-forward.
- **REST contract preserved:** the gateway's `UseProtoNames`/`EmitUnpopulated` marshaler and the
  `Idempotency-Key` header matcher are unchanged; gRPC status→HTTP mapping is identical over the
  loopback conn, so M1b's snake_case JSON contract does not regress.

### Wrinkle 1 — cookie-authed REST callers have no `Authorization` header

Their role lives only in the request context set by `mw.Authenticate`; context values do not
cross the loopback wire. → a gateway **metadata annotator** (`runtime.WithMetadata`) runs after
`mw.Authenticate`, reads the role from the request context, and injects a freshly **signed**
token under a **distinct metadata key `x-sbx-authz`** (reusing `auth.Signer`).

grpc-gateway already forwards the inbound `Authorization` header as metadata `authorization`
(backwards-compat, unprefixed) **and** `grpcgateway-authorization`. Using a distinct key avoids a
two-`authorization`-values ambiguity. The interceptor resolves identity in strict order:

1. `x-sbx-authz` → `signer.Verify` → **user role**. *(loopback REST: identical for bearer- and
   cookie-authed callers; the forwarded `authorization` is ignored on this path.)*
2. else `authorization` → `KeyStore.RoleForKey` → **user role**. *(native-gRPC API-key clients;
   forwarded user creds)*
3. else `x-sbx-node-auth` → **node identity** (peer; §3).
4. else `Unauthenticated`.

Trust anchor for (1) is the HMAC signature (per-process `signer`), so an external gRPC caller
cannot forge `x-sbx-authz`.

### Wrinkle 2 — forwarding must not double-fire

`OwnerProxy` runs **before** the gateway, so it short-circuits remote sandbox ids; the loopback
only delivers **local** ids to the chain. The existing unary `Forward` interceptor handles
native-gRPC remote forwarding and is a no-op pass-through for local ids.

## Components

### 1. Interceptor chain (on `grpcSrv`; **always wired, even standalone**)

Unary chain `[authn, authz, forward]`; stream chain `[authn, authz]` (M4 forwarding is
unary-only; streams — `WatchEvents` — don't forward at the gRPC layer). Closing the native-gRPC
port matters even on a solo node, so authn/authz wire up regardless of cluster mode (node-key/pin
logic only engages when peers exist).

- **authn** (unary + stream): establishes identity per the order above, putting a **user role**
  and/or a **node identity** into ctx, and **rejecting** anything with no valid credential
  (`codes.Unauthenticated`).
- **authz** (unary + stream): **2-bucket policy** keyed by `info.FullMethod`:
  - **Mutating methods require an admin *user* role.** A node-only principal can never authorize a
    mutation by itself; forwarded mutations carry the user's admin credential alongside the
    node-key, so they pass.
  - **Everything else (reads + `WatchEvents`) requires *any* authenticated principal** — node-key
    or user. (So the SSE merge's node-only `WatchEvents` is allowed; a node-only principal can read
    but not mutate.)
  - **Drift guard test:** enumerate each registered service's methods and assert every one is
    classified (read vs mutating) and that each mutating method rejects a `read-only`/node-only ctx.
- **forward**: the existing `apiserver.Forwarder.UnaryInterceptor`, unchanged.

### 2. Gateway metadata annotator

`runtime.WithMetadata(roleAnnotator)`: reads `auth.RoleFromContext(r.Context())` (set by the
wrapping `mw.Authenticate`) and returns `metadata.Pairs("x-sbx-authz", signer.Mint(role, now+shortTTL))`
when present (short TTL — seconds; the token never leaves the process). Empty when unauthenticated
(the interceptor then rejects; `mw.Authenticate` already 401s first).

### 3. Node-key authentication (ADR-0004) — both layers, one identity

The single pinned identity is the node's Ed25519 **node key** (its pubkey is gossiped in
`NodeState.PubKey` and its hash is the `node_id`). No separate cert fingerprint.

**App-layer (who is the calling node) — audience-bound, replay-tight:**
- *Client:* `peer.Pool` attaches `PerRPCCredentials` returning
  `x-sbx-node-auth: <callerNodeID>.<targetNodeID>.<unixSec>.<base64(ed25519.Sign(priv, callerNodeID|targetNodeID|unixSec))>`.
  The dialer always knows `targetNodeID` (it resolved owner→addr to dial). `RequireTransportSecurity()=true`.
- *Server (authn interceptor):* parse → look up `pubkey` from gossip (`NodeState.PubKey`,
  TOFU-pinned) → verify `DeriveNodeID(pubkey)==callerNodeID`, `ed25519.Verify`,
  **`targetNodeID==own node_id`** (audience), freshness `|now-unixSec| ≤ 30s`, and `callerNodeID`
  not in the local denylist. On success the node identity is attributed in ctx.
- *Replay bound:* audience-binding stops a peer from replaying a captured token to a third node;
  the ±30s window bounds same-target replay statelessly. **Requires NTP/clock-sync across nodes;
  fail-closed on skew** (documented operational requirement).

**Transport (is the channel the right node) — TLS pinned to the node key:**
- The node's TLS leaf cert is regenerated as an **Ed25519 cert whose key *is* the node key**
  (self-signed by it), so the leaf cert pubkey == gossiped `NodeState.PubKey`. (`tlsutil` changes;
  existing nodes regenerate their throwaway self-signed cert once on upgrade.)
- Replace `InsecureSkipVerify:true` at **both** sites with `VerifyPeerCertificate` that pins:
  presented leaf cert pubkey == the target's gossiped `PubKey`, and `DeriveNodeID(pubkey)==expected
  owner node_id`. `InsecureSkipVerify` stays only to disable default CA-chain building; the pin is
  the real check (standard Go self-signed-pinning idiom).
- **Fail-closed** when no pin is known for the target yet (no silent fallback to unverified TLS).

**Pinning wiring:** `routing.Table` (the node directory) carries the pubkey alongside the address —
`entry{addr, cordoned, pubkey}`, `Upsert(nodeID, addr, cordoned, pubkey)`, `PubKey(nodeID)` —
populated from the gossiped `NodeState`. `peer.Pool` gains a **pin resolver** (`addr → expected
pubkey`, backed by `routing.Table`) to build per-peer pinned TLS creds, plus the local node-key
`PerRPCCredentials`; conns stay cached by addr (1:1 with nodeID). `OwnerProxy` builds its
`http.Transport` TLS config per target via the same resolver.

### 4. Gossip wiring

Populate `NodeState.PubKey = identity.PublicKey` in `internal/node/node.go` (currently unset).
No `CertFP` field — the pubkey covers both the PerRPC proof and the TLS pin.

## Cross-node credentials (ADR-0010)

A forwarded mutating call is authorized at the **owning** node, which must first authenticate the
forwarded credential. Two decisions make user credentials swarm-verifiable:

- **Shared `api_keys` (invariant).** Operators configure identical `api_keys` on every node — the
  leaderless swarm has no shared user store; `cluster_secret` gates *nodes*, `api_keys` gate *users*,
  replicated by the operator.
- **Swarm-wide session signing key.** Derive the session signer key from `cluster_secret`
  (`HKDF-SHA256(cluster_secret, "sbx-session-v1")`) instead of `id.PrivateKey.Seed()`; fall back to
  the per-node seed only when standalone. A cookie minted by any node then verifies on every node, so
  **cross-node browser sessions work** via OwnerProxy's existing cookie forwarding with no new
  mechanism. CSRF is already stateless double-submit (cookie value == header), so it works cross-node
  unchanged.

Rationale + trade-offs (per-node isolation already spent by shared admin api_keys) are in ADR-0010.

## Data-flow summary (every path authenticated + authorized)

| Path | Authn | Authz | Forwarding |
|------|-------|-------|------------|
| REST local mutation | HTTP mw → annotator signs role into `x-sbx-authz` → loopback → authn verifies | authz (admin user) | OwnerProxy local pass-through |
| REST remote mutation | OwnerProxy forwards (pinned TLS; user bearer/cookie) → owner re-auths via HTTP mw + annotator | owner's authz (admin user) | OwnerProxy → owner |
| gRPC local mutation | authn (`authorization` API key) | authz (admin user) | Forward local pass-through |
| gRPC remote mutation | authn (caller node-key + user API key) → forward; owner re-authn+authz | owner's authz (admin user) | Forward → owner (pinned TLS + node-key) |
| SSE merge `WatchEvents` (peer) | authn (node-key only) | authz: read/WatchEvents → allowed | n/a (direct per-peer stream) |
| SSE (logs/stats/events, browser) | HTTP mw (reads) | n/a (read) | OwnerProxy (pinned TLS) |

## Error handling

- authn failure → `Unauthenticated`/401; authz failure → `PermissionDenied`/403 (gateway maps
  gRPC→HTTP identically over loopback).
- TLS pin mismatch / unknown pin / denylisted / stale/forged node-key → dial or call rejected,
  logged with the `node_id` + reason. **Never log cert bytes, node keys, or tokens** (secrets
  invariant, spec §11).

## Testing

- **authz drift guard:** every mutating method rejects `read-only` and node-only; reads pass.
- **authn interceptor (unary + stream):** no creds → Unauthenticated; valid `x-sbx-authz` → user
  role; valid API key → user role; valid audience-bound node-key → node identity; wrong-audience /
  stale / forged node-key → reject.
- **annotator round-trip:** cookie-authed REST → mutating RPC succeeds for admin, 403 for read-only
  (exercises the signed-token bridge through the loopback).
- **TLS pinning:** peer with non-matching leaf pubkey rejected; matching accepted; missing pin
  fail-closed. (`peer` + `apiserver`, `-race`.)
- **Cross-node session:** a cookie minted on node A authorizes a forwarded call on node B (swarm-wide
  session key).
- **Integration (`//go:build integration`):** a forwarded mutating call across nodes carries
  node-key + user creds and is authorized end-to-end; an un-pinned/unauthenticated dial fails;
  `WatchEvents` merge across nodes succeeds on node-key alone.
- **No-regression:** default suite + `-race` green; REST snake_case contract intact; secrets-leak
  invariant preserved.

## Files touched (anticipated)

- `internal/auth/` — `mutatingMethods` classification set; signed `x-sbx-authz` mint/verify reuse.
  (Authorization is centralized in the interceptor keyed by gRPC method — handlers get **no**
  per-method `RequireAdmin`.)
- `internal/apiserver/server.go` — in-memory loopback listener + gateway re-wire
  (`RegisterXHandlerServer` → loopback `RegisterXHandler`), `WithMetadata` annotator, unary+stream
  interceptor chains (authn + authz + existing forward).
- `internal/apiserver/` — new `authn.go` / `authz.go` (unary + stream); `forward_http.go` OwnerProxy
  per-target pinned transport; loopback listener helper.
- `internal/peer/client.go` — pin resolver → per-peer pinned TLS creds + node-key `PerRPCCredentials`.
- `internal/nodekey/` (new) or `internal/identity/` — audience-bound PerRPC sign/verify helpers.
- `internal/tlsutil/tlsutil.go` — generate the TLS leaf cert from the Ed25519 node key.
- `internal/membership/state.go` / `internal/node/node.go` — populate `NodeState.PubKey`; thread it
  into `routing.Table`; build the pinned dialer; derive the swarm-wide session key from
  `cluster_secret`.
- `internal/routing/table.go` — carry the pubkey in `entry`; `Upsert`/`PubKey`.
- `internal/auth/session.go` / signer wiring — swarm-wide session key derivation (ADR-0010).
- Tests across the above; integration extension in `internal/membership/`.
