/**
 * The scrollback panel: the age of the capture, the truncation notice, the
 * copy that never refetches, and the two things about the markup that are
 * decisions rather than styling.
 *
 * Rendered with `react-dom/server`, as everything else in this suite is, and
 * for the same reason as `KillDialog.test.tsx`: the body goes inside a bare
 * `<Dialog open>` -- the Radix root is a context provider with no DOM of its
 * own and `DialogTitle` throws outside one -- but *not* inside `DialogContent`,
 * which is a portal into a `document` that does not exist under vitest's node
 * environment.
 *
 * That is also why nothing here asserts the `onOpenAutoFocus` prop. It is a
 * prop of `DialogContent`, which never renders in this suite, so there is no
 * markup it could appear in and a test claiming to check it would be a test
 * that cannot fail. The half of that mechanism vitest can see is
 * `preventAutoFocus` itself, driven directly below; the other half -- that
 * `document.activeElement` after opening is the dialog and not the first
 * tabbable child -- is a Playwright assertion in Task 9.
 */

import { renderToStaticMarkup } from 'react-dom/server'
import { describe, expect, it, vi } from 'vitest'

import {
  CapturePanelBody,
  captureAge,
  copyCapture,
  preventAutoFocus,
  truncationNotice,
} from './CapturePanel'
import type { CapturePanelBodyProps } from './CapturePanel'
import { Dialog } from '@/components/ui/dialog'
import type { Capture } from '@/lib/capture'

/** A fixed wall clock, so every age fixture below is an explicit offset off it. */
const T0 = 1_700_000_000_000

function capture(over: Partial<Capture> = {}): Capture {
  return {
    paneId: '%3',
    text: '$ make test\nok\n',
    lines: 1000,
    truncated: false,
    capturedAt: T0,
    ...over,
  }
}

function render(over: Partial<CapturePanelBodyProps> = {}): string {
  return renderToStaticMarkup(
    <Dialog open>
      <CapturePanelBody
        paneId="%3"
        capture={capture()}
        error={null}
        loading={false}
        copied={false}
        now={T0}
        onRecapture={() => {}}
        onCopy={() => {}}
        {...over}
      />
    </Dialog>,
  )
}

/** The `<pre>`'s opening tag, or a thrown error naming what was rendered instead. */
function preTag(markup: string): string {
  const found = markup.match(/<pre[^>]*>/)
  if (!found) throw new Error(`no <pre> in ${markup}`)
  return found[0]
}

/** What the `<pre>` wraps, still HTML-escaped. */
function preBody(markup: string): string {
  const found = markup.match(/<pre[^>]*>([\s\S]*?)<\/pre>/)
  if (!found) throw new Error(`no <pre> in ${markup}`)
  return found[1]
}

/**
 * The declarations of an element's inline `style`, as a list.
 *
 * Split rather than searched, because `user-select:text` is a substring of
 * `-webkit-user-select:text` -- a `toContain` would pass on the prefixed
 * property alone, which is the `disabled:pointer-events-none` trap wearing a
 * different hat.
 */
function styleDecls(tag: string): string[] {
  const found = tag.match(/ style="([^"]*)"/)
  if (!found) throw new Error(`no style attribute on ${tag}`)
  return found[1].split(';').filter((d) => d !== '')
}

describe('captureAge', () => {
  // Literal offsets, not the module's own thresholds: a fixture derived from
  // the constant a mutant retargets moves with the mutant and never fails.
  it('says "just now" for the first second', () => {
    expect(captureAge(T0, T0)).toBe('just now')
    expect(captureAge(T0, T0 + 999)).toBe('just now')
  })

  it('counts seconds from one second', () => {
    expect(captureAge(T0, T0 + 1000)).toBe('1s ago')
    expect(captureAge(T0, T0 + 14_000)).toBe('14s ago')
    expect(captureAge(T0, T0 + 59_999)).toBe('59s ago')
  })

  it('counts minutes from one minute', () => {
    expect(captureAge(T0, T0 + 60_000)).toBe('1m ago')
    expect(captureAge(T0, T0 + 180_000)).toBe('3m ago')
    expect(captureAge(T0, T0 + 3_599_999)).toBe('59m ago')
  })

  it('counts hours from one hour', () => {
    expect(captureAge(T0, T0 + 3_600_000)).toBe('1h ago')
    expect(captureAge(T0, T0 + 7_200_000)).toBe('2h ago')
  })

  it('never ages backwards when the two clocks disagree', () => {
    // `capturedAt` is the daemon's clock and `now` is the browser's; they are
    // not the same clock and nothing synchronises them. A panel that printed
    // "-4s ago" would be telling the user the capture is from the future.
    expect(captureAge(T0, T0 - 4000)).toBe('just now')
    expect(captureAge(T0, T0 - 90_000)).toBe('just now')
  })
})

