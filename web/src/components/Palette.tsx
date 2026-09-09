/**
 * Jump anywhere: a fuzzy list of every pane the daemon can see, plus the one
 * terminal action that has nowhere else to live.
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
 * Panes, and "enter copy mode". Not devices, not the theme: those live one
 * click away in the footer menu, they are used about once a month, and a
 * palette that matches them competes with the panes for the first row -- typing
 * "de" to reach a pane running `deploy` should not surface "Devices…". The
 * palette's job is moving between agents, and everything in it moves the tab or
 * changes what the pane is doing.
 */

import { Columns2, Copy, SquareTerminal } from 'lucide-react'
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
  | { kind: 'copy-mode' }

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
        const label = `${session.key} › ${window.index}: ${window.name}`
        const detail = split ? `pane ${pane.paneIndex}` : null
        entries.push({
          id: pane.paneId,
          label,
          detail,
          command: pane.command,
          search: [
            session.key,
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

export interface PaletteProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  groups: readonly SessionNode[]
  activePane: string | null
  activeSession: string | null
  onSelectPane: (paneId: string, groupKey: string) => void
  /**
   * Enter tmux copy mode. Returns false when the socket is not ready, which
   * this component reports rather than swallowing: a palette that closes on a
   * command that did nothing is indistinguishable from one that worked.
   */
  onCopyMode: () => boolean
}

export function Palette({
  open,
  onOpenChange,
  groups,
  activePane,
  activeSession,
  onSelectPane,
  onCopyMode,
}: PaletteProps) {
  const [failure, setFailure] = useState<string | null>(null)

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

  function run(action: PaletteAction) {
    if (action.kind === 'copy-mode') {
      if (!onCopyMode()) {
        // Stay open and say so. The terminal refuses copy mode when the socket
        // is not ready, and that is exactly the moment a user would otherwise
        // assume the key went through.
        setFailure('The terminal is not connected, so copy mode did nothing.')
        return
      }
    } else {
      onSelectPane(action.paneId, action.groupKey)
    }
    change(false)
  }

  return (
    <CommandDialog
      open={open}
      onOpenChange={change}
      title="Command palette"
      description="Jump to a pane, or run a terminal command"
    >
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
                onSelect={() => run(entry.action)}
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
          <CommandGroup heading="Terminal">
            <CommandItem
              value="copy mode scrollback search"
              onSelect={() => run({ kind: 'copy-mode' })}
            >
              <Copy aria-hidden />
              Enter copy mode
              <CommandShortcut>scrollback</CommandShortcut>
            </CommandItem>
          </CommandGroup>
        </CommandList>
      </Command>
    </CommandDialog>
  )
}

export default Palette
