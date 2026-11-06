// managed by tmux-web (tmux-web-schema: 2)
// Reinstalling or updating the integration overwrites this file.
// It does one thing: `tmux set-option -p @tmux_web_agent`. Nothing else.
//
// WHAT IS DELIBERATELY NOT HERE. No state name, no mapping from an event to a
// state, no activity text, no sanitizer, no command line. All of that is one
// table in `tmux-web report` (Go), shared by the three integrations, so that
// it exists once instead of drifting across a TypeScript extension, a
// JavaScript plugin and a settings.json the user owns. If you find yourself
// writing the word "working" or "idle" in this file, or reducing a payload to
// a line of text, something has gone wrong. permission.asked in particular
// carries a `metadata` whose keys vary by permission class -- one of them a
// whole patch -- and reducing that is activity.go's job, per class.
//
// What is left is two runtime gates that only the runtime can apply: which
// process this is, and which SESSION an event belongs to.
//
// THE TWO HOOKS ARE NEVER ASYNC AND NEVER RETURN A VALUE. opencode V2 awaits
// registered hooks -- a stall in one is a stall in the turn -- so the prompt
// and tool handlers push to the queue and return. The queue's push() returns
// undefined for the same reason: there is nothing for an awaited hook to wait
// on even by accident. The BUS is no longer a hook: it is a subscription over
// the server's event stream, run in the background, and nothing opencode does
// waits on it either. Each delivered event goes through the same sync handler
// the hooks use.

// The queue, inlined here by `tmux-web install-integration` -- one report in
// flight per pane, and the claim that leaves exactly one of two installed
// copies reporting. Its single source of truth is internal/integrations/queue.ts;
// this file may not gain a second copy of any of it.
import { claimReporter, makeQueue, spawnReport } from './queue.ts'

/**
 * The bus events this plugin forwards, and the reason there is a list at all.
 *
 * The V2 bus carries EVERYTHING: one delta event per streamed token, a usage
 * update per step, an inbox event per queued message. Forwarding the lot would
 * be a fork per token. The Go table would ignore them all -- an event it does
 * not know writes nothing -- but it would ignore them one process at a time.
 *
 * These are exactly the bus events `rules["opencode"]` in
 * internal/report/events.go maps, and
 * TestTheOpencodePluginForwardsExactlyTheEventsTheTableKnows reads this very
 * declaration and holds it to that table -- unconditionally, unlike the live
 * wiring test, which skips wherever opencode is not installed.
 *
 * MEASURED on opencode 2.2.5, and it is the largest change the V2 port made:
 * V1's `session.status` and `session.idle` are NOT on the public event stream
 * any more -- neither reaches a plugin subscription, with a client connected
 * or without one -- and `todo.updated` is gone with the todo feature's own
 * retreat from the bus. The turn lifecycle this feature used to read off
 * status/busy and idle now arrives as `session.execution.started` (the turn
 * start proper), `session.step.started` (busy's stand-in, one per model step),
 * and `session.execution.succeeded` / `.failed` / `.interrupted` (the turn
 * end, one edge each). `session.created` is not in the whitelist on purpose:
 * it is this plugin's own bookkeeping, the one event that establishes
 * parentage, and not a state of the pane. `session.step.failed` is not either:
 * a failed step retried within the turn is not a state of the pane, and the
 * turn that dies says so on `session.execution.failed`.
 *
 * @type {Set<string>}
 */
const busEvents = new Set([
  'session.execution.started',
  'session.step.started',
  'session.execution.succeeded',
  'session.execution.failed',
  'session.execution.interrupted',
  'permission.asked',
])

/**
 * sessionTree remembers which sessions are children of which.
 *
 * WHY THIS EXISTS. Child sessions arrive in every hook and TMUX_PANE is the
 * same for all of them, so an unfiltered child event overwrites the root's
 * state -- and the damage that matters is a child's turn end firing while the
 * root is still working, which becomes a false `done` badge mid-turn. Measured
 * on opencode 1.18.30 and true on 2.2.5: a subagent's turn end arrives before
 * the root's, while the root is still working.
 *
 * WHY IT IS HERE AND NOT IN GO. Deciding whether session X is a child needs
 * memory of an earlier event -- `session.created`, whose `data.parentID`
 * carries the parent, and which is the only event that carries it at all.
 * Every `tmux-web report` process is fresh and has no memory of anything.
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
 * 88 captured turns went still while waiting on the model. WITHOUT A CLIENT
 * CONNECTED, A SUBAGENT FALSE-TURN-END ON OPENCODE IS UNDEFENDED. This is not
 * two independent mechanisms; it is one mechanism that only runs when somebody
 * is watching, and this map.
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
     * note records what one bus event says about parentage. Called on every
     * subscription delivery, BEFORE the forwarding whitelist and before the
     * root gate -- session.created is not forwarded, so a note() that ran
     * after either gate would never see the only event that names a parent.
     *
     * The V2 bus wraps its payload in `data`; V1 nested it under
     * `properties.info`. Both are read here, because a V1-format copy can
     * still be sitting beside this one in an old project and the gate may as
     * well be right about both rather than right about one.
     *
     * @param {unknown} payload
     */
    note(payload) {
      const data = payload?.data ?? payload?.properties
      const id = data?.sessionID ?? data?.info?.id
      const parent = data?.info?.parentID ?? data?.parentID
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
 * handlers is the three delivery points, minus the queue.
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
 * The two hooks are plain sync functions: opencode awaits them. The bus
 * handler is the same sync shape even though nothing awaits it, because the
 * queue's discipline is the point.
 *
 * @param {(item: { event: string, payload?: unknown }) => void} report
 * @returns {{ prompt: (a?: unknown) => void, tool: (a?: unknown) => void, bus: (a?: unknown) => void }}
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
    // not an id is `prompt.text` -- the user's raw prompt. pi's `input` is
    // sent the same way and for the same reason.
    //
    // V1's name for this hook was chat.message; V2 calls it prompt and fires
    // it once per prompt ADMISSION -- the item name follows the runtime, and
    // events.go's table keys on it.
    prompt: (event) => {
      if (!tree.isRoot(event?.sessionID)) return
      report({ event: 'prompt' })
    },

    // The one hook with the tool's arguments under it. Forwarded WHOLE --
    // {tool, sessionID, agent, messageID, id, input} -- because which keys of
    // it mean anything, and how a path or a command becomes one line, is
    // activity.go's decision, made against the recorded fixtures. V1 carried
    // the args under a second `output` argument; V2 keeps one event with the
    // args on `input`, and the bash tool is now named shell.
    tool: (event) => {
      if (!tree.isRoot(event?.sessionID)) return
      report({ event: 'tool.execute.before', payload: event })
    },

    // The bus. `session.execution.started` and `session.step.started` are
    // working edges (the turn start proper, and busy's stand-in -- step
    // started fires once per model step, exactly as often as V1's status busy
    // did), `permission.asked` is the blocked one, and the three
    // `session.execution.*` endings are the turn end. Each is forwarded whole
    // and unreduced, envelope and all.
    bus: (event) => {
      tree.note(event)
      if (!busEvents.has(event?.type)) return
      if (!tree.isRoot(event?.data?.sessionID)) return
      report({ event: event.type, payload: event })
    },
  }
}

