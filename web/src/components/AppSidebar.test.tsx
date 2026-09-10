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

import { renderToStaticMarkup } from 'react-dom/server'
import { describe, expect, it } from 'vitest'

import { AppSidebar } from './AppSidebar'
import { SidebarProvider } from '@/components/ui/sidebar'
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
    // A shell: the daemon computes no state for it, and "" is not a state.
    agentState: '',
    finishedAt: 0,
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
    'wterm-web:seen': JSON.stringify(seen),
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
  const task = '✳ Categorización productos southafrica'

  it('puts the pane title on its own line, not in the command capsule', () => {
    const markup = render(fromRows([row({ command: 'claude', title: task })]))
    expect(rowText(markup)).toEqual([
      expect.objectContaining({ kind: 'title', text: task }),
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
    for (const host of ['devbox', 'dev-box.local', 'x1_carbon', 'w520']) {
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

  it('cuts the title in CSS and carries the whole of it in title=', () => {
    const markup = render(fromRows([row({ command: 'claude', title: task })]))
    // The wire caps a title at 256 bytes and the sidebar is 16rem wide, so a
    // real one is always cut. Both halves of that: the hook for the rule that
    // clips it and scrolls it on hover -- `.row-line` in index.css, and all a
    // static render can see of any of it -- and the native tooltip, which is
    // where the rest stays reachable with no hover at all.
    const [said] = rowText(markup)
    expect(said.tag).toContain('row-line')
    expect(said.tag).toContain(`title="${task}"`)
    // The marquee moves a child of the clip, not the clip: one box cannot both
    // hide its overflow and slide inside itself.
    expect(markup).toContain(`>${task}</span></span>`)
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
      expect.objectContaining({ kind: 'title', text: '✳ one' }),
      expect.objectContaining({ kind: 'title', text: '✳ two' }),
    ])
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
        row({ windowIndex: 1, paneId: '%2', paneIndex: 1, command: 'vim' }),
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
    expect(rowText(markup)[0]).toMatchObject({ kind: 'title', text: '✳ Fixing the build' })
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
    expect(rowText(markup)[0]).toMatchObject({ kind: 'title', text: '✳ Fixing the build' })
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
