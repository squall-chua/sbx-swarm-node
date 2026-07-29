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
      templates: ['docker.io/library/smoketpl:v1'], workspaces: [], labels: {}, capabilities: [], kits: ['tools', 'extras'] }]),
    refreshNodes: vi.fn(),
  }),
}))

// A templates chip wraps in UTooltip when it differs from its requestable form,
// which needs UApp's TooltipProvider; stub it (renders its slot) like the
// Overview test does, since this test mounts the page bare.
const mountOpts = { global: { stubs: { UTooltip: { template: '<div><slot /></div>' } } } }

describe('Nodes', () => {
  it('cordon posts the target node_id in the body', async () => {
    const w = await mountSuspended(Nodes, mountOpts)
    await w.find('[data-test="cordon-n1"]').trigger('click')
    expect(post).toHaveBeenCalledWith('/v1/node/cordon', { node_id: 'n1' })
  })

  it('shows a badge per advertised kit', async () => {
    const w = await mountSuspended(Nodes, mountOpts)
    const kits = w.find('[data-test="kits-n1"]')
    expect(kits.text()).toContain('tools')
    expect(kits.text()).toContain('extras')
  })

  it('shows the requestable template form on the chip, not the canonical gossiped string', async () => {
    const w = await mountSuspended(Nodes, mountOpts)
    expect(w.text()).toContain('smoketpl:v1')
    expect(w.text()).not.toContain('docker.io/library/smoketpl:v1')
  })
})
