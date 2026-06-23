// @vitest-environment nuxt
import { ref } from 'vue'
import { describe, it, expect, vi } from 'vitest'
import { mountSuspended } from '@nuxt/test-utils/runtime'
import Operations from '../app/pages/operations.vue'

vi.mock('../app/composables/useSwarm', () => ({
  useSwarm: () => ({
    operations: ref([{ id: 'op1', type: 'provision', state: 'done', sandbox_id: 'n1.s1', error: '', created_at: '', updated_at: '' }]),
    refreshOperations: vi.fn(),
  }),
}))

describe('Operations', () => {
  it('lists operations newest-first from the store', async () => {
    const w = await mountSuspended(Operations)
    expect(w.text()).toContain('op1')
    expect(w.text()).toContain('provision')
  })
})
