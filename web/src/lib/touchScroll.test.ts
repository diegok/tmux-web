import { describe, expect, it } from 'vitest'

import {
  FALLBACK_ROW_HEIGHT_PX,
  GLIDE_TRAVEL_CEILING_PX,
  Glide,
  MAX_FLICK_PX_PER_MS,
  MIN_FLICK_PX_PER_MS,
  SWIPE_THRESHOLD_PX,
  SwipeScroller,
  TMUX_LINES_PER_NOTCH,
  framesIn,
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

/** Run a glide to a standstill and return the pixels it covered. */
function total(g: Glide, dtMs = 16): number {
  let sum = 0
  let frames = 0
  while (g.moving && frames++ < 10_000) sum += g.step(dtMs)
  return sum
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

  it('sizes a notch for the receiver, and for the font', () => {
    // Not tmux's five: a notch forwarded to an application moves that
    // application's own lines, measured at 1 for claude and pi and 3 for
    // opencode. See `scrollGain.ts`.
    expect(pxPerNotch(17, 1)).toBe(17)
    expect(pxPerNotch(17, 3)).toBe(51)
    // The terminal settings feature makes the row variable: 20px text must
    // move 20px rows. The two features meet here for the first time.
    expect(pxPerNotch(20, 1)).toBe(20)
    // Before the terminal has laid out, the fallback row times the RECEIVER's
    // lines -- not tmux's five.
    expect(pxPerNotch(0, 1)).toBe(FALLBACK_ROW_HEIGHT_PX)
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

// --- the flick ---------------------------------------------------------------
//
// WHY. A gesture that stops dead when the finger lifts means one screenful per
// swipe: measured, a 700px drag is 30 lines. Reading back through what an agent
// printed takes a dozen swipes, which is the other half of "the scroll feels
// unresponsive". Every touch surface answers a flick by carrying on.

describe('a flick', () => {
  it('does not start from a slow drag', () => {
    const g = new Glide()
    // Half the threshold: a finger being dragged deliberately, not thrown.
    expect(g.start(MIN_FLICK_PX_PER_MS / 2)).toBe(false)
    expect(g.moving).toBe(false)
    expect(g.step(16)).toBe(0)
  })

  it('starts from a throw, in the direction of the throw', () => {
    const g = new Glide()
    expect(g.start(2)).toBe(true)
    expect(g.moving).toBe(true)
    expect(g.step(16)).toBeGreaterThan(0)

    const up = new Glide()
    up.start(-2)
    expect(up.step(16)).toBeLessThan(0)
  })

  it('slows down and stops by itself', () => {
    const g = new Glide()
    g.start(2)
    let last = Infinity
    let frames = 0
    while (g.moving && frames < 10_000) {
      const px = Math.abs(g.step(16))
      // Monotonically decaying, never a step bigger than the one before.
      expect(px).toBeLessThanOrEqual(last + 1e-9)
      last = px
      frames++
    }
    expect(g.moving).toBe(false)
    // A flick lasts a moment, not a minute: bounds measured from FRICTION.
    expect(frames).toBeGreaterThan(5)
    expect(frames).toBeLessThan(200)
  })

  it('carries a throw further than a slow drag', () => {
    const slow = new Glide()
    slow.start(MIN_FLICK_PX_PER_MS * 1.1)
    const fast = new Glide()
    fast.start(4)
    expect(Math.abs(total(fast))).toBeGreaterThan(Math.abs(total(slow)) * 2)
  })

  // Frame rate must not change where the content ends up. A phone that drops
  // frames would otherwise scroll a different distance from one that does not.
  it('travels about the same distance however coarse the frames are', () => {
    const fine = new Glide()
    fine.start(2)
    const coarse = new Glide()
    coarse.start(2)
    const a = total(fine, 8)
    const b = total(coarse, 32)
    expect(Math.abs(a - b) / Math.abs(a)).toBeLessThan(0.25)
  })

  // However hard it is thrown, it must not fly to the far end of a 100k-line
  // scrollback: that is a lost place, not a scroll. The bound is not a cap but
  // the sum of the series -- speed clamp times frame over one minus friction --
  // so this fails if either of those is loosened.
  it('never travels further than the speed clamp and the friction allow', () => {
    const g = new Glide()
    g.start(500)
    expect(Math.abs(total(g))).toBeLessThanOrEqual(GLIDE_TRAVEL_CEILING_PX + 1)
    // And the ceiling is a real limit, not a number so large it cannot bite:
    // 1600px is about 94 lines of a 17px row.
    expect(GLIDE_TRAVEL_CEILING_PX).toBeLessThan(2000)
  })

  // Velocity is a division, and a coalesced pointermove with a 1ms stamp would
  // otherwise read as hundreds of px/ms and spend the whole travel cap in a
  // single frame -- a jump, not a scroll.
  it('does not believe an impossible speed', () => {
    const sane = new Glide()
    sane.start(MAX_FLICK_PX_PER_MS)
    const absurd = new Glide()
    absurd.start(MAX_FLICK_PX_PER_MS * 1000)
    expect(absurd.step(16)).toBeCloseTo(sane.step(16), 6)
  })

  it('can be stopped in its tracks, which is what a finger coming down does', () => {
    const g = new Glide()
    g.start(3)
    g.step(16)
    g.stop()
    expect(g.moving).toBe(false)
    expect(g.step(16)).toBe(0)
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

describe('the glide\u2019s notches, capped', () => {
  // Once the gain can be 17px instead of 85px, the same flick asks for ~94
  // notches instead of 19 -- more than it can deliver inside its own ~600ms
  // lifetime, so without a cap it would go on scrolling well after the gesture
  // visibly stopped. Capping and discarding the overflow lands every regime in
  // the same place without modelling anybody's accelerator, and it is measured
  // that a tight burst is worth LESS than the same notches spread over frames
  // (16 lines against 19), so the queued ones were never worth their count.
  it('pays the finger every notch it earns and the glide one per frame', () => {
    const s = new SwipeScroller()
    s.start(100, 100, 17)
    s.move(100, 100 + SWIPE_THRESHOLD_PX)
    // A drag that crosses four notches in one move delivers four: one notch
    // per frame at 17px is 1018px/s of finger, past any thumb, so a cap on the
    // finger could only ever lag a fast drag.
    expect(Math.abs(s.move(100, 100 + SWIPE_THRESHOLD_PX + 4 * 17))).toBe(4)
    // The glide, in both directions.
    expect(s.glide(5 * 17)).toBe(-1)
    expect(s.glide(-5 * 17)).toBe(1)
  })

  // The cap is per 16ms of glide, NOT per call, and this is the invariant
  // `Glide.step` already promises in its own words: "a phone that drops frames
  // must not scroll a different distance from one that does not". It keeps that
  // promise in PIXELS. Counting the cap per callback would break it in NOTCHES,
  // which is the thing the user actually sees -- measured by simulation, a
  // 17px notch throws 44 notches at 60fps against 27 at 30fps, so a phone
  // repainting a busy agent pane would throw a third shorter.
  it('throws the same distance however often the frames arrive', () => {
    const thrown = (notchPx: number, dtMs: number): number => {
      const scroller = new SwipeScroller()
      scroller.start(100, 100, notchPx)
      scroller.move(100, 100 + SWIPE_THRESHOLD_PX)
      const g = new Glide()
      g.start(MAX_FLICK_PX_PER_MS)
      let notches = 0
      while (g.moving) {
        notches += Math.abs(scroller.glide(g.step(dtMs), framesIn(dtMs)))
      }
      return notches
    }
    for (const notchPx of [17, 51, 85]) {
      const fast = thrown(notchPx, 16)
      const slow = thrown(notchPx, 33)
      expect(
        Math.abs(fast - slow),
        `a ${notchPx}px notch throws ${fast} notches at 60fps and ${slow} at 30fps`,
      ).toBeLessThanOrEqual(2)
    }
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
