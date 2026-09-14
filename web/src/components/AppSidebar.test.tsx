/**
 * What the sidebar puts on screen, rendered with `react-dom/server`.
 *
 * No jsdom and no testing library: `renderToStaticMarkup` is enough for the
 * rules that are worth pinning here -- which rows exist, which one is marked
 * active, and which of the empty states is showing -- and it runs in the same
 * node environment as the rest of the suite. Effects do not run under it, which
 * is why `useIsMobile` reads as false and the desktop layout is what gets
 * rendered; the mobile `Sheet` is shadcn's code, not this file's.
 *
 * Clicks are deliberately absent. A static render has no handlers to fire, and
 * the interesting half of a click -- that it reaches the terminal's ref -- is
 * App.tsx's wiring and Task 23's Playwright run.
 */

import { readFileSync } from 'node:fs'

import { renderToStaticMarkup } from 'react-dom/server'
import { describe, expect, it } from 'vitest'

import { AGENT_TITLE_PREFIXES, AppSidebar } from './AppSidebar'
import { SidebarProvider } from '@/components/ui/sidebar'
import { rowMenu, rowTargetForWindow } from '@/lib/manage'
import { groupRows, readSeen, viewedSeen } from '@/lib/useSnapshot'
import type { SnapshotRow, SnapshotState } from '@/lib/useSnapshot'

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
    // The default is what tmux gives an untouched pane -- the hostname -- so
    // every test that does not care about titles is still rendering the case
    // the sidebar has to keep out of the rows.
    title: 'devbox',
    // No mode: where most panes are, and what the copy-mode control offers
    // to enter from.
    paneMode: '',
    // The pane's working directory, as of the poll; nothing renders it yet.
    path: '/srv/work',
    // Not in a work tree, which is what "" means. Nothing renders it yet.
    branch: '',
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

function state(over: Partial<SnapshotState> = {}): SnapshotState {
  return {
    groups: [],
    rows: [],
    // A tmux server generation the fixtures can key `seen` against. Non-empty
    // on purpose: with "" the browser refuses to compute `done` at all, so a
    // default of "" would make every done test vacuously pass.
    serverStart: '1757500000',
    loaded: true,
    stale: false,
    error: null,
    trouble: 0,
    unauthorized: false,
    ...over,
  }
}

function fromRows(rows: SnapshotRow[], over: Partial<SnapshotState> = {}): SnapshotState {
  return state({ rows, groups: groupRows(rows), ...over })
}

/**
 * Render with a `localStorage` this test controls.
 *
 * `seen` is App's, passed in as a prop, and `render` below derives it exactly
 * as App does -- `readSeen` off this storage, then `viewedSeen` for the pane
 * the tab is looking at, which is what decides which panes read done. There is
 * no such global under vitest's node environment, so this installs one for the
 * duration of a render rather than mocking the module.
 */
function withSeen<T>(seen: Record<string, number>, fn: () => T): T {
  const items: Record<string, string> = {
    'tmux-web:seen': JSON.stringify(seen),
  }
  const fake = {
    getItem: (k: string) => items[k] ?? null,
    setItem: (k: string, v: string) => {
      items[k] = v
    },
  }
  const had = 'localStorage' in globalThis
  const before = (globalThis as { localStorage?: unknown }).localStorage
  Object.defineProperty(globalThis, 'localStorage', {
    value: fake,
    configurable: true,
  })
  try {
    return fn()
  } finally {
    if (had)
      Object.defineProperty(globalThis, 'localStorage', {
        value: before,
        configurable: true,
      })
    else delete (globalThis as { localStorage?: unknown }).localStorage
  }
}

function render(
  snapshot: SnapshotState,
  over: { activePane?: string | null; activeSession?: string | null } = {},
): string {
  const activePane = over.activePane ?? null
  return renderToStaticMarkup(
    <SidebarProvider>
      <AppSidebar
        snapshot={snapshot}
        // What `useSeenPanes` hands the sidebar from App, spelled out here
        // rather than called: a hook's first render is all `renderToStaticMarkup`
        // runs, and this is that render's answer.
        seen={viewedSeen(readSeen(), snapshot.serverStart, activePane, snapshot.rows)}
        activePane={activePane}
        activeSession={over.activeSession ?? 'work'}
        onSelectPane={() => {}}
        onRefresh={() => {}}
        onIntent={() => {}}
      />
    </SidebarProvider>,
  )
}

/** Every state dot's state, in document order. "" panes render none at all. */
function dots(markup: string): string[] {
  return [...markup.matchAll(/data-agent-state="([^"]*)"/g)].map((m) => m[1])
}

/**
 * Every mark's accessible name, in document order.
 *
 * Scoped to `<svg>` on purpose: the state dot and the tmux-active marker are
 * named spans with `role="img"` too, so a helper that swept the markup for
 * every accessible name would report a mark on rows that have none.
 */
function markNames(markup: string): string[] {
  return [...markup.matchAll(/<svg[^>]*aria-label="([^"]*)"/g)].map((m) => m[1])
}

/** Every element carrying data-active="true", as its opening tag. */
function activeTags(markup: string): string[] {
  return [...markup.matchAll(/<[^>]*data-active="true"[^>]*>/g)].map((m) => m[0])
}

/**
 * What every pane row says about itself, in document order -- and, just as
 * importantly, *which line* it says it on.
 *
 * The two shapes are not interchangeable and telling them apart is most of what
 * the assertions below are for. A `command` is the monospaced capsule on the
 * row's first line, beside the window name; a `title` is the dim second line
 * underneath it. A helper that returned only the text would pass with the two
 * swapped, which is precisely the regression that would undo this layout.
 *
 * One alternation rather than two passes, so the results come back interleaved
 * in document order and a test can say which row said what.
 */
function rowText(markup: string): { kind: 'command' | 'title'; tag: string; text: string }[] {
  const re =
    /(<span[^>]*data-slot="badge"[^>]*>)([^<]*)<\/span>|(<span[^>]*class="row-line[^"]*"[^>]*>)<span>([^<]*)<\/span>/g
  return [...markup.matchAll(re)].map((m) =>
    m[1] === undefined
      ? { kind: 'title' as const, tag: m[3], text: m[4] }
      : { kind: 'command' as const, tag: m[1], text: m[2] },
  )
}

/**
 * The user label riding on each row's first line, in document order.
 *
 * Matched *through* the name span, so a label rendered anywhere else -- on the
 * second line, where a lone label still belongs -- is not found by this. A
 * helper that searched the whole markup for the words would report a label on
 * the first line whichever line it was actually on, which is the one thing
 * these tests are here to tell apart.
 */
function rowLabels(markup: string): string[] {
  const re = /<span class="truncate">[^<]*<\/span><span data-row-label[^>]*>([^<]*)<\/span>/g
  return [...markup.matchAll(re)].map((m) => m[1])
}

const splitWindow: SnapshotRow[] = [
  row({ windowIndex: 1, windowName: 'api', paneId: '%4', paneIndex: 0, command: 'claude' }),
  row({
    windowIndex: 1,
    windowName: 'api',
    paneId: '%2',
    paneIndex: 1,
    command: 'vim',
    paneActive: false,
  }),
]

