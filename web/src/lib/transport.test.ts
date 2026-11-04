import { readFileSync } from 'node:fs'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { PANE_MESSAGE, parsePaneMessage } from '@/components/Terminal'

import {
  ACK_MESSAGE,
  FRAME_CONTROL,
  FRAME_DATA,
  MAX_DATA_PAYLOAD,
  Transport,
  UNACKED_LIMIT_BYTES,
} from './transport'
import type { ControlMessage, TransportClose, TransportOptions } from './transport'

// --- a WebSocket the tests drive by hand ------------------------------------
//
// Modelled closely enough on the real thing to be worth trusting: close() goes
// through CLOSING rather than firing onclose synchronously, and send() throws
// on a socket that has not opened yet, the way a browser does. That last one is
// load-bearing -- it means dropping the guard in #write shows up as an
// exception rather than as a silently missing assertion.

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
  readyState = 0 // CONNECTING
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
    if (this.readyState === 0) throw new Error('InvalidStateError: still CONNECTING')
    if (this.readyState !== 1) return // a browser discards these silently
    this.sent.push(new Uint8Array(data))
  }

  close(code = 1000, reason = '') {
    this.closedWith = { code, reason }
    this.readyState = 2 // CLOSING; onclose arrives later, if at all
  }

  // --- test drivers ---

  open() {
    this.readyState = 1
    this.onopen?.()
  }

  /** Deliver a whole frame, prefix included, the way the server sends it. */
  receive(frame: Uint8Array) {
    const buf = frame.buffer.slice(frame.byteOffset, frame.byteOffset + frame.byteLength)
    this.onmessage?.({ data: buf } as MessageEvent)
  }

  receiveRaw(data: unknown) {
    this.onmessage?.({ data } as MessageEvent)
  }

  emitClose(code = 1006, reason = '', wasClean = false) {
    this.readyState = 3
    this.onclose?.({ code, reason, wasClean } as CloseEvent)
  }

  emitError() {
    this.onerror?.(new Event('error'))
  }
}

function makeTransport(overrides: Partial<TransportOptions> = {}) {
  const onData = vi.fn()
  const onControl = vi.fn()
  const onOpen = vi.fn()
  const onClose = vi.fn()
  const onError = vi.fn()
  const transport = new Transport({
    url: 'ws://localhost/ws?session=work',
    onData,
    onControl,
    onOpen,
    onClose,
    onError,
    ...overrides,
  })
  return { transport, ws: MockWebSocket.last, onData, onControl, onOpen, onClose, onError }
}

function frame(kind: number, payload: Uint8Array | string): Uint8Array {
  const bytes = typeof payload === 'string' ? new TextEncoder().encode(payload) : payload
  const out = new Uint8Array(bytes.length + 1)
  out[0] = kind
  out.set(bytes, 1)
  return out
}

const text = (bytes: Uint8Array) => new TextDecoder().decode(bytes)

beforeEach(() => {
  MockWebSocket.reset()
  vi.stubGlobal('WebSocket', MockWebSocket)
  vi.spyOn(console, 'warn').mockImplementation(() => {})
})

afterEach(() => {
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
  vi.useRealTimers()
})

// --- the cross-language wire contract ---------------------------------------
//
// The protocol is implemented twice, in two languages, from one specification
// that lives in neither. These tests read the Go source so that a divergence is
// a failing `pnpm test` naming the file to look at, rather than a terminal that
// mysteriously stops resizing. The Go side pins the same bytes from its own
// direction in TestFrameWireBytes.

const goSource = (path: string) =>
  readFileSync(new URL(`../../../${path}`, import.meta.url), 'utf8')

