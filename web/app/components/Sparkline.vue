<script setup lang="ts">
import { toPoints } from './Sparkline'

const props = defineProps<{
  values: number[]
  label: string
}>()

const W = 200
const H = 40

const points = computed(() => toPoints(props.values, W, H))
const latest = computed(() =>
  props.values.length > 0 ? props.values[props.values.length - 1].toFixed(1) : '—',
)
</script>

<template>
  <div class="flex flex-col gap-1">
    <div class="flex items-center justify-between">
      <span class="text-xs text-muted font-medium uppercase tracking-wide">{{ label }}</span>
      <span class="text-sm font-mono tabular-nums text-highlighted font-semibold">{{ latest }}%</span>
    </div>
    <svg
      :viewBox="`0 0 ${W} ${H}`"
      :width="W"
      :height="H"
      class="w-full overflow-visible"
      aria-hidden="true"
    >
      <polyline
        v-if="points"
        :points="points"
        fill="none"
        class="stroke-primary"
        stroke-width="1.5"
        stroke-linejoin="round"
        stroke-linecap="round"
      />
    </svg>
  </div>
</template>