describe('AppSidebar', () => {
  it('renders a window with one pane as just the window', () => {
    const markup = render(fromRows([row({ command: 'claude' })]))
    // No sub-menu at all: a lone child under a window is a row and a
    // disclosure spent saying nothing.
    expect(markup).not.toContain('data-sidebar="menu-sub"')
    expect(markup).toContain('0: shell')
    // The command is still there -- as the window's own badge.
    expect(markup).toContain('claude')
  })

  it('renders panes as sub-items once a window has more than one', () => {
    const markup = render(fromRows(splitWindow))
    expect(markup).toContain('data-sidebar="menu-sub"')
    expect(markup).toContain('pane 0')
    expect(markup).toContain('pane 1')
    expect(markup).toContain('vim')
  })

  it('highlights the pane the terminal is on, not the one tmux calls active', () => {
    // tmux says %4 is the active pane of window 1; this tab is looking at %2.
    const markup = render(fromRows(splitWindow), { activePane: '%2' })
    const active = activeTags(markup)
    expect(active).toHaveLength(1)
    expect(active[0]).toContain('%2')
    expect(active[0]).not.toContain('%4')
  })

  it('marks a single-pane window active when the tab is in it', () => {
    const markup = render(fromRows([row({ paneId: '%7' })]), { activePane: '%7' })
    expect(activeTags(markup)).toHaveLength(1)
  })

  it('highlights nothing when the selected pane is not in the snapshot', () => {
    // The pane died under the selection: the daemon logged a failed select and
    // the row is simply gone. Nothing is invented in its place.
    const markup = render(fromRows(splitWindow), { activePane: '%99' })
    expect(activeTags(markup)).toHaveLength(0)
    expect(markup).not.toContain('%99')
  })

  it('shows every session group, and says which have lost their own session', () => {
    const markup = render(
      fromRows([
        row({ groupKey: 'work', sessionName: 'work', paneId: '%0' }),
        row({
          groupKey: 'notes',
          sessionName: 'notes',
          paneId: '%1',
          appOwned: true,
        }),
      ]),
    )
    expect(markup).toContain('work')
    expect(markup).toContain('notes')
    expect(markup).toContain('(orphaned)')
  })

  it('refuses clicks into a group with no session of its own left', () => {
    const rows = [
      row({ groupKey: 'work', paneId: '%0' }),
      row({ groupKey: 'notes', paneId: '%1', appOwned: true }),
      row({ groupKey: 'notes', paneId: '%3', paneIndex: 1, appOwned: true, paneActive: false }),
    ]
    const markup = render(fromRows(rows), { activeSession: 'work' })
    // The orphaned group's window row and both of its pane rows are refused;
    // attaching there would 404 and drop a working socket to do it. Nothing in
    // the reachable group is disabled.
    expect(markup.match(/<button[^>]*disabled=""/g) ?? []).toHaveLength(3)
    expect(markup).toContain('only alive because a tab is holding the group open')
  })

  it('still allows clicks in the group this tab is already inside', () => {
    // The user killed the namesake session under an attached tab: the group is
    // app-owned all the way down, but this tab's own socket can still select
    // its panes.
    const rows = [row({ groupKey: 'work', paneId: '%0', appOwned: true })]
    const markup = render(fromRows(rows), { activeSession: 'work' })
    expect(markup).not.toContain('disabled=""')
  })

  it('offers no session to create when tmux has none, since the daemon cannot', () => {
    const markup = render(state({ loaded: true }))
    expect(markup).toContain('No tmux session')
    expect(markup).toContain('tmux new -s work')
  })

  it('shows skeletons, not an empty state, before the first poll answers', () => {
    const markup = render(state({ loaded: false }))
    expect(markup).toContain('data-sidebar="menu-skeleton"')
    expect(markup).not.toContain('No tmux session')
  })

  it('shows the failure only when there is no tree behind it', () => {
    const dead = render(state({ loaded: false, error: 'cannot read the tmux server', trouble: 1 }))
    expect(dead).toContain('Cannot read tmux')
    expect(dead).toContain('cannot read the tmux server')
    expect(dead).toContain('Retry now')

    // With rows in hand, a failed poll leaves them alone and says nothing at
    // all until it has failed enough to matter.
    const withRows = render(fromRows(splitWindow, { error: 'boom', trouble: 1 }))
    expect(withRows).toContain('pane 0')
    expect(withRows).not.toContain('Cannot read tmux')
    expect(withRows).not.toContain('boom')
  })

  it('says the tree is stale without removing it', () => {
    const markup = render(fromRows(splitWindow, { stale: true, error: 'tmux is not answering' }))
    expect(markup).toContain('pane 0')
    expect(markup).toContain('last good state')
    expect(markup).toContain('tmux is not answering')
  })

  it('replaces everything with the enrollment message once the device is revoked', () => {
    const markup = render(state({ unauthorized: true, error: 'unauthorized' }))
    expect(markup).toContain('no longer enrolled')
    expect(markup).not.toContain('Retry now')
  })
})


/**
 * What a pane row says it is doing, and where on the row it says it.
 *
 * With three agents running, `pane_current_command` reads `claude`, `claude`,
 * `claude`; the pane title says what each of them is working on. These pin
 * which of the three candidates -- label, title, command -- reaches the DOM,
 * the line it lands on, and the `title=` attribute that keeps the rest of a cut
 * one reachable.
 *
 * The line is not decoration. A title is a sentence and a command is one word,
 * and putting the sentence back in the word's capsule is exactly the layout
 * this replaced -- so every case below asserts `kind` as well as text.
 */
