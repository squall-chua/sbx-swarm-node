// Typed REST client for the node API. Cookie auth (credentials:include) + double-submit
// CSRF on mutations (ADR-0006). 401 -> drop session and bounce to /login. We deliberately
// do NOT auto-redirect on 403: the UI gates mutations by role, so a 403 is rare and is
// surfaced as an error (session expiry shows up as 401 first, since the cookie is checked
// before CSRF in internal/auth/auth.go).
function readCookie(name: string): string {
  const m = document.cookie.match(new RegExp('(?:^|; )' + name + '=([^;]*)'))
  return m ? decodeURIComponent(m[1]) : ''
}

export type Api = {
  get: (path: string) => Promise<any>
  post: (path: string, body?: unknown, headers?: Record<string, string>) => Promise<any>
  put: (path: string, body?: unknown) => Promise<any>
  del: (path: string) => Promise<any>
}

export function createApi(base: string, onAuthLost: () => void, fetchImpl: typeof fetch = fetch): Api {
  async function req(method: string, path: string, body?: unknown, extra?: Record<string, string>) {
    const headers: Record<string, string> = { 'Content-Type': 'application/json', ...(extra ?? {}) }
    if (method !== 'GET' && method !== 'HEAD') headers['X-CSRF-Token'] = readCookie('sbx_csrf')
    const res = await fetchImpl(base.replace(/\/$/, '') + path, {
      method,
      headers,
      credentials: 'include',
      body: body === undefined ? undefined : JSON.stringify(body),
    })
    if (res.status === 401) {
      onAuthLost()
      throw new Error('unauthorized')
    }
    if (!res.ok) {
      // Surface the server's error message (grpc-gateway returns {code,message});
      // the daemon prefixes its real reason with "ERROR:". Fall back to the status.
      let msg = `${method} ${path} -> ${res.status}`
      try {
        const m = (await res.json())?.message
        if (m) msg = String(m).includes('ERROR:') ? String(m).split('ERROR:').pop()!.trim() : String(m)
      } catch { /* non-JSON error body: keep the generic message */ }
      throw new Error(msg)
    }
    return res.status === 204 ? null : res.json()
  }
  return {
    get: (p) => req('GET', p),
    post: (p, b, h) => req('POST', p, b, h),
    put: (p, b) => req('PUT', p, b),
    del: (p) => req('DELETE', p),
  }
}

// Nuxt wrapper: same-origin base; on auth loss, clear the flag and route to /login.
export const useApi = (): Api =>
  createApi(useRuntimeConfig().public.apiBase as string, () => {
    if (import.meta.client) localStorage.removeItem('sbx_logged_in')
    navigateTo('/login')
  })
