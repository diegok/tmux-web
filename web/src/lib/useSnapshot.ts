/**
 * The sidebar's view of tmux: `GET /api/snapshot`, polled, grouped into the
 * session → window → pane tree the sidebar renders.
 *
 * The daemon already forks one `tmux list-panes -a` every 1.5s and hands every
 * tab the same cached answer, so this poll is an HTTP request against memory,
 * not a tmux fork. Polling faster than the daemon refreshes would only burn
 * requests to re-read the same bytes; polling slower would add lag to a number
 * the design already accepts as the cost of not running a control-mode sidecar.
 * Hence exactly `POLL_INTERVAL_MS` -- and `HIDDEN_POLL_INTERVAL_MS` once the
 * tab is in the background, where the tab badge is the only reader left and a
 * minute is all the browser will run a timer at anyway.
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
 *
 * The one exception, and it is deliberate: **`seen`** -- which finished runs
 * this device has already been shown. That is not a model of tmux, it is a
 * model of *this browser*, which is exactly why it cannot live on the daemon:
 * the phone must keep its badge after the laptop has cleared its own. It is
 * keyed on the tmux server generation so that it cannot outlive the panes it
 * describes. See `isDone`.
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
 * Poll period while the tab is in the background.
 *
 * A hidden tab keeps polling, slowly, because that is the only way the tab
 * badge can fire: it is the answer to "do I need to go back to it?", and the
 * tab it is asked about is by definition not the one being looked at. A loop
 * that parked while hidden could never change the count in the one situation
 * the count is for.
 *
 * **A minute, because a minute is what the browser will actually deliver.**
 * Chromium throttles a hidden page's timers to roughly one a second after ten
 * seconds, and after five minutes hidden switches to intensive throttling,
 * where timers are aligned to a one-minute wall-clock grid; Firefox and Safari
 * clamp to about one a second and suspend outright when the device sleeps. So
 * anything under 60s is a number the browser rounds up rather than a cadence:
 * 30s would ask for twice the requests and, past the five-minute mark, still
 * arrive once a minute. What to expect in practice is one poll a minute, up to
 * a minute late where the grid falls, and nothing at all on a locked phone or a
 * discarded tab -- see `SnapshotPoller.wake`, which is what covers the gap the
 * moment you look.
 *
 * Not an option on the poller, unlike `POLL_INTERVAL_MS`: the visible cadence
 * tracks the daemon's refresh and a caller can reasonably know better, while
 * this one tracks what the browser is willing to run, which no caller knows
 * better than this module.
 */
