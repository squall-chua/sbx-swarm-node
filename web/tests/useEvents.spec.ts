import { describe, it, expect, vi } from 'vitest'
import { createEvents, SWARM_EVENT_TYPES } from '../app/composables/useEvents'

class FakeES {
  url: string
  withCredentials: boolean
  listeners: Record<string, Function> = {}
  closed = false
  constructor(url: string, init?: { withCredentials?: boolean }) {
    this.url = url
    this.withCredentials = !!init?.withCredentials
  }
  addEventListener(type: string, fn: Function) { this.listeners[type] = fn }
  close() { this.closed = true }
  emit(type: string, data: string) { this.listeners[type]?.({ type, data }) }
}

describe('createEvents', () => {
  it('registers a listener per type (never onmessage), with credentials', () => {
    let es!: FakeES
    const ES = vi.fn(function (u: string, i: any) { return (es = new FakeES(u, i)) }) as any
    const seen: Array<[string, any]> = []
    const unsub = createEvents('/', ES)(['sandbox.created', 'operation.done'], (t, d) => seen.push([t, d]))

    expect(es.withCredentials).toBe(true)
    expect(es.url).toBe('/v1/events')
    expect(Object.keys(es.listeners).sort()).toEqual(['operation.done', 'sandbox.created'])

    es.emit('sandbox.created', '{"status":"created"}')
    expect(seen).toEqual([['sandbox.created', { status: 'created' }]])

    unsub()
    expect(es.closed).toBe(true)
  })

  it('adds the sandbox filter to the query string', () => {
    let es!: FakeES
    const ES = vi.fn(function (u: string, i: any) { return (es = new FakeES(u, i)) }) as any
    createEvents('/', ES)(['sandbox.created'], () => {}, { sandbox: 'n1.abc' })
    expect(es.url).toBe('/v1/events?sandbox=n1.abc')
  })

  it('exports the known swarm event types', () => {
    expect(SWARM_EVENT_TYPES).toContain('sandbox.created')
    expect(SWARM_EVENT_TYPES).toContain('operation.done')
  })
})
