/**
 * The shell: sidebar on the left, one terminal on the right.
 *
 * ## Who owns what
 *
 * The tree comes from `useSnapshot` (the daemon's cached `tmux list-panes`),
 * the current pane comes from `<Terminal>`, and this component owns exactly one
 * piece of state neither of them can: **which base session this tab is attached
 * to**. That is a prop of the socket -- `/ws?session=work` -- so changing it
 * replaces the WebSocket and the throwaway tmux session behind it, which is
 * why it is not something a click should do casually.
 *
 * It matters because a pane can only be selected from a socket whose session is
 * in the same group as that pane's window. `select-window -t '=<throwaway>:@7'`
 * against a window belonging to another group fails; the daemon logs it and
 * carries on, so a naive sidebar would silently do nothing. Clicking a pane in
 * another session therefore re-attaches first and replays the selection once
 * the new socket is ready.
 *
 * ## What the header says
 *
 * The breadcrumb is `session › window › command`, resolved from the snapshot
 * for the pane the terminal reports. The dot is the socket, not tmux: green
 * only while input is actually going somewhere.
 */

import { useCallback, useEffect, useRef, useState } from 'react'

import { AppSidebar } from '@/components/AppSidebar'
import { Terminal } from '@/components/Terminal'
import type { TerminalHandle, TerminalStatus } from '@/components/Terminal'
import { Separator } from '@/components/ui/separator'
import { SidebarInset, SidebarProvider, SidebarTrigger } from '@/components/ui/sidebar'
import { findPane, resolveSession, useSnapshot } from '@/lib/useSnapshot'

/** Where this tab remembers its base session, so a reload lands where it was. */
const SESSION_KEY = 'wterm-web:session'

function readStoredSession(): string | null {
  try {
    return globalThis.sessionStorage?.getItem(SESSION_KEY) ?? null
  } catch {
    // Disabled storage costs the tab its session across a reload, nothing more.
    return null
  }
}

function storeSession(session: string): void {
  try {
    globalThis.sessionStorage?.setItem(SESSION_KEY, session)
  } catch {
    /* see above */
  }
}

/**
 * `?session=` pins the tab to a base session by hand. It is also an escape
 * hatch: a tab started this way is never moved by the resolver below, so a
 * session the snapshot cannot see -- because the whole poll is failing -- can
 * still be attached to.
 */
function forcedSession(): string | null {
  return new URLSearchParams(window.location.search).get('session')
}

export default function App() {
  const snapshot = useSnapshot()
  const term = useRef<TerminalHandle>(null)
  const [status, setStatus] = useState<TerminalStatus | null>(null)

  // Read once. `?session=` also disables the resolver below, so a tab pinned by
  // hand is never moved by a snapshot that cannot see its session.
  const [forced] = useState(forcedSession)
  const [picked, setPicked] = useState<string | null>(() => forced ?? readStoredSession())

  /**
   * A pane clicked in a session this tab is not attached to. It drives the
   * highlight immediately, so the click is acknowledged now rather than when
   * the replacement socket comes up, and it is what the effect below replays.
   */
  const [pendingPane, setPendingPane] = useState<string | null>(null)

  const { groups, loaded } = snapshot

  // Task 20's placeholder guessed "main", which the daemon answers with a 404
  // on the socket -- surfacing as a terminal that reconnects forever. The rules
  // behind this live in the lib, where they can be tested.
  const session = resolveSession(groups, { picked, forced, loaded })

  useEffect(() => {
    if (session) storeSession(session)
  }, [session])

  /**
   * Replay a cross-session click once the new socket is up. `select` on a
   * socket that is not ready would be remembered by the *old* TerminalSession,
   * which the switch threw away.
   *
   * `status.pane` is what ends this: `select` records the pane locally whether
   * or not tmux honours it, so a pane that died in between still settles rather
   * than being re-sent on every status change.
   */
  useEffect(() => {
    if (!pendingPane || status?.phase !== 'ready' || status.pane === pendingPane) return
    term.current?.select(pendingPane)
  }, [pendingPane, status?.phase, status?.pane])

  const handleSelectPane = useCallback(
    (paneId: string, groupKey: string) => {
      if (groupKey !== session) {
        setPendingPane(paneId)
        setPicked(groupKey)
        return
      }
      setPendingPane(null)
      // False means the id was not a pane id, which from a snapshot-derived
      // row means the wire contract moved under us. Saying nothing would look
      // exactly like a click that worked.
      if (!term.current?.select(paneId)) {
        console.error('sidebar: the terminal refused to select', paneId)
      }
    },
    [session],
  )

  // The pane the sidebar highlights and the breadcrumb describes: what the
  // terminal says it is pinned to, with an optimistic override while a
  // cross-session switch is in flight. Never `paneActive` -- that is tmux's
  // current pane per window, which is shared by the group and up to 1.5s old.
  const activePane = pendingPane ?? status?.pane ?? null
  const located = findPane(groups, activePane)

  return (
    <SidebarProvider>
      <AppSidebar
        snapshot={snapshot}
        activePane={activePane}
        activeSession={session}
        onSelectPane={handleSelectPane}
        onRefresh={snapshot.refresh}
      />
      <SidebarInset className="min-h-svh">
        <header className="flex h-11 shrink-0 items-center gap-2 border-b px-2">
          <SidebarTrigger />
          <Separator orientation="vertical" className="mr-1 !h-4" />
          <Breadcrumb session={session} located={located} activePane={activePane} loaded={loaded} />
          <div className="ml-auto flex items-center gap-2">
            <button
              type="button"
              onClick={() => term.current?.copyMode()}
              disabled={status?.phase !== 'ready'}
              className="hover:bg-accent hover:text-accent-foreground rounded-md border px-2 py-1 text-xs font-medium disabled:opacity-50"
              title="Enter tmux copy mode, where this app's scrollback lives"
            >
              Copy mode
            </button>
            <ConnectionDot status={status} />
          </div>
        </header>
        <div className="min-h-0 flex-1">
          {session ? (
            <Terminal session={session} onStatusChange={setStatus} ref={term} />
          ) : (
            <NoSession loaded={loaded} />
          )}
        </div>
      </SidebarInset>
    </SidebarProvider>
  )
}

