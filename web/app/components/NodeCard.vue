<script setup lang="ts">
interface NodeSummary {
  node_id: string
  node_name: string
  cordoned: boolean
  draining: boolean
  limit_cpu: number
  alloc_cpu: number
  actual_cpu: number
  limit_mem_kb: number
  alloc_mem_kb: number
  templates: string[]
  workspaces: string[]
  labels: Record<string, string>
  capabilities: string[]
}

interface SandboxSummary {
  id: string
  owner_node: string
  status: string
}

const props = defineProps<{ node: NodeSummary }>()

const swarm = useSwarm()

const nodeSandboxes = computed(() =>
  (swarm?.sandboxes.value ?? []).filter((s: SandboxSummary) => s.owner_node === props.node.node_id)
)

// CPU: percent of limit
const cpuAllocPct = computed(() =>
  props.node.limit_cpu > 0
    ? Math.round((props.node.alloc_cpu / props.node.limit_cpu) * 100)
    : 0
)
const cpuActualPct = computed(() =>
  props.node.limit_cpu > 0
    ? Math.round((props.node.actual_cpu / props.node.limit_cpu) * 100)
    : 0
)

// Mem: percent of limit
const memAllocPct = computed(() =>
  props.node.limit_mem_kb > 0
    ? Math.round((props.node.alloc_mem_kb / props.node.limit_mem_kb) * 100)
    : 0
)

function fmtMem(kb: number): string {
  if (kb >= 1_048_576) return `${(kb / 1_048_576).toFixed(1)} GB`
  if (kb >= 1024) return `${(kb / 1024).toFixed(0)} MB`
  return `${kb} KB`
}

// status → color map
const statusColor: Record<string, string> = {
  running: 'success',
  published: 'success',
  done: 'success',
  pending: 'warning',
  draining: 'warning',
  stopped: 'error',
  deleted: 'error',
  lost: 'error',
  error: 'error',
  publish_failed: 'error',
  revoke: 'error',
}

function sandboxColor(status: string): string {
  return statusColor[status] ?? 'neutral'
}

const cpuBarColor = computed(() => {
  if (cpuActualPct.value >= 90) return 'error'
  if (cpuActualPct.value >= 70) return 'warning'
  return 'primary'
})

const memBarColor = computed(() => {
  if (memAllocPct.value >= 90) return 'error'
  if (memAllocPct.value >= 70) return 'warning'
  return 'primary'
})
</script>

<template>
  <UCard variant="outline" class="flex flex-col gap-3">
    <!-- Header: name + status badges -->
    <template #header>
      <div class="flex items-center justify-between gap-2 flex-wrap">
        <span class="font-mono text-sm font-semibold text-highlighted truncate">
          {{ node.node_name }}
        </span>
        <div class="flex items-center gap-1.5 shrink-0">
          <UBadge
            v-if="node.cordoned"
            label="Cordoned"
            color="warning"
            variant="subtle"
            size="xs"
          />
          <UBadge
            v-if="node.draining"
            label="Draining"
            color="warning"
            variant="subtle"
            size="xs"
          />
        </div>
      </div>
    </template>

    <!-- Body: resource bars + sandboxes -->
    <div class="flex flex-col gap-4">
      <!-- CPU -->
      <div class="flex flex-col gap-1">
        <div class="flex items-center justify-between text-xs text-muted">
          <span>CPU</span>
          <span class="tabular-nums">
            actual <strong class="text-default">{{ node.actual_cpu }}</strong>
            / alloc <strong class="text-default">{{ node.alloc_cpu }}</strong>
            / limit <strong class="text-default">{{ node.limit_cpu }}</strong>
          </span>
        </div>
        <!-- Actual CPU (solid bar) -->
        <UProgress
          :model-value="cpuActualPct"
          :color="cpuBarColor"
          size="xs"
          aria-label="CPU actual utilisation"
        />
        <!-- Alloc CPU (dimmer bar below) -->
        <UProgress
          :model-value="cpuAllocPct"
          color="neutral"
          size="2xs"
          aria-label="CPU allocated"
        />
      </div>

      <!-- Memory -->
      <div class="flex flex-col gap-1">
        <div class="flex items-center justify-between text-xs text-muted">
          <span>Memory</span>
          <span class="tabular-nums">
            alloc <strong class="text-default">{{ fmtMem(node.alloc_mem_kb) }}</strong>
            / limit <strong class="text-default">{{ fmtMem(node.limit_mem_kb) }}</strong>
          </span>
        </div>
        <UProgress
          :model-value="memAllocPct"
          :color="memBarColor"
          size="xs"
          aria-label="Memory allocated"
        />
      </div>

      <!-- Sandboxes on this node -->
      <div v-if="nodeSandboxes.length > 0" class="flex flex-wrap gap-1.5">
        <UBadge
          v-for="sb in nodeSandboxes"
          :key="sb.id"
          :label="sb.id"
          :color="sandboxColor(sb.status)"
          variant="subtle"
          size="xs"
          class="font-mono"
        />
      </div>
      <p v-else class="text-xs text-muted italic">No sandboxes</p>
    </div>
  </UCard>
</template>
