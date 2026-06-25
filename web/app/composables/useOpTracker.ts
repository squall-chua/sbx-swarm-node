// Tracks an async operation to its terminal state. CreateSandbox/DeleteSandbox
// return a pending Operation; the real success/failure lands later on the live
// operations list (driven by operation.* SSE events). track() watches that list
// for the op id and fires onDone/onError once — otherwise an async failure is
// silent. The watcher self-stops on terminal or after a timeout so it never dangles.
//
// Call useOpTracker() at setup; invoke the returned track() from a handler.
export function useOpTracker() {
  const swarm = useSwarm()
  return function track(
    opId: string | undefined,
    handlers: { onDone?: (op: any) => void; onError?: (op: any) => void },
    timeoutMs = 5 * 60_000,
  ): void {
    if (!opId || !swarm) return
    let stopWatch: (() => void) | null = null
    let timer: ReturnType<typeof setTimeout> | null = null
    const finish = () => { stopWatch?.(); if (timer) clearTimeout(timer) }
    stopWatch = watch(() => swarm!.operations.value, (ops) => {
      const op = (ops ?? []).find((o: any) => o.id === opId)
      if (!op) return
      if (op.state === 'done') { finish(); handlers.onDone?.(op) }
      else if (op.state === 'error') { finish(); handlers.onError?.(op) }
    })
    timer = setTimeout(finish, timeoutMs)
  }
}
