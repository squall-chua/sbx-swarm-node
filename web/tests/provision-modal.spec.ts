// @vitest-environment nuxt
import { ref } from 'vue'
import { describe, it, expect, vi } from 'vitest'
import { DOMWrapper } from '@vue/test-utils'
import { mountSuspended } from '@nuxt/test-utils/runtime'
import ProvisionModal from '../app/components/ProvisionModal.vue'

vi.mock('../app/composables/useApi', () => ({ useApi: () => ({ post: vi.fn(async () => ({})), get: vi.fn(async () => ({})) }) }))
vi.mock('../app/composables/useSwarm', () => ({
  useSwarm: () => ({
    nodes: ref([{ node_id: 'n1', node_name: 'alpha', templates: [], workspaces: ['repo'], kits: ['tools', 'extras'] }]),
    refreshSandboxes: vi.fn(),
  }),
}))

// UModal teleports its content to document.body, outside the mountSuspended
// wrapper's root — query the real DOM to find it.
function findLabelFor(text: string) {
  const body = new DOMWrapper(document.body)
  return body.findAll('label').find((l) => l.text() === text)
}

describe('ProvisionModal accessibility', () => {
  it('associates the Workspaces label with its select via for/id', async () => {
    await mountSuspended(ProvisionModal, { props: { open: true }, attachTo: document.body })
    const label = findLabelFor('Workspaces')
    expect(label).toBeTruthy()
    const forId = label!.attributes('for')
    expect(forId).toBeTruthy()
    expect(document.getElementById(forId!)).toBeTruthy()
  })

  it('associates the Kits label with its select via for/id', async () => {
    await mountSuspended(ProvisionModal, { props: { open: true }, attachTo: document.body })
    const label = findLabelFor('Kits')
    expect(label).toBeTruthy()
    const forId = label!.attributes('for')
    expect(forId).toBeTruthy()
    expect(document.getElementById(forId!)).toBeTruthy()
  })
})
