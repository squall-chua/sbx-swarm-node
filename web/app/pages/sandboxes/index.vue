<script setup lang="ts">
import type { TableColumn } from '@nuxt/ui'

const swarm = useSwarm()
const session = useSession()

// ── Drawer integration point (Task 10) ──────────────────────────────────────
// Row click sets selectedId; the drawer component (Task 10) will mount here.
const selectedId = ref<string | null>(null)
const drawerOpen = ref(false)

// Deep-link: /sandboxes?id=<id> (e.g. from the ⌘K palette) opens that drawer.
const route = useRoute()
function openFromQuery(id: unknown) {
  if (typeof id === 'string' && id) {
    selectedId.value = id
    drawerOpen.value = true
  }
}
onMounted(() => openFromQuery(route.query.id))
watch(() => route.query.id, openFromQuery)

// @nuxt/ui v4 calls onSelect(event, row) — the DOM event is first, the TanStack Row
// (with .original = the data item) second. Take the row as the SECOND arg.
function onRowClick(_e: Event, row: any) {
  selectedId.value = row?.original?.id ?? null
  drawerOpen.value = selectedId.value != null
  // Task 10 will mount <SandboxDrawer :id="selectedId" v-model:open="drawerOpen" /> below.
}

// ── Filters ──────────────────────────────────────────────────────────────────
const statusFilter = ref<string>('All')
const labelFilter = ref<string>('')
const searchFilter = ref<string>('') // partial match on id / owner node

const hasFilters = computed(() =>
  statusFilter.value !== 'All' || !!labelFilter.value || !!searchFilter.value)

const allStatuses = computed(() => {
  const seen = new Set<string>()
  for (const sb of swarm?.sandboxes.value ?? []) {
    if (sb.status) seen.add(sb.status)
  }
  return ['All', ...Array.from(seen).sort()]
})

// Flatten labels to a "key=value key=value" haystack for substring search. The old
// approach matched the raw JSON, which silently failed natural "key=value"/"key:value"
// queries and false-matched JSON punctuation (":", "{").
function labelHaystack(labels: Record<string, string> | undefined): string {
  return Object.entries(labels ?? {}).map(([k, v]) => `${k}=${v}`).join(' ').toLowerCase()
}

const filtered = computed(() => {
  // Normalize the query: trim, lowercase, accept ":" as "=" so both separators work.
  const q = labelFilter.value.trim().toLowerCase().replace(/:/g, '=')
  const s = searchFilter.value.trim().toLowerCase()
  return (swarm?.sandboxes.value ?? []).filter((sb: any) => {
    const matchStatus = statusFilter.value === 'All' || sb.status === statusFilter.value
    const matchLabel = !q || labelHaystack(sb.labels).includes(q)
    const matchSearch = !s
      || String(sb.id ?? '').toLowerCase().includes(s)
      || String(sb.owner_node ?? '').toLowerCase().includes(s)
    return matchStatus && matchLabel && matchSearch
  })
})

// ── Table columns ────────────────────────────────────────────────────────────
const columns: TableColumn<any>[] = [
  {
    accessorKey: 'id',
    header: 'ID',
  },
  {
    accessorKey: 'owner_node',
    header: 'Owner node',
  },
  {
    accessorKey: 'status',
    header: 'Status',
  },
  {
    accessorKey: 'labels',
    header: 'Labels',
  },
  {
    accessorKey: 'branch',
    header: 'Branch',
  },
  {
    accessorKey: 'last_publish',
    header: 'Last publish',
  },
]

// ── Provision modal ──────────────────────────────────────────────────────────
const provisionOpen = ref(false)

// ── Format helpers ───────────────────────────────────────────────────────────
function fmtDate(ts: string | null | undefined): string {
  if (!ts) return '—'
  try {
    return new Date(ts).toLocaleString()
  } catch {
    return ts
  }
}
</script>

