/**
 * One line of text at the foot of the terminal, and a way to put it on the
 * pane's PTY: answering a blocked agent without attaching a terminal.
 *
 * ## The two rules that are not obvious
 *
 * **Cancel the pane's mode first.** Task 16 measured what happens otherwise,
 * through a real PTY: with the pane in copy mode, writing `echo quit PARTIAL\r`
 * loses its `q` to copy mode's cancel key and the shell runs `uit PARTIAL`. A
 * reply is prose, prose contains a cancel key early, and the difference between
 * cancelling first and not is the difference between a reply that arrives whole
 * and one that arrives as a fragment of a command. So every non-empty send is
 * `end-mode` then bytes, in that order, and the socket is what orders them.
 *
 * **The line ends with CR (`0x0d`), never LF.** `0x0a` is `C-j`, a different
 * key, and an agent's prompt does not answer to it.
 *
 * **A multi-line paste is bracketed.** See `willBracket`: pasted text with a
 * newline in it goes out inside `ESC[200~` … `ESC[201~`, and with no Return
 * after it. Measured on tmux 3.7b, through the same PTY a browser tab's bytes
 * take, against Claude Code 2.1.267, opencode 1.18.30 and pi -- all three set
 * `DECSET 2004` at their prompt (`#{bracket_paste_flag}` is 1) and all three
 * honour the wrappers: three lines land as one draft, unsubmitted, and
 * opencode collapses them into its own `[Pasted ~3 lines]` chip, which is a
 * thing it can only do because the wrappers told it a paste had happened.
 *
 * ## Why the box is always here
 *
 * It does not appear on `blocked` and it does not take focus on mount. Three
 * reasons, in the order they matter:
 *
 * 1. A control that vanishes under you while you are typing into it teaches you
 *    not to trust it. The sidebar already has a rule against row elements that
 *    move on agent state, and this is the worse case of it: a badge you learn
 *    to ignore costs attention, a box that vanishes costs the sentence.
 * 2. `blocked` is late -- about 1.5 s on the screen path and about 6 s on the
 *    report path. A box gated on it arrives after you wanted it.
 * 3. The box is also how you answer what is never detected. Four of Claude's
 *    notifications produce no badge on either authority, and those are exactly
 *    the moments you are looking at the pane, understand what it wants, and
 *    need to type.
 *
 * ## Why everything here is a pure function
 *
 * This project's frontend tests render with `react-dom/server` in vitest's node
 * environment. `renderToStaticMarkup` fires no handlers, so a rule written
 * inside `onKeyDown` is a rule no test reaches. Every decision the box makes is
 * therefore exported and taken by a function above; the handlers only call
 * them.
 */

import { useRef, useState } from 'react'
import type { KeyboardEvent } from 'react'

import { Button } from '@/components/ui/button'
import { Textarea } from '@/components/ui/textarea'

/** One thing to put on the wire. `end-mode` is a control message; `data` is bytes. */
export type ReplyFrame = { kind: 'end-mode'; pane: string } | { kind: 'data'; bytes: string }

/**
 * What `dispatchReply` needs of the terminal: exactly the two methods, so this
 * file never sees a `TerminalHandle` and its tests never need one.
 */
export interface ReplySink {
  send(bytes: string | Uint8Array): boolean
  endMode(pane: string): boolean
}

/** What the box says when it is empty. */
export const REPLY_PLACEHOLDER = 'Reply to this pane…'

/**
 * The two halves of a bracketed paste.
 *
 * Written with `\x1b` and never a rendered `^[` or a two-character `\e`: this
 * repo has already lost a day to an escape sequence retyped out of prose rather
 * than built from the byte, and the test checks the char codes for that reason.
 */
export const PASTE_START = '\x1b[200~'
export const PASTE_END = '\x1b[201~'

/** Where the draft in the box came from. */
export type ReplyOrigin = 'paste' | 'type'

/**
 * The origin of the draft after one change to it.
 *
 * Sticky, and deliberately: paste a stack trace and then type "what causes
 * this?" under it and the whole draft is still a paste, because the twelve
 * lines above the question are still twelve lines. An empty box resets, so a
 * draft cleared and retyped by hand is typing again.
 *
 * `fromPaste` is whether a `paste` event fired for *this* change. React runs
 * `onPaste` before the `onChange` it causes, which is what makes a ref set in
 * one and read in the other the honest answer rather than a guess.
 */
export function draftOrigin(prev: ReplyOrigin, next: string, fromPaste: boolean): ReplyOrigin {
  if (next === '') return 'type'
  return fromPaste ? 'paste' : prev
}

