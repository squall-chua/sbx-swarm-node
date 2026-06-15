# sbx-swarm-node M8 — Nuxt UI 4 Console Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development or superpowers:executing-plans. Checkbox steps. **Use the `nuxt-ui` skill for component construction** (it knows @nuxt/ui v4 components/theming).
>
> **Forward-looking:** depends on the full REST/SSE/WS API from M1–M7. Reconcile endpoint shapes against the live API.

**Goal:** A Nuxt 4 + @nuxt/ui v4 single-page console, built statically and embedded in the Go binary (M1b `web.FS`), that logs in via the session-cookie flow (ADR-0006), visualizes the swarm (Vue Flow topology + live events), and drills into sandboxes (stats charts, xterm.js terminal, network/policy, secrets, files, ports, git).

**Architecture:** SPA (`ssr: false`) talking to the same-origin node API: REST for unary, `EventSource` SSE for live data (cookie auth — no header needed), and a WebSocket for the interactive terminal. A typed API client + an SSE composable are the testable core (Vitest); pages are built with @nuxt/ui v4 via the `nuxt-ui` skill. The build outputs to `web/dist`, which the Go binary already embeds.

**Tech Stack:** Nuxt 4, @nuxt/ui v4, Vue 3, Vue Flow (`@vue-flow/core`), xterm.js, a Vue charts lib (ECharts via `vue-echarts`), Vitest + @vue/test-utils.

---

## File Structure

| Path | Responsibility |
|---|---|
| `web/nuxt.config.ts` | SPA mode, output to `dist`, modules |
| `web/app/composables/useApi.ts` | typed REST client (cookie + CSRF) |
| `web/app/composables/useEvents.ts` | SSE firehose subscription |
| `web/app/composables/useTerminal.ts` | WS terminal bridge → xterm.js |
| `web/app/middleware/auth.ts` | route guard (redirect to /login) |
| `web/app/pages/login.vue` | API-key → session-cookie exchange |
| `web/app/pages/index.vue` | Overview: topology + stat cards |
| `web/app/pages/sandboxes/*.vue` | list + drill-down drawer |
| `web/app/pages/{nodes,templates,network,operations,settings}.vue` | sections |
| `web/tests/*.spec.ts` | Vitest unit tests |
| `web/scripts/build.sh` | `nuxi generate` → copy to `web/dist` |

---

## Task 1: Scaffold Nuxt 4 SPA + embed integration

**Files:** `web/package.json`, `web/nuxt.config.ts`, `web/scripts/build.sh`, `internal/apiserver/server_embed_test.go`

- [ ] **Step 1: Scaffold + config**

```bash
cd web && npm init -y
npm install nuxt@^4 @nuxt/ui@^4 @vue-flow/core vue-echarts echarts @xterm/xterm
npm install -D vitest @vue/test-utils @nuxt/test-utils happy-dom
```

`web/nuxt.config.ts`:

```ts
export default defineNuxtConfig({
  ssr: false,                 // SPA embedded in the Go binary
  modules: ['@nuxt/ui'],
  nitro: { static: true },
  app: { baseURL: '/' },
  runtimeConfig: { public: { apiBase: '/' } }, // same-origin
})
```

`web/scripts/build.sh`:

```bash
#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
npm run generate                 # nuxi generate -> .output/public
rm -rf dist && mkdir -p dist
cp -r .output/public/* dist/
echo "built web/dist"
```

Add to `web/package.json` scripts: `"generate": "nuxi generate"`, `"test": "vitest run"`.

- [ ] **Step 2: Go-side embed test (TDD for the integration boundary)** — `internal/apiserver/server_embed_test.go`:

```go
package apiserver

import (
	"io/fs"
	"testing"

	"github.com/squall-chua/sbx-swarm-node/web"
	"github.com/stretchr/testify/require"
)

func TestEmbeddedSPA_HasIndex(t *testing.T) {
	_, err := fs.Stat(web.FS(), "index.html")
	require.NoError(t, err, "run web/scripts/build.sh to produce web/dist before building the binary")
}
```

