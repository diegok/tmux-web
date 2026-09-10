/**
 * The navigation tree: every tmux session, window and pane the daemon can see,
 * refreshed from `useSnapshot` and clicked to move this tab.
 *
 * ## Shape
 *
 * `SidebarGroup` per session group, `SidebarMenu` of windows inside it, and
 * `SidebarMenuSub` of panes **only when a window has more than one**. A window
 * with a single pane is the pane, and rendering a lone child under it would add
 * a row and a disclosure to say nothing. The `Badge` says what the pane is: its
 * label, its title, or `pane_current_command` -- see `paneBadge`.
 *
 * ## Which one needs you
 *
 * An agent pane carries its agent's mark and a state dot; a window and a
 * session carry the most urgent state under them, `blocked > done > working >
 * idle`, so the question is answerable without expanding anything. A pane the
 * daemon computed no state for carries neither -- a shell is not idle, it is a
 * shell. `done` is the one state this browser works out for itself, by
 * comparing the daemon's `finishedAt` with what it remembers being shown; see
 * `useSeenPanes`.
 *
 * A session row shows the **live session name**, never the group key it is
 * identified by: tmux freezes `session_group` at the pre-rename name, so a
 * sidebar labelled on it makes renaming look like it did nothing.
 *
 * ## Responsiveness is shadcn's, not ours
 *
 * `Sidebar` collapses to icons on desktop and swaps itself for a `Sheet` drawer
 * below the mobile breakpoint on its own. Nothing here reimplements that: the
 * design's "phone as a bonus" decision is only cheap while there is exactly one
 * layout to maintain.
 *
 * ## Where the highlight comes from
 *
 * Not from `paneActive`. That field is tmux's active pane *per window*, shared
 * by every session in the group and up to 1.5s out of date, so it answers "what
 * would this window show if you opened it", not "what is this tab looking at".
 * The highlight is the pane the `<Terminal>` says it is pinned to, passed down
 * as `activePane` -- see App.tsx.
 */

import { Columns2, Plus, RefreshCw, SquareTerminal, TriangleAlert } from 'lucide-react'
import { Fragment } from 'react'
import type { ReactNode } from 'react'

import { AGENT_MARKS, AgentIcon } from '@/components/AgentIcon'
import { UserMenu } from '@/components/UserMenu'
import { Badge } from '@/components/ui/badge'
import {
  ContextMenu,
  ContextMenuContent,
  ContextMenuItem,
  ContextMenuLabel,
  ContextMenuSeparator,
  ContextMenuTrigger,
} from '@/components/ui/context-menu'
import {
  Sidebar,
  SidebarContent,
  SidebarGroup,
  SidebarGroupContent,
  SidebarGroupLabel,
  SidebarFooter,
  SidebarHeader,
  SidebarMenu,
  SidebarMenuButton,
  SidebarMenuItem,
  SidebarMenuSkeleton,
  SidebarMenuSub,
  SidebarMenuSubButton,
  SidebarMenuSubItem,
  SidebarRail,
} from '@/components/ui/sidebar'
import { TooltipProvider } from '@/components/ui/tooltip'
import type { TerminalPhase } from '@/components/Terminal'
import { NO_WINDOW_ID, newSessionPrompt, newWindowAction, rowMenu, rowTargetForWindow } from '@/lib/manage'
import type { MenuIntent, RowTarget } from '@/lib/manage'
import { cn } from '@/lib/utils'
import { paneState, sessionState, useSeenPanes, windowState, windowTarget } from '@/lib/useSnapshot'
import type {
  DisplayState,
  PaneNode,
  SeenMap,
  SessionNode,
  SnapshotQuestion,
  SnapshotState,
  WindowNode,
} from '@/lib/useSnapshot'


