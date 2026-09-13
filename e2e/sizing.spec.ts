/**
 * How the browser's size reaches tmux.
 *
 * This lives here and not in the vitest suite because there is nothing in
 * between to test. The app never measures the terminal itself: `<WTermView
 * autoResize>` runs a `ResizeObserver` inside `@wterm/react` and calls
 * `onResize`, so everything upstream of `TerminalSession.noteResize` is layout,
 * and layout under `react-dom/server` in a node environment has no heights at
 * all. The seam below `noteResize` -- the debounce and the `resize` frame -- is
 * unit-tested in `web/src/components/__tests__`; the seam above it is only
 * observable in a real browser, which is this file.
 */
import { test, expect, enroll } from './harness'
import type { Page } from '@playwright/test'
import type { TmuxWeb } from './harness'

/**
 * The size of the *window*, which is what the pane is drawn at. Read through
 * the base session rather than the tab's throwaway one: they share the window,
 * and the base session's name is the only one this test knows.
 */
function windowSize(tmuxWeb: TmuxWeb): string {
  return tmuxWeb.tmux('display-message', '-p', '-t', '=e2e:', '#{window_width}x#{window_height}')
}

/** Wait out the 150ms resize debounce and the round trip to tmux. */
async function settled(page: Page, tmuxWeb: TmuxWeb, want: (size: string) => boolean): Promise<string> {
  const until = Date.now() + 10_000
  let last = ''
  for (;;) {
    last = windowSize(tmuxWeb)
    if (want(last)) return last
    if (Date.now() > until) return last
    await page.waitForTimeout(100)
  }
}

/** The size once two readings a debounce apart agree. */
async function stable(page: Page, tmuxWeb: TmuxWeb): Promise<string> {
  const until = Date.now() + 10_000
  let prev = windowSize(tmuxWeb)
  for (;;) {
    await page.waitForTimeout(400)
    const now = windowSize(tmuxWeb)
    if (now === prev && now !== '' && dims(now).rows > 0) return now
    if (Date.now() > until) return now
    prev = now
  }
}

function dims(size: string): { cols: number; rows: number } {
  const [c, r] = size.split('x').map(Number)
  return { cols: c, rows: r }
}

/**
 * The owner's report: "cuando paso el navegador a full-screen y vuelvo, se
 * agranda y al volver no se achica."
 *
 * Both axes are asserted, and the rows are the half that regressed: the shell
 * was `min-h-svh`, so it grew with the terminal's own rows and never came back,
 * while the width -- bounded all along -- returned correctly. A test that
 * checked only the size were-we-bigger-than-before, or only the columns, would
 * have passed throughout the defect.
 */
test('the window comes back down when the viewport does', async ({ page, tmuxWeb }) => {
  await enroll(page, tmuxWeb, 'sizing')

  // Not simply the first reading. The socket goes live at the daemon's initial
  // 80x24 and the browser's real size arrives one debounce later, so a `start`
  // taken too early is a size the viewport never had -- and "came back to
  // start" would then be asserting against a number that was never true.
  const start = dims(await stable(page, tmuxWeb))

  await page.setViewportSize({ width: 1900, height: 1040 })
  const grown = dims(
    await settled(page, tmuxWeb, (s) => dims(s).cols > start.cols && dims(s).rows > start.rows),
  )
  expect(grown.cols, 'going full-screen never widened the window').toBeGreaterThan(start.cols)
  expect(grown.rows, 'going full-screen never heightened the window').toBeGreaterThan(start.rows)

  await page.setViewportSize({ width: 1280, height: 800 })
  const back = dims(await settled(page, tmuxWeb, (s) => s === `${start.cols}x${start.rows}`))
  expect(back.cols, 'the columns never came back down').toBe(start.cols)
  expect(
    back.rows,
    `the rows never came back down: ${grown.rows} -> ${back.rows}, wanted ${start.rows}. ` +
      'The shell is taller than the viewport, so the terminal kept the rows it laid out ' +
      'while full-screen and tmux was never told the smaller height.',
  ).toBe(start.rows)
})

/**
 * The layout invariant the fix rests on, asserted directly so a regression says
 * *why* rather than only that some number did not come back. A shell taller
 * than the viewport is the whole defect: `flex-1 min-h-0` cannot shrink a
 * parent whose height is its own content.
 */
test('the shell is never taller than the viewport', async ({ page, tmuxWeb }) => {
  await enroll(page, tmuxWeb, 'sizing')

  const overflow = async () =>
    await page.evaluate(() => ({
      shell: document.documentElement.scrollHeight,
      viewport: window.innerHeight,
    }))

  await page.setViewportSize({ width: 1900, height: 1040 })
  await page.waitForTimeout(800)
  await page.setViewportSize({ width: 1280, height: 800 })
  await page.waitForTimeout(800)

  const after = await overflow()
  expect(
    after.shell,
    `the page scrolls: ${after.shell}px of content in a ${after.viewport}px viewport`,
  ).toBeLessThanOrEqual(after.viewport)
})

/**
 * Task 21, end to end: the resize path is suppressed while an app-owned text
 * control has focus.
 *
 * This is the half of the feature no vitest test can reach. The unit suite
 * calls `noteResize` by hand; here the shrink arrives the way it does on a
 * phone -- the layout viewport gets smaller, `@wterm/react`'s own
 * `ResizeObserver` fires, and the only thing standing between that and a
 * `resize` frame is the `#suppressed` gate. And the frame matters because the
 * window is *shared*: the owner's local client is attached to it, and tmux
 * hands the window to whichever client resized last. A phone tapping its reply
 * box must not drag his laptop's terminal down to keyboard height.
 *
 * The viewport shrink stands in for the keyboard, which Playwright cannot open.
 * That is the same event a keyboard produces under
 * `interactive-widget=resizes-content` (Task 22), and the app cannot tell the
 * two apart.
 */
test('a focused reply box freezes the size this tab is sharing', async ({ page, tmuxWeb }) => {
  await enroll(page, tmuxWeb, 'sizing')
  const start = dims(await stable(page, tmuxWeb))

  await page.getByRole('textbox', { name: 'Reply to this pane' }).click()
  await page.setViewportSize({ width: 1280, height: 480 })

  // Long enough that a resize would have gone out and come back: the debounce
  // is 150ms and the snapshot poll behind everything else here is 1.5s.
  await page.waitForTimeout(2000)
  expect(
    windowSize(tmuxWeb),
    'the window followed the shrunk tab while its reply box had focus, which on a ' +
      'phone is the soft keyboard resizing somebody else’s terminal',
  ).toBe(`${start.cols}x${start.rows}`)

  // Focus back into the terminal, which is the one focus that never suppresses
  // -- and the size in force by then goes out once.
  await page.getByRole('textbox', { name: 'Terminal' }).click()
  const after = dims(await settled(page, tmuxWeb, (s) => dims(s).rows < start.rows))
  expect(after.rows, 'the size held back while suppressed was never sent on the lift').toBeLessThan(
    start.rows,
  )
})