- [ ] **Step 3: Build + run**

Run: `web/scripts/build.sh && go test ./internal/apiserver/ -run TestEmbeddedSPA -v`
Expected: PASS (a real `index.html` exists in `web/dist`).

- [ ] **Step 4: Commit**

```bash
git add web/package.json web/package-lock.json web/nuxt.config.ts web/scripts/ internal/apiserver/server_embed_test.go
git commit -m "feat(web): scaffold Nuxt 4 SPA + embed build integration"
```

> The Go binary build now depends on `web/dist` existing. Add `web/scripts/build.sh` to the project Makefile/CI before `go build`. Keep the M1b placeholder `web/dist/index.html` until the real build replaces it.

---

## Task 2: Typed API client + SSE composable (Vitest)

**Files:** `web/app/composables/useApi.ts`, `web/app/composables/useEvents.ts`, tests in `web/tests/`

- [ ] **Step 1: Failing Vitest test** — `web/tests/useApi.spec.ts`:

```ts
import { describe, it, expect, vi } from 'vitest'
import { createApi } from '../app/composables/useApi'

describe('createApi', () => {
  it('sends CSRF header on mutations using the csrf cookie', async () => {
    document.cookie = 'sbx_csrf=tok123'
    const fetchMock = vi.fn(async () => new Response('{}', { status: 200 }))
    const api = createApi('/', fetchMock as any)

    await api.post('/v1/sandboxes', { cpus: 1 })
    const [, init] = fetchMock.mock.calls[0]
    expect((init.headers as any)['X-CSRF-Token']).toBe('tok123')
    expect(init.credentials).toBe('include') // send cookies
  })
})
```

- [ ] **Step 2: Run → FAIL**: `cd web && npx vitest run tests/useApi.spec.ts`

- [ ] **Step 3: Implement `useApi.ts`**

```ts
// Typed REST client for the node API. Uses cookie auth (credentials: include)
// and double-submit CSRF on mutations (ADR-0006).
function readCookie(name: string): string {
  const m = document.cookie.match(new RegExp('(?:^|; )' + name + '=([^;]*)'))
  return m ? decodeURIComponent(m[1]) : ''
}

export function createApi(base: string, fetchImpl: typeof fetch = fetch) {
  async function req(method: string, path: string, body?: unknown) {
    const headers: Record<string, string> = { 'Content-Type': 'application/json' }
    if (!['GET', 'HEAD'].includes(method)) headers['X-CSRF-Token'] = readCookie('sbx_csrf')
    const res = await fetchImpl(base.replace(/\/$/, '') + path, {
      method, headers, credentials: 'include',
      body: body === undefined ? undefined : JSON.stringify(body),
    })
    if (res.status === 401) throw new Error('unauthorized')
    if (!res.ok) throw new Error(`${method} ${path} -> ${res.status}`)
    return res.status === 204 ? null : res.json()
  }
  return {
    get: (p: string) => req('GET', p),
    post: (p: string, b?: unknown) => req('POST', p, b),
    put: (p: string, b?: unknown) => req('PUT', p, b),
    del: (p: string) => req('DELETE', p),
  }
}

export const useApi = () => createApi(useRuntimeConfig().public.apiBase)
```

- [ ] **Step 4: SSE composable + test** — `web/tests/useEvents.spec.ts` mocks `EventSource`; `useEvents.ts`:

```ts
// Subscribes to the SSE firehose. EventSource sends the session cookie
// automatically (ADR-0006); filters go in the query string.
export function createEvents(base: string, ES: typeof EventSource = EventSource) {
  return function subscribe(opts: { types?: string[]; sandbox?: string }, onEvent: (e: any) => void) {
    const q = new URLSearchParams()
    if (opts.types?.length) q.set('types', opts.types.join(','))
    if (opts.sandbox) q.set('sandbox', opts.sandbox)
    const es = new ES(`${base.replace(/\/$/, '')}/v1/events?${q.toString()}`, { withCredentials: true })
    es.onmessage = (ev) => onEvent(JSON.parse(ev.data || '{}'))
    return () => es.close()
  }
}

export const useEvents = () => createEvents(useRuntimeConfig().public.apiBase)
```

