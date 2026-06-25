<script setup lang="ts">
import { toPoints } from './Sparkline'

const props = withDefaults(defineProps<{
  values: number[]
  label: string
  max?: number // y-axis floor; the axis grows past it only if data spikes above. Omit = auto-scale.
  unit?: string
}>(), { unit: '%' })

const W = 200
const H = 40

// Fixed scale: at least `max`, growing only if the data exceeds it. Keeps proportions
// honest (3% reads as 3% of the box height, not full height) and gives a stable axis.
const ceiling = computed(() => Math.max(props.max ?? 0, 1, ...props.values))
const points = computed(() => toPoints(props.values, W, H, ceiling.value))
const latest = computed(() =>
  props.values.length > 0 ? props.values[props.values.length - 1].toFixed(1) : '—',
)
</script>

<template>
  <div class="flex flex-col gap-1.5">
    <div class="flex items-center justify-between">
      <span class="text-xs text-muted font-medium uppercase tracking-wide">{{ label }}</span>
      <span class="text-sm font-mono tabular-nums text-highlighted font-semibold">{{ latest }}{{ unit }}</span>
    </div>
    <div class="flex items-stretch gap-2">
      <!-- y-axis scale -->
      <div class="flex flex-col justify-between text-[10px] leading-none text-muted tabular-nums">
        <span>{{ Math.round(ceiling) }}{{ unit }}</span>
        <span>0{{ unit }}</span>
      </div>
      <svg
        :viewBox="`0 0 ${W} ${H}`"
        preserveAspectRatio="none"
        class="w-full h-16 overflow-visible"
        aria-hidden="true"
      >
        <!-- top (ceiling) + baseline (0) gridlines -->
        <line x1="0" y1="0" :x2="W" y2="0" class="stroke-current text-muted" stroke-width="1" stroke-dasharray="2 2" vector-effect="non-scaling-stroke" />
        <line x1="0" :y1="H" :x2="W" :y2="H" class="stroke-current text-muted" stroke-width="1" vector-effect="non-scaling-stroke" />
        <polyline
          v-if="points"
          :points="points"
          fill="none"
          class="stroke-primary"
          stroke-width="1.5"
          stroke-linejoin="round"
          stroke-linecap="round"
          vector-effect="non-scaling-stroke"
        />
      </svg>
    </div>
  </div>
</template>
