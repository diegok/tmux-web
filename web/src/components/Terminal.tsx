/**
 * The terminal pane: one `@wterm/react` terminal bound to one tmux attach, with
 * the reconnect policy that the transport deliberately does not have.
 *
 * `Transport` (see `@/lib/transport`) owns exactly one WebSocket for its whole
 * life and never reopens, because a reopen here is not a reopen at all -- the
 * server answers a new socket with a *new* throwaway tmux session, grouped onto
 * the same base session but landing on the group's first window, wherever this
 * tab was. Everything that makes a reconnect safe therefore lives in this file:
 *
 *   - Nothing typed at a dead socket is kept. `write` returns false and says so
 *     in the UI instead of queueing, because the only place a queue could be
 *     flushed to is a different pane, seconds later, in front of a different
 *     agent. This is the one invariant worth breaking the component over.
 *   - The pane the tab was looking at is remembered in `sessionStorage` (as a
 *     tmux pane id -- `%3` -- since `select` refuses anything else) and
 *     re-selected on the new socket *before* input is enabled, so the first
 *     keystroke after a blip cannot land in window 0. If that pane died in the
 *     meantime the server logs the failure and carries on, which leaves the tab
 *     where it attached -- the group's *first* window, measured on tmux 3.7b,
 *     not the base session's current one: the fallback the design asks for, for
 *     free.
 *   - Every socket then *asks* where it landed, and the answer is what makes
 *     that fallback visible. Nothing else can tell the tab: its own throwaway
 *     tmux session has a current window independent of the user's terminal --
 *     which is why the session exists at all -- and no snapshot row names it.
 *     Without the question, a tab that had never clicked anything reported no
 *     pane at all, so the sidebar highlighted nothing and the breadcrumb said
 *     only the session's name over a terminal full of somebody's work.
 *   - Resize is debounced. tmux sizes a window to its most recently active
 *     client, so an undebounced drag would repeatedly yank the dimensions of
 *     the terminal the user is sitting in front of locally.
 *   - Reconnecting stops when the base session is *gone* rather than merely
 *     unreachable. v2 lets the owner kill the session their own tab is attached
 *     to -- killing a base session's last window destroys the whole group, the
 *     app's `@tmux_web_owned` member included -- and against a session that no
 *     longer exists the backoff would retry until the tab was closed. The
 *     distinction cannot be read off the socket (see `probeSession`), so it is
 *     asked of `/api/snapshot`, and only a fresh snapshot that has lost the
 *     session stops the loop.
 *
 * The socket-driving half is `TerminalSession`, a plain class with no React in
 * it. That is deliberate: it is the part with ordering, timers and a state
 * machine, and it can be tested in vitest's node environment against the real
 * `Transport` and a stubbed `WebSocket` -- no jsdom, no renderer, and no test
 * that only proves React rendered.
 *
 * ## For the sidebar and the palette (tasks 21 and 22)
 *
 * The current pane is owned *here*, not by the parent: this component is the
 * only thing that knows when a socket was replaced and the selection has to be
 * replayed. Drive it through the ref handle and read it back from the status:
 *
 * ```tsx
 * const term = useRef<TerminalHandle>(null)
 * const [status, setStatus] = useState<TerminalStatus | null>(null)
 * <AppSidebar activePane={status?.pane} onSelect={(p) => term.current?.select(p)} />
 * <Terminal session={base} onStatusChange={setStatus} ref={term} />
 * ```
 */

import { Terminal as WTermView, useTerminal } from '@wterm/react'
import { useCallback, useEffect, useImperativeHandle, useRef, useState } from 'react'
import type { Ref } from 'react'

import { installLinkOpener } from '@/lib/links'
import { Transport } from '@/lib/transport'
import type { TransportClose } from '@/lib/transport'
import { fetchSnapshot } from '@/lib/useSnapshot'
import type { FetchLike, SnapshotRow } from '@/lib/useSnapshot'

// The terminal's own stylesheet: without it wterm renders as unstyled rows.
// The package exposes it only through the extensionless "./css" export
// condition, which `allowArbitraryExtensions` cannot type (TS2882) and which
// the deep path into src/ cannot bypass -- the exports map blocks it. A
// declaration file would be the tidier fix, but it belongs to whoever owns the
// app's global types rather than to this component.
// @ts-expect-error -- untyped css export, resolved by vite
import '@wterm/react/css'

/**
 * How long a burst of resizes has to settle before one is sent.
 *
 * The number is from the design document, and the reason it is not zero is not
 * bandwidth: tmux resizes a window to fit its most recently active client, so
 * every intermediate size this tab reports is briefly imposed on the user's own
 * local attach to the same window. 150ms is below the threshold where a
 * finished drag feels laggy and far above a browser's resize event rate.
 */
export const RESIZE_DEBOUNCE_MS = 150

