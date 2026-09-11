/**
 * The management API as this app speaks it, and everything the menus and the
 * kill dialog decide before a pixel is drawn.
 *
 * The wire shape is implemented twice, in two languages, so the routes and the
 * `confirm` field are pinned against the Go source rather than against a copy
 * of themselves -- the approach useSnapshot.test.ts and DevicesDialog.test.ts
 * already take. A route renamed on the daemon then fails here instead of in a
 * browser.
 */

import { readFileSync } from 'node:fs'

import { describe, expect, it, vi } from 'vitest'

import {
  PANES_URL,
  SESSIONS_URL,
  WINDOWS_URL,
  ZOOM_HINT,
  agentPhrase,
  describeAction,
  killWarnings,
  manageRequest,
  newSessionPrompt,
  newWindowAction,
  performManage,
  planKill,
  promptReady,
  promptValues,
  rowMenu,
  rowTargetForWindow,
  runManage,
} from './manage'
import type { ManageAction, MenuEntry } from './manage'
import { groupRows } from './useSnapshot'
import type { SnapshotRow } from './useSnapshot'

const goSource = (path: string) => readFileSync(new URL(`../../../${path}`, import.meta.url), 'utf8')

function row(over: Partial<SnapshotRow> = {}): SnapshotRow {
  return {
    groupKey: 'work',
    sessionId: '$1',
    sessionName: 'work',
    paneId: '%0',
    paneIndex: 0,
    appOwned: false,
    label: '',
    // `@N`, derived from the index so that a fixture varying `windowIndex`
    // still describes two *different* windows -- the tree keys on the id.
    windowId: `@${over.windowIndex ?? 0}`,
    windowIndex: 0,
    windowName: 'shell',
    paneActive: true,
    command: 'zsh',
    title: 'devbox',
    agentState: '',
    finishedAt: 0,
    // Nothing reported for this pane and nothing decided its state: "" in
    // both, which is where every pane that is not an agent sits.
    activity: '',
    stateSource: '',
    question: undefined,
    ...over,
  }
}

/** A tree, and the three targets a row can be. */
function tree(rows: SnapshotRow[]) {
  const groups = groupRows(rows)
  const session = groups[0]
  const window = session.windows[0]
  return { groups, session, window, pane: window.panes[0] }
}

function response(status: number, body: unknown, json = true): Response {
  return {
    ok: status >= 200 && status < 300,
    status,
    json: async () => {
      if (!json) throw new SyntaxError('not json')
      return body
    },
  } as unknown as Response
}

function ids(entries: MenuEntry[]): string[] {
  return entries.map((e) => e.id)
}

// --- the contract with the daemon -------------------------------------------

describe('contract with the daemon', () => {
  it('calls the routes the daemon registers', () => {
    const server = goSource('internal/front/server.go')
    for (const route of [
      `"POST ${SESSIONS_URL}"`,
      `"POST ${WINDOWS_URL}"`,
      `"POST ${PANES_URL}"`,
      `"PATCH ${SESSIONS_URL}/{id}"`,
      `"PATCH ${WINDOWS_URL}/{id}"`,
      `"PATCH ${PANES_URL}/{id}"`,
      `"POST ${PANES_URL}/{id}/zoom"`,
      `"DELETE ${SESSIONS_URL}/{id}"`,
      `"DELETE ${WINDOWS_URL}/{id}"`,
      `"DELETE ${PANES_URL}/{id}"`,
    ]) {
      expect(server).toContain(route)
    }
  })

  it('sends the field names the handlers decode', () => {
    const manage = goSource('internal/front/manage.go')
    // Every JSON key this file puts on the wire, as the Go handlers spell them.
    const tags = ['name', 'path', 'session', 'fromPane', 'pane', 'direction', 'label', 'confirm']
    for (const tag of tags) {
      expect(manage).toContain(`json:"${tag}"`)
    }
  })

  it('spells the split directions the way the daemon maps them', () => {
    const tmux = goSource('internal/tmux/manage.go')
    // The daemon turns these into -h and -v. Sending "-h" from here would be
    // the frontend deciding which way a tmux split line runs, which is the
    // thing that mapping exists to keep out of the browser.
    expect(tmux).toContain('SplitRight = "right"')
    expect(tmux).toContain('SplitDown  = "down"')
  })
})

