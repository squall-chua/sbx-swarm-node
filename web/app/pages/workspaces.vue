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
