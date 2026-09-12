import { readFileSync } from 'node:fs'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import {
  HIDDEN_POLL_INTERVAL_MS,
  POLL_INTERVAL_MS,
  SNAPSHOT_URL,
  SnapshotFetchError,
  SnapshotPoller,
  SEEN_STORAGE_KEY,
  attachTarget,
  chooseSession,
  fetchSnapshot,
  findPane,
  groupRows,
  isDone,
  landedLabel,
  markSeen,
  mostUrgent,
  paneState,
  parseRememberedTarget,
  parseSnapshot,
  readSeen,
  resolveSession,
  seenKey,
  sessionState,
  viewedSeen,
  windowState,
  succeedPane,
  windowTarget,
  writeSeen,
} from './useSnapshot'
import type {
  PaneNode,
  SeenMap,
  SessionNode,
  SnapshotPayload,
  SnapshotRow,
  SnapshotState,
} from './useSnapshot'

// --- fixtures ---------------------------------------------------------------

function row(over: Partial<SnapshotRow> = {}): SnapshotRow {
  return {
    groupKey: 'work',
    sessionId: '$0',
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
    // What tmux gives a pane nothing has titled: the hostname.
    title: 'devbox',
    // The pane's working directory, as of the poll. On the wire and in the
    // tree; nothing renders it yet.
    path: '/srv/work',
    // A shell: the daemon computes no state for it.
    agentState: '',
    finishedAt: 0,
    // No integration reported anything for this pane, and nothing decided its
    // state -- which is what "" means in both fields, and the case every pane
    // that is not an agent is in.
    activity: '',
    stateSource: '',
    // Present as a key and undefined as a value: `question` is omitempty on the
    // Go side, and the contract check below compares KEYS, so a fixture that
    // simply left it out would report the field as missing from TypeScript.
    question: undefined,
    ...over,
  }
}

/**
 * A window whose panes were split and killed until the ids stopped matching the
 * layout. This is the real ordering the design verified against tmux: ids run
 * %0 %4 %2 %1 while the panes on screen are 0 1 2 3.
 */
const scrambled: SnapshotRow[] = [
  row({ paneId: '%0', paneIndex: 0, command: 'zsh', paneActive: false }),
  row({ paneId: '%4', paneIndex: 1, command: 'claude', paneActive: true }),
  row({ paneId: '%2', paneIndex: 2, command: 'vim', paneActive: false }),
  row({ paneId: '%1', paneIndex: 3, command: 'npm', paneActive: false }),
]

function ok(payload: Partial<SnapshotPayload> & { panes: SnapshotRow[] }): SnapshotPayload {
  return { serverStart: '1757500000', stale: false, error: null, ...payload }
}

// --- the contract with the Go daemon ----------------------------------------
//
// Same approach as transport.test.ts: the wire shape is implemented twice, in
// two languages, so the constants are pinned against the Go source rather than
// against a copy of themselves. A Go-side change that this file does not follow
// then fails here instead of in a browser.

const goSource = (path: string) =>
  readFileSync(new URL(`../../../${path}`, import.meta.url), 'utf8')

describe('contract with the daemon', () => {
  it('polls at the interval the daemon refreshes the snapshot at', () => {
    const m = goSource('internal/front/server.go').match(
      /DefaultPollInterval\s*=\s*(\d+)\s*\*\s*time\.Millisecond/,
    )
    if (!m) throw new Error('DefaultPollInterval not found in internal/front/server.go')
    // Faster only re-reads the same cached bytes; slower adds lag to a number
    // the design already accepts.
    expect(POLL_INTERVAL_MS).toBe(Number(m[1]))
  })

  it('asks for the route the daemon serves', () => {
    expect(goSource('internal/front/server.go')).toContain(`"GET ${SNAPSHOT_URL}"`)
  })

  it('uses the json names tmux.Row marshals', () => {
    const struct = goSource('internal/tmux/snapshot.go').match(/type Row struct \{([\s\S]*?)\n\}/)
    if (!struct) throw new Error('type Row not found in internal/tmux/snapshot.go')
    // The name is everything before the first comma: `question` is omitempty,
    // and a pattern that stopped at the quote would simply not see it -- which
    // reads as "TypeScript is missing a field" rather than as a broken check.
    const tags = [...struct[1].matchAll(/json:"([^"]+)"/g)].map((m) => m[1].split(',')[0])
    // A literal, never `Object.keys(row()).length`: the count is here to make a
    // field added on one side only fail, and a count derived from the
    // TypeScript side would agree with itself forever.
    expect(tags).toHaveLength(19)
    expect(Object.keys(row()).sort()).toEqual(tags.sort())
  })
})

// --- parseSnapshot ----------------------------------------------------------

describe('parseSnapshot', () => {
  it('reads null panes as an empty snapshot, not an error', () => {
    // The Go side returns nil for both "no panes" and "no tmux server", and
    // both are answers rather than faults. The daemon rewrites nil to [] today;
    // this is the case that must not become a thrown error if it stops.
    const empty = { panes: [], serverStart: '', stale: false, error: null }
    expect(parseSnapshot({ panes: null })).toEqual(empty)
    expect(parseSnapshot({})).toEqual(empty)
  })

  it('keeps the rows and the stale flag', () => {
    const parsed = parseSnapshot({ panes: scrambled, stale: true, error: 'tmux exited' })
    expect(parsed.panes).toEqual(scrambled)
    expect(parsed.stale).toBe(true)
    expect(parsed.error).toBe('tmux exited')
  })

  it('treats an absent or empty error string as no error', () => {
    expect(parseSnapshot({ panes: [], error: '' }).error).toBeNull()
    expect(parseSnapshot({ panes: [], stale: 'yes' }).stale).toBe(false)
  })

  it('refuses a body where an array belongs', () => {
    expect(() => parseSnapshot({ panes: 'nope' })).toThrow(SnapshotFetchError)
    expect(() => parseSnapshot(null)).toThrow(SnapshotFetchError)
    expect(() => parseSnapshot('[]')).toThrow(SnapshotFetchError)
  })
})

// --- fetchSnapshot ----------------------------------------------------------

function response(status: number, body: unknown, json = true): Response {
  return {
    ok: status >= 200 && status < 300,
    status,
    json: async () => {
      if (!json) throw new SyntaxError('Unexpected token u in JSON')
      return body
    },
  } as unknown as Response
}

describe('fetchSnapshot', () => {
  it('requests the snapshot endpoint with the abort signal', async () => {
    const controller = new AbortController()
    const fetchImpl = vi.fn(async (_url: string, _init: RequestInit) =>
      response(200, { panes: scrambled }),
    )
    const payload = await fetchSnapshot(controller.signal, fetchImpl)

    expect(fetchImpl).toHaveBeenCalledTimes(1)
    expect(fetchImpl.mock.calls[0][0]).toBe('/api/snapshot')
    expect(fetchImpl.mock.calls[0][1].signal).toBe(controller.signal)
    expect(payload.panes).toHaveLength(4)
  })

  it("surfaces the daemon's own message on a 503", async () => {
    const fetchImpl = async () =>
      response(503, { error: 'cannot read the tmux server: exit status 1' })
    await expect(fetchSnapshot(new AbortController().signal, fetchImpl)).rejects.toMatchObject({
      status: 503,
      message: 'cannot read the tmux server: exit status 1',
      unauthorized: false,
    })
  })

  it('flags 401 and 403 as unauthorized even with a plain-text body', async () => {
    for (const status of [401, 403]) {
      const fetchImpl = async () => response(status, null, false)
      const err = await fetchSnapshot(new AbortController().signal, fetchImpl).catch((e) => e)
      expect(err).toBeInstanceOf(SnapshotFetchError)
      expect(err.unauthorized).toBe(true)
      expect(err.message).toContain(String(status))
    }
  })
})

// --- grouping ---------------------------------------------------------------