/** First reconnect delay, doubling per consecutive failure. */
export const BACKOFF_BASE_MS = 500

/**
 * Ceiling on the reconnect delay. Deliberately short: the realistic fault here
 * is a laptop that slept or a wifi handover, and the user is sitting in front
 * of the tab waiting for their agent to come back. There is no herd to protect
 * -- this is a single-user daemon -- so the only cost of retrying every 15s
 * indefinitely is one refused TCP connection per 15s.
 */
export const BACKOFF_MAX_MS = 15_000

/** Proportional jitter applied to each delay, in both directions. */
export const BACKOFF_JITTER = 0.25

/**
 * How long a socket may have been silent before a wake will probe it.
 *
 * A GUESS -- nobody has measured this, and design open question 2 is still
 * open. Anchored at one end only: it must sit below what a person would call
 * frozen. It cannot be anchored at the other end, because the traffic that
 * would anchor it -- the server's 20s ping -- is answered below the JavaScript
 * API and never touches `lastRecvAt`.
 *
 * Wrong high costs a zombie socket that survives one wake and waits for the
 * next. Wrong low costs two tmux forks per glance at a phone -- three when the
 * window-id hint is stale or absent. Measurable by backgrounding a real phone
 * for an hour and recording how long a wake takes to produce a frame.
 */
export const LIVENESS_SLACK_MS = 45_000

/**
 * How long the wake probe waits for an answer before deciding the socket is
 * dead and reconnecting.
 *
 * A GUESS -- nobody has measured this either; same open question. Wrong low
 * costs one unnecessary reconnect: a fork, a PTY, a redraw, and the remembered
 * pane re-selected. Annoying, not destructive.
 */
export const PROBE_TIMEOUT_MS = 5_000

/**
 * How long a socket may sit in CONNECTING before a wake gives up on it.
 *
 * A GUESS, like the two above. This case is not established to be permanent: a
 * stalled handshake eventually fails at the TCP or proxy layer and fires
 * onclose, which puts it back on the backoff ladder. What is established is the
 * timescale -- that failure is minutes of somebody else's timeout, and a wake
 * is about the seconds a person will look at a frozen terminal.
 */
export const CONNECT_STALL_MS = 10_000

/**
 * Reconnect delay for the nth consecutive failure (n counted from zero).
 *
 * Exponential from BACKOFF_BASE_MS, capped at BACKOFF_MAX_MS, then jittered.
 * The jitter matters less than it would in a fleet, but it keeps a tab that
 * dropped when the daemon restarted from retrying in lockstep with every other
 * tab that dropped at the same instant.
 */
export function backoffDelay(attempt: number, random: () => number = Math.random): number {
  const exponential = BACKOFF_BASE_MS * 2 ** Math.max(0, attempt)
  const jitter = 1 + (random() * 2 - 1) * BACKOFF_JITTER
  // Clamped once, after the jitter, so that there is exactly one place the cap
  // is enforced -- capping before the jitter as well reads as belt and braces
  // but is unreachable code, and unreachable code is untestable code.
  return Math.round(Math.min(BACKOFF_MAX_MS, Math.max(BACKOFF_BASE_MS, exponential * jitter)))
}

/**
 * Whether a closed socket should be reconnected.
 *
 * 1000 is the one code the daemon sends on purpose, from `wsWriteLoop` with the
 * reason "session ended": the PTY reached EOF because the user typed `exit` or
 * the base session was killed. Reconnecting from that would silently create a
 * fresh tmux session behind a user who just ended one, so it is the only code
 * that stops the loop. Everything else -- 1006 from a vanished peer, 1001 from
 * a daemon restart, 1005 from a socket that never opened -- is a fault, and a
 * fault is exactly what tmux's persistence exists to survive.
 *
 * This answers only "was this close deliberate?". Whether the session behind
 * the socket still exists is a different question that no close code carries --
 * see `probeSession` -- and the answer to it can stop the loop this one starts.
 */
export function shouldReconnect(close: TransportClose): boolean {
  return close.code !== 1000
}

/**
 * How long a presence probe may take before it is abandoned as unknown.
 *
 * Shorter than the snapshot poll's own timeout on purpose. A probe races the
 * backoff it is trying to interrupt, and one still outstanding after several
 * retries has already lost that race -- it would only pile a second and third
 * request onto a daemon that is evidently not answering. Five seconds is far
 * past a local daemon serving a cached snapshot out of memory.
 */
export const SESSION_PROBE_TIMEOUT_MS = 5000

/**
 * What a probe found out about the base session.
 *
 * "unknown" is not a failure to be retried here: it is the answer whenever the
 * daemon could not be reached or could not reach tmux, and in that case the
 * reconnect loop is already doing the right thing. Only "gone" changes
 * anything, which is what keeps a network fault from being mistaken for a kill.
 */
export type SessionPresence = 'present' | 'gone' | 'unknown'

