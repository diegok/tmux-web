import { renderToStaticMarkup } from 'react-dom/server'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { FRAME_CONTROL, FRAME_DATA } from '@/lib/transport'
import { SNAPSHOT_URL } from '@/lib/useSnapshot'
import type { SnapshotRow } from '@/lib/useSnapshot'

import {
  BACKOFF_BASE_MS,
  BACKOFF_MAX_MS,
  ConnectionPill,
  RESIZE_DEBOUNCE_MS,
  SESSION_PROBE_TIMEOUT_MS,
  TerminalSession,
  backoffDelay,
  isPaneId,
  paneStorageKey,
  probeSession,
  shouldReconnect,
  snapshotHasSession,
  terminalUrl,
} from './Terminal'
import type { PaneStorage, SessionPresence, TerminalStatus } from './Terminal'

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

/**
 * Drain the microtask queue.
 *
 * The probe's result reaches the session through `.catch().then()`, which is
 * two hops past the resolve; a handful of turns is plenty and, unlike a timer,
 * is unaffected by `vi.useFakeTimers`.
 */
async function flush() {
  for (let i = 0; i < 5; i++) await Promise.resolve()
}

/**
 * A presence probe whose answers the test controls.
 *
 * Deliberately never auto-resolving: every existing test in this file runs with
 * an unanswered probe, which is exactly the "the daemon has not said anything
 * yet" case, and proves the reconnect behaviour they pin does not depend on one.
 */
function makeProbe() {
  interface Deferred {
    resolve: (p: SessionPresence) => void
    reject: (e: Error) => void
  }
  const pending: Deferred[] = []
  let calls = 0
  const take = (): Deferred => {
    const next = pending.shift()
    if (!next) throw new Error('no probe is outstanding')
    return next
  }
  return {
    probe: () => {
      calls++
      return new Promise<SessionPresence>((resolve, reject) => pending.push({ resolve, reject }))
    },
    get calls() {
      return calls
    },
    get outstanding() {
      return pending.length
    },
    /** Answer the oldest outstanding probe and let the session react. */
    async answer(presence: SessionPresence) {
      take().resolve(presence)
      await flush()
    },
    /** Reject it instead, as an injected probe with a bug would. */
    async fail() {
      take().reject(new Error('boom'))
      await flush()
    },
  }
}

function makeSession(
  opts: {
    storage?: PaneStorage | null
    onStatus?: (s: TerminalStatus) => void
    probe?: () => Promise<SessionPresence>
  } = {},
) {
  const statuses: TerminalStatus[] = []
  const received: Uint8Array[] = []
  const storage = opts.storage === undefined ? memoryStorage() : opts.storage
  const probe = makeProbe()
  const term = new TerminalSession({
    url: 'ws://localhost/ws?session=work',
    session: 'work',
    paneKey: paneStorageKey('work'),
    onData: (bytes) => void received.push(bytes),
    onStatus: (s) => {
      statuses.push(s)
      opts.onStatus?.(s)
    },
    storage,
    // Neutral jitter, so delays are exactly the exponential schedule.
    random: () => 0.5,
    probe: opts.probe ?? probe.probe,
  })
  return { term, statuses, received, storage, probe }
}

/** A snapshot row, with only the fields this file cares about spelled out. */
function row(over: Partial<SnapshotRow> & Pick<SnapshotRow, 'sessionName'>): SnapshotRow {
  return {
    // Defaults to the session name, as tmux does for an ungrouped session.
    groupKey: over.sessionName,
    sessionId: '$1',
    paneId: '%1',
    paneIndex: 0,
    appOwned: false,
    label: '',
    windowIndex: 0,
    windowName: 'w',
    paneActive: true,
    command: 'zsh',
    title: 't',
    ...over,
  }
}

