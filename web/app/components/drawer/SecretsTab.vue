<script setup lang="ts">
const props = defineProps<{ id: string }>()

const api = useApi()
const session = useSession()
const toast = useToast()

interface CustomSecret { host: string; env: string; placeholder?: string }
interface StoredSecret { name: string; type: string; scope?: string } // type: "service" | "registry"; scope: "" = node-global, else owning sandbox id
interface SecretsResponse { custom: CustomSecret[]; stored: StoredSecret[] }

const secrets = ref<SecretsResponse>({ custom: [], stored: [] })
const loading = ref(false)

async function fetchSecrets() {
  loading.value = true
  try {
    secrets.value = await api.get(`/v1/sandboxes/${props.id}/secrets`)
  } catch (e: any) {
    toast.add({ title: 'Failed to load secrets', description: e?.message, color: 'error' })
  } finally {
    loading.value = false
  }
}

// Add form. The daemon rejects a host with a scheme/port and an env name that
// isn't UPPER_SNAKE — validate here so the user gets a clear message, not a 500.
const addHost = ref('')
const addEnv = ref('')
const addValue = ref('')
const addLoading = ref(false)

const hostError = computed(() => {
  const h = addHost.value.trim()
  if (h && /:\/\/|:|\s/.test(h)) return 'Bare host or IP only — no scheme (https://) or port (:443).'
  return ''
})
const envError = computed(() => {
  const e = addEnv.value.trim()
  if (e && !/^[A-Z_][A-Z0-9_]*$/.test(e)) return 'UPPER_SNAKE_CASE only (A–Z, 0–9, _; not starting with a digit).'
  return ''
})
const canAdd = computed(() =>
  !!addHost.value.trim() && !!addEnv.value.trim() && !!addValue.value && !hostError.value && !envError.value)

async function doAdd() {
  if (!canAdd.value) return
  addLoading.value = true
  try {
    await api.put(`/v1/sandboxes/${props.id}/secrets`, {
      scope: props.id,
      host: addHost.value.trim(),
      env: addEnv.value.trim(),
      value: addValue.value,
    })
    toast.add({ title: 'Secret added', color: 'success' })
    addHost.value = ''
    addEnv.value = ''
    addValue.value = ''
    await fetchSecrets()
  } catch (e: any) {
    toast.add({ title: 'Failed to add secret', description: e?.message, color: 'error' })
  } finally {
    addLoading.value = false
  }
}

// Delete
const deleteLoading = ref<string | null>(null)

async function doDelete(host: string) {
  if (!confirm(`Delete all secrets for host "${host}"?`)) return
  deleteLoading.value = host
  try {
    await api.del(`/v1/sandboxes/${props.id}/secrets/${host}`)
    toast.add({ title: 'Secret deleted', color: 'success' })
    await fetchSecrets()
  } catch (e: any) {
    toast.add({ title: 'Failed to delete secret', description: e?.message, color: 'error' })
  } finally {
    deleteLoading.value = null
  }
}

// Stored-secret delete uses the secret's OWN scope ("" -> _node), so deleting an
// inherited node-global entry targets node-global, not this sandbox.
const storedDeleteLoading = ref<string | null>(null)

async function doDeleteStored(s: StoredSecret) {
  if (!confirm(`Delete stored ${s.type || 'secret'} "${s.name}" (${s.scope || 'global'})?`)) return
  const key = `${s.scope}:${s.name}`
  storedDeleteLoading.value = key
  try {
    await api.del(`/v1/sandboxes/${s.scope || '_node'}/stored-secrets/${s.name}`)
    toast.add({ title: 'Stored secret deleted', color: 'success' })
    await fetchSecrets()
  } catch (e: any) {
    toast.add({ title: 'Failed to delete stored secret', description: e?.message, color: 'error' })
  } finally {
    storedDeleteLoading.value = null
  }
}

onMounted(fetchSecrets)
</script>

<template>
  <div class="flex flex-col gap-6 pt-4">

    <!-- Loading skeleton -->
    <div v-if="loading" class="flex flex-col gap-3">
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
            <div class="flex flex-col gap-0.5 min-w-0">
              <div class="flex items-center gap-2 min-w-0">
                <span class="font-mono text-default truncate">{{ s.host }}</span>
                <span class="text-muted">·</span>
                <span class="font-mono text-muted text-xs">{{ s.env }}</span>
              </div>
              <span
                v-if="s.placeholder"
                class="font-mono text-xs text-dimmed truncate"
                :title="s.placeholder"
              >placeholder {{ s.placeholder }}</span>
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
                :loading="deleteLoading === s.host"
                @click="doDelete(s.host)"
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
          <div
            v-for="s in secrets.stored"
            :key="`${s.scope}:${s.name}`"
            class="flex items-center gap-1.5 rounded-md bg-elevated px-2 py-1"
          >
            <UBadge
              :label="s.type || 'secret'"
              :color="s.type === 'registry' ? 'info' : 'neutral'"
              variant="subtle"
              size="xs"
              class="capitalize"
            />
            <span class="font-mono text-xs text-default">{{ s.name }}</span>
            <UButton
              v-if="session.isAdmin.value"
              icon="i-lucide-trash-2"
              size="xs"
              color="error"
              variant="ghost"
              aria-label="Delete stored secret"
              :loading="storedDeleteLoading === `${s.scope}:${s.name}`"
              @click="doDeleteStored(s)"
            />
          </div>
        </div>
      </div>

      <!-- Add form (admin only) -->
      <template v-if="session.isAdmin.value">
        <USeparator />
        <div class="flex flex-col gap-3">
          <p class="text-xs font-semibold text-muted uppercase tracking-wide">Add Secret</p>
          <p class="text-xs text-muted">
            Values are write-only and never displayed. Set env variables per host for this sandbox.
          </p>
          <div class="flex flex-col gap-2">
            <div class="flex flex-col gap-1">
              <UInput
                v-model="addHost"
                placeholder="host (e.g. api.example.com)"
                size="sm"
                aria-label="Secret host"
                data-test="secret-host"
                :color="hostError ? 'error' : undefined"
              />
              <p v-if="hostError" class="text-xs text-error">{{ hostError }}</p>
            </div>
            <div class="flex flex-col gap-1">
              <UInput
                v-model="addEnv"
                placeholder="env var name (e.g. API_KEY)"
                size="sm"
                aria-label="Environment variable name"
                data-test="secret-env"
                :color="envError ? 'error' : undefined"
              />
              <p v-if="envError" class="text-xs text-error">{{ envError }}</p>
            </div>
            <UInput
              v-model="addValue"
              type="password"
              placeholder="secret value"
              size="sm"
              aria-label="Secret value"
              data-test="secret-value"
            />
            <UButton
              label="Add Secret"
              icon="i-lucide-plus"
              size="sm"
              :loading="addLoading"
              :disabled="!canAdd"
              data-test="secret-add"
              @click="doAdd"
            />
          </div>
        </div>
      </template>
    </template>

  </div>
</template>
