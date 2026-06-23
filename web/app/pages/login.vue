<script setup lang="ts">
definePageMeta({ layout: false })

const session = useSession()
const toast = useToast()

const apiKey = ref('')
const loading = ref(false)

async function onSubmit() {
  if (!apiKey.value.trim()) return
  loading.value = true
  try {
    await session.login(apiKey.value)
    await navigateTo('/')
  } catch {
    toast.add({
      title: 'Authentication failed',
      description: 'invalid key',
      color: 'error',
    })
  } finally {
    loading.value = false
  }
}
</script>

<template>
  <div class="flex min-h-dvh items-center justify-center bg-elevated px-4">
    <UCard class="w-full max-w-sm">
      <template #header>
        <div class="flex flex-col items-center gap-2 py-2">
          <div class="flex items-center gap-2">
            <UIcon name="i-lucide-boxes" class="text-primary size-7" aria-hidden="true" />
            <span class="text-lg font-semibold text-highlighted tracking-tight">sbx-swarm</span>
          </div>
          <p class="text-sm text-muted">Sign in with your API key to continue</p>
        </div>
      </template>

      <form class="space-y-4" @submit.prevent="onSubmit">
        <div class="flex flex-col gap-1.5">
          <label for="api-key" class="text-sm font-medium text-default">
            API Key
          </label>
          <UInput
            id="api-key"
            v-model="apiKey"
            type="password"
            placeholder="Enter your API key"
            autocomplete="current-password"
            :disabled="loading"
            :ui="{ base: 'font-mono' }"
            aria-label="API Key"
            aria-required="true"
          />
        </div>

        <UButton
          type="submit"
          label="Sign in"
          block
          :loading="loading"
          :disabled="loading || !apiKey.trim()"
          aria-label="Sign in to sbx-swarm"
        />
      </form>
    </UCard>
  </div>
</template>
