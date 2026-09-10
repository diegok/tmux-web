/**
 * The navigation tree: every tmux session, window and pane the daemon can see,
 * refreshed from `useSnapshot` and clicked to move this tab.
 *
 * ## Shape
 *
 * `SidebarGroup` per session group, `SidebarMenu` of windows inside it, and
 * `SidebarMenuSub` of panes **only when a window has more than one**. A window
 * with a single pane is the pane, and rendering a lone child under it would add
 * a row and a disclosure to say nothing.
 *
 * ## What a pane row says, and on which line
 *
 * `paneText` picks one of label, title or `pane_current_command` -- and *where*
 * it goes follows from which one won. A command is a program's name: one short
 * word, and it keeps the monospaced capsule it has always had, on the same line
 * as the window name. A title is what an agent is working on: prose, routinely
 * longer than the sidebar is wide, and it gets **a second line of its own** --
 * no capsule, a step smaller and a step dimmer than the name above it, cut with
 * an ellipsis and scrolled on hover (see `.row-line` in `index.css`).
 *
 * That split is the point. Cramming a sentence into the capsule that held `zsh`
 * squeezed the window name it sat beside, and cut both; giving every `zsh` row
 * a second line to say `zsh` on would cost a line per row to say nothing. Only
 * the rows with something to say grow.
 *
 * A title also arrives with the agent's own branding on the front of it -- `✳ `,
 * `OC | `, `π - ` -- which the row is already saying in the mark beside it, so
 * one recognised prefix comes off before the title is shown. That is display
 * only and it is per agent: see `AGENT_TITLE_PREFIXES`.
 *
 * ## Blocks
 *
 * A session group is ruled off from the one above it. A rule is 1px of height
 * and no width at all, which is what makes it affordable on a phone -- indents
 * and gutters are not -- and it survives into the collapsed icon rail, where
 * the groups are otherwise a single column of undifferentiated glyphs.
 *
 * ## Which one needs you
 *
 * An agent pane carries its agent's mark and a state dot; a window and a
 * session carry the most urgent state under them, `blocked > done > working >
 * idle`, so the question is answerable without expanding anything. A pane the
 * daemon computed no state for carries neither -- a shell is not idle, it is a
 * shell. `done` is the one state this browser works out for itself, by
 * comparing the daemon's `finishedAt` with what it remembers being shown -- see
 * `useSeenPanes`, which App calls and passes in as `seen`.
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
import { newSessionPrompt, newWindowAction, rowMenu, rowTargetForWindow } from '@/lib/manage'
import type { MenuIntent, RowTarget } from '@/lib/manage'
import { cn } from '@/lib/utils'
import { paneState, sessionState, windowState, windowTarget } from '@/lib/useSnapshot'
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
  /**
   * This device's memory of which finished runs it has already been shown --
   * `useSeenPanes`, called in App.
   *
   * A prop rather than a hook call here because the tab badge counts the same
   * `done` panes one level up: two copies of the map would each clear their own
   * half, and the badge would go on counting the pane you are looking at.
   */
  seen: SeenMap
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
  seen,
  activePane,
  activeSession,
  onSelectPane,
  onRefresh,
  onIntent,
  connection = null,
}: AppSidebarProps) {
  const { groups, loaded, serverStart } = snapshot

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
            // The block separator. `first:` rather than a gap or a margin so
            // the rule only ever appears *between* groups, and 1px of height
            // is the whole cost -- a phone loses no width to it.
            <SidebarGroup
              key={session.key}
              className="border-sidebar-border border-t first:border-t-0"
            >
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
                {/*
                  shadcn ships this list at `gap-0`, which was fine while every
                  row was one line: the rows were the rhythm. A two-line row is
                  a block, and blocks that touch read as one list again.
                */}
                <SidebarMenu className="gap-0.5">
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
            className={cn(
              // `h-auto` rather than shadcn's `size="lg"`: lg is a fixed h-12,
              // which would make every one-line row 48px tall to accommodate
              // the rows that have a title, and it also sets `p-0` in the icon
              // rail -- which shifts the glyph 8px left of every other row's.
              // Growing only when there is a second line keeps the tree as
              // short as it was and leaves the rail untouched, because the
              // `size-8!` that clips it there is still the one in force.
              'row-hover h-auto min-h-8',
              holdsActive && split ? 'text-sidebar-accent-foreground' : undefined,
            )}
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
            {lone ? (
              <PaneLines pane={lone} name={label} commandWidth="max-w-32" />
            ) : (
              // A split window has no pane of its own to describe, so it is the
              // row it always was: one line, one name.
              <span className="truncate">{label}</span>
            )}
          </SidebarMenuButton>
        </div>
      </RowMenu>

      {split && (
        <SidebarMenuSub>
          {window.panes.map((pane) => (
            <SidebarMenuSubItem key={pane.paneId}>
              <RowMenu
                target={{ kind: 'pane', session, window, pane }}
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
                  // `h-auto` for the same reason as the window row above,
                  // against this button's own fixed `h-7`. `w-full` because a
                  // `<button>` is shrink-to-fit and shadcn does not set it
                  // here: without it the second line is only as wide as the
                  // words above it, which is the cramping this rework is
                  // about. It also puts the command capsule on the right-hand
                  // edge, where the window rows have always had it.
                  className="row-hover h-auto min-h-7 w-full"
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
                    <PaneLines
                      pane={pane}
                      name={`pane ${pane.paneIndex}`}
                      commandWidth="max-w-24"
                      afterName={
                        pane.active && (
                          <span
                            className="bg-sidebar-foreground/40 size-1.5 shrink-0 rounded-full"
                            title="tmux's current pane in this window"
                            // role="img" is what gives an empty span a name a
                            // screen reader will read; aria-label alone on a
                            // generic element is ignored.
                            role="img"
                            aria-label="current in tmux"
                          />
                        )
                      }
                    />
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

/**
 * Each agent's own branding, at the head of the titles it writes.
 *
 * The row already says which agent this is: `AgentIcon` draws the project's own
 * mark a few pixels to the left of this text. Saying it again in words spends
 * the *front* of a 16rem line repeating the icon -- and the front of the line is
 * the half that survives the ellipsis, so the redundant characters are the ones
 * that always show and the useful ones are what gets cut.
 *
 * Keyed by `pane_current_command`, and a subset of Go's `tmux.Agents` exactly as
 * `AGENT_MARKS` is: a pane that is not a known agent has its title left alone.
 * That is not a formality. A shell sitting where an agent last ran keeps the
 * agent's title -- `zsh` titled `π - browsers` is on this machine right now --
 * and that row has no mark beside it, so there is nothing there for the branding
 * to be redundant with.
 *
 * Each entry is the branding *without* its trailing space; the separator is the
 * matching rule's, not the table's, so that a title trimmed down to nothing but
 * its branding still matches. See `withoutAgentPrefix`.
 *
 * ## Where each entry came from
 *
 * Every string below was read out of the binary that writes it and checked
 * against the bytes of a live pane title, because a prefix table that is a
 * little bit wrong either does nothing or eats a word.
 *
 * **claude** -- `claude.exe` is Bun-compiled but its JS is literal inside, and
 * it builds the title as `` `${frame} ${text}` `` out of
 * `var eD=["◐","◑"],tD="✳"`: `tD` when it is holding still, `eD[n]` when it is
 * animating, a frame every 960ms. Under a multiplexer the animation is gated
 * off and the title holds at `✳` -- which is what sampling the live panes gave,
 * 1500 reads of `#{pane_title}` over ten minutes with both claude panes mid-run
 * and `✳` every time. That gate is a feature flag, so the two animated frames
 * are listed anyway: the prefix coming back the moment an agent starts working
 * is precisely the moment it is being looked at.
 *
 * The rest -- `· ✢ ✶ ✻ ✽` -- are the frames of the *in-pane* spinner in the
 * same binary (three arrays of them, one per terminal). That is where the
 * `✻ ✽ ✳ ✶` set in the report comes from; whether a build ever drove the title
 * off it or the report is reading the glyph on the `Ruminating…` line instead,
 * this file has no evidence either way, and it does not need any: five strings
 * cost nothing and make the entry version-proof in both directions. Note the
 * design doc's own correction on the same glyph -- a `✳` that is *assumed* to
 * animate has already been wrong here once, so the safe move is to cover the
 * set and pin it.
 *
 * Not `*`. The same spinner has an ASCII-asterisk frame for terminals that are
 * not ghostty, and it has never been a title glyph. A line of prose that opens
 * `* ` is ordinary; a line that opens `✻ ` is not. It is the one candidate that
 * could take a word off a real title, and it buys nothing observable.
 *
 * **opencode** -- `` setTerminalTitle(`OC | ${title}`) ``, an ASCII pipe, which
 * is what the live pane's title bytes say too (`4f 43 20 7c 20`). The report
 * quotes it with a box-drawing `│`; nothing ships that today, but one extra
 * string makes sure a font that draws `|` like `│` cannot turn a mis-read into
 * a bug.
 *
 * **pi** -- `APP_TITLE` is `"π"` and the title is `` `${APP_TITLE} - ${cwd}` ``,
 * so `π -` (live bytes `cf 80 20 2d 20`). `APP_TITLE` becomes the *configured*
 * name when pi is renamed in its config, and that name is unknowable from here,
 * so a renamed pi keeps its whole title. Showing a prefix is a much smaller
 * failure than guessing at one.
 */
export const AGENT_TITLE_PREFIXES: Record<string, readonly string[]> = {
  claude: ['✳', '◐', '◑', '·', '✢', '✶', '✻', '✽'],
  opencode: ['OC |', 'OC │'],
  pi: ['π -'],
}

/**
 * A title with its agent's branding taken off the front, or the title unchanged.
 *
 * **One prefix, matched literally, only for the agent that writes it, and only
 * where a space or the end of the title follows it.** No character class and no
 * repetition: `✳ ✻ done` loses the `✳` and keeps the `✻`, `✱ done` keeps its
 * glyph because `✱` is not `✳`, `✳done` keeps its because nothing separates the
 * two, and `π - x` on a claude pane keeps its because claude does not write it.
 * Getting a redundant glyph off the front of a row is worth a little; taking the
 * first word off a real title is worth a good deal less than nothing, and a rule
 * loose enough to do the first is loose enough to do the second.
 *
 * "Or the end of the title" is what makes `π - `, whose caller has already
 * trimmed it to `π -`, come back as the empty string rather than as itself --
 * which is the answer `paneText` needs in order to fall back rather than render
 * a blank line.
 *
 * `Object.hasOwn` rather than a bare lookup: a command is whatever binary the
 * user happened to run, and `constructor` is a legal filename.
 */
function withoutAgentPrefix(title: string, command: string): string {
  if (!Object.hasOwn(AGENT_TITLE_PREFIXES, command)) return title
  for (const prefix of AGENT_TITLE_PREFIXES[command]) {
    if (!title.startsWith(prefix)) continue
    const rest = title.slice(prefix.length)
    if (rest === '') return ''
    if (rest.startsWith(' ')) return rest.trim()
  }
  return title
}

/** What a pane row says about itself, and which line it says it on. */
interface PaneText {
  text: string
  /**
   * The native tooltip, when the row has more to say than it shows. A blocked
   * agent's question always does -- its choices are what you need in order to
   * decide whether it is worth switching to -- and a title does whenever it is
   * cut, which the row cannot know and does not have to: the attribute costs
   * nothing when the text happens to fit.
   */
  tooltip?: string
  /**
   * The text is `pane_current_command` -- a program name, not prose someone
   * wrote. That is what decides the whole layout of the row: it keeps the
   * monospaced capsule, on the same line as the name, and gets no tooltip of
   * its own -- a command is one short word, and on a pane row what the row's
   * own `title=` already says (the pane id and index) is more use than
   * repeating it.
   */
  fromCommand: boolean
}

/**
 * What a pane row says: **label, else title, else command**.
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
 * to the command, so those rows look exactly as they did before this existed --
 * one line, one capsule -- and only the rows with something to say grow one.
 *
 * ## Earned on the raw title, shown without the branding
 *
 * A title that earns the row then loses its agent's own prefix -- see
 * `AGENT_TITLE_PREFIXES`. Both of the tests above are made against the title
 * tmux reported, deliberately, and only the *display* is stripped: `π - master`
 * earns its row because `π - master` has a space in it, and re-testing the
 * `master` that is left would fail the hostname rule and drop the row back to a
 * `pi` capsule -- losing the one word it had to say to a rule written for
 * `thinkpad`.
 *
 * The one thing stripping may not do is empty the row. A title that is nothing
 * but branding -- `π - `, a pi with no session yet -- has nothing left once the
 * branding goes, so it falls through to the command, the same fallback the
 * hostname and the echoed command already take. A dim, empty second line would
 * be a worse row than the glyph was.
 *
 * Nothing else on the way in is touched. A label is the user's own words and a
 * blocked question is the agent's own, quoted; neither is branding, and neither
 * is something this may edit.
 */
function paneText(
  pane: Pick<PaneNode, 'command' | 'title' | 'label' | 'agentState' | 'question'>,
): PaneText {
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
  if (label !== '') return { text: label, fromCommand: false, tooltip: label }

  const title = pane.title.trim()
  const command = pane.command.trim()
  if (title !== '' && !HOSTNAME_LIKE.test(title) && title.toLowerCase() !== command.toLowerCase()) {
    const text = withoutAgentPrefix(title, command)
    if (text !== '') return { text, fromCommand: false, tooltip: text }
  }
  return { text: pane.command, fromCommand: true }
}

/**
 * A pane row's text: the name, and what the pane is doing under it.
 *
 * One line or two, decided by `paneText` and by nothing else. A command shares
 * the first line with the name, in the capsule it has always had; a title takes
 * a second line to itself.
 *
 * The two lines are one column so that the name truncates against the same edge
 * the title does, and so the row's leading icon centres against the pair rather
 * than against the first line. `min-w-0` on both is what lets either of them
 * truncate at all: a flex item's floor is its content, so without it a long
 * title would push the row wider than the sidebar instead of being cut.
 */
function PaneLines({
  pane,
  name,
  commandWidth,
  afterName,
}: {
  pane: PaneNode
  name: string
  /** How much of the first line the capsule may take, before the name gives way. */
  commandWidth: string
  /** The tmux-active marker, on a split window's pane rows. */
  afterName?: ReactNode
}) {
  const { text, fromCommand, tooltip } = paneText(pane)
  return (
    <span className="flex min-w-0 flex-1 flex-col justify-center gap-0.5">
      <span className="flex min-w-0 items-center gap-2">
        <span className="truncate">{name}</span>
        {afterName}
        {fromCommand && (
          // Unchanged, deliberately: a command is a program's name, and a row
          // running `zsh` should go on looking exactly like a row running
          // `zsh`. The distinction between "this is a process" and "this is
          // what an agent is doing" is carried by the shape of the row.
          <Badge variant="secondary" className={cn('ml-auto truncate font-mono', commandWidth)}>
            {text}
          </Badge>
        )}
      </span>
      {!fromCommand && (
        <span
          // `.row-line` is the clip, the ellipsis and the hover marquee, all of
          // which are CSS -- see index.css. Dimmer *and* a step smaller than the
          // name above: the name is the identity and this is the description,
          // and the tree is read by scanning the names.
          className="row-line text-sidebar-foreground/60 text-xs leading-tight font-normal"
          title={tooltip}
        >
          {/* The element the marquee moves. It has to be a child of the clip:
              one box cannot both hide its overflow and slide inside itself. */}
          <span>{text}</span>
        </span>
      )}
    </span>
  )
}

/**
 * The choices, under the question, in the one tooltip the second line can carry.
 *
 * A native `title` is what the rest of this file already uses for the overflow
 * of a truncated line, and it is the only tooltip a row can have without
 * fighting the `title=` the row itself sets. Newlines are honoured by every
 * browser's implementation of it. It is also the whole of what a reduced-motion
 * reader gets in place of the marquee, and the whole of what a phone gets in
 * place of the hover -- which is why it is set on every title, cut or not.
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
