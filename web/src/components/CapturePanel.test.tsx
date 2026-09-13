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
 *
 * The selector added in Task 8 *is* renderable here, and deliberately so: it is
 * a native `<select>` rather than a Radix one, so which panes it offers and
 * which of them are disabled are facts about markup this suite can read back.
 * `disabled` on an `<option>` is a real attribute; `disabled` looked for in a
 * shadcn button's class list is this project's recorded test that always
 * passes.
 */

import { isValidElement } from 'react'
import type { ReactElement, ReactNode } from 'react'
import { renderToStaticMarkup } from 'react-dom/server'
import { describe, expect, it, vi } from 'vitest'

import {
  CapturePanelBody,
  captureAge,
  choosePane,
  copyCapture,
  paneOptionLabel,
  preventAutoFocus,
  truncationNotice,
} from './CapturePanel'
import type { CapturePanelBodyProps } from './CapturePanel'
import { paneEntries } from './Palette'
import { Dialog } from '@/components/ui/dialog'
import type { Capture } from '@/lib/capture'
import { groupRows } from '@/lib/useSnapshot'
import type { SnapshotRow } from '@/lib/useSnapshot'

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

/**
 * A snapshot row, defaulting to a plain shell in the session this tab is on.
 *
 * The same shape `Palette.test.tsx` builds its rows from, and for the same
 * reason: the selector's rows come from `paneEntries`, so its fixtures have to
 * be snapshot rows rather than hand-written entries -- a hand-written entry
 * list would assert nothing about where the rows come from.
 */
function row(over: Partial<SnapshotRow> = {}): SnapshotRow {
  return {
    groupKey: 'work',
    sessionId: '$0',
    sessionName: 'work',
    paneId: '%3',
    paneIndex: 0,
    appOwned: false,
    label: '',
    windowId: `@${over.windowIndex ?? 0}`,
    windowIndex: 0,
    windowName: 'shell',
    paneActive: true,
    command: 'zsh',
    title: 'devbox',
    path: '/srv/work',
    branch: '',
    agentState: '',
    finishedAt: 0,
    activity: '',
    stateSource: '',
    question: undefined,
    ...over,
  }
}

/**
 * Three sessions: the one this tab is attached to, a second live one, and a
 * group whose own sessions are gone.
 *
 * Two of those are the point. The panel's selector spans every session in the
 * snapshot -- a selector that showed only the current one would be a different
 * feature -- and the orphaned group is what the `unreachable` flag is for.
 */
const GROUPS = groupRows([
  row({ paneId: '%3', command: 'zsh' }),
  row({
    groupKey: 'notes',
    sessionId: '$1',
    sessionName: 'notes',
    paneId: '%7',
    windowIndex: 1,
    windowName: 'edit',
    command: 'nvim',
  }),
  row({
    groupKey: 'dead',
    sessionId: '$2',
    sessionName: 'dead',
    appOwned: true,
    paneId: '%9',
    windowIndex: 2,
    windowName: 'agent',
    command: 'claude',
  }),
])

/** The session this tab is attached to, in every fixture below. */
const HERE = 'work'

