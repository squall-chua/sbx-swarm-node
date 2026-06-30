<script setup lang="ts">
import { onScopeDispose } from 'vue'

const props = defineProps<{ id: string }>()

const CAP = 60

const cpuSeries = ref<number[]>([])  // % utilization
const memSeries = ref<number[]>([])  // GB used
const diskSeries = ref<number[]>([]) // GB used

// Latest snapshot for the numeric summary.
const cores = ref(0)
const cpuPct = ref(0)
const memUsedGB = ref(0)
const memTotalGB = ref(0)
const diskUsedGB = ref(0)
const diskTotalGB = ref(0)

const KIB_PER_GB = 1024 * 1024 // /proc reports KiB

function pushRing(series: Ref<number[]>, val: number) {
  series.value = [...series.value.slice(-(CAP - 1)), val]
}

const es = new EventSource(`/v1/sandboxes/${props.id}/stats`, { withCredentials: true })

es.addEventListener('stats', (ev: MessageEvent) => {
  try {
    const d = JSON.parse(ev.data)
    cores.value = typeof d.cores === 'number' ? d.cores : 0
    cpuPct.value = typeof d.cpu_percent === 'number' ? d.cpu_percent : 0
    memUsedGB.value = (d.mem_used_kb ?? 0) / KIB_PER_GB
    memTotalGB.value = (d.mem_total_kb ?? 0) / KIB_PER_GB
    diskUsedGB.value = d.disk_used_gb ?? 0
    diskTotalGB.value = d.disk_total_gb ?? 0
    pushRing(cpuSeries, cpuPct.value)
    pushRing(memSeries, memUsedGB.value)
    pushRing(diskSeries, diskUsedGB.value)
  } catch {
    // ignore malformed frames
  }
})

onScopeDispose(() => {
  es.close()
})

const g = (n: number) => n.toFixed(1)
</script>

<template>
  <div class="flex flex-col gap-4 pt-2">
    <!-- Numeric summary -->
    <div class="grid grid-cols-[auto_1fr] gap-x-6 gap-y-1.5 text-sm">
      <span class="text-muted font-medium">CPUs</span>
      <span class="text-default tabular-nums">{{ cores }} <span class="text-muted">({{ Math.round(cpuPct) }}%)</span></span>
      <span class="text-muted font-medium">Memory</span>
      <span class="text-default tabular-nums">{{ g(memUsedGB) }}GB used / {{ g(memTotalGB) }}GB total</span>
      <span class="text-muted font-medium">Disk</span>
      <span class="text-default tabular-nums">{{ g(diskUsedGB) }}GB used / {{ g(diskTotalGB) }}GB total</span>
    </div>

    <div class="rounded-lg border border-default bg-elevated p-4">
      <Sparkline :values="cpuSeries" label="CPU" :max="100" />
    </div>
    <div class="rounded-lg border border-default bg-elevated p-4">
      <Sparkline :values="memSeries" label="Memory" :max="memTotalGB || 1" unit="GB" />
    </div>
    <div class="rounded-lg border border-default bg-elevated p-4">
      <Sparkline :values="diskSeries" label="Disk" :max="diskTotalGB || 1" unit="GB" />
    </div>
    <p v-if="cpuSeries.length === 0" class="text-xs text-muted text-center">
      Waiting for stats…
    </p>
  </div>
</template>
