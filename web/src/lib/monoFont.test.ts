import { readFileSync } from 'node:fs'

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import {
  FONT_WAIT_MS,
  MONO_FAMILY,
  MONO_PROBE_TEXT,
  TERM_FONT_STACK,
  monoFontReady,
} from './monoFont'

describe('the font the terminal grid is measured in', () => {
  it('leads the stack with the bundled family, and keeps a system mono behind it', () => {
    // The order is the whole point: the first name is the one this app ships,
    // so every device measures the same cell. The rest is what a browser that
    // could not fetch it falls back to -- still monospaced, never the UI sans.
    expect(TERM_FONT_STACK.startsWith(MONO_FAMILY)).toBe(true)
    expect(TERM_FONT_STACK).toMatch(/monospace$/)
  })

  it('probes a latin glyph and a box-drawing glyph, because they are separate files', () => {
    // fontsource splits Geist Mono by unicode-range: the cell yardstick comes
    // from the latin subset and the frames an agent draws from symbols2
    // (U+2500-259F). Asking for only one would measure before the other
    // arrived.
    expect(MONO_PROBE_TEXT).toContain('0')
    expect(MONO_PROBE_TEXT).toContain('─')
  })
})

describe('the font the rest of the app is monospaced in', () => {
  it('is the same stack, to the character, as the terminal uses', () => {
    // Two copies: the stylesheet's `--font-mono`, which Tailwind's `font-mono`
    // and so the capture panel, the reply box and the sidebar's command
    // capsules read, and TERM_FONT_STACK, which the terminal element carries
    // inline. A comment saying "keep these in step" is not a mechanism; this
    // is. They must be byte-identical, because the point is that text quoted
    // out of a pane lines up with the pane.
    const css = readFileSync(new URL('../index.css', import.meta.url), 'utf8')
    const declared = /--font-mono:\s*([^;]+);/.exec(css)?.[1]
    expect(declared, 'src/index.css declares no --font-mono').toBeDefined()
    expect(declared).toBe(TERM_FONT_STACK)
  })
})

describe('waiting for that font', () => {
  beforeEach(() => {
    vi.useFakeTimers()
  })

  afterEach(() => {
    vi.useRealTimers()
  })

  it('says so at once where the browser has no font loading API', async () => {
    // jsdom and a server render both land here. "absent" is not a failure: it
    // means there is nothing to wait for, so the terminal must mount now.
    await expect(monoFontReady(undefined)).resolves.toBe('absent')
  })

  it('reports ready once the faces are loaded, and asks for both subsets', async () => {
    const asked: Array<[string, string | undefined]> = []
    const fonts = {
      load: (font: string, text?: string) => {
        asked.push([font, text])
        return Promise.resolve([])
      },
    }
    await expect(monoFontReady(fonts)).resolves.toBe('ready')
    expect(asked).toHaveLength(1)
    expect(asked[0][0]).toContain(MONO_FAMILY)
    expect(asked[0][1]).toBe(MONO_PROBE_TEXT)
  })

  it('gives up after the wait and lets the terminal mount anyway', async () => {
    // A font that never arrives must not leave a blank terminal. The grid is
    // then measured in whatever the device has, which is exactly today's
    // behaviour -- the bad case this change improves on, not a new one.
    const pending = monoFontReady({ load: () => new Promise(() => {}) })
    await vi.advanceTimersByTimeAsync(FONT_WAIT_MS - 1)
    let settled = false
    void pending.then(() => {
      settled = true
    })
    await Promise.resolve()
    expect(settled).toBe(false)
    await vi.advanceTimersByTimeAsync(1)
    await expect(pending).resolves.toBe('timeout')
  })

  it('does not wait out the timeout when the load fails outright', async () => {
    const pending = monoFontReady({ load: () => Promise.reject(new Error('offline')) })
    await expect(pending).resolves.toBe('failed')
  })

  it('stops its timer once the font is in, so no stray work outlives the wait', async () => {
    // The timer is the only thing here that can outlive the promise. A leaked
    // one would fire into a torn-down tree.
    await expect(monoFontReady({ load: () => Promise.resolve([]) })).resolves.toBe('ready')
    expect(vi.getTimerCount()).toBe(0)
  })
})
