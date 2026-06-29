<script setup lang="ts">
import type { TabsItem } from '@nuxt/ui'

const props = defineProps<{
  id: string | null
  open: boolean
}>()

const emit = defineEmits<{
  'update:open': [value: boolean]
}>()

const api = useApi()
const toast = useToast()

// ── Tab state ────────────────────────────────────────────────────────────────
// The Terminal tab is mounted lazily on first visit, then kept alive (v-show, not
// unmounted) for the rest of the drawer's life so switching tabs never drops the
// session. Other tabs keep the default unmount-on-hide so their SSE/polls don't run
// in the background.
const activeTab = ref('info')
const terminalMounted = ref(false)
watch(activeTab, (t) => { if (t === 'terminal') terminalMounted.value = true })

// ── Close confirmation ───────────────────────────────────────────────────────
// A live terminal WebSocket dies when the drawer unmounts; warn before dismissing
// so an accidental backdrop/Escape/X doesn't drop the session.
const terminalActive = ref(false)
const confirmCloseOpen = ref(false)

function requestClose(value: boolean) {
  if (!value && terminalActive.value) {
    confirmCloseOpen.value = true // intercept: keep the drawer open, ask first
    return
  }
  emit('update:open', value)
}

function confirmClose() {
  confirmCloseOpen.value = false
  emit('update:open', false)
}

async function copyId() {
  if (!props.id) return
  try {
    await navigator.clipboard.writeText(props.id)
    toast.add({ title: 'Sandbox ID copied', color: 'success', icon: 'i-lucide-copy' })
  } catch { /* clipboard unavailable — non-critical */ }
}

// ── Sandbox data ─────────────────────────────────────────────────────────────
const sandbox = ref<any>(null)
const loading = ref(false)

async function fetchSandbox() {
  if (!props.id) return
  loading.value = true
  sandbox.value = null
  try {
    sandbox.value = await api.get(`/v1/sandboxes/${props.id}`)
  } catch (e: any) {
    toast.add({ title: 'Failed to load sandbox', description: e?.message, color: 'error' })
  } finally {
    loading.value = false
  }
}

// Fetch when drawer opens; clear when it closes
watch(
  () => props.open,
  (isOpen) => {
    if (isOpen && props.id) {
      fetchSandbox()
    } else {
      sandbox.value = null
      terminalActive.value = false
      confirmCloseOpen.value = false
      terminalMounted.value = false
      activeTab.value = 'info'
      terminalMounted.value = false
      activeTab.value = 'info'
    }
  },
  { immediate: true },
)

// Re-fetch if id changes while open
watch(
  () => props.id,
  (id) => {
    if (props.open && id) fetchSandbox()
  },
)

// ── Tabs ─────────────────────────────────────────────────────────────────────
const tabItems = computed<TabsItem[]>(() => {
  const items: TabsItem[] = [
    { label: 'Info',     slot: 'info',     value: 'info',     icon: 'i-lucide-info' },
    { label: 'Terminal', slot: 'terminal', value: 'terminal', icon: 'i-lucide-terminal' },
    { label: 'Stats',    slot: 'stats',    value: 'stats',    icon: 'i-lucide-bar-chart-2' },
    { label: 'Logs',     slot: 'logs',     value: 'logs',     icon: 'i-lucide-scroll-text' },
    { label: 'Network',  slot: 'network',  value: 'network',  icon: 'i-lucide-network' },
    { label: 'Secrets',  slot: 'secrets',  value: 'secrets',  icon: 'i-lucide-lock' },
  ]
  if (sandbox.value?.branch) {
    items.push({ label: 'Git', slot: 'git', value: 'git', icon: 'i-lucide-git-branch' })
  }
  items.push({ label: 'Files', slot: 'files', value: 'files', icon: 'i-lucide-folder' })
  return items
})
</script>

