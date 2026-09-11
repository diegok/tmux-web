// The opencode plugin's two runtime gates, driven with a fake `report`.
//
// Layer 0 of Task 18's test story, and here it is the bigger half: opencode's
// child sessions arrive in every hook with the same TMUX_PANE, and a child's
// `session.idle` fires 2.05 s BEFORE the root's while the root is still busy.
// Unfiltered, that is a false `done` badge in the middle of a turn. The map
// that stops it is the only real logic in the file, and the Go wiring test
// that would otherwise be its only cover is allowed to t.Skip on a machine
// without opencode -- and cannot reach the child case at all, because a
// subagent needs a working model.
//
// So every case below is one of the mutants in Task 18's table, plus the two
// the table forgot (the TMUX_PANE guard, and a payload with no session id at
// all). Nothing here spawns a process or touches tmux.

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

// A recorder standing in for the queue's push.
function recorder() {
  const items: Item[] = []
  return { items, report: (item: Item) => void items.push(item) }
}

// One recorded payload, from the fifteen Task 12 captured off opencode
// 1.18.30. Read from the file rather than retyped, for the same reason pi's
// test reads its own: this plugin must hand the event over WHOLE, and every
// reduction -- the basename rule, the per-permission-class metadata reduction,
// the 1 KiB cap -- lives in `tmux-web report`.
function fixture(name: string): any {
  return JSON.parse(
    readFileSync(new URL(`../../cmd/tmux-web/testdata/hooks/opencode/${name}.json`, import.meta.url), 'utf8'),
  )
}

// The two session ids in the capture. The child's session.created names the
// root in `properties.info.parentID`, which is the ONLY event that establishes
// parentage -- session.idle carries `properties.sessionID` and nothing else.
const rootID = 'ses_f7135fcc9ffebuJVObk3PCMWN8'
const childID = 'ses_f7133db4cffe0EZNl6K9sGw9IX'

// What opencode's `event` hook is really handed: one object with an `event`
// key, measured on 1.18.30 rather than assumed. The second argument is
// undefined.
const bus = (event: unknown) => ({ event })

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
    // grandchild as a root of its own -- and its idle is the same false `done`
    // the direct child's would have been.
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
    // note() is called on every bus event, so it meets fourteen shapes that
    // are not session.created -- and, in a plugin that must never throw inside
    // a hook opencode awaits, a malformed one too.
    const tree = sessionTree()
    for (const p of [
      fixture('session_idle_child'),
      fixture('session_status_busy'),
      fixture('todo_updated'),
      undefined,
      null,
      'not an object',
      { type: 'session.created' },
      { type: 'session.created', properties: { info: { id: childID, parentID: childID } } },
    ]) {
      expect(() => tree.note(p)).not.toThrow()
    }
    expect(tree.isRoot(childID)).toBe(true)
  })
})

