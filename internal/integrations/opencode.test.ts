// The opencode plugin's two runtime gates, driven with a fake `report`.
//
// Layer 0 of Task 18's test story, and here it is the bigger half: opencode's
// child sessions arrive in every hook with the same TMUX_PANE, and a child's
// turn end fires BEFORE the root's while the root is still working. Unfiltered,
// that is a false `done` badge in the middle of a turn. The map that stops it
// is the only real logic in the file, and the Go wiring test that would
// otherwise be its only cover is allowed to t.Skip on a machine without
// opencode -- and cannot reach the child case at all, because a subagent needs
// a working model.
//
// So every case below is one of the mutants in Task 18's table, plus the two
// the table forgot (the TMUX_PANE guard, and a payload with no session id at
// all). Nothing here spawns a process or touches tmux.
//
// THE V2 SHAPE, and it is why this file looks different from its V1 self. The
// default export is no longer a factory opencode calls -- it is a definition
// with a `setup(ctx)` opencode calls ONCE, and the three delivery points are
// registered through that context: `ctx.session.hook('prompt', ...)`,
// `ctx.tool.hook('execute.before', ...)` and `ctx.event.subscribe(...)`. The
// hooks are driven here through a fake ctx; the subscription is driven through
// a fake async iterable the test delivers events into, because the loop is the
// one async piece and everything after it is the same sync push the hooks use.

import { readFileSync } from 'node:fs'

import { beforeEach, describe, expect, it, vi } from 'vitest'

import opencodePlugin, { handlers, sessionTree } from './opencode.js'
import { makeQueue, spawnReport } from './queue.ts'

// The real queue module with spies over it: the default export is the only
// place the agent NAME appears, and `spawnReport('pi')` in this file is a
// mutant no handler test can see -- it changes no item and no gate, only the
// argv of a process none of them spawns. Its only other cover is the Go wiring
// test, which skips when opencode is not installed.
vi.mock('./queue.ts', { spy: true })

type Item = { event: string; payload?: unknown }
type Ctx = Parameters<typeof opencodePlugin.setup>[0]

// A recorder standing in for the queue's push.
function recorder() {
  const items: Item[] = []
  return { items, report: (item: Item) => void items.push(item) }
}

// One recorded payload, from the captures off opencode 2.2.5 (and one off the
// 1.18.30 era for the parentage chain no V2 capture holds -- session.created
// is delivered before any plugin is up, so the V2 copies of these two came off
// the wire rather than out of a plugin). Read from the file rather than
// retyped, for the same reason pi's test reads its own: this plugin must hand
// the event over WHOLE, and every reduction -- the basename rule, the
// per-action-class metadata reduction, the 1 KiB cap -- lives in
// `tmux-web report`.
function fixture(name: string): any {
  return JSON.parse(
    readFileSync(new URL(`../../cmd/tmux-web/testdata/hooks/opencode/${name}.json`, import.meta.url), 'utf8'),
  )
}

// The two session ids in the captures. The child's session.created names the
// root in `data.parentID`, which is the ONLY event that establishes parentage
// -- a turn end carries `data.sessionID` and nothing else. The root here is
// the throwaway capture project's own session; the child is the subagent that
// ran beside it.
const rootID = 'ses_e53290111ffeiB9gk7g0uS78cw'
const childID = 'ses_e532901edffe1tGNdi6hzoXlyC'

// subscribeFake is the async iterable `ctx.event.subscribe` is asked for, plus
// the two handles a test drives it with: deliver() hands it one bus event, and
// finish() ends the stream the way an abort does.
function subscribeFake() {
  const buffer: unknown[] = []
  const waiting: Array<(r: IteratorResult<unknown>) => void> = []
  let ended = false
  const finish = () => {
    ended = true
    while (waiting.length) waiting.shift()!({ value: undefined, done: true })
  }
  return {
    deliver: (event: unknown) => {
      const w = waiting.shift()
      if (w) w({ value: event, done: false })
      else buffer.push(event)
    },
    finish,
    subscribe: (opts?: { signal?: AbortSignal }) => {
      opts?.signal?.addEventListener('abort', finish, { once: true })
      return {
        [Symbol.asyncIterator]: () => ({
          next: (): Promise<IteratorResult<unknown>> => {
            if (buffer.length) return Promise.resolve({ value: buffer.shift(), done: false })
            if (ended) return Promise.resolve({ value: undefined, done: true })
            return new Promise((resolve) => waiting.push(resolve))
          },
        }),
      }
    },
  }
}

