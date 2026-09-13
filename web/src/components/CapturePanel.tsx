/**
 * A pane's scrollback, read-only, selectable, and copyable in one tap.
 *
 * The app's answer to "I want to read what the agent said ten screens ago, and
 * quote a line of it back". Today the only way to do that in the browser is to
 * scroll the pane into tmux copy mode, which is a mouse-driven selection on a
 * device with no mouse -- and worse, a pane left in copy mode eats the reply
 * box's text and delivers the remainder to the program as input. Removing the
 * reason to be in copy mode at all is most of what this panel is for.
 *
 * ## The selector *is* the tab's selection, and what that costs
 *
 * Changing pane in here goes through App's `handleSelectPane` -- the same call
 * a sidebar row click and a palette row make -- and never through a `useState`
 * of this panel's own. The alternative is deliberately unrepresentable: the
 * reply box (Phase D) writes to the tab's client's active pane, and on a phone
 * this panel covers it, so a panel-local selection would let you read pane A,
 * close the panel, type into the box and send to pane B, **with nothing on
 * screen having lied to you at any point**. There is one current pane in this
 * app and this is one more control over it, not a second one.
 *
 * **The cost, stated rather than discovered: this moves tmux's active pane, and
 * the active pane is a window property shared with every attached client.** So
 * browsing panes in here nudges the owner's local terminal in that window --
 * one of the three window-level properties v1 records as shared with anyone
 * else attached. That is accepted: it is not a new class of surprise, since a
 * sidebar row click already does exactly this, but it is the existing one
 * reached through a control that *invites browsing*, and this paragraph is
 * where that is written down rather than folded away.
 *
 * **A pane in another group is not one fork.** `handleSelectPane` returns early
 * for a pane outside the tab's session group, setting `pendingPane` and
 * re-attaching -- which tears the socket down, brings up a new throwaway tmux
 * session in the new group, replays the selection on it and redraws. This
 * selector reuses the palette's `paneEntries`, which spans **every** session in
 * the snapshot (the current one is a flag on a row, not a filter over them), so
 * the panel makes that path far easier to reach than the sidebar did. Hence the
 * rows carry the palette's `unreachable` flag unchanged: a group with no
 * session of its own left is offered and disabled rather than hidden, because
 * those panes are running agents and dropping a working socket to discover the
 * daemon will 404 is worse than a greyed row.
 *
 * **The capture follows the selection; it does not race it.** Nothing here
 * issues a capture *after* a select. The panel has no pane of its own to
 * capture: `paneId` is the tab's, the fetch effect is keyed on it, and
 * `TerminalSession.select()` writes the pane and emits a status synchronously
 * inside the click -- so `activePane`, and therefore this prop, has already
 * moved by the time the effect runs. The version with a race in it is the one
 * that commands its own capture alongside the select and passes it whichever
 * pane id it happened to be holding.
 *
 * **What one selector does not buy.** The two controls can never *name*
 * different panes; what neither controls is that pane moving underneath them.
 * The active pane is shared, `App` records that `#pane` is written only by
 * `select` and that nothing corrects it, and it deliberately declines to read
 * `paneActive` back from the snapshot. Re-converging on a wake is Phase A's
 * job. This panel's contribution is only that it does not add a *second* way to
 * diverge.
 *
 * **A native `<select>`, not a Radix one.** Two reasons, pointing the same way.
 * iOS and Android both answer a native select with a picker rather than a
 * keyboard, which is decision 1 below applied to a second control; and a Radix
 * `Select` portals its listbox into a `document`, so which rows exist and which
 * are disabled would be assertable only from Playwright -- and `disabled`
 * looked for in a shadcn class list is this project's recorded test that always
 * passes, because the Tailwind list contains `disabled:pointer-events-none`.
 *
 * ## Four things here are decisions, not styling
 *
 * **1. A `<pre>`, never a `<textarea>`.** The textarea is the version that
 * looks better -- it scrolls, it selects, it is one element. It is also a
 * focusable text field, so focusing it raises the soft keyboard, which is the
 * exact thing this panel exists to avoid on the device it exists for. The
 * `<pre>` carries `white-space: pre-wrap`, `overflow-wrap: anywhere` and
 * `user-select: text` as inline style rather than as Tailwind classes, because
 * these three are the behaviour and a test can read them back off the markup;
 * a class name only says a stylesheet somewhere might.
 *
 * **2. `onOpenAutoFocus` is prevented.** Radix's `Dialog` focuses its first
 * tabbable child on open. Since Task 8 that child is the **pane selector**,
 * which makes this prevention load-bearing rather than prophylactic: focus
 * moving into a control on open is a keyboard on a phone, and a focused select
 * is one an errant swipe can also change. Focus goes to the dialog container
 * instead, so Escape still closes it and everything inside is still reachable
 * by tab. This is the first dialog in the app that needs it: do not copy the
 * shape of `DevicesDialog` or `KillDialog`, neither of which has this problem.
 *
 * **3. Copy-all copies from state already in memory and never fetches first.**
 * Two reasons, and they point the same way. iOS Safari rejects a `writeText`
 * that is not synchronously inside the user gesture, so an `await` before it
 * loses the clipboard; and what the user copies must be what they were looking
 * at, which a fresh capture is not. `copyCapture` therefore takes the text as
 * an argument and has nowhere to fetch from. *Whether copying from memory
 * inside the handler is enough for iOS is design open question 7 and needs a
 * device* -- this is written so that it can be, and that is not the same as
 * knowing that it is.
 *
 * **4. No auto-refresh.** In the order the reasons matter: a re-render
 * destroys a selection in progress and this panel exists to be selected from;
 * an interval is a second poll at a cadence nobody chose, against a fork that
 * costs 2-8 ms rather than the snapshot's shared one; and a panel that keeps up
 * with the pane is a second terminal, which is what closing the panel already
 * gets you. Note that on a phone the panel is full-screen and the live terminal
 * is *behind* it, not beside it -- so "there is one of those on the other side
 * of the screen" is false there and is not the reason. The only refresh is the
 * Recapture button. **A `setInterval` added here is a regression no test
 * catches; this comment is the check.**
 *
 * The one clock in this file is the header's, which ages the capture out loud
 * (`captured 14s ago`) once a second. That is a snapshot admitting it is one.
 * It re-renders the body, but the `<pre>`'s text node is unchanged, so React
 * leaves that DOM node alone and a selection inside it survives -- which is
 * exactly what a refresh of the capture itself would not do.
 *
 * ## The depth, and the control this panel does not have
 *
 * The daemon picks the depth (`defaultCaptureLines`) and this panel asks for no
 * particular one; see `lib/capture.ts` for why a second default would be one
 * too many. Design open question 8 asks whether the visible screen should be
 * offered as a distinct choice: measured, `-p` alone is **2.6 ms and 4 KB**
 * against **5.1 ms and 168 KB** for the depth default, which on a phone on a
 * train is a real difference. The numbers are here so the control can be argued
 * for against them rather than added because it seems nice.
 */

