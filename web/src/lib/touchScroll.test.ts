import { describe, expect, it } from 'vitest'

import {
  FALLBACK_ROW_HEIGHT_PX,
  SWIPE_THRESHOLD_PX,
  SwipeScroller,
  TMUX_LINES_PER_NOTCH,
  pxPerNotch,
} from './touchScroll'

/** One notch, for a 17px row: the geometry measured in a real browser. */
const NOTCH = pxPerNotch(17)

/** Drag a finger from y0 by a series of absolute positions, summing notches. */
function drag(positions: number[], x = 100, notch = NOTCH): number {
  const s = new SwipeScroller()
  s.start(x, positions[0], notch)
  let total = 0
  for (const y of positions.slice(1)) total += s.move(x, y)
  s.end()
  return total
}

describe('pxPerNotch', () => {
  it('is the row height times what tmux scrolls for one notch', () => {
    expect(pxPerNotch(17)).toBe(17 * TMUX_LINES_PER_NOTCH)
    expect(pxPerNotch(24)).toBe(24 * TMUX_LINES_PER_NOTCH)
  })

  // The terminal has no row height before it lays out, and a notch of 0px would
  // be an infinite loop in `move`.
  it.each([0, -1, Number.NaN])('falls back rather than returning zero for %s', (h) => {
    expect(pxPerNotch(h)).toBe(FALLBACK_ROW_HEIGHT_PX * TMUX_LINES_PER_NOTCH)
  })
})

describe('a tap', () => {
  // The whole reason for the threshold. A tap is how the user puts the cursor
  // somewhere and how tmux learns which pane was touched; a recogniser that
  // engaged on a stray pixel would cost them that.
  it('produces no notches and never engages', () => {
    const s = new SwipeScroller()
    s.start(100, 400, NOTCH)
    expect(s.move(101, 403)).toBe(0)
    expect(s.move(100, 399)).toBe(0)
    expect(s.engaged).toBe(false)
  })

  it.each([0, 1, SWIPE_THRESHOLD_PX - 1])('does not engage after %ipx of travel', (dy) => {
    const s = new SwipeScroller()
    s.start(100, 400, NOTCH)
    s.move(100, 400 + dy)
    expect(s.engaged).toBe(false)
  })
})

describe('a vertical swipe', () => {
  // MEASURED end to end before this existed: from a pane not in copy mode, the
  // finger had to travel 180px -- half a phone screen -- before one line of
  // text moved. 12px to pass the threshold, then a whole 85px notch that tmux
  // spends entering copy mode without scrolling, then a second notch to
  // actually move. That is what "unresponsive" was.
  //
  // So the gesture answers at the threshold with a priming notch. It is NOT
  // charged to the accumulator, and that is the whole point: charging it would
  // repay the loan out of the first real notch and put the first movement back
  // at 170px, which is the bug. The cost is in `reverses mid-gesture`.
  it('emits a notch the instant it engages, so the gesture answers at once', () => {
    const s = new SwipeScroller()
    s.start(100, 400, NOTCH)
    expect(s.move(100, 400 + SWIPE_THRESHOLD_PX)).toBe(-1)
  })

  it('primes in the direction the finger is going, not always back', () => {
    const s = new SwipeScroller()
    s.start(100, 400, NOTCH)
    expect(s.move(100, 400 - SWIPE_THRESHOLD_PX)).toBe(1)
  })

  // The lead is one notch for the whole gesture, not one per sample: a priming
  // notch per move would run away from the finger.
  it('primes once, not once per sample', () => {
    const s = new SwipeScroller()
    s.start(100, 400, NOTCH)
    s.move(100, 400 + SWIPE_THRESHOLD_PX)
    expect(s.move(100, 400 + SWIPE_THRESHOLD_PX + 1)).toBe(0)
    expect(s.move(100, 400 + SWIPE_THRESHOLD_PX + 2)).toBe(0)
  })

  it('engages once past the threshold', () => {
    const s = new SwipeScroller()
    s.start(100, 400, NOTCH)
    s.move(100, 400 + SWIPE_THRESHOLD_PX)
    expect(s.engaged).toBe(true)
  })

  // Dragging DOWN pulls older content down from above, which is what every
  // touch surface does and what `deltaY < 0` means to @wterm (code 64).
  // One notch of travel is one notch of content PLUS the priming notch above,
  // in every total below.
  it('scrolls back into history when the finger moves down', () => {
    expect(drag([400, 400 + NOTCH])).toBe(-2)
    expect(drag([400, 400 + 3 * NOTCH])).toBe(-4)
  })

  it('scrolls forward when the finger moves up', () => {
    expect(drag([400, 400 - NOTCH])).toBe(2)
    expect(drag([400, 400 - 3 * NOTCH])).toBe(4)
  })

  // The content has to follow the finger whether the browser delivers the
  // gesture as two events or as fifty.
  it.each([1, 2, 5, 17, 50])('emits the same total however finely it is sampled (%i steps)', (steps) => {
    const total = 4 * NOTCH
    const positions = [400]
    for (let i = 1; i <= steps; i++) positions.push(400 + (total * i) / steps)
    expect(drag(positions)).toBe(-5)
  })

  // Travel is kept, not rounded away: a slow drag of half a notch at a time
  // still scrolls, and the threshold's own travel counts towards the first
  // notch rather than being dropped.
  it('accumulates travel smaller than a notch', () => {
    const s = new SwipeScroller()
    s.start(100, 400, NOTCH)
    let y = 400
    let total = 0
    for (let i = 0; i < 8; i++) {
      y += NOTCH / 2
      total += s.move(100, y)
    }
    expect(total).toBe(-5)
  })

  it('counts the threshold travel towards the first notch', () => {
    // Exactly one notch of total travel produces exactly one notch, even
    // though the first SWIPE_THRESHOLD_PX of it went into deciding.
    expect(drag([400, 400 + SWIPE_THRESHOLD_PX, 400 + NOTCH])).toBe(-2)
  })

  // The totals here are net, and they are the displacement MINUS the priming
  // notch -- which is the price of the priming notch, stated. A gesture that
  // returns to where it began has still scrolled one notch in the direction it
  // set off in, because that notch was never charged for. Everything after the
  // first notch tracks the finger exactly, which is what makes the offset a
  // constant and not a drift.
  it('reverses mid-gesture, one notch behind the displacement', () => {
    // Down a notch then back up to the start: displacement 0, and what is left
    // is the priming notch, still spent downward.
    expect(drag([400, 400 + NOTCH, 400 - NOTCH])).toBe(0)
    // One notch further up: displacement +1, net +1 -- not +2, for the same
    // reason.
    expect(drag([400, 400 + NOTCH, 400 - NOTCH, 400 - 2 * NOTCH])).toBe(1)
    // Two notches down and all the way back: displacement 0, net -1.
    expect(drag([400, 400 + 2 * NOTCH, 400])).toBe(-1)
    // The drift test: a longer round trip leaves the SAME single notch behind,
    // never a growing one.
    expect(drag([400, 400 + 6 * NOTCH, 400])).toBe(-1)
  })
})