<template>
  <!-- Content renders directly into the layout's slot — no extra panel wrapper -->
  <div class="flex flex-col gap-4 p-4 md:p-6">

    <!-- ── Page header / toolbar ─────────────────────────────────────────── -->
    <div class="flex flex-wrap items-center justify-between gap-3">
      <h1 class="text-lg font-semibold text-highlighted">Sandboxes</h1>

      <div class="flex items-center gap-2 flex-wrap">
        <!-- Status filter -->
        <!-- Search: id / owner node (partial) -->
        <UInput
          v-model="searchFilter"
          icon="i-lucide-search"
          placeholder="Search id / owner node…"
          size="sm"
          aria-label="Search by id or owner node"
          class="min-w-48"
        />

        <USelect
          v-model="statusFilter"
          :items="allStatuses"
          size="sm"
          aria-label="Filter by status"
          class="min-w-32"
        />

        <!-- Label search -->
        <UInput
          v-model="labelFilter"
          icon="i-lucide-tag"
          placeholder="Filter by label (key=value)…"
          size="sm"
          aria-label="Filter by label"
          class="min-w-40"
        />

        <!-- Refresh -->
        <UButton
          icon="i-lucide-refresh-cw"
          color="neutral"
          variant="outline"
          size="sm"
          aria-label="Refresh sandboxes"
          @click="swarm?.refreshSandboxes()"
        />

        <!-- Provision — admin only -->
        <UButton
          v-if="session.isAdmin.value"
          icon="i-lucide-plus"
          label="Provision"
          size="sm"
          @click="provisionOpen = true"
        />
      </div>
    </div>

    <!-- ── Table ─────────────────────────────────────────────────────────── -->
    <UTable
      :data="filtered"
      :columns="columns"
      class="w-full"
      :ui="{ tr: 'cursor-pointer hover:bg-elevated/60 transition-colors' }"
      @select="onRowClick"
    >
      <!-- ID column: monospace -->
      <template #id-cell="{ row }">
        <span class="font-mono text-sm text-default">{{ row.original.id }}</span>
      </template>

      <!-- Owner node: monospace -->
      <template #owner_node-cell="{ row }">
        <span class="font-mono text-sm text-muted">{{ row.original.owner_node ?? '—' }}</span>
      </template>

      <!-- Status -->
      <template #status-cell="{ row }">
        <StatusPill :status="row.original.status ?? 'unknown'" kind="sandbox" />
      </template>

      <!-- Labels: key=value badges -->
      <template #labels-cell="{ row }">
        <div
          v-if="row.original.labels && Object.keys(row.original.labels).length"
          class="flex flex-wrap gap-1"
        >
          <UBadge
            v-for="(v, k) in row.original.labels"
            :key="k"
            :label="`${k}=${v}`"
            color="neutral"
            variant="subtle"
            size="xs"
          />
        </div>
        <span v-else class="text-muted">—</span>
      </template>

      <!-- Branch: monospace -->
      <template #branch-cell="{ row }">
        <span
          v-if="row.original.branch"
          class="font-mono text-xs text-muted"
        >{{ row.original.branch }}</span>
        <span v-else class="text-muted">—</span>
      </template>

      <!-- Last publish: formatted date -->
      <template #last_publish-cell="{ row }">
        <span class="text-xs text-muted tabular-nums">
          {{ fmtDate(row.original.last_publish) }}
        </span>
      </template>

      <!-- Empty state -->
      <template #empty>
        <div class="flex flex-col items-center justify-center gap-2 py-12 text-center">
          <UIcon name="i-lucide-box" class="size-8 text-muted" aria-hidden="true" />
          <p class="text-sm text-muted">
            {{ hasFilters ? 'No sandboxes match the current filters.' : 'No sandboxes yet.' }}
          </p>
          <UButton
            v-if="session.isAdmin.value && !hasFilters"
            label="Provision your first sandbox"
            variant="ghost"
            size="sm"
            @click="provisionOpen = true"
          />
        </div>
      </template>
    </UTable>

    <!-- Row count -->
    <p
      v-if="filtered.length > 0"
      class="text-xs text-muted tabular-nums"
    >
      {{ filtered.length }} sandbox{{ filtered.length === 1 ? '' : 'es' }}
      <template v-if="hasFilters">
        (filtered from {{ swarm?.sandboxes.value.length ?? 0 }} total)
      </template>
    </p>

    <!-- Sandbox drawer — mounts on row click; guard for null selectedId -->
    <SandboxDrawer
      v-if="selectedId"
      :id="selectedId"
      v-model:open="drawerOpen"
    />

  </div>

  <!-- Provision modal -->
  <ProvisionModal
    v-model:open="provisionOpen"
  />
</template>
