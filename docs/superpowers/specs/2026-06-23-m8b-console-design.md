# sbx-swarm-node M8b — Nuxt UI 4 Swarm Console (design)

> **Context:** M8 was split. **M8a** (merged `@17ba536`) shipped the five backend
> capabilities a console needs. **M8b** (this spec) is the Nuxt 4 + @nuxt/ui v4 SPA that
> consumes the *now-complete* API, embedded in and served by the node. This spec
> supersedes the assumptions in the stale pre-split plan
> `docs/superpowers/plans/2026-06-15-sbx-swarm-node-m8-console.md` — every endpoint here is
> reconciled against the real shipped proto + handlers.

**Goal:** A single-page console served by the node that lets an operator see the swarm
(nodes, sandboxes, operations, templates) and drive it (provision, terminal, stats/logs,
cordon/drain/revoke, policy/secrets, git publish) — over the same-origin authed API.

## API reconciliation (stale plan → reality)

Findings that shaped this design; the stale plan assumed several of these wrong.

**Confirmed correct (kept):**
- SPA → `web/dist` → `web.FS()` embed, served open at `/`; authed API under `/v1/`.
- Login: `POST /v1/auth/session` w/ `Authorization: Bearer <key>`, `credentials:include`
  → 204 sets `sbx_session` (httpOnly, Secure, SameSite=Strict) + `sbx_csrf`
  (JS-readable, Secure, SameSite=Strict).
- CSRF (double-submit): read `sbx_csrf` cookie, send `X-CSRF-Token` on cookie-auth
  mutations (`internal/auth/auth.go`).
- Terminal WS `GET /v1/sandboxes/{id}/terminal`: **binary** frames = I/O; **text** frame
  `{"type":"resize","cols":N,"rows":M}` = resize; same-origin enforced (ADR-0017);
  cookie-only for browsers.
- Files API still deferred → that tab is a stub.

**Wrong/stale (fixed here):**
1. **SSE is named-event, not `onmessage`.** The server writes `id:` / `event:<type>` /
   `data:<json>` (`internal/apiserver/sse.go` `writeSSE`). Because every frame has an
   `event:` field, the browser's `EventSource.onmessage` **never fires** — clients must
   `addEventListener(<type>, …)` per type.
2. **Event type filter is EXACT match, not prefix** (`internal/events/event.go` `matches`).
   The stale `types:['sandbox','operation','membership','scheduling']` matches nothing.
   Real emitted types:
   - `sandbox.created` (payload `{status}`), `sandbox.<status>` (e.g. `.running`/`.stopped`,
     nil payload), `sandbox.deleted`, `sandbox.lost`, `sandbox.published` /
     `sandbox.publish_failed` (payload `{branch}`).
   - `operation.pending|running|done|error` (payload `{op_id,type}`).
   - **No `membership`/`scheduling` events exist** → no event-backed swarm-map "pulse".
3. **SSE payloads are thin pokes, not deltas.** Only `id`/`event`/`data` cross the wire
   (envelope `node_id`/`sandbox_id` are dropped unless inside the payload), and per ADR-0008
   the firehose is best-effort, not a source of truth. → Live views treat an event as
   "something in this family changed, refetch", never as a precise delta.
4. **Cordon/drain path.** Not `POST /v1/nodes/{id}/cordon`. Real: `POST /v1/node/cordon`
   (+`/uncordon`, `/drain`) with `{node_id}` body, empty = self (ADR-0018, `node.proto`).

**Better than the stale plan knew (free wins):**
5. **Stats can stream.** `GET /v1/sandboxes/{id}/stats` with `Accept: text/event-stream`
   → SSE `event: stats` every 2s (`observe_sse.go`, active when observe collectors are
   wired); a plain `GET` returns unary JSON. `EventSource` sets that Accept automatically,
   so the Stats tab streams. (The collectors are wired unconditionally in `node.go:113`, so
   the stream is always present in a real node; `EventSource` auto-reconnect covers blips.)
6. **Per-sandbox logs SSE exists.** `GET /v1/sandboxes/{id}/logs` → SSE `event: log`.

**Other shape notes (from proto + gateway):**
- gateway marshals snake_case with `EmitUnpopulated:true` → every JSON field is always
  present (clean TS typing).
- `Stats{cores,cpu_percent,mem_total_kb,mem_used_kb}` (no disk).
- `Exec` returns `bytes` stdout/stderr (base64 over JSON) — one-shot; the interactive
  path is the terminal WS.
- Policy/secrets are **scoped**: `/v1/sandboxes/{scope}/…`, `scope=""` = node-global,
  `scope=<id>` = per-sandbox. `SetPolicy` is allow/deny **per host** (not free-form), and is
  **add-only** — there is no remove-policy-rule RPC (only `DeleteSecret` for secrets).
  Secrets: `custom` (host+env) + `stored` (names); **value write-only** (never returned).