describe('wire contract with the Go implementation', () => {
  it('uses the frame prefixes declared in internal/ptybridge/frame.go', () => {
    const src = goSource('internal/ptybridge/frame.go')
    const constant = (name: string) => {
      const m = src.match(new RegExp(`${name}\\s+byte\\s*=\\s*(0x[0-9a-fA-F]+)`))
      if (!m) throw new Error(`${name} not found in internal/ptybridge/frame.go`)
      return Number.parseInt(m[1], 16)
    }
    expect(FRAME_DATA).toBe(constant('FrameData'))
    expect(FRAME_CONTROL).toBe(constant('FrameControl'))
  })

  it('pins the prefix bytes literally, so a coordinated rename is still a review', () => {
    expect(FRAME_DATA).toBe(0x00)
    expect(FRAME_CONTROL).toBe(0x01)
    expect(FRAME_DATA).not.toBe(FRAME_CONTROL)
  })

  it('emits only field names that wsControlMessage in internal/front/ws.go decodes', () => {
    const src = goSource('internal/front/ws.go')
    const struct = src.match(/type wsControlMessage struct \{([\s\S]*?)\n\}/)
    if (!struct) throw new Error('wsControlMessage not found in internal/front/ws.go')
    const fields = new Set([...struct[1].matchAll(/`json:"([^"]+)"`/g)].map((m) => m[1]))
    expect(fields).toEqual(new Set(['type', 'cols', 'rows', 'pane']))

    const { transport, ws } = makeTransport()
    ws.open()
    transport.resize(120, 40)
    transport.select('%3')
    transport.copyMode('%7')
    transport.copyMode()
    transport.endMode('%3')
    transport.where()
    for (const sentFrame of ws.sent) {
      for (const key of Object.keys(JSON.parse(text(sentFrame.subarray(1))))) {
        expect(fields).toContain(key)
      }
    }
  })

  // The one message that travels the other way. Both halves are pinned: the
  // literal the daemon marshals, and the literal the browser matches on. A
  // rename on one side only leaves a tab that never learns which pane it is
  // showing -- silently, because an unrecognised control message is dropped.
  it('reads the daemon pane message by the type wsPaneType declares', () => {
    const src = goSource('internal/front/ws.go')
    const m = src.match(/const wsPaneType = "([^"]+)"/)
    if (!m) throw new Error('wsPaneType not found in internal/front/ws.go')
    expect(PANE_MESSAGE).toBe(m[1])
    expect(PANE_MESSAGE).toBe('pane')

    // And the field it carries is one the browser reads off the same struct.
    const struct = src.match(/type wsPaneMessage struct \{([\s\S]*?)\n\}/)
    if (!struct) throw new Error('wsPaneMessage not found in internal/front/ws.go')
    const fields = new Set([...struct[1].matchAll(/`json:"([^"]+)"`/g)].map((m) => m[1]))
    expect(fields).toEqual(new Set(['type', 'pane']))
    expect(parsePaneMessage({ type: PANE_MESSAGE, pane: '%3' })).toBe('%3')
  })

  // The other message that travels this way, and the one the input journal
  // depends on: a rename on one side only would leave a tab that frees nothing,
  // so every keystroke it ever sent would still read as unconfirmed.
  it('reads the daemon ack by the type wsAckType declares', () => {
    const src = goSource('internal/front/ws.go')
    const m = src.match(/const wsAckType = "([^"]+)"/)
    if (!m) throw new Error('wsAckType not found in internal/front/ws.go')
    expect(ACK_MESSAGE).toBe(m[1])
    expect(ACK_MESSAGE).toBe('ack')

    const struct = src.match(/type wsAckMessage struct \{([\s\S]*?)\n\}/)
    if (!struct) throw new Error('wsAckMessage not found in internal/front/ws.go')
    const fields = new Set([...struct[1].matchAll(/`json:"([^"]+)"`/g)].map((f) => f[1]))
    expect(fields).toEqual(new Set(['type', 'bytes']))
  })

  it('emits only message types the Go control switch handles', () => {
    const src = goSource('internal/front/ws.go')
    const handled = new Set([...src.matchAll(/\n\tcase "([^"]+)":/g)].map((m) => m[1]))
    expect(handled).toEqual(new Set(['resize', 'select', 'copy-mode', 'end-mode', 'where']))

    const { transport, ws } = makeTransport()
    ws.open()
    transport.resize(80, 24)
    transport.select('%3')
    transport.copyMode()
    transport.endMode('%3')
    transport.where()
    for (const sentFrame of ws.sent) {
      expect(handled).toContain(JSON.parse(text(sentFrame.subarray(1))).type)
    }
  })
})

