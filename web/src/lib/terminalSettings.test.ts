/**
 * The terminal's own settings: what a stored value is allowed to be, what the
 * two numbers that make up a cell are, and which wterm theme each choice maps
 * to.
 *
 * Most of this file is one table per way a stored value can be wrong. The
 * settings live in `localStorage`, which means they outlive the code that
 * wrote them: a value from a version that offered 9px, or stored the size as a
 * string, or named a theme this one has never heard of, is not a hypothetical
 * -- it is what the next release reads. Every one of those has to come back as
 * the default without throwing, because the alternative is a blank screen on a
 * phone whose only way out is clearing site data.
 */

import { beforeEach, describe, expect, it, vi } from 'vitest'

import {
  DEFAULT_TERMINAL_SETTINGS,
  FONT_SIZES,
  TERMINAL_SETTINGS_KEY,
  TERMINAL_THEMES,
  applyTerminalSettings,
  getTerminalGrid,
  getTerminalSettings,
  parseTerminalSettings,
  readTerminalSettings,
  reportTerminalGrid,
  resetTerminalSettings,
  rowHeightFor,
  subscribeTerminalSettings,
  wtermThemeFor,
  writeTerminalSettings,
} from './terminalSettings'
import type { SettingsStore, TerminalTheme } from './terminalSettings'

/** A store that holds one string, so a write can be read back. */
function memoryStore(seed: string | null = null): SettingsStore & { value: string | null } {
  return {
    value: seed,
    getItem(k: string) {
      return k === TERMINAL_SETTINGS_KEY ? this.value : null
    },
    setItem(k: string, v: string) {
      if (k === TERMINAL_SETTINGS_KEY) this.value = v
    },
  }
}

// The stores are module-level, so every test here starts from a known state --
// without this one test's choice is the next one's start state.
beforeEach(() => {
  resetTerminalSettings(null)
})

describe('parseTerminalSettings', () => {
  it('reads a well-formed value', () => {
    expect(parseTerminalSettings('{"fontSize":18,"theme":"monokai"}')).toEqual({
      fontSize: 18,
      theme: 'monokai',
    })
  })

  // Nothing usable in the value at all: both fields default.
  it.each([null, '', 'not json', '[]', '"14"', '42'])('falls back entirely on %s', (raw) => {
    expect(parseTerminalSettings(raw as string | null)).toEqual({ fontSize: 14, theme: 'system' })
  })

  // One bad field, and only that field defaults. A size from a version with a
  // different range must not cost the user their palette, and vice versa: the
  // fields are validated one by one, never as a pair.
  it.each([
    ['{"fontSize":9,"theme":"monokai"}', 14, 'monokai'], // under the floor
    ['{"fontSize":25,"theme":"monokai"}', 14, 'monokai'], // over the ceiling
    ['{"fontSize":14.5,"theme":"monokai"}', 14, 'monokai'], // not a whole pixel
    ['{"fontSize":"20","theme":"monokai"}', 14, 'monokai'], // wrong type
    ['{"fontSize":null,"theme":"monokai"}', 14, 'monokai'],
    ['{"theme":"monokai"}', 14, 'monokai'], // size missing
    ['{"fontSize":18,"theme":"hotdog"}', 18, 'system'], // foreign theme
    ['{"fontSize":18,"theme":null}', 18, 'system'],
    ['{"fontSize":18}', 18, 'system'], // theme missing
  ])('reads %s as %ipx and %s', (raw, size, theme) => {
    expect(parseTerminalSettings(raw as string)).toEqual({ fontSize: size, theme })
  })

  it('keeps the half it can read', () => {
    // A theme this version does not know must not cost the size the user chose.
    expect(parseTerminalSettings('{"fontSize":20,"theme":"hotdog"}')).toEqual({
      fontSize: 20,
      theme: 'system',
    })
  })
})

