// @vitest-environment nuxt
import { ref } from 'vue'
import { describe, it, expect, vi } from 'vitest'
import { DOMWrapper } from '@vue/test-utils'
import { mountSuspended } from '@nuxt/test-utils/runtime'
import InfoTab from '../app/components/drawer/InfoTab.vue'

const post = vi.fn(async () => ({}))
vi.mock('../app/composables/useApi', () => ({ useApi: () => ({ post, get: vi.fn(async () => ({})) }) }))
vi.mock('../app/composables/useSession', () => ({ useSession: () => ({ isAdmin: ref(true) }) }))
// useOpTracker pulls in the swarm/events store (EventSource); the no-op keeps InfoTab mountable.
vi.mock('../app/composables/useOpTracker', () => ({ useOpTracker: () => () => {} }))

describe('InfoTab actions', () => {
  it('Stop posts to the stop endpoint', async () => {
    const w = await mountSuspended(InfoTab, { props: { sandbox: { id: 'n1.s1', status: 'running', ports: [] } } })
    await w.find('[data-test="stop"]').trigger('click')
    expect(post).toHaveBeenCalledWith('/v1/sandboxes/n1.s1/stop')
  })

  it('shows a badge per kit', async () => {
    const w = await mountSuspended(InfoTab, {
      props: { sandbox: { id: 'n1.s1', status: 'running', ports: [], kits: ['tools', 'extras'] } },
    })
    expect(w.text()).toContain('tools')
    expect(w.text()).toContain('extras')
  })

  it('disables save-as-template while the sandbox is running', async () => {
    const w = await mountSuspended(InfoTab, { props: { sandbox: { id: 'n1.s1', status: 'running', ports: [] } } })
    expect(w.find('[data-test="save-template"]').attributes('disabled')).toBeDefined()
  })

  it('posts the tag when saving a stopped sandbox', async () => {
    // UModal teleports its content to document.body, outside the mountSuspended
    // wrapper's root — query the real DOM to find it (see provision-modal.spec.ts).
    const w = await mountSuspended(InfoTab, {
      props: { sandbox: { id: 'n1.s1', status: 'stopped', ports: [] } },
      attachTo: document.body,
    })
    await w.find('[data-test="save-template"]').trigger('click')
    const body = new DOMWrapper(document.body)
    await body.find('[data-test="save-template-tag"]').setValue('myimage:v1')
    await body.find('[data-test="save-template-confirm"]').trigger('click')
    expect(post).toHaveBeenCalledWith('/v1/sandboxes/n1.s1/template', { tag: 'myimage:v1' })
  })
})