// --- manageRequest ----------------------------------------------------------

describe('manageRequest', () => {
  it('percent-encodes a pane id, because % is not a legal path byte', () => {
    const req = manageRequest({ verb: 'kill-pane', pane: '%3' })
    // encodeURIComponent("%3") is "%253". Sent raw, "%3" is an invalid
    // percent-escape and net/http answers 400 before the mux is consulted --
    // a failure that looks like a broken endpoint rather than a broken caller.
    expect(req.url).toBe('/api/panes/%253')
    expect(req.url).not.toBe('/api/panes/%3')
  })

  it('encodes window and session ids too, though they do not need it', () => {
    expect(manageRequest({ verb: 'kill-window', window: '@7' }).url).toBe('/api/windows/%407')
    expect(manageRequest({ verb: 'kill-session', session: '$1' }).url).toBe('/api/sessions/%241')
  })

  it('puts confirm on every delete', () => {
    const kills: ManageAction[] = [
      { verb: 'kill-pane', pane: '%3' },
      { verb: 'kill-window', window: '@7' },
      { verb: 'kill-session', session: '$1' },
    ]
    for (const action of kills) {
      const req = manageRequest(action)
      expect(req.method).toBe('DELETE')
      // Without it the daemon answers 400: it mirrors the dialog, so that a
      // request which never passed through the dialog cannot destroy a window.
      expect(req.body).toEqual({ confirm: true })
    }
  })

  it('puts confirm on nothing else', () => {
    const others: ManageAction[] = [
      { verb: 'new-session', name: 'api', path: '' },
      { verb: 'new-window', session: '$1', name: '', fromPane: '%3' },
      { verb: 'split', pane: '%3', direction: 'right' },
      { verb: 'rename-session', session: '$1', name: 'api' },
      { verb: 'rename-window', window: '@7', name: 'api' },
      { verb: 'label-pane', pane: '%3', label: 'reviewer' },
      { verb: 'zoom', pane: '%3' },
    ]
    for (const action of others) {
      expect(manageRequest(action).body ?? {}).not.toHaveProperty('confirm')
      expect(manageRequest(action).method).not.toBe('DELETE')
    }
  })

  it('zooms with a POST to the pane sub-route and no body', () => {
    expect(manageRequest({ verb: 'zoom', pane: '%12' })).toEqual({
      method: 'POST',
      url: '/api/panes/%2512/zoom',
      body: null,
    })
  })

  it('sends an empty label rather than omitting it, because empty clears', () => {
    // `{}` would leave the daemon's Label at "" too, but only by accident of
    // Go's zero value. The dialog means "erase this", and it says so.
    expect(manageRequest({ verb: 'label-pane', pane: '%3', label: '' }).body).toEqual({ label: '' })
  })

  it('renames by id, never by name', () => {
    const req = manageRequest({ verb: 'rename-session', session: '$1', name: 'api' })
    expect(req).toEqual({ method: 'PATCH', url: '/api/sessions/%241', body: { name: 'api' } })
  })

  it('carries the split direction in the body, not in the path', () => {
    expect(manageRequest({ verb: 'split', pane: '%3', direction: 'down' })).toEqual({
      method: 'POST',
      url: PANES_URL,
      body: { pane: '%3', direction: 'down' },
    })
  })
})

// --- performManage ----------------------------------------------------------

