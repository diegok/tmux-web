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

  await swipe(page, 300, 700)

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