/**
 * Whether the snapshot still contains the session the socket targets.
 *
 * Matched on `sessionId` or `sessionName`, and never on `groupKey`, because
 * those two are exactly the question the daemon asks: `ServeHTTP` in
 * `internal/front/ws.go` resolves a `$N` as an id and anything else as an exact
 * name. The app sends the id (`attachTarget`), a person typing `?session=` by
 * hand sends a name, and both must read as present here or a live terminal is
 * condemned by its own probe.
 *
 * `groupKey` is `session_group`, which tmux freezes at the group's pre-rename
 * name and keeps long after the namesake session has died -- so a group can
 * still be full of panes (the app's own throwaway members, or a renamed
 * survivor) while `?session=<key>` 404s. Matching on the group key would call
 * that session present and leave the tab retrying forever, which is the whole
 * bug this is here to fix.
 */
export function snapshotHasSession(panes: readonly SnapshotRow[], session: string): boolean {
  return panes.some((pane) => pane.sessionId === session || pane.sessionName === session)
}

/**
 * Ask `/api/snapshot` whether the base session still exists.
 *
 * This is a *probe*, not a socket close code, because the browser cannot see
 * the one the daemon would want to send. `ws.go` refuses a missing session
 * before the upgrade, with `404 no such session` -- and a WebSocket handshake
 * that fails on an HTTP status is reported to script as `close` code 1006 with
 * an empty reason, identical to a refused TCP connection or a dropped network.
 * The status is not exposed by the WebSocket API at all. A distinct close code
 * is therefore not available for the case that matters (the daemon never opens
 * a socket it would send one on), so the only way for this tab to tell "killed"
 * from "unreachable" is to ask a second question over a channel that can
 * answer: the snapshot the sidebar is already polling.
 *
 * Every failure answers "unknown". A refused fetch, a 401 from an expired
 * device cookie, a body that is not a snapshot -- none of them is evidence that
 * a tmux session died, and treating them as such would stop reconnecting after
 * an ordinary blip, which is the worse of the two failures by far.
 *
 * A *stale* snapshot answers "unknown" as well. Stale means the daemon's own
 * poll of tmux failed and it is serving the last good rows; those rows cannot
 * condemn a session, because the reason they are stale may be the same reason
 * the socket dropped. Only a fresh snapshot gets to say "gone".
 */
export async function probeSession(
  session: string,
  fetchImpl?: FetchLike,
): Promise<SessionPresence> {
  const controller = new AbortController()
  const timer = setTimeout(() => controller.abort(), SESSION_PROBE_TIMEOUT_MS)
  try {
    const snapshot = await fetchSnapshot(controller.signal, fetchImpl)
    if (snapshot.stale) return 'unknown'
    return snapshotHasSession(snapshot.panes, session) ? 'present' : 'gone'
  } catch {
    return 'unknown'
  } finally {
    clearTimeout(timer)
  }
}

/**
 * Whether s is a tmux pane id, e.g. "%3". Mirrors `wsIsPaneID` in
 * `internal/front/ws.go`, and for the same reason: `tmux select-pane -t work`
 * exits 0 and moves the pane the *user* is sitting in front of. The server
 * refuses non-pane targets, so this is only here to keep a bad id out of
 * `sessionStorage`, where it would survive reloads.
 */
export function isPaneId(s: string): boolean {
  return /^%\d+$/.test(s)
}

/** Where this tab remembers the pane it was looking at, per base session. */
export function paneStorageKey(session: string): string {
  return `tmux-web:pane:${session}`
}

/**
 * The `type` of the server's answer to `where`. Mirrors `wsPaneType` in
 * `internal/front/ws.go`.
 */
export const PANE_MESSAGE = 'pane'

/**
 * The pane id in a `{type:'pane',pane:'%3'}` control message, or null for
 * anything else that arrived.
 *
 * Everything off the socket is validated here rather than trusted, and the
 * reason is not hypothetical politeness: what this returns is written straight
 * into the tab's idea of where it is, which drives the sidebar highlight, the
 * breadcrumb, the pane re-selected on the next reconnect, and which pane's
 * "finished" badge gets cleared. A "" that slipped through would clear the
 * badge on nothing and re-select nothing, silently.
 */
export function parsePaneMessage(message: unknown): string | null {
  if (typeof message !== 'object' || message === null) return null
  const m = message as { type?: unknown; pane?: unknown }
  if (m.type !== PANE_MESSAGE) return null
  return typeof m.pane === 'string' && isPaneId(m.pane) ? m.pane : null
}

/**
 * The terminal socket's url for a base session.
 *
 * Same-origin by construction -- the daemon serves both the SPA and `/ws`, and
 * the WebSocket handler refuses any Origin but its own.
 */
export function terminalUrl(
  session: string,
  location: { protocol: string; host: string } = window.location,
): string {
  const scheme = location.protocol === 'https:' ? 'wss:' : 'ws:'
  return `${scheme}//${location.host}/ws?session=${encodeURIComponent(session)}`
}

