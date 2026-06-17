# Internal provision is node-authorized; admin enforced once at ingress

The swarm's authorization rule (ADR-0004, security follow-on) is **two-bucket**:
mutating RPCs require an admin *user* role; a node-key principal alone may read
but **never mutates**; unknown methods fail closed. The internal node→node
`InternalService.Provision` RPC creates a sandbox on a target node — a mutation —
yet we authorize it on **node identity alone** (`principal.node == true`, a
verified swarm peer), in a distinct `internalMethods` bucket. A user-only
principal cannot call it.

This is sound because **admin is enforced exactly once, synchronously, at the
request's entry node**: `SandboxService.CreateSandbox` is a mutating method, so
the caller's admin user role is checked before the asynchronous provision
operation even starts. The coordinator then runs inside that detached operation
and dials the chosen target through `peer.Pool`, which attaches the node-key
`PerRPCCredentials`. The target authorizes the hop by swarm membership, not by a
re-presented user role.

## Why not forward the user's admin token to the target

The alternative — keep `Provision` a full mutation and thread the caller's admin
credential to the target alongside the node-key (as the synchronous gRPC
forwarder does for remote Get/Delete) — is both more plumbing and fragile here:

- The create operation runs **detached on `context.Background()`** so it can
  outlive the HTTP request; the request-scoped metadata carrying the user token
  is already gone by the time the coordinator fires.
- The REST role token (`x-sbx-authz`) is minted with a **30-second TTL**. A slow
  provision would outlive the token, and the target would reject a still-valid
  placement. Capturing and refreshing the token across the async boundary adds
  state for no security gain.

## Consequences

- **Bounded relaxation.** A verified swarm peer can trigger an internal provision
  without re-presenting a user role. The blast radius is bounded by: node-key is
  already the swarm-membership trust boundary (ADR-0004); the target's
  **admission** caps resource consumption (`TryReserve` against the provision
  limit); and revocation rides the existing denylist hook (gossiped propagation
  is vNext).
- `Provision` carries no gateway annotation and is registered on the gRPC server
  only — never reachable over REST.
- The authz drift-guard test enumerates `InternalService` so `Provision` must
  stay classified; it is node-gated, not open.

## Considered

- **Forward the admin token** (above) — rejected: detached-context + token-TTL
  fragility, no added safety over ingress enforcement.
- **Make `Provision` admin-required and run placement synchronously in the
  request** — would block the client on cross-node provisioning and abandon the
  async-operation model (M1c); rejected.
