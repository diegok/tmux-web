/**
 * The sidebar's view of tmux: `GET /api/snapshot`, polled, grouped into the
 * session → window → pane tree the sidebar renders.
 *
 * The daemon already forks one `tmux list-panes -a` every 1.5s and hands every
 * tab the same cached answer, so this poll is an HTTP request against memory,
 * not a tmux fork. Polling faster than the daemon refreshes would only burn
 * requests to re-read the same bytes; polling slower would add lag to a number
 * the design already accepts as the cost of not running a control-mode sidecar.
 * Hence exactly `POLL_INTERVAL_MS`.
 *
 * ## What this file will not do
 *
 * **It never blanks a tree that was correct 1.5s ago.** tmux prints "server
 * exited unexpectedly" for a few milliseconds while a server restarts, a phone
 * changing cells drops a request outright, and either would empty the sidebar
 * for one interval and refill it on the next. The last good rows are held and
 * the failure is reported beside them; the daemon takes the same position one
 * layer down (see `snapshot` in `internal/front/server.go`), and this is the
 * client half of that decision.
 *
 * **It never re-sorts.** `tmux.Dedupe` orders rows by (group, window index,
 * pane index) -- the order on screen. Pane ids are allocation order, so after a
 * split-and-kill cycle they run `%0 %4 %2 %1` against a layout of `0 1 2 3`.
 * Grouping here preserves arrival order for exactly that reason.
 *
 * **It keeps no model of tmux.** The response renders directly; nothing is
 * merged across polls except the last good copy, so there is no client-side
 * state that can drift from the server.
 */

import { useCallback, useEffect, useRef, useState } from 'react'

/** Where the cached snapshot lives. Same origin: the daemon serves this SPA. */
export const SNAPSHOT_URL = '/api/snapshot'

/**
 * Poll period, matching `front.DefaultPollInterval` on the Go side.
 *
 * This is the daemon's own refresh interval. A client that polled at 500ms
 * would get the same three answers, and one that polled at 5s would add its own
 * lag on top of tmux's.
 */
export const POLL_INTERVAL_MS = 1500

/**
 * How long one poll may take before it is abandoned.
 *
 * Without this a request that never settles never rejects either, and the
 * sidebar freezes on stale rows forever with no indication -- the realistic way
 * to reach that state is a phone that lost its network mid-request. Generous
 * relative to a request the daemon answers from memory.
 */
export const POLL_TIMEOUT_MS = 8000

/**
 * Consecutive troubled polls before the UI says the tree is stale.
 *
 * One is not enough: a single failed fork inside the daemon sets its `stale`
 * flag for one interval, and flashing a warning for 1.5s every time tmux
 * hiccups trains the user to ignore it. Two consecutive (~3s) means something
 * is actually wrong.
 */
export const TROUBLE_BEFORE_STALE = 2

/**
 * One pane, exactly as `tmux.Row` marshals it. The json tags on the Go struct
 * are the contract; these names mirror them field for field.
 */