export interface AppSidebarProps {
  /** Everything `useSnapshot` knows, including why it might be out of date. */
  snapshot: SnapshotState
  /** The pane this tab is pinned to, from the terminal's status. */
  activePane: string | null
  /** The base session this tab is attached to. */
  activeSession: string | null
  /**
   * A pane row was clicked. The group key comes with it because a pane in
   * another session group cannot be selected from this tab's socket -- the
   * window is not in its group -- so the caller has to re-attach first.
   */
  onSelectPane: (paneId: string, groupKey: string) => void
  /** Poll now: the Retry button on the failed and empty states. */
  onRefresh: () => void
  /**
   * A management entry was chosen -- from a row's context menu, from the
   * long-press that opens the same menu on touch, or from one of the `+`
   * buttons.
   *
   * The sidebar decides *what is offered* (that is `rowMenu`, over the row that
   * was clicked) and nothing about what happens next: a rename opens a prompt,
   * a kill opens the two-step dialog, and a split is sent immediately. All
   * three live in App, which owns the dialogs and the refresh.
   */
  onIntent: (intent: MenuIntent) => void
  /**
   * The socket's phase, for the footer's second line. Passed down rather than
   * read here: the terminal owns it, and the sidebar is not on that path.
   */
  connection?: TerminalPhase | null
}

export function AppSidebar({
  snapshot,
  activePane,
  activeSession,
  onSelectPane,
  onRefresh,
  onIntent,
  connection = null,
}: AppSidebarProps) {
  const { groups, loaded, serverStart, rows } = snapshot
  // This device's memory of which finished runs it has already been shown, and
  // the write that clears one: looking at a pane is what marks it seen.
  const seen = useSeenPanes(serverStart, activePane, rows)

  return (
    // The tooltips on the collapsed rail are this component's, so the provider
    // is too: `SidebarMenuButton tooltip=` renders a Radix `Tooltip`, which
    // throws outright without one in scope. shadcn's `SidebarProvider` does not
    // supply it in this version. Task 22: tooltips outside this subtree need
    // their own provider, or move this one up to App.
    <TooltipProvider>
      <Sidebar collapsible="icon">
        <SidebarHeader>
          <div className="flex items-center gap-2 px-2 py-1">
            <SquareTerminal className="size-4 shrink-0" aria-hidden />
            <span className="truncate font-medium group-data-[collapsible=icon]:hidden">
              tmux-web
            </span>
            {/*
              The one affordance that cannot live on a row: with no tmux server
              running there are no rows, nothing to right-click, and v1 could
              not fix that from the browser at all. It sits in the header so it
              is in the same place whether the tree is empty or full.
            */}
            <NewButton
              label="New session"
              className="ml-auto group-data-[collapsible=icon]:hidden"
              onClick={() => onIntent({ kind: 'prompt', prompt: newSessionPrompt() })}
            />
          </div>
        </SidebarHeader>

        <SidebarContent>
          {groups.map((session) => (
            <SidebarGroup key={session.key}>
              <RowMenu
                target={{ kind: 'session', session }}
                activeSession={activeSession}
                onIntent={onIntent}
              >
                <SidebarGroupLabel
                  className={session.key === activeSession ? 'text-sidebar-foreground' : undefined}
                >
                  {/*
                    The roll-up: the most urgent state anywhere under this
                    session. Visible only while the sidebar is expanded --
                    shadcn fades group labels out in the icon rail -- so the
                    window rows below carry their own, on their icons, for the
                    collapsed case.
                  */}
                  <StateDot state={sessionState(session, serverStart, seen)} className="mr-1.5" />
                  {/*
                    The live session name, never `session.key`. tmux freezes
                    session_group at the name the group was created under, so a
                    sidebar keyed and labelled on it shows the pre-rename name
                    forever -- which makes renaming from the browser look like it
                    did nothing. The key is still the identity: it is the React
                    key, the `?session=` value and what a click carries.
                  */}
                  <span className="truncate">{session.name}</span>
                  {session.appOnly && (
                    // The user's own session under this group name is gone; its
                    // windows are only still alive because a browser tab is holding
                    // the group open. Worth saying, because attaching to it by name
                    // is exactly what the daemon will refuse.
                    <span className="text-sidebar-foreground/50 ml-1 truncate text-[10px] font-normal">
                      (orphaned)
                    </span>
                  )}
                  {/*
                    The session row's own `+`. A new window is what a session
                    row can create -- the row is the session -- and it opens in
                    the working directory of the pane that window would show,
                    which the daemon resolves from the pane id.
                  */}
                  {!session.appOnly && (
                    <NewButton
                      label={`New window in "${session.name}"`}
                      className="ml-auto"
                      onClick={() =>
                        onIntent({
                          kind: 'run',
                          action: newWindowAction({ kind: 'session', session }),
                        })
                      }
                    />
                  )}
                </SidebarGroupLabel>
              </RowMenu>
              <SidebarGroupContent>
                <SidebarMenu>
                  {session.windows.map((window) => (
                    <WindowItem
                      key={window.key}
                      window={window}
                      session={session}
                      activePane={activePane}
                      activeSession={activeSession}
                      onSelectPane={onSelectPane}
                      onIntent={onIntent}
                      serverStart={serverStart}
                      seen={seen}
                      // An orphaned group has no session of its own left to
                      // attach to, so a click could only start a socket the
                      // daemon answers with 404 -- and it would drop a working
                      // one to do it. Unless this tab is already inside that
                      // group, in which case its own socket can still select
                      // these panes and nothing is out of reach.
                      reachable={!session.appOnly || session.key === activeSession}
                    />
                  ))}
                </SidebarMenu>
              </SidebarGroupContent>
            </SidebarGroup>
          ))}

          <SidebarStatus
            snapshot={snapshot}
            onRefresh={onRefresh}
            onIntent={onIntent}
            hasTree={loaded && groups.length > 0}
          />
        </SidebarContent>

        <SidebarFooter>
          <UserMenu connection={connection} />
        </SidebarFooter>

        <SidebarRail />
      </Sidebar>
    </TooltipProvider>
  )
}

