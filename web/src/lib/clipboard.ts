/**
 * Putting text the *terminal* sent us on the user's clipboard.
 *
 * The decode is `osc52.ts`'s problem. This file's problem is that the write has
 * no user gesture behind it: the text arrives in a WebSocket message, which is
 * a network callback, and browsers guard the clipboard with user activation.
 * Chrome grants `clipboard-write` to the focused tab and lets it through;
 * Safari, and iOS Safari in particular, is stricter and can refuse outright.
 *
 * So a refusal is an expected outcome rather than an error, and it has an
 * answer: the text is handed back, and the caller offers a button. A tap on
 * that button IS a gesture, so the retry runs inside one and succeeds. Nothing
 * is lost either way -- the selection is still in tmux's buffer and still on
 * screen.
 *
 * Success is deliberately silent. The copy that produces it is a mouse drag,
 * and a toast on every drag would be its own bug.
 */

/**
 * The one thing this needs from `navigator.clipboard`.
 *
 * `CapturePanel` declares an identical interface for its own button. They are
 * structurally the same type and deliberately not shared: that one is about a
 * click the user just made, this one is about a write with no click anywhere
 * near it, and the two will not stay the same shape.
 */
export interface ClipboardWriter {
  writeText(text: string): Promise<void>
}

/** Why a copy did not land, and what it was. */
export interface CopyFailure {
  /** The text that did not make it, so a retry has something to write. */
  text: string
  /** The browser's own words, for the message the user sees. */
  reason: string
}

/** What a browser with no clipboard at all gets told. */
const NO_CLIPBOARD = 'this browser will not give the page a clipboard'

/**
 * Write text the terminal handed over. Resolves with null when it landed and
 * with a CopyFailure when the browser refused.
 *
 * It never rejects. A rejected promise here would be an unhandled rejection in
 * a WebSocket callback, which is a console error on a path that is allowed to
 * fail.
 */
export function writeTerminalClipboard(
  text: string,
  clipboard: ClipboardWriter | undefined = globalThis.navigator?.clipboard,
): Promise<CopyFailure | null> {
  if (!clipboard) return Promise.resolve({ text, reason: NO_CLIPBOARD })
  // Inside the try as well as the catch: a clipboard whose writeText throws
  // synchronously -- which is what an older WebKit does when the context is
  // not secure -- would otherwise escape past the `.catch` below.
  try {
    return clipboard.writeText(text).then(
      () => null,
      (err: unknown) => ({ text, reason: reasonOf(err) }),
    )
  } catch (err) {
    return Promise.resolve({ text, reason: reasonOf(err) })
  }
}

function reasonOf(err: unknown): string {
  if (err instanceof Error && err.message !== '') return err.message
  const s = String(err)
  return s === '' ? 'the browser refused without saying why' : s
}
