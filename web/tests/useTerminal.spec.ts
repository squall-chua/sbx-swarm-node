import { describe, it, expect, vi } from 'vitest'
import { createTerminal } from '../app/composables/useTerminal'

class FakeWS {
  static OPEN = 1
  readyState = 1
  binaryType = ''
  onmessage: ((e: any) => void) | null = null
  sent: any[] = []
  closed = false
  constructor(public url: string) {}
  send(d: any) { this.sent.push(d) }
  close() { this.closed = true }
}

describe('createTerminal', () => {
  it('bridges WS<->xterm and pings keepalive', () => {
    vi.useFakeTimers()
    let ws!: FakeWS
    const WS = vi.fn(function (u: string) { return (ws = new FakeWS(u)) }) as any
    const written: Uint8Array[] = []
    let dataCb: (d: string) => void = () => {}
    const io = { onData: (cb: any) => (dataCb = cb), write: (b: Uint8Array) => written.push(b) }
    const keepAlive = vi.fn()

    const term = createTerminal('wss://x/v1/sandboxes/s1/terminal', io, keepAlive, { WS, keepAliveMs: 1000 })
    expect(ws.binaryType).toBe('arraybuffer')

    // server -> xterm
    ws.onmessage!({ data: new TextEncoder().encode('hi').buffer })
    expect(new TextDecoder().decode(written[0])).toBe('hi')

    // xterm -> server (binary)
    dataCb('x')
    expect(ws.sent[0]).toBeInstanceOf(Uint8Array)

    // resize -> text JSON
    term.resize(80, 24)
    expect(JSON.parse(ws.sent[1] as string)).toEqual({ type: 'resize', cols: 80, rows: 24 })

    // keepalive ticks
    vi.advanceTimersByTime(1000)
    expect(keepAlive).toHaveBeenCalledOnce()

    term.close()
    expect(ws.closed).toBe(true)
    vi.useRealTimers()
  })
})