/**
 * One window, with its panes underneath when there is more than one.
 *
 * Clicking the window row selects the window's own active pane, so the row does
 * what `select-window` alone would do rather than silently landing on pane 0.
 * Right-clicking it -- or holding it down on a touch screen -- offers what can
 * be done to that window; the pane rows offer what can be done to a pane.
 */
function WindowItem({
  window,
  session,
  activePane,
  activeSession,
  onSelectPane,
  onIntent,
  reachable,
  serverStart,
  seen,
}: {
  window: WindowNode
  session: SessionNode
  activePane: string | null
  activeSession: string | null
  onSelectPane: (paneId: string, groupKey: string) => void
  onIntent: (intent: MenuIntent) => void
  reachable: boolean
  serverStart: string
  seen: SeenMap
}) {
  const sessionKey = session.key
  const split = window.panes.length > 1
  const target = windowTarget(window)
  const holdsActive = window.panes.some((p) => p.paneId === activePane)
  const label = `${window.index}: ${window.name}`
  // The window's own roll-up. On a single-pane window that is the pane's state,
  // which is why the pane below it gets no second dot of its own.
  const state = windowState(window, serverStart, seen)
  const lone = split ? undefined : window.panes[0]
  const unreachable = reachable
    ? undefined
    : 'This session was killed; its panes are only alive because a tab is holding the group open'

  // On a single-pane window this row *is* the pane, and its menu is the pane's
  // too -- the rule is `rowTargetForWindow`'s, where it can be tested.
  const rowTarget = rowTargetForWindow(session, window)

  return (
    <SidebarMenuItem>
      <RowMenu target={rowTarget} activeSession={activeSession} onIntent={onIntent}>
        <div>
          <SidebarMenuButton
            // A split window is "active" when the tab is in one of its panes, but
            // the pane row below carries the highlight; marking both would read as
            // two selections.
            isActive={holdsActive && !split}
            tooltip={label}
            disabled={!reachable}
            title={unreachable}
            onClick={() => target && onSelectPane(target, sessionKey)}
            className={holdsActive && split ? 'text-sidebar-accent-foreground' : undefined}
          >
            <RowIcon
              state={state}
              icon={
                split ? (
                  <Columns2 aria-hidden />
                ) : lone && Object.hasOwn(AGENT_MARKS, lone.command) ? (
                  // The agent's own mark says more than a generic terminal glyph,
                  // and on a single-pane window this row *is* the pane. A pane
                  // running something else keeps the glyph.
                  <AgentIcon command={lone.command} />
                ) : (
                  <SquareTerminal aria-hidden />
                )
              }
            />
            <span className="truncate">{label}</span>
            {lone && <PaneBadge pane={lone} width="max-w-32" />}
          </SidebarMenuButton>
        </div>
      </RowMenu>

      {split && (
        <SidebarMenuSub>
          {window.panes.map((pane) => (
            <SidebarMenuSubItem key={pane.paneId}>
              <RowMenu
                target={{ kind: 'pane', session, window, windowId: NO_WINDOW_ID, pane }}
                activeSession={activeSession}
                onIntent={onIntent}
              >
                <SidebarMenuSubButton
                  asChild
                  isActive={pane.paneId === activePane}
                  // `title` rather than a tooltip: sub-items are hidden in the
                  // icon-collapsed rail, so there is nothing for a tooltip to
                  // hang off, and the pane id is the thing a user debugging a
                  // selection actually wants to read.
                  title={unreachable ?? `${pane.paneId} · pane ${pane.paneIndex}`}
                >
                  <button
                    type="button"
                    disabled={!reachable}
                    onClick={() => onSelectPane(pane.paneId, sessionKey)}
                  >
                    <RowIcon
                      state={paneState(pane, serverStart, seen)}
                      // Null rather than an <AgentIcon> that renders nothing:
                      // an element returning null is still an element, and
                      // RowIcon would reserve 16px of gutter for it on every
                      // shell row. hasOwn rather than `in` because `in` walks the
                      // prototype, so a pane whose command happened to be
                      // `toString` would take this branch and be handed a
                      // function to draw.
                      icon={
                        Object.hasOwn(AGENT_MARKS, pane.command) ? (
                          <AgentIcon command={pane.command} />
                        ) : null
                      }
                    />
                    <span className="truncate">pane {pane.paneIndex}</span>
                    {pane.active && (
                      <span
                        className="bg-sidebar-foreground/40 size-1.5 shrink-0 rounded-full"
                        title="tmux's current pane in this window"
                        // role="img" is what gives an empty span a name a screen
                        // reader will read; aria-label alone on a generic element
                        // is ignored.
                        role="img"
                        aria-label="current in tmux"
                      />
                    )}
                    <PaneBadge pane={pane} width="max-w-24" />
                  </button>
                </SidebarMenuSubButton>
              </RowMenu>
            </SidebarMenuSubItem>
          ))}
        </SidebarMenuSub>
      )}
    </SidebarMenuItem>
  )
}

