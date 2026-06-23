<script setup lang="ts">
// Network / Security page — node-global scope ("").
// Policy and secrets use scope="" which renders as /v1/sandboxes//policy
// (empty path segment) — that is the node-global scope the gateway expects.

const api = useApi()
const session = useSession()
const toast = useToast()

// ── Policy ───────────────────────────────────────────────────────────────────
interface PolicyRule { host: string; decision: 'allow' | 'deny' }

const policy = ref<PolicyRule[]>([])
const policyLoading = ref(false)

async function fetchPolicy() {
  policyLoading.value = true
  try {
    const res = await api.get('/v1/sandboxes//policy')
    policy.value = res?.rules ?? []
  } catch (e: any) {
    toast.add({ title: 'Failed to load policy', description: e?.message, color: 'error' })
  } finally {
    policyLoading.value = false
  }
}

const addHost = ref('')
const addDecision = ref<'allow' | 'deny'>('allow')
const addLoading = ref(false)

const decisionOptions = [
  { label: 'Allow', value: 'allow' },
  { label: 'Deny', value: 'deny' },
]

async function doAddRule() {
  if (!addHost.value) return
  addLoading.value = true
  try {
    await api.put('/v1/sandboxes//policy', {
      scope: '',
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

// ── Secrets ───────────────────────────────────────────────────────────────────
interface CustomSecret { host: string; env: string }
interface StoredSecret { name: string }
interface SecretsResponse { custom: CustomSecret[]; stored: StoredSecret[] }

const secrets = ref<SecretsResponse>({ custom: [], stored: [] })
const secretsLoading = ref(false)

async function fetchSecrets() {
  secretsLoading.value = true
  try {
    const res = await api.get('/v1/sandboxes//secrets')
    secrets.value = res ?? { custom: [], stored: [] }
  } catch (e: any) {
    toast.add({ title: 'Failed to load secrets', description: e?.message, color: 'error' })
  } finally {
    secretsLoading.value = false
  }
}

const secretHost = ref('')
const secretEnv = ref('')
const secretValue = ref('')
const secretAddLoading = ref(false)

async function doAddSecret() {
  if (!secretHost.value || !secretEnv.value || !secretValue.value) return
  secretAddLoading.value = true
  try {
    await api.put('/v1/sandboxes//secrets', {
      scope: '',
      host: secretHost.value,
      env: secretEnv.value,
      value: secretValue.value,
    })
    toast.add({ title: 'Secret added', color: 'success' })
    secretHost.value = ''
    secretEnv.value = ''
    secretValue.value = ''
    await fetchSecrets()
  } catch (e: any) {
    toast.add({ title: 'Failed to add secret', description: e?.message, color: 'error' })
  } finally {
    secretAddLoading.value = false
  }
}

const secretDeleteLoading = ref<string | null>(null)

async function doDeleteSecret(host: string) {
  if (!confirm(`Delete all secrets for host "${host}"?`)) return
  secretDeleteLoading.value = host
  try {
    await api.del(`/v1/sandboxes//secrets/${host}`)
    toast.add({ title: 'Secret deleted', color: 'success' })
    await fetchSecrets()
  } catch (e: any) {
    toast.add({ title: 'Failed to delete secret', description: e?.message, color: 'error' })
  } finally {
    secretDeleteLoading.value = null
  }
}

onMounted(() => {
  fetchPolicy()
  fetchSecrets()
})
</script>

<template>
  <div class="flex flex-col gap-6 p-4 md:p-6">

    <!-- Page header -->
    <div class="flex flex-wrap items-center justify-between gap-3">
      <h1 class="text-lg font-semibold text-highlighted">Network / Security</h1>
      <UButton
        icon="i-lucide-refresh-cw"
        color="neutral"
        variant="outline"
        size="sm"
        aria-label="Refresh network/security"
        @click="fetchPolicy(); fetchSecrets()"
      />
    </div>

    <!-- ── Egress Policy ─────────────────────────────────────────────────────── -->
    <UCard variant="outline">
      <template #header>
        <div class="flex items-center justify-between gap-2">
          <p class="text-sm font-semibold text-highlighted">Egress Policy (node-global)</p>
          <UBadge
            :label="`${policy.length} rule${policy.length === 1 ? '' : 's'}`"
            color="neutral"
            variant="subtle"
            size="xs"
          />
        </div>
      </template>

      <div class="flex flex-col gap-4">
        <!-- Loading -->
        <div v-if="policyLoading" class="flex flex-col gap-2">
          <USkeleton class="h-4 w-full" />
          <USkeleton class="h-4 w-3/4" />
        </div>

        <!-- Rules list -->
        <div v-else-if="policy.length > 0" class="flex flex-col gap-2">
          <div
            v-for="rule in policy"
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
          <USeparator />
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
    </UCard>

    <!-- ── Secrets ────────────────────────────────────────────────────────────── -->
    <UCard variant="outline">
      <template #header>
        <p class="text-sm font-semibold text-highlighted">Secrets (node-global)</p>
      </template>

      <div class="flex flex-col gap-4">
        <!-- Loading -->
        <div v-if="secretsLoading" class="flex flex-col gap-2">
          <USkeleton class="h-4 w-3/4" />
          <USkeleton class="h-4 w-1/2" />
        </div>

        <template v-else>
          <!-- Custom secrets -->
          <div class="flex flex-col gap-3">
            <p class="text-xs font-semibold text-muted uppercase tracking-wide">
              Custom (host + env)
              <span class="font-mono text-xs font-normal ml-1">({{ secrets.custom.length }})</span>
            </p>

            <div v-if="secrets.custom.length > 0" class="flex flex-col gap-2">
              <div
                v-for="s in secrets.custom"
                :key="`${s.host}:${s.env}`"
                class="flex items-center justify-between gap-3 rounded-md bg-elevated px-3 py-2 text-sm"
              >
                <div class="flex items-center gap-2 min-w-0">
                  <span class="font-mono text-default truncate">{{ s.host }}</span>
                  <span class="text-muted">·</span>
                  <span class="font-mono text-muted text-xs">{{ s.env }}</span>
                </div>
                <div class="flex items-center gap-2 shrink-0">
                  <UBadge label="write-only" color="neutral" variant="subtle" size="xs" />
                  <UButton
                    v-if="session.isAdmin.value"
                    icon="i-lucide-trash-2"
                    size="xs"
                    color="error"
                    variant="ghost"
                    aria-label="Delete secret"
                    :loading="secretDeleteLoading === s.host"
                    @click="doDeleteSecret(s.host)"
                  />
                </div>
              </div>
            </div>
            <p v-else class="text-sm text-muted">No custom secrets configured.</p>
          </div>

          <!-- Stored secrets -->
          <div v-if="secrets.stored.length > 0" class="flex flex-col gap-3">
            <p class="text-xs font-semibold text-muted uppercase tracking-wide">
              Stored
              <span class="font-mono text-xs font-normal ml-1">({{ secrets.stored.length }})</span>
            </p>
            <div class="flex flex-wrap gap-2">
              <UBadge
                v-for="s in secrets.stored"
                :key="s.name"
                :label="s.name"
                color="neutral"
                variant="subtle"
                size="sm"
                class="font-mono"
              />
            </div>
          </div>

          <!-- Add secret form (admin only) -->
          <template v-if="session.isAdmin.value">
            <USeparator />
            <div class="flex flex-col gap-3">
              <p class="text-xs font-semibold text-muted uppercase tracking-wide">Add Secret</p>
              <p class="text-xs text-muted">
                Values are write-only and never displayed. Set env variables per host at node-global scope.
              </p>
              <div class="flex flex-col gap-2">
                <UInput
                  v-model="secretHost"
                  placeholder="host (e.g. api.example.com)"
                  size="sm"
                  aria-label="Secret host"
                />
                <UInput
                  v-model="secretEnv"
                  placeholder="env var name (e.g. API_KEY)"
                  size="sm"
                  aria-label="Environment variable name"
                />
                <UInput
                  v-model="secretValue"
                  type="password"
                  placeholder="secret value"
                  size="sm"
                  aria-label="Secret value"
                />
                <UButton
                  label="Add Secret"
                  icon="i-lucide-plus"
                  size="sm"
                  :loading="secretAddLoading"
                  :disabled="!secretHost || !secretEnv || !secretValue"
                  @click="doAddSecret"
                />
              </div>
            </div>
          </template>
        </template>
      </div>
    </UCard>

  </div>
</template>
