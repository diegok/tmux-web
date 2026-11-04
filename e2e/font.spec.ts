/**
 * The font the grid is measured and drawn in.
 *
 * This is the only place it can be checked. `@wterm/dom` draws a row as runs of
 * text in one span under `white-space: pre` -- not a box per cell -- so the
 * grid holds together only while every glyph on the line has exactly the cell's
 * advance width. Whether that is true depends on which font the browser
 * actually resolved, which subsets of it arrived, and when they arrived
 * relative to wterm's one-off measurement of a hidden `W`. None of that exists
 * outside a real browser; `web/src/lib/monoFont.test.ts` owns the wait's
 * arithmetic and cannot tell you what was resolved.
 *
 * The owner's report, from a phone: "lo que falta es que la font sea monospace
 * para que todo se renderice bien."
 */
import { test, expect, enroll, BASE_SESSION } from './harness'
import type { Page } from '@playwright/test'
import type { TmuxWeb } from './harness'

const pane = `${BASE_SESSION}:0`

/** What the terminal's own cell is, measured the way `WTerm` measures it. */
async function grid(page: Page): Promise<{ width: number; cell: number; family: string }> {
  return await page.evaluate(() => {
    const el = document.querySelector('.wterm')
    if (!el) throw new Error('no terminal')
    const cs = getComputedStyle(el)
    const padX = (parseFloat(cs.paddingLeft) || 0) + (parseFloat(cs.paddingRight) || 0)
    // A hidden `W` inside the grid, which is both what wterm measures and the
    // only way to read the cell without reaching into its private fields.
    const row = document.createElement('div')
    row.className = 'term-row'
    row.style.visibility = 'hidden'
    row.style.position = 'absolute'
    const probe = document.createElement('span')
    probe.textContent = 'W'
    row.appendChild(probe)
    const container = el.querySelector('.term-grid') ?? el
    container.appendChild(row)
    const cell = probe.getBoundingClientRect().width
    row.remove()
    return { width: el.clientWidth - padX, cell, family: cs.fontFamily }
  })
}

/**
 * Make sure the faces are in before anything is measured.
 *
 * Without this a measurement taken early reads the *fallback's* cell and then
 * agrees with a tmux that was sized by that same fallback -- a test that passes
 * precisely when the bug is present. Found by mutation: with the wait in
 * `Terminal` removed, the measurement below and tmux agreed on 118 columns
 * while the font being painted in gives 125.
 */
async function withFontsIn(page: Page): Promise<void> {
  await page.evaluate(() =>
    // A face that cannot be fetched is not this helper's business: the app
    // proceeds in that case too (`monoFontReady`'s 'failed'), and a helper that
    // threw would report the network where the point is the misalignment.
    document.fonts.load("1rem 'Geist Mono Variable'", '0\u2500').then(
      () => undefined,
      () => undefined,
    ),
  )
}

/** The columns tmux is actually drawing the pane at. */
function tmuxCols(tmuxWeb: TmuxWeb): number {
  return Number(tmuxWeb.tmux('display-message', '-p', '-t', `=${BASE_SESSION}:`, '#{window_width}'))
}

/** Wait until two readings a resize-debounce apart agree. */
async function stableCols(page: Page, tmuxWeb: TmuxWeb): Promise<number> {
  const until = Date.now() + 10_000
  let prev = tmuxCols(tmuxWeb)
  for (;;) {
    await page.waitForTimeout(400)
    const now = tmuxCols(tmuxWeb)
    if (now === prev && now > 0) return now
    if (Date.now() > until) return now
    prev = now
  }
}

/**
 * Until this app shipped a monospaced font of its own, the terminal asked for
 * `'Menlo', 'Consolas', 'DejaVu Sans Mono', 'Courier New', monospace` and got
 * whatever the device had -- which on a phone is the bare `monospace` generic.
 * Asserting the resolved family is the only way to catch a stylesheet order, a
 * dropped import or a `.wterm` rule out-specifying ours: all three leave the
 * app looking fine on the machine that has DejaVu Sans Mono installed.
 */
