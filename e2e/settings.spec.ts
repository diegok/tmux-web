/**
 * The terminal's settings, end to end: a size the user picks has to reach tmux,
 * and a palette they pick has to reach the pixels.
 *
 * Neither claim is reachable from the vitest suite. A font size changes no box,
 * so `@wterm/dom` never re-measures its cell on its own -- the whole mechanism
 * is a 1px nudge whose only observable effect is that the column count tmux is
 * told changes, and there is no layout at all under `react-dom/server`. The
 * palette is a class on wterm's own element and a background painted from it.
 *
 * The `window_width` assertion in the first test is a NUMBER on purpose. It is
 * the only thing here that binds the nudge: if the nudge never fires, tmux
 * keeps the old column count, the grid still holds all of it, and every
 * invariant below stays green over a terminal drawing wider than its own box --
 * which is the defect, not the absence of one.
 */
import { test, expect, enroll, openTerminalSettings, BASE_SESSION } from './harness'
import type { Page } from '@playwright/test'
import type { TmuxWeb } from './harness'

/** wterm's grid is this wide. Its number to change, not ours to depend on. */
const GRID_MAX_COLS = 256

/** The columns tmux is drawing the shared window at. */
function tmuxCols(tmuxWeb: TmuxWeb): number {
  return Number(tmuxWeb.tmux('display-message', '-p', '-t', `=${BASE_SESSION}:`, '#{window_width}'))
}

/** Wait for a column count that satisfies `want`, then for it to stop moving. */
async function settledCols(
  page: Page,
  tmuxWeb: TmuxWeb,
  want: (cols: number) => boolean,
): Promise<number> {
  const until = Date.now() + 15_000
  while (Date.now() < until) {
    if (want(tmuxCols(tmuxWeb))) break
    await page.waitForTimeout(100)
  }
  let prev = tmuxCols(tmuxWeb)
  for (;;) {
    await page.waitForTimeout(400)
    const now = tmuxCols(tmuxWeb)
    if (now === prev && now > 0) return now
    if (Date.now() > until) return now
    prev = now
  }
}

/** The columns once two readings a resize-debounce apart agree. */
const stableCols = (page: Page, tmuxWeb: TmuxWeb) => settledCols(page, tmuxWeb, () => true)

/**
 * The terminal's box, its cell and the two custom properties that make the
 * cell -- measured the way `WTerm` measures them, with a hidden `W` inside a
 * real `.term-row`, as `font.spec.ts` does.
 */
async function box(page: Page): Promise<{
  width: number
  cell: number
  rowHeight: number
  fontSize: string
  hostPadRight: string
}> {
  return await page.evaluate(() => {
    const el = document.querySelector('.wterm')
    if (!el) throw new Error('no terminal')
    const cs = getComputedStyle(el)
    const padX = (parseFloat(cs.paddingLeft) || 0) + (parseFloat(cs.paddingRight) || 0)
    const row = document.createElement('div')
    row.className = 'term-row'
    row.style.visibility = 'hidden'
    row.style.position = 'absolute'
    const probe = document.createElement('span')
    probe.textContent = 'W'
    row.append(probe)
    const container = el.querySelector('.term-grid') ?? el
    container.append(row)
    const cell = probe.getBoundingClientRect().width
    row.remove()
    const first = el.querySelector('.term-row')
    const host = el.parentElement
    return {
      width: el.clientWidth - padX,
      cell,
      rowHeight: first ? first.getBoundingClientRect().height : 0,
      fontSize: cs.getPropertyValue('--term-font-size').trim(),
      hostPadRight: host ? getComputedStyle(host).paddingRight : 'no host',
    }
  })
}

/** The widest run of `W` the terminal has actually drawn. */
async function widestW(page: Page): Promise<number> {
  return await page.evaluate(() => {
    const el = document.querySelector('.wterm')
    let widest = 0
    for (const row of el ? [...el.querySelectorAll('.term-row')] : []) {
      const text = (row.textContent ?? '').trim()
      if (/^W+$/.test(text)) widest = Math.max(widest, text.length)
    }
    return widest
  })
}

/** The colour the terminal is painting its background in. */
const background = (page: Page) =>
  page.evaluate(() => {
    const el = document.querySelector('.wterm')
    if (!el) throw new Error('no terminal')
    return getComputedStyle(el).backgroundColor
  })