describe('groupRows', () => {
  it('keeps the order the daemon sent, not the order of the pane ids', () => {
    const [session] = groupRows(scrambled)
    expect(session.windows[0].panes.map((p) => p.paneId)).toEqual(['%0', '%4', '%2', '%1'])
    expect(session.windows[0].panes.map((p) => p.paneIndex)).toEqual([0, 1, 2, 3])
  })

  it('carries the title and label onto the pane, since the badge is made of them', () => {
    const [session] = groupRows([row({ title: '✳ Categorización', label: 'prod db' })])
    expect(session.windows[0].panes[0]).toMatchObject({
      command: 'zsh',
      title: '✳ Categorización',
      label: 'prod db',
    })
  })

  // Nothing renders the path yet, which is exactly why it needs a test of its
  // own: a field the tree silently drops looks fine in every screenshot, and
  // the first thing to want it (the git context the roadmap prices per pane)
  // would find the tree carrying `undefined` and go back to asking tmux.
  it('carries the working directory onto the pane, though nothing renders it', () => {
    const [session] = groupRows([row({ path: '/srv/api' })])
    expect(session.windows[0].panes[0].path).toBe('/srv/api')
  })

  it('carries the agent fields onto the pane, since the dot is made of them', () => {
    const question = { text: 'Run `rm -rf build`?', choices: ['Yes', 'No'] }
    const [session] = groupRows([
      row({
        command: 'claude',
        agentState: 'blocked',
        finishedAt: 900,
        question,
      }),
    ])
    expect(session.windows[0].panes[0]).toMatchObject({
      agentState: 'blocked',
      finishedAt: 900,
      question,
    })
  })

  it('names a session by its live name, never by the group key', () => {
    // tmux freezes session_group at the name the group was created under, so
    // after `rename-session work3 -> api` every row still carries group=work3.
    // A sidebar labelled on the group shows the old name forever, which is what
    // made rename from the browser look like it did nothing.
    const [session] = groupRows([row({ groupKey: 'work3', sessionName: 'api' })])
    expect(session.name).toBe('api')
    // And the group key is still the identity: the React key, `?session=`, and
    // what a click carries.
    expect(session.key).toBe('work3')
  })

  it('takes the name from the session the user made, not from a throwaway', () => {
    // The app's own sessions are grouped with the user's and have generated
    // names; `list-panes -a` reports them in whatever order tmux likes.
    const [session] = groupRows([
      row({
        groupKey: 'work3',
        sessionName: 'tmux-web-1',
        paneId: '%0',
        appOwned: true,
      }),
      row({
        groupKey: 'work3',
        sessionName: 'api',
        paneId: '%1',
        appOwned: false,
      }),
      row({
        groupKey: 'work3',
        sessionName: 'tmux-web-2',
        paneId: '%2',
        appOwned: true,
      }),
    ])
    expect(session.name).toBe('api')
  })

  it('carries the id of the session the user made, from the row that named it', () => {
    // Every session-level management call targets this: rename, kill, and the
    // session a new window goes in. It has to be the *user's* session -- the
    // daemon refuses to rename or kill an @tmux_web_owned one -- and it has to come
    // from the same row as the name, or a rename dialog titled "api" sends the
    // id of a throwaway.
    const [session] = groupRows([
      row({ groupKey: 'work3', sessionId: '$9', sessionName: 'tmux-web-1', paneId: '%0', appOwned: true }),
      row({ groupKey: 'work3', sessionId: '$3', sessionName: 'api', paneId: '%1', appOwned: false }),
      row({ groupKey: 'work3', sessionId: '$8', sessionName: 'tmux-web-2', paneId: '%2', appOwned: true }),
    ])
    expect(session.sessionId).toBe('$3')
    expect(session.name).toBe('api')
  })

  it('keeps the first live session, when the user has grouped two of their own', () => {
    // `tmux new -t work` puts a second real session in the same group. A label
    // that flipped between two live names every poll would be worse than one
    // that picks the first and stays there -- and the id has to stay with it,
    // or the dialog renames a session the row was never about.
    const [session] = groupRows([
      row({ groupKey: 'work', sessionId: '$1', sessionName: 'work', paneId: '%0' }),
      row({ groupKey: 'work', sessionId: '$5', sessionName: 'work-two', paneId: '%1' }),
    ])
    expect(session.name).toBe('work')
    expect(session.sessionId).toBe('$1')
  })

  it('falls back to the first row is id when every session is app-owned', () => {
    const [session] = groupRows([
      row({ groupKey: 'work3', sessionId: '$9', sessionName: 'tmux-web-1', appOwned: true }),
    ])
    // Nothing here is addressable -- the daemon refuses app sessions -- but an
    // id is better than "", which tmux resolves to "whatever is current".
    expect(session.sessionId).toBe('$9')
  })

  it('still shows a live name when every session in the group is app-owned', () => {
    // The user killed the namesake under an attached tab. The group key is the
    // dead session's name; the throwaway's own name is at least a live one.
    const [session] = groupRows([
      row({ groupKey: 'work3', sessionName: 'tmux-web-1', appOwned: true }),
    ])
    expect(session.name).toBe('tmux-web-1')
  })

  it('falls back to the group key when a row carries no name at all', () => {
    const [session] = groupRows([row({ groupKey: 'work3', sessionName: '' })])
    expect(session.name).toBe('work3')
  })

  it('keeps sessions and windows in arrival order', () => {
    const groups = groupRows([
      row({ groupKey: 'work', windowIndex: 2, windowName: 'api' }),
      row({ groupKey: 'work', windowIndex: 0, windowName: 'shell', paneId: '%9' }),
      row({ groupKey: 'admin', windowIndex: 0, windowName: 'logs', paneId: '%7' }),
    ])
    expect(groups.map((g) => g.key)).toEqual(['work', 'admin'])
    expect(groups[0].windows.map((w) => w.index)).toEqual([2, 0])
  })

  it('splits panes into windows, and windows carry more than one only when tmux does', () => {
    const groups = groupRows([
      row({ windowIndex: 0, paneId: '%0' }),
      row({ windowIndex: 1, paneId: '%1', paneIndex: 0, windowName: 'api' }),
      row({ windowIndex: 1, paneId: '%2', paneIndex: 1, windowName: 'api' }),
    ])
    expect(groups[0].windows.map((w) => w.panes.length)).toEqual([1, 2])
    expect(groups[0].windows[1].name).toBe('api')
  })

  it('merges rows for one window even if they do not arrive together', () => {
    const groups = groupRows([
      row({ windowIndex: 1, paneId: '%1', paneIndex: 0 }),
      row({ windowIndex: 2, paneId: '%5', paneIndex: 0 }),
      row({ windowIndex: 1, paneId: '%2', paneIndex: 1 }),
    ])
    expect(groups[0].windows).toHaveLength(2)
    expect(groups[0].windows[0].panes.map((p) => p.paneId)).toEqual(['%1', '%2'])
  })

  it('marks a group orphaned only when every surviving row is app-owned', () => {
    expect(groupRows([row({ appOwned: true })])[0].appOnly).toBe(true)
    expect(
      groupRows([row({ appOwned: true }), row({ paneId: '%1', appOwned: false })])[0].appOnly,
    ).toBe(false)
  })

  it('gives windows keys that are unique across sessions', () => {
    const groups = groupRows([
      row({ groupKey: 'a', windowIndex: 0, paneId: '%0' }),
      row({ groupKey: 'b', windowIndex: 0, paneId: '%1' }),
    ])
    expect(groups[0].windows[0].key).not.toBe(groups[1].windows[0].key)
  })

  it('carries the window id every window operation targets', () => {
    // Without it the sidebar cannot name a window at all: PATCH and DELETE
    // /api/windows/{id} validate an @N, so rename and kill are unreachable --
    // which is why `rowMenu` leaves them out when this is "".
    const [session] = groupRows([row({ windowId: '@7', windowIndex: 2 })])
    expect(session.windows[0].id).toBe('@7')
  })

  it('carries the agent report and the state is source onto the pane node', () => {
    // The tree is what the sidebar renders, so a field the wire carries and
    // `groupRows` drops is a field nothing can ever show. Asserted on the node,
    // from a row that differs from the default in both fields.
    const [session] = groupRows([
      row({ command: 'claude', agentState: 'working', activity: 'edit report.go', stateSource: 'event' }),
    ])
    const pane = session.windows[0].panes[0]
    expect(pane.activity).toBe('edit report.go')
    expect(pane.stateSource).toBe('event')
  })

  it('keys a window on its id, so renumbering it does not replace its row', () => {
    // `move-window` renumbers, and a kill lets a later window take the index
    // that was freed. A key built on the index is one two different windows
    // wear one after the other; @N is stable for the window's life.
    const before = groupRows([
      row({ windowId: '@7', windowIndex: 2, windowName: 'api' }),
      row({ windowId: '@3', windowIndex: 3, windowName: 'notes', paneId: '%1' }),
    ])
    expect(before[0].windows.map((w) => w.key)).toEqual(['work:@7', 'work:@3'])

    const after = groupRows([
      row({ windowId: '@7', windowIndex: 0, windowName: 'api' }),
      row({ windowId: '@3', windowIndex: 1, windowName: 'notes', paneId: '%1' }),
    ])
    expect(after[0].windows.map((w) => w.key)).toEqual(['work:@7', 'work:@3'])

    // And the converse: a window killed and replaced at the same index is not
    // the same row, however alike the two look.
    const replaced = groupRows([row({ windowId: '@9', windowIndex: 2, windowName: 'api' })])
    expect(replaced[0].windows[0].key).not.toBe(before[0].windows[0].key)
  })

  it('falls back to the index when the wire carried no id at all', () => {
    // A daemon too old to send `windowId`. Keying every row on "" would fold a
    // whole session into one window and lose every row but the first; the
    // fallback keeps the tree, and the empty `id` is what keeps rename and kill
    // off a window nothing can address.
    const [session] = groupRows([
      row({ windowId: '', windowIndex: 0, windowName: 'shell' }),
      row({ windowId: '', windowIndex: 1, windowName: 'api', paneId: '%1' }),
    ])
    expect(session.windows.map((w) => w.name)).toEqual(['shell', 'api'])
    expect(session.windows.map((w) => w.key)).toEqual(['work:0', 'work:1'])
    expect(session.windows.map((w) => w.id)).toEqual(['', ''])
  })
})

