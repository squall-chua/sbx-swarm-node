import { describe, it, expect, vi } from 'vitest'
import { createApi } from '../app/composables/useApi'

describe('createApi', () => {
  it('sends CSRF header + credentials on mutations, not on GET', async () => {
    document.cookie = 'sbx_csrf=tok123'
    const fetchMock = vi.fn(async () => new Response('{}', { status: 200 }))
    const api = createApi('/', () => {}, fetchMock as any)

    await api.post('/v1/sandboxes', { cpus: 1 })
    let [, init] = fetchMock.mock.calls[0]
    expect((init.headers as any)['X-CSRF-Token']).toBe('tok123')
    expect(init.credentials).toBe('include')

    await api.get('/v1/sandboxes')
    ;[, init] = fetchMock.mock.calls[1]
    expect((init.headers as any)['X-CSRF-Token']).toBeUndefined()
  })

  it('passes extra headers (e.g. Idempotency-Key) on post', async () => {
    const fetchMock = vi.fn(async () => new Response('{}', { status: 200 }))
    const api = createApi('/', () => {}, fetchMock as any)
    await api.post('/v1/sandboxes', { cpus: 1 }, { 'Idempotency-Key': 'abc' })
    const [, init] = fetchMock.mock.calls[0]
    expect((init.headers as any)['Idempotency-Key']).toBe('abc')
  })

  it('calls onAuthLost on 401', async () => {
    const fetchMock = vi.fn(async () => new Response('nope', { status: 401 }))
    const onAuthLost = vi.fn()
    const api = createApi('/', onAuthLost, fetchMock as any)
    await expect(api.get('/v1/nodes')).rejects.toThrow()
    expect(onAuthLost).toHaveBeenCalledOnce()
  })
})
