# sbx-swarm-node M8a — Backend API Completion (design)

> **Context:** The M8 console plan (`docs/superpowers/plans/2026-06-15-sbx-swarm-node-m8-console.md`)
> predates M1–M7 shipping and assumes five endpoints the live API does not have. Rather
> than cut console features, M8 is split: **M8a** (this spec) fills the backend gaps;
> **M8b** builds the Nuxt console against the now-complete API. M8b is out of scope here.

**Goal:** Add the five backend capabilities a full swarm console needs, each following an
existing node pattern and each independently testable (Go unit + env-gated live-daemon
integration, like `internal/sandbox/sdkbackend_integration_test.go`).

## Gap → addition

| Console need | Missing today | M8a addition |
|---|---|---|
| Swarm topology | only `GET /v1/node` (self) | `ListNodes` → `GET /v1/nodes` |
| Templates page | not exposed (backend has it) | `ListTemplates` → `GET /v1/templates` |
| Operations history | no REST (ops persist already) | `ListOperations` → `GET /v1/operations` |
| Cordon/drain a peer | self-only | `node_id` on Cordon/Drain (forward to peer) |
| Interactive terminal | only unary `exec` | WS `GET /v1/sandboxes/{id}/terminal` |

## Architecture

The reads (nodes/templates/operations) are ordinary gRPC methods with grpc-gateway HTTP
annotations — same shape as every existing read. Cross-node cordon/drain extends the
existing self-only methods with an optional target and reuses the node-key gRPC forward
path. The terminal is the only non-gRPC addition: a native HTTP handler (grpc-gateway
cannot do WebSockets), exactly like the existing SSE handler.

### 1. `ListNodes` — `GET /v1/nodes` (read-only)
- New `NodeService.ListNodes`. Returns **self + gossiped peers**.
- Per node: `node_id`, `node_name`, `cordoned`, `labels`, `capabilities`, `workspaces`,
  `templates` (names), and `alloc`/`limit`/`actual` for cpu/mem/disk.
- **No `reachable` field** — liveness is *presence*: `NotifyLeave` removes a dead node from the
  routing table (firing `MarkUnreachable` on its sandboxes), so any node returned here is alive
  by construction. The "an owner died" signal stays the **sandbox** `Unreachable` state; node
  liveness and the sandbox `Unreachable`/`Lost` terms remain separate (glossary boundary).
- **`draining` is self-only** — `NodeState` does not gossip a draining flag, so a peer's draining
  state is unknowable; report it only for self (peers always `false`), or read it from `GET /v1/node`.
- Sources: self from local `sandbox.Capacity` + `NodeInfo`; peers from `routing.Table.Peers()`
  (+ `Addr`/`IsCordoned`) joined with `membership.Cluster.peerStates` (`NodeState` already carries
  labels, capabilities, workspaces, templates, and load). Standalone (no cluster) → just self.
- The gossiped per-node `templates` names make the swarm-wide template catalog available here
  (see §2). Test: fake routing table + cluster snapshot → assert self-plus-peers shape; standalone → self only.

### 2. `ListTemplates` — `GET /v1/templates` (read-only)
- New `NodeService.ListTemplates`, delegating to `Backend().ListTemplates()` (already exists,
  verified live), returning the **local node's rich templates** (repo/tag/flavor/image-id/created).
- **Decision A (corrected): the swarm-wide template *union* is already available** — `NodeState`
  gossips each peer's `Templates []string`, surfaced per node by `ListNodes` (§1). So nothing is
  deferred: the console's Templates page gets "which nodes hold which templates" from `ListNodes`
  (names), and `ListTemplates` adds the **local** node's rich metadata. The only thing genuinely
  unavailable is rich metadata for *peers'* templates (gossip carries names only) — not needed in v1.
- Test: Fake backend returns canned templates → assert passthrough.

### 3. `ListOperations` — `GET /v1/operations` (read-only)
- New `SandboxService.ListOperations`. Operations **already persist** in the bbolt
  `operations` bucket — add `ops.Manager.List()` returning records **newest-first**, with an
  optional `?limit` (default a sane cap, e.g. 200, to bound the response).
- Fields: `id`, `type`, `state`, `sandbox_id`, `error`, `created_at`, `updated_at`.
- This is the **durable** operation history (the bbolt bucket); it is distinct from the best-effort
  event firehose (ADR-0008). The console reads history here and overlays live `operation.*` events
  in M8b — the firehose is not the source of truth and is not made into one.
- Test: write several ops via the manager → `List` returns them newest-first; `limit` honored.

### 4. Cross-node cordon / drain
- Add `node_id` to `CordonRequest` / `DrainRequest`. **Empty = self** (today's behavior,
  backward compatible). Non-empty and ≠ self → **forward the RPC to that peer** via the peer
  pool, re-authenticating with the node key — the same mechanism `internal/apiserver/forward.go`
  uses for owner-routed sandbox calls (here the route key is the request's `node_id`, not a
  sandbox owner).
