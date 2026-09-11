/**
 * The palette's two halves that can be pinned without a browser: what the chord
 * matcher accepts and how it is installed, and what rows the snapshot turns
 * into.
 *
 * Rendering follows AppSidebar.test.tsx -- `react-dom/server`, node
 * environment, no jsdom -- so a *closed* palette is what a static render can
 * show. That is not nothing: an open `CommandDialog` is a Radix portal, and the
 * thing worth proving here is that mounting the palette in the shell costs the
 * page nothing when it is shut.
 */

import { renderToStaticMarkup } from 'react-dom/server'
import { describe, expect, it, vi } from 'vitest'

import {
  Palette,
  PaletteBody,
  actionEntries,
  isPaletteChord,
  installPaletteChord,
  paneEntries,
} from './Palette'
import { ZOOM_HINT } from '@/lib/manage'
import { groupRows } from '@/lib/useSnapshot'
import type { SnapshotRow } from '@/lib/useSnapshot'

function row(over: Partial<SnapshotRow> = {}): SnapshotRow {
  return {
    groupKey: 'work',
    sessionId: '$0',
    sessionName: 'work',
    paneId: '%0',
    paneIndex: 0,
    appOwned: false,
    label: '',
    // `@N`, derived from the index so that a fixture varying `windowIndex`
    // still describes two *different* windows -- the tree keys on the id.
    windowId: `@${over.windowIndex ?? 0}`,
    windowIndex: 0,
    windowName: 'shell',
    paneActive: true,
    command: 'zsh',
    // tmux's default for an untouched pane: the hostname.
    title: 'devbox',
    // The pane's working directory, as of the poll; nothing renders it yet.
    path: '/srv/work',
    // A shell: the daemon computes no state for it, and "" is not a state.
    agentState: '',
    finishedAt: 0,
    // Nothing reported for this pane and nothing decided its state: "" in
    // both, which is where every pane that is not an agent sits.
    activity: '',
    stateSource: '',
    // Present as a key and undefined as a value: `question` is omitempty on the
    // Go side and the wire contract compares keys.
    question: undefined,
    ...over,
  }
}

/** A keydown, with only the fields the matcher reads. */
function key(over: Partial<KeyboardEvent> = {}): KeyboardEvent {
  return {
    key: 'k',
    code: 'KeyK',
    ctrlKey: true,
    altKey: true,
    metaKey: false,
    shiftKey: false,
    preventDefault: () => {},
    stopPropagation: () => {},
    ...over,
  } as KeyboardEvent
}

// --- the chord --------------------------------------------------------------

describe('isPaletteChord', () => {
  it('is Ctrl+Alt+K', () => {
    expect(isPaletteChord(key())).toBe(true)
  })

  it('accepts the physical K key whatever the layout produced', () => {
    // Ctrl+Alt is AltGr on several European layouts, so `key` is some other
    // character entirely -- `code` is what keeps the chord on the same key cap.
    expect(isPaletteChord(key({ key: 'ĸ', code: 'KeyK' }))).toBe(true)
    // ...and the other way round, for a layout where the code is not KeyK.
    expect(isPaletteChord(key({ key: 'K', code: 'Semicolon' }))).toBe(true)
  })

  it('needs both modifiers, and refuses Meta', () => {
    expect(isPaletteChord(key({ ctrlKey: false }))).toBe(false)
    expect(isPaletteChord(key({ altKey: false }))).toBe(false)
    // Cmd+Ctrl+Alt+K belongs to the window manager, not to this app.
    expect(isPaletteChord(key({ metaKey: true }))).toBe(false)
  })

  it('is not any other key', () => {
    expect(isPaletteChord(key({ key: 'j', code: 'KeyJ' }))).toBe(false)
  })
})

/** An EventTarget that records how it was subscribed to. */
function recordingTarget() {
  const added: { type: string; handler: EventListener; options: unknown }[] = []
  const removed: { type: string; handler: EventListener; options: unknown }[] = []
  const target: EventTarget = {
    addEventListener: (type, handler, options) =>
      added.push({ type, handler: handler as EventListener, options }),
    removeEventListener: (type, handler, options) =>
      removed.push({ type, handler: handler as EventListener, options }),
    dispatchEvent: () => true,
  }
  return { target, added, removed }
}

/** Both spellings of "capture phase" that addEventListener accepts. */
function capturing(options: unknown): boolean {
  return options === true || (typeof options === 'object' && options !== null && (options as AddEventListenerOptions).capture === true)
}

