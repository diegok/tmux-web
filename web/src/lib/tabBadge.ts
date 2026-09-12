/**
 * The tab badge: `(2) tmux-web` in the document title and a dot on the favicon
 * while something is blocked or has finished since this device last looked.
 *
 * It is the answer to "do I need to go back to it?" for a tab that is not the
 * one you are looking at, which is the only moment the sidebar cannot answer it
 * -- so what it counts is exactly what the sidebar's dots call `blocked` and
 * `done`. `working` is not counted: an agent that is working is the state you
 * left it in, and a number that never went to zero would stop being read.
 *
 * ## What this badge cannot do, and why it is still the choice
 *
 * **A hidden tab is throttled, and a locked phone is stopped.** The count is
 * computed from snapshots, so it moves only as often as the tab manages to take
 * one -- and `SnapshotPoller` keeps taking them in the background, at
 * `HIDDEN_POLL_INTERVAL_MS` rather than the visible cadence. That is a minute,
 * because a minute is what browsers will actually run a hidden tab's timers at:
 * Chromium aligns them to a one-minute grid once the tab has been hidden five
 * minutes. So a blocked agent lights the tab up about a minute later, up to two
 * in the worst case, which is the badge doing its job while you are away.
 *
 * What it cannot survive is a device that is not running timers at all. A
 * locked phone or a discarded tab shows nothing until you look -- and then
 * shows the truth at once, because becoming visible wakes the poller instead of
 * making it wait out the slow interval (see `SnapshotPoller.wake`).
 *
 * That is the accepted cost of choosing a badge over Web Push. Push would
 * survive a locked phone -- it is the only thing that would -- and it costs a
 * service worker, a VAPID key pair, per-device subscriptions kept on the
 * daemon, and a permission prompt, for an app whose whole premise is a single
 * binary and a browser. Written down here rather than rediscovered: if someone
 * later wants a notification that reaches a phone in a pocket, this is not a
 * bug in the badge, it is the feature the badge was chosen instead of.
 */

import { useEffect, useState } from 'react'

import { mostUrgent, paneState } from './useSnapshot'
import type { SeenMap, SnapshotRow } from './useSnapshot'

/**
 * The unbadged title, which must be the one `index.html` ships.
 *
 * Held as a constant rather than read off `document.title` at startup: this
 * module *writes* that title, so reading it back is a value that has already
 * been through here once -- and under React's StrictMode remount the second
 * read would find `(2) tmux-web` and treat it as the app's name. The test pins
 * it against `index.html`, so the two cannot drift apart silently.
 */
export const BASE_TITLE = 'tmux-web'

/** The icon `index.html` links, and what the badge is drawn on top of. */
export const FAVICON_URL = '/favicon.svg'

/**
 * The dot's colours, as the hex spellings of the sidebar's `bg-amber-500` and
 * `bg-emerald-500` (see `STATE_TONE` in AppSidebar.tsx). Literal because a
 * favicon is composed into a `data:` URL, where a Tailwind class and a CSS
 * custom property both mean nothing.
 */
export const DOT_COLOR: Record<'blocked' | 'done', string> = {
  blocked: '#f59e0b',
  done: '#10b981',
}

/** The two states worth interrupting for, and `""` for "nothing is". */
export type AttentionState = 'blocked' | 'done' | ''

/** How many panes want you, and the most urgent thing they want. */
export interface Attention {
  count: number
  /** `blocked` outranks `done`, exactly as it does in the sidebar's roll-up. */
  state: AttentionState
}

/**
 * What the tab has to say for itself, over the rows of one snapshot.
 *
 * Over every row in the snapshot, not only the session this tab is attached to:
 * the sidebar shows every session the daemon can see and rolls their states up,
 * and a badge that counted less than the tree does would send you looking for a
 * pane that is not the one that wants you.
 *
 * `paneState` is the same function the sidebar's dots come from, so the badge
 * cannot count something the tree does not show -- `done` included, which is
 * this device's own answer about a finish it has not been shown yet and is why
 * `seen` has to come in here rather than the count being computed on the
 * daemon.
 */
export function attention(
  rows: readonly SnapshotRow[],
  serverStart: string,
  seen: SeenMap,
): Attention {
  const states: AttentionState[] = []
  for (const row of rows) {
    const state = paneState(row, serverStart, seen)
    if (state === 'blocked' || state === 'done') states.push(state)
  }
  // Ranked by STATE_ORDER rather than by a second copy of the ordering here.
  // Narrowed rather than cast: `mostUrgent` may answer with any state, and only
  // these two can be in the list it was given.
  const urgent = mostUrgent(states)
  return {
    count: states.length,
    state: urgent === 'blocked' || urgent === 'done' ? urgent : '',
  }
}

/**
 * The document title for a count.
 *
 * The count comes first because a tab strip crops titles from the right, and a
 * badge you have to widen the tab to read is not a badge.
 */
export function tabTitle(count: number, base: string = BASE_TITLE): string {
  return count > 0 ? `(${count}) ${base}` : base
}

