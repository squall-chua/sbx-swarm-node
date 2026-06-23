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

const publishLoading = ref(false)
const branchOverride = ref('')

function fmtDate(ts: string | null | undefined): string {
  if (!ts) return '—'
  try { return new Date(ts).toLocaleString() } catch { return ts }
}

async function doPublish() {
  publishLoading.value = true
  try {
    const body: Record<string, string> = {}
    if (branchOverride.value) body.branch = branchOverride.value
    const op = await api.post(`/v1/sandboxes/${props.sandbox.id}/git/publish`, body)
    toast.add({
      title: 'Publish triggered',
      description: op?.id ? `Operation: ${op.id}` : undefined,
      color: 'success',
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
