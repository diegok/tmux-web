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
  it('engages once past the threshold', () => {
    const s = new SwipeScroller()
    s.start(100, 400, NOTCH)
    s.move(100, 400 + SWIPE_THRESHOLD_PX)
    expect(s.engaged).toBe(true)
  })

  // Dragging DOWN pulls older content down from above, which is what every
  // touch surface does and what `deltaY < 0` means to @wterm (code 64).
  it('scrolls back into history when the finger moves down', () => {
    expect(drag([400, 400 + NOTCH])).toBe(-1)
    expect(drag([400, 400 + 3 * NOTCH])).toBe(-3)
  })

  it('scrolls forward when the finger moves up', () => {
    expect(drag([400, 400 - NOTCH])).toBe(1)
    expect(drag([400, 400 - 3 * NOTCH])).toBe(3)
  })

  // The content has to follow the finger whether the browser delivers the
  // gesture as two events or as fifty.
  it.each([1, 2, 5, 17, 50])('emits the same total however finely it is sampled (%i steps)', (steps) => {
    const total = 4 * NOTCH
    const positions = [400]
    for (let i = 1; i <= steps; i++) positions.push(400 + (total * i) / steps)
    expect(drag(positions)).toBe(-4)
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
    expect(total).toBe(-4)
  })

  it('counts the threshold travel towards the first notch', () => {
    // Exactly one notch of total travel produces exactly one notch, even
    // though the first SWIPE_THRESHOLD_PX of it went into deciding.
    expect(drag([400, 400 + SWIPE_THRESHOLD_PX, 400 + NOTCH])).toBe(-1)
  })

  // The totals here are net, and the content following the finger is what they
  // mean: a gesture that ends one notch above where it started has scrolled
  // forward by one, whatever route it took to get there.
  it('reverses mid-gesture without losing count', () => {
    // Down a notch (-1), then up two from there (+2): net +1, and the finger
    // did end one notch above the start.
    expect(drag([400, 400 + NOTCH, 400 - NOTCH])).toBe(1)
    // One more notch up: net +2, finger two notches above the start.
    expect(drag([400, 400 + NOTCH, 400 - NOTCH, 400 - 2 * NOTCH])).toBe(2)
    // And the invariant behind both: the net is the total displacement.
    expect(drag([400, 400 + 2 * NOTCH, 400])).toBe(0)
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
    expect(s.move(100 + SWIPE_THRESHOLD_PX, 400 + 2 * NOTCH)).toBe(-2)
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
