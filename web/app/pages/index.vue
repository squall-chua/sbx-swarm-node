<script setup lang="ts">
const swarm = useSwarm()
const status = useStatus()

const ready = computed(() => swarm?.ready.value ?? false)

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
  Object.values(sandboxesByStatus.value).reduce((a, b) => a + b, 0),
)
const runningSandboxes = computed(() => sandboxesByStatus.value['running'] ?? 0)

// Operations carry a `state` field (see operations.vue) — not `status`.
// The API returns operations newest-first, so the first 6 are the most recent.
const recentOps = computed(() => (swarm?.operations.value ?? []).slice(0, 6))

function fmtFull(ts: string | null | undefined): string {
  if (!ts) return ''
  try { return new Date(ts).toLocaleString() } catch { return ts ?? '' }
}
// fmtAgo renders a compact relative time (e.g. "5m ago"); full timestamp on hover.
function fmtAgo(ts: string | null | undefined): string {
  if (!ts) return ''
  const t = new Date(ts).getTime()
  if (isNaN(t)) return ''
  const s = Math.max(0, Math.floor((Date.now() - t) / 1000))
  if (s < 60) return `${s}s ago`
  const m = Math.floor(s / 60); if (m < 60) return `${m}m ago`
  const h = Math.floor(m / 60); if (h < 24) return `${h}h ago`
  return `${Math.floor(h / 24)}d ago`
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

    <!-- ── KPI cards (each drills into its page) ──────────────────────────── -->
    <div class="grid grid-cols-1 sm:grid-cols-2 xl:grid-cols-4 gap-4">
      <!-- Nodes -->
      <UTooltip text="View all nodes">
        <NuxtLink to="/nodes" class="group block">
          <UCard variant="outline" class="h-full transition-colors group-hover:border-accented">
            <template #header>
              <div class="flex items-center gap-2 text-muted text-xs font-medium uppercase tracking-wide">
                <UIcon name="i-lucide-server" class="size-3.5" />
                Nodes
                <UIcon name="i-lucide-arrow-right" class="size-3.5 ml-auto opacity-0 -translate-x-1 group-hover:opacity-100 group-hover:translate-x-0 transition" />
              </div>
            </template>
            <USkeleton v-if="!ready" class="h-9 w-12" />
            <p v-else class="text-3xl font-bold text-highlighted tabular-nums font-mono">{{ nodeCount }}</p>
            <p class="text-xs text-muted mt-1">total in swarm</p>
          </UCard>
        </NuxtLink>
      </UTooltip>

      <!-- Sandboxes -->
      <UTooltip text="View all sandboxes">
        <NuxtLink to="/sandboxes" class="group block">
          <UCard variant="outline" class="h-full transition-colors group-hover:border-accented">
            <template #header>
              <div class="flex items-center gap-2 text-muted text-xs font-medium uppercase tracking-wide">
                <UIcon name="i-lucide-box" class="size-3.5" />
                Sandboxes
                <UIcon name="i-lucide-arrow-right" class="size-3.5 ml-auto opacity-0 -translate-x-1 group-hover:opacity-100 group-hover:translate-x-0 transition" />
              </div>
            </template>
            <USkeleton v-if="!ready" class="h-9 w-12" />
            <template v-else>
              <p class="text-3xl font-bold text-highlighted tabular-nums font-mono">{{ totalSandboxes }}</p>
              <p class="text-xs text-muted mt-1">
                <span class="text-success font-medium">{{ runningSandboxes }} running</span>
                <template v-if="totalSandboxes - runningSandboxes > 0">
                  &nbsp;· {{ totalSandboxes - runningSandboxes }} other
                </template>
              </p>
            </template>
          </UCard>
        </NuxtLink>
      </UTooltip>

      <!-- CPU — live swarm-average utilisation + trend -->
      <UTooltip text="Live CPU — open per-node detail">
        <NuxtLink to="/nodes" class="group block">
          <UCard variant="outline" class="h-full transition-colors group-hover:border-accented">
            <USkeleton v-if="!ready" class="h-14 w-full" />
            <Sparkline v-else label="CPU · live avg" :values="swarm.cpuHistory.value" />
          </UCard>
        </NuxtLink>
      </UTooltip>

      <!-- Memory — live swarm-average utilisation + trend -->
      <UTooltip text="Live memory — open per-node detail">
        <NuxtLink to="/nodes" class="group block">
          <UCard variant="outline" class="h-full transition-colors group-hover:border-accented">
            <USkeleton v-if="!ready" class="h-14 w-full" />
            <Sparkline v-else label="Memory · live avg" :values="swarm.memHistory.value" />
          </UCard>
        </NuxtLink>
      </UTooltip>
    </div>

    <!-- ── Swarm map ──────────────────────────────────────────────────────── -->
    <div>
      <div class="flex items-center justify-between mb-3">
        <h2 class="text-sm font-medium text-muted uppercase tracking-wide">Swarm map</h2>
        <ULink to="/nodes" class="text-xs text-primary hover:underline inline-flex items-center gap-1">
          All nodes <UIcon name="i-lucide-arrow-right" class="size-3.5" />
        </ULink>
      </div>

      <!-- First-load skeletons -->
      <div v-if="!ready" class="grid grid-cols-1 sm:grid-cols-2 xl:grid-cols-3 gap-4">
        <USkeleton v-for="i in 3" :key="i" class="h-40 w-full" />
      </div>

      <SwarmMap v-else-if="swarm.nodes.value.length" />

      <UAlert
        v-else
        icon="i-lucide-server-off"
        title="No nodes"
        description="No nodes are currently visible in the swarm."
        color="neutral"
        variant="subtle"
      />
    </div>

    <!-- ── Recent operations ──────────────────────────────────────────────── -->
    <div>
      <div class="flex items-center justify-between mb-3">
        <h2 class="text-sm font-medium text-muted uppercase tracking-wide">Recent operations</h2>
        <ULink to="/operations" class="text-xs text-primary hover:underline inline-flex items-center gap-1">
          All operations <UIcon name="i-lucide-arrow-right" class="size-3.5" />
        </ULink>
      </div>
      <UCard variant="outline">
        <div v-if="recentOps.length > 0" class="flex flex-col divide-y divide-default/60">
          <div
            v-for="op in recentOps"
            :key="op.id"
            class="flex items-center justify-between gap-3 py-2 first:pt-0 last:pb-0"
          >
            <div class="flex items-center gap-2 min-w-0">
              <span class="font-mono text-xs text-muted">{{ op.type }}</span>
              <span class="font-mono text-xs text-default truncate">{{ op.id }}</span>
            </div>
            <div class="flex items-center gap-3 shrink-0">
              <span class="tabular-nums text-xs text-muted" :title="fmtFull(op.created_at)">{{ fmtAgo(op.created_at) }}</span>
              <StatusPill :status="op.state" kind="operation" size="xs" />
            </div>
          </div>
        </div>
        <p v-else class="text-sm text-muted italic">No recent operations.</p>
      </UCard>
    </div>
  </div>
</template>