/** Where the connection is. */
export type TerminalPhase =
  /** No socket yet, or one that has not opened. Input goes nowhere. */
  | 'connecting'
  /** Open, resized, and the remembered pane re-selected. Input is live. */
  | 'ready'
  /** Dropped by a fault; a retry is scheduled. Input goes nowhere. */
  | 'reconnecting'
  /** tmux ended the session. Nothing will be retried automatically. */
  | 'ended'
  /**
   * The base session no longer exists, so no retry can ever succeed. Terminal,
   * until the user picks another session or asks for this name again.
   */
  | 'gone'
  /** `stop()` was called: the component unmounted or the session changed. */
  | 'closed'

export interface TerminalStatus {
  phase: TerminalPhase
  /** Consecutive failed connections; back to 0 on every successful open. */
  attempt: number
  /** Delay until the scheduled retry, when phase is "reconnecting". */
  retryDelayMs: number
  /**
   * The pane this tab is looking at, and the app's whole answer to "which of
   * these am I in".
   *
   * Null only before the first socket has opened and been answered. It used to
   * stay null for the entire life of a tab that never clicked anything -- the
   * field was only ever written by `select()` -- so a freshly loaded tab
   * highlighted no row and printed a breadcrumb with only a session name in it,
   * while showing a live pane. It is now also written by the server's answer to
   * `where`; see `#landed`.
   */
  pane: string | null
  /**
   * Something the user typed was discarded because the socket was not open.
   * Cleared on the next open. The UI has to say this out loud: the alternative
   * to a queue is silence, and silence in a terminal reads as a hung agent.
   */
  inputDropped: boolean
}

/** The slice of `sessionStorage` this component uses. */
export interface PaneStorage {
  getItem(key: string): string | null
  setItem(key: string, value: string): void
  removeItem(key: string): void
}

export interface TerminalSessionOptions {
  /** WebSocket url, including `?session=`. */
  url: string
  /**
   * Base tmux session name this socket targets. Carried separately from `url`
   * because it is what the presence probe asks about, and because `url` is
   * overridable for stories and tests.
   */
  session: string
  /** `sessionStorage` key holding the remembered pane id. */
  paneKey: string
  /** Sink for PTY bytes. */
  onData: (bytes: Uint8Array) => void
  /** Called on every state change, including the first. */
  onStatus: (status: TerminalStatus) => void
  /** Defaults to `sessionStorage`; null disables persistence. */
  storage?: PaneStorage | null
  /** Injectable for tests. */
  random?: () => number
  /**
   * Injectable clock. Defaults to Date.now, and is handed to every Transport
   * this session builds so that one clock answers for the whole session's
   * `lastRecvAt` and `connectStartedAt`.
   */
  now?: () => number
  /**
   * Answers whether the base session still exists, after a socket dropped.
   * Defaults to a `/api/snapshot` probe; injectable for tests.
   */
  probe?: () => Promise<SessionPresence>
}

function defaultStorage(): PaneStorage | null {
  try {
    // Absent under vitest's node environment, and it throws rather than
    // returning null in a Safari private window.
    return globalThis.sessionStorage ?? null
  } catch {
    return null
  }
}

/**
 * One tmux attach, reconnected as needed. Owns the Transport, the backoff
 * timer, the resize debounce and the remembered pane.
 *
 * Deliberately free of React so that the ordering rules it enforces -- select
 * before input, one live socket at a time -- are testable without a renderer.
 */
export class TerminalSession {
  readonly #opts: TerminalSessionOptions
  readonly #storage: PaneStorage | null
  readonly #random: () => number
  readonly #now: () => number

  /**
   * The one live socket, or null between attempts. Every callback checks its
   * own transport against this before acting: a late `onClose` from a socket
   * we already replaced would otherwise schedule a second reconnect, and two
   * reconnect chains means two tmux sessions racing to be this tab's.
   */
  #transport: Transport | null = null

  #phase: TerminalPhase = 'connecting'
  #attempt = 0
  #retryDelayMs = 0
  #pane: string | null = null
  #inputDropped = false

  /**
   * A `where` is outstanding on the current socket and its answer may still be
   * adopted.
   *
   * This is what keeps the server's answer from ever overruling the user.
   * `select()` clears it, so a click that happens while the question is in
   * flight wins and the reply is discarded rather than dragging the tab back to
   * where it was a few milliseconds ago. Set on each open and cleared on each
   * close, so an answer from a socket that has been replaced is inert twice
   * over -- the transport identity check catches it too.
   */
  #awaitingWhere = false

  /** Last size wterm reported; 0 until it has laid out. */
  #cols = 0
  #rows = 0
  /** The size actually sent on this socket, so a reopen re-sends. */
  #sentCols = 0
  #sentRows = 0

