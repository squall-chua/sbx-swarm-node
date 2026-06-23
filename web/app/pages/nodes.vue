<script setup lang="ts">
interface NodeSummary {
  node_id: string
  node_name: string
  cordoned: boolean
  draining: boolean
  limit_cpu: number
  alloc_cpu: number
  actual_cpu: number
  limit_mem_kb: number
  alloc_mem_kb: number
  actual_mem: number
  templates: string[]
  workspaces: string[]
  labels: Record<string, string>
  capabilities: string[]
}

const swarm = useSwarm()
const session = useSession()
const api = useApi()
const toast = useToast()

// ── Revoked list ─────────────────────────────────────────────────────────────
const revokedIds = ref<string[]>([])

async function loadRevoked() {
  try {
    const res = await api.get('/v1/node/revoked')
    revokedIds.value = res?.node_ids ?? []
  } catch {
    revokedIds.value = []
  }
}

onMounted(() => { loadRevoked() })

// ── Action loading state ─────────────────────────────────────────────────────
const loading = ref<Record<string, boolean>>({})

function setLoading(key: string, val: boolean) {
  loading.value = { ...loading.value, [key]: val }
}

// ── Revoke confirm modal ──────────────────────────────────────────────────────
const revokeTarget = ref<string | null>(null)
const revokeOpen = ref(false)

function promptRevoke(nodeId: string) {
  revokeTarget.value = nodeId
  revokeOpen.value = true
}

// ── Node actions ─────────────────────────────────────────────────────────────
async function nodeAction(action: string, nodeId: string) {
  const key = `${action}-${nodeId}`
  setLoading(key, true)
  try {
    await api.post(`/v1/node/${action}`, { node_id: nodeId })
    await swarm?.refreshNodes()
    if (action === 'revoke') await loadRevoked()
    toast.add({ title: `${action} succeeded`, color: 'success' })
  } catch (e: any) {
    toast.add({ title: `${action} failed`, description: e?.message, color: 'error' })
  } finally {
    setLoading(key, false)
  }
}

async function confirmRevoke() {
  if (!revokeTarget.value) return
  revokeOpen.value = false
  await nodeAction('revoke', revokeTarget.value)
  revokeTarget.value = null
}

// ── Resource helpers ──────────────────────────────────────────────────────────
// actual_cpu / actual_mem are 0..1+ fractions (already normalised vs limit)
function cpuActualPct(n: NodeSummary): number {
  return Math.round(n.actual_cpu * 100)
}
function cpuAllocPct(n: NodeSummary): number {
  return n.limit_cpu > 0 ? Math.round((n.alloc_cpu / n.limit_cpu) * 100) : 0
}
function memActualPct(n: NodeSummary): number {
  return Math.round(n.actual_mem * 100)
}
function memAllocPct(n: NodeSummary): number {
  return n.limit_mem_kb > 0 ? Math.round((n.alloc_mem_kb / n.limit_mem_kb) * 100) : 0
}
function fmtMem(kb: number): string {
  if (kb >= 1_048_576) return `${(kb / 1_048_576).toFixed(1)} GB`
  if (kb >= 1024) return `${(kb / 1024).toFixed(0)} MB`
  return `${kb} KB`
}
function cpuBarColor(n: NodeSummary): string {
  const p = cpuActualPct(n)
  if (p >= 90) return 'error'
  if (p >= 70) return 'warning'
  return 'primary'
}
function memActualBarColor(n: NodeSummary): string {
  const p = memActualPct(n)
  if (p >= 90) return 'error'
  if (p >= 70) return 'warning'
  return 'primary'
}
function memAllocBarColor(n: NodeSummary): string {
  const p = memAllocPct(n)
  if (p >= 90) return 'error'
  if (p >= 70) return 'warning'
  return 'primary'
}

const nodes = computed(() => swarm?.nodes.value ?? [])
</script>