/**
 * TMUX_WEB_SCHEMA is the schema number of THIS copy, and it is the same number
 * as the one in the `managed by tmux-web (tmux-web-schema: N)` header of this
 * file. cmd/tmux-web/install.go holds its `opencodeSchema` against this
 * constant on every run of the Go suite, because two numbers that must agree
 * and are written down twice are two numbers that drift. It lives HERE since
 * the opencode V2 port and not in queue.ts: pi's install did not change shape
 * when this one did, and one shared constant would have forced a false "your
 * file changed" note onto one of the two. pi.ts carries its own.
 */
const TMUX_WEB_SCHEMA = 2

/**
 * What opencode loads. One tree, one queue and one claim per plugin instance.
 *
 * THE SHAPE, because it is the one thing V2 checks before anything here runs:
 * the default export must be an OBJECT carrying an `id` and a `setup`
 * function -- a V1 factory function fails the schema check and the plugin
 * never loads. The id is stable; plugin storage and the diagnostics key on it.
 * The definition is written as a plain object literal rather than through
 * `Plugin.define` from `@opencode/plugin` so this file stays dependency-free
 * -- it imports nothing but `node:child_process`, and nothing in opencode's
 * plugin bootstrap is obliged to be resolvable from here.
 *
 * THE TMUX_PANE GUARD IS FIRST, and it is not a precaution copied from
 * somewhere. This plugin runs IN-PROCESS, in opencode's background service, as
 * a descendant of some pane's shell, so it normally inherits that pane's
 * TMUX_PANE. But a service started by a bare shell plus clients attaching
 * gives this same plugin either no pane to report on or -- worse -- a stale
 * TMUX_PANE from whatever shell started the service, and every session's
 * state would be written onto somebody else's row. No pane, no plugin: setup
 * returns having registered nothing, and opencode never calls it again. It is
 * ALSO in front of the claim below, so a plugin with no pane to report on
 * does not take the slot away from a copy that has one.
 *
 * THE CLAIM IS MADE HERE AND CHECKED AT THE QUEUE, and those are deliberately
 * two moments. Since `--global`, one opencode process can hold two copies of
 * this file -- $XDG_CONFIG_HOME/opencode/plugin/ loads in every project and
 * .opencode/plugin/ loads in this one, and opencode dedupes neither -- so both
 * copies' hooks are called on every event. A copy that loses the claim cannot
 * withdraw the hooks it has already registered, so it stays registered and
 * says nothing. See queue.ts for why the winner is the newer schema rather
 * than the first to load.
 *
 * THE SUBSCRIPTION IS THE ONE ASYNC PIECE, and it is disposed the way the
 * runtime asks: setup returns the cleanup function, and aborting the
 * controller ends the iteration. Everything the loop does per event is the
 * same sync push the hooks do.
 */
export default {
  id: 'tmux-web',
  setup(ctx) {
    if (!process.env.TMUX_PANE) return
    const mine = claimReporter(TMUX_WEB_SCHEMA)
    const q = makeQueue(spawnReport('opencode'))
    const h = handlers((item) => {
      if (mine()) q.push(item)
    })

    void ctx.session.hook('prompt', (event) => h.prompt(event))
    void ctx.tool.hook('execute.before', (event) => h.tool(event))

    const controller = new AbortController()
    void (async () => {
      try {
        for await (const event of ctx.event.subscribe({ signal: controller.signal })) {
          // One delivery, one sync handler, one queue push. A throw here is a
          // dead reporting feature -- the loop ends and takes the badge with
          // it -- so the handler is guarded even though it is written not to
          // throw: the event shape is the runtime's, not ours.
          try {
            h.bus(event)
          } catch (err) {
            // Swallowed: a malformed delivery is one lost badge, not a reason
            // to stop watching the bus.
          }
        }
      } catch (err) {
        // The subscription itself failed -- aborted on unload, or the stream
        // broke. Either way the feature is over; say nothing.
      }
    })()

    return () => controller.abort()
  },
}