describe('performManage', () => {
  it('sends the method, the body and the device cookie', async () => {
    const fetchImpl = vi.fn(async (_url: string, _init: RequestInit) => response(201, { id: '%9' }))
    const result = await performManage({ verb: 'split', pane: '%3', direction: 'right' }, fetchImpl)

    expect(fetchImpl.mock.calls[0][0]).toBe(PANES_URL)
    const init = fetchImpl.mock.calls[0][1]
    expect(init.method).toBe('POST')
    expect(init.credentials).toBe('same-origin')
    expect(init.body).toBe(JSON.stringify({ pane: '%3', direction: 'right' }))
    expect((init.headers as Record<string, string>)['Content-Type']).toBe('application/json')
    expect(result).toEqual({ ok: true, id: '%9' })
  })

  it('sends no body and no content type for a zoom', async () => {
    const fetchImpl = vi.fn(async (_url: string, _init: RequestInit) => response(204, null))
    await performManage({ verb: 'zoom', pane: '%3' }, fetchImpl)
    const init = fetchImpl.mock.calls[0][1]
    expect(init.body).toBeUndefined()
    expect(init.headers).not.toHaveProperty('Content-Type')
  })

  it('reads a 204 as success with nothing created', async () => {
    const fetchImpl = async () => response(204, null)
    expect(await performManage({ verb: 'kill-pane', pane: '%3' }, fetchImpl)).toEqual({
      ok: true,
      id: null,
    })
  })

  it("quotes tmux's own words on a refusal", async () => {
    const fetchImpl = async () => response(400, { error: "can't find pane: %7" })
    expect(await performManage({ verb: 'kill-pane', pane: '%7' }, fetchImpl)).toEqual({
      ok: false,
      message: "can't find pane: %7",
    })
  })

  it('falls back to the status when the body is not JSON', async () => {
    // Protect answers 401 in plain text, so the status is all there is to say.
    const fetchImpl = async () => response(401, null, false)
    expect(await performManage({ verb: 'zoom', pane: '%3' }, fetchImpl)).toEqual({
      ok: false,
      message: 'the daemon answered 401',
    })
  })

  it('reports a dropped connection rather than throwing', async () => {
    const fetchImpl = async () => {
      throw new TypeError('Failed to fetch')
    }
    // A phone that changed cells mid-request. Every caller does the same thing
    // with this as with a 400, and a throw here would move that decision into a
    // catch block in a component where no test runs it.
    expect(await performManage({ verb: 'zoom', pane: '%3' }, fetchImpl)).toEqual({
      ok: false,
      message: 'Failed to fetch',
    })
  })
})

// --- runManage --------------------------------------------------------------

describe('runManage', () => {
  it('reports a failure and refreshes the sidebar', async () => {
    const notify = vi.fn()
    const refresh = vi.fn()
    await runManage(
      { verb: 'kill-pane', pane: '%7' },
      { notify, refresh, fetchImpl: async () => response(400, { error: "can't find pane: %7" }) },
    )
    // Both halves matter. A silent failure is a click that looks like it
    // worked; a stale row is a row that offers to kill something already gone.
    expect(notify).toHaveBeenCalledWith({
      title: 'Could not kill %7',
      description: "can't find pane: %7",
    })
    expect(refresh).toHaveBeenCalledTimes(1)
  })

  it('says nothing on success, but still refreshes', async () => {
    const notify = vi.fn()
    const refresh = vi.fn()
    await runManage(
      { verb: 'split', pane: '%3', direction: 'right' },
      { notify, refresh, fetchImpl: async () => response(201, { id: '%9' }) },
    )
    expect(notify).not.toHaveBeenCalled()
    // A successful split changes the tree as much as a failed kill does, and
    // waiting out a poll for it makes the sidebar look like it ignored a click.
    expect(refresh).toHaveBeenCalledTimes(1)
  })

  it('does not retry', async () => {
    const fetchImpl = vi.fn(async (_url: string, _init: RequestInit) => response(400, { error: 'no' }))
    await runManage({ verb: 'kill-pane', pane: '%7' }, { notify: () => {}, refresh: () => {}, fetchImpl })
    // A failed kill that silently succeeded on a retry is worse than one that
    // failed.
    expect(fetchImpl).toHaveBeenCalledTimes(1)
  })
})

