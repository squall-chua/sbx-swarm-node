<script setup lang="ts">
const props = defineProps<{ id: string }>()

const api = useApi()
const session = useSession()
const toast = useToast()

// ── Egress log (blocked + allowed) ───────────────────────────────────────────
interface EgressEntry { host: string; first_seen: string; last_seen: string; count?: number }
interface EgressResponse {
  blocked?: EgressEntry[]
  distinct_count?: number
  allowed?: EgressEntry[]
  allowed_distinct_count?: number
}

const egress = ref<EgressResponse>({ blocked: [], distinct_count: 0, allowed: [], allowed_distinct_count: 0 })
const egressLoading = ref(false)

async function fetchEgress() {
  egressLoading.value = true
  try {
    egress.value = await api.get(`/v1/sandboxes/${props.id}/network/blocked`)
  } catch (e: any) {
    toast.add({ title: 'Failed to load egress log', description: e?.message, color: 'error' })
  } finally {
    egressLoading.value = false
  }
}

// Fully fixed column widths (shared by both egress tables) + table-fixed layout so
// the Blocked and Allowed columns line up exactly, regardless of content or whether
// a table is empty. No percentage column (it resolves non-deterministically here).
// The trailing action column (unblock/block) is admin-only.
const egressColumns = computed(() => {
  const cols: any[] = [
    { accessorKey: 'host',       header: 'Host',       meta: { class: { td: 'w-64', th: 'w-64' } } },
    { accessorKey: 'first_seen', header: 'First Seen', meta: { class: { td: 'w-48', th: 'w-48' } } },
    { accessorKey: 'last_seen',  header: 'Last Seen',  meta: { class: { td: 'w-48', th: 'w-48' } } },
    { accessorKey: 'hits',       header: 'Hits',       meta: { class: { td: 'w-16 text-right', th: 'w-16 text-right' } } },
  ]
  if (session.isAdmin.value) {
    cols.push({ id: 'action', header: '', meta: { class: { td: 'w-28 text-right', th: 'w-28' } } })
  }
  return cols
})

function fmtDate(ts: string | null | undefined): string {
  if (!ts) return '—'
  try { return new Date(ts).toLocaleString() } catch { return ts }
}

function toRows(entries: EgressEntry[] | undefined) {
  return (entries ?? []).map((e) => ({
    host: e.host,
    first_seen: fmtDate(e.first_seen),
    last_seen: fmtDate(e.last_seen),
    hits: e.count ?? 0,
  }))
}
const blockedRows = computed(() => toRows(egress.value.blocked))
const allowedRows = computed(() => toRows(egress.value.allowed))

// Unblock (allow) or block (deny) a host straight from the egress log. Egress hosts
// are host:port, the same format policy rules use, so they pass through verbatim.
const policyBusy = ref<string | null>(null)
async function setPolicy(host: string, decision: 'allow' | 'deny') {
  policyBusy.value = `${decision}:${host}`
  try {
    await api.put(`/v1/sandboxes/${props.id}/policy`, { scope: props.id, decision, host })
    toast.add({ title: decision === 'allow' ? `Unblocked ${host}` : `Blocked ${host}`, color: 'success' })
    await Promise.all([fetchPolicy(), fetchEgress()])
  } catch (e: any) {
    toast.add({ title: 'Failed to update policy', description: e?.message, color: 'error' })
  } finally {
    policyBusy.value = null
  }
}

// ── Policy ───────────────────────────────────────────────────────────────────
// ListPolicy returns richer rows than the add form sends: the hosts come back in
// `resources` (comma-joined), plus provenance/applies_to/rule/type — not `host`.
interface PolicyRule {
  provenance?: string
  applies_to?: string
  rule?: string
  type?: string
  decision?: string
  resources?: string
}
interface PolicyResponse { rules?: PolicyRule[] }

function hostsOf(rule: PolicyRule): string[] {
  return rule.resources ? rule.resources.split(',').map((h) => h.trim()).filter(Boolean) : []
}

