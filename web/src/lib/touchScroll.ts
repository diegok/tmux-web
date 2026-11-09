/**
 * Turning a finger into tmux's scrollback.
 *
 * ## Why there is anything to do
 *
 * On a phone the terminal could not be scrolled at all. MEASURED with real
 * touch events: a vertical swipe across the terminal changed nothing --
 * `pane_mode` empty before and after -- while a single mouse wheel event
 * produced `copy-mode` immediately. `@wterm/dom` registers `keydown`, `paste`,
 * `mousedown` and `wheel`, and NO touch listener of any kind, so there was
 * nothing for a finger to drive.
 *
 * ## Why it emits wheel events rather than mouse reports
 *
 * tmux already understands the wheel, and `@wterm/dom` already knows how to
 * encode one for whatever mouse mode the application turned on -- SGR, and only
 * when the application asked for tracking at all, which is the check that keeps
 * this from sending junk to a pane running something that does not want it.
 * Re-encoding `\x1b[<64;c;rM` here would be a second implementation of that
 * decision, and a worse one, because it would have to guess the mode.
 *
 * Measured, from `@wterm/dom`'s own handler: ONE wheel event is ONE mouse
 * report, and `deltaY`'s magnitude is discarded -- only its sign is read
 * (`deltaY < 0` is code 64, back into history; `> 0` is 65). So a swipe has to
 * emit one event per notch it wants, and a single event with a big delta does
 * nothing extra. That is what this class counts.
 *
 * ## The gain
 *
 * MEASURED end to end in a real browser against a real tmux 3.7b: the first
 * notch only enters copy-mode, and every notch after it moves
 * `scroll_position` by 5. With a row 17px tall, one notch is 85px of content,
 * so one notch per 85px of finger travel makes the text follow the finger.
 * TMUX_LINES_PER_NOTCH is tmux's default rather than a law -- a configured
 * `WheelUpPane` could make it anything -- and getting it wrong changes how fast
 * the content moves, never whether it moves.
 *
 * AND IT IS NOT ALWAYS FIVE. Those five lines are tmux's copy-mode table, and
 * tmux only scrolls that table when the pane is in a mode or has neither the
 * alternate screen nor mouse tracking on. Otherwise it forwards the notch to
 * the application, which scrolls by its own quantum -- 1 line in `claude` and
 * `pi`, 3 in `opencode`, all measured -- so a swipe over an agent pane moved
 * the content a FIFTH as far as the finger. Which of the two applies is
 * `scrollGain.ts`'s decision; this module takes the answer as a number, through
 * the getter `installSwipeScroll` is given, and multiplies the row by it.
 *
 * ## The priming notch, and the dead zone it removes
 *
 * 1:1 is right in the steady state and was wrong at the start. MEASURED on a
 * 390x844 viewport against a real tmux, swiping down from a pane in no mode:
 *
 *   |  finger travel | what the user saw                        |
 *   | -------------- | ---------------------------------------- |
 *   |      0 - 75px  | nothing                                  |
 *   |          90px  | copy-mode entered, scroll_position 0     |
 *   |         180px  | scroll_position 5 -- the FIRST movement  |
 *
 * Half a phone screen of travel before one line moved, because the threshold
 * costs 12px and then tmux spends an entire notch entering copy-mode without
 * scrolling. Latency was not the problem: the position was already updated
 * ~5ms after each move, so tmux was keeping up the whole time.
 *
 * So `move` emits one notch the moment it engages. In a pane already in
 * copy-mode the content moves at 12px of travel; in a pane that has to enter
 * copy-mode first that notch is swallowed, and the first movement comes at
 * 95px instead of 180px. The remaining 85px is tmux's own 5-line quantum, not
 * something this module can shrink while it scrolls with the wheel.
 *
 * The priming notch is not charged to the accumulator, and that is a real
 * trade, not an oversight: charging it would repay the loan out of the first
 * real notch and put the first movement back at 170px. The cost is that a
 * gesture leads the finger by one notch, so a swipe that goes down and comes
 * back to where it started has scrolled one notch back. `touchScroll.test.ts`
 * nails both halves.
 */

/**
 * How far tmux scrolls for one wheel notch, in lines. Measured on 3.7b with
 * the default bindings; see the module comment.
 */
export const TMUX_LINES_PER_NOTCH = 5