describe('describeAction', () => {
  it('names the attempt by id, beside the message that names the same id', () => {
    expect(describeAction({ verb: 'kill-pane', pane: '%7' })).toBe('kill %7')
    expect(describeAction({ verb: 'rename-session', session: '$1', name: 'api' })).toBe(
      'rename $1 to "api"',
    )
    expect(describeAction({ verb: 'split', pane: '%3', direction: 'down' })).toBe(
      'split %3 to the bottom',
    )
    expect(describeAction({ verb: 'label-pane', pane: '%3', label: '' })).toBe(
      'clear the label on %3',
    )
  })
})

// --- rowMenu ----------------------------------------------------------------

describe('rowMenu', () => {
  it('offers a pane rename, a window, two splits, a zoom and a kill', () => {
    const { session, window, pane } = tree([row({ paneId: '%3' }), row({ paneId: '%4', paneIndex: 1 })])
    const entries = rowMenu({ kind: 'pane', session, window, pane }, 'work')
    expect(ids(entries)).toEqual([
      'label-pane',
      'new-window',
      'split-right',
      'split-down',
      'zoom',
      'kill',
    ])
    // The kill is last and is the only red one, so it is never the entry under
    // a thumb aiming at the one above it.
    expect(entries.at(-1)?.danger).toBe(true)
    expect(entries.filter((e) => e.danger)).toHaveLength(1)
  })

  it('says out loud that a zoom reaches every client', () => {
    const { session, window, pane } = tree([row()])
    const zoom = rowMenu({ kind: 'pane', session, window, pane }, 'work').find(
      (e) => e.id === 'zoom',
    )
    // Zoom is a window property, so it moves the terminal on the host too.
    expect(zoom?.hint).toBe(ZOOM_HINT)
    expect(ZOOM_HINT).toMatch(/every client/)
  })

  it('splits and zooms the pane in hand, and the window row uses its active one', () => {
    const { session, window, pane } = tree([
      row({ paneId: '%3', paneActive: false }),
      row({ paneId: '%4', paneIndex: 1, paneActive: true }),
    ])
    const fromPane = rowMenu({ kind: 'pane', session, window, pane }, 'work')
    expect(fromPane.find((e) => e.id === 'split-right')?.intent).toEqual({
      kind: 'run',
      action: { verb: 'split', pane: '%3', direction: 'right' },
    })
    const fromWindow = rowMenu({ kind: 'window', session, window }, 'work')
    // tmux's active pane for that window -- the same pane clicking the row
    // navigates to, so the split lands where the click would have taken you.
    expect(fromWindow.find((e) => e.id === 'zoom')?.intent).toEqual({
      kind: 'run',
      action: { verb: 'zoom', pane: '%4' },
    })
  })

  it('leaves out the window rename and kill when the wire carried no window id', () => {
    // The daemon addresses windows by @N and validates the shape, so a window
    // the snapshot named no id for -- one from a daemon too old to send the
    // field -- has nothing to send. A menu entry that cannot work is worse than
    // one that is not there.
    const none = tree([row({ windowId: '' }), row({ paneId: '%1', paneIndex: 1, windowId: '' })])
    expect(ids(rowMenu({ kind: 'window', ...none }, 'work'))).toEqual([
      'new-window',
      'split-right',
      'split-down',
      'zoom',
    ])
    // And with one, both come back with no other change -- and both address the
    // id off the row rather than the index or the name.
    const { session, window } = tree([
      row({ windowId: '@7', windowIndex: 2, windowName: 'api' }),
      row({ paneId: '%1', paneIndex: 1, windowId: '@7', windowIndex: 2, windowName: 'api' }),
    ])
    const withId = rowMenu({ kind: 'window', session, window }, 'work')
    expect(ids(withId)).toEqual([
      'rename-window',
      'new-window',
      'split-right',
      'split-down',
      'zoom',
      'kill',
    ])
    expect(withId.at(-1)?.intent).toMatchObject({
      kind: 'kill',
      plan: { action: { verb: 'kill-window', window: '@7' } },
    })
    const rename = withId[0]
    if (rename.intent.kind !== 'prompt') throw new Error('rename should open a prompt')
    expect(rename.intent.prompt.build({ name: 'billing' })).toEqual({
      verb: 'rename-window',
      window: '@7',
      name: 'billing',
    })
    // It opens showing the window's live name, as the session rename does.
    expect(promptValues(rename.intent.prompt)).toEqual({ name: 'api' })
  })

  it('renames a session by its id, not by the group key tmux froze', () => {
    // The group key is the pre-rename name forever: `kill-session -t '=work3'`
    // fails on a session living happily as `api`. This is the whole reason
    // sessionId is on the wire.
    const { session } = tree([row({ groupKey: 'work3', sessionId: '$3', sessionName: 'api' })])
    const rename = rowMenu({ kind: 'session', session }, 'work3').find(
      (e) => e.id === 'rename-session',
    )
    if (rename?.intent.kind !== 'prompt') throw new Error('rename should open a prompt')
    expect(rename.intent.prompt.build({ name: 'billing' })).toEqual({
      verb: 'rename-session',
      session: '$3',
      name: 'billing',
    })
    // And it opens showing the live name, not the group key.
    expect(promptValues(rename.intent.prompt)).toEqual({ name: 'api' })
  })

  it('offers nothing at all on a group that is only this app is own sessions', () => {
    // The daemon refuses to rename or kill an @tmux_web_owned session -- killing one
    // would drop a live tab's socket for no reason the owner could understand
    // -- so every entry would be a refusal waiting to happen.
    const { session } = tree([row({ appOwned: true, groupKey: 'dead' })])
    expect(rowMenu({ kind: 'session', session }, 'work')).toEqual([])
  })

  it('creates a window in the session, from the pane in hand', () => {
    const { session, window, pane } = tree([row({ sessionId: '$1', paneId: '%3' })])
    // Only ids cross the wire: the daemon resolves #{pane_current_path} for %3
    // itself, which is what keeps working directories out of the browser.
    expect(newWindowAction({ kind: 'pane', session, window, pane })).toEqual({
      verb: 'new-window',
      session: '$1',
      name: '',
      fromPane: '%3',
    })
    // From a session row there is no pane in hand, and tmux picks the directory.
    expect(newWindowAction({ kind: 'session', session })).toEqual({
      verb: 'new-window',
      session: '$1',
      name: '',
      fromPane: '',
    })
  })

  it('names a pane through the label option, and lets an empty name clear it', () => {
    const { session, window, pane } = tree([row({ paneId: '%3', label: 'reviewer' })])
    const entry = rowMenu({ kind: 'pane', session, window, pane }, 'work').find(
      (e) => e.id === 'label-pane',
    )
    if (entry?.intent.kind !== 'prompt') throw new Error('label should open a prompt')
    const spec = entry.intent.prompt
    expect(promptValues(spec)).toEqual({ label: 'reviewer' })
    // Not required: erasing the name is the point of having one.
    expect(promptReady(spec, { label: '' })).toBe(true)
    expect(spec.build({ label: '' })).toEqual({ verb: 'label-pane', pane: '%3', label: '' })
  })
})

