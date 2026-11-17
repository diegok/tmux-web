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
import { test, expect, enroll, openReplyBox, replyBox, BASE_SESSION } from './harness'
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
 * The owner's report: "la barra de abajo sale como cortada, como si recibiera
 * mal el tamaño de la pantalla", plus a pi whose dialog "tiembla" and a claude
 * that takes three or four more keystrokes than it should to wrap a line.
 *
 * One cause under all three. `@wterm/core`'s WASM grid is 256 COLUMNS WIDE, and
 * `@wterm/dom` reports the column count it derived from its own box -- not the
 * one the grid can hold -- through `onResize`. tmux-web then tells tmux that
 * number, so tmux draws a screen wider than the terminal can hold and every
 * column past 256 is written where nobody can see it: a status line looks cut,
 * a TUI redrawing across the edge flickers, and the app's own wrap column sits
 * off-screen.
 *
 * MEASURED here, sweeping the viewport, before the clamp existed:
 *
 *     tmux cols:  140  227  245  247  250  252  265  285
 *     rendered:   140  227  245  247  250  252  256  256
 *
 * The assertion is the INVARIANT, not the number: whatever tmux is told, the
 * grid draws all of it. A wterm that raises its cap keeps this test green and a
 * tmux-web that clamps too late makes it fail, which is the right way round --
 * 256 is wterm's number to change, not ours to depend on.
 */
