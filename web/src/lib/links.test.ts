import { Renderer } from '@wterm/dom'
import { describe, expect, it } from 'vitest'
import { isOpenModifier, urlAt } from './links'

describe('urlAt', () => {
  const at = (text: string, needle: string, offset = 0) =>
    urlAt(text, text.indexOf(needle) + offset)

  it('finds a bare URL from anywhere inside it', () => {
    const line = 'created PR https://github.com/example/project/pull/13914 ok'
    for (const off of [0, 5, 20, 44]) {
      expect(at(line, 'https', off)).toBe('https://github.com/example/project/pull/13914')
    }
  })

  it('ignores text that is not a URL', () => {
    expect(at('just some words here', 'words')).toBeNull()
    expect(at('ftp://example.com/x', 'ftp')).toBeNull()
    expect(at('git@github.com:me/repo.git', 'git@')).toBeNull()
  })

  it('drops prose punctuation that follows a URL', () => {
    expect(at('see https://example.com/a.', 'https')).toBe('https://example.com/a')
    expect(at('see https://example.com/a, then', 'https')).toBe('https://example.com/a')
    expect(at('"https://example.com/a"', 'https')).toBe('https://example.com/a')
  })

  it('keeps brackets that belong to the URL but not ones that wrap it', () => {
    // The distinguishing case: both end in ')'.
    expect(at('(see https://example.com/a)', 'https')).toBe('https://example.com/a')
    expect(at('https://en.wikipedia.org/wiki/Bracket_(disambiguation)', 'https')).toBe(
      'https://en.wikipedia.org/wiki/Bracket_(disambiguation)',
    )
  })

  it('keeps query strings and fragments intact', () => {
    const u = 'https://example.com/s?q=a+b&n=1#frag'
    expect(at(`x ${u} y`, 'https')).toBe(u)
  })

  it('handles a URL at either edge of the line', () => {
    expect(at('https://example.com/a rest', 'https')).toBe('https://example.com/a')
    expect(at('rest https://example.com/a', 'https')).toBe('https://example.com/a')
  })

  it('returns null for an out-of-range index rather than throwing', () => {
    expect(urlAt('https://example.com', -1)).toBeNull()
    expect(urlAt('https://example.com', 999)).toBeNull()
  })
})

describe('isOpenModifier', () => {
  const ev = (o: Partial<Record<'shiftKey' | 'ctrlKey' | 'metaKey', boolean>>) => ({
    shiftKey: false,
    ctrlKey: false,
    metaKey: false,
    ...o,
  })

  it('accepts shift, ctrl and meta', () => {
    expect(isOpenModifier(ev({ shiftKey: true }))).toBe(true)
    expect(isOpenModifier(ev({ ctrlKey: true }))).toBe(true)
    expect(isOpenModifier(ev({ metaKey: true }))).toBe(true)
  })

  it('rejects a plain click, which belongs to tmux', () => {
    expect(isOpenModifier(ev({}))).toBe(false)
  })
})

/**
 * The other half of `installLinkOpener`: what an OSC 8 anchor's href can be.
 *
 * That path hands `anchor.href` to `open()` with no scheme check of its own,
 * because wterm builds `a.term-link` only for http(s) -- `safeLinkHref` in
 * @wterm/dom's renderer. Nothing in this repo would notice that filter going
 * away in a `^0.5.0` bump, and the first place it would show is a terminal row
 * turning `javascript:` into a click, so it is pinned here against whatever
 * renderer is actually installed.
 *
 * This suite has no DOM, so the anchor cannot be built and clicked; the row
 * markup is checked at the point the filter runs instead. `_buildRowContent` is
 * the narrowest way into it -- of the DOM it only writes `innerHTML` and two
 * style properties, which a plain object can stand in for. If a later version
 * renames it, failing here is still the right outcome: the invariant has to be
 * re-read either way, and the throw below says so.
 */
describe('the renderer this delegates OSC 8 hrefs to', () => {
  /** One row of `link`, every cell carrying `uri` as its OSC 8 target. */
  function rowMarkup(uri: string): string {
    const renderer = Object.create(Renderer.prototype) as unknown as {
      cols: number
      prevRowBg: string[]
      _buildRowContent: (
        rowEl: { innerHTML: string; style: Record<string, string> },
        getCell: (col: number) => unknown,
        lineLen: number,
        cursorCol: number,
        rowIndex: number,
      ) => void
    }
    if (typeof renderer._buildRowContent !== 'function') {
      throw new Error('@wterm/dom moved Renderer#_buildRowContent: re-check its href filter')
    }
    const text = 'link'
    renderer.cols = text.length
    renderer.prevRowBg = []
    const rowEl = { innerHTML: '', style: {} as Record<string, string> }
    renderer._buildRowContent(
      rowEl,
      (col) => ({
        char: text.codePointAt(col),
        chars: text[col],
        width: 1,
        fg: -1,
        bg: -1,
        flags: 0,
        linkUri: uri,
        linkId: '1',
      }),
      text.length,
      -1,
      -1,
    )
    return rowEl.innerHTML
  }

  it('turns an http or https target into a term-link anchor', () => {
    // Not a formality: without it every refusal below would also pass against a
    // renderer that had stopped emitting anchors at all.
    expect(rowMarkup('https://example.com/a')).toContain(
      '<a class="term-link" href="https://example.com/a"',
    )
    expect(rowMarkup('http://example.com/a')).toContain(
      '<a class="term-link" href="http://example.com/a"',
    )
  })

  it('emits no anchor at all for a target we would otherwise open', () => {
    for (const uri of [
      'javascript:alert(1)',
      'data:text/html,<script>alert(1)</script>',
      'file:///etc/passwd',
      'vbscript:msgbox(1)',
      'https://example.com /a',
    ]) {
      const markup = rowMarkup(uri)
      expect(markup, uri).not.toContain('term-link')
      expect(markup, uri).not.toContain('href=')
    }
  })
})
