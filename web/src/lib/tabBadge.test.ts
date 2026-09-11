/**
 * What the tab says about panes you are not looking at.
 *
 * Everything here is a plain function over a snapshot's rows, which is the
 * point of the split: the two effects in tabBadge.ts do nothing but write what
 * `tabBadge` decided onto `document`, and this suite has no DOM by design (see
 * the note on `useTabBadge`).
 */

import { readFileSync } from 'node:fs'

import { describe, expect, it } from 'vitest'

import {
  BASE_TITLE,
  DOT_COLOR,
  FAVICON_URL,
  attention,
  badgedIcon,
  iconDataUrl,
  tabBadge,
  tabTitle,
} from './tabBadge'
import type { SeenMap, SnapshotRow } from './useSnapshot'

const webFile = (path: string) => readFileSync(new URL(`../../${path}`, import.meta.url), 'utf8')

function row(over: Partial<SnapshotRow> = {}): SnapshotRow {
  return {
    groupKey: 'work',
    sessionId: '$0',
    sessionName: 'work',
    paneId: '%0',
    paneIndex: 0,
    appOwned: false,
    label: '',
    windowId: '@0',
    windowIndex: 0,
    windowName: 'shell',
    paneActive: true,
    command: 'claude',
    title: 'devbox',
    // The pane's working directory, as of the poll; nothing renders it yet.
    path: '/srv/work',
    agentState: '',
    finishedAt: 0,
    // Nothing reported for this pane and nothing decided its state: "" in
    // both, which is where every pane that is not an agent sits.
    activity: '',
    stateSource: '',
    question: undefined,
    ...over,
  }
}

/** A tmux server generation. Non-empty: with "" no pane can read `done` at all. */
const GEN = '1757500000'

/** A stand-in for the real icon, small enough to assert against whole. */
const ICON = '<svg viewBox="0 0 48 46"><path d="M0 0h48v46H0z"/></svg>'

// --- what the page ships ----------------------------------------------------
//
// Both constants are also written in index.html, so they are pinned against it
// rather than against a copy of themselves: renaming the app there would
// otherwise leave the badge rewriting the title to the old name on the first
// poll, and moving the icon would leave it restoring a href that 404s.

describe('contract with index.html', () => {
  const html = webFile('index.html')

  it('badges the title the page was served with', () => {
    expect(html).toContain(`<title>${BASE_TITLE}</title>`)
  })

  it('draws on the icon the page links, and restores that same href', () => {
    expect(html).toContain(`href="${FAVICON_URL}"`)
    expect(tabBadge({ count: 0, state: '' }, ICON).icon).toBe(FAVICON_URL)
  })

  it('can badge the real favicon', () => {
    // The shipped icon is one <svg> with a <defs> at the end; the dot goes
    // after all of it. If that ever stops being true this is where it shows up.
    const badged = badgedIcon(webFile('public/favicon.svg'), 'blocked')
    expect(badged).toContain(DOT_COLOR.blocked)
    expect(badged?.endsWith('</svg>')).toBe(true)
    expect([...(badged ?? '').matchAll(/<svg/g)]).toHaveLength(1)
  })
})

// --- what counts ------------------------------------------------------------

describe('attention', () => {
  it('counts blocked and done, and nothing else', () => {
    // The two states that mean "go back to it". A working agent is the state
    // you left it in, and a shell -- "" from the daemon -- is not a state.
    const rows = [
      row({ paneId: '%0', agentState: 'blocked' }),
      row({ paneId: '%1', agentState: 'idle', finishedAt: 1000 }),
      row({ paneId: '%2', agentState: 'working' }),
      row({ paneId: '%3', agentState: 'idle' }),
      row({ paneId: '%4', agentState: '', command: 'zsh' }),
    ]
    expect(attention(rows, GEN, {})).toEqual({ count: 2, state: 'blocked' })
  })

  it('counts a finish this device has not been shown, and stops when it has', () => {
    // `done` is the browser's own answer, not the daemon's: the phone keeps its
    // badge after the laptop has cleared its own, so `seen` decides.
    const rows = [row({ paneId: '%3', agentState: 'idle', finishedAt: 1000 })]
    expect(attention(rows, GEN, {})).toEqual({ count: 1, state: 'done' })
    const seen: SeenMap = { [`${GEN}:%3`]: 1000 }
    expect(attention(rows, GEN, seen)).toEqual({ count: 0, state: '' })
  })

  it('refuses to count done with no tmux server generation to key it on', () => {
    // Same rule as the sidebar's badge: without a generation, a remembered %3
    // from the previous tmux server would suppress -- or invent -- a badge on
    // an unrelated new pane.
    const rows = [row({ agentState: 'idle', finishedAt: 1000 })]
    expect(attention(rows, '', {})).toEqual({ count: 0, state: '' })
    // A blocked pane needs no memory, so it is still counted.
    expect(attention([row({ agentState: 'blocked' })], '', {})).toEqual({
      count: 1,
      state: 'blocked',
    })
  })

  it('ranks blocked above done however the rows arrive', () => {
    const done = row({ paneId: '%1', agentState: 'idle', finishedAt: 1000 })
    const blocked = row({ paneId: '%2', agentState: 'blocked' })
    expect(attention([done, blocked], GEN, {}).state).toBe('blocked')
    expect(attention([blocked, done], GEN, {}).state).toBe('blocked')
  })

  it('says nothing at all about an empty snapshot', () => {
    expect(attention([], GEN, {})).toEqual({ count: 0, state: '' })
  })
})

