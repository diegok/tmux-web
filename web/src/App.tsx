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
import {
  attachTarget,
  findPane,
  isSessionId,
  parseRememberedTarget,
  landedLabel,
  resolveSession,
  succeedPane,
  useSeenPanes,
  useSnapshot,
} from '@/lib/useSnapshot'
import type { PaneLocation, RememberedTarget } from '@/lib/useSnapshot'

/** Where this tab remembers its base session, so a reload lands where it was. */
const SESSION_KEY = 'tmux-web:session'

/**
 * Where it remembers the *address* of that session, which is not the same value.
 *
 * The key above is `session_group`, an identity; this is the `$N` the socket
 * actually attached with. Kept so the first mount after a reload is already
 * right -- see `attachTarget`, which explains what the second address costs.
 * Written as one JSON object rather than a second bare key so a memory can
 * never be read against the wrong session: the two halves arrive together or
 * not at all.
 */
const TARGET_KEY = 'tmux-web:target'

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
 * The address this tab last attached with, or null. The checking is in
 * `parseRememberedTarget`, where it can be tested without a browser.
 */
function readStoredTarget(): RememberedTarget | null {
  try {
    return parseRememberedTarget(globalThis.sessionStorage?.getItem(TARGET_KEY) ?? null)
  } catch {
    // Storage refused. The tab attaches by key, as it did before this existed.
    return null
  }
}

