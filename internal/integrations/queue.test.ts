// The single-slot queue, and the argv that goes through it.
//
// This module is the one piece of shipped behaviour in this feature that lives
// in TS/JS rather than in Go, and until this file existed it had no test story
// at all. It runs under `web/`'s vitest (node environment) via one extra entry
// in that config's `include`; the module itself stays here, beside the other
// integration sources, because `internal/integrations/` is what the installer
// //go:embed's. A copy under web/ is exactly what this arrangement exists to
// prevent.

import { beforeEach, describe, expect, it, vi } from 'vitest'

import { TMUX_WEB_SCHEMA, claimReporter, makeQueue, spawnReport } from './queue'

// A spawn that records what it was handed and hands back a promise the test
// resolves by hand. Every ordering claim in here is about WHEN that promise
// settles, so no test may resolve one implicitly.
function recordingSpawn() {
  const calls: { event: string; payload?: unknown }[] = []
  const resolvers: (() => void)[] = []
  const spawn = (item: { event: string; payload?: unknown }) => {
    calls.push(item)
    return new Promise<void>((resolve) => resolvers.push(resolve))
  }
  return { calls, resolvers, spawn }
}

// Flush every microtask the queue's chain is made of. A macrotask boundary
// drains the whole microtask queue first, so one hop is enough however many
// `.then`s the drain is written with.
const settle = () => new Promise((r) => setTimeout(r, 0))

describe('makeQueue', () => {
  it('keeps exactly one report in flight and collapses a burst to the newest', async () => {
    const spy = recordingSpawn()
    const q = makeQueue(spy.spawn)
    q.push({ event: 'tool_execution_start', payload: { tool: 'read' } })
    q.push({ event: 'tool_execution_start', payload: { tool: 'edit' } })
    q.push({ event: 'tool_execution_start', payload: { tool: 'bash' } })
    q.push({ event: 'tool_execution_start', payload: { tool: 'grep' } })
    q.push({ event: 'ui_prompt_start', payload: { title: 'Approve?' } })
    // Nothing has finished yet, so the four that arrived behind the first one
    // are still a single slot holding the newest.
    expect(spy.calls).toHaveLength(1)
    spy.resolvers[0]()
    await settle()
    // Five events, one in flight: exactly one queued, and it is the newest.
    expect(spy.calls).toHaveLength(2)
    expect(spy.calls[1].event).toBe('ui_prompt_start')
    expect(spy.calls[1].payload).toEqual({ title: 'Approve?' })
  })

  it('starts the queued spawn only after the in-flight one has finished', async () => {
    // This is what the queue gives the daemon's ordering filter, and it is the
    // whole of it: `report` stamps its own timestamp at process start, so two
    // reports that overlap could be stamped in either order, and the filter
    // refuses anything not newer than what it accepted -- i.e. it would
    // silently drop the newer STATE for having the older stamp.
    const spy = recordingSpawn()
    const q = makeQueue(spy.spawn)
    q.push({ event: 'input' })
    q.push({ event: 'agent_settled' })
    // The lifecycle, not a field: the second spawn must not have happened
    // while the first one's promise is still pending, however many turns of
    // the event loop go by.
    await settle()
    await settle()
    expect(spy.calls).toHaveLength(1)
    spy.resolvers[0]()
    await settle()
    expect(spy.calls).toHaveLength(2)
    expect(spy.calls[1].event).toBe('agent_settled')
  })

  it('drains one slot at a time, not the whole backlog at once', async () => {
    // The slot is refilled while the collapsed spawn is itself in flight. The
    // second drain must wait for the second child the same way the first did,
    // or "finished" degrades to "spawned" one hop further down.
    const spy = recordingSpawn()
    const q = makeQueue(spy.spawn)
    q.push({ event: 'a' })
    q.push({ event: 'b' })
    spy.resolvers[0]()
    await settle()
    expect(spy.calls.map((c) => c.event)).toEqual(['a', 'b'])
    q.push({ event: 'c' })
    await settle()
    // 'b' is still running, so 'c' is only queued.
    expect(spy.calls).toHaveLength(2)
    spy.resolvers[1]()
    await settle()
    expect(spy.calls.map((c) => c.event)).toEqual(['a', 'b', 'c'])
    // And the slot is empty again: a drained item is never spawned twice.
    spy.resolvers[2]()
    await settle()
    expect(spy.calls).toHaveLength(3)
  })

  it('never rejects, whatever the spawn does', async () => {
    // An integration that throws inside an agent's hook is worse than one that
    // reports nothing: pi and opencode await handlers with NO timeout.
    const unhandled: unknown[] = []
    const onUnhandled = (e: unknown) => unhandled.push(e)
    process.on('unhandledRejection', onUnhandled)
    try {
      const calls: string[] = []
      const q = makeQueue((item: { event: string }) => {
        calls.push(item.event)
        return Promise.reject(new Error('tmux-web: no such file or directory'))
      })
      expect(() => q.push({ event: 'input' })).not.toThrow()
      q.push({ event: 'agent_settled' })
      await settle()
      // The failure is swallowed AND the slot is released: a rejection that
      // escaped would leave the queue wedged with the drain never running.
      expect(calls).toEqual(['input', 'agent_settled'])
      await settle()
      expect(unhandled).toEqual([])
    } finally {
      process.off('unhandledRejection', onUnhandled)
    }
  })

  it('survives a spawn that throws synchronously', async () => {
    // `child_process.spawn` throws on a bad argv rather than rejecting, and it
    // is called from inside push, on the agent's own stack.
    const calls: string[] = []
    const q = makeQueue((item: { event: string }) => {
      calls.push(item.event)
      throw new Error('EINVAL')
    })
    expect(() => q.push({ event: 'input' })).not.toThrow()
    await settle()
    q.push({ event: 'agent_settled' })
    await settle()
    expect(calls).toEqual(['input', 'agent_settled'])
  })

  it('does not await the spawn', async () => {
    // Spawn, do not await. The spawn itself is a few milliseconds and that is
    // the budget. A 3s stall in pi's `input` delayed the turn by 3.03s; 4s in
    // opencode's chain delayed session.status busy to 8956ms.
    const calls: string[] = []
    const q = makeQueue((item: { event: string }) => {
      calls.push(item.event)
      return new Promise<void>(() => {}) // never settles
    })
    const returned = q.push({ event: 'input' })
    // Nothing for a handler to await, even by accident, and the child was
    // started on the way past rather than a microtask later.
    expect(returned).toBeUndefined()
    expect(calls).toEqual(['input'])
  })
})

