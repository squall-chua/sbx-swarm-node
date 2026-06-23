<script setup lang="ts">
import '@xterm/xterm/css/xterm.css'
import { createTerminal } from '~/composables/useTerminal'

const props = defineProps<{ id: string }>()

const api = useApi()
const termEl = ref<HTMLDivElement | null>(null)

// Held so we can call close()/dispose() on unmount
let termClose: (() => void) | null = null
let termDispose: (() => void) | null = null

onMounted(async () => {
  if (!termEl.value) return

  // Dynamic import keeps xterm out of SSR/prerender (it accesses document at module load)
  const { Terminal } = await import('@xterm/xterm')
  const { FitAddon } = await import('@xterm/addon-fit')

  const term = new Terminal({ convertEol: true })
  const fit = new FitAddon()
  term.loadAddon(fit)
  term.open(termEl.value)
  fit.fit()

  const io = {
    onData: term.onData.bind(term),
    write: (b: Uint8Array) => term.write(b),
  }

  const wsProto = location.protocol === 'https:' ? 'wss:' : 'ws:'
  const { resize, close } = createTerminal(
    `${wsProto}//${location.host}/v1/sandboxes/${props.id}/terminal`,
    io,
    () => api.post(`/v1/sandboxes/${props.id}/keepalive`),
  )

  // Send initial size
  resize(term.cols, term.rows)

  // Re-fit + resize on container size changes
  const ro = new ResizeObserver(() => {
    fit.fit()
    resize(term.cols, term.rows)
  })
  if (termEl.value) ro.observe(termEl.value)

  termClose = () => {
    ro.disconnect()
    close()
  }
  termDispose = () => term.dispose()
})

onBeforeUnmount(() => {
  termClose?.()
  termDispose?.()
})
</script>

<template>
  <div class="pt-2">
    <div
      ref="termEl"
      class="rounded-lg overflow-hidden border border-default bg-[#1e1e2e]"
      style="height: 400px;"
      aria-label="Sandbox terminal"
    />
  </div>
</template>
