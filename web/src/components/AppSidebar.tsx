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

import { Columns2, RefreshCw, SquareTerminal, TriangleAlert } from 'lucide-react'

import { UserMenu } from '@/components/UserMenu'
import { Badge } from '@/components/ui/badge'
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
import { cn } from '@/lib/utils'
import { windowTarget } from '@/lib/useSnapshot'
import type { PaneNode, SnapshotState, WindowNode } from '@/lib/useSnapshot'

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
  connection = null,
}: AppSidebarProps) {
  const { groups, loaded } = snapshot

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
          </div>
        </SidebarHeader>

        <SidebarContent>
          {groups.map((session) => (
            <SidebarGroup key={session.key}>
              <SidebarGroupLabel
                className={session.key === activeSession ? 'text-sidebar-foreground' : undefined}
              >
                <span className="truncate">{session.key}</span>
                {session.appOnly && (
                  // The user's own session under this group name is gone; its
                  // windows are only still alive because a browser tab is holding
                  // the group open. Worth saying, because attaching to it by name
                  // is exactly what the daemon will refuse.
                  <span className="text-sidebar-foreground/50 ml-1 truncate text-[10px] font-normal">
                    (orphaned)
                  </span>
                )}
              </SidebarGroupLabel>
              <SidebarGroupContent>
                <SidebarMenu>
                  {session.windows.map((window) => (
                    <WindowItem
                      key={window.key}
                      window={window}
                      sessionKey={session.key}
                      activePane={activePane}
                      onSelectPane={onSelectPane}
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
 */
function WindowItem({
  window,
  sessionKey,
  activePane,
  onSelectPane,
  reachable,
}: {
  window: WindowNode
  sessionKey: string
  activePane: string | null
  onSelectPane: (paneId: string, groupKey: string) => void
  reachable: boolean
}) {
  const split = window.panes.length > 1
  const target = windowTarget(window)
  const holdsActive = window.panes.some((p) => p.paneId === activePane)
  const label = `${window.index}: ${window.name}`
  const unreachable = reachable
    ? undefined
    : 'This session was killed; its panes are only alive because a tab is holding the group open'

  return (
    <SidebarMenuItem>
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
        {split ? <Columns2 aria-hidden /> : <SquareTerminal aria-hidden />}
        <span className="truncate">{label}</span>
        {!split && window.panes[0] && <PaneBadge pane={window.panes[0]} width="max-w-32" />}
      </SidebarMenuButton>

      {split && (
        <SidebarMenuSub>
          {window.panes.map((pane) => (
            <SidebarMenuSubItem key={pane.paneId}>
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
            </SidebarMenuSubItem>
          ))}
        </SidebarMenuSub>
      )}
    </SidebarMenuItem>
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
  hasTree,
}: {
  snapshot: SnapshotState
  onRefresh: () => void
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
        <p className="text-sidebar-foreground/70 mt-1">
          Nothing is running on this host yet. Start one over SSH —{' '}
          <code className="font-mono">tmux new -s work</code> — and it appears here within a
          couple of seconds.
        </p>
        <RetryButton onRefresh={onRefresh} />
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
function paneBadge(pane: Pick<PaneNode, 'command' | 'title' | 'label'>): PaneBadgeText {
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
  const { text, fromCommand } = paneBadge(pane)
  return (
    <Badge
      variant="secondary"
      title={fromCommand ? undefined : text}
      className={cn('ml-auto truncate', width, fromCommand ? 'font-mono' : 'shrink font-normal')}
    >
      {text}
    </Badge>
  )
}

export default AppSidebar
