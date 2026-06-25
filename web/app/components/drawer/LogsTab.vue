<script setup lang="ts">
import { onScopeDispose, nextTick } from 'vue'

const props = defineProps<{ id: string }>()

// The backend streams a file INSIDE the sandbox via `tail -F <path>` (follows
// across rotation/truncation). It needs a path — there is no default — so let the
// user point it at any file. With no path the daemon rejects the attach and the
// stream dies silently, which is why this panel used to sit empty.
const CAP = 1000
const path = ref('')
const active = ref('') // the path currently being followed
const status = ref<'idle' | 'streaming' | 'ended'>('idle')
const lines = ref<string[]>([])
const preEl = ref<HTMLPreElement | null>(null)
let es: EventSource | null = null

async function appendLine(text: string) {
  lines.value = [...lines.value.slice(-(CAP - 1)), text]
  await nextTick()
  if (preEl.value) preEl.value.scrollTop = preEl.value.scrollHeight
}

function stop() {
  es?.close()
  es = null
}

function follow() {
  const p = path.value.trim()
  if (!p) return
  stop()
  lines.value = []
  active.value = p
  status.value = 'streaming'
  es = new EventSource(`/v1/sandboxes/${props.id}/logs?path=${encodeURIComponent(p)}`, { withCredentials: true })
  es.addEventListener('log', (ev: MessageEvent) => appendLine(ev.data))
  es.addEventListener('error', () => {
    // Stream ended or the path couldn't be opened. Close so the browser doesn't
    // auto-reconnect and hammer a bad path.
    stop()
    status.value = 'ended'
  })
}

function stopFollowing() {
  stop()
  status.value = 'idle'
}

const emptyMsg = computed(() => {
  if (status.value === 'streaming') return `Following ${active.value} — waiting for output…`
  if (status.value === 'ended') return 'Stream ended — check the path exists inside the sandbox.'
  return 'Enter a file path above to follow its logs.'
})

onScopeDispose(stop)
</script>

<template>
  <div class="flex flex-col gap-2 pt-2 h-full">
    <!-- path picker: tails a file inside the sandbox -->
    <div class="flex items-center gap-2">
      <UInput
        v-model="path"
        placeholder="/path/inside/sandbox.log"
        size="sm"
        class="flex-1 font-mono"
        aria-label="Log file path inside the sandbox"
        data-test="log-path"
        @keydown.enter="follow"
      />
      <UButton
        label="Follow"
        icon="i-lucide-play"
        size="sm"
        :disabled="!path.trim()"
        data-test="log-follow"
        @click="follow"
      />
      <UButton
        v-if="status === 'streaming'"
        label="Stop"
        icon="i-lucide-square"
        color="neutral"
        variant="outline"
        size="sm"
        @click="stopFollowing"
      />
    </div>
    <p class="text-xs text-muted">
      Follows a file inside the sandbox with <span class="font-mono">tail -F</span> — e.g. an app or agent log path.
    </p>

    <pre
      ref="preEl"
      class="flex-1 overflow-y-auto rounded-lg border border-default bg-elevated p-3 text-xs font-mono text-highlighted leading-relaxed"
      style="max-height: 480px; min-height: 200px;"
      aria-label="Sandbox log output"
    ><template v-if="lines.length > 0">{{ lines.join('\n') }}</template><template v-else><span class="text-muted">{{ emptyMsg }}</span></template></pre>
  </div>
</template>