// --- outbound ---------------------------------------------------------------

describe('outgoing frames', () => {
  it('asks for arraybuffer messages rather than Blobs', () => {
    const { ws } = makeTransport()
    expect(ws.binaryType).toBe('arraybuffer')
  })

  it('prefixes PTY input with 0x00 and sends it as bytes', () => {
    const { transport, ws } = makeTransport()
    ws.open()
    expect(transport.send('ls\r')).toBe(true)
    expect(ws.sent).toHaveLength(1)
    expect(ws.sent[0][0]).toBe(0x00)
    expect(text(ws.sent[0].subarray(1))).toBe('ls\r')
  })

  it('encodes string input as UTF-8, not as one byte per code unit', () => {
    const { transport, ws } = makeTransport()
    ws.open()
    transport.send('é')
    expect([...ws.sent[0]]).toEqual([0x00, 0xc3, 0xa9])
  })

  it('passes Uint8Array input through unchanged behind the prefix', () => {
    const { transport, ws } = makeTransport()
    ws.open()
    transport.send(new Uint8Array([0x1b, 0x5b, 0x41]))
    expect([...ws.sent[0]]).toEqual([0x00, 0x1b, 0x5b, 0x41])
  })

  it('prefixes control messages with 0x01 and encodes them as JSON', () => {
    const { transport, ws } = makeTransport()
    ws.open()
    expect(transport.resize(120, 40)).toBe(true)
    expect(ws.sent[0][0]).toBe(0x01)
    expect(JSON.parse(text(ws.sent[0].subarray(1)))).toEqual({
      type: 'resize',
      cols: 120,
      rows: 40,
    })

    transport.select('%3')
    expect(ws.sent[1][0]).toBe(0x01)
    expect(JSON.parse(text(ws.sent[1].subarray(1)))).toEqual({ type: 'select', pane: '%3' })

    transport.copyMode('%7')
    expect(JSON.parse(text(ws.sent[2].subarray(1)))).toEqual({ type: 'copy-mode', pane: '%7' })

    // No pane means "the pane this tab is looking at"; the key is omitted
    // rather than sent as undefined or null, both of which JSON.stringify
    // would turn into something the Go decoder reads differently.
    transport.copyMode()
    expect(JSON.parse(text(ws.sent[3].subarray(1)))).toEqual({ type: 'copy-mode' })

    // end-mode is the mirror image: the pane is never optional, because the
    // daemon refuses an unnamed one rather than popping a mode off whatever
    // pane happens to be current. Asserted exactly, so a message that lost its
    // pane on the way to the wire is a failure here and not a reply that
    // silently lands in copy mode and gets truncated.
    transport.endMode('%3')
    expect(ws.sent[4][0]).toBe(0x01)
    expect(JSON.parse(text(ws.sent[4].subarray(1)))).toEqual({ type: 'end-mode', pane: '%3' })
  })

  it('never sends a text frame', () => {
    const { transport, ws } = makeTransport()
    ws.open()
    transport.send('hello')
    transport.resize(80, 24)
    for (const sentFrame of ws.sent) expect(sentFrame).toBeInstanceOf(Uint8Array)
  })

  it('splits a paste too large for the server read limit', () => {
    const { transport, ws } = makeTransport()
    ws.open()
    const big = new Uint8Array(MAX_DATA_PAYLOAD + 100).fill(0x61)
    expect(transport.send(big)).toBe(true)
    expect(ws.sent).toHaveLength(2)
    expect(ws.sent[0]).toHaveLength(MAX_DATA_PAYLOAD + 1)
    expect(ws.sent[1]).toHaveLength(101)
    for (const sentFrame of ws.sent) expect(sentFrame[0]).toBe(0x00)
    const rejoined = ws.sent.flatMap((f) => [...f.subarray(1)])
    expect(rejoined).toHaveLength(big.length)
  })
})