describe('findPane', () => {
  const groups = groupRows(scrambled)

  it('locates a pane and its window', () => {
    const at = findPane(groups, '%2')
    expect(at?.pane.command).toBe('vim')
    expect(at?.window.index).toBe(0)
    expect(at?.session.key).toBe('work')
  })

  it('is null for a pane that is not in the snapshot, and for no pane', () => {
    expect(findPane(groups, '%99')).toBeNull()
    expect(findPane(groups, null)).toBeNull()
  })
})

describe('succeedPane', () => {
  // Two windows in one session, three panes: an editor in `1: api` beside a
  // shell, and a shell alone in `0: shell`.
  const live = [
    row({ windowIndex: 0, windowName: 'shell', paneId: '%0', paneActive: true }),
    row({ windowIndex: 1, windowName: 'api', paneId: '%1', paneActive: true, command: 'sh' }),
    row({
      windowIndex: 1,
      windowName: 'api',
      paneId: '%75',
      paneIndex: 1,
      paneActive: false,
      command: 'vim',
    }),
  ]
  const gone = (rows: SnapshotRow[], paneId: string) => {
    const at = findPane(groupRows(rows), paneId)
    if (!at) throw new Error(`fixture does not contain ${paneId}`)
    return at
  }

  it('lands on the dead pane\'s own window when the window survived it', () => {
    const was = gone(live, '%75')
    const after = groupRows(live.filter((r) => r.paneId !== '%75'))
    const to = succeedPane(after, was, 'work')
    expect(to?.pane.paneId).toBe('%1')
    // The point of preferring the window: the user closed an editor in `api`
    // and is still in `api`, not thrown back to window 0.
    expect(to?.window.name).toBe('api')
  })

  it('falls back to the first window when the whole window went with the pane', () => {
    const was = gone(live, '%1')
    // `%1` was the last pane in `api`, so tmux took the window too.
    const after = groupRows(live.filter((r) => r.windowIndex !== 1))
    const to = succeedPane(after, was, 'work')
    expect(to?.pane.paneId).toBe('%0')
    expect(to?.window.name).toBe('shell')
  })

  it('prefers the surviving window over the first one', () => {
    // Guards the order specifically: window 0 is present and would be a
    // perfectly good answer, so a rule that simply took `windows[0]` passes
    // every test above and fails this one.
    const was = gone(live, '%75')
    const after = groupRows(live.filter((r) => r.paneId !== '%75'))
    expect(succeedPane(after, was, 'work')?.window.index).toBe(1)
  })

  it('follows the window active pane, not the first in layout order', () => {
    const rows = [
      row({ windowIndex: 0, windowName: 'shell', paneId: '%0', paneActive: true }),
      row({ windowIndex: 1, windowName: 'api', paneId: '%1', paneActive: false }),
      row({ windowIndex: 1, windowName: 'api', paneId: '%2', paneIndex: 1, paneActive: true }),
      row({ windowIndex: 1, windowName: 'api', paneId: '%75', paneIndex: 2, paneActive: false }),
    ]
    const was = gone(rows, '%75')
    const after = groupRows(rows.filter((r) => r.paneId !== '%75'))
    // %1 is first in layout order; %2 is where tmux actually moved the client.
    expect(succeedPane(after, was, 'work')?.pane.paneId).toBe('%2')
  })

  // --- when the session went too ---------------------------------------------
  //
  // Closing the last tab of a session destroys it. The tab does not stay
  // disconnected -- `resolveSession` has already moved it to another session and
  // the terminal is showing a live pane of that one -- so the successor has to
  // be able to cross a session boundary, or the app ends up pointing into a
  // session that no longer exists with nothing highlighted anywhere.

  // Two windows in a second session, the first of them split, so "the first
  // window" and "the window's active pane" are both real choices here.
  const elsewhere = [
    row({
      groupKey: 'other',
      sessionName: 'other',
      windowId: '@10',
      windowIndex: 0,
      windowName: 'work',
      paneId: '%10',
      paneActive: false,
      command: 'sh',
    }),
    row({
      groupKey: 'other',
      sessionName: 'other',
      windowId: '@10',
      windowIndex: 0,
      windowName: 'work',
      paneId: '%11',
      paneIndex: 1,
      paneActive: true,
      command: 'claude',
    }),
    row({
      groupKey: 'other',
      sessionName: 'other',
      windowId: '@11',
      windowIndex: 1,
      windowName: 'logs',
      paneId: '%12',
    }),
  ]

  it('moves to the session the tab is attached to now when its own session went', () => {
    const was = gone(live, '%75')
    const to = succeedPane(groupRows(elsewhere), was, 'other')
    expect(to?.session.key).toBe('other')
    // The first window of it, and that window's active pane -- not %10, which
    // is merely first in layout order.
    expect(to?.window.name).toBe('work')
    expect(to?.pane.paneId).toBe('%11')
  })

  it('is null when the session went and there is nowhere the tab is attached', () => {
    const was = gone(live, '%75')
    expect(succeedPane(groupRows(elsewhere), was, null)).toBeNull()
  })

  it('is null when the tab is pinned to a session the snapshot does not have', () => {
    // `?session=api` carries a hand-typed name, which is not a group key the
    // snapshot knows. A pinned tab must stay pinned rather than wander.
    const was = gone(live, '%75')
    expect(succeedPane(groupRows(elsewhere), was, 'api')).toBeNull()
  })

  it('looks for the surviving window only in the pane\'s own session', () => {
    // The fallback session's windows are not candidates for "the window this
    // pane was in" -- that sentence is about one session. tmux allocates `@N`
    // per server so it would never actually offer a collision, but the rule is
    // not allowed to be right only because of that: here the other session's
    // second window wears `@1`, the id `%75`'s window had, and the answer is
    // still the *first* window of the session the tab is now attached to.
    const was = gone(live, '%75')
    const collides = elsewhere.map((r) =>
      r.windowIndex === 1 ? { ...r, windowId: '@1' } : r,
    )
    const to = succeedPane(groupRows(collides), was, 'other')
    expect(to?.window.name).toBe('work')
    expect(to?.pane.paneId).toBe('%11')
  })

  it('prefers the dead pane\'s own session over the attached one', () => {
    // Guards the order. `attached` is usually the *same* session, so a rule
    // that tried it first would pass every other test here and would silently
    // throw the user back to window 0 on every ordinary pane death.
    const was = gone(live, '%75')
    const after = groupRows([...live.filter((r) => r.paneId !== '%75'), ...elsewhere])
    const to = succeedPane(after, was, 'other')
    expect(to?.session.key).toBe('work')
    expect(to?.pane.paneId).toBe('%1')
  })

  it('is null for an empty snapshot, so a failed poll cannot move a tab', () => {
    // The daemon reports its own failed tmux poll as an error with no rows.
    // Treating that as "every pane died" would yank every tab off its pane.
    expect(succeedPane([], gone(live, '%75'), 'work')).toBeNull()
  })

  it('never hands back the pane it was told is gone', () => {
    // A caller that passed a pane still in the snapshot must not be told to
    // re-select it: the caller would do so on every poll, forever.
    const was = gone(live, '%1')
    expect(succeedPane(groupRows(live.filter((r) => r.windowIndex === 1)), was, 'work')).toBeNull()
  })

  it('follows the window id, not its index, across a renumber', () => {
    // tmux reuses window indices: with `renumber-windows on`, killing the pane
    // that took a window with it shifts every window after it down one. A rule
    // that remembered "window 1" would then land on whatever window *became*
    // number 1, which is a different window that merely inherited the number.
    const before = [
      row({ windowId: '@9', windowIndex: 0, windowName: 'shell', paneId: '%0' }),
      row({ windowId: '@7', windowIndex: 1, windowName: 'api', paneId: '%1' }),
      row({
        windowId: '@7',
        windowIndex: 1,
        windowName: 'api',
        paneId: '%75',
        paneIndex: 1,
        paneActive: false,
        command: 'vim',
      }),
    ]
    const was = gone(before, '%75')
    // %75 closed, and the windows were renumbered the other way round.
    const after = groupRows([
      row({ windowId: '@7', windowIndex: 0, windowName: 'api', paneId: '%1' }),
      row({ windowId: '@9', windowIndex: 1, windowName: 'shell', paneId: '%0' }),
    ])
    const to = succeedPane(after, was, 'work')
    expect(to?.window.id).toBe('@7')
    expect(to?.pane.paneId).toBe('%1')
  })

  it('matches the window by key when the daemon sent no window id', () => {
    const old = live.map((r) => ({ ...r, windowId: '' }))
    const was = gone(old, '%75')
    const after = groupRows(old.filter((r) => r.paneId !== '%75'))
    expect(succeedPane(after, was, 'work')?.pane.paneId).toBe('%1')
  })
})