describe('spawnReport', () => {
  // A stand-in for node:child_process.spawn: records argv, collects stdin and
  // lets the test decide when the child exits.
  function fakeProcessSpawn() {
    const calls: { file: string; args: string[] }[] = []
    const stdin: string[] = []
    let exit: (() => void) | undefined
    const spawn = (file: string, args: string[]) => {
      calls.push({ file, args })
      const handlers: Record<string, () => void> = {}
      exit = () => handlers.close?.()
      return {
        on(name: string, fn: () => void) {
          handlers[name] = fn
        },
        stdin: {
          end(chunk: string) {
            stdin.push(chunk)
          },
        },
      }
    }
    return { calls, stdin, spawn, exit: () => exit?.() }
  }

  it('builds the report argv, once, for both integrations', async () => {
    const fake = fakeProcessSpawn()
    const spawn = spawnReport('pi', fake.spawn as never)
    spawn({ event: 'tool_execution_start', payload: { tool: 'bash' } })
    expect(fake.calls).toHaveLength(1)
    expect(fake.calls[0].file).toBe('tmux-web')
    expect(fake.calls[0].args).toEqual(['report', '--agent', 'pi', '--event', 'tool_execution_start'])
    expect(fake.stdin).toEqual(['{"tool":"bash"}'])
  })

  it('sends an empty object when the item has no payload', async () => {
    const fake = fakeProcessSpawn()
    spawnReport('opencode', fake.spawn as never)({ event: 'session.idle' })
    expect(fake.stdin).toEqual(['{}'])
  })

  it('settles when the child exits, not when it is spawned', async () => {
    // The mutant the queue's own tests CANNOT catch, because they inject the
    // spawn: settle at spawn time and the queue's "the collapsed spawn starts
    // after the in-flight one finished" degrades to "after it started", two
    // children stamp out of order, and the daemon's ordering filter drops the
    // newer state.
    const fake = fakeProcessSpawn()
    const settled = vi.fn()
    spawnReport('pi', fake.spawn as never)({ event: 'input' }).then(settled)
    await settle()
    expect(settled).not.toHaveBeenCalled()
    fake.exit()
    await settle()
    expect(settled).toHaveBeenCalledTimes(1)
  })

  it('settles rather than rejecting when the child cannot be started', async () => {
    const spawn = spawnReport('claude', ((): never => {
      throw new Error('ENOENT')
    }) as never)
    await expect(spawn({ event: 'input' })).resolves.toBeUndefined()
  })

  it('settles on the error event a failed exec reports asynchronously', async () => {
    // spawn() returns a child and reports ENOENT on the 'error' event; no
    // 'close' follows on some platforms, so a promise waiting only for close
    // would never settle and the queue's slot would never be released.
    const handlers: Record<string, () => void> = {}
    const child = {
      on(name: string, fn: () => void) {
        handlers[name] = fn
      },
      stdin: { end() {} },
    }
    const settled = vi.fn()
    spawnReport('pi', (() => child) as never)({ event: 'input' }).then(settled)
    await settle()
    expect(settled).not.toHaveBeenCalled()
    handlers.error?.()
    await settle()
    expect(settled).toHaveBeenCalledTimes(1)
  })
})