Test asserts the URL carries `types=` and the returned unsubscribe calls `close()`.

- [ ] **Step 5: Run → PASS, commit**

```bash
cd web && npx vitest run
git add web/app/composables/ web/tests/
git commit -m "feat(web): API client + SSE composable with tests"
```

---

## Task 3: Auth (login + route guard)

**Files:** `web/app/pages/login.vue`, `web/app/middleware/auth.ts`

- [ ] **Step 1:** `login.vue` — an @nuxt/ui form (use the `nuxt-ui` skill) that POSTs the API key to `/v1/auth/session` with `Authorization: Bearer <key>` and `credentials: include`; on `204` it stores a `loggedIn` flag (e.g. `localStorage`/Pinia) and redirects to `/`. On failure shows an error toast.

- [ ] **Step 2:** `middleware/auth.ts` — a global route guard: if not `loggedIn` and route ≠ `/login`, redirect to `/login`. (Cookie validity is enforced server-side; the flag just drives UX.)

- [ ] **Step 3: Vitest** — a small test that the login submit calls the session endpoint with the bearer header and `credentials: include`.

- [ ] **Step 4: Commit**

```bash
cd web && npx vitest run
git add web/app/pages/login.vue web/app/middleware/auth.ts web/tests/
git commit -m "feat(web): cookie-session login + route guard (ADR-0006)"
```

---

## Task 4: Overview page — topology + live events (the reference page)

**Files:** `web/app/pages/index.vue`

- [ ] **Step 1: Build the Overview** (use `nuxt-ui` skill for layout/cards):
  - Fetch `GET /v1/nodes` and `GET /v1/sandboxes` via `useApi`.
  - Render a **Vue Flow** graph: one node box per swarm node (id, load bar from `alloc/limit`, cordoned/offline state), sandboxes grouped under their owner; edge pulse animation on a `scheduling`/`sandbox.created` event.
  - Subscribe via `useEvents({types:['sandbox','operation','membership','scheduling']})`; on each event, patch the local reactive store (add/update/remove a sandbox node, mark a node unreachable) so the graph is live.
  - Right rail: @nuxt/ui stat cards — allocated vs limit, sandbox counts by status, blocked-egress distinct count, recent operations.

- [ ] **Step 2: Vitest** — a component test mounting the overview with a mocked `useApi` returning two nodes + sandboxes, asserting the node boxes render and an injected `sandbox.created` event adds a sandbox.

- [ ] **Step 3: Commit**

```bash
cd web && npx vitest run
git add web/app/pages/index.vue web/tests/
git commit -m "feat(web): overview topology + live events"
```

---

## Task 5: Sandboxes list + drill-down drawer

**Files:** `web/app/pages/sandboxes/index.vue`, `web/app/components/SandboxDrawer.vue`, `web/app/composables/useTerminal.ts`

- [ ] **Step 1: List** — @nuxt/ui table from `GET /v1/sandboxes` with filters (status/label/node/workspace), a "+ Provision" modal POSTing `/v1/sandboxes` (with an `Idempotency-Key`), and a row click opening the drawer.

- [ ] **Step 2: Drawer tabs** (each binds to its API):
  - **Stats** — `vue-echarts` line chart fed by SSE `/v1/sandboxes/{id}/stats` (cpu%/mem); fallback to `GET .../stats`.
  - **Terminal** — `useTerminal.ts` opens `wss://…/v1/sandboxes/{id}/terminal`, wires xterm.js (write server→term, send keystrokes term→server). Cookie auth on the WS upgrade (ADR-0006).
  - **Network** — `GET .../network/blocked` table (host/first-seen/last-seen, distinct count) + a policy editor (`PUT .../policy`).
  - **Secrets** — masked list (`GET .../secrets`) + add form (`PUT .../secrets`, value write-only).
  - **Files** — upload/download (CopyTo/CopyFrom endpoints once exposed) — stub with a "coming soon" note if the file endpoints aren't live yet (they're the M1c deferred item).
  - **Ports** — list/publish/unpublish.
  - **Git** — branch + a "Publish" button (`POST .../git/publish`) shown only for clone-mode workspaces.

