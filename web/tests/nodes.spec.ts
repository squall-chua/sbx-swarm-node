// @vitest-environment nuxt
import { ref } from 'vue'
import { describe, it, expect, vi } from 'vitest'
import { mountSuspended } from '@nuxt/test-utils/runtime'
import Nodes from '../app/pages/nodes.vue'

const post = vi.fn(async () => ({}))
vi.mock('../app/composables/useApi', () => ({ useApi: () => ({ post, get: vi.fn(async () => ({ node_ids: [] })) }) }))
vi.mock('../app/composables/useSession', () => ({ useSession: () => ({ isAdmin: ref(true) }) }))
vi.mock('../app/composables/useSwarm', () => ({
  useSwarm: () => ({
    nodes: ref([{ node_id: 'n1', node_name: 'alpha', cordoned: false, draining: false,
      limit_cpu: 8, alloc_cpu: 0, actual_cpu: 0, limit_mem_kb: 1, alloc_mem_kb: 0,
      templates: [], workspaces: [], labels: {}, capabilities: [] }]),
    refreshNodes: vi.fn(),
  }),
}))

describe('Nodes', () => {
  it('cordon posts the target node_id in the body', async () => {
    const w = await mountSuspended(Nodes)
    await w.find('[data-test="cordon-n1"]').trigger('click')
    expect(post).toHaveBeenCalledWith('/v1/node/cordon', { node_id: 'n1' })
  })
})
