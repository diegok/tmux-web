// @ts-nocheck -- see "THIS FILE'S BODY IS PLAIN JAVASCRIPT" below. The types
// in this file are JSDoc, and TypeScript reads JSDoc types only in `.js`
// files: in a `.ts` file they are comments, so `new Promise((resolve) => ...)`
// infers `Promise<unknown>` and every `resolve()` in here is an arity error
// that annotating the file -- the one thing this file may not have -- is the
// only fix for. The pragma is a comment, so it survives into the `.js` the
// installer ships, where the JSDoc below starts being read for real. Its cost
// is that pi.ts sees these two exports as `any`; pi.ts's own body is checked
// in full by web/tsconfig.integrations.json.
//
// One report in flight per pane, and the argv that report is made of.
//
// THIS FILE'S BODY IS PLAIN JAVASCRIPT, deliberately, even though it is named
// .ts: the installer ships it into a `.js` context, so a type annotation here
// would force that installer either to strip types or to reopen a file it has
// already shipped. The .ts extension buys the test beside it, nothing more --
// what types there are below are JSDoc, which is a comment.
//
// WHAT IT IS FOR. A burst of tool calls must not become a queue of forks. At
// most one `wterm-web report` runs per pane at a time; a state that arrives
// while one is running replaces whatever was waiting, because only the newest
// state is worth writing. Both pi and opencode are long-lived runtimes and can
// hold the slot in module scope. Claude Code gets no queue -- each hook is a
// fresh process and there is nowhere to put one -- and the daemon's ordering
// rule stands in for the queue it cannot have.
//
// THE ITEM CARRIES NO TIMESTAMP, and there is nowhere for one to come from. An
// item is exactly what `report` needs on its command line and its stdin --
// { event, payload } -- and the timestamp is stamped by `report` itself, from
// time.Now() at process start. So the ordering guarantee here is SERIALIZATION,
// not stamping: the collapsed spawn starts only after the in-flight one has
// FINISHED, so the process it starts stamps a strictly later millisecond and
// the daemon's ordering filter sees the two states in the order the events
// happened. Settle a spawn early -- at fork time rather than at child exit --
// and "finished" degrades to "spawned", two children can stamp out of order,
// and the filter silently drops the newer state for carrying the older stamp.
// A reader who assumes the queue carries times will add a field nothing fills.
//
// AND NOTHING HERE IS EVER AWAITED BY A HANDLER. push() returns undefined, so
// there is nothing for an agent's hook to await even by accident: pi and
// opencode await their handlers with no timeout at all, and a reporting
// feature that can stall a turn is worse than no reporting feature. The queue
// waits on the child; the handlers do not wait on the queue.

import { spawn as spawnChildProcess } from 'node:child_process'

/**
 * @typedef {{ event: string, payload?: unknown }} ReportItem
 *   Exactly what `wterm-web report` needs: the event name for its command line
 *   and the hook's own JSON for its stdin. No timestamp; see above.
 */

/**
 * makeQueue builds the slot. `spawn(item)` returns a promise that settles when
 * that report has finished; the queue never looks inside an item.
 *
 * @param {(item: ReportItem) => unknown} spawn
 * @returns {{ push: (item: ReportItem) => void }}
 */
export function makeQueue(spawn) {
  // The two pieces of state, and there are only two: whether a child is
  // running, and the single item waiting for it. An array here would turn a
  // burst of tool calls into a backlog of forks, which is the thing this
  // module exists to stop.
  let running = false
  /** @type {ReportItem | null} */
  let queued = null

  /** @param {ReportItem} item */
  function start(item) {
    running = true
    let pending
    try {
      pending = spawn(item)
    } catch (err) {
      // child_process.spawn throws on a bad argv rather than rejecting, and it
      // is called on the agent's own stack.
      pending = undefined
    }
    // The drain runs on both outcomes. A failed report is silence -- never a
    // rejection escaping into a hook -- and, just as important, a rejection
    // that escaped would leave `running` true forever and wedge the pane's
    // reporting for the life of the runtime.
    Promise.resolve(pending)
      .catch(() => {})
      .then(() => {
        running = false
        const next = queued
        queued = null
        if (next) start(next)
      })
  }

  return {
    /** @param {ReportItem} item */
    push(item) {
      if (running) {
        // Keep the newest and drop what it replaced: an intermediate state
        // that was never written is a state nobody needed to see.
        queued = item
        return
      }
      start(item)
    },
  }
}

/**
 * spawnReport builds the one command line this feature has, for the one agent
 * named. Tasks 17 and 18 import it rather than assembling argv of their own.
 *
 * The second parameter exists for this module's own test and defaults to the
 * real thing; nothing that ships passes it.
 *
 * @param {string} agent
 * @param {typeof spawnChildProcess} [spawnProcess]
 * @returns {(item: ReportItem) => Promise<void>}
 */
