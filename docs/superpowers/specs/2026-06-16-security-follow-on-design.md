# Security follow-on — design

**Date:** 2026-06-16 · **Status:** approved, pending implementation plan
**Branch (planned):** `security-node-trust`

## Why

M1–M4 deliberately deferred the auth/trust work; M4 made it load-bearing by adding
node↔node gRPC/REST forwarding with no peer authentication. Three gaps must close
before the node is safe on an untrusted network:

1. **No role-gate.** A `read-only` principal can call every mutating RPC
   (Create/Delete/Start/Stop/Exec/AgentRun, SetPolicy/SetSecret/DeleteSecret,
   Cordon/Uncordon/Drain). `auth.RequireRole` exists but is unwired.
2. **No gRPC app-layer auth.** The multiplexed native-gRPC port is callable with
   no identity at all (`auth.RoleFromContext` is only set by the HTTP middleware).
3. **`InsecureSkipVerify` peer dials.** Peers dial each other with
   `tls.Config{InsecureSkipVerify:true}` at two sites (`peer.Pool` creds in
   `internal/node/node.go`; the `OwnerProxy` transport in
   `internal/apiserver/forward_http.go`) — MITM-able.

All three ship in **one milestone / one branch** (user decision).

## Threat model

- **Over-privileged caller:** an authenticated `read-only` principal mutates state.
  → role-gate.
- **Unauthenticated network access** to the raw gRPC port. → gRPC authn interceptor
  that rejects un-credentialed calls.
- **Network MITM** on A→B forwards, or an untrusted host impersonating a peer. →
  Ed25519 node-key proof + pinned peer TLS.

**Out of scope (vNext, documented):** gossiped *revocation* denylist propagation.
We add a *local* denylist hook in the interceptor; distributing revocations over
gossip is a later, eventually-consistent layer (ADR-0004 treats it as such).

## The design crux (resolved)

The in-process grpc-gateway (`RegisterSandboxServiceHandlerServer`) calls service
handlers **directly** — REST traffic never traverses gRPC server interceptors
(this is M4's "C1"; it's why M4's forwarding interceptor didn't cover REST). So a
single gRPC interceptor cannot, as-is, gate both paths.

**Resolution: loopback-gRPC unification.** Re-wire the gateway to dial an
**in-memory loopback gRPC connection** to the same `*grpc.Server`. A single
`grpc.Server` can `Serve` an in-memory listener *and* serve external HTTP/2 via
the multiplexer's `ServeHTTP`; registered services and the interceptor chain are
shared. Net effect:

```
REST        : client → HTTP mw.Authenticate → OwnerProxy → gateway
              → [in-memory loopback gRPC conn] → grpcSrv → interceptors → handler
native gRPC : client → multiplexer (h2 + application/grpc) → grpcSrv → interceptors → handler
```

One interceptor chain = one authorization source of truth, covering both paths.

**Why loopback over dual-layer:** DRY-est, single authz source keyed by the real
gRPC method name (no parallel REST-route list to drift). Trade-offs accepted: a
bigger wiring change and an in-process pipe hop per REST call (in-memory, not a
network/TLS socket — cheap).

### Two wrinkles the loopback creates, and how they're handled

1. **Cookie-authed REST callers have no `Authorization` header.** Their role lives
   only in the request context set by `mw.Authenticate`, and context values do not
   cross the loopback gRPC wire. → a gateway **metadata annotator**
   (`runtime.WithMetadata`) runs after `mw.Authenticate`, reads the role from the
   request context, and injects `authorization: Bearer <signer.Mint(role, shortExp)>`
   as gRPC metadata. It is a *signed* assertion (reuses the existing `auth.Signer`,
   HMAC over the node key seed), so an external gRPC caller cannot forge a role
   header. Bearer-authed REST callers get the same treatment (their role is
   re-minted); the original bearer is irrelevant past the HTTP layer.

2. **Forwarding must not double-fire.** `OwnerProxy` runs **before** the gateway, so
   it short-circuits remote sandbox ids; the loopback only ever carries **local**
   calls into the interceptor chain. The existing `Forward` interceptor still
   handles native-gRPC remote forwarding and is a no-op pass-through for the local
   ids the loopback delivers. M4's forwarding path is otherwise untouched.

## Components