/**
 * The app's icon with a dot on it, or null when there is nothing to draw on.
 *
 * The dot is composed into the real favicon's own markup rather than replacing
 * it, so the tab still looks like this app at 16px -- and it is appended last,
 * which is what puts it on top of the glyph. Bottom-right, where every platform
 * puts a badge.
 *
 * Null for a source this cannot be inserted into (an icon that is not SVG, or a
 * request that answered with something else), and for "nothing needs
 * attention", which has no dot to draw. The caller falls back to the plain
 * icon: the title still carries the count, so a failure here costs the dot and
 * nothing else.
 */
export function badgedIcon(source: string, state: AttentionState): string | null {
  if (state === '') return null
  const close = source.lastIndexOf('</svg>')
  if (close < 0) return null
  // In the icon's own 48x46 viewBox. r=13 is about a quarter of the width,
  // which is the smallest that survives being drawn at 16 physical pixels.
  const dot = `<circle cx="34" cy="32" r="13" fill="${DOT_COLOR[state]}"/>`
  return `${source.slice(0, close)}${dot}${source.slice(close)}`
}

/**
 * An SVG document as a `href`.
 *
 * Percent-encoded rather than base64: the markup is mostly ASCII, so this is
 * both smaller and legible in devtools, and `encodeURIComponent` escapes the
 * `#` in every colour -- which would otherwise cut the URL short at the first
 * fill and leave the tab with a broken icon.
 */
export function iconDataUrl(svg: string): string {
  return `data:image/svg+xml,${encodeURIComponent(svg)}`
}

/** What the tab should be showing. */
export interface TabBadge {
  title: string
  /** A `data:` URL while something needs you, `FAVICON_URL` when nothing does. */
  icon: string
}

/**
 * The whole decision, as one pure function over the snapshot's answer and the
 * icon source (null until it has been read, or if reading it failed).
 *
 * Both halves are restored together when the count returns to zero. That is the
 * case worth stating: a badge that is set but never cleared is worse than no
 * badge, because the first time it is wrong it is never trusted again.
 */
export function tabBadge(attention: Attention, source: string | null): TabBadge {
  const badged =
    attention.count > 0 && source !== null ? badgedIcon(source, attention.state) : null
  return {
    title: tabTitle(attention.count),
    icon: badged === null ? FAVICON_URL : iconDataUrl(badged),
  }
}

/**
 * Read the icon this page links, for `badgedIcon` to draw on.
 *
 * Same origin and already in the browser's cache -- the tab fetched it to show
 * it -- so this is a cache read rather than a request, and it happens once,
 * lazily, the first time anything needs a badge. A tab that never has an agent
 * blocked never asks for it at all.
 */
async function readIconSource(): Promise<string | null> {
  try {
    const res = await fetch(FAVICON_URL, { credentials: 'same-origin' })
    if (!res.ok) return null
    const text = await res.text()
    return text.includes('</svg>') ? text : null
  } catch {
    // Offline, or an icon that is not there. The count still reaches the title.
    return null
  }
}

/** Point the page's `<link rel="icon">` at `href`, adding one if it has none. */
function setIcon(doc: Document, href: string): void {
  let link = doc.querySelector<HTMLLinkElement>('link[rel="icon"]')
  if (!link) {
    link = doc.createElement('link')
    link.rel = 'icon'
    doc.head.append(link)
  }
  link.type = 'image/svg+xml'
  link.href = href
}

/**
 * Keep the tab's title and icon showing what the snapshot says, for as long as
 * the app is mounted.
 *
 * **The only part of this file no unit test reaches** is the two effects: this
 * suite renders with `react-dom/server`, which never runs one, and there is no
 * DOM renderer here by design. Everything they decide -- what the title reads,
 * which icon is shown, when the dot is drawn and when both go back to normal --
 * is `tabBadge`'s, tested directly. What is left uncovered is that the values
 * are written to `document` at all and that the icon is read once rather than
 * on every poll; Task 15's Playwright run is the layer that sees a real tab.
 *
 * Nothing is restored on unmount: the app is the page, so the only thing that
 * unmounts this is the tab closing.
 */
export function useTabBadge(
  rows: readonly SnapshotRow[],
  serverStart: string,
  seen: SeenMap,
): void {
  const { count, state } = attention(rows, serverStart, seen)
  const [source, setSource] = useState<string | null>(null)
  // Read once, and only once something wants a dot: `source !== null` is what
  // stops it, and `wanted` -- a boolean, not the count -- is what keeps a
  // second, third and fourth blocked agent from re-running it. A read that
  // failed leaves `source` null and is not retried until the count has been
  // back to zero and risen again, which is as often as it is worth asking.
  const wanted = count > 0
  useEffect(() => {
    if (!wanted || source !== null) return
    let live = true
    void readIconSource().then((text) => {
      if (live && text !== null) setSource(text)
    })
    return () => {
      live = false
    }
  }, [wanted, source])

  useEffect(() => {
    const badge = tabBadge({ count, state }, source)
    document.title = badge.title
    setIcon(document, badge.icon)
  }, [count, state, source])
}