export interface SnapshotRow {
  /** session_group, falling back to session_name. What the sidebar labels. */
  groupKey: string
  /**
   * `$N`, and the session's live name beside it. Both differ from `groupKey`:
   * tmux keeps the *pre-rename* name in session_group forever, so the group key
   * is neither an address nor a display name. Mirrored here because the wire
   * carries them; what the sidebar does with them is Task 10's.
   */
  sessionId: string
  sessionName: string
  /** e.g. "%3". Unique per tmux server and stable for the pane's life. */
  paneId: string
  /** Position in the window's layout. Not the id -- see the header comment. */
  paneIndex: number
  /** Row came from a session this app created (`@wterm_web`). */
  appOwned: boolean
  /** `@wterm_label`: a name the user gave this pane, or "" when unset. */
  label: string
  windowIndex: number
  windowName: string
  /** tmux's current pane *for that window*. Windows are shared by a group. */
  paneActive: boolean
  /** pane_current_command: `claude`, `vim`, `zsh`. */
  command: string
  /**
   * `pane_title`, normalised by tmux's own OSC parser and capped at 256 bytes
   * by the daemon. Claude Code sets it to what it is working on; a plain shell
   * leaves it at the hostname. `paneBadge` decides which of those is worth a
   * row.
   */
  title: string
  /**
   * `""`, `"working"`, `"idle"` or `"blocked"`.
   *
   * `""` means the daemon computed no state -- the pane is not a known agent,
   * or the poll was taken with no browser connected. It is not a state and is
   * never rendered as one. The frontend never decides what counts as an agent:
   * that list lives on the Go side and gates capture, state and logo together.
   */
  agentState: string
  /**
   * Unix ms of this pane's most recent working -> idle edge, or 0 if it has not
   * had one. A timestamp rather than a `done` flag because "done" would have to
   * be cleared by somebody: the browser compares this against its own memory of
   * what it has already looked at.
   */
  finishedAt: number
  /**
   * What a blocked agent is waiting on, when the daemon could read it.
   *
   * Absent for every pane that is not a blocked agent -- and also for a blocked
   * agent whose dialog the daemon's grammar did not recognise, which is the
   * point of it being optional: the badge comes from `agentState`, and a
   * restyled dialog costs the quote rather than the state.
   */
  question?: SnapshotQuestion
}

/** The request on a blocked agent's screen, as `tmux.Question` marshals it. */
export interface SnapshotQuestion {
  text: string
  /** Omitted by the daemon when empty, hence optional here. */
  choices?: string[]
}

/** The body of `GET /api/snapshot`, normalised. */
export interface SnapshotPayload {
  panes: SnapshotRow[]
  /** The daemon's most recent poll failed; these rows predate it. */
  stale: boolean
  /** Why that poll failed, when it did. */
  error: string | null
}

/** A poll that did not produce a snapshot. */
export class SnapshotFetchError extends Error {
  /** HTTP status, or 0 for a transport-level failure. */
  readonly status: number
  constructor(message: string, status = 0) {
    super(message)
    this.name = 'SnapshotFetchError'
    this.status = status
  }
  /**
   * The device cookie is gone or was revoked. Distinguished because it is the
   * one failure retrying cannot fix: the poll would 401 every 1.5s forever.
   */
  get unauthorized(): boolean {
    return this.status === 401 || this.status === 403
  }
}

/**
 * Normalise a decoded response body.
 *
 * `panes` is missing or null for both "no panes" and "no tmux server at all",
 * which are the same answer to the sidebar's question and neither of them an
 * error. The daemon currently rewrites nil to `[]` before it marshals, but that
 * is one line of Go away from changing and `null.map` is a blank screen, so the
 * case is handled here rather than trusted. Anything else where an array
 * belongs is a genuinely broken response and is refused, so that a wire change
 * surfaces as an error message instead of a silently empty sidebar.
 */
export function parseSnapshot(body: unknown): SnapshotPayload {
  if (typeof body !== 'object' || body === null) {
    throw new SnapshotFetchError('the snapshot response was not an object')
  }
  const raw = body as { panes?: unknown; stale?: unknown; error?: unknown }
  let panes: SnapshotRow[]
  if (raw.panes === undefined || raw.panes === null) {
    panes = []
  } else if (Array.isArray(raw.panes)) {
    panes = raw.panes as SnapshotRow[]
  } else {
    throw new SnapshotFetchError('the snapshot response carried no pane list')
  }
  return {
    panes,
    stale: raw.stale === true,
    error: typeof raw.error === 'string' && raw.error !== '' ? raw.error : null,
  }
}

/** Injectable for tests; defaults to the global. */
export type FetchLike = (url: string, init: RequestInit) => Promise<Response>

/**
 * One poll.
 *
 * A non-2xx is turned into a SnapshotFetchError carrying the daemon's own
 * message when it sent JSON -- 503 says "cannot read the tmux server: ..." and
 * that sentence is more useful in the UI than "503". `Protect` answers 401 in
 * plain text, so the body is parsed opportunistically and never required.
 */
