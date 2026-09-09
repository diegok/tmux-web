import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { FRAME_CONTROL, FRAME_DATA } from '@/lib/transport'

import {
  BACKOFF_BASE_MS,
  BACKOFF_MAX_MS,
  RESIZE_DEBOUNCE_MS,
  TerminalSession,
  backoffDelay,
  isPaneId,
  paneStorageKey,
  shouldReconnect,
  terminalUrl,
} from './Terminal'
import type { PaneStorage, TerminalStatus } from './Terminal'

// What is tested here is TerminalSession: the socket lifecycle, the ordering
// rule that a keystroke can never overtake the select that positions the new
// session, the resize debounce, and the backoff. It runs against the *real*
// Transport with a stubbed WebSocket, so the assertions are on actual wire
// frames rather than on a mock of our own protocol.
//
// The React wrapper is deliberately not rendered. It has no logic beyond
// forwarding wterm's props into this class, and a jsdom test of it could only
// assert that React rendered -- while the thing that would actually break (does
// a real terminal resize, reconnect and land on the right pane?) is what Task
// 23 drives through Playwright against a real tmux.

class MockWebSocket {
  static instances: MockWebSocket[] = []

  static reset() {
    MockWebSocket.instances = []
  }

  static get last(): MockWebSocket {
    const ws = MockWebSocket.instances.at(-1)
    if (!ws) throw new Error('no WebSocket was constructed')
    return ws
  }

  readonly url: string
  binaryType = 'blob'
  readyState = 0
  readonly sent: Uint8Array[] = []
  closedWith: { code: number; reason: string } | null = null

  onopen: (() => void) | null = null
  onmessage: ((event: MessageEvent) => void) | null = null
  onclose: ((event: CloseEvent) => void) | null = null
  onerror: ((event: Event) => void) | null = null

  constructor(url: string) {
    this.url = url
    MockWebSocket.instances.push(this)
  }

  send(data: Uint8Array) {
    // A browser throws here rather than buffering, which is what turns a
    // missing `connected` guard into a visible failure instead of a silent one.
    if (this.readyState === 0) throw new Error('InvalidStateError: still CONNECTING')
    if (this.readyState !== 1) return
    this.sent.push(new Uint8Array(data))
  }

  close(code = 1000, reason = '') {
    this.closedWith = { code, reason }
    this.readyState = 2
  }

  open() {
    this.readyState = 1
    this.onopen?.()
  }

  receive(frame: Uint8Array) {
    const buf = frame.buffer.slice(frame.byteOffset, frame.byteOffset + frame.byteLength)
    this.onmessage?.({ data: buf } as MessageEvent)
  }

  emitClose(code = 1006, reason = '', wasClean = false) {
    this.readyState = 3
    this.onclose?.({ code, reason, wasClean } as CloseEvent)
  }
}

/** One sent frame, decoded far enough to assert on. */
interface SentFrame {
  kind: number
  text: string
  json: Record<string, unknown> | null
}

function frames(ws: MockWebSocket): SentFrame[] {
  return ws.sent.map((f) => {
    const text = new TextDecoder().decode(f.subarray(1))
    let json: Record<string, unknown> | null = null
    if (f[0] === FRAME_CONTROL) json = JSON.parse(text)
    return { kind: f[0], text, json }
  })
}

const controls = (ws: MockWebSocket) => frames(ws).filter((f) => f.kind === FRAME_CONTROL)
const data = (ws: MockWebSocket) => frames(ws).filter((f) => f.kind === FRAME_DATA)
const types = (ws: MockWebSocket) => controls(ws).map((f) => f.json?.type)

function memoryStorage(seed: Record<string, string> = {}): PaneStorage & { map: Map<string, string> } {
  const map = new Map(Object.entries(seed))
  return {
    map,
    getItem: (k) => map.get(k) ?? null,
    setItem: (k, v) => void map.set(k, v),
    removeItem: (k) => void map.delete(k),
  }
}

function makeSession(opts: { storage?: PaneStorage | null; onStatus?: (s: TerminalStatus) => void } = {}) {
  const statuses: TerminalStatus[] = []
  const received: Uint8Array[] = []
  const storage = opts.storage === undefined ? memoryStorage() : opts.storage
  const term = new TerminalSession({
    url: 'ws://localhost/ws?session=work',
    paneKey: paneStorageKey('work'),
    onData: (bytes) => void received.push(bytes),
    onStatus: (s) => {
      statuses.push(s)
      opts.onStatus?.(s)
    },
    storage,
    // Neutral jitter, so delays are exactly the exponential schedule.
    random: () => 0.5,
  })
  return { term, statuses, received, storage }
}

