<script setup lang="ts">
// One consistent status indicator: dot/icon + label, never color alone.
// Backed by useStatus so the two vocabularies (sandbox vs operation) stay DRY.
const props = withDefaults(defineProps<{
  status: string
  kind?: 'sandbox' | 'operation'
  size?: 'xs' | 'sm' | 'md'
}>(), { kind: 'sandbox', size: 'sm' })

const s = useStatus()
const meta = computed(() => props.kind === 'operation' ? s.operation(props.status) : s.sandbox(props.status))
</script>

<template>
  <UBadge
    :color="meta.color"
    :icon="meta.icon"
    :label="meta.label"
    variant="subtle"
    :size="size"
  />
</template>
