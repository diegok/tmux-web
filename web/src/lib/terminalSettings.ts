/**
 * The terminal's own settings -- its font size and its colours -- and the grid
 * size it reports back.
 *
 * These are *not* the interface theme. `Appearance` in the user menu is the
 * app's light/dark, which next-themes owns; this module is about the thing
 * drawing inside the terminal, which has its own palettes and its own cell.
 *
 * ## Why two module-level stores and not a context
 *
 * There is exactly one producer of each value and one consumer, and they live
 * in sibling subtrees: the dialog hangs off `UserMenu` in the sidebar footer,
 * the terminal is mounted in the main panel. A context would have to wrap the
 * whole app to join them, which means a provider every test of either subtree
 * then has to know about. Two `useSyncExternalStore` stores at module scope
 * join them with no provider, no prop threading, and no re-render of anything
 * between the two.
 *
 * Snapshots are returned **by identity** when nothing changed. This is not an
 * optimisation: `useSyncExternalStore` compares with `Object.is` and a fresh
 * object every call is an infinite render loop.
 *
 * ## Why the row height is round(size * 1.2)
 *
 * `@wterm/dom` draws a row as a hard pixel box -- `.term-row { height:
 * var(--term-row-height) }` -- that does not follow the font. Raise the font
 * alone and the text is clipped by its own row. The shipped pair is
 * `--term-font-size: 14px` with `--term-row-height: 17px`, and
 * `round(14 * 1.2) = 17` reproduces it exactly, which is why the ratio is not
 * something prettier: at the default this function must be a no-op, or every
 * terminal that never touches these settings changes the day this ships.
 *
 * ## Why `dark` maps to `undefined`
 *
 * wterm's default palette is the VS-Code dark one, and it is selected by the
 * *absence* of a theme class. `theme-dark` is not a class that exists, so
 * asking for dark means asking for nothing.
 */

import { useSyncExternalStore } from 'react'

/** Per device, because a phone and a 27" monitor do not want the same cell. */
export const TERMINAL_SETTINGS_KEY = 'tmux-web:terminal'

/** Below 10px the box-drawing characters stop resolving on a phone. */
export const FONT_SIZE_MIN = 10
/**
 * A grouped session shares its window with the owner's own client, so a size
 * this tab picks is a size his terminal gets too; and past ~24px the column
 * count on a laptop falls low enough that agent output starts wrapping.
 */
export const FONT_SIZE_MAX = 24
/** What `@wterm/dom` ships, so the default changes nothing. */
export const DEFAULT_FONT_SIZE = 14

/** Every whole size in range: the picker offers all of them, with no gaps. */
export const FONT_SIZES: readonly number[] = Array.from(
  { length: FONT_SIZE_MAX - FONT_SIZE_MIN + 1 },
  (_, i) => FONT_SIZE_MIN + i,
)

export const ROW_HEIGHT_RATIO = 1.2

export type TerminalTheme = 'system' | 'dark' | 'light' | 'solarized-dark' | 'monokai'

/**
 * One list, not two controls: wterm's light/dark and its palettes are a single
 * axis with exactly one light member, so a separate "light or dark" switch
 * would have had nothing to say about Monokai.
 */
export const TERMINAL_THEMES: readonly { value: TerminalTheme; label: string }[] = [
  { value: 'system', label: 'Follow interface' },
  { value: 'dark', label: 'Dark' },
  { value: 'light', label: 'Light' },
  { value: 'solarized-dark', label: 'Solarized Dark' },
  { value: 'monokai', label: 'Monokai' },
]

export interface TerminalSettings {
  fontSize: number
  theme: TerminalTheme
}

/**
 * `system` by default, and that is a behaviour change: until now the terminal
 * was wterm's dark even when the interface was light.
 */
export const DEFAULT_TERMINAL_SETTINGS: TerminalSettings = {
  fontSize: DEFAULT_FONT_SIZE,
  theme: 'system',
}

/** The two halves of `localStorage` this module uses, and nothing more. */
export interface SettingsStore {
  getItem(k: string): string | null
  setItem(k: string, v: string): void
}

/**
 * Site data can be blocked outright (a private window, an embedded webview),
 * and then even *reaching* for `localStorage` throws. Same contract as
 * `readSeen` in `lib/useSnapshot.ts`.
 */
function defaultStore(): SettingsStore | null {
  try {
    return globalThis.localStorage ?? null
  } catch {
    return null
  }
}

function isSize(v: unknown): v is number {
  return typeof v === 'number' && Number.isInteger(v) && v >= FONT_SIZE_MIN && v <= FONT_SIZE_MAX
}

function isTheme(v: unknown): v is TerminalTheme {
  return typeof v === 'string' && TERMINAL_THEMES.some((t) => t.value === v)
}

/**
 * Read a stored value as settings. Anything it cannot make sense of -- absent,
 * not JSON, not an object, a size from a version with a different range, a
 * theme this one has never heard of -- reads as that field's default, field by
 * field: a theme we do not know must not cost the user the size they chose.
 */