/**
 * The management menu for one row: right-click, or hold it down on a touch
 * screen.
 *
 * Radix's trigger runs a 700ms timer on a touch or pen pointerdown and opens
 * the same menu from it, so the long-press the design asks for is the same code
 * as the right-click rather than a second gesture to maintain.
 *
 * What is offered is `rowMenu`'s answer and nothing else -- this renders it.
 * When a row has nothing to offer (an app-owned group, which the daemon refuses
 * to touch) the children are returned bare, so no menu opens on a row where
 * every entry would be disabled.
 */
function RowMenu({
  target,
  activeSession,
  onIntent,
  children,
}: {
  target: RowTarget
  activeSession: string | null
  onIntent: (intent: MenuIntent) => void
  children: ReactNode
}) {
  const entries = rowMenu(target, activeSession)
  if (entries.length === 0) return <>{children}</>
  return (
    <ContextMenu>
      <ContextMenuTrigger asChild>{children}</ContextMenuTrigger>
      <ContextMenuContent>
        <ContextMenuLabel>{menuHeading(target)}</ContextMenuLabel>
        {entries.map((entry) => (
          <Fragment key={entry.id}>
            {/*
              The kill is the only `danger` entry, and the rule is the design's:
              it is separated from everything above it, so the destructive item
              is never the one under a thumb that meant to hit the item above.
            */}
            {entry.danger && <ContextMenuSeparator />}
            <ContextMenuItem
              variant={entry.danger ? 'destructive' : 'default'}
              onSelect={() => onIntent(entry.intent)}
            >
              <span>{entry.label}</span>
              {entry.hint && (
                // Rendered rather than hidden in a tooltip: this is the line
                // saying zoom moves the terminal on the host too, and a phone
                // has no hover to reveal it with.
                <span className="text-muted-foreground text-xs">{entry.hint}</span>
              )}
            </ContextMenuItem>
          </Fragment>
        ))}
      </ContextMenuContent>
    </ContextMenu>
  )
}