describe('what a pane row says', () => {
  // What tmux actually reports for a working claude pane, glyph and all, and
  // what the row is expected to show once the agent's own branding comes off --
  // see "the agent's own branding" below for that rule on its own.
  const task = '✳ Categorización productos southafrica'
  const shown = 'Categorización productos southafrica'

  it('puts the pane title on its own line, not in the command capsule', () => {
    const markup = render(fromRows([row({ command: 'claude', title: task })]))
    expect(rowText(markup)).toEqual([
      expect.objectContaining({ kind: 'title', text: shown }),
    ])
    // Not "claude" anywhere: the point of the row is that three agents no
    // longer read alike.
    expect(markup).not.toContain('claude')
  })

  it('shows a plain-ASCII title too, not only one with a glyph in it', () => {
    // Nothing about the rule may key off the `✳` Claude Code happens to lead
    // with: vim, ssh and opencode all write plain words, and a title made of
    // nothing but hostname characters is still a title once it has a space in
    // it.
    const plain = 'fix the login redirect'
    const [said] = rowText(render(fromRows([row({ command: 'claude', title: plain })])))
    expect(said).toMatchObject({ kind: 'title', text: plain })
  })

  it('keeps the command in its capsule when the title is only tmux\'s hostname', () => {
    // Verified on this machine: an untouched pane's title is the hostname. A
    // sidebar that showed it would print the same word down every shell row --
    // and, now, would spend a second line on every one of them to do it.
    for (const host of ['devbox', 'build-box.local', 'dev_laptop', 'desk01']) {
      const markup = render(fromRows([row({ command: 'zsh', title: host })]))
      const [said] = rowText(markup)
      expect(said).toMatchObject({ kind: 'command', text: 'zsh' })
      expect(said.tag).toContain('font-mono')
      // And the row stayed one line: no second line was rendered at all.
      expect(markup).not.toContain('row-line')
    }
  })

  it('keeps the command when there is no title at all', () => {
    const [said] = rowText(render(fromRows([row({ command: 'vim', title: '' })])))
    expect(said).toMatchObject({ kind: 'command', text: 'vim' })
  })

  it('keeps the command when the title only repeats it', () => {
    // The one case where "same as the command" does work the hostname rule
    // would not have done: a command that is not itself hostname-shaped.
    // (`-zsh` is a plausible login shell rather than a verified tmux output --
    // what is pinned is the rule, not the input.) The text is identical
    // whichever branch wins, so the assertion is that the row is still showing
    // a *command* -- monospaced, on the first line, no tooltip -- and has not
    // quietly switched to the title.
    const [said] = rowText(render(fromRows([row({ command: '-zsh', title: '-zsh' })])))
    expect(said).toMatchObject({ kind: 'command', text: '-zsh' })
    expect(said.tag).toContain('font-mono')
    expect(said.tag).not.toContain('title=')
  })

  it('prefers a label the user set over both', () => {
    const markup = render(fromRows([row({ command: 'psql', title: task, label: 'prod db' })]))
    expect(rowText(markup)[0]).toMatchObject({ kind: 'title', text: 'prod db' })
    expect(markup).not.toContain(task)
    expect(markup).not.toContain('psql')
  })

  it('shows a label that a title would not have earned', () => {
    // A label is chosen, not defaulted, so it is not asked to earn the row the
    // way a title is: "notes" is hostname-shaped and shows anyway.
    const [said] = rowText(render(fromRows([row({ command: 'zsh', label: 'notes' })])))
    expect(said).toMatchObject({ kind: 'title', text: 'notes' })
  })

  it('treats a blank label as no label', () => {
    const [said] = rowText(render(fromRows([row({ command: 'zsh', label: '   ' })])))
    expect(said).toMatchObject({ kind: 'command', text: 'zsh' })
  })

  it('shows the activity under the name', () => {
    const markup = render(
      fromRows([row({ command: 'claude', title: task, activity: 'run go test ./...' })]),
    )
    // Above the title, and that ordering is the whole of what this feature is
    // worth: the title says what this pane *is* and is frozen at the first
    // turn, the activity says what the agent is doing now.
    expect(rowText(markup)).toEqual([
      expect.objectContaining({ kind: 'title', text: 'run go test ./...' }),
    ])
    expect(markup).not.toContain(shown)
    // And it takes the second line's whole bargain, not just its slot: clipped
    // in CSS and carried entire in `title=`, which is the whole of what a phone
    // and a reduced-motion reader get in place of the hover marquee. An
    // activity is capped at 128 bytes and the sidebar is 16rem wide, so it is
    // cut about as often as a title is.
    const [said] = rowText(markup)
    expect(said.tag).toContain('row-line')
    expect(said.tag).toContain('title="run go test ./..."')
  })

  it('puts a user label on the first line when there is an activity for the second', () => {
    // Both present. The label is identity and rides with the name; the activity
    // is description and keeps the second line. Neither is dropped -- which is
    // what the old precedence did to whichever one lost.
    const markup = render(
      fromRows([row({ command: 'claude', label: 'prod db', activity: 'run go test ./...' })]),
    )
    expect(rowLabels(markup)).toEqual(['prod db'])
    expect(rowText(markup)).toEqual([
      expect.objectContaining({ kind: 'title', text: 'run go test ./...' }),
    ])
    // The first line now has two things on it and a fixed 16rem to put them
    // in, so the label makes the same bargain the second line does: cut here
    // rather than widening the sidebar, and carried whole in `title=`.
    expect(markup).toMatch(
      /<span data-row-label[^>]*class="[^"]*truncate[^"]*"[^>]*title="prod db"/,
    )
  })

  it('keeps a lone label on the second line, exactly as before', () => {
    // The layout change is scoped to "both exist". A labelled pane with no
    // integration must look exactly as it looks today.
    const markup = render(fromRows([row({ command: 'zsh', label: 'notes' })]))
    expect(rowText(markup)).toEqual([
      expect.objectContaining({ kind: 'title', text: 'notes' }),
    ])
    // And nothing joined the name: not the label on the first line as well as
    // the second, and not an empty first-line slot waiting for one.
    expect(markup).not.toContain('data-row-label')
  })

  it('prefers a blocked question to both', () => {
    const markup = render(
      fromRows([
        row({
          command: 'claude',
          label: 'prod db',
          activity: 'run go test ./...',
          agentState: 'blocked',
          question: { text: 'Do you want to run `rm -rf build`?' },
        }),
      ]),
    )
    expect(rowText(markup)).toEqual([
      expect.objectContaining({ kind: 'title', text: 'Do you want to run `rm -rf build`?' }),
    ])
    // The question takes the whole row, first line included: what it is asking
    // outranks both the name the user gave the pane and the tool call the
    // agent is stopped in front of.
    expect(markup).not.toContain('run go test')
    expect(markup).not.toContain('prod db')
  })

  it('falls back to the title, then the command, when there is no activity', () => {
    // The whole v2 ladder still has to work: most panes will never have an
    // integration, and that is the floor this design degrades to everywhere.
    const titled = render(fromRows([row({ command: 'claude', title: task, activity: '' })]))
    expect(rowText(titled)).toEqual([expect.objectContaining({ kind: 'title', text: shown })])

    const plain = render(fromRows([row({ command: 'zsh', title: 'devbox', activity: '' })]))
    expect(rowText(plain)).toEqual([expect.objectContaining({ kind: 'command', text: 'zsh' })])

    // Whitespace is not an activity either. An empty second line is a worse row
    // than no second line, and the sanitizer can hand back a string that
    // trimmed to nothing.
    const blank = render(fromRows([row({ command: 'claude', title: task, activity: '  ' })]))
    expect(rowText(blank)).toEqual([expect.objectContaining({ kind: 'title', text: shown })])
  })

  it('renders a row identically whichever authority decided its state', () => {
    // `stateSource` is on the wire so that the daemon's tests can tell the two
    // authorities apart. It must never reach the browser's output: two visibly
    // different kinds of state dot teach a user to trust one and ignore the
    // other, which is the badge-integrity failure arriving through a third
    // door. A tooltip saying which is fine; a class is not.
    const base = { command: 'claude', agentState: 'idle', activity: 'run go' }
    const fromEvent = render(fromRows([row({ ...base, stateSource: 'event' })]))
    const fromScreen = render(fromRows([row({ ...base, stateSource: 'screen' })]))
    expect(fromEvent).toBe(fromScreen)
    // Not vacuously equal: the row really is in there.
    expect(fromEvent).toContain('run go')

    // Positive control: something the row IS allowed to change on must differ,
    // or the comparison above is measuring nothing.
    const blocked = render(fromRows([row({ ...base, agentState: 'blocked', stateSource: 'event' })]))
    expect(blocked).not.toBe(fromEvent)
  })

  it('cuts the title in CSS and carries the whole of it in title=', () => {
    const markup = render(fromRows([row({ command: 'claude', title: task })]))
    // The wire caps a title at 256 bytes and the sidebar is 16rem wide, so a
    // real one is always cut. Both halves of that: the hook for the rule that
    // clips it and scrolls it on hover -- `.row-line` in index.css, and all a
    // static render can see of any of it -- and the native tooltip, which is
    // where the rest stays reachable with no hover at all.
    const [said] = rowText(markup)
    expect(said.tag).toContain('row-line')
    expect(said.tag).toContain(`title="${shown}"`)
    // The marquee moves a child of the clip, not the clip: one box cannot both
    // hide its overflow and slide inside itself.
    expect(markup).toContain(`>${shown}</span></span>`)
  })

  it('draws the title quieter than the name it sits under', () => {
    // The window name is the identity and the title is the description, so the
    // title is the one that gives way: dimmer, and a step smaller. Whether that
    // is what the browser actually computes is e2e's question -- see
    // `agents.spec.ts`, which reads it back off a live row.
    const markup = render(fromRows([row({ command: 'claude', title: task })]))
    const [said] = rowText(markup)
    expect(said.tag).toContain('text-sidebar-foreground/60')
    expect(said.tag).toContain('text-xs')
    // And the name above it took neither.
    expect(markup).toMatch(/<span class="truncate">0: shell<\/span>/)
  })

  it('gives every pane of a split window its own line', () => {
    const markup = render(
      fromRows([
        row({ windowIndex: 1, windowName: 'api', paneId: '%4', command: 'claude', title: '✳ one' }),
        row({
          windowIndex: 1,
          windowName: 'api',
          paneId: '%2',
          paneIndex: 1,
          command: 'claude',
          title: '✳ two',
          paneActive: false,
        }),
      ]),
    )
    expect(rowText(markup)).toEqual([
      expect.objectContaining({ kind: 'title', text: 'one' }),
      expect.objectContaining({ kind: 'title', text: 'two' }),
    ])
  })
})
/**
 * The branch chip: which branch the pane's directory is on, right-aligned on
 * the row's first line.
 *
 * Two rules are pinned here and they are the whole point of the block.
 *
 * The first is *presence*. The chip is a function of the pane's directory and
 * of nothing else -- so the four-state test below is four fixtures rather than
 * one loop over a single render, because the mistake it exists to catch is a
 * chip that renders only where the command capsule did not. `paneText` takes
 * its top rung exactly when `agentState === 'blocked'`, so such a chip would be
 * missing from an un-integrated agent's row until the moment it asked a
 * question, then appear, then vanish again when the block cleared -- a row
 * element moving on agent state, which is the one thing this sidebar's rules
 * forbid. Three of the four fixtures below go red on it; the blocked one, which
 * is the state that mistake accidentally gets right, does not.
 *
 * The second is the *width budget*. The two chips share one capped flex box, so
 * they trade against each other instead of each holding a `max-w` it has no
 * room to spend. `Badge`'s own base class string carries `shrink-0`, and a chip
 * that keeps it cannot compress -- the row overflows and
 * `SidebarMenuSubButton`'s `overflow-hidden` clips whichever chip is outermost.
 * Which is why the shrink assertion below is scoped to the pair: a `toContain`
 * over the whole row would find `shrink-0` on the state dot and pass whatever
 * the chips do.
 *
 * No test here can see a computed width -- vitest renders to a string. The
 * 80px group cap is arithmetic against a 150px first line, and it is the number
 * to move if the name floor it was computed from turns out to be wrong.
 */

/** Every element carrying `data-row-branch`, as its opening tag. */
function branchChips(markup: string): string[] {
  return [...markup.matchAll(/<span[^>]*data-row-branch[^>]*>/g)].map((m) => m[0])
}

/** What each branch chip says, in document order. */
function branchText(markup: string): string[] {
  return [...markup.matchAll(/<span[^>]*data-row-branch[^>]*>([^<]*)<\/span>/g)].map((m) => m[1])
}

/**
 * One element's class list, as tokens.
 *
 * Tokens rather than a substring search, because every assertion in this block
 * is about a class that is a prefix or a suffix of another one that is really
 * there: `shrink-0` inside `group-data-[collapsible=icon]:shrink-0`,
 * `border-border` inside `focus-visible:border-ring`, `max-w-16` inside
 * `sm:max-w-16`. A `toContain` on the string would pass on the neighbour.
 */
function classesOf(tag: string): string[] {
  const m = tag.match(/class="([^"]*)"/)
  return m ? m[1].split(/\s+/).filter(Boolean) : []
}

/**
 * The chip pair on each row: its own opening tag, and the markup *inside* it.
 *
 * The inner markup is cut by walking `<span>` depth from the pair's opening tag
 * rather than by a lazy regex, so that the slice stops at the pair's own close
 * and not at the first `</span>` a chip happens to contain. Everything the row
 * puts on its first line is a span, which is what makes the walk sufficient.
 */
