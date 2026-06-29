<script setup lang="ts">
const props = defineProps<{ sandbox: { id: string } }>()

const api = useApi()
const session = useSession()
const toast = useToast()

const file = ref<File | null>(null)
const dest = ref('')
const uploading = ref(false)
const dlPath = ref('/home/agent/')

// Default to /home/agent/<filename> unless the operator typed an absolute path.
function resolvedDest(): string {
  const d = dest.value.trim()
  const name = file.value?.name ?? ''
  if (d) return d.startsWith('/') ? d : `/home/agent/${d}`
  return `/home/agent/${name}`
}
function filesUrl(p: string): string {
  return `/v1/sandboxes/${props.sandbox.id}/files?path=${encodeURIComponent(p)}`
}
function onPick(e: Event) {
  file.value = (e.target as HTMLInputElement).files?.[0] ?? null
}
async function doUpload() {
  if (!file.value) return
  uploading.value = true
  try {
    await api.upload(filesUrl(resolvedDest()), file.value)
    toast.add({ title: `Uploaded to ${resolvedDest()}`, color: 'success', icon: 'i-lucide-check-circle' })
  } catch (e: any) {
    toast.add({ title: 'Upload failed', description: e?.message, color: 'error', icon: 'i-lucide-alert-circle' })
  } finally {
    uploading.value = false
  }
}
function doDownload() {
  const p = dlPath.value.trim()
  if (!p.startsWith('/')) {
    toast.add({ title: 'Path must be absolute', color: 'error' })
    return
  }
  const a = document.createElement('a')
  a.href = api.downloadUrl(filesUrl(p))
  a.download = p.split('/').pop() || 'download'
  document.body.appendChild(a)
  a.click()
  a.remove()
}
</script>

<template>
  <div class="flex flex-col gap-6 pt-4">
    <template v-if="session.isAdmin.value">
      <!-- Upload -->
      <div class="flex flex-col gap-2">
        <p class="text-xs font-semibold text-muted uppercase tracking-wide">Upload</p>
        <input type="file" aria-label="File to upload" @change="onPick">
        <UInput v-model="dest" size="sm" placeholder="destination (default: /home/agent/<filename>)" aria-label="Destination path" />
        <UButton
          label="Upload"
          icon="i-lucide-upload"
          size="sm"
          color="primary"
          :loading="uploading"
          :disabled="!file"
          data-test="upload"
          @click="doUpload"
        />
      </div>

      <USeparator />

      <!-- Download -->
      <div class="flex flex-col gap-2">
        <p class="text-xs font-semibold text-muted uppercase tracking-wide">Download</p>
        <UInput v-model="dlPath" size="sm" placeholder="/absolute/container/path" aria-label="Download path" data-test="dl-path" />
        <UButton
          label="Download"
          icon="i-lucide-download"
          size="sm"
          color="primary"
          :disabled="!dlPath.startsWith('/') || dlPath.endsWith('/')"
          data-test="download"
          @click="doDownload"
        />
      </div>
    </template>

    <UAlert
      v-else
      color="neutral"
      variant="subtle"
      icon="i-lucide-lock"
      title="Admin only"
      description="File transfer requires admin access."
    />
  </div>
</template>
