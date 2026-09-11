// The pi extension's three filters, driven with a fake `report`.
//
// The extension has no logic beyond those filters -- no state names, no
// mapping, no argv, no text -- so this file is short on purpose. What it is
// NOT is a "registers five handlers" mock test: every case below is one of the
// mutants in Task 17's table, and the Go wiring test that would otherwise be
// their only cover is allowed to skip on a machine without pi installed.
// Driven here, they are covered on every run.

import { readFileSync } from 'node:fs'

import { describe, expect, it, vi } from 'vitest'

import piExtension, { handlers } from './pi.ts'
import { spawnReport } from './queue.ts'

// The real queue module, with spies over it: the default export is the only
// place the agent NAME appears, and `spawnReport('opencode')` in this file is
// a mutant the filter tests cannot see -- it changes no item and no handler,
// only the argv of a process none of them spawns. Without this, its only cover
// is the Go wiring test, which is allowed to skip on a machine without pi.
vi.mock('./queue.ts', { spy: true })

type Item = { event: string; payload?: unknown }

// A recorder standing in for the queue's push. It is the whole world the
// handlers can reach: nothing here spawns a process or touches tmux.
function recorder() {
  const items: Item[] = []
  return { items, report: (item: Item) => void items.push(item) }
}

// The contexts pi really hands over, measured on pi 0.85.1 rather than
// guessed: `ctx.mode` is "tui" in the TUI and "print" in an async subagent's
// own process, and `isIdle()` is TRUE in both at session_start and at
// agent_settled. See the comment in pi.ts.
const tui = (idle = true) => ({ mode: 'tui', hasUI: true, isIdle: () => idle })
const rpc = (idle = true) => ({ mode: 'rpc', hasUI: true, isIdle: () => idle })

// One recorded payload, read from the same fixtures Go's table is tested
// against. The point of reading the file rather than retyping a shape is that
// this extension must hand the event over WHOLE and unreduced: the basename
// rule, the command-whole rule and the 1 KiB cap all live in `wterm-web
// report`, and a reduction applied here would be a second copy of them.
function fixture(name: string): unknown {
  return JSON.parse(
    readFileSync(new URL(`../../cmd/wterm-web/testdata/hooks/pi/${name}.json`, import.meta.url), 'utf8'),
  )
}

