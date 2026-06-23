// @vitest-environment nuxt
import { ref } from 'vue'
import { describe, it, expect, vi } from 'vitest'
import { mountSuspended } from '@nuxt/test-utils/runtime'
import Index from '../app/pages/index.vue'

vi.mock('../app/composables/useSwarm', () => ({
  useSwarm: () => ({
    nodes: ref([
      { node_id: 'n1', node_name: 'alpha', cordoned: false, draining: false,
        limit_cpu: 8, alloc_cpu: 2, actual_cpu: 1, limit_mem_kb: 100, alloc_mem_kb: 10,
        templates: [], workspaces: [], labels: {}, capabilities: [] },
    ]),
    sandboxes: ref([{ id: 'n1.s1', owner_node: 'n1', status: 'running' }]),
    operations: ref([]),
    refreshAll: vi.fn(),
  }),
}))

describe('Overview', () => {
  it('renders a card per node with its name', async () => {
    const wrapper = await mountSuspended(Index)
    expect(wrapper.text()).toContain('alpha')
  })
})
