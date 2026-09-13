/**
 * The one rule in `App` that is not wiring: when a cross-session click stops
 * driving the highlight.
 *
 * Everything else this component does needs a renderer with effects -- a
 * `TerminalHandle` ref, a socket that becomes ready, a snapshot poll -- and
 * this suite is `react-dom/server` in node by design, so it lives in
 * `e2e/deadpane.spec.ts` instead. What is testable here is the decision itself,
 * and it is worth testing here because the two ways of getting it wrong are
 * both invisible on screen at the moment they happen: holding the override
 * forever pins `activePane` to a pane id for the life of the tab, and dropping
 * it too early un-acknowledges a click the user just made.
 */

import { describe, expect, it } from 'vitest'

import { focusAfterSelect, pendingStep } from './App'
import type { TerminalPhase, TerminalStatus } from '@/components/Terminal'

/** A terminal status, defaulting to a live socket that reports no pane. */
function status(over: Partial<TerminalStatus> = {}): TerminalStatus {
  return { phase: 'ready', attempt: 0, retryDelayMs: 0, pane: null, inputDropped: false, ...over }
}

describe('pendingStep', () => {
  it('holds the override until the replacement socket is ready', () => {
    // The window this state exists for: the old socket is gone, the new one is
    // not up, and the click has to look answered in the meantime.
    expect(pendingStep('%7', null)).toBe('hold')
    expect(pendingStep('%7', status({ phase: 'connecting' }))).toBe('hold')
    expect(pendingStep('%7', status({ phase: 'reconnecting' }))).toBe('hold')
  })

  it('holds it on a socket that is not ready even when it already names the pane', () => {
    // A tab that had been on this pane before remembers it per session, so the
    // new `TerminalSession` reports it before the socket is live. That is not
    // the confirmation this waits for: nothing has been selected yet.
    expect(pendingStep('%7', status({ phase: 'connecting', pane: '%7' }))).toBe('hold')
  })

  it('replays the selection on a ready socket that is on another pane', () => {
    expect(pendingStep('%7', status({ pane: null }))).toBe('replay')
    expect(pendingStep('%7', status({ pane: '%3' }))).toBe('replay')
  })

  it('is landed once the ready socket reports the pane', () => {
    expect(pendingStep('%7', status({ pane: '%7' }))).toBe('landed')
  })

  it('lands only where dropping the override cannot change what is on screen', () => {
    // `activePane` is `pendingPane ?? status.pane`, so "landed" is the one
    // answer that lets the caller drop the override without the highlight or
    // the breadcrumb moving. Any other reading of the same statuses would.
    const statuses: TerminalStatus[] = [
      status({ pane: '%7' }),
      status({ pane: '%3' }),
      status({ pane: null }),
      status({ phase: 'connecting', pane: '%7' }),
      status({ phase: 'ended', pane: '%7' }),
    ]
    for (const s of statuses) {
      if (pendingStep('%7', s) !== 'landed') continue
      expect(s.pane, 'the override was dropped onto a different pane').toBe('%7')
    }
    // And it does say landed for at least one of them, so the loop above is
    // not vacuously true.
    expect(statuses.map((s) => pendingStep('%7', s))).toContain('landed')
  })

  it('never lands on a socket that is not live, whatever it reports', () => {
    const dead: TerminalPhase[] = ['connecting', 'reconnecting', 'ended', 'gone', 'closed']
    for (const phase of dead) {
      expect(pendingStep('%7', status({ phase, pane: '%7' })), phase).toBe('hold')
    }
  })
})

describe('focusAfterSelect', () => {
  it('hands the terminal the keyboard by default', () => {
    // The sidebar row and the palette. Both take focus themselves -- a sidebar
    // button keeps it, a closing palette leaves it on <body> -- so without this
    // you land on a pane and cannot type into it. Pinned from this side as
    // well as from the panel's, because a default that quietly became "only
    // when asked" is a worse regression than the bug the option exists for.
    expect(focusAfterSelect()).toBe(true)
    expect(focusAfterSelect(undefined)).toBe(true)
    expect(focusAfterSelect({})).toBe(true)
    expect(focusAfterSelect({ focus: true })).toBe(true)
  })

  it('declines only when a caller says so', () => {
    // The capture panel, and nothing else today: it is a modal, the focus
    // scope owns focus while it is open, and on a phone taking it would raise
    // the soft keyboard the panel exists to avoid.
    expect(focusAfterSelect({ focus: false })).toBe(false)
  })
})