/**
 * Finger travel before a touch is treated as a scroll.
 *
 * It exists so that a TAP is still a tap. A tap is how the user puts the cursor
 * somewhere and how tmux is told which pane was touched, and a recogniser that
 * engaged on the first stray pixel would cost them that.
 */
export const SWIPE_THRESHOLD_PX = 12

/** Row height to assume when the terminal has not laid out yet. */
export const FALLBACK_ROW_HEIGHT_PX = 17

/**
 * One notch of content, in CSS pixels: a row times the lines the RECEIVER
 * scrolls for one notch.
 *
 * `linesPerNotch` defaults to tmux's five, which is right only while tmux is
 * the thing handling the wheel. `installSwipeScroll` passes what
 * `scrollGain.ts` decided for the attached pane, which is 1 in `claude` and
 * `pi` and 3 in `opencode` -- and getting it wrong changes how fast the content
 * moves, never whether it moves.
 */
export function pxPerNotch(rowHeight: number, linesPerNotch = TMUX_LINES_PER_NOTCH): number {
  const h = rowHeight > 0 ? rowHeight : FALLBACK_ROW_HEIGHT_PX
  const lines = linesPerNotch > 0 ? linesPerNotch : TMUX_LINES_PER_NOTCH
  return h * lines
}

/**
 * How fast a finger must be leaving the glass for the gesture to carry on
 * without it, in CSS pixels per millisecond. 0.3 is 300px a second: below that
 * the finger is being dragged deliberately and should stop where it is put.
 */
export const MIN_FLICK_PX_PER_MS = 0.3

/**
 * The fastest a flick is believed to be, in px/ms. 6 is 6000px a second, past
 * anything a thumb does on a 400px-wide screen, and it exists because velocity
 * is a division: one coalesced pointermove with a 1ms stamp would otherwise
 * read as hundreds of px/ms and spend the whole travel cap in one frame.
 */
export const MAX_FLICK_PX_PER_MS = 6

/** Below this speed a glide has arrived. */
export const GLIDE_STOP_PX_PER_MS = 0.02

/** The frame the friction is quoted for. */
const GLIDE_FRAME_MS = 16

/**
 * What is left of the speed after each 16ms of gliding. 0.94 per frame decays a
 * flick to a tenth in about 600ms, which is roughly how long a thrown list
 * takes to settle on a phone and short enough that a second swipe is never
 * fighting the first.
 */
export const GLIDE_FRICTION_PER_FRAME = 0.94

/**
 * The furthest a single flick can carry, in CSS pixels.
 *
 * DERIVED, not chosen, and that is the point: a geometric series of
 * `v * dt * friction^n` sums to `v * dt / (1 - friction)`, so the speed clamp
 * and the friction already bound the travel and a separate cap would be a
 * number that never fires. There WAS one, at 4000px, and mutation testing
 * found it: removing it changed nothing any test could see, because the real
 * ceiling is 1600px -- about 94 lines at a 17px row. MEASURED end to end: a
 * 300px flick settles 110 lines back, which is 1870px of travel counting the
 * finger's own 300px.
 */
export const GLIDE_TRAVEL_CEILING_PX =
  (MAX_FLICK_PX_PER_MS * GLIDE_FRAME_MS) / (1 - GLIDE_FRICTION_PER_FRAME)

/**
 * What is left of a flick once the finger is gone.
 *
 * Pure arithmetic, stepped by the caller, so the frame loop lives with the DOM
 * and the physics can be tested without one. Positive is the direction a finger
 * moving down was going.
 */
export class Glide {
  #velocity = 0

  /**
   * Begin gliding at this speed, in px/ms. Returns whether it took: a gesture
   * slower than MIN_FLICK_PX_PER_MS was a drag and ends where it was left.
   */
  start(velocity: number): boolean {
    if (!Number.isFinite(velocity) || Math.abs(velocity) < MIN_FLICK_PX_PER_MS) {
      this.#velocity = 0
      return false
    }
    this.#velocity =
      Math.sign(velocity) * Math.min(Math.abs(velocity), MAX_FLICK_PX_PER_MS)
    return true
  }

  get moving(): boolean {
    return this.#velocity !== 0
  }

  /** Give up on the rest of the glide. A finger coming down does this. */
  stop(): void {
    this.#velocity = 0
  }

