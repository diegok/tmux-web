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
