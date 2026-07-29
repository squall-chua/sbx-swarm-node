// @vitest-environment nuxt
import { ref } from 'vue'
import { describe, it, expect, vi } from 'vitest'
import { mountSuspended } from '@nuxt/test-utils/runtime'
import Templates from '../app/pages/templates.vue'

vi.mock('../app/composables/useApi', () => ({
  useApi: () => ({ get: vi.fn(async () => ({ templates: [] })) }),
}))
vi.mock('../app/composables/useSwarm', () => ({
  useSwarm: () => ({
    nodes: ref([{ node_id: 'n1', node_name: 'alpha', templates: ['docker.io/library/smoketpl:v1'] }]),
    refreshNodes: vi.fn(),
  }),
}))

describe('Templates distribution list', () => {
  it('shows the requestable form, with the canonical gossiped string as a subtitle', async () => {
    const w = await mountSuspended(Templates)
    const text = w.text()
    // The canonical string appears exactly once, inside the "listed as" subtitle —
    // not as the primary label too.
    expect(text.split('docker.io/library/smoketpl:v1')).toHaveLength(2)
    expect(text).toContain('listed as docker.io/library/smoketpl:v1')
  })
})
