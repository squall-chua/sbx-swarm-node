// @vitest-environment nuxt
import { ref } from 'vue'
import { describe, it, expect, vi } from 'vitest'
import { DOMWrapper, flushPromises } from '@vue/test-utils'
import { mountSuspended } from '@nuxt/test-utils/runtime'
import ProvisionModal from '../app/components/ProvisionModal.vue'

const post = vi.fn(async () => ({}))
vi.mock('../app/composables/useApi', () => ({ useApi: () => ({ post, get: vi.fn(async () => ({})) }) }))
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

describe('ProvisionModal template field', () => {
  it('submits a typed template that is not in the options list', async () => {
    await mountSuspended(ProvisionModal, { props: { open: true }, attachTo: document.body })
    const body = new DOMWrapper(document.body)

    // Agent is required to enable the Provision button.
    await body.find('#prov-agent').trigger('click')
    await flushPromises()
    const agentItem = body.findAll('[data-slot="item"]').find((el) => el.text() === 'claude')
    expect(agentItem).toBeTruthy()
    await agentItem!.trigger('keydown', { key: 'Enter' }) // USelect commits on Enter/pointerup, not a plain click
    await flushPromises()

    // Template: this swarm holds nothing, so templateOptions is empty and the
    // typed value has no match — create-item offers it, not a listed one.
    await body.find('#prov-template').trigger('click')
    await flushPromises()
    const search = body.find('input[data-slot="input"]')
    expect(search.exists()).toBe(true)
    await search.setValue('ghcr.io/org/untracked:1')
    await flushPromises()
    const createItem = body.findAll('[data-slot="item"]').find((el) => el.text().includes('ghcr.io/org/untracked:1'))
    expect(createItem).toBeTruthy()
    await createItem!.trigger('click')
    await flushPromises()

    const submit = body.findAll('button').find((el) => el.text() === 'Provision')
    expect(submit!.attributes('disabled')).toBeUndefined()
    await submit!.trigger('click')
    await flushPromises()

    expect(post).toHaveBeenCalled()
    const [, submittedBody] = post.mock.calls[0]
    expect((submittedBody as any).template).toBe('ghcr.io/org/untracked:1')
  })
})
