/**
 * Jump anywhere: a fuzzy list of every pane the daemon can see, plus the
 * terminal actions that have nowhere else to live.
 *
 * ## The chord, and why it is not the way in
 *
 * A focused terminal swallows nearly every key, and the obvious candidates are
 * taken: `Ctrl+K` is readline's kill-line, `Ctrl+B` is the tmux prefix, `Alt+*`
 * is Meta. That leaves `Ctrl+Alt+K`, which is what the design picked -- and
 * `Ctrl+Alt` *is* AltGr on several European layouts, so on a Spanish or German
 * keyboard this chord is a character someone might be trying to type. It is
 * therefore a convenience and never the only door: the header button is the
 * primary affordance and this file is mounted whether or not the chord works.
 *
 * The listener is installed in the **capture** phase on `window`, which is what
 * puts it ahead of wterm's own keydown handler on the terminal element, and it
 * calls `stopPropagation` there so the keystroke never reaches the pane at all.
 * Without both halves the chord would open the palette *and* send something to
 * whatever agent is running.
 *
 * ## What is in it
 *
 * Panes, and the two things the header row offers: the copy-mode control --
 * one row, pointing into copy mode or back out of it depending on the pane --
 * and the reply box's toggle. Not devices, not the theme: those live one click
 * away in the footer menu, they are used about once a month, and a palette that
 * matches them competes with the panes for the first row -- typing "de" to
 * reach a pane running `deploy` should not surface "Devices…". The palette's
 * job is moving between agents, and everything in it moves the tab, changes
 * what the pane is doing, or changes how you talk to it.
 */

import { Columns2, Copy, CopySlash, MessageSquare, SquareTerminal, Wrench } from 'lucide-react'
import { useCallback, useEffect, useRef, useState } from 'react'

import { Badge } from '@/components/ui/badge'
import {
  Command,
  CommandDialog,
  CommandEmpty,
  CommandGroup,
  CommandInput,
  CommandItem,
  CommandList,
  CommandSeparator,
  CommandShortcut,
} from '@/components/ui/command'
import { replyToggleLabel } from '@/components/ReplyBox'
import type { CopyControl } from '@/lib/copyMode'
import { newSessionPrompt, rowMenu } from '@/lib/manage'
import type { MenuEntry, MenuIntent, RowTarget } from '@/lib/manage'
import { findPane } from '@/lib/useSnapshot'
import type { SessionNode } from '@/lib/useSnapshot'

/** What the chord is, in one place, for the UI hint and the matcher. */
export const PALETTE_CHORD_LABEL = 'Ctrl+Alt+K'

/**
 * Whether a keydown is the palette chord.
 *
 * `code` is checked first so the chord stays on the physical K key whatever the
 * layout produces -- on a layout where Ctrl+Alt is AltGr, `key` is some other
 * character entirely. `key` is still accepted for the layouts where the code is
 * not KeyK but the letter is. `metaKey` is excluded so that a window-manager or
 * macOS chord that happens to include Ctrl+Alt is not swallowed.
 */
export function isPaletteChord(event: KeyboardEvent): boolean {
  if (!event.ctrlKey || !event.altKey || event.metaKey) return false
  return event.code === 'KeyK' || event.key === 'k' || event.key === 'K'
}

/**
 * Listen for the chord ahead of the terminal.
 *
 * Capture phase on `window`: wterm's handler is on the terminal element, and a
 * bubble-phase listener here would run after it -- too late to stop the key
 * reaching an agent. Returns the unsubscribe.
 */
export function installPaletteChord(target: EventTarget, toggle: () => void): () => void {
  const handler = (event: Event) => {
    const key = event as KeyboardEvent
    if (!isPaletteChord(key)) return
    key.preventDefault()
    // Capture-phase stopPropagation: the event never reaches the terminal, so
    // the chord cannot also type into whatever agent has focus.
    key.stopPropagation()
    toggle()
  }
  target.addEventListener('keydown', handler, { capture: true })
  return () => target.removeEventListener('keydown', handler, { capture: true })
}

/** What selecting a row does. */
export type PaletteAction =
  | { kind: 'pane'; paneId: string; groupKey: string }
  // One kind for the one copy-mode control, whichever way it is pointing. Two
  // kinds were two rows, and two rows for one idea is the defect this replaced:
  // the owner could not tell what "End mode" meant next to "Copy mode".
  | { kind: 'toggle-copy' }
  | { kind: 'toggle-reply' }

