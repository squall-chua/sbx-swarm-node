import { describe, it, expect } from 'vitest'
import { ref, markRaw } from 'vue'

// Guards the useSwarm() singleton fix (app/composables/useSwarm.ts): the store
// holds live refs (nodes/sandboxes/operations). Nuxt useState backs the holder
// with Vue reactive state, which DEEPLY reactifies an assigned object and
// UNWRAPS its nested refs — so `store.nodes.value` becomes undefined and every
// consumer crashes on `.length`. markRaw must wrap the store. This reproduces
// the exact mechanism with Vue primitives.
describe('store-in-reactive-container ref unwrapping', () => {
  it('without markRaw, nested refs are unwrapped (.value is undefined) — the bug', () => {
    const holder = ref<any>(null)
    holder.value = { nodes: ref<number[]>([1, 2]) } // reactified on assign
    expect(holder.value.nodes.value).toBeUndefined()
  })

  it('with markRaw, nested refs survive so .value works — the fix', () => {
    const holder = ref<any>(null)
    holder.value = markRaw({ nodes: ref<number[]>([1, 2]) })
    expect(holder.value.nodes.value).toEqual([1, 2])
  })
})