### 1. Interceptor chain (chained on `grpcSrv`, order: authn → authz → forward)

- **authn** (`grpc.UnaryServerInterceptor` + stream variant): establishes identity,
  puts role (and, for peers, node identity) in ctx, and **rejects** anything with no
  valid credential (`codes.Unauthenticated`). Credential sources, tried in order:
  1. **Signed role token** — `authorization: Bearer <token>` where the token verifies
     against `auth.Signer.Verify` → role. (loopback REST path)
  2. **Bearer API key** — `authorization: Bearer <key>` where `KeyStore.RoleForKey`
     resolves → role. (native-gRPC clients with an API key; forwarded user creds)
  3. **Node-key PerRPC token** — `x-sbx-node-auth` metadata (see §3). Authenticates the
     calling *node*; carries no user role on its own (a forwarded call also carries
     the user's bearer/signed-token in (1)/(2) for authz).

  A call may present both a node-key token (caller = trusted peer) and a user
  credential (end-user role) — both are honored.

- **authz**: keyed by `info.FullMethod`. A static `mutatingMethods` set requires role
  `admin`; otherwise `codes.PermissionDenied`. Reads (List*/Get*) require only a valid
  authenticated role. Single source of truth.
  - **Drift guard test:** enumerate each registered service's methods via proto
    reflection / a hand-maintained inventory and assert every method is classified
    (read vs mutating), and that each mutating method rejects a `read-only` ctx.

- **forward**: the existing `apiserver.Forwarder.UnaryInterceptor`, unchanged.

### 2. Gateway metadata annotator

`runtime.NewServeMux(..., runtime.WithMetadata(roleAnnotator))`. `roleAnnotator(ctx,
r)` reads `auth.RoleFromContext(r.Context())` (set by `mw.Authenticate`, which wraps
the gateway) and, when present, returns `metadata.Pairs("authorization", "Bearer "+
signer.Mint(role, now+shortTTL))`. Nil/empty when unauthenticated (the interceptor
then rejects — defense in depth; `mw.Authenticate` already 401s first).

### 3. Node-key authentication (ADR-0004) — both layers

**App-layer (who is the calling node):**
- *Client:* `peer.Pool` dials with `grpc.WithPerRPCCredentials(nodeKeyCreds)`.
  `nodeKeyCreds.GetRequestMetadata` returns
  `x-sbx-node-auth: <nodeID>.<unixSec>.<base64(ed25519.Sign(priv, nodeID|unixSec))>`.
- *Server (authn interceptor):* parse → look up `pubkey` from gossip
  (`NodeState.PubKey`, TOFU-pinned) → verify `DeriveNodeID(pubkey)==nodeID`,
  `ed25519.Verify`, freshness window (|now-unixSec| ≤ ~30s, stateless replay bound),
  and `nodeID` not in the local denylist. On success, node identity is attributed in
  ctx.
- *Trade-off:* signed-timestamp (vs interactive challenge/response) keeps it stateless
  and PerRPCCredentials-friendly; the ±30s window is the replay bound. Documented.

**Transport (is the channel the right node):**
- Replace `InsecureSkipVerify:true` at **both** sites with a `VerifyPeerCertificate`
  that **pins** `sha256(presented leaf cert DER)` to the peer's gossiped
  `NodeState.CertFP`. `InsecureSkipVerify` stays `true` only to disable the default CA
  chain build; the pin does the real check (standard Go self-signed-pinning idiom).
- *Per-peer dial:* the dialer must know the target owner's pinned identity. The
  Forwarder/OwnerProxy already resolve `owner` (node_id) → `addr` via `routing.Table`;
  extend the routing/gossip surface to also expose the owner's `PubKey` + `CertFP`, and
  thread the expected pin into the dial (`peer.Pool` gains a per-peer creds/verify
  resolver; OwnerProxy builds its `http.Transport` TLS config per target).
- *Fail-closed:* if no pin is known for the target yet, the dial is refused with a
  clear error (no silent fallback to unverified TLS).

### 4. Gossip wiring

- Populate `NodeState.PubKey = identity.PublicKey` in `internal/node/node.go`
  (currently unset).
- Add `NodeState.CertFP []byte` (`sha256(leaf cert DER)`), populate it from the loaded
  TLS cert, and include it on the bulk (TCP push/pull) channel alongside `PubKey`.
- Surface `PubKey` + `CertFP` for a given owner through the routing/cluster layer so
  the dialer can pin.

## Data-flow summary (every path authenticated + authorized)

| Path | Authn | Authz | Forwarding |
|------|-------|-------|------------|
| REST local mutation | HTTP mw → annotator signs role → loopback → authn verifies token | authz interceptor (method map) | OwnerProxy local pass-through |
| REST remote mutation | OwnerProxy forwards (pinned TLS, user bearer/cookie) → owner re-auths via HTTP mw | owner's authz interceptor | OwnerProxy → owner |
| gRPC local mutation | authn interceptor (bearer / signed token) | authz interceptor | Forward interceptor local pass-through |
| gRPC remote mutation | authn (caller node-key + user bearer) → forward; owner re-authn+authz | owner's authz interceptor | Forward interceptor → owner (pinned TLS + node-key) |
| SSE (logs/stats/events) | HTTP mw (reads; no admin gate) | n/a (read) | OwnerProxy (pinned TLS) |

## Error handling

- authn failure → `codes.Unauthenticated` (gRPC) / 401 (REST, via gateway status
  mapping); the HTTP `mw.Authenticate` already 401s before the gateway for the REST
  path.
- authz failure → `codes.PermissionDenied` / 403.
- TLS pin mismatch / unknown pin → dial error surfaced as a 502/`Unavailable` on the
  forwarding path; logged with the target node_id (never the cert bytes).
- Node-key verify failure (bad sig / stale / denylisted) → `codes.Unauthenticated`,
  logged with node_id + reason.

## Testing

- **authz drift guard** (above): every mutating method rejects `read-only`; reads pass.
- **authn interceptor:** table-driven — no creds → Unauthenticated; valid signed token →
  role; valid bearer → role; valid node-key → node identity; stale/forged node-key →
  reject.
- **annotator round-trip:** cookie-authed REST request → mutating RPC succeeds for
  admin, 403 for read-only (exercises the signed-token bridge end-to-end through the
  loopback).
- **TLS pinning:** a peer presenting a non-matching cert fingerprint is rejected;
  matching is accepted; missing pin is fail-closed. (`peer` + `apiserver` packages,
  with `-race`.)
- **Integration (`//go:build integration`):** extend the membership multi-node test so a
  forwarded mutating call across nodes carries node-key + user creds and is
  authorized end-to-end; assert an un-pinned/unauthenticated dial fails.
- **No-regression:** existing default suite + `-race` on concurrent pkgs stays green;
  secrets leak invariant (spec §11) preserved (never log cert bytes / node keys / tokens).

## Files touched (anticipated)

- `internal/auth/` — the `mutatingMethods` classification set + signed-token
  mint/verify reuse. (Authorization is centralized in the interceptor keyed by gRPC
  method — handlers get **no** per-method `RequireAdmin` calls.)
- `internal/apiserver/server.go` — loopback listener + gateway re-wire
  (`RegisterXHandlerServer` → loopback `RegisterXHandler`), `WithMetadata` annotator,
  interceptor chain (authn + authz + existing forward).
- `internal/apiserver/` — new `authn.go` / `authz.go` interceptors; `forward_http.go`
  OwnerProxy TLS pinning.
- `internal/peer/client.go` — per-peer creds/verify + node-key PerRPCCredentials.
- `internal/identity/` or new `internal/nodekey/` — PerRPC sign/verify helpers.
- `internal/membership/state.go` — `CertFP` field; `internal/node/node.go` — populate
  `PubKey`/`CertFP`, build pinned dialer.
- `internal/routing/` — expose owner `PubKey`/`CertFP` for pinning.
- Tests across the above; integration test extension in `internal/membership/`.

## Open implementation notes (settle during planning)

- In-memory loopback listener: `net.Pipe`-backed listener vs `bufconn`. Prefer a tiny
  production listener (avoid the `test/bufconn` import in prod paths) unless bufconn is
  deemed acceptable.
- Stream interceptor parity (events/SSE are HTTP, but `EventService.WatchEvents` is a
  native gRPC stream — it needs the authn stream interceptor too).
- Short TTL for the minted loopback role token (seconds; it never leaves the process).
