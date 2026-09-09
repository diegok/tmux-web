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