describe('landedLabel', () => {
  const at = (rows: SnapshotRow[], paneId: string) => {
    const found = findPane(groupRows(rows), paneId)
    if (!found) throw new Error(`fixture does not contain ${paneId}`)
    return found
  }
  const here = [
    row({ windowIndex: 0, windowName: 'shell', paneId: '%0' }),
    row({ windowIndex: 2, windowName: 'api', paneId: '%1', command: 'vim' }),
  ]
  const away = [row({ groupKey: 'other', sessionName: 'other', paneId: '%9', command: 'claude' })]

  it('names the window and the command for a move inside one session', () => {
    expect(landedLabel(at(here, '%1'), at(here, '%0'))).toBe('2: api \u203a vim')
  })

  it('puts the session in front when the move crossed one', () => {
    // The session changed only because the old one was destroyed, and it is the
    // part of "where am I" the breadcrumb alone would not tell them.
    expect(landedLabel(at(away, '%9'), at(here, '%0'))).toBe('other \u203a 0: shell \u203a claude')
  })
})

describe('windowTarget', () => {
  it('prefers the window active pane over the first one', () => {
    expect(windowTarget(groupRows(scrambled)[0].windows[0])).toBe('%4')
  })

  it('falls back to the first pane when tmux reports none active', () => {
    const rows = scrambled.map((r) => ({ ...r, paneActive: false }))
    expect(windowTarget(groupRows(rows)[0].windows[0])).toBe('%0')
  })
})

describe('chooseSession', () => {
  const groups = groupRows([
    row({ groupKey: 'orphan', appOwned: true, paneId: '%8' }),
    row({ groupKey: 'work', paneId: '%0' }),
    row({ groupKey: 'admin', paneId: '%1' }),
  ])

  it('keeps a preferred session that still exists', () => {
    expect(chooseSession(groups, 'admin')).toBe('admin')
  })

  it('skips a group whose own session is gone, since the socket would 404', () => {
    expect(chooseSession(groups, 'vanished')).toBe('work')
    expect(chooseSession(groups, null)).toBe('work')
  })

  it('is null when there is nothing attachable', () => {
    expect(chooseSession([], 'work')).toBeNull()
    expect(chooseSession(groupRows([row({ groupKey: 'orphan', appOwned: true })]))).toBeNull()
  })
})

describe('resolveSession', () => {
  const groups = groupRows([
    row({ groupKey: 'work', paneId: '%0' }),
    row({ groupKey: 'admin', paneId: '%1' }),
  ])
  const choice = { picked: null as string | null, forced: null as string | null, loaded: true }

  it('honours ?session= even when the snapshot has never heard of it', () => {
    // The escape hatch: a failing poll must not stop a user attaching by hand.
    expect(resolveSession(groups, { ...choice, forced: 'ghost', picked: 'work' })).toBe('ghost')
    expect(resolveSession([], { ...choice, forced: 'ghost' })).toBe('ghost')
  })

  it('attaches to the remembered session before the first poll answers', () => {
    // Otherwise a reload waits out a poll before it can show anything.
    expect(resolveSession([], { ...choice, picked: 'work', loaded: false })).toBe('work')
  })

  it('keeps the remembered session once the snapshot confirms it', () => {
    expect(resolveSession(groups, { ...choice, picked: 'admin' })).toBe('admin')
  })

  it('moves off a session a loaded snapshot no longer has', () => {
    // Staying would leave the terminal reconnecting against the daemon's 404
    // forever, with no way back short of editing the URL.
    expect(resolveSession(groups, { ...choice, picked: 'killed' })).toBe('work')
  })

  it('picks the first attachable group when the tab has no preference', () => {
    expect(resolveSession(groups, choice)).toBe('work')
    expect(resolveSession([], choice)).toBeNull()
  })
})

describe('attachTarget', () => {
  // The shape the bug needs: a session renamed *after* its group existed, which
  // is every session this app has ever attached to. tmux froze `session_group`
  // at "work3" while the session lives on as "api" under id "$4".
  const renamed = groupRows([row({ groupKey: 'work3', sessionName: 'api', sessionId: '$4' })])

  it('addresses a renamed session by its id, never by the frozen group key', () => {
    // Identity and address are two different answers, and this is the seam
    // between them: the tab still *thinks* in group keys -- that is the React
    // key and what a sidebar click carries -- but what goes on the wire is the
    // id. `has-session -t =work3` answers "can't find session: work3", which
    // reached the owner as `Session "work3" is gone` on every pane he clicked.
    const session = resolveSession(renamed, { picked: 'work3', forced: null, loaded: true })
    expect(session).toBe('work3')
    expect(attachTarget(renamed, session)).toBe('$4')
  })

  it('passes a hand-typed ?session= through, because a person types a name', () => {
    // `?session=` is a user-facing parameter; the daemon accepts a name as well
    // as an id, and a name it has never heard of is the documented escape hatch
    // for a tab attaching while the poll is failing.
    expect(attachTarget(renamed, 'api')).toBe('api')
    expect(attachTarget(renamed, 'ghost')).toBe('ghost')
  })

  it('falls back to the key when the rows carry no id', () => {
    // A daemon too old to send `sessionId`. The group key is the wrong address
    // only after a rename, so it is a better last resort than attaching to
    // nothing at all.
    expect(attachTarget(groupRows([row({ groupKey: 'work', sessionId: '' })]), 'work')).toBe('work')
  })

  it('has nothing to address when no session resolved', () => {
    expect(attachTarget(renamed, null)).toBeNull()
  })

  it('reaches an orphaned group through the member it has left', () => {
    // The user's own session died and only this app's throwaway is holding the
    // group's windows open. `chooseSession` still refuses to land a fresh tab
    // there and the sidebar still marks the row orphaned -- but a tab that asks
    // for it by name gets the id of what is actually there, which is a group
    // full of running agents rather than a 404. The id is a real session in the
    // right group, which is all an attach needs.
    const orphan = groupRows([
      row({ groupKey: 'work', sessionName: '_web-abcd', sessionId: '$9', appOwned: true }),
    ])
    expect(attachTarget(orphan, 'work')).toBe('$9')
  })

  // --- the remembered address ------------------------------------------------

  it('uses the address the tab remembers while the snapshot has nothing to say', () => {
    // The whole point: on a reload the tab knows the group key it was on, and
    // the key is not an address. Without this it attaches by name, the first
    // poll turns the name into "$4", and the socket is thrown away and reopened.
    expect(attachTarget([], 'work3', { key: 'work3', id: '$4' })).toBe('$4')
  })

  it('prefers the snapshot to the memory, always', () => {
    // The remembered id is a tab-lifetime old and tmux restarts its ids at $0
    // with the server, so it can name a different session entirely. It is only
    // ever the answer while nothing better exists; the moment a poll lands, the
    // poll is right.
    expect(attachTarget(renamed, 'work3', { key: 'work3', id: '$99' })).toBe('$4')
  })

  it('ignores a memory of a different session', () => {
    // `?session=api` typed by hand on a tab that last sat on "work3". The pin
    // means that session and no other, so a remembered address filed under
    // another key must not become the target.
    expect(attachTarget([], 'api', { key: 'work3', id: '$4' })).toBe('api')
  })

  it('ignores a memory that is not a session id', () => {
    // sessionStorage is not a trusted input: it survives reloads, and anything
    // that reaches this return value goes into `/ws?session=` -- where tmux
    // resolves a great many strings to "whatever is current".
    for (const bad of ['work', '$', '', '$4;kill-server', '4']) {
      expect(attachTarget([], 'work3', { key: 'work3', id: bad })).toBe('work3')
    }
  })

  it('has nothing to address when no session resolved, memory or not', () => {
    expect(attachTarget([], null, { key: 'work3', id: '$4' })).toBeNull()
  })
})

