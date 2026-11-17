/**
 * Which of the header's optional controls this device wants.
 *
 * ## Why these are not in `TerminalSettings`
 *
 * That module's own header says what it is about: the thing drawing *inside*
 * the terminal -- its cell, its palette. A button in the app's header is not
 * that, and folding two booleans in there to save a file would have made its
 * first paragraph false.
 *
 * ## Why a module-level store
 *
 * The same reason `terminalSettings.ts` has one, and it is worth repeating
 * because this module depends on it more sharply. The producer is the user
 * menu, which hangs off the sidebar's footer; the consumer is the header, in
 * `App`. Sibling subtrees, so a context would have to wrap the whole app --
 * a provider every test of either subtree then has to know about.
 *
 * ## Why there are two kinds of subscriber
 *
 * `useHeaderSettings` is the React one, through `useSyncExternalStore`.
 * `onHeaderSettingsChange` is a plain callback, and it exists for one case:
 * turning the reply box's control off has to close the box, and closing it has
 * to arm `noteLayoutChange` *before* React is told -- that is the claim that
 * tells tmux this resize is one a person asked for rather than a soft keyboard
 * (see `showReply` in `App.tsx` and `suppressesResize` in `Terminal.tsx`). An
 * effect watching the flag runs after the commit that already unmounted the
 * box, so the claim would land after the resize it exists to cover. With
 * `LAYOUT_CHANGE_GRACE_MS` at one second that would usually work, which is the
 * worst kind of wrong.
 *
 * So these callbacks run synchronously inside the setter, before the React
 * watchers, and they are handed the new value rather than having to read it.
 *
 * ## Why both default to on
 *
 * The owner asked for defaults by device -- both on a phone, only the
 * magnifier on a desktop. A default by device has to guess the device, which
 * is what the reply box's own comment refuses, and the guess is wrong on a
 * touchscreen laptop and on a tablet with a keyboard. These settings are
 * already per device, so the same end state costs one click, once, on the
 * laptop. His call; see the design doc.
 */

import { useSyncExternalStore } from 'react'

import type { SettingsStore } from './terminalSettings'

/** Per device, like the terminal's own settings, and for the same reason. */
export const HEADER_SETTINGS_KEY = 'tmux-web:header'

export interface HeaderSettings {
  /** The magnifier: the palette's only door on a phone, where there is no chord. */
  showJump: boolean
  /** The reply box's toggle. Not the box: see `setHeaderSettings`. */
  showReply: boolean
}

/** Both on, on every device. See the header. */
export const DEFAULT_HEADER_SETTINGS: HeaderSettings = { showJump: true, showReply: true }

/**
 * Site data can be blocked outright (a private window, an embedded webview),
 * and then even *reaching* for `localStorage` throws. Same contract as
 * `defaultStore` in `terminalSettings.ts`.
 */
function defaultStore(): SettingsStore | null {
  try {
    return globalThis.localStorage ?? null
  } catch {
    return null
  }
}

function isFlag(v: unknown): v is boolean {
  return typeof v === 'boolean'
}

/**
 * Read a stored value as settings. Anything it cannot make sense of -- absent,
 * not JSON, not an object, a field of the wrong type -- reads as that field's
 * default, field by field: a flag we cannot parse must not cost the user the
 * other one.
 */
export function parseHeaderSettings(raw: string | null): HeaderSettings {
  if (!raw) return DEFAULT_HEADER_SETTINGS
  let parsed: unknown
  try {
    parsed = JSON.parse(raw)
  } catch {
    return DEFAULT_HEADER_SETTINGS
  }
  if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) {
    return DEFAULT_HEADER_SETTINGS
  }
  const { showJump, showReply } = parsed as Partial<HeaderSettings>
  return {
    showJump: isFlag(showJump) ? showJump : DEFAULT_HEADER_SETTINGS.showJump,
    showReply: isFlag(showReply) ? showReply : DEFAULT_HEADER_SETTINGS.showReply,
  }
}

/** Load the settings. A store that refuses or throws reads as the defaults. */
export function readHeaderSettings(store: SettingsStore | null = defaultStore()): HeaderSettings {
  try {
    return parseHeaderSettings(store?.getItem(HEADER_SETTINGS_KEY) ?? null)
  } catch {
    return DEFAULT_HEADER_SETTINGS
  }
}

/**
 * Persist the settings. A store that refuses the write costs this device its
 * memory of the choice and nothing else -- the choice itself is already live.
 */
export function writeHeaderSettings(
  s: HeaderSettings,
  store: SettingsStore | null = defaultStore(),
): void {
  try {
    store?.setItem(HEADER_SETTINGS_KEY, JSON.stringify(s))
  } catch {
    // Quota, or a browser that blocked site data. See defaultStore.
  }
}

// --- the store --------------------------------------------------------------

let store: SettingsStore | null = defaultStore()
let settings: HeaderSettings | null = null
const watchers = new Set<() => void>()
const listeners = new Set<(s: HeaderSettings) => void>()

/** The `getSnapshot` the hook uses. Seeded from storage on first read. */
export function getHeaderSettings(): HeaderSettings {
  if (settings === null) settings = readHeaderSettings(store)
  return settings
}

export function subscribeHeaderSettings(cb: () => void): () => void {
  watchers.add(cb)
  return () => watchers.delete(cb)
}

/** Read the settings, re-rendering when they change. */
export function useHeaderSettings(): HeaderSettings {
  return useSyncExternalStore(subscribeHeaderSettings, getHeaderSettings, getHeaderSettings)
}

/**
 * Be told the new value the moment it changes, before React is. Returns the
 * unsubscribe. See the header for the one case this exists for.
 */
export function onHeaderSettingsChange(fn: (s: HeaderSettings) => void): () => void {
  listeners.add(fn)
  return () => listeners.delete(fn)
}

/**
 * Change one or both flags, live, and remember it. A field the patch does not
 * name keeps what it had; a patch that changes nothing notifies nobody and
 * writes nothing.
 *
 * This turns a control off. It does not turn the thing the control governs off
 * -- the reply box's own open state is `App`'s, and the palette's row still
 * opens it on a device whose header does not carry the button. What the
 * listeners below do about that is theirs; see `AfterClose` in `Palette.tsx`
 * for the other half of the same idea.
 */
export function setHeaderSettings(patch: Partial<HeaderSettings>): void {
  const current = getHeaderSettings()
  const next: HeaderSettings = {
    showJump: isFlag(patch.showJump) ? patch.showJump : current.showJump,
    showReply: isFlag(patch.showReply) ? patch.showReply : current.showReply,
  }
  if (next.showJump === current.showJump && next.showReply === current.showReply) return
  settings = next
  writeHeaderSettings(next, store)
  // Before the React watchers, and synchronously: the ordering is the point.
  for (const fn of listeners) fn(next)
  for (const cb of watchers) cb()
}

/**
 * Forget everything and read again from `store`. Tests only: the store is
 * module-level, so without this one test's choice is the next one's start
 * state. Passing `null` means "no storage at all"; omitting it means
 * `localStorage`.
 */
export function resetHeaderSettings(s: SettingsStore | null = defaultStore()): void {
  store = s
  settings = null
  for (const cb of watchers) cb()
}
