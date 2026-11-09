/**
 * Scrolling the terminal with a finger.
 *
 * It could not be done at all: `@wterm/dom` registers `keydown`, `paste`,
 * `mousedown` and `wheel`, and no touch listener of any kind, so a swipe
 * across the terminal changed nothing while a mouse wheel entered copy-mode
 * immediately. That asymmetry is the bug, and it is only visible with real
 * touch events against a real tmux -- `lib/touchScroll.test.ts` owns the
 * arithmetic and cannot tell you whether the events it produces move tmux.
 *
 * It is also the only place that can catch the failure the implementation was
 * rewritten for: a swipe that scrolls ONCE and then dies, because the repaint
 * detaches the node the finger came down on. See `installSwipeScroll`.
 *
 * The touches go through CDP because Playwright's touchscreen can tap but not
 * drag.
 */
import { test, expect, enroll, BASE_SESSION } from './harness'
import type { Page } from '@playwright/test'
import type { TmuxWeb } from './harness'

// A phone, because that is where this matters and where `isMobile` makes the
// browser deliver touches the way one does.
test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true })

const pane = `${BASE_SESSION}:0`

/** Drag one finger from y0 to y1 down the middle of the screen. */
async function swipe(page: Page, y0: number, y1: number, steps = 10): Promise<void> {
  const cdp = await page.context().newCDPSession(page)
  const x = 195
  const send = (type: 'touchStart' | 'touchMove' | 'touchEnd', y: number) =>
    cdp.send('Input.dispatchTouchEvent', {
      type,
      touchPoints: type === 'touchEnd' ? [] : [{ x, y }],
    })
  await send('touchStart', y0)
  for (let i = 1; i <= steps; i++) await send('touchMove', y0 + ((y1 - y0) * i) / steps)
  await send('touchEnd', y1)
  await cdp.detach()
}

/**
 * Drag with a pause between samples, so the finger leaves slowly.
 *
 * 25px every 120ms is 0.21px/ms, below MIN_FLICK_PX_PER_MS: a deliberate drag,
 * which must stop where it is put.
 */
async function slowDrag(page: Page, y0: number, y1: number): Promise<void> {
  const cdp = await page.context().newCDPSession(page)
  const x = 195
  const send = (type: 'touchStart' | 'touchMove' | 'touchEnd', y: number) =>
    cdp.send('Input.dispatchTouchEvent', {
      type,
      touchPoints: type === 'touchEnd' ? [] : [{ x, y }],
    })
  const steps = Math.round(Math.abs(y1 - y0) / 25)
  await send('touchStart', y0)
  for (let i = 1; i <= steps; i++) {
    await send('touchMove', y0 + ((y1 - y0) * i) / steps)
    await page.waitForTimeout(120)
  }
  await send('touchEnd', y1)
  await cdp.detach()
}

/** Put a finger down and lift it, without moving: below the swipe threshold. */
async function tap(page: Page, x: number, y: number): Promise<void> {
  const cdp = await page.context().newCDPSession(page)
  await cdp.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: [{ x, y }] })
  await cdp.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] })
  await cdp.detach()
}

const ask = (tmuxWeb: TmuxWeb, format: string) =>
  tmuxWeb.tmux('display-message', '-p', '-t', pane, format)

/** Enough output to have something above the viewport. */
async function fillScrollback(tmuxWeb: TmuxWeb): Promise<void> {
  tmuxWeb.tmux('send-keys', '-t', pane, 'seq 1 500', 'Enter')
  await expect
    .poll(() => tmuxWeb.tmux('capture-pane', '-p', '-t', pane), {
      message: 'the pane never filled with output to scroll',
    })
    .toContain('500')
}