// fakeCtx is what setup is handed. The three delivery points record their
// registrations, and the subscription comes from the feed a test passes in.
function fakeCtx(feed: ReturnType<typeof subscribeFake>): Ctx {
  return {
    session: { hook: vi.fn() },
    tool: { hook: vi.fn() },
    event: { subscribe: vi.fn(feed.subscribe) },
  } as unknown as Ctx
}

// The loop consumes deliveries on the microtask queue, so a test that has
// just delivered an event waits one tick before asserting on the push.
const settled = () => new Promise((resolve) => setTimeout(resolve, 0))

describe('sessionTree', () => {
  it('registers a child from the session.created that names its parent', () => {
    const tree = sessionTree()
    tree.note(fixture('session_created_root'))
    tree.note(fixture('session_created_child'))
    expect(tree.isRoot(rootID)).toBe(true)
    expect(tree.isRoot(childID)).toBe(false)
  })

  it('resolves a nested chain to the top, not one link', () => {
    // A subagent that launches a subagent. Only the immediate parent is in any
    // one payload, so a map that stops at "is my parent the root" reports the
    // grandchild as a root of its own -- and its turn end is the same false
    // `done` the direct child's would have been.
    const tree = sessionTree()
    tree.note(created('ses_a', undefined))
    tree.note(created('ses_b', 'ses_a'))
    tree.note(created('ses_c', 'ses_b'))
    expect(tree.isRoot('ses_a')).toBe(true)
    expect(tree.isRoot('ses_b')).toBe(false)
    expect(tree.isRoot('ses_c')).toBe(false)
  })

  it('reads a session it has never seen as the root, which is the fail-open direction', () => {
    // Asserted deliberately, and it is the direction this filter CANNOT be
    // inverted in. The discriminator is absence-coded: "no parentID" means
    // root, so a payload that lost its parentID -- renamed, nested one level
    // deeper, dropped in a refactor -- reads as root and reports. Inverting it
    // to "report only what I have seen a root session.created for" silences
    // every pane whose opencode started before the plugin did, which is every
    // pane after an install.
    const tree = sessionTree()
    tree.note(fixture('session_created_child'))
    expect(tree.isRoot('ses_nobody_ever_mentioned')).toBe(true)
  })

  it('refuses a payload that carries no session id at all', () => {
    // The one direction it CAN be tightened in, and the tightening the plan
    // asks for: fail-open is about an unknown id, not about a missing one. A
    // hook whose shape changed under us hands over `undefined`, and reporting
    // on `undefined` would be reporting on every session at once.
    const tree = sessionTree()
    expect(tree.isRoot(undefined)).toBe(false)
    expect(tree.isRoot('')).toBe(false)
    expect(tree.isRoot(null)).toBe(false)
    expect(tree.isRoot({ id: rootID })).toBe(false)
  })

  it('records nothing from the payloads that say nothing about parentage', () => {
    // note() is called on every subscription delivery, so it meets the rest of
    // the bus -- and, in a plugin whose host awaits its hooks, a malformed
    // delivery too.
    const tree = sessionTree()
    for (const p of [
      fixture('session_execution_succeeded_child'),
      fixture('session_step_started'),
      fixture('permission_asked_shell'),
      undefined,
      null,
      'not an object',
      { type: 'session.created' },
      { type: 'session.created', data: { sessionID: childID, parentID: childID } },
      { type: 'session.created', properties: { info: { id: childID, parentID: childID } } },
    ]) {
      expect(() => tree.note(p)).not.toThrow()
    }
    expect(tree.isRoot(childID)).toBe(true)
  })

  it('still reads the V1 payload shape, because an old copy may sit beside this one', () => {
    // note() reads `data` first and `properties.info` second. A stale
    // V1-format file in an old project never loads under V2 -- the runtime
    // refuses it at the schema -- but the gate costs one `??` to be right
    // about both, and a refactor that re-nests the payload fails safe toward
    // root either way.
    const tree = sessionTree()
    tree.note({ type: 'session.created', properties: { sessionID: 'ses_v1', info: { id: 'ses_v1', parentID: 'ses_root' } } })
    expect(tree.isRoot('ses_v1')).toBe(false)
  })
})

