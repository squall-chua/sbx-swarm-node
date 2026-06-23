# sbx-swarm-node M8b — Nuxt UI 4 Swarm Console Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.
>
> **View-construction convention (not a placeholder):** Logic units (the Go backend task, the
> composables, the live store, all tests, build wiring) are fully coded below. For `.vue`
> **views/components**, each task gives (a) the real Vitest gate and (b) a precise component
> spec — data calls, props, interactions, role-gating, which @nuxt/ui components. **Build the
> markup with the `nuxt-ui` skill** (it knows @nuxt/ui v4 component APIs/theming) **and the
> `ui-ux-pro-max` skill** (visual design language). This mirrors the approved spec
> `docs/superpowers/specs/2026-06-23-m8b-console-design.md`; consult it for rationale.

**Goal:** A Nuxt 4 + @nuxt/ui v4 single-page console, built to `web/dist` and served by the
node, that visualizes the swarm and drives it (provision, terminal, stats/logs, cordon/drain/
revoke, policy/secrets, git publish) over the same-origin authed API.

**Architecture:** SPA (`ssr:false`) on the node's same-origin API: REST for unary, one
app-wide `EventSource` firehose for coarse "event poke → refetch" live data, per-sandbox
`EventSource` stats/logs and a WebSocket terminal opened on demand. The testable core is three
composables (`useApi`/`useEvents`/`useTerminal`) + a live store (`useSwarm`) under Vitest;
views are @nuxt/ui v4. Builds to `web/dist`, embedded via the existing `web.FS()`.

**Tech Stack:** Nuxt 4, @nuxt/ui v4, Vue 3, TypeScript, @xterm/xterm + @xterm/addon-fit,
Vitest + @vue/test-utils + @nuxt/test-utils + happy-dom. Backend: Go 1.25, buf/protobuf.

## Global Constraints

- Go `1.25.0`; module `github.com/squall-chua/sbx-swarm-node`.
- New **runtime** deps limited to: `@nuxt/ui`, `@xterm/xterm`, `@xterm/addon-fit`. Dev/test:
  `nuxt`, `vitest`, `@vue/test-utils`, `@nuxt/test-utils`, `happy-dom`. No `@vue-flow/core`,
  no `echarts`/`vue-echarts` (swarm map = card grid; stats = SVG sparkline).
- Nuxt config: `ssr: false` (SPA embedded in the Go binary), same-origin `apiBase: '/'`.
- REST JSON is **snake_case** (`UseProtoNames`) and **every field is always present**
  (`EmitUnpopulated`). Type accordingly.
- Auth (ADR-0006): login `POST /v1/auth/session` w/ `Authorization: Bearer <key>` →
  `sbx_session` (httpOnly) + `sbx_csrf` (JS-readable) cookies; mutations send
  `X-CSRF-Token` = `sbx_csrf` cookie; `credentials:include` everywhere.
- `web/dist` is **gitignored except a committed placeholder `index.html`**; the real SPA is
  built by `web/scripts/build.sh` before `go build`.
- gofmt **only your own new/touched files** (`main` is broadly gofmt-dirty, unenforced);
  `go build`/`go vet`/`go test` are truth; after `buf generate`, editor "undefined" errors are
  false — trust the CLI.
- Merges are **user-driven local ff-merges** — commit per task, do not merge unprompted.
  Commit trailer: `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`.
- The **server is the real authz gate**; UI role-gating (via `NodeInfo.role`) is UX only.
- Work on branch `m8b-console` (already created; the spec is committed there).

---

## Task 1: Backend — `role` on `NodeInfo`

The console needs the caller's role to hide admin actions from `read-only` keys; nothing
exposes it today. Add one proto field, populate it from the authenticated principal.

**Files:**
- Modify: `proto/sbxswarm/v1/node.proto` (NodeInfo message)
- Regenerate: `internal/gen/sbxswarm/v1/node.pb.go` (via `buf generate`)
- Modify: `internal/apiserver/nodeservice.go` (`GetNodeInfo`)
- Test: `internal/apiserver/nodeservice_test.go`

**Interfaces:**
- Produces: REST `GET /v1/node` JSON now includes `"role": "admin" | "read-only" | ""`.

- [ ] **Step 1: Write the failing test** — append to `internal/apiserver/nodeservice_test.go`:

```go
func TestGetNodeInfo_ReturnsCallerRole(t *testing.T) {
	svc := NewNodeService("n1", "node-one", "v1.2.3")

	adminCtx := context.WithValue(context.Background(), principalCtxKey{}, principal{userRole: "admin"})
	info, err := svc.GetNodeInfo(adminCtx, &sbxv1.GetNodeInfoRequest{})
	require.NoError(t, err)
	require.Equal(t, "admin", info.Role)

	roCtx := context.WithValue(context.Background(), principalCtxKey{}, principal{userRole: "read-only"})
	info, err = svc.GetNodeInfo(roCtx, &sbxv1.GetNodeInfoRequest{})
	require.NoError(t, err)
	require.Equal(t, "read-only", info.Role)
}
```

(Ensure the test file imports `context`, `testing`, `github.com/stretchr/testify/require`, and
`sbxv1 "github.com/squall-chua/sbx-swarm-node/internal/gen/sbxswarm/v1"` — most already exist.)

- [ ] **Step 2: Run test to verify it fails to compile**

Run: `go test ./internal/apiserver/ -run TestGetNodeInfo_ReturnsCallerRole`
Expected: FAIL — `info.Role undefined` (the proto field does not exist yet).

- [ ] **Step 3: Add the proto field** — in `proto/sbxswarm/v1/node.proto`, the `NodeInfo`
message currently ends at `bool draining = 5;`. Add:

```proto
message NodeInfo {
  string node_id = 1;
  string node_name = 2;
  string version = 3;
  bool cordoned = 4;
  bool draining = 5;
  string role = 6; // caller's role: "admin" | "read-only" | "" (node/none)
}
```

- [ ] **Step 4: Regenerate**

Run: `buf generate`
Expected: `internal/gen/sbxswarm/v1/node.pb.go` now has a `Role` field on `NodeInfo`. (Editor
may show stale "undefined" — ignore; trust the next `go build`.)

- [ ] **Step 5: Populate it** — in `internal/apiserver/nodeservice.go`, change `GetNodeInfo`
to read the principal from context:

```go
func (s *NodeService) GetNodeInfo(ctx context.Context, _ *sbxv1.GetNodeInfoRequest) (*sbxv1.NodeInfo, error) {
	return &sbxv1.NodeInfo{
		NodeId:   s.nodeID,
		NodeName: s.nodeName,
		Version:  s.version,
		Role:     principalFromContext(ctx).userRole,
	}, nil
}
```

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test ./internal/apiserver/ -run 'TestGetNodeInfo|TestAuthz_AllMethodsClassified'`
Expected: PASS (the authz drift guard still passes — no method added, only a field).

- [ ] **Step 7: Build + vet the whole module**

Run: `go build ./... && go vet ./internal/apiserver/`
Expected: clean.

- [ ] **Step 8: gofmt your touched Go files**

Run: `gofmt -w internal/apiserver/nodeservice.go internal/apiserver/nodeservice_test.go`

- [ ] **Step 9: Commit**

```bash
git add proto/sbxswarm/v1/node.proto internal/gen/sbxswarm/v1/ internal/apiserver/nodeservice.go internal/apiserver/nodeservice_test.go
git commit -m "feat(node): expose caller role on GetNodeInfo for console gating" -m "Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 2: Scaffold Nuxt 4 SPA + embed/dev wiring

**Files:**
- Create: `web/package.json`, `web/nuxt.config.ts`, `web/tsconfig.json`, `web/vitest.config.ts`
- Create: `web/app/app.vue`
- Create: `web/scripts/build.sh`
- Create: `web/.gitignore`
- Keep: `web/dist/index.html` (existing placeholder — do not delete)
- Modify: root `Makefile` (add `web` target) or create one if absent
- Test: `internal/apiserver/server_embed_test.go`

**Interfaces:**
- Produces: `npm --prefix web run build` (alias for `web/scripts/build.sh`) emits `web/dist/`;
  `npm --prefix web test` runs Vitest; `npm --prefix web run dev` runs the SPA with an API
  proxy. `web.FS()` still serves `index.html`.

- [ ] **Step 1: Init + install** (from repo root):

```bash
cd web && npm init -y
npm install nuxt@^4 @nuxt/ui@^4 @xterm/xterm @xterm/addon-fit
npm install -D vitest @vue/test-utils @nuxt/test-utils happy-dom
cd ..
```