function render(over: Partial<CapturePanelBodyProps> = {}): string {
  return renderToStaticMarkup(
    <Dialog open>
      <CapturePanelBody
        paneId="%3"
        groups={GROUPS}
        activeSession={HERE}
        onSelectPane={() => {}}
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

/**
 * The element `CapturePanelBody` builds for a given set of props, without a
 * renderer.
 *
 * Calling a component as a plain function is only legal because this one uses
 * no hooks, and *that is the property*, not an accident of the harness: a panel
 * holding a selection of its own would need `useState` here. The throw below is
 * what that would become, and it names the reason rather than surfacing React's
 * "invalid hook call".
 */
function bodyTree(over: Partial<CapturePanelBodyProps> = {}): ReactNode {
  const props: CapturePanelBodyProps = {
    paneId: '%3',
    groups: GROUPS,
    activeSession: HERE,
    onSelectPane: () => {},
    capture: capture(),
    error: null,
    loading: false,
    copied: false,
    now: T0,
    onRecapture: () => {},
    onCopy: () => {},
    ...over,
  }
  let node: ReactNode = null
  let hooked: string | null = null
  try {
    node = CapturePanelBody(props)
  } catch (err) {
    hooked = String(err)
  }
  expect(hooked, '<CapturePanelBody> called a hook, so it holds a pane of its own').toBeNull()
  return node
}

/** The first element of a tag in a tree, or a throw naming what was searched. */
function findByType(node: ReactNode, type: string): ReactElement {
  const found = search(node, type)
  if (!found) throw new Error(`no <${type}> in the tree`)
  return found
}

function search(node: ReactNode, type: string): ReactElement | null {
  if (Array.isArray(node)) {
    for (const child of node) {
      const hit = search(child as ReactNode, type)
      if (hit) return hit
    }
    return null
  }
  if (!isValidElement(node)) return null
  if (node.type === type) return node
  return search((node.props as { children?: ReactNode }).children, type)
}

/** Every `<option>` in a render, opening tag and label together. */
function optionTags(markup: string): string[] {
  return [...markup.matchAll(/<option[^>]*>[\s\S]*?<\/option>/g)].map((m) => m[0])
}

/** The one `<option>` carrying a value, or a throw naming what was there instead. */
function optionFor(markup: string, value: string): string {
  const found = optionTags(markup).filter((t) => t.includes(`value="${value}"`))
  if (found.length !== 1) {
    throw new Error(`expected one <option value="${value}">, found ${found.length} in ${markup}`)
  }
  return found[0]
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

// --- the selector, which *is* the tab's selection ---------------------------

describe('paneOptionLabel', () => {
  const [work, notes] = paneEntries(GROUPS, '%3', HERE)

  it('names the session, the window and what is running', () => {
    expect(paneOptionLabel(work)).toBe('work › 0: shell · zsh')
    expect(paneOptionLabel(notes)).toBe('notes › 1: edit · nvim')
  })

  it('names the pane too, once the window is split', () => {
    const split = groupRows([
      row({ paneId: '%3', paneIndex: 0, command: 'zsh' }),
      row({ paneId: '%4', paneIndex: 1, command: 'claude', paneActive: false }),
    ])
    const [, second] = paneEntries(split, '%3', HERE)
    expect(second.detail).toBe('pane 1')
    expect(paneOptionLabel(second)).toBe('work › 0: shell · pane 1 · claude')
  })

  it('says out loud that a pane cannot be attached to', () => {
    const [, , orphan] = paneEntries(GROUPS, '%3', HERE)
    expect(orphan.unreachable).toBe(true)
    expect(paneOptionLabel(orphan)).toContain('unreachable')
  })
})

describe('choosePane', () => {
  /** Every call the selector made, in the order it made them. */
  function recorder() {
    const calls: [string, string, { focus?: boolean } | undefined][] = []
    return {
      calls,
      selectPane(paneId: string, groupKey: string, opts?: { focus?: boolean }) {
        calls.push([paneId, groupKey, opts])
      },
    }
  }

  const entries = paneEntries(GROUPS, '%3', HERE)

  it('sends the chosen pane and its own group, never the one already on screen', () => {
    // `%7` is in another session group, and `%3` -- the pane the panel is
    // showing -- is in this one. A selector that passed the current pane's
    // group, or the current pane's id, would re-select the pane the user was
    // trying to leave and the two ids here are what says so.
    const rec = recorder()
    choosePane(entries, '%7', '%3', rec.selectPane)
    expect(rec.calls).toEqual([['%7', 'notes', { focus: false }]])
  })

  it('does not focus the terminal', () => {
    // The assertion that keeps the soft keyboard off a phone on every pane
    // change. `handleSelectPane`'s same-group branch ends by focusing the
    // terminal, which from inside this modal is the terminal's input taking
    // focus within a tap gesture -- in the full-screen dialog whose whole
    // reason to exist is being readable without the keyboard.
    const rec = recorder()
    choosePane(entries, '%7', '%3', rec.selectPane)
    expect(rec.calls[0][2]).toEqual({ focus: false })
  })

  it('does nothing at all for the pane already on screen', () => {
    // Not merely "does not re-capture": nothing is sent, so tmux's shared
    // active pane is not written either. A selector that fired on every render
    // would show up here and nowhere else.
    const rec = recorder()
    choosePane(entries, '%3', '%3', rec.selectPane)
    expect(rec.calls).toEqual([])
  })

  it('refuses a pane in a group this tab cannot attach to', () => {
    const rec = recorder()
    expect(entries.find((e) => e.id === '%9')?.unreachable).toBe(true)
    choosePane(entries, '%9', '%3', rec.selectPane)
    expect(rec.calls).toEqual([])
  })

  it('refuses an id no row offers', () => {
    // The placeholder row, and a pane that died between the render and the
    // tap. Neither is a pane this tab can be moved to.
    const rec = recorder()
    choosePane(entries, '', '%3', rec.selectPane)
    choosePane(entries, '%404', '%3', rec.selectPane)
    expect(rec.calls).toEqual([])
  })
})

describe('<CapturePanelBody> selector', () => {
  it('offers every pane in the snapshot, not only the current session’s', () => {
    // The panel's rows are the palette's, and the palette's span every session:
    // the current one is a flag on a row, never a filter over them.
    const values = optionTags(render()).map((t) => t.match(/value="([^"]*)"/)?.[1])
    expect(values).toEqual(['%3', '%7', '%9'])
  })

  it('shows the pane the panel is captured from as the chosen one', () => {
    // One pane drives the header, the selected row and the fetch. There is no
    // second pane in this component for them to disagree about.
    const markup = render({ paneId: '%7' })
    expect(optionFor(markup, '%7')).toContain('selected=""')
    expect(optionFor(markup, '%3')).not.toContain('selected')
    expect(markup).toContain('Scrollback · %7')
  })

  it('disables the panes in a group this tab cannot attach to, and only those', () => {
    const markup = render()
    // `disabled=""` on an `<option>`, which is the attribute and not a class
    // name that happens to contain the word.
    expect(optionFor(markup, '%9')).toContain('disabled=""')
    expect(optionFor(markup, '%3')).not.toContain('disabled')
    expect(optionFor(markup, '%7')).not.toContain('disabled')
  })

  it('still names the pane when the snapshot has no row for it', () => {
    // A pane that died, or a snapshot that has not loaded. The selector must
    // still show what the header says, or the two disagree on screen.
    const markup = render({ paneId: '%404' })
    expect(optionFor(markup, '%404')).toContain('selected=""')
    expect(markup).toContain('Scrollback · %404')
  })

  it('has something to say when there is no pane at all', () => {
    const markup = render({ paneId: null, capture: null })
    expect(optionFor(markup, '')).toContain('selected=""')
  })

  it('sends a choice to the tab, with the keyboard declined', () => {
    // The headline decision, at the level where it can actually be wrong: the
    // control's own handler, dispatching the chosen pane through the callback
    // App fills with `handleSelectPane`. A panel that answered its own selector
    // with a `setState` would leave this list empty -- and would name a pane
    // the reply box below it does not.
    const calls: [string, string, { focus?: boolean } | undefined][] = []
    const select = findByType(
      bodyTree({
        onSelectPane: (paneId, groupKey, opts) => calls.push([paneId, groupKey, opts]),
      }),
      'select',
    )
    const onChange = (select.props as { onChange: (e: unknown) => void }).onChange
    onChange({ target: { value: '%7' } })
    expect(calls).toEqual([['%7', 'notes', { focus: false }]])
  })

  it('shows the pane the header names, and takes its value from nowhere else', () => {
    const select = findByType(bodyTree({ paneId: '%7' }), 'select')
    expect((select.props as { value: string }).value).toBe('%7')
  })
})
