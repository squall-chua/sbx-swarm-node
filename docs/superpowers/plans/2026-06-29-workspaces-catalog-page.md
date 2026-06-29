# Workspaces Catalog Page Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a console **Workspaces** page — a table cataloguing every workspace advertised across the swarm (one row per name), showing its advertising node(s), whether it's git-backed, and how many sandboxes mount it (expandable to the sandbox list) — and expose the git-backed flag through the node API.

**Architecture:** A new additive `git_workspaces` field on `NodeSummary` (subset of `workspaces` whose node config has a `git:` block) is populated from config and rides gossip exactly like `workspaces`. The page is a pure client-side join of `swarm.nodes` (advertisers + git flag) and `swarm.sandboxes` (mounts) — no new endpoint. The aggregation lives in a pure, unit-tested util; the page renders it with `UTable` + expandable rows.

**Tech Stack:** Go (`internal/node`, `internal/apiserver`, `internal/membership`), protobuf via `buf`, Nuxt 4 + `@nuxt/ui` v4 console, Vitest.

## Global Constraints

- **Spec:** `docs/superpowers/specs/2026-06-29-workspaces-catalog-page-design.md`. Glossary terms **Workspace** / **Git-backed workspace** in `CONTEXT.md` (don't re-litigate).
- **Workspace identity = logical name**; the catalog groups by name (matches scheduler placement). UI label is "Provided by"; the domain verb is **advertise**.
- **Git-backed = advertised, not inferred.** `gitBacked` = any advertiser lists the name in `git_workspaces`; `gitMixed` = advertisers disagree → badge "git (mixed)" + tooltip naming the non-git advertiser(s).
- **No aggregate access column** — per-mount access shows only in the expanded sandbox list.
- **Mounts are connected-node-scoped** (inherited: `ListSandboxes` is local-only). The catalog uses the same `/v1/sandboxes` the Sandboxes page uses; remote-node mounts aren't shown. Don't attempt swarm-wide sandbox aggregation.
- **`git_workspaces` field number is 17** (next free in `NodeSummary`).
- **gofmt only the files you touch** (repo is broadly gofmt-dirty but unenforced).
- **Go tests:** plain `go test ./internal/node/ ./internal/apiserver/` (no bare-repo env override needed).
- **Web build** after `.vue`/`.ts` changes: `cd web && bash scripts/build.sh` (the Go binary embeds `web/dist`). Web tests: focused `npm --prefix web test -- tests/X.spec.ts` (keeps cwd=web; do NOT use `npm exec`); full suite `npm --prefix web test`.
- **Proto:** this feature changes `node.proto` → run `buf generate` after the edit.

---

### Task 1: Backend — expose `git_workspaces` on NodeSummary

**Files:**
- Modify: `proto/sbxswarm/v1/node.proto` (add field; regenerate `internal/gen/...` via `buf generate`)
- Modify: `internal/membership/state.go` (add `GitWorkspaces` to `NodeState` bulk section)
- Modify: `internal/apiserver/nodeservice.go` (add `GitWorkspaces` to `NodeRow` + proto mapping)
- Modify: `internal/node/node.go` (add `gitWorkspaceNames` helper + populate 3 sites)
- Test: `internal/node/node_test.go` (helper unit test), `internal/apiserver/nodeservice_test.go` (proto-mapping test)

**Interfaces:**
- Produces:
  - proto `NodeSummary.git_workspaces` (`repeated string`, field 17) → REST JSON `git_workspaces` (snake_case via `UseProtoNames`)
  - `membership.NodeState.GitWorkspaces []string` (`json:"git_ws,omitempty"`)
  - `apiserver.NodeRow.GitWorkspaces []string`
  - `func gitWorkspaceNames(ws []config.WorkspaceConfig) []string` (names where `w.Git != nil`)
- Consumes: `config.WorkspaceConfig.Git` (non-nil ⇒ git-backed).

- [ ] **Step 1: Add the proto field and regenerate**

In `proto/sbxswarm/v1/node.proto`, in `message NodeSummary`, after `double actual_mem = 16;` add:

```proto
  repeated string git_workspaces = 17; // subset of `workspaces` whose node config is git-backed
```

Then run: `buf generate`
Verify: `grep -n "GitWorkspaces" internal/gen/sbxswarm/v1/node.pb.go` shows a generated `GetGitWorkspaces()`/field.

- [ ] **Step 2: Write the failing helper test**

In `internal/node/node_test.go` (the package already imports `config` and `require`), add:

```go
func TestGitWorkspaceNames(t *testing.T) {
	ws := []config.WorkspaceConfig{
		{Name: "repo", Git: &config.GitConfig{}},
		{Name: "plain"},
		{Name: "repo2", Git: &config.GitConfig{Remote: "git@x:y.git"}},
	}
	require.Equal(t, []string{"repo", "repo2"}, gitWorkspaceNames(ws))
	require.Empty(t, gitWorkspaceNames([]config.WorkspaceConfig{{Name: "plain"}}))
}
```

- [ ] **Step 3: Run it to verify it fails**

Run: `go test ./internal/node/ -run TestGitWorkspaceNames`
Expected: FAIL — `undefined: gitWorkspaceNames`.

- [ ] **Step 4: Add the helper and the struct fields, then wire population + mapping**

In `internal/node/node.go`, next to `workspaceNames` (~L598), add:

```go
// gitWorkspaceNames returns the names of the git-backed workspaces (those whose
// config has a git block, ADR-0015). Subset of workspaceNames.
func gitWorkspaceNames(ws []config.WorkspaceConfig) []string {
	out := make([]string, 0, len(ws))
	for _, w := range ws {
		if w.Git != nil {
			out = append(out, w.Name)
		}
	}
	return out
}
```

In `internal/membership/state.go`, in `NodeState` (bulk section, right after the `Workspaces` field):

```go
	GitWorkspaces   []string          `json:"git_ws,omitempty"`
```

In `internal/apiserver/nodeservice.go`, in `type NodeRow struct`, add a line after `Capabilities, Workspaces, Templates []string`:

```go
	GitWorkspaces                       []string
```

In `internal/apiserver/nodeservice.go`, the `ListNodes` proto builder (~L183), add `GitWorkspaces: r.GitWorkspaces,` to the `&sbxv1.NodeSummary{...}` literal (alongside `Workspaces: r.Workspaces`):

```go
			Labels: r.Labels, Capabilities: r.Capabilities, Workspaces: r.Workspaces, Templates: r.Templates, GitWorkspaces: r.GitWorkspaces,
```

In `internal/node/node.go`, add `GitWorkspaces: gitWorkspaceNames(cfg.Workspaces),` at the two local build sites — the gossip `localNS := membership.NodeState{...}` literal (after `Workspaces: workspaceNames(cfg.Workspaces),`, ~L206) and the self `apiserver.NodeRow{...}` literal in `SetNodeLister` (after `Workspaces: workspaceNames(cfg.Workspaces),`, ~L248). And in `rowFromState` (~L618) add `GitWorkspaces: ns.GitWorkspaces,` to the returned `apiserver.NodeRow{...}` (alongside `Workspaces: ns.Workspaces`).

- [ ] **Step 5: Run the helper test (GREEN) + build**

Run: `go test ./internal/node/ -run TestGitWorkspaceNames && go build ./...`
Expected: PASS; build clean.

- [ ] **Step 6: Write the proto-mapping test**

In `internal/apiserver/nodeservice_test.go`, add (mirrors `TestNodeService_ListNodes`):

```go
func TestNodeService_ListNodes_GitWorkspaces(t *testing.T) {
	svc := NewNodeService("n1", "node-one", "test")
	svc.SetNodeLister(func() []NodeRow {
		return []NodeRow{
			{NodeID: "n1", Workspaces: []string{"repo", "plain"}, GitWorkspaces: []string{"repo"}},
		}
	})
	resp, err := svc.ListNodes(context.Background(), &sbxv1.ListNodesRequest{})
	require.NoError(t, err)
	require.Len(t, resp.Nodes, 1)
	require.Equal(t, []string{"repo", "plain"}, resp.Nodes[0].Workspaces)
	require.Equal(t, []string{"repo"}, resp.Nodes[0].GitWorkspaces)
}
```

- [ ] **Step 7: Run tests, gofmt, commit**

Run: `go test ./internal/node/ ./internal/apiserver/ && go vet ./internal/node/ ./internal/apiserver/ ./internal/membership/`
Expected: PASS; vet clean.

```bash
gofmt -w proto/sbxswarm/v1/node.proto internal/membership/state.go internal/apiserver/nodeservice.go internal/node/node.go internal/node/node_test.go internal/apiserver/nodeservice_test.go
git add proto/sbxswarm/v1/node.proto internal/gen/sbxswarm/v1/ internal/membership/state.go internal/apiserver/nodeservice.go internal/node/node.go internal/node/node_test.go internal/apiserver/nodeservice_test.go
git commit -m "feat(workspaces): expose git_workspaces on NodeSummary (advertised git-backed flag)"
```

> Note: `gofmt` does not format `.proto`; it is listed harmlessly (no-op). The generated `internal/gen/...` is committed as part of the proto change.

---

### Task 2: Frontend — pure workspace-catalog derivation

**Files:**
- Create: `web/app/utils/workspaceCatalog.ts`
- Test: `web/tests/workspaceCatalog.spec.ts`

**Interfaces:**
- Produces: `buildWorkspaceCatalog(nodes, sandboxes): WorkspaceRow[]` (auto-imported in Nuxt from `app/utils/`), with exported types `WorkspaceRow { name, providers, gitBacked, gitMixed, nonGitProviders, mounts }` and `WorkspaceMount { id, node, status, branch, readOnly }`.
- Consumes: node objects (`{ node_name, workspaces?, git_workspaces? }`) and sandbox objects (`{ id, owner_node?, status?, branch?, workspaces?: {name, read_only?}[] }`) — the shapes the store already holds.

- [ ] **Step 1: Write the failing unit test**

Create `web/tests/workspaceCatalog.spec.ts` (plain Vitest — no nuxt env needed):

```ts
import { describe, it, expect } from 'vitest'
import { buildWorkspaceCatalog } from '../app/utils/workspaceCatalog'

describe('buildWorkspaceCatalog', () => {
  it('aggregates advertisers, git flag, and mounts by name', () => {
    const nodes = [{ node_name: 'dev-node', workspaces: ['repo', 'plain'], git_workspaces: ['repo'] }]
    const sandboxes = [
      { id: 's1', owner_node: 'dev-node', status: 'running', branch: 'main', workspaces: [{ name: 'repo', read_only: false }] },
      { id: 's2', owner_node: 'dev-node', status: 'stopped', branch: '', workspaces: [{ name: 'repo', read_only: true }] },
    ]
    const rows = buildWorkspaceCatalog(nodes, sandboxes)
    const repo = rows.find(r => r.name === 'repo')!
    expect(repo.providers).toEqual(['dev-node'])
    expect(repo.gitBacked).toBe(true)
    expect(repo.gitMixed).toBe(false)
    expect(repo.mounts).toHaveLength(2)
    expect(repo.mounts[1].readOnly).toBe(true)
    const plain = rows.find(r => r.name === 'plain')!
    expect(plain.gitBacked).toBe(false)
    expect(plain.mounts).toHaveLength(0)
  })

  it('flags gitMixed when advertisers disagree', () => {
    const nodes = [
      { node_name: 'a', workspaces: ['repo'], git_workspaces: ['repo'] },
      { node_name: 'b', workspaces: ['repo'], git_workspaces: [] },
    ]
    const rows = buildWorkspaceCatalog(nodes, [])
    expect(rows[0].gitBacked).toBe(true)
    expect(rows[0].gitMixed).toBe(true)
    expect(rows[0].nonGitProviders).toEqual(['b'])
  })

  it('falls back to mount nodes when no node advertises the workspace', () => {
    const rows = buildWorkspaceCatalog([], [{ id: 's1', owner_node: 'dev-node', workspaces: [{ name: 'ghost' }] }])
    expect(rows[0].providers).toEqual(['dev-node'])
    expect(rows[0].gitBacked).toBe(false)
  })
})
```

- [ ] **Step 2: Run it to verify it fails**

Run: `npm --prefix web test -- tests/workspaceCatalog.spec.ts`
Expected: FAIL — cannot resolve `../app/utils/workspaceCatalog`.

- [ ] **Step 3: Implement the derivation**

Create `web/app/utils/workspaceCatalog.ts`:

```ts
export interface WorkspaceMount {
  id: string
  node: string
  status: string
  branch: string
  readOnly: boolean
}

export interface WorkspaceRow {
  name: string
  providers: string[]
  gitBacked: boolean
  gitMixed: boolean
  nonGitProviders: string[]
  mounts: WorkspaceMount[]
}

interface NodeLike { node_name: string, workspaces?: string[], git_workspaces?: string[] }
interface MountLike { name: string, read_only?: boolean }
interface SandboxLike { id: string, owner_node?: string, status?: string, branch?: string, workspaces?: MountLike[] }

// buildWorkspaceCatalog groups workspaces by logical name across the swarm:
// advertisers (and which of them mark it git-backed) from nodes, mounts from
// sandboxes. A workspace seen only via a mount (no advertiser) still gets a row.
export function buildWorkspaceCatalog(nodes: NodeLike[], sandboxes: SandboxLike[]): WorkspaceRow[] {
  type Acc = { providers: Set<string>, gitProviders: Set<string>, nonGitProviders: Set<string>, mounts: WorkspaceMount[] }
  const map = new Map<string, Acc>()
  const ensure = (name: string): Acc => {
    let e = map.get(name)
    if (!e) { e = { providers: new Set(), gitProviders: new Set(), nonGitProviders: new Set(), mounts: [] }; map.set(name, e) }
    return e
  }

  for (const n of nodes ?? []) {
    const git = new Set(n.git_workspaces ?? [])
    for (const w of n.workspaces ?? []) {
      const e = ensure(w)
      e.providers.add(n.node_name)
      ;(git.has(w) ? e.gitProviders : e.nonGitProviders).add(n.node_name)
    }
  }
  for (const s of sandboxes ?? []) {
    for (const m of s.workspaces ?? []) {
      ensure(m.name).mounts.push({
        id: s.id,
        node: s.owner_node ?? '',
        status: s.status ?? '',
        branch: s.branch ?? '',
        readOnly: !!m.read_only,
      })
    }
  }

  const rows: WorkspaceRow[] = []
  for (const [name, e] of map) {
    const providers = e.providers.size
      ? [...e.providers]
      : [...new Set(e.mounts.map(m => m.node).filter(Boolean))]
    rows.push({
      name,
      providers: providers.sort(),
      gitBacked: e.gitProviders.size > 0,
      gitMixed: e.gitProviders.size > 0 && e.nonGitProviders.size > 0,
      nonGitProviders: [...e.nonGitProviders].sort(),
      mounts: e.mounts,
    })
  }
  return rows.sort((a, b) => a.name.localeCompare(b.name))
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `npm --prefix web test -- tests/workspaceCatalog.spec.ts`
Expected: PASS (3 tests).

- [ ] **Step 5: Commit**

```bash
git add web/app/utils/workspaceCatalog.ts web/tests/workspaceCatalog.spec.ts
git commit -m "feat(web): workspace catalog derivation (group by name, git flag, mounts)"
```

---

### Task 3: Frontend — Workspaces page + nav

**Files:**
- Create: `web/app/pages/workspaces.vue`
- Modify: `web/app/layouts/default.vue` (nav item)
- Test: `web/tests/workspaces.spec.ts`

**Interfaces:**
- Consumes: `buildWorkspaceCatalog` (Task 2), `useSwarm()` (`nodes`, `sandboxes`, `refreshAll`), auto-imported `StatusPill`, `navigateTo`.

- [ ] **Step 1: Write the failing page test**

Create `web/tests/workspaces.spec.ts`:

```ts
// @vitest-environment nuxt
import { ref } from 'vue'
import { describe, it, expect, vi } from 'vitest'
import { flushPromises } from '@vue/test-utils'
import { mountSuspended } from '@nuxt/test-utils/runtime'
import Workspaces from '../app/pages/workspaces.vue'

vi.mock('../app/composables/useSwarm', () => ({
  useSwarm: () => ({
    nodes: ref([{ node_name: 'dev-node', workspaces: ['repo'], git_workspaces: ['repo'] }]),
    sandboxes: ref([{ id: 'n1.s1', owner_node: 'dev-node', status: 'running', branch: 'main', workspaces: [{ name: 'repo', read_only: false }] }]),
    refreshAll: vi.fn(),
  }),
}))

describe('Workspaces', () => {
  it('lists a git-backed workspace with its mount count and expands to the sandbox', async () => {
    const w = await mountSuspended(Workspaces)
    expect(w.text()).toContain('repo')
    expect(w.text()).toContain('git')
    expect(w.text()).toContain('1 sandbox')
    await w.find('[data-test="expand-repo"]').trigger('click')
    await flushPromises()
    expect(w.text()).toContain('n1.s1')
  })
})
```

- [ ] **Step 2: Run it to verify it fails**

Run: `npm --prefix web test -- tests/workspaces.spec.ts`
Expected: FAIL — cannot resolve `../app/pages/workspaces.vue`.

- [ ] **Step 3: Implement the page**

Create `web/app/pages/workspaces.vue`:

```vue
<script setup lang="ts">
const swarm = useSwarm()
const expanded = ref<Record<string, boolean>>({})

const rows = computed(() => buildWorkspaceCatalog(swarm.nodes.value as any[], swarm.sandboxes.value as any[]))

const columns = [
  { id: 'expand' },
  { accessorKey: 'name', header: 'Workspace' },
  { accessorKey: 'providers', header: 'Provided by' },
  { accessorKey: 'git', header: 'Git' },
  { accessorKey: 'mounts', header: 'Mounts' },
]
function mountsLabel(n: number): string {
  return n === 0 ? 'none' : `${n} sandbox${n > 1 ? 'es' : ''}`
}
</script>

<template>
  <!-- Renders directly into layout slot — no extra panel wrapper -->
  <div class="flex flex-col gap-4 p-4 md:p-6">
    <div class="flex items-center justify-between gap-3">
      <h1 class="text-xl font-semibold text-highlighted">Workspaces</h1>
      <UButton icon="i-lucide-refresh-cw" size="sm" color="neutral" variant="outline" label="Refresh" @click="swarm.refreshAll()" />
    </div>

    <UTable
      v-model:expanded="expanded"
      :data="rows"
      :columns="columns"
      :expanded-options="{ getRowCanExpand: () => true }"
      :get-row-id="(r: any) => r.name"
      data-test="ws-table"
    >
      <template #expand-cell="{ row }">
        <UButton
          :icon="row.getIsExpanded() ? 'i-lucide-chevron-down' : 'i-lucide-chevron-right'"
          color="neutral"
          variant="ghost"
          size="xs"
          :disabled="!row.original.mounts.length"
          :data-test="`expand-${row.original.name}`"
          @click="row.toggleExpanded()"
        />
      </template>

      <template #name-cell="{ row }">
        <span class="font-mono text-default">{{ row.original.name }}</span>
      </template>

      <template #providers-cell="{ row }">
        <div class="flex flex-wrap gap-1">
          <UBadge v-for="p in row.original.providers" :key="p" :label="p" color="neutral" variant="subtle" size="xs" class="font-mono" />
        </div>
      </template>

      <template #git-cell="{ row }">
        <UBadge
          v-if="row.original.gitBacked"
          :label="row.original.gitMixed ? 'git (mixed)' : 'git'"
          icon="i-lucide-git-branch"
          color="primary"
          variant="subtle"
          size="xs"
          :title="row.original.gitMixed ? `Not git-backed on: ${row.original.nonGitProviders.join(', ')}` : undefined"
        />
        <span v-else class="text-dimmed">—</span>
      </template>

      <template #mounts-cell="{ row }">
        <span :class="row.original.mounts.length ? 'text-default' : 'text-dimmed'">{{ mountsLabel(row.original.mounts.length) }}</span>
      </template>

      <template #expanded="{ row }">
        <div class="flex flex-col gap-1 py-1 pl-8">
          <div
            v-for="m in row.original.mounts"
            :key="m.id"
            class="flex items-center gap-3 rounded px-2 py-1 text-xs cursor-pointer hover:bg-elevated/50"
            :data-test="`mount-${m.id}`"
            @click="navigateTo('/sandboxes')"
          >
            <span class="font-mono text-default truncate max-w-50">{{ m.id }}</span>
            <span class="text-muted">{{ m.node }}</span>
            <StatusPill v-if="m.status" :status="m.status" kind="sandbox" size="xs" />
            <span v-if="m.branch" class="font-mono text-muted">{{ m.branch }}</span>
            <span class="text-muted">{{ m.readOnly ? 'read-only' : 'writable' }}</span>
          </div>
        </div>
      </template>

      <template #empty>
        <div class="py-8 text-center text-muted">No workspaces advertised in the swarm.</div>
      </template>
    </UTable>
  </div>
</template>
```

- [ ] **Step 4: Add the nav item**

In `web/app/layouts/default.vue`, in the `navItems` array, after the Templates line (`{ label: 'Templates', icon: 'i-lucide-file-code', to: '/templates' },`) add:

```js
  { label: 'Workspaces', icon: 'i-lucide-folder-git-2', to: '/workspaces' },
```

- [ ] **Step 5: Run the page test (GREEN)**

Run: `npm --prefix web test -- tests/workspaces.spec.ts`
Expected: PASS. If the expand assertion fails, confirm the `:expanded-options="{ getRowCanExpand: () => true }"` prop and the `#expanded` slot are present (UTable v4 only renders the expanded row when the row can expand and `row.toggleExpanded()` has fired).

- [ ] **Step 6: Run the whole web suite + build the SPA**

Run: `npm --prefix web test && cd web && bash scripts/build.sh`
Expected: all specs pass; build succeeds (Go embeds `web/dist`).

- [ ] **Step 7: Commit**

```bash
git add web/app/pages/workspaces.vue web/app/layouts/default.vue web/tests/workspaces.spec.ts
git commit -m "feat(web): Workspaces catalog page (advertisers, git badge, mounts, expand)"
```

---

## Final verification

- [ ] `go test ./internal/node/ ./internal/apiserver/ ./internal/membership/ 2>&1 | grep -E 'FAIL|panic'` is empty.
- [ ] `go build ./... && go vet ./internal/...` clean.
- [ ] `npm --prefix web test` green; `cd web && bash scripts/build.sh` succeeds.
- [ ] Manual smoke (optional, live node): open the console → **Workspaces** in the nav → the swarm's workspaces appear; a git-backed one shows the **git** badge; a workspace with sandboxes shows the **Mounts** count and expands to the sandbox list. (REST check: `curl -sk -H "Authorization: Bearer <admin-key>" https://localhost:8444/v1/nodes` includes `git_workspaces` per node.)