- [ ] **Step 2: `web/nuxt.config.ts`**

```ts
export default defineNuxtConfig({
  ssr: false,                          // SPA embedded in the Go binary
  modules: ['@nuxt/ui'],
  devtools: { enabled: false },
  app: { baseURL: '/' },
  runtimeConfig: { public: { apiBase: '/' } }, // same-origin
  nitro: {
    static: true,
    // `nuxt dev` proxies the authed API + SSE/WS to a running node (self-signed TLS).
    devProxy: {
      '/v1':      { target: 'https://localhost:8443/v1',      secure: false, ws: true, changeOrigin: true },
      '/healthz': { target: 'https://localhost:8443/healthz', secure: false },
      '/metrics': { target: 'https://localhost:8443/metrics', secure: false },
    },
  },
})
```

- [ ] **Step 3: `web/package.json` scripts** — set the `scripts` block to:

```json
{
  "scripts": {
    "dev": "nuxi dev",
    "build": "bash scripts/build.sh",
    "generate": "nuxi generate",
    "test": "vitest run"
  }
}
```

- [ ] **Step 4: `web/scripts/build.sh`**

```bash
#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
npm run generate            # nuxi generate -> .output/public
rm -rf dist && mkdir -p dist
cp -r .output/public/* dist/
echo "built web/dist"
```

Then: `chmod +x web/scripts/build.sh`

- [ ] **Step 5: `web/.gitignore`** (keep the placeholder, ignore real build output + node_modules):

```gitignore
node_modules/
.nuxt/
.output/
# Built SPA is produced by scripts/build.sh; keep only the placeholder index.html.
dist/*
!dist/index.html
```

- [ ] **Step 6: `web/app/app.vue`** (root — @nuxt/ui requires the `UApp` wrapper):

```vue
<template>
  <UApp>
    <NuxtPage />
  </UApp>
</template>
```

- [ ] **Step 7: `web/vitest.config.ts`**

```ts
import { defineConfig } from 'vitest/config'

export default defineConfig({
  test: {
    environment: 'happy-dom',
    globals: true,
  },
})
```

- [ ] **Step 8: Go embed test** — create `internal/apiserver/server_embed_test.go`:

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

- [ ] **Step 9: Makefile target** — add to the root `Makefile` (create it if absent):

```make
.PHONY: web
web: ## Build the embedded console SPA into web/dist
	bash web/scripts/build.sh

.PHONY: build
build: web ## Build the node binary (console embedded)
	go build ./...
```

- [ ] **Step 10: Verify the Go side compiles against the placeholder**

Run: `go test ./internal/apiserver/ -run TestEmbeddedSPA -v`
Expected: PASS (the committed placeholder `web/dist/index.html` satisfies `fs.Stat`).

- [ ] **Step 11: Verify the SPA builds**

Run: `npm --prefix web run build`
Expected: prints `built web/dist`; `web/dist/index.html` is now a real Nuxt entry (gitignored).

- [ ] **Step 12: Restore the placeholder for the commit** (so the repo keeps the tiny placeholder,
not the build output):

```bash
git checkout -- web/dist/index.html
```

- [ ] **Step 13: Commit**

```bash
git add web/package.json web/package-lock.json web/nuxt.config.ts web/tsconfig.json web/vitest.config.ts web/app/app.vue web/scripts/ web/.gitignore Makefile internal/apiserver/server_embed_test.go
git commit -m "feat(web): scaffold Nuxt 4 SPA + embed/dev build wiring" -m "Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 3: `useApi` — typed REST client (Vitest)

**Files:**
- Create: `web/app/composables/useApi.ts`
- Test: `web/tests/useApi.spec.ts`

**Interfaces:**
- Produces: `createApi(base: string, onAuthLost: () => void, fetchImpl?): Api` where
  `Api = { get, post(path, body?, headers?), put, del }`, each returning `Promise<any>`.
  `useApi()` is the Nuxt wrapper. Mutations send `X-CSRF-Token`; `401` → `onAuthLost()`.

- [ ] **Step 1: Write the failing test** — `web/tests/useApi.spec.ts`:

```ts
import { describe, it, expect, vi } from 'vitest'
import { createApi } from '../app/composables/useApi'

describe('createApi', () => {
  it('sends CSRF header + credentials on mutations, not on GET', async () => {
    document.cookie = 'sbx_csrf=tok123'
    const fetchMock = vi.fn(async () => new Response('{}', { status: 200 }))
    const api = createApi('/', () => {}, fetchMock as any)

    await api.post('/v1/sandboxes', { cpus: 1 })
    let [, init] = fetchMock.mock.calls[0]
    expect((init.headers as any)['X-CSRF-Token']).toBe('tok123')
    expect(init.credentials).toBe('include')

    await api.get('/v1/sandboxes')
    ;[, init] = fetchMock.mock.calls[1]
    expect((init.headers as any)['X-CSRF-Token']).toBeUndefined()
  })

  it('passes extra headers (e.g. Idempotency-Key) on post', async () => {
    const fetchMock = vi.fn(async () => new Response('{}', { status: 200 }))
    const api = createApi('/', () => {}, fetchMock as any)
    await api.post('/v1/sandboxes', { cpus: 1 }, { 'Idempotency-Key': 'abc' })
    const [, init] = fetchMock.mock.calls[0]
    expect((init.headers as any)['Idempotency-Key']).toBe('abc')
  })

  it('calls onAuthLost on 401', async () => {
    const fetchMock = vi.fn(async () => new Response('nope', { status: 401 }))
    const onAuthLost = vi.fn()
    const api = createApi('/', onAuthLost, fetchMock as any)
    await expect(api.get('/v1/nodes')).rejects.toThrow()
    expect(onAuthLost).toHaveBeenCalledOnce()
  })
})
```

- [ ] **Step 2: Run → FAIL**

Run: `npm --prefix web exec vitest run tests/useApi.spec.ts`
Expected: FAIL — cannot resolve `createApi`.

- [ ] **Step 3: Implement** — `web/app/composables/useApi.ts`:

```ts
// Typed REST client for the node API. Cookie auth (credentials:include) + double-submit
// CSRF on mutations (ADR-0006). 401 -> drop session and bounce to /login. We deliberately
// do NOT auto-redirect on 403: the UI gates mutations by role, so a 403 is rare and is
// surfaced as an error (session expiry shows up as 401 first, since the cookie is checked
// before CSRF in internal/auth/auth.go).
function readCookie(name: string): string {
  const m = document.cookie.match(new RegExp('(?:^|; )' + name + '=([^;]*)'))
  return m ? decodeURIComponent(m[1]) : ''
}

export type Api = {
  get: (path: string) => Promise<any>
  post: (path: string, body?: unknown, headers?: Record<string, string>) => Promise<any>
  put: (path: string, body?: unknown) => Promise<any>
  del: (path: string) => Promise<any>
}

export function createApi(base: string, onAuthLost: () => void, fetchImpl: typeof fetch = fetch): Api {
  async function req(method: string, path: string, body?: unknown, extra?: Record<string, string>) {
    const headers: Record<string, string> = { 'Content-Type': 'application/json', ...(extra ?? {}) }
    if (method !== 'GET' && method !== 'HEAD') headers['X-CSRF-Token'] = readCookie('sbx_csrf')
    const res = await fetchImpl(base.replace(/\/$/, '') + path, {
      method,
      headers,
      credentials: 'include',
      body: body === undefined ? undefined : JSON.stringify(body),
    })
    if (res.status === 401) {
      onAuthLost()
      throw new Error('unauthorized')
    }
    if (!res.ok) throw new Error(`${method} ${path} -> ${res.status}`)
    return res.status === 204 ? null : res.json()
  }
  return {
    get: (p) => req('GET', p),
    post: (p, b, h) => req('POST', p, b, h),
    put: (p, b) => req('PUT', p, b),
    del: (p) => req('DELETE', p),
  }
}

// Nuxt wrapper: same-origin base; on auth loss, clear the flag and route to /login.
export const useApi = (): Api =>
  createApi(useRuntimeConfig().public.apiBase as string, () => {
    if (import.meta.client) localStorage.removeItem('sbx_logged_in')
    navigateTo('/login')
  })
