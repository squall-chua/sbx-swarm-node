# sbx-swarm-node — Design

**Status:** Draft (approved for planning) · **Date:** 2026-06-15

A single Go binary that runs a Docker-sandbox host as either a standalone node or a member of a
decentralized swarm. It wraps the [`sbx-go-sdk`](https://github.com/squall-chua/sbx-go-sdk) (v0.1.2),
adds a network layer (gRPC + REST + SSE), peer-to-peer membership via gossip, constraint-based
placement, and an embedded web console.

---

## 1. Goals

1. Offer a programmatic API (gRPC + REST) for clients to **manage and monitor** sandboxes.
2. Run **standalone**, or **form a swarm** with peers over gossip — **no central controller**.
3. In swarm mode, **intelligently place** a sandbox on a member node using a selected strategy and
   hard constraints (workspaces, resource limits).
4. Keep running **independently** if it drops out of the swarm, and **rejoin** the same or a new
   swarm later, reconciling state.
5. Provide a **web console** to manage, monitor, and visualize provisioning.

Non-goals (v1): multi-tenancy/RBAC, cross-node workspace data replication, auto-reschedule of
sandboxes on node failure, host-OS metric collection, scheduled provisioning, OpenTelemetry tracing.

---

## 2. Background — the sbx-go-sdk (v0.1.2)

A "sandbox" is a disposable, isolated micro-VM managed by a **local per-host daemon** (`sandboxd`)
plus the `sbx` binary. The SDK talks to the local daemon over a Unix socket and shells out to `sbx`.
**The SDK has no remote/network API** — this node *is* the network layer around it.

Capabilities we rely on (package → function):

- `client` — `New(WithAutoStart/WithStrictVersion)`, `Health`, `DaemonStatus`; sentinels
  `ErrSandboxNotFound/Exists/NotRunning/IncompatibleVersion/ErrUnexpectedFormat`.
- `sandbox` — `Create`, `Run`, `List`, `Get`, `Start`, `Stop`, `Remove`, `Inspect`, `State`,
  `CopyTo`, `CopyFrom`, `PublishPort`, `Ports`, `UnpublishPort`, `SaveTemplate`.
  Create options: `WithAgent`, `WithWorkspace(path[:ro])`, **`WithClone()`** (private in-container git
  clone instead of bind-mount), `WithName`, `WithCPUs`, `WithMemory`, `WithTemplate`, `WithProfile`.
- `exec` — `Exec` (capture), `ExecInteractive` (TTY → `AttachSession`), `ExecDetached`,
  **`Stats(ctx, sb) → *Usage`** (`Cores, CPUPercent, MemTotalKB, MemAvailableKB, MemUsedKB,
  UptimeSeconds, DiskTotalGB, DiskUsedGB`; ~200 ms in-VM probe; running-only),
  **`Logs(path) → AttachSession`** (continuous `tail -F`).
- `policy` — `SetDefault`, `Allow`, `Deny`, `RemoveRule`, `Reset`, **`List → []PolicyRule`**
  (PROVENANCE, APPLIES_TO, POLICY/RULE, TYPE, DECISION, RESOURCES) + `ListRaw`, `Profiles`,
  **`Log → *LogResult{ BlockedHosts []BlockedHost{Host, VMName} }`** (blocked egress only,
  daemon-wide, one-shot, no timestamps).
- `secret` — `SetCustom(scope, CustomSecret{Host, Env, Value})`, **`List → *Secrets{Stored, Custom}`**
  + `ListRaw`, `Remove(scope, host)`. **Experimental**; value leaks into host process listings; SDK
  recommends `exec.WithEnv` for headless credentials.
- `template` — `List`, `Load`, plus `sb.SaveTemplate`.

Version pin: SDK **v0.1.2**, targeting `sbx`/`sandboxd` v0.32.0, daemon REST API 0.10.0. Validate at
startup with `WithStrictVersion`.

**Design implication — `SandboxBackend` interface.** All SDK use sits behind a single internal
interface so the rest of the system is testable with a fake. The SDK adapter is the only place that
imports `sbx-go-sdk`.

---

## 3. Architecture

Approach **A — leaderless gossip with target-authoritative placement** (AP / partition-tolerant). No
leader, no consensus on the hot path; the node chosen for a sandbox is the source of truth for it.
(Rejected: Raft-based scheduling — a leader is a soft central controller and blocks scheduling under
partition, breaking run-solo. Deferred: a CRDT global registry for consistent cluster-wide queries.)

One process per host, all components in-process:

| Component | Responsibility |
|---|---|
| **API Server** | gRPC + grpc-gateway (REST) on one TLS port; SSE handlers; WS terminal bridge; serves embedded SPA; auth |
| **Coordinator** | Runs filter→score for incoming provisions; forwards to target; retries on NACK |
| **Sandbox Manager** | Owns local sandboxes; drives `SandboxBackend`; reconciles SDK truth ↔ store |
| **Scheduler** | Pure filter→score over the gossiped view; pluggable strategies |
| **Membership** | `hashicorp/memberlist`: gossip, SWIM failure detection, encrypted wire + join secret; disseminates node state in three tiers (tiny `NodeMeta` / TCP push-pull / delta broadcasts — ADR-0005) |
| **Metrics Collector** | Polls `exec.Stats` per running sandbox; caches; aggregates `actual_util` |
| **Network Log Collector** | Polls `policy.Log`; diffs; accumulates blocked-egress events |
| **Event Bus** | In-process pub/sub of all domain events; backs SSE + gRPC `WatchEvents` |
| **Git Lifecycle** | Pre-fetch / publish for clone-mode workspaces; host-side credentialed git |
| **Reaper** | Enforces absolute **TTL**; optional **activity-based idle** (opt-in): idle = no in-flight op + `last_activity_at` past `idle_timeout` ⇒ **stop** (triggers auto-publish), removed later by retention. Never CPU-based, never while an op is active |
| **State Store** | `bbolt` — node identity, sandbox records, operations, blocked-egress, audit |
| **Peer Client** | gRPC client to peers: provision RPC, request/stream forwarding, `WatchEvents` |

---

## 4. Data model

**Node config** (file + env + flags; precedence flags > env > file):
**node keypair** (generated + persisted on first run; `node_id = short-hash(pubkey)`, self-certifying —
ADR-0004), `node_name`, REST/gRPC bind addr (one TLS port), gossip bind
addr, `join` seed peers (empty ⇒ standalone), `cluster_secret` (memberlist keyring key),
`provision_limits {cpu_cores, memory_bytes}`, `workspaces [{name, host_path, read_only,
git?{bare:true, remote, default_branch, auth_secret_ref, allow_push, pre_steps[[argv]],
publish_steps[[argv]], exec_allowlist}}]`, `swarm_id`/`swarm_name`, `cluster_secret`, node `labels`,
`api_keys` (each with a role: `admin` | `read-only`), TLS server cert/key (no CA — ADR-0004), default
strategy, persistence path, poll intervals (stats,
network), reaper config. **Git pre/publish pipelines live here (node-local) — never in API requests
(ADR-0003).**

**Disseminated node state** (split across `memberlist` channels by size — ADR-0005: tiny `NodeMeta`
over UDP = `node_id`/REST address/`cordoned`/`state_version`/**`protocol_version`** [ADR-0009];
everything bulky below over TCP push/pull; per-change deltas via broadcast): `node_id`, **`node_pubkey`**
(pinned TOFU for peer auth — ADR-0004), `node_name`, **REST address** (for proxy/forward), liveness,
**`capabilities`** (clone/stats/logs/structured_policy… from sbx/SDK version — ADR-0009), `provision_limits`, `allocated {cpu, mem}`, `actual_util {cpu, mem}`
(secondary), available **workspace names** (+ ro/rw, git-enabled), available **template names**,
labels, **owned sandbox IDs**
(for `sandbox_id → owner` routing map), `blocked_egress_distinct_count`, `cordoned` flag, state
version.

**Sandbox record** (authoritative on owner, persisted): swarm `id` = `<owner_node_id>.<ulid>`
(self-routing — see ADR-0002), `owner_node_id`, SDK ref/name,
spec `{template, agent, cpu, memory, workspaces[], clone bool, env_keys[] (values NOT stored), labels,
ttl, idle_timeout, idle_reap bool (opt-in)}`, `status`
(`requested→placing→provisioning→running→stopped→failed`, plus `unreachable` = owner suspect/dead per
gossip [peer guess], and `lost` = owner-confirmed gone [terminal, owner-declared only]),
published ports, `last_stats {usage, sampled_at}`, **`last_activity_at`** (bumped on any swarm-side
touch — drives idle), git `{branch, last_publish}`, timestamps.

**Operation record** (async): `op_id` = `<coordinator_node_id>.<ulid>` (self-routing; operations are
**not** gossiped — a poll routes to the coordinator via the prefix), type
(`provision|stop|remove|publish|…`), optional **`idempotency_key`**, `state`
(`pending→running→done|error`), target node, sandbox id, error, timestamps. Provision idempotency: the
coordinator persists `idempotency_key → op_id` (TTL'd ~1h) and is authoritative; the mapping is also
delta-broadcast so peers dedupe a failover retry within propagation time (narrow residual race
accepted). Clients should retry the same endpoint, then poll the op (self-routing) rather than
re-POSTing.

**Blocked-egress pair** (per node, persisted): `{host, sandbox_id, node_id, first_seen, last_seen}` —
distinct `(host, sandbox)` pairs only; timestamps synthesized by us (the SDK provides none); attributed
via `VMName`. **No attempt/frequency count** — `policy.Log` returns presence, not occurrences. A
`distinct_count` (number of distinct pairs) is the only derivable aggregate.

**Domain event** (event bus / SSE — best-effort notification, **not** a durable log; ADR-0008):
`{id (per-node monotonic + node_id), ts, type, node_id, sandbox_id?, payload}`, served from a bounded
per-node replay buffer. Types: membership (up/suspect/down), sandbox lifecycle, scheduling
(candidates/winner/nack/retry), operation, policy/secret/git-publish (audit), cordon/drain, reconcile.

**Audit record** (persisted): credentialed/sensitive actions — git publish, policy change, secret
set/remove — `{actor, action, target, outcome, ts}`; **never** records secret values.

---

## 5. Transport & API

**One TLS port, multiplexed.** A single listener dispatches: HTTP/2 + `content-type:
application/grpc` → gRPC server; other HTTP → grpc-gateway (REST/JSON, **unary only**) and SSE/WS
handlers; unmatched paths → embedded SPA static file server. TLS with ALPN negotiates h2/http1.1
(h2c in dev).

**Node↔node = native gRPC** (TLS + shared-secret + per-node-key auth, no CA — ADR-0004): provision RPC,
transparent request/stream
forwarding to a sandbox's owner, and `WatchEvents` for the swarm-wide event merge. Replaces any HTTP
reverse-proxy.

**Streaming model:**

| Stream | Native client | Browser / HTTP |
|---|---|---|
| swarm events, stats, logs, network-blocked, operations | gRPC server-stream | **SSE** |
| interactive terminal (bidi) | gRPC bidi | **WS bridge** (only WS in the system) |
| CRUD / management (unary) | gRPC | REST via grpc-gateway |

SSE: `GET /v1/events?types=&node=&sandbox=` (+ per-sandbox `/stats`, `/logs`, `/network`); `id:` drives
`Last-Event-ID` resume against a bounded per-node ring buffer; `event:` carries the type.

**API surface** (`/v1`, bearer-auth, TLS; every call auto-routes to the owning node):

- **Sandboxes:** `POST /sandboxes` (→`202 {op_id}`; accepts an **`Idempotency-Key`** header — a repeat
  returns the same op, see §Operations), `GET /sandboxes` (filter label/node/status/
  workspace), `GET|DELETE /sandboxes/{id}`, `POST /sandboxes/{id}/{start|stop}`,
  `POST /sandboxes/{id}/exec` (sync capture), **`POST /sandboxes/{id}/agent-run`** (async `ExecDetached`
→ op; request supplies the command — safe because sandbox-contained, unlike host-side git per ADR-0003;
owner polls to **exit code**; optional `publish_on_success`), `GET /sandboxes/{id}/stats?fresh=`, ports
(`GET/POST/DELETE`), files
  (CopyTo/From), `POST /sandboxes/{id}/template`, `POST /sandboxes/{id}/git/publish`.
- **Sandbox streams:** SSE `…/stats`, `…/logs`, `…/network`; WS `…/terminal`.
- **Network/secrets (per-sandbox):** `GET …/network/blocked`, `GET/PUT …/policy`,
  `GET/PUT/DELETE …/secrets` (write-only, masked).
- **Templates (swarm catalog):** `GET/POST /templates`, `GET/DELETE /templates/{name}`.
- **Nodes/cluster:** `GET /nodes`, `GET /nodes/{id}`, `POST /nodes/{id}/{cordon|uncordon|drain}`,
  node-level policy/secrets, `GET /cluster`, `PUT /cluster/policy/default` (fan-out),
  `POST /cluster/{join|leave}`, `GET /events` (SSE firehose).
- **Operations:** `GET /operations[/{id}]`; SSE via the events firehose (filter by type).
- **Auth:** `POST /auth/session` (exchange an API key for a browser session cookie), `DELETE
  /auth/session` (logout) — see ADR-0006.
- **Ops:** `GET /healthz`, `/readyz`, `/metrics` (Prometheus). OpenAPI spec generated from gateway
  annotations.

---

## 6. Scheduling — filter → score

**Constraint-based placement** over the gossiped view:

1. **Filter (hard predicates):** keep nodes that (a) advertise **every** requested workspace by name,
   (b) advertise the requested **template** (if any), (c) provide every required **capability** (e.g.
   `clone` for a clone-mode request — ADR-0009), (d) have remaining capacity within `provision_limit`
   for requested cpu/mem, (e) are not `cordoned`, (f) satisfy label affinity/anti-affinity.
2. **Score (strategy, tiebreak — ADR-0007):** each candidate's score is its **post-placement
   dominant-resource ratio** `max((allocated.cpu+req.cpu)/limit.cpu, (allocated.mem+req.mem)/limit.mem)`
   (ratios → heterogeneous nodes comparable; dominant resource → never pack a node tight on the *other*
   dimension). `least-loaded` (default) minimizes it; `bin-pack` maximizes it (≤ 1.0); `spread`
   minimizes by sandbox count / label distribution; `label-affinity`/`anti-affinity`; optional
   `least-actual-load` using gossiped `actual_util`. **Ties broken by `hash(request_id ⊕ node_id)`** —
   identical ranking across coordinators (convergence, fewer admission bounces) yet spread across nodes
   (no lowest-id hotspot). (`round-robin` omitted — no honest semantics without a shared cursor.)
3. No node passes filter ⇒ **reject** (`no eligible node`).

**Workspace model:** logical name → local host path per node (operator-managed; the swarm does **not**
sync data). If only one node advertises a name, placement naturally pins there.

**Target-authoritative admission.** The coordinator forwards the provision RPC to the top candidate.
The target re-checks against its **real** local `allocated` vs `provision_limit` and workspace
availability; on success it reserves capacity, calls `Create`, persists the record, ACKs; otherwise
NACKs. Coordinator tries the next candidate; exhausted ⇒ operation fails. This tolerates stale gossip:
brief double-picks self-heal because the target enforces truth.

**Capacity accounting:** `allocated` = Σ requested cpu/mem of non-terminal local sandboxes. Reservations
are **soft (in-memory)**, held on the target from admission until `Create` resolves; `Create` runs under
a timeout and on failure/timeout releases the reservation + best-effort removes any partial sandbox +
errors the op. The **durable source of truth is `SandboxBackend.List()`** — a periodic reconcile loop
(and restart/rejoin reconcile) recomputes `allocated` from it, so any leaked reservation self-heals and
a crash can't leak (in-memory holds vanish and are rebuilt). Orphaned sandboxes (coordinator died
mid-placement) stay discoverable via `GET /sandboxes` by their stamped `idempotency_key`/labels.

**Primary signal = reservation** (deterministic, cheap). **Secondary = actual utilization** from
`exec.Stats` (observability + optional strategy). Host-OS metrics are out of scope for v1.

---

## 7. Membership, failure, drop-out & rejoin

- **Startup modes (never block on seeds; never auto-mint when a join is intended):**
  (a) **no persisted Swarm ID + no seeds** → mint a Swarm ID, run standalone;
  (b) **no persisted Swarm ID + seeds configured** → **pending-join**: serve locally as a
  standalone-of-one (it provisions immediately; `node_id` comes from the node key, independent of Swarm
  ID), **never mint**, retry seeds in the background, **adopt** the Swarm ID on first contact (then
  re-gossip its pre-join sandboxes);
  (c) **persisted Swarm ID (rejoin) + peers unreachable** → run solo with that ID (a partitioned
  member), keep retrying, reconcile on contact.
  Operators can explicitly promote a stuck pending-join node to standalone.
- `memberlist` keyring = `cluster_secret`: encrypted gossip **and** join gate in one. `join` seeds at
  startup; runtime `POST /cluster/{join,leave}`. A joiner must match the swarm's **protocol major**
  (ADR-0009) and the **Swarm ID** under the same secret (ADR-0001), else it refuses with a loud alert;
  minor version skew is tolerated (additive protos) so rolling upgrades and rejoin-after-upgrade work.
- SWIM failure detection. On a peer → suspect/`dead`: peers mark its sandboxes **`unreachable`** (a
  guess — they may be alive behind a partition) and emit alert events. Only the **owner** ever
  declares a sandbox **`lost`**, and only during reconcile when it confirms the sandbox is absent from
  its own daemon; `lost` is terminal and triggers capacity reclaim + cleanup (routing map, ports,
  clone-mode `sandbox-<name>` remote/instance dir) + a `sandbox.lost` event. **No auto-reschedule** and
  **no auto-escalation** `unreachable→lost` in v1 (an unreachable node's capacity is unusable by the
  swarm anyway); `unreachable` clears when the owner returns or an operator purges the node.
- **Run-solo:** a node that loses all peers keeps serving its own sandboxes — it is authoritative for
  them (AP).
- **Rejoin:** stable persisted `node_id`; re-advertise state; **reconcile** by diffing
  `SandboxBackend.List()` (SDK truth) against stored records to heal drift, then re-gossip; peers
  clear `lost` marks. Switching swarms = change `cluster_secret`/seeds; local sandboxes travel with
  the node.

---

## 8. Cross-node routing & event fan-out

- **Self-routing IDs (ADR-0002):** a sandbox/op ID's `<nodeID>` prefix names the owner/coordinator;
  any node forwards via the membership address table with no lookup, which also closes the post-create
  race. The gossiped `sandbox_id → owner` map (each node gossips its owned IDs) remains the authority
  and the `lost`-cleanup index. Operations are not gossiped — a poll routes to the coordinator by
  prefix. Unary and streams are relayed to the owner; unknown/forgotten id ⇒ 404, terminal `lost` ⇒
  410 Gone.
- **Stream relay semantics:** the client stream and the owner gRPC stream share a context — client
  disconnect cancels both and tears down the owner's `ExecInteractive`/`Logs` session or stats poll (no
  leaks). Backpressure is per-type: **terminal + logs are lossless** (gRPC flow control + bounded
  blocking buffer; disconnect-with-error rather than silently drop); **stats are lossy** (coalesce
  latest-wins). Owner death mid-stream ⇒ clean stream-end + sandbox → `unreachable`; reconnect is fresh
  for terminal (no TTY resume) and stats, re-tail for logs, `Last-Event-ID` best-effort for events. The
  **edge enforces the caller's role** and **propagates it in gRPC metadata**; the owner trusts the peer
  via node-key auth (ADR-0004) and re-checks (defense-in-depth).
- **Swarm-wide events:** a client on node A merges A's local event bus with peer `WatchEvents` gRPC
  streams (de-duped by event id) → one SSE/gRPC feed from any node. Membership events are already
  known locally everywhere via gossip.

---

## 9. Observability

- **Per-sandbox stats:** Metrics Collector polls `exec.Stats` on an interval (default 10 s), caches
  `last_stats`, and gossips a secondary `actual_util`. `?fresh=true` forces a probe. Never called on a
  request hot path (~200 ms in-VM probe). **`exec.Stats` is sandbox-relative** (`CPUPercent` is % of
  *that* sandbox's `Cores`, `MemTotalKB` is *that* sandbox's limit), so `actual_util` must reconstruct
  absolutes first: `actual_util.cpu = Σ_running (CPUPercent_i/100 × Cores_i) / provision_limit.cpu`;
  `actual_util.mem = Σ_running MemUsedKB_i / provision_limit.mem`. Denominator is `provision_limit` (the
  SDK exposes no host totals), keeping it comparable to the reservation `allocated` ratio. Secondary +
  slightly stale by design.
- **Logs:** `exec.Logs(path)` continuous follow → streamed (gRPC / SSE). Client specifies the file
  path; default per template.
- **Blocked egress:** Network Log Collector polls `policy.Log`, maps `VMName → sandbox_id`, dedupes to
  distinct `(host, sandbox)` pairs, and stamps `first_seen`/`last_seen`. Surfaced as a **security audit
  view** (which blocked hosts a sandbox tried to reach), **not** a rate/frequency — the SDK exposes
  presence only. Only aggregate is `distinct_count`. Handle `ErrUnexpectedFormat` by falling back to
  `ListRaw` and emitting a warning event. (Reframe when the SDK exposes timestamps/counts.)
- **Metrics:** Prometheus `/metrics` (sandbox counts by state, allocation vs limit, scheduling
  outcomes, gossip health, op latencies). **Logging:** `slog` structured.
- **Event bus + SSE firehose** (§8) for live UI and external subscribers — **best-effort** (ADR-0008):
  per-node total order, cross-node best-effort by `ts`, at-least-once resume with possible gaps; clients
  needing certainty reconcile via `GET`. The durable **audit log** (git/policy/secret) is separate.

---

## 10. Network policy management

Expose `policy.Allow/Deny/SetDefault/RemoveRule/Reset/List/Profiles` via REST + UI.
- Policy is **per-node-daemon**: per-sandbox policy routes to the owner; a **swarm-wide default** is a
  convenience that **fans `SetDefault` out to every node** (each daemon has its own default).
- `List` returns structured `[]PolicyRule`; `Profiles`/`ListRaw` pass through raw text.
- `scope`: `""` = global (per-node), sandbox name = per-sandbox.

---

## 11. Secrets & sensitive-data handling

**Cross-cutting rule.** Secret values **and** sandbox `env` values are **write-only**: never logged,
never gossiped, never persisted in `bbolt` (we store env **keys** only), masked in any `List`/UI
output, transmitted only over TLS. The node is a pass-through to the owning daemon and retains nothing.

**Mechanisms (both in v1):**
1. **Env-at-provision** (`exec.WithEnv`) — the SDK-recommended path for credentials.
2. **Experimental secret API** (`SetCustom/List/Remove`) exposed via REST + UI, **clearly labeled
   experimental**, with the safeguards above. Per-node-daemon scope (per-sandbox routes to owner;
   "global" is per-node, swarm-wide = explicit opt-in fan-out, discouraged due to exposure). Caveat
   documented in UI: `SetCustom` briefly exposes the value in host process listings.

---

## 12. Git-backed workspaces (clone mode)

Uses the **native** `sbx --clone` (`sandbox.WithClone()`): the sandbox gets a **private in-container
git clone**; the host working tree is never touched; sbx auto-configures `origin` and a host-side
`sandbox-<name>` remote; a single clone-mode sandbox can hold many branches. This gives concurrency
isolation for free — no worktrees, no `--shared` plumbing.

**v1 constraint — clone mode ⇒ exactly one (git-backed) workspace** (the clone target). A request with
`clone:true` and more than one workspace is **rejected**; mixing a clone with extra bind-mount
workspaces is deferred until verified against sbx. Non-clone sandboxes may still mount multiple
workspaces.

A git-backed workspace's `host_path` is a **bare/mirror repo** owned exclusively by the swarm (no
working tree ⇒ no merge/dirt hazards; clone-from-bare works fine). All operations on it are serialized
by a **per-workspace lock** so concurrent provisions of the same workspace don't race PRE updates
against clone-sourcing.

Lifecycle (workspace must be configured `git`-enabled with `allow_push`):
1. **PRE:** node freshens the base from upstream — refs only:
   `git -C <host_path> fetch origin '+refs/heads/*:refs/heads/*'`.
2. **PROVISION:** `Create(WithWorkspace(name), WithClone())`.
3. **AGENT WORKS:** branches/commits in its private clone; no credentials in the agent sandbox.
4. **PUBLISH** (explicit `POST /sandboxes/{id}/git/publish` **and** auto on graceful stop): node runs
   `git -C <host_path> fetch sandbox-<name> <branch>` (local, no creds) then
   `git -C <host_path> push origin <branch>` (upstream, creds).

**Customizable, per-repo:** `pre_steps`/`publish_steps` are operator-defined **argv-array** pipelines in
each node's **local workspace config** (ADR-0003), run **without a shell** — covering LFS, submodules,
tags, sparse-checkout, etc. The PRE refs-only fetch and the default fetch-from-sandbox + push shown
above are the built-in defaults a workspace inherits unless it overrides them. The API/swarm protocol
**never carries commands**: a request references the workspace by name and supplies only validated
*values* (`{branch}`, `{base_ref}`, `{remote}`, `{sandbox_remote}`, `{commit_message}`) bound as
discrete argv. An `exec_allowlist` (default `git`, `git-lfs`) is defense-in-depth.

**Security:** no shell — argv steps only; the node builds `argv`; request-supplied values validated
(reject leading `-`, control chars). No API-key holder or peer can induce arbitrary execution on a node
(ADR-0003). **Credentialed upstream ops run host-side on the node** using a per-workspace
credential from config (secret-managed, scoped to the remote, not gossiped/logged). Every git op is
audited (workspace/branch/ref/outcome — never secrets) and gated by `allow_push`. (Native `WithClone()`
clones are self-contained inside the sandbox VM — no object dependency on the host base — so no gc/prune
coordination is required.)

---

## 13. Templates catalog

Templates are an **advertised node capability + scheduler constraint**, exactly like workspaces (no
propagation in v1). Each node gossips the template names it holds (via per-host `sandbox.SaveTemplate`
/ `template.List`); a request with `WithTemplate(name)` is **filtered** to nodes that advertise it. The
"catalog" is just the gossiped union of names + metadata. Operators are responsible for getting a
template onto the nodes that should run it. **Recipe-based auto-build / image shipping is deferred to
vNext.**

---

## 14. Web console (Nuxt UI 4)

Nuxt 4 + `@nuxt/ui` v4, built as a **static SPA** (`ssr: false`) and **embedded via `embed.FS`** into
the binary; served from the same TLS port. Talks to the node via REST (unary) + SSE (streams) + the
WS terminal bridge.
- **Overview:** live **Vue Flow** topology (nodes + sandboxes, load bars, placement animation driven
  by the events firehose); stat cards (allocated vs limit, sandbox counts, blocked-egress, recent
  operations).
- **Sandboxes:** table + drill-down drawer — Stats (charts), Terminal (xterm.js), Network (blocked +
  policy editor), Secrets (masked), Files, Ports, Git (branch/publish).
- **Nodes:** limits/util, workspaces, labels, cordon/drain, default policy.
- **Templates, Network/Security, Operations, Settings.**

Topology graph via Vue Flow; terminal via xterm.js; charts via a Vue charts lib (ECharts/unovis).

---

## 15. Security model (consolidated)

- TLS on the single port; two auth paths resolving to a key + role (ADR-0006): **`Authorization:
  Bearer`** for API clients, **httpOnly session cookie** (via `POST /auth/session`) for the browser
  console — because `EventSource` can't set headers. CSRF protection (double-submit / custom header) on
  cookie mutations. Two v1 roles: **`admin`** (full) and **`read-only`** (GET + SSE subscribe only; no
  mutations, exec/terminal, publish, or secret reads). Finer per-resource/tenant RBAC deferred.
- **Node↔node trust (ADR-0004):** TLS for encryption; `cluster_secret` gates gossip + bootstraps
  trust; per-node self-generated key (`node_id = hash(pubkey)`) pinned TOFU via gossip for
  spoof-resistant identity; revocation via gossiped key denylist. CA-issued mTLS deferred to vNext
  behind the gRPC-auth interface.
- Sensitive-data rule (§11): write-only secrets/env, masked, never logged/gossiped/persisted.
- Git: typed verbs, validated argv, host-side credentials scoped per workspace, audited, `allow_push`
  gate; agent sandbox credential-free.
- Audit log for credentialed/sensitive actions.

---

## 16. Configuration & project layout

- Config via file + env + flags (flags > env > file). `node_id` derived from the persisted node key on
  first run.
- **Hot-reload** via SIGHUP + admin-only `POST /v1/admin/reload` (validate-before-apply, then re-gossip
  changed advertised state) for the mutable subset: `workspaces`, `provision_limits`, default strategy,
  `api_keys`, `labels`, git pre/publish pipelines, poll/reaper settings, TLS server cert. **Restart
  required** (reload warns + ignores) for: node keypair/`node_id`, bind addresses,
  `cluster_secret`/swarm membership, persistence path.
- **Config changes never evict running sandboxes.** Removing a workspace or lowering `provision_limits`
  affects only *future* admissions (a lowered limit can leave the node transiently over-committed; it
  admits nothing new until `allocated` drops below the new ceiling). Revoking an API key invalidates
  that key and its sessions immediately.
- **Persistence (`bbolt`):** buckets `meta` (`schema_version`, **node key**, `node_id`,
  `swarm_id`/`swarm_name`), `sandboxes`, `operations` (TTL'd), `idempotency` (TTL'd), `blocked_egress`,
  `audit` (append-only, retained). **Derived/in-memory only** (rebuilt from `SandboxBackend.List()` +
  gossip on restart): `allocated`, soft reservations, the `sandbox_id→owner` routing map, peer state,
  the event replay buffer. Migrations run **ordered forward** by `schema_version`; the node **refuses to
  start on a newer-than-binary schema** (downgrade guard, pairs with ADR-0009's same-major rule).
- **Node key is critical, irreplaceable state.** Since IDs are self-routing (`node_id = hash(pubkey)` —
  ADR-0002/0004), losing the key yields a new identity and **orphans every sandbox this node owns**
  (their `<node_id>.` prefix stops resolving → they age `unreachable→lost`). Persist it durably, back it
  up / store it separately, and treat key loss as **node replacement**, not a restart.
- Standard Go layout: `cmd/sbx-swarm-node`, `internal/{config,api,grpc,gateway,sse,scheduler,
  membership,sandboxd (SandboxBackend + SDK adapter + fake),store,events,metrics,netlog,git,reaper,
  templates,proxy}`, `proto/`, `web/` (Nuxt app + `embed.go`), `docs/`.

---

## 17. Testing strategy

- **Unit (table-driven, `testify` + `goleak`):** scheduler filter/score, config precedence, git
  `argv` builder + name validation, `coltable`-style parsing fallbacks, state store, event bus.
- **`SandboxBackend` fake** powers fast, deterministic component tests without a real daemon.
- **Multi-node integration:** N in-process nodes over loopback gossip — placement, target-side
  admission, cross-node forwarding, drop-out/rejoin reconcile, swarm-wide event merge.
- **Tagged SDK integration:** behind a build tag against a real `sandboxd`/`sbx` for the adapter and
  git lifecycle.
- **E2E smoke:** scripts launching real nodes + daemon.

---

## 18. v1 scope & milestones

**In v1:** standalone+swarm node; constraint scheduler + strategies + admission; gRPC + gateway +
SSE + WS terminal on one port; sandbox lifecycle/exec/ports/files/template-snapshot/labels/TTL+idle;
stats + logs + Prometheus + event bus; blocked-egress visibility + policy management; secrets
(env + experimental API, safeguarded); git clone-mode lifecycle; template catalog; failure
detect/mark-(un)reachable/lost + cordon/drain; auth (bearer + cookie sessions, two roles) + TLS +
node-key trust; embedded Nuxt UI 4 console.

**Deferred:** multi-tenancy/RBAC, webhooks, pre-warmed pools, scheduled provisioning, OTel,
published CLI/generated SDKs, host-OS metrics, CRDT registry, auto-reschedule, raw host exec,
workspace data replication, **template propagation (recipe-build / image-ship / registry)**,
**CA-issued node mTLS**.

**Milestones (each → its own implementation plan):**
1. **Standalone foundation** — layout, config, `bbolt`, `SandboxBackend` (+adapter+fake), one-port
   gRPC+gateway server, sandbox CRUD/exec/ports/files, operations, **in-process event bus**, auth/TLS,
   health/metrics.
2. **Observability** — stats poll+cache+stream (SSE/gRPC), logs follow, network-blocked collector,
   Prometheus.
3. **Network policy + secrets** — structured policy mgmt + fan-out, secret API + env safeguards.
4. **Swarm** — memberlist gossip, node-state delegate, `sandbox_id→owner` index, cross-node gRPC
   forwarding + stream relay, **peer event fan-out (swarm-wide SSE)**, join/leave, rejoin reconcile,
   failure detect, cordon/drain.
5. **Scheduling** — filter→score (workspace + **template** + capacity + label constraints),
   strategies, admission/retry, capacity accounting, template advertise/filter.
6. **Git workspaces** — clone-mode pre/publish lifecycle, host-side creds, audit.
7. **TTL/idle reaper.**
8. **Nuxt UI 4 console** — embed, overview/topology, sandbox drill-down, nodes, templates,
   network/security, operations, settings, terminal bridge.

---

## 19. Open questions / risks

- **`policy.Log` cost & dedup** at scale (one-shot, daemon-wide, no timestamps) — tune poll interval;
  watch for table-format drift (`ErrUnexpectedFormat`). Confirm against a live daemon whether the list
  is **cumulative** (since startup) or **rolling** — `last_seen` is only informative if rolling.
  Attempt-frequency awaits richer SDK support.
- **`exec.Stats` overhead** (~200 ms in-VM probe, running-only) — interval + caching; never on hot
  path.
- **Stale-gossip placement races** — mitigated by target admission + retry; quantify retry bounds.
- **Secret exposure** via `SetCustom` process-listing leak — labeled experimental; prefer env.
- **Template propagation mechanism** (ship image vs build recipe) — to be detailed in M7.
- **`Last-Event-ID` resume** across a distributed merge is best-effort (bounded per-node buffer).
- **Stats poller backpressure** — many running sandboxes × ~200 ms `exec.Stats` can exceed the poll
  interval; use a bounded-concurrency pool, per-sandbox jitter, skip-if-in-flight, error backoff.
- **SSE fan-out at scale** — a node maintains one shared `WatchEvents` stream per peer (N−1 per node),
  fanned to local SSE subscribers; fine for small/medium swarms, a gossiped event digest is the
  large-swarm escape hatch.
- **`ExecDetached` completion fidelity** — verify against the SDK whether it surfaces an **exit code**
  or only liveness; agent-run op terminal state (`done` vs `error`) depends on it. If liveness-only,
  degrade to "exited, code unknown" and document.