import { Copy, Loader2, RefreshCw } from 'lucide-react'
import { useCallback, useEffect, useState } from 'react'

import { paneEntries } from '@/components/Palette'
import type { PaletteEntry } from '@/components/Palette'
import { Button } from '@/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { fetchCapture } from '@/lib/capture'
import type { Capture } from '@/lib/capture'
import type { SessionNode } from '@/lib/useSnapshot'

/**
 * How old the capture on screen is, in the coarsest unit that still says
 * something: `just now`, `14s ago`, `3m ago`, `2h ago`.
 *
 * `capturedAt` is the daemon's clock and `now` is the browser's. They are not
 * the same clock and nothing synchronises them, so a skew in either direction
 * is ordinary -- and a negative age would tell the user the capture is from the
 * future. Anything under a second, in either direction, is "just now".
 */
export function captureAge(capturedAt: number, now: number): string {
  const ms = now - capturedAt
  if (ms < 1000) return 'just now'
  const seconds = Math.floor(ms / 1000)
  if (seconds < 60) return `${seconds}s ago`
  const minutes = Math.floor(seconds / 60)
  if (minutes < 60) return `${minutes}m ago`
  return `${Math.floor(minutes / 60)}h ago`
}

/**
 * What the panel says when the daemon dropped the oldest part of the capture
 * to fit its byte cap, or null when it did not.
 *
 * The capture then begins mid-screen, and a panel that stayed quiet about it
 * would be showing a conversation that silently starts in the middle -- which
 * is indistinguishable, on screen, from an agent that started talking there.
 */