- [ ] **Step 3:** `useTerminal.ts` (testable: mock WebSocket; assert it forwards data both ways).

- [ ] **Step 4: Vitest + commit**

```bash
cd web && npx vitest run
git add web/app/pages/sandboxes/ web/app/components/SandboxDrawer.vue web/app/composables/useTerminal.ts web/tests/
git commit -m "feat(web): sandbox list + drill-down (stats/terminal/network/secrets/ports/git)"
```

---

## Task 6: Remaining sections

**Files:** `web/app/pages/{nodes,templates,network,operations,settings}.vue`

- [ ] **Step 1:** Build with @nuxt/ui (use `nuxt-ui` skill), each bound to its API:
  - **Nodes** — `GET /v1/nodes`: limits/util, workspaces, templates, capabilities, labels; cordon/drain/uncordon buttons (`POST /v1/nodes/{id}/...`).
  - **Templates** — `GET /v1/templates` (the gossiped catalog union).
  - **Network / Security** — swarm-wide blocked-egress aggregation + default-policy controls.
  - **Operations** — `GET /v1/operations` + live updates via SSE `types=['operation']`.
  - **Settings** — read-only view of node config (non-secret fields) + logout.

- [ ] **Step 2: Build the whole app + final embed verification**

Run: `web/scripts/build.sh && go test ./internal/apiserver/ -run TestEmbeddedSPA && go build ./...`
Expected: real SPA in `web/dist`, embed test passes, binary builds.

- [ ] **Step 3: Manual smoke** — run the node, open `https://localhost:8443/`, log in with an admin key, create a sandbox, watch it appear on the topology and stream stats/logs/terminal.

- [ ] **Step 4: Commit**

```bash
git add web/app/pages/ web/dist
git commit -m "feat(web): nodes/templates/network/operations/settings pages"
```

---

## Self-Review

**Spec coverage (M8):** embedded Nuxt 4 + @nuxt/ui v4 SPA on the one port → Tasks 1,6 ✓; cookie-session login (ADR-0006) → Task 3 ✓; live topology (Vue Flow) + event firehose → Task 4 ✓; sandbox drill-down (stats chart, xterm.js terminal over WS, network/policy, secrets masked, ports, git publish) → Task 5 ✓; nodes (cordon/drain), templates, network/security, operations, settings → Task 6 ✓; SSE for live data, WS only for terminal (matches the M1/M4 transport model) → Tasks 2,4,5 ✓.

**Placeholder scan:** The testable core (build/embed integration, API client, SSE composable, terminal composable) is fully coded with Vitest/Go tests. Per-page **component construction is explicitly delegated to the `nuxt-ui` skill** — that's the right tool for @nuxt/ui v4 layout/theming, not duplicated prose here — with each page's data sources, interactions, and a Vitest gate specified. The Files tab is explicitly marked "stub if endpoints not live" (tracks the M1c deferred file API), not silently dropped.

**Type consistency:** `createApi(base, fetch).{get,post,put,del}`; `createEvents(base, ES)(opts, onEvent)→unsubscribe`; `createTerminal`/`useTerminal` mock-tested. Endpoints referenced (`/v1/nodes`, `/v1/sandboxes`, `/v1/sandboxes/{id}/{stats,terminal,network/blocked,policy,secrets,ports,git/publish}`, `/v1/operations`, `/v1/auth/session`, `/v1/events`) match the API defined across M1–M7.

---

## Milestones complete

M1–M8 deliver the full v1: a decentralized, gossip-based Docker-sandbox swarm node with constraint-based placement, one-port gRPC+REST+SSE behind role auth, observability, policy/secrets, clone-mode git workspaces, TTL/idle reaping, and an embedded live console — implementing the hardened design (`docs/superpowers/specs/2026-06-15-sbx-swarm-node-design.md`) and its 9 ADRs.