```

- [ ] **Step 4: Run → PASS**

Run: `npm --prefix web exec vitest run tests/useApi.spec.ts`
Expected: PASS (3 tests).

- [ ] **Step 5: Commit**

```bash
git add web/app/composables/useApi.ts web/tests/useApi.spec.ts
git commit -m "feat(web): typed REST client with CSRF + auth-loss handling" -m "Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 4: `useEvents` — SSE firehose (Vitest)

The server emits **named** events (`event:<type>`), so we must `addEventListener` per type —
`onmessage` never fires (finding #1). We enumerate the known types; missed/unknown types are
caught by `useSwarm`'s periodic backstop (Task 7).

**Files:**
- Create: `web/app/composables/useEvents.ts`
- Test: `web/tests/useEvents.spec.ts`

**Interfaces:**
- Produces: `SWARM_EVENT_TYPES: string[]`; `createEvents(base, ES?)` returns
  `subscribe(types: string[], onEvent: (type: string, data: any) => void, opts?: { sandbox?: string }) => () => void`.
  `useEvents()` is the Nuxt wrapper.

- [ ] **Step 1: Write the failing test** — `web/tests/useEvents.spec.ts`:

```ts
import { describe, it, expect, vi } from 'vitest'
import { createEvents, SWARM_EVENT_TYPES } from '../app/composables/useEvents'

class FakeES {
  url: string
  withCredentials: boolean
  listeners: Record<string, Function> = {}
  closed = false
  constructor(url: string, init?: { withCredentials?: boolean }) {
    this.url = url
    this.withCredentials = !!init?.withCredentials
  }
  addEventListener(type: string, fn: Function) { this.listeners[type] = fn }
  close() { this.closed = true }
  emit(type: string, data: string) { this.listeners[type]?.({ type, data }) }
}

describe('createEvents', () => {
  it('registers a listener per type (never onmessage), with credentials', () => {
    let es!: FakeES
    const ES = vi.fn((u: string, i: any) => (es = new FakeES(u, i))) as any
    const seen: Array<[string, any]> = []
    const unsub = createEvents('/', ES)(['sandbox.created', 'operation.done'], (t, d) => seen.push([t, d]))

    expect(es.withCredentials).toBe(true)
    expect(es.url).toBe('/v1/events')
    expect(Object.keys(es.listeners).sort()).toEqual(['operation.done', 'sandbox.created'])

    es.emit('sandbox.created', '{"status":"created"}')
    expect(seen).toEqual([['sandbox.created', { status: 'created' }]])

    unsub()
    expect(es.closed).toBe(true)
  })

  it('adds the sandbox filter to the query string', () => {
    let es!: FakeES
    const ES = vi.fn((u: string, i: any) => (es = new FakeES(u, i))) as any
    createEvents('/', ES)(['sandbox.created'], () => {}, { sandbox: 'n1.abc' })
    expect(es.url).toBe('/v1/events?sandbox=n1.abc')
  })

  it('exports the known swarm event types', () => {
    expect(SWARM_EVENT_TYPES).toContain('sandbox.created')
    expect(SWARM_EVENT_TYPES).toContain('operation.done')
  })
})
```

- [ ] **Step 2: Run → FAIL**

Run: `npm --prefix web exec vitest run tests/useEvents.spec.ts`
Expected: FAIL — cannot resolve `createEvents`.

- [ ] **Step 3: Implement** — `web/app/composables/useEvents.ts`:

```ts
// Subscribes to the SSE firehose (/v1/events). The server writes named events
// (event:<type>), so onmessage never fires — we addEventListener per type. EventSource
// sends the session cookie automatically (ADR-0006). Frames are thin pokes: payload only,
// no sandbox id (finding #3), so callers refetch rather than apply deltas.
export const SWARM_EVENT_TYPES = [
  'sandbox.created', 'sandbox.running', 'sandbox.stopped', 'sandbox.deleted',
  'sandbox.lost', 'sandbox.published', 'sandbox.publish_failed',
  'operation.pending', 'operation.running', 'operation.done', 'operation.error',
]

export function createEvents(base: string, ES: typeof EventSource = EventSource) {
  return function subscribe(
    types: string[],
    onEvent: (type: string, data: any) => void,
    opts: { sandbox?: string } = {},
  ): () => void {
    const q = new URLSearchParams()
    if (opts.sandbox) q.set('sandbox', opts.sandbox)
    const qs = q.toString()
    const url = `${base.replace(/\/$/, '')}/v1/events${qs ? '?' + qs : ''}`
    const es = new ES(url, { withCredentials: true })
    const handler = (ev: MessageEvent) => onEvent((ev as any).type, ev.data ? JSON.parse(ev.data) : null)
    for (const t of types) es.addEventListener(t, handler as EventListener)
    return () => es.close()
  }
}

export const useEvents = () => createEvents(useRuntimeConfig().public.apiBase as string)
```

- [ ] **Step 4: Run → PASS**

Run: `npm --prefix web exec vitest run tests/useEvents.spec.ts`
Expected: PASS (3 tests).

- [ ] **Step 5: Commit**

```bash
git add web/app/composables/useEvents.ts web/tests/useEvents.spec.ts
git commit -m "feat(web): SSE firehose composable (named events, per-type listeners)" -m "Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 5: `useTerminal` — WebSocket ↔ xterm bridge (Vitest)

Binary frames = I/O (keystrokes are **binary**, so the server routes them to stdin); a text
JSON frame = resize. While attached, KeepAlive on a ticker (idle-stop footgun).

**Files:**
- Create: `web/app/composables/useTerminal.ts`
- Test: `web/tests/useTerminal.spec.ts`

**Interfaces:**
- Consumes: a `TermIO` (`{ onData(cb), write(bytes) }`) — Task 11's `Terminal.vue` adapts
  xterm to this.
- Produces: `createTerminal(wsUrl, io, keepAlive, opts?) => { resize(cols, rows), close() }`.

- [ ] **Step 1: Write the failing test** — `web/tests/useTerminal.spec.ts`:

```ts
import { describe, it, expect, vi } from 'vitest'
import { createTerminal } from '../app/composables/useTerminal'

class FakeWS {
  static OPEN = 1
  readyState = 1
  binaryType = ''
  onmessage: ((e: any) => void) | null = null
  sent: any[] = []
  closed = false
  constructor(public url: string) {}
  send(d: any) { this.sent.push(d) }
  close() { this.closed = true }
}

describe('createTerminal', () => {
  it('bridges WS<->xterm and pings keepalive', () => {
    vi.useFakeTimers()
    let ws!: FakeWS
    const WS = vi.fn((u: string) => (ws = new FakeWS(u))) as any
    const written: Uint8Array[] = []
    let dataCb: (d: string) => void = () => {}
    const io = { onData: (cb: any) => (dataCb = cb), write: (b: Uint8Array) => written.push(b) }
    const keepAlive = vi.fn()

    const term = createTerminal('wss://x/v1/sandboxes/s1/terminal', io, keepAlive, { WS, keepAliveMs: 1000 })
    expect(ws.binaryType).toBe('arraybuffer')

    // server -> xterm
    ws.onmessage!({ data: new TextEncoder().encode('hi').buffer })
    expect(new TextDecoder().decode(written[0])).toBe('hi')

    // xterm -> server (binary)
    dataCb('x')
    expect(ws.sent[0]).toBeInstanceOf(Uint8Array)

    // resize -> text JSON
    term.resize(80, 24)
    expect(JSON.parse(ws.sent[1] as string)).toEqual({ type: 'resize', cols: 80, rows: 24 })

    // keepalive ticks
    vi.advanceTimersByTime(1000)
    expect(keepAlive).toHaveBeenCalledOnce()

    term.close()
    expect(ws.closed).toBe(true)
    vi.useRealTimers()
  })
})
```

- [ ] **Step 2: Run → FAIL**

Run: `npm --prefix web exec vitest run tests/useTerminal.spec.ts`
Expected: FAIL — cannot resolve `createTerminal`.

- [ ] **Step 3: Implement** — `web/app/composables/useTerminal.ts`:

```ts
// Bridges a sandbox terminal WebSocket to an xterm-like IO. Binary frames carry I/O
// (keystrokes go out as binary so the server routes them to stdin); a text JSON frame
// {"type":"resize",cols,rows} resizes. While attached we KeepAlive on a ticker so an idle
// interactive session isn't idle-stopped (the server bumps Activity only at attach).
export type TermIO = {
  onData: (cb: (d: string) => void) => void
  write: (bytes: Uint8Array) => void
}

export function createTerminal(
  wsUrl: string,
  io: TermIO,
  keepAlive: () => void,
  opts: { WS?: typeof WebSocket; keepAliveMs?: number } = {},
) {
  const WS = opts.WS ?? WebSocket
  const ws = new WS(wsUrl)
  ws.binaryType = 'arraybuffer'
  ws.onmessage = (ev: MessageEvent) => {
    if (ev.data instanceof ArrayBuffer) io.write(new Uint8Array(ev.data))
  }
  io.onData((d) => {
    if (ws.readyState === 1) ws.send(new TextEncoder().encode(d))
  })
  const resize = (cols: number, rows: number) => {
    if (ws.readyState === 1) ws.send(JSON.stringify({ type: 'resize', cols, rows }))
  }
  const timer = setInterval(keepAlive, opts.keepAliveMs ?? 60_000)
  const close = () => {
    clearInterval(timer)
    ws.close()
  }
  return { resize, close }
}
```

- [ ] **Step 4: Run → PASS**

Run: `npm --prefix web exec vitest run tests/useTerminal.spec.ts`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add web/app/composables/useTerminal.ts web/tests/useTerminal.spec.ts
git commit -m "feat(web): WebSocket terminal bridge with keepalive" -m "Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 6: Auth — login page + global guard + `useSession` (role bootstrap)

**Files:**
- Create: `web/app/composables/useSession.ts`
- Create: `web/app/middleware/auth.global.ts`
- Create: `web/app/pages/login.vue`
- Test: `web/tests/useSession.spec.ts`

**Interfaces:**
- Consumes: `useApi` (Task 3).
- Produces: `useSession()` → `{ loggedIn: Ref<boolean>, role: Ref<string>, login(key), logout(), loadRole() }`.
  `login` POSTs the bearer key to `/v1/auth/session`; `loadRole` GETs `/v1/node` → `role`.
  `isAdmin` helper: `computed(() => role.value === 'admin')`.

- [ ] **Step 1: Write the failing test** — `web/tests/useSession.spec.ts` (tests the pure core
`createSession(api, fetchImpl)`):

```ts
import { describe, it, expect, vi } from 'vitest'
import { createSession } from '../app/composables/useSession'

describe('createSession', () => {
  it('login POSTs the bearer key to the session endpoint', async () => {
    const fetchMock = vi.fn(async () => new Response(null, { status: 204 }))
    const api = { get: vi.fn(async () => ({ role: 'admin' })) } as any
    const s = createSession('/', api, fetchMock as any)

    await s.login('secret-key')
    const [url, init] = fetchMock.mock.calls[0]
    expect(url).toBe('/v1/auth/session')
    expect(init.method).toBe('POST')
    expect((init.headers as any).Authorization).toBe('Bearer secret-key')
    expect(init.credentials).toBe('include')
    expect(s.loggedIn.value).toBe(true)
  })

  it('loadRole pulls role from GET /v1/node', async () => {
    const api = { get: vi.fn(async () => ({ role: 'read-only' })) } as any
    const s = createSession('/', api, vi.fn() as any)
    await s.loadRole()
    expect(api.get).toHaveBeenCalledWith('/v1/node')
    expect(s.role.value).toBe('read-only')
  })
})
```

- [ ] **Step 2: Run → FAIL**

Run: `npm --prefix web exec vitest run tests/useSession.spec.ts`
Expected: FAIL — cannot resolve `createSession`.

- [ ] **Step 3: Implement `useSession.ts`**:

```ts
import { ref } from 'vue'
import type { Api } from './useApi'

// Session state for routing/UX only — the server enforces real auth. login() exchanges a
// bearer key for cookies; loadRole() reads the caller's role from GET /v1/node (Task 1).
export function createSession(base: string, api: Api, fetchImpl: typeof fetch = fetch) {
  const loggedIn = ref(false)
  const role = ref('')

  async function login(key: string) {
    const res = await fetchImpl(base.replace(/\/$/, '') + '/v1/auth/session', {
      method: 'POST',
      headers: { Authorization: 'Bearer ' + key },
      credentials: 'include',
    })
    if (res.status !== 204) throw new Error('invalid key')
    loggedIn.value = true
    await loadRole()
  }
  async function loadRole() {
    role.value = (await api.get('/v1/node'))?.role ?? ''
  }
  function logout() {
    loggedIn.value = false
    role.value = ''
  }
  return { loggedIn, role, login, loadRole, logout }
}

// Nuxt singleton via useState; persists the loggedIn flag in localStorage for the guard.
export const useSession = () => {
  const base = useRuntimeConfig().public.apiBase as string
  const api = useApi()
  const loggedIn = useState('sbx_logged_in', () => import.meta.client && localStorage.getItem('sbx_logged_in') === '1')
  const role = useState('sbx_role', () => '')
  return {
    loggedIn,
    role,
    isAdmin: computed(() => role.value === 'admin'),
    async login(key: string) {
      const core = createSession(base, api)
      await core.login(key)
      loggedIn.value = true
      role.value = core.role.value
      if (import.meta.client) localStorage.setItem('sbx_logged_in', '1')
    },
    async loadRole() {
      role.value = (await api.get('/v1/node'))?.role ?? ''
    },
    logout() {
      loggedIn.value = false
      role.value = ''
      if (import.meta.client) localStorage.removeItem('sbx_logged_in')
      navigateTo('/login')
    },
  }
}
```

- [ ] **Step 4: Run → PASS**

Run: `npm --prefix web exec vitest run tests/useSession.spec.ts`
Expected: PASS.

- [ ] **Step 5: `web/app/middleware/auth.global.ts`**:

```ts
export default defineNuxtRouteMiddleware((to) => {
  const { loggedIn } = useSession()
  if (!loggedIn.value && to.path !== '/login') return navigateTo('/login')
  if (loggedIn.value && to.path === '/login') return navigateTo('/')
})
```

- [ ] **Step 6: Build `login.vue` with the `nuxt-ui` + `ui-ux-pro-max` skills** to satisfy:
  - A centered card with a single password `UInput` (the API key) + a submit `UButton`.
  - On submit: `await useSession().login(key)`; on success `navigateTo('/')`; on error show a
    `useToast()` error ("invalid key").
  - `definePageMeta({ layout: false })` (login is outside the dashboard shell).

- [ ] **Step 7: Manual check** — `npm --prefix web run dev` (with a node running on :8443),
visit `/`, confirm redirect to `/login`; enter a valid admin key → lands on `/` (blank for
now). Confirm an invalid key toasts an error.

- [ ] **Step 8: Commit**

```bash
git add web/app/composables/useSession.ts web/app/middleware/auth.global.ts web/app/pages/login.vue web/tests/useSession.spec.ts
git commit -m "feat(web): cookie-session login + global guard + role bootstrap" -m "Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 7: App shell (dashboard layout/nav) + `useSwarm` live store

**Files:**
- Create: `web/app/composables/useSwarm.ts`
- Create: `web/app/layouts/default.vue`
- Test: `web/tests/useSwarm.spec.ts`

**Interfaces:**
- Consumes: `useApi` (Task 3), `useEvents` + `SWARM_EVENT_TYPES` (Task 4).
- Produces: `createSwarmStore(api, subscribe, opts?)` → `{ nodes, sandboxes, operations,
  refreshAll, refreshNodes, refreshSandboxes, refreshOperations, stop }` (Vue refs).
  `useSwarm()` is the Nuxt singleton wrapper.

- [ ] **Step 1: Write the failing test** — `web/tests/useSwarm.spec.ts`:

```ts
import { describe, it, expect, vi } from 'vitest'
import { createSwarmStore } from '../app/composables/useSwarm'

describe('createSwarmStore', () => {
  it('a sandbox.* event pokes a debounced sandbox refetch', async () => {
    vi.useFakeTimers()
    const api = {
      get: vi.fn(async (p: string) => {
        if (p === '/v1/sandboxes') return { sandboxes: [{ id: 's1' }] }
        if (p === '/v1/nodes') return { nodes: [] }
        if (p === '/v1/operations') return { operations: [] }
      }),
    } as any
    let handler!: (type: string, data: any) => void
    const subscribe = vi.fn((_types: string[], cb: any) => { handler = cb; return () => {} })

    const store = createSwarmStore(api, subscribe, { debounceMs: 100, backstopMs: 999999 })
    api.get.mockClear()

    handler('sandbox.created', null)
    handler('sandbox.created', null) // debounced: collapses to one
    vi.advanceTimersByTime(100)
    await Promise.resolve()

    expect(api.get).toHaveBeenCalledWith('/v1/sandboxes')
    store.stop()
    vi.useRealTimers()
  })
})
```

- [ ] **Step 2: Run → FAIL**

Run: `npm --prefix web exec vitest run tests/useSwarm.spec.ts`
Expected: FAIL — cannot resolve `createSwarmStore`.

- [ ] **Step 3: Implement `useSwarm.ts`**:

```ts
import { ref } from 'vue'
import type { Api } from './useApi'
import { SWARM_EVENT_TYPES } from './useEvents'

type Subscribe = (
  types: string[],
  onEvent: (type: string, data: any) => void,
  opts?: { sandbox?: string },
) => () => void

// App-wide live store. One unfiltered firehose drives coarse refetches: a sandbox.* event
// refetches the sandbox list (+ nodes, since allocation changes); operation.* refetches
// operations. A periodic backstop catches any missed/unknown-type events (findings #2/#3).
export function createSwarmStore(api: Api, subscribe: Subscribe, opts: { debounceMs?: number; backstopMs?: number } = {}) {
  const nodes = ref<any[]>([])
  const sandboxes = ref<any[]>([])
  const operations = ref<any[]>([])

  const refreshNodes = async () => { nodes.value = (await api.get('/v1/nodes'))?.nodes ?? [] }
  const refreshSandboxes = async () => { sandboxes.value = (await api.get('/v1/sandboxes'))?.sandboxes ?? [] }
  const refreshOperations = async () => { operations.value = (await api.get('/v1/operations'))?.operations ?? [] }
  const refreshAll = () => Promise.all([refreshNodes(), refreshSandboxes(), refreshOperations()])

  const debounce = (fn: () => void, ms: number) => {
    let t: any
    return () => { clearTimeout(t); t = setTimeout(fn, ms) }
  }
  const d = opts.debounceMs ?? 300
  const pokeSandboxes = debounce(refreshSandboxes, d)
  const pokeNodes = debounce(refreshNodes, d)
  const pokeOps = debounce(refreshOperations, d)

  const unsub = subscribe(SWARM_EVENT_TYPES, (type) => {
    if (type.startsWith('sandbox.')) { pokeSandboxes(); pokeNodes() }
    else if (type.startsWith('operation.')) { pokeOps() }
  })
  const interval = setInterval(refreshAll, opts.backstopMs ?? 25_000)
  const stop = () => { unsub(); clearInterval(interval) }

  return { nodes, sandboxes, operations, refreshAll, refreshNodes, refreshSandboxes, refreshOperations, stop }
}

// Nuxt singleton: created once, shared across views.
export const useSwarm = () => {
  const holder = useState<ReturnType<typeof createSwarmStore> | null>('sbx_swarm', () => null)
  if (!holder.value && import.meta.client) {
    holder.value = createSwarmStore(useApi(), useEvents())
    holder.value.refreshAll()
  }
  return holder.value!
}
```

- [ ] **Step 4: Run → PASS**

Run: `npm --prefix web exec vitest run tests/useSwarm.spec.ts`
Expected: PASS.

- [ ] **Step 5: Build `layouts/default.vue` with the `nuxt-ui` + `ui-ux-pro-max` skills** to satisfy:
  - An @nuxt/ui **dashboard** shell (sidebar + main panel) with nav links: Overview `/`,
    Sandboxes `/sandboxes`, Nodes `/nodes`, Templates `/templates`, Network `/network`,
    Operations `/operations`, Settings `/settings`.
  - Header shows the node name + role badge (from `useSession().role`) and a logout button
    (`useSession().logout()`).
  - `<slot />` (or `<NuxtPage/>` via layout) renders the active page.

- [ ] **Step 6: Manual check** — `npm --prefix web run dev`, log in, confirm the sidebar renders
and links navigate (pages may be empty until built).

- [ ] **Step 7: Commit**

```bash
git add web/app/composables/useSwarm.ts web/app/layouts/default.vue web/tests/useSwarm.spec.ts
git commit -m "feat(web): app shell + live swarm store (event poke -> refetch)" -m "Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 8: Overview — swarm map + stat cards

**Files:**
- Create: `web/app/pages/index.vue`
- Create: `web/app/components/NodeCard.vue`
- Test: `web/tests/overview.spec.ts`

**Interfaces:**
- Consumes: `useSwarm()` (`nodes`, `sandboxes`, `operations`).

- [ ] **Step 1: Write the failing component test** — `web/tests/overview.spec.ts`:

```ts
// @vitest-environment nuxt
import { describe, it, expect, vi } from 'vitest'
import { mountSuspended } from '@nuxt/test-utils/runtime'
import Index from '../app/pages/index.vue'

vi.mock('../app/composables/useSwarm', () => ({
  useSwarm: () => ({
    nodes: ref([
      { node_id: 'n1', node_name: 'alpha', cordoned: false, draining: false,
        limit_cpu: 8, alloc_cpu: 2, actual_cpu: 1, limit_mem_kb: 100, alloc_mem_kb: 10,
        templates: [], workspaces: [], labels: {}, capabilities: [] },
    ]),
    sandboxes: ref([{ id: 'n1.s1', owner_node: 'n1', status: 'running' }]),
    operations: ref([]),
    refreshAll: vi.fn(),
  }),
}))

describe('Overview', () => {
  it('renders a card per node with its name', async () => {
    const wrapper = await mountSuspended(Index)
    expect(wrapper.text()).toContain('alpha')
  })
})
```

- [ ] **Step 2: Run → FAIL**

Run: `npm --prefix web exec vitest run tests/overview.spec.ts`
Expected: FAIL — `index.vue` does not exist.

- [ ] **Step 3: Build `NodeCard.vue` + `index.vue` with the `nuxt-ui` + `ui-ux-pro-max` skills**
to satisfy:
  - `index.vue`: a row of @nuxt/ui stat cards — node count; sandboxes grouped by status;
    Σ `alloc_cpu`/`limit_cpu` across nodes; blocked-egress distinct (omit if not cheaply
    available — leave a TODO comment, not a stat); recent operations (first ~5 of
    `operations`).
  - Below: the **swarm map** = a responsive grid of `NodeCard` (one per `nodes` entry). No
    edges.
  - `NodeCard.vue` props: a `NodeSummary`. Shows node_name, cordoned/draining badges, CPU/mem
    load bars (`actual` and `alloc` vs `limit`), and chips for the sandboxes whose
    `owner_node` === this node's `node_id` (passed in or filtered from `useSwarm().sandboxes`).
  - Live by virtue of `useSwarm()` refs; add a manual refresh button calling `refreshAll()`.

- [ ] **Step 4: Run → PASS** (adjust the test's text assertion to the actual rendered node-name
element if needed — the assertion *logic* "card shows the node name" is the gate).

Run: `npm --prefix web exec vitest run tests/overview.spec.ts`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add web/app/pages/index.vue web/app/components/NodeCard.vue web/tests/overview.spec.ts
git commit -m "feat(web): overview with swarm map + stat cards" -m "Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 9: Sandboxes list + Provision modal

**Files:**
- Create: `web/app/pages/sandboxes/index.vue`
- Create: `web/app/components/ProvisionModal.vue`
- Test: `web/tests/provision.spec.ts`

**Interfaces:**
- Consumes: `useApi`, `useSwarm` (`sandboxes`, `nodes` for template/workspace options).
- Produces: navigates to the drawer by selecting a row (drawer is Task 10).

- [ ] **Step 1: Write the failing test** — `web/tests/provision.spec.ts` (tests the submit
builds the right `CreateSandbox` body with an Idempotency-Key; the pure builder is exported
from the component file):

```ts
import { describe, it, expect } from 'vitest'
import { buildCreateBody } from '../app/components/ProvisionModal'

describe('buildCreateBody', () => {
  it('maps the form to a snake_case CreateSandbox body, dropping empties', () => {
    const body = buildCreateBody({
      agent: 'claude', template: 'base', cpus: 2, memory_bytes: 1073741824, disk_gb: 5,
      workspaces: [{ name: 'repo', read_only: true }],
      clone: true, branch: 'feat/x', strategy: 'bin-pack',
      env: { FOO: 'bar' }, labels: {}, node_affinity: {}, node_anti_affinity: {},
    })
    expect(body.agent).toBe('claude')
    expect(body.workspaces).toEqual([{ name: 'repo', read_only: true }])
    expect(body.clone).toBe(true)
    expect(body.branch).toBe('feat/x')
    expect(body.env).toEqual({ FOO: 'bar' })
    expect('labels' in body).toBe(false)        // empty maps dropped
    expect('node_affinity' in body).toBe(false)
  })
})
```

- [ ] **Step 2: Run → FAIL**

Run: `npm --prefix web exec vitest run tests/provision.spec.ts`
Expected: FAIL — cannot resolve `buildCreateBody`.

- [ ] **Step 3: Create the pure builder** — `web/app/components/ProvisionModal.ts`:

```ts
// Maps the Provision form to a CreateSandbox request body (snake_case), dropping empty
// optional maps/strings so the server applies its defaults.
export type ProvisionForm = {
  agent: string; template: string; cpus: number; memory_bytes: number; disk_gb: number
  workspaces: { name: string; read_only: boolean }[]
  clone: boolean; branch: string; strategy: string
  env: Record<string, string>; labels: Record<string, string>
  node_affinity: Record<string, string>; node_anti_affinity: Record<string, string>
}

export function buildCreateBody(f: ProvisionForm): Record<string, any> {
  const body: Record<string, any> = {
    agent: f.agent, template: f.template, cpus: f.cpus,
    memory_bytes: f.memory_bytes, disk_gb: f.disk_gb,
  }
  if (f.workspaces.length) body.workspaces = f.workspaces
  if (f.clone) { body.clone = true; if (f.branch) body.branch = f.branch }
  if (f.strategy) body.strategy = f.strategy
  for (const k of ['env', 'labels', 'node_affinity', 'node_anti_affinity'] as const) {
    if (Object.keys(f[k]).length) body[k] = f[k]
  }
  return body
}
```

- [ ] **Step 4: Run → PASS**

Run: `npm --prefix web exec vitest run tests/provision.spec.ts`
Expected: PASS.

- [ ] **Step 5: Build `ProvisionModal.vue` + `sandboxes/index.vue` with the `nuxt-ui` +
`ui-ux-pro-max` skills** to satisfy:
  - `sandboxes/index.vue`: a `UTable` of `useSwarm().sandboxes` (columns: id, owner_node,
    status, branch, last_publish) with status/label filters; a "Provision" `UButton` opening
    `ProvisionModal`; selecting a row opens `SandboxDrawer` (Task 10) for that id.
  - `ProvisionModal.vue`: a `UModal` with the **tiered** form — visible: agent, template
    (`USelect` from the catalog union = distinct `templates` across `useSwarm().nodes`), cpus,
    memory, disk_gb, workspaces (multi-select from distinct `workspaces` across nodes, each
    with a `read_only` toggle); collapsible **Advanced**: clone+branch, strategy (`USelect`:
    `least-loaded|bin-pack|spread|least-actual-load`), env / labels / node_affinity /
    node_anti_affinity (key-value editors). On submit:
    `useApi().post('/v1/sandboxes', buildCreateBody(form), { 'Idempotency-Key': crypto.randomUUID() })`;
    on success close + toast + `useSwarm().refreshSandboxes()`; on error toast the message
    (covers the clone-on-non-git-workspace rejection).

- [ ] **Step 6: Manual check** — dev server: open the modal, provision a sandbox, see it appear
in the table (via the firehose poke).

- [ ] **Step 7: Commit**

```bash
git add web/app/pages/sandboxes/index.vue web/app/components/ProvisionModal.vue web/app/components/ProvisionModal.ts web/tests/provision.spec.ts
git commit -m "feat(web): sandbox list + tiered provision modal" -m "Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 10: SandboxDrawer shell + Info/Actions/Ports tab

**Files:**
- Create: `web/app/components/SandboxDrawer.vue`
- Create: `web/app/components/drawer/InfoTab.vue`
- Test: `web/tests/drawer-info.spec.ts`

**Interfaces:**
- Consumes: `useApi`, `useSession` (`isAdmin` for gating).
- Produces: a tabbed drawer; later tasks (11, 12) add tabs. Props: `{ id: string }`.

- [ ] **Step 1: Write the failing test** — `web/tests/drawer-info.spec.ts`:

```ts
// @vitest-environment nuxt
import { describe, it, expect, vi } from 'vitest'
import { mountSuspended } from '@nuxt/test-utils/runtime'
import InfoTab from '../app/components/drawer/InfoTab.vue'

const post = vi.fn(async () => ({}))
vi.mock('../app/composables/useApi', () => ({ useApi: () => ({ post, get: vi.fn(async () => ({})) }) }))
vi.mock('../app/composables/useSession', () => ({ useSession: () => ({ isAdmin: ref(true) }) }))

describe('InfoTab actions', () => {
  it('Stop posts to the stop endpoint', async () => {
    const w = await mountSuspended(InfoTab, { props: { sandbox: { id: 'n1.s1', status: 'running', ports: [] } } })
    await w.find('[data-test="stop"]').trigger('click')
    expect(post).toHaveBeenCalledWith('/v1/sandboxes/n1.s1/stop')
  })
})
```

- [ ] **Step 2: Run → FAIL**

Run: `npm --prefix web exec vitest run tests/drawer-info.spec.ts`
Expected: FAIL — `InfoTab.vue` does not exist.

- [ ] **Step 3: Build `SandboxDrawer.vue` + `drawer/InfoTab.vue` with the `nuxt-ui` +
`ui-ux-pro-max` skills** to satisfy:
  - `SandboxDrawer.vue`: a `USlideover`/`UDrawer` with a `UTabs`: **Info**, **Terminal**,
    **Stats**, **Logs**, **Network**, **Secrets**, **Git** (only if the sandbox has a
    `branch`), **Files** (Task 12). Fetches `GET /v1/sandboxes/{id}` on open; passes the
    sandbox to tabs. Closing the drawer unmounts tabs (so their streams close).
  - `InfoTab.vue` props `{ sandbox }`: shows id, owner_node, status, branch, last_publish,
    labels. Action buttons (each with a `data-test` attribute) — **gated by
    `useSession().isAdmin`** (hidden/disabled for read-only): Start `data-test="start"`
    (`POST …/start`), Stop `data-test="stop"` (`POST …/stop`), Delete `data-test="delete"`
    (`DELETE …`, with confirm), KeepAlive `data-test="keepalive"` (`POST …/keepalive`). Ports
    section: `GET …/ports` list + a publish form (`POST …/ports` `{ container_port }`); note
    there is no unpublish endpoint.

- [ ] **Step 4: Run → PASS**

Run: `npm --prefix web exec vitest run tests/drawer-info.spec.ts`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add web/app/components/SandboxDrawer.vue web/app/components/drawer/InfoTab.vue web/tests/drawer-info.spec.ts
git commit -m "feat(web): sandbox drawer shell + info/actions/ports tab (role-gated)" -m "Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 11: Drawer live tabs — Terminal + Stats + Logs

**Files:**
- Create: `web/app/components/drawer/TerminalTab.vue`
- Create: `web/app/components/drawer/StatsTab.vue`
- Create: `web/app/components/drawer/LogsTab.vue`
- Create: `web/app/components/Sparkline.vue`
- Test: `web/tests/sparkline.spec.ts`

**Interfaces:**
- Consumes: `useTerminal` (Task 5), `useApi`, xterm.
- Produces: live tabs mounted by `SandboxDrawer`.

- [ ] **Step 1: Write the failing test** — `web/tests/sparkline.spec.ts` (the only pure-logic
unit here; terminal/stats/logs are manual-smoke since they bind to live streams):

```ts
import { describe, it, expect } from 'vitest'
import { toPoints } from '../app/components/Sparkline'

describe('toPoints', () => {
  it('maps values to an SVG polyline over the given box, scaling to max', () => {
    const pts = toPoints([0, 50, 100], 100, 20) // width=100, height=20
    expect(pts).toBe('0,20 50,10 100,0')
  })
  it('handles an empty series', () => {
    expect(toPoints([], 100, 20)).toBe('')
  })
})
```

- [ ] **Step 2: Run → FAIL**

Run: `npm --prefix web exec vitest run tests/sparkline.spec.ts`
Expected: FAIL — cannot resolve `toPoints`.

- [ ] **Step 3: Create the sparkline math** — `web/app/components/Sparkline.ts`:

```ts
// Maps a numeric series to an SVG polyline "points" string over a width x height box.
// y is inverted (SVG origin top-left); scales to the series max (min 1 to avoid /0).
export function toPoints(values: number[], width: number, height: number): string {
  if (values.length === 0) return ''
  const max = Math.max(1, ...values)
  const step = values.length === 1 ? 0 : width / (values.length - 1)
  return values
    .map((v, i) => `${Math.round(i * step)},${Math.round(height - (v / max) * height)}`)
    .join(' ')
}
```

- [ ] **Step 4: Run → PASS**

Run: `npm --prefix web exec vitest run tests/sparkline.spec.ts`
Expected: PASS.

- [ ] **Step 5: Build the three tabs + `Sparkline.vue` with the `nuxt-ui` + `ui-ux-pro-max`
skills** to satisfy:
  - `Sparkline.vue` props `{ values: number[], label: string }`: renders an SVG `<polyline>`
    from `toPoints(values, w, h)` + the latest value as text.
  - `StatsTab.vue` props `{ id }`: opens `new EventSource('/v1/sandboxes/'+id+'/stats',
    { withCredentials: true })`, listens for `event: stats`, pushes `cpu_percent` and
    `mem_used_kb/mem_total_kb*100` into two ring buffers (cap ~60), feeds two `Sparkline`s.
    Closes the EventSource on unmount (`onScopeDispose`).
  - `LogsTab.vue` props `{ id }`: opens `EventSource('/v1/sandboxes/'+id+'/logs', …)`, listens
    for `event: log`, appends `ev.data` lines to a scrollback `<pre>` (cap ~1000 lines).
    Closes on unmount.
  - `TerminalTab.vue` props `{ id }`: creates an xterm `Terminal` + `FitAddon`, `term.open(el)`;
    builds the IO adapter `{ onData: term.onData.bind(term), write: (b) => term.write(b) }`;
    calls `createTerminal('wss://'+location.host+'/v1/sandboxes/'+id+'/terminal', io,
    () => useApi().post('/v1/sandboxes/'+id+'/keepalive'))`. On fit/resize, call the returned
    `resize(term.cols, term.rows)`. On unmount call `close()` and `term.dispose()`. Import
    `@xterm/xterm/css/xterm.css`.

- [ ] **Step 6: Manual smoke** — dev server: open a running sandbox's drawer; Terminal echoes a
shell, Stats sparklines move, Logs stream. Switch tabs and confirm streams close (no console
errors / leaked connections).

- [ ] **Step 7: Commit**

```bash
git add web/app/components/drawer/TerminalTab.vue web/app/components/drawer/StatsTab.vue web/app/components/drawer/LogsTab.vue web/app/components/Sparkline.vue web/app/components/Sparkline.ts web/tests/sparkline.spec.ts
git commit -m "feat(web): drawer terminal + stats sparkline + logs tabs" -m "Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 12: Drawer mgmt tabs — Network/policy + Secrets + Git + Files-stub

**Files:**
- Create: `web/app/components/drawer/NetworkTab.vue`
- Create: `web/app/components/drawer/SecretsTab.vue`
- Create: `web/app/components/drawer/GitTab.vue`
- Create: `web/app/components/drawer/FilesTab.vue`
- Test: `web/tests/drawer-secrets.spec.ts`

**Interfaces:**
- Consumes: `useApi`, `useSession` (`isAdmin`). Scope = the sandbox id.

- [ ] **Step 1: Write the failing test** — `web/tests/drawer-secrets.spec.ts`:

```ts
// @vitest-environment nuxt
import { describe, it, expect, vi } from 'vitest'
import { mountSuspended } from '@nuxt/test-utils/runtime'
import SecretsTab from '../app/components/drawer/SecretsTab.vue'

const put = vi.fn(async () => ({}))
const get = vi.fn(async () => ({ custom: [], stored: [] }))
vi.mock('../app/composables/useApi', () => ({ useApi: () => ({ put, get, del: vi.fn(async () => ({})) }) }))
vi.mock('../app/composables/useSession', () => ({ useSession: () => ({ isAdmin: ref(true) }) }))

describe('SecretsTab', () => {
  it('adding a secret PUTs scope=id, host, env, value', async () => {
    const w = await mountSuspended(SecretsTab, { props: { id: 'n1.s1' } })
    await w.find('[data-test="secret-host"]').setValue('api.example.com')
    await w.find('[data-test="secret-env"]').setValue('API_KEY')
    await w.find('[data-test="secret-value"]').setValue('s3cr3t')
    await w.find('[data-test="secret-add"]').trigger('click')
    expect(put).toHaveBeenCalledWith('/v1/sandboxes/n1.s1/secrets',
      { scope: 'n1.s1', host: 'api.example.com', env: 'API_KEY', value: 's3cr3t' })
  })
})
```

- [ ] **Step 2: Run → FAIL**

Run: `npm --prefix web exec vitest run tests/drawer-secrets.spec.ts`
Expected: FAIL — `SecretsTab.vue` does not exist.

- [ ] **Step 3: Build the four tabs with the `nuxt-ui` + `ui-ux-pro-max` skills** to satisfy
(all mutations gated by `useSession().isAdmin`):
  - `NetworkTab.vue` props `{ id }`: blocked-egress `UTable` from `GET …/network/blocked`
    (host, first_seen, last_seen + distinct_count header); policy editor — `GET …/policy`
    list + an **add-only** allow/deny form (`PUT …/policy` `{ scope: id, decision, host }`).
    Note in the UI that rules can't be deleted (no remove-rule API).
  - `SecretsTab.vue` props `{ id }`: `GET …/secrets` → masked list of `custom` (host+env) and
    `stored` (names); add form (`data-test` host/env/value + add) →
    `PUT …/secrets { scope: id, host, env, value }`; delete (`DELETE …/secrets/{host}`).
    Value is write-only — never displayed.
  - `GitTab.vue` props `{ sandbox }`: shows `branch` + `last_publish`; a Publish button
    (`POST …/git/publish` `{}`, optional branch override) → toast the returned operation.
  - `FilesTab.vue`: a static "coming soon" `UAlert` (Files API deferred).

- [ ] **Step 4: Run → PASS**

Run: `npm --prefix web exec vitest run tests/drawer-secrets.spec.ts`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add web/app/components/drawer/NetworkTab.vue web/app/components/drawer/SecretsTab.vue web/app/components/drawer/GitTab.vue web/app/components/drawer/FilesTab.vue web/tests/drawer-secrets.spec.ts
git commit -m "feat(web): drawer network/secrets/git tabs + files stub (role-gated)" -m "Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 13: Nodes page (cordon/drain/revoke)

**Files:**
- Create: `web/app/pages/nodes.vue`
- Test: `web/tests/nodes.spec.ts`

**Interfaces:**
- Consumes: `useSwarm` (`nodes`), `useApi`, `useSession` (`isAdmin`).

- [ ] **Step 1: Write the failing test** — `web/tests/nodes.spec.ts`:

```ts
// @vitest-environment nuxt
import { describe, it, expect, vi } from 'vitest'
import { mountSuspended } from '@nuxt/test-utils/runtime'
import Nodes from '../app/pages/nodes.vue'

const post = vi.fn(async () => ({}))
vi.mock('../app/composables/useApi', () => ({ useApi: () => ({ post, get: vi.fn(async () => ({ node_ids: [] })) }) }))
vi.mock('../app/composables/useSession', () => ({ useSession: () => ({ isAdmin: ref(true) }) }))
vi.mock('../app/composables/useSwarm', () => ({
  useSwarm: () => ({
    nodes: ref([{ node_id: 'n1', node_name: 'alpha', cordoned: false, draining: false,
      limit_cpu: 8, alloc_cpu: 0, actual_cpu: 0, limit_mem_kb: 1, alloc_mem_kb: 0,
      templates: [], workspaces: [], labels: {}, capabilities: [] }]),
    refreshNodes: vi.fn(),
  }),
}))

describe('Nodes', () => {
  it('cordon posts the target node_id in the body', async () => {
    const w = await mountSuspended(Nodes)
    await w.find('[data-test="cordon-n1"]').trigger('click')
    expect(post).toHaveBeenCalledWith('/v1/node/cordon', { node_id: 'n1' })
  })
})
```

- [ ] **Step 2: Run → FAIL**

Run: `npm --prefix web exec vitest run tests/nodes.spec.ts`
Expected: FAIL — `nodes.vue` does not exist.

- [ ] **Step 3: Build `nodes.vue` with the `nuxt-ui` + `ui-ux-pro-max` skills** to satisfy:
  - A card or `UTable` per `useSwarm().nodes`: node_name, cordoned/draining badges, CPU/mem
    `limit`/`alloc`/`actual` bars, labels, capabilities, workspaces, templates.
  - Admin actions (gated by `isAdmin`, each with `data-test="<action>-<node_id>"`): Cordon
    (`POST /v1/node/cordon { node_id }`), Uncordon (`/uncordon`), Drain (`/drain`), Revoke
    (`POST /v1/node/revoke { node_id }`, with confirm). Show the revoked list from
    `GET /v1/node/revoked`. After each action call `useSwarm().refreshNodes()`.
  - Empty/standalone: a single self-node renders fine.

- [ ] **Step 4: Run → PASS**

Run: `npm --prefix web exec vitest run tests/nodes.spec.ts`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add web/app/pages/nodes.vue web/tests/nodes.spec.ts
git commit -m "feat(web): nodes page with cordon/drain/revoke (role-gated)" -m "Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 14: Templates + Network/Security + Operations + Settings pages

**Files:**
- Create: `web/app/pages/templates.vue`
- Create: `web/app/pages/network.vue`
- Create: `web/app/pages/operations.vue`
- Create: `web/app/pages/settings.vue`
- Test: `web/tests/operations.spec.ts`

**Interfaces:**
- Consumes: `useApi`, `useSwarm`, `useSession`.

- [ ] **Step 1: Write the failing test** — `web/tests/operations.spec.ts`:

```ts
// @vitest-environment nuxt
import { describe, it, expect, vi } from 'vitest'
import { mountSuspended } from '@nuxt/test-utils/runtime'
import Operations from '../app/pages/operations.vue'

vi.mock('../app/composables/useSwarm', () => ({
  useSwarm: () => ({
    operations: ref([{ id: 'op1', type: 'provision', state: 'done', sandbox_id: 'n1.s1', error: '', created_at: '', updated_at: '' }]),
    refreshOperations: vi.fn(),
  }),
}))

describe('Operations', () => {
  it('lists operations newest-first from the store', async () => {
    const w = await mountSuspended(Operations)
    expect(w.text()).toContain('op1')
    expect(w.text()).toContain('provision')
  })
})
```

- [ ] **Step 2: Run → FAIL**

Run: `npm --prefix web exec vitest run tests/operations.spec.ts`
Expected: FAIL — `operations.vue` does not exist.

- [ ] **Step 3: Build the four pages with the `nuxt-ui` + `ui-ux-pro-max` skills** to satisfy:
  - `templates.vue`: `GET /v1/templates` → `UTable` (repository, tag, id, agent, created_at);
    a second section "which nodes hold each template" derived from `useSwarm().nodes[].templates`.
  - `network.vue` (Network / Security): node-global policy (scope `""`) — `GET
    /v1/sandboxes//policy` list + add-only allow/deny form (`PUT /v1/sandboxes//policy
    { scope: '', decision, host }`); node-global secrets (`GET/PUT/DELETE /v1/sandboxes//secrets`,
    scope `''`). (Per-sandbox blocked egress stays in the drawer.) Mutations gated by `isAdmin`.
  - `operations.vue`: `UTable` of `useSwarm().operations` (id, type, state, sandbox_id, error,
    created_at) newest-first; live via the store's `operation.*` pokes; manual refresh.
  - `settings.vue`: `GET /v1/node` self info (node_id, node_name, version, cordoned, draining,
    role) read-only; a logout button (`useSession().logout()`).

  Note: scope `""` in the path renders as `/v1/sandboxes//policy` (empty segment) — that is the
  node-global scope the gateway expects; do not substitute a placeholder.

