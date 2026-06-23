<script setup lang="ts">
const api = useApi()
const session = useSession()
const toast = useToast()

interface NodeInfo {
  node_id: string
  node_name: string
  version: string
  cordoned: boolean
  draining: boolean
  role: string
}

const nodeInfo = ref<NodeInfo | null>(null)
const loading = ref(false)

async function fetchNodeInfo() {
  loading.value = true
  try {
    nodeInfo.value = await api.get('/v1/node')
  } catch (e: any) {
    toast.add({ title: 'Failed to load node info', description: e?.message, color: 'error' })
  } finally {
    loading.value = false
  }
}

function roleColor(role: string): string {
  if (role === 'admin') return 'primary'
  return 'neutral'
}

onMounted(fetchNodeInfo)
</script>

<template>
  <div class="flex flex-col gap-6 p-4 md:p-6">

    <!-- Page header -->
    <div class="flex flex-wrap items-center justify-between gap-3">
      <h1 class="text-lg font-semibold text-highlighted">Settings</h1>
      <UButton
        label="Sign out"
        icon="i-lucide-log-out"
        color="error"
        variant="outline"
        size="sm"
        @click="session.logout()"
      />
    </div>

    <!-- Node self info -->
    <UCard variant="outline">
      <template #header>
        <p class="text-sm font-semibold text-highlighted">This node</p>
      </template>

      <!-- Loading skeleton -->
      <div v-if="loading" class="flex flex-col gap-3">
        <USkeleton class="h-4 w-full" />
        <USkeleton class="h-4 w-2/3" />
        <USkeleton class="h-4 w-1/2" />
      </div>

      <div v-else-if="nodeInfo" class="flex flex-col gap-3">
        <!-- node_id -->
        <div class="flex items-start justify-between gap-4">
          <span class="text-xs text-muted uppercase tracking-wide font-medium w-28 shrink-0 pt-0.5">Node ID</span>
          <span class="font-mono text-sm text-default break-all">{{ nodeInfo.node_id }}</span>
        </div>

        <!-- node_name -->
        <div class="flex items-start justify-between gap-4">
          <span class="text-xs text-muted uppercase tracking-wide font-medium w-28 shrink-0 pt-0.5">Name</span>
          <span class="font-mono text-sm text-default">{{ nodeInfo.node_name }}</span>
        </div>

        <!-- version -->
        <div class="flex items-start justify-between gap-4">
          <span class="text-xs text-muted uppercase tracking-wide font-medium w-28 shrink-0 pt-0.5">Version</span>
          <span class="font-mono text-sm text-muted">{{ nodeInfo.version || '—' }}</span>
        </div>

        <!-- role -->
        <div class="flex items-start justify-between gap-4">
          <span class="text-xs text-muted uppercase tracking-wide font-medium w-28 shrink-0 pt-0.5">Role</span>
          <UBadge
            :label="nodeInfo.role || 'unknown'"
            :color="roleColor(nodeInfo.role)"
            variant="subtle"
            size="sm"
          />
        </div>

        <!-- cordoned -->
        <div class="flex items-start justify-between gap-4">
          <span class="text-xs text-muted uppercase tracking-wide font-medium w-28 shrink-0 pt-0.5">Cordoned</span>
          <UBadge
            v-if="nodeInfo.cordoned"
            label="Yes"
            color="warning"
            variant="subtle"
            size="sm"
          />
          <span v-else class="text-sm text-muted">No</span>
        </div>

        <!-- draining -->
        <div class="flex items-start justify-between gap-4">
          <span class="text-xs text-muted uppercase tracking-wide font-medium w-28 shrink-0 pt-0.5">Draining</span>
          <UBadge
            v-if="nodeInfo.draining"
            label="Yes"
            color="warning"
            variant="subtle"
            size="sm"
          />
          <span v-else class="text-sm text-muted">No</span>
        </div>
      </div>

      <UAlert
        v-else
        icon="i-lucide-server-off"
        title="Could not load node info"
        description="Check network connectivity and try refreshing."
        color="error"
        variant="subtle"
      />
    </UCard>

  </div>
</template>