describe('truncationNotice', () => {
  it('is silent when nothing was dropped', () => {
    expect(truncationNotice(false, 1000)).toBeNull()
    expect(truncationNotice(false, 5000)).toBeNull()
  })

  it('says the oldest lines are missing, and how deep the capture went', () => {
    const notice = truncationNotice(true, 1000)
    expect(notice).not.toBeNull()
    expect(notice).toMatch(/oldest/)
    expect(notice).toContain('1000')
    expect(truncationNotice(true, 5000)).toContain('5000')
  })
})

describe('preventAutoFocus', () => {
  it('prevents the default focus move', () => {
    // Radix focuses the dialog's first tabbable child on open, which on a
    // phone raises the soft keyboard this panel exists to avoid. This is the
    // whole of the logic; the resulting `document.activeElement` is Task 9's.
    const preventDefault = vi.fn()
    preventAutoFocus({ preventDefault } as unknown as Event)
    expect(preventDefault).toHaveBeenCalledTimes(1)
  })
})

describe('copyCapture', () => {
  function writer() {
    const written: string[] = []
    return {
      written,
      writeText(text: string) {
        written.push(text)
        return Promise.resolve()
      },
    }
  }

  it('writes exactly the text it was handed', async () => {
    const clipboard = writer()
    await copyCapture('$ make test\nok\n', clipboard)
    expect(clipboard.written).toEqual(['$ make test\nok\n'])
  })

  it('writes synchronously, before the first await, and fetches nothing first', () => {
    // iOS Safari rejects a `writeText` that is not synchronously inside the
    // user gesture, so anything awaited before it -- a fresh capture above
    // all -- loses the clipboard. And what the user copies must be what they
    // were looking at, which a refetch is not.
    const fetchSpy = vi.fn(() => Promise.reject(new Error('the panel must not fetch to copy')))
    const previous = globalThis.fetch
    globalThis.fetch = fetchSpy as unknown as typeof globalThis.fetch
    try {
      const clipboard = writer()
      const pending = copyCapture('on screen', clipboard)
      // Not awaited: this is the same turn the click handler runs in.
      expect(clipboard.written).toEqual(['on screen'])
      expect(fetchSpy).not.toHaveBeenCalled()
      return pending
    } finally {
      globalThis.fetch = previous
    }
  })

  it('reports a browser that will not give the page a clipboard', async () => {
    await expect(copyCapture('anything', undefined)).rejects.toThrow(/clipboard/)
  })
})

describe('<CapturePanelBody>', () => {
  it('is a <pre> and not a <textarea>', () => {
    // The headline decision: a textarea is a focusable text field, and
    // focusing one on a phone opens the keyboard this panel exists to avoid.
    const markup = render()
    expect(markup).not.toContain('<textarea')
    expect(preBody(markup)).toBe('$ make test\nok\n')
  })

  it('makes the capture selectable, unprefixed property included', () => {
    const decls = styleDecls(preTag(render()))
    expect(decls).toContain('user-select:text')
    expect(decls).toContain('white-space:pre-wrap')
    expect(decls).toContain('overflow-wrap:anywhere')
  })

  it('ages the capture in the header', () => {
    expect(render({ now: T0 + 14_000 })).toContain('captured 14s ago')
    expect(render({ now: T0 + 180_000 })).toContain('captured 3m ago')
  })

  it('says when the daemon cut the top off', () => {
    expect(render({ capture: capture({ truncated: true, lines: 1000 }) })).toMatch(/oldest/)
    expect(render()).not.toMatch(/oldest/)
  })

  it('shows the daemon’s own sentence when the capture failed', () => {
    const markup = render({ capture: null, error: "can't find pane: %99" })
    expect(markup).toContain('can&#x27;t find pane: %99')
  })

  it('offers a recapture, because nothing here refreshes on its own', () => {
    expect(render()).toContain('Recapture')
  })
})
