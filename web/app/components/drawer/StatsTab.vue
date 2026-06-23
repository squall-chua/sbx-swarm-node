<script setup lang="ts">
import { onScopeDispose } from 'vue'

const props = defineProps<{ id: string }>()

const CAP = 60

const cpuSeries = ref<number[]>([])
const memSeries = ref<number[]>([])

function pushRing(series: Ref<number[]>, val: number) {
  series.value = [...series.value.slice(-(CAP - 1)), val]
}

const es = new EventSource(`/v1/sandboxes/${props.id}/stats`, { withCredentials: true })

es.addEventListener('stats', (ev: MessageEvent) => {
  try {
    const d = JSON.parse(ev.data)
    pushRing(cpuSeries, typeof d.cpu_percent === 'number' ? d.cpu_percent : 0)
    const memPct =
      d.mem_total_kb && d.mem_total_kb > 0
        ? (d.mem_used_kb / d.mem_total_kb) * 100
        : 0
    pushRing(memSeries, memPct)
  } catch {
    // ignore malformed frames
  }
})

onScopeDispose(() => {
  es.close()
})
</script>

<template>
  <div class="flex flex-col gap-6 pt-2">
    <div class="rounded-lg border border-default bg-elevated p-4">
      <Sparkline :values="cpuSeries" label="CPU" />
    </div>
    <div class="rounded-lg border border-default bg-elevated p-4">
      <Sparkline :values="memSeries" label="Memory" />
    </div>
    <p v-if="cpuSeries.length === 0" class="text-xs text-muted text-center">
      Waiting for stats…
    </p>
  </div>
</template>