- [ ] **Step 4: Run → PASS**

Run: `npm --prefix web exec vitest run tests/operations.spec.ts`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add web/app/pages/templates.vue web/app/pages/network.vue web/app/pages/operations.vue web/app/pages/settings.vue web/tests/operations.spec.ts
git commit -m "feat(web): templates/network/operations/settings pages" -m "Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

---

## Task 15: Whole-app build + final embed verify + manual smoke

**Files:**
- Verify only (no new source); optionally update root `README.md` with the build step.

- [ ] **Step 1: Full Vitest suite**

Run: `npm --prefix web test`
Expected: all spec files PASS.

- [ ] **Step 2: Build the SPA**

Run: `npm --prefix web run build`
Expected: `built web/dist`; `web/dist/` now holds the real bundle (gitignored).

- [ ] **Step 3: Go embed + build against the real bundle**

Run: `go test ./internal/apiserver/ -run TestEmbeddedSPA && go build ./...`
Expected: PASS + clean build (the binary now embeds the real console).

- [ ] **Step 4: Manual smoke against the live daemon** — start a node (`backend: sdk`, live
`sbx` daemon per the repo's integration setup) and open `https://localhost:8443/`:
  - Redirected to `/login`; log in with an **admin** key → dashboard renders.
  - Provision a sandbox (Provision modal) → it appears in the table + on the Overview swarm map.
  - Open the drawer → Terminal echoes a shell; Stats sparklines move; Logs stream.
  - Cordon then uncordon the self node; confirm the badge flips.
  - Log in with a **read-only** key in a second session → admin actions (provision, cordon,
    delete, policy/secret writes) are hidden/disabled.

- [ ] **Step 5: (Optional) README build note** — document that `make web` (or
`web/scripts/build.sh`) must run before `go build` so the binary embeds the console.

- [ ] **Step 6: Commit** (only if README changed)

```bash
git add README.md
git commit -m "docs(web): note the console build step before go build" -m "Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>"
```

- [ ] **Step 7: Hand back to the user for the ff-merge** — do NOT merge unprompted. Summarize
the branch and ask whether to merge `m8b-console` into `main`.

---

## Self-Review

**Spec coverage** (against `2026-06-23-m8b-console-design.md`):
- Backend `role` on `NodeInfo` → Task 1 ✓
- Scaffold + embed/dev wiring (gitignore dist + placeholder, build.sh, Makefile, dev proxy,
  Go embed test) → Task 2 ✓
- `useApi` (CSRF/credentials/401) → Task 3 ✓; `useEvents` (named events, per-type, unsubscribe)
  → Task 4 ✓; `useTerminal` (bidir binary, resize, keepalive) → Task 5 ✓
- Auth (login + global guard + role bootstrap, 401 handling) → Tasks 3, 6 ✓
- App shell + `useSwarm` (single app-wide firehose, coarse poke→refetch, backstop) → Task 7 ✓
- Overview swarm map + stat cards → Task 8 ✓
- Sandboxes list + tiered Provision modal (Idempotency-Key) → Task 9 ✓
- Drawer Info/Actions/ports (role-gated) → Task 10 ✓; live Terminal/Stats(sparkline)/Logs
  → Task 11 ✓; Network/policy(add-only)/Secrets(write-only)/Git/Files-stub → Task 12 ✓
- Nodes (cordon/drain/revoke via `{node_id}`) → Task 13 ✓
- Templates + Network/Security(node-global) + Operations(live) + Settings(logout) → Task 14 ✓
- Whole-app build + embed verify + manual smoke (admin + read-only) → Task 15 ✓
- Deliberate simplifications: swarm map (card grid, no Vue Flow) Tasks 7-8; SVG sparkline
  (no ECharts) Task 11 ✓
- Out of scope honored: Files stub (Task 12), no logout endpoint (client-side, Tasks 6/14),
  policy add-only (Tasks 12/14), no UnpublishPort (Task 10) ✓

**Placeholder scan:** logic units fully coded; every `.vue` task pairs a real Vitest gate with
a concrete component spec (data calls, props, `data-test` hooks, role-gating) — markup is
skill-delegated per the stated convention, not left "TBD". No "add error handling"/"similar to
Task N" steps.

**Type consistency:** `createApi(base, onAuthLost, fetch?) → {get,post(p,b?,h?),put,del}` used
consistently (Tasks 3,6,7,9–14); `createEvents(...)(types, onEvent, opts?)` + `SWARM_EVENT_TYPES`
(Tasks 4,7); `createTerminal(wsUrl, io, keepAlive, opts?) → {resize, close}` (Tasks 5,11);
`createSwarmStore(api, subscribe, opts?) → {nodes,sandboxes,operations,refresh*,stop}`
(Tasks 7,8,13,14); `useSession() → {loggedIn, role, isAdmin, login, loadRole, logout}`
(Tasks 6,7,10,12,13,14); `buildCreateBody(form)` (Task 9); `toPoints(values,w,h)` (Task 11).
Backend `NodeInfo.role` produced in Task 1, consumed in Task 6.
