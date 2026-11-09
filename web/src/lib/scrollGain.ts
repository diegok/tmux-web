/**
 * How many lines one wheel notch is worth, which depends on who receives it.
 *
 * ## Why this is not a constant
 *
 * `touchScroll.ts` turns finger travel into wheel notches, and to make the
 * content follow the finger it has to know how far one notch goes. It assumed
 * tmux's five lines. That is right only while **tmux** is the thing handling the
 * wheel, and in the panes this app exists to drive it never is.
 *
 * tmux's own `WheelUpPane` binding is the fork, and it reads three facts:
 *
 *	WheelUpPane if-shell -F "#{||:#{alternate_on},#{pane_in_mode},#{mouse_any_flag}}" \
 *	  { send-keys -M } { copy-mode -e }
 *	copy-mode   WheelUpPane select-pane \; send-keys -X -N 5 scroll-up
 *
 * So: in a mode, the copy-mode table scrolls and the `-N 5` is where five came
 * from. In no mode and on the alternate screen or with mouse tracking on, tmux
 * forwards the notch (`send -M`) and the APPLICATION scrolls, by its own
 * quantum. Measured live, every coding agent is in that third case and sets
 * both facts: `claude alt=1 any=1`, `opencode alt=1 any=1`, `pi alt=1 any=1`,
 * against a shell pane's `alt=0 any=0`.
 *
 * ## The quanta, measured
 *
 * On a tmux server of its own, with the SGR report injected as pane input
 * exactly as `send -M` forwards it, counting the rows a marker line moved:
 *
 *	tmux copy-mode   5 lines per notch   (the -N 5 above)
 *	claude           1
 *	opencode         3
 *	pi               1
 *
 * Measured with notches **60ms apart or more**, which matters because `claude`
 * accelerates: ten notches 60ms apart moved 10 lines, the same ten at 10-30ms
 * moved 19, and thirty back to back moved 127 (4.2 per notch). Nobody's
 * acceleration curve is modelled here. At 17px per notch a finger needs only
 * 283px/s to space notches 60ms apart, so in `claude` a slow drag tracks 1:1
 * and a brisk one runs at about twice the finger: two zones, by arithmetic. The
 * slow end is where 1:1 earns its keep, because that is the drag placing
 * something precisely; a quantum of 2 would buy the brisk end by making every
 * careful drag move at half the finger.
 *
 * ## Why the table is keyed on the command
 *
 * Because that is the only name on the wire: `@tmux_web_owned` is a flag, and
 * the identity of an agent is derived from `pane_current_command` on both sides
 * already (`tmux.KnownAgent`, `AGENT_MARKS`). This is not the frontend deciding
 * what counts as an agent -- the Go list still gates state, capture and the
 * logo, and this decides none of them. It is a per-program constant, and a
 * command it has never heard of gets `DEFAULT_APP_LINES_PER_NOTCH` and scrolls
 * fine. Keying it on `agentState` would be worse: that field is `""` whenever
 * the poll was taken with no browser connected, so the gain would change under
 * a pane that did not.
 *
 * ## Why `inMode` is the app's decided value
 *
 * The caller passes what it BELIEVES the mode to be, not the raw
 * `#{pane_mode}`, and `App.tsx` already holds that value because the poll is up
 * to 1.5s behind its own copy-mode button. Reading the raw field has one
 * genuinely bad case: an agent pane put into copy mode from this app's button
 * is still `alternate_on = 1`, so the raw read says "forwarded", picks a
 * quantum of 1, and scrolls **five times too far** through the one thing tmux
 * is now scrolling itself. Too large an assumed quantum feels heavy and is
 * recoverable; too small flies past what the reader was looking at and is not.
 *
 * ## Why a module-level store and not a prop
 *
 * The producer is `App`, which holds the snapshot and the attached pane; the
 * consumer is a DOM listener installed once, with `[]` deps, inside
 * `Terminal`'s effect -- it must not be torn down mid-gesture to learn a new
 * number. So the value is read through a getter at the start of each gesture,
 * which also means a gesture keeps one gain from beginning to end. It is a
 * number, not state React renders, so unlike `terminalSettings.ts` there is
 * nothing to subscribe to and no snapshot identity to keep stable.
 */

import { TMUX_LINES_PER_NOTCH } from './touchScroll'

/**
 * What an application nobody measured is assumed to scroll per notch.
 *
 * Three because it is both the conventional wheel delta and the safe side of
 * the error: the content moves at `real / assumed` times the finger, so
 * assuming too large a quantum makes an unknown TUI feel heavy -- recoverable
 * -- and assuming too small flies past what the user was reading.
 */
export const DEFAULT_APP_LINES_PER_NOTCH = 3