beforeEach(() => {
  MockWebSocket.reset()
  vi.stubGlobal('WebSocket', MockWebSocket)
  vi.useFakeTimers()
  vi.spyOn(console, 'warn').mockImplementation(() => {})
})

afterEach(() => {
  vi.useRealTimers()
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
})

describe('pure helpers', () => {
  it('builds a same-origin ws url, upgrading to wss on https', () => {
    expect(terminalUrl('work', { protocol: 'http:', host: 'localhost:7000' })).toBe(
      'ws://localhost:7000/ws?session=work',
    )
    expect(terminalUrl('work', { protocol: 'https:', host: 'tmux.example.com' })).toBe(
      'wss://tmux.example.com/ws?session=work',
    )
  })

  it('escapes the session name into the query', () => {
    expect(terminalUrl('a b&c', { protocol: 'http:', host: 'h' })).toBe(
      'ws://h/ws?session=a%20b%26c',
    )
  })

  it('accepts only tmux pane ids', () => {
    expect(isPaneId('%3')).toBe(true)
    expect(isPaneId('%12')).toBe(true)
    // Each of these exits 0 in tmux and moves the pane the *user* is in.
    expect(isPaneId('work')).toBe(false)
    expect(isPaneId('%')).toBe(false)
    expect(isPaneId('%3a')).toBe(false)
    expect(isPaneId('work:1.0')).toBe(false)
    expect(isPaneId('')).toBe(false)
  })

  it('backs off exponentially, capped', () => {
    const half = () => 0.5 // no jitter
    expect([0, 1, 2, 3, 4, 5, 6, 20].map((n) => backoffDelay(n, half))).toEqual([
      500, 1000, 2000, 4000, 8000, 15000, 15000, 15000,
    ])
  })

  it('keeps every jittered delay inside the bounds', () => {
    for (const r of [0, 0.001, 0.25, 0.5, 0.75, 0.999]) {
      for (const attempt of [0, 1, 2, 3, 4, 5, 9]) {
        const d = backoffDelay(attempt, () => r)
        expect(d).toBeGreaterThanOrEqual(BACKOFF_BASE_MS)
        expect(d).toBeLessThanOrEqual(BACKOFF_MAX_MS)
      }
    }
    // Jitter actually moves the number, or the constant is dead code.
    expect(backoffDelay(2, () => 0.1)).not.toBe(backoffDelay(2, () => 0.9))
  })

  it('reconnects from every close but the daemon deliberate 1000', () => {
    const close = (code: number, reason = '', wasClean = false) => ({ code, reason, wasClean })
    // wsWriteLoop: the PTY reached EOF, the session is over.
    expect(shouldReconnect(close(1000, 'session ended', true))).toBe(false)
    expect(shouldReconnect(close(1006))).toBe(true)
    expect(shouldReconnect(close(1001, 'going away', true))).toBe(true)
    expect(shouldReconnect(close(1005))).toBe(true)
    expect(shouldReconnect(close(1003, 'malformed frame', true))).toBe(true)
  })
})

describe('connecting', () => {
  it('is not ready until the socket opens, and drops what is typed meanwhile', () => {
    const { term, statuses } = makeSession()
    term.start()
    expect(term.status.phase).toBe('connecting')

    expect(term.write('ls\n')).toBe(false)
    expect(term.status.inputDropped).toBe(true)
    expect(statuses.at(-1)?.inputDropped).toBe(true)

    MockWebSocket.last.open()
    expect(term.status.phase).toBe('ready')
    // The crux: nothing typed at a socket that was not ready is replayed.
    expect(data(MockWebSocket.last)).toEqual([])
    expect(term.status.inputDropped).toBe(false)
  })

  it('refuses input on a socket that is open but not yet positioned', () => {
    // Deliberately stricter than "the socket would take it". This state is not
    // reachable through a browser WebSocket, which flips readyState and fires
    // `open` in one task -- it is forced here to pin the gate on *ready*
    // rather than on writability, because "writable" is true a moment before
    // the select that decides which pane those bytes land in.
    const { term } = makeSession()
    term.start()
    const ws = MockWebSocket.last
    ws.readyState = 1
    expect(term.write('ls\n')).toBe(false)
    expect(data(ws)).toEqual([])
    ws.open()
    expect(data(ws)).toEqual([])
  })

  it('sends keystrokes once ready', () => {
    const { term } = makeSession()
    term.start()
    MockWebSocket.last.open()
    expect(term.write('ls\n')).toBe(true)
    expect(data(MockWebSocket.last).map((f) => f.text)).toEqual(['ls\n'])
  })

  it('hands PTY bytes to the terminal', () => {
    const { term, received } = makeSession()
    term.start()
    const ws = MockWebSocket.last
    ws.open()
    ws.receive(new Uint8Array([FRAME_DATA, 0x68, 0x69]))
    expect(received.map((b) => new TextDecoder().decode(b))).toEqual(['hi'])
  })
})

