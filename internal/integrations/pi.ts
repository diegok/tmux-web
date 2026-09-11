// managed by tmux-web (tmux-web-schema: 1)
// Reinstalling or updating the integration overwrites this file.
// It does one thing: `tmux set-option -p @tmux_web_agent`. Nothing else.
//
// WHAT IS DELIBERATELY NOT HERE. No state name, no mapping from an event to a
// state, no activity text, no sanitizer, no command line. All of that is one
// table in `tmux-web report` (Go), shared by the three integrations, so that
// it exists once instead of drifting across a TypeScript extension, a
// JavaScript plugin and a settings.json the user owns. If you find yourself
// writing the word "working" or "idle" in this file, something has gone wrong.
//
// What is left is three runtime filters that only the runtime can apply,
// because they read `ctx`, which is not part of any event payload and
// therefore cannot reach Go at all unless this file puts it there.

import { claimReporter, makeQueue, spawnReport } from './queue.ts'

/** Exactly what `tmux-web report` needs: an event name and the hook's own
 *  JSON. No timestamp -- `report` stamps one from its own process start. */
type ReportItem = { event: string; payload?: unknown }

/** pi's second handler argument. Only the two members this file reads are
 *  named; the real object carries eighteen. */
type Ctx = { mode?: string; isIdle?: () => boolean }

/** A pi event handler. It must never return a promise: see `handlers`. */
type Handler = (event: unknown, ctx?: Ctx) => void

/** The object pi hands the default export. */
type Pi = { on: (name: string, handler: Handler) => unknown }

// handlers is exported, and that export is what makes this file testable at
// all. Its three filters -- the mode gate, the root flag and agent_settled's
// isIdle check -- are the only logic in the integration, and the Go wiring
// test that would otherwise be their only cover is allowed to t.Skip when pi
// is not installed. Driven from pi.test.ts with a fake `report`, they are
// covered on every run.
//
// NOTHING HERE IS ASYNC AND NOTHING RETURNS A VALUE. pi awaits handlers with
// no timeout whatsoever: a measured 3s stall in `input` delayed the turn by
// 3.03s, and 4s in a tool hook delayed the tool result by 4.00s. The queue's
// push() returns undefined for the same reason, so there is nothing for a
// handler to await even by accident.
export function handlers(report: (item: ReportItem) => void): Record<string, Handler> {
  // Whether this process is the pane's root pi. Set by the one filter that can
  // decide it, and never cleared: an extension reload builds a new closure
  // anyway, and it gets a new session_start with it.
  let root = false

  return {
    session_start: (_event, ctx) => {
      // TUI only, and this gate is MEASURED, not a precaution copied from
      // another integration. An async pi subagent (`async: true`) is a
      // separate OS process: it discovers and loads this same extension,
      // inherits TMUX_PANE=%0 unchanged, and emits its own full event stream
      // ending in an agent_settled whose payload -- {"type":"agent_settled"}
      // and nothing else -- is BYTE-IDENTICAL to the root's, 7.0s before the
      // root's real one. ctx.isIdle() is true in it as well, at both its
      // session_start and its agent_settled. Nothing in the payload separates
      // them. The one thing that does is that the subagent process reports
      // mode "print" (hasUI false) where the root reports "tui"; confirmed
      // again on pi 0.85.1 while this file was written, in both `pi -p` and a
      // real TUI. Relax this gate and a subagent's finish becomes the pane's
      // finish, which is a false `done` badge that never expires.
      //
      // RPC and JSON modes are headless too, and RPC reports hasUI true, so
      // `mode` is the reliable gate and `hasUI` is not. This is the ONE filter
      // of the three that fails CLOSED: a missing or unexpected mode reports
      // nothing.
      if (ctx?.mode !== 'tui') return
      root = true
      // A reload can replace this extension mid-run without another
      // agent_start, so an extension that only ever sets state on transitions
      // comes back from a reload believing nothing is happening.
      //
      // The idle branch is a RE-ASSERTION, not an edge -- a reload can recur
      // arbitrarily often inside one resting period, so as an edge it would
      // write idle;<now> and re-badge every device on every reload. `report`
      // knows that from its own table; the extension just names the event.
      //
      // `tmux_web_is_idle` is the key cmd/tmux-web/events.go discriminates on,
      // and the namespace is because the key is ours and not pi's. ctx is not
      // part of pi's event object and no recorded payload carries it, so this
      // is the only place it can come from. The Go side reads anything absent
      // or wrong-typed as "working" -- deliberately the opposite fail
      // direction from the other discriminators, because both branches are
      // known and only one of them rests forever -- so `=== true` here keeps
      // the value a boolean whatever ctx does.
      report({ event: 'session_start', payload: { tmux_web_is_idle: ctx?.isIdle?.() === true } })
    },

    // The turn-start invariant. pi's turn end is an edge: it writes a resting
    // state without reading what is standing, which is safe only because this
    // write put a non-resting one in front of it. Without this handler,
    // agent_settled's write is suppressed and that turn loses its badge.
    //
    // The prompt text is NOT sent. This event's mapping is rung 4 of the
    // activity ladder -- a state-only report -- and the text it could send is
    // the user's raw prompt.
    input: () => {
      if (root) report({ event: 'input' })
    },

    // The event and nothing but the event: which of its keys mean anything,
    // and how a path or a command is reduced to one line, is activity.go's
    // decision and it is made against the recorded fixtures.
    tool_execution_start: (event) => {
      if (root) report({ event: 'tool_execution_start', payload: event })
    },

    ui_prompt_start: (event) => {
      if (root) report({ event: 'ui_prompt_start', payload: event })
    },

    // The turn end. `isIdle()` is what separates the root's own settle from a
    // settle it is still working behind -- it does NOT separate the root from
    // an async subagent, which reports idle just as truthfully in its own
    // process; the mode gate above is what does that.
    agent_settled: (_event, ctx) => {
      if (root && ctx?.isIdle?.() === true) report({ event: 'agent_settled' })
    },
  }
}

// What pi loads. It owns two things the factory does not: the real queue, which
// holds one report in flight per pane and collapses a burst of tool calls to
// the newest state rather than to a backlog of forks, and the claim that
// decides whether this copy is the one that should be reporting at all.
//
// THE CLAIM IS MADE HERE AND CHECKED AT THE QUEUE, and those are deliberately
// two moments. Since `--global`, one pi process can hold two copies of this
// file -- $PI_CODING_AGENT_DIR/extensions/ auto-loads in every project and
// .pi/extensions/ loads in this one, and pi dedupes neither -- so both copies
// register all five handlers and both are called on every event. A copy that
// loses the claim cannot unregister anything (pi's `on` has no inverse), so it
// stays registered and says nothing. See queue.ts for why the winner is the
// newer schema rather than the first to load.
export default function (pi: Pi) {
  const mine = claimReporter()
  const q = makeQueue(spawnReport('pi'))
  const report = (item: ReportItem) => {
    if (mine()) q.push(item)
  }
  for (const [name, fn] of Object.entries(handlers(report))) pi.on(name, fn)
}