<template>
  <!-- Renders directly into layout slot — no extra panel wrapper -->
  <div class="flex flex-col gap-4 p-4 md:p-6">

    <!-- ── Page header ──────────────────────────────────────────────────────── -->
    <div class="flex flex-wrap items-center justify-between gap-3">
      <h1 class="text-lg font-semibold text-highlighted">Nodes</h1>
      <UButton
        icon="i-lucide-refresh-cw"
        color="neutral"
        variant="outline"
        size="sm"
        aria-label="Refresh nodes"
        @click="swarm?.refreshNodes(); loadRevoked()"
      />
    </div>

    <!-- ── Node cards ───────────────────────────────────────────────────────── -->
    <div
      v-if="nodes.length"
      class="grid grid-cols-1 sm:grid-cols-2 xl:grid-cols-3 gap-4"
    >
      <UCard
        v-for="node in nodes"
        :key="node.node_id"
        variant="outline"
        class="flex flex-col gap-3"
      >
        <template #header>
          <div class="flex items-start justify-between gap-2 flex-wrap">
            <!-- Name + badges -->
            <div class="flex flex-col gap-1 min-w-0">
              <span class="font-mono text-sm font-semibold text-highlighted truncate">
                {{ node.node_name }}
              </span>
              <span class="font-mono text-xs text-muted">{{ node.node_id }}</span>
            </div>
            <div class="flex items-center gap-1.5 shrink-0 flex-wrap">
              <UBadge
                v-if="node.cordoned"
                label="Cordoned"
                color="warning"
                variant="subtle"
                size="xs"
              />
              <UBadge
                v-if="node.draining"
                label="Draining"
                color="warning"
                variant="subtle"
                size="xs"
              />
            </div>
          </div>
        </template>

        <div class="flex flex-col gap-4">
          <!-- CPU -->
          <div class="flex flex-col gap-1">
            <div class="flex items-center justify-between text-xs text-muted">
              <span>CPU</span>
              <span class="tabular-nums">
                actual <strong class="text-default">{{ cpuActualPct(node) }}%</strong>
                · alloc <strong class="text-default">{{ node.alloc_cpu }}/{{ node.limit_cpu }}</strong> cores
              </span>
            </div>
            <UProgress
              :model-value="Math.min(100, cpuActualPct(node))"
              :color="cpuBarColor(node)"
              size="xs"
              aria-label="CPU actual utilisation"
            />
            <UProgress
              :model-value="cpuAllocPct(node)"
              color="neutral"
              size="2xs"
              aria-label="CPU allocated"
            />
          </div>

          <!-- Memory -->
          <div class="flex flex-col gap-1">
            <div class="flex items-center justify-between text-xs text-muted">
              <span>Memory</span>
              <span class="tabular-nums">
                actual <strong class="text-default">{{ memActualPct(node) }}%</strong>
                · alloc <strong class="text-default">{{ fmtMem(node.alloc_mem_kb) }}/{{ fmtMem(node.limit_mem_kb) }}</strong>
              </span>
            </div>
            <!-- Actual memory (solid bar) -->
            <UProgress
              :model-value="Math.min(100, memActualPct(node))"
              :color="memActualBarColor(node)"
              size="xs"
              aria-label="Memory actual utilisation"
            />
            <!-- Alloc memory (dimmer bar below) -->
            <UProgress
              :model-value="memAllocPct(node)"
              :color="memAllocBarColor(node)"
              size="2xs"
              aria-label="Memory allocated"
            />
          </div>

          <!-- Labels -->
          <div v-if="Object.keys(node.labels ?? {}).length" class="flex flex-col gap-1">
            <span class="text-xs text-muted uppercase tracking-wide font-medium">Labels</span>
            <div class="flex flex-wrap gap-1">
              <UBadge
                v-for="(v, k) in node.labels"
                :key="k"
                :label="`${k}=${v}`"
                color="neutral"
                variant="subtle"
                size="xs"
                class="font-mono"
              />
            </div>
          </div>

          <!-- Capabilities -->
          <div v-if="(node.capabilities ?? []).length" class="flex flex-col gap-1">
            <span class="text-xs text-muted uppercase tracking-wide font-medium">Capabilities</span>
            <div class="flex flex-wrap gap-1">
              <UBadge
                v-for="cap in node.capabilities"
                :key="cap"
                :label="cap"
                color="neutral"
                variant="subtle"
                size="xs"
                class="font-mono"
              />
            </div>
          </div>

          <!-- Workspaces -->
          <div v-if="(node.workspaces ?? []).length" class="flex flex-col gap-1">
            <span class="text-xs text-muted uppercase tracking-wide font-medium">Workspaces</span>
            <p class="text-xs text-muted tabular-nums">{{ (node.workspaces ?? []).length }}</p>
          </div>

          <!-- Templates -->
          <div v-if="(node.templates ?? []).length" class="flex flex-col gap-1">
            <span class="text-xs text-muted uppercase tracking-wide font-medium">Templates</span>
            <div class="flex flex-wrap gap-1">
              <UBadge
                v-for="t in node.templates"
                :key="t"
                :label="t"
                color="neutral"
                variant="subtle"
                size="xs"
                class="font-mono"
              />
            </div>
          </div>

          <!-- Admin actions — hidden for read-only callers -->
          <div
            v-if="session.isAdmin.value"
            class="flex flex-wrap items-center gap-2 pt-1 border-t border-default/30"
          >
            <!-- Cordon -->
            <UButton
              v-if="!node.cordoned"
              :data-test="`cordon-${node.node_id}`"
              label="Cordon"
              icon="i-lucide-shield-off"
              color="neutral"
              variant="outline"
              size="xs"
              :loading="loading[`cordon-${node.node_id}`]"
              :aria-label="`Cordon node ${node.node_name}`"
              @click="nodeAction('cordon', node.node_id)"
            />
            <!-- Uncordon -->
            <UButton
              v-if="node.cordoned"
              :data-test="`uncordon-${node.node_id}`"
              label="Uncordon"
              icon="i-lucide-shield-check"
              color="neutral"
              variant="outline"
              size="xs"
              :loading="loading[`uncordon-${node.node_id}`]"
              :aria-label="`Uncordon node ${node.node_name}`"
              @click="nodeAction('uncordon', node.node_id)"
            />
            <!-- Drain -->
            <UButton
              :data-test="`drain-${node.node_id}`"
              label="Drain"
              icon="i-lucide-arrow-down-to-line"
              color="warning"
              variant="subtle"
              size="xs"
              :loading="loading[`drain-${node.node_id}`]"
              :disabled="node.draining"
              :aria-label="`Drain node ${node.node_name}`"
              @click="nodeAction('drain', node.node_id)"
            />
            <!-- Revoke — destructive, confirm required -->
            <UButton
              :data-test="`revoke-${node.node_id}`"
              label="Revoke"
              icon="i-lucide-ban"
              color="error"
              variant="subtle"
              size="xs"
              :loading="loading[`revoke-${node.node_id}`]"
              :aria-label="`Revoke node ${node.node_name}`"
              @click="promptRevoke(node.node_id)"
            />
          </div>
        </div>
      </UCard>
    </div>

    <!-- Empty state -->
    <UAlert
      v-else
      icon="i-lucide-server-off"
      title="No nodes"
      description="No nodes are currently visible in the swarm."
      color="neutral"
      variant="subtle"
    />

    <!-- ── Revoked list ───────────────────────────────────────────────────── -->
    <div v-if="revokedIds.length" class="flex flex-col gap-2">
      <h2 class="text-sm font-medium text-muted uppercase tracking-wide">Revoked nodes</h2>
      <div class="flex flex-wrap gap-2">
        <UBadge
          v-for="id in revokedIds"
          :key="id"
          :label="id"
          color="error"
          variant="subtle"
          size="sm"
          class="font-mono"
        />
      </div>
    </div>
  </div>

  <!-- ── Revoke confirm modal ────────────────────────────────────────────── -->
  <UModal v-model:open="revokeOpen" title="Confirm revoke">
    <template #body>
      <p class="text-sm text-default">
        Are you sure you want to revoke node
        <span class="font-mono text-error">{{ revokeTarget }}</span>?
        This action cannot be undone.
      </p>
    </template>
    <template #footer>
      <div class="flex justify-end gap-2">
        <UButton
          label="Cancel"
          color="neutral"
          variant="outline"
          size="sm"
          @click="revokeOpen = false"
        />
        <UButton
          label="Revoke"
          color="error"
          size="sm"
          @click="confirmRevoke"
        />
      </div>
    </template>
  </UModal>
</template>