- **Decision B: extend the existing methods** (`POST /v1/node/cordon` with `{node_id}` body)
  rather than add a parallel `/v1/nodes/{id}/cordon` path — smaller surface, one code path.
- **Forward-to-peer, not gossiped directive** (ADR-0018): the target node updates its own gossiped
  `NodeState.Cordoned` — the deliberate opposite of revocation's gossiped denylist (ADR-0013),
  because a cordoned node is still trusted and authoritative over its own state.
- Mutating (admin). `Uncordon` gets the same treatment for symmetry.
- Test: forward-path unit test — a request with a peer `node_id` reaches that peer's
  self-cordon; empty `node_id` still cordons locally.

### 5. Terminal WebSocket — `GET /v1/sandboxes/{id}/terminal`
- **Native HTTP handler** on the REST mux, wrapped by the auth middleware, registered under
  `/v1/sandboxes/{id}/...` so it sits **behind the existing `OwnerProxy`**. Go's
  `httputil.ReverseProxy` transparently proxies `Connection: Upgrade` (WebSocket), so a
  **peer-owned sandbox's terminal is forwarded to its owner for free** — no custom stream proxy.
- New backend method `ExecInteractive(ctx, name, cmd, tty) (Session, error)` where `Session`
  exposes `Stdin() io.Writer`, `Stdout() io.Reader`, `Resize(cols, rows)`, `Wait() (int, error)`,
  `Close()`. SDKBackend implements it via the SDK `exec.ExecInteractive(..., WithTTY())`
  (`AttachSession` already provides exactly this surface). Fake gets a minimal echo stub so
  unit tests need no daemon.
- Bridge in the handler: copy `Stdout` → WS (binary frames) and WS (binary) → `Stdin`; a JSON
  control frame `{"type":"resize","cols":N,"rows":M}` calls `Resize`; on `Wait()` return,
  close the socket.
- **Decision C: dependency `github.com/coder/websocket`** (minimal, context-aware, net/http
  native) for the server side; the client is the browser's `WebSocket` (xterm.js) in M8b.
- **Decision D (ADR-0017):** default command `/bin/sh`. Auth is **cookie or bearer**, but note a
  browser `WebSocket` can set *neither* a custom header nor the CSRF token, so browser terminals are
  **cookie-only**; bearer remains for non-browser clients. The upgrade is a `GET` (evades the
  unsafe-method CSRF check), so a same-origin **`Origin` allowlist** replaces the double-submit token
  to stop Cross-Site WebSocket Hijacking. Reject mismatched `Origin` with 403.
- Tests: Fake-backed handler driven by a `coder/websocket` client — echo round-trip + a resize
  control frame is parsed and applied; a cross-origin upgrade is rejected. Env-gated
  integration test runs a real interactive shell against the live daemon (e.g. `echo` then exit,
  assert output + exit code).

## Cross-cutting
- Every new gRPC method is classified in `internal/apiserver/authz.go` (reads → read-only,
  cordon/drain → mutating) or `TestAuthz_AllMethodsClassified` fails by design.
- Proto edits (`node.proto`, `sandbox.proto`) regenerate via `buf generate`; gopls shows
  false "undefined/redeclared" after generation — trust `go build`/`go vet`.
- The terminal endpoint is **not** a gRPC method, so it is exempt from the authz drift guard;
  its auth is the middleware + origin check, asserted by its own handler test.
- TDD throughout (red test first); merges are user-driven local ff-merges.

## Out of scope (follow-ups)
- **M8b** — the Nuxt 4 + @nuxt/ui v4 console that consumes these endpoints.
- **Rich template metadata for peers** — gossip carries template *names* only; peers' tag/flavor/
  created are not surfaced (the swarm-wide *union of names* is available now, via `ListNodes`).
- **Files API** (CopyTo/CopyFrom) over REST — still the M1c deferred item; M8b stubs that tab.
- **SSE stats stream** — charts poll `GET .../stats`; no streaming-stats endpoint added.

## Self-review
- **Placeholders:** none — each addition names its endpoint, data source, authz class, and test.
- **Consistency:** reads are gRPC+gateway (matches existing); terminal is a native handler
  (matches SSE); cross-node forwarding reuses the node-key forward path (matches sandbox
  forwarding). No contradictions.
- **Scope:** one milestone of backend additions, all in `internal/` + proto; the frontend is
  explicitly deferred to M8b. Right-sized for a single implementation plan.
- **Ambiguity:** decisions A–D resolve the open forks explicitly (A: rich-local `ListTemplates` +
  swarm union from `ListNodes` gossip; B: `node_id` on existing methods, forward-to-peer per ADR-0018;
  C: `coder/websocket`; D: `/bin/sh` + Origin allowlist per ADR-0017). Glossary term **Terminal
  session** added to `CONTEXT.md`.
