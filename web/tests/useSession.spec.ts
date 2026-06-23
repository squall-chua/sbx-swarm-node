import { describe, it, expect, vi } from 'vitest'
import { createSession } from '../app/composables/useSession'

describe('createSession', () => {
  it('login POSTs the bearer key to the session endpoint', async () => {
    const fetchMock = vi.fn(async () => new Response(null, { status: 204 }))
    const api = { get: vi.fn(async () => ({ role: 'admin' })) } as any
    const s = createSession('/', api, fetchMock as any)

    await s.login('secret-key')
    const [url, init] = fetchMock.mock.calls[0]
    expect(url).toBe('/v1/auth/session')
    expect(init.method).toBe('POST')
    expect((init.headers as any).Authorization).toBe('Bearer secret-key')
    expect(init.credentials).toBe('include')
    expect(s.loggedIn.value).toBe(true)
  })

  it('loadRole pulls role from GET /v1/node', async () => {
    const api = { get: vi.fn(async () => ({ role: 'read-only' })) } as any
    const s = createSession('/', api, vi.fn() as any)
    await s.loadRole()
    expect(api.get).toHaveBeenCalledWith('/v1/node')
    expect(s.role.value).toBe('read-only')
  })
})