function chipPairs(markup: string): { tag: string; inner: string }[] {
  const out: { tag: string; inner: string }[] = []
  for (const m of markup.matchAll(/<span[^>]*data-row-chips[^>]*>/g)) {
    const open = m.index + m[0].length
    let depth = 1
    const re = /<span\b|<\/span>/g
    re.lastIndex = open
    let t: RegExpExecArray | null
    while ((t = re.exec(markup)) !== null) {
      depth += t[0] === '</span>' ? -1 : 1
      if (depth === 0) break
    }
    if (t === null) throw new Error('unbalanced chip pair')
    out.push({ tag: m[0], inner: markup.slice(open, t.index) })
  }
  return out
}

/** The badges inside one chip pair, as their opening tags, in document order. */
function chipsIn(inner: string): string[] {
  return [...inner.matchAll(/<span[^>]*data-slot="badge"[^>]*>/g)].map((m) => m[0])
}

/** The markup inside each row's first line -- the span the name sits in. */
function firstLines(markup: string): string[] {
  const out: string[] = []
  for (const m of markup.matchAll(/<span[^>]*data-row-top[^>]*>/g)) {
    const open = m.index + m[0].length
    let depth = 1
    const re = /<span\b|<\/span>/g
    re.lastIndex = open
    let t: RegExpExecArray | null
    while ((t = re.exec(markup)) !== null) {
      depth += t[0] === '</span>' ? -1 : 1
      if (depth === 0) break
    }
    if (t === null) throw new Error('unbalanced first line')
    out.push(markup.slice(open, t.index))
  }
  return out
}

/** The four values `agentState` can take, and what each one needs to be real. */
const everyState: [string, Partial<SnapshotRow>][] = [
  ['', {}],
  ['working', { agentState: 'working' }],
  // A question, because `paneText`'s blocked rung is taken only when the daemon
  // actually read the dialog -- and it is that rung, not the state, that a
  // capsule-shaped presence rule would key the chip off.
  [
    'blocked',
    { agentState: 'blocked', question: { text: 'Run `rm -rf build`?', choices: ['Yes', 'No'] } },
  ],
  ['idle', { agentState: 'idle' }],
]

describe('the branch chip', () => {
  it.each(everyState)('draws the branch on a %s pane', (_state, over) => {
    const markup = render(fromRows([row({ command: 'claude', branch: 'main', ...over })]))
    expect(branchText(markup)).toEqual(['main'])
  })

  it.each(everyState)('draws no chip on a %s pane outside a work tree', (_state, over) => {
    const markup = render(fromRows([row({ command: 'claude', branch: '', ...over })]))
    expect(branchChips(markup)).toEqual([])
  })

  it('draws the chip on a detached head, which is a directory fact like any other', () => {
    const markup = render(fromRows([row({ branch: '@1a2b3c4' })]))
    expect(branchText(markup)).toEqual(['@1a2b3c4'])
  })

  it('draws the branch beside the capsule, outside it', () => {
    // A plain shell: `paneText` falls to the command, so the capsule is there
    // too and the two have to share the line.
    const markup = render(fromRows([row({ command: 'zsh', branch: 'main' })]))
    const [pair] = chipPairs(markup)
    const chips = chipsIn(pair.inner)
    expect(chips).toHaveLength(2)
    // The capsule first, the branch outermost: the row is clipped from its
    // right edge, and the branch is the one that may go.
    expect(chips[0]).not.toContain('data-row-branch')
    expect(chips[1]).toContain('data-row-branch')
    expect(pair.inner).toContain('>zsh<')
    expect(pair.inner).toContain('>main<')
  })

  it('draws the branch outline where the capsule is filled, so the two do not read alike', () => {
    const markup = render(fromRows([row({ command: 'zsh', branch: 'main' })]))
    const [capsule, chip] = chipsIn(chipPairs(markup)[0].inner)
    // The variants' own distinguishing declarations: `outline` replaces the
    // base `border-transparent` with a visible border and sets no fill;
    // `secondary` fills and leaves the border transparent.
    expect(classesOf(chip)).toContain('border-border')
    expect(classesOf(chip)).not.toContain('bg-secondary')
    expect(classesOf(capsule)).toContain('bg-secondary')
    expect(classesOf(capsule)).not.toContain('border-border')
    // And the relationship, so that making both of them outline is caught even
    // if the literals above ever move.
    expect(classesOf(chip)).not.toEqual(classesOf(capsule))
  })

  it('caps the pair and lets both chips inside it compress', () => {
    const markup = render(fromRows(splitWindow.map((r) => ({ ...r, branch: 'main' }))))
    const [pair] = chipPairs(markup)
    expect(classesOf(pair.tag)).toContain('max-w-20')
    expect(classesOf(pair.tag)).toContain('min-w-0')
    const chips = chipsIn(pair.inner)
    expect(chips).toHaveLength(2)
    for (const chip of chips) {
      expect(classesOf(chip)).toContain('min-w-0')
      expect(classesOf(chip)).toContain('truncate')
      // The one that matters: `Badge`'s base string is `shrink-0`, and a chip
      // that keeps it ignores the cap above entirely. Scoped to the pair -- the
      // tmux-active dot on this same line is `shrink-0` and always will be.
      expect(classesOf(chip)).not.toContain('shrink-0')
    }
  })

  it('caps the branch below the pair, so a long branch cannot evict the capsule', () => {
    const markup = render(fromRows([row({ command: 'zsh', branch: 'release/2026-09-13' })]))
    expect(classesOf(branchChips(markup)[0])).toContain('max-w-16')
  })

  it('puts the chip on the first line, beside the name rather than under it', () => {
    const markup = render(fromRows([row({ command: 'zsh', branch: 'main' })]))
    const [first] = firstLines(markup)
    expect(first).toContain('data-row-branch')
    // And the second line, where a prefixed branch would have been cheaper to
    // fit, does not have it.
    expect(markup.slice(markup.indexOf('row-line'))).not.toContain('data-row-branch')
  })

  it('draws the chip at both row depths, since the branch is not a depth', () => {
    const lone = render(fromRows([row({ branch: 'main' })]))
    expect(lone).not.toContain('data-sidebar="menu-sub"')
    expect(branchText(lone)).toEqual(['main'])

    const split = render(
      fromRows(splitWindow.map((r, i) => ({ ...r, branch: i === 0 ? 'main' : 'topic' }))),
    )
    expect(split).toContain('data-sidebar="menu-sub"')
    expect(branchText(split)).toEqual(['main', 'topic'])
  })

  it('leaves the deeper row the tighter capsule and the shallower row its own', () => {
    // The pair cap is the row's, not the chip's: a pane that is its own window
    // sits one nesting level up and has ~49px more line to spend, so capping it
    // at the split row's 80px would take room the row actually has.
    const lone = chipPairs(render(fromRows([row({ command: 'zsh', branch: 'main' })])))[0]
    const split = chipPairs(
      render(fromRows(splitWindow.map((r) => ({ ...r, branch: 'main' })))),
    )[0]
    expect(classesOf(lone.tag)).toContain('max-w-32')
    expect(classesOf(split.tag)).toContain('max-w-20')
  })
})

/**
 * The agent's own branding, which the row is already saying in the mark beside
 * the text.
 *
 * Every fixture below is a title an agent really writes -- the glyphs and the
 * separators were read off the live panes and out of the shipping binaries, and
 * `AGENT_TITLE_PREFIXES` records where each one came from. What is pinned here
 * is the *narrowness* of the rule, because that is the half that can do damage:
 * the wrong agent, the second prefix, the near-miss glyph and the row that ends
 * up with nothing to say are each their own test.
 */