test('a swipe down scrolls back into the scrollback, and up comes back', async ({
  page,
  tmuxWeb,
}) => {
  await enroll(page, tmuxWeb, 'swipe')
  await fillScrollback(tmuxWeb)

  // Nothing is in a mode to begin with, so nothing below can pass on a state
  // that was already there.
  expect(ask(tmuxWeb, '#{pane_mode}')).toBe('')

  // DRAGGED, not thrown, and that is now load-bearing: a flick keeps moving
  // after the finger is up, so reading a position straight after one measures
  // a moving target. MEASURED: this same gesture thrown settles at line 110,
  // and read immediately it gives 25 -- a number that would pass the bound
  // below by luck of timing. The flick has its own test.
  await slowDrag(page, 300, 700)

  await expect
    .poll(() => ask(tmuxWeb, '#{pane_mode}'), {
      message: 'a swipe down did not put the pane into copy-mode, so it scrolled nothing',
      timeout: 15_000,
    })
    .toBe('copy-mode')

  // The GAIN, not just "it moved". MEASURED on tmux 3.7b with a 17px row: a
  // notch is five lines, 400px of travel is four notches, the first of which
  // only enters the mode -- so three notches land and the position is 15. The
  // bounds are wide enough not to be brittle and tight enough to catch the
  // failure this was rewritten for, where the gesture dies after one notch and
  // the position stays at 0, and to catch a gain that is out by an order of
  // magnitude either way.
  const position = Number(ask(tmuxWeb, '#{scroll_position}'))
  expect(position).toBeGreaterThan(4)
  expect(position).toBeLessThan(40)

  // A scroll must not also be a selection. The compatibility mouse events a
  // touch produces are what tmux would read as a drag, and a swipe that
  // scrolled AND selected would be its own bug.
  expect(ask(tmuxWeb, '#{selection_present}')).toBe('0')

  // And back out again, which is the half that makes it usable: tmux leaves
  // copy-mode of its own accord once the view reaches the bottom.
  await swipe(page, 700, 300)
  await expect
    .poll(() => ask(tmuxWeb, '#{pane_mode}'), {
      message: 'swiping up never brought the pane back out of the scrollback',
      timeout: 15_000,
    })
    .toBe('')
})

// The dead zone, which is what "the swipe is unresponsive" turned out to mean.
// MEASURED before the priming notch existed: from a pane in no mode, the finger
// had to travel 180px -- half this viewport -- before one line of text moved,
// because 12px go to the threshold and then tmux spends a whole 85px notch
// entering copy-mode without scrolling. Only a real tmux can tell you this: the
// unit tests know the arithmetic and cannot know that tmux eats the first
// notch.
test('the text starts moving within half a notch of travel, not two', async ({
  page,
  tmuxWeb,
}) => {
  await enroll(page, tmuxWeb, 'deadzone')
  await fillScrollback(tmuxWeb)
  expect(ask(tmuxWeb, '#{pane_mode}')).toBe('')

  const cdp = await page.context().newCDPSession(page)
  const x = 195
  const send = (type: 'touchStart' | 'touchMove' | 'touchEnd', y: number) =>
    cdp.send('Input.dispatchTouchEvent', {
      type,
      touchPoints: type === 'touchEnd' ? [] : [{ x, y }],
    })

  // Creep down 20px at a time and note the travel at which the content first
  // moved. 100px is the budget: the threshold plus the one notch tmux spends
  // entering the mode, and nothing more.
  const BUDGET_PX = 100
  let movedAt = -1
  await send('touchStart', 120)
  for (let travel = 20; travel <= 300 && movedAt < 0; travel += 20) {
    await send('touchMove', 120 + travel)
    await page.waitForTimeout(150)
    if (Number(tmuxWeb.tmux('display-message', '-p', '-t', pane, '#{scroll_position}')) > 0) {
      movedAt = travel
    }
  }
  await send('touchEnd', 420)
  await cdp.detach()

  expect(movedAt, 'the content never moved at all within 300px of travel').toBeGreaterThan(0)
  expect(
    movedAt,
    `the first line of text only moved after ${movedAt}px of finger travel`,
  ).toBeLessThanOrEqual(BUDGET_PX)
})

// The other half of the gesture contract, and the reason for the threshold: a
// tap is how the user puts the cursor somewhere and how tmux learns which pane
// was touched. A recogniser that engaged on a stray pixel would cost them that,
// and would be maddening on a phone.
test('a tap does not scroll', async ({ page, tmuxWeb }) => {
  await enroll(page, tmuxWeb, 'tap')
  await fillScrollback(tmuxWeb)

  // A few pixels of drift, which is what a real thumb does.
  await swipe(page, 400, 404, 2)
  await page.waitForTimeout(1500)

  expect(ask(tmuxWeb, '#{pane_mode}')).toBe('')
})

// --- the flick ---------------------------------------------------------------
//
// A gesture that stops dead when the finger lifts means one screenful per
// swipe: MEASURED, 700px of travel is 30 lines, so reading back through what an
// agent printed took a dozen swipes. `lib/touchScroll.test.ts` owns the physics
// and cannot say whether a thrown finger moves a real tmux further than a
// dragged one. That pair is the whole contract, and it needs both halves:
// without the slow drag, a gain bug would pass this file.