describe('resize', () => {
  it('sends one frame per burst, with the last size, after the debounce', () => {
    const { term } = makeSession()
    term.start()
    const ws = MockWebSocket.last
    ws.open()

    for (const cols of [80, 90, 100, 110]) term.noteResize(cols, 24)
    vi.advanceTimersByTime(RESIZE_DEBOUNCE_MS - 1)
    // tmux sizes a window to its most recently active client: an intermediate
    // size sent here would yank the user's own local attach mid-drag.
    expect(types(ws)).toEqual([])

    vi.advanceTimersByTime(1)
    expect(controls(ws).map((f) => f.json)).toEqual([{ type: 'resize', cols: 110, rows: 24 }])
  })

  it('does not repeat a size the socket already has', () => {
    const { term } = makeSession()
    term.start()
    const ws = MockWebSocket.last
    ws.open()

    term.noteResize(100, 40)
    vi.advanceTimersByTime(RESIZE_DEBOUNCE_MS)
    term.noteResize(100, 40)
    vi.advanceTimersByTime(RESIZE_DEBOUNCE_MS)
    expect(controls(ws)).toHaveLength(1)

    term.noteResize(101, 40)
    vi.advanceTimersByTime(RESIZE_DEBOUNCE_MS)
    expect(controls(ws)).toHaveLength(2)
  })

  it('ignores a zero size, which is a terminal that has not laid out', () => {
    const { term } = makeSession()
    term.start()
    const ws = MockWebSocket.last
    ws.open()
    term.noteResize(0, 0)
    term.noteResize(80, 0)
    vi.advanceTimersByTime(RESIZE_DEBOUNCE_MS)
    expect(types(ws)).toEqual([])
  })

  it('does not let a zero size overwrite the last real one', () => {
    const { term } = makeSession()
    term.start()
    const first = MockWebSocket.last
    first.open()
    term.noteResize(120, 50)
    vi.advanceTimersByTime(RESIZE_DEBOUNCE_MS)

    // wterm reports 0x0 while the container is detached -- which is exactly
    // what happens around a re-layout. Taking it would leave the pane at the
    // server's initial 80x24 after the next reconnect.
    term.noteResize(0, 0)
    vi.advanceTimersByTime(RESIZE_DEBOUNCE_MS)

    first.emitClose(1006)
    vi.advanceTimersByTime(BACKOFF_BASE_MS)
    const second = MockWebSocket.last
    second.open()
    expect(controls(second).map((f) => f.json)).toEqual([{ type: 'resize', cols: 120, rows: 50 }])
  })

  it('re-sends the size on a new socket, which attaches at 80x24', () => {
    const { term } = makeSession()
    term.start()
    const first = MockWebSocket.last
    first.open()
    term.noteResize(120, 50)
    vi.advanceTimersByTime(RESIZE_DEBOUNCE_MS)
    expect(controls(first).map((f) => f.json)).toEqual([{ type: 'resize', cols: 120, rows: 50 }])

    first.emitClose(1006)
    vi.advanceTimersByTime(BACKOFF_BASE_MS)
    const second = MockWebSocket.last
    expect(second).not.toBe(first)
    second.open()
    expect(controls(second).map((f) => f.json)).toEqual([{ type: 'resize', cols: 120, rows: 50 }])
  })

  it('sends a size measured before the socket opened', () => {
    const { term } = makeSession()
    term.noteResize(90, 30)
    term.start()
    const ws = MockWebSocket.last
    // The debounce fires while the socket is still connecting: the frame is
    // dropped there and has to come back on open, not be lost.
    vi.advanceTimersByTime(RESIZE_DEBOUNCE_MS)
    expect(types(ws)).toEqual([])
    ws.open()
    expect(controls(ws).map((f) => f.json)).toEqual([{ type: 'resize', cols: 90, rows: 30 }])
  })
})

