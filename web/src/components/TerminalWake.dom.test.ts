// @vitest-environment jsdom
//
// This file exists only because vitest runs the rest of this suite under
// `environment: 'node'`, where `document` does not exist. Everything about what
// a wake *does* is tested in Terminal.test.tsx by calling `wake()`. What is
// tested here is the two lines that make a real browser call it, and that
// `stop()` takes them back off -- a listener that outlives its session keeps a
// dead TerminalSession alive for the life of the page.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { TerminalSession, paneStorageKey } from './Terminal'

/**
 * The smallest thing `Transport` can construct. Deliberately not imported from
 * Terminal.test.tsx: that harness asserts on wire frames and carries a node
 * environment's assumptions, and this file needs neither.
 */
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

function makeSession() {
  return new TerminalSession({
    url: 'ws://localhost/ws?session=work',
    session: 'work',
    paneKey: paneStorageKey('work'),
    onData: () => {},
    onStatus: () => {},
    storage: null,
    // Never answered: nothing here reaches the presence probe, and a real
    // fetch under jsdom would be a request to nowhere.
    probe: () => new Promise(() => {}),
  })
}

/** jsdom's `visibilityState` is a prototype getter; shadow it per test. */
function setVisibility(state: 'visible' | 'hidden') {
  Object.defineProperty(document, 'visibilityState', { configurable: true, value: state })
}

/** Just enough of a spy to read back what it was called with. */
interface ListenerSpy {
  mock: { calls: unknown[][] }
}

/** The handler `target.addEventListener` was given for this event, if any. */
function handlerFor(spy: ListenerSpy, type: string): unknown {
  const call = spy.mock.calls.find((c) => c[0] === type)
  return call?.[1]
}

let onDoc: ListenerSpy
let offDoc: ListenerSpy
let onWin: ListenerSpy
let offWin: ListenerSpy

beforeEach(() => {
  vi.stubGlobal('WebSocket', StubWebSocket)
  setVisibility('visible')
  onDoc = vi.spyOn(document, 'addEventListener')
  offDoc = vi.spyOn(document, 'removeEventListener')
  onWin = vi.spyOn(window, 'addEventListener')
  offWin = vi.spyOn(window, 'removeEventListener')
})

afterEach(() => {
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
  // @ts-expect-error -- drop the shadow and let the prototype getter answer again.
  delete document.visibilityState
})

describe('the wake listeners', () => {
  it('registers visibilitychange on the document and pageshow on the window', () => {
    const term = makeSession()
    expect(handlerFor(onDoc, 'visibilitychange')).toBeUndefined()

    term.start()

    expect(handlerFor(onDoc, 'visibilitychange')).toBeTypeOf('function')
    // `pageshow` is the one that carries a bfcache restore, which on iOS can
    // arrive with no visibilitychange at all.
    expect(handlerFor(onWin, 'pageshow')).toBeTypeOf('function')
  })

  it('removes both on stop, with the same function reference', () => {
    const term = makeSession()
    term.start()
    const added = { visible: handlerFor(onDoc, 'visibilitychange'), show: handlerFor(onWin, 'pageshow') }

    term.stop()

    // Identity, not merely "removeEventListener was called": a `remove` with a
    // fresh arrow removes nothing at all, and nothing else in this repository's
    // suites can see that.
    expect(handlerFor(offDoc, 'visibilitychange')).toBe(added.visible)
    expect(handlerFor(offWin, 'pageshow')).toBe(added.show)
  })

  it('wakes when the tab becomes visible and not when it goes away', () => {
    const term = makeSession()
    term.start()
    const wake = vi.spyOn(term, 'wake')

    // The event fires on hide as well as on show, and probing a tab that is
    // going away is two forks nobody will see the result of.
    setVisibility('hidden')
    document.dispatchEvent(new Event('visibilitychange'))
    expect(wake).not.toHaveBeenCalled()

    setVisibility('visible')
    document.dispatchEvent(new Event('visibilitychange'))
    expect(wake).toHaveBeenCalledTimes(1)

    // `pageshow` carries no such filter: a page being shown is the whole event.
    window.dispatchEvent(new Event('pageshow'))
    expect(wake).toHaveBeenCalledTimes(2)

    term.stop()
    document.dispatchEvent(new Event('visibilitychange'))
    window.dispatchEvent(new Event('pageshow'))
    expect(wake).toHaveBeenCalledTimes(2)
  })
})
