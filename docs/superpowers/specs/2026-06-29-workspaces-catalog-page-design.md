# Workspaces Catalog Page — Design

**Date:** 2026-06-29 · **Branch:** `m8b-console`

## Goal

Add a **Workspaces** page to the console: a table cataloguing every workspace provided across the swarm, one row per distinct workspace name, with its providing node(s), whether it is **git-backed**, how many sandboxes mount it, the access mode, and — on expand — the list of sandboxes that mount it. To make the git-backed flag authoritative, expose it through the node API (it is known to the node from config but not currently surfaced).

## Background / Data model

- A node advertises the workspaces it provides as a list of **names**: `NodeSummary.workspaces` (`repeated string`), populated from `config.WorkspaceConfig.Name`.
- A workspace is **git-backed iff its config has a `git:` block** — `config.WorkspaceConfig.Git != nil` ("non-nil => git-backed (clone-only, ADR-0015)"). The node already computes this (`buildGitWorkspaces`), but only the *name* reaches the API.
- A sandbox records the workspaces it **mounts**: `Sandbox.workspaces` (`repeated WorkspaceMount { name, read_only }`), plus the sandbox's own `owner_node`, `status`, and `branch`.
- The console already fetches both lists into the `useSwarm` store (`swarm.nodes`, `swarm.sandboxes`), auto-refreshed via SSE pokes + a 25 s backstop. Node cards already render `node.workspaces` as badges.

The catalog is therefore a **client-side join** of `swarm.nodes` (providers + git flag) and `swarm.sandboxes` (mounts). The only backend work is exposing the git-backed flag.

## Non-goals

- No new `/v1/workspaces` endpoint — the two source lists already exist and are already in the store. Aggregation is a trivial client-side join.
- No write actions (create/delete workspaces) — read-only catalog.
- No change to scheduler placement, which keys on workspace *names* only and is unaffected by the additive field.

## Backend change — expose git-backed (additive)

Mirror the existing `workspaces` (names) field with a parallel `git_workspaces` carrying the subset of names that are git-backed. Additive everywhere; no existing type changes, so scheduler/gossip name-set logic is untouched.

1. **proto** `proto/sbxswarm/v1/node.proto`, `message NodeSummary`: add `repeated string git_workspaces = 17;` (next free field number). Run `buf generate`.
2. **`internal/membership/state.go`** `NodeState`: add `GitWorkspaces []string` in the **bulk** section (alongside `Workspaces`, with a `json:"git_ws,omitempty"` tag) so it propagates on the same TCP push/pull path. (Bulk fields are NOT propagated by `ml.UpdateNode`; that matches `Workspaces` today — no new propagation path needed.)
3. **`internal/apiserver/nodeservice.go`** `NodeRow`: add `GitWorkspaces []string`. In the `ListNodes` proto builder (~L183) map `GitWorkspaces: r.GitWorkspaces`.
4. **`internal/node/node.go`**:
   - Add helper `gitWorkspaceNames(ws []config.WorkspaceConfig) []string` → names where `w.Git != nil`.
   - Self `NodeRow` build (~L206): `GitWorkspaces: gitWorkspaceNames(cfg.Workspaces)`.
   - Gossip `NodeState` build (~L248): `GitWorkspaces: gitWorkspaceNames(cfg.Workspaces)`.
   - `rowFromState` (~L618, peer view): `GitWorkspaces: ns.GitWorkspaces`.
5. REST JSON stays snake_case (gateway uses `UseProtoNames`) → field serialises as `git_workspaces`.

### Backend test

Go test (node-service level, fake-backed): a config with one git-backed workspace (`Git != nil`) and one plain workspace → `ListNodes` row has the git-backed name in `git_workspaces` and **not** the plain one; `workspaces` still lists both. Asserts `gitWorkspaceNames` selection and the proto/REST mapping.

## Frontend

### Page & navigation

