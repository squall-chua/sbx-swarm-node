// @vitest-environment nuxt
import { ref } from 'vue'
import { describe, it, expect, vi } from 'vitest'
import { DOMWrapper, flushPromises } from '@vue/test-utils'
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
    w.unmount() // UModal teleports outside the wrapper root; unmount so its content
    // doesn't leak into the next test's document.body query.
  })

  it('disables save-template-confirm until a tag is typed', async () => {
    const w = await mountSuspended(InfoTab, {
      props: { sandbox: { id: 'n1.s1', status: 'stopped', ports: [] } },
      attachTo: document.body,
    })
    await w.find('[data-test="save-template"]').trigger('click')
    const body = new DOMWrapper(document.body)
    expect(body.find('[data-test="save-template-confirm"]').attributes('disabled')).toBeDefined()
    await body.find('[data-test="save-template-tag"]').setValue('myimage:v1')
    expect(body.find('[data-test="save-template-confirm"]').attributes('disabled')).toBeUndefined()
    w.unmount() // see note above — leaves the modal open, so must unmount explicitly.
  })

  it('clears the tag after a successful save, so reopening does not resubmit the old tag', async () => {
    const w = await mountSuspended(InfoTab, {
      props: { sandbox: { id: 'n1.s1', status: 'stopped', ports: [] } },
      attachTo: document.body,
    })
    const body = new DOMWrapper(document.body)
    await w.find('[data-test="save-template"]').trigger('click')
    await body.find('[data-test="save-template-tag"]').setValue('myimage:v1')
    await body.find('[data-test="save-template-confirm"]').trigger('click')
    await flushPromises() // let the awaited api.post() in doSaveTemplate settle before reopening

    // Reopen: the tag input must be empty, and Save must be disabled again.
    await w.find('[data-test="save-template"]').trigger('click')
    expect((body.find('[data-test="save-template-tag"]').element as HTMLInputElement).value).toBe('')
    expect(body.find('[data-test="save-template-confirm"]').attributes('disabled')).toBeDefined()
    w.unmount()
  })

  it('clears the tag on cancel, so reopening does not resubmit the old tag', async () => {
    const w = await mountSuspended(InfoTab, {
      props: { sandbox: { id: 'n1.s1', status: 'stopped', ports: [] } },
      attachTo: document.body,
    })
    const body = new DOMWrapper(document.body)
    await w.find('[data-test="save-template"]').trigger('click')
    await body.find('[data-test="save-template-tag"]').setValue('myimage:v1')
    const cancel = body.findAll('button').find((el) => el.text() === 'Cancel')
    await cancel!.trigger('click')

    // Reopen: the tag input must be empty, and Save must be disabled again.
    await w.find('[data-test="save-template"]').trigger('click')
    expect((body.find('[data-test="save-template-tag"]').element as HTMLInputElement).value).toBe('')
    expect(body.find('[data-test="save-template-confirm"]').attributes('disabled')).toBeDefined()
    w.unmount()
  })
})