/** One row. */
export interface PaletteEntry {
  /** Unique, and the cmdk value: the pane id keeps duplicates apart. */
  id: string
  /** `work › 1: api`, for the row. */
  label: string
  /** `pane 2`, when the window is split. */
  detail: string | null
  /** `pane_current_command`. */
  command: string
  /** Everything cmdk fuzzy-matches, in the `session/window/pane` order the design asks for. */
  search: string
  action: PaletteAction
  /** The pane this tab is already pinned to. */
  current: boolean
  /**
   * The group has no session of its own left, so this tab cannot attach to it.
   * Shown and disabled rather than hidden: those panes are running agents, and
   * silently omitting them reads as "the palette lost my session".
   */
  unreachable: boolean
}

/**
 * Every pane, in snapshot order.
 *
 * Snapshot order is the order on screen -- the daemon sorts by (group, window
 * index, pane index) and the sidebar preserves it -- so the palette's unfiltered
 * list reads the same top to bottom as the tree beside it.
 */
export function paneEntries(
  groups: readonly SessionNode[],
  activePane: string | null,
  activeSession: string | null,
): PaletteEntry[] {
  const entries: PaletteEntry[] = []
  for (const session of groups) {
    // Same rule as the sidebar: an orphaned group is only reachable from a tab
    // already inside it, because attaching to it by name is what the daemon
    // refuses.
    const unreachable = session.appOnly && session.key !== activeSession
    for (const window of session.windows) {
      const split = window.panes.length > 1
      for (const pane of window.panes) {
        // The live session name, never `session.key`. tmux freezes
        // session_group at the name the group was created under, so a row
        // labelled from the key shows the pre-rename name forever -- and the
        // sidebar beside it shows the new one, which makes a rename look like
        // it half worked. The key stays the identity: it is what `action`
        // carries and what re-attaching uses.
        const label = `${session.name} › ${window.index}: ${window.name}`
        const detail = split ? `pane ${pane.paneIndex}` : null
        entries.push({
          id: pane.paneId,
          label,
          detail,
          command: pane.command,
          search: [
            // Both names, so a group renamed from `work3` to `api` is still
            // reachable by typing either -- the one on screen and the one in
            // the `?session=` URL a tab may have been opened with.
            session.name,
            session.key === session.name ? '' : session.key,
            `${window.index}: ${window.name}`,
            detail ?? '',
            pane.command,
            pane.paneId,
          ]
            .filter(Boolean)
            .join('/'),
          action: { kind: 'pane', paneId: pane.paneId, groupKey: session.key },
          current: pane.paneId === activePane,
          unreachable,
        })
      }
    }
  }
  return entries
}

/** A management entry in the palette, with what it acts on spelled out. */
export interface PaletteActionEntry extends MenuEntry {
  /** `pane 2 · %3`, or `session "work"`: the palette has no row to point at. */
  detail: string
}

/**
 * The management actions, scoped to the pane this tab is looking at.
 *
 * The sidebar scopes its menu to the row you right-clicked; the palette has no
 * row, so it scopes to the current pane and the session that pane is in. Same
 * `rowMenu` behind both, so an action can never exist on one surface and not
 * the other, and the labels are the same words.
 *
 * "New session" is the exception and is always present: it is the one action
 * that needs no pane, and the case it exists for -- an empty tmux server -- is
 * exactly the case where there is no current pane to scope to.
 */
export function actionEntries(
  groups: readonly SessionNode[],
  activePane: string | null,
  activeSession: string | null,
): PaletteActionEntry[] {
  const entries: PaletteActionEntry[] = []
  const located = findPane(groups, activePane)
  if (located) {
    const { session, window, pane } = located
    const target: RowTarget = { kind: 'pane', session, window, pane }
    const where = `pane ${pane.paneIndex} · ${pane.paneId}`
    for (const entry of rowMenu(target, activeSession)) {
      entries.push({
        ...entry,
        id: `pane:${entry.id}`,
        detail: where,
        search: `${entry.search} ${where}`,
      })
    }
    for (const entry of rowMenu({ kind: 'session', session }, activeSession)) {
      // The session row offers a new window too, and it is the same action on
      // the same session -- one row for it, on the pane, where it opens in the
      // directory you are working in.
      if (entry.id === 'new-window') continue
      entries.push({
        ...entry,
        id: `session:${entry.id}`,
        detail: `session "${session.name}"`,
        search: `${entry.search} session ${session.name}`,
      })
    }
  }
  entries.push({
    id: 'new-session',
    label: 'New session…',
    detail: 'tmux server',
    search: 'new session create tmux',
    intent: { kind: 'prompt', prompt: newSessionPrompt() },
  })
  return entries
}

