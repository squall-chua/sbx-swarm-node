<script setup lang="ts">
const props = defineProps<{ id: string }>()

const api = useApi()
const session = useSession()
const toast = useToast()

// ── Blocked egress ───────────────────────────────────────────────────────────
interface BlockedEntry { host: string; first_seen: string; last_seen: string }
interface BlockedResponse { distinct_count?: number; entries?: BlockedEntry[] }

const blocked = ref<BlockedResponse>({ distinct_count: 0, entries: [] })
const blockedLoading = ref(false)

async function fetchBlocked() {
  blockedLoading.value = true
  try {
    blocked.value = await api.get(`/v1/sandboxes/${props.id}/network/blocked`)
  } catch (e: any) {
    toast.add({ title: 'Failed to load blocked egress', description: e?.message, color: 'error' })
  } finally {
    blockedLoading.value = false
  }
}

const blockedColumns = [
  { accessorKey: 'host',       header: 'Host' },
  { accessorKey: 'first_seen', header: 'First Seen' },
  { accessorKey: 'last_seen',  header: 'Last Seen' },
]

function fmtDate(ts: string | null | undefined): string {
  if (!ts) return '—'
  try { return new Date(ts).toLocaleString() } catch { return ts }
}

const blockedRows = computed(() =>
  (blocked.value.entries ?? []).map((e) => ({
    host: e.host,
    first_seen: fmtDate(e.first_seen),
    last_seen: fmtDate(e.last_seen),
  })),
)

// ── Policy ───────────────────────────────────────────────────────────────────
interface PolicyRule { host: string; decision: 'allow' | 'deny' }
interface PolicyResponse { rules?: PolicyRule[] }

const policy = ref<PolicyResponse>({ rules: [] })
const policyLoading = ref(false)

async function fetchPolicy() {
  policyLoading.value = true
  try {
    policy.value = await api.get(`/v1/sandboxes/${props.id}/policy`)
  } catch (e: any) {
    toast.add({ title: 'Failed to load policy', description: e?.message, color: 'error' })
  } finally {
    policyLoading.value = false
  }
}

// Add policy rule form
const addHost = ref('')
const addDecision = ref<'allow' | 'deny'>('allow')
const addLoading = ref(false)

const decisionOptions = [
  { label: 'Allow', value: 'allow' },
  { label: 'Deny',  value: 'deny' },
]

async function doAddRule() {
  if (!addHost.value) return
  addLoading.value = true
  try {
    await api.put(`/v1/sandboxes/${props.id}/policy`, {
      scope: props.id,
      decision: addDecision.value,
      host: addHost.value,
    })
    toast.add({ title: 'Policy rule added', color: 'success' })
    addHost.value = ''
    await fetchPolicy()
  } catch (e: any) {
    toast.add({ title: 'Failed to add policy rule', description: e?.message, color: 'error' })
  } finally {
    addLoading.value = false
  }
}

onMounted(() => {
  fetchBlocked()
  fetchPolicy()
})
</script>

<template>
  <div class="flex flex-col gap-6 pt-4">

    <!-- ── Blocked Egress ───────────────────────────────────────────────── -->
    <div class="flex flex-col gap-3">
      <div class="flex items-center justify-between">
        <p class="text-xs font-semibold text-muted uppercase tracking-wide">
          Blocked Egress
        </p>
        <UBadge
          v-if="blocked.distinct_count != null"
          :label="`${blocked.distinct_count} distinct hosts`"
          color="neutral"
          variant="subtle"
          size="xs"
        />
      </div>

      <div v-if="blockedLoading" class="flex flex-col gap-2">
        <USkeleton class="h-4 w-full" />
        <USkeleton class="h-4 w-3/4" />
      </div>

      <UTable
        v-else
        :data="blockedRows"
        :columns="blockedColumns"
        class="text-sm"
      >
        <template #host-cell="{ row }">
          <span class="font-mono text-default">{{ row.original.host }}</span>
        </template>
        <template #first_seen-cell="{ row }">
          <span class="tabular-nums text-muted">{{ row.original.first_seen }}</span>
        </template>
        <template #last_seen-cell="{ row }">
          <span class="tabular-nums text-muted">{{ row.original.last_seen }}</span>
        </template>
        <template #empty>
          <div class="flex flex-col items-center justify-center gap-2 py-8 text-center">
            <UIcon name="i-lucide-shield-check" class="size-6 text-muted" aria-hidden="true" />
            <p class="text-sm text-muted">No blocked egress recorded.</p>
          </div>
        </template>
      </UTable>
    </div>

    <USeparator />

    <!-- ── Policy rules ────────────────────────────────────────────────── -->
    <div class="flex flex-col gap-3">
      <p class="text-xs font-semibold text-muted uppercase tracking-wide">
        Egress Policy Rules
      </p>

      <div v-if="policyLoading" class="flex flex-col gap-2">
        <USkeleton class="h-4 w-full" />
        <USkeleton class="h-4 w-2/3" />
      </div>

      <div v-else-if="(policy.rules ?? []).length > 0" class="flex flex-col gap-2">
        <div
          v-for="rule in policy.rules"
          :key="`${rule.decision}:${rule.host}`"
          class="flex items-center gap-3 rounded-md bg-elevated px-3 py-2 text-sm"
        >
          <UBadge
            :label="rule.decision"
            :color="rule.decision === 'allow' ? 'success' : 'error'"
            variant="subtle"
            size="xs"
          />
          <span class="font-mono text-default truncate">{{ rule.host }}</span>
        </div>
      </div>
      <p v-else class="text-sm text-muted">No policy rules configured.</p>

      <!-- Note: rules can't be deleted -->
      <UAlert
        color="neutral"
        variant="subtle"
        icon="i-lucide-info"
        title="Add-only"
        description="Rules cannot be deleted via the console — no remove-rule API is available."
        size="xs"
      />

      <!-- Add rule form (admin only) -->
      <template v-if="session.isAdmin.value">
        <div class="flex flex-col gap-2">
          <p class="text-xs font-medium text-muted">Add rule</p>
          <div class="flex gap-2 flex-wrap">
            <USelect
              v-model="addDecision"
              :items="decisionOptions"
              value-key="value"
              size="sm"
              class="w-28"
              aria-label="Decision"
            />
            <UInput
              v-model="addHost"
              placeholder="host (e.g. api.example.com)"
              size="sm"
              class="flex-1 min-w-40"
              aria-label="Policy host"
            />
            <UButton
              label="Add"
              icon="i-lucide-plus"
              size="sm"
              :loading="addLoading"
              :disabled="!addHost"
              @click="doAddRule"
            />
          </div>
        </div>
      </template>
    </div>

  </div>
</template>
