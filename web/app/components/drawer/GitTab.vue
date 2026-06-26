<script setup lang="ts">
const props = defineProps<{
  sandbox: {
    id: string
    branch?: string
    last_publish?: string
  }
}>()

const api = useApi()
const session = useSession()
const toast = useToast()
const trackOp = useOpTracker()

const publishLoading = ref(false)
const branchOverride = ref('')

// ── Branch selection ─────────────────────────────────────────────────────────
const branches = ref<string[]>([])
const selected = ref<string[]>([])
const branchesLoading = ref(false)
const branchesLoaded = ref(false)

function fmtDate(ts: string | null | undefined): string {
  if (!ts) return '—'
  try { return new Date(ts).toLocaleString() } catch { return ts }
}

async function loadBranches() {
  branchesLoading.value = true
  try {
    const res = await api.get(`/v1/sandboxes/${props.sandbox.id}/git/branches`)
    branches.value = Array.isArray(res) ? res : (res?.branches ?? [])
    selected.value = []
    branchesLoaded.value = true
  } catch (e: any) {
    toast.add({ title: 'Failed to list branches', description: e?.message, color: 'error' })
  } finally {
    branchesLoading.value = false
  }
}

async function publishSelected() {
  if (!selected.value.length) return
  publishLoading.value = true
  const n = selected.value.length
  try {
    const op = await api.post(`/v1/sandboxes/${props.sandbox.id}/git/publish`, { branches: selected.value })
    toast.add({ title: `Publishing ${n} branch${n > 1 ? 'es' : ''}…`, color: 'info', icon: 'i-lucide-loader' })
    trackOp(op?.id, {
      onDone: () => toast.add({ title: `Published ${n} branch${n > 1 ? 'es' : ''}`, color: 'success', icon: 'i-lucide-check-circle' }),
      onError: (o) => toast.add({
        title: 'Publish failed',
        description: o.error || 'The publish did not complete.',
        color: 'error',
        icon: 'i-lucide-alert-circle',
      }),
    })
  } catch (e: any) {
    toast.add({ title: 'Publish failed', description: e?.message, color: 'error' })
  } finally {
    publishLoading.value = false
  }
}

async function doPublish() {
  publishLoading.value = true
  try {
    const body: Record<string, string> = {}
    if (branchOverride.value) body.branch = branchOverride.value
    const op = await api.post(`/v1/sandboxes/${props.sandbox.id}/git/publish`, body)
    // Publish is async: toast on the terminal op, not on accept — otherwise a
    // failed (or no-op) publish is silent.
    toast.add({ title: 'Publishing…', color: 'info', icon: 'i-lucide-loader' })
    trackOp(op?.id, {
      onDone: () => toast.add({ title: 'Published', color: 'success', icon: 'i-lucide-check-circle' }),
      onError: (o) => toast.add({
        title: 'Publish failed',
        description: o.error || 'The publish did not complete.',
        color: 'error',
        icon: 'i-lucide-alert-circle',
      }),
    })
  } catch (e: any) {
    toast.add({ title: 'Publish failed', description: e?.message, color: 'error' })
  } finally {
    publishLoading.value = false
  }
}
</script>

<template>
  <div class="flex flex-col gap-6 pt-4">

    <!-- Branch info -->
    <div class="grid grid-cols-[auto_1fr] gap-x-4 gap-y-2 items-baseline text-sm">
      <span class="text-muted font-medium">Branch</span>
      <span class="font-mono text-default">{{ sandbox.branch ?? '—' }}</span>

      <span class="text-muted font-medium">Last publish</span>
      <span class="tabular-nums text-muted">{{ fmtDate(sandbox.last_publish) }}</span>
    </div>

    <!-- Publish form (admin only) -->
    <template v-if="session.isAdmin.value">
      <USeparator />
      <div class="flex flex-col gap-3">
        <p class="text-xs font-semibold text-muted uppercase tracking-wide">Publish</p>
        <div class="flex flex-col gap-2">
          <UInput
            v-model="branchOverride"
            :placeholder="`branch override (default: ${sandbox.branch ?? 'current'})`"
            size="sm"
            aria-label="Branch override"
          />
          <UButton
            label="Publish now"
            icon="i-lucide-git-branch"
            size="sm"
            color="primary"
            :loading="publishLoading"
            @click="doPublish"
          />
        </div>
        <p class="text-xs text-muted">
          Publishes the sandbox workspace to the git branch. Leave override empty to use the
          sandbox's configured branch. The returned operation can be tracked on the Operations page.
        </p>
      </div>

      <!-- Multi-select: pick which of the agent's branches to publish -->
      <USeparator />
      <div class="flex flex-col gap-3">
        <div class="flex items-center justify-between">
          <p class="text-xs font-semibold text-muted uppercase tracking-wide">Agent branches</p>
          <UButton
            label="Refresh"
            icon="i-lucide-refresh-cw"
            size="xs"
            color="neutral"
            variant="ghost"
            :loading="branchesLoading"
            @click="loadBranches"
          />
        </div>

        <p v-if="!branchesLoaded" class="text-xs text-muted">
          Refresh to list the agent's branches (the sandbox must be running).
        </p>
        <p v-else-if="branches.length === 0" class="text-xs text-muted">No branches found.</p>
        <template v-else>
          <UCheckboxGroup v-model="selected" :items="branches" class="font-mono text-sm" />
          <UButton
            :label="`Publish selected (${selected.length})`"
            icon="i-lucide-upload"
            size="sm"
            color="primary"
            :loading="publishLoading"
            :disabled="selected.length === 0"
            @click="publishSelected"
          />
        </template>
      </div>
    </template>

    <UAlert
      v-else
      color="neutral"
      variant="subtle"
      icon="i-lucide-lock"
      title="Admin only"
      description="Publishing requires admin access."
    />

  </div>
</template>