describe('handlers', () => {
  it('reports nothing at all from a session that is not the TUI', () => {
    // The fails-closed gate and the `root` flag in one fixture. An async pi
    // subagent is a SEPARATE OS PROCESS that loads this same extension,
    // inherits TMUX_PANE, and emits an agent_settled byte-identical to the
    // root's 7.0s before the root's real one -- with ctx.isIdle() true. The
    // mode is the only thing that separates them, so this case is the one
    // standing between a subagent's finish and a false `done` badge on the
    // owner's pane.
    const r = recorder()
    const h = handlers(r.report)
    h.session_start({ type: 'session_start', reason: 'startup' }, rpc())
    expect(r.items).toEqual([])
    // And every later event of that session stays silent too: the gate is not
    // only about session_start's own write, it is what arms everything else.
    h.input({ type: 'input', text: 'write a haiku' }, rpc())
    h.tool_execution_start(fixture('tool_execution_start_bash'), rpc())
    h.ui_prompt_start(fixture('ui_prompt_start_input'), rpc())
    h.agent_settled({ type: 'agent_settled' }, rpc())
    expect(r.items).toEqual([])
  })

  it('reports the TUI session, and the turn start that follows it', () => {
    const r = recorder()
    const h = handlers(r.report)
    h.session_start({ type: 'session_start', reason: 'startup' }, tui(true))
    // `wterm_is_idle` is the contract with cmd/wterm-web/events.go, which
    // discriminates pi's session_start on exactly this key -- ctx is not part
    // of pi's event object and no recorded payload carries it, so the
    // extension is the only thing that can put it there. A different key name
    // is not a smaller bug: the Go side reads a missing key as `working`, so
    // the idle branch would simply never be taken and a reload on a resting
    // pane would keep writing a transient state.
    expect(r.items).toEqual([{ event: 'session_start', payload: { wterm_is_idle: true } }])
    h.input({ type: 'input', text: 'write a haiku', source: 'interactive' }, tui(true))
    // The turn-start invariant. pi's turn end is an EDGE -- it writes idle
    // without reading what is standing -- and that is only safe because this
    // write put a non-resting state in front of it.
    expect(r.items[1]).toEqual({ event: 'input' })
  })

  it('reports the working session_start as a working one', () => {
    // The other branch of the only event on any agent that is an edge on one
    // side and a re-assertion on the other. A reload mid-turn must not tell
    // the daemon the agent is resting.
    const r = recorder()
    const h = handlers(r.report)
    h.session_start({ type: 'session_start', reason: 'startup' }, tui(false))
    expect(r.items).toEqual([{ event: 'session_start', payload: { wterm_is_idle: false } }])
  })

  it('hands the tool and prompt events over whole', () => {
    const r = recorder()
    const h = handlers(r.report)
    h.session_start({ type: 'session_start', reason: 'startup' }, tui(true))
    h.tool_execution_start(fixture('tool_execution_start_bash'), tui(false))
    h.ui_prompt_start(fixture('ui_prompt_start_input'), tui(false))
    expect(r.items.slice(1)).toEqual([
      { event: 'tool_execution_start', payload: fixture('tool_execution_start_bash') },
      { event: 'ui_prompt_start', payload: fixture('ui_prompt_start_input') },
    ])
  })

  it('does not report a settle it cannot confirm', () => {
    const r = recorder()
    const h = handlers(r.report)
    h.session_start({ type: 'session_start', reason: 'startup' }, tui(true))
    r.items.length = 0

    // Mid-turn: pi settles the ROOT's agent_settled only when it is idle, and
    // an agent that is still working must not be reported as finished.
    h.agent_settled({ type: 'agent_settled' }, tui(false))
    expect(r.items).toEqual([])
    // A ctx with no isIdle at all -- a pi that changed its context shape --
    // fails closed here as well, because `idle` never expires once written.
    h.agent_settled({ type: 'agent_settled' }, { mode: 'tui', hasUI: true } as never)
    expect(r.items).toEqual([])

    h.agent_settled({ type: 'agent_settled' }, tui(true))
    expect(r.items).toEqual([{ event: 'agent_settled' }])
  })

  it('records nothing but an event name and the hook’s own payload', () => {
    // The file's one-line contract with Go, and the assertion that catches
    // somebody "helpfully" adding a state name, a timestamp or a --text here.
    // Every reduction and the whole sanitizer live in `wterm-web report`; an
    // item with a third key is an integration that has started deciding
    // things.
    const r = recorder()
    const h = handlers(r.report)
    h.session_start({ type: 'session_start', reason: 'startup' }, tui(true))
    h.input({ type: 'input', text: 'write a haiku' }, tui(true))
    h.tool_execution_start(fixture('tool_execution_start_write'), tui(false))
    h.ui_prompt_start(fixture('ui_prompt_start_custom'), tui(false))
    h.agent_settled({ type: 'agent_settled' }, tui(true))

    expect(r.items.map((i) => i.event)).toEqual([
      'session_start',
      'input',
      'tool_execution_start',
      'ui_prompt_start',
      'agent_settled',
    ])
    for (const item of r.items) {
      expect(Object.keys(item).sort().join(',')).toMatch(/^event(,payload)?$/)
      expect(typeof item.event).toBe('string')
    }
  })

  it('never returns anything a pi handler could await', () => {
    // pi awaits handlers with NO timeout -- a measured 3s stall in `input`
    // delayed the turn by 3.03s -- so nothing here may be a promise, and that
    // includes the accidental promise an `async` handler returns.
    const r = recorder()
    const h = handlers(r.report)
    for (const [name, fn] of Object.entries(handlers(r.report))) {
      expect(fn.constructor.name, `${name} is an async function`).toBe('Function')
    }
    const returned = h.session_start({ type: 'session_start' }, tui(true))
    expect(returned).toBeUndefined()
    expect(h.input({ type: 'input' }, tui(true))).not.toBeInstanceOf(Promise)
  })
})

describe('the default export', () => {
  it('registers exactly the five events pi is asked about', () => {
    // The names are written out rather than taken from handlers() on purpose:
    // against `Object.keys(handlers(...))` a dropped handler would move both
    // sides of the comparison at once and survive. These five are the contract
    // with cmd/wterm-web/events.go's pi table.
    const registered: string[] = []
    piExtension({
      on(name: string) {
        registered.push(name)
      },
    })
    expect(registered.sort()).toEqual(
      ['agent_settled', 'input', 'session_start', 'tool_execution_start', 'ui_prompt_start'].sort(),
    )
  })

  it('reports as pi, through a queue of its own', () => {
    // `pi` is what cmd/wterm-web/events.go keys its table on and what the
    // daemon derives from pane_current_command; the wrong name here is a
    // silent no-op on every event, because an agent the table does not know
    // reports nothing.
    vi.mocked(spawnReport).mockClear()
    piExtension({ on() {} })
    expect(spawnReport).toHaveBeenCalledTimes(1)
    expect(spawnReport).toHaveBeenCalledWith('pi')
  })
})
