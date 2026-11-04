import { describe, expect, it } from 'vitest'

import { Osc52Scanner, OSC52_MAX_BODY, decodeOsc52Body } from './osc52'

/**
 * What tmux really sends, built here rather than pasted, so a test about
 * splitting is not also a test about my base64.
 */
function b64(text: string): string {
  let s = ''
  for (const byte of new TextEncoder().encode(text)) s += String.fromCharCode(byte)
  return btoa(s)
}

const ST = '\x1b\\'

function seqText(text: string, pc = 'c', terminator = ST): string {
  return `\x1b]52;${pc};${b64(text)}${terminator}`
}

const enc = (s: string) => new TextEncoder().encode(s)

/** Feed a string in one go. */
function scan(s: string): string[] {
  return new Osc52Scanner().feed(enc(s))
}

/** Feed a string in pieces of n bytes, returning everything found. */
function scanInChunks(s: string, n: number): string[] {
  const scanner = new Osc52Scanner()
  const bytes = enc(s)
  const found: string[] = []
  for (let i = 0; i < bytes.length; i += n) {
    found.push(...scanner.feed(bytes.subarray(i, i + n)))
  }
  return found
}

describe('a complete sequence', () => {
  it('is decoded with the ESC-backslash terminator tmux emits', () => {
    expect(scan(seqText('hello'))).toEqual(['hello'])
  })

  // Not hypothetical: `set-clipboard external` passes an application's own
  // OSC 52 through untouched, and plenty of applications terminate with BEL.
  it('is decoded with the BEL terminator', () => {
    expect(scan(seqText('hello', 'c', '\x07'))).toEqual(['hello'])
  })

  it.each(['c', 'p', 's', '0', '7', ''])('is decoded whatever the selection field is (%s)', (pc) => {
    expect(scan(seqText('sel', pc))).toEqual(['sel'])
  })

  it('decodes the payload as UTF-8, not as bytes', () => {
    const text = 'árbol — ñandú 🌍'
    expect(scan(seqText(text))).toEqual([text])
  })

  it('finds a sequence surrounded by ordinary output', () => {
    expect(scan(`$ git status\r\n${seqText('copied')}On branch main\r\n`)).toEqual(['copied'])
  })

  it('finds both sequences when one buffer carries two', () => {
    expect(scan(seqText('first') + 'noise' + seqText('second'))).toEqual(['first', 'second'])
  })
})

describe('a sequence split across frames', () => {
  // The case this scanner exists for: the payload is as long as the selection,
  // so the long copies -- the ones worth having -- are the ones that span
  // frames. One byte at a time is the worst case and the strongest assertion.
  it.each([1, 2, 3, 5, 7, 16, 64])('is decoded when fed %i bytes at a time', (n) => {
    const text = 'a selection long enough to be split many times over'
    expect(scanInChunks(`before${seqText(text)}after`, n)).toEqual([text])
  })

  it('is decoded when the break falls between the ESC and the backslash', () => {
    const s = seqText('split-terminator')
    const cut = s.length - 1
    const scanner = new Osc52Scanner()
    expect(scanner.feed(enc(s.slice(0, cut)))).toEqual([])
    expect(scanner.feed(enc(s.slice(cut)))).toEqual(['split-terminator'])
  })

  it('is decoded when the break falls between the ESC and the bracket', () => {
    const s = seqText('split-introducer')
    const scanner = new Osc52Scanner()
    expect(scanner.feed(enc(s.slice(0, 1)))).toEqual([])
    expect(scanner.feed(enc(s.slice(1)))).toEqual(['split-introducer'])
  })
})

describe('what must not reach the clipboard', () => {
  // `?` is the application ASKING what the clipboard holds. Treating it as a
  // copy would put a literal question mark on the user's clipboard.
  it('ignores a clipboard query', () => {
    expect(scan(`\x1b]52;c;?${ST}`)).toEqual([])
  })

  // An empty payload is a request to CLEAR the clipboard. A stray one in
  // someone's output would silently wipe whatever they had copied, so it is
  // not honoured at all.
  it('ignores an empty payload rather than clearing the clipboard', () => {
    expect(scan(`\x1b]52;c;${ST}`)).toEqual([])
    expect(decodeOsc52Body([...enc('52;c;')])).toBeNull()
  })

  it('ignores other OSC sequences', () => {
    expect(scan(`\x1b]0;a window title${ST}`)).toEqual([])
    expect(scan(`\x1b]8;;https://example.com${ST}link\x1b]8;;${ST}`)).toEqual([])
    // The prefix has to match exactly: 52 is not 520, and not 5.
    expect(scan(`\x1b]520;c;${b64('no')}${ST}`)).toEqual([])
    expect(scan(`\x1b]5;c;${b64('no')}${ST}`)).toEqual([])
  })

  it('ignores a body with no selection separator', () => {
    expect(scan(`\x1b]52;${b64('no')}${ST}`)).toEqual([])
  })

  it('ignores a payload that is not base64', () => {
    expect(scan(`\x1b]52;c;!!!not base64!!!${ST}`)).toEqual([])
  })

  // A real terminal parser treats ESC in the escape state as RESTARTING the
  // escape rather than as a byte of it, and so does this one. Without it a
  // doubled ESC ahead of a copy would cost that copy.
  it('treats a second ESC as the start of the sequence, not as a byte of it', () => {
    expect(scan(`\x1b\x1b]52;c;${b64('doubled esc')}${ST}`)).toEqual(['doubled esc'])
  })

  it('ignores a bare ESC that starts nothing', () => {
    expect(scan('\x1b')).toEqual([])
    expect(scan('\x1b[2J\x1b[H')).toEqual([])
  })
})

