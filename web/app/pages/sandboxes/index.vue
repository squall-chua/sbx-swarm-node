<script setup lang="ts">
import type { TableColumn } from '@nuxt/ui'

const swarm = useSwarm()
const session = useSession()

// ── Drawer integration point (Task 10) ──────────────────────────────────────
// Row click sets selectedId; the drawer component (Task 10) will mount here.
const selectedId = ref<string | null>(null)
const drawerOpen = ref(false)

// @nuxt/ui v4 calls onSelect(event, row) — the DOM event is first, the TanStack Row
// (with .original = the data item) second. Take the row as the SECOND arg.
function onRowClick(_e: Event, row: any) {
  selectedId.value = row?.original?.id ?? null
  drawerOpen.value = selectedId.value != null
  // Task 10 will mount <SandboxDrawer :id="selectedId" v-model:open="drawerOpen" /> below.
}

// ── Status → color map (from design-language.md) ────────────────────────────
function statusColor(status: string): string {
  switch (status) {
    case 'running':
    case 'published':
    case 'done':
      return 'success'
    case 'pending':
    case 'running-operation':
    case 'draining':
      return 'warning'
    case 'stopped':
    case 'deleted':
    case 'lost':
    case 'error':
    case 'publish_failed':
    case 'revoke':
      return 'error'
    default:
      return 'neutral'
  }
}

// ── Filters ──────────────────────────────────────────────────────────────────
const statusFilter = ref<string>('All')
const labelFilter = ref<string>('')

const allStatuses = computed(() => {
  const seen = new Set<string>()
  for (const sb of swarm?.sandboxes.value ?? []) {
    if (sb.status) seen.add(sb.status)
  }
  return ['All', ...Array.from(seen).sort()]
})

const filtered = computed(() => {
  return (swarm?.sandboxes.value ?? []).filter((sb: any) => {
    const matchStatus = statusFilter.value === 'All' || sb.status === statusFilter.value
    const matchLabel = !labelFilter.value
      || JSON.stringify(sb.labels ?? {}).toLowerCase().includes(labelFilter.value.toLowerCase())
    return matchStatus && matchLabel
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
          placeholder="Filter by label…"
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

      <!-- Status: UBadge -->
      <template #status-cell="{ row }">
        <UBadge
          :label="row.original.status ?? 'unknown'"
          :color="statusColor(row.original.status)"
          variant="subtle"
          size="sm"
        />
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
            {{ statusFilter !== 'All' || labelFilter ? 'No sandboxes match the current filters.' : 'No sandboxes yet.' }}
          </p>
          <UButton
            v-if="session.isAdmin.value && statusFilter === 'All' && !labelFilter"
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
      <template v-if="statusFilter !== 'All' || labelFilter">
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
