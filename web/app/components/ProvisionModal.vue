<script setup lang="ts">
import type { TableColumn } from '@nuxt/ui'
import { buildCreateBody, type ProvisionForm } from './ProvisionModal'

const props = defineProps<{
  open: boolean
}>()
const emit = defineEmits<{
  'update:open': [value: boolean]
}>()

const swarm = useSwarm()
const api = useApi()
const toast = useToast()

// ── Derived options ──────────────────────────────────────────────────────────

// DISTINCT templates across all nodes
const templateOptions = computed<string[]>(() => {
  const seen = new Set<string>()
  for (const node of swarm?.nodes.value ?? []) {
    for (const t of node.templates ?? []) {
      seen.add(typeof t === 'string' ? t : t.name ?? t.id ?? String(t))
    }
  }
  return Array.from(seen).sort()
})

// DISTINCT workspaces across all nodes
const workspaceOptions = computed<string[]>(() => {
  const seen = new Set<string>()
  for (const node of swarm?.nodes.value ?? []) {
    for (const w of node.workspaces ?? []) {
      seen.add(typeof w === 'string' ? w : w.name ?? String(w))
    }
  }
  return Array.from(seen).sort()
})

const strategyOptions = [
  { label: 'Least loaded', value: 'least-loaded' },
  { label: 'Bin-pack', value: 'bin-pack' },
  { label: 'Spread', value: 'spread' },
  { label: 'Least actual load', value: 'least-actual-load' },
]

// Supported provision agents. No daemon endpoint exposes this list (the sbx CLI
// hardcodes it), so it's maintained here — source: `sbx run --help` (sbx v0.33.0).
const agentOptions = [
  'claude', 'codex', 'copilot', 'cursor', 'docker-agent',
  'droid', 'gemini', 'kiro', 'opencode', 'shell',
]

// ── Form state ───────────────────────────────────────────────────────────────

const defaultForm = (): ProvisionForm => ({
  name: '',
  agent: '',
  template: '',
  cpus: 1,
  memory_bytes: 0,
  disk_gb: 0,
  workspaces: [],
  clone: false,
  branch: '',
  strategy: '',
  env: [],
  labels: [],
  node_affinity: [],
  node_anti_affinity: [],
})

const form = reactive<ProvisionForm>(defaultForm())

// Memory helper: collect as GiB displayed, store as bytes
const memoryGb = ref(0)
watch(memoryGb, (v) => { form.memory_bytes = Math.round(v * 1073741824) })

// Workspace multi-select: names selected + per-name read_only toggle
const selectedWorkspaceNames = ref<string[]>([])
const workspaceReadOnly = ref<Record<string, boolean>>({})

watch(selectedWorkspaceNames, (names) => {
  form.workspaces = names.map((name) => ({
    name,
    read_only: workspaceReadOnly.value[name] ?? false,
  }))
})

function toggleReadOnly(name: string) {
  workspaceReadOnly.value[name] = !(workspaceReadOnly.value[name] ?? false)
  // recompute
  form.workspaces = selectedWorkspaceNames.value.map((n) => ({
    name: n,
    read_only: workspaceReadOnly.value[n] ?? false,
  }))
}

// Key-value editor helpers. Rows carry a stable id so editing a key doesn't
// remount the input (focus loss); blank/duplicate keys are allowed while typing
// and resolved into a Record at submit (buildCreateBody). v-model binds the row
// fields directly — no per-keystroke object rebuild.
type KVKey = 'env' | 'labels' | 'node_affinity' | 'node_anti_affinity'
let nextKVId = 0

function addKV(key: KVKey) {
  form[key].push({ id: nextKVId++, k: '', v: '' })
}

function removeKV(key: KVKey, id: number) {
  form[key] = form[key].filter((row) => row.id !== id)
}

// Advanced section open state
const advancedOpen = ref(false)

// ── Submit ───────────────────────────────────────────────────────────────────

const submitting = ref(false)
const trackOp = useOpTracker()

