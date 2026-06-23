<script setup lang="ts">
import { onScopeDispose, nextTick } from 'vue'

const props = defineProps<{ id: string }>()

const CAP = 1000
const lines = ref<string[]>([])
const preEl = ref<HTMLPreElement | null>(null)

async function appendLine(text: string) {
  lines.value = [...lines.value.slice(-(CAP - 1)), text]
  await nextTick()
  if (preEl.value) preEl.value.scrollTop = preEl.value.scrollHeight
}

const es = new EventSource(`/v1/sandboxes/${props.id}/logs`, { withCredentials: true })

es.addEventListener('log', (ev: MessageEvent) => {
  appendLine(ev.data)
})

onScopeDispose(() => {
  es.close()
})
</script>

<template>
  <div class="flex flex-col gap-2 pt-2 h-full">
    <pre
      ref="preEl"
      class="flex-1 overflow-y-auto rounded-lg border border-default bg-elevated p-3 text-xs font-mono text-highlighted leading-relaxed"
      style="max-height: 480px; min-height: 200px;"
      aria-label="Sandbox log output"
    ><template v-if="lines.length === 0"><span class="text-muted">Waiting for logs…</span></template><template v-else>{{ lines.join('\n') }}</template></pre>
  </div>
</template>