describe("the agent's own branding", () => {
  /** What the one row of a one-row snapshot says, and on which line. */
  const rowSaid = (over: Partial<SnapshotRow>) => rowText(render(fromRows([row(over)])))[0]

  it('takes claude\'s glyph off the front of the title', () => {
    // Measured: `tmux display -p '#{pane_title}'` on a live claude pane gives
    // `e2 9c b3 20` and then the words.
    const markup = render(fromRows([row({ command: 'claude', title: '✳ Issue 13846 en master' })]))
    expect(rowText(markup)[0]).toMatchObject({ kind: 'title', text: 'Issue 13846 en master' })
    // Nowhere in the row, not even in the tooltip: the tooltip is the overflow
    // of this line, so it has to be the same text.
    expect(markup).not.toContain('✳')
  })

  it("takes opencode's and pi's off too", () => {
    // Both measured the same way. opencode's bar is an ASCII pipe, `4f 43 20 7c
    // 20`, and pi's separator is a hyphen with a space on each side.
    expect(rowSaid({ command: 'opencode', title: 'OC | Revisión de worktrees' })).toMatchObject({
      kind: 'title',
      text: 'Revisión de worktrees',
    })
    expect(rowSaid({ command: 'pi', title: 'π - master' })).toMatchObject({
      kind: 'title',
      text: 'master',
    })
    // opencode's bar with a box-drawing `│` instead: not what the binary
    // writes, but what the report quoted, and one extra table entry is cheaper
    // than finding out a font lied.
    expect(rowSaid({ command: 'opencode', title: 'OC │ Revisión de worktrees' })).toMatchObject({
      kind: 'title',
      text: 'Revisión de worktrees',
    })
  })

  it('covers every frame claude animates that glyph through', () => {
    // The prefix is only worth removing if it stays removed while the agent is
    // working, which is exactly when the row is being looked at. Each of these
    // can lead a title: `✳` when it holds still, `◐ ◑` while it animates, and
    // the older spinner set the report saw. A list that covers only what a
    // parked agent shows would put the glyph back the moment one started.
    for (const glyph of ['✳', '◐', '◑', '·', '✢', '✶', '✻', '✽']) {
      expect(rowSaid({ command: 'claude', title: `${glyph} Compiling the parser` })).toMatchObject({
        kind: 'title',
        text: 'Compiling the parser',
      })
    }
  })

  it('leaves a pane that is not one of Go\'s agents completely alone', () => {
    // Live on this machine: pane %31 runs `zsh` and is still titled `π -
    // browsers` from the pi that ran there. That row carries no mark, so the
    // branding is not redundant with anything -- and the lookup is exact, the
    // way `tmux.KnownAgent` is, so neither `claude-helper` nor `CLAUDE` is
    // claude.
    // `constructor` and `toString` are legal filenames, and a plain-object
    // lookup answers for both of them; the table is asked whether it *owns* the
    // key, so neither one reaches the loop.
    for (const command of [
      'zsh',
      'bash',
      'vim',
      'claude-helper',
      'CLAUDE',
      'Pi',
      'constructor',
      'toString',
    ]) {
      expect(rowSaid({ command, title: 'π - browsers' })).toMatchObject({
        kind: 'title',
        text: 'π - browsers',
      })
      expect(rowSaid({ command, title: '✳ Issue 13846 en master' })).toMatchObject({
        kind: 'title',
        text: '✳ Issue 13846 en master',
      })
    }
  })

  it('takes one prefix off, never two', () => {
    // A doubled glyph is not something an agent writes; it is the shape a
    // greedy rule takes when it meets a title that opens with a real one.
    expect(rowSaid({ command: 'claude', title: '✳ ✻ Issue 13846' })).toMatchObject({
      kind: 'title',
      text: '✻ Issue 13846',
    })
    expect(rowSaid({ command: 'pi', title: 'π - π - master' })).toMatchObject({
      kind: 'title',
      text: 'π - master',
    })
  })

  it('will not strip a prefix a different agent writes', () => {
    expect(rowSaid({ command: 'claude', title: 'π - master' })).toMatchObject({
      kind: 'title',
      text: 'π - master',
    })
    expect(rowSaid({ command: 'pi', title: '✳ Issue 13846' })).toMatchObject({
      kind: 'title',
      text: '✳ Issue 13846',
    })
    expect(rowSaid({ command: 'opencode', title: '✳ Issue 13846' })).toMatchObject({
      kind: 'title',
      text: '✳ Issue 13846',
    })
  })

  it('will not strip something that merely starts like a prefix', () => {
    // `✱` is not `✳`, `•` is not `·`, and neither a glyph run together with the
    // first word nor one followed by punctuation is the branding. Matching is
    // literal and the separator is required, so all four keep what they had.
    for (const title of ['✱ Issue 13846', '• Issue 13846', '✳Issue 13846', '✳: Issue 13846']) {
      expect(rowSaid({ command: 'claude', title })).toMatchObject({ kind: 'title', text: title })
    }
  })

  it('falls back to the command when the title is nothing but branding', () => {
    // A pi between sessions writes `π - ` and nothing else. Stripped, that row
    // has nothing to say, so it takes the fallback the hostname and the echoed
    // command already take -- and, crucially, does not grow a dim empty second
    // line where the title used to be.
    const markup = render(fromRows([row({ command: 'pi', title: 'π - ' })]))
    expect(rowText(markup)[0]).toMatchObject({ kind: 'command', text: 'pi' })
    expect(markup).not.toContain('row-line')
    // The same for a lone claude glyph.
    const claude = render(fromRows([row({ command: 'claude', title: '✳' })]))
    expect(rowText(claude)[0]).toMatchObject({ kind: 'command', text: 'claude' })
    expect(claude).not.toContain('row-line')
  })

  it('judges whether the title earned the row before stripping, not after', () => {
    // `master` on its own is hostname-shaped and would lose to the command; `π
    // - master` is not, and is the whole reason the row exists. Stripping is
    // display only, so the row keeps the one word it had to say.
    expect(rowSaid({ command: 'pi', title: 'π - master' })).toMatchObject({
      kind: 'title',
      text: 'master',
    })
    expect(rowSaid({ command: 'claude', title: '✳ devbox' })).toMatchObject({
      kind: 'title',
      text: 'devbox',
    })
  })

  it('leaves the label alone -- those are the user\'s own words', () => {
    // A label is typed, not generated, so a glyph in one is there because
    // somebody put it there. Stripping applied to whatever `paneText` returns,
    // rather than to the title branch, would edit it.
    expect(rowSaid({ command: 'claude', label: '✳ prod db', title: '✳ Issue 13846' })).toMatchObject(
      { kind: 'title', text: '✳ prod db' },
    )
  })

  it('quotes the blocked question exactly, glyph and all', () => {
    // The daemon quotes the dialog it read; this is the agent asking, not the
    // agent branding itself, and the same one-level-too-high mistake would cut
    // the first word off a question. Shaped like a title on purpose.
    const markup = render(
      fromRows([
        row({
          command: 'claude',
          title: '✳ Issue 13846',
          agentState: 'blocked',
          question: { text: '✳ Overwrite src/main.ts?', choices: ['Yes', 'No'] },
        }),
      ]),
    )
    expect(rowText(markup)[0]).toMatchObject({
      kind: 'title',
      text: '✳ Overwrite src/main.ts?',
    })
  })
})

/**
 * The state dot: which agent needs you, answered without expanding anything.
 *
 * The daemon reports `agentState` and a `finishedAt` timestamp. What reaches
 * the DOM is a dot per agent pane, a roll-up on the window and session rows
 * above it, and -- for `done` alone -- a comparison against what this browser
 * has already been shown.
 */
