<script setup lang="ts">
const props = defineProps<{
  sandbox: {
    id: string
    name?: string
    owner_node?: string
    status?: string
    agent?: string
    branch?: string
    last_publish?: string
    labels?: Record<string, string>
    workspaces?: Array<{ name: string; read_only?: boolean }>
    ports?: Array<{ container_port: number; host_port?: number; protocol?: string }>
  }
}>()

const emit = defineEmits<{ updated: [sandbox: any] }>()

const api = useApi()
const session = useSession()
const toast = useToast()
const trackOp = useOpTracker()

// ── Status → color ──────────────────────────────────────────────────────────
function statusColor(status: string | undefined): string {
  switch (status) {
    case 'running':
    case 'published':
    case 'done':
      return 'success'
    case 'pending':
    case 'running-operation':
    case 'draining':
      return 'warning'
    case 'stopped':
    case 'deleted':
    case 'lost':
    case 'error':
    case 'publish_failed':
    case 'revoke':
      return 'error'
    default:
      return 'neutral'
  }
}

// ── Ports ───────────────────────────────────────────────────────────────────
const ports = ref<Array<{ container_port: number; host_port?: number; protocol?: string }>>(props.sandbox.ports ?? [])
const publishPort = ref<number | null>(null)
const publishLoading = ref(false)
const unpublishing = ref<number | null>(null) // container port currently being unpublished

async function fetchPorts() {
  try {
    const res = await api.get(`/v1/sandboxes/${props.sandbox.id}/ports`)
    ports.value = Array.isArray(res) ? res : (res?.ports ?? [])
  } catch {
    // best-effort
  }
}

async function doPublishPort() {
  if (!publishPort.value) return
  publishLoading.value = true
  try {
    await api.post(`/v1/sandboxes/${props.sandbox.id}/ports`, { container_port: publishPort.value })
    publishPort.value = null
    await fetchPorts()
    toast.add({ title: 'Port published', color: 'success' })
  } catch (e: any) {
    toast.add({ title: 'Failed to publish port', description: e?.message, color: 'error' })
  } finally {
    publishLoading.value = false
  }
}

async function doUnpublishPort(containerPort: number) {
  unpublishing.value = containerPort
  try {
    await api.del(`/v1/sandboxes/${props.sandbox.id}/ports/${containerPort}`)
    await fetchPorts()
    toast.add({ title: 'Port unpublished', color: 'success' })
  } catch (e: any) {
    toast.add({ title: 'Failed to unpublish port', description: e?.message, color: 'error' })
  } finally {
    unpublishing.value = null
  }
}

// ── Actions ─────────────────────────────────────────────────────────────────
const actionLoading = ref<string | null>(null)
const deleteConfirmOpen = ref(false)

// Lifecycle gating: Start only makes sense when not running, Stop only when running.
const isRunning = computed(() => props.sandbox.status === 'running')

async function doAction(action: string) {
  actionLoading.value = action
  try {
    // Start/Stop/KeepAlive are synchronous and return the updated sandbox. Bubble it
    // up so the drawer reflects the new status, and toast the actual result.
    const updated = await api.post(`/v1/sandboxes/${props.sandbox.id}/${action}`)
    if (updated && typeof updated === 'object') emit('updated', updated)
    if (action === 'keepalive') {
      toast.add({ title: 'Keep-alive sent', color: 'success', icon: 'i-lucide-heart-pulse' })
    } else {
      toast.add({ title: `Sandbox is now ${updated?.status ?? action}`, color: 'success', icon: 'i-lucide-check-circle' })
    }
  } catch (e: any) {
    toast.add({ title: `Failed: ${action}`, description: e?.message, color: 'error' })
  } finally {
    actionLoading.value = null
  }
}

async function doDelete() {
  deleteConfirmOpen.value = false
  actionLoading.value = 'delete'
  try {
    // Delete is async: the API returns a pending operation. Toast on the actual
    // removal (terminal op), not on accept — otherwise a failed delete is silent.
    const op = await api.del(`/v1/sandboxes/${props.sandbox.id}`)
    toast.add({ title: 'Removing sandbox…', color: 'info', icon: 'i-lucide-loader' })
    trackOp(op?.id, {
      onDone: () => toast.add({ title: 'Sandbox removed', color: 'success', icon: 'i-lucide-check-circle' }),
      onError: (o) => toast.add({
        title: 'Failed to remove sandbox',
        description: o.error || 'The sandbox could not be removed.',
        color: 'error',
        icon: 'i-lucide-alert-circle',
      }),
    })
  } catch (e: any) {
    toast.add({ title: 'Failed to delete sandbox', description: e?.message, color: 'error' })
  } finally {
    actionLoading.value = null
  }
}

function fmtDate(ts: string | null | undefined): string {
  if (!ts) return '—'
  try { return new Date(ts).toLocaleString() } catch { return ts }
}

onMounted(fetchPorts)
</script>