/**
 * `work › 2: api › claude`.
 *
 * A pane the snapshot cannot find is not hidden: the terminal is still pinned
 * to it as far as this tab knows, and "%3 (gone)" is the honest reading of a
 * pane that died under the selection -- the daemon logged a failed select and
 * left the session on whatever tmux moved to. The next poll usually resolves it
 * by the terminal reporting a different pane, or by the user clicking one.
 */
function Breadcrumb({
  session,
  located,
  activePane,
  loaded,
}: {
  session: string | null
  located: ReturnType<typeof findPane>
  activePane: string | null
  loaded: boolean
}) {
  const sep = <span className="text-muted-foreground/50 px-1.5">›</span>
  return (
    <nav aria-label="Location" className="flex min-w-0 items-center text-sm">
      <span className="text-muted-foreground truncate">{session ?? '—'}</span>
      {located ? (
        <>
          {sep}
          <span className="text-muted-foreground truncate">
            {located.window.index}: {located.window.name}
          </span>
          {sep}
          <span className="truncate font-medium">{located.pane.command}</span>
        </>
      ) : (
        activePane &&
        loaded && (
          <>
            {sep}
            <span className="text-muted-foreground truncate italic">{activePane} is gone</span>
          </>
        )
      )}
    </nav>
  )
}

/** The socket's state, in the one place a user glances at without reading. */
function ConnectionDot({ status }: { status: TerminalStatus | null }) {
  const phase = status?.phase ?? 'connecting'
  const tone =
    phase === 'ready'
      ? 'bg-emerald-500'
      : phase === 'ended' || phase === 'closed'
        ? 'bg-muted-foreground'
        : 'bg-amber-500 animate-pulse'
  return (
    <span className="text-muted-foreground flex items-center gap-1.5 text-xs" role="status">
      <span className={`size-2 rounded-full ${tone}`} aria-hidden />
      <span className="hidden sm:inline">{phase}</span>
    </span>
  )
}

/**
 * No terminal, because there is no session to attach to yet. Deliberately not
 * an offer to create one: v1's API has no session-creating endpoint, and a
 * button that cannot work is worse than a sentence that explains.
 */
function NoSession({ loaded }: { loaded: boolean }) {
  return (
    <div className="text-muted-foreground flex h-full items-center justify-center p-6 text-center text-sm">
      {loaded ? (
        <p className="max-w-sm">
          There is no tmux session to attach to. Start one on the host —{' '}
          <code className="font-mono">tmux new -s work</code> — and this tab picks it up.
        </p>
      ) : (
        <p>Looking for tmux…</p>
      )}
    </div>
  )
}
