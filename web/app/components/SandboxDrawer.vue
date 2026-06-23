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
    { label: 'Info',     slot: 'info',     icon: 'i-lucide-info' },
    { label: 'Terminal', slot: 'terminal', icon: 'i-lucide-terminal' },
    { label: 'Stats',    slot: 'stats',    icon: 'i-lucide-bar-chart-2' },
    { label: 'Logs',     slot: 'logs',     icon: 'i-lucide-scroll-text' },
    { label: 'Network',  slot: 'network',  icon: 'i-lucide-network' },
    { label: 'Secrets',  slot: 'secrets',  icon: 'i-lucide-lock' },
  ]
  if (sandbox.value?.branch) {
    items.push({ label: 'Git', slot: 'git', icon: 'i-lucide-git-branch' })
  }
  items.push({ label: 'Files', slot: 'files', icon: 'i-lucide-folder' })
  return items
})
</script>

<template>
  <USlideover
    :open="open"
    :title="id ?? 'Sandbox'"
    :description="sandbox?.owner_node ? `Owner: ${sandbox.owner_node}` : undefined"
    :ui="{ width: 'max-w-2xl' }"
    @update:open="emit('update:open', $event)"
  >
    <template #body>
      <!-- Loading state -->
      <div v-if="loading" class="flex flex-col gap-3 p-4">
        <USkeleton class="h-4 w-1/2" />
        <USkeleton class="h-4 w-3/4" />
        <USkeleton class="h-4 w-2/3" />
      </div>

      <!-- Tab contents — only mounted while the drawer is open -->
      <div v-else-if="open" class="p-4">
        <UTabs :items="tabItems" class="w-full">
          <!-- Info tab: real implementation -->
          <template #info>
            <div class="pt-4">
              <DrawerInfoTab v-if="sandbox" :sandbox="sandbox" />
              <UAlert
                v-else
                color="neutral"
                variant="subtle"
                title="Loading sandbox data…"
              />
            </div>
          </template>

          <!-- Terminal tab -->
          <template #terminal>
            <DrawerTerminalTab v-if="id" :id="id" />
          </template>

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
            <DrawerFilesTab />
          </template>
        </UTabs>
      </div>
    </template>
  </USlideover>
</template>