/**
 * Whether this draft goes out wrapped.
 *
 * Two conditions, and the second one is the interesting half. A **single-line**
 * paste is not wrapped: it has no interior newline to protect, so wrapping it
 * would buy nothing and cost the Return -- and a rule that fires on everything
 * is a rule no fixture can hold still. A **typed** draft is not wrapped either,
 * even a multi-line one: Shift+Enter is a line the user asked for, one at a
 * time, with the pane in front of them.
 */
export function willBracket(text: string, origin: ReplyOrigin): boolean {
  return origin === 'paste' && text.includes('\n')
}

/**
 * What one press of a send control puts on the wire.
 *
 * Returns the frames in order, or `[]` for a send that must not happen at all.
 *
 * Whitespace is *not* empty. A bare space is a legitimate answer -- it is what
 * "press any key" wants and what advances a pager -- so it is sent like any
 * other text. The natural guess is the opposite, which is why there is no
 * `trim()` here and why the test pins it.
 *
 * A null pane yields no `end-mode` frame and sends the data anyway. `end-mode`
 * names a pane and the server refuses an unnamed one, but the bytes reach this
 * tab's client wherever tmux has it, which is the pane the user is looking at.
 * Blocking a reply on a pane id the app has not learned yet would refuse to
 * answer the agent on screen.
 *
 * A bracketed draft ignores `withReturn` and ends at `ESC[201~`. The Return is
 * the submit, and twelve pasted lines are the one thing you want to read back
 * off the pane before you spend a turn on them; both send controls therefore
 * deliver the same bytes for a multi-line paste, and `replyHint` says so before
 * the user presses either. Wrapping is a browser-side fact -- it is a property
 * of what the input widget did, not of the pane -- so no flag for it goes on
 * the wire and the daemon never learns a paste happened.
 */
export function replyFrames(
  text: string,
  opts: { pane: string | null; withReturn: boolean; origin: ReplyOrigin },
): ReplyFrame[] {
  if (text === '') return []
  const bytes = willBracket(text, opts.origin)
    ? `${PASTE_START}${text}${PASTE_END}`
    : opts.withReturn
      ? `${text}\r`
      : text
  const data: ReplyFrame = { kind: 'data', bytes }
  return opts.pane === null ? [data] : [{ kind: 'end-mode', pane: opts.pane }, data]
}

/**
 * Put the frames on the wire, in the array's order.
 *
 * One place, so the ordering rule lives in `replyFrames` and nowhere else. The
 * sink App passes is the terminal handle, whose `send` is the transport -- see
 * the note on `TerminalHandle.send` in `Terminal.tsx`, which is the one thing
 * about this feature that no test in vitest can check.
 */
export function dispatchReply(frames: ReplyFrame[], sink: ReplySink | null): void {
  if (!sink) return
  for (const frame of frames) {
    if (frame.kind === 'end-mode') sink.endMode(frame.pane)
    else sink.send(frame.bytes)
  }
}

/** What a keystroke in the box means. */
export type ReplyIntent = 'send' | 'newline' | 'blur' | 'pass'

/**
 * Read a keydown.
 *
 * `pass` means the box does nothing and does not preventDefault, which is what
 * keeps `Ctrl+Alt+K` working from inside it: the palette's listener is in the
 * capture phase on `window`, ahead of wterm's, and the box must not become a
 * third thing that eats the chord. Any modified Enter is `pass` for the same
 * reason -- guessing at a chord someone else may own is how you swallow it.
 */
export function replyKey(event: {
  key: string
  shiftKey: boolean
  ctrlKey: boolean
  altKey: boolean
  metaKey: boolean
}): ReplyIntent {
  if (event.ctrlKey || event.altKey || event.metaKey) return 'pass'
  if (event.key === 'Escape') return 'blur'
  if (event.key !== 'Enter') return 'pass'
  return event.shiftKey ? 'newline' : 'send'
}

/**
 * What the box holds after the visible pane moved, and whether it owes the user
 * a word about it.
 *
 * The bytes go to this tab's client, which follows the selection, so a sentence
 * half-typed against one agent would silently retarget to another one's prompt.
 * Clearing is the safe half; saying so is the other half, because a box that
 * empties itself while you look away is indistinguishable from one that lost
 * what you typed.
 */
export function draftAfterPaneChange(
  draft: string,
  from: string | null,
  to: string | null,
): { draft: string; discarded: boolean } {
  if (from === to) return { draft, discarded: false }
  return { draft: '', discarded: draft !== '' }
}

/** The line under the box: where the bytes go, and what happened to the last draft. */
export function replyHint(pane: string | null, discarded: boolean, bracketed: boolean): string {
  if (discarded) return 'The pane changed — the draft was cleared.'
  // No mention of Ctrl+C anywhere in this component: in a text field that is
  // the browser's copy, it cannot be reclaimed, and an interrupt stays
  // something you send by focusing the terminal. Saying otherwise would be
  // promising a key that does nothing.
  const where = pane === null ? 'this pane' : pane
  // The one case where the send controls do not do what their labels say, so
  // the line says it instead of letting the user find out by pressing Send and
  // watching nothing get answered.
  if (bracketed) return `Pasted lines go to ${where} as text — no Return, submit them in the pane.`
  return `Enter sends to ${where}, Shift+Enter for a newline.`
}

