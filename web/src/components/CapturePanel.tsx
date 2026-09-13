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
 * tabbable child on open. Here that is a button today and the pane selector
 * from Task 8 on -- either way, focus moving into the dialog's controls on a
 * phone is a keyboard. Focus goes to the dialog container instead, so Escape
 * still closes it and everything inside is still reachable by tab. This is the
 * first dialog in the app that needs it: do not copy the shape of
 * `DevicesDialog` or `KillDialog`, neither of which has this problem.
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
  /** The pane the capture is of, named in the header even before one arrives. */
  paneId: string | null
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
  capture,
  error,
  loading,
  copied,
  now,
  onRecapture,
  onCopy,
}: CapturePanelBodyProps) {
  const notice = capture ? truncationNotice(capture.truncated, capture.lines) : null
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
  /** The pane to capture. Task 8 puts a selector on this. */
  paneId: string | null
}

/**
 * The panel, full-screen, with one capture in it.
 *
 * Opening captures, and Recapture captures again. Nothing else does -- see
 * decision 4 in the module comment.
 */
export function CapturePanel({ open, onOpenChange, paneId }: CapturePanelProps) {
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