/** What the menu says it is acting on, so a mis-aimed right-click is obvious. */
function menuHeading(target: RowTarget): string {
  switch (target.kind) {
    case 'session':
      return `session "${target.session.name}"`
    case 'window':
      return `window ${target.window.index}: ${target.window.name}`
    case 'pane':
      return `pane ${target.pane.paneIndex} · ${target.pane.paneId}`
  }
}

/** A small `+`, named for a screen reader and for the hover it gets on desktop. */
function NewButton({
  label,
  onClick,
  className,
}: {
  label: string
  onClick: () => void
  className?: string
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      title={label}
      aria-label={label}
      className={cn(
        'hover:bg-sidebar-accent hover:text-sidebar-accent-foreground flex size-5 shrink-0 items-center justify-center rounded-md',
        className,
      )}
    >
      <Plus className="size-3.5" aria-hidden />
    </button>
  )
}

/**
 * Everything the tree cannot say for itself: the first load, the three ways of
 * having nothing to show, and a quiet note when what is shown is old.
 *
 * The distinction that matters is between *no panes* and *no answer*. An empty
 * tmux server is a 200 with zero rows and is not a fault; a failed poll with
 * rows behind it must leave those rows alone; a failed poll with nothing behind
 * it is the only case where the sidebar has genuinely nothing to render.
 */
function SidebarStatus({
  snapshot,
  onRefresh,
  onIntent,
  hasTree,
}: {
  snapshot: SnapshotState
  onRefresh: () => void
  onIntent: (intent: MenuIntent) => void
  hasTree: boolean
}) {
  const { loaded, stale, error, unauthorized } = snapshot

  // Hidden in the collapsed rail: none of it fits in 3rem, and the tree beside
  // it is reduced to icons anyway.
  const box = 'px-3 py-2 text-xs group-data-[collapsible=icon]:hidden'

  if (unauthorized) {
    return (
      <div className={box} role="status">
        <p className="text-sidebar-foreground font-medium">This device is no longer enrolled</p>
        <p className="text-sidebar-foreground/70 mt-1">
          Its access was revoked, or the cookie expired. Enroll again from the host with{' '}
          <code className="font-mono">wterm-web enroll</code>.
        </p>
      </div>
    )
  }

  if (!loaded) {
    return error ? (
      <div className={box} role="status">
        <p className="text-sidebar-foreground flex items-center gap-1.5 font-medium">
          <TriangleAlert className="size-3.5" aria-hidden />
          Cannot read tmux
        </p>
        <p className="text-sidebar-foreground/70 mt-1 break-words">{error}</p>
        <RetryButton onRefresh={onRefresh} />
      </div>
    ) : (
      // First load. Skeletons rather than "Loading…" because the tree they
      // stand in for arrives within one poll.
      <div className="px-2 py-1" aria-hidden>
        <SidebarMenu>
          {[0, 1, 2].map((i) => (
            <SidebarMenuItem key={i}>
              <SidebarMenuSkeleton showIcon />
            </SidebarMenuItem>
          ))}
        </SidebarMenu>
      </div>
    )
  }

  if (!hasTree) {
    return (
      <div className={box} role="status">
        <p className="text-sidebar-foreground font-medium">No tmux session</p>
        {/*
          v1 could only tell you to go and start one over SSH, because it had no
          endpoint that could. It has one now, and this is the case the design
          calls out as the one with nothing to right-click: an empty tmux
          server, reachable from a phone that has no shell on this host at all.
        */}
        <p className="text-sidebar-foreground/70 mt-1">
          Nothing is running on this host yet. Start one here, or over SSH with{' '}
          <code className="font-mono">tmux new -s work</code>.
        </p>
        <div className="flex flex-wrap items-center gap-2">
          <button
            type="button"
            onClick={() => onIntent({ kind: 'prompt', prompt: newSessionPrompt() })}
            className="hover:bg-sidebar-accent hover:text-sidebar-accent-foreground mt-2 inline-flex items-center gap-1.5 rounded-md border px-2 py-1 font-medium"
          >
            <Plus className="size-3" aria-hidden />
            New session
          </button>
          <RetryButton onRefresh={onRefresh} />
        </div>
      </div>
    )
  }

  if (stale) {
    return (
      <div className={`${box} text-sidebar-foreground/70`} role="status">
        <p className="flex items-center gap-1.5">
          <TriangleAlert className="size-3.5 shrink-0" aria-hidden />
          <span>Showing the last good state — tmux is not answering.</span>
        </p>
        {error && <p className="mt-1 break-words">{error}</p>}
      </div>
    )
  }

  return null
}