<template>
  <div class="flex flex-col gap-6">

    <!-- ── Metadata ─────────────────────────────────────────────────────── -->
    <div class="flex flex-col gap-3">
      <div class="grid grid-cols-[auto_1fr] gap-x-4 gap-y-2 items-baseline text-sm">
        <template v-if="sandbox.name">
          <span class="text-muted font-medium">Name</span>
          <span class="text-sm font-medium text-default break-all">{{ sandbox.name }}</span>
        </template>

        <span class="text-muted font-medium">ID</span>
        <span class="font-mono text-sm text-default break-all">{{ sandbox.id }}</span>

        <span class="text-muted font-medium">Owner node</span>
        <span class="font-mono text-sm text-muted">{{ sandbox.owner_node ?? '—' }}</span>

        <span class="text-muted font-medium">Status</span>
        <UBadge
          :label="sandbox.status ?? 'unknown'"
          :color="statusColor(sandbox.status)"
          variant="subtle"
          size="sm"
          class="justify-self-start"
        />

        <template v-if="sandbox.agent">
          <span class="text-muted font-medium">Agent</span>
          <span class="text-sm text-default">{{ sandbox.agent }}</span>
        </template>

        <template v-if="sandbox.branch">
          <span class="text-muted font-medium">Branch</span>
          <span class="font-mono text-xs text-muted">{{ sandbox.branch }}</span>
        </template>

        <span class="text-muted font-medium">Last publish</span>
        <span class="text-xs text-muted tabular-nums">{{ fmtDate(sandbox.last_publish) }}</span>

        <template v-if="sandbox.workspaces && sandbox.workspaces.length > 0">
          <span class="text-muted font-medium">Workspaces</span>
          <div class="flex flex-wrap gap-1">
            <UBadge
              v-for="w in sandbox.workspaces"
              :key="w.name"
              :label="w.read_only ? `${w.name} (ro)` : w.name"
              :color="w.read_only ? 'neutral' : 'info'"
              variant="subtle"
              size="sm"
              class="font-mono"
            />
          </div>
        </template>

        <template v-if="sandbox.labels && Object.keys(sandbox.labels).length > 0">
          <span class="text-muted font-medium">Labels</span>
          <div class="flex flex-wrap gap-1">
            <UBadge
              v-for="(v, k) in sandbox.labels"
              :key="k"
              :label="`${k}=${v}`"
              color="neutral"
              variant="subtle"
              size="sm"
            />
          </div>
        </template>
      </div>
    </div>

    <!-- ── Actions (admin only) ─────────────────────────────────────────── -->
    <template v-if="session.isAdmin.value">
      <USeparator />
      <div class="flex flex-col gap-2">
        <p class="text-xs font-semibold text-muted uppercase tracking-wide">Actions</p>
        <div class="flex flex-wrap gap-2">
          <UButton
            data-test="start"
            label="Start"
            icon="i-lucide-play"
            size="sm"
            color="success"
            variant="outline"
            :loading="actionLoading === 'start'"
            :disabled="isRunning"
            @click="doAction('start')"
          />
          <UButton
            data-test="stop"
            label="Stop"
            icon="i-lucide-square"
            size="sm"
            color="warning"
            variant="outline"
            :loading="actionLoading === 'stop'"
            :disabled="!isRunning"
            @click="doAction('stop')"
          />
          <UButton
            data-test="keepalive"
            label="Keep Alive"
            icon="i-lucide-heart-pulse"
            size="sm"
            color="neutral"
            variant="outline"
            :loading="actionLoading === 'keepalive'"
            @click="doAction('keepalive')"
          />
          <UButton
            data-test="delete"
            label="Delete"
            icon="i-lucide-trash-2"
            size="sm"
            color="error"
            variant="outline"
            :loading="actionLoading === 'delete'"
            @click="deleteConfirmOpen = true"
          />
        </div>
      </div>

      <!-- Delete confirm modal -->
      <UModal
        v-model:open="deleteConfirmOpen"
        title="Delete sandbox"
        description="This will permanently delete the sandbox. This action cannot be undone."
        :ui="{ footer: 'justify-end' }"
      >
        <template #footer="{ close }">
          <UButton label="Cancel" color="neutral" variant="outline" @click="close" />
          <UButton label="Delete" color="error" @click="doDelete" />
        </template>
      </UModal>
    </template>

    <!-- ── Ports ────────────────────────────────────────────────────────── -->
    <USeparator />
    <div class="flex flex-col gap-3">
      <p class="text-xs font-semibold text-muted uppercase tracking-wide">Ports</p>

      <div v-if="ports.length > 0" class="flex flex-col gap-1">
        <div
          v-for="p in ports"
          :key="p.container_port"
          class="flex items-center gap-2 text-sm"
        >
          <span class="font-mono text-default">{{ p.container_port }}</span>
          <span class="text-muted">→</span>
          <span class="font-mono text-muted">{{ p.host_port ?? '—' }}</span>
          <UBadge
            v-if="p.protocol"
            :label="p.protocol"
            color="neutral"
            variant="subtle"
            size="xs"
          />
          <UButton
            v-if="session.isAdmin.value"
            icon="i-lucide-x"
            color="error"
            variant="ghost"
            size="xs"
            class="ml-auto"
            :loading="unpublishing === p.container_port"
            :aria-label="`Unpublish port ${p.container_port}`"
            @click="doUnpublishPort(p.container_port)"
          />
        </div>
      </div>
      <p v-else class="text-sm text-muted">No ports published.</p>

      <!-- Publish port form (admin only) -->
      <template v-if="session.isAdmin.value">
        <div class="flex items-center gap-2">
          <UInputNumber
            v-model="publishPort"
            placeholder="Container port"
            :min="1"
            :max="65535"
            size="sm"
            class="w-40"
            aria-label="Container port to publish"
          />
          <UButton
            label="Publish"
            size="sm"
            :loading="publishLoading"
            :disabled="!publishPort"
            @click="doPublishPort"
          />
        </div>
      </template>
    </div>

  </div>
</template>
