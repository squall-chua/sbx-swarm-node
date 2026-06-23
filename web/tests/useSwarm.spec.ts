import { describe, it, expect, vi } from 'vitest'
import { createSwarmStore } from '../app/composables/useSwarm'

describe('createSwarmStore', () => {
  it('a sandbox.* event pokes a debounced sandbox refetch', async () => {
    vi.useFakeTimers()
    const api = {
      get: vi.fn(async (p: string) => {
        if (p === '/v1/sandboxes') return { sandboxes: [{ id: 's1' }] }
        if (p === '/v1/nodes') return { nodes: [] }
        if (p === '/v1/operations') return { operations: [] }
      }),
    } as any
    let handler!: (type: string, data: any) => void
    const subscribe = vi.fn((_types: string[], cb: any) => { handler = cb; return () => {} })

    const store = createSwarmStore(api, subscribe, { debounceMs: 100, backstopMs: 999999 })
    api.get.mockClear()

    handler('sandbox.created', null)
    handler('sandbox.created', null) // debounced: collapses to one
    vi.advanceTimersByTime(100)
    await Promise.resolve()

    expect(api.get).toHaveBeenCalledWith('/v1/sandboxes')
    store.stop()
    vi.useRealTimers()
  })
})
