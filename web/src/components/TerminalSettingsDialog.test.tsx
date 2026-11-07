/**
 * The terminal's settings dialog: the readout's words, and the controls a
 * static render can see.
 *
 * `DialogContent` is a portal, and a portal renders to the empty string under
 * `renderToStaticMarkup` -- measured, not assumed, and asserted below. So the
 * body is its own component, `TerminalSettingsPanel`, exactly as
 * `DevicesDialog` exports `MintedPanel` and `DeviceRow` for the same reason.
 * What a real click does to a real `<select>` is `e2e/settings.spec.ts`.
 */

import { renderToStaticMarkup } from 'react-dom/server'
import { beforeEach, describe, expect, it } from 'vitest'

import { TerminalSettingsDialog, TerminalSettingsPanel, gridLabel } from './TerminalSettingsDialog'
import {
  FONT_SIZES,
  TERMINAL_SETTINGS_KEY,
  TERMINAL_THEMES,
  reportTerminalGrid,
  resetTerminalSettings,
} from '@/lib/terminalSettings'
import type { SettingsStore, TerminalGrid } from '@/lib/terminalSettings'

function storeHolding(value: string): SettingsStore {
  return { getItem: (k) => (k === TERMINAL_SETTINGS_KEY ? value : null), setItem: () => {} }
}

beforeEach(() => {
  resetTerminalSettings(null)
})

// --- what the readout says --------------------------------------------------

describe('gridLabel', () => {
  it.each([
    [null, 'no terminal attached'],
    [{ cols: 232, rows: 64, capped: false }, '232 × 64'],
    [{ cols: 256, rows: 78, capped: true }, '256 × 78 (capped — the grid cannot draw wider)'],
  ])('says %j as %s', (grid, want) =>
    expect(gridLabel(grid as TerminalGrid | null)).toBe(want as string),
  )
})

// --- the controls -----------------------------------------------------------

describe('<TerminalSettingsPanel>', () => {
  it('offers every size in range, with the stored one selected', () => {
    resetTerminalSettings(storeHolding('{"fontSize":18,"theme":"system"}'))
    const markup = renderToStaticMarkup(<TerminalSettingsPanel />)

    for (const size of FONT_SIZES) expect(markup).toContain(`value="${size}"`)
    // Every whole size and no others: a gap would make 12 -> 18 two resizes.
    expect(markup.match(/<option/g)).toHaveLength(FONT_SIZES.length)
    expect(markup).toMatch(/<option[^>]*value="18"[^>]*selected/)
    expect(markup).toContain('Font size')
  })

  it('offers the five themes, with the stored one checked', () => {
    resetTerminalSettings(storeHolding('{"fontSize":14,"theme":"monokai"}'))
    const markup = renderToStaticMarkup(<TerminalSettingsPanel />)

    expect(markup).toContain('Colours')
    for (const theme of TERMINAL_THEMES) expect(markup).toContain(theme.label)
    expect(markup.match(/type="radio"/g)).toHaveLength(TERMINAL_THEMES.length)
    // Exactly one checked radio, and it is the stored theme -- two would mean
    // the group shows two palettes at once. Matched on the whole tag because
    // React decides the attribute order, not the JSX.
    const checked = markup.match(/<input[^>]*checked[^>]*>/g)
    expect(checked).toHaveLength(1)
    expect(checked?.[0]).toContain('value="monokai"')
  })

  it('shows the reported grid', () => {
    reportTerminalGrid({ cols: 232, rows: 64, capped: false })
    expect(renderToStaticMarkup(<TerminalSettingsPanel />)).toContain('232 × 64')
  })

  it('says so when no terminal is attached rather than inventing a size', () => {
    // Which is what the first paint looks like, and what an unmounted terminal
    // leaves behind.
    expect(renderToStaticMarkup(<TerminalSettingsPanel />)).toContain('no terminal attached')
  })
})

describe('<TerminalSettingsDialog>', () => {
  it('is a portal, so a static render shows nothing either way', () => {
    // Open or closed, `renderToStaticMarkup` sees nothing -- which is why the
    // body above is a component of its own. The same fact is why
    // UserMenu.test.tsx cannot see the menu's items.
    const open = renderToStaticMarkup(<TerminalSettingsDialog open onOpenChange={() => {}} />)
    const shut = renderToStaticMarkup(
      <TerminalSettingsDialog open={false} onOpenChange={() => {}} />,
    )
    expect(open).toBe('')
    expect(shut).toBe('')
  })
})