export function spawnReport(agent, spawnProcess = spawnChildProcess) {
  return function (item) {
    return new Promise((resolve) => {
      let child
      try {
        child = spawnProcess('wterm-web', ['report', '--agent', agent, '--event', item.event], {
          // stdin carries the hook's own payload. The child's output goes
          // nowhere: `report` writes its diagnostics to stderr, and the stderr
          // of a pi or opencode plugin is the pane the user is looking at.
          stdio: ['pipe', 'ignore', 'ignore'],
        })
      } catch (err) {
        resolve()
        return
      }
      // Settle exactly once, and only when the child is gone. 'error' is the
      // asynchronous half of a failed exec -- an ENOENT for a wterm-web that
      // is not on PATH arrives there, and no 'close' need follow it, so a
      // promise waiting only for 'close' would never settle and the slot would
      // never be released.
      let done = false
      const settle = () => {
        if (done) return
        done = true
        resolve()
      }
      child.on('error', settle)
      child.on('close', settle)
      try {
        child.stdin.end(JSON.stringify(item.payload ?? {}))
      } catch (err) {
        // A child that died between spawn and write reports itself on 'error'.
      }
    })
  }
}

// -- the one-reporter claim ---------------------------------------------------
//
// WHAT THIS IS FOR. From the version that added `--global`, this integration
// can be installed in TWO PLACES AT ONCE: in the agent's own configuration
// directory, where it loads in every project, and in one project's own. Neither
// runtime dedupes by filename -- measured on opencode 1.18.30 and pi 0.85.1 --
// so both copies load, in one process, and both register handlers.
//
// The damage is not the pane option: both copies write the same value, so the
// row looks right. It is that every event becomes TWO `wterm-web report` forks,
// and that two single-slot queues then race for one pane -- which is the one
// ordering guarantee makeQueue above exists to give, and the thing this whole
// module is built around.
//
// WHY IT COMPARES SCHEMAS RATHER THAN TAKING THE FIRST CLAIM. The two runtimes
// load the two scopes in OPPOSITE ORDERS -- opencode does global first, pi does
// project first -- so a first-wins guard picks a different copy in each. After a
// partial upgrade (a new global install over an old project one, or the other
// way round) that means the OLDER copy wins in one of the two runtimes, silently
// and for as long as the mismatch lasts. Comparing WTERM_SCHEMA makes the answer
// the same in both: the newer copy reports.
//
// The claim is made when the copy LOADS and the answer is read when it REPORTS,
// and those have to be two separate moments. A copy that loses the slot has
// already returned its handlers to the runtime by then -- there is no
// unregistering in either API -- so the loser must go on being called and go on
// saying nothing.

/**
 * WTERM_SCHEMA is the schema number of THIS copy, and it is the same number as
 * the one in the `managed by tmux-web (wterm-schema: N)` header of every file
 * the installer writes. cmd/wterm-web/install.go holds its own `wtermSchema`
 * against this constant on every run of the Go suite, because two numbers that
 * must agree and are written down twice are two numbers that drift.
 */
export const WTERM_SCHEMA = 1

/** The key on globalThis the two copies meet at. Namespaced, because it is a
 *  key in somebody else's process. */
const CLAIM_SLOT = '__wterm_web_reporter__'

/**
 * claimReporter claims the right to report for this process, and returns the
 * predicate that says whether this copy still holds it.
 *
 * `>=` rather than `>`: two copies of the SAME schema is the ordinary case -- a
 * global install and a project install made by the same wterm-web -- and one of
 * them has to win. The later loader takes it, which is arbitrary and is meant
 * to be: the two files are byte-identical, so there is nothing to choose
 * between them beyond leaving exactly one.
 *
 * @param {number} [schema] this copy's schema; the parameter exists for the
 *   test, and nothing that ships passes it.
 * @returns {() => boolean} whether this copy is the one that should report
 */
export function claimReporter(schema = WTERM_SCHEMA) {
  // Identity, not a name or a number: it is the only thing two copies of this
  // same code cannot accidentally share.
  const me = {}
  let slot = globalThis[CLAIM_SLOT]
  // Anything at all can be sitting on that key, and a guard that throws on it
  // throws inside a plugin factory -- which on opencode is exactly where a
  // throw costs the pane its reporting. Anything we do not recognise is
  // replaced rather than reasoned about.
  if (slot === null || typeof slot !== 'object' || typeof slot.schema !== 'number') {
    slot = { schema: -1, owner: null }
    globalThis[CLAIM_SLOT] = slot
  }
  if (schema >= slot.schema) {
    slot.schema = schema
    slot.owner = me
  }
  // The slot is re-read at report time, not captured: a later copy that found
  // something foreign on the key will have replaced the whole object, and a
  // predicate closed over the old one would have both copies reporting.
  return () => globalThis[CLAIM_SLOT] === slot && slot.owner === me
}
