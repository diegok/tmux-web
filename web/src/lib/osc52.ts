/**
 * OSC 52, the sequence a terminal application uses to put text on the
 * clipboard of whatever is drawing it.
 *
 * ## Why this file exists
 *
 * Highlighting text in the browser terminal did not copy anything, and the only
 * way to get text out was the capture panel. The cause was not where it looked.
 * `mouse on` means tmux owns the drag, so the browser never gets a DOM
 * selection -- but tmux does not keep the result to itself. MEASURED against a
 * second tmux standing in for the browser: with `set-clipboard external` (the
 * default, and what the owner runs) and the owner's own binding,
 *
 *     bind -T copy-mode-vi MouseDragEnd1Pane send -X copy-pipe-and-cancel "xclip ..."
 *
 * tmux sends the selected text to its client as OSC 52 on every drag. It was
 * arriving all along and being thrown away here, because `@wterm/dom` exposes
 * `onData`, `onTitle` and `onResize` and no hook for OSC at all. So the
 * sequence is read out of the byte stream on the way past, by this scanner.
 *
 * The owner's `xclip` is a separate matter and not this file's business: it
 * writes the X clipboard of the machine tmux runs on, which is why copying
 * "used to work" from a local terminal and never worked from the phone.
 *
 * ## Observe, never rewrite
 *
 * `feed` returns what it found and leaves the caller's bytes alone: the stream
 * still goes to the terminal exactly as it arrived. Stripping the sequence
 * would mean rebuilding every buffer that contains one for no benefit -- wterm
 * already ignores an OSC it does not know, which is why nothing has been
 * appearing on screen in place of these for months.
 *
 * ## The shape it is reading
 *
 *     ESC ]  52 ; Pc ; Pd  ST
 *
 * `Pc` is the selection -- `c`, `p`, `s`, a cut-buffer digit, or empty -- and
 * this scanner does not care which: tmux only sends one when the user copied,
 * and a browser has exactly one clipboard to put it on. `Pd` is base64, or `?`
 * to *ask* for the clipboard rather than set it. `ST` is BEL or `ESC \`, and
 * both are in the wild: tmux emits `ESC \` but passes an application's BEL
 * form straight through under `set-clipboard external`.
 *
 * Only the 7-bit `ESC ]` introducer is recognised. The 8-bit C1 form (0x9d) is
 * not: it is indistinguishable from a UTF-8 continuation byte in a stream that
 * is mostly UTF-8 text, and tmux does not emit it.
 */

const ESC = 0x1b
const BEL = 0x07
/** `]`, which turns an ESC into an OSC introducer. */
const OSC_INTRO = 0x5d
/** `\`, which turns an ESC into the string terminator. */
const BACKSLASH = 0x5c

/**
 * The most OSC body this scanner will hold before giving up on a sequence.
 *
 * A cap is required rather than prudent: the body is accumulated across feeds,
 * so a stream containing `ESC ]` and no terminator -- a truncated sequence, a
 * binary file catted into the pane -- would otherwise grow this buffer without
 * limit for as long as the tab stays open. 1 MiB of base64 is about 768 KiB of
 * text, far more than anyone selects, and well above tmux's own habits.
 *
 * Hitting it drops the sequence and keeps scanning for the terminator, so the
 * scanner resyncs instead of mistaking the rest of the stream for a body.
 */
export const OSC52_MAX_BODY = 1 << 20

/** The ASCII prefix that marks an OSC body as a clipboard sequence. */
const CLIPBOARD_PREFIX = '52;'

/**
 * Decode one OSC body -- the bytes between `ESC ]` and the terminator -- into
 * the text it would put on the clipboard, or null when it would put nothing
 * there.
 *
 * Null covers every case where writing would be wrong rather than merely
 * unnecessary, and one of them matters: an OSC 52 whose `Pd` is empty is a
 * request to CLEAR the clipboard. Honouring it would mean a stray sequence in
 * someone's output silently wiping whatever they had copied, so it is ignored.
 * `?` is a query -- the application asking what the clipboard holds -- and is
 * not a copy either.
 *
 * Exported for its own tests: it is the half of this file with all the format
 * in it, and the state machine above has none.
 */