describe('rowTargetForWindow', () => {
  it('treats a single-pane window row as the pane it is', () => {
    const { session, window } = tree([row({ paneId: '%3' })])
    // The row already shows that pane's badge and that pane's agent mark, and
    // it has to be able to kill it: a window target would offer to kill the
    // window instead, leaving the pane itself with no kill on any row it has.
    expect(rowTargetForWindow(session, window)).toMatchObject({
      kind: 'pane',
      pane: { paneId: '%3' },
    })
    expect(ids(rowMenu(rowTargetForWindow(session, window), 'work'))).toContain('kill')
  })

  it('treats a split window row as the window', () => {
    const { session, window } = tree([
      row({ paneId: '%3', windowId: '@7' }),
      row({ paneId: '%4', paneIndex: 1, windowId: '@7' }),
    ])
    // Its panes have rows of their own underneath, and those carry the pane
    // menus.
    expect(rowTargetForWindow(session, window).kind).toBe('window')
    // And it keeps the window's id, or the one row that stands for the window
    // would be the one row that cannot rename or kill it.
    const entries = rowMenu(rowTargetForWindow(session, window), 'work')
    expect(ids(entries)).toContain('rename-window')
    expect(entries.at(-1)?.intent).toMatchObject({
      kind: 'kill',
      plan: { action: { verb: 'kill-window', window: '@7' } },
    })
  })
})

