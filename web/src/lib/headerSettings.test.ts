import { beforeEach, describe, expect, it } from 'vitest'

import {
  DEFAULT_HEADER_SETTINGS,
  onHeaderSettingsChange,
  parseHeaderSettings,
  readHeaderSettings,
  resetHeaderSettings,
  setHeaderSettings,
  writeHeaderSettings,
  type HeaderSettings,
} from './headerSettings'

// The store is module-level, so without this one test's choice is the next
// one's start state. `null` is "no storage at all", which is also the shape a
// browser with site data blocked presents.
beforeEach(() => {
  resetHeaderSettings(null)
})

describe('parseHeaderSettings', () => {
  it('reads both flags on when there is nothing stored', () => {
    expect(parseHeaderSettings(null)).toEqual({ showJump: true, showReply: true })
  })

  it('reads what was stored', () => {
    expect(parseHeaderSettings('{"showJump":true,"showReply":false}')).toEqual({
      showJump: true,
      showReply: false,
    })
  })

  // A value written by a version that is not this one. Field by field, so a
  // field we cannot make sense of never costs the user the other one.
  it('falls back field by field, so one bad field does not cost the other', () => {
    expect(parseHeaderSettings('{"showJump":"yes","showReply":false}')).toEqual({
      showJump: true,
      showReply: false,
    })
  })

  it('reads the defaults from anything that is not an object of flags', () => {
    for (const raw of ['', 'null', '[]', '{', '"on"', '7']) {
      expect(parseHeaderSettings(raw), raw).toEqual(DEFAULT_HEADER_SETTINGS)
    }
  })
})

describe('readHeaderSettings', () => {
  // Site data can be blocked outright -- a private window, an embedded webview
  // -- and then even reaching for `localStorage` throws. The header must still
  // come up, with both controls on it.
  it('reads the defaults from a store that throws', () => {
    const store = {
      getItem(): string | null {
        throw new Error('blocked')
      },
      setItem(): void {},
    }
    expect(readHeaderSettings(store)).toEqual(DEFAULT_HEADER_SETTINGS)
  })
})

describe('writeHeaderSettings', () => {
  it('swallows a store that refuses the write', () => {
    const store = {
      getItem: (): string | null => null,
      setItem(): void {
        throw new Error('quota')
      },
    }
    expect(() => writeHeaderSettings(DEFAULT_HEADER_SETTINGS, store)).not.toThrow()
  })
})

describe('setHeaderSettings', () => {
  // The ordering the reply box's close depends on: App's subscriber has to run
  // before React is told, because what it calls arms a resize claim that a
  // post-commit effect would arm too late.
  it('tells its subscribers synchronously, before it returns', () => {
    const seen: HeaderSettings[] = []
    const off = onHeaderSettingsChange((s) => seen.push(s))

    setHeaderSettings({ showReply: false })
    expect(seen).toEqual([{ showJump: true, showReply: false }])

    off()
    setHeaderSettings({ showReply: true })
    expect(seen).toHaveLength(1)
  })

  it('keeps the field a patch does not name, and notifies nobody for a no-op', () => {
    const seen: HeaderSettings[] = []
    const off = onHeaderSettingsChange((s) => seen.push(s))

    setHeaderSettings({ showJump: false })
    expect(seen).toEqual([{ showJump: false, showReply: true }])

    setHeaderSettings({ showJump: false })
    expect(seen, 'a patch that changes nothing still woke everyone up').toHaveLength(1)

    off()
  })
})