describe('gestures that are not a scroll', () => {
  // A sideways drag belongs to whatever else wants it. Hijacking it would make
  // horizontal selection impossible and would feel like the terminal fighting
  // the finger.
  it('is rejected when it starts sideways', () => {
    const s = new SwipeScroller()
    s.start(100, 400, NOTCH)
    expect(s.move(100 + SWIPE_THRESHOLD_PX, 400)).toBe(0)
    expect(s.engaged).toBe(false)
  })

  // Latched, deliberately: a drag that starts sideways and then curves down
  // must not turn into a scroll under the user's finger.
  it('stays rejected when a sideways drag later turns vertical', () => {
    const s = new SwipeScroller()
    s.start(100, 400, NOTCH)
    s.move(100 + SWIPE_THRESHOLD_PX, 400)
    expect(s.move(100 + SWIPE_THRESHOLD_PX, 400 + 10 * NOTCH)).toBe(0)
    expect(s.engaged).toBe(false)
  })

  // A diagonal drag that is mostly vertical is still a scroll: fingers are not
  // straight.
  it('engages on a mostly-vertical diagonal', () => {
    const s = new SwipeScroller()
    s.start(100, 400, NOTCH)
    expect(s.move(100 + SWIPE_THRESHOLD_PX, 400 + 2 * NOTCH)).toBe(-3)
    expect(s.engaged).toBe(true)
  })

  // Pinch-zoom: the caller rejects on a second finger, and the rest of the
  // gesture must stay rejected however it moves.
  it('emits nothing once rejected for a second finger', () => {
    const s = new SwipeScroller()
    s.start(100, 400, NOTCH)
    s.move(100, 400 + 2 * NOTCH)
    expect(s.engaged).toBe(true)
    s.reject()
    expect(s.move(100, 400 + 10 * NOTCH)).toBe(0)
    expect(s.engaged).toBe(false)
  })
})

describe('between gestures', () => {
  it('starts each gesture from scratch', () => {
    const s = new SwipeScroller()
    s.start(100, 400, NOTCH)
    s.move(100, 400 + NOTCH)
    s.end()
    expect(s.engaged).toBe(false)
    // A new gesture at a wildly different y must not read as enormous travel.
    s.start(100, 50, NOTCH)
    expect(s.move(100, 51)).toBe(0)
  })
})