describe('resynchronising', () => {
  // Every giving-up path has to leave the scanner able to see the NEXT
  // sequence. A scanner that got stuck would stop copying until the tab was
  // reloaded, which is indistinguishable from the bug this fixes.
  it('finds the next sequence after an unparseable one', () => {
    expect(scan(`\x1b]52;c;!!!${ST}${seqText('after')}`)).toEqual(['after'])
  })

  it('finds the next sequence after a control string cancelled by an ESC', () => {
    // ESC [ inside the body: not the terminator, so the string is cancelled.
    expect(scan(`\x1b]52;c;${b64('lost')}\x1b[0m${seqText('after')}`)).toEqual(['after'])
  })

  // The re-dispatch case: the cancelling ESC is immediately followed by `]`,
  // which is the introducer of the sequence that must still be seen.
  it('finds a sequence that starts in the ESC that cancelled the one before', () => {
    expect(scan(`\x1b]52;c;${b64('lost')}${seqText('after')}`)).toEqual(['after'])
  })

  it('finds the next sequence after a body that never terminated', () => {
    const scanner = new Osc52Scanner()
    expect(scanner.feed(enc(`\x1b]52;c;${b64('never ends')}`))).toEqual([])
    // The next sequence's own ESC cancels the unterminated body, and its `]` is
    // re-dispatched as an introducer -- which is the whole reason the bodyEsc
    // state re-dispatches instead of dropping the byte. Without that, one
    // truncated sequence would cost the copy after it as well.
    expect(scanner.feed(enc(seqText('after')))).toEqual(['after'])
  })

  it('drops an oversized body and then keeps working', () => {
    const scanner = new Osc52Scanner()
    // No terminator: the body grows past the cap, which must bound the memory
    // this holds rather than the stream it can read.
    const huge = enc('\x1b]52;c;' + 'A'.repeat(OSC52_MAX_BODY + 1000))
    expect(scanner.feed(huge)).toEqual([])
    // The terminator for the abandoned string, then a real one.
    expect(scanner.feed(enc(ST + seqText('after')))).toEqual(['after'])
  })
})

describe('decodeOsc52Body', () => {
  it('reads the payload after the selection field', () => {
    expect(decodeOsc52Body([...enc(`52;c;${b64('x')}`)])).toBe('x')
    expect(decodeOsc52Body([...enc(`52;;${b64('x')}`)])).toBe('x')
  })

  // Both of these hold because `atob` is the HTML spec's FORGIVING base64
  // decoder, not because this file does anything about them. They are pinned
  // here so that swapping the decoder for a stricter one fails loudly instead
  // of quietly dropping sequences that travelled through a line-wrapper.
  //
  // The payloads matter: `b64('abc')` is "YWJj", which has no padding to lose,
  // so an earlier version of this test stripped nothing and asserted nothing.
  // A mutation run caught that. 'ab' and 'a' really do have padding.
  it.each([
    ['ab', 'YWI='],
    ['a', 'YQ=='],
  ])('accepts base64 that lost the padding of %s', (text, encoded) => {
    expect(encoded).toMatch(/=$/)
    expect(decodeOsc52Body([...enc('52;c;' + encoded.replace(/=+$/, ''))])).toBe(text)
  })

  it('ignores whitespace inside the payload', () => {
    const text = 'a longer selection that someone wrapped'
    const wrapped = b64(text).replace(/(.{8})/g, '$1\r\n')
    expect(wrapped).toContain('\r\n')
    expect(decodeOsc52Body([...enc(`52;c;${wrapped}`)])).toBe(text)
  })

  // A selection of a pane holding half-written UTF-8 should arrive with
  // replacement characters rather than not arrive.
  it('keeps broken UTF-8 as replacement characters', () => {
    const broken = btoa('\xff\xfe')
    expect(decodeOsc52Body([...enc(`52;c;${broken}`)])).toBe('��')
  })

  it('rejects a body that is not a clipboard sequence', () => {
    expect(decodeOsc52Body([...enc('0;title')])).toBeNull()
    expect(decodeOsc52Body([])).toBeNull()
    expect(decodeOsc52Body([...enc('52')])).toBeNull()
  })
})