export async function fetchSnapshot(
  signal: AbortSignal,
  fetchImpl: FetchLike = ((url, init) => globalThis.fetch(url, init)) as FetchLike,
): Promise<SnapshotPayload> {
  const res = await fetchImpl(SNAPSHOT_URL, {
    signal,
    headers: { Accept: 'application/json' },
    // The device cookie is same-origin and HttpOnly; spelled out so a future
    // absolute URL does not quietly drop it.
    credentials: 'same-origin',
  })
  if (!res.ok) {
    let message = `snapshot request failed (${res.status})`
    try {
      const body = (await res.json()) as { error?: unknown }
      if (typeof body?.error === 'string' && body.error !== '') message = body.error
    } catch {
      // Plain-text or empty body: the status is all there is to say.
    }
    throw new SnapshotFetchError(message, res.status)
  }
  return parseSnapshot(await res.json())
}

/** A pane in the tree. */
export interface PaneNode {
  paneId: string
  paneIndex: number
  command: string
  /** The pane's tmux title; the hostname when nothing has set one. */
  title: string
  /** `@wterm_label`, or "" -- the only one of the three the user chose. */
  label: string
  /** tmux's active pane within this window. */
  active: boolean
  appOwned: boolean
}

/** A window in the tree. Panes are sub-items only when there is more than one. */
export interface WindowNode {
  /** Stable React key: group and window index, which are unique together. */
  key: string
  index: number
  name: string
  panes: PaneNode[]
}

/** A tmux session (really a session *group*) in the tree. */
export interface SessionNode {
  key: string
  windows: WindowNode[]
  /** No member of this group is a session the user made; see `chooseSession`. */
  appOnly: boolean
}

/**
 * Build the sidebar tree from snapshot rows.
 *
 * Arrival order is preserved at every level. The daemon has already
 * deduplicated by pane id and sorted by (group, window index, pane index),
 * which is the order the user sees on screen; sorting again here could only
 * disagree with it. Rows for a group or window that arrive non-contiguously
 * (which the current daemon cannot produce) are merged into the node that
 * already exists rather than starting a second one, so a duplicate label is
 * impossible even if that ordering guarantee is ever lost.
 */
export function groupRows(rows: readonly SnapshotRow[]): SessionNode[] {
  const sessions = new Map<string, SessionNode>()
  const windows = new Map<string, WindowNode>()
  for (const row of rows) {
    let session = sessions.get(row.groupKey)
    if (!session) {
      session = { key: row.groupKey, windows: [], appOnly: true }
      sessions.set(row.groupKey, session)
    }
    // One non-app row anywhere in the group means the group has a session the
    // user can be attached to under its own name.
    if (!row.appOwned) session.appOnly = false

    // A colon cannot appear in a tmux session name -- tmux uses it as the
    // session:window separator and rejects one in a name -- so this is a
    // unique React key and still readable in devtools.
    const windowKey = `${row.groupKey}:${row.windowIndex}`
    let win = windows.get(windowKey)
    if (!win) {
      win = { key: windowKey, index: row.windowIndex, name: row.windowName, panes: [] }
      windows.set(windowKey, win)
      session.windows.push(win)
    }
    win.panes.push({
      paneId: row.paneId,
      paneIndex: row.paneIndex,
      command: row.command,
      title: row.title,
      label: row.label,
      active: row.paneActive,
      appOwned: row.appOwned,
    })
  }
  return [...sessions.values()]
}

/** Where a pane sits in the tree, for the breadcrumb and the highlight. */
export interface PaneLocation {
  session: SessionNode
  window: WindowNode
  pane: PaneNode
}

/** Locate a pane id in the tree, or null if the snapshot no longer has it. */
export function findPane(
  groups: readonly SessionNode[],
  paneId: string | null,
): PaneLocation | null {
  if (!paneId) return null
  for (const session of groups) {
    for (const window of session.windows) {
      for (const pane of window.panes) {
        if (pane.paneId === paneId) return { session, window, pane }
      }
    }
  }
  return null
}

/**
 * The pane a click on the *window* row should land on: tmux's active pane for
 * that window, else the first in layout order.
 *
 * Selecting the window's own active pane is what makes clicking a window
 * equivalent to `select-window` alone, which is what the row promises.
 */
