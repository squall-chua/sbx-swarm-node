// Bridges a sandbox terminal WebSocket to an xterm-like IO. Binary frames carry I/O
// (keystrokes go out as binary so the server routes them to stdin); a text JSON frame
// {"type":"resize",cols,rows} resizes. While attached we KeepAlive on a ticker so an idle
// interactive session isn't idle-stopped (the server bumps Activity only at attach).
export type TermIO = {
  onData: (cb: (d: string) => void) => void
  write: (bytes: Uint8Array) => void
}

export function createTerminal(
  wsUrl: string,
  io: TermIO,
  keepAlive: () => void,
  opts: { WS?: typeof WebSocket; keepAliveMs?: number } = {},
) {
  const WS = opts.WS ?? WebSocket
  const ws = new WS(wsUrl)
  ws.binaryType = 'arraybuffer'
  ws.onmessage = (ev: MessageEvent) => {
    if (ev.data instanceof ArrayBuffer) io.write(new Uint8Array(ev.data))
  }
  io.onData((d) => {
    if (ws.readyState === 1) ws.send(new TextEncoder().encode(d))
  })
  const resize = (cols: number, rows: number) => {
    if (ws.readyState === 1) ws.send(JSON.stringify({ type: 'resize', cols, rows }))
  }
  const timer = setInterval(keepAlive, opts.keepAliveMs ?? 60_000)
  const close = () => {
    clearInterval(timer)
    ws.close()
  }
  return { resize, close }
}