test('the terminal is drawn in the monospaced font this app ships', async ({ page, tmuxWeb }) => {
  await enroll(page, tmuxWeb, 'font')

  const { family } = await grid(page)
  expect(
    family,
    `the terminal resolved ${family}, so it is being drawn in whatever this device happens to have`,
  ).toContain('Geist Mono Variable')

  // Resolving the name is not the same as having the glyphs. fontsource splits
  // the family by unicode-range, and the subset that matters most is the one
  // carrying U+2500-259F: the frames every agent draws. Of the three
  // monospaced families measured on fontsource it is the only one that has it,
  // and a face left `unloaded` means those frames came from the device.
  const faces = await page.evaluate(() => {
    const out: { family: string; status: string; range: string }[] = []
    document.fonts.forEach((f) => out.push({ family: f.family, status: f.status, range: f.unicodeRange }))
    return out
  })
  const loaded = faces.filter((f) => f.family.includes('Geist Mono') && f.status === 'loaded')
  expect(loaded.length, `loaded faces: ${JSON.stringify(faces.filter((f) => f.family.includes('Geist Mono')))}`).toBeGreaterThanOrEqual(2)
  expect(
    loaded.some((f) => f.range.includes('2500')),
    'the box-drawing subset was never fetched, so an agent\'s frames fall back to the device',
  ).toBe(true)
})

/**
 * The invariant the whole change exists for, asserted on the glyphs that break
 * it in practice.
 *
 * MEASURED in Chromium at 14px, with a cell of 8.438px: `─` `│` `╭` `█` and `…`
 * land exactly on the cell where DejaVu Sans Mono answers, but U+273B -- the
 * spinner `claude` prints -- is 12px on the bare `monospace` generic, 1.428
 * cells, so every character to its right on that line is 4.4px out of place.
 * A run of frame characters is the longest such run an agent draws, and the
 * drift accumulates across all of it.
 */
test('a run of frame characters lands exactly on the grid', async ({ page, tmuxWeb }) => {
  await enroll(page, tmuxWeb, 'font')
  await withFontsIn(page)

  const n = 60
  const frame = '─'.repeat(n)
  tmuxWeb.tmux('send-keys', '-t', pane, `clear; printf '%s\\n' '${frame}'`, 'Enter')
  const row = page.locator('.term-row', { hasText: frame }).first()
  await row.waitFor()

  const { cell } = await grid(page)
  // A Range over exactly those 60 characters, because the row is one run: the
  // frames and the spaces padding the line out to the grid's width share a
  // style, so wterm draws them in a single span and the span's own width says
  // nothing about where the frames end.
  const drawn = await row.evaluate((el, count: number) => {
    const node = [...el.querySelectorAll('span')]
      .map((s) => s.firstChild)
      .find((n): n is Text => n instanceof Text && n.data.startsWith('\u2500'))
    if (!node) throw new Error(`no run begins with a frame character; row drew ${JSON.stringify(el.textContent)}`)
    const range = document.createRange()
    range.setStart(node, 0)
    range.setEnd(node, count)
    return range.getBoundingClientRect().width
  }, n)

  const want = n * cell
  expect(
    Math.abs(drawn - want),
    `${n} frame characters drew ${drawn.toFixed(2)}px where the grid is ${want.toFixed(2)}px ` +
      `(${(drawn / n).toFixed(3)}px per cell against ${cell.toFixed(3)}): the frames are coming ` +
      'from a different font than the one the cell was measured in, so everything to their right ' +
      'on the line is out of place',
  ).toBeLessThan(1)
})

/**
 * The hazard the wait in `monoFontReady` exists for, made deterministic.
 *
 * fontsource declares these faces `font-display: swap`, and `WTerm` measures
 * the cell once as it mounts and then only when its own box changes size
 * (`_setupResizeObserver`). So a face that lands after the mount leaves tmux
 * sized to the *fallback* cell until something resizes the pane -- on a phone,
 * until it is rotated.
 *
 * It is worth 8 columns on this viewport, not a rounding error: Geist Mono's
 * cell is 8px at 14px against DejaVu Sans Mono's 8.429px, so a 1256px grid is
 * 157 columns in the font that is drawn and 149 in the one it would have been
 * measured in. tmux would wrap every line 8 columns early, for the life of the
 * tab.
 */
test('a font that arrives late still measures the grid', async ({ page, tmuxWeb }) => {
  // Well inside FONT_WAIT_MS, so the wait is what resolves this, not its
  // timeout -- and late enough that the terminal would certainly have mounted
  // and measured first without it.
  await page.route(/geist-mono.*\.woff2/, async (route) => {
    await new Promise((r) => setTimeout(r, 1200))
    await route.continue()
  })

  await enroll(page, tmuxWeb, 'font')

  await withFontsIn(page)
  const { width, cell } = await grid(page)
  const want = Math.floor(width / cell)
  const got = await stableCols(page, tmuxWeb)
  expect(
    Math.abs(got - want),
    `tmux is drawing ${got} columns where the font the terminal is painting in gives ${want}: ` +
      'the grid was measured before the face arrived, and nothing re-measures it until the ' +
      'pane is resized',
  ).toBeLessThanOrEqual(1)
})