// --- the title --------------------------------------------------------------

describe('tabTitle', () => {
  it('leads with the count, where a cropped tab strip still shows it', () => {
    expect(tabTitle(1)).toBe('(1) tmux-web')
    expect(tabTitle(12)).toBe('(12) tmux-web')
  })

  it('is the plain name when nothing needs you', () => {
    expect(tabTitle(0)).toBe(BASE_TITLE)
    expect(tabTitle(0)).not.toContain('(')
  })
})

// --- the icon ---------------------------------------------------------------

describe('badgedIcon', () => {
  it('draws the dot on top of the icon, keeping the icon', () => {
    const badged = badgedIcon(ICON, 'blocked')
    // Everything that was there, plus a circle, and the circle last so that it
    // is painted over the glyph rather than under it.
    expect(badged).toBe(
      `<svg viewBox="0 0 48 46"><path d="M0 0h48v46H0z"/><circle cx="34" cy="32" r="13" fill="${DOT_COLOR.blocked}"/></svg>`,
    )
  })

  it('colours the dot by what is waiting', () => {
    expect(badgedIcon(ICON, 'done')).toContain(DOT_COLOR.done)
    expect(badgedIcon(ICON, 'done')).not.toContain(DOT_COLOR.blocked)
    expect(DOT_COLOR.blocked).not.toBe(DOT_COLOR.done)
  })

  it('has nothing to draw for no state, and nowhere to draw on a non-svg', () => {
    expect(badgedIcon(ICON, '')).toBeNull()
    expect(badgedIcon('\x89PNG\r\n', 'blocked')).toBeNull()
  })
})

describe('iconDataUrl', () => {
  it('escapes what would otherwise cut the url short', () => {
    const url = iconDataUrl('<svg fill="#f59e0b"/>')
    expect(url.startsWith('data:image/svg+xml,')).toBe(true)
    // A raw "#" would make the rest of the icon a fragment identifier, and the
    // tab would show a blank square.
    expect(url).not.toContain('#')
    expect(decodeURIComponent(url.slice('data:image/svg+xml,'.length))).toBe(
      '<svg fill="#f59e0b"/>',
    )
  })
})

// --- what the tab ends up showing -------------------------------------------

describe('tabBadge', () => {
  it('badges both the title and the icon while something is waiting', () => {
    const badge = tabBadge({ count: 2, state: 'blocked' }, ICON)
    expect(badge.title).toBe('(2) tmux-web')
    expect(badge.icon).toBe(iconDataUrl(badgedIcon(ICON, 'blocked') ?? ''))
    expect(badge.icon).not.toBe(FAVICON_URL)
  })

  it('restores both when the count comes back to zero', () => {
    // The case the whole feature rests on: a badge that is set but never
    // cleared is worse than no badge, because the first time it is wrong it is
    // never trusted again.
    const badge = tabBadge({ count: 0, state: '' }, ICON)
    expect(badge.title).toBe(BASE_TITLE)
    expect(badge.icon).toBe(FAVICON_URL)
  })

  it('shows nothing on a snapshot where nothing needs attention', () => {
    const rows = [
      row({ paneId: '%0', agentState: 'working' }),
      row({ paneId: '%1', agentState: 'idle' }),
      row({ paneId: '%2', agentState: '', command: 'zsh' }),
    ]
    expect(tabBadge(attention(rows, GEN, {}), ICON)).toEqual({
      title: BASE_TITLE,
      icon: FAVICON_URL,
    })
  })

  it('never lets the icon disagree with the title', () => {
    // The count decides both. If they could come apart, a tab reading plain
    // "tmux-web" could still be wearing a dot -- the same "wrong once, never
    // trusted again" failure as a badge that is set and never cleared.
    const badge = tabBadge({ count: 0, state: 'blocked' }, ICON)
    expect(badge.title).toBe(BASE_TITLE)
    expect(badge.icon).toBe(FAVICON_URL)
  })

  it('still counts in the title when the icon could not be read', () => {
    // The icon is fetched once and lazily, so `null` is both "not yet" and
    // "that request failed". Either way the dot is what is lost, not the count.
    const badge = tabBadge({ count: 1, state: 'done' }, null)
    expect(badge.title).toBe('(1) tmux-web')
    expect(badge.icon).toBe(FAVICON_URL)
  })
})