describe('parseRememberedTarget', () => {
  it('reads back what the tab wrote', () => {
    expect(parseRememberedTarget('{"key":"work3","id":"$4"}')).toEqual({ key: 'work3', id: '$4' })
  })

  it('is a miss for anything that is not one', () => {
    // sessionStorage survives reloads and is editable, and this runs on the
    // app's very first render -- so every one of these has to be a null, not a
    // throw and not a half-built object that reaches `/ws?session=`.
    for (const bad of [
      null,
      '',
      'not json',
      'null',
      '"work3"',
      '42',
      '[]',
      '{}',
      '{"key":"work3"}',
      '{"id":"$4"}',
      '{"key":"","id":"$4"}',
      '{"key":"work3","id":""}',
      '{"key":1,"id":"$4"}',
      '{"key":"work3","id":4}',
    ]) {
      expect(parseRememberedTarget(bad)).toBeNull()
    }
  })

  it('keeps only the two fields it knows', () => {
    // Whatever else is in there is not carried forward: the object goes into
    // render state, and a field nothing validated is a field something later
    // reads.
    expect(parseRememberedTarget('{"key":"work3","id":"$4","pane":"%9"}')).toEqual({
      key: 'work3',
      id: '$4',
    })
  })
})

// --- what one page load costs ------------------------------------------------

/**
 * Every address a page load hands the terminal, in order.
 *
 * `<Terminal>` opens one socket per address: the effect that builds the
 * `TerminalSession` is keyed on the url, so a second distinct address is a
 * second socket -- and on the daemon that is a second `has-session`, a second
 * `ptybridge.Open`, and a `_web-*` tmux session created and destroyed. Every
 * behavioural assertion in this suite stays green when that happens, because
 * both addresses name the same session and the tab ends up in the right place
 * either way. Counting is the only assertion that can see it.
 */
function addressesDuringAPageLoad(
  remembered: { key: string; id: string } | null,
  groups: readonly SessionNode[],
  forced: string | null = null,
): string[] {
  const picked = remembered?.key ?? null
  const seen: string[] = []
  // Render one: the tab has its memory and no snapshot. Render two: the first
  // poll has landed. Those are the two renders a reload produces.
  for (const loaded of [false, true]) {
    const g = loaded ? groups : []
    const session = resolveSession(g, { picked, forced, loaded })
    const target = attachTarget(g, session, remembered)
    if (target !== null && target !== seen.at(-1)) seen.push(target)
  }
  return seen
}

describe('one page load, one socket', () => {
  const live = groupRows([row({ groupKey: 'work3', sessionName: 'api', sessionId: '$4' })])

  it('hands the terminal one address across the first poll', () => {
    const got = addressesDuringAPageLoad({ key: 'work3', id: '$4' }, live)
    expect(got).toEqual(['$4'])
  })

  it('hands the terminal one address for a hand-typed ?session= too', () => {
    // A person types a name, and a name is what the daemon gets -- before and
    // after the poll, so the socket is not rebuilt underneath it. The pin also
    // survives the poll, which is what makes both renders agree.
    expect(addressesDuringAPageLoad(null, live, 'api')).toEqual(['api'])
  })

  it('remembers where the pane it was looking at is filed', () => {
    // The pane memory is keyed on the address (`paneStorageKey`), so a second
    // address is also a second `sessionStorage` key -- the tab re-selects
    // nothing on the reload it was supposed to land on, and leaves a key behind.
    const got = addressesDuringAPageLoad({ key: 'work3', id: '$4' }, live)
    expect(new Set(got).size).toBe(1)
  })
})

// --- the polling loop -------------------------------------------------------

/** Drives a poller with a fetcher the test resolves by hand. */
function harness(opts: { hidden?: () => boolean } = {}) {
  const states: SnapshotState[] = []
  const calls: { resolve: (p: SnapshotPayload) => void; reject: (e: unknown) => void }[] = []
  let queued: (() => SnapshotPayload | Promise<SnapshotPayload>) | null = null

  const poller = new SnapshotPoller({
    onState: (s) => states.push(s),
    isHidden: opts.hidden ?? (() => false),
    fetcher: () => {
      if (queued) return Promise.resolve(queued())
      return new Promise<SnapshotPayload>((resolve, reject) => calls.push({ resolve, reject }))
    },
  })

  return {
    poller,
    states,
    calls,
    /** Every later poll answers with this until told otherwise. */
    answerWith(fn: () => SnapshotPayload) {
      queued = fn
    },
    get last() {
      return states.at(-1)!
    },
    /** Let pending microtasks and the interval run. */
    async tick(ms = POLL_INTERVAL_MS) {
      await vi.advanceTimersByTimeAsync(ms)
    },
  }
}