describe('installPaletteChord', () => {
  it('listens in the capture phase, ahead of the terminal', () => {
    const { target, added } = recordingTarget()
    installPaletteChord(target, () => {})
    expect(added).toHaveLength(1)
    expect(added[0].type).toBe('keydown')
    // The whole point. wterm's handler is on the terminal element; a
    // bubble-phase listener here would run after the keystroke had already
    // been sent to an agent.
    expect(capturing(added[0].options)).toBe(true)
  })

  it('swallows the chord instead of also typing it', () => {
    const { target, added } = recordingTarget()
    const toggle = vi.fn()
    installPaletteChord(target, toggle)

    const preventDefault = vi.fn()
    const stopPropagation = vi.fn()
    added[0].handler(key({ preventDefault, stopPropagation }) as unknown as Event)

    expect(toggle).toHaveBeenCalledTimes(1)
    expect(preventDefault).toHaveBeenCalledTimes(1)
    // Stopping propagation in the capture phase is what keeps the key out of
    // the pane; without it the palette opens *and* the agent gets a keystroke.
    expect(stopPropagation).toHaveBeenCalledTimes(1)
  })

  it('leaves every other key alone', () => {
    const { target, added } = recordingTarget()
    const toggle = vi.fn()
    installPaletteChord(target, toggle)

    const preventDefault = vi.fn()
    const stopPropagation = vi.fn()
    added[0].handler(
      key({ key: 'a', code: 'KeyA', preventDefault, stopPropagation }) as unknown as Event,
    )

    expect(toggle).not.toHaveBeenCalled()
    expect(preventDefault).not.toHaveBeenCalled()
    expect(stopPropagation).not.toHaveBeenCalled()
  })

  it('unsubscribes the same listener from the same phase', () => {
    const { target, added, removed } = recordingTarget()
    installPaletteChord(target, () => {})()
    expect(removed).toHaveLength(1)
    expect(removed[0].handler).toBe(added[0].handler)
    expect(capturing(removed[0].options)).toBe(true)
  })
})

// --- the rows ---------------------------------------------------------------

describe('paneEntries', () => {
  const groups = groupRows([
    row({ windowIndex: 0, windowName: 'shell', paneId: '%0', command: 'zsh' }),
    row({ windowIndex: 1, windowName: 'api', paneId: '%4', paneIndex: 0, command: 'claude' }),
    row({
      windowIndex: 1,
      windowName: 'api',
      paneId: '%5',
      paneIndex: 1,
      command: 'npm',
      paneActive: false,
    }),
  ])

  it('has one row per pane, in snapshot order', () => {
    const entries = paneEntries(groups, null, 'work')
    expect(entries.map((e) => e.id)).toEqual(['%0', '%4', '%5'])
  })

  it('names the pane only when the window is split', () => {
    const [shell, api0, api1] = paneEntries(groups, null, 'work')
    expect(shell.detail).toBeNull()
    expect(api0.detail).toBe('pane 0')
    expect(api1.detail).toBe('pane 1')
    expect(api0.label).toBe('work › 1: api')
  })

  it('matches on session, window, pane and command', () => {
    const [, api0] = paneEntries(groups, null, 'work')
    // The design's `session/window/pane`, plus the command -- which is how a
    // user actually finds an agent -- and the pane id, which keeps two
    // identically named rows apart for cmdk.
    expect(api0.search).toBe('work/1: api/pane 0/claude/%4')
  })

  it('marks the pane the tab is already on', () => {
    const entries = paneEntries(groups, '%4', 'work')
    expect(entries.filter((e) => e.current).map((e) => e.id)).toEqual(['%4'])
  })

  it('disables panes in a group this tab cannot attach to', () => {
    // Same rule as the sidebar: the group's own sessions are gone, so the
    // daemon would answer a new socket for it with a 404 -- and dropping a
    // working socket to find that out is worse than a greyed row.
    const orphan = groupRows([row({ groupKey: 'dead', appOwned: true, paneId: '%9' })])
    expect(paneEntries(orphan, null, 'work')[0].unreachable).toBe(true)
    // Unless this tab is already inside it, where its own socket still works.
    expect(paneEntries(orphan, null, 'dead')[0].unreachable).toBe(false)
  })
})

/**
 * The rename that has to be visible on both surfaces at once.
 */
describe('a renamed session', () => {
  const renamed = groupRows([
    row({ groupKey: 'work3', sessionName: 'api', paneId: '%4', command: 'claude' }),
  ])

  it('is labelled by its live name, never by the group key', () => {
    // tmux freezes session_group at the pre-rename name, so a row built from
    // the key reads `work3` forever -- while the sidebar beside it reads `api`,
    // which makes a rename look like it half worked.
    expect(paneEntries(renamed, null, 'work3')[0].label).toBe('api › 0: shell')
  })

  it('is still addressed by the group key, and findable under both names', () => {
    const [entry] = paneEntries(renamed, null, 'work3')
    // The key is the identity: it is what a click carries and what `?session=`
    // holds, so it stays in the search text as well as the new name.
    expect(entry.action).toEqual({ kind: 'pane', paneId: '%4', groupKey: 'work3' })
    expect(entry.search).toContain('api')
    expect(entry.search).toContain('work3')
  })
})

