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
  upload: (path: string, body: Blob, onProgress?: (loaded: number, total: number) => void) => Promise<void>
  download: (path: string, onProgress?: (loaded: number, total: number) => void) => Promise<Blob>
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
  const root = base.replace(/\/$/, '')
  return {
    get: (p) => req('GET', p),
    post: (p, b, h) => req('POST', p, b, h),
    put: (p, b) => req('PUT', p, b),
    del: (p) => req('DELETE', p),
    // XMLHttpRequest (not fetch) so we can report upload byte-progress via
    // xhr.upload.onprogress. Raw body, no Content-Type; CSRF + cookie like req.
    upload: (p, body, onProgress) => new Promise((resolve, reject) => {
      const xhr = new XMLHttpRequest()
      xhr.open('PUT', root + p)
      xhr.withCredentials = true
      xhr.setRequestHeader('X-CSRF-Token', readCookie('sbx_csrf'))
      if (onProgress) xhr.upload.onprogress = (e) => { if (e.lengthComputable) onProgress(e.loaded, e.total) }
      xhr.onload = () => {
        if (xhr.status === 401) { onAuthLost(); reject(new Error('unauthorized')); return }
        if (xhr.status >= 200 && xhr.status < 300) { resolve(); return }
        let msg = `PUT ${p} -> ${xhr.status}`
        try { const m = JSON.parse(xhr.responseText)?.message; if (m) msg = String(m) } catch { /* keep generic */ }
        reject(new Error(msg))
      }
      xhr.onerror = () => reject(new Error('network error'))
      xhr.send(body)
    }),
    // Stream the response so we can report download byte-progress, then hand back
    // a Blob for the caller to save.
    download: async (p, onProgress) => {
      const res = await fetchImpl(root + p, { credentials: 'include' })
      if (res.status === 401) { onAuthLost(); throw new Error('unauthorized') }
      if (!res.ok) {
        const err = new Error(res.status === 404 ? 'not found' : `GET ${p} -> ${res.status}`) as Error & { status?: number }
        err.status = res.status
        throw err
      }
      const total = Number(res.headers.get('Content-Length')) || 0
      if (!res.body?.getReader) return res.blob()
      const reader = res.body.getReader()
      const chunks: BlobPart[] = []
      let loaded = 0
      for (;;) {
        const { done, value } = await reader.read()
        if (done) break
        chunks.push(value)
        loaded += value.byteLength
        if (onProgress) onProgress(loaded, total)
      }
      return new Blob(chunks)
    },
  }
}

// Nuxt wrapper: same-origin base; on auth loss, clear the flag and route to /login.
export const useApi = (): Api =>
  createApi(useRuntimeConfig().public.apiBase as string, () => {
    if (import.meta.client) localStorage.removeItem('sbx_logged_in')
    navigateTo('/login')
  })