describe('position', () => {
  it('remembers the selected pane in storage, as a pane id', () => {
    const storage = memoryStorage()
    const { term } = makeSession({ storage })
    term.start()
    MockWebSocket.last.open()

    expect(term.select('%7')).toBe(true)
    expect(types(MockWebSocket.last)).toEqual(['select'])
    expect(storage.map.get('wterm-web:pane:work')).toBe('%7')
    expect(term.status.pane).toBe('%7')
  })

  it('refuses a target that is not a pane id', () => {
    const storage = memoryStorage()
    const { term } = makeSession({ storage })
    term.start()
    MockWebSocket.last.open()

    // `tmux select-pane -t work` exits 0 and moves the pane the user is in.
    expect(term.select('work')).toBe(false)
    expect(types(MockWebSocket.last)).toEqual([])
    expect(storage.map.size).toBe(0)
  })

  it('restores the remembered pane on a fresh session object', () => {
    const storage = memoryStorage({ 'wterm-web:pane:work': '%4' })
    const { term } = makeSession({ storage })
    expect(term.status.pane).toBe('%4')
    term.start()
    MockWebSocket.last.open()
    expect(controls(MockWebSocket.last).map((f) => f.json)).toEqual([{ type: 'select', pane: '%4' }])
  })

  it('ignores a stored value that is not a pane id', () => {
    const storage = memoryStorage({ 'wterm-web:pane:work': 'work:1.0' })
    const { term } = makeSession({ storage })
    expect(term.status.pane).toBeNull()
    term.start()
    MockWebSocket.last.open()
    expect(types(MockWebSocket.last)).toEqual([])
  })

  it('re-selects the remembered pane on every reconnect', () => {
    const { term } = makeSession()
    term.start()
    const first = MockWebSocket.last
    first.open()
    term.select('%9')

    first.emitClose(1006)
    vi.advanceTimersByTime(BACKOFF_BASE_MS)
    const second = MockWebSocket.last
    second.open()
    // Without this the tab silently lands on the group's active window --
    // window 0 -- after every blip.
    expect(controls(second).map((f) => f.json)).toEqual([{ type: 'select', pane: '%9' }])
  })

  it('keeps a pane selected while disconnected and applies it on reopen', () => {
    const { term } = makeSession()
    term.start()
    const first = MockWebSocket.last
    first.open()
    first.emitClose(1006)

    expect(term.select('%5')).toBe(true)
    vi.advanceTimersByTime(BACKOFF_BASE_MS)
    const second = MockWebSocket.last
    second.open()
    expect(controls(second).map((f) => f.json)).toEqual([{ type: 'select', pane: '%5' }])
  })

  it('puts the select on the wire before any input can be', () => {
    // The whole point of "enable input only after the select". A keystroke that
    // overtook it would be typed into whatever pane the new throwaway session
    // landed on -- someone else's agent.
    let opens = 0
    let wasReady = false
    const { term } = makeSession({
      onStatus: (s) => {
        // Types the instant input is declared live, which is the earliest a
        // real keystroke could arrive.
        const becameReady = s.phase === 'ready' && !wasReady
        wasReady = s.phase === 'ready'
        if (becameReady && ++opens === 2) term.write('rm -rf /\n')
      },
    })
    term.start()
    const first = MockWebSocket.last
    first.open()
    term.select('%2')
    term.noteResize(100, 40)
    vi.advanceTimersByTime(RESIZE_DEBOUNCE_MS)

    first.emitClose(1006)
    vi.advanceTimersByTime(BACKOFF_BASE_MS)
    const second = MockWebSocket.last
    second.open()

    expect(frames(second).map((f) => (f.kind === FRAME_CONTROL ? f.json?.type : 'data'))).toEqual([
      'resize',
      'select',
      'data',
    ])
  })

  it('sends copy-mode only while ready', () => {
    const { term } = makeSession()
    term.start()
    expect(term.copyMode()).toBe(false)
    const ws = MockWebSocket.last
    ws.open()
    expect(term.copyMode()).toBe(true)
    expect(term.copyMode('%3')).toBe(true)
    expect(controls(ws).map((f) => f.json)).toEqual([
      { type: 'copy-mode' },
      { type: 'copy-mode', pane: '%3' },
    ])
  })
})

