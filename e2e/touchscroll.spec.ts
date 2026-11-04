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