describe('storage that will not cooperate', () => {
  it('reads the defaults from storage that throws', () => {
    const store = {
      getItem() {
        throw new Error('blocked')
      },
      setItem() {},
    }
    expect(readTerminalSettings(store)).toEqual(DEFAULT_TERMINAL_SETTINGS)
  })

  it('drops a write to storage that throws', () => {
    const store = {
      getItem: () => null,
      setItem() {
        throw new Error('blocked')
      },
    }
    expect(() => writeTerminalSettings({ fontSize: 20, theme: 'dark' }, store)).not.toThrow()
  })

  it('reads the defaults with no storage at all', () => {
    expect(readTerminalSettings(null)).toEqual(DEFAULT_TERMINAL_SETTINGS)
  })

  it('round-trips through a store that works', () => {
    const store = memoryStore()
    writeTerminalSettings({ fontSize: 20, theme: 'monokai' }, store)
    expect(readTerminalSettings(store)).toEqual({ fontSize: 20, theme: 'monokai' })
  })
})

describe('rowHeightFor', () => {
  // The first is the one that matters: it is what @wterm/dom ships, and a row
  // box that disagreed with the font would clip its own text.
  it('reproduces wterm’s own default', () => expect(rowHeightFor(14)).toBe(17))

  it.each([
    [10, 12],
    [12, 14],
    [16, 19],
    [18, 22],
    [20, 24],
    [24, 29],
  ])('%ipx rows are %ipx', (size, want) => expect(rowHeightFor(size)).toBe(want))
})

describe('wtermThemeFor', () => {
  it.each([
    ['dark', 'dark', undefined], // wterm's default IS the absent class
    ['light', 'dark', 'light'],
    ['solarized-dark', 'light', 'solarized-dark'],
    ['monokai', 'dark', 'monokai'],
    ['system', 'light', 'light'],
    ['system', 'dark', undefined],
    ['system', undefined, undefined], // before next-themes has resolved
  ])('%s under a %s interface', (theme, resolved, want) => {
    expect(wtermThemeFor(theme as TerminalTheme, resolved as string | undefined)).toBe(want)
  })
})

describe('what the dialog offers', () => {
  it('offers every whole size in range exactly once', () => {
    expect(FONT_SIZES).toEqual([10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24])
  })

  it('names the five themes in the order they are declared', () => {
    expect(TERMINAL_THEMES).toEqual([
      { value: 'system', label: 'Follow interface' },
      { value: 'dark', label: 'Dark' },
      { value: 'light', label: 'Light' },
      { value: 'solarized-dark', label: 'Solarized Dark' },
      { value: 'monokai', label: 'Monokai' },
    ])
  })
})

describe('the settings store', () => {
  it('notifies subscribers on a change and persists it', () => {
    const store = memoryStore('{"fontSize":12,"theme":"monokai"}')
    resetTerminalSettings(store)
    const saw = vi.fn()
    const stop = subscribeTerminalSettings(saw)

    applyTerminalSettings({ fontSize: 20 })

    expect(saw).toHaveBeenCalledTimes(1)
    // A patch is a patch: the theme it did not mention survives.
    expect(getTerminalSettings()).toEqual({ fontSize: 20, theme: 'monokai' })
    expect(store.value).toBe('{"fontSize":20,"theme":"monokai"}')
    stop()
  })

  it('returns the same snapshot by identity when nothing changed', () => {
    // useSyncExternalStore re-renders forever otherwise.
    const first = getTerminalSettings()
    expect(getTerminalSettings()).toBe(first)

    applyTerminalSettings({ fontSize: first.fontSize, theme: first.theme })
    expect(getTerminalSettings()).toBe(first)

    applyTerminalSettings({ fontSize: 20 })
    expect(getTerminalSettings()).not.toBe(first)
  })

  it('stops notifying once unsubscribed', () => {
    const saw = vi.fn()
    subscribeTerminalSettings(saw)()
    applyTerminalSettings({ fontSize: 20 })
    expect(saw).not.toHaveBeenCalled()
  })
})

describe('the reported grid', () => {
  it('has no grid until one is reported', () => {
    expect(getTerminalGrid()).toBeNull()

    reportTerminalGrid({ cols: 232, rows: 64, capped: false })
    expect(getTerminalGrid()).toEqual({ cols: 232, rows: 64, capped: false })

    // Which is what unmounting sends.
    reportTerminalGrid(null)
    expect(getTerminalGrid()).toBeNull()
  })

  it('returns the same grid by identity when nothing changed', () => {
    reportTerminalGrid({ cols: 232, rows: 64, capped: false })
    const first = getTerminalGrid()
    reportTerminalGrid({ cols: 232, rows: 64, capped: false })
    expect(getTerminalGrid()).toBe(first)
  })
})