function RetryButton({ onRefresh }: { onRefresh: () => void }) {
  return (
    <button
      type="button"
      onClick={onRefresh}
      className="hover:bg-sidebar-accent hover:text-sidebar-accent-foreground mt-2 inline-flex items-center gap-1.5 rounded-md border px-2 py-1 font-medium"
    >
      <RefreshCw className="size-3" aria-hidden />
      Retry now
    </button>
  )
}

/**
 * A pane title that is only a hostname: one token of the characters a hostname
 * is made of, and nothing else.
 *
 * tmux gives every pane the machine's hostname as its title and leaves it there
 * until something sets one, so a sidebar that showed titles unconditionally
 * would print the same word down every shell row -- the field would cost a row
 * of width to say where you already know you are. Nothing else about a title
 * distinguishes the default from a real one; it is not empty and it is not the
 * command.
 *
 * Narrow on purpose: it suppresses `thinkpad` and `dev-box.local` but not
 * `~/devel/tmux-web` or `✳ Thinking`, so the only titles it can lose are
 * single bare words, and losing one costs the command that was there before.
 */
const HOSTNAME_LIKE = /^[A-Za-z0-9][A-Za-z0-9._-]*$/

/** What a pane's badge says, and whether the text is a command. */
interface PaneBadgeText {
  text: string
  /**
   * The native tooltip, when the badge has more to say than it shows. Only a
   * blocked agent's question does: its choices are what you need in order to
   * decide whether it is worth switching to.
   */
  tooltip?: string
  /**
   * The text is `pane_current_command` -- a program name, not prose someone
   * wrote. It keeps the monospaced badge and gets no tooltip of its own: a
   * command is one short word, and on a pane row what the row's own `title=`
   * already says -- the pane id and index -- is more use than repeating it.
   */
  fromCommand: boolean
}

/**
 * The badge for a pane: **label, else title, else command**.
 *
 * The label is a name the user gave this pane and wins outright, including over
 * a title a program is rewriting underneath it -- that is the whole point of
 * having one. (Nothing sets a label from the browser until Task 12's rename;
 * `tmux set -p @wterm_label` already does, and the field is already on the
 * wire, so the order is honoured now rather than left as a field that is read
 * and ignored.)
 *
 * A title has to earn the row. Claude Code sets it to what it is working on,
 * which is far better than three rows all reading `claude`, but two kinds of
 * title say nothing the row does not already say: the hostname every untouched
 * pane carries, and a title that is just the command again. Both fall through
 * to the command, so those rows look exactly as they did before this existed.
 */
function paneBadge(
  pane: Pick<PaneNode, 'command' | 'title' | 'label' | 'agentState' | 'question'>,
): PaneBadgeText {
  // A blocked agent's own words outrank both. The whole app exists to answer
  // "which one needs me, and for what", and once one of them is asking, the
  // question is the answer -- the identity is still in the window row above it
  // and in the agent's mark beside it. Only when the daemon actually read the
  // dialog: detection and extraction are separate, so a restyled approval box
  // costs the quote and keeps the state.
  if (pane.agentState === 'blocked' && pane.question && pane.question.text.trim() !== '') {
    const text = pane.question.text.trim()
    return {
      text,
      fromCommand: false,
      tooltip: questionTooltip(text, pane.question),
    }
  }

  const label = pane.label.trim()
  if (label !== '') return { text: label, fromCommand: false }

  const title = pane.title.trim()
  const command = pane.command.trim()
  if (title !== '' && !HOSTNAME_LIKE.test(title) && title.toLowerCase() !== command.toLowerCase()) {
    return { text: title, fromCommand: false }
  }
  return { text: pane.command, fromCommand: true }
}

/**
 * The badge itself.
 *
 * Truncation is CSS: the sidebar is 16rem wide and a title arrives capped at
 * 256 bytes, so no width the badge could be given makes measuring in JS worth a
 * layout pass. The full text goes in `title=`, which is where the rest of a
 * truncated row is reachable without a tooltip library and without a click.
 *
 * `shrink` overrides the badge's own `shrink-0` for a title: when a long window
 * name and a long title compete for one row, the window name is the identity
 * and the title is the description, so the title is what gives way.
 */