test('tmux is never told a width the terminal cannot draw', async ({ page, tmuxWeb }) => {
  // Wide enough to pass the cap at this font size: 285 columns on a 2560px
  // viewport, where the grid can hold 256.
  await page.setViewportSize({ width: 2560, height: 900 })
  await enroll(page, tmuxWeb, 'wide')
  const cols = dims(await stable(page, tmuxWeb)).cols

  // A line exactly as wide as the pane, in the one character that cannot be
  // confused with the shell's own output.
  tmuxWeb.tmux('send-keys', '-t', '=e2e:', 'clear', 'Enter')
  tmuxWeb.tmux('send-keys', '-t', '=e2e:', `printf '%${cols}s' | tr ' ' W`, 'Enter')

  const drawn = async () =>
    await page.evaluate(() => {
      const el = document.querySelector('.wterm')
      let widest = 0
      for (const row of el ? [...el.querySelectorAll('.term-row')] : []) {
        const text = (row.textContent ?? '').trim()
        if (/^W+$/.test(text)) widest = Math.max(widest, text.length)
      }
      return widest
    })

  const until = Date.now() + 10_000
  let rendered = 0
  while (Date.now() < until) {
    rendered = await drawn()
    if (rendered >= cols) break
    await page.waitForTimeout(200)
  }
  expect(
    rendered,
    `tmux drew a ${cols}-column line and the terminal rendered ${rendered} of it. ` +
      `The columns past the end are written where nobody can see them: that is the cut ` +
      `status bar, the flickering dialog and the late wrap, all three.`,
  ).toBe(cols)
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
 * Task 21 and Task 23, end to end and in one test, because they are one
 * mechanism: which resizes this tab is allowed to share, and which it must
 * swallow.
 *
 * The window is *shared* -- the owner's local client is attached to it and tmux
 * hands the window to whichever client resized last -- so a phone tapping its
 * reply box must not drag his laptop's terminal down to keyboard height. That is
 * Task 21's `#suppressed` gate, and it is the half no vitest test can reach: the
 * unit suite calls `noteResize` by hand, while here the shrink arrives the way
 * it does on a phone, through `@wterm/react`'s own `ResizeObserver`.
 *
 * Task 23 then made the box's *own* appearance a resize: it is a flex sibling,
 * so opening it shortens the terminal on purpose, and the toggle focuses the box
 * -- arming the very suppression that would swallow it. React focuses an
 * `autoFocus` element during the commit, before the browser lays out, so the
 * focus genuinely does arrive first. Both claims are in one test because the
 * distinction is the feature: the same focused textarea, two resizes, and only
 * the one the user asked for goes out.
 *
 * The viewport shrink stands in for the keyboard, which Playwright cannot open.
 * That is the same event a keyboard produces under
 * `interactive-widget=resizes-content` (Task 22), and the app cannot tell the
 * two apart -- which is exactly why `noteLayoutChange` exists and why it is
 * armed by the toggle rather than inferred from the focus.
 */
test('the toggle s resize goes out and the keyboard s does not', async ({ page, tmuxWeb }) => {
  await enroll(page, tmuxWeb, 'sizing')
  const start = dims(await stable(page, tmuxWeb))

  // The box takes its rows from the terminal, and tmux has to be told: a pane
  // still drawn at rows that are now behind the box is the defect this whole
  // ordering exists to avoid.
  await openReplyBox(page)
  await expect(replyBox(page), 'the toggle did not focus the box, so this test is about nothing')
    .toBeFocused()
  const withBox = dims(await settled(page, tmuxWeb, (s) => dims(s).rows < start.rows))
  expect(
    withBox.rows,
    `opening the reply box shortened the terminal and tmux was never told: still ` +
      `${withBox.rows} rows, was ${start.rows}. The box focused itself in the same commit, ` +
      'so the resize arrived into a suppressed session and was held -- see ' +
      'TerminalSession.noteLayoutChange.',
  ).toBeLessThan(start.rows)
  expect(withBox.cols, 'the box changed the width, which it has no business doing').toBe(start.cols)

  // And now the keyboard, with focus already in the box and nothing having been
  // pressed: held, exactly as before.
  await page.setViewportSize({ width: 1280, height: 480 })

  // Long enough that a resize would have gone out and come back: the debounce
  // is 150ms and the snapshot poll behind everything else here is 1.5s.
  await page.waitForTimeout(2000)
  expect(
    windowSize(tmuxWeb),
    'the window followed the shrunk tab while its reply box had focus, which on a ' +
      'phone is the soft keyboard resizing somebody else’s terminal',
  ).toBe(`${withBox.cols}x${withBox.rows}`)

  // Focus back into the terminal, which is the one focus that never suppresses
  // -- and the size in force by then goes out once.
  await page.getByRole('textbox', { name: 'Terminal' }).click()
  const after = dims(await settled(page, tmuxWeb, (s) => dims(s).rows < withBox.rows))
  expect(after.rows, 'the size held back while suppressed was never sent on the lift').toBeLessThan(
    withBox.rows,
  )
})

// Padding is space the grid does not get, and `@wterm/dom`'s own stylesheet
// ships `.wterm { padding: 12px }` -- a frame of the page's background around
// a terminal that draws its own, which after a few days of use the owner asked
// to have back: "quita espacio y al ser otro color que la terminal, queda
// raro." On a phone 24px of width is a column and a half of an 80-column pane.
//
// Asserted in a browser because it is the one place the override is decided:
// the rule it has to beat is in a stylesheet imported from inside a module, so
// what makes ours win is that it is INLINE on the element, not a selector of
// ours out-specifying theirs. A stylesheet of our own would be back to arguing
// about import order -- the same argument `Terminal.tsx` already makes for
// `--term-font-family`.
test('the terminal draws to its own edge, not inside wterm’s 12px frame', async ({
  page,
  tmuxWeb,
}) => {
  await enroll(page, tmuxWeb, 'padding')

  const padding = await page.evaluate(() => {
    const el = document.querySelector('.wterm')
    if (!el) throw new Error('no .wterm element')
    const s = getComputedStyle(el)
    return [s.paddingTop, s.paddingRight, s.paddingBottom, s.paddingLeft]
  })

  expect(padding, 'the terminal is still sitting inside a 12px frame').toEqual([
    '1px',
    '1px',
    '1px',
    '1px',
  ])
})

// The owner's complaint, in a browser: "en movil entre la lupa y el resto de
// elementos no deja lugar para ver en que estás." The breadcrumb is the only
// thing in the header that says which pane you are looking at, and the four
// controls to its right were three text buttons that never shrink.
//
// Measured here rather than in pixels of header: a pixel budget would have to
// be rewritten the next time a glyph changes, and the property that matters is
// the one the owner named -- can you read which pane you are in.
test('the breadcrumb is readable on a phone, not squeezed by the controls', async ({
  page,
  tmuxWeb,
}) => {
  // A command with a name of a realistic length. `sh`, which the base window
  // runs, fits in the ~30px the old header left over, and this test would then
  // pass against the very layout it exists to reject.
  tmuxWeb.tmux('new-window', '-d', '-t', BASE_SESSION, '-n', 'tools', tmuxWeb.fakeAgent('codewatch'))
  await enroll(page, tmuxWeb, 'phone')
  await page.setViewportSize({ width: 390, height: 844 })

  // Picked through the palette rather than the sidebar: the palette's rows are
  // named after the pane's own command, which is the string this test is about.
  //
  // And picked at 390px on purpose. Below `sm` the button's text is
  // `display:none`, which takes it out of the accessible name, so this click is
  // the one place in the suite that goes through the `aria-label` the header's
  // controls carry for exactly this width. Delete that label and this line
  // stops finding the button -- which is the point: without it the whole
  // decision was mutation-survivable.
  await page.getByRole('button', { name: /Jump to/ }).click()
  await page.getByRole('option', { name: /codewatch/ }).click()
  const command = page.getByLabel('Location').locator('span').last()
  await expect(command).toHaveText('codewatch')

  // The names must not change with the width, which is the whole of why these
  // two carry an explicit `aria-label`. Below `sm` their text is
  // `display:none` and therefore out of the accessible name, so without the
  // label each would be named by its `title` instead -- "Jump to a pane or run
  // a command (…)" and "Show the reply box" -- and every locator in this suite
  // that names them would be naming something else at this width.
  //
  // `toHaveAccessibleName` and not a `getByRole` name option: that option
  // matches a substring by default, and "Reply box" is a substring of "Show the
  // reply box", so the locator would survive the very mutation this asserts
  // against. Measured: it did.
  await expect(page.getByRole('button', { name: /Jump to/ })).toHaveAccessibleName('Jump to…')
  await expect(page.getByRole('button', { name: /reply/i })).toHaveAccessibleName('Reply box')

  // Measured, both sides of the change, with this fixture: 74px of text in a
  // 56px box before -- the clip the owner was complaining about -- and 74 in 74
  // after, which is the span taking exactly what its text needs because the
  // room is finally there. The estimate in the design said ~30px of breadcrumb
  // and ~260 after; the real figures are 56 and enough, so the table was
  // optimistic about the old header and right about the shape of the fix.
  const fit = await command.evaluate((el) => ({
    text: el.scrollWidth,
    box: el.clientWidth,
  }))
  expect(
    fit.text,
    `the pane's command is clipped on a phone: ${fit.text}px of text in ${fit.box}px of box`,
  ).toBeLessThanOrEqual(fit.box)
})
