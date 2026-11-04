/**
 * The font the terminal grid is measured in, and the wait that makes the
 * measurement right the first time.
 *
 * ## Why this app has to ship a monospaced font
 *
 * `@wterm/dom` draws a row as *runs of text* in one `<span>` under
 * `white-space: pre` -- not one box per cell. Only the block elements
 * (U+2580-259F) and double-width cells get a `1ch` box of their own. So the
 * grid holds together only while every glyph on the line has exactly the cell's
 * advance width, and a single glyph from a different font shifts everything to
 * its right.
 *
 * Until this module existed the app shipped no monospaced font at all: the
 * terminal asked for `'Menlo', 'Consolas', 'DejaVu Sans Mono', 'Courier New',
 * monospace` and got whatever the device had. MEASURED in Chromium at 14px, the
 * cell being 8.438px:
 *
 *   - box drawing, blocks and the ellipsis: 8.438px, exact, on this Linux box,
 *     where DejaVu Sans Mono answers;
 *   - U+23FA, the dot `claude` prints for a tool call: 8px, 0.948 cells;
 *   - U+273B, its spinner, on the bare `monospace` generic a phone falls back
 *     to: 12px -- 1.428 cells, so 4.4px of drift for the rest of that line.
 *
 * ## Why Geist Mono
 *
 * Of the three monospaced families on fontsource that were measured, only Geist
 * Mono carries a subset covering `U+2500-259F` -- the box drawing *and* the
 * blocks, which is what an agent's frames are made of. JetBrains Mono and Noto
 * Sans Mono both stop at the latin and cyrillic ranges, so the frames would
 * still have fallen back to the device. It is also the monospaced cut of the
 * sans this UI already uses, and costs 23KB + 5.8KB over the wire.
 *
 * Glyphs outside `U+2500-259F` -- U+23FA and U+273B among them -- still come
 * from the device, and still drift. Pinning those would mean a box per cell,
 * which is the renderer's decision, not this app's.
 *
 * ## Why the wait
 *
 * fontsource declares these faces `font-display: swap`, and `WTerm` measures
 * the cell *once*, from a hidden `W`, then only again when its own box changes
 * size (`_setupResizeObserver`). A face that lands after that measurement
 * leaves tmux sized to the fallback's cell for as long as nobody rotates the
 * phone or opens the keyboard. So the terminal waits for the faces before it
 * mounts -- bounded, because a font that never arrives must not leave a blank
 * pane.
 */

/** The family this app ships. Must match the `font-family` fontsource declares. */
export const MONO_FAMILY = "'Geist Mono Variable'"

/**
 * What the terminal and every other monospaced surface ask for. The fallbacks
 * are only reached when the bundled face could not be fetched, and every one of
 * them is monospaced: the one thing that must never happen here is landing on a
 * proportional font.
 */
export const TERM_FONT_STACK = `${MONO_FAMILY}, ui-monospace, 'Menlo', 'Consolas', 'DejaVu Sans Mono', monospace`

/**
 * The text whose faces must be in before the grid is measured.
 *
 * Two glyphs, because fontsource splits the family by `unicode-range` and these
 * two live in different files: `0` is in the latin subset -- and is also what
 * `1ch` means, so it is the cell yardstick itself -- while `─` is in symbols2,
 * the subset that carries the frames. Asking for `0` alone would let the
 * measurement happen before the frames could be drawn in the right font.
 */
export const MONO_PROBE_TEXT = '0─'

/**
 * How long the terminal waits. Long enough for a LAN fetch of 29KB several
 * times over, short enough that a phone on a dead network is not staring at an
 * empty pane -- this app's whole failure mode is a phone with a bad link.
 */
export const FONT_WAIT_MS = 1500

/** What became of the wait. Every outcome means "mount the terminal now". */
export type MonoFontOutcome = 'ready' | 'absent' | 'timeout' | 'failed'

/** The slice of `FontFaceSet` this needs, so a test can pass two lines of it. */
export interface FontLoader {
  load(font: string, text?: string): Promise<unknown>
}

/**
 * Resolve once the bundled faces can be drawn with, or once waiting has gone on
 * long enough. Never rejects: the caller's only move is to mount either way.
 */
export function monoFontReady(
  fonts: FontLoader | undefined,
  waitMs: number = FONT_WAIT_MS,
): Promise<MonoFontOutcome> {
  if (!fonts) return Promise.resolve('absent')
  return new Promise<MonoFontOutcome>((resolve) => {
    const timer = setTimeout(() => resolve('timeout'), waitMs)
    const settle = (outcome: MonoFontOutcome) => {
      clearTimeout(timer)
      resolve(outcome)
    }
    // A size is required by the shorthand grammar, and is irrelevant to which
    // faces match: `load` resolves faces by family and unicode-range.
    fonts.load(`1rem ${MONO_FAMILY}`, MONO_PROBE_TEXT).then(
      () => settle('ready'),
      () => settle('failed'),
    )
  })
}