function storeTarget(remembered: RememberedTarget): void {
  try {
    globalThis.sessionStorage?.setItem(TARGET_KEY, JSON.stringify(remembered))
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

/**
 * What is left to do about a cross-session click, given what the terminal now
 * says. The rule behind the effect in `App`, out here where it can be tested:
 * everything else that effect does needs a live socket and a ref.
 *
 * `pendingPane` is an *optimistic* answer to "which pane is this tab on". It is
 * written the instant a pane in another session is clicked -- before the socket
 * that could select it exists -- so that the highlight and the breadcrumb
 * answer the click now rather than a reconnect later. Something has to end it,
 * and this is that something:
 *
 *  - **hold** -- the replacement socket is not live, so the override is still
 *    the only answer anything has. This is the state the whole mechanism exists
 *    for, and it is why "not ready" is checked before the pane is looked at:
 *    `TerminalSession` reports the pane it remembers for that session from the
 *    moment it is constructed, and a value read out of `sessionStorage` is not
 *    a socket that has selected anything.
 *  - **replay** -- the socket is live and on some other pane: send it the
 *    selection it could not receive when it was clicked.
 *  - **landed** -- the terminal reports the clicked pane itself. The override
 *    is now a second copy of `status.pane` and the caller must drop it.
 *
 * Dropping it exactly there is what makes the hand-off invisible: `activePane`
 * is `pendingPane ?? status.pane`, and `landed` is the one state in which the
 * fallback already reads the same pane, so nothing on screen moves as the
 * override goes away. Dropping it any earlier un-acknowledges the click --
 * `activePane` would fall back to the pane of the socket that was just thrown
 * away, and then to null. Never dropping it is worse and is what this rule was
 * written for: an optimistic override held past its landing is a *pin*, and it
 * pins `activePane` to that id for the life of the tab. Nothing shows until the
 * pane dies, and then everything does -- the successor effect stands down while
 * a switch is in flight, so there is no successor and no toast, the breadcrumb
 * settles on "%N is gone" over a live terminal, and every later reconnect's
 * `where` answer is masked by an id from a click made minutes ago.
 */
export type PendingStep = 'hold' | 'replay' | 'landed'

export function pendingStep(pending: string, status: TerminalStatus | null): PendingStep {
  if (status?.phase !== 'ready') return 'hold'
  return status.pane === pending ? 'landed' : 'replay'
}

export default function App() {
  const snapshot = useSnapshot()
  const term = useRef<TerminalHandle>(null)
  const [status, setStatus] = useState<TerminalStatus | null>(null)

  // Read once. `?session=` also disables the resolver below, so a tab pinned by
  // hand is never moved by a snapshot that cannot see its session.
  const [forced] = useState(forcedSession)
  const [picked, setPicked] = useState<string | null>(() => forced ?? readStoredSession())

  // Read once, for the same reason `forced` is: this is what the *first* render
  // attaches with, and a value that arrived later would be a value that arrived
  // after the socket it exists to address.
  const [remembered] = useState(readStoredTarget)

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
  //
  // `session` is the group key: an identity, and the vocabulary everything in
  // this component speaks -- the sidebar highlight, the click handler, what the
  // tab remembers across a reload. It is not an address, and handing it to the
  // socket is what made a renamed session unreachable: tmux freezes
  // `session_group` at the name the group was created under, and this app
  // creates the group itself on its first attach.
  const session = resolveSession(groups, { picked, forced, loaded })

  // What `/ws?session=` carries: the session id, or a hand-typed name passed
  // through. Derived here and nowhere else, so there is exactly one place where
  // identity becomes an address.
  const target = attachTarget(groups, session, remembered)

  // And the third thing the key is not: a display name. The header has to say
  // what the sidebar says, and both read the live `session_name` off the group
  // -- a breadcrumb printing the key would go on naming a renamed session by
  // the name it no longer has. The raw value is the fallback for a `?session=`
  // the snapshot has never heard of, where it is all there is to print.
  const sessionName = groups.find((g) => g.key === session)?.name ?? session

  useEffect(() => {
    if (session) storeSession(session)
    // Only an id is worth remembering. `target` is the session key passed
    // through whenever the snapshot has no address for it -- storing that would
    // remember the value this exists to replace, and the next load would attach
    // by name all over again.
    if (session && target && isSessionId(target)) storeTarget({ key: session, id: target })
  }, [session, target])

  /**
   * Replay a cross-session click once the new socket is up, and let the
   * override go once it has landed. `select` on a socket that is not ready
   * would be remembered by the *old* TerminalSession, which the switch threw
   * away.
   *
   * `status.pane` is what ends this: `select` records the pane locally whether
   * or not tmux honours it, so a pane that died in between still settles rather
   * than being re-sent on every status change -- and settling is what hands the
   * answer back to the terminal. See `pendingStep` for why the hand-off happens
   * there and not a moment before or after.
   */
  useEffect(() => {
    if (!pendingPane) return
    switch (pendingStep(pendingPane, status)) {
      case 'replay':
        term.current?.select(pendingPane)
        term.current?.focus()
        break
      case 'landed':
        // The one setState in an effect that oxlint would rather not see, and
        // it is not derivable during render: what it records is that the
        // terminal has caught up with a click, which is an event, not a
        // function of this render's props. It converges immediately --
        // `activePane` reads the same pane either way -- and runs once per
        // cross-session click.
        setPendingPane(null)
        break
    }
  }, [pendingPane, status])

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
  // terminal says it is on, with an optimistic override while a cross-session
  // switch is in flight.
  //
  // Never `paneActive`, and never it combined with a window: those are per
  // *session*, the snapshot is deduplicated to one row per pane with the user's
  // own session preferred, and this tab is attached through a throwaway session
  // whose current window is deliberately its own. Reading the snapshot that way
  // would highlight whatever the local terminal is looking at -- confidently,
  // and up to 1.5s late. The terminal asks its own socket instead; see
  // `TerminalSession`.
  const activePane = pendingPane ?? status?.pane ?? null
  const located = findPane(groups, activePane)

  /**
   * The last place the terminal was known to be.
   *
   * Remembered because `located` is null from the instant the pane dies, which
   * is exactly the moment its *window* is needed: the successor rule wants to
   * put the user back in the window they were working in, and by then the
   * snapshot no longer says which one that was.
   */
  const lastLocated = useRef<PaneLocation | null>(null)
  useEffect(() => {
    if (located) lastLocated.current = located
  }, [located])

  /**
   * The pane died under the selection. Follow tmux to whatever succeeded it.
   *
   * This app's record of where the tab is pointing is only ever written by
   * `select`: `TerminalSession` remembers the last pane it asked for and
   * nothing corrects it. tmux, meanwhile, has already moved the client to
   * another pane and taken the keyboard with it -- so with no correction the
   * user reads and types into one pane while the breadcrumb names another, the
   * sidebar highlights that other one, and `useSeenPanes` clears the badge on a
   * pane that no longer exists instead of the one being read.
   *
   * Re-pinning is what makes the app agree with the screen again. It is not the
   * whole answer, though: the user did lose their place and is owed the reason,
   * so the move is announced rather than performed silently.
   *
   * Focus is deliberately not taken. Navigating on purpose moves the keyboard
   * (see `handleSelectPane`) because the click came from outside the terminal;
   * this move did not, the caret is wherever the user left it, and yanking it
   * out of an open palette would be its own bug.
   *
   * ## The death that takes the session with it
   *
   * Closing the last tab of a session destroys the session, and this tab is
   * attached through a throwaway session in its group, so that goes too. It does
   * not end there: `resolveSession` has already moved the tab to another session
   * and the terminal comes back on a live pane of that one. The successor
   * therefore has to be allowed to cross a session boundary -- `session`, below,
   * is what lets it -- and until it was, this case ended with the app pointing
   * into a session that no longer existed: no row highlighted, a breadcrumb with
   * only a session name in it, and a user looking at a live terminal with
   * nothing anywhere saying which pane it was.
   *
   * The `activePane` guard survives that, and the reason is worth writing down
   * because it does not look like it should. `<Terminal>` is rebuilt around the
   * new address on the same render, and the pane it remembers is filed *per
   * address*, so the status it reports next has no pane in it at all. But that
   * status is a `setState` from a child effect: it is queued, not applied, and
   * the effect here still runs against the status committed before the switch --
   * which still names the pane that died. By the render where `activePane` would
   * be null, the move has happened and `located` ends this effect one line
   * earlier. Guarding on `lastLocated` alone instead was tried and changes
   * nothing that any test can see, while giving every *other* reason the
   * terminal's address can change -- a group key resolving to a `$N` on the
   * first poll, say -- a way to yank a tab off a perfectly good pane.
   *
   * Two of the guards are load-bearing. `lastLocated` having to still describe
   * `activePane` is what keeps this to one move per death: after the `select`
   * the remembered location is the successor, so the next poll falls straight
   * through. And a null successor -- an empty snapshot from a failed daemon
   * poll, a `?session=` tab pinned to a session that is wholly gone, a tab
   * reloaded onto a pane that had already died -- leaves the tab exactly where
   * it is, and the breadcrumb goes on saying the pane is gone, because then that
   * is the whole of what is known.
   */
  useEffect(() => {
    if (!loaded || !activePane || located || pendingPane) return
    const was = lastLocated.current
    if (!was || was.pane.paneId !== activePane) return
    // `session` and not `was.session.key`: where the tab is attached *now*, so
    // that a session destroyed under it can still be succeeded by the one the
    // socket has already moved to.
    const to = succeedPane(groups, was, session)
    if (!to || !term.current?.select(to.pane.paneId)) return
    lastLocated.current = to
    toast(`${was.pane.command} closed`, {
      description: `${activePane} is gone. Moved to ${landedLabel(to, was)}.`,
    })
  }, [loaded, activePane, located, pendingPane, groups, session])

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
        {/*
          `h-svh` and not `min-h-svh`: a floor with no ceiling is what made the
          terminal grow on full-screen and never shrink back.

          The column below is `flex-1 min-h-0`, which can only shrink if its
          parent has a height that does not come from its own content. Under
          `min-h-svh` the parent's height *was* its content: once the terminal
          had laid out, say, 54 rows, those rows were 986px of content inside an
          800px viewport, the shell grew to 986px, and `flex-1` then resolved
          against 986px on every later pass. Widening worked because the shell
          has always been bounded horizontally; narrowing the viewport left the
          rows where they were, so tmux was told a height that had never come
          back down. See `e2e/sizing.spec.ts`.

          `overflow-hidden` keeps that a layout invariant rather than a
          coincidence: the shell is exactly one viewport and the page never
          scrolls, so nothing inside can push a height back into it. Menus,
          dialogs and toasts are unaffected -- every one of them portals to
          <body>, outside this element.
        */}
        <SidebarInset className="h-svh overflow-hidden">
          <header className="flex h-11 shrink-0 items-center gap-2 border-b px-2">
            <SidebarTrigger />
            <Separator orientation="vertical" className="mr-1 !h-4" />
            <Breadcrumb
              session={sessionName}
              located={located}
              activePane={activePane}
              loaded={loaded}
            />
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
            {target ? (
              <Terminal
                session={target}
                label={sessionName ?? target}
                onStatusChange={setStatus}
                ref={term}
              />
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
 * A pane the snapshot cannot find is still named rather than hidden, but this
 * is now the *last* resort and not the first. A pane that dies under the
 * selection is normally succeeded within a poll -- see the `succeedPane` effect
 * in `App` -- and the breadcrumb then describes the pane the terminal actually
 * moved to, which is the question it exists to answer.
 *
 * What is left here is the case where nothing better is known. Not the session
 * going: that is followed too, into whichever session the tab was moved to. Nor
 * a tab loading onto a pane that had already died: the socket now answers with
 * the pane it actually landed on, so what is remembered being gone is corrected
 * rather than printed. It is a pane that dies in the gap between the socket
 * answering and the snapshot catching up, and a `?session=` tab pinned to a
 * session that has died, which must stay pinned rather than wander. Naming the
 * dead pane is then the honest reading, because it is all the tab has.
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
