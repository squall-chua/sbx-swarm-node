// @vitest-environment nuxt
import { ref } from 'vue'
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { mountSuspended } from '@nuxt/test-utils/runtime'
import Network from '../app/pages/network.vue'

const post = vi.fn(async () => ({ allowed: false, deny_kind: 'implicit', reason: 'no rule matched' }))
const get = vi.fn(async (p: string) =>
  p.endsWith('/policy') ? { rules: [] } : { custom: [], stored: [] })
vi.mock('../app/composables/useApi', () => ({
  useApi: () => ({ get, post, put: vi.fn(async () => ({})), del: vi.fn(async () => ({})) }),
}))
vi.mock('../app/composables/useSession', () => ({ useSession: () => ({ isAdmin: ref(true) }) }))

describe('network policy check', () => {
  beforeEach(() => post.mockClear())

  it('POSTs the target and shows the verdict', async () => {
    const w = await mountSuspended(Network)
    await w.find('[data-test="policy-check-target"]').setValue('api.example.com')
    await w.find('[data-test="policy-check"]').trigger('click')
    expect(post).toHaveBeenCalledWith('/v1/sandboxes/_node/policy/check',
      { scope: '_node', target: 'api.example.com' })
    await new Promise((r) => setTimeout(r, 0))
    expect(w.find('[data-test="policy-check-result"]').text()).toContain('denied')
  })

  it('does not call the API with an empty target', async () => {
    const w = await mountSuspended(Network)
    await w.find('[data-test="policy-check"]').trigger('click')
    expect(post).not.toHaveBeenCalled()
  })
})