export function parseTerminalSettings(raw: string | null): TerminalSettings {
  if (!raw) return DEFAULT_TERMINAL_SETTINGS
  let parsed: unknown
  try {
    parsed = JSON.parse(raw)
  } catch {
    return DEFAULT_TERMINAL_SETTINGS
  }
  if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) {
    return DEFAULT_TERMINAL_SETTINGS
  }
  const { fontSize, theme } = parsed as Partial<TerminalSettings>
  return {
    fontSize: isSize(fontSize) ? fontSize : DEFAULT_FONT_SIZE,
    theme: isTheme(theme) ? theme : DEFAULT_TERMINAL_SETTINGS.theme,
  }
}

/** Load the settings. A store that refuses or throws reads as the defaults. */
export function readTerminalSettings(store: SettingsStore | null = defaultStore()): TerminalSettings {
  try {
    return parseTerminalSettings(store?.getItem(TERMINAL_SETTINGS_KEY) ?? null)
  } catch {
    return DEFAULT_TERMINAL_SETTINGS
  }
}

/**
 * Persist the settings. A store that refuses the write costs this device its
 * memory of the choice and nothing else -- the choice itself is already live.
 */
export function writeTerminalSettings(
  s: TerminalSettings,
  store: SettingsStore | null = defaultStore(),
): void {
  try {
    store?.setItem(TERMINAL_SETTINGS_KEY, JSON.stringify(s))
  } catch {
    // Quota, or a browser that blocked site data. See defaultStore.
  }
}

/** The pixel height of one row at `fontSize`. See the header for the ratio. */
export function rowHeightFor(fontSize: number): number {
  return Math.round(fontSize * ROW_HEIGHT_RATIO)
}

/**
 * The value for `@wterm/react`'s `theme` prop, which it turns into a
 * `theme-<x>` class. `undefined` means wterm's own default, which is the dark
 * one -- see the header.
 */
export function wtermThemeFor(theme: TerminalTheme, resolved: string | undefined): string | undefined {
  if (theme === 'system') return resolved === 'light' ? 'light' : undefined
  if (theme === 'dark') return undefined
  return theme
}

/** What the terminal last told tmux, as the dialog reports it back. */
export interface TerminalGrid {
  cols: number
  rows: number
  /** The box is wider than the grid can draw, so `cols` is the clamp, not the box. */
  capped: boolean
}

// --- the settings store -----------------------------------------------------

let store: SettingsStore | null = defaultStore()
let settings: TerminalSettings | null = null
const settingsWatchers = new Set<() => void>()

/** The `getSnapshot` the hook uses. Seeded from storage on first read. */
export function getTerminalSettings(): TerminalSettings {
  if (settings === null) settings = readTerminalSettings(store)
  return settings
}

export function subscribeTerminalSettings(cb: () => void): () => void {
  settingsWatchers.add(cb)
  return () => settingsWatchers.delete(cb)
}

/** Read the settings, re-rendering when they change. */
export function useTerminalSettings(): TerminalSettings {
  return useSyncExternalStore(subscribeTerminalSettings, getTerminalSettings, getTerminalSettings)
}

/**
 * Change one or both settings, live, and remember it. A field the patch does
 * not name, or names with a value this version does not accept, keeps what it
 * had; a patch that changes nothing notifies nobody and writes nothing.
 */
export function applyTerminalSettings(patch: Partial<TerminalSettings>): void {
  const current = getTerminalSettings()
  const next: TerminalSettings = {
    fontSize: isSize(patch.fontSize) ? patch.fontSize : current.fontSize,
    theme: isTheme(patch.theme) ? patch.theme : current.theme,
  }
  if (next.fontSize === current.fontSize && next.theme === current.theme) return
  settings = next
  writeTerminalSettings(next, store)
  for (const cb of settingsWatchers) cb()
}

/**
 * Forget everything and read again from `store`. Tests only: the stores are
 * module-level, so without this one test's choice is the next one's start
 * state. Passing `null` means "no storage at all"; omitting it means
 * `localStorage`.
 */
export function resetTerminalSettings(s: SettingsStore | null = defaultStore()): void {
  store = s
  settings = null
  grid = null
  for (const cb of settingsWatchers) cb()
  for (const cb of gridWatchers) cb()
}

// --- the reported grid ------------------------------------------------------

let grid: TerminalGrid | null = null
const gridWatchers = new Set<() => void>()

function getGrid(): TerminalGrid | null {
  return grid
}

function subscribeGrid(cb: () => void): () => void {
  gridWatchers.add(cb)
  return () => gridWatchers.delete(cb)
}

/** The `getSnapshot` the hook uses: `null` until a terminal has reported one. */
export function getTerminalGrid(): TerminalGrid | null {
  return grid
}

/**
 * Read the size the terminal last settled on. `null` means no terminal is
 * mounted, or none has resized yet -- the dialog says so rather than inventing
 * a number.
 */
export function useTerminalGrid(): TerminalGrid | null {
  return useSyncExternalStore(subscribeGrid, getGrid, getGrid)
}

/**
 * Report the grid. The terminal calls this from the same callback that tells
 * tmux, with the clamped pair it actually sent; `null` on unmount.
 */
export function reportTerminalGrid(next: TerminalGrid | null): void {
  const same =
    next === null
      ? grid === null
      : grid !== null &&
        grid.cols === next.cols &&
        grid.rows === next.rows &&
        grid.capped === next.capped
  if (same) return
  grid = next
  for (const cb of gridWatchers) cb()
}