describe('the state dot', () => {
  const GEN = '1757500000'

  /** One agent pane, with the fields the dot is made of. */
  function agent(over: Partial<SnapshotRow> = {}): SnapshotRow {
    return row({ command: 'claude', agentState: 'idle', ...over })
  }

  it('puts no dot on a pane the daemon computed no state for', () => {
    // "" is not a state: the pane is not a known agent, or the poll was taken
    // with no browser connected. A dot there would be a claim about a pane
    // nothing looked at, and a shell is not idle -- it is a shell.
    expect(dots(render(fromRows([row({ command: 'zsh', agentState: '' })])))).toEqual([])
    // Not even with a finish stamp left over from when it was an agent.
    expect(
      dots(render(fromRows([row({ command: 'zsh', agentState: '', finishedAt: 900 })]))),
    ).toEqual([])
  })

  it("shows the daemon's state on an agent pane", () => {
    for (const state of ['working', 'idle', 'blocked']) {
      // The session row and the window row, which on a single-pane window are
      // both just this pane rolled up.
      expect(dots(render(fromRows([agent({ agentState: state })])))).toEqual([state, state])
    }
  })

  it('gives every pane of a split window its own dot', () => {
    const markup = render(
      fromRows([
        agent({
          windowIndex: 1,
          paneId: '%4',
          paneIndex: 0,
          agentState: 'blocked',
        }),
        agent({
          windowIndex: 1,
          paneId: '%2',
          paneIndex: 1,
          agentState: 'working',
        }),
      ]),
    )
    // The session and window rows roll up first, then each pane in layout
    // order.
    expect(dots(markup)).toEqual(['blocked', 'blocked', 'blocked', 'working'])
  })

  it('rolls the most urgent state up to the window and the session', () => {
    // Three windows, one state each, and neither the first nor the last row is
    // the answer -- so neither "take the first" nor "take the last" passes.
    const markup = render(
      fromRows([
        agent({ windowIndex: 0, paneId: '%0', agentState: 'idle' }),
        agent({ windowIndex: 1, paneId: '%1', agentState: 'blocked' }),
        agent({ windowIndex: 2, paneId: '%2', agentState: 'working' }),
      ]),
    )
    expect(dots(markup)).toEqual(['blocked', 'idle', 'blocked', 'working'])
  })

  it('rolls up the most urgent state, not the least', () => {
    // Every adjacent pair of the order, as a two-pane window. A roll-up that
    // reversed the comparison would report the calmer one every time.
    for (const [urgent, calm] of [
      ['blocked', 'working'],
      ['working', 'idle'],
    ] as const) {
      const markup = render(
        fromRows([
          agent({
            windowIndex: 3,
            paneId: '%8',
            paneIndex: 0,
            agentState: calm,
          }),
          agent({
            windowIndex: 3,
            paneId: '%9',
            paneIndex: 1,
            agentState: urgent,
          }),
        ]),
      )
      // Session, window, then the two panes in layout order.
      expect(dots(markup)).toEqual([urgent, urgent, calm, urgent])
    }
  })

  it('says nothing at all about a session running no agents', () => {
    const markup = render(
      fromRows([row({ command: 'zsh' }), row({ paneId: '%1', paneIndex: 1, command: 'vim' })]),
    )
    expect(dots(markup)).toEqual([])
  })

  it('reads an idle pane with an unseen finish as done', () => {
    const rows = [agent({ paneId: '%3', agentState: 'idle', finishedAt: 900 })]
    const markup = withSeen({}, () => render(fromRows(rows, { serverStart: GEN })))
    expect(dots(markup)).toEqual(['done', 'done'])
  })

  it('reads it as idle once this browser has been shown that finish', () => {
    const rows = [agent({ paneId: '%3', agentState: 'idle', finishedAt: 900 })]
    const markup = withSeen({ [`${GEN}:%3`]: 900 }, () =>
      render(fromRows(rows, { serverStart: GEN })),
    )
    expect(dots(markup)).toEqual(['idle', 'idle'])
  })

  it('ignores a seen entry from a tmux server that has been restarted', () => {
    // Pane ids restart at %0 when the tmux server does. A key without the
    // generation in it -- or a lookup that ignores the generation -- lets a
    // stale %3 suppress the badge on an unrelated new pane, silently.
    const rows = [agent({ paneId: '%3', agentState: 'idle', finishedAt: 900 })]
    const markup = withSeen({ '1757400000:%3': 900, '%3': 900 }, () =>
      render(fromRows(rows, { serverStart: GEN })),
    )
    expect(dots(markup)).toEqual(['done', 'done'])
  })

  it('shows no done badge at all when the daemon sent no generation', () => {
    // "" cannot key a `seen` entry safely, so the honest answer is the state
    // without it. A missing badge costs a glance; a wrong one costs trust.
    const rows = [agent({ paneId: '%3', agentState: 'idle', finishedAt: 900 })]
    const markup = withSeen({}, () => render(fromRows(rows, { serverStart: '' })))
    expect(dots(markup)).toEqual(['idle', 'idle'])
  })

  it('clears the badge on the pane this tab is looking at', () => {
    // Viewing is what marks a pane seen, and it is the only thing that does --
    // no per-device state ever reaches the daemon.
    const rows = [
      agent({
        paneId: '%3',
        paneIndex: 0,
        agentState: 'idle',
        finishedAt: 900,
      }),
      agent({
        paneId: '%4',
        paneIndex: 1,
        agentState: 'idle',
        finishedAt: 900,
      }),
    ]
    const markup = withSeen({}, () =>
      render(fromRows(rows, { serverStart: GEN }), { activePane: '%3' }),
    )
    // Session and window still read done because of %4; %3 is cleared and %4
    // is still asking to be looked at.
    expect(dots(markup)).toEqual(['done', 'done', 'idle', 'done'])
  })

  it('carries a name for each state, since a colour alone is not one', () => {
    const markup = render(fromRows([agent({ agentState: 'blocked' })]))
    expect(markup).toContain('blocked — waiting for an answer')
    expect(markup).toContain('role="img"')
  })
})

/**
 * The agent's own mark, mounted. `AgentIcon` has shipped since Task 11 with
 * nothing rendering it.
 */
describe('the agent mark', () => {
  it('shows the running agent’s mark on a single-pane window row', () => {
    const markup = render(fromRows([row({ command: 'claude', agentState: 'working' })]))
    expect(markup).toContain('aria-label="Claude Code"')
    // In place of the generic terminal glyph, not beside it.
    expect(markup).not.toContain('lucide-square-terminal lucide-terminal-square"')
  })

  it('shows one per pane in a split window', () => {
    const markup = render(
      fromRows([
        row({ windowIndex: 1, paneId: '%4', paneIndex: 0, command: 'claude' }),
        row({
          windowIndex: 1,
          paneId: '%2',
          paneIndex: 1,
          command: 'opencode',
        }),
      ]),
    )
    expect(markup).toContain('aria-label="Claude Code"')
    expect(markup).toContain('aria-label="opencode"')
  })

  it('does not mistake an inherited property for a mark', () => {
    // `'toString' in AGENT_MARKS` is true -- `in` walks the prototype -- and
    // the branch it opens hands AgentIcon a function to draw. Contrived as a
    // pane command, but the check either reads own properties or it does not.
    const markup = render(
      fromRows([
        // A single-pane window, which takes the mark on its own row, and a
        // split one, whose panes take theirs on the sub-rows. Both branches.
        row({ windowIndex: 0, paneId: '%0', command: 'toString' }),
        row({
          windowIndex: 1,
          paneId: '%4',
          paneIndex: 0,
          command: 'toString',
        }),
        row({
          windowIndex: 1,
          paneId: '%2',
          paneIndex: 1,
          command: 'constructor',
        }),
      ]),
    )
    // `fill="currentColor"` is AgentIcon's and nothing else's -- lucide's icons
    // are `fill="none" stroke="currentColor"` -- so this is "no mark was
    // drawn". Under `in`, both rows get an empty one, announced to a screen
    // reader as "toString" and "Object", because a function has a `.name`.
    expect(markup).not.toContain('fill="currentColor"')
    expect(markup).not.toContain('aria-label="toString"')
  })

  it('leaves a pane running something else undecorated', () => {
    const markup = render(
      fromRows([
        row({ windowIndex: 1, paneId: '%4', paneIndex: 0, command: 'zsh' }),
        row({ windowIndex: 1, paneId: '%2', paneIndex: 1, command: 'psql' }),
      ]),
    )
    expect(markup).not.toContain('aria-label="Claude Code"')
    // And no wrapper reserving a 16px gutter for the mark it does not have:
    // with no state and no mark, these rows are exactly what they were before
    // any of this existed.
    expect(markup).not.toContain('relative flex size-4')
    expect(dots(markup)).toEqual([])
  })
})

/**
 * The editor mark: the other thing that is always open in a tmux session.
 *
 * Deliberately *not* an entry in the agents table, and the tests below are
 * mostly about that difference. An agent's mark travels with a state -- a dot
 * that says this pane needs you -- and an editor has no state to travel with:
 * putting one in the agents table would give a text editor a status badge and
 * ask the daemon to start classifying a pane nobody can be blocked in. Agents
 * are things with state; this is identity and nothing else.
 */
