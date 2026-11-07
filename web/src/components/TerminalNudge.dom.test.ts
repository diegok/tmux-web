// @vitest-environment jsdom
//
// One fact, which needs a real DOM and a real React render: changing the font
// size nudges the terminal's box, and mounting does not. What a nudge *does* --
// the 1px, the frame it is reverted on, the arming that rides on the restore --
// is four assertions in Terminal.test.tsx against `nudgeRemeasure` directly.
// This file exists because deleting the effect that calls it would leave every
// one of those green while the terminal quietly kept telling tmux the column
// count it had before the font changed.
//
// No `act()` here, deliberately: wrapping this component's mount in `act`
// never returns -- measured, 120s with no output -- because the session's own
// timers keep the queue act waits on non-empty. Real ticks instead, which is
// also closer to what a browser does with these two frames.

import { createElement } from 'react'
import { afterEach, beforeEach, expect, it, vi } from 'vitest'

// A real wterm mounts a WASM grid onto a canvas, which jsdom has not got. The
// element it renders is not what is under test here; the box around it is.
vi.mock('@wterm/react', () => ({
  Terminal: (props: { style?: Record<string, string> }) =>
    createElement('div', { 'data-wterm': '', style: props.style }),
  useTerminal: () => ({ ref: () => {}, write: () => {}, focus: () => {} }),
}))

const { Terminal } = await import('./Terminal')
const { applyTerminalSettings, resetTerminalSettings } = await import('@/lib/terminalSettings')
const { createRoot } = await import('react-dom/client')

/** The smallest thing `Transport` can construct. It never opens. */
class StubWebSocket {
  binaryType = 'blob'
  readyState = 0
  onopen: (() => void) | null = null
  onmessage: ((event: MessageEvent) => void) | null = null
  onclose: ((event: CloseEvent) => void) | null = null
  onerror: ((event: Event) => void) | null = null
  send() {}
  close() {
    this.readyState = 3
  }
}

/** Let React flush its render and run its effects. */
const tick = () => new Promise((resolve) => setTimeout(resolve, 0))

/**
 * Frames the test runs by hand.
 *
 * jsdom delivers real animation frames on a ~16ms timer, which makes "nothing
 * was nudged" a race against the clock rather than an assertion -- measured:
 * with jsdom's own frames, deleting the skip-the-first-run guard still passed.
 */
let frames: (() => void)[] = []

let container: HTMLDivElement
let root: ReturnType<typeof createRoot>

beforeEach(() => {
  frames = []
  vi.stubGlobal('requestAnimationFrame', (cb: () => void) => void frames.push(cb))
  vi.stubGlobal('WebSocket', StubWebSocket)
  // The presence probe is a fetch, and a real one here is a request to nowhere.
  vi.stubGlobal('fetch', () => new Promise(() => {}))
  resetTerminalSettings(null)
  container = document.createElement('div')
  document.body.append(container)
  root = createRoot(container)
})

afterEach(() => {
  root.unmount()
  container.remove()
  resetTerminalSettings(null)
  vi.unstubAllGlobals()
  vi.restoreAllMocks()
})

it('nudges the box when the font size changes, and not on mount', async () => {
  root.render(createElement(Terminal, { session: 'work', url: 'ws://localhost/ws?session=work' }))
  await tick()

  const host = container.firstElementChild as HTMLElement
  // Guards the assertions below: a placeholder with no terminal in it would
  // have no box to nudge, and every expectation here would pass by accident.
  expect(host.querySelector('[data-wterm]')).not.toBeNull()
  // The mount already measured in the font it mounted with, so a nudge here
  // would buy a second resize frame for nothing.
  expect(host.style.paddingRight).toBe('')
  expect(frames).toHaveLength(0)

  applyTerminalSettings({ fontSize: 20 })
  await tick()
  expect(host.style.paddingRight).toBe('1px')
  expect(frames).toHaveLength(1)

  // The shrink is held through a frame of its own -- that is the frame the
  // ResizeObserver is broadcast in, and the reason this is two frames and not
  // one. See `nudgeRemeasure`.
  for (const frame of frames.splice(0)) frame()
  expect(host.style.paddingRight).toBe('1px')
  expect(frames).toHaveLength(1)

  // And then it comes off: not a pixel of padding left on the terminal forever.
  for (const frame of frames.splice(0)) frame()
  expect(host.style.paddingRight).toBe('')
})
