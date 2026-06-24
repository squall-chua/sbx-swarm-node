import { markRaw, ref } from 'vue'
import type { Api } from './useApi'
import { SWARM_EVENT_TYPES } from './useEvents'

type Subscribe = (
  types: string[],
  onEvent: (type: string, data: any) => void,
  opts?: { sandbox?: string },
) => () => void

// App-wide live store. One unfiltered firehose drives coarse refetches: a sandbox.* event
// refetches the sandbox list (+ nodes, since allocation changes); operation.* refetches
// operations. A periodic backstop catches any missed/unknown-type events (findings #2/#3).
export function createSwarmStore(api: Api, subscribe: Subscribe, opts: { debounceMs?: number; backstopMs?: number } = {}) {
  const nodes = ref<any[]>([])
  const sandboxes = ref<any[]>([])
  const operations = ref<any[]>([])

  const refreshNodes = async () => { nodes.value = (await api.get('/v1/nodes'))?.nodes ?? [] }
  const refreshSandboxes = async () => { sandboxes.value = (await api.get('/v1/sandboxes'))?.sandboxes ?? [] }
  const refreshOperations = async () => { operations.value = (await api.get('/v1/operations'))?.operations ?? [] }
  const refreshAll = () => Promise.all([refreshNodes(), refreshSandboxes(), refreshOperations()])

  const debounce = (fn: () => void, ms: number) => {
    let t: any
    return () => { clearTimeout(t); t = setTimeout(fn, ms) }
  }
  const d = opts.debounceMs ?? 300
  const pokeSandboxes = debounce(refreshSandboxes, d)
  const pokeNodes = debounce(refreshNodes, d)
  const pokeOps = debounce(refreshOperations, d)

  const unsub = subscribe(SWARM_EVENT_TYPES, (type) => {
    if (type.startsWith('sandbox.')) { pokeSandboxes(); pokeNodes() }
    else if (type.startsWith('operation.')) { pokeOps() }
  })
  const interval = setInterval(refreshAll, opts.backstopMs ?? 25_000)
  const stop = () => { unsub(); clearInterval(interval) }

  return { nodes, sandboxes, operations, refreshAll, refreshNodes, refreshSandboxes, refreshOperations, stop }
}

// Nuxt singleton: created once, shared across views.
export const useSwarm = () => {
  const holder = useState<ReturnType<typeof createSwarmStore> | null>('sbx_swarm', () => null)
  if (!holder.value && import.meta.client) {
    // markRaw: the store holds live refs (nodes/sandboxes/operations). useState
    // backs holder with Vue reactive state, which would deeply reactify the store
    // and UNWRAP those nested refs — making `swarm.nodes.value` undefined and
    // crashing every consumer. markRaw keeps the store's refs intact.
    holder.value = markRaw(createSwarmStore(useApi(), useEvents()))
    holder.value.refreshAll()
  }
  return holder.value!
}