function PaneBadge({ pane, width }: { pane: PaneNode; width: string }) {
  const { text, fromCommand, tooltip } = paneBadge(pane)
  return (
    <Badge
      variant="secondary"
      title={tooltip ?? (fromCommand ? undefined : text)}
      className={cn('ml-auto truncate', width, fromCommand ? 'font-mono' : 'shrink font-normal')}
    >
      {text}
    </Badge>
  )
}

/**
 * The choices, under the question, in the one tooltip a badge can carry.
 *
 * A native `title` is what the rest of this file already uses for the overflow
 * of a truncated badge, and it is the only tooltip a row can have without
 * fighting the `title=` the row itself sets. Newlines are honoured by every
 * browser's implementation of it.
 *
 * The choices are what make the question actionable -- "Yes / Yes, and don't
 * ask again / No" tells you whether this is a decision or a formality -- but
 * they are far too wide for a 16rem sidebar, so they live here rather than in
 * the row.
 */
function questionTooltip(text: string, question: SnapshotQuestion): string {
  const choices = question.choices ?? []
  return choices.length === 0 ? text : `${text}\n\n${choices.join('\n')}`
}

/**
 * How each state looks and what it is called.
 *
 * Colour is the glance and the label is the answer: the dot is 8px and carries
 * no text, so `aria-label` is the whole of what a screen reader gets and
 * `title` is the whole of what a user who cannot tell amber from emerald gets.
 * Never colour alone.
 *
 * `working` is the only one that moves. A pulse on `blocked` would be louder
 * than the state that is actually waiting on nobody, and four animated dots in
 * a sidebar is a slot machine.
 */
const STATE_TONE: Record<DisplayState, { className: string; label: string }> = {
  blocked: {
    className: 'bg-amber-500',
    label: 'blocked — waiting for an answer',
  },
  done: {
    className: 'bg-emerald-500',
    label: 'done — finished since you last looked',
  },
  working: { className: 'bg-sky-500 animate-pulse', label: 'working' },
  idle: { className: 'bg-sidebar-foreground/30', label: 'idle' },
}

/**
 * One state dot, or nothing at all.
 *
 * `""` renders nothing, and that is the rule the whole feature rests on: it
 * means the daemon computed no state -- the pane is not a known agent, or the
 * poll was taken with no browser connected -- and a dot there would be a claim
 * about a pane nothing looked at. A shell gets no dot.
 */
function StateDot({ state, className }: { state: DisplayState | ''; className?: string }) {
  if (state === '') return null
  const tone = STATE_TONE[state]
  return (
    <span
      // The hook every test in this file uses, and the one thing about the dot
      // that is not a colour.
      data-agent-state={state}
      className={cn('size-2 shrink-0 rounded-full', tone.className, className)}
      // role="img" is what gives an empty span a name a screen reader will
      // read; aria-label alone on a generic element is ignored.
      role="img"
      aria-label={tone.label}
      title={tone.label}
    />
  )
}

/**
 * A row's leading icon with its state dot on the corner.
 *
 * The dot rides the icon rather than sitting at the end of the row because the
 * end of the row is not there when the sidebar is collapsed: shadcn shrinks a
 * menu button to `size-8` with `overflow-hidden`, so everything after the first
 * 16px is clipped. This keeps "which project needs me" answerable from the icon
 * rail, which is the case the roll-up exists for. The 4px overhang stays inside
 * the button's own 8px padding, so it survives that clip.
 *
 * `icon` is null for a pane running something with no mark: an element that
 * renders nothing is still an element, and reserving a 16px gutter on every
 * shell row for one would be worse than the dot moving 16px left.
 */
function RowIcon({ state, icon }: { state: DisplayState | ''; icon: ReactNode }) {
  // Nothing to hang a dot on, or no dot to hang: both render exactly what came
  // in, so a row with no agent in it is the row it was before this existed.
  if (state === '') return <>{icon}</>
  if (!icon) return <StateDot state={state} />
  return (
    <span className="relative flex size-4 shrink-0 items-center justify-center">
      {icon}
      <StateDot state={state} className="absolute -right-1 -bottom-1" />
    </span>
  )
}

export default AppSidebar
