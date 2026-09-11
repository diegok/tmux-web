// managed by tmux-web (wterm-schema: 1)
// Reinstalling or updating the integration overwrites this file.
// It does one thing: `tmux set-option -p @wterm_agent`. Nothing else.
//
// WHAT IS DELIBERATELY NOT HERE. No state name, no mapping from an event to a
// state, no activity text, no sanitizer, no command line. All of that is one
// table in `wterm-web report` (Go), shared by the three integrations, so that
// it exists once instead of drifting across a TypeScript extension, a
// JavaScript plugin and a settings.json the user owns. If you find yourself
// writing the word "working" or "idle" in this file, or reducing a payload to
// a line of text, something has gone wrong. permission.asked in particular
// carries no question string and a `metadata` whose keys vary by permission
// class -- one of them a whole unified diff -- and reducing that is
// activity.go's job, per class.
//
// What is left is two runtime gates that only the runtime can apply: which
// process this is, and which SESSION an event belongs to.
//
// NOTHING HERE IS ASYNC AND NOTHING RETURNS A VALUE. opencode awaits its hooks
// SEQUENTIALLY ACROSS PLUGINS -- a measured 4 s stall in one hook pushed a
// turn's status to 8956 ms -- so every handler pushes to the queue and
// returns. The queue's push() returns undefined for the same reason: there is
// nothing for an awaited hook to wait on even by accident.

import { makeQueue, spawnReport } from './queue.ts'

/**
 * The bus events this plugin forwards, and the reason there is a list at all.
 *
 * opencode's `event` hook sees EVERYTHING: one `message.part.updated` per
 * streamed chunk, 45 `plugin.added` at startup, a `session.updated` on every
 * turn of the crank. Forwarding the lot would be a fork per token. The Go
 * table would ignore them all -- an event it does not know writes nothing --
 * but it would ignore them one process at a time.
 *
 * These are exactly the bus events `eventRules["opencode"]` in
 * cmd/wterm-web/events.go maps. `session.created` is not among them on
 * purpose: it is this plugin's own bookkeeping, the one event that establishes
 * parentage, and not a state of the pane. `session.error` is not among them
 * either -- it is real and it does fire, but a failed provider call is not a
 * state this feature reports.
 *
 * @type {Set<string>}
 */
const busEvents = new Set(['session.status', 'permission.asked', 'todo.updated', 'session.idle'])

/**
 * sessionTree remembers which sessions are children of which.
 *
 * WHY THIS EXISTS. Child sessions arrive in every hook and TMUX_PANE is the
 * same for all of them, so an unfiltered child event overwrites the root's
 * state -- and the damage that matters is a child's turn end firing while the
 * root is still working, which becomes a false `done` badge mid-turn. Measured
 * on opencode 1.18.30: a subagent's `session.idle` arrives 2.05 s before the
 * root's, while the root is still `busy`.
 *
 * WHY IT IS HERE AND NOT IN GO. Deciding whether session X is a child needs
 * memory of an earlier event -- `session.created`, whose `properties.info`
 * carries the `parentID`, and which is the only event that carries it at all.
 * Every `wterm-web report` process is fresh and has no memory of anything.
 *
 * IT IS ABSENCE-CODED AND THEREFORE FAILS OPEN: a payload that lost its
 * parentID -- renamed, nested a level deeper, dropped in a refactor -- reads
 * as root. That cannot be inverted, because "only report sessions I saw a root
 * session.created for" silences every pane whose opencode started before the
 * plugin did, which is every pane immediately after an install. It can only be
 * tightened, and it is: a payload that does not carry a session id we can read
 * is refused rather than reported.
 *
 * WHAT THIS FILTER ACTUALLY BUYS, stated honestly because the alternative is
 * somebody assuming it is belt and braces. Behind it there is exactly one
 * thing: evidence rule 3, which needs a connected client, needs the pane to
 * keep churning for a whole verification window, and leaks even then -- 4 of
 * 88 captured turns went still while waiting on the model. WITH NO CLIENT
 * CONNECTED, A SUBAGENT FALSE-IDLE ON OPENCODE IS UNDEFENDED. This is not two
 * independent mechanisms; it is one mechanism that only runs when somebody is
 * watching, and this map.
 *
 * EXPORTED, and for the same reason pi.ts exports its handlers: it is the only
 * real logic in the file, and the Go wiring test that would otherwise be its
 * only cover is allowed to skip when opencode is not installed -- and cannot
 * reach this case even when it runs, because a subagent needs a working model
 * and the wiring test deliberately has none.
 */