// --- inbound ----------------------------------------------------------------

describe('incoming frames', () => {
  it('routes 0x00 frames to the terminal sink, without the prefix', () => {
    const { ws, onData, onControl } = makeTransport()
    ws.open()
    ws.receive(frame(FRAME_DATA, 'hello'))
    expect(onData).toHaveBeenCalledTimes(1)
    expect(text(onData.mock.calls[0][0])).toBe('hello')
    expect(onControl).not.toHaveBeenCalled()
  })

  it('routes 0x01 frames to the control handler, parsed, and never to the terminal', () => {
    const { ws, onData, onControl } = makeTransport()
    ws.open()
    ws.receive(frame(FRAME_CONTROL, '{"type":"resize","cols":80,"rows":24}'))
    expect(onControl).toHaveBeenCalledWith({ type: 'resize', cols: 80, rows: 24 })
    expect(onData).not.toHaveBeenCalled()
  })

  it('does not confuse a data frame whose payload happens to be JSON', () => {
    const { ws, onData, onControl } = makeTransport()
    ws.open()
    ws.receive(frame(FRAME_DATA, '{"type":"resize"}'))
    expect(text(onData.mock.calls[0][0])).toBe('{"type":"resize"}')
    expect(onControl).not.toHaveBeenCalled()
  })

  it('accepts a bare prefix as an empty payload, like ptybridge.Decode', () => {
    const { ws, onData } = makeTransport()
    ws.open()
    ws.receive(new Uint8Array([FRAME_DATA]))
    expect(onData).toHaveBeenCalledTimes(1)
    expect(onData.mock.calls[0][0]).toHaveLength(0)
  })

  it('drops frames with an unknown kind instead of guessing', () => {
    const { ws, onData, onControl } = makeTransport()
    ws.open()
    ws.receive(frame(0x02, 'who knows'))
    expect(onData).not.toHaveBeenCalled()
    expect(onControl).not.toHaveBeenCalled()
    expect(console.warn).toHaveBeenCalled()
  })

  it('drops an empty message and a non-binary one without touching either sink', () => {
    const { ws, onData, onControl } = makeTransport()
    ws.open()
    ws.receive(new Uint8Array([]))
    ws.receiveRaw('0x00 as text')
    ws.receiveRaw(new Blob([new Uint8Array([FRAME_DATA])]))
    expect(onData).not.toHaveBeenCalled()
    expect(onControl).not.toHaveBeenCalled()
  })

  it('survives a control frame that is not JSON', () => {
    const { ws, onControl } = makeTransport()
    ws.open()
    ws.receive(frame(FRAME_CONTROL, 'not json'))
    expect(onControl).not.toHaveBeenCalled()
    ws.receive(frame(FRAME_CONTROL, '{"type":"ok"}'))
    expect(onControl).toHaveBeenCalledWith({ type: 'ok' })
  })

  it('works without an onControl handler at all', () => {
    const onData = vi.fn()
    const transport = new Transport({ url: 'ws://localhost/ws?session=work', onData })
    const ws = MockWebSocket.last
    ws.open()
    expect(() => ws.receive(frame(FRAME_CONTROL, '{"type":"x"}'))).not.toThrow()
    expect(transport.connected).toBe(true)
  })
})

// --- liveness, which the wake handler reads ---------------------------------
//
// Two timestamps. Nothing in this class reads either of them; they exist for
// `TerminalSession`'s wake handler to tell a socket that is merely quiet from
// one that is dead, so they are tested from the outside the way it sees them.