test('a bigger font makes tmux narrower, and the grid still holds all of it', async ({
  page,
  tmuxWeb,
}) => {
  await page.setViewportSize({ width: 1600, height: 900 })
  await enroll(page, tmuxWeb, 'settings')
  // Not the first reading: the socket goes live at the daemon's initial 80x24
  // and the browser's real size arrives a debounce later.
  const before = await stableCols(page, tmuxWeb)

  await openTerminalSettings(page)
  await page.getByLabel('Font size').selectOption('20')
  // Closed before anything is measured: the dialog is modal, and a scroll lock
  // that pads the body is a width change of its own. This is also what a person
  // does -- the setting applies as it is picked, with nothing to confirm.
  await page.keyboard.press('Escape')
  await expect(page.getByRole('dialog', { name: 'Terminal' })).toBeHidden()

  const after = await settledCols(page, tmuxWeb, (cols) => cols < before)
  const { width, cell, rowHeight, fontSize, hostPadRight } = await box(page)
  expect(
    after,
    `tmux is still ${after} columns wide after the cell grew from 14px to 20px (was ${before}). ` +
      `The terminal reads --term-font-size: ${fontSize}, a ${cell}px cell in a ${width}px box, ` +
      `rows ${rowHeight}px, host padding-right ${hostPadRight}. ` +
      'Nothing re-measured the cell: wterm measures at init and then only when its own box ' +
      'changes size, and a font size changes no box. tmux is now drawing a screen wider than ' +
      'the terminal can hold.',
  ).toBeLessThan(before)

  // The two properties travel together or the text is clipped by its own row.
  expect(fontSize).toBe('20px')
  expect(
    rowHeight,
    `rows are ${rowHeight}px tall at a 20px font, wanted 24. @wterm/dom ships 17px and ` +
      're-affirms --term-row-height from a probe at init, so this is what says our value ' +
      'survived rather than being overwritten.',
  ).toBeCloseTo(24, 1)

  // The nudge shrinks the box by 1px and restores it on the next frame. A
  // padding still set is the shrink being the last thing tmux heard.
  expect(hostPadRight, 'the 1px nudge was never taken back off').toBe('0px')

  const want = Math.min(Math.floor(width / cell), GRID_MAX_COLS)
  expect(
    Math.abs(after - want),
    `tmux is drawing ${after} columns where the cell now being painted in gives ${want}`,
    // One column of tolerance, as `font.spec.ts` takes: the cell is sub-pixel
    // and the two measurements are taken a frame apart.
  ).toBeLessThanOrEqual(1)

  // And the invariant the clamp exists for, unchanged by any of the above:
  // whatever tmux was told, the terminal draws all of it.
  tmuxWeb.tmux('send-keys', '-t', `=${BASE_SESSION}:`, 'clear', 'Enter')
  tmuxWeb.tmux('send-keys', '-t', `=${BASE_SESSION}:`, `printf '%${after}s' | tr ' ' W`, 'Enter')
  const until = Date.now() + 10_000
  let rendered = 0
  while (Date.now() < until) {
    rendered = await widestW(page)
    if (rendered >= after) break
    await page.waitForTimeout(200)
  }
  expect(
    rendered,
    `tmux drew a ${after}-column line and the terminal rendered ${rendered} of it`,
  ).toBe(after)
})

test('the terminal follows the interface until it is told not to', async ({ page, tmuxWeb }) => {
  // Which is the behaviour change this feature ships: the terminal used to be
  // wterm's dark palette even with the whole interface in light.
  await page.emulateMedia({ colorScheme: 'light' })
  await enroll(page, tmuxWeb, 'colours')

  const light = 'rgb(250, 250, 250)'
  const dark = 'rgb(30, 30, 30)'

  await expect
    .poll(() => background(page), { message: 'a light interface did not get a light terminal' })
    .toBe(light)

  // Still following, with no reload: the interface's own theme is resolved on
  // every render and the wterm theme prop is reactive.
  await page.emulateMedia({ colorScheme: 'dark' })
  await expect
    .poll(() => background(page), { message: 'the terminal did not follow the interface into dark' })
    .toBe(dark)

  // And an explicit choice stops following it.
  await openTerminalSettings(page)
  await page.getByRole('radio', { name: 'Light', exact: true }).check()
  await page.keyboard.press('Escape')
  await expect(page.getByRole('dialog', { name: 'Terminal' })).toBeHidden()

  await expect
    .poll(() => background(page), { message: 'picking Light did not paint the terminal light' })
    .toBe(light)
})
