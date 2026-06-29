<script setup lang="ts">
const props = defineProps<{ sandbox: { id: string } }>()

const api = useApi()
const session = useSession()
const toast = useToast()

const files = ref<File[] | null>(null)
const destDir = ref('/home/agent')
const uploading = ref(false)
const dlPath = ref('/home/agent/')

function filesUrl(p: string): string {
  return `/v1/sandboxes/${props.sandbox.id}/files?path=${encodeURIComponent(p)}`
}
// Per-file destination is <folder>/<original name>. The folder defaults to
// /home/agent; a relative folder is taken under /home/agent.
function destFor(name: string): string {
  let dir = destDir.value.trim() || '/home/agent'
  if (!dir.startsWith('/')) dir = `/home/agent/${dir}`
  return `${dir.replace(/\/+$/, '')}/${name}`
}
async function doUpload() {
  const picked = files.value ?? []
  if (!picked.length) return
  uploading.value = true
  let ok = 0
  const failed: string[] = []
  for (const f of picked) {
    try {
      await api.upload(filesUrl(destFor(f.name)), f)
      ok++
    } catch (e: any) {
      failed.push(`${f.name}: ${e?.message ?? 'failed'}`)
    }
  }
  uploading.value = false
  const dir = destDir.value.trim() || '/home/agent'
  if (ok) toast.add({ title: `Uploaded ${ok} file${ok > 1 ? 's' : ''} to ${dir}`, color: 'success', icon: 'i-lucide-check-circle' })
  if (failed.length) toast.add({ title: `${failed.length} upload${failed.length > 1 ? 's' : ''} failed`, description: failed.join('\n'), color: 'error', icon: 'i-lucide-alert-circle' })
  if (!failed.length) files.value = null
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
      <div class="flex flex-col gap-3">
        <p class="text-xs font-semibold text-muted uppercase tracking-wide">Upload</p>
        <UFileUpload
          v-model="files"
          multiple
          variant="area"
          layout="list"
          icon="i-lucide-upload"
          label="Drop files here or click to browse"
          description="Each file is uploaded into the destination folder"
          :file-image="false"
          :ui="{ base: 'min-h-28', fileLeadingAvatar: 'size-5' }"
        />
        <UInput
          v-model="destDir"
          size="sm"
          placeholder="/home/agent"
          aria-label="Destination folder"
          data-test="dest-dir"
        >
          <template #leading>
            <UIcon name="i-lucide-folder" class="size-4 text-dimmed" />
          </template>
        </UInput>
        <UButton
          :label="files && files.length > 1 ? `Upload ${files.length} files` : 'Upload'"
          icon="i-lucide-upload"
          size="sm"
          color="primary"
          block
          :loading="uploading"
          :disabled="!files || !files.length"
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
          block
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