describe('lastRecvAt', () => {
  it('is stamped at construction, so a socket that has said nothing is not instantly stale', () => {
    let now = 1_000
    const { transport } = makeTransport({ now: () => now })
    expect(transport.lastRecvAt).toBe(1_000)

    // A fixture true by accident is the trap here: assert the clock has moved
    // and the field has not, so a `now()` getter would be caught.
    now = 9_000
    expect(transport.lastRecvAt).toBe(1_000)
  })

  it('is re-stamped when the socket opens', () => {
    let now = 1_000
    const { transport, ws } = makeTransport({ now: () => now })
    now = 6_000
    ws.open()
    expect(transport.lastRecvAt).toBe(6_000)
  })

  it('counts a control frame, not only data', () => {
    let now = 1_000
    const { transport, ws } = makeTransport({ now: () => now })
    ws.open()
    now = 2_000
    // A `pane` answer is the only inbound control frame the daemon sends, and
    // it is the whole point: a probe's answer is what proves the socket alive.
    ws.receive(frame(FRAME_CONTROL, '{"type":"pane","pane":"%3"}'))
    expect(transport.lastRecvAt).toBe(2_000)
  })

  it('counts a data frame', () => {
    let now = 1_000
    const { transport, ws } = makeTransport({ now: () => now })
    ws.open()
    now = 3_000
    ws.receive(frame(FRAME_DATA, 'output'))
    expect(transport.lastRecvAt).toBe(3_000)
  })

  it('counts a frame the transport goes on to reject as unusable', () => {
    // An empty frame, a non-binary message, an unknown kind: still a peer that
    // is talking. Liveness is about the socket, not about the payload being
    // useful, so the stamp sits in front of every validity check.
    let now = 1_000
    const { transport, ws } = makeTransport({ now: () => now })
    ws.open()

    now = 4_000
    ws.receiveRaw('0x00 as text')
    expect(transport.lastRecvAt).toBe(4_000)

    now = 5_000
    ws.receive(new Uint8Array([]))
    expect(transport.lastRecvAt).toBe(5_000)

    now = 6_000
    ws.receive(frame(0x02, 'a kind neither end knows'))
    expect(transport.lastRecvAt).toBe(6_000)
  })

  it('does not count a frame that arrives after close(), which suppresses everything', () => {
    let now = 1_000
    const { transport, ws } = makeTransport({ now: () => now })
    ws.open()

    // Stamped once while the socket is live, so that the assertion after the
    // close is a field that stopped moving rather than one that never moved.
    now = 2_000
    ws.receive(frame(FRAME_DATA, 'output'))
    expect(transport.lastRecvAt).toBe(2_000)

    transport.close()
    now = 5_000
    ws.receive(frame(FRAME_DATA, 'late output'))
    expect(transport.lastRecvAt).toBe(2_000)
  })
})

describe('connectStartedAt', () => {
  it('is the construction time and does not move when the socket opens', () => {
    let now = 1_000
    const { transport, ws } = makeTransport({ now: () => now })
    expect(transport.connectStartedAt).toBe(1_000)

    // The open is the point. The mutant this test exists for is "re-stamp
    // connectStartedAt in ws.onopen", beside the `lastRecvAt` stamp that
    // legitimately goes there -- and the wake handler's CONNECTING case cannot
    // kill it, because that fixture's socket never opens and nothing that
    // never opens can catch a re-stamp on open. So the kill is here: advance
    // the clock, open, and assert the field did not follow.
    now = 7_000
    ws.open()
    expect(transport.connectStartedAt).toBe(1_000)

    now = 9_000
    expect(transport.connectStartedAt).toBe(1_000)
  })
})

// --- the reason this class exists -------------------------------------------

