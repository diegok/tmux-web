import { afterEach, describe, expect, it } from 'vitest'

import {
  APP_LINES_PER_NOTCH,
  DEFAULT_APP_LINES_PER_NOTCH,
  appLinesPerNotch,
  getLinesPerNotch,
  linesPerNotch,
  reportScrollTarget,
  scrollTargetFor,
  resetScrollGain,
} from './scrollGain'
import type { ScrollTarget } from './scrollGain'
import { TMUX_LINES_PER_NOTCH } from './touchScroll'

describe('appLinesPerNotch', () => {
  it('is what each program was measured to scroll', () => {
    expect(appLinesPerNotch('claude')).toBe(1)
    expect(appLinesPerNotch('pi')).toBe(1)
    expect(appLinesPerNotch('opencode')).toBe(3)
  })

  it('assumes the conventional three for anything it has not measured', () => {
    // Heavy is recoverable, lost is not: too large a quantum makes an unknown
    // TUI feel heavy, too small flies past what the user was reading.
    expect(appLinesPerNotch('vim')).toBe(DEFAULT_APP_LINES_PER_NOTCH)
    expect(appLinesPerNotch('')).toBe(DEFAULT_APP_LINES_PER_NOTCH)
  })

  it('is keyed on the command and nothing else', () => {
    // The whole table, so a key added in the wrong shape -- a path, a pretty
    // name, an agent id -- fails here rather than silently never matching
    // `pane_current_command`.
    expect(Object.keys(APP_LINES_PER_NOTCH).sort()).toEqual(['claude', 'opencode', 'pi'])
  })
})

describe('linesPerNotch', () => {
  const agent: ScrollTarget = {
    command: 'claude',
    inMode: false,
    alternateOn: true,
    mouseAnyFlag: true,
  }

  it("is tmux's five with no pane to go on", () => {
    expect(linesPerNotch(null)).toBe(TMUX_LINES_PER_NOTCH)
  })

  it("is tmux's five for a plain shell pane", () => {
    expect(
      linesPerNotch({ command: 'zsh', inMode: false, alternateOn: false, mouseAnyFlag: false }),
    ).toBe(TMUX_LINES_PER_NOTCH)
  })

  it("is the application's quantum on the alternate screen", () => {
    expect(linesPerNotch(agent)).toBe(1)
    // Either fact alone is enough: tmux's own predicate is an OR, and an agent
    // that turned tracking off is still on the alternate screen, where tmux
    // has no scrollback to show.
    expect(linesPerNotch({ ...agent, mouseAnyFlag: false })).toBe(1)
    expect(linesPerNotch({ ...agent, alternateOn: false })).toBe(1)
    expect(linesPerNotch({ ...agent, command: 'opencode' })).toBe(3)
  })

  it("is tmux's five whenever the pane is in a mode, alternate screen or not", () => {
    // The unrecoverable direction: in a mode tmux handles the wheel itself, so
    // reading the application's quantum here would scroll five times too far
    // through the one thing tmux is scrolling. It covers both the mode the poll
    // reported -- including a tree-mode on top of an agent -- and the copy mode
    // this app's own button has just asked for and not yet seen confirmed.
    expect(linesPerNotch({ ...agent, inMode: true })).toBe(TMUX_LINES_PER_NOTCH)
  })
})

describe('scrollTargetFor', () => {
  const agentPane = {
    command: 'claude',
    paneMode: '',
    alternateOn: true,
    mouseAnyFlag: true,
  }

  it('has nothing to report for no pane', () => {
    expect(scrollTargetFor(null, false)).toBeNull()
  })

  it('reads a pane in no mode as the application receiving', () => {
    expect(linesPerNotch(scrollTargetFor(agentPane, false))).toBe(1)
  })

  // THE UNRECOVERABLE DIRECTION, and the reason this mapping is a function
  // rather than four lines inside a component. `inCopyMode` is a closed set of
  // copy-mode and view-mode, so the app's own decided value is FALSE for a
  // tree-mode or a clock-mode -- and a tree-mode on top of an agent pane still
  // reports alternate_on = 1. Reading only the decided value would call that
  // pane forwarded, take the application's quantum of 1, and scroll five times
  // too far through the one thing tmux is scrolling itself.
  it.each(['copy-mode', 'view-mode', 'tree-mode', 'clock-mode', 'options-mode'])(
    'reads an agent pane in %s as tmux receiving',
    (paneMode) => {
      expect(linesPerNotch(scrollTargetFor({ ...agentPane, paneMode }, false))).toBe(
        TMUX_LINES_PER_NOTCH,
      )
    },
  )

  // The other half: the poll is up to 1.5s behind this app's own copy-mode
  // button, so the click's answer has to count before the field carries it.
  it('reads a pane the app has just put into copy mode as tmux receiving', () => {
    expect(linesPerNotch(scrollTargetFor(agentPane, true))).toBe(TMUX_LINES_PER_NOTCH)
  })
})

describe('the store', () => {
  afterEach(() => resetScrollGain())

  it("is tmux's five until a pane is reported, and again once it is not", () => {
    expect(getLinesPerNotch()).toBe(TMUX_LINES_PER_NOTCH)
    reportScrollTarget({
      command: 'opencode',
      inMode: false,
      alternateOn: true,
      mouseAnyFlag: true,
    })
    expect(getLinesPerNotch()).toBe(3)
    // A pane that died, or a tab that has not landed on one: back to the
    // quantum that is right for whatever tmux is showing.
    reportScrollTarget(null)
    expect(getLinesPerNotch()).toBe(TMUX_LINES_PER_NOTCH)
  })
})