// --- the management rows ----------------------------------------------------

describe('actionEntries', () => {
  const groups = groupRows([
    row({ windowIndex: 1, windowName: 'api', paneId: '%4', command: 'claude' }),
    row({ windowIndex: 1, windowName: 'api', paneId: '%5', paneIndex: 1, paneActive: false }),
  ])

  it('offers the current pane is actions, and the session is', () => {
    const entries = actionEntries(groups, '%4', 'work')
    expect(entries.map((e) => e.id)).toEqual([
      'pane:label-pane',
      'pane:new-window',
      'pane:split-right',
      'pane:split-down',
      'pane:zoom',
      'pane:kill',
      'session:rename-session',
      'session:kill',
      'new-session',
    ])
  })

  it('acts on the pane this tab is looking at', () => {
    const entries = actionEntries(groups, '%5', 'work')
    expect(entries.find((e) => e.id === 'pane:split-right')?.intent).toEqual({
      kind: 'run',
      action: { verb: 'split', pane: '%5', direction: 'right' },
    })
    // And says which pane, because the palette has no row to point at.
    expect(entries.find((e) => e.id === 'pane:zoom')?.detail).toBe('pane 1 · %5')
  })

  it('carries the same zoom warning the context menu shows', () => {
    const zoom = actionEntries(groups, '%4', 'work').find((e) => e.id === 'pane:zoom')
    expect(zoom?.hint).toBe(ZOOM_HINT)
  })

  it('still offers a new session with no pane and no tmux at all', () => {
    // The case the design names: an empty tmux server has no row to
    // right-click and no current pane to scope to.
    expect(actionEntries([], null, null).map((e) => e.id)).toEqual(['new-session'])
  })
})

// --- mounting ---------------------------------------------------------------

describe('<Palette>', () => {
  it('renders no palette while it is closed', () => {
    const markup = renderToStaticMarkup(
      <Palette
        open={false}
        onOpenChange={() => {}}
        groups={groupRows([row()])}
        activePane="%0"
        activeSession="work"
        onSelectPane={() => {}}
        onCopyMode={() => true}
        onIntent={() => {}}
      />,
    )
    // Not empty: shadcn's own CommandDialog renders its sr-only DialogHeader
    // outside DialogContent, so the accessible title is in the page whether or
    // not the dialog is open. That is ui/command.tsx's business. What matters
    // here is that mounting the palette in the shell puts no input, no list and
    // no rows on the page until it is opened.
    expect(markup).not.toContain('command-input')
    expect(markup).not.toContain('command-item')
    expect(markup).not.toContain('dialog-content')
  })
})

/**
 * The rows themselves, rendered without the dialog that portals them.
 *
 * An open `CommandDialog` cannot be rendered under vitest's node environment,
 * so `PaletteBody` is what the suite can see. What it pins is that the three
 * groups exist and that the management rows are among them -- the palette
 * carrying the same actions as the sidebar is the half of this that a unit
 * test can reach.
 */
describe('<PaletteBody>', () => {
  const groups = groupRows([
    row({ windowIndex: 1, windowName: 'api', paneId: '%4', command: 'claude' }),
  ])

  function render(over: { activePane?: string | null } = {}): string {
    const activePane = over.activePane === undefined ? '%4' : over.activePane
    return renderToStaticMarkup(
      <PaletteBody
        entries={paneEntries(groups, activePane, 'work')}
        actions={actionEntries(groups, activePane, 'work')}
        failure={null}
        onRun={() => {}}
        onIntent={() => {}}
      />,
    )
  }

  it('carries panes, management and the terminal action', () => {
    const markup = render()
    expect(markup).toContain('Panes')
    expect(markup).toContain('Manage')
    expect(markup).toContain('Enter copy mode')
  })

  it('offers the same actions the row menus do, on the current pane', () => {
    const markup = render()
    expect(markup).toContain('Split right')
    expect(markup).toContain('Zoom window')
    expect(markup).toContain('Kill pane…')
    // And the one line that has to travel with the zoom wherever it is offered.
    expect(markup).toContain(ZOOM_HINT)
  })

  it('marks the kill red here too', () => {
    expect(render()).toMatch(/text-destructive[^>]*>[^<]*(<[^>]*>)*[^<]*Kill pane/)
  })

  it('still offers a new session with nothing selected', () => {
    const markup = render({ activePane: null })
    expect(markup).toContain('New session…')
    expect(markup).not.toContain('Split right')
  })
})