describe('nothing is buffered across a close', () => {
  it('drops writes made before the socket opens', () => {
    const { transport, ws } = makeTransport()
    expect(transport.state).toBe('connecting')
    expect(transport.send('typed too early')).toBe(false)
    expect(transport.resize(80, 24)).toBe(false)
    expect(ws.sent).toEqual([])

    // The failure mode this whole class exists to avoid: a flush on open.
    ws.open()
    expect(ws.sent).toEqual([])
  })

  it('drops writes made after the socket closes, and keeps dropping them', () => {
    const { transport, ws } = makeTransport()
    ws.open()
    transport.send('before')
    ws.emitClose(1006, '', false)

    expect(transport.state).toBe('closed')
    expect(transport.send('after')).toBe(false)
    expect(transport.select('%3')).toBe(false)
    expect(ws.sent).toHaveLength(1)
    expect(text(ws.sent[0].subarray(1))).toBe('before')
  })

  it('never reopens a socket by itself, however long the caller waits', () => {
    vi.useFakeTimers()
    const { transport, ws } = makeTransport()
    ws.open()
    ws.emitClose(1006, '', false)
    transport.send('keystroke into the void')
    vi.advanceTimersByTime(120_000)
    expect(MockWebSocket.instances).toHaveLength(1)
    expect(ws.sent).toEqual([])
  })

  it('does not replay a dead transport into the socket that replaces it', () => {
    // A reconnect is a new Transport on a new tmux session. Bytes typed into
    // the old one must not surface in the new one's pane.
    const first = makeTransport()
    first.ws.open()
    first.ws.emitClose(1006, '', false)
    first.transport.send('rm -rf /\r')

    const second = makeTransport()
    second.ws.open()
    expect(second.ws.sent).toEqual([])
    expect(second.ws).not.toBe(first.ws)
  })

  it('drops writes while the socket is CLOSING, not only once it is CLOSED', () => {
    const { transport, ws } = makeTransport()
    ws.open()
    ws.close() // readyState CLOSING; onclose has not arrived yet
    expect(transport.state).toBe('closed')
    expect(transport.send('during the handshake')).toBe(false)
    expect(ws.sent).toEqual([])
  })
})

// --- what the server has not confirmed --------------------------------------
//
// WHY THIS EXISTS. From the daemon's own log, on a connection that died while
// the owner was typing into an agent's prompt: frames_in=289 bytes_in=2219
// (7.7 bytes a frame -- keystrokes), slowest_write=0s (the server never
// blocked), killed by a pong that never came back. The browser had handed those
// keystrokes to a socket that was still OPEN on a stalled link, so `send`
// returned true, the app believed them sent, and they died in the socket's own
// buffer. The journal is how a tab can tell which of its bytes only ever
// existed in the browser.
//
// It is NOT an outbound queue: nothing here is ever written anywhere. The
// `nothing is buffered across a close` tests above are the other half of that
// and must keep passing.

