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
    ready: ref(true),
    cpuHistory: ref([10, 20, 30]),
    memHistory: ref([5, 10, 15]),
    refreshAll: vi.fn(),
  }),
}))

describe('Overview', () => {
  it('renders a card per node with its name', async () => {
    // UTooltip needs UApp's TooltipProvider; stub it (renders its slot) since this
    // test mounts the page bare. The real app provides it via <UApp> in app.vue.
    const wrapper = await mountSuspended(Index, {
      global: { stubs: { UTooltip: { template: '<div><slot /></div>' } } },
    })
    expect(wrapper.text()).toContain('alpha')
  })
})