describe('handlers', () => {
  it('says nothing at all for a child session', () => {
    // THE case this file exists for. A child's session.idle arrives 2.05 s
    // before the root's, while the root is still busy, and nothing but this
    // map separates them: the payload is `{sessionID}` and nothing else.
    const r = recorder()
    const h = handlers(r.report)
    h.event(bus(fixture('session_created_root')))
    h.event(bus(fixture('session_created_child')))
    const child = fixture('chat_message_child')
    h['chat.message'](child.input, child.output)
    h.event(bus(fixture('session_idle_child')))
    expect(r.items).toEqual([])

    // And the root's own idle, in the same run, still reports: a filter that
    // silences a child by silencing the pane is not a filter.
    h.event(bus(fixture('session_idle_root')))
    expect(r.items).toEqual([{ event: 'session.idle', payload: fixture('session_idle_root') }])
  })

  it('says nothing for a grandchild either', () => {
    // The nested chain through the real hook, because `note` runs on the
    // session.created that the forwarding whitelist does NOT contain: a `note`
    // called after that gate registers nothing and every child reports.
    const r = recorder()
    const h = handlers(r.report)
    h.event(bus(fixture('session_created_child')))
    h.event(bus(created('ses_grandchild', childID)))
    h.event(bus(idle('ses_grandchild')))
    expect(r.items).toEqual([])
  })

  it('forwards the root turn in the order it happened, turn start first', () => {
    // The turn-start invariant, as a sequence. opencode's turn end is an EDGE
    // -- it writes idle without reading what is standing -- and that is safe
    // only because a working-producing event was written in front of it.
    // opencode has two of those, chat.message and session.status(busy), and
    // dropping either from this file loses a badge on any turn the other one
    // was collapsed out of by the queue.
    const r = recorder()
    const h = handlers(r.report)
    const msg = fixture('chat_message')
    const tool = fixture('tool_execute_before_bash')
    h['chat.message'](msg.input, msg.output)
    h.event(bus(fixture('session_status_busy')))
    h.event(bus(fixture('todo_updated')))
    h['tool.execute.before'](tool.input, tool.output)
    h.event(bus(fixture('permission_asked_bash')))
    h.event(bus(fixture('session_idle_root')))
    expect(r.items.map((i) => i.event)).toEqual([
      'chat.message',
      'session.status',
      'todo.updated',
      'tool.execute.before',
      'permission.asked',
      'session.idle',
    ])
  })

  it('hands the bus events over whole and the tool call as both its arguments', () => {
    const r = recorder()
    const h = handlers(r.report)
    const tool = fixture('tool_execute_before_write')
    h['tool.execute.before'](tool.input, tool.output)
    h.event(bus(fixture('todo_updated')))
    h.event(bus(fixture('permission_asked_edit')))
    expect(r.items).toEqual([
      // events.go reads input.tool and output.args -- the tool's arguments are
      // under the SECOND of this hook's two arguments, so an item built from
      // `input` alone loses every activity line opencode has.
      { event: 'tool.execute.before', payload: { input: tool.input, output: tool.output } },
      { event: 'todo.updated', payload: fixture('todo_updated') },
      // Unreduced, `diff` and all: permission.asked's metadata keys vary by
      // permission class and one of them is a whole unified diff. The
      // reduction is activity.go's, per class, and a second copy of it here is
      // a second copy to keep in step.
      { event: 'permission.asked', payload: fixture('permission_asked_edit') },
    ])
  })

  it('sends the turn start without the prompt the user typed', () => {
    // chat.message's mapping is rung 4 of the activity ladder -- a state-only
    // report -- and the only string its payload carries that is not an id is
    // output.parts[].text, which is the user's raw prompt. pi's `input` is
    // sent the same way and for the same reason.
    const r = recorder()
    const h = handlers(r.report)
    const msg = fixture('chat_message')
    expect(JSON.stringify(msg)).toContain('Write a haiku about tmux panes')
    h['chat.message'](msg.input, msg.output)
    expect(r.items).toEqual([{ event: 'chat.message' }])
  })

  it('forwards session.status whole and invents no second turn end', () => {
    // The DECISION, asserted: opencode's turn end is `session.idle` alone.
    // `session.status` fires `idle` in the same millisecond, and events.go's
    // whitelist maps only `busy`, so this file forwards the event unreduced
    // and lets that whitelist adjudicate. Treating status idle as a turn end
    // as well would put two resting writes inside one resting period -- which
    // by the design's own criterion makes each of them a re-assertion rather
    // than an edge, and re-dates a finish the first one already dated.
    const r = recorder()
    const h = handlers(r.report)
    h.event(bus(fixture('session_status_busy')))
    h.event(bus(fixture('session_status_idle')))
    expect(r.items).toEqual([
      { event: 'session.status', payload: fixture('session_status_busy') },
      { event: 'session.status', payload: fixture('session_status_idle') },
    ])
  })

  it('forwards nothing but the six events the Go table knows', () => {
    // The bus is CHATTY -- one `message.part.updated` per streamed chunk, 45
    // `plugin.added` at startup, `session.updated` on every turn of the crank
    // -- so an unfiltered `event` hook is a fork per token. session.error is
    // in the table's mutant row for a second reason: it is a real event that
    // really fires (a bad provider key produced one in the run this plugin was
    // verified against) and it is not a state of the pane.
    const r = recorder()
    const h = handlers(r.report)
    for (const type of [
      'session.error',
      'message.part.updated',
      'message.updated',
      'session.updated',
      'session.diff',
      'plugin.added',
      'catalog.updated',
      'reference.updated',
      'integration.updated',
      'session.created',
      'session.deleted',
    ]) {
      h.event(bus({ id: 'evt_x', type, properties: { sessionID: rootID } }))
    }
    expect(r.items).toEqual([])
  })

  it('survives a hook argument that is not the shape it was yesterday', () => {
    // Every one of these runs on opencode's own stack inside a hook it awaits.
    // A throw here is a plugin that breaks the agent it was supposed to watch.
    const r = recorder()
    const h = handlers(r.report)
    expect(() => h.event(undefined)).not.toThrow()
    expect(() => h.event({})).not.toThrow()
    expect(() => h.event(bus({ type: 'session.idle' }))).not.toThrow()
    expect(() => h['chat.message'](undefined, undefined)).not.toThrow()
    expect(() => h['tool.execute.before'](undefined, undefined)).not.toThrow()
    expect(r.items).toEqual([])
  })

  it('records nothing but an event name and the hook’s own payload', () => {
    // The one-line contract with Go, and the assertion that catches somebody
    // adding a state name, a timestamp or a --text here.
    const r = recorder()
    const h = handlers(r.report)
    const tool = fixture('tool_execute_before_task')
    const msg = fixture('chat_message')
    h['chat.message'](msg.input, msg.output)
    h['tool.execute.before'](tool.input, tool.output)
    h.event(bus(fixture('session_idle_root')))
    expect(r.items).toHaveLength(3)
    for (const item of r.items) {
      expect(Object.keys(item).sort().join(',')).toMatch(/^event(,payload)?$/)
      expect(typeof item.event).toBe('string')
    }
  })

  it('never returns anything an awaited hook could wait on', () => {
    // opencode awaits its hooks SEQUENTIALLY ACROSS PLUGINS: 4 s of stall in
    // one of them pushed a turn's status to 8956 ms. push() returns undefined
    // so there is nothing to await even by accident, and that only holds if
    // nothing here is async.
    const r = recorder()
    const h = handlers(r.report)
    for (const [name, fn] of Object.entries(h)) {
      expect(fn.constructor.name, `${name} is an async function`).toBe('Function')
    }
    expect(h.event(bus(fixture('session_idle_root')))).toBeUndefined()
  })
})