<template>
  <USlideover
    :open="open"
    :title="id ?? 'Sandbox'"
    :description="sandbox?.owner_node ? `Owner: ${sandbox.owner_node}` : undefined"
    :ui="{ content: 'max-w-4xl' }"
    @update:open="requestClose"
  >
    <!-- Rich header: id + status + owner + branch + copy -->
    <template #header="{ close }">
      <div class="flex items-start justify-between gap-3 w-full">
        <div class="flex flex-col gap-1.5 min-w-0">
          <div class="flex items-center gap-1.5">
            <UIcon name="i-lucide-box" class="size-4 text-primary shrink-0" />
            <span class="font-mono text-sm font-semibold text-highlighted truncate">{{ id }}</span>
            <UButton
              icon="i-lucide-copy"
              size="xs"
              color="neutral"
              variant="ghost"
              aria-label="Copy sandbox ID"
              @click="copyId"
            />
          </div>
          <div class="flex items-center gap-2 flex-wrap">
            <StatusPill v-if="sandbox?.status" :status="sandbox.status" kind="sandbox" size="xs" />
            <span v-if="sandbox?.owner_node" class="font-mono text-xs text-muted">{{ sandbox.owner_node }}</span>
            <UBadge
              v-if="sandbox?.branch"
              :label="sandbox.branch"
              icon="i-lucide-git-branch"
              color="neutral"
              variant="subtle"
              size="xs"
              class="font-mono"
            />
          </div>
        </div>
        <UButton
          icon="i-lucide-x"
          color="neutral"
          variant="ghost"
          size="sm"
          aria-label="Close"
          @click="close"
        />
      </div>
    </template>

    <template #body>
      <!-- Loading state -->
      <div v-if="loading" class="flex flex-col gap-3 p-4">
        <USkeleton class="h-4 w-1/2" />
        <USkeleton class="h-4 w-3/4" />
        <USkeleton class="h-4 w-2/3" />
      </div>

      <!-- Tab contents — only mounted while the drawer is open -->
      <div v-else-if="open" class="p-4">
        <UTabs v-model="activeTab" :items="tabItems" class="w-full">
          <!-- Info tab: real implementation -->
          <template #info>
            <div class="pt-4">
              <DrawerInfoTab v-if="sandbox" :sandbox="sandbox" @updated="sandbox = $event" />
              <UAlert
                v-else
                color="neutral"
                variant="subtle"
                title="Loading sandbox data…"
              />
            </div>
          </template>

          <!-- Terminal tab: panel left empty on purpose — the terminal is rendered
               once below and kept alive via v-show so it survives tab switches. -->

          <!-- Stats tab -->
          <template #stats>
            <DrawerStatsTab v-if="id" :id="id" />
          </template>

          <!-- Logs tab -->
          <template #logs>
            <DrawerLogsTab v-if="id" :id="id" />
          </template>

          <!-- Network tab -->
          <template #network>
            <DrawerNetworkTab v-if="id" :id="id" />
          </template>

          <!-- Secrets tab -->
          <template #secrets>
            <DrawerSecretsTab v-if="id" :id="id" />
          </template>

          <!-- Git tab (only shown when sandbox has a branch) -->
          <template #git>
            <DrawerGitTab v-if="sandbox" :sandbox="sandbox" />
          </template>

          <!-- Files tab -->
          <template #files>
            <DrawerFilesTab v-if="id" :id="id" />
          </template>
        </UTabs>

        <!-- Persistent terminal: mounted on first visit, then kept alive across tab
             switches via v-show (the Terminal tab's own panel above is left empty). -->
        <div v-if="terminalMounted && id" v-show="activeTab === 'terminal'">
          <DrawerTerminalTab :id="id" @active="terminalActive = $event" />
        </div>
      </div>
    </template>
  </USlideover>

  <!-- Confirm before dismissing while a terminal session is live -->
  <UModal
    v-model:open="confirmCloseOpen"
    title="End terminal session?"
    description="The terminal is still connected to this sandbox. Closing the drawer will end the session."
    :ui="{ footer: 'justify-end' }"
  >
    <template #footer="{ close }">
      <UButton label="Keep open" color="neutral" variant="outline" @click="close" />
      <UButton label="Close & end session" color="error" @click="confirmClose" />
    </template>
  </UModal>
</template>