describe('handlers', () => {
  it('says nothing at all for a child session', () => {
    // THE case this file exists for. A child's turn end arrives before the
    // root's, while the root is still working, and nothing but this map
    // separates them: the turn-end payload is `{data: {sessionID}}` and
    // nothing else.
    const r = recorder()
    const h = handlers(r.report)
    h.bus(fixture('session_created_root'))
    h.bus(fixture('session_created_child'))
    // No capture holds a CHILD's prompt -- the capture project's own session
    // never spawned a subagent that prompted -- so this is the recorded root
    // prompt with the recorded child's id: the gate reads the session id and
    // nothing else, and the point is the gate, not the text.
    const child = { ...fixture('prompt'), sessionID: childID }
    h.prompt(child)
    h.bus(fixture('session_execution_succeeded_child'))
    expect(r.items).toEqual([])

    // And the root's own turn end, in the same run, still reports: a filter
    // that silences a child by silencing the pane is not a filter.
    h.bus(fixture('session_execution_succeeded'))
    expect(r.items).toEqual([
      { event: 'session.execution.succeeded', payload: fixture('session_execution_succeeded') },
    ])
  })

  it('says nothing for a grandchild either', () => {
    // The nested chain through the real delivery point, because `note` runs on
    // the session.created that the forwarding whitelist does NOT contain: a
    // `note` called after that gate registers nothing and every child reports.
    const r = recorder()
    const h = handlers(r.report)
    h.bus(fixture('session_created_child'))
    h.bus(created('ses_grandchild', childID))
    h.bus(idle('ses_grandchild'))
    expect(r.items).toEqual([])
  })

  it('forwards the root turn in the order it happened, turn start first', () => {
    // The turn-start invariant, as a sequence. opencode's turn end is an EDGE
    // -- it writes idle without reading what is standing -- and that is safe
    // only because a working-producing event was written in front of it.
    // opencode has several of those -- the prompt hook, execution.started,
    // step.started -- and dropping any one of them loses a badge on any turn
    // the others were collapsed out of by the queue.
    const r = recorder()
    const h = handlers(r.report)
    const tool = fixture('tool_execute_before_shell')
    h.prompt(fixture('prompt'))
    h.bus(fixture('session_execution_started'))
    h.bus(fixture('session_step_started'))
    h.tool(tool)
    h.bus(fixture('permission_asked_shell'))
    h.bus(fixture('session_execution_succeeded'))
    expect(r.items.map((i) => i.event)).toEqual([
      'prompt',
      'session.execution.started',
      'session.step.started',
      'tool.execute.before',
      'permission.asked',
      'session.execution.succeeded',
    ])
  })

  it('hands the bus events over whole and the tool call as the one event it is', () => {
    const r = recorder()
    const h = handlers(r.report)
    const tool = fixture('tool_execute_before_write')
    h.tool(tool)
    h.bus(fixture('permission_asked_edit'))
    expect(r.items).toEqual([
      // The V2 hook carries one event with the tool's arguments on `input`,
      // where V1 split them across two arguments. events.go reads `tool` and
      // `input` off it -- a payload built from anything less loses every
      // activity line opencode has.
      { event: 'tool.execute.before', payload: tool },
      // Unreduced, `patch` and all: permission.asked's metadata keys vary by
      // action class and one of them is a whole patch. The reduction is
      // activity.go's, per class, and a second copy of it here is a second
      // copy to keep in step.
      { event: 'permission.asked', payload: fixture('permission_asked_edit') },
    ])
  })

  it('sends the turn start without the prompt the user typed', () => {
    // The prompt hook's mapping is rung 4 of the activity ladder -- a
    // state-only report -- and the only string in its payload that is not an
    // id is `prompt.text`, which is the user's raw prompt. pi's `input` is
    // sent the same way and for the same reason.
    const r = recorder()
    const h = handlers(r.report)
    const msg = fixture('prompt')
    expect(JSON.stringify(msg)).toContain('Use the shell tool to run exactly')
    h.prompt(msg)
    expect(r.items).toEqual([{ event: 'prompt' }])
  })

  it('forwards nothing but the events the Go table knows', () => {
    // The bus is CHATTY -- one delta event per streamed token, a usage update
    // per step, an inbox event per queued message -- so an unfiltered
    // subscription is a fork per token. session.step.failed is in the mutant
    // list for a second reason: it is a real event that really fires (the
    // captured dead-provider turn produced one) and a failed STEP within a
    // turn is not a state of the pane.
    const r = recorder()
    const h = handlers(r.report)
    for (const type of [
      'session.step.failed',
      'session.step.ended',
      'session.text.delta',
      'session.reasoning.delta',
      'session.usage.updated',
      'session.inbox.enqueued',
      'session.created',
      'session.deleted',
      'message.part.updated',
      'plugin.updated',
    ]) {
      h.bus({ id: 'evt_x', type, data: { sessionID: rootID } })
    }
    expect(r.items).toEqual([])
  })

  it('survives a delivery that is not the shape it was yesterday', () => {
    // Every one of these runs on opencode's own stack: the hooks are awaited,
    // and the bus loop is the thing keeping the badge alive. A throw here is a
    // plugin that breaks the agent it was supposed to watch.
    const r = recorder()
    const h = handlers(r.report)
    expect(() => h.bus(undefined)).not.toThrow()
    expect(() => h.bus({})).not.toThrow()
    expect(() => h.bus({ type: 'session.execution.succeeded' })).not.toThrow()
    expect(() => h.prompt(undefined)).not.toThrow()
    expect(() => h.tool(undefined)).not.toThrow()
    expect(r.items).toEqual([])
  })

  it('records nothing but an event name and the hook’s own payload', () => {
    // The one-line contract with Go, and the assertion that catches somebody
    // adding a state name, a timestamp or a --text here.
    const r = recorder()
    const h = handlers(r.report)
    const tool = fixture('tool_execute_before_write')
    h.prompt(fixture('prompt'))
    h.tool(tool)
    h.bus(fixture('session_execution_succeeded'))
    expect(r.items).toHaveLength(3)
    for (const item of r.items) {
      expect(Object.keys(item).sort().join(',')).toMatch(/^event(,payload)?$/)
      expect(typeof item.event).toBe('string')
    }
  })

  it('never returns anything an awaited hook could wait on', () => {
    // opencode awaits its registered hooks: a stall in one of them is a stall
    // in the turn. push() returns undefined so there is nothing to await even
    // by accident, and that only holds if nothing here is async. The bus
    // handler keeps the same sync shape even though the loop does not await
    // it, because the queue's discipline is the point.
    const r = recorder()
    const h = handlers(r.report)
    for (const [name, fn] of Object.entries(h)) {
      expect(fn.constructor.name, `${name} is an async function`).toBe('Function')
    }
    expect(h.bus(fixture('session_execution_succeeded'))).toBeUndefined()
  })
})

