<script setup lang="ts">
import type { TableColumn } from '@nuxt/ui'

interface Template {
  id: string
  repository: string
  tag: string
  agent: string
  created_at: string
}

const api = useApi()
const swarm = useSwarm()
const toast = useToast()

const templates = ref<Template[]>([])
const loading = ref(false)

async function fetchTemplates() {
  loading.value = true
  try {
    const res = await api.get('/v1/templates')
    templates.value = res?.templates ?? []
  } catch (e: any) {
    toast.add({ title: 'Failed to load templates', description: e?.message, color: 'error' })
  } finally {
    loading.value = false
  }
}

onMounted(fetchTemplates)

// Build a map: template name → node names that advertise it
const templateNodeMap = computed<Record<string, string[]>>(() => {
  const nodes = swarm?.nodes.value ?? []
  const map: Record<string, string[]> = {}
  for (const node of nodes) {
    for (const tmpl of node.templates ?? []) {
      if (!map[tmpl]) map[tmpl] = []
      map[tmpl].push(node.node_name ?? node.node_id)
    }
  }
  return map
})

// Unique template names from node gossip
const gossipedTemplates = computed(() => Object.keys(templateNodeMap.value).sort())

function fmtDate(ts: string | null | undefined): string {
  if (!ts) return '—'
  try { return new Date(ts).toLocaleString() } catch { return ts }
}

const columns: TableColumn<Template>[] = [
  { accessorKey: 'repository', header: 'Repository' },
  { accessorKey: 'tag', header: 'Tag' },
  { accessorKey: 'id', header: 'ID' },
  { accessorKey: 'agent', header: 'Agent' },
  { accessorKey: 'created_at', header: 'Created' },
]
</script>

<template>
  <div class="flex flex-col gap-6 p-4 md:p-6">

    <!-- Page header -->
    <div class="flex flex-wrap items-center justify-between gap-3">
      <h1 class="text-lg font-semibold text-highlighted">Templates</h1>
      <UButton
        icon="i-lucide-refresh-cw"
        color="neutral"
        variant="outline"
        size="sm"
        :loading="loading"
        aria-label="Refresh templates"
        @click="fetchTemplates(); swarm?.refreshNodes()"
      />
    </div>

    <!-- Templates table -->
    <div class="flex flex-col gap-2">
      <UTable
        :data="templates"
        :columns="columns"
        class="w-full"
      >
        <template #repository-cell="{ row }">
          <span class="text-sm text-default">{{ row.original.repository }}</span>
        </template>
        <template #tag-cell="{ row }">
          <UBadge
            :label="row.original.tag || 'latest'"
            color="neutral"
            variant="subtle"
            size="xs"
            class="font-mono"
          />
        </template>
        <template #id-cell="{ row }">
          <span class="font-mono text-xs text-muted">{{ row.original.id }}</span>
        </template>
        <template #agent-cell="{ row }">
          <span class="text-sm text-default">{{ row.original.agent || '—' }}</span>
        </template>
        <template #created_at-cell="{ row }">
          <span class="tabular-nums text-muted text-sm">{{ fmtDate(row.original.created_at) }}</span>
        </template>
        <template #empty>
          <div class="flex flex-col items-center justify-center gap-2 py-12 text-center">
            <UIcon name="i-lucide-package-x" class="size-8 text-muted" aria-hidden="true" />
            <p class="text-sm text-muted">No templates registered.</p>
          </div>
        </template>
      </UTable>
    </div>

    <USeparator />

    <!-- Node template distribution (from gossip) -->
    <div class="flex flex-col gap-3">
      <h2 class="text-sm font-medium text-muted uppercase tracking-wide">Template distribution across nodes</h2>

      <UAlert
        v-if="gossipedTemplates.length === 0"
        icon="i-lucide-server-off"
        title="No template distribution data"
        description="Nodes have not gossiped any template information yet."
        color="neutral"
        variant="subtle"
      />

      <div v-else class="flex flex-col gap-3">
        <div
          v-for="tmplName in gossipedTemplates"
          :key="tmplName"
          class="flex items-start gap-3 rounded-md bg-elevated px-4 py-3"
        >
          <span class="font-mono text-sm text-default min-w-0 shrink-0">{{ tmplName }}</span>
          <div class="flex flex-wrap gap-1.5">
            <UBadge
              v-for="nodeName in templateNodeMap[tmplName]"
              :key="nodeName"
              :label="nodeName"
              color="primary"
              variant="subtle"
              size="xs"
            />
          </div>
        </div>
      </div>
    </div>

  </div>
</template>
