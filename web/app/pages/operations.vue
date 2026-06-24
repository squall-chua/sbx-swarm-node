<script setup lang="ts">
import type { TableColumn } from '@nuxt/ui'

interface Operation {
  id: string
  type: string
  state: string
  sandbox_id: string
  error: string
  created_at: string
  updated_at?: string
}

const swarm = useSwarm()

const operations = computed<Operation[]>(() => {
  const ops = swarm?.operations.value ?? []
  return [...ops].reverse()
})

function fmtDate(ts: string | null | undefined): string {
  if (!ts) return '—'
  try { return new Date(ts).toLocaleString() } catch { return ts }
}

const columns: TableColumn<Operation>[] = [
  { accessorKey: 'id', header: 'ID' },
  { accessorKey: 'type', header: 'Type' },
  { accessorKey: 'state', header: 'State' },
  { accessorKey: 'sandbox_id', header: 'Sandbox' },
  { accessorKey: 'error', header: 'Error' },
  { accessorKey: 'created_at', header: 'Created' },
]
</script>

<template>
  <div class="flex flex-col gap-4 p-4 md:p-6">

    <!-- Page header -->
    <div class="flex flex-wrap items-center justify-between gap-3">
      <h1 class="text-lg font-semibold text-highlighted">Operations</h1>
      <UButton
        icon="i-lucide-refresh-cw"
        color="neutral"
        variant="outline"
        size="sm"
        aria-label="Refresh operations"
        @click="swarm?.refreshOperations()"
      />
    </div>

    <!-- Operations table -->
    <UTable
      :data="operations"
      :columns="columns"
      class="w-full"
    >
      <template #id-cell="{ row }">
        <span class="font-mono text-sm text-default">{{ row.original.id }}</span>
      </template>
      <template #type-cell="{ row }">
        <span class="text-sm text-default">{{ row.original.type }}</span>
      </template>
      <template #state-cell="{ row }">
        <StatusPill :status="row.original.state" kind="operation" />
      </template>
      <template #sandbox_id-cell="{ row }">
        <span class="font-mono text-sm text-muted">{{ row.original.sandbox_id }}</span>
      </template>
      <template #error-cell="{ row }">
        <span v-if="row.original.error" class="text-error text-sm">{{ row.original.error }}</span>
        <span v-else class="text-muted">—</span>
      </template>
      <template #created_at-cell="{ row }">
        <span class="tabular-nums text-muted text-sm">{{ fmtDate(row.original.created_at) }}</span>
      </template>
      <template #empty>
        <div class="flex flex-col items-center justify-center gap-2 py-12 text-center">
          <UIcon name="i-lucide-list-x" class="size-8 text-muted" aria-hidden="true" />
          <p class="text-sm text-muted">No operations yet.</p>
        </div>
      </template>
    </UTable>

  </div>
</template>