// -- the default export ------------------------------------------------------
//
// Driven through setup() with a fake ctx, because what the handler tests
// cannot see is whether the registrations happen, on which delivery points,
// and -- for the claim -- on which side of the queue: claim at load, check at
// report. A copy that has lost the slot has already handed opencode its
// registrations -- there is no unregistering -- so it must go on being called
// and go on saying nothing.

// loadCopy runs setup the way opencode does -- once, with one context -- and
// gives back everything a test drives or asserts on: the feed the subscription
// delivers into, the registered hook callbacks, and the items the copy's own
// queue was pushed. Two calls are two copies in one process, which is what a
// global install plus a project install is.
async function loadCopy() {
  const pushed: Item[] = []
  vi.mocked(makeQueue).mockReturnValueOnce({ push: (item: Item) => void pushed.push(item) })
  const feed = subscribeFake()
  const ctx = fakeCtx(feed)
  const cleanup = opencodePlugin.setup(ctx)
  const prompt = vi.mocked(ctx.session.hook).mock.calls.find(([name]: unknown[]) => name === 'prompt')?.[1] as
    | ((a?: unknown) => void)
    | undefined
  const tool = vi.mocked(ctx.tool.hook).mock.calls.find(([name]: unknown[]) => name === 'execute.before')?.[1] as
    | ((a?: unknown) => void)
    | undefined
  expect(prompt, 'the prompt hook was not registered').toBeTypeOf('function')
  expect(tool, 'the tool hook was not registered').toBeTypeOf('function')
  return { feed, ctx, cleanup, prompt: prompt!, tool: tool!, pushed }
}