export function windowTarget(window: WindowNode): string | null {
  return (window.panes.find((p) => p.active) ?? window.panes[0])?.paneId ?? null
}

/**
 * Which base session a fresh tab should attach to.
 *
 * `preferred` (the `?session=` query, or the one this tab used last) wins if it
 * is still in the snapshot. Otherwise the first group that still has a session
 * of its own: a group whose only surviving members are app-created sessions
 * still shows its panes -- those are running agents -- but `?session=work`
 * would 404 on the socket, because the daemon checks `has-session -t =work`
 * and the namesake is what died.
 */
export function chooseSession(
  groups: readonly SessionNode[],
  preferred?: string | null,
): string | null {
  if (preferred && groups.some((g) => g.key === preferred)) return preferred
  return groups.find((g) => !g.appOnly)?.key ?? null
}

/** What the tab knows about which session it should be attached to. */
export interface SessionChoice {
  /** The session the user last clicked, or the one remembered from a reload. */
  picked: string | null
  /** `?session=`, which pins the tab and disables every rule below. */
  forced: string | null
  /** Whether the snapshot has ever answered. */
  loaded: boolean
}

/**
 * The base session for the socket: `/ws?session=`.
 *
 * Resolved from the snapshot rather than guessed, and resolved during render
 * rather than in an effect, so no frame is drawn against a session that has
 * already been ruled out.
 *
 * A remembered session is trusted until a *loaded* snapshot contradicts it. On
 * a reload that means the terminal attaches immediately instead of waiting out
 * a poll; when the session really has died it means the tab moves to one that
 * exists instead of reconnecting forever against the daemon's 404.
 */
export function resolveSession(
  groups: readonly SessionNode[],
  { picked, forced, loaded }: SessionChoice,
): string | null {
  if (forced) return forced
  if (picked && (!loaded || groups.some((g) => g.key === picked))) return picked
  return chooseSession(groups, picked)
}

/**
 * Shallow field-wise equality over two snapshots' rows.
 *
 * Every field the wire carries, not only the ones something renders today: a
 * row that compares equal keeps the previous tree object and React reconciles
 * nothing, so a field left out here is a field that can change in tmux and
 * never reach the DOM. A pane title does exactly that -- Claude Code rewrites
 * it when its task changes and nothing else about the pane moves.
 */
function rowsEqual(a: readonly SnapshotRow[], b: readonly SnapshotRow[]): boolean {
  if (a.length !== b.length) return false
  return a.every((x, i) => {
    const y = b[i]
    return (
      x.paneId === y.paneId &&
      x.groupKey === y.groupKey &&
      x.sessionId === y.sessionId &&
      x.sessionName === y.sessionName &&
      x.paneIndex === y.paneIndex &&
      x.windowIndex === y.windowIndex &&
      x.windowName === y.windowName &&
      x.command === y.command &&
      x.title === y.title &&
      x.label === y.label &&
      x.paneActive === y.paneActive &&
      x.appOwned === y.appOwned &&
      x.agentState === y.agentState &&
      x.finishedAt === y.finishedAt &&
      questionsEqual(x.question, y.question)
    )
  })
}

/**
 * Whether two panes are asking the same thing.
 *
 * `question` is the one field on the wire that is not a scalar, so it needs its
 * own comparison: `x.question === y.question` is false on every poll, since the
 * daemon parses a fresh object out of the capture each time, and the tree would
 * be rebuilt every 1.5s for every blocked pane.
 */
function questionsEqual(a?: SnapshotQuestion, b?: SnapshotQuestion): boolean {
  if (a === undefined || b === undefined) return a === b
  const ac = a.choices ?? []
  const bc = b.choices ?? []
  return a.text === b.text && ac.length === bc.length && ac.every((c, i) => c === bc[i])
}

