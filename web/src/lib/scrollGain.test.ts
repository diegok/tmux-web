import { afterEach, describe, expect, it } from 'vitest'

import {
  APP_LINES_PER_NOTCH,
  DEFAULT_APP_LINES_PER_NOTCH,
  appLinesPerNotch,
  getLinesPerNotch,
  linesPerNotch,
  reportScrollTarget,
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