export function sessionTree() {
  /** @type {Map<string, string>} session id -> parent id, for sessions we have seen */
  const parents = new Map()

  return {
    /**
     * note records what one payload says about parentage. Called on every bus
     * event, BEFORE the forwarding whitelist and before the root gate --
     * session.created is not forwarded, so a note() that ran after either gate
     * would never see the only event that names a parent.
     *
     * @param {unknown} payload
     */
    note(payload) {
      const info = payload?.properties?.info
      const id = info?.id ?? payload?.properties?.sessionID
      const parent = info?.parentID
      if (typeof id !== 'string' || typeof parent !== 'string') return
      if (id === '' || parent === '' || id === parent) return
      parents.set(id, parent)
    },

    /**
     * isRoot walks the chain to the top. An id we have never seen is root:
     * absence-coded, failing open, as above. An id we cannot read at all is
     * not -- that is the tightening, and it is the difference between "I do
     * not know whose session this is" and "this session has no parent".
     *
     * The walk is what makes a subagent that launched a subagent silent too:
     * only the immediate parent is in any one payload, so a map that answers
     * "is my parent the root" reports a grandchild as a root of its own.
     *
     * @param {unknown} sessionID
     * @returns {boolean}
     */
    isRoot(sessionID) {
      if (typeof sessionID !== 'string' || sessionID === '') return false
      let id = sessionID
      // Bounded, because a cycle in this map would be an opencode bug we
      // should survive rather than hang the agent's own hook on.
      for (let hops = 0; hops < 64 && parents.has(id); hops++) id = parents.get(id)
      return id === sessionID
    },
  }
}

/**
 * handlers is the hook object, minus the queue.
 *
 * Exported for the same reason pi.ts exports its own: driven here from
 * opencode.test.ts with a fake `report`, the gates are covered on every run
 * rather than only on a machine with opencode installed.
 *
 * Each handler is gated on the session being the root and pushes `{ event,
 * payload? }` -- Task 16's item shape, no timestamp, no state name -- and each
 * one is written so that a hook argument that is not the shape it was
 * yesterday returns quietly instead of throwing on opencode's own stack.
 *
 * @param {(item: { event: string, payload?: unknown }) => void} report
 * @returns {Record<string, (a?: unknown, b?: unknown) => void>}
 */
export function handlers(report) {
  const tree = sessionTree()

  return {
    // The turn start, and load-bearing beyond "the agent is working": the turn
    // end is an EDGE -- it writes a resting state without reading what is
    // standing -- and that is only safe because a turn start put a non-resting
    // one in front of it.
    //
    // THE PROMPT IS NOT SENT. This event's mapping is rung 4 of the activity
    // ladder, a state-only report, and the only string in its payload that is
    // not an id is `output.parts[].text` -- the user's raw prompt. pi's `input`
    // is sent the same way and for the same reason.
    'chat.message': (input) => {
      if (!tree.isRoot(input?.sessionID)) return
      report({ event: 'chat.message' })
    },

    // Both arguments, because the tool's arguments are under the SECOND of
    // them: events.go's textOpencodeTool reads `input.tool` and `output.args`.
    // Which keys of those mean anything, and how a path or a command becomes
    // one line, is activity.go's decision, made against the recorded fixtures.
    'tool.execute.before': (input, output) => {
      if (!tree.isRoot(input?.sessionID)) return
      report({ event: 'tool.execute.before', payload: { input, output } })
    },

    // The bus. `session.status` (whose busy/idle discrimination is events.go's
    // whitelist, not ours -- the turn end is `session.idle` alone, and status
    // idle fires in the same millisecond), `permission.asked`, `todo.updated`
    // and `session.idle` are forwarded whole and unreduced.
    event: (arg) => {
      const event = arg?.event
      tree.note(event)
      if (!busEvents.has(event?.type)) return
      if (!tree.isRoot(event?.properties?.sessionID)) return
      report({ event: event.type, payload: event })
    },
  }
}

/**
 * What opencode loads. One tree and one queue per plugin instance.
 *
 * THE TMUX_PANE GUARD IS FIRST, and it is not a precaution copied from
 * somewhere. This plugin runs IN-PROCESS, as a child of the pane's shell, so
 * it normally inherits the pane's TMUX_PANE. But `opencode serve` somewhere
 * else plus a client attaching puts this same plugin in the SERVER's process,
 * where either there is no pane to report on or -- worse -- there is a stale
 * TMUX_PANE from whatever shell started the server, and this session's state
 * would be written onto somebody else's row. No pane, no plugin: an empty hook
 * object, and not one fork's worth of machinery built.
 */
export default async function () {
  if (!process.env.TMUX_PANE) return {}
  const q = makeQueue(spawnReport('opencode'))
  return handlers((item) => q.push(item))
}
