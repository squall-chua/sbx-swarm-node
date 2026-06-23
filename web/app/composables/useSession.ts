import { ref } from 'vue'
import type { Api } from './useApi'

// Session state for routing/UX only — the server enforces real auth. login() exchanges a
// bearer key for cookies; loadRole() reads the caller's role from GET /v1/node (Task 1).
export function createSession(base: string, api: Api, fetchImpl: typeof fetch = fetch) {
  const loggedIn = ref(false)
  const role = ref('')

  async function login(key: string) {
    const res = await fetchImpl(base.replace(/\/$/, '') + '/v1/auth/session', {
      method: 'POST',
      headers: { Authorization: 'Bearer ' + key },
      credentials: 'include',
    })
    if (res.status !== 204) throw new Error('invalid key')
    loggedIn.value = true
    await loadRole()
  }
  async function loadRole() {
    role.value = (await api.get('/v1/node'))?.role ?? ''
  }
  function logout() {
    loggedIn.value = false
    role.value = ''
  }
  return { loggedIn, role, login, loadRole, logout }
}

// Nuxt singleton via useState; persists the loggedIn flag in localStorage for the guard.
export const useSession = () => {
  const base = useRuntimeConfig().public.apiBase as string
  const api = useApi()
  const loggedIn = useState('sbx_logged_in', () => import.meta.client && localStorage.getItem('sbx_logged_in') === '1')
  const role = useState('sbx_role', () => '')
  return {
    loggedIn,
    role,
    isAdmin: computed(() => role.value === 'admin'),
    async login(key: string) {
      const core = createSession(base, api)
      await core.login(key)
      loggedIn.value = true
      role.value = core.role.value
      if (import.meta.client) localStorage.setItem('sbx_logged_in', '1')
    },
    async loadRole() {
      role.value = (await api.get('/v1/node'))?.role ?? ''
    },
    logout() {
      loggedIn.value = false
      role.value = ''
      if (import.meta.client) localStorage.removeItem('sbx_logged_in')
      navigateTo('/login')
    },
  }
}