describe('reconnect', () => {
  it('waits out the backoff, then opens exactly one new socket', () => {
    const { term } = makeSession()
    term.start()
    MockWebSocket.last.open()
    MockWebSocket.last.emitClose(1006)
    expect(term.status.phase).toBe('reconnecting')
    expect(term.status.retryDelayMs).toBe(BACKOFF_BASE_MS)

    vi.advanceTimersByTime(BACKOFF_BASE_MS - 1)
    expect(MockWebSocket.instances).toHaveLength(1)
    vi.advanceTimersByTime(1)
    expect(MockWebSocket.instances).toHaveLength(2)
  })

  it('doubles the delay per consecutive failure and resets it on success', () => {
    const { term } = makeSession()
    term.start()
    const delays: number[] = []
    for (let i = 0; i < 3; i++) {
      MockWebSocket.last.emitClose(1006)
      delays.push(term.status.retryDelayMs)
      vi.advanceTimersByTime(term.status.retryDelayMs)
    }
    expect(delays).toEqual([500, 1000, 2000])
    expect(term.status.attempt).toBe(3)

    MockWebSocket.last.open()
    expect(term.status.attempt).toBe(0)
    MockWebSocket.last.emitClose(1006)
    expect(term.status.retryDelayMs).toBe(BACKOFF_BASE_MS)
  })

  it('does not reconnect after tmux ended the session', () => {
    const { term } = makeSession()
    term.start()
    MockWebSocket.last.open()
    MockWebSocket.last.emitClose(1000, 'session ended', true)

    expect(term.status.phase).toBe('ended')
    vi.advanceTimersByTime(60_000)
    // A reconnect here would quietly create a second tmux session behind a
    // user who just typed `exit`.
    expect(MockWebSocket.instances).toHaveLength(1)
  })

  it('reconnects from ended only when the user asks', () => {
    const { term } = makeSession()
    term.start()
    MockWebSocket.last.open()
    MockWebSocket.last.emitClose(1000, 'session ended', true)
    term.retryNow()
    expect(MockWebSocket.instances).toHaveLength(2)
    expect(term.status.phase).toBe('connecting')
  })

  it('retries immediately when asked, without leaving the timer armed', () => {
    const { term } = makeSession()
    term.start()
    MockWebSocket.last.emitClose(1006)
    term.retryNow()
    expect(MockWebSocket.instances).toHaveLength(2)
    vi.advanceTimersByTime(60_000)
    expect(MockWebSocket.instances).toHaveLength(2)
  })

  it('ignores a late close from a socket it already replaced', () => {
    const { term } = makeSession()
    term.start()
    const first = MockWebSocket.last
    first.emitClose(1006)
    vi.advanceTimersByTime(BACKOFF_BASE_MS)
    const second = MockWebSocket.last
    second.open()

    // A duplicated reconnect chain means two throwaway tmux sessions racing to
    // be this tab's.
    first.emitClose(1006)
    expect(term.status.phase).toBe('ready')
    vi.advanceTimersByTime(60_000)
    expect(MockWebSocket.instances).toHaveLength(2)
  })

  it('ignores data from a socket it already replaced', () => {
    const { term, received } = makeSession()
    term.start()
    const first = MockWebSocket.last
    first.emitClose(1006)
    vi.advanceTimersByTime(BACKOFF_BASE_MS)
    MockWebSocket.last.open()

    first.readyState = 1
    first.receive(new Uint8Array([FRAME_DATA, 0x78]))
    expect(received).toEqual([])
  })
})

describe('stop', () => {
  it('closes the socket and never reconnects', () => {
    const { term } = makeSession()
    term.start()
    const ws = MockWebSocket.last
    ws.open()
    term.stop()

    expect(ws.closedWith?.code).toBe(1000)
    expect(term.status.phase).toBe('closed')
    ws.emitClose(1006)
    vi.advanceTimersByTime(60_000)
    expect(MockWebSocket.instances).toHaveLength(1)
  })

  it('cancels a pending reconnect', () => {
    const { term } = makeSession()
    term.start()
    MockWebSocket.last.emitClose(1006)
    term.stop()
    vi.advanceTimersByTime(60_000)
    expect(MockWebSocket.instances).toHaveLength(1)
  })

  it('cancels a pending resize', () => {
    const { term } = makeSession()
    term.start()
    const ws = MockWebSocket.last
    ws.open()
    term.noteResize(120, 50)
    term.stop()
    vi.advanceTimersByTime(RESIZE_DEBOUNCE_MS)
    expect(types(ws)).toEqual([])
  })

  it('refuses input after stopping', () => {
    const { term } = makeSession()
    term.start()
    MockWebSocket.last.open()
    term.stop()
    expect(term.write('x')).toBe(false)
  })
})

describe('storage failures', () => {
  it('works with no storage at all', () => {
    const { term } = makeSession({ storage: null })
    term.start()
    MockWebSocket.last.open()
    expect(term.select('%3')).toBe(true)
    expect(term.status.pane).toBe('%3')
  })

  it('survives a storage that throws, as a private window does', () => {
    const throwing: PaneStorage = {
      getItem() {
        throw new Error('SecurityError')
      },
      setItem() {
        throw new Error('QuotaExceededError')
      },
      removeItem() {},
    }
    const { term } = makeSession({ storage: throwing })
    expect(term.status.pane).toBeNull()
    term.start()
    MockWebSocket.last.open()
    expect(term.select('%3')).toBe(true)
  })
})