export function truncationNotice(truncated: boolean, lines: number): string | null {
  if (!truncated) return null
  return `Too much to send: the oldest of these ${lines} lines were dropped, so this starts mid-screen.`
}

/**
 * Radix's open-focus, refused.
 *
 * Exported and one line long because that is the only shape this suite can
 * see: the tests render with `react-dom/server` and never inside
 * `DialogContent`, so the prop itself appears in no markup. The behaviour --
 * `document.activeElement` being the dialog and not its first tabbable child --
 * is asserted under Playwright in Task 9.
 */
export function preventAutoFocus(e: Event): void {
  e.preventDefault()
}

/**
 * How `handleSelectPane` is called from here.
 *
 * Structural rather than imported from `App`, which imports this module: the
 * shape is three arguments and the third is the one this panel exists to pass.
 */
export type SelectPane = (paneId: string, groupKey: string, opts?: { focus?: boolean }) => void

/**
 * One row of the selector, in words.
 *
 * The palette's own fields, in the palette's own order, so a pane reads the
 * same here as it does there -- `work › 1: api · pane 0 · claude`. The command is
 * last because it is what a person actually looks for; `unreachable` is said
 * out loud because a disabled row with no reason on it reads as a bug.
 */
export function paneOptionLabel(entry: PaletteEntry): string {
  const parts = [entry.label]
  if (entry.detail) parts.push(entry.detail)
  parts.push(entry.command)
  if (entry.unreachable) parts.push('unreachable')
  return parts.join(' · ')
}

/**
 * A row was chosen: move the tab, or do nothing at all.
 *
 * Three refusals, and each of them is a thing that would otherwise be sent to
 * tmux. The pane already on screen is the first: re-selecting it writes the
 * shared active pane again for no gain, and a selector that fired on every
 * render rather than on every change would show up here and nowhere else. An
 * `unreachable` row is the second -- the browser will not let a disabled
 * `<option>` be chosen, and this is the half that does not depend on the
 * browser. An id no row offers is the third: the placeholder, and a pane that
 * died between the render and the tap.
 *
 * Nothing is captured here. The capture is keyed on the pane the tab is on, so
 * it follows this call rather than racing it; see the module comment.
 */
export function choosePane(
  entries: readonly PaletteEntry[],
  chosen: string,
  current: string | null,
  selectPane: SelectPane,
): void {
  if (chosen === current) return
  const entry = entries.find((e) => e.id === chosen)
  if (!entry || entry.unreachable || entry.action.kind !== 'pane') return
  // `{ focus: false }`, and this is the whole reason `handleSelectPane` has an
  // options argument: from inside a Radix modal, focusing the terminal raises
  // a phone's soft keyboard inside the tap gesture. See `focusAfterSelect`.
  selectPane(entry.action.paneId, entry.action.groupKey, { focus: false })
}

/** The one thing this panel needs from `navigator.clipboard`. */
export interface ClipboardWriter {
  writeText(text: string): Promise<void>
}

/**
 * Copy the capture. Takes the text, and so has nowhere to fetch from.
 *
 * Deliberately not `async`: the `writeText` call has to happen in the same turn
 * as the click that caused it, and an `async` function with anything awaited
 * ahead of it would not. See decision 3 in the module comment.
 */
export function copyCapture(
  text: string,
  clipboard: ClipboardWriter | undefined = globalThis.navigator?.clipboard,
): Promise<void> {
  if (!clipboard) {
    return Promise.reject(new Error('this browser will not give the page a clipboard'))
  }
  return clipboard.writeText(text)
}

export interface CapturePanelBodyProps {
  /**
   * The pane the capture is of, named in the header even before one arrives --
   * and the selector's value. This is the tab's selection, handed down; the
   * panel keeps no pane of its own for it to disagree with.
   */
  paneId: string | null
  /** Every session the snapshot can see, for the selector. */
  groups: readonly SessionNode[]
  /** The group this tab is attached to, which is what makes a row reachable. */
  activeSession: string | null
  /** `handleSelectPane`. Called with `{ focus: false }`; see `choosePane`. */
  onSelectPane: SelectPane
  capture: Capture | null
  /** The daemon's own sentence about why there is no capture. */
  error: string | null
  loading: boolean
  /** The clipboard write succeeded, so the button says so for a moment. */
  copied: boolean
  /** The browser's clock, ticked once a second while a capture is on screen. */
  now: number
  onRecapture: () => void
  onCopy: () => void
}