// -- the one-reporter claim ---------------------------------------------------
//
// The duplicate-load problem, measured on opencode 1.18.30 and pi 0.85.1:
// neither runtime dedupes by filename, so the same integration installed both
// globally and in the project is LOADED TWICE in one process. The effect on the
// pane option is benign -- both copies write the same value -- but it doubles
// every `tmux-web report` spawn and puts two single-slot queues in a race for
// the pane, which is the one ordering guarantee queue.ts exists to give.
//
// The two runtimes load the two scopes in OPPOSITE ORDERS -- opencode does
// global first, pi does project first -- so "the first one to claim wins" would
// pick a different copy in each, and after a partial upgrade that means the
// OLDER copy wins in one of them. Every case below is one of those orders.
describe('claimReporter', () => {
  const slot = '__tmux_web_reporter__'
  beforeEach(() => {
    delete (globalThis as Record<string, unknown>)[slot]
  })

  it('lets a single copy report', () => {
    expect(claimReporter(TMUX_WEB_SCHEMA)()).toBe(true)
  })

  it('leaves exactly one owner when two copies of the same schema load', () => {
    const first = claimReporter(TMUX_WEB_SCHEMA)
    const second = claimReporter(TMUX_WEB_SCHEMA)
    expect([first(), second()]).toEqual([false, true])
  })

  // opencode's order: the global copy loads first. A stale global and a fresh
  // project install must leave the PROJECT copy reporting.
  it('hands the slot to a newer schema loading second', () => {
    const older = claimReporter(1)
    const newer = claimReporter(2)
    expect([older(), newer()]).toEqual([false, true])
  })

  // pi's order: the project copy loads first. The same partial upgrade seen
  // from the other side, and the case a first-wins guard gets wrong.
  it('keeps the slot when an older schema loads second', () => {
    const newer = claimReporter(2)
    const older = claimReporter(1)
    expect([newer(), older()]).toEqual([true, false])
  })

  // Three copies cannot happen today, but "the newest wins" has to mean the
  // newest and not the last, whatever order they arrive in.
  it('keeps the newest of three whatever the order', () => {
    const a = claimReporter(2)
    const b = claimReporter(3)
    const c = claimReporter(1)
    expect([a(), b(), c()]).toEqual([false, true, false])
  })

  // The slot is a key on globalThis in somebody else's process. Anything at all
  // can be sitting there, and a guard that throws on it takes the integration
  // down with it -- on opencode that is inside the plugin factory, which is
  // exactly where a throw costs the pane its reporting.
  it('survives a foreign value in the slot', () => {
    ;(globalThis as Record<string, unknown>)[slot] = 'not ours'
    expect(claimReporter(TMUX_WEB_SCHEMA)()).toBe(true)
  })

  // And the case that makes the predicate re-read globalThis instead of closing
  // over the object it claimed. A copy that arrives after something foreign has
  // landed on the key has to REPLACE the slot -- there is nothing in it to
  // trust -- and the copy that owned the old one must lose. A predicate that
  // only remembered `slot.owner === me` would have both of them reporting,
  // which is the exact failure the whole claim exists to prevent.
  it('drops an owner whose slot has been replaced', () => {
    const first = claimReporter(2)
    ;(globalThis as Record<string, unknown>)[slot] = 'somebody else got here'
    const second = claimReporter(1)
    expect([first(), second()]).toEqual([false, true])
  })
})