describe('SnapshotPoller', () => {
  beforeEach(() => vi.useFakeTimers())
  afterEach(() => vi.useRealTimers())

  it('polls once immediately and then on the interval', async () => {
    const h = harness()
    h.answerWith(() => ok({ panes: scrambled }))
    h.poller.start()
    await h.tick(0)
    expect(h.last.loaded).toBe(true)
    expect(h.states).toHaveLength(1)

    await h.tick(POLL_INTERVAL_MS - 1)
    expect(h.states).toHaveLength(1) // not a millisecond early
    await h.tick(1)
    expect(h.states).toHaveLength(2)
  })

  it('keeps the last good tree when a poll fails', async () => {
    const h = harness()
    h.answerWith(() => ok({ panes: scrambled }))
    h.poller.start()
    await h.tick(0)
    const good = h.last.groups

    h.answerWith(() => {
      throw new SnapshotFetchError('network down')
    })
    await h.tick()

    // The whole point of the file: a sidebar that was correct 1.5s ago is not
    // blanked by one bad poll.
    expect(h.last.groups).toBe(good)
    expect(h.last.rows).toHaveLength(4)
    expect(h.last.loaded).toBe(true)
    expect(h.last.error).toBe('network down')
  })

  it('carries the server generation through, and holds it across a failure', async () => {
    const h = harness()
    h.answerWith(() => ok({ panes: scrambled, serverStart: '1757500000' }))
    h.poller.start()
    await h.tick(0)
    expect(h.last.serverStart).toBe('1757500000')

    h.answerWith(() => {
      throw new SnapshotFetchError('network down')
    })
    await h.tick()
    // Dropping it here would key every `seen` lookup on "" for one interval,
    // and `isDone` refuses to compute a badge without a generation -- so every
    // done dot would blink off and back on with each hiccup.
    expect(h.last.serverStart).toBe('1757500000')

    // A restarted tmux server is a new generation, and it replaces the old one
    // rather than being merged with it.
    h.answerWith(() => ok({ panes: scrambled, serverStart: '1757509999' }))
    await h.tick()
    expect(h.last.serverStart).toBe('1757509999')
  })

  it('says stale only after two consecutive bad polls', async () => {
    const h = harness()
    h.answerWith(() => ok({ panes: scrambled }))
    h.poller.start()
    await h.tick(0)

    h.answerWith(() => {
      throw new Error('one blip')
    })
    await h.tick()
    expect(h.last.stale).toBe(false)
    expect(h.last.trouble).toBe(1)

    await h.tick()
    expect(h.last.stale).toBe(true)
    expect(h.last.trouble).toBe(2)
  })

  it("counts the daemon's own stale flag as trouble, and a clean poll clears it", async () => {
    const h = harness()
    h.answerWith(() => ok({ panes: scrambled, stale: true, error: 'tmux is restarting' }))
    h.poller.start()
    await h.tick(0)
    await h.tick()
    expect(h.last.stale).toBe(true)
    expect(h.last.error).toBe('tmux is restarting')

    h.answerWith(() => ok({ panes: scrambled }))
    await h.tick()
    expect(h.last.stale).toBe(false)
    expect(h.last.trouble).toBe(0)
    expect(h.last.error).toBeNull()
  })

  it('shows an empty tmux server as loaded with no groups', async () => {
    const h = harness()
    h.answerWith(() => ok({ panes: [] }))
    h.poller.start()
    await h.tick(0)
    expect(h.last.loaded).toBe(true)
    expect(h.last.groups).toEqual([])
    expect(h.last.error).toBeNull()
  })

  it('stops polling for good once the device is unauthorized', async () => {
    const h = harness()
    h.answerWith(() => {
      throw new SnapshotFetchError('unauthorized', 401)
    })
    h.poller.start()
    await h.tick(0)
    expect(h.last.unauthorized).toBe(true)

    const seen = h.states.length
    await h.tick(POLL_INTERVAL_MS * 10)
    expect(h.states).toHaveLength(seen)
  })

  it('asks for the one cadence a background tab is actually given', () => {
    // Pinned as a number rather than derived, because every timing assertion
    // below is written in terms of this constant and would follow it anywhere.
    // The two bounds are two different arguments:
    //
    // Not below a minute -- Chromium's intensive throttling aligns a hidden
    // page's timers to a one-minute wall-clock grid after five minutes hidden,
    // so anything shorter is rounded up to a minute anyway and only buys extra
    // requests in the first five minutes, which is exactly when the tab is most
    // likely to come back and be woken instead.
    //
    // Not above it either -- the worst case a user sees is one interval plus a
    // full minute of grid skew, so 60s is a badge up to two minutes late and
    // anything slower makes that worse for nothing: the requests it saves are
    // one HTTP read of an already-computed cache.
    expect(HIDDEN_POLL_INTERVAL_MS).toBeGreaterThanOrEqual(60_000)
    expect(HIDDEN_POLL_INTERVAL_MS).toBeLessThanOrEqual(60_000)
    // And slower than the visible one, which is the entire point of having two.
    expect(HIDDEN_POLL_INTERVAL_MS).toBeGreaterThan(POLL_INTERVAL_MS)
  })

  it('keeps polling while the tab is hidden, on the hidden cadence', async () => {
    // The badge exists to answer "do I need to go back to it?" for a tab you
    // are *not* looking at. A loop that parked while hidden could never change
    // the count in the only situation the count is for.
    let hidden = true
    const h = harness({ hidden: () => hidden })
    h.answerWith(() => ok({ panes: scrambled }))
    h.poller.start()
    await h.tick(0)
    expect(h.states).toHaveLength(1)

    // Not the visible cadence: five of those pass with nothing sent.
    await h.tick(POLL_INTERVAL_MS * 5)
    expect(h.states).toHaveLength(1)

    await h.tick(HIDDEN_POLL_INTERVAL_MS - POLL_INTERVAL_MS * 5 - 1)
    expect(h.states).toHaveLength(1) // not a millisecond early
    h.answerWith(() =>
      ok({ panes: scrambled.map((r) => (r.paneId === '%4' ? { ...r, agentState: 'blocked' } : r)) }),
    )
    await h.tick(1)
    expect(h.states).toHaveLength(2)
    // And the answer actually reached the state, which is what the badge reads.
    expect(h.last.rows.find((r) => r.paneId === '%4')?.agentState).toBe('blocked')

    // Still hidden: the next one is a hidden interval away, not a visible one.
    await h.tick(POLL_INTERVAL_MS * 5)
    expect(h.states).toHaveLength(2)
    await h.tick(HIDDEN_POLL_INTERVAL_MS - POLL_INTERVAL_MS * 5)
    expect(h.states).toHaveLength(3)
  })

  it('goes back to the visible cadence once the tab is looked at again', async () => {
    let hidden = true
    const h = harness({ hidden: () => hidden })
    h.answerWith(() => ok({ panes: scrambled }))
    h.poller.start()
    await h.tick(0)
    expect(h.states).toHaveLength(1)

    hidden = false
    h.poller.wake()
    await h.tick(0)
    expect(h.states).toHaveLength(2)

    await h.tick(POLL_INTERVAL_MS - 1)
    expect(h.states).toHaveLength(2)
    await h.tick(1)
    expect(h.states).toHaveLength(3)
  })

  it('wake() cuts the hidden wait short rather than leaving a stale first frame', async () => {
    // The reason the loop used to park: a throttled interval would leave the
    // first visible frame showing a minute-old tree. The hidden cadence is a
    // minute, so wake() is the whole answer to that -- it must not merely queue
    // behind the timer it found armed.
    let hidden = true
    const h = harness({ hidden: () => hidden })
    h.answerWith(() => ok({ panes: scrambled }))
    h.poller.start()
    await h.tick(0)
    await h.tick(5_000)
    expect(h.states).toHaveLength(1)

    hidden = false
    h.poller.wake()
    await h.tick(0)
    expect(h.states).toHaveLength(2)
    // The minute-long timer it interrupted is gone rather than still pending,
    // so it cannot fire a spare poll 55s from now: exactly one timer is armed,
    // and it is the visible-cadence one asserted above.
    expect(vi.getTimerCount()).toBe(1)
  })

  it('a wake mid-flight polls again as soon as the hidden request settles, without racing it', async () => {
    // A hidden request that is already out was asked before you looked, so its
    // answer is not the fresh frame wake() promises -- but firing a second
    // request alongside it would let two answers land in either order and put
    // an older tree on screen than the one already there. So: never two in
    // flight, and the wake is honoured the moment the first one settles.
    let hidden = true
    const h = harness({ hidden: () => hidden })
    h.poller.start()
    await h.tick(0)
    expect(h.calls).toHaveLength(1)

    hidden = false
    h.poller.wake()
    await h.tick(0)
    expect(h.calls).toHaveLength(1) // the wake did not open a second one
    expect(h.states).toHaveLength(0)

    const older = scrambled.map((r) => (r.paneId === '%4' ? { ...r, command: 'zsh' } : r))
    h.calls[0].resolve(ok({ panes: older }))
    await h.tick(0)
    expect(h.states).toHaveLength(1)
    // Immediately, not a visible interval later: the wake was owed a poll.
    expect(h.calls).toHaveLength(2)

    h.calls[1].resolve(ok({ panes: scrambled }))
    await h.tick(0)
    expect(h.states).toHaveLength(2)
    // And the newer answer is the one on screen, in that order.
    expect(h.last.rows.find((r) => r.paneId === '%4')?.command).toBe('claude')
  })

  it('ignores a wake while the loop is already running', async () => {
    const h = harness()
    h.answerWith(() => ok({ panes: scrambled }))
    h.poller.start()
    await h.tick(0)
    h.poller.wake()
    h.poller.wake()
    await h.tick(0)
    expect(h.states).toHaveLength(1)
  })

  it('never has two requests in flight', async () => {
    const h = harness()
    h.poller.start()
    await h.tick(0)
    expect(h.calls).toHaveLength(1)

    // The interval elapses several times while the first request hangs, and an
    // impatient user hits Retry on top of it.
    await h.tick(POLL_INTERVAL_MS * 4)
    h.poller.refresh()
    h.poller.refresh()
    await h.tick(0)
    expect(h.calls).toHaveLength(1)

    h.calls[0].resolve(ok({ panes: scrambled }))
    await h.tick(0)
    await h.tick()
    expect(h.calls).toHaveLength(2)
  })

  it('abandons a request that never settles', async () => {
    const states: SnapshotState[] = []
    const poller = new SnapshotPoller({
      onState: (s) => states.push(s),
      isHidden: () => false,
      timeoutMs: 100,
      fetcher: (signal) =>
        new Promise((_, reject) => {
          signal.addEventListener('abort', () => reject(new Error('aborted')))
        }),
    })
    poller.start()
    await vi.advanceTimersByTimeAsync(99)
    expect(states).toHaveLength(0)
    await vi.advanceTimersByTimeAsync(1)
    expect(states).toHaveLength(1)
    expect(states[0].error).toBe('aborted')
    poller.stop()
  })

  it('refresh() polls now instead of waiting out the interval', async () => {
    const h = harness()
    h.answerWith(() => ok({ panes: scrambled }))
    h.poller.start()
    await h.tick(0)
    h.poller.refresh()
    await h.tick(0)
    expect(h.states).toHaveLength(2)
  })

  it('stop() ends the loop and reports nothing further', async () => {
    const h = harness()
    h.poller.start()
    h.poller.stop()
    h.calls[0].resolve(ok({ panes: scrambled }))
    await h.tick(POLL_INTERVAL_MS * 3)
    expect(h.states).toHaveLength(0)
  })

  it('keeps the tree identity when nothing changed, and replaces it when it did', async () => {
    const h = harness()
    h.answerWith(() => ok({ panes: scrambled.map((r) => ({ ...r })) }))
    h.poller.start()
    await h.tick(0)
    const first = h.last.groups
    await h.tick()
    expect(h.last.groups).toBe(first)

    h.answerWith(() =>
      ok({ panes: scrambled.map((r) => (r.paneId === '%2' ? { ...r, command: 'less' } : r)) }),
    )
    await h.tick()
    expect(h.last.groups).not.toBe(first)
    expect(h.last.groups[0].windows[0].panes[2].command).toBe('less')
  })

  it('compares every field the wire carries, so no change is swallowed', async () => {
    // Whether the previous tree object survives a poll is decided by rowsEqual,
    // and React reconciles nothing when it does -- so a field left out of that
    // comparison is a field that can change in tmux and never reach the DOM.
    // The live case is the pane title: Claude Code rewrites it when its task
    // changes and nothing else about the pane moves at all.
    //
    // Driven off the fixture's own keys rather than a list written out here, so
    // a field added to the wire is covered on the day it arrives.
    const base = row({ paneId: '%1' })
    for (const key of Object.keys(base) as (keyof SnapshotRow)[]) {
      const was = base[key]
      const moved: SnapshotRow = {
        ...base,
        [key]: typeof was === 'string' ? `${was}-moved` : typeof was === 'number' ? was + 1 : !was,
      }
      const h = harness()
      h.answerWith(() => ok({ panes: [{ ...base }] }))
      h.poller.start()
      await h.tick(0)
      const first = h.last.groups

      h.answerWith(() => ok({ panes: [moved] }))
      await h.tick()
      expect(h.last.groups, `a change to ${key} was swallowed`).not.toBe(first)
    }
  })

  // The two fields v3 added get this spelled out rather than left to the loop
  // above, because they are the case that loop exists for and the one where
  // being wrong is invisible: an agent's activity line is the field that moves
  // most often with nothing else about the pane moving at all, so a `rowsEqual`
  // that does not compare it keeps the previous tree object, React reconciles
  // nothing, and a stale line sits on screen while every other test stays
  // green. Each field is moved on its own, from a payload identical in every
  // other field, and the identical-payload leg below it is what keeps a
  // `rowsEqual` that simply always returns false from passing this.
  it.each([
    ['activity', { activity: 'edit report.go' }],
    ['stateSource', { stateSource: 'event' }],
    // `path` is the field the loop above would be least likely to miss and the
    // easiest to leave out of `rowsEqual` by hand, because nothing renders it
    // yet: a comparison that ignores it keeps the previous tree object, React
    // reconciles nothing, and the pane that has `cd`'d somewhere else goes on
    // reporting the directory it left -- to whatever reads it next.
    ['path', { path: '/srv/other' }],
  ])('rebuilds the tree when only %s changed', async (_name, moved) => {
    const base = row({ paneId: '%1', command: 'claude', agentState: 'working' })

    const h = harness()
    h.answerWith(() => ok({ panes: [{ ...base }] }))
    h.poller.start()
    await h.tick(0)
    const first = h.last.groups

    h.answerWith(() => ok({ panes: [{ ...base }] }))
    await h.tick()
    expect(h.last.groups, 'an identical payload rebuilt the tree').toBe(first)

    h.answerWith(() => ok({ panes: [{ ...base, ...moved }] }))
    await h.tick()
    expect(h.last.groups).not.toBe(first)
  })

  // `question` is the only field on the wire that is not a scalar, so it is the
  // only one where identity and equality come apart. The daemon parses a fresh
  // object out of the capture every poll, so comparing by reference would
  // rebuild the tree every 1.5s for every blocked pane -- and comparing not at
  // all would leave a changed question stuck on screen.
  it('rebuilds only when the question itself changed', async () => {
    const asking = (text: string, choices: string[]) =>
      row({ paneId: '%1', command: 'claude', question: { text, choices } })

    const h = harness()
    h.answerWith(() => ok({ panes: [asking('Do you want to create fixture.txt?', ['Yes', 'No'])] }))
    h.poller.start()
    await h.tick(0)
    const first = h.last.groups

    // A structurally identical question in a new object: nothing changed.
    h.answerWith(() => ok({ panes: [asking('Do you want to create fixture.txt?', ['Yes', 'No'])] }))
    await h.tick()
    expect(h.last.groups, 'a re-parsed but identical question rebuilt the tree').toBe(first)

    // Same text, different options -- an agent that moved on to another ask.
    h.answerWith(() =>
      ok({ panes: [asking('Do you want to create fixture.txt?', ['Yes', 'Yes to all', 'No'])] }),
    )
    await h.tick()
    expect(h.last.groups, 'a change to the choices was swallowed').not.toBe(first)
    const second = h.last.groups

    h.answerWith(() => ok({ panes: [asking('Do you want to run rm -rf?', ['Yes', 'Yes to all', 'No'])] }))
    await h.tick()
    expect(h.last.groups, 'a change to the question text was swallowed').not.toBe(second)
  })

  it('notices a pane appearing or disappearing even when the count is the same', async () => {
    const h = harness()
    h.answerWith(() => ok({ panes: scrambled }))
    h.poller.start()
    await h.tick(0)
    const first = h.last.groups

    h.answerWith(() =>
      ok({ panes: scrambled.map((r) => (r.paneId === '%1' ? { ...r, paneId: '%6' } : r)) }),
    )
    await h.tick()
    expect(h.last.groups).not.toBe(first)
  })
})