test('a thrown finger carries on, and a dragged one stops where it is put', async ({
  page,
  tmuxWeb,
}) => {
  test.setTimeout(90_000)
  await enroll(page, tmuxWeb, 'flick')
  await fillScrollback(tmuxWeb)
  expect(ask(tmuxWeb, '#{pane_mode}')).toBe('')

  // The same 300px of travel, dragged. MEASURED without a glide: 300px is
  // three notches past the one tmux spends entering copy-mode, so about 15
  // lines.
  await slowDrag(page, 120, 420)
  await expect
    .poll(() => ask(tmuxWeb, '#{pane_mode}'), { timeout: 15_000 })
    .toBe('copy-mode')
  const dragged = Number(ask(tmuxWeb, '#{scroll_position}'))
  expect(dragged, 'a slow drag did not scroll at all').toBeGreaterThan(4)
  expect(
    dragged,
    `a 300px drag reached line ${dragged}, which means it glided when it should not`,
  ).toBeLessThan(40)

  // Back to the bottom, so the flick starts from the same place.
  tmuxWeb.tmux('send-keys', '-X', '-t', pane, 'cancel')
  await expect.poll(() => ask(tmuxWeb, '#{pane_mode}'), { timeout: 15_000 }).toBe('')

  // And now the same distance, thrown.
  await swipe(page, 120, 420, 4)
  await expect
    .poll(() => ask(tmuxWeb, '#{pane_mode}'), { timeout: 15_000 })
    .toBe('copy-mode')

  // It is still moving, so wait for it to settle rather than reading mid-flight.
  let settled = 0
  await expect
    .poll(
      () => {
        const now = Number(ask(tmuxWeb, '#{scroll_position}'))
        const stable = now === settled
        settled = now
        return stable && now > 0
      },
      { message: 'the flick never came to a stop', timeout: 20_000 },
    )
    .toBe(true)

  expect(
    settled,
    `the flick stopped at line ${settled}, no further than the drag reached (${dragged})`,
  ).toBeGreaterThan(dragged * 2)
  // And bounded: GLIDE_MAX_TRAVEL_PX is 4000px, about 235 lines at a 17px row,
  // so a flick that reached the far end of a 500-line scrollback would mean the
  // cap is not working.
  expect(settled, `the flick flew to line ${settled}`).toBeLessThan(300)
})

// A flick that cannot be stopped is worse than no flick: the user throws the
// pane, sees the line they wanted go past, and has to chase it. Every touch
// surface stops on contact, and only a browser can say whether this one does --
// the physics class knows `stop()` works, not that a finger calls it.
test('a touch stops a flick where it is touched', async ({ page, tmuxWeb }) => {
  test.setTimeout(60_000)
  await enroll(page, tmuxWeb, 'stopflick')
  await fillScrollback(tmuxWeb)

  await swipe(page, 120, 420, 4)
  await expect.poll(() => ask(tmuxWeb, '#{pane_mode}'), { timeout: 15_000 }).toBe('copy-mode')

  // Mid-flight, a tap. MEASURED: the same flick left alone settles at line
  // 110, so a position well short of that is proof this landed while the pane
  // was still moving rather than after it had stopped on its own.
  //
  // Re-measured after the glide's notches were capped, because a cap could only
  // have made it shorter: still 110, with the finger's own drag 15 of it. The
  // cap does not bite in this regime -- 1600px of travel at an 85px notch is 19
  // notches, spread over the ~92 frames the friction takes to give up, so it
  // never asks for two in one frame. It bites at a 17px notch, where the same
  // travel wants 94.
  await tap(page, 195, 400)
  const atTouch = Number(ask(tmuxWeb, '#{scroll_position}'))
  expect(
    atTouch,
    `the pane was already at line ${atTouch} when it was touched, so the flick had finished and this proves nothing`,
  ).toBeLessThan(90)

  await page.waitForTimeout(1200)
  expect(
    Number(ask(tmuxWeb, '#{scroll_position}')),
    'the pane kept gliding after it was touched',
  ).toBe(atTouch)
})