- `web/app/pages/workspaces.vue`.
- Nav item in `web/app/layouts/default.vue`: `{ label: 'Workspaces', icon: 'i-lucide-folder-git-2', to: '/workspaces' }`, placed after **Templates**.
- Extend the web `NodeRow` type (used in `nodes.vue`, and the page) with `git_workspaces?: string[]`.

### Catalog derivation (computed from the store)

Group by workspace name across `swarm.nodes` and `swarm.sandboxes`:

```
type Mount = { id: string; node: string; status: string; branch: string; readOnly: boolean }
type Row   = { name: string; providers: string[]; gitBacked: boolean; mounts: Mount[] }
```

- For each node: for each `n.workspaces[]` → add `n.node_name` to that workspace's `providers`; if the name is in `n.git_workspaces` → `gitBacked = true`.
- For each sandbox: for each `s.workspaces[]` (`{name, read_only}`) → push `{ id: s.id, node: s.owner_node, status: s.status, branch: s.branch, readOnly: read_only }` onto that workspace's `mounts`.
- Defensive: a workspace that appears only in a sandbox mount (no node advertises it) still gets a row; `providers` falls back to the distinct `owner_node`s of its mounts.
- Sort rows by name.

### Top-level table (`UTable`, @nuxt/ui v4 API: `:data`, `accessorKey`/`header`, `#<key>-cell`)

One row per workspace:

| Column | Content |
|---|---|
| Workspace | name, mono |
| Provided by | `UBadge` per provider node (reuse the node-card chip style: `color="neutral" variant="subtle" size="xs"`) |
| Git | git-backed → `UBadge color="primary" variant="subtle"` "git" with `i-lucide-git-branch`; else muted "—" |
| In use | `mounts.length` → "N sandbox(es)"; 0 → muted "unused" |
| Access | derived from `mounts`: all RO → "read-only"; all RW → "writable"; both → "mixed"; none → "—" |

### Expanded row — mounting sandboxes (the requested list)

Expanding a workspace row reveals a compact inner list of its `mounts`, one line each: **sandbox id** (mono, truncated), **node**, **status** (`StatusPill` if convenient, else text), **branch**, **access** (read-only / writable). Clicking a sandbox line navigates to `/sandboxes` (`navigateTo('/sandboxes')`) — no drawer cross-wiring. Workspaces with zero mounts are not expandable (or expand to an "Not mounted by any sandbox" line).

### Page chrome

- In-content header with the title and a **Refresh** button calling `swarm.refreshAll()` (consistent with other pages).
- Empty state (`UAlert` or simple centered text) when the catalog is empty.
- Read-only view available to any authenticated user (no admin gate) — it only reflects list data already visible on the Nodes/Sandboxes pages.

### Frontend test

Vitest (`web/tests/workspaces.spec.ts`), nuxt env, mocking `useSwarm` with:
- node `dev-node` providing `workspaces: ['repo', 'plain']`, `git_workspaces: ['repo']`
- two sandboxes mounting `repo` (one read-only, one writable, one with `branch: 'main'`), zero mounting `plain`

Assert: row `repo` shows provider `dev-node`, a **git** badge, in-use "2 sandboxes", access "mixed"; row `plain` shows **no** git badge and "unused". Expanding `repo` lists both sandbox ids.

## Files

- `proto/sbxswarm/v1/node.proto` (+ regenerated `*.pb.go` via `buf generate`)
- `internal/membership/state.go`, `internal/apiserver/nodeservice.go`, `internal/node/node.go`
- `internal/apiserver/nodeservice_test.go` (or node-level test) — backend test
- `web/app/pages/workspaces.vue` (new), `web/app/layouts/default.vue` (nav), `web/app/pages/nodes.vue` (NodeRow type — or wherever the shared type lives)
- `web/tests/workspaces.spec.ts` (new)
- `web/dist` rebuilt (gitignored; embed via `make build`)

## Risks / notes

- `git_workspaces` must ride gossip for **remote** nodes' badges to be correct — handled by adding it to `NodeState` bulk fields (same path as `Workspaces`). Single-node deployments use the local self-row path and are unaffected by gossip.
- `buf generate` is required (proto change) — unlike the file-transfer feature.
