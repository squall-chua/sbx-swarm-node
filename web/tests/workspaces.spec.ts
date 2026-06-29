// @vitest-environment nuxt
import { ref } from 'vue'
import { describe, it, expect, vi } from 'vitest'
import { flushPromises } from '@vue/test-utils'
import { mountSuspended } from '@nuxt/test-utils/runtime'
import Workspaces from '../app/pages/workspaces.vue'

vi.mock('../app/composables/useSwarm', () => ({
  useSwarm: () => ({
    nodes: ref([{ node_name: 'dev-node', workspaces: ['repo'], git_workspaces: ['repo'] }]),
    sandboxes: ref([{ id: 'n1.s1', owner_node: 'dev-node', status: 'running', branch: 'main', workspaces: [{ name: 'repo', read_only: false }] }]),
    refreshAll: vi.fn(),
  }),
}))

describe('Workspaces', () => {
  it('lists a git-backed workspace with its mount count and expands to the sandbox', async () => {
    const w = await mountSuspended(Workspaces)
    expect(w.text()).toContain('repo')
    expect(w.text()).toContain('git')
    expect(w.text()).toContain('1 sandbox')
    await w.find('[data-test="expand-repo"]').trigger('click')
    await flushPromises()
    expect(w.text()).toContain('n1.s1')
  })
})
