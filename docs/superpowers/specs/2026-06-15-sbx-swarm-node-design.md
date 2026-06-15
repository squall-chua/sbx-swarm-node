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
| **Membership** | `hashicorp/memberlist`: gossip, SWIM failure detection, encrypted wire + join secret; broadcasts node state via a delegate |
| **Metrics Collector** | Polls `exec.Stats` per running sandbox; caches; aggregates `actual_util` |
| **Network Log Collector** | Polls `policy.Log`; diffs; accumulates blocked-egress events |
| **Event Bus** | In-process pub/sub of all domain events; backs SSE + gRPC `WatchEvents` |
| **Git Lifecycle** | Pre-fetch / publish for clone-mode workspaces; host-side credentialed git |
| **Reaper** | Enforces TTL + idle timeout; cleans up |
| **State Store** | `bbolt` — node identity, sandbox records, operations, blocked-egress, audit |
| **Peer Client** | gRPC client to peers: provision RPC, request/stream forwarding, `WatchEvents` |

---

## 4. Data model

**Node config** (file + env + flags; precedence flags > env > file):
`node_id` (generated once, persisted), `node_name`, REST/gRPC bind addr (one TLS port), gossip bind
addr, `join` seed peers (empty ⇒ standalone), `cluster_secret` (memberlist keyring key),
`provision_limits {cpu_cores, memory_bytes}`, `workspaces [{name, host_path, read_only, git?{remote,
default_branch, auth_secret_ref, allow_push}}]`, node `labels`, `api_keys`, TLS cert/key + CA (node
mTLS), default strategy, persistence path, poll intervals (stats, network), reaper config.

**Gossiped node state** (memberlist delegate; small): `node_id`, `node_name`, **REST address**
(for proxy/forward), liveness, `provision_limits`, `allocated {cpu, mem}`, `actual_util {cpu, mem}`
(secondary), available **workspace names** (+ ro/rw, git-enabled), labels, **owned sandbox IDs**
(for `sandbox_id → owner` routing map), `blocked_egress_count`, `cordoned` flag, state version.

**Sandbox record** (authoritative on owner, persisted): swarm `id`, `owner_node_id`, SDK ref/name,
spec `{template, agent, cpu, memory, workspaces[], clone bool, env_keys[] (values NOT stored), labels,
ttl, idle_timeout}`, `status` (`requested→placing→provisioning→running→stopped→failed→lost`),
published ports, `last_stats {usage, sampled_at}`, git `{branch, last_publish}`, timestamps.

**Operation record** (async): `op_id`, type (`provision|stop|remove|publish|…`), `state`
(`pending→running→done|error`), target node, sandbox id, error, timestamps.

**Blocked-egress event** (per node, persisted): `{host, sandbox_id, node_id, first_seen, last_seen,
count}` — synthesized timestamps (SDK provides none), attributed via `VMName`.

**Domain event** (event bus / SSE): `{id (per-node monotonic + node_id), ts, type, node_id,
sandbox_id?, payload}`. Types: membership (up/suspect/down), sandbox lifecycle, scheduling
(candidates/winner/nack/retry), operation, policy/secret/git-publish (audit), cordon/drain, template
propagate, reconcile.

**Audit record** (persisted): credentialed/sensitive actions — git publish, policy change, secret
set/remove — `{actor, action, target, outcome, ts}`; **never** records secret values.

---

## 5. Transport & API

**One TLS port, multiplexed.** A single listener dispatches: HTTP/2 + `content-type:
application/grpc` → gRPC server; other HTTP → grpc-gateway (REST/JSON, **unary only**) and SSE/WS
handlers; unmatched paths → embedded SPA static file server. TLS with ALPN negotiates h2/http1.1
(h2c in dev).

**Node↔node = native gRPC** (mTLS via cluster CA): provision RPC, transparent request/stream
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

- **Sandboxes:** `POST /sandboxes` (→`202 {op_id}`), `GET /sandboxes` (filter label/node/status/
  workspace), `GET|DELETE /sandboxes/{id}`, `POST /sandboxes/{id}/{start|stop}`,
  `POST /sandboxes/{id}/exec`, `GET /sandboxes/{id}/stats?fresh=`, ports (`GET/POST/DELETE`), files
  (CopyTo/From), `POST /sandboxes/{id}/template`, `POST /sandboxes/{id}/git/publish`.