export const HIDDEN_POLL_INTERVAL_MS = 60_000

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
  /** Row came from a session this app created (`@tmux_web_owned`). */
  appOwned: boolean
  /**
   * `@tmux_web_label`: a name given to this pane, or "" when unset -- and also ""
   * when what was set sanitised away to nothing. The daemon repairs this field
   * rather than trusting it (control bytes to spaces, invalid UTF-8 to U+FFFD,
   * capped and trimmed): it is the one thing on the wire that anything holding
   * the tmux socket can write, agent integrations included.
   */
  label: string
  /**
   * `@N`: the window's identity, and what every window operation targets.
   *
   * Here for the same reason `sessionId` is: `windowIndex` is a *position*.
   * tmux renumbers indices on `move-window` and reuses them after a kill, so a
   * rename or a kill addressed by index can land on a window other than the one
   * the sidebar was showing -- and the daemon refuses it anyway, since
   * `PATCH`/`DELETE /api/windows/{id}` validate an `@N`.
   *
   * "" only from a daemon too old to send the field. Nothing that needs an
   * address is offered for such a window; see `NO_WINDOW_ID` in manage.ts.
   */
  windowId: string
  /** Position in the session, in display order. Not the id -- see `windowId`. */
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
   * `#{pane_mode}`: the name of the **top layer** of the pane's mode stack, and
   * `""` for a pane in no mode at all -- which is most of them.
   *
   * tmux's own mode names, measured rather than guessed: `copy-mode`,
   * `view-mode`, `tree-mode`, `clock-mode`, `options-mode`. Modes stack and
   * this names only the topmost, which is the whole reason it is here rather
   * than a boolean: the header's one copy-mode control can leave a copy layer
   * and cannot leave a choose-tree, and `#{pane_in_mode}` -- a count -- cannot
   * tell those apart. `inCopyMode` in `copyMode.ts` is the rule; nothing else
   * should read this field directly.
   *
   * It rides the same `list-panes` the rest of this record comes from, so it
   * costs no extra fork.
   */
  paneMode: string
  /**
   * The pane's working directory, as of the poll that produced this row.
   *
   * It rides a tagged block of its own in the daemon's one tmux invocation,
   * not the record the fields above come from: tmux sanitises titles and
   * refuses a newline in a session or window name, but hands
   * `pane_current_path` over exactly as it is, and a second such field in one
   * record shifts every field after it. See `PathFormat` on the Go side.
   *
   * "" when the poll carried no path for the pane. Nothing renders it, and
   * nothing may treat it as current: it is up to a poll interval old and can
   * name a directory that has since been removed.
   */
  path: string
  /**
   * The pane's git branch, `"@<7-hex>"` when its HEAD is detached, and `""`
   * when `path` is not inside a work tree at all -- which is most panes.
   *
   * Read off `.git/HEAD` by the daemon, on its own schedule rather than the
   * poll's, so it is stale in exactly the way `path` is. Nothing the agent says
   * reaches it: the filesystem is the only authority on which branch a
   * directory is on.
   */
  branch: string
  /**
   * What the agent's own integration says it is doing, or "".
   *
   * "" covers every pane no integration reports for -- not an agent, no
   * integration installed, a report gone stale -- so it is far more often empty
   * than not. The daemon sanitises and caps it on the way in and again on the
   * way out; like `label`, it is a value something other than the daemon wrote.
   */
  activity: string
  /**
   * Which authority decided `agentState`: `"event"` (the agent's own report),
   * `"screen"` (the churn classifier), or `""` when nothing did.
   *
   * On the wire mainly so that tests can see it: a report and the classifier
   * agreeing on `working` is indistinguishable from the precedence being
   * backwards. The UI may put it in a tooltip and must **not** branch a row's
   * appearance on it -- two visibly different kinds of state dot teach the user
   * to trust one and ignore the other.
   */
  stateSource: string
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
  /**
   * The tmux server's generation (`#{start_time}`), or "" when no tmux server
   * is running -- and "" too from a daemon too old to send the field.
   *
   * Every `seen` key is prefixed with it, because pane ids restart at `%0` when
   * the tmux server does: without it a remembered `%3` from the previous server
   * silently suppresses the done badge on an unrelated new pane.
   */
  serverStart: string
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
  const raw = body as {
    panes?: unknown
    serverStart?: unknown
    stale?: unknown
    error?: unknown
  }
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
    // Anything that is not a string is no generation at all, and "" is the
    // documented "cannot key a `seen` entry safely" value -- see `isDone`.
    serverStart: typeof raw.serverStart === 'string' ? raw.serverStart : '',
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
  /** `@tmux_web_label`, or "" -- the only one of the three the user chose. */
  label: string
  /**
   * `SnapshotRow.paneMode`: the top layer of the pane's mode stack, or "".
   *
   * Carried onto the node because the header's copy-mode control reads it for
   * the pane the tab is looking at, and `findPane` is how it gets there. When
   * `branch` was added to the wire it was left out of this interface and out of
   * `groupRows`, so `pane.branch` did not exist and nobody noticed until
   * something tried to render it.
   */
  paneMode: string
  /**
   * `SnapshotRow.path`: the pane's working directory as of the poll, or "".
   *
   * Carried through the tree though nothing renders it, so that the first
   * thing to want it does not have to go back to the daemon for a value the
   * poll already brought. Stale by up to one interval; never an authority.
   */
  path: string
  /**
   * `SnapshotRow.branch`: the branch `path` is on, `"@<7-hex>"` for a detached
   * HEAD, `""` for a directory that is not in a work tree.
   *
   * Stale in exactly the way `path` is, and for the same reason -- it is read
   * off the filesystem on the daemon's own schedule. The sidebar's chip is the
   * only thing that renders it.
   */
  branch: string
  /** tmux's active pane within this window. */
  active: boolean
  appOwned: boolean
  /** `SnapshotRow.activity`: what the agent says it is doing, or "". */
  activity: string
  /** `SnapshotRow.stateSource`: "event", "screen" or "". Never styles the row. */
  stateSource: string
  /** `SnapshotRow.agentState`, carried through unchanged. "" is not a state. */
  agentState: string
  /** `SnapshotRow.finishedAt`: unix ms of the last working -> idle edge, or 0. */
  finishedAt: number
  /** `SnapshotRow.question`, present only on a blocked pane the daemon read. */
  question?: SnapshotQuestion
}

/** A window in the tree. Panes are sub-items only when there is more than one. */
export interface WindowNode {
  /**
   * Stable React key: the group and the window's `@N`, falling back to its
   * index where the wire carried no id. See `groupRows`.
   */
  key: string
  /**
   * `@N`, and "" from a daemon too old to send one. What a rename or a kill
   * targets -- and what their absence from the menu is decided on.
   */
  id: string
  index: number
  name: string
  panes: PaneNode[]
}