async function onSubmit() {
  submitting.value = true
  try {
    const body = buildCreateBody(form)
    const agent = form.agent
    const op = await api.post('/v1/sandboxes', body, { 'Idempotency-Key': crypto.randomUUID() })
    emit('update:open', false)
    toast.add({
      title: 'Provisioning sandbox…',
      description: `Creating "${agent}" sandbox.`,
      color: 'info',
      icon: 'i-lucide-loader',
    })
    trackOp(op?.id, {
      onDone: (o) => {
        toast.add({
          title: 'Sandbox created',
          description: `"${agent}" sandbox ${o.sandbox_id ?? ''} is ready.`.trim(),
          color: 'success',
          icon: 'i-lucide-check-circle',
        })
        swarm?.refreshSandboxes()
      },
      onError: (o) => toast.add({
        title: 'Provision failed',
        description: o.error || 'The sandbox could not be created.',
        color: 'error',
        icon: 'i-lucide-alert-circle',
      }),
    })
    await swarm?.refreshSandboxes()
    // reset
    Object.assign(form, defaultForm())
    memoryGb.value = 0
    selectedWorkspaceNames.value = []
    workspaceReadOnly.value = {}
    advancedOpen.value = false
  } catch (err: any) {
    toast.add({
      title: 'Provision failed',
      description: err?.message ?? String(err),
      color: 'error',
      icon: 'i-lucide-alert-circle',
    })
  } finally {
    submitting.value = false
  }
}

function onClose() {
  emit('update:open', false)
}
</script>