/**
 * Lines per notch for the programs that were measured, keyed on
 * `pane_current_command`.
 *
 * `claude` is 1 knowing its accelerator takes a brisk drag to about twice the
 * finger; see the header. It is a one-line change if a thumb says otherwise.
 */
export const APP_LINES_PER_NOTCH: Readonly<Record<string, number>> = {
  claude: 1,
  pi: 1,
  opencode: 3,
}

/** Lines per notch for a program, measured or assumed. */
export function appLinesPerNotch(command: string): number {
  return APP_LINES_PER_NOTCH[command] ?? DEFAULT_APP_LINES_PER_NOTCH
}

/** What the gesture needs to know about the pane it is scrolling. */
export interface ScrollTarget {
  /** `pane_current_command`, which is what the quantum table is keyed on. */
  command: string
  /**
   * Whether the pane is in a tmux mode -- as the APP has decided, not as the
   * raw field says. See the header: this is the half where being wrong in one
   * direction scrolls five times too far.
   */
  inMode: boolean
  /** `#{alternate_on}`. */
  alternateOn: boolean
  /** `#{mouse_any_flag}`. */
  mouseAnyFlag: boolean
}

/**
 * tmux's own predicate, restated: five lines unless the pane is in no mode
 * **and** (on the alternate screen **or** tracking the mouse), in which case
 * the application receives the notch and its own quantum applies.
 *
 * `null` -- no pane attached, or one that has just died -- is tmux's five.
 */
export function linesPerNotch(target: ScrollTarget | null): number {
  if (target === null) return TMUX_LINES_PER_NOTCH
  if (target.inMode) return TMUX_LINES_PER_NOTCH
  if (target.alternateOn || target.mouseAnyFlag) return appLinesPerNotch(target.command)
  return TMUX_LINES_PER_NOTCH
}

/**
 * The pane fields this module needs, which is a subset of `PaneNode`.
 *
 * Structural rather than imported so that the gain does not depend on the
 * snapshot's types -- and so this function can be tested without a tree.
 */
export interface ScrollTargetPane {
  command: string
  /** The raw `#{pane_mode}` off the poll: "" for a pane in no mode at all. */
  paneMode: string
  alternateOn: boolean
  mouseAnyFlag: boolean
}

/**
 * Turn the attached pane and the app's own copy-mode answer into a target.
 *
 * The `inMode` half is BOTH of its sources, and neither is redundant:
 *
 *   - `paneMode !== ""` is every tmux mode, not just the copy ones. A
 *     `tree-mode` or a `clock-mode` on top of an agent is tmux's to scroll too,
 *     and `inCopyMode` is a closed set of `copy-mode` and `view-mode`, so the
 *     decided value is false for both of those while `alternate_on` is still 1.
 *     Without this clause such a pane would take the application's quantum and
 *     scroll FIVE TIMES TOO FAR -- the unrecoverable direction.
 *   - `inCopyMode` is what this app has decided, which the field cannot know
 *     yet: the poll is up to 1.5s behind the header's own copy-mode button, and
 *     an agent pane put into copy mode from that button is still
 *     `alternate_on = 1`.
 *
 * A lifted function rather than four lines inside the component because a rule
 * written inside a component in this app is a rule no test in this suite can
 * reach -- the reason `copyMode.ts` exists at all.
 */
export function scrollTargetFor(
  pane: ScrollTargetPane | null,
  inCopyMode: boolean,
): ScrollTarget | null {
  if (pane === null) return null
  return {
    command: pane.command,
    inMode: pane.paneMode !== '' || inCopyMode,
    alternateOn: pane.alternateOn,
    mouseAnyFlag: pane.mouseAnyFlag,
  }
}

// --- the store ---------------------------------------------------------------

let reported: ScrollTarget | null = null

/**
 * Report the pane the gesture will scroll: the ACTIVE pane, which is not
 * necessarily the one under the finger.
 *
 * The snapshot carries no pane geometry and tmux routes a wheel report by its
 * coordinates, so in a split window a swipe over the inactive half uses the
 * active half's quantum. Accepted rather than fixed: a tap selects a pane,
 * which is how one starts working with it anyway.
 */
export function reportScrollTarget(target: ScrollTarget | null): void {
  reported = target
}

/**
 * The gain for the reported pane, read at the start of each gesture.
 *
 * Up to one poll interval stale, like everything else off the snapshot: a pane
 * that has just changed regime gives the previous gain for one gesture and the
 * next poll corrects it.
 */
export function getLinesPerNotch(): number {
  return linesPerNotch(reported)
}

/** Forget the reported pane. Tests only: the store is module-level. */
export function resetScrollGain(): void {
  reported = null
}
