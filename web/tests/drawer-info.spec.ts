// @vitest-environment nuxt
import { ref } from 'vue'
import { describe, it, expect, vi } from 'vitest'
import { mountSuspended } from '@nuxt/test-utils/runtime'
import InfoTab from '../app/components/drawer/InfoTab.vue'

const post = vi.fn(async () => ({}))
vi.mock('../app/composables/useApi', () => ({ useApi: () => ({ post, get: vi.fn(async () => ({})) }) }))
vi.mock('../app/composables/useSession', () => ({ useSession: () => ({ isAdmin: ref(true) }) }))

describe('InfoTab actions', () => {
  it('Stop posts to the stop endpoint', async () => {
    const w = await mountSuspended(InfoTab, { props: { sandbox: { id: 'n1.s1', status: 'running', ports: [] } } })
    await w.find('[data-test="stop"]').trigger('click')
    expect(post).toHaveBeenCalledWith('/v1/sandboxes/n1.s1/stop')
  })
})