export interface PaletteProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  groups: readonly SessionNode[]
  activePane: string | null
  activeSession: string | null
  onSelectPane: (paneId: string, groupKey: string) => void
  /**
   * The copy-mode control as it stands right now: which way it points, and what
   * the row says. Computed in App from the attached pane's `#{pane_mode}`, so
   * this palette and the header button can never disagree about it.
   */
  copy: CopyControl
  /**
   * Take the control's action -- into copy mode, or back out of it. Returns
   * false when the socket is not ready, which this component reports rather
   * than swallowing: a palette that closes on a command that did nothing is
   * indistinguishable from one that worked.
   */
  onToggleCopy: () => boolean
  /** Whether the reply box is on screen, which is what its row's label says. */
  replyOpen: boolean
  /** Show the reply box, or put it away. */
  onToggleReply: () => void
  /**
   * Put the caret in the reply box. Called once, as this dialog closes over a
   * box it has just opened; see `keepFocus`.
   */
  onFocusReply: () => void
  /**
   * A management entry was chosen. The palette closes and App takes it from
   * there -- a prompt or the kill dialog, or straight to the daemon.
   */
  onIntent: (intent: MenuIntent) => void
}

export function Palette({
  open,
  onOpenChange,
  groups,
  activePane,
  activeSession,
  onSelectPane,
  copy,
  onToggleCopy,
  replyOpen,
  onToggleReply,
  onFocusReply,
  onIntent,
}: PaletteProps) {
  const [failure, setFailure] = useState<string | null>(null)

  /** The row that just ran placed the caret itself; let Radix's restore pass. */
  const keepFocus = useRef(false)

  // The chord toggles, so the same keystroke closes what it opened. Held in a
  // ref so the listener is installed once rather than re-installed on every
  // render of a component whose props change 1.5s at a time.
  const latest = useRef({ open, onOpenChange })
  useEffect(() => {
    latest.current = { open, onOpenChange }
  })
  useEffect(() => {
    return installPaletteChord(window, () => {
      const { open: isOpen, onOpenChange: change } = latest.current
      change(!isOpen)
    })
  }, [])

  const change = useCallback(
    (next: boolean) => {
      if (!next) setFailure(null)
      onOpenChange(next)
    },
    [onOpenChange],
  )

  const entries = paneEntries(groups, activePane, activeSession)
  const actions = actionEntries(groups, activePane, activeSession)

  function run(action: PaletteAction) {
    switch (action.kind) {
      case 'pane':
        onSelectPane(action.paneId, action.groupKey)
        break
      case 'toggle-copy':
        if (!onToggleCopy()) {
          // Stay open and say so. The terminal refuses both directions when the
          // socket is not ready, and that is exactly the moment a user would
          // otherwise assume the key went through.
          setFailure(
            copy.inCopy
              ? 'The terminal is not connected, so leaving copy mode did nothing.'
              : 'The terminal is not connected, so copy mode did nothing.',
          )
          return
        }
        break
      case 'toggle-reply':
        // Nothing to refuse: the box is this app's own, and showing it needs no
        // socket. It is the one row here that changes the layout rather than
        // the pane.
        //
        // The flag is about what happens a moment later, and it was measured
        // rather than guessed. This dialog keeps a focus scope up through its
        // exit animation and then restores focus to whatever had it before --
        // which in this app is `<body>`, because wterm replaces the terminal's
        // own element as it redraws and the saved node is gone by then. So the
        // one row whose entire purpose is handing the user something to type
        // in would open a box and leave the caret nowhere. Only this row opts
        // out; every other row's focus behaviour is left exactly as it was.
        keepFocus.current = true
        onToggleReply()
        break
    }
    change(false)
  }

  return (
    <CommandDialog
      open={open}
      onOpenChange={change}
      title="Command palette"
      description="Jump to a pane, or run a terminal command"
      onCloseAutoFocus={(event) => {
        if (!keepFocus.current) return
        keepFocus.current = false
        // Both halves are needed and neither is enough. Preventing the restore
        // stops Radix putting the caret back on `<body>`; focusing here rather
        // than letting the box's own `autoFocus` do it is because that fires
        // while this dialog's focus scope is still up -- the trap pulls the
        // caret straight back out, and the box ends up on screen with nothing
        // typing into it. This event is the moment the scope lets go.
        event.preventDefault()
        onFocusReply()
      }}
    >
      <PaletteBody
        entries={entries}
        actions={actions}
        failure={failure}
        replyOpen={replyOpen}
        copy={copy}
        onRun={run}
        onIntent={(intent) => {
          onIntent(intent)
          // Closed either way: a prompt and the kill dialog both open over this
          // one, and a `run` intent has already been sent. Neither has anything
          // more to say here.
          change(false)
        }}
      />
    </CommandDialog>
  )
}

