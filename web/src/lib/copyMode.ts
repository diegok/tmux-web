/**
 * The one copy-mode control: what it says, what it does, and what it shows in
 * the moment between the click and the poll that confirms it.
 *
 * The header used to carry two buttons, "Copy mode" and "End mode", and the
 * owner did not know what the second one meant -- which is the real defect:
 * one action must not have two names, and two buttons for one idea make the
 * user work out which of them applies. There is one control now, and its label
 * follows the pane's actual mode: **Copy mode** on a pane that is not in one,
 * **Exit copy** on a pane that is. The palette shows whichever row applies, for
 * the same reason.
 *
 * The rules live here, outside the components, because `renderToStaticMarkup`
 * fires no handlers: a rule written inside an `onClick` is a rule no test in
 * this suite can reach.
 */

/**
 * The modes `send-keys -X cancel` can pop, and therefore the ones this app can
 * offer to leave.
 *
 * Measured on tmux 3.7b, pane by pane, rather than assumed. `copy-mode` is the
 * scrollback; `view-mode` is the read-only pager a `run-shell -t` leaves
 * behind, which is the same command table and cancels the same way. Everything
 * else tmux can put a pane in -- `tree-mode`, `clock-mode`, `options-mode` --
 * refuses that command with "not in a mode", exit 1, unchanged.
 *
 * A set rather than a substring test: "copy-mode" is a prefix of nothing, but a
 * rule written as `mode.includes('copy')` would be one tmux mode name away from
 * silently widening.
 */
const COPY_MODES: readonly string[] = ['copy-mode', 'view-mode']

/**
 * Whether the TOP layer of a pane's mode stack is a copy layer.
 *
 * This is the whole subtlety of the feature. tmux modes **stack**:
 * `#{pane_in_mode}` is a count of layers and `#{pane_mode}` names only the top
 * one. A pane scrolled up inside a choose-tree reports `copy-mode` at depth 2,
 * and a pane sitting in the choose-tree itself reports `tree-mode` at depth 1
 * while being every bit as "in a mode".
 *
 * So "is this pane in a mode" is the wrong question, and a control built on it
 * would offer to leave a mode that `cancel` cannot leave -- the button would do
 * nothing, twice, and then go on saying the pane was in copy mode. What a click
 * can act on is the top layer, which is exactly what this reads.
 *
 * `""` -- no mode at all, which is where most panes are -- is false.
 */
export function inCopyMode(paneMode: string): boolean {
  return COPY_MODES.includes(paneMode)
}

/**
 * How long the browser trusts its own click over the snapshot.
 *
 * The label comes from a poll the daemon takes every `POLL_INTERVAL_MS`, so a
 * button that waited for it would rename itself up to a second and a half after
 * it was pressed -- which reads as a control that did nothing and then did
 * something on its own. It flips on click instead and this is how long that
 * flip stands.
 *
 * Two intervals, not one: the answer has to survive the daemon's poll AND the
 * tab's, and the two are not in step. In practice it is far shorter than that
 * -- the daemon forces a poll as soon as the command applies (see `settle` in
 * ws.go) and the tab re-reads the snapshot on the same click -- so this is the
 * deadline for the case those miss, not the normal wait.
 *
 * It is a deadline rather than "hold until the pane's mode changes" because the
 * command can be refused: `cancel` on a pane that left copy mode between the
 * poll and the click changes nothing, so a flip waiting for a change would wait
 * for the life of the tab.
 */
export const COPY_FLIP_MS = 3000

/** A click on the control, held until the poll catches up with it. */
export interface CopyFlip {
  /**
   * The pane it was aimed at. A flip never follows the user onto another pane:
   * selecting a second pane while one stands must not label that pane with
   * this one's mode.
   */
  paneId: string | null
  /** What the click asked for: `true` for "now in copy mode". */
  copy: boolean
  /** `Date.now()` when it was clicked. */
  at: number
}

/** What the header button and the palette row should render right now. */
export interface CopyControl {
  /** Whether the control is showing the pane as being in copy mode. */
  inCopy: boolean
  /** The button's text, and the palette row's. */
  label: string
  /** The button's `title`: the tooltip, in a full sentence. */
  title: string
  /** The palette row's trailing hint -- a few words, not a sentence. */
  hint: string
  /** What cmdk fuzzy-matches for the row this describes. */
  search: string
  /**
   * Which control message a click sends. Both are exactly the messages the
   * daemon already spoke -- merging the two buttons changed the UI, not the
   * protocol.
   */
  action: 'copy-mode' | 'end-mode'
}

const ENTER: CopyControl = {
  inCopy: false,
  label: 'Copy mode',
  title: "Enter tmux copy mode, where this app's scrollback lives",
  hint: 'scrollback',
  search: 'copy mode scrollback search',
  action: 'copy-mode',
}

const LEAVE: CopyControl = {
  inCopy: true,
  label: 'Exit copy',
  title: 'Leave copy mode, so typing reaches the pane again',
  hint: 'typing reaches the pane',
  search: 'exit copy mode leave end scrollback typing',
  action: 'end-mode',
}

/**
 * The control for a pane, given what the last poll said and what the user did
 * since.
 *
 * There are only two answers, and they are returned by reference, so a control
 * that has not changed keeps its identity from render to render. Neither may be
 * mutated by a caller.
 *
 * `paneMode` is the snapshot's `#{pane_mode}` for `pane`, `flip` is the click
 * being waited on (or null), and `now` is `Date.now()` -- passed in so the rule
 * is a function of its arguments and a test needs no clock.
 *
 * The flip wins only while it is fresh AND aimed at this pane. Once it expires
 * the poll's answer wins, whatever it says: that is what makes a refused
 * command correct itself instead of leaving the button lying about the pane.
 */
export function copyControl(
  paneMode: string,
  pane: string | null,
  flip: CopyFlip | null,
  now: number,
): CopyControl {
  let inCopy = inCopyMode(paneMode)
  // `pane === null` never matches, even against a flip recorded with a null
  // pane: a tab that has not landed on a pane has nothing to have clicked on.
  if (flip && pane !== null && flip.paneId === pane && now - flip.at < COPY_FLIP_MS) {
    inCopy = flip.copy
  }
  return inCopy ? LEAVE : ENTER
}