  /**
   * Advance by one frame of dtMs and return the pixels covered.
   *
   * The distance is taken at the speed held BEFORE the friction is applied, and
   * the friction is raised to dt/16 rather than applied once per call, so a
   * 30fps frame covers what two 60fps frames would. A phone that drops frames
   * must not scroll a different distance from one that does not.
   */
  step(dtMs: number): number {
    if (!this.moving || dtMs <= 0) return 0
    const px = this.#velocity * dtMs
    this.#velocity *= GLIDE_FRICTION_PER_FRAME ** (dtMs / GLIDE_FRAME_MS)
    if (Math.abs(this.#velocity) < GLIDE_STOP_PX_PER_MS) this.#velocity = 0
    return px
  }
}

/**
 * One finger's worth of gesture, from touchstart to touchend.
 *
 * A class rather than a reducer because the caller is a DOM listener with
 * nowhere to keep state, and because "is this gesture a scroll yet" is exactly
 * what the listener needs in order to decide whether to call `preventDefault`.
 */
export class SwipeScroller {
  #startX = 0
  #startY = 0
  #lastY = 0
  #pxPerNotch = pxPerNotch(FALLBACK_ROW_HEIGHT_PX)
  /** Signed finger travel not yet turned into notches; positive is downward. */
  #pending = 0
  #engaged = false
  /**
   * Latched when the gesture was decided to be something else -- sideways, or
   * more than one finger. Latched for the rest of the gesture on purpose: a
   * drag that starts sideways and drifts vertical must not be hijacked
   * halfway through.
   */
  #rejected = false

  /**
   * True once this gesture is a scroll. While it is false the caller leaves the
   * event alone, which is what keeps a tap a tap.
   */
  get engaged(): boolean {
    return this.#engaged
  }

  /** Begin a gesture. notchPx is one notch of content, in CSS pixels. */
  start(x: number, y: number, notchPx: number): void {
    this.#startX = x
    this.#startY = y
    this.#lastY = y
    this.#pxPerNotch = notchPx > 0 ? notchPx : pxPerNotch(FALLBACK_ROW_HEIGHT_PX)
    this.#pending = 0
    this.#engaged = false
    this.#rejected = false
  }

  /** Abandon this gesture -- a second finger arrived, or it was cancelled. */
  reject(): void {
    this.#rejected = true
    this.#engaged = false
    this.#pending = 0
  }

  /**
   * Continue a gesture. Returns the notches to dispatch: negative is that many
   * back into the scrollback (`deltaY < 0`), positive that many forward, 0 is
   * "nothing yet".
   */
  move(x: number, y: number): number {
    if (this.#rejected) return 0

    let priming = 0
    if (this.#engaged) {
      this.#pending += y - this.#lastY
    } else {
      const dx = x - this.#startX
      const dy = y - this.#startY
      if (Math.abs(dx) >= SWIPE_THRESHOLD_PX && Math.abs(dx) >= Math.abs(dy)) {
        this.reject()
        return 0
      }
      if (Math.abs(dy) < SWIPE_THRESHOLD_PX) return 0
      this.#engaged = true
      // Counted from the start, not from here: the travel that proved this was
      // a swipe is still travel, and dropping it makes the first notch late.
      this.#pending = dy
      // And one notch right now, in the direction of travel, so the gesture
      // answers at the threshold instead of at a full notch. See the module
      // comment: this is what takes the first visible movement from 180px of
      // finger travel down to 95px. Deliberately NOT charged to #pending.
      priming = dy > 0 ? -1 : 1
    }
    this.#lastY = y
    return priming + this.#drain()
  }

  /**
   * Feed pixels straight into the accumulator, bypassing the threshold and the
   * priming notch: the gesture has already been recognised, and a glide is its
   * continuation rather than a new gesture.
   *
   * AT MOST ONE NOTCH, and the overflow is discarded rather than carried. The
   * finger's own notches are never capped -- see `move` -- and the asymmetry is
   * measured. A fast glide at a 17px notch asks for ~94 notches it cannot
   * deliver inside its own ~600ms lifetime, so carrying them would keep
   * scrolling after the gesture visibly stopped; and a tight burst is worth
   * LESS to the receiving application than the same notches spread over frames
   * (10 notches moved 16 lines back to back against 19 at 10-30ms spacing --
   * input coalescing), so the queued ones were never worth their count.
   * Discarding lands every regime in the same place without modelling anybody's
   * accelerator. The cap cannot bite a drag: one notch per frame at 17px is
   * 1018px/s of finger.
   */
  glide(px: number): number {
    if (this.#rejected) return 0
    this.#pending += px
    const notches = this.#drain()
    return notches === 0 ? 0 : Math.sign(notches)
  }

  /** Turn accumulated travel into whole notches, keeping the remainder. */
  #drain(): number {
    let notches = 0
    while (Math.abs(this.#pending) >= this.#pxPerNotch) {
      if (this.#pending > 0) {
        // The finger moved down, so older content comes down from above, which
        // is `deltaY < 0` -- @wterm's code 64.
        notches -= 1
        this.#pending -= this.#pxPerNotch
      } else {
        notches += 1
        this.#pending += this.#pxPerNotch
      }
    }
    return notches
  }

  /** End the gesture. */
  end(): void {
    this.#engaged = false
    this.#rejected = false
    this.#pending = 0
  }
}

/**
 * Wire a terminal host element up so a vertical swipe scrolls the pane.
 *
 * Returns the cleanup, in the shape `installLinkOpener` uses.
 *
 * ## Pointer events with capture, not touch events
 *
 * This was written with touch events first, and MEASURED its way off them. A
 * swipe would scroll once and then die: of ten moves the handler saw three,
 * then six, never all ten, and no touchend either. The cause is not the
 * gesture and not the recogniser:
 *
 *   | what the touchmove handler did        | moves delivered |
 *   | ------------------------------------- | --------------- |
 *   | nothing                               | 3               |
 *   | preventDefault only                   | 10              |
 *   | preventDefault + wheel on document    | 10              |
 *   | preventDefault + wheel on the terminal| 6               |
 *
 * A wheel that reaches the terminal makes tmux scroll, tmux's answer repaints
 * the grid, and the repaint REPLACES the span the finger came down on. A touch
 * event keeps going to the element the touch started on even once that element
 * is detached -- and a detached element has no ancestors, so the event stops
 * bubbling to the host where the listener lives. The handler is not called
 * again, and the gesture is over. Nothing about that is particular to the
 * driver: the same repaint happens on a phone.
 *
 * Pointer events fix it properly. `setPointerCapture` on the host retargets
 * every later event of the gesture AT THE HOST, so what happens to the node
 * underneath stops mattering. The first row of the table above is why
 * `touch-action` is set too: a listener that leaves the gesture alone does not
 * get it, because the browser takes it for panning.
 */
export function installSwipeScroll(
  host: HTMLElement,
  linesPerNotch: () => number,
): () => void {
  const scroller = new SwipeScroller()

  /** The element @wterm listens on, looked up per use: it is remounted. */
  const terminalEl = (): Element => host.querySelector('.wterm') ?? host

  const rowHeight = (): number => {
    const row = host.querySelector('.term-row')
    return row ? row.getBoundingClientRect().height : FALLBACK_ROW_HEIGHT_PX
  }

  // Notches owed to the terminal, and the frame that will pay them. Batched by
  // frame rather than dispatched inline: a flick can produce a move per
  // millisecond, and one wheel event per notch per frame is plenty.
  let owed = 0
  let frame = 0
  let lastX = 0
  let lastY = 0
  let active = -1

  // The last two samples of the gesture, for the speed it is leaving at. Two
  // rather than a smoothed window: what matters is how fast the finger was
  // going as it left, and the velocity clamp in `Glide.start` is what absorbs
  // a single freak sample.
  let lastT = 0
  let prevY = 0
  let prevT = 0

  const glide = new Glide()
  let glideFrame = 0
  let glideAt = 0

  const flush = (): void => {
    frame = 0
    const notches = owed
    owed = 0
    if (notches === 0) return
    const target = terminalEl()
    // One event per notch. @wterm reads only the SIGN of deltaY -- measured --
    // so a single event with a large delta would scroll exactly one notch.
    const deltaY = notches > 0 ? 1 : -1
    for (let i = 0; i < Math.abs(notches); i++) {
      target.dispatchEvent(
        new WheelEvent('wheel', {
          deltaY,
          deltaMode: 0,
          clientX: lastX,
          clientY: lastY,
          bubbles: true,
          cancelable: true,
        }),
      )
    }
  }

  const schedule = (): void => {
    if (frame === 0) frame = requestAnimationFrame(flush)
  }

  /** One frame of the flick that outlived the finger. */
  const stepGlide = (now: number): void => {
    glideFrame = 0
    const dt = glideAt === 0 ? GLIDE_FRAME_MS : now - glideAt
    glideAt = now
    const px = glide.step(dt)
    if (px !== 0) {
      owed += scroller.glide(px)
      flush()
    }
    if (glide.moving) glideFrame = requestAnimationFrame(stepGlide)
  }

  const stopGlide = (): void => {
    glide.stop()
    if (glideFrame !== 0) {
      cancelAnimationFrame(glideFrame)
      glideFrame = 0
    }
  }

  const release = (): void => {
    if (active !== -1 && host.hasPointerCapture(active)) host.releasePointerCapture(active)
    active = -1
    // Before `scroller.end()`, which clears the accumulator the glide feeds.
    if (scroller.engaged && prevT !== lastT) {
      const velocity = (lastY - prevY) / (lastT - prevT)
      if (glide.start(velocity)) {
        glideAt = 0
        glideFrame = requestAnimationFrame(stepGlide)
        schedule()
        return
      }
    }
    scroller.end()
    schedule()
  }

  const onDown = (event: PointerEvent): void => {
    // Only fingers. A mouse keeps every behaviour it has today, drag-selection
    // included, and a stylus is close enough to a mouse to leave alone.
    if (event.pointerType !== 'touch') return
    // A finger on the glass stops the glide, always, before anything else is
    // decided about this gesture -- including a second finger's, and including
    // a tap, which is how a user stops a long flick to read something.
    stopGlide()
    if (active !== -1) {
      // A second finger: this is a pinch, not a scroll, and it stays that way
      // for the rest of the gesture.
      scroller.reject()
      return
    }
    active = event.pointerId
    host.setPointerCapture(event.pointerId)
    // Both read HERE, at the start of the gesture, so one swipe keeps one gain
    // from beginning to end -- and a getter rather than a number because these
    // listeners are installed once, with no deps, and must not be torn down
    // mid-gesture to learn that the attached pane changed.
    scroller.start(event.clientX, event.clientY, pxPerNotch(rowHeight(), linesPerNotch()))
    lastY = event.clientY
    prevY = event.clientY
    lastT = event.timeStamp
    prevT = event.timeStamp
  }

  const onMove = (event: PointerEvent): void => {
    if (event.pointerId !== active) return
    const notches = scroller.move(event.clientX, event.clientY)
    if (!scroller.engaged) return
    // Suppresses the compatibility mouse events Chromium would otherwise make
    // from this touch, which tmux would read as a drag-selection. Without it a
    // swipe would scroll AND select.
    event.preventDefault()
    // Sampled on every move, not only the ones that produced a notch: the
    // speed the finger left at is a property of the finger, not of where the
    // notch boundaries happened to fall.
    prevY = lastY
    prevT = lastT
    lastX = event.clientX
    lastY = event.clientY
    lastT = event.timeStamp
    if (notches === 0) return
    owed += notches
    schedule()
  }

  const onUp = (event: PointerEvent): void => {
    if (event.pointerId !== active) return
    release()
  }

  // `touch-action` is what stops the browser claiming the gesture before the
  // handler ever sees it -- the three-moves row of the table above. `none`
  // rather than `pan-x pinch-zoom` because the terminal fills a phone's screen
  // and there is nothing underneath it to pan or zoom; the cost is that
  // pinch-zoom over the terminal is gone, which is a deliberate trade for
  // being able to scroll at all.
  const previousTouchAction = host.style.touchAction
  host.style.touchAction = 'none'

  host.addEventListener('pointerdown', onDown)
  host.addEventListener('pointermove', onMove)
  host.addEventListener('pointerup', onUp)
  host.addEventListener('pointercancel', onUp)
  return () => {
    stopGlide()
    host.style.touchAction = previousTouchAction
    host.removeEventListener('pointerdown', onDown)
    host.removeEventListener('pointermove', onMove)
    host.removeEventListener('pointerup', onUp)
    host.removeEventListener('pointercancel', onUp)
    if (frame !== 0) cancelAnimationFrame(frame)
  }
}