export interface PaletteBodyProps {
  entries: PaletteEntry[]
  actions: PaletteActionEntry[]
  failure: string | null
  /** What the reply row offers to do: show the box, or put it away. */
  replyOpen: boolean
  /** The copy-mode control: one row, pointing whichever way the pane needs. */
  copy: CopyControl
  onRun: (action: PaletteAction) => void
  onIntent: (intent: MenuIntent) => void
}

/**
 * The palette's three groups, outside the dialog that portals them.
 *
 * Split out for the same reason the dialogs' bodies are: an open
 * `CommandDialog` is a Radix portal into a `document` that does not exist under
 * vitest's node environment, so the rows -- which ones there are, and what they
 * say -- would otherwise be reachable only from Playwright.
 */
export function PaletteBody({
  entries,
  actions,
  failure,
  replyOpen,
  copy,
  onRun,
  onIntent,
}: PaletteBodyProps) {
  return (
    <Command>
      <CommandInput placeholder="Jump to session/window/pane…" />
      <CommandList>
        <CommandEmpty>Nothing matches.</CommandEmpty>
        {failure && (
          <p role="alert" className="text-destructive px-3 py-2 text-xs">
            {failure}
          </p>
        )}
        <CommandGroup heading="Panes">
          {entries.map((entry) => (
            <CommandItem
              key={entry.id}
              value={entry.search}
              disabled={entry.unreachable}
              onSelect={() => onRun(entry.action)}
            >
              {entry.detail ? <Columns2 aria-hidden /> : <SquareTerminal aria-hidden />}
              <span className="truncate">{entry.label}</span>
              {entry.detail && (
                <span className="text-muted-foreground shrink-0 text-xs">{entry.detail}</span>
              )}
              {entry.current && (
                <span className="text-muted-foreground shrink-0 text-xs">· current</span>
              )}
              <Badge variant="secondary" className="ml-auto max-w-24 truncate font-mono">
                {entry.command}
              </Badge>
            </CommandItem>
          ))}
        </CommandGroup>
        <CommandSeparator />
        {/*
          The same actions as the sidebar's context menus, from the same
          `rowMenu`, scoped to the pane this tab is looking at. Not a second
          surface with its own idea of what can be done.
        */}
        <CommandGroup heading="Manage">
          {actions.map((entry) => (
            <CommandItem
              key={entry.id}
              value={entry.search}
              onSelect={() => onIntent(entry.intent)}
              className={entry.danger ? 'text-destructive' : undefined}
            >
              <Wrench aria-hidden />
              <span className="truncate">{entry.label}</span>
              {entry.hint && (
                // The line saying zoom moves every client, including the
                // terminal on the host. Same words as the context menu.
                <span className="text-muted-foreground hidden shrink-0 text-xs sm:inline">
                  {entry.hint}
                </span>
              )}
              <span className="text-muted-foreground ml-auto shrink-0 text-xs">{entry.detail}</span>
            </CommandItem>
          ))}
        </CommandGroup>
        <CommandSeparator />
        {/*
          One row per header button, which is the rule this group did not keep
          until Task 23: `end-mode` was left out as out of Task 17's scope, and
          the header is where a phone has the least room. The reply toggle is
          here for the same reason -- it is the affordance the box now depends
          on, and a chord-driven palette is the fastest way to it.

          The copy-mode row is ONE row and follows the pane, for the reason the
          header button does: this list used to carry "Enter copy mode" and
          "Leave copy mode" side by side, and a user who cannot tell which of
          them applies is a user the palette has made guess. The row that does
          not apply is not disabled, it is absent -- a greyed row in a fuzzy
          list is still something to read past.
        */}
        <CommandGroup heading="Terminal">
          <CommandItem value={copy.search} onSelect={() => onRun({ kind: 'toggle-copy' })}>
            {copy.inCopy ? <CopySlash aria-hidden /> : <Copy aria-hidden />}
            {copy.label}
            <CommandShortcut>{copy.hint}</CommandShortcut>
          </CommandItem>
          <CommandItem
            value="reply box write type answer agent keyboard"
            onSelect={() => onRun({ kind: 'toggle-reply' })}
          >
            <MessageSquare aria-hidden />
            {replyToggleLabel(replyOpen)}
            <CommandShortcut>{replyOpen ? 'gives the rows back' : 'costs two rows'}</CommandShortcut>
          </CommandItem>
        </CommandGroup>
      </CommandList>
    </Command>
  )
}

export default Palette
