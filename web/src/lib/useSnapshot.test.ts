import { readFileSync } from 'node:fs'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import {
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
  markSeen,
  mostUrgent,
  paneState,
  parseSnapshot,
  readSeen,
  resolveSession,
  seenKey,
  sessionState,
  viewedSeen,
  windowState,
  windowTarget,
  writeSeen,
} from './useSnapshot'
import type { PaneNode, SeenMap, SnapshotPayload, SnapshotRow, SnapshotState } from './useSnapshot'

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

  it('does not poll while the tab is hidden, and catches up on wake', async () => {
    let hidden = false
    const h = harness({ hidden: () => hidden })
    h.answerWith(() => ok({ panes: scrambled }))
    h.poller.start()
    await h.tick(0)
    expect(h.states).toHaveLength(1)

    hidden = true
    await h.tick(POLL_INTERVAL_MS * 5)
    expect(h.states).toHaveLength(1)

    hidden = false
    h.poller.wake()
    await h.tick(0)
    expect(h.states).toHaveLength(2)
    // And the loop is running again rather than parked.
    await h.tick()
    expect(h.states).toHaveLength(3)
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