describe('the bytes the server has not confirmed', () => {
  const ack = (ws: MockWebSocket, bytes: unknown) =>
    ws.receive(frame(FRAME_CONTROL, JSON.stringify({ type: 'ack', bytes })))

  it('is empty on a socket that has sent nothing', () => {
    const { transport, ws } = makeTransport()
    ws.open()
    expect(transport.unacked).toHaveLength(0)
    expect(transport.unackedTruncated).toBe(false)
  })

  it('holds what was sent until the server says it arrived', () => {
    const { transport, ws } = makeTransport()
    ws.open()
    transport.send('hola')
    expect(text(transport.unacked)).toBe('hola')
    ack(ws, 4)
    expect(transport.unacked).toHaveLength(0)
  })

  it('trims exactly what was acknowledged and keeps the rest', () => {
    const { transport, ws } = makeTransport()
    ws.open()
    transport.send('hola')
    ack(ws, 2)
    // The pty took "ho". "la" is still only in this browser.
    expect(text(transport.unacked)).toBe('la')
  })

  it('counts bytes, not characters, so a multi-byte char is not half-freed', () => {
    const { transport, ws } = makeTransport()
    ws.open()
    transport.send('ñ') // two bytes in UTF-8
    ack(ws, 1)
    expect(transport.unacked).toHaveLength(1)
    ack(ws, 2)
    expect(transport.unacked).toHaveLength(0)
  })

  it('spans several sends, in the order they were written', () => {
    const { transport, ws } = makeTransport()
    ws.open()
    transport.send('one ')
    transport.send('two ')
    transport.send('three')
    ack(ws, 4)
    expect(text(transport.unacked)).toBe('two three')
  })

  // JavaScript coerces: without a type check, "4" compares equal to 4 and
  // frees four bytes the pty never took. This is the one malformed ack that
  // does damage rather than nothing, so it is the one worth a test.
  it('ignores an ack whose count is a numeric string', () => {
    const { transport, ws } = makeTransport()
    ws.open()
    transport.send('hola')
    ack(ws, '4')
    expect(text(transport.unacked)).toBe('hola')
  })

  it('does not go negative on an ack for more than was ever sent', () => {
    const { transport, ws } = makeTransport()
    ws.open()
    transport.send('hi')
    ack(ws, 9999)
    expect(transport.unacked).toHaveLength(0)
    transport.send('more')
    expect(text(transport.unacked)).toBe('more')
  })

  it('ignores an ack whose count is not a number', () => {
    const { transport, ws } = makeTransport()
    ws.open()
    transport.send('hola')
    ack(ws, 'all of it')
    ack(ws, null)
    ack(ws, undefined)
    expect(text(transport.unacked)).toBe('hola')
  })

  // Only input is journaled. A resize replayed into a pane would be noise, and
  // the server does not count them either -- see TestOnlyInputIsAcknowledged.
  it('does not hold control messages', () => {
    const { transport, ws } = makeTransport()
    ws.open()
    transport.resize(80, 24)
    transport.select('%3')
    expect(transport.unacked).toHaveLength(0)
  })

  // A write that returned false never reached the socket, and the class
  // documents those bytes as gone. Journaling them would make `unacked` a
  // record of two different things.
  it('does not hold writes that were dropped for a closed socket', () => {
    const { transport, ws } = makeTransport()
    ws.open()
    ws.emitClose(1006, '', false)
    expect(transport.send('into the void')).toBe(false)
    expect(transport.unacked).toHaveLength(0)
  })

  // One frame's maximum is 512KiB and the journal holds 64KiB, so a paste that
  // had to be split is deliberately NOT held whole -- the cap is sized for what
  // a person types during a stall, and a paste that big still exists wherever
  // it was pasted from. What has to hold is that the chunks AFTER the first are
  // journaled too, and that the end of the paste is what survives.
  it('journals past the first chunk of a paste it had to split', () => {
    const { transport, ws } = makeTransport()
    ws.open()
    expect(MAX_DATA_PAYLOAD).toBeGreaterThan(UNACKED_LIMIT_BYTES)
    transport.send('x'.repeat(MAX_DATA_PAYLOAD) + 'THE END')
    const held = transport.unacked
    expect(held).toHaveLength(UNACKED_LIMIT_BYTES)
    expect(text(held).endsWith('THE END')).toBe(true)
    expect(transport.unackedTruncated).toBe(true)
    // And a partial ack still trims from the correct offset afterwards, which
    // is what the `start` field is for: the second chunk's bytes begin at
    // MAX_DATA_PAYLOAD, not at 0.
    ack(ws, MAX_DATA_PAYLOAD + 7)
    expect(transport.unacked).toHaveLength(0)
  })

  it('copies what it holds, so a caller reusing its buffer cannot rewrite history', () => {
    const { transport } = makeTransport()
    MockWebSocket.last.open()
    const scratch = new TextEncoder().encode('good')
    transport.send(scratch)
    scratch.set(new TextEncoder().encode('evil'))
    expect(text(transport.unacked)).toBe('good')
  })

  it('keeps the ack to itself rather than handing it to the caller', () => {
    const { transport, ws, onControl } = makeTransport()
    ws.open()
    transport.send('hola')
    ack(ws, 4)
    // The caller validates meaning for the messages that are its business; the
    // ack is this class's own bookkeeping about its own writes.
    expect(onControl).not.toHaveBeenCalled()
  })

  // Bounded, because this is a browser tab that may live for weeks. The cap
  // keeps the NEWEST bytes: if something has to be lost, losing the start of a
  // huge paste beats losing the end of what was just typed.
  it('keeps the newest bytes when more is outstanding than it will hold', () => {
    const { transport } = makeTransport()
    MockWebSocket.last.open()
    expect(UNACKED_LIMIT_BYTES).toBeGreaterThan(1024)
    transport.send('a'.repeat(UNACKED_LIMIT_BYTES))
    transport.send('TAIL')
    expect(transport.unacked).toHaveLength(UNACKED_LIMIT_BYTES)
    expect(text(transport.unacked).endsWith('TAIL')).toBe(true)
    expect(transport.unackedTruncated).toBe(true)
  })

  // The cap trims the oldest entry in place, and the entry has to remember
  // where its first byte now sits: a later partial ack is measured from that
  // offset, and an offset left behind by the trim makes the ack cut the wrong
  // amount off. Measured with the numbers below: 65526 bytes left, not 65516.
  it('trims from the right offset after the cap has already cut an entry', () => {
    const { transport, ws } = makeTransport()
    ws.open()
    transport.send('A'.repeat(UNACKED_LIMIT_BYTES))
    transport.send('B'.repeat(10))
    // 10 bytes over the cap, so the head lost its first 10 and now starts at 10.
    expect(transport.unacked).toHaveLength(UNACKED_LIMIT_BYTES)
    ack(ws, 20)
    expect(transport.unacked).toHaveLength(UNACKED_LIMIT_BYTES - 10)
  })

  it('stops claiming truncation once everything outstanding is confirmed', () => {
    const { transport, ws } = makeTransport()
    ws.open()
    transport.send('a'.repeat(UNACKED_LIMIT_BYTES + 100))
    expect(transport.unackedTruncated).toBe(true)
    ack(ws, UNACKED_LIMIT_BYTES + 100)
    expect(transport.unacked).toHaveLength(0)
    expect(transport.unackedTruncated).toBe(false)
  })
})