/** Everything the sidebar needs to render, including why it might be wrong. */
export interface SnapshotState {
  /** The tree. Empty until the first successful poll. */
  groups: SessionNode[]
  /** The rows behind it, in server order. */
  rows: SnapshotRow[]
  /** At least one poll has succeeded, so `groups` is an answer and not a guess. */
  loaded: boolean
  /**
   * These rows are older than they should be: consecutive polls have failed, or
   * the daemon is reporting its own tmux poll as failing. Deliberately not set
   * by a single bad poll -- see TROUBLE_BEFORE_STALE.
   */
  stale: boolean
  /** The most recent failure, whether or not it was enough to set `stale`. */
  error: string | null
  /** Consecutive troubled polls; 0 after any clean one. */
  trouble: number
  /** This device is no longer enrolled. Polling has stopped for good. */
  unauthorized: boolean
}

const EMPTY_STATE: SnapshotState = {
  groups: [],
  rows: [],
  loaded: false,
  stale: false,
  error: null,
  trouble: 0,
  unauthorized: false,
}

export interface SnapshotPollerOptions {
  onState: (state: SnapshotState) => void
  /** Defaults to `fetchSnapshot`. */
  fetcher?: (signal: AbortSignal) => Promise<SnapshotPayload>
  intervalMs?: number
  timeoutMs?: number
  /** Defaults to `document.hidden`; see the class comment. */
  isHidden?: () => boolean
}

/**
 * The polling loop, with no React in it so that the parts with timers and
 * failure rules can be tested without a renderer.
 *
 * Two behaviours are worth stating out loud:
 *
 * **A hidden tab does not poll.** Not to save the daemon a fork -- it polls
 * tmux on its own schedule regardless of whether anyone is looking -- but
 * because a phone in a pocket has no use for a sidebar it is not showing, and
 * browsers throttle background timers to something between "one a minute" and
 * "never" anyway. Stopping deliberately and refreshing on the way back is
 * honest about that, where a throttled interval would leave the first visible
 * frame showing a minute-old tree. Nothing is queued while hidden: the next
 * poll is a fresh read of a cache the daemon has been keeping current.
 *
 * **Only one request is ever in flight.** The next poll is scheduled after the
 * previous one settles, not on a fixed interval, so a slow response cannot
 * stack requests behind it.
 */
export class SnapshotPoller {
  readonly #opts: SnapshotPollerOptions
  readonly #fetcher: (signal: AbortSignal) => Promise<SnapshotPayload>
  readonly #intervalMs: number
  readonly #timeoutMs: number
  readonly #isHidden: () => boolean

  #state: SnapshotState = EMPTY_STATE
  #timer: ReturnType<typeof setTimeout> | null = null
  #inFlight: AbortController | null = null
  #waitingForVisible = false
  #stopped = false

  constructor(opts: SnapshotPollerOptions) {
    this.#opts = opts
    this.#fetcher = opts.fetcher ?? ((signal) => fetchSnapshot(signal))
    this.#intervalMs = opts.intervalMs ?? POLL_INTERVAL_MS
    this.#timeoutMs = opts.timeoutMs ?? POLL_TIMEOUT_MS
    this.#isHidden =
      opts.isHidden ?? (() => typeof document !== 'undefined' && document.hidden === true)
  }

  get state(): SnapshotState {
    return this.#state
  }