// loadCopy loads the plugin the way opencode does -- one call of the default
// export -- and gives back both halves of what that call produces: the hook
// object it returned, and the items its own queue was pushed. Two calls are two
// copies in one process, which is what a global install plus a project install
// is.
async function loadCopy() {
  const pushed: Item[] = []
  vi.mocked(makeQueue).mockReturnValueOnce({ push: (item: Item) => void pushed.push(item) })
  const hooks = (await opencodePlugin()) as Record<string, (a?: unknown, b?: unknown) => void>
  return { hooks, pushed }
}

// The claim lives on globalThis, which vitest does not reset between tests.
beforeEach(() => {
  delete (globalThis as Record<string, unknown>)['__tmux_web_reporter__']
})

describe('the default export', () => {
  it('registers exactly the three hooks opencode is asked about', async () => {
    // Written out rather than taken from handlers(): against
    // `Object.keys(handlers(...))` a dropped hook would move both sides of the
    // comparison at once and survive. `event` is the bus -- permission.asked
    // arrives there and the `permission.ask` hook never fires at all.
    vi.stubEnv('TMUX_PANE', '%0')
    const hooks = await opencodePlugin()
    expect(Object.keys(hooks).sort()).toEqual(['chat.message', 'event', 'tool.execute.before'].sort())
    vi.unstubAllEnvs()
  })

  // The duplicate-load case, and on opencode it is the GLOBAL copy that loads
  // first -- the opposite order from pi, which is why the claim in queue.ts
  // compares schema numbers instead of taking the first claimant. From the
  // version that added `--global`, one process can hold two copies of this
  // plugin: $XDG_CONFIG_HOME/opencode/plugin/ loads in every project and
  // .opencode/plugin/ loads in this one, and opencode dedupes neither by
  // filename nor by content. Both copies' hooks are called on every event;
  // without the claim both would fork a `tmux-web report`, and two single-slot
  // queues would race for one pane.
  //
  // Driven through the real default export rather than through claimReporter,
  // because what queue.test.ts cannot see is whether this file CALLS it, and on
  // which side of the queue: claim at load, check at report. A copy that has
  // lost the slot has already handed opencode its hooks -- there is no
  // unregistering -- so it must go on being called and go on saying nothing.
  it('leaves exactly one of two loaded copies reporting', async () => {
    vi.stubEnv('TMUX_PANE', '%0')
    const first = await loadCopy()
    const second = await loadCopy()

    first.hooks['chat.message']({ sessionID: 'ses_root' })
    second.hooks['chat.message']({ sessionID: 'ses_root' })

    expect(first.pushed).toEqual([])
    expect(second.pushed).toEqual([{ event: 'chat.message' }])
    vi.unstubAllEnvs()
  })

  it('reports as opencode, through a queue of its own', async () => {
    // `opencode` is what events.go keys its table on and what the daemon
    // derives from pane_current_command; the wrong name here is a silent no-op
    // on every event, because an agent the table does not know reports
    // nothing.
    vi.stubEnv('TMUX_PANE', '%0')
    vi.mocked(spawnReport).mockClear()
    await opencodePlugin()
    expect(spawnReport).toHaveBeenCalledTimes(1)
    expect(spawnReport).toHaveBeenCalledWith('opencode')
    vi.unstubAllEnvs()
  })

  it('does nothing at all outside a tmux pane', async () => {
    // The mutant the table forgot. This plugin runs IN-PROCESS, so it usually
    // inherits the pane's TMUX_PANE -- but `opencode serve` elsewhere plus a
    // client attaching puts the same plugin in the server's process, where
    // there is no pane to report on and `report` would either find nothing or,
    // worse, find a stale TMUX_PANE from whatever shell started the server and
    // write this session's state onto somebody else's row.
    vi.stubEnv('TMUX_PANE', undefined)
    vi.mocked(spawnReport).mockClear()
    const hooks = await opencodePlugin()
    expect(hooks).toEqual({})
    // And not one fork's worth of machinery is built, either.
    expect(spawnReport).not.toHaveBeenCalled()
    vi.unstubAllEnvs()
  })
})

// -- payloads derived from the recorded ones -------------------------------
//
// Only for the chains no capture holds: a subagent that launches a subagent,
// which opencode allows and which the capture did not provoke. The shape is
// copied from session_created_child.json, which is real.

function created(id: string, parentID: string | undefined) {
  const info: Record<string, unknown> = { id, slug: 'derived', version: '1.18.30', agent: 'general' }
  if (parentID !== undefined) info.parentID = parentID
  return { id: `evt_${id}`, type: 'session.created', properties: { sessionID: id, info } }
}

function idle(id: string) {
  return { id: `evt_idle_${id}`, type: 'session.idle', properties: { sessionID: id } }
}