// --- the other regime --------------------------------------------------------
//
// Every test above runs over a shell pane at a prompt, which is the case tmux
// handles ITSELF: it puts the pane into copy-mode and scrolls its own table, 5
// lines per notch. That is one of two regimes, and it is not the one this app
// exists to drive. With the pane on the alternate screen or tracking the mouse
// -- which every coding agent does, both at once -- tmux forwards the notch to
// the application instead, and the application scrolls by its own quantum. The
// gesture used to assume 5 there too, so a swipe moved the content a FIFTH as
// far as the finger.
//
// The receiver here is `cat -v` on the alternate screen with tracking on: a
// program with no agent in it, whose quantum is therefore
// DEFAULT_APP_LINES_PER_NOTCH, and which prints one visible `^[[<64;c;rM` per
// report it is handed -- so the reports can be counted off the screen rather
// than inferred from how far something moved.
test('a swipe over an application sends it a notch per row times its quantum', async ({
  page,
  tmuxWeb,
}) => {
  test.setTimeout(90_000)
  await enroll(page, tmuxWeb, 'appregime')

  // stty raw -echo so `cat` is handed each report as it arrives rather than a
  // line at a time, and so the tty does not echo the escapes back into the
  // screen it is being counted off.
  tmuxWeb.tmux(
    'send-keys',
    '-t',
    pane,
    String.raw`stty raw -echo; printf '\033[?1049h\033[?1003h\033[?1006h'; cat -v`,
    'Enter',
  )
  await expect
    .poll(() => ask(tmuxWeb, '#{alternate_on}#{mouse_any_flag}'), {
      message: 'the pane never turned on the alternate screen and mouse tracking',
      timeout: 15_000,
    })
    .toBe('11')
  // `cat` is in no quantum table, so the gain is the default three.
  expect(ask(tmuxWeb, '#{pane_current_command}')).toBe('cat')

  // The facts have to reach the BROWSER before the swipe, and they ride the
  // snapshot poll -- so this waits for the wire rather than for a timeout, and
  // is an end-to-end check of the two fields at the same time.
  await expect
    .poll(
      () =>
        page.evaluate(async () => {
          const res = await fetch('/api/snapshot', { credentials: 'same-origin' })
          const body = (await res.json()) as {
            panes?: { alternateOn?: boolean; mouseAnyFlag?: boolean }[]
          }
          return (body.panes ?? []).some((p) => p.alternateOn === true && p.mouseAnyFlag === true)
        }),
      { message: 'the daemon never published the pane as being on the alternate screen' },
    )
    .toBe(true)

  // And then the last hop, which the fetch above does NOT prove: that poll read
  // the daemon's cache, and the app runs a 1.5s poll of its own that nothing
  // here pushes. The gain is latched once, at touchstart, off the app's own
  // tree -- so a swipe that started between the two polls would take tmux's
  // five lines and fail this test for a reason that is not a bug. The
  // breadcrumb renders `located.pane.command`, and the command only becomes
  // `cat` after the `printf` that set both facts has already run, so seeing it
  // proves the row the gain is read from is the row with the facts in it.
  await expect(page.getByLabel('Location')).toContainText('cat')

  // Measured, not assumed: the terminal settings feature makes the row height
  // variable, and the whole gain is a multiple of it.
  const rowHeight = await page.evaluate(
    () => document.querySelector('.term-row')!.getBoundingClientRect().height,
  )
  expect(rowHeight).toBeGreaterThan(0)

  const TRAVEL_PX = 300
  await slowDrag(page, 120, 120 + TRAVEL_PX)

  // One report per notch, counted off the screen.
  //
  // The prediction is exact rather than approximate, and the ±1 is for a
  // dropped sample rather than for slack: the travel buys
  // `floor(TRAVEL / notchPx)` notches and the gesture emits ONE MORE the moment
  // it engages -- the priming notch, which is what takes the first visible
  // movement from 180px of travel down to 95px. At a 17px row that is 6.
  //
  // The band has to be tight enough to fail on the bug this exists for: tmux's
  // own five lines a notch would buy `floor(300 / 85) + 1` = 4 here, which a
  // looser lower bound would have let through.
  const notchPx = rowHeight * 3
  const expected = Math.floor(TRAVEL_PX / notchPx) + 1
  const atFiveLines = Math.floor(TRAVEL_PX / (rowHeight * 5)) + 1
  expect(
    expected - 1,
    `this fixture cannot tell the two gains apart: ${expected} notches at the ` +
      `application's quantum against ${atFiveLines} at tmux's`,
  ).toBeGreaterThan(atFiveLines)

  // `-J` joins wrapped lines: a report that straddled the last column would
  // otherwise be split by a newline mid-token and not be counted, which errs
  // toward passing a swipe that sent too many.
  const count = () =>
    (tmuxWeb.tmux('capture-pane', '-p', '-J', '-t', pane).match(/<64;/g) ?? []).length
  await expect
    .poll(count, {
      message: `a ${TRAVEL_PX}px drag should send ${expected} notches to the application`,
      timeout: 15_000,
    })
    .toBeGreaterThanOrEqual(expected - 1)
  const reports = count()
  expect(
    reports,
    `the application received ${reports} notches for ${TRAVEL_PX}px of finger: at ` +
      `${rowHeight}px a row and 3 lines a notch it should be ${expected}. Five lines a ` +
      `notch -- tmux's quantum, which is what this assumed before the gain followed ` +
      `the receiver -- would be ${atFiveLines}`,
  ).toBeLessThanOrEqual(expected + 1)
})