export function decodeOsc52Body(body: readonly number[]): string | null {
  // The body of a clipboard sequence is ASCII throughout -- a digit pair, a
  // selection character, semicolons and base64 -- so reading it as code units
  // cannot corrupt the part being parsed. The payload's own UTF-8 is decoded
  // from the base64 below, not from here.
  let head = ''
  for (let i = 0; i < body.length && i < CLIPBOARD_PREFIX.length; i++) {
    head += String.fromCharCode(body[i])
  }
  if (head !== CLIPBOARD_PREFIX) return null

  // After `52;` comes Pc, then one `;`, then Pd. Base64 contains no `;`, so the
  // first one ends the selection field.
  let sep = -1
  for (let i = CLIPBOARD_PREFIX.length; i < body.length; i++) {
    if (body[i] === 0x3b /* ; */) {
      sep = i
      break
    }
  }
  // DELIBERATELY REDUNDANT, and a mutation run says so: deleting this line
  // changes no test, because without a second `;` the payload below starts at
  // index 0 and therefore still contains the `52;`, and `atob` rejects `;`.
  // It stays because "a body with no selection field is not a clipboard
  // sequence" is the rule, and leaving that to the base64 alphabet would make
  // it true by luck.
  if (sep < 0) return null

  let encoded = ''
  for (let i = sep + 1; i < body.length; i++) encoded += String.fromCharCode(body[i])

  // The empty case is load-bearing and the `?` case is not, which is worth
  // knowing apart. An empty payload is a request to CLEAR the clipboard, and
  // `atob('')` is a perfectly good empty string, so without this a stray
  // sequence would wipe what the user had copied. `?` is a QUERY, and `atob`
  // would reject it anyway because `?` is not in the base64 alphabet --
  // measured -- so this half is intent made explicit rather than a defence.
  if (encoded === '' || encoded === '?') return null

  return decodeBase64Utf8(encoded)
}

/**
 * base64 to text, or null if it is not base64.
 *
 * `atob` is the whole decoder and that is a measured decision, not laziness.
 * It implements the HTML spec's *forgiving* base64: ASCII whitespace is
 * stripped, and missing padding is accepted except for the one length that no
 * padding could rescue (`length % 4 === 1`, which throws). Restoring the
 * padding by hand first, which this used to do, changed no input's outcome.
 */
function decodeBase64Utf8(encoded: string): string | null {
  let binary: string
  try {
    binary = atob(encoded)
  } catch {
    return null
  }
  const bytes = new Uint8Array(binary.length)
  for (let i = 0; i < binary.length; i++) bytes[i] = binary.charCodeAt(i)
  // `fatal: false`, deliberately: a selection of a pane holding broken UTF-8 --
  // a half-written line, a binary file -- should arrive with replacement
  // characters in it rather than not arrive.
  return new TextDecoder('utf-8').decode(bytes)
}

/** The scanner's position in a sequence, carried between feeds. */
type State =
  /** Nothing started; looking for ESC. */
  | 'ground'
  /** An ESC arrived; `]` would start a body. */
  | 'esc'
  /** Inside an OSC body, accumulating. */
  | 'body'
  /** An ESC arrived inside a body; `\` would terminate it. */
  | 'bodyEsc'

/**
 * A byte-stream scanner that reports the clipboard texts it sees.
 *
 * One instance per socket. It is stateful on purpose: a sequence routinely
 * spans frames -- the payload is as long as the selection, and the framing is
 * whatever TCP and the write loop chose -- so a scanner that only looked inside
 * one buffer would miss exactly the long selections that are worth copying.
 */
export class Osc52Scanner {
  #state: State = 'ground'
  #body: number[] = []
  /** Set when the body passed OSC52_MAX_BODY; cleared when the string ends. */
  #overflowed = false

  /**
   * Feed the bytes that are on their way to the terminal. Returns the clipboard
   * texts completed by these bytes, in the order they finished -- usually none,
   * and more than one only when a frame carried two.
   */
  feed(bytes: Uint8Array): string[] {
    const found: string[] = []
    for (let i = 0; i < bytes.length; i++) {
      const b = bytes[i]
      switch (this.#state) {
        case 'ground':
          if (b === ESC) this.#state = 'esc'
          break

        case 'esc':
          this.#afterEsc(b)
          break

        case 'body':
          if (b === BEL) this.#end(found)
          else if (b === ESC) this.#state = 'bodyEsc'
          else this.#accumulate(b)
          break

        case 'bodyEsc':
          if (b === BACKSLASH) {
            this.#end(found)
          } else {
            // An ESC that is not the terminator cancels the control string.
            // The byte after it is still a byte of the stream, and it may be
            // the `]` of the next sequence -- a title written immediately
            // after a copy is exactly that -- so it is re-dispatched rather
            // than dropped.
            this.#discard()
            this.#state = 'esc'
            this.#afterEsc(b)
          }
          break
      }
    }
    return found
  }

  /** What a byte means with one ESC already seen. */
  #afterEsc(b: number): void {
    if (b === OSC_INTRO) {
      this.#state = 'body'
      this.#body = []
      this.#overflowed = false
    } else if (b === ESC) {
      // A second ESC: still exactly one ESC pending.
      this.#state = 'esc'
    } else {
      this.#state = 'ground'
    }
  }

  #accumulate(b: number): void {
    if (this.#body.length >= OSC52_MAX_BODY) {
      // Keep scanning for the terminator, but stop holding the body: the
      // sequence is already lost and the point now is to resync.
      this.#overflowed = true
      this.#body = []
      return
    }
    this.#body.push(b)
  }

  #end(found: string[]): void {
    const body = this.#body
    const overflowed = this.#overflowed
    this.#discard()
    if (overflowed) return
    const text = decodeOsc52Body(body)
    if (text !== null) found.push(text)
  }

  #discard(): void {
    this.#state = 'ground'
    this.#body = []
    this.#overflowed = false
  }
}