  #resizeTimer: ReturnType<typeof setTimeout> | null = null
  #retryTimer: ReturnType<typeof setTimeout> | null = null
  #stopped = false

  readonly #probe: () => Promise<SessionPresence>
  /** One probe at a time: a flapping socket must not fan out into requests. */
  #probing = false
  /**
   * Bumped on every successful open. A probe carries the value it was fired
   * under and is discarded if it changed, because an opened socket is proof
   * the session exists -- the daemon runs `has-session` before it upgrades --
   * and a snapshot that raced the kill would otherwise strand a live terminal.
   */
  #openEpoch = 0

  constructor(opts: TerminalSessionOptions) {
    this.#opts = opts
    this.#storage = opts.storage === undefined ? defaultStorage() : opts.storage
    this.#random = opts.random ?? Math.random
    this.#now = opts.now ?? Date.now
    this.#probe = opts.probe ?? (() => probeSession(opts.session))
    this.#pane = this.#readPane()
  }

  get status(): TerminalStatus {
    return {
      phase: this.#phase,
      attempt: this.#attempt,
      retryDelayMs: this.#retryDelayMs,
      pane: this.#pane,
      inputDropped: this.#inputDropped,
    }
  }

  /** Open the first socket and report the initial status. */
  start(): void {
    if (this.#stopped || this.#transport) return
    this.#connect()
  }

  /**
   * Close for good: no further socket, timer or status. Idempotent, and the
   * only way this class stops on its own terms -- everything else is a
   * reconnect.
   */
  stop(): void {
    if (this.#stopped) return
    this.#stopped = true
    this.#clearTimers()
    this.#transport?.close(1000, 'tab closed')
    this.#transport = null
    this.#phase = 'closed'
    this.#emit()
  }

  /**
   * Send what the user typed. Returns false when the bytes were dropped, which
   * is also when the UI starts saying so.
   *
   * The phase check is the point of this method. `Transport.send` would refuse
   * on a closed socket anyway, but "ready" is a stricter gate: it is not set
   * until the remembered pane has been re-selected on this socket, so a
   * keystroke cannot overtake the select and reach the wrong pane.
   */
  write(data: string | Uint8Array): boolean {
    if (this.#phase !== 'ready' || !this.#transport?.send(data)) {
      this.#noteDrop()
      return false
    }
    return true
  }

  /**
   * Note a new terminal size. The send is debounced by RESIZE_DEBOUNCE_MS;
   * nothing reaches tmux until the burst settles.
   */
  noteResize(cols: number, rows: number): void {
    // A zero axis is not a size, and taking one would be worse than ignoring
    // it: it would overwrite the last real size, so the next reconnect would
    // leave the pane at the server's initial 80x24. wterm reports 0 while it
    // is still laying out.
    if (this.#stopped || cols <= 0 || rows <= 0) return
    this.#cols = cols
    this.#rows = rows
    if (this.#resizeTimer) clearTimeout(this.#resizeTimer)
    this.#resizeTimer = setTimeout(() => {
      this.#resizeTimer = null
      this.#sendResize()
    }, RESIZE_DEBOUNCE_MS)
  }

  /**
   * Point this tab's session at a pane and remember it for the next socket.
   * Returns false for anything that is not a pane id, which is the frontend's
   * half of the guard in `tmux.Client.SelectPane`.
   */
  select(pane: string): boolean {
    if (!isPaneId(pane)) {
      console.warn('terminal: refusing to select non-pane target', pane)
      return false
    }
    this.#pane = pane
    this.#writePane(pane)
    // The user has answered "where am I" by moving, so a reply still in flight
    // is stale before it lands. See #awaitingWhere.
    this.#awaitingWhere = false
    // Not an error when the socket is down: it is remembered, and every open
    // replays it before enabling input.
    if (this.#phase === 'ready') this.#transport?.select(pane)
    this.#emit()
    return true
  }

  /** Put a pane into tmux copy-mode; omit the pane for this tab's current one. */
  copyMode(pane?: string): boolean {
    if (this.#phase !== 'ready' || !this.#transport) return false
    return this.#transport.copyMode(pane)
  }

  /**
   * Reconnect now instead of waiting out the backoff, and from "ended" or
   * "gone" as well: there the user is explicitly asking for a new session,
   * which is what a new socket is. A session name can be reused, so "gone" has
   * to be an escapable state -- it says this tab stopped trying, not that
   * trying is forbidden.
   */
  retryNow(): void {
    if (this.#stopped || this.#transport) return
    this.#clearTimers()
    this.#attempt = 0
    this.#connect()
  }

  #connect(): void {
    this.#retryDelayMs = 0
    this.#phase = this.#attempt === 0 ? 'connecting' : 'reconnecting'
    const transport = new Transport({
      url: this.#opts.url,
      now: this.#now,
      onData: (bytes) => {
        if (transport === this.#transport) this.#opts.onData(bytes)
      },
      onControl: (message) => {
        if (transport === this.#transport) this.#landed(message)
      },
      onOpen: () => this.#opened(transport),
      onClose: (event) => this.#closed(transport, event),
    })
    this.#transport = transport
    this.#emit()
  }

  /**
   * A socket opened. Size and position are restored here, in this order, and
   * only then does input become live -- see `write`.
   */
  #opened(transport: Transport): void {
    if (transport !== this.#transport) return
    this.#attempt = 0
    this.#retryDelayMs = 0
    this.#inputDropped = false
    this.#openEpoch++

    // The server attaches at 80x24 until told otherwise, so a reconnect that
    // skipped this would redraw the pane at the wrong size.
    this.#sentCols = 0
    this.#sentRows = 0
    this.#sendResize()

    // Before 'ready'. If this pane is gone the server logs it and the session
    // stays where it attached, which is the intended fallback -- and which the
    // `where` below is what turns into something the tab can actually show.
    if (this.#pane) transport.select(this.#pane)

    // And then: "so where am I?". After the select, never before, because the
    // server answers with where the tab actually *is* -- which is the
    // remembered pane when it was still alive, and the pane tmux left this tab
    // on when it was not. One question covers both, and covers the case this
    // exists for: a tab that has just attached and remembers nothing at all,
    // which until now had no idea which pane it was showing. See
    // `Transport.where`.
    this.#awaitingWhere = transport.where()

    this.#phase = 'ready'
    this.#emit()
  }

  /**
   * The server said which pane this tab is on.
   *
   * Adopted into the same field a click writes, so everything downstream --
   * the sidebar highlight, the breadcrumb, the pane replayed on the next
   * reconnect, the "finished" badge that viewing a pane clears -- needs to know
   * nothing about where the value came from.
   *
   * Deliberately not written to `sessionStorage`. What is remembered there is
   * where the *user* put themselves, and it is the only thing allowed to
   * outlive the tab's socket; an observation does not need to be, because a
   * reload asks this question again and gets a current answer, while persisting
   * it would quietly turn "the window this group is on" into a pin.
   */
  #landed(message: unknown): void {
    const pane = parsePaneMessage(message)
    if (pane === null) {
      console.warn('terminal: ignoring an unusable control message', message)
      return
    }
    // A click since the question was asked outranks the answer to it, and so
    // does an answer to a question this session did not ask.
    if (!this.#awaitingWhere) return
    this.#awaitingWhere = false
    if (pane === this.#pane) return
    this.#pane = pane
    this.#emit()
  }

  #closed(transport: Transport, event: TransportClose): void {
    if (transport !== this.#transport) return
    this.#transport = null
    this.#awaitingWhere = false
    if (this.#resizeTimer) {
      clearTimeout(this.#resizeTimer)
      this.#resizeTimer = null
    }

    if (!shouldReconnect(event)) {
      this.#phase = 'ended'
      this.#retryDelayMs = 0
      this.#emit()
      // "session ended" is what a kill looks like from inside an attached tab
      // as well as what `exit` looks like, and the two want different words and
      // a different button. Asking costs one request against a session that
      // just died either way.
      this.#checkGone()
      return
    }

    const delay = backoffDelay(this.#attempt, this.#random)
    this.#attempt++
    this.#phase = 'reconnecting'
    this.#retryDelayMs = delay
    this.#retryTimer = setTimeout(() => {
      this.#retryTimer = null
      if (!this.#stopped) this.#connect()
    }, delay)
    this.#emit()

    // Fired *beside* the backoff rather than in front of it, deliberately. The
    // retry is scheduled first and runs on time, so a dropped network
    // reconnects exactly as fast as it did before this existed; the probe only
    // ever cancels a retry that was going to fail. Waiting for an answer before
    // scheduling would put a request that may never settle in the path of every
    // ordinary blip.
    this.#checkGone()
  }

  /**
   * Ask whether the base session still exists, and stop for good if it does
   * not.
   *
   * Nothing here retries the probe. It is fired again by the next close, and
   * between now and then the backoff is doing its job, so a probe that could
   * not get an answer costs a few more seconds of retrying rather than a
   * permanently wrong state.
   */
  #checkGone(): void {
    if (this.#stopped || this.#probing) return
    this.#probing = true
    const epoch = this.#openEpoch
    void this.#probe()
      // A probe that throws is not evidence of anything; `probeSession` already
      // answers "unknown" rather than rejecting, and this covers an injected one.
      .catch(() => 'unknown' as SessionPresence)
      .then((presence) => {
        this.#probing = false
        if (this.#stopped || presence !== 'gone') return
        if (epoch !== this.#openEpoch) return
        this.#markGone()
      })
  }

  /**
   * The session is gone: stop, and say so. The design's answer for "my group is
   * gone" is to report it and offer the session list, not to keep retrying
   * against something that cannot come back on its own.
   */
  #markGone(): void {
    this.#clearTimers()
    // A retry may already have opened a socket that is still connecting. It is
    // aimed at a session the daemon will refuse; closing it here means the
    // refusal does not arrive as a fresh close and start the loop again.
    this.#transport?.close(1000, 'session gone')
    this.#transport = null
    this.#phase = 'gone'
    this.#retryDelayMs = 0
    this.#emit()
  }

  #sendResize(): void {
    if (this.#cols <= 0 || this.#rows <= 0) return
    if (this.#cols === this.#sentCols && this.#rows === this.#sentRows) return
    if (!this.#transport?.resize(this.#cols, this.#rows)) return
    this.#sentCols = this.#cols
    this.#sentRows = this.#rows
  }

  #noteDrop(): void {
    if (this.#inputDropped || this.#stopped) return
    this.#inputDropped = true
    this.#emit()
  }

  #clearTimers(): void {
    if (this.#resizeTimer) clearTimeout(this.#resizeTimer)
    if (this.#retryTimer) clearTimeout(this.#retryTimer)
    this.#resizeTimer = null
    this.#retryTimer = null
  }

  #readPane(): string | null {
    try {
      const pane = this.#storage?.getItem(this.#opts.paneKey)
      // A stored value that is not a pane id can only come from an older or
      // broken build; dropping it beats sending it and having the daemon
      // refuse every reconnect.
      return pane && isPaneId(pane) ? pane : null
    } catch {
      return null
    }
  }

  #writePane(pane: string): void {
    try {
      this.#storage?.setItem(this.#opts.paneKey, pane)
    } catch {
      // A full or disabled storage costs position across a reload, nothing more.
    }
  }

  #emit(): void {
    this.#opts.onStatus(this.status)
  }
}

export interface TerminalHandle {
  /** Move this tab to a pane id (`%3`) and remember it. */
  select(pane: string): boolean
  /** Enter tmux copy-mode, where this app's scrollback lives. */
  copyMode(pane?: string): boolean
  focus(): void
  /** Reconnect immediately, ignoring the backoff. */
  retry(): void
}

export interface TerminalProps {
  /**
   * The tmux session this tab groups onto, as an *address*: a `$N` id, or a
   * name when `?session=` was typed by hand. It is what `/ws?session=` carries,
   * what the presence probe asks about, and what the remembered pane is filed
   * under -- never a `session_group` key, which tmux freezes at the group's
   * pre-rename name and which addresses nothing after a rename.
   */
  session: string
  /**
   * What to call that session when telling the user it is gone. Defaults to
   * `session`, which is right while the two coincide and wrong once the address
   * is an id: `Session "$4" is gone` names nothing a person can find in the
   * sidebar.
   */
  label?: string
  /** Overrides the derived WebSocket url; for stories and tests. */
  url?: string
  className?: string
  /** Connection state, for the header dot and toasts. */
  onStatusChange?: (status: TerminalStatus) => void
  ref?: Ref<TerminalHandle>
}

const INITIAL_STATUS: TerminalStatus = {
  phase: 'connecting',
  attempt: 0,
  retryDelayMs: 0,
  pane: null,
  inputDropped: false,
}

export function Terminal({ session, label, url, className, onStatusChange, ref }: TerminalProps) {
  const { ref: termRef, write, focus } = useTerminal()
  const [status, setStatus] = useState<TerminalStatus>(INITIAL_STATUS)

  const sessionRef = useRef<TerminalSession | null>(null)
  // Kept in a ref so that a parent passing an inline callback does not tear
  // down the socket on every render. Written in an effect rather than during
  // render, which is where React allows a ref to be touched.
  const statusCallback = useRef(onStatusChange)
  useEffect(() => {
    statusCallback.current = onStatusChange
  })

  // wterm reports its size once it has laid out, which can be before or after
  // the effect below runs. Kept here so a size measured first is not lost.
  const size = useRef<{ cols: number; rows: number } | null>(null)

  // Ctrl/Cmd/Shift+click opens a link. Installed on the wrapper rather than on
  // wterm's own element so it survives wterm re-rendering rows, and in the
  // capture phase so it settles the click before wterm turns it into a tmux
  // mouse report. A plain click is untouched and still reaches tmux.
  const hostRef = useRef<HTMLDivElement | null>(null)
  useEffect(() => {
    const el = hostRef.current
    return el ? installLinkOpener(el) : undefined
  }, [])

  const socketUrl = url ?? terminalUrl(session)

  // One socket per (session, url). Note that React's StrictMode runs this
  // twice in development, so a dev tab briefly creates two throwaway tmux
  // sessions -- the first is closed by the cleanup below and collected by
  // destroy-unattached. Production mounts once.
  useEffect(() => {
    const term = new TerminalSession({
      url: socketUrl,
      session,
      paneKey: paneStorageKey(session),
      onData: write,
      onStatus: (next) => {
        setStatus(next)
        statusCallback.current?.(next)
      },
    })
    sessionRef.current = term
    if (size.current) term.noteResize(size.current.cols, size.current.rows)
    term.start()
    return () => {
      term.stop()
      sessionRef.current = null
    }
  }, [session, socketUrl, write])

  useImperativeHandle(
    ref,
    () => ({
      select: (pane: string) => sessionRef.current?.select(pane) ?? false,
      copyMode: (pane?: string) => sessionRef.current?.copyMode(pane) ?? false,
      focus,
      retry: () => sessionRef.current?.retryNow(),
    }),
    [focus],
  )

  // Stable, and always installed: wterm's own `onData` wiring is torn out when
  // the prop goes undefined, so gating input by removing the handler would
  // leave keystrokes with nowhere to be noticed. They are refused one layer in,
  // by TerminalSession.write, which is what makes the pill below appear.
  const handleData = useCallback((data: string) => {
    sessionRef.current?.write(data)
  }, [])

  const handleResize = useCallback((cols: number, rows: number) => {
    size.current = { cols, rows }
    sessionRef.current?.noteResize(cols, rows)
  }, [])

  const live = status.phase === 'ready'

  // Focus once, when the terminal first goes live. Not on every reconnect: the
  // wterm instance and its DOM survive a dropped socket, so focus was never
  // lost, and stealing it back would yank the caret out of the command palette
  // of a user who opened it while the connection was flapping.
  const focused = useRef(false)
  useEffect(() => {
    if (live && !focused.current) {
      focused.current = true
      focus()
    }
  }, [live, focus])

  return (
    <div
      ref={hostRef}
      className={`relative h-full w-full overflow-hidden ${className ?? ''}`}
    >
      <WTermView
        ref={termRef}
        autoResize
        cursorBlink
        onData={handleData}
        onResize={handleResize}
        className={`h-full w-full transition-opacity ${live ? '' : 'opacity-60'}`}
      />
      <ConnectionPill
        status={status}
        session={label ?? session}
        onRetry={() => sessionRef.current?.retryNow()}
      />
    </div>
  )
}

/**
 * The whole of the disconnected UI: a pill in the corner, never a modal.
 *
 * A dialog over a terminal hides the very output the user is waiting on, and
 * this app's failure mode -- a phone changing networks -- resolves itself in a
 * second or two. But saying nothing is worse: keystrokes typed while the socket
 * is down are gone, and a terminal that silently ignores typing is
 * indistinguishable from an agent that hung. So the pill states the phase, the
 * dimmed terminal makes "not live" visible without reading it, and typing into
 * a dead socket escalates the wording rather than opening anything.
 *
 * Exported only so its copy can be rendered and asserted. What this says is the
 * whole of what the user is told when a session they killed cannot come back,
 * and it is not reachable from `TerminalSession`'s tests.
 */
export function ConnectionPill({
  status,
  session,
  onRetry,
}: {
  status: TerminalStatus
  /** What to call the session in the "gone" copy: a name, not an address. */
  session: string
  onRetry: () => void
}) {
  const { phase, attempt, inputDropped } = status
  if (phase === 'closed' || (phase === 'ready' && !inputDropped)) return null

  let text: string
  let action: string | null = null
  switch (phase) {
    case 'connecting':
      text = 'Connecting…'
      break
    case 'reconnecting':
      text = inputDropped
        ? `Reconnecting (${attempt}) — typing is not being sent`
        : `Reconnecting (${attempt})…`
      action = 'Retry now'
      break
    case 'ended':
      text = 'Session ended'
      action = 'New session'
      break
    case 'gone':
      // Names the session, because the sidebar is right there and the next
      // thing to do is pick a different one from it. "Try again" stays because
      // a tmux session name can be reused: if the owner recreates it, one
      // click is the whole recovery.
      text = `Session "${session}" is gone — pick another in the sidebar`
      action = 'Try again'
      break
    default:
      text = 'Connection is failing — some keystrokes were dropped'
      break
  }

  return (
    <div
      role="status"
      aria-live="polite"
      className="bg-background/90 text-foreground absolute top-2 right-2 z-10 flex items-center gap-2 rounded-md border px-3 py-1.5 text-xs shadow-sm backdrop-blur"
    >
      <span
        className={`size-2 rounded-full ${phase === 'ended' || phase === 'gone' ? 'bg-muted-foreground' : 'bg-destructive animate-pulse'}`}
        aria-hidden
      />
      <span>{text}</span>
      {action && (
        <button
          type="button"
          onClick={onRetry}
          className="hover:bg-accent hover:text-accent-foreground rounded border px-2 py-0.5 font-medium"
        >
          {action}
        </button>
      )}
    </div>
  )
}

export default Terminal
