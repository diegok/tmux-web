import { readFileSync } from 'node:fs'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import {
  POLL_INTERVAL_MS,
  SNAPSHOT_URL,
  SnapshotFetchError,
  SnapshotPoller,
  chooseSession,
  fetchSnapshot,
  findPane,
  groupRows,
  parseSnapshot,
  resolveSession,
  windowTarget,
} from './useSnapshot'
import type { SnapshotPayload, SnapshotRow, SnapshotState } from './useSnapshot'

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
    windowIndex: 0,
    windowName: 'shell',
    paneActive: true,
    command: 'zsh',
    // What tmux gives a pane nothing has titled: the hostname.
    title: 'devbox',
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
  return { stale: false, error: null, ...payload }
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
    const tags = [...struct[1].matchAll(/json:"([^",]+)"/g)].map((m) => m[1])
    expect(tags).toHaveLength(12)
    expect(Object.keys(row()).sort()).toEqual(tags.sort())
  })
})

// --- parseSnapshot ----------------------------------------------------------

describe('parseSnapshot', () => {
  it('reads null panes as an empty snapshot, not an error', () => {
    // The Go side returns nil for both "no panes" and "no tmux server", and
    // both are answers rather than faults. The daemon rewrites nil to [] today;
    // this is the case that must not become a thrown error if it stops.
    expect(parseSnapshot({ panes: null })).toEqual({ panes: [], stale: false, error: null })
    expect(parseSnapshot({})).toEqual({ panes: [], stale: false, error: null })
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
