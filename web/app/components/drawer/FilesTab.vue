<script setup lang="ts">
const props = defineProps<{ id: string }>()

const api = useApi()
const session = useSession()
const toast = useToast()

const files = ref<File[] | null>(null)
const destDir = ref('/home/agent')
const dlPath = ref('/home/agent/')

// Upload progress. upPct hits 100 when the body is sent; the server then transfers
// to the sandbox (no client-visible progress), shown as a "Finalizing" indeterminate bar.
const uploading = ref(false)
const upName = ref('')
const upPos = ref('')
const upPct = ref(0)

// Download progress. Null = indeterminate: the server stages the whole file before
// streaming, so there are no bytes (and no %) until it starts sending.
const downloading = ref(false)
const dlName = ref('')
const dlPct = ref<number | null>(0)
const notFoundOpen = ref(false)
const notFoundMsg = ref('')

function filesUrl(p: string): string {
  return `/v1/sandboxes/${props.id}/files?path=${encodeURIComponent(p)}`
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
  for (let i = 0; i < picked.length; i++) {
    const f = picked[i]!
    upName.value = f.name
    upPos.value = `${i + 1}/${picked.length}`
    upPct.value = 0
    try {
      await api.upload(filesUrl(destFor(f.name)), f, (loaded, total) => {
        upPct.value = total ? Math.round((loaded / total) * 100) : 0
      })
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
async function doDownload() {
  const p = dlPath.value.trim()
  if (!p.startsWith('/')) {
    toast.add({ title: 'Path must be absolute', color: 'error' })
    return
  }
  downloading.value = true
  dlName.value = p.split('/').pop() || 'download'
  dlPct.value = null
  try {
    const blob = await api.download(filesUrl(p), (loaded, total) => {
      dlPct.value = total ? Math.round((loaded / total) * 100) : null
    })
    const url = URL.createObjectURL(blob)
    const a = document.createElement('a')
    a.href = url
    a.download = dlName.value
    document.body.appendChild(a)
    a.click()
    a.remove()
    URL.revokeObjectURL(url)
    toast.add({ title: `Downloaded ${dlName.value}`, color: 'success', icon: 'i-lucide-check-circle' })
  } catch (e: any) {
    if (e?.status === 404) {
      notFoundMsg.value = `"${dlName.value}" was not found at ${p}. Check the path and try again.`
      notFoundOpen.value = true
    } else {
      toast.add({ title: 'Download failed', description: e?.message, color: 'error', icon: 'i-lucide-alert-circle' })
    }
  } finally {
    downloading.value = false
  }
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

        <div v-if="uploading" class="flex flex-col gap-1" data-test="up-progress">
          <div class="flex justify-between gap-2 text-xs text-muted">
            <span class="truncate">{{ upPct >= 100 ? 'Finalizing' : 'Uploading' }} {{ upName }}</span>
            <span class="shrink-0">{{ upPos }}</span>
          </div>
          <UProgress :model-value="upPct >= 100 ? null : upPct" size="sm" />
        </div>

        <UButton
          :label="files && files.length > 1 ? `Upload ${files.length} files` : 'Upload'"
          icon="i-lucide-upload"
          size="sm"
          color="primary"
          block
          :loading="uploading"
          :disabled="!files || !files.length || uploading"
          data-test="upload"
          @click="doUpload"
        />
      </div>

      <USeparator />

      <!-- Download -->
      <div class="flex flex-col gap-2">
        <p class="text-xs font-semibold text-muted uppercase tracking-wide">Download</p>
        <UInput v-model="dlPath" size="sm" placeholder="/absolute/container/path" aria-label="Download path" data-test="dl-path" />

        <div v-if="downloading" class="flex flex-col gap-1" data-test="dl-progress">
          <div class="flex justify-between gap-2 text-xs text-muted">
            <span class="truncate">{{ dlPct === null ? 'Preparing' : 'Downloading' }} {{ dlName }}</span>
            <span v-if="dlPct !== null" class="shrink-0">{{ dlPct }}%</span>
          </div>
          <UProgress :model-value="dlPct" size="sm" />
        </div>

        <UButton
          label="Download"
          icon="i-lucide-download"
          size="sm"
          color="primary"
          block
          :loading="downloading"
          :disabled="!dlPath.startsWith('/') || dlPath.endsWith('/') || downloading"
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

    <UModal
      v-model:open="notFoundOpen"
      title="File not found"
      :ui="{ footer: 'justify-end' }"
    >
      <template #body>
        <div class="flex items-start gap-3">
          <UIcon name="i-lucide-file-x" class="size-6 shrink-0 text-error" />
          <p class="text-sm text-default">{{ notFoundMsg }}</p>
        </div>
      </template>
      <template #footer="{ close }">
        <UButton label="Close" color="neutral" variant="outline" @click="close" />
      </template>
    </UModal>
  </div>
</template>