- `NodeSummary` carries `actual_cpu`/`actual_mem` (gossiped util) and per-node
  `templates` names; `draining` is meaningful for self only (peers always `false`).
- Ports: `PublishPort` + `ListPorts` only — **no UnpublishPort** in the API.
- **No server-side logout endpoint** exists.
- **No endpoint exposes the caller's role** (session returns 204; `GetNodeInfo` returns
  node info, not *your* role) → M8b adds a `role` field to `NodeInfo` (see Components).

## Architecture

Nuxt 4 SPA (`ssr:false`), built to `web/dist`, embedded via the existing `web.FS()` and
served open at `/`. Same-origin throughout: REST for unary calls, `EventSource` for live
data (cookie auth — `EventSource` can't set headers, hence ADR-0006), one WebSocket for the
terminal. The **testable core** is three composables (`useApi`, `useEvents`, `useTerminal`)
covered by Vitest; pages are built with @nuxt/ui v4 (via the `nuxt-ui` skill) and styled per
the `ui-ux-pro-max` design guidance. Shared reactive state via Nuxt `useState` (no Pinia).

**Live-data model — "event poke → refetch" (the architectural heart):** a single
**app-wide, unfiltered** `/v1/events` subscription (in `useSwarm`). A frame can't identify
*which* sandbox changed (the SSE frame drops the envelope and `sandbox.<status>` payloads
are nil — finding #3), so events are treated as **coarse pokes**: any `sandbox.*` →
debounced refetch of the sandbox list; `operation.*` → refetch operations; the open drawer
refetches its own sandbox on any `sandbox.*`. A 20–30s periodic refetch backstops missed
events; manual refresh everywhere. Chosen because events are thin best-effort pokes
(findings #2/#3, ADR-0008) — applying them as deltas would fight the backend. Per-sandbox
streams (stats/logs/terminal) open only for the active drawer tab and close on
tab-switch/drawer-close. (Upgrade path if coarse refetch proves chatty: open a second
`?sandbox=<id>` firehose per drawer for precisely-scoped pokes — `useEvents` already takes
`types`/`sandbox`.)

## Components

### Backend prerequisite (one tiny addition)
- **`role` on `NodeInfo`** — `GetNodeInfo` populates the caller's role from
  `principalFromContext(ctx)` (crosses the loopback as the signed `x-sbx-authz` role). The
  console calls `GET /v1/node` on load and uses it to hide/disable admin affordances for
  `read-only` keys. The server remains the real gate (role-gate + authz). `GetNodeInfo` is
  already read-only-classified, so no authz-drift change. One proto field + regen + ~3 lines.

### Composables (Vitest-covered core)
- `app/composables/useApi.ts` — typed REST client. `credentials:include`; `GET/HEAD` send
  no CSRF; mutations send `X-CSRF-Token` = `sbx_csrf` cookie; `401` **or `403` csrf-fail**
  (the CSRF cookie expires with the session) → clear `loggedIn`, redirect `/login`; `204` →
  null. `{get,post,put,del}`.
- `app/composables/useEvents.ts` — `createEvents(base, ES)(opts,onEvent) → unsubscribe`.
  Builds `/v1/events?types=<exact csv>&sandbox=<id>`; registers `addEventListener(type, …)`
  **per type** (never `onmessage`); `withCredentials`; unsubscribe closes the source.
- `app/composables/useTerminal.ts` — `createTerminal(wsUrl, WS)`: WS binary → xterm write,
  xterm data → WS binary, resize → text `{"type":"resize",cols,rows}`; close on unmount.
- `app/composables/useSwarm.ts` — shared `useState` for `nodes`/`sandboxes`/`operations`;
  owns the single app-wide unfiltered `useEvents` subscription (coarse poke → debounced
  refetch of the affected list) + periodic backstop; exposes refresh fns. One firehose for
  the whole app.

### Shell & routing
- `app/app.vue` (`UApp` root), `app/layouts/default.vue` (@nuxt/ui dashboard sidebar nav).
- `app/middleware/auth.global.ts` — no `loggedIn` flag & route≠`/login` → redirect.
- `app/pages/login.vue` — bearer-key → session-cookie exchange; error toast on 401.

### Views (full console)
- `pages/index.vue` **Overview** — stat cards (node count; sandboxes by status; Σ alloc/limit;
  blocked-egress distinct; recent operations) + the **swarm map: a responsive node-card grid**
  (load bars from actual/alloc vs limit; sandbox chips grouped by owner; cordoned/draining
  badges). No edges — the swarm is a flat mesh. Live via `useSwarm`.
- `pages/sandboxes/index.vue` **Sandboxes** — filterable @nuxt/ui table (status/label) +
  Provision modal + row → `components/SandboxDrawer.vue`. **Provision modal** (`POST
  /v1/sandboxes` with an `Idempotency-Key` header) is **tiered**: visible fields = agent,
  template (dropdown from the catalog union), cpus, memory, disk_gb, workspaces (multi-select
  from advertised names + per-mount `read_only`); a collapsible **Advanced** = clone+branch,
  strategy (the four known strategies), env (k/v editor), labels (k/v editor),
  node_affinity / node_anti_affinity (k/v editors). Caveat: the form can't tell which
  workspaces are git-backed (names only), so clone is best-effort — the server rejects an
  invalid clone and the modal surfaces the error.
- `components/SandboxDrawer.vue` tabs:
  - *Info/Actions* — id/owner_node/status/branch/last_publish/labels; start/stop/delete/
    keepalive; ports list + publish (no unpublish).
  - *Terminal* — `components/Terminal.vue` (xterm) over WS via `useTerminal`. While
    attached, POST `…/keepalive` on a ~60s interval (cleared on tab/drawer close) so an
    idle interactive session isn't idle-stopped mid-use when idle-stop is enabled
    (the server bumps Activity only once at attach — finding).
  - *Stats* — `components/Sparkline.vue` fed by `EventSource('…/stats')` (`event: stats`,
    2s). No GET-poll fallback: observe is always wired (`node.go:113`) so the stream is
    always available, and `EventSource` auto-reconnects on transient blips.
  - *Logs* — live `EventSource('…/logs')` (`event: log`) into a scrollback pane.
  - *Network* — blocked-egress table (`GET …/network/blocked`: host/first_seen/last_seen +
    distinct_count) + per-sandbox policy (`GET/PUT …/policy`, scope=id, allow/deny host).
    **Policy is view + add only** — there is no remove-rule RPC (`PolicyService` has no
    delete-policy method), so the editor adds allow/deny rules but can't delete them.
  - *Secrets* — `GET/PUT/DELETE …/secrets` (scope=id); masked list, value write-only.
  - *Git* — branch + Publish (`POST …/git/publish`); shown only when the sandbox has a
    recorded branch (clone-mode).
  - *Files* — stub ("coming soon"; deferred API).
- `pages/nodes.vue` **Nodes** — cards from `GET /v1/nodes`: limit/alloc/actual util bars,
  labels/capabilities/workspaces/templates; cordon/uncordon/drain (`POST /v1/node/{…}`
  `{node_id}`); revoke (`POST /v1/node/revoke`) + revoked list (`GET /v1/node/revoked`).
  Admin actions hidden/disabled for `read-only` (via `NodeInfo.role`); server is the real gate.
- `pages/templates.vue` **Templates** — `GET /v1/templates` (local rich:
  repository/tag/id/agent/created_at) + "which nodes hold it" derived from
  `GET /v1/nodes[].templates`.
- `pages/network.vue` **Network / Security** — node-global (scope `""`) policy + secrets
  management. (Per-sandbox blocked egress lives in the drawer; no swarm-global blocked
  endpoint exists.)
- `pages/operations.vue` **Operations** — `GET /v1/operations` history (newest-first,
  `?limit`) + live `operation.*` overlay.
- `pages/settings.vue` **Settings** — `GET /v1/node` self info (read-only) + logout
  (client-side flag clear + redirect; no server logout endpoint).

### Deliberate simplifications vs the stale plan (approved)
- **Swarm map is a node-card grid, not Vue Flow.** The swarm is a flat leaderless mesh —
  no hierarchy/edges to draw, and no `scheduling`/`membership` events to animate (finding
  #2). Drops `@vue-flow/core`.
- **Stats chart is a hand-rolled SVG sparkline, not ECharts.** A cpu%/mem% line from a 2s
  tick is a small `<polyline>` component, zero dep. Drops `echarts`/`vue-echarts` (~1MB);
  add `uPlot` later only if axes/zoom are needed.

**Net new runtime deps:** `@nuxt/ui` + `@xterm/xterm` (+ `@xterm/addon-fit`). Dev/test:
`vitest`, `@vue/test-utils`, `@nuxt/test-utils`, `happy-dom`.

## Build / dev wiring

- `web/nuxt.config.ts`: `ssr:false`, `modules:['@nuxt/ui']`, static output, `apiBase:'/'`
  (same-origin). `nuxt dev` proxies `/v1`, `/healthz`, `/metrics` to a running node
  (TLS-insecure for localhost self-signed cert) for hot-reload development.
- `web/scripts/build.sh`: `nuxi generate` → copy `.output/public/*` → `web/dist/`.
- **`web/dist` is gitignored** except a committed placeholder `index.html`, so
  `//go:embed dist` and `go test ./...` always compile without a frontend build. The real
  build is a documented pre-`go build` step (Makefile target + README); from-source binaries
  run `web/scripts/build.sh` once.
- `internal/apiserver/server_embed_test.go`: `fs.Stat(web.FS(),"index.html")` smoke — the
  Go side of the embed boundary.

## Testing (bar: unit core + component + manual smoke)

- **Vitest unit:** `useApi` (CSRF header from cookie on mutations; `credentials:include`;
  GET sends no CSRF; 401 path), `useEvents` (URL carries exact `types`/`sandbox`; registers
  per-type listeners not `onmessage`; unsubscribe closes), `useTerminal` (forwards binary
  both ways; emits resize JSON; closes on unmount).
- **Component (@vue/test-utils + happy-dom):** Overview (mock `useApi` → renders node cards
  + stat cards; an injected `sandbox.created` event triggers a refetch), SandboxDrawer
  (Provision posts with `Idempotency-Key`; tabs render against a mocked API).
- **Go:** `server_embed_test.go` passes against the placeholder or a real build.
- **Manual smoke:** build → run node (`backend:sdk`, live daemon) → login → provision →
  watch it appear → open terminal, stats, logs. No browser e2e (no browser CI today).

## Task outline (formalized later by writing-plans)

0. Backend prerequisite: add `role` to `NodeInfo`, populate from the principal (TDD: a
   `GetNodeInfo` test asserting the role round-trips for admin vs read-only).
1. Scaffold Nuxt 4 SPA + embed/dev wiring (gitignore dist + placeholder, `build.sh`,
   Makefile target, `nuxt dev` proxy, Go embed test).
2. `useApi` + `useEvents` + `useTerminal` (Vitest, TDD).
3. Auth: `login.vue` + `auth.global.ts` + 401 handling.
4. App shell (dashboard layout/nav) + `useSwarm` live store (poke → refetch).
5. Overview: stat cards + swarm-map card-grid (live).
6. Sandboxes: list + Provision modal + drawer Info/Actions/ports.
7. Drawer live tabs: Terminal (xterm) + Stats (sparkline) + Logs.
8. Drawer mgmt tabs: Network/policy + Secrets + Git + Files-stub.
9. Nodes page (cordon/drain/revoke + revoked list).
10. Templates + Network/Security + Operations + Settings.
11. Whole-app build + final embed verify + manual smoke.

Execution: `nuxt-ui` (component construction/theming) + `ui-ux-pro-max` (visual design
language) + `superpowers:subagent-driven-development` (task-by-task with reviews), as M8a
was executed. Merges are user-driven local ff-merges.

## Out of scope (carried forward)

- Files API tab (CopyTo/CopyFrom over REST) — still the M1c deferred item; stubbed.
- Server-side logout / session invalidation — no endpoint; client clears the flag only.
- Rich template metadata for *peers* (gossip carries names only).
- Swarm-global blocked-egress aggregation (blocked is per-sandbox only).
- Policy-rule deletion (no remove-rule RPC; the policy editor is add-only — backend gap).
- Browser e2e (Playwright).
- M8a's small review nits (terminal err-log, forward `out==nil` panic, test nits) —
  optionally folded into task 1.

## Self-review

- **Placeholders:** none — each view names its endpoint(s), live source, and test.
- **Consistency:** every endpoint matches the shipped proto/handlers (reconciliation
  section); live-data model matches the firehose's best-effort thin-poke semantics
  (ADR-0008); auth matches `internal/auth/auth.go` + ADR-0006; terminal matches
  `terminal.go` + ADR-0017; cordon matches ADR-0018.
- **Scope:** one milestone, all under `web/` + one Go embed test; backend is complete (M8a).
  Right-sized for a single implementation plan (~11 tasks).
- **Ambiguity:** the four forks are resolved — full-console scope; gitignored-dist
  build-step wiring; unit+component+smoke test bar; card-grid swarm map + SVG sparkline over
  Vue Flow/ECharts.
- **ADRs:** none warranted. The poke-refetch live model is a direct consequence of ADR-0008
  (firehose is best-effort, not a log); `role` on `NodeInfo`, the terminal keepalive, and
  swarm-map-over-graph are reversible frontend choices documented above.
- **Grill outcomes (this review):** role exposure via `NodeInfo.role` (backend task 0);
  single app-wide unfiltered firehose with coarse refetch; cross-node stats/logs/terminal
  confirmed working via `OwnerProxy` (`FlushInterval:-1`); terminal sends periodic KeepAlive
  while attached; tiered Provision modal; stats GET-poll fallback dropped (observe always
  wired); "swarm map" replaces "topology"; policy editor is add-only.