/**
 * Whether the reply box is on screen.
 *
 * It takes the pane's agent state and ignores it, and that is the whole point
 * of the signature: the box moving on agent state is the mistake worth a test,
 * so the argument exists to make that mistake expressible and killable. See the
 * three reasons in this file's header.
 */
export function showReplyBox(view: { attached: boolean; agentState: string }): boolean {
  return view.attached
}

export interface ReplyBoxProps {
  /**
   * The pane the bytes will reach: this tab's current pane, or null before the
   * tab has learned it.
   */
  pane: string | null
  /** Where the frames go. App walks them onto the terminal handle. */
  onSend: (frames: ReplyFrame[]) => void
  className?: string
}

/**
 * The box.
 *
 * Never disabled, even while the socket is down. `TerminalSession.write`
 * refuses the bytes one layer in and the connection pill starts saying that
 * typing is not being sent, which is the same bargain the terminal itself
 * makes: a control that goes dead is indistinguishable from an agent that hung.
 */
export function ReplyBox({ pane, onSend, className }: ReplyBoxProps) {
  const [text, setText] = useState('')
  const [origin, setOrigin] = useState<ReplyOrigin>('type')
  const [discarded, setDiscarded] = useState(false)
  const box = useRef<HTMLTextAreaElement | null>(null)
  // Set by `onPaste`, read by the `onChange` that same event causes, cleared
  // there. A ref and not state because it must be readable inside the very next
  // render's handler rather than after a re-render.
  const pasting = useRef(false)

  // React's own "adjust state when a prop changes" shape, rather than an
  // effect: the clear has to happen before the box is painted against the new
  // pane, or one frame of the old draft is on screen under the new pane's name.
  const [seenPane, setSeenPane] = useState(pane)
  if (seenPane !== pane) {
    const next = draftAfterPaneChange(text, seenPane, pane)
    setSeenPane(pane)
    setText(next.draft)
    setOrigin('type')
    setDiscarded(next.discarded)
  }

  const bracketed = willBracket(text, origin)

  function fire(withReturn: boolean) {
    const frames = replyFrames(text, { pane, withReturn, origin })
    if (frames.length === 0) return
    onSend(frames)
    setText('')
    setOrigin('type')
    setDiscarded(false)
  }

  function onKeyDown(event: KeyboardEvent<HTMLTextAreaElement>) {
    switch (replyKey(event)) {
      case 'send':
        event.preventDefault()
        fire(true)
        break
      case 'blur':
        event.preventDefault()
        box.current?.blur()
        break
      // `newline` and `pass` are both "let the textarea have it". They are
      // separate cases so that Shift+Enter is a decision this file made rather
      // than a default it happened to inherit.
      default:
        break
    }
  }

  return (
    <div className={`flex shrink-0 items-start gap-2 border-t px-2 py-1.5 ${className ?? ''}`}>
      <div className="flex min-w-0 flex-1 flex-col gap-0.5">
        <Textarea
          ref={box}
          rows={1}
          value={text}
          onChange={(e) => {
            setText(e.target.value)
            setOrigin(draftOrigin(origin, e.target.value, pasting.current))
            pasting.current = false
            setDiscarded(false)
          }}
          onPaste={() => {
            pasting.current = true
          }}
          onKeyDown={onKeyDown}
          placeholder={REPLY_PLACEHOLDER}
          aria-label="Reply to this pane"
          className="max-h-32 min-h-8 resize-none py-1 font-mono text-sm"
        />
        <span className="text-muted-foreground px-0.5 text-[10px]">
          {replyHint(pane, discarded, bracketed)}
        </span>
      </div>
      <Button
        type="button"
        size="sm"
        onClick={() => fire(true)}
        title={
          bracketed
            ? 'Send the pasted lines as text — no Return, so nothing is submitted'
            : 'Send the reply and a Return'
        }
      >
        Send
      </Button>
      {/*
        The small one. Answering a `y/n` prompt or a single-key menu is one
        character and no Return -- a box that always appends CR cannot express
        it, and the Return it would append is a second keystroke into whatever
        the prompt did next.
      */}
      <Button
        type="button"
        size="xs"
        variant="outline"
        onClick={() => fire(false)}
        title="Send the text with no Return — for a y/n prompt or a single-key menu"
      >
        No ⏎
      </Button>
    </div>
  )
}

export default ReplyBox
