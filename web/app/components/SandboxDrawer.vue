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

          <!-- Terminal tab: placeholder for Task 11 -->
          <template #terminal>
            <div class="pt-4">
              <!-- TODO(Task 11): <DrawerTerminalTab :id="id" /> -->
              <UAlert color="neutral" variant="subtle" title="Terminal" description="Coming soon" />
            </div>
          </template>

          <!-- Stats tab: placeholder for Task 11 -->
          <template #stats>
            <div class="pt-4">
              <!-- TODO(Task 11): <DrawerStatsTab :id="id" /> -->
              <UAlert color="neutral" variant="subtle" title="Stats" description="Coming soon" />
            </div>
          </template>

          <!-- Logs tab: placeholder for Task 11 -->
          <template #logs>
            <div class="pt-4">
              <!-- TODO(Task 11): <DrawerLogsTab :id="id" /> -->
              <UAlert color="neutral" variant="subtle" title="Logs" description="Coming soon" />
            </div>
          </template>

          <!-- Network tab: placeholder for Task 12 -->
          <template #network>
            <div class="pt-4">
              <!-- TODO(Task 12): <DrawerNetworkTab :sandbox="sandbox" /> -->
              <UAlert color="neutral" variant="subtle" title="Network" description="Coming soon" />
            </div>
          </template>

          <!-- Secrets tab: placeholder for Task 12 -->
          <template #secrets>
            <div class="pt-4">
              <!-- TODO(Task 12): <DrawerSecretsTab :id="id" /> -->
              <UAlert color="neutral" variant="subtle" title="Secrets" description="Coming soon" />
            </div>
          </template>

          <!-- Git tab: placeholder for Task 12 (only shown when sandbox has a branch) -->
          <template #git>
            <div class="pt-4">
              <!-- TODO(Task 12): <DrawerGitTab :id="id" :branch="sandbox?.branch" /> -->
              <UAlert color="neutral" variant="subtle" title="Git" description="Coming soon" />
            </div>
          </template>

          <!-- Files tab: placeholder for Task 12 -->
          <template #files>
            <div class="pt-4">
              <!-- TODO(Task 12): <DrawerFilesTab :id="id" /> -->
              <UAlert color="neutral" variant="subtle" title="Files" description="Coming soon" />
            </div>
          </template>
        </UTabs>
      </div>
    </template>
  </USlideover>
</template>