// --- lifecycle the caller drives reconnection from --------------------------

describe('lifecycle', () => {
  it('reports open, and the close code and reason, so the caller can decide', () => {
    const { onOpen, onClose, ws } = makeTransport()
    ws.open()
    expect(onOpen).toHaveBeenCalledTimes(1)

    ws.emitClose(1000, 'session ended', true)
    const event = onClose.mock.calls[0][0] as TransportClose
    expect(event).toEqual({ code: 1000, reason: 'session ended', wasClean: true })
  })

  it('forwards socket errors', () => {
    const { onError, ws } = makeTransport()
    ws.emitError()
    expect(onError).toHaveBeenCalledTimes(1)
  })

  it('connects to the url it was given, once', () => {
    const { transport } = makeTransport()
    expect(MockWebSocket.instances).toHaveLength(1)
    expect(MockWebSocket.last.url).toBe('ws://localhost/ws?session=work')
    expect(transport.url).toBe('ws://localhost/ws?session=work')
  })

  it('stays silent after a close the caller asked for', () => {
    const { transport, ws, onClose, onData, onError } = makeTransport()
    ws.open()
    transport.close(1000, 'navigating away')
    expect(ws.closedWith).toEqual({ code: 1000, reason: 'navigating away' })

    // A caller that tore this down must not then see an onClose it would read
    // as a network drop and reconnect from.
    ws.emitClose(1000, 'navigating away', true)
    ws.receive(frame(FRAME_DATA, 'late output'))
    ws.emitError()
    expect(onClose).not.toHaveBeenCalled()
    expect(onData).not.toHaveBeenCalled()
    expect(onError).not.toHaveBeenCalled()
  })

  it('closes only once', () => {
    const { transport, ws } = makeTransport()
    ws.open()
    transport.close()
    ws.closedWith = null
    transport.close()
    expect(ws.closedWith).toBeNull()
  })
})

// --- types are part of the contract too -------------------------------------

describe('control message types', () => {
  it('accepts exactly the shapes the server understands', () => {
    const messages: ControlMessage[] = [
      { type: 'resize', cols: 80, rows: 24 },
      { type: 'select', pane: '%3' },
      { type: 'copy-mode', pane: '%3' },
      { type: 'copy-mode' },
      { type: 'end-mode', pane: '%3' },
      { type: 'where' },
    ]
    const { transport, ws } = makeTransport()
    ws.open()
    for (const message of messages) expect(transport.sendControl(message)).toBe(true)
    expect(ws.sent).toHaveLength(messages.length)
  })
})