/**
 * The panel's contents, with no portal around them.
 *
 * Split out for the reason `KillDialogBody` is: the suite renders with
 * `react-dom/server` in a node environment, where an open Radix dialog is a
 * portal into a `document` that does not exist.
 */
export function CapturePanelBody({
  paneId,
  groups,
  activeSession,
  onSelectPane,
  capture,
  error,
  loading,
  copied,
  now,
  onRecapture,
  onCopy,
}: CapturePanelBodyProps) {
  const notice = capture ? truncationNotice(capture.truncated, capture.lines) : null
  const entries = paneEntries(groups, paneId, activeSession)
  // The pane the tab is on with no row to stand for it: it died between polls,
  // or the snapshot has not loaded. Without this the control would show blank
  // while the header names a pane, which is the one thing a single selection is
  // supposed to make impossible.
  const orphaned = paneId !== null && !entries.some((e) => e.id === paneId)
  return (
    <>
      <DialogHeader className="pr-8">
        <DialogTitle>Scrollback{paneId ? ` · ${paneId}` : ''}</DialogTitle>
        <DialogDescription>
          {capture
            ? `captured ${captureAge(capture.capturedAt, now)}`
            : loading
              ? 'capturing…'
              : 'nothing captured'}
        </DialogDescription>
      </DialogHeader>

      {/*
        The selector, and it *is* the tab's selection -- see the module comment
        for what that costs and why it is worth it. A native `<select>`: on a
        phone it is a picker rather than a keyboard, and here it is markup a
        test can read rather than a portal it cannot.
      */}
      <select
        aria-label="Pane"
        className="bg-background text-foreground w-full rounded-md border px-2 py-1 text-sm"
        value={paneId ?? ''}
        onChange={(e) => choosePane(entries, e.target.value, paneId, onSelectPane)}
      >
        {paneId === null && <option value="">No pane</option>}
        {orphaned && <option value={paneId}>{paneId} · gone</option>}
        {entries.map((entry) => (
          <option key={entry.id} value={entry.id} disabled={entry.unreachable}>
            {paneOptionLabel(entry)}
          </option>
        ))}
      </select>

      <div className="flex flex-wrap items-center gap-2">
        <Button type="button" variant="outline" size="sm" onClick={onRecapture} disabled={loading}>
          {loading ? (
            <Loader2 className="size-3.5 animate-spin" aria-hidden />
          ) : (
            <RefreshCw className="size-3.5" aria-hidden />
          )}
          Recapture
        </Button>
        <Button
          type="button"
          variant="outline"
          size="sm"
          onClick={onCopy}
          disabled={capture === null}
        >
          <Copy className="size-3.5" aria-hidden />
          {copied ? 'Copied' : 'Copy all'}
        </Button>
      </div>

      {error !== null && (
        <p className="text-destructive text-sm" role="alert">
          {error}
        </p>
      )}
      {notice !== null && <p className="text-muted-foreground text-xs">{notice}</p>}

      {/*
        The read-and-copy surface. `<pre>` and not `<textarea>`; the three
        properties that make it wrap and select are inline style rather than
        classes so that they are the thing a test reads back. The palette is the
        terminal's own, so the panel looks like the pane it came from.
      */}
      <pre
        className="bg-background text-foreground min-h-0 flex-1 overflow-auto rounded-md border p-2 font-mono text-xs leading-snug"
        style={{
          whiteSpace: 'pre-wrap',
          overflowWrap: 'anywhere',
          // Both spellings: iOS Safari still wants the prefixed one, and it is
          // the device this whole panel is for.
          WebkitUserSelect: 'text',
          userSelect: 'text',
        }}
      >
        {capture?.text ?? ''}
      </pre>
    </>
  )
}