// Only this sandbox's OWN rules are removable from its scope. Inherited node-global
// rules (applies_to "all") belong to the node scope, not this sandbox.
function isOwnScope(rule: PolicyRule): boolean {
  return rule.applies_to === `sandbox:${props.id}`
}

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

// Remove a rule by deleting each of its resources (sbx removes one host at a time).
const removingKey = ref<string | null>(null)
function ruleKey(rule: PolicyRule): string {
  return `${rule.decision}:${rule.rule}:${rule.resources}`
}
async function doRemoveRule(rule: PolicyRule) {
  const hosts = hostsOf(rule)
  if (!hosts.length) return
  removingKey.value = ruleKey(rule)
  try {
    for (const h of hosts) {
      await api.del(`/v1/sandboxes/${props.id}/policy/${encodeURIComponent(h)}`)
    }
    toast.add({ title: 'Policy rule removed', color: 'success' })
  } catch (e: any) {
    toast.add({ title: 'Failed to remove rule', description: e?.message, color: 'error' })
  } finally {
    await fetchPolicy()
    removingKey.value = null
  }
}

onMounted(() => {
  fetchEgress()
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
          v-if="egress.distinct_count != null"
          :label="`${egress.distinct_count} distinct hosts`"
          color="neutral"
          variant="subtle"
          size="xs"
        />
      </div>

      <div v-if="egressLoading" class="flex flex-col gap-2">
        <USkeleton class="h-4 w-full" />
        <USkeleton class="h-4 w-3/4" />
      </div>

      <UTable
        v-else
        :data="blockedRows"
        :columns="egressColumns"
        :ui="{ base: 'table-fixed' }"
        class="text-sm"
      >
        <template #host-cell="{ row }">
          <span class="font-mono text-default block truncate" :title="row.original.host">{{ row.original.host }}</span>
        </template>
        <template #first_seen-cell="{ row }">
          <span class="tabular-nums text-muted">{{ row.original.first_seen }}</span>
        </template>
        <template #last_seen-cell="{ row }">
          <span class="tabular-nums text-muted">{{ row.original.last_seen }}</span>
        </template>
        <template #hits-cell="{ row }">
          <span class="tabular-nums text-default">{{ row.original.hits }}</span>
        </template>
        <template #action-cell="{ row }">
          <UButton
            label="Unblock"
            icon="i-lucide-shield-off"
            color="success"
            variant="subtle"
            size="xs"
            :loading="policyBusy === `allow:${row.original.host}`"
            @click="setPolicy(row.original.host, 'allow')"
          />
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

    <!-- ── Allowed Egress ───────────────────────────────────────────────── -->
    <div class="flex flex-col gap-3">
      <div class="flex items-center justify-between">
        <p class="text-xs font-semibold text-muted uppercase tracking-wide">
          Allowed Egress
        </p>
        <UBadge
          v-if="egress.allowed_distinct_count != null"
          :label="`${egress.allowed_distinct_count} distinct hosts`"
          color="neutral"
          variant="subtle"
          size="xs"
        />
      </div>

      <div v-if="egressLoading" class="flex flex-col gap-2">
        <USkeleton class="h-4 w-full" />
        <USkeleton class="h-4 w-3/4" />
      </div>

      <UTable
        v-else
        :data="allowedRows"
        :columns="egressColumns"
        :ui="{ base: 'table-fixed' }"
        class="text-sm"
      >
        <template #host-cell="{ row }">
          <span class="font-mono text-default block truncate" :title="row.original.host">{{ row.original.host }}</span>
        </template>
        <template #first_seen-cell="{ row }">
          <span class="tabular-nums text-muted">{{ row.original.first_seen }}</span>
        </template>
        <template #last_seen-cell="{ row }">
          <span class="tabular-nums text-muted">{{ row.original.last_seen }}</span>
        </template>
        <template #hits-cell="{ row }">
          <span class="tabular-nums text-default">{{ row.original.hits }}</span>
        </template>
        <template #action-cell="{ row }">
          <UButton
            label="Block"
            icon="i-lucide-ban"
            color="error"
            variant="subtle"
            size="xs"
            :loading="policyBusy === `deny:${row.original.host}`"
            @click="setPolicy(row.original.host, 'deny')"
          />
        </template>
        <template #empty>
          <div class="flex flex-col items-center justify-center gap-2 py-8 text-center">
            <UIcon name="i-lucide-globe" class="size-6 text-muted" aria-hidden="true" />
            <p class="text-sm text-muted">No allowed egress recorded.</p>
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

      <!-- each rule collapses its hosts/detail behind a summary header -->
      <div v-else-if="(policy.rules ?? []).length > 0" class="flex flex-col gap-2">
        <UCollapsible
          v-for="(rule, i) in policy.rules"
          :key="i"
          class="rounded-md bg-elevated border border-default"
        >
          <button
            type="button"
            class="group flex items-center gap-2 w-full px-3 py-2 text-sm text-left cursor-pointer hover:bg-accented/40 rounded-md transition-colors"
          >
            <UBadge
              v-if="rule.decision"
              :label="rule.decision"
              :icon="rule.decision === 'allow' ? 'i-lucide-check' : 'i-lucide-ban'"
              :color="rule.decision === 'allow' ? 'success' : 'error'"
              variant="subtle"
              size="xs"
              class="shrink-0"
            />
            <span class="font-mono text-default truncate">{{ hostsOf(rule)[0] || rule.rule || 'rule' }}</span>
            <UBadge
              v-if="rule.provenance"
              :label="rule.provenance"
              color="neutral"
              variant="subtle"
              size="xs"
              class="shrink-0 hidden sm:inline-flex"
            />
            <span class="ml-auto text-xs text-muted tabular-nums shrink-0">
              {{ hostsOf(rule).length }} host{{ hostsOf(rule).length === 1 ? '' : 's' }}
            </span>
            <UIcon
              name="i-lucide-chevron-down"
              class="size-4 text-muted shrink-0 transition-transform group-data-[state=open]:rotate-180"
            />
          </button>

          <template #content>
            <div class="flex flex-col gap-2 px-3 pb-3 pt-2 border-t border-default">
              <!-- hosts -->
              <div class="flex flex-wrap gap-1">
                <UBadge
                  v-for="host in hostsOf(rule)"
                  :key="host"
                  :label="host"
                  color="neutral"
                  variant="subtle"
                  size="xs"
                  class="font-mono"
                />
                <span v-if="hostsOf(rule).length === 0 && rule.type !== 'raw'" class="text-xs text-muted">any host</span>
              </div>

              <!-- raw fallback: the daemon returned an unparsed rule -->
              <pre v-if="rule.type === 'raw'" class="font-mono text-xs text-muted whitespace-pre-wrap break-all">{{ rule.rule }}</pre>

              <!-- provenance / scope / rule name -->
              <div
                v-if="rule.applies_to || rule.provenance || (rule.rule && rule.type !== 'raw')"
                class="flex flex-wrap items-center gap-x-3 gap-y-0.5 text-xs text-muted"
              >
                <span v-if="rule.applies_to">applies to <span class="font-mono text-default">{{ rule.applies_to }}</span></span>
                <span v-if="rule.provenance">source <span class="font-mono text-default">{{ rule.provenance }}</span></span>
                <span v-if="rule.rule && rule.type !== 'raw'">rule <span class="font-mono text-default">{{ rule.rule }}</span></span>
              </div>

              <!-- remove (admin only; only this sandbox's own rules, with a resource to target) -->
              <div v-if="session.isAdmin.value && isOwnScope(rule) && hostsOf(rule).length > 0" class="pt-1">
                <UButton
                  label="Remove rule"
                  icon="i-lucide-trash-2"
                  color="error"
                  variant="subtle"
                  size="xs"
                  :loading="removingKey === ruleKey(rule)"
                  @click="doRemoveRule(rule)"
                />
              </div>
            </div>
          </template>
        </UCollapsible>
      </div>
      <p v-else class="text-sm text-muted">No policy rules configured.</p>

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