// --- which agent needs you --------------------------------------------------
//
// The four states, the roll-up that makes a collapsed sidebar useful, and the
// one of them this browser computes for itself.

describe('the wire carries the tmux server generation', () => {
  it('reads the field the daemon sends on the envelope, not on a row', () => {
    // `seen` is keyed on it, so a rename of the Go field would silently turn
    // every done badge off -- `isDone` refuses to compute one without it.
    const go = goSource('internal/front/server.go')
    const struct = go.match(/type snapshotResponse struct \{([\s\S]*?)\n\}/)
    if (!struct) throw new Error('type snapshotResponse not found in internal/front/server.go')
    expect(struct[1]).toContain('json:"serverStart"')
    expect(parseSnapshot({ panes: [], serverStart: '1757500000' }).serverStart).toBe('1757500000')
  })

  it('reads anything that is not a string as no generation at all', () => {
    // "" is the documented "cannot key a seen entry safely" value, and a daemon
    // too old to send the field must land on it rather than on "undefined".
    expect(parseSnapshot({ panes: [], serverStart: 12345 }).serverStart).toBe('')
    expect(parseSnapshot({ panes: [] }).serverStart).toBe('')
  })
})

describe('mostUrgent', () => {
  it('ranks blocked over done over working over idle', () => {
    // Every adjacent pair, in both argument orders, so that neither a reversed
    // comparison nor a "first one wins" can pass.
    const pairs = [
      ['blocked', 'done'],
      ['done', 'working'],
      ['working', 'idle'],
    ] as const
    for (const [urgent, calm] of pairs) {
      expect(mostUrgent([urgent, calm])).toBe(urgent)
      expect(mostUrgent([calm, urgent])).toBe(urgent)
    }
    // And end to end, so a comparator that is only wrong across two ranks is
    // caught too.
    expect(mostUrgent(['idle', 'working', 'blocked', 'done'])).toBe('blocked')
    expect(mostUrgent(['idle', 'working', 'done'])).toBe('done')
  })

  it('skips panes with no state rather than ranking them', () => {
    // "" is not a state: three shells and one blocked agent is a blocked
    // window, and three shells are nothing at all.
    expect(mostUrgent(['', 'idle', ''])).toBe('idle')
    expect(mostUrgent(['', '', ''])).toBe('')
    expect(mostUrgent([])).toBe('')
  })
})

