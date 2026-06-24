<script setup lang="ts">
// Compact "map" view of the swarm: one tile per node, heat-tinted by live CPU
// utilisation, with a dot per sandbox (colored by status) living inside it.
// Denser than the detailed NodeCards on /nodes — built for at-a-glance scanning.
const swarm = useSwarm()
const status = useStatus()

const nodes = computed(() => swarm?.nodes.value ?? [])

function cpuPct(n: any): number {
  return Math.min(100, Math.round((n.actual_cpu ?? 0) * 100))
}

// Heat buckets: cool (<50) → warm (50-80) → hot (>80).
function heat(n: any): 'ok' | 'warn' | 'hot' {
  const p = cpuPct(n)
  return p >= 80 ? 'hot' : p >= 50 ? 'warn' : 'ok'
}
const BAR: Record<string, string> = { ok: 'bg-success', warn: 'bg-warning', hot: 'bg-error' }
const PCT_TEXT: Record<string, string> = { ok: 'text-success', warn: 'text-warning', hot: 'text-error' }

function nodeSandboxes(n: any): any[] {
  return (swarm?.sandboxes.value ?? []).filter((s: any) => s.owner_node === n.node_id)
}

// Sandbox dot color from the shared status vocabulary → static bg class.
const DOT: Record<string, string> = {
  success: 'bg-success', warning: 'bg-warning', error: 'bg-error', info: 'bg-info', neutral: 'bg-muted',
}
function dotClass(s: string): string {
  return DOT[status.sandbox(s).color] ?? 'bg-muted'
}
</script>

<template>
  <div class="grid grid-cols-2 sm:grid-cols-3 lg:grid-cols-4 gap-3">
    <UTooltip
      v-for="node in nodes"
      :key="node.node_id"
      :text="`${node.node_name} · CPU ${cpuPct(node)}% · ${nodeSandboxes(node).length} sandbox${nodeSandboxes(node).length === 1 ? '' : 'es'} — open Nodes`"
    >
      <NuxtLink
        to="/nodes"
        class="rounded-md border border-default bg-elevated p-3 flex flex-col gap-2 transition-colors hover:border-accented hover:bg-accented/30 h-full"
      >
      <!-- Name + live CPU% -->
      <div class="flex items-center justify-between gap-2">
        <span class="font-mono text-xs font-semibold text-highlighted truncate">{{ node.node_name }}</span>
        <span class="font-mono text-[11px] tabular-nums shrink-0" :class="PCT_TEXT[heat(node)]">{{ cpuPct(node) }}%</span>
      </div>

      <!-- Heat bar -->
      <div class="h-1 rounded bg-accented overflow-hidden">
        <div class="h-full transition-all duration-300" :class="BAR[heat(node)]" :style="{ width: cpuPct(node) + '%' }" />
      </div>

      <!-- Cordon / drain flags -->
      <div v-if="node.cordoned || node.draining" class="flex items-center gap-1">
        <UIcon v-if="node.cordoned" name="i-lucide-shield-off" class="size-3 text-warning" aria-label="Cordoned" />
        <UIcon v-if="node.draining" name="i-lucide-droplet" class="size-3 text-warning" aria-label="Draining" />
      </div>

      <!-- Sandboxes inside this node -->
      <div class="flex flex-wrap items-center gap-1 min-h-3">
        <span
          v-for="sb in nodeSandboxes(node)"
          :key="sb.id"
          class="size-2 rounded-full shrink-0"
          :class="dotClass(sb.status)"
          :title="`${sb.id} · ${sb.status}`"
        />
        <span v-if="!nodeSandboxes(node).length" class="text-[10px] text-dimmed">idle</span>
      </div>
      </NuxtLink>
    </UTooltip>
  </div>
</template>