// The claim lives on globalThis, which vitest does not reset between tests.
beforeEach(() => {
  delete (globalThis as Record<string, unknown>)['__tmux_web_reporter__']
})

describe('the default export', () => {
  it('is a definition with an id and a setup, not a factory', () => {
    // The one thing V2 checks before anything else here runs. A V1 factory
    // function fails the schema check and the plugin never loads -- which is
    // the failure this port exists to fix, so the shape itself is asserted.
    expect(opencodePlugin).toBeTypeOf('object')
    expect(opencodePlugin.id).toBe('tmux-web')
    expect(opencodePlugin.setup).toBeTypeOf('function')
  })

  it('registers exactly the two hooks and the subscription', () => {
    // Written out rather than taken from handlers(): against
    // `Object.keys(handlers(...))` a dropped delivery point would move both
    // sides of the comparison at once and survive. The bus is not a hook any
    // more -- permission.asked arrives through the subscription, which is
    // also where every other bus event comes from.
    vi.stubEnv('TMUX_PANE', '%0')
    const feed = subscribeFake()
    const ctx = fakeCtx(feed)
    opencodePlugin.setup(ctx)
    expect(vi.mocked(ctx.session.hook).mock.calls.map(([name]: unknown[]) => name)).toEqual(['prompt'])
    expect(vi.mocked(ctx.tool.hook).mock.calls.map(([name]: unknown[]) => name)).toEqual(['execute.before'])
    expect(vi.mocked(ctx.event.subscribe)).toHaveBeenCalledTimes(1)
    vi.unstubAllEnvs()
  })

  it('reports as opencode, through a queue of its own', () => {
    // `opencode` is what events.go keys its table on and what the daemon
    // derives from pane_current_command; the wrong name here is a silent no-op
    // on every event, because an agent the table does not know reports
    // nothing.
    vi.stubEnv('TMUX_PANE', '%0')
    vi.mocked(spawnReport).mockClear()
    const feed = subscribeFake()
    opencodePlugin.setup(fakeCtx(feed))
    expect(spawnReport).toHaveBeenCalledTimes(1)
    expect(spawnReport).toHaveBeenCalledWith('opencode')
    vi.unstubAllEnvs()
  })

  it('does nothing at all outside a tmux pane', () => {
    // The mutant the table forgot. This plugin runs IN-PROCESS, so it usually
    // inherits the pane's TMUX_PANE -- but a service started by a bare shell
    // puts the same plugin in a process where there is no pane to report on,
    // and `report` would either find nothing or, worse, find a stale
    // TMUX_PANE from whatever shell started the service and write this
    // session's state onto somebody else's row.
    vi.stubEnv('TMUX_PANE', undefined)
    vi.mocked(spawnReport).mockClear()
    const feed = subscribeFake()
    const ctx = fakeCtx(feed)
    opencodePlugin.setup(ctx)
    expect(ctx.session.hook).not.toHaveBeenCalled()
    expect(ctx.tool.hook).not.toHaveBeenCalled()
    expect(ctx.event.subscribe).not.toHaveBeenCalled()
    // And not one fork's worth of machinery is built, either.
    expect(spawnReport).not.toHaveBeenCalled()
    vi.unstubAllEnvs()
  })

  it('leaves exactly one of two loaded copies reporting', async () => {
    // From the version that added `--global`, one process can hold two copies
    // of this plugin: $XDG_CONFIG_HOME/opencode/plugin/ loads in every
    // project and .opencode/plugin/ loads in this one, and opencode dedupes
    // neither by filename nor by content. Both copies' delivery points are
    // called on every event; without the claim both would fork a
    // `tmux-web report`, and two single-slot queues would race for one pane.
    vi.stubEnv('TMUX_PANE', '%0')
    const first = await loadCopy()
    const second = await loadCopy()

    first.prompt({ sessionID: 'ses_root' })
    second.prompt({ sessionID: 'ses_root' })

    expect(first.pushed).toEqual([])
    expect(second.pushed).toEqual([{ event: 'prompt' }])
    vi.unstubAllEnvs()
  })

  it('the subscription forwards the root turn and survives the stream ending', async () => {
    // The bus is the one async piece: driven here through the fake iterable,
    // delivered events land on the same sync handler the hooks use, and
    // finishing the stream -- what the abort in the cleanup does -- ends the
    // loop without a throw escaping.
    vi.stubEnv('TMUX_PANE', '%0')
    const copy = await loadCopy()
    copy.feed.deliver(fixture('session_created_root'))
    copy.feed.deliver(fixture('session_execution_started'))
    copy.feed.deliver(fixture('session_created_child'))
    copy.feed.deliver(fixture('session_execution_succeeded_child'))
    copy.feed.deliver(fixture('session_execution_succeeded'))
    copy.feed.finish()
    await settled()

    expect(copy.pushed.map((i) => i.event)).toEqual([
      'session.execution.started',
      'session.execution.succeeded',
    ])
    expect(copy.cleanup).toBeTypeOf('function')
    vi.unstubAllEnvs()
  })

  it('the cleanup aborts the subscription', async () => {
    // Registrations are scoped to the plugin and opencode disposes them on
    // unload; the subscription is the one resource this plugin owns, and the
    // runtime's contract is that setup returns the thing that stops it.
    vi.stubEnv('TMUX_PANE', '%0')
    const copy = await loadCopy()
    copy.cleanup?.()
    await settled()
    copy.feed.deliver(fixture('session_execution_started'))
    await settled()
    expect(copy.pushed).toEqual([])
    vi.unstubAllEnvs()
  })
})

// -- payloads derived from the recorded ones -------------------------------
//
// Only for the chains no capture holds: a subagent that launches a subagent,
// which opencode allows and which the captures did not provoke. The shape is
// copied from session_created_child.json, which is real; the turn end is the
// turn end V2 actually sends.

function created(id: string, parentID: string | undefined) {
  const data: Record<string, unknown> = {
    sessionID: id,
    slug: 'derived',
    version: '2.2.5',
    projectID: 'global',
  }
  if (parentID !== undefined) data.parentID = parentID
  return { id: `evt_${id}`, created: 1793901133356, type: 'session.created', data }
}

function idle(id: string) {
  return { id: `evt_idle_${id}`, created: 1793901184769, type: 'session.execution.succeeded', data: { sessionID: id } }
}
