// @vitest-environment nuxt
import { ref } from 'vue'
import { describe, it, expect, vi } from 'vitest'
import { flushPromises } from '@vue/test-utils'
import { mountSuspended } from '@nuxt/test-utils/runtime'
import FilesTab from '../app/components/drawer/FilesTab.vue'

const upload = vi.fn(async () => {})
const downloadUrl = vi.fn((p: string) => 'https://node' + p)
vi.mock('../app/composables/useApi', () => ({
  useApi: () => ({ upload, downloadUrl, get: vi.fn(), post: vi.fn() }),
}))
vi.mock('../app/composables/useSession', () => ({ useSession: () => ({ isAdmin: ref(true) }) }))

async function pick(w: any, files: File[]) {
  const input = w.find('input[type="file"]').element as HTMLInputElement
  Object.defineProperty(input, 'files', { value: files, configurable: true })
  await w.find('input[type="file"]').trigger('change')
  await flushPromises()
}

describe('FilesTab', () => {
  it('uploads each chosen file into the default /home/agent folder', async () => {
    upload.mockClear()
    const w = await mountSuspended(FilesTab, { props: { id: 'n1.s1' } })
    const a = new File(['a'], 'a.txt', { type: 'text/plain' })
    const b = new File(['b'], 'b.txt', { type: 'text/plain' })
    await pick(w, [a, b])
    await w.find('[data-test="upload"]').trigger('click')
    await flushPromises()
    expect(upload).toHaveBeenCalledTimes(2)
    expect(upload).toHaveBeenCalledWith('/v1/sandboxes/n1.s1/files?path=%2Fhome%2Fagent%2Fa.txt', a)
    expect(upload).toHaveBeenCalledWith('/v1/sandboxes/n1.s1/files?path=%2Fhome%2Fagent%2Fb.txt', b)
  })

  it('uploads into the typed destination folder, joining each filename', async () => {
    upload.mockClear()
    const w = await mountSuspended(FilesTab, { props: { id: 'n1.s1' } })
    await w.find('[data-test="dest-dir"]').setValue('/srv/data/')
    const f = new File(['x'], 'x.txt', { type: 'text/plain' })
    await pick(w, [f])
    await w.find('[data-test="upload"]').trigger('click')
    await flushPromises()
    expect(upload).toHaveBeenCalledWith('/v1/sandboxes/n1.s1/files?path=%2Fsrv%2Fdata%2Fx.txt', f)
  })

  it('download builds the file URL from the path field', async () => {
    const w = await mountSuspended(FilesTab, { props: { id: 'n1.s1' } })
    await w.find('[data-test="dl-path"]').setValue('/home/agent/out.txt')
    await w.find('[data-test="download"]').trigger('click')
    expect(downloadUrl).toHaveBeenCalledWith('/v1/sandboxes/n1.s1/files?path=%2Fhome%2Fagent%2Fout.txt')
  })
})
