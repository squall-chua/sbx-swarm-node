// @vitest-environment nuxt
import { ref } from 'vue'
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { mountSuspended } from '@nuxt/test-utils/runtime'
import SecretsTab from '../app/components/drawer/SecretsTab.vue'

const put = vi.fn(async () => ({}))
const get = vi.fn(async () => ({ custom: [], stored: [] }))
vi.mock('../app/composables/useApi', () => ({ useApi: () => ({ put, get, del: vi.fn(async () => ({})) }) }))
vi.mock('../app/composables/useSession', () => ({ useSession: () => ({ isAdmin: ref(true) }) }))

describe('SecretsTab', () => {
  beforeEach(() => {
    put.mockClear()
    // Reset, not clear: a queued mockResolvedValueOnce that its own test never
    // consumed would otherwise seed the next test's mount. Reset drops the
    // queue and puts back the empty-list default.
    get.mockReset()
  })
  afterEach(() => vi.unstubAllGlobals())

  it('adding a secret PUTs scope=id, host, env, value', async () => {
    const w = await mountSuspended(SecretsTab, { props: { id: 'n1.s1' } })
    await w.find('[data-test="secret-host"]').setValue('api.example.com')
    await w.find('[data-test="secret-env"]').setValue('API_KEY')
    await w.find('[data-test="secret-value"]').setValue('s3cr3t')
    await w.find('[data-test="secret-add"]').trigger('click')
    expect(put).toHaveBeenCalledWith('/v1/sandboxes/n1.s1/secrets',
      { scope: 'n1.s1', host: 'api.example.com', env: 'API_KEY', value: 's3cr3t' })
  })

  it('blocks a lowercase env name before it can 500 (no PUT)', async () => {
    const w = await mountSuspended(SecretsTab, { props: { id: 'n1.s1' } })
    await w.find('[data-test="secret-host"]').setValue('api.example.com')
    await w.find('[data-test="secret-env"]').setValue('api_key') // lowercase — daemon would reject
    await w.find('[data-test="secret-value"]').setValue('s3cr3t')
    await w.find('[data-test="secret-add"]').trigger('click')
    expect(put).not.toHaveBeenCalled()
  })

  it('confirms a re-point to a different host, and cancelling blocks the PUT', async () => {
    get.mockResolvedValueOnce({ custom: [{ host: 'old.example.com', env: 'API_KEY' }], stored: [] })
    const confirmFn = vi.fn(() => false)
    vi.stubGlobal('confirm', confirmFn)
    const w = await mountSuspended(SecretsTab, { props: { id: 'n1.s1' } })
    await w.find('[data-test="secret-host"]').setValue('new.example.com')
    await w.find('[data-test="secret-env"]').setValue('API_KEY')
    await w.find('[data-test="secret-value"]').setValue('s3cr3t')
    await w.find('[data-test="secret-add"]').trigger('click')
    expect(confirmFn).toHaveBeenCalled()
    expect(put).not.toHaveBeenCalled()
  })

  it('does not prompt on a rotation (same env, same host)', async () => {
    get.mockResolvedValueOnce({ custom: [{ host: 'api.example.com', env: 'API_KEY' }], stored: [] })
    const confirmFn = vi.fn(() => false)
    vi.stubGlobal('confirm', confirmFn)
    const w = await mountSuspended(SecretsTab, { props: { id: 'n1.s1' } })
    await w.find('[data-test="secret-host"]').setValue('api.example.com')
    await w.find('[data-test="secret-env"]').setValue('API_KEY')
    await w.find('[data-test="secret-value"]').setValue('rotated')
    await w.find('[data-test="secret-add"]').trigger('click')
    expect(confirmFn).not.toHaveBeenCalled()
    expect(put).toHaveBeenCalled()
  })
})
