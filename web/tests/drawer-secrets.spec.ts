// @vitest-environment nuxt
import { ref } from 'vue'
import { describe, it, expect, vi } from 'vitest'
import { mountSuspended } from '@nuxt/test-utils/runtime'
import SecretsTab from '../app/components/drawer/SecretsTab.vue'

const put = vi.fn(async () => ({}))
const get = vi.fn(async () => ({ custom: [], stored: [] }))
vi.mock('../app/composables/useApi', () => ({ useApi: () => ({ put, get, del: vi.fn(async () => ({})) }) }))
vi.mock('../app/composables/useSession', () => ({ useSession: () => ({ isAdmin: ref(true) }) }))

describe('SecretsTab', () => {
  it('adding a secret PUTs scope=id, host, env, value', async () => {
    const w = await mountSuspended(SecretsTab, { props: { id: 'n1.s1' } })
    await w.find('[data-test="secret-host"]').setValue('api.example.com')
    await w.find('[data-test="secret-env"]').setValue('API_KEY')
    await w.find('[data-test="secret-value"]').setValue('s3cr3t')
    await w.find('[data-test="secret-add"]').trigger('click')
    expect(put).toHaveBeenCalledWith('/v1/sandboxes/n1.s1/secrets',
      { scope: 'n1.s1', host: 'api.example.com', env: 'API_KEY', value: 's3cr3t' })
  })
})