export interface CapturePanelProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  /**
   * The pane to capture: the tab's selection, and the selector's value. The
   * panel holds no second copy of it -- moving the selector moves this, by
   * going through `onSelectPane`.
   */
  paneId: string | null
  /** Every session the snapshot can see, for the selector. */
  groups: readonly SessionNode[]
  /** The group this tab is attached to. */
  activeSession: string | null
  /** `handleSelectPane`, unchanged, and called with `{ focus: false }`. */
  onSelectPane: SelectPane
}

/**
 * The panel, full-screen, with one capture in it.
 *
 * Opening captures, and Recapture captures again. Nothing else does -- see
 * decision 4 in the module comment.
 */
export function CapturePanel({
  open,
  onOpenChange,
  paneId,
  groups,
  activeSession,
  onSelectPane,
}: CapturePanelProps) {
  const [capture, setCapture] = useState<Capture | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(false)
  const [copied, setCopied] = useState(false)
  const [now, setNow] = useState(() => Date.now())
  /** Bumped by Recapture. The one thing besides opening that re-runs the fetch. */
  const [asked, setAsked] = useState(0)

  // Closed: drop the capture. It is a snapshot of a moment, and reopening the
  // panel onto a screenful of text from an hour ago -- correctly aged, and
  // still wrong to be looking at -- is worse than a blank one for 300ms.
  useEffect(() => {
    if (open) return
    setCapture(null)
    setError(null)
    setCopied(false)
  }, [open])

  // Opening, Recapture, and a change of pane. The third is the selector's:
  // moving it moves the tab, which moves `paneId`, which lands here -- so the
  // capture follows the selection instead of racing it, and there is no path
  // that fetches one pane's scrollback under another pane's name.
  useEffect(() => {
    if (!open || paneId === null) return
    const controller = new AbortController()
    setLoading(true)
    setError(null)
    setCopied(false)
    void fetchCapture(paneId, { signal: controller.signal }).then((result) => {
      if (controller.signal.aborted) return
      setLoading(false)
      // The clock is set from the answer rather than left where it was, so the
      // age starts at "just now" instead of at however long the panel has been
      // open. `fetchCapture` never throws; see lib/capture.ts.
      setNow(Date.now())
      if (result.ok) {
        setCapture(result.capture)
      } else {
        setCapture(null)
        setError(result.message)
      }
    })
    return () => controller.abort()
  }, [open, paneId, asked])

  // Ages the header, and nothing else. This does not refetch: see decision 4.
  useEffect(() => {
    if (!open || capture === null) return
    const timer = setInterval(() => setNow(Date.now()), 1000)
    return () => clearInterval(timer)
  }, [open, capture])

  const onCopy = useCallback(() => {
    if (capture === null) return
    // From memory, synchronously, inside the gesture. Nothing is fetched.
    copyCapture(capture.text).then(
      () => setCopied(true),
      (err: unknown) => {
        setCopied(false)
        setError(err instanceof Error ? err.message : String(err))
      },
    )
  }, [capture])

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent
        // The app's first full-screen dialog: the default is centred and
        // `sm:max-w-sm`, which for a screenful of scrollback on a phone is a
        // letterbox. `max-w-none` and `sm:max-w-none` both, because the default
        // sets the second one and only the same breakpoint can override it;
        // the two `translate-*-0` cancel the centring transform. `h-svh` on top
        // of `inset-0` is deliberate and not redundant: an over-constrained box
        // drops `bottom`, so the panel is the *small* viewport tall and never
        // runs under a phone's toolbars.
        className="inset-0 flex h-svh w-full max-w-none translate-x-0 translate-y-0 flex-col gap-3 rounded-none sm:max-w-none"
        // Decision 2. Without this Radix focuses the first tabbable child,
        // which on a phone is the soft keyboard this panel exists to avoid.
        onOpenAutoFocus={preventAutoFocus}
      >
        <CapturePanelBody
          paneId={paneId}
          groups={groups}
          activeSession={activeSession}
          onSelectPane={onSelectPane}
          capture={capture}
          error={error}
          loading={loading}
          copied={copied}
          now={now}
          onRecapture={() => setAsked((n) => n + 1)}
          onCopy={onCopy}
        />
      </DialogContent>
    </Dialog>
  )
}

export default CapturePanel
