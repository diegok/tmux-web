/**
 * Opening links from the terminal.
 *
 * Two sources, one gesture. Programs that emit OSC 8 (gh, delta, eza) already
 * arrive as real `<a class="term-link">` anchors, because tmux is told this
 * client renders hyperlinks -- see internal/tmux/session.go. Everything else is
 * plain text, so a bare URL has to be recovered from the row under the pointer.
 *
 * The plain-text path deliberately does not touch the DOM. wterm re-renders
 * dirty rows every animation frame, so anchors injected into a row would be
 * wiped, and a URL spanning a wrapped line would be cut in half. Reading the
 * text at click time costs a hover underline and nothing else.
 */

/** Schemes we will open. Matches wterm's own filter for OSC 8 hrefs. */
const SCHEME = /https?:\/\//

/**
 * Trailing characters that are almost never part of a URL: prose punctuation
 * that follows one. Brackets are handled separately, since they are common
 * *inside* URLs -- a Wikipedia or generated-doc link may legitimately end in one.
 */
const TRAILING = /[.,;:!?'"`]+$/

/** Characters that end a URL token. Whitespace, and the quotes shells add. */
const BOUNDARY = /[\s"'`<>]/

/**
 * Balance one bracket pair at the end of a URL.
 *
 * `(see https://example.com/a)` should not include the closing paren, but
 * `https://en.wikipedia.org/wiki/Bracket_(disambiguation)` should. Counting is
 * what tells them apart.
 */
function trimUnbalanced(url: string, open: string, close: string): string {
  while (url.endsWith(close)) {
    let depth = 0
    for (const ch of url) {
      if (ch === open) depth++
      else if (ch === close) depth--
    }
    if (depth >= 0) break
    url = url.slice(0, -1)
  }
  return url
}

/**
 * The URL at `index` in `text`, or null.
 *
 * `index` may land anywhere inside the URL, because it comes from wherever the
 * pointer was.
 */
export function urlAt(text: string, index: number): string | null {
  if (index < 0 || index > text.length) return null

  let start = index
  while (start > 0 && !BOUNDARY.test(text[start - 1])) start--
  let end = index
  while (end < text.length && !BOUNDARY.test(text[end])) end++

  let token = text.slice(start, end)
  if (!SCHEME.test(token)) return null
  // A token like `(https://example.com` picks up the opening paren from prose.
  token = token.replace(/^[([{<'"`]+/, '')
  if (!token.startsWith('http')) return null

  token = token.replace(TRAILING, '')
  token = trimUnbalanced(token, '(', ')')
  token = trimUnbalanced(token, '[', ']')

  try {
    // The definitive scheme gate, and not a second copy of SCHEME above. SCHEME
    // matched the *raw* token, and the token has been rewritten since --
    // opening brackets, TRAILING and trimUnbalanced each cut characters off it
    // -- while `startsWith('http')` only catches a token whose scheme was eaten
    // outright. What the caller opens is `u.href`, so the protocol has to be
    // read off the same parse that produced it.
    const u = new URL(token)
    return u.protocol === 'http:' || u.protocol === 'https:' ? u.href : null
  } catch {
    return null
  }
}

/** True when the event carries a modifier we treat as "open this link". */
export function isOpenModifier(e: {
  shiftKey: boolean
  ctrlKey: boolean
  metaKey: boolean
}): boolean {
  return e.shiftKey || e.ctrlKey || e.metaKey
}

/** Row element wterm renders each terminal line into. */
const ROW = '.term-row'

/** The caret position under a point, across the two APIs browsers offer. */
function caretAt(doc: Document, x: number, y: number): { node: Node; offset: number } | null {
  const withPosition = doc as Document & {
    caretPositionFromPoint?: (x: number, y: number) => { offsetNode: Node; offset: number } | null
  }
  if (typeof withPosition.caretPositionFromPoint === 'function') {
    const p = withPosition.caretPositionFromPoint(x, y)
    return p ? { node: p.offsetNode, offset: p.offset } : null
  }
  const withRange = doc as Document & {
    caretRangeFromPoint?: (x: number, y: number) => Range | null
  }
  if (typeof withRange.caretRangeFromPoint === 'function') {
    const r = withRange.caretRangeFromPoint(x, y)
    return r ? { node: r.startContainer, offset: r.startOffset } : null
  }
  return null
}

/** Offset of (node, offset) within row.textContent. */
function offsetInRow(row: Element, node: Node, offset: number): number {
  const walker = row.ownerDocument.createTreeWalker(row, NodeFilter.SHOW_TEXT)
  let seen = 0
  while (walker.nextNode()) {
    if (walker.currentNode === node) return seen + offset
    seen += walker.currentNode.textContent?.length ?? 0
  }
  return seen
}

/**
 * The line under the pointer, joined with its neighbours.
 *
 * A URL longer than the terminal is wide is split across rows with no marker in
 * the text, so reading one row would yield half a link. Joining the row before
 * and after costs nothing when the URL does not wrap: the extra text is on the
 * far side of a space, and urlAt stops at whitespace.
 */
function joinedLine(row: Element): { text: string; base: number } {
  const prev = row.previousElementSibling?.matches(ROW) ? row.previousElementSibling : null
  const next = row.nextElementSibling?.matches(ROW) ? row.nextElementSibling : null
  const before = (prev?.textContent ?? '').trimEnd()
  const mid = row.textContent ?? ''
  const after = (next?.textContent ?? '').trimEnd()
  return { text: before + mid + after, base: before.length }
}

/**
 * Open links from the terminal, and report whether the element was wired.
 *
 * Runs in the capture phase so it settles the click before wterm decides
 * whether to forward it to tmux as a mouse report. A plain click is left alone
 * and still reaches tmux.
 *
 * The two `open` features are load-bearing, not boilerplate. `noopener` severs
 * `window.opener`, so a page reached from a terminal row cannot navigate the
 * tab it came from -- reverse tabnabbing, aimed at a window that is a live
 * shell on an already-enrolled device. `noreferrer` keeps this host out of the
 * `Referer` the opened site reads; on a deployment that is one private name,
 * the name is the thing worth not announcing.
 */
export function installLinkOpener(
  el: HTMLElement,
  open: (url: string) => void = (url) => window.open(url, '_blank', 'noopener,noreferrer'),
): () => void {
  const onClick = (event: MouseEvent) => {
    if (event.button !== 0 || !isOpenModifier(event)) return

    const target = event.target
    if (!(target instanceof Element)) return

    // An OSC 8 anchor already knows its own destination, and it is the one the
    // emitting program chose -- the visible text is often not the URL at all
    // ("#13914"), so re-deriving it from the text would be wrong.
    //
    // Its href gets no scheme check here because it never had a chance to be
    // anything else: wterm builds `a.term-link` only for http(s) and emits no
    // anchor at all otherwise. That is a dependency's invariant holding a click
    // path, so links.test.ts pins it against the installed renderer.
    const anchor = target.closest('a.term-link')
    if (anchor instanceof HTMLAnchorElement && anchor.href) {
      event.preventDefault()
      event.stopPropagation()
      open(anchor.href)
      return
    }

    const row = target.closest(ROW)
    if (!row) return
    const caret = caretAt(el.ownerDocument, event.clientX, event.clientY)
    if (!caret) return

    const { text, base } = joinedLine(row)
    const url = urlAt(text, base + offsetInRow(row, caret.node, caret.offset))
    if (!url) return

    event.preventDefault()
    event.stopPropagation()
    open(url)
  }

  el.addEventListener('click', onClick, true)
  return () => el.removeEventListener('click', onClick, true)
}