// --- planKill ---------------------------------------------------------------

describe('planKill', () => {
  it('names what dies, including the agent running in it', () => {
    const { session, window } = tree([
      row({ paneId: '%1', windowName: 'api', command: 'claude', agentState: 'working' }),
      row({ paneId: '%2', paneIndex: 1, windowName: 'api' }),
      row({ paneId: '%3', paneIndex: 2, windowName: 'api' }),
    ])
    const plan = planKill({ kind: 'window', session, window }, 'other')
    expect(plan?.title).toBe('kill window "api"')
    expect(plan?.detail).toBe('3 panes, one running claude')
  })

  it('counts a lone pane and a lone window in the singular', () => {
    const { session, window } = tree([row({ windowName: 'api' })])
    expect(planKill({ kind: 'window', session, window }, 'other')?.detail).toBe(
      '1 pane',
    )
    expect(planKill({ kind: 'session', session }, 'other')?.detail).toBe('1 window, 1 pane')
  })

  it('says which pane, and what is running in it', () => {
    const { session, window, pane } = tree([
      row({ paneId: '%5', paneIndex: 1, windowName: 'api', command: 'claude', agentState: 'idle' }),
      row({ paneId: '%6', paneIndex: 2, windowName: 'api' }),
    ])
    const plan = planKill({ kind: 'pane', session, window, pane }, 'other')
    expect(plan?.title).toBe('kill pane 1 of "api"')
    // The id and the command: a row 1.5s old is exactly the case where "pane 1"
    // alone is not enough to know which one is about to die.
    expect(plan?.detail).toBe('%5, running claude')
  })

  it('has nothing to kill for a window with no id, or an app-owned group', () => {
    const { session, window } = tree([row({ windowId: '' })])
    expect(planKill({ kind: 'window', session, window }, 'work')).toBeNull()
    const app = tree([row({ appOwned: true })])
    expect(planKill({ kind: 'session', session: app.session }, 'work')).toBeNull()
  })

  it('carries the id the daemon addresses, per kind', () => {
    const { session, window, pane } = tree([
      row({ sessionId: '$3', paneId: '%5', windowId: '@7', windowIndex: 2 }),
    ])
    expect(planKill({ kind: 'pane', session, window, pane }, 'work')?.action).toEqual({
      verb: 'kill-pane',
      pane: '%5',
    })
    // @N, never the index: tmux renumbers those, and the daemon rejects
    // anything that is not an @N anyway.
    expect(planKill({ kind: 'window', session, window }, 'work')?.action).toEqual({
      verb: 'kill-window',
      window: '@7',
    })
    expect(planKill({ kind: 'session', session }, 'work')?.action).toEqual({
      verb: 'kill-session',
      session: '$3',
    })
  })
})

describe('agentPhrase', () => {
  it('counts what the daemon called an agent, not what the command looks like', () => {
    const { window } = tree([
      // The daemon computes no state with no browser connected, and "" is not a
      // state. Matching the command against a list here would be a second copy
      // of the Go Agents list -- and the copy is what eventually disagrees.
      row({ paneId: '%1', command: 'claude', agentState: '' }),
      row({ paneId: '%2', paneIndex: 1, command: 'zsh', agentState: '' }),
    ])
    expect(agentPhrase(window.panes)).toBe('')
  })

  it('names one agent, and counts several', () => {
    const one = tree([row({ command: 'claude', agentState: 'idle' })])
    expect(agentPhrase(one.window.panes)).toBe('one running claude')

    const many = tree([
      row({ paneId: '%1', command: 'claude', agentState: 'working' }),
      row({ paneId: '%2', paneIndex: 1, command: 'opencode', agentState: 'blocked' }),
    ])
    expect(agentPhrase(many.window.panes)).toBe('2 running claude and opencode')
  })
})

