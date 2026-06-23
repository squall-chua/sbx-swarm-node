// Subscribes to the SSE firehose (/v1/events). The server writes named events
// (event:<type>), so onmessage never fires — we addEventListener per type. EventSource
// sends the session cookie automatically (ADR-0006). Frames are thin pokes: payload only,
// no sandbox id (finding #3), so callers refetch rather than apply deltas.
export const SWARM_EVENT_TYPES = [
  'sandbox.created', 'sandbox.running', 'sandbox.stopped', 'sandbox.deleted',
  'sandbox.lost', 'sandbox.published', 'sandbox.publish_failed',
  'operation.pending', 'operation.running', 'operation.done', 'operation.error',
]

export function createEvents(base: string, ES: typeof EventSource = EventSource) {
  return function subscribe(
    types: string[],
    onEvent: (type: string, data: any) => void,
    opts: { sandbox?: string } = {},
  ): () => void {
    const q = new URLSearchParams()
    if (opts.sandbox) q.set('sandbox', opts.sandbox)
    const qs = q.toString()
    const url = `${base.replace(/\/$/, '')}/v1/events${qs ? '?' + qs : ''}`
    const es = new ES(url, { withCredentials: true })
    const handler = (ev: MessageEvent) => onEvent((ev as any).type, ev.data ? JSON.parse(ev.data) : null)
    for (const t of types) es.addEventListener(t, handler as EventListener)
    return () => es.close()
  }
}

export const useEvents = () => createEvents(useRuntimeConfig().public.apiBase as string)
