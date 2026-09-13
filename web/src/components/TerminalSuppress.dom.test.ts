// @vitest-environment jsdom
//
// The companion to TerminalWake.dom.test.ts, and a separate file for the same
// reason: the rest of this suite runs under `environment: 'node'`, where there
// is no `document` to register a listener on.
//
// What suppression *does* -- which sizes go out and which do not -- is tested
// in Terminal.test.tsx by calling `noteFocus` directly. What is tested here is
// only the wiring a real browser needs: that `start()` puts the two focus
// listeners on the document, that `stop()` takes the same two references back
// off, and that each one hands `noteFocus` the element that ends up focused --
// which for `focusout` is `relatedTarget`, not the element being left.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { TerminalSession, paneStorageKey } from './Terminal'

/** The smallest thing `Transport` can construct; see the wake file's copy. */
class StubWebSocket {
  binaryType = 'blob'
  readyState = 0
  onopen: (() => void) | null = null
  onmessage: ((event: MessageEvent) => void) | null = null
  onclose: ((event: CloseEvent) => void) | null = null
  onerror: ((event: Event) => void) | null = null
  readonly url: string
  constructor(url: string) {
    this.url = url
  }
  send() {}
  close() {
    this.readyState = 3
  }
}

function makeSession(host?: Element) {
  return new TerminalSession({
    url: 'ws://localhost/ws?session=work',
    session: 'work',
    paneKey: paneStorageKey('work'),
    onData: () => {},
    onStatus: () => {},
    storage: null,
    probe: () => new Promise(() => {}),
    host: () => host ?? null,
  })
}

/** Just enough of a spy to read back what it was called with. */
interface ListenerSpy {
  mock: { calls: unknown[][] }
}

/** The handler `document.addEventListener` was given for this event, if any. */
function handlerFor(spy: ListenerSpy, type: string): unknown {
  const call = spy.mock.calls.find((c) => c[0] === type)
  return call?.[1]
}

let onDoc: ListenerSpy
let offDoc: ListenerSpy

beforeEach(() => {
  vi.stubGlobal('WebSocket', StubWebSocket)
  onDoc = vi.spyOn(document, 'addEventListener')
  offDoc = vi.spyOn(document, 'removeEventListener')
})

afterEach(() => {
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
  document.body.innerHTML = ''
})

describe('the focus listeners', () => {
  it('registers focusin and focusout on the document', () => {
    const term = makeSession()
    expect(handlerFor(onDoc, 'focusin')).toBeUndefined()

    term.start()

    // `focusin` and not `focus`: only the bubbling pair reaches the document,
    // which is what covers the reply box, the capture panel and anything a
    // later task adds without registering a thing.
    expect(handlerFor(onDoc, 'focusin')).toBeTypeOf('function')
    expect(handlerFor(onDoc, 'focusout')).toBeTypeOf('function')
    term.stop()
  })

  it('removes both on stop, with the same function reference', () => {
    const term = makeSession()
    term.start()
    const added = { in: handlerFor(onDoc, 'focusin'), out: handlerFor(onDoc, 'focusout') }

    term.stop()

    // Identity, not merely "removeEventListener was called": a `remove` with a
    // fresh arrow removes nothing, and the listener would keep this session and
    // its socket alive for the life of the page.
    expect(handlerFor(offDoc, 'focusin')).toBe(added.in)
    expect(handlerFor(offDoc, 'focusout')).toBe(added.out)
  })

  it('hands noteFocus the element gaining focus, on both events', () => {
    const box = document.createElement('textarea')
    document.body.append(box)
    const term = makeSession()
    term.start()
    const noteFocus = vi.spyOn(term, 'noteFocus')

    box.dispatchEvent(new FocusEvent('focusin', { bubbles: true }))
    expect(noteFocus).toHaveBeenLastCalledWith(box)

    // On `focusout` the element in the event is the one being *left*; the one
    // that matters is `relatedTarget`. Reading the target here would re-arm
    // suppression on the very event that should lift it.
    box.dispatchEvent(new FocusEvent('focusout', { bubbles: true, relatedTarget: null }))
    expect(noteFocus).toHaveBeenLastCalledWith(null)

    term.stop()
    box.dispatchEvent(new FocusEvent('focusin', { bubbles: true }))
    expect(noteFocus).toHaveBeenCalledTimes(2)
  })
})