/** A `fetch` that answers `/api/snapshot` with this body. */
function snapshotFetch(body: unknown, status = 200) {
  const calls: string[] = []
  const fetchImpl = (url: string) => {
    calls.push(url)
    return Promise.resolve({
      ok: status >= 200 && status < 300,
      status,
      json: () => Promise.resolve(body),
    } as Response)
  }
  return { fetchImpl, calls }
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

describe('session presence probe', () => {
  it('matches the live session name, never the frozen group key', () => {
    // tmux keeps session_group at the group's *pre-rename* name forever, so a
    // group whose namesake died still carries rows keyed "work" -- the app's
    // own `_web-` members. `has-session -t =work` fails against exactly that,
    // so reading the group key here would report a dead session as present and
    // leave the tab retrying against a 404 for good.
    const rows = [row({ sessionName: '_web-abcd', groupKey: 'work', appOwned: true })]
    expect(snapshotHasSession(rows, 'work')).toBe(false)
    expect(snapshotHasSession(rows, '_web-abcd')).toBe(true)
  })

  it('finds the session among other groups', () => {
    const rows = [row({ sessionName: '0' }), row({ sessionName: 'work' })]
    expect(snapshotHasSession(rows, 'work')).toBe(true)
    expect(snapshotHasSession(rows, 'wor')).toBe(false) // no prefix matching, as `=` pins
    expect(snapshotHasSession([], 'work')).toBe(false)
  })

  it('reads a fresh snapshot as present or gone', async () => {
    const present = snapshotFetch({ panes: [row({ sessionName: 'work' })], stale: false })
    await expect(probeSession('work', present.fetchImpl)).resolves.toBe('present')
    expect(present.calls).toEqual([SNAPSHOT_URL])

    const gone = snapshotFetch({ panes: [row({ sessionName: '0' })], stale: false })
    await expect(probeSession('work', gone.fetchImpl)).resolves.toBe('gone')
  })

  it('refuses to condemn a session on a stale snapshot', async () => {
    // Stale means the daemon's own poll of tmux failed and it is serving the
    // last good rows -- possibly for the same reason the socket dropped.
    const { fetchImpl } = snapshotFetch({ panes: [row({ sessionName: '0' })], stale: true })
    await expect(probeSession('work', fetchImpl)).resolves.toBe('unknown')
  })

  it('answers unknown for every way of not knowing', async () => {
    const rejecting = () => Promise.reject(new Error('network down'))
    await expect(probeSession('work', rejecting)).resolves.toBe('unknown')

    // 401: the device cookie expired. Not a dead tmux session.
    const unauthorized = snapshotFetch({}, 401)
    await expect(probeSession('work', unauthorized.fetchImpl)).resolves.toBe('unknown')

    const nonsense = snapshotFetch({ panes: 'not-a-list' })
    await expect(probeSession('work', nonsense.fetchImpl)).resolves.toBe('unknown')

    const throwing = () => {
      throw new Error('sync throw')
    }
    await expect(probeSession('work', throwing)).resolves.toBe('unknown')
  })

  it('abandons a request that never settles', async () => {
    let aborted = false
    const hanging = (_url: string, init: RequestInit) =>
      new Promise<Response>((_resolve, reject) => {
        init.signal?.addEventListener('abort', () => {
          aborted = true
          reject(new Error('AbortError'))
        })
      })

    const result = probeSession('work', hanging)
    vi.advanceTimersByTime(SESSION_PROBE_TIMEOUT_MS)
    await expect(result).resolves.toBe('unknown')
    expect(aborted).toBe(true)
  })
})

describe('the session is gone', () => {
  it('stops retrying, cancelling the retry that was already scheduled', async () => {
    const { term, statuses, probe } = makeSession()
    term.start()
    MockWebSocket.last.open()
    MockWebSocket.last.emitClose(1006)
    expect(term.status.phase).toBe('reconnecting')

    await probe.answer('gone')
    expect(term.status.phase).toBe('gone')
    expect(term.status.retryDelayMs).toBe(0)
    // Announced, not merely readable: the pill and the sidebar only ever learn
    // about this through onStatus, so a transition that does not emit is a
    // terminal that dims and says nothing.
    expect(statuses.at(-1)?.phase).toBe('gone')

    vi.advanceTimersByTime(60_000)
    expect(MockWebSocket.instances).toHaveLength(1)
  })

  it('keeps retrying after an ordinary drop', async () => {
    // The worse of the two failures: a tab that stops reconnecting after a wifi
    // handover looks exactly like a dead agent and needs a reload to recover.
    const { term, probe } = makeSession()
    term.start()
    MockWebSocket.last.open()
    MockWebSocket.last.emitClose(1006)

    await probe.answer('present')
    expect(term.status.phase).toBe('reconnecting')
    vi.advanceTimersByTime(BACKOFF_BASE_MS)
    expect(MockWebSocket.instances).toHaveLength(2)
  })

  it('keeps retrying when the probe could not find out', async () => {
    const { term, probe } = makeSession()
    term.start()
    MockWebSocket.last.emitClose(1006)

    await probe.answer('unknown')
    expect(term.status.phase).toBe('reconnecting')
    vi.advanceTimersByTime(BACKOFF_BASE_MS)
    expect(MockWebSocket.instances).toHaveLength(2)
  })

  it('keeps retrying when the probe itself throws', async () => {
    const { term, probe } = makeSession()
    term.start()
    MockWebSocket.last.emitClose(1006)

    await probe.fail()
    expect(term.status.phase).toBe('reconnecting')
    vi.advanceTimersByTime(BACKOFF_BASE_MS)
    expect(MockWebSocket.instances).toHaveLength(2)
  })

  it('does not wait for the probe before reconnecting', async () => {
    // The probe runs beside the backoff, not in front of it, so a blip
    // reconnects on exactly the schedule it did before any of this existed.
    const { term } = makeSession()
    term.start()
    MockWebSocket.last.emitClose(1006)
    vi.advanceTimersByTime(BACKOFF_BASE_MS)
    expect(MockWebSocket.instances).toHaveLength(2)
    expect(term.status.phase).toBe('reconnecting')
  })

  it('closes a socket the backoff already opened', async () => {
    const { term, probe } = makeSession()
    term.start()
    MockWebSocket.last.emitClose(1006)
    vi.advanceTimersByTime(BACKOFF_BASE_MS)
    const second = MockWebSocket.last
    expect(MockWebSocket.instances).toHaveLength(2)

    // The answer lands while the second socket is still handshaking. Left open
    // it would be refused by the daemon, and that refusal would arrive as a
    // fresh close and start the loop over.
    await probe.answer('gone')
    expect(term.status.phase).toBe('gone')
    expect(second.closedWith).toEqual({ code: 1000, reason: 'session gone' })
    vi.advanceTimersByTime(60_000)
    expect(MockWebSocket.instances).toHaveLength(2)
  })

  it('discards an answer that lost the race to a socket opening', async () => {
    // An open is proof: the daemon runs `has-session` before it upgrades. A
    // snapshot taken before the session came back must not strand a live tab.
    const { term, probe } = makeSession()
    term.start()
    MockWebSocket.last.emitClose(1006)
    vi.advanceTimersByTime(BACKOFF_BASE_MS)
    MockWebSocket.last.open()
    expect(term.status.phase).toBe('ready')

    await probe.answer('gone')
    expect(term.status.phase).toBe('ready')
    expect(term.write('x')).toBe(true)
  })

  it('upgrades a clean "session ended" close to gone', async () => {
    // Killing the base session's last window destroys the whole group, so the
    // tab sees the same 1000 it sees when the user typed `exit`. Only the probe
    // tells those apart, and they deserve different words and a different button.
    const { term, probe } = makeSession()
    term.start()
    MockWebSocket.last.open()
    MockWebSocket.last.emitClose(1000, 'session ended', true)
    expect(term.status.phase).toBe('ended')

    await probe.answer('gone')
    expect(term.status.phase).toBe('gone')
  })

  it('leaves a plain exit as ended', async () => {
    const { term, probe } = makeSession()
    term.start()
    MockWebSocket.last.open()
    MockWebSocket.last.emitClose(1000, 'session ended', true)

    await probe.answer('present')
    expect(term.status.phase).toBe('ended')
  })

  it('refuses input while gone', async () => {
    const { term, probe } = makeSession()
    term.start()
    MockWebSocket.last.open()
    MockWebSocket.last.emitClose(1006)
    await probe.answer('gone')

    expect(term.write('x')).toBe(false)
    expect(term.status.inputDropped).toBe(true)
  })

  it('reconnects from gone when the user asks, and can go gone again', async () => {
    // A tmux session name can be reused, so this state has to be escapable --
    // and having escaped it, the tab has to be able to re-enter it.
    const { term, probe } = makeSession()
    term.start()
    MockWebSocket.last.emitClose(1006)
    await probe.answer('gone')
    expect(term.status.phase).toBe('gone')

    term.retryNow()
    expect(MockWebSocket.instances).toHaveLength(2)
    expect(term.status.phase).toBe('connecting')
    MockWebSocket.last.open()
    expect(term.status.phase).toBe('ready')

    MockWebSocket.last.emitClose(1006)
    expect(probe.calls).toBe(2)
    await probe.answer('gone')
    expect(term.status.phase).toBe('gone')
  })

  it('keeps only one probe in flight while a socket flaps', async () => {
    const { term, probe } = makeSession()
    term.start()
    MockWebSocket.last.emitClose(1006)
    vi.advanceTimersByTime(BACKOFF_BASE_MS)
    MockWebSocket.last.emitClose(1006)
    vi.advanceTimersByTime(BACKOFF_BASE_MS * 2)
    MockWebSocket.last.emitClose(1006)

    expect(probe.calls).toBe(1)
    expect(probe.outstanding).toBe(1)
    await probe.answer('unknown')

    // ...and the next drop asks again, rather than never asking twice.
    vi.advanceTimersByTime(60_000)
    MockWebSocket.last.emitClose(1006)
    expect(probe.calls).toBe(2)
    await probe.answer('gone')
    expect(term.status.phase).toBe('gone')
  })

  it('ignores an answer that arrives after stop', async () => {
    const { term, probe } = makeSession()
    term.start()
    MockWebSocket.last.emitClose(1006)
    term.stop()

    await probe.answer('gone')
    expect(term.status.phase).toBe('closed')
  })

  it('wires the default probe to /api/snapshot, asking about its own session', async () => {
    // No `probe` option: this is the wiring the app actually ships, and the one
    // place the session *name* has to reach the snapshot query. Both directions
    // are asserted, because a probe that asked about the wrong name -- or about
    // nothing at all -- would still answer "gone" for a snapshot that happens
    // not to contain it, and would look correct from the failing side alone.
    const dropAndProbe = async (panes: SnapshotRow[]) => {
      const { fetchImpl, calls } = snapshotFetch({ panes, stale: false })
      vi.stubGlobal('fetch', fetchImpl)
      const term = new TerminalSession({
        url: 'ws://localhost/ws?session=work',
        session: 'work',
        paneKey: paneStorageKey('work'),
        onData: () => {},
        onStatus: () => {},
        storage: null,
        random: () => 0.5,
      })
      term.start()
      MockWebSocket.last.emitClose(1006)
      await flush()
      expect(calls).toEqual([SNAPSHOT_URL])
      return term
    }

    const gone = await dropAndProbe([row({ sessionName: '0' })])
    expect(gone.status.phase).toBe('gone')
    gone.stop()

    const alive = await dropAndProbe([row({ sessionName: '0' }), row({ sessionName: 'work' })])
    expect(alive.status.phase).toBe('reconnecting')
    alive.stop()
  })
})

describe('the pill', () => {
  const pill = (status: Partial<TerminalStatus>, session = 'work') =>
    renderToStaticMarkup(
      <ConnectionPill
        status={{
          phase: 'connecting',
          attempt: 0,
          retryDelayMs: 0,
          pane: null,
          inputDropped: false,
          ...status,
        }}
        session={session}
        onRetry={() => {}}
      />,
    )

  it('names the session and points at the sidebar when it is gone', () => {
    const html = pill({ phase: 'gone' }, 'work')
    expect(html).toContain('Session &quot;work&quot; is gone')
    expect(html).toContain('sidebar')
    // Not "Reconnecting": the whole point is that this tab stopped.
    expect(html).not.toContain('Reconnecting')
    // Still a way back, because the name can be reused.
    expect(html).toContain('Try again')
  })

  it('does not pulse a dot at a session that will not come back', () => {
    expect(pill({ phase: 'gone' })).not.toContain('animate-pulse')
    expect(pill({ phase: 'reconnecting', attempt: 2 })).toContain('animate-pulse')
  })

  it('still says the other phases', () => {
    expect(pill({ phase: 'ended' })).toContain('Session ended')
    expect(pill({ phase: 'reconnecting', attempt: 2 })).toContain('Reconnecting (2)')
    expect(pill({ phase: 'ready' })).toBe('')
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