- **Sandbox streams:** SSE `…/stats`, `…/logs`, `…/network`; WS `…/terminal`.
- **Network/secrets (per-sandbox):** `GET …/network/blocked`, `GET/PUT …/policy`,
  `GET/PUT/DELETE …/secrets` (write-only, masked).
- **Templates (swarm catalog):** `GET/POST /templates`, `GET/DELETE /templates/{name}`.
- **Nodes/cluster:** `GET /nodes`, `GET /nodes/{id}`, `POST /nodes/{id}/{cordon|uncordon|drain}`,
  node-level policy/secrets, `GET /cluster`, `PUT /cluster/policy/default` (fan-out),
  `POST /cluster/{join|leave}`, `GET /events` (SSE firehose).
- **Operations:** `GET /operations[/{id}]`; SSE via the events firehose (filter by type).
- **Ops:** `GET /healthz`, `/readyz`, `/metrics` (Prometheus). OpenAPI spec generated from gateway
  annotations.

---

## 6. Scheduling — filter → score

**Constraint-based placement** over the gossiped view:

1. **Filter (hard predicates):** keep nodes that (a) advertise **every** requested workspace by name,
   (b) have remaining capacity within `provision_limit` for requested cpu/mem, (c) are not
   `cordoned`, (d) satisfy label affinity/anti-affinity.
2. **Score (strategy, tiebreak):** `least-loaded` (default, by reservation), `bin-pack`, `spread`,
   `round-robin`, `label-affinity`; optional `least-actual-load` using gossiped `actual_util`.
3. No node passes filter ⇒ **reject** (`no eligible node`).

**Workspace model:** logical name → local host path per node (operator-managed; the swarm does **not**
sync data). If only one node advertises a name, placement naturally pins there.

**Target-authoritative admission.** The coordinator forwards the provision RPC to the top candidate.
The target re-checks against its **real** local `allocated` vs `provision_limit` and workspace
availability; on success it reserves capacity, calls `Create`, persists the record, ACKs; otherwise
NACKs. Coordinator tries the next candidate; exhausted ⇒ operation fails. This tolerates stale gossip:
brief double-picks self-heal because the target enforces truth.

**Capacity accounting:** `allocated` = Σ requested cpu/mem of non-terminal local sandboxes; reserve
on provision, release on stop/remove/fail.

**Primary signal = reservation** (deterministic, cheap). **Secondary = actual utilization** from
`exec.Stats` (observability + optional strategy). Host-OS metrics are out of scope for v1.

---

## 7. Membership, failure, drop-out & rejoin

- `memberlist` keyring = `cluster_secret`: encrypted gossip **and** join gate in one. `join` seeds at
  startup; runtime `POST /cluster/{join,leave}`.
- SWIM failure detection. On a peer → `dead`: mark its sandboxes `lost`, emit alert events. **No
  auto-reschedule** in v1 (sandbox in-VM state and node-local workspace data can't be recovered
  remotely).
- **Run-solo:** a node that loses all peers keeps serving its own sandboxes — it is authoritative for
  them (AP).
- **Rejoin:** stable persisted `node_id`; re-advertise state; **reconcile** by diffing
  `SandboxBackend.List()` (SDK truth) against stored records to heal drift, then re-gossip; peers
  clear `lost` marks. Switching swarms = change `cluster_secret`/seeds; local sandboxes travel with
  the node.

---

## 8. Cross-node routing & event fan-out

- Each node gossips its **owned sandbox IDs**; peers maintain a `sandbox_id → owner` map. A request
  for a non-local sandbox is forwarded via the gRPC Peer Client to the owner (unary and streams
  relayed). On unknown id ⇒ 404.
- **Swarm-wide events:** a client on node A merges A's local event bus with peer `WatchEvents` gRPC
  streams (de-duped by event id) → one SSE/gRPC feed from any node. Membership events are already
  known locally everywhere via gossip.

---

## 9. Observability

- **Per-sandbox stats:** Metrics Collector polls `exec.Stats` on an interval (default 10 s), caches
  `last_stats`, aggregates `actual_util`, gossips the secondary signal. `?fresh=true` forces a probe.
  Never called on a request hot path (~200 ms in-VM probe).
