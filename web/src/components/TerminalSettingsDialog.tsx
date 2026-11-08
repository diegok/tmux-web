/**
 * The terminal's settings: how big its text is, and what colours it draws in.
 *
 * Deliberately *not* the interface's theme -- that is `Appearance`, the next
 * item down in the same menu, and it stays there. The two are different things: one
 * is the app's light/dark, the other is the palette of the thing drawing inside
 * the terminal, which ships its own and has exactly one light member.
 *
 * ## Why the body is a component of its own
 *
 * `DialogContent` is a Radix portal, and a portal renders to the empty string
 * under `renderToStaticMarkup` -- so the dialog shell cannot be asserted on in
 * a test suite that has no DOM. `TerminalSettingsPanel` is the body, renderable
 * on its own, the same split `DevicesDialog` makes for `MintedPanel` and
 * `DeviceRow`.
 *
 * ## Why these are native controls
 *
 * The size is a native `<select>`: on a phone that *is* the OS picker, a wheel
 * sized for a thumb, where a styled listbox of fifteen numbers is a scroll
 * trap. The themes are a native radio group for the same reason -- one tap per
 * choice, with the platform's own hit targets. Neither needs a wrapper
 * component, and both are styled from the app's tokens.
 *
 * There is no OK button. A size or a colour applies as it is chosen, because
 * the only way to judge either is to look at the terminal behind the dialog.
 */

import type { ReactElement } from 'react'

import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import {
  FONT_SIZES,
  TERMINAL_THEMES,
  applyTerminalSettings,
  useTerminalGrid,
  useTerminalSettings,
} from '@/lib/terminalSettings'
import type { TerminalGrid } from '@/lib/terminalSettings'

export interface TerminalSettingsDialogProps {
  open: boolean
  onOpenChange: (open: boolean) => void
}

/**
 * The size the terminal last settled on, in words.
 *
 * Three cases, and the third is the point: a clamped `256` cannot be told apart
 * from a box that really is 256 columns wide, so when the clamp is biting the
 * readout says so. Without that line, a terminal whose box is wider than the
 * grid can draw looks like a terminal that fits.
 */
export function gridLabel(grid: TerminalGrid | null): string {
  if (!grid) return 'no terminal attached'
  const size = `${grid.cols} × ${grid.rows}`
  return grid.capped ? `${size} (capped — the grid cannot draw wider)` : size
}

/** The dialog's body: two controls and one readout. */
export function TerminalSettingsPanel(): ReactElement {
  const { fontSize, theme } = useTerminalSettings()
  const grid = useTerminalGrid()

  return (
    <div className="grid gap-5">
      <div className="grid gap-1.5">
        <label htmlFor="terminal-font-size" className="text-sm font-medium">
          Font size
        </label>
        <select
          id="terminal-font-size"
          value={fontSize}
          onChange={(e) => applyTerminalSettings({ fontSize: Number(e.target.value) })}
          // text-base below md, because a control under 16px makes iOS Safari
          // zoom the whole page on focus and never zoom back.
          className="border-input focus-visible:border-ring focus-visible:ring-ring/50 dark:bg-input/30 h-8 w-full rounded-lg border bg-transparent px-2.5 py-1 text-base transition-colors outline-none focus-visible:ring-3 md:text-sm"
        >
          {FONT_SIZES.map((size) => (
            <option key={size} value={size}>
              {size} px
            </option>
          ))}
        </select>
        <p className="text-muted-foreground text-xs">{gridLabel(grid)}</p>
      </div>

      <fieldset className="grid gap-2">
        <legend className="mb-1.5 text-sm font-medium">Colours</legend>
        {TERMINAL_THEMES.map((option) => (
          <label key={option.value} className="flex items-center gap-2 text-sm">
            <input
              type="radio"
              name="terminal-theme"
              value={option.value}
              checked={theme === option.value}
              onChange={() => applyTerminalSettings({ theme: option.value })}
              className="accent-primary size-4"
            />
            {option.label}
          </label>
        ))}
      </fieldset>
    </div>
  )
}

export function TerminalSettingsDialog({
  open,
  onOpenChange,
}: TerminalSettingsDialogProps): ReactElement {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="sm:max-w-sm">
        <DialogHeader>
          <DialogTitle>Terminal</DialogTitle>
          <DialogDescription>
            This device only, remembered here. Changes apply as you make them.
          </DialogDescription>
        </DialogHeader>

        <TerminalSettingsPanel />
      </DialogContent>
    </Dialog>
  )
}