<template>
  <UModal
    :open="open"
    title="Provision sandbox"
    description="Configure resources for the new sandbox."
    :ui="{ footer: 'justify-end gap-2' }"
    @update:open="emit('update:open', $event)"
  >
    <template #body>
      <div class="space-y-4">
        <!-- ── Core fields ──────────────────────────────────────────────── -->
        <div class="grid grid-cols-1 sm:grid-cols-2 gap-4">
          <!-- Name (optional) -->
          <div class="sm:col-span-2">
            <label class="block text-sm font-medium text-default mb-1" for="prov-name">
              Name <span class="text-muted font-normal">(optional)</span>
            </label>
            <UInput
              id="prov-name"
              v-model="form.name"
              placeholder="Auto-derived from agent + workspace"
              aria-label="Sandbox name"
              class="w-full"
            />
          </div>

          <!-- Agent -->
          <div class="sm:col-span-2">
            <label class="block text-sm font-medium text-default mb-1" for="prov-agent">
              Agent <span class="text-error" aria-hidden="true">*</span>
            </label>
            <USelect
              id="prov-agent"
              v-model="form.agent"
              :items="agentOptions"
              placeholder="Select an agent"
              aria-label="Agent name"
              class="w-full"
            />
          </div>

          <!-- Template -->
          <div class="sm:col-span-2">
            <label class="block text-sm font-medium text-default mb-1" for="prov-template">
              Template <span class="text-muted font-normal">(optional)</span>
            </label>
            <USelect
              id="prov-template"
              v-model="form.template"
              :items="templateOptions"
              placeholder="Default (agent's image)"
              aria-label="Sandbox template"
              class="w-full"
            />
          </div>

          <!-- CPUs -->
          <div>
            <label class="block text-sm font-medium text-default mb-1" for="prov-cpus">
              CPUs
            </label>
            <UInput
              id="prov-cpus"
              v-model.number="form.cpus"
              type="number"
              :min="1"
              placeholder="1"
              aria-label="CPU count"
              class="w-full"
            />
          </div>

          <!-- Memory -->
          <div>
            <label class="block text-sm font-medium text-default mb-1" for="prov-mem">
              Memory (GiB)
            </label>
            <UInput
              id="prov-mem"
              v-model.number="memoryGb"
              type="number"
              :min="0"
              :step="0.5"
              placeholder="0"
              aria-label="Memory in GiB"
              class="w-full"
            />
            <p v-if="form.memory_bytes > 0" class="text-xs text-muted mt-1 tabular-nums">
              {{ form.memory_bytes.toLocaleString() }} bytes
            </p>
          </div>

          <!-- Disk -->
          <div>
            <label class="block text-sm font-medium text-default mb-1" for="prov-disk">
              Disk (GB)
            </label>
            <UInput
              id="prov-disk"
              v-model.number="form.disk_gb"
              type="number"
              :min="0"
              placeholder="0"
              aria-label="Disk in GB"
              class="w-full"
            />
          </div>
        </div>

        <!-- Workspaces multi-select -->
        <div>
          <label class="block text-sm font-medium text-default mb-1">
            Workspaces
          </label>
          <USelectMenu
            v-model="selectedWorkspaceNames"
            :items="workspaceOptions"
            multiple
            placeholder="Select workspaces…"
            aria-label="Workspaces"
            class="w-full"
          />
          <!-- Per-workspace read_only toggle -->
          <div
            v-if="selectedWorkspaceNames.length > 0"
            class="mt-2 space-y-1"
          >
            <div
              v-for="name in selectedWorkspaceNames"
              :key="name"
              class="flex items-center justify-between gap-2 px-2 py-1 rounded bg-elevated text-sm"
            >
              <span class="font-mono text-default truncate">{{ name }}</span>
              <label class="flex items-center gap-1.5 cursor-pointer select-none text-muted">
                <UCheckbox
                  :model-value="workspaceReadOnly[name] ?? false"
                  :aria-label="`Read-only: ${name}`"
                  @update:model-value="toggleReadOnly(name)"
                />
                <span>Read-only</span>
              </label>
            </div>
          </div>
        </div>

        <!-- ── Advanced (collapsible) ────────────────────────────────────── -->
        <UCollapsible v-model:open="advancedOpen">
          <UButton
            variant="ghost"
            color="neutral"
            size="sm"
            :icon="advancedOpen ? 'i-lucide-chevron-up' : 'i-lucide-chevron-down'"
            trailing
            aria-label="Toggle advanced options"
            class="-ml-1"
          >
            Advanced
          </UButton>

          <template #content>
            <div class="mt-3 space-y-4 border-t border-default pt-4">
              <!-- Clone + branch -->
              <div class="flex items-center gap-3">
                <UCheckbox
                  v-model="form.clone"
                  label="Clone workspace on provision"
                  aria-label="Clone workspace"
                />
              </div>
              <div v-if="form.clone">
                <label class="block text-sm font-medium text-default mb-1" for="prov-branch">
                  Branch
                </label>
                <UInput
                  id="prov-branch"
                  v-model="form.branch"
                  placeholder="e.g. main"
                  aria-label="Branch name"
                  class="w-full"
                />
              </div>

              <!-- Strategy -->
              <div>
                <label class="block text-sm font-medium text-default mb-1" for="prov-strategy">
                  Placement strategy
                </label>
                <USelect
                  id="prov-strategy"
                  v-model="form.strategy"
                  :items="strategyOptions"
                  value-key="value"
                  placeholder="Default (least-loaded)"
                  aria-label="Placement strategy"
                  class="w-full"
                />
              </div>

              <!-- KV editors -->
              <template v-for="kvKey in (['env', 'labels', 'node_affinity', 'node_anti_affinity'] as const)" :key="kvKey">
                <div>
                  <div class="flex items-center justify-between mb-1">
                    <span class="text-sm font-medium text-default capitalize">
                      {{ kvKey.replace(/_/g, ' ') }}
                    </span>
                    <UButton
                      icon="i-lucide-plus"
                      size="xs"
                      color="neutral"
                      variant="ghost"
                      :aria-label="`Add ${kvKey} entry`"
                      @click="addKV(kvKey)"
                    />
                  </div>
                  <div
                    v-if="form[kvKey].length > 0"
                    class="space-y-1"
                  >
                    <div
                      v-for="row in form[kvKey]"
                      :key="row.id"
                      class="flex items-center gap-2"
                    >
                      <UInput
                        v-model="row.k"
                        placeholder="key"
                        class="w-1/3 font-mono text-xs"
                        :aria-label="`${kvKey} key`"
                      />
                      <UInput
                        v-model="row.v"
                        placeholder="value"
                        class="flex-1 font-mono text-xs"
                        :aria-label="`${kvKey} value`"
                      />
                      <UButton
                        icon="i-lucide-x"
                        size="xs"
                        color="neutral"
                        variant="ghost"
                        :aria-label="`Remove ${kvKey} entry`"
                        @click="removeKV(kvKey, row.id)"
                      />
                    </div>
                  </div>
                  <p v-else class="text-xs text-muted italic">No entries</p>
                </div>
              </template>
            </div>
          </template>
        </UCollapsible>
      </div>
    </template>

    <template #footer="{ close }">
      <UButton
        label="Cancel"
        color="neutral"
        variant="outline"
        :disabled="submitting"
        @click="close"
      />
      <UButton
        label="Provision"
        icon="i-lucide-zap"
        :loading="submitting"
        :disabled="!form.agent"
        @click="onSubmit"
      />
    </template>
  </UModal>
</template>
