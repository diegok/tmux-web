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
import { groupRows } from '@/lib/useSnapshot'
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
    windowIndex: 0,
    windowName: 'shell',
    paneActive: true,
    command: 'zsh',
    // The default is what tmux gives an untouched pane -- the hostname -- so
    // every test that does not care about titles is still rendering the case
    // the sidebar has to keep out of the rows.
    title: 'devbox',
    ...over,
  }
}

function state(over: Partial<SnapshotState> = {}): SnapshotState {
  return {
    groups: [],
    rows: [],
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

function render(
  snapshot: SnapshotState,
  over: { activePane?: string | null; activeSession?: string | null } = {},
): string {
  return renderToStaticMarkup(
    <SidebarProvider>
      <AppSidebar
        snapshot={snapshot}
        activePane={over.activePane ?? null}
        activeSession={over.activeSession ?? 'work'}
        onSelectPane={() => {}}
        onRefresh={() => {}}
      />
    </SidebarProvider>,
  )
}

/** Every element carrying data-active="true", as its opening tag. */
function activeTags(markup: string): string[] {
  return [...markup.matchAll(/<[^>]*data-active="true"[^>]*>/g)].map((m) => m[0])
}

/** Every badge, in document order: its opening tag and the text inside it. */
function badges(markup: string): { tag: string; text: string }[] {
  return [...markup.matchAll(/(<span[^>]*data-slot="badge"[^>]*>)([^<]*)<\/span>/g)].map((m) => ({
    tag: m[1],
    text: m[2],
  }))
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
        row({ groupKey: 'work', paneId: '%0' }),
        row({ groupKey: 'notes', paneId: '%1', appOwned: true }),
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
 * The badge, which is the only field on a row that says what a pane is doing.
 *
 * With three agents running, `pane_current_command` reads `claude`, `claude`,
 * `claude`; the pane title says what each of them is working on. These pin
 * which of the three candidates -- label, title, command -- reaches the DOM,
 * and the `title=` attribute that keeps the rest of a truncated one reachable.
 */
describe('the pane badge', () => {
  const task = '✳ Categorización productos southafrica'

  it('shows the pane title instead of the command', () => {
    const markup = render(fromRows([row({ command: 'claude', title: task })]))
    const [badge] = badges(markup)
    expect(badge.text).toBe(task)
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
    const [badge] = badges(render(fromRows([row({ command: 'claude', title: plain })])))
    expect(badge.text).toBe(plain)
  })

  it('keeps the command when the title is only the hostname tmux defaults to', () => {
    // Verified on this machine: an untouched pane's title is the hostname. A
    // sidebar that showed it would print the same word down every shell row.
    for (const host of ['devbox', 'dev-box.local', 'x1_carbon', 'w520']) {
      const [badge] = badges(render(fromRows([row({ command: 'zsh', title: host })])))
      expect(badge.text).toBe('zsh')
      expect(badge.tag).toContain('font-mono')
    }
  })

  it('keeps the command when there is no title at all', () => {
    const [badge] = badges(render(fromRows([row({ command: 'vim', title: '' })])))
    expect(badge.text).toBe('vim')
  })

  it('keeps the command when the title only repeats it', () => {
    // The one case where "same as the command" does work the hostname rule
    // would not have done: a command that is not itself hostname-shaped.
    // (`-zsh` is a plausible login shell rather than a verified tmux output --
    // what is pinned is the rule, not the input.) The text is identical
    // whichever branch wins, so the assertion is that the row is still showing
    // a *command* -- monospaced, no tooltip -- and has not quietly switched to
    // the title.
    const [badge] = badges(render(fromRows([row({ command: '-zsh', title: '-zsh' })])))
    expect(badge.text).toBe('-zsh')
    expect(badge.tag).toContain('font-mono')
    expect(badge.tag).not.toContain('title=')
  })

  it('prefers a label the user set over both', () => {
    const markup = render(fromRows([row({ command: 'psql', title: task, label: 'prod db' })]))
    const [badge] = badges(markup)
    expect(badge.text).toBe('prod db')
    expect(markup).not.toContain(task)
    expect(markup).not.toContain('psql')
  })

  it('shows a label that a title would not have earned', () => {
    // A label is chosen, not defaulted, so it is not asked to earn the row the
    // way a title is: "notes" is hostname-shaped and shows anyway.
    const [badge] = badges(render(fromRows([row({ command: 'zsh', label: 'notes' })])))
    expect(badge.text).toBe('notes')
  })

  it('treats a blank label as no label', () => {
    const [badge] = badges(render(fromRows([row({ command: 'zsh', label: '   ' })])))
    expect(badge.text).toBe('zsh')
  })

  it('truncates in CSS and carries the full text in title=', () => {
    const markup = render(fromRows([row({ command: 'claude', title: task })]))
    // The wire caps a title at 256 bytes and the sidebar is 16rem wide, so the
    // badge always truncates a real one. Both halves of that: the class that
    // clips it -- all a static render can see of the layout -- and the native
    // tooltip where the rest of it stays reachable.
    expect(badges(markup)[0].tag).toContain('truncate')
    expect(badges(markup)[0].tag).toContain(`title="${task}"`)
  })

  it('gives every pane of a split window its own badge', () => {
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
    expect(badges(markup).map((b) => b.text)).toEqual(['✳ one', '✳ two'])
  })
})
