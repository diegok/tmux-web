/**
 * The terminal pane: one `@wterm/react` terminal bound to one tmux attach, with
 * the reconnect policy that the transport deliberately does not have.
 *
 * `Transport` (see `@/lib/transport`) owns exactly one WebSocket for its whole
 * life and never reopens, because a reopen here is not a reopen at all -- the
 * server answers a new socket with a *new* throwaway tmux session, grouped onto
 * the same base session but landing on whatever window that group's active
 * window happens to be. Everything that makes a reconnect safe therefore lives
 * in this file:
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
 *     on the group's active window: the fallback the design asks for, for free.
 *   - Resize is debounced. tmux sizes a window to its most recently active
 *     client, so an undebounced drag would repeatedly yank the dimensions of
 *     the terminal the user is sitting in front of locally.
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
 */
export function shouldReconnect(close: TransportClose): boolean {
  return close.code !== 1000
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
  return `wterm-web:pane:${session}`
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
  /** `stop()` was called: the component unmounted or the session changed. */
  | 'closed'

export interface TerminalStatus {
  phase: TerminalPhase
  /** Consecutive failed connections; back to 0 on every successful open. */
  attempt: number
  /** Delay until the scheduled retry, when phase is "reconnecting". */
  retryDelayMs: number
  /** Pane id this tab is pinned to, or null for the group's active window. */
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

  /** Last size wterm reported; 0 until it has laid out. */
  #cols = 0
  #rows = 0
  /** The size actually sent on this socket, so a reopen re-sends. */
  #sentCols = 0
  #sentRows = 0

  #resizeTimer: ReturnType<typeof setTimeout> | null = null
  #retryTimer: ReturnType<typeof setTimeout> | null = null
  #stopped = false

  constructor(opts: TerminalSessionOptions) {
    this.#opts = opts
    this.#storage = opts.storage === undefined ? defaultStorage() : opts.storage
    this.#random = opts.random ?? Math.random
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
   * Reconnect now instead of waiting out the backoff, and from "ended" as well:
   * there the user is explicitly asking for a new session, which is what a new
   * socket is.
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
      onData: (bytes) => {
        if (transport === this.#transport) this.#opts.onData(bytes)
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

    // The server attaches at 80x24 until told otherwise, so a reconnect that
    // skipped this would redraw the pane at the wrong size.
    this.#sentCols = 0
    this.#sentRows = 0
    this.#sendResize()

    // Before 'ready'. If this pane is gone the server logs it and the session
    // stays on the group's active window, which is the intended fallback.
    if (this.#pane) transport.select(this.#pane)

    this.#phase = 'ready'
    this.#emit()
  }

  #closed(transport: Transport, event: TransportClose): void {
    if (transport !== this.#transport) return
    this.#transport = null
    if (this.#resizeTimer) {
      clearTimeout(this.#resizeTimer)
      this.#resizeTimer = null
    }

    if (!shouldReconnect(event)) {
      this.#phase = 'ended'
      this.#retryDelayMs = 0
      this.#emit()
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
  /** Base tmux session this tab groups onto. */
  session: string
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

export function Terminal({ session, url, className, onStatusChange, ref }: TerminalProps) {
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
 */
function ConnectionPill({
  status,
  onRetry,
}: {
  status: TerminalStatus
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
        className={`size-2 rounded-full ${phase === 'ended' ? 'bg-muted-foreground' : 'bg-destructive animate-pulse'}`}
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
