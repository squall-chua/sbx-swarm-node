// @vitest-environment nuxt
import { ref } from 'vue'
import { describe, it, expect, vi } from 'vitest'
import { mountSuspended } from '@nuxt/test-utils/runtime'
import FilesTab from '../app/components/drawer/FilesTab.vue'

const upload = vi.fn(async () => {})
const downloadUrl = vi.fn((p: string) => 'https://node' + p)
vi.mock('../app/composables/useApi', () => ({
  useApi: () => ({ upload, downloadUrl, get: vi.fn(), post: vi.fn() }),
}))
vi.mock('../app/composables/useSession', () => ({ useSession: () => ({ isAdmin: ref(true) }) }))

describe('FilesTab', () => {
  it('upload PUTs the chosen file to the default /home/agent path', async () => {
    const w = await mountSuspended(FilesTab, { props: { sandbox: { id: 'n1.s1' } } })
    const file = new File(['hi'], 'report.txt', { type: 'text/plain' })
    const input = w.find('input[type="file"]').element as HTMLInputElement
    Object.defineProperty(input, 'files', { value: [file] })
    await w.find('input[type="file"]').trigger('change')
    await w.find('[data-test="upload"]').trigger('click')
    expect(upload).toHaveBeenCalledWith('/v1/sandboxes/n1.s1/files?path=%2Fhome%2Fagent%2Freport.txt', file)
  })

  it('download builds the file URL from the path field', async () => {
    const w = await mountSuspended(FilesTab, { props: { sandbox: { id: 'n1.s1' } } })
    await w.find('[data-test="dl-path"]').setValue('/home/agent/out.txt')
    await w.find('[data-test="download"]').trigger('click')
    expect(downloadUrl).toHaveBeenCalledWith('/v1/sandboxes/n1.s1/files?path=%2Fhome%2Fagent%2Fout.txt')
  })
})
