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

import { Plus, Search } from 'lucide-react'
import { ThemeProvider } from 'next-themes'
import { useCallback, useEffect, useReducer, useRef, useState } from 'react'
import { toast } from 'sonner'

import { AppSidebar } from '@/components/AppSidebar'
import { KILL_CLOSED, KillDialog, canKill, killDialogReducer } from '@/components/KillDialog'
import { PALETTE_CHORD_LABEL, Palette } from '@/components/Palette'
import {
  PROMPT_CLOSED,
  PromptDialog,
  promptAction,
  promptDialogReducer,
} from '@/components/PromptDialog'
import { Terminal } from '@/components/Terminal'
import type { TerminalHandle, TerminalStatus } from '@/components/Terminal'
import { Separator } from '@/components/ui/separator'
import { SidebarInset, SidebarProvider, SidebarTrigger } from '@/components/ui/sidebar'
import { Toaster } from '@/components/ui/sonner'
import { newSessionPrompt, runManage } from '@/lib/manage'
import type { ManageAction, MenuIntent } from '@/lib/manage'
import { useTabBadge } from '@/lib/tabBadge'
import { findPane, resolveSession, useSeenPanes, useSnapshot } from '@/lib/useSnapshot'

/** Where this tab remembers its base session, so a reload lands where it was. */
const SESSION_KEY = 'tmux-web:session'

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

  /** The command palette, opened by the header button or by Ctrl+Alt+K. */
  const [paletteOpen, setPaletteOpen] = useState(false)

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
    term.current?.focus()
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
        return
      }
      // Navigating means "I want to work in that pane", so the keyboard has to
      // follow. Both affordances that get here take focus themselves -- a
      // sidebar button keeps it, and the palette closing leaves it on <body> --
      // so without this you land on a pane and cannot type into it until you
      // click the terminal. On a phone that also means no on-screen keyboard,
      // and the palette is the primary way to navigate there.
      term.current.focus()
    },
    [session],
  )

  // The pane the sidebar highlights and the breadcrumb describes: what the
  // terminal says it is pinned to, with an optimistic override while a
  // cross-session switch is in flight. Never `paneActive` -- that is tmux's
  // current pane per window, which is shared by the group and up to 1.5s old.
  const activePane = pendingPane ?? status?.pane ?? null
  const located = findPane(groups, activePane)

  // This device's memory of which finished runs it has already been shown, and
  // the write that clears one: looking at a pane is what marks it seen. It is
  // read here rather than inside the sidebar because the tab badge counts the
  // same `done` panes -- two copies of the map would each clear their own half,
  // and the badge would keep counting a pane you are looking at.
  const seen = useSeenPanes(snapshot.serverStart, activePane, snapshot.rows)

  // `(2) tmux-web` and a dot on the favicon while an agent is blocked or has
  // finished unseen. Both are late while the tab is in the background and stop
  // entirely on a locked phone -- the accepted cost of a badge over push, which
  // the module header spells out.
  useTabBadge(snapshot.rows, snapshot.serverStart, seen)

  // The two dialogs management needs, and the reducers behind them. Both state
  // machines live in their own files, where the rules that matter -- a
  // dismissal disarming the kill, a prompt opening on the *current* name --
  // are testable without a renderer.
  const [kill, dispatchKill] = useReducer(killDialogReducer, KILL_CLOSED)
  const [prompt, dispatchPrompt] = useReducer(promptDialogReducer, PROMPT_CLOSED)

  const { refresh } = snapshot

  /**
   * Send one management call.
   *
   * The failure path is `runManage`'s: a toast naming what was attempted and
   * tmux's own words about why not, and a refresh either way so the sidebar
   * catches up now rather than in 1.5s. Nothing is retried -- a failed kill
   * that silently succeeded on a retry is worse than one that failed.
   */
  const send = useCallback(
    (action: ManageAction) =>
      runManage(action, {
        notify: ({ title, description }) => toast.error(title, { description }),
        refresh,
      }),
    [refresh],
  )

  /** A menu entry was chosen, from the sidebar or from the palette. */
  const handleIntent = useCallback(
    (intent: MenuIntent) => {
      if (intent.kind === 'run') {
        void send(intent.action)
      } else if (intent.kind === 'prompt') {
        dispatchPrompt({ type: 'open', spec: intent.prompt })
      } else {
        dispatchKill({ type: 'open', plan: intent.plan })
      }
    },
    [send],
  )

  const submitPrompt = useCallback(() => {
    const action = promptAction(prompt)
    if (!action) return
    dispatchPrompt({ type: 'send' })
    // Closed on success, held open on a refusal. What the daemon refuses here
    // is the *name* -- a ":", a leading "-", the length cap -- and that is
    // fixable in the field it was typed in, so closing would only mean typing
    // it all again.
    void send(action).then((result) =>
      dispatchPrompt(result.ok ? { type: 'dismiss' } : { type: 'settle' }),
    )
  }, [prompt, send])

  const confirmKill = useCallback(() => {
    // The same rule the red button's `disabled` is drawn from, rather than a
    // second spelling of it here: a handler that trusted the button would kill
    // on an Enter that reached it some other way.
    if (!canKill(kill) || !kill.plan) return
    dispatchKill({ type: 'send' })
    // Closed either way, unlike the prompt: a kill fails because the id it
    // named is already gone, and there is nothing in this dialog to correct.
    // The toast says what happened and the sidebar has already refreshed.
    void send(kill.plan.action).finally(() => dispatchKill({ type: 'dismiss' }))
  }, [kill, send])

  // False means the socket is not ready, which the palette reports rather than
  // closing on a command that did nothing. The header button is disabled in
  // that state, so it never gets there.
  const copyMode = useCallback(() => {
    const ok = term.current?.copyMode() ?? false
    // Copy mode is driven from the keyboard, so it is useless without focus --
    // and the header button steals it on click.
    if (ok) term.current?.focus()
    return ok
  }, [])

  return (
    // One theme for the app and the terminal at once -- both are CSS custom
    // properties -- so the provider wraps everything, and the class it writes on
    // <html> is what `@custom-variant dark` in index.css keys off.
    <ThemeProvider attribute="class" defaultTheme="system" enableSystem disableTransitionOnChange>
      <SidebarProvider>
        <AppSidebar
          snapshot={snapshot}
          seen={seen}
          activePane={activePane}
          activeSession={session}
          onSelectPane={handleSelectPane}
          onRefresh={snapshot.refresh}
          onIntent={handleIntent}
          connection={status?.phase ?? null}
        />
        <SidebarInset className="min-h-svh">
          <header className="flex h-11 shrink-0 items-center gap-2 border-b px-2">
            <SidebarTrigger />
            <Separator orientation="vertical" className="mr-1 !h-4" />
            <Breadcrumb session={session} located={located} activePane={activePane} loaded={loaded} />
            <div className="ml-auto flex items-center gap-2">
              <button
                type="button"
                onClick={() => setPaletteOpen(true)}
                className="hover:bg-accent hover:text-accent-foreground flex items-center gap-1.5 rounded-md border px-2 py-1 text-xs font-medium"
                title={`Jump to a pane or run a command (${PALETTE_CHORD_LABEL})`}
              >
                <Search className="size-3" aria-hidden />
                <span className="hidden sm:inline">Jump to…</span>
                <kbd className="text-muted-foreground hidden font-mono text-[10px] md:inline">
                  {PALETTE_CHORD_LABEL}
                </kbd>
              </button>
              <button
                type="button"
                onClick={() => copyMode()}
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
              <NoSession
                loaded={loaded}
                onNewSession={() =>
                  dispatchPrompt({ type: 'open', spec: newSessionPrompt() })
                }
              />
            )}
          </div>
        </SidebarInset>

        <Palette
          open={paletteOpen}
          onOpenChange={setPaletteOpen}
          groups={groups}
          activePane={activePane}
          activeSession={session}
          onSelectPane={handleSelectPane}
          onCopyMode={copyMode}
          onIntent={handleIntent}
        />

        <PromptDialog
          state={prompt}
          onDismiss={() => dispatchPrompt({ type: 'dismiss' })}
          onChange={(field, value) => dispatchPrompt({ type: 'set', field, value })}
          onSubmit={submitPrompt}
        />
        <KillDialog
          state={kill}
          onDismiss={() => dispatchKill({ type: 'dismiss' })}
          onArmedChange={(armed) => dispatchKill({ type: 'arm', armed })}
          onConfirm={confirmKill}
        />
        {/*
          Where a failed management call lands: what was attempted, and tmux's
          own sentence about why not. Mounted here rather than beside each
          caller so there is one of it -- sonner renders a single region and a
          second `<Toaster>` would double every toast.
        */}
        <Toaster position="bottom-right" closeButton />
      </SidebarProvider>
    </ThemeProvider>
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
 * No terminal, because there is no session to attach to yet.
 *
 * v1 could only explain how to start one over SSH: it had no session-creating
 * endpoint, and a button that cannot work is worse than a sentence that does.
 * It has one now, so this is a button -- and this is the case the design points
 * at, a phone with no shell on this host and an empty tmux server.
 */
function NoSession({ loaded, onNewSession }: { loaded: boolean; onNewSession: () => void }) {
  return (
    <div className="text-muted-foreground flex h-full items-center justify-center p-6 text-center text-sm">
      {loaded ? (
        <div className="max-w-sm space-y-3">
          <p>
            There is no tmux session to attach to. Start one here, or on the host with{' '}
            <code className="font-mono">tmux new -s work</code>.
          </p>
          <button
            type="button"
            onClick={onNewSession}
            className="hover:bg-accent hover:text-accent-foreground inline-flex items-center gap-1.5 rounded-md border px-2.5 py-1.5 font-medium"
          >
            <Plus className="size-3.5" aria-hidden />
            New session
          </button>
        </div>
      ) : (
        <p>Looking for tmux…</p>
      )}
    </div>
  )
}