// --- the cascade, and what it costs this tab --------------------------------

describe('killWarnings', () => {
  it('says nothing extra when a pane has siblings', () => {
    const { session, window, pane } = tree([
      row({ paneId: '%1' }),
      row({ paneId: '%2', paneIndex: 1 }),
    ])
    expect(killWarnings({ kind: 'pane', session, window, pane }, 'work')).toEqual([])
  })

  it('says the window closes with its last pane', () => {
    const { session, window, pane } = tree([
      row({ windowIndex: 0, windowName: 'api' }),
      row({ paneId: '%9', windowIndex: 1, windowName: 'notes' }),
    ])
    const warnings = killWarnings({ kind: 'pane', session, window, pane }, 'work')
    expect(warnings).toHaveLength(1)
    expect(warnings[0]).toContain('the window closes too')
  })

  it('says the session goes, and that this tab goes with it', () => {
    // Verified on a live server during design: killing a base session's last
    // window destroys the whole group, this app's own member included, which
    // drops the socket of every attached tab.
    const { session, window, pane } = tree([row({ windowName: 'api' })])
    const warnings = killWarnings({ kind: 'pane', session, window, pane }, 'work')
    expect(warnings.join(' ')).toContain('the session closes with it')
    expect(warnings.at(-1)).toContain('this tab disconnects')
  })

  it('warns about somebody else is tab when this one is elsewhere', () => {
    const { session, window } = tree([row({ groupKey: 'other', windowName: 'api' })])
    const warnings = killWarnings({ kind: 'window', session, window }, 'work')
    expect(warnings.at(-1)).toContain('Any tab attached to that session disconnects')
    expect(warnings.join(' ')).not.toContain('this tab disconnects')
  })

  it('does not claim a session kill disconnects the tab attached to it', () => {
    // It does not: this tab is attached to a throwaway session grouped with
    // that one, so the windows survive exactly as they do when the user's
    // session dies on the host -- the state the sidebar labels "(orphaned)".
    // What is lost is the ability to attach to that name again.
    const { session } = tree([row(), row({ paneId: '%9', windowIndex: 1 })])
    const warnings = killWarnings({ kind: 'session', session }, 'work')
    expect(warnings.join(' ')).not.toContain('disconnect')
    expect(warnings.join(' ')).toContain('nothing can attach to it by name again')
  })
})

// --- prompts ----------------------------------------------------------------

describe('prompts', () => {
  it('holds a required field to something non-blank', () => {
    const spec = newSessionPrompt()
    expect(promptReady(spec, { name: '', path: '' })).toBe(false)
    expect(promptReady(spec, { name: '   ', path: '' })).toBe(false)
    expect(promptReady(spec, { name: 'api', path: '' })).toBe(true)
  })

  it('leaves every other rule to the daemon', () => {
    // tmux refuses a leading "-", a ":", a "." and control bytes, and the
    // daemon caps the length. A second copy of those rules here is a copy that
    // drifts from the one that is enforced; the refusal comes back as a toast.
    const spec = newSessionPrompt()
    expect(promptReady(spec, { name: '-rf', path: '' })).toBe(true)
    expect(promptReady(spec, { name: 'a:b', path: '' })).toBe(true)
  })

  it('trims what it sends', () => {
    expect(newSessionPrompt().build({ name: ' api ', path: ' /tmp ' })).toEqual({
      verb: 'new-session',
      name: 'api',
      path: '/tmp',
    })
  })
})