- **Logs:** `exec.Logs(path)` continuous follow → streamed (gRPC / SSE). Client specifies the file
  path; default per template.
- **Blocked egress:** Network Log Collector polls `policy.Log`, diffs against the last snapshot,
  accumulates events with synthesized timestamps + counts, attributed by `VMName`. Handle
  `ErrUnexpectedFormat` by falling back to `ListRaw` and emitting a warning event.
- **Metrics:** Prometheus `/metrics` (sandbox counts by state, allocation vs limit, scheduling
  outcomes, gossip health, op latencies). **Logging:** `slog` structured.
- **Event bus + SSE firehose** (§8) for live UI and external subscribers.

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

Lifecycle (workspace must be configured `git`-enabled with `allow_push`):
1. **PRE (optional, declarative):** node freshens the host workspace repo from upstream
   (`git -C <host_path> fetch origin && merge --ff-only`).
2. **PROVISION:** `Create(WithWorkspace(name), WithClone())`.
3. **AGENT WORKS:** branches/commits in its private clone; no credentials in the agent sandbox.
4. **PUBLISH** (explicit `POST /sandboxes/{id}/git/publish` **and** auto on graceful stop): node runs
   `git -C <host_path> fetch sandbox-<name> <branch>` (local, no creds) then
   `git -C <host_path> push origin <branch>` (upstream, creds).

**Security:** no shell — typed verbs only; the node builds `argv`; branch/ref names validated (reject
leading `-`, etc.). **Credentialed upstream ops run host-side on the node** using a per-workspace
credential from config (secret-managed, scoped to the remote, not gossiped/logged). Every git op is
audited (workspace/branch/ref/outcome — never secrets) and gated by `allow_push`. The Reaper must not
`gc`/prune a base repo while shared clones depend on it.

---

## 13. Templates catalog

A **swarm-wide template catalog** layered over per-host `sandbox.SaveTemplate` / `template.List/Load`.
Register a template once; the node propagates it (or its build recipe) so any eligible node can
provision from the same base. Catalog entries gossiped (names/metadata); contents propagated on demand
to the placement target before `Create`.

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

- TLS on the single port; **bearer/API-key auth** on all `/v1` endpoints (except health).
- **Node↔node mTLS** via a cluster CA; gossip encrypted + gated by `cluster_secret`.
- Sensitive-data rule (§11): write-only secrets/env, masked, never logged/gossiped/persisted.
- Git: typed verbs, validated argv, host-side credentials scoped per workspace, audited, `allow_push`
  gate; agent sandbox credential-free.
- Audit log for credentialed/sensitive actions.

---

## 16. Configuration & project layout

- Config via file + env + flags (flags > env > file). Stable `node_id` persisted on first run.
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
detect/mark-lost/alert + cordon/drain; auth + TLS + node mTLS; embedded Nuxt UI 4 console.

**Deferred:** multi-tenancy/RBAC, webhooks, pre-warmed pools, scheduled provisioning, OTel,
published CLI/generated SDKs, host-OS metrics, CRDT registry, auto-reschedule, raw host exec,
workspace data replication.

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
5. **Scheduling** — filter→score, strategies, admission/retry, capacity accounting.
6. **Git workspaces** — clone-mode pre/publish lifecycle, host-side creds, audit.
7. **Templates catalog + propagation; TTL/idle reaper.**
8. **Nuxt UI 4 console** — embed, overview/topology, sandbox drill-down, nodes, templates,
   network/security, operations, settings, terminal bridge.

---

## 19. Open questions / risks

- **`policy.Log` cost & dedup** at scale (one-shot, daemon-wide, no timestamps) — tune poll interval;
  watch for table-format drift (`ErrUnexpectedFormat`).
- **`exec.Stats` overhead** (~200 ms in-VM probe, running-only) — interval + caching; never on hot
  path.
- **Stale-gossip placement races** — mitigated by target admission + retry; quantify retry bounds.
- **Secret exposure** via `SetCustom` process-listing leak — labeled experimental; prefer env.
- **Template propagation mechanism** (ship image vs build recipe) — to be detailed in M7.
- **`Last-Event-ID` resume** across a distributed merge is best-effort (bounded per-node buffer).