/** A tmux session (really a session *group*) in the tree. */
export interface SessionNode {
  /**
   * The session *group*: the identity and the React key, and what a sidebar
   * click carries.
   *
   * Neither what the sidebar prints nor an address. tmux freezes
   * `session_group` at the name the group was created under, so after a rename
   * this is the *old* name forever -- see `name` for what to show and
   * `sessionId` for what to send. `?session=` carries the id; `attachTarget`
   * is where the two part company.
   */
  key: string
  /**
   * The live `session_name` to display, falling back to `key` when the rows
   * carry none.
   *
   * Taken from the group's first non-app-owned row, because that is the session
   * the user made and named; a group's app-owned members are throwaways this app
   * created and their names are generated. When every member is app-owned the
   * first row still answers, which is better than printing a stale group key.
   */
  name: string
  /**
   * `$N`: what every session-level management call targets.
   *
   * Chosen the same way as `name`, from the group's first non-app-owned row,
   * and for the same reason -- that is the session the user made, and the one
   * the daemon will consent to rename or kill. Never the group key: tmux keeps
   * the pre-rename name there, so `kill-session -t '=work3'` fails on a session
   * living happily as `api`.
   */
  sessionId: string
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
      session = {
        key: row.groupKey,
        // A placeholder only until a row supplies one; the group key is the
        // pre-rename name, so it is the last resort rather than the default.
        name: row.sessionName || row.groupKey,
        sessionId: row.sessionId,
        windows: [],
        appOnly: true,
      }
      sessions.set(row.groupKey, session)
    }
    // One non-app row anywhere in the group means the group has a session the
    // user can be attached to under its own name.
    if (!row.appOwned) {
      // The first such row names the group. Later ones do not overwrite it:
      // `tmux new -t work` puts a second real session in the group, and a label
      // that flipped between two live names every poll would be worse than one
      // that picks the first and stays there.
      if (session.appOnly) {
        session.name = row.sessionName || row.groupKey
        // The id the user's session is addressed by, taken from the same row as
        // the name so that the two cannot describe different sessions: a
        // rename dialog titled "work" must not send `$4`.
        session.sessionId = row.sessionId
      }
      session.appOnly = false
    }

    // `@N` and not the index, because the index is a position: tmux renumbers
    // on `move-window` and reuses an index after a kill, so a key built on one
    // is a key two different windows wear one after the other -- and the
    // sidebar sorts by index precisely because it moves. The id is stable for
    // the window's life. A row that carries none (a daemon too old to send the
    // field) falls back to the index rather than keying every window in the
    // snapshot on "" and merging the whole session into one row; `id` stays ""
    // there, which is what keeps rename and kill off it.
    //
    // Still scoped to the group. A colon cannot appear in a tmux session name
    // -- tmux uses it as the session:window separator and rejects one in a name
    // -- so this is unique across the tree and still readable in devtools. The
    // scoping is what the index key already had and is kept deliberately: an id
    // is unique per *server*, so an unscoped key would make grouping depend on
    // the daemon deduping a window that two sessions share (`Dedupe` does, by
    // pane id) rather than on anything this function can see.
    const windowKey = `${row.groupKey}:${row.windowId || row.windowIndex}`
    let win = windows.get(windowKey)
    if (!win) {
      win = {
        key: windowKey,
        id: row.windowId,
        index: row.windowIndex,
        name: row.windowName,
        panes: [],
      }
      windows.set(windowKey, win)
      session.windows.push(win)
    }
    win.panes.push({
      paneId: row.paneId,
      paneIndex: row.paneIndex,
      command: row.command,
      title: row.title,
      label: row.label,
      paneMode: row.paneMode,
      path: row.path,
      branch: row.branch,
      active: row.paneActive,
      appOwned: row.appOwned,
      activity: row.activity,
      stateSource: row.stateSource,
      agentState: row.agentState,
      finishedAt: row.finishedAt,
      question: row.question,
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
 * Where a tab should go when the pane it was pinned to has left the snapshot.
 * `null` means "nowhere better", and the caller must then stay put.
 *
 * ## Why this follows rather than reports
 *
 * A tab does not stop looking at tmux when its pane dies. tmux moves the
 * client to another pane in the window, or to another window, and the keyboard
 * goes with it -- so the terminal on screen is live and typing into it works.
 * What goes stale is only *this app's* record: `TerminalSession` remembers the
 * last pane it asked for and nothing ever corrects it, so the breadcrumb, the
 * sidebar highlight and `useSeenPanes` all go on naming a pane that no longer
 * exists while the user reads a different, living one.
 *
 * That is the part that is not honest. "%75 is gone" is a true sentence about
 * %75 and a false answer to the question a breadcrumb exists to answer, which
 * is *where am I*. Re-pinning is what makes the two agree again; it is a
 * correction, not a convenience, and the caller still has to say out loud that
 * the pane closed.
 *
 * ## The order, and why it is the window first
 *
 * The window is where the work is. Closing an editor in `2: api` should leave
 * you in `2: api`, not at the top of the session -- and the window's own active
 * pane is exactly where tmux moved the client, so following it agrees with the
 * screen the user is already looking at instead of overriding it.
 *
 * Only when the window went too -- the editor was the last pane in it -- does
 * this fall back to the session's first window.
 *
 * ## And when the session went too
 *
 * Closing the last tab of a session destroys the session, and with it the group
 * this tab was attached through. The tab does not sit there disconnected:
 * `resolveSession` has already moved it to another session and the terminal is
 * showing a live pane of that one. So there *is* somewhere better than nowhere,
 * and `attached` -- the group key the tab is attached to now -- is what names
 * it. Without this the app is pointing into a session that no longer exists: no
 * row is highlighted, the breadcrumb has nothing to describe, and the user is
 * looking at a terminal with no indication anywhere of which pane it is.
 *
 * It is the last candidate on purpose. While the dead pane's own session is
 * still in the snapshot it is always the better answer, and `attached` is
 * usually that same session anyway -- this only differs in the case that made
 * it necessary.
 *
 * ## Why "nowhere better" is a real answer
 *
 * A snapshot with no panes at all is not evidence that every pane died: the
 * daemon reports its own failed tmux poll by returning an error with no rows.
 * Moving on that would yank a tab off a perfectly good pane because a poll
 * hiccuped. Returning null keeps the caller where it is, which is the one
 * behaviour that is safe to take on a snapshot that may be wrong.
 *
 * A tab pinned by `?session=` reaches the same answer by the same route rather
 * than by a rule of its own: the name it is pinned to is not a group key the
 * snapshot knows, so the fallback finds no session and the tab stays where it
 * was told to be.
 */
export function succeedPane(
  groups: readonly SessionNode[],
  gone: PaneLocation,
  attached: string | null,
): PaneLocation | null {
  const own = groups.find((g) => g.key === gone.session.key)
  const session = own ?? groups.find((g) => attached !== null && g.key === attached)
  if (!session) return null

  // The window's id is the identity; `key` is the fallback for a daemon too
  // old to send `@N`, and is what `groupRows` already keys the tree on.
  //
  // Looked for only in the pane's *own* session, and `own` is what says so.
  // "The window this pane was in" is a sentence about one session, and a
  // fallback to a different one has no such window by construction -- so the
  // fallback lands on the new session's first window, which is the only thing
  // it could mean. tmux would make the guard unreachable, since `@N` is unique
  // per server, but the rule does not depend on that being true and should not
  // read as though it does.
  const same = own?.windows.find((w) =>
    gone.window.id !== '' ? w.id === gone.window.id : w.key === gone.window.key,
  )
  for (const window of [same, session.windows[0]]) {
    if (!window) continue
    const paneId = windowTarget(window)
    // Never hand back the pane that is gone. It cannot be in `groups` -- that
    // is what made this call happen -- but a caller that re-selected it would
    // loop on every poll, so the guard is here rather than at each call site.
    if (!paneId || paneId === gone.pane.paneId) continue
    const pane = window.panes.find((p) => p.paneId === paneId)
    if (pane) return { session, window, pane }
  }
  return null
}

/**
 * Where a tab landed, for the sentence that tells the user it moved.
 *
 * `2: api › vim` normally, because the session did not change and naming it
 * again would just be the breadcrumb read aloud. When the move crossed sessions
 * -- which only happens because the old one was destroyed -- the session goes
 * in front, since that is the part of "where am I" that changed and the part
 * the user has no other way to notice.
 */
export function landedLabel(to: PaneLocation, from: PaneLocation): string {
  const where = `${to.window.index}: ${to.window.name} › ${to.pane.command}`
  return to.session.key === from.session.key ? where : `${to.session.name} › ${where}`
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

// --- which agent needs you --------------------------------------------------
//
// The one question the app exists to answer. The daemon reports what each agent
// pane is doing; everything below turns that into the four things a row can say
// and rolls them up the tree, because a collapsed sidebar is the case that has
// to work.

/**
 * The states a row can display, **most urgent first**.
 *
 * The order is the roll-up: a window shows the most urgent state among its
 * panes and a session among its windows, so a blocked agent three levels down
 * is visible without expanding anything.
 *
 * `done` is not one of the daemon's states. The daemon reports a *timestamp*,
 * `finishedAt`, and `done` is what this browser makes of it -- see `isDone`.
 */
export const STATE_ORDER = ['blocked', 'done', 'working', 'idle'] as const

/** One of `STATE_ORDER`. `""` is separate: it means nothing computed a state. */
export type DisplayState = (typeof STATE_ORDER)[number]

/**
 * The most urgent of a set of states, or `""` when none of them is a state.
 *
 * `""` is skipped rather than ranked. A window of three shells and one blocked
 * agent reads blocked; a window of three shells reads nothing at all, which is
 * the point of the daemon leaving `agentState` empty for a pane it did not
 * classify.
 */
export function mostUrgent(states: Iterable<DisplayState | ''>): DisplayState | '' {
  // `number`, not the literal 4 a const tuple's length infers to.
  let best: number = STATE_ORDER.length
  for (const state of states) {
    const rank = (STATE_ORDER as readonly string[]).indexOf(state)
    if (rank >= 0 && rank < best) best = rank
  }
  return best === STATE_ORDER.length ? '' : STATE_ORDER[best]
}

/**
 * A pane that finished since this browser last looked at it.
 *
 * `finishedAt` is the daemon's unix-ms stamp of the last working -> idle edge
 * and `seen` is this device's memory of the one it has already been shown, so
 * the badge clears per device: the phone keeps it after the laptop has cleared
 * its own, and no per-device state ever reaches the daemon.
 *
 * **Without a server generation there is no `done`.** The key is
 * `${serverStart}:${paneId}` because pane ids restart at `%0` when the tmux
 * server does; with `serverStart` empty every pane would share one unqualified
 * key across server restarts, and a stale `%3` would silently suppress the
 * badge on an unrelated new pane. Refusing to compute it is the honest failure:
 * a missing badge costs a glance, a wrong one costs trust in all of them.
 */
export function isDone(
  pane: Pick<PaneNode, 'paneId' | 'finishedAt'>,
  serverStart: string,
  seen: SeenMap,
): boolean {
  if (serverStart === '' || pane.finishedAt <= 0) return false
  return pane.finishedAt > (seen[seenKey(serverStart, pane.paneId)] ?? 0)
}

/**
 * What one pane's dot says.
 *
 * `blocked` and `working` are the daemon's own answers and are reported as
 * given. `done` refines `idle`: a finished run this browser has not looked at
 * is the thing you want to be told about, and one it has is simply idle.
 *
 * A *working* pane is working even when it carries an unseen `finishedAt` --
 * that stamp is the end of an earlier run, and the pane has since started
 * another. Nothing is lost by saying so: the next working -> idle edge stamps a
 * newer `finishedAt` and the badge comes back.
 *
 * Any other value, `""` included, is not a state. The daemon owns the list of
 * what counts as an agent; a value this file does not know is a wire change,
 * and rendering a dot for it would be inventing a state.
 */
export function paneState(
  pane: Pick<PaneNode, 'paneId' | 'agentState' | 'finishedAt'>,
  serverStart: string,
  seen: SeenMap,
): DisplayState | '' {
  switch (pane.agentState) {
    case 'blocked':
      return 'blocked'
    case 'working':
      return 'working'
    case 'idle':
      return isDone(pane, serverStart, seen) ? 'done' : 'idle'
    default:
      return ''
  }
}

/** The most urgent state among a window's panes. */
export function windowState(
  window: WindowNode,
  serverStart: string,
  seen: SeenMap,
): DisplayState | '' {
  return mostUrgent(window.panes.map((p) => paneState(p, serverStart, seen)))
}

/** The most urgent state among a session's windows. */
export function sessionState(
  session: SessionNode,
  serverStart: string,
  seen: SeenMap,
): DisplayState | '' {
  return mostUrgent(session.windows.map((w) => windowState(w, serverStart, seen)))
}

// --- what this browser has already looked at --------------------------------

/** Where `seen` lives. One key: the map is small and read whole on every load. */
export const SEEN_STORAGE_KEY = 'tmux-web:seen'

/**
 * `finishedAt` of the newest finish this device has been shown, per pane.
 *
 * Keys are `${serverStart}:${paneId}` -- never a bare pane id. Values are the
 * daemon's unix-ms stamps, compared with `>` so that a stamp equal to the one
 * already seen is not a new finish.
 */
export type SeenMap = Readonly<Record<string, number>>

/** The `seen` key for one pane under one tmux server generation. */
export function seenKey(serverStart: string, paneId: string): string {
  return `${serverStart}:${paneId}`
}

/** Just enough of `Storage` to be swapped for a fake in a test. */
export interface StorageLike {
  getItem(key: string): string | null
  setItem(key: string, value: string): void
}

/**
 * `localStorage`, or null where there is none.
 *
 * Absent under vitest's node environment, and a *throw* rather than an absence
 * in a browser with site data blocked -- Safari's private mode is the usual
 * one. Both mean the same thing here: the device forgets which finishes it has
 * been shown, every badge reappears once, and nothing else changes.
 */
function defaultStorage(): StorageLike | null {
  try {
    return globalThis.localStorage ?? null
  } catch {
    return null
  }
}

/** Load `seen`. A corrupt or foreign value reads as empty rather than throwing. */
export function readSeen(
  storage: StorageLike | null = defaultStorage(),
): SeenMap {
  try {
    const raw = storage?.getItem(SEEN_STORAGE_KEY)
    if (!raw) return {}
    const parsed: unknown = JSON.parse(raw)
    if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) return {}
    const out: Record<string, number> = {}
    for (const [k, v] of Object.entries(parsed)) {
      if (typeof v === 'number' && Number.isFinite(v)) out[k] = v
    }
    return out
  } catch {
    return {}
  }
}

/** Persist `seen`. A storage that refuses the write costs the memory, nothing more. */
export function writeSeen(
  seen: SeenMap,
  storage: StorageLike | null = defaultStorage(),
): void {
  try {
    storage?.setItem(SEEN_STORAGE_KEY, JSON.stringify(seen))
  } catch {
    // Quota, or a browser that blocked site data. See defaultStorage.
  }
}

/**
 * Record that this device has been shown `finishedAt` for one pane.
 *
 * Returns the map **unchanged, by reference** when there is nothing to record,
 * so a caller can use identity to decide whether to persist and re-render. A
 * stamp that is not newer than the one already stored is nothing to record --
 * including `0`, which is what a pane that has never finished carries and what
 * every pane carries again after a daemon restart rebuilds its map from
 * nothing. Lowering an entry there would make one already-seen finish
 * announceable a second time.
 *
 * Writing also drops entries from other tmux server generations: they can never
 * match a lookup again, and this is the only moment that knows which generation
 * is current.
 */
export function markSeen(
  seen: SeenMap,
  serverStart: string,
  paneId: string,
  finishedAt: number,
): SeenMap {
  if (serverStart === '') return seen
  const key = seenKey(serverStart, paneId)
  if (finishedAt <= (seen[key] ?? 0)) return seen
  const prefix = `${serverStart}:`
  const next: Record<string, number> = {}
  for (const [k, v] of Object.entries(seen)) {
    if (k.startsWith(prefix)) next[k] = v
  }
  next[key] = finishedAt
  return next
}

/**
 * `seen` with the pane this tab is looking at marked as seen.
 *
 * Viewing is what clears a done badge, and it is the only thing that does: the
 * daemon is never told, so every device clears its own.
 *
 * Pure, and applied during render rather than in an effect, so the pane you are
 * looking at never shows a done badge for one frame before it clears -- and so
 * that the rule can be tested without a renderer.
 *
 * The active pane is looked up in `rows` rather than trusted, because a pane
 * the snapshot no longer carries has no `finishedAt` to record -- and inventing
 * one would suppress the badge on whatever pane inherits that id after a
 * restart. Returns the map unchanged by reference when nothing changed.
 */
export function viewedSeen(
  seen: SeenMap,
  serverStart: string,
  activePane: string | null,
  rows: readonly SnapshotRow[],
): SeenMap {
  if (!activePane) return seen
  const row = rows.find((r) => r.paneId === activePane)
  if (!row) return seen
  return markSeen(seen, serverStart, activePane, row.finishedAt)
}

/**
 * Which base session a fresh tab should attach to.
 *
 * `preferred` (the `?session=` query, or the one this tab used last) wins if it
 * is still in the snapshot. Otherwise the first group that still has a session
 * of its own: a group whose only surviving members are app-created sessions
 * still shows its panes -- those are running agents -- but the session the user
 * made is what died, and the throwaway that outlived it is not a thing to
 * attach a tab to: it is `destroy-unattached on` and goes the moment its own
 * client does.
 *
 * The answer is a group key, which is an identity and not an address;
 * `attachTarget` turns it into the one the socket uses.
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
 * Which session this tab is attached to, as a group key.
 *
 * An identity, not an address: `attachTarget` turns the answer into what
 * `/ws?session=` carries. Everything else in the app -- the sidebar highlight,
 * the click handler, what the tab remembers across a reload -- compares against
 * the key, so this stays in that vocabulary.
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
 * The address a tab was last attached with, filed under the session it belongs
 * to.
 *
 * Both halves are needed and neither is the other. The key is the identity --
 * `session_group`, which is what a sidebar click carries and what the tab
 * remembers across a reload -- and the id is the address tmux answers to. A
 * remembered id without its key would be applied to whatever session the tab
 * resolved to this time, which after a `?session=` typed by hand is a different
 * one.
 */
export interface RememberedTarget {
  /** The group key the address belongs to. */
  key: string
  /** The `$N` the tab attached with. */
  id: string
}

/**
 * A `RememberedTarget` out of storage, or null for anything that is not one.
 *
 * Pure, and separate from the read that produces the string, because
 * `sessionStorage` is not a trusted input: it survives reloads, it is editable,
 * and what comes out of here becomes `/ws?session=`. Everything is checked --
 * that it parses, that it is an object, that both halves are non-empty strings
 * -- so a value left by an older build, or by hand, is a miss rather than a
 * crash on the app's very first render. `attachTarget` checks the id itself as
 * well; this is the shape, that is the meaning.
 */
export function parseRememberedTarget(raw: string | null): RememberedTarget | null {
  if (!raw) return null
  let parsed: unknown
  try {
    parsed = JSON.parse(raw)
  } catch {
    return null
  }
  if (typeof parsed !== 'object' || parsed === null) return null
  const { key, id } = parsed as { key?: unknown; id?: unknown }
  if (typeof key !== 'string' || typeof id !== 'string' || !key || !id) return null
  return { key, id }
}

/**
 * The address to put in `?session=` for a resolved session.
 *
 * This is the seam between identity and address, and the two are not the same
 * value. The group key is the identity -- the React key, what a sidebar click
 * carries, what the tab remembers across a reload -- and it is `session_group`,
 * which tmux freezes at the name the group was created under. This app creates
 * that group itself on the first attach, so from then on a rename leaves the
 * key naming a session that no longer answers to it: `has-session -t =work3`
 * says "can't find session: work3" while the session is alive as `api`, the
 * handshake 404s, and the tab reports `Session "work3" is gone` for every pane
 * the user clicks. A reload does not help, because the key it remembers is the
 * stale one. `$N` is what tmux will answer to, for as long as the session
 * lives, whatever it is called.
 *
 * A session the snapshot does not know is passed through unchanged, and that is
 * the point rather than a fallback: `?session=` is also typed by hand, where a
 * name is the only thing a person could write, and a tab pinned that way must
 * still attach while the poll is failing. The daemon accepts both and tells
 * them apart by the `$` -- see `wsSessionTarget` in `internal/front/ws.go`.
 *
 * ## What `remembered` is for
 *
 * Between those two cases sits the one a reload lands in: the tab knows which
 * session it was on, and what it knows is the *key*. Attaching by key and then
 * re-addressing when the first poll turns it into `$N` costs a whole extra
 * socket per page load -- a second `has-session`, a second `ptybridge.Open`,
 * and a `_web-*` tmux session created and immediately destroyed -- and it files
 * the remembered pane under two `sessionStorage` keys on the way past. It is
 * also often simply wrong: the key is frozen at the name the group was created
 * under, so after a rename the first socket 404s and the tab reports
 * `Session "work3" is gone` until the poll rescues it.
 *
 * So the tab remembers the address beside the key, and hands it back here. It
 * is the answer only while the snapshot has none -- the poll is always
 * preferred, because a remembered id is up to a tab lifetime old and tmux
 * restarts its ids at `$0` with the server, so it can name a different session
 * after a restart. When it does, the first loaded snapshot maps the key to the
 * real id, the address changes, and the socket is rebuilt: the same correction
 * a stale remembered *key* has always relied on, and no worse than what the key
 * alone would have done.
 *
 * Holding the terminal back until `loaded` would also remove the second socket,
 * and was rejected: it delays the terminal on every load by a whole poll, in an
 * app whose `TROUBLE_BEFORE_STALE` exists precisely because polls can be slow.
 * Making the first mount correct costs nothing on screen.
 */
export function attachTarget(
  groups: readonly SessionNode[],
  session: string | null,
  remembered?: RememberedTarget | null,
): string | null {
  if (!session) return null
  // `||` and not `??`: a group built from rows carrying no `sessionId` -- a
  // daemon too old to send the field -- has "" here, and "" addresses whatever
  // tmux considers current.
  const known = groups.find((g) => g.key === session)?.sessionId
  if (known) return known
  if (remembered?.key === session && isSessionId(remembered.id)) return remembered.id
  return session
}

/**
 * Whether s is a tmux session id, e.g. "$4". Mirrors `ValidateSessionID` in
 * `internal/tmux/target.go`.
 *
 * Applied to what comes back out of `sessionStorage`, which is not a trusted
 * input: it survives reloads, the user can edit it, and what passes here goes
 * straight into `/ws?session=`, where tmux resolves a great many strings to
 * "whatever is current" and exits 0.
 */
export function isSessionId(s: string): boolean {
  return /^\$\d+$/.test(s)
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
      // Not covered by windowIndex: a window killed and replaced under the same
      // index carries a new `@N` and nothing else that moves, so without this
      // the previous tree object survives, React reconciles nothing, and the
      // menu goes on offering to rename and kill the window that died.
      x.windowId === y.windowId &&
      x.windowIndex === y.windowIndex &&
      x.windowName === y.windowName &&
      x.command === y.command &&
      x.title === y.title &&
      x.label === y.label &&
      // The field a control renders directly: the header's copy-mode button is
      // named from it. A pane enters or leaves copy mode with its title, its
      // command and its state all standing still, so a comparison that skips
      // this keeps the previous tree object, React reconciles nothing, and the
      // button goes on offering the action the pane is already past.
      x.paneMode === y.paneMode &&
      // Compared though nothing renders it, and for the reason this function's
      // header gives: "every field the wire carries, not only the ones
      // something renders today". A pane that `cd`s somewhere else moves this
      // and nothing else, so left out here it would change in tmux and never
      // reach the tree -- and the check would be added back the day something
      // finally read it, by someone debugging why it was always wrong.
      x.path === y.path &&
      // Moves entirely on its own: a `git switch` changes this and nothing else
      // about the pane, so a comparison that skips it leaves the chip naming
      // the branch the pane has left.
      x.branch === y.branch &&
      x.paneActive === y.paneActive &&
      x.appOwned === y.appOwned &&
      x.agentState === y.agentState &&
      // The activity line is the field that moves on its own most often: an
      // agent reports a new one on every tool call while its title, command
      // and state all stand still. Left out here, it would change in tmux and
      // never reach the DOM.
      x.activity === y.activity &&
      x.stateSource === y.stateSource &&
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
  /**
   * The tmux server generation the rows came from; "" when none is known.
   *
   * Held across a failed poll along with the rows, so a hiccup cannot make
   * every done badge reappear for one interval by dropping the key prefix.
   */
  serverStart: string
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
  serverStart: '',
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
 * **A hidden tab polls slowly rather than not at all.** It once parked
 * entirely, on the reasoning that a phone in a pocket has no use for a sidebar
 * it is not showing -- but the tab badge is not the sidebar, and a parked loop
 * made the badge unable to fire in the only situation it exists for. So the
 * cadence switches to `HIDDEN_POLL_INTERVAL_MS` instead, which is what a
 * background tab's timers are throttled to anyway.
 *
 * The objection that parking answered was that a throttled interval leaves the
 * first visible frame showing a minute-old tree. `wake()` is what answers it
 * now: coming back cuts the long wait short and polls immediately, so what you
 * see on the way back is fresh whether the loop was slow or stopped.
 *
 * It costs the daemon nothing to speak of. `/api/snapshot` is served from the
 * poller's in-memory cache, so a hidden tab's request is a read of bytes that
 * already exist, not a tmux fork -- and the fork cadence that produces them is
 * gated on a live terminal socket (`Registry.Live`), which a hidden tab is
 * holding open either way. Ten hidden tabs are ten reads a minute; nothing here
 * needs coalescing.
 *
 * **Only one request is ever in flight.** The next poll is scheduled after the
 * previous one settles, not on a fixed interval, so a slow response cannot
 * stack requests behind it -- and that is also what keeps a wake from racing a
 * hidden request already on the wire, since two answers can never land out of
 * order and put an older tree on screen than the one already there.
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
  /**
   * The pending timer is the hidden cadence's, so `wake()` may cut it short.
   *
   * Written only by `#schedule`, read only by `wake`, and deliberately not
   * cleared when the timer fires or is cancelled -- because it cannot be read
   * while stale. Every path that leaves a timer behind either stops the poller
   * or reaches `#poll`, which claims `#inFlight` synchronously, and `wake`
   * answers that case before it ever looks here.
   */
  #armedHidden = false
  /** A `wake()` that arrived mid-request, owed a poll once it settles. */
  #wakePending = false
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

  /**
   * The tab became visible again: don't make it wait out the hidden cadence.
   *
   * This is the whole reason a slow hidden poll is allowed to exist -- without
   * it the first visible frame could be a minute old. Three cases, and the
   * third is the one worth stating:
   *
   * - A hidden-cadence timer is pending: cancel it and poll now.
   * - Already on the visible cadence: nothing to cut short, so nothing happens.
   *   A spurious wake cannot turn into an extra request.
   * - A request is in flight: it went out before you looked, so its answer is
   *   not the fresh frame this promises -- but a second request alongside it
   *   would let two answers land in either order. The wake is remembered and
   *   honoured the instant the first one settles instead.
   */
  wake(): void {
    if (this.#stopped) return
    if (this.#inFlight) {
      this.#wakePending = true
      return
    }
    if (!this.#armedHidden) return
    if (this.#timer) clearTimeout(this.#timer)
    this.#timer = null
    void this.#poll()
  }

  /** Poll now instead of waiting out the interval; the Retry button. */
  refresh(): void {
    if (this.#stopped) return
    if (this.#timer) clearTimeout(this.#timer)
    this.#timer = null
    void this.#poll()
  }

  async #poll(): Promise<void> {
    if (this.#stopped || this.#inFlight) return
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
      serverStart: payload.serverStart,
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
      serverStart: prev.serverStart,
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

  /**
   * Arm the next poll, at whichever cadence the tab is owed right now.
   *
   * Visibility is read here, when the timer is armed, and there is nowhere else
   * to read it: the delay has to be chosen before the wait starts. But the tab
   * is usually hidden *after* the next poll was scheduled -- switching away is
   * the whole event -- so going away costs one last poll at the visible cadence
   * before the loop settles onto the slow one. That is one request, once. The
   * other direction is the one that would be felt, a minute of waiting on a tab
   * you are looking at, and `wake()` is what answers it.
   */
  #schedule(): void {
    if (this.#stopped || this.#timer || this.#inFlight) return
    const owed = this.#wakePending
    this.#wakePending = false
    const hidden = this.#isHidden()
    this.#armedHidden = hidden
    this.#timer = setTimeout(
      () => {
        this.#timer = null
        void this.#poll()
      },
      owed ? 0 : hidden ? HIDDEN_POLL_INTERVAL_MS : this.#intervalMs,
    )
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

/**
 * This device's memory of which finished runs it has already been shown, kept
 * current with the pane the tab is looking at.
 *
 * Read from `localStorage` once, during the first render, so the first frame
 * after a reload already has the badges right rather than showing every pane as
 * done for a tick and then clearing them.
 *
 * What is *returned* is derived during render, so looking at a pane clears its
 * badge in the same frame -- and so that the rule is testable under
 * `react-dom/server`, where effects never run. Only the `localStorage` write is
 * an effect, because only that is one. It is idempotent: it re-runs on its own
 * result and `viewedSeen` answers with the same map by reference the second
 * time.
 *
 * **Called in App, not in the sidebar.** Task 14's tab badge counts the same
 * `done` panes one level up, and two copies of this map would each clear their
 * own half -- the badge would go on counting the pane you are looking at. App
 * calls it and hands the answer down as a prop; the rules stay here, where they
 * are testable without a renderer.
 *
 * **The one thing under here no unit test reaches** is that this effect is
 * wired at all: `renderToStaticMarkup` does not run effects, and this suite has
 * no DOM renderer by design. Everything the effect decides -- `viewedSeen`,
 * `writeSeen`, the key format -- is tested directly; deleting the effect itself
 * would cost the memory across a reload and nothing within a session, and it is
 * the e2e run that would notice.
 */
export function useSeenPanes(
  serverStart: string,
  activePane: string | null,
  rows: readonly SnapshotRow[],
): SeenMap {
  const [stored, setStored] = useState<SeenMap>(readSeen)
  const seen = viewedSeen(stored, serverStart, activePane, rows)
  useEffect(() => {
    if (seen === stored) return
    writeSeen(seen)
    // oxlint react(set-state-in-effect) flags this, and it is right to ask.
    // The answer is that `seen` *accumulates*: without folding the mark back
    // into state, the next pane you look at would be derived from the map as it
    // was on load and the previous pane's mark would be lost. It converges in
    // one extra render -- `viewedSeen` then answers with the same map by
    // reference -- and only runs when a pane you are looking at has a finish
    // this device has not been shown, which is at most once per finished run.
    setStored(seen)
  }, [seen, stored])
  return seen
}
