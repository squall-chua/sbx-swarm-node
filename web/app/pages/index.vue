<script setup lang="ts">
const swarm = useSwarm()

// ── Stat computations ─────────────────────────────────────────────────────────

const nodeCount = computed(() => swarm?.nodes.value.length ?? 0)

const sandboxesByStatus = computed(() => {
  const counts: Record<string, number> = {}
  for (const sb of swarm?.sandboxes.value ?? []) {
    counts[sb.status] = (counts[sb.status] ?? 0) + 1
  }
  return counts
})

const totalSandboxes = computed(() =>
  Object.values(sandboxesByStatus.value).reduce((a, b) => a + b, 0)
)

const runningSandboxes = computed(() => sandboxesByStatus.value['running'] ?? 0)

const cpuAllocTotal = computed(() =>
  (swarm?.nodes.value ?? []).reduce((sum: number, n: any) => sum + (n.alloc_cpu ?? 0), 0)
)
const cpuLimitTotal = computed(() =>
  (swarm?.nodes.value ?? []).reduce((sum: number, n: any) => sum + (n.limit_cpu ?? 0), 0)
)

// TODO: blocked-egress distinct count not available from current /v1/nodes API
// (would require per-sandbox network-policy data). Omitted until endpoint ships.

const recentOps = computed(() => (swarm?.operations.value ?? []).slice(0, 5))

// ── Status badge color ────────────────────────────────────────────────────────
const opStatusColor: Record<string, string> = {
  done: 'success',
  running: 'warning',
  pending: 'warning',
  error: 'error',
  failed: 'error',
}

function opColor(status: string): string {
  return opStatusColor[status] ?? 'neutral'
}

async function refresh() {
  await swarm?.refreshAll()
}
</script>

<template>
  <div class="flex flex-col gap-6 p-4 md:p-6">
    <!-- Page header -->
    <div class="flex items-center justify-between gap-4">
      <h1 class="text-lg font-semibold text-highlighted">Overview</h1>
      <UButton
        icon="i-lucide-refresh-cw"
        label="Refresh"
        color="neutral"
        variant="outline"
        size="sm"
        aria-label="Refresh swarm data"
        @click="refresh"
      />
    </div>

    <!-- ── Stat cards ─────────────────────────────────────────────────────── -->
    <div class="grid grid-cols-1 sm:grid-cols-2 xl:grid-cols-4 gap-4">
      <!-- Nodes -->
      <UCard variant="outline">
        <template #header>
          <div class="flex items-center gap-2 text-muted text-xs font-medium uppercase tracking-wide">
            <UIcon name="i-lucide-server" class="size-3.5" />
            Nodes
          </div>
        </template>
        <p class="text-3xl font-bold text-highlighted tabular-nums">{{ nodeCount }}</p>
        <p class="text-xs text-muted mt-1">total in swarm</p>
      </UCard>

      <!-- Sandboxes -->
      <UCard variant="outline">
        <template #header>
          <div class="flex items-center gap-2 text-muted text-xs font-medium uppercase tracking-wide">
            <UIcon name="i-lucide-box" class="size-3.5" />
            Sandboxes
          </div>
        </template>
        <p class="text-3xl font-bold text-highlighted tabular-nums">{{ totalSandboxes }}</p>
        <p class="text-xs text-muted mt-1">
          <span class="text-success font-medium">{{ runningSandboxes }} running</span>
          <template v-if="totalSandboxes - runningSandboxes > 0">
            &nbsp;· {{ totalSandboxes - runningSandboxes }} other
          </template>
        </p>
      </UCard>

      <!-- CPU -->
      <UCard variant="outline">
        <template #header>
          <div class="flex items-center gap-2 text-muted text-xs font-medium uppercase tracking-wide">
            <UIcon name="i-lucide-cpu" class="size-3.5" />
            CPU (alloc / limit)
          </div>
        </template>
        <p class="text-3xl font-bold text-highlighted tabular-nums">
          {{ cpuAllocTotal }}<span class="text-muted text-lg font-normal"> / {{ cpuLimitTotal }}</span>
        </p>
        <UProgress
          v-if="cpuLimitTotal > 0"
          :model-value="Math.round((cpuAllocTotal / cpuLimitTotal) * 100)"
          color="primary"
          size="xs"
          class="mt-2"
          aria-label="Total CPU allocation"
        />
        <p v-else class="text-xs text-muted mt-1">no nodes</p>
      </UCard>

      <!-- Recent operations -->
      <UCard variant="outline">
        <template #header>
          <div class="flex items-center gap-2 text-muted text-xs font-medium uppercase tracking-wide">
            <UIcon name="i-lucide-activity" class="size-3.5" />
            Recent operations
          </div>
        </template>
        <div v-if="recentOps.length > 0" class="flex flex-col gap-1.5">
          <div
            v-for="op in recentOps"
            :key="op.id"
            class="flex items-center justify-between gap-2"
          >
            <span class="font-mono text-xs text-default truncate">{{ op.id }}</span>
            <UBadge
              :label="op.status"
              :color="opColor(op.status)"
              variant="subtle"
              size="xs"
            />
          </div>
        </div>
        <p v-else class="text-xs text-muted italic">No recent operations</p>
      </UCard>
    </div>

    <!-- ── Swarm map ──────────────────────────────────────────────────────── -->
    <div>
      <h2 class="text-sm font-medium text-muted uppercase tracking-wide mb-3">Swarm map</h2>
      <div
        v-if="swarm?.nodes.value.length"
        class="grid grid-cols-1 sm:grid-cols-2 xl:grid-cols-3 gap-4"
      >
        <NodeCard
          v-for="node in swarm.nodes.value"
          :key="node.node_id"
          :node="node"
        />
      </div>
      <UAlert
        v-else
        icon="i-lucide-server-off"
        title="No nodes"
        description="No nodes are currently visible in the swarm."
        color="neutral"
        variant="subtle"
      />
    </div>
  </div>
</template>