  /** Poll now, then keep polling. */
  start(): void {
    if (this.#stopped) return
    void this.#poll()
  }

  /** Stop for good: no further request, timer or state. Idempotent. */
  stop(): void {
    if (this.#stopped) return
    this.#stopped = true
    if (this.#timer) clearTimeout(this.#timer)
    this.#timer = null
    this.#inFlight?.abort()
    this.#inFlight = null
  }

  /** The tab became visible again. Polls immediately if the loop was parked. */
  wake(): void {
    if (this.#stopped || !this.#waitingForVisible) return
    this.#waitingForVisible = false
    void this.#poll()
  }

  /** Poll now instead of waiting out the interval; the Retry button. */
  refresh(): void {
    if (this.#stopped) return
    if (this.#timer) clearTimeout(this.#timer)
    this.#timer = null
    this.#waitingForVisible = false
    void this.#poll()
  }

  async #poll(): Promise<void> {
    if (this.#stopped || this.#inFlight) return
    // Checked here rather than where the timer is armed, because the tab is
    // usually hidden *after* the next poll was already scheduled -- switching
    // away is the whole event. Parking without a timer is what makes the loop
    // stop rather than run at whatever rate the browser throttles it to; wake()
    // restarts it.
    if (this.#isHidden()) {
      this.#waitingForVisible = true
      return
    }
    const controller = new AbortController()
    this.#inFlight = controller
    const timeout = setTimeout(() => controller.abort(), this.#timeoutMs)
    try {
      const payload = await this.#fetcher(controller.signal)
      if (!this.#stopped) this.#succeeded(payload)
    } catch (err) {
      if (!this.#stopped) this.#failed(err)
    } finally {
      clearTimeout(timeout)
      if (this.#inFlight === controller) this.#inFlight = null
      this.#schedule()
    }
  }

  #succeeded(payload: SnapshotPayload): void {
    const prev = this.#state
    // A tree that did not change keeps its identity, so React reconciles
    // nothing and the DOM under the cursor is not replaced every 1.5s.
    const unchanged = prev.loaded && rowsEqual(prev.rows, payload.panes)
    const trouble = payload.stale ? prev.trouble + 1 : 0
    this.#emit({
      rows: unchanged ? prev.rows : payload.panes,
      groups: unchanged ? prev.groups : groupRows(payload.panes),
      loaded: true,
      stale: trouble >= TROUBLE_BEFORE_STALE,
      error: payload.error,
      trouble,
      unauthorized: false,
    })
  }

  #failed(err: unknown): void {
    const prev = this.#state
    const message = err instanceof Error ? err.message : String(err)
    const unauthorized = err instanceof SnapshotFetchError && err.unauthorized
    const trouble = prev.trouble + 1
    this.#emit({
      // The last good tree survives every failure. This is the rule the whole
      // file exists to keep: a sidebar that was correct 1.5s ago stays on
      // screen through a tmux restart or a lost request.
      rows: prev.rows,
      groups: prev.groups,
      loaded: prev.loaded,
      stale: trouble >= TROUBLE_BEFORE_STALE,
      error: message,
      trouble,
      unauthorized,
    })
    // Nothing this loop can do will make a revoked cookie work; retrying it
    // every 1.5s until the tab closes only moves the daemon's rate limiter.
    if (unauthorized) this.stop()
  }

  #schedule(): void {
    if (this.#stopped || this.#timer || this.#inFlight) return
    this.#timer = setTimeout(() => {
      this.#timer = null
      void this.#poll()
    }, this.#intervalMs)
  }

  #emit(state: SnapshotState): void {
    this.#state = state
    this.#opts.onState(state)
  }
}

export interface UseSnapshotOptions {
  fetcher?: (signal: AbortSignal) => Promise<SnapshotPayload>
  intervalMs?: number
}

export interface UseSnapshotResult extends SnapshotState {
  /** Poll immediately; the empty state's Retry button. */
  refresh: () => void
}

/**
 * Subscribe to the tmux snapshot for as long as the component is mounted.
 *
 * The visibility listener lives here rather than in SnapshotPoller so the
 * poller itself stays free of the DOM and testable under vitest's node
 * environment.
 */
export function useSnapshot(options: UseSnapshotOptions = {}): UseSnapshotResult {
  const [state, setState] = useState<SnapshotState>(EMPTY_STATE)
  const poller = useRef<SnapshotPoller | null>(null)
  const { fetcher, intervalMs } = options

  useEffect(() => {
    const p = new SnapshotPoller({ onState: setState, fetcher, intervalMs })
    poller.current = p
    const onVisibility = () => {
      if (!document.hidden) p.wake()
    }
    document.addEventListener('visibilitychange', onVisibility)
    p.start()
    return () => {
      document.removeEventListener('visibilitychange', onVisibility)
      p.stop()
      poller.current = null
    }
  }, [fetcher, intervalMs])

  const refresh = useCallback(() => poller.current?.refresh(), [])
  return { ...state, refresh }
}