describe('the editor mark', () => {
  it('marks a window running an editor', () => {
    const markup = render(fromRows([row({ command: 'vim' })]))
    expect(markup).toContain('aria-label="Vim"')
    // In place of the generic terminal glyph, not beside it.
    expect(markup).not.toContain('lucide-square-terminal lucide-terminal-square"')
  })

  it('marks each pane of a split window', () => {
    const markup = render(
      fromRows([
        row({ windowIndex: 1, paneId: '%4', paneIndex: 0, command: 'vim' }),
        row({ windowIndex: 1, paneId: '%2', paneIndex: 1, command: 'nvim', paneActive: false }),
      ]),
    )
    expect(markup).toContain('aria-label="Vim"')
    expect(markup).toContain('aria-label="Neovim"')
  })

  it('never puts a state dot on an editor', () => {
    // The whole reason this is not another row in the agents table. An editor
    // is not working, is not blocked and cannot be idle; a dot here would be a
    // claim about a pane the daemon never looked at, on the one signal in the
    // sidebar that has to stay worth trusting.
    const alone = render(fromRows([row({ command: 'nvim' })]))
    expect(alone).toContain('aria-label="Neovim"')
    expect(dots(alone)).toEqual([])
    // And no wrapper reserving the corner a dot would ride in.
    expect(alone).not.toContain('relative flex size-4')

    // The same render, beside an agent that does have one, so the emptiness
    // above is the editor's and not the fixture's.
    const beside = render(
      fromRows([
        row({ windowIndex: 0, paneId: '%0', command: 'vim' }),
        row({ windowIndex: 1, paneId: '%1', command: 'claude', agentState: 'working' }),
      ]),
    )
    expect(beside).toContain('aria-label="Vim"')
    // The session roll-up and the agent's own window row. The editor window in
    // between contributes nothing.
    expect(dots(beside)).toEqual(['working', 'working'])
  })

  it('reserves no gutter for a mark that is not there', () => {
    // `paneMark` answers null, not an element that renders nothing: an element
    // returning null is still an element, and `RowIcon` would wrap it and hang
    // the state dot off its corner -- moving the dot 16px right on every pane
    // row running something with no mark at all. Visible only where there is a
    // dot to move, hence the states, and only on the pane rows: the window row
    // above them has an icon of its own either way, and its wrapper is the one
    // occurrence these counts expect.
    const panes = (a: string, b: string) =>
      fromRows([
        row({ windowIndex: 1, paneId: '%4', paneIndex: 0, command: a, agentState: 'working' }),
        row({
          windowIndex: 1,
          paneId: '%2',
          paneIndex: 1,
          command: b,
          agentState: 'working',
          paneActive: false,
        }),
      ])
    const gutters = (markup: string) => markup.match(/relative flex size-4/g)?.length ?? 0

    const bare = render(panes('psql', 'toString'))
    expect(markNames(bare)).toEqual([])
    expect(gutters(bare)).toBe(1)
    // Not vacuous: the dots are all there, and a row that *does* have a mark
    // gets exactly the wrapper this one is refusing.
    expect(dots(bare)).toEqual(['working', 'working', 'working', 'working'])
    expect(gutters(render(panes('claude', 'vim')))).toBe(3)
  })

  it('leaves the agent marks alone', () => {
    // Two tables, looked up in order, and an agent is still an agent: nothing
    // about adding editors may reach a row that had a mark already.
    const markup = render(fromRows([row({ command: 'claude', agentState: 'blocked' })]))
    expect(markup).toContain('aria-label="Claude Code"')
    expect(markup).not.toContain('aria-label="Vim"')
    expect(dots(markup)).toEqual(['blocked', 'blocked'])
  })

  it('does not mistake an inherited property for an editor', () => {
    // `'toString' in EDITOR_MARKS` is true, and the branch it opens draws a
    // mark on a pane running a command nothing has a mark for.
    const markup = render(
      fromRows([
        row({ windowIndex: 0, paneId: '%0', command: 'toString' }),
        row({ windowIndex: 1, paneId: '%4', paneIndex: 0, command: 'constructor' }),
        row({ windowIndex: 1, paneId: '%2', paneIndex: 1, command: 'valueOf' }),
      ]),
    )
    expect(markNames(markup)).toEqual([])
  })

  it('is exact about which command is an editor', () => {
    // The lookup is the same exact match the agent table uses: an editor is
    // the binary named in the table and nothing that merely looks like it.
    for (const command of ['vi', 'VIM', 'nvim-qt', 'neovim', 'vimdiff', 'gvim']) {
      const markup = render(fromRows([row({ command })]))
      expect(markNames(markup), command).toEqual([])
    }
  })
})

/**
 * What a blocked agent is asking. The state says one needs you; this says what
 * for, which is what decides whether it is worth switching to.
 */
describe('the blocked question', () => {
  const question = {
    text: 'Do you want to run `rm -rf build`?',
    choices: ['Yes', "Yes, and don't ask again", 'No'],
  }

  it('puts the question in the row, ahead of the title', () => {
    const markup = render(
      fromRows([
        row({
          command: 'claude',
          title: '✳ Fixing the build',
          agentState: 'blocked',
          question,
        }),
      ]),
    )
    expect(rowText(markup)[0]).toMatchObject({
      kind: 'title',
      text: 'Do you want to run `rm -rf build`?',
    })
    expect(markup).not.toContain('✳ Fixing the build')
    // And the row is not describing itself as a program: the question took the
    // line, so there is no capsule on this row at all.
    expect(rowText(markup).map((r) => r.kind)).toEqual(['title'])
  })

  it('puts the choices in the tooltip, where they fit', () => {
    const markup = render(fromRows([row({ command: 'claude', agentState: 'blocked', question })]))
    const [said] = rowText(markup)
    expect(said.tag).toContain('Yes, and don&#x27;t ask again')
    expect(said.tag).toContain('No')
  })

  it('outranks even a label the user set, since blocked is the whole point', () => {
    const markup = render(
      fromRows([
        row({
          command: 'claude',
          label: 'prod db',
          agentState: 'blocked',
          question,
        }),
      ]),
    )
    expect(rowText(markup)[0]).toMatchObject({
      kind: 'title',
      text: 'Do you want to run `rm -rf build`?',
    })
  })

  it('keeps the row it had when the daemon could not read the dialog', () => {
    // Detection and extraction are separate on purpose: a restyled approval box
    // costs the quote, never the state.
    const markup = render(
      fromRows([
        row({
          command: 'claude',
          title: '✳ Fixing the build',
          agentState: 'blocked',
        }),
      ]),
    )
    expect(rowText(markup)[0]).toMatchObject({ kind: 'title', text: 'Fixing the build' })
    expect(dots(markup)).toEqual(['blocked', 'blocked'])
  })

  it('ignores a question on a pane that is not blocked', () => {
    const markup = render(
      fromRows([
        row({
          command: 'claude',
          title: '✳ Fixing the build',
          agentState: 'working',
          question,
        }),
      ]),
    )
    expect(rowText(markup)[0]).toMatchObject({ kind: 'title', text: 'Fixing the build' })
  })
})

/**
 * The session row's own text, which is the one place a rename has to show up.
 */