describe('paneState', () => {
  const seen: SeenMap = {}
  const at = (over: Partial<PaneNode>) => ({
    paneId: '%1',
    agentState: '',
    finishedAt: 0,
    ...over,
  })

  it('renders no state for a pane the daemon did not classify', () => {
    // A shell is not idle; it is a shell. "" means nothing computed a state --
    // the pane is not a known agent, or nobody was watching -- and a dot there
    // would be a claim about a pane nothing looked at.
    expect(paneState(at({ agentState: '' }), '1', seen)).toBe('')
    // Including when it carries a finish stamp from when it *was* an agent.
    expect(paneState(at({ agentState: '', finishedAt: 500 }), '1', seen)).toBe('')
  })

  it('renders no state for a value it does not recognise', () => {
    // The daemon owns the list. A fourth state arriving on the wire is a wire
    // change, and inventing a dot for it is worse than showing none.
    expect(paneState(at({ agentState: 'thinking' }), '1', seen)).toBe('')
  })

  it("reports the daemon's own blocked and working unchanged", () => {
    expect(paneState(at({ agentState: 'blocked' }), '1', seen)).toBe('blocked')
    expect(paneState(at({ agentState: 'working' }), '1', seen)).toBe('working')
  })

  it('turns an idle pane with an unseen finish into done', () => {
    expect(paneState(at({ agentState: 'idle', finishedAt: 500 }), '1', seen)).toBe('done')
    expect(paneState(at({ agentState: 'idle', finishedAt: 0 }), '1', seen)).toBe('idle')
    expect(
      paneState(at({ agentState: 'idle', finishedAt: 500 }), '1', {
        '1:%1': 500,
      }),
    ).toBe('idle')
  })

  it('keeps a blocked pane blocked even when it also has an unseen finish', () => {
    // Both are true and only one is the answer: blocked is what needs you now.
    expect(paneState(at({ agentState: 'blocked', finishedAt: 500 }), '1', seen)).toBe('blocked')
  })

  it('keeps a working pane working even when it has an unseen finish', () => {
    // The stamp is the end of an *earlier* run and the pane has started
    // another. Nothing is lost: the next working -> idle edge stamps a newer
    // finishedAt and the badge comes back.
    expect(paneState(at({ agentState: 'working', finishedAt: 500 }), '1', seen)).toBe('working')
  })
})

describe('isDone', () => {
  it('keys what this browser has seen on the tmux server generation', () => {
    // Pane ids restart at %0 when the tmux server does. A `seen` entry from the
    // previous server must not suppress the badge on the pane that inherited
    // its id -- which is exactly what a bare `seen["%3"]` would do.
    const pane = { paneId: '%3', finishedAt: 500 }
    expect(isDone(pane, '200', { '100:%3': 900 })).toBe(true)
    expect(isDone(pane, '200', { '200:%3': 900 })).toBe(false)
    // And the key really is `${serverStart}:${paneId}`, not some other join.
    expect(isDone(pane, '200', { [seenKey('200', '%3')]: 900 })).toBe(false)
  })

  it('refuses to compute done at all without a generation', () => {
    // "" is what an empty tmux server sends and what a daemon too old to send
    // the field leaves behind. Every pane would share one unqualified key, so
    // the honest answer is no badge: a missing one costs a glance, a wrong one
    // costs trust in all of them.
    expect(isDone({ paneId: '%3', finishedAt: 500 }, '', {})).toBe(false)
  })

  it('is false for a pane that has never finished', () => {
    expect(isDone({ paneId: '%3', finishedAt: 0 }, '200', {})).toBe(false)
  })

  it('needs the finish to be strictly newer than the one already seen', () => {
    expect(isDone({ paneId: '%3', finishedAt: 500 }, '200', { '200:%3': 499 })).toBe(true)
    expect(isDone({ paneId: '%3', finishedAt: 500 }, '200', { '200:%3': 500 })).toBe(false)
  })
})

describe('the roll-up', () => {
  const pane = (over: Partial<SnapshotRow>) => row({ command: 'claude', ...over })

  it('gives a window the most urgent state among its panes', () => {
    const [session] = groupRows([
      pane({ paneId: '%0', paneIndex: 0, agentState: 'idle' }),
      pane({ paneId: '%1', paneIndex: 1, agentState: 'blocked' }),
      pane({ paneId: '%2', paneIndex: 2, agentState: 'working' }),
    ])
    expect(windowState(session.windows[0], '1', {})).toBe('blocked')
  })

  it('gives a session the most urgent state among its windows', () => {
    const [session] = groupRows([
      pane({ paneId: '%0', windowIndex: 0, agentState: 'idle' }),
      pane({
        paneId: '%1',
        windowIndex: 1,
        agentState: 'idle',
        finishedAt: 500,
      }),
      pane({ paneId: '%2', windowIndex: 2, agentState: 'working' }),
    ])
    // The middle window is the only one with an unseen finish, so the session
    // reads done -- above working, below blocked.
    expect(session.windows.map((w) => windowState(w, '1', {}))).toEqual(['idle', 'done', 'working'])
    expect(sessionState(session, '1', {})).toBe('done')
  })

  it('says nothing about a session running no agents', () => {
    const [session] = groupRows([row({ command: 'zsh' }), row({ paneId: '%1', command: 'vim' })])
    expect(sessionState(session, '1', {})).toBe('')
  })
})

describe('seen', () => {
  /** A `Storage` that keeps its one value in a closure. */
  function fakeStorage(initial: Record<string, string> = {}) {
    const items = { ...initial }
    return {
      items,
      getItem: (k: string) => items[k] ?? null,
      setItem: (k: string, v: string) => {
        items[k] = v
      },
    }
  }

  it('stores every entry under the server generation', () => {
    const next = markSeen({}, '1757500000', '%3', 900)
    expect(next).toEqual({ '1757500000:%3': 900 })
    // Spelled out rather than built with seenKey, so a change to the key
    // format has to be made here too.
    expect(Object.keys(next)).toEqual(['1757500000:%3'])
  })

  it('drops entries from tmux servers that are gone', () => {
    // They can never match a lookup again, and this is the only moment that
    // knows which generation is current.
    const next = markSeen({ '100:%3': 900, '200:%1': 5 }, '200', '%3', 950)
    expect(next).toEqual({ '200:%1': 5, '200:%3': 950 })
  })

  it('never lowers an entry, so one finish is never announced twice', () => {
    // A daemon restart rebuilds its map from nothing and reports finishedAt 0
    // for every pane. Recording that would make an already-seen finish
    // announceable again as soon as the stamp came back.
    const seen = { '200:%3': 900 }
    expect(markSeen(seen, '200', '%3', 0)).toBe(seen)
    expect(markSeen(seen, '200', '%3', 900)).toBe(seen)
    expect(markSeen(seen, '200', '%3', 901)).toEqual({ '200:%3': 901 })
  })

  it('records nothing at all without a generation to key it on', () => {
    const seen = {}
    expect(markSeen(seen, '', '%3', 900)).toBe(seen)
  })

  it('marks the pane the tab is looking at, and only that one', () => {
    const rows = [
      row({ paneId: '%0', agentState: 'idle', finishedAt: 700 }),
      row({ paneId: '%1', agentState: 'idle', finishedAt: 900 }),
    ]
    expect(viewedSeen({}, '200', '%1', rows)).toEqual({ '200:%1': 900 })
  })

  it('records nothing for a pane the snapshot no longer carries', () => {
    // Inventing a stamp here would suppress the badge on whatever pane
    // inherits that id.
    const seen = {}
    expect(viewedSeen(seen, '200', '%9', [row({ paneId: '%0' })])).toBe(seen)
    expect(viewedSeen(seen, '200', null, [row({ paneId: '%0' })])).toBe(seen)
  })

  it('round-trips through storage under one key', () => {
    const storage = fakeStorage()
    writeSeen({ '200:%3': 900 }, storage)
    expect(storage.items[SEEN_STORAGE_KEY]).toBe('{"200:%3":900}')
    expect(readSeen(storage)).toEqual({ '200:%3': 900 })
  })

  it('reads a missing, corrupt or foreign value as nothing seen', () => {
    // Clearing browser data forgets it and every badge reappears once, which is
    // harmless. Throwing would blank the sidebar.
    expect(readSeen(fakeStorage())).toEqual({})
    expect(readSeen(fakeStorage({ [SEEN_STORAGE_KEY]: 'not json' }))).toEqual({})
    expect(readSeen(fakeStorage({ [SEEN_STORAGE_KEY]: '[1,2]' }))).toEqual({})
    expect(readSeen(fakeStorage({ [SEEN_STORAGE_KEY]: '{"a":"soon"}' }))).toEqual({})
    expect(readSeen(null)).toEqual({})
  })

  it('survives a storage that throws on every access', () => {
    // Safari's private mode, and any browser told to block site data.
    const hostile = {
      getItem() {
        throw new Error('SecurityError')
      },
      setItem() {
        throw new Error('SecurityError')
      },
    }
    expect(readSeen(hostile)).toEqual({})
    expect(() => writeSeen({ '200:%3': 900 }, hostile)).not.toThrow()
  })
})
