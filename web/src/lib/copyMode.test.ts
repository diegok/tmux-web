import { describe, expect, it } from 'vitest'

import { COPY_FLIP_MS, copyControl, inCopyMode, type CopyFlip } from './copyMode'

/** The pane the control is aimed at in every case below. */
const PANE = '%3'

/** A click on that pane, `ago` milliseconds before `now`. */
function flip(copy: boolean, ago = 0, paneId: string | null = PANE): CopyFlip {
  return { paneId, copy, at: 1_000_000 - ago }
}

const NOW = 1_000_000

describe('inCopyMode', () => {
  // Measured on tmux 3.7b, and the measurement is the test: these are tmux's
  // own mode names, not an API, and nothing else in this app would fail if one
  // of them were spelled wrong here.
  it('is the copy family and nothing else', () => {
    expect(inCopyMode('copy-mode')).toBe(true)
    // `run-shell -t` leaves a pane in tmux's read-only pager. It is the same
    // command table and `cancel` pops it, which is the question this asks.
    expect(inCopyMode('view-mode')).toBe(true)
    expect(inCopyMode('')).toBe(false)
    expect(inCopyMode('tree-mode')).toBe(false)
    expect(inCopyMode('clock-mode')).toBe(false)
    expect(inCopyMode('options-mode')).toBe(false)
  })

  // The rule this whole module exists for. `pane_in_mode` is a COUNT of layers
  // and `pane_mode` names only the top one, so "the pane is in some mode" and
  // "the top layer is a copy layer" are different questions with different
  // answers -- and only the second one `send-keys -X cancel` can act on.
  it('reads the top layer, not whether the pane is in a mode at all', () => {
    // A pane scrolled up inside a choose-tree: pane_in_mode is 2 here, and the
    // top layer is the copy layer. Leaving it lands back in the tree.
    expect(inCopyMode('copy-mode')).toBe(true)
    // The tree on its own is pane_in_mode 1, and `cancel` refuses on it. A rule
    // written against the count would offer to leave a mode it cannot leave.
    expect(inCopyMode('tree-mode')).toBe(false)
  })
})

describe('copyControl', () => {
  it('offers copy mode on a pane that is not in one', () => {
    const c = copyControl('', PANE, null, NOW)
    expect(c.inCopy).toBe(false)
    expect(c.label).toBe('Copy mode')
    expect(c.action).toBe('copy-mode')
  })

  it('offers the way out of the mode it is in', () => {
    const c = copyControl('copy-mode', PANE, null, NOW)
    expect(c.inCopy).toBe(true)
    expect(c.label).toBe('Exit copy')
    expect(c.action).toBe('end-mode')
  })

  // The defect that prompted the merge: two controls, "Copy mode" and "End
  // mode", for what the user reads as one thing. Whatever else changes, the two
  // states must not share a label or an action.
  it('never gives the two states the same name or the same action', () => {
    const out = copyControl('', PANE, null, NOW)
    const back = copyControl('copy-mode', PANE, null, NOW)
    expect(out.label).not.toBe(back.label)
    expect(out.action).not.toBe(back.action)
    expect(out.title).not.toBe(back.title)
    expect(out.hint).not.toBe(back.hint)
    expect(out.search).not.toBe(back.search)
  })

  // A pane in a mode this app cannot leave still offers the way IN: entering
  // stacks a copy layer on top, which is what `cancel` then pops back off.
  it('offers copy mode on a pane that is in another kind of mode', () => {
    for (const mode of ['tree-mode', 'clock-mode', 'options-mode']) {
      expect(copyControl(mode, PANE, null, NOW).label, mode).toBe('Copy mode')
      expect(copyControl(mode, PANE, null, NOW).action, mode).toBe('copy-mode')
    }
  })

  describe('the optimistic flip', () => {
    // The poll is up to 1.5s behind, and a button that renames itself a second
    // and a half after it was pressed reads as broken.
    it('shows what the click asked for before the poll has caught up', () => {
      expect(copyControl('', PANE, flip(true), NOW).label).toBe('Exit copy')
      expect(copyControl('copy-mode', PANE, flip(false), NOW).label).toBe('Copy mode')
    })

    // The half that matters more. A command can be refused -- `cancel` on a
    // pane that left copy mode between the poll and the click -- and an
    // optimistic label that never re-syncs is worse than a slow one: it says
    // the pane is in a state it is not, for the life of the tab.
    it('reverts once the poll has had time to disagree', () => {
      const stale = flip(true, COPY_FLIP_MS)
      expect(copyControl('', PANE, stale, NOW).label).toBe('Copy mode')
      expect(copyControl('', PANE, stale, NOW).inCopy).toBe(false)
      // And the other direction: an "Exit copy" that tmux refused.
      expect(copyControl('copy-mode', PANE, flip(false, COPY_FLIP_MS), NOW).label).toBe('Exit copy')
    })

    it('holds the flip right up to the deadline and not past it', () => {
      expect(copyControl('', PANE, flip(true, COPY_FLIP_MS - 1), NOW).label).toBe('Exit copy')
      expect(copyControl('', PANE, flip(true, COPY_FLIP_MS), NOW).label).toBe('Copy mode')
    })

    // The flip belongs to the pane it was clicked on. Selecting another pane
    // while it stands must not label THAT pane with this one's mode.
    it('never follows the click onto another pane', () => {
      expect(copyControl('', '%9', flip(true), NOW).label).toBe('Copy mode')
      expect(copyControl('copy-mode', '%9', flip(false), NOW).label).toBe('Exit copy')
    })

    // A tab that has not landed on a pane yet has no mode to read and no flip
    // to hold: `null` must not match a flip recorded against `null`.
    it('needs a pane to hold a flip against', () => {
      expect(copyControl('', null, flip(true, 0, null), NOW).label).toBe('Copy mode')
    })

    it('is a window, not a lock: the poll wins once it has spoken', () => {
      // The confirming case -- both agree -- reads the same either way, which
      // is what makes the flip invisible when it works.
      expect(copyControl('copy-mode', PANE, flip(true), NOW).label).toBe('Exit copy')
      expect(copyControl('copy-mode', PANE, flip(true, COPY_FLIP_MS), NOW).label).toBe('Exit copy')
    })
  })
})