describe('the session row', () => {
  it('shows the live session name, not the group key tmux froze', () => {
    // Renaming `work3` to `api` leaves group=work3 on every row forever. A
    // sidebar labelled on the group key shows `work3` until the session dies,
    // so Task 8's rename appears to do nothing at all.
    const markup = render(fromRows([row({ groupKey: 'work3', sessionName: 'api' })]))
    expect(markup).toContain('>api<')
    expect(markup).not.toContain('work3')
  })

  it('still keys and addresses the session on the group', () => {
    // The name is display only. `?session=` and the click handler both carry
    // the group key, and the highlight compares against it.
    const markup = render(fromRows([row({ groupKey: 'work3', sessionName: 'api' })]), {
      activeSession: 'work3',
    })
    // The active session's label is the emphasised one.
    expect(markup).toMatch(/text-sidebar-foreground"[^>]*>(<[^>]*>)*<span class="truncate">api</)
  })
})

/**
 * A window still wearing the name of the shell that created it.
 *
 * `automatic-rename off` stops tmux overwriting a name you chose, which is why
 * it is set; the side effect is that every window never named by hand keeps its
 * shell's name forever, so the line whose job is to identify a window running
 * an agent reads `0: zsh`.
 *
 * What is pinned below is mostly the *narrowness* of the rule, because that is
 * the half that can do damage: a false positive silently replaces a name
 * somebody chose on purpose.
 */
describe('a window named after its shell', () => {
  /** Each window row's first line, `index: name`, in document order. */
  const windowNames = (markup: string) =>
    [...markup.matchAll(/<span class="truncate">(\d+: [^<]*)<\/span>/g)].map((m) => m[1])

  /** A two-pane window named after its shell, one pane of which tmux calls active. */
  const split = (active: 0 | 1 | null) => [
    row({
      windowIndex: 1,
      windowName: 'zsh',
      paneId: '%4',
      paneIndex: 0,
      command: 'vim',
      paneActive: active === 0,
    }),
    row({
      windowIndex: 1,
      windowName: 'zsh',
      paneId: '%2',
      paneIndex: 1,
      command: 'claude',
      paneActive: active === 1,
    }),
  ]

  it('shows what the window is running in place of the shell that named it', () => {
    const markup = render(
      fromRows([row({ windowName: 'zsh', command: 'claude', activity: 'run go test ./...' })]),
    )
    expect(windowNames(markup)).toEqual(['0: claude'])
    expect(markup).not.toContain('0: zsh')
    // And it borrowed the command, not the second line: the activity is still
    // the activity, where it was.
    expect(rowText(markup)).toEqual([
      expect.objectContaining({ kind: 'title', text: 'run go test ./...' }),
    ])
  })

  it('covers every shell a window can end up named after', () => {
    // The set is the whole rule. Emptied, or short by one, the row it was
    // written for goes back to identifying nothing -- and says so nowhere.
    for (const shell of ['zsh', 'bash', 'fish', 'sh', 'dash', 'ksh', 'tcsh', 'csh']) {
      const markup = render(fromRows([row({ windowName: shell, command: 'claude' })]))
      expect(windowNames(markup), shell).toEqual(['0: claude'])
    }
  })

  it('leaves a name a human chose exactly alone', () => {
    // Matching is equality on the whole name, not a search inside it, and it is
    // case-sensitive and untrimmed: every one of these is a name somebody typed,
    // and replacing one would be this feature doing the damage it exists to
    // undo.
    for (const name of [
      'api',
      'zsh-notes',
      'my zsh',
      'bashful',
      'fish tank',
      'Zsh',
      'ZSH',
      'zsh ',
    ]) {
      const markup = render(fromRows([row({ windowName: name, command: 'claude' })]))
      expect(windowNames(markup), name).toEqual([`0: ${name}`])
      expect(markup, name).not.toContain('0: claude')
    }
  })

  it('accepts the one case it cannot tell apart', () => {
    // A window a human deliberately named `zsh` is indistinguishable from one
    // tmux named that way, because tmux keeps no record of which. It loses its
    // name. Documented and accepted: the machinery to tell the two apart does
    // not exist, and the row it costs is one the user can rename back.
    const markup = render(fromRows([row({ windowName: 'zsh', command: 'vim' })]))
    expect(windowNames(markup)).toEqual(['0: vim'])
  })

  it('changes nothing about a window that really is a shell', () => {
    // The name and the command are the same word, so there is nothing new to
    // say and the row is exactly the row it was -- capsule included.
    const markup = render(fromRows([row({ windowName: 'zsh', command: 'zsh', title: 'devbox' })]))
    expect(windowNames(markup)).toEqual(['0: zsh'])
    expect(rowText(markup)).toEqual([expect.objectContaining({ kind: 'command', text: 'zsh' })])
  })

  it('does not say the same word twice on one line', () => {
    // The capsule on the right of the first line is there to say which program
    // this is. Once the name is saying it, the capsule is that word again, an
    // inch away.
    const markup = render(fromRows([row({ windowName: 'zsh', command: 'vim', title: 'devbox' })]))
    expect(windowNames(markup)).toEqual(['0: vim'])
    expect(rowText(markup)).toEqual([])
    // Control: a window with a name of its own keeps its capsule, because there
    // the name and the capsule are two different answers.
    const named = render(fromRows([row({ windowName: 'edit', command: 'vim', title: 'devbox' })]))
    expect(rowText(named)).toEqual([expect.objectContaining({ kind: 'command', text: 'vim' })])
  })

  it('keeps the shell name when there is no command to put in its place', () => {
    const markup = render(fromRows([row({ windowName: 'zsh', command: '' })]))
    expect(windowNames(markup)).toEqual(['0: zsh'])
  })

  it('shows the pane a click on the row would land on when the window is split', () => {
    // A split window's row is not a pane, and its panes have rows of their own
    // directly underneath -- naming them all here would print that list twice.
    // The pane the row's own click selects is the one answer that cannot
    // disagree with what the row does, because it is the same call that decides
    // it.
    expect(windowNames(render(fromRows(split(1))))).toEqual(['1: claude'])
    // Not "take the first pane": the active one is second here.
    expect(windowNames(render(fromRows(split(0))))).toEqual(['1: vim'])
    // And the panes still say what they are, underneath.
    expect(render(fromRows(split(1)))).toContain('pane 1')
  })

  it('falls back to the first pane when tmux calls none of them active', () => {
    expect(windowNames(render(fromRows(split(null))))).toEqual(['1: vim'])
  })

  it('is gone the moment the window has a name of its own', () => {
    // Keyed off the name in this poll and on nothing else, so the poll that
    // reports the rename is the poll that drops the fallback. Nothing is
    // remembered and nothing has to be cleared.
    expect(windowNames(render(fromRows([row({ windowName: 'zsh', command: 'claude' })])))).toEqual([
      '0: claude',
    ])
    expect(windowNames(render(fromRows([row({ windowName: 'api', command: 'claude' })])))).toEqual([
      '0: api',
    ])
  })

  it('renames the window tmux has, not the word the row shows', () => {
    // Display only. A Radix menu is portalled and never mounts in a static
    // render -- see `management affordances` -- so what the menu would offer is
    // asked of `rowMenu` directly, over the very tree the sidebar just
    // rendered: a rename that targeted the displayed name would offer `claude`
    // as the initial value and rename the window to what it is running. What
    // this reaches is the tree and `rowMenu` over it, which is where a fallback
    // written one layer too low would show up; that the *component* hands the
    // menu this node and not a doctored copy of it is e2e's to see, and
    // `manage.spec.ts` says so in its own header.
    const snapshot = fromRows(split(1))
    expect(windowNames(render(snapshot))).toEqual(['1: claude'])

    const session = snapshot.groups[0]
    const entry = rowMenu(rowTargetForWindow(session, session.windows[0]), 'work').find(
      (e) => e.id === 'rename-window',
    )
    if (entry?.intent.kind !== 'prompt') throw new Error('no rename-window prompt on the row')
    expect(entry.label).toBe('Rename window "zsh"…')
    expect(entry.intent.prompt.fields[0].initial).toBe('zsh')
    // And the tree the row was drawn from still carries the real name, so
    // everything else addressed off it -- the kill dialog, the palette -- is
    // addressing the window tmux has.
    expect(session.windows[0].name).toBe('zsh')
  })
})

/**
 * Management: the menu on every row, and the two `+` buttons that exist for the
 * case with no row to open a menu on.
 *
 * A static render cannot open a Radix context menu -- the content is portalled
 * and only mounts when it is opened -- so what is pinned here is that every row
 * *carries* a trigger, and that `rowMenu` (tested in lib/manage.test.ts)
 * decides what goes in it. Opening one is the Playwright run's job.
 */
describe('management affordances', () => {
  it('puts a context menu on the session, the window and each pane', () => {
    const markup = render(
      fromRows([row({ paneId: '%0' }), row({ paneId: '%1', paneIndex: 1, paneActive: false })]),
    )
    // Session label, window row, and one per pane. Radix's own trigger handles
    // both the right-click and the 700ms long-press on touch.
    expect(markup.match(/data-slot="context-menu-trigger"/g)).toHaveLength(4)
  })

  it('leaves no menu on a group the daemon refuses to touch', () => {
    // Every entry on an app-owned group would be a refusal waiting to happen,
    // and a menu of disabled rows is worse than no menu.
    const markup = render(fromRows([row({ groupKey: 'dead', appOwned: true })]), {
      activeSession: 'dead',
    })
    // The window and its lone pane still have theirs; the session label does not.
    expect(markup.match(/data-slot="context-menu-trigger"/g)).toHaveLength(1)
  })

  it('offers a new session from the header, whatever the tree looks like', () => {
    expect(render(fromRows([row()]))).toContain('aria-label="New session"')
    // And with no tmux server at all, which is the case it exists for: there is
    // no row to right-click, and v1 could not fix that from the browser.
    const empty = render(state({ loaded: true }))
    expect(empty).toContain('aria-label="New session"')
    expect(empty).toContain('New session')
    // The v1 copy sent you to SSH because there was no endpoint. There is one.
    expect(empty).not.toContain('and it appears here within a')
  })

  it('offers a new window from the session row it would go in', () => {
    const markup = render(fromRows([row({ groupKey: 'work3', sessionName: 'api' })]))
    expect(markup).toContain('aria-label="New window in &quot;api&quot;"')
  })

  it('offers no new window on an orphaned group', () => {
    // Its only session is this app's throwaway one; a window created in it dies
    // with the tab.
    const markup = render(fromRows([row({ groupKey: 'dead', appOwned: true })]))
    expect(markup).not.toContain('New window in')
  })
})

/**
 * The one list that decides who is an agent lives in Go, and this file's
 * branding table has to stay under it.
 *
 * Same invariant, and the same one-directional reading, as
 * `AgentIcon.test.tsx`: `tmux.Agents` gates capture, state and mark together,
 * so a command Go does not call an agent must never have its title edited here.
 * A *subset*, not equality -- Go may name an agent whose branding has not been
 * established (e2e runs a scripted fake one), and that pane correctly keeps its
 * title whole.
 */
describe('contract with the daemon', () => {
  it('only strips prefixes from commands Go classifies as agents', () => {
    const source = readFileSync(new URL('../../../internal/tmux/agent.go', import.meta.url), 'utf8')
    const m = source.match(/Agents\s*=\s*\[\]string\{([^}]*)\}/)
    if (!m) throw new Error('Agents not found in internal/tmux/agent.go')
    const agents = [...m[1].matchAll(/"([^"]*)"/g)].map((q) => q[1])
    expect(agents.length).toBeGreaterThan(0)
    for (const command of Object.keys(AGENT_TITLE_PREFIXES)) {
      expect(agents).toContain(command)
    }
  })

  it('gives every agent it knows a prefix that could actually match', () => {
    // An empty string is a prefix of everything, and a table entry that is one
    // would silently strip nothing while looking like it strips everything.
    for (const [command, prefixes] of Object.entries(AGENT_TITLE_PREFIXES)) {
      expect(prefixes.length, command).toBeGreaterThan(0)
      for (const prefix of prefixes) {
        expect(prefix, command).not.toBe('')
        // The separator belongs to the rule, not the table -- carrying one here
        // would stop `withoutAgentPrefix` recognising a title trimmed down to
        // nothing but its branding.
        expect(prefix, command).toBe(prefix.trim())
      }
    }
  })
})
