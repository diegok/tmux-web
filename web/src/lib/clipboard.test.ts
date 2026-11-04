import { describe, expect, it, vi } from 'vitest'

import { writeTerminalClipboard } from './clipboard'

describe('writeTerminalClipboard', () => {
  it('resolves with null when the write lands', async () => {
    const writeText = vi.fn(() => Promise.resolve())
    await expect(writeTerminalClipboard('copied', { writeText })).resolves.toBeNull()
    expect(writeText).toHaveBeenCalledWith('copied')
  })

  // The expected outcome on iOS Safari, not an edge case: there is no user
  // gesture behind a WebSocket message. The text has to come back so the
  // caller can offer a button whose tap IS one.
  it('hands the text back when the browser refuses', async () => {
    const writeText = vi.fn(() => Promise.reject(new Error('NotAllowedError: no user activation')))
    await expect(writeTerminalClipboard('refused', { writeText })).resolves.toEqual({
      text: 'refused',
      reason: 'NotAllowedError: no user activation',
    })
  })

  it('hands the text back when writeText throws instead of rejecting', async () => {
    const writeText = vi.fn(() => {
      throw new Error('insecure context')
    })
    await expect(writeTerminalClipboard('thrown', { writeText })).resolves.toEqual({
      text: 'thrown',
      reason: 'insecure context',
    })
  })

  it('hands the text back when there is no clipboard at all', async () => {
    await expect(writeTerminalClipboard('none', undefined)).resolves.toEqual({
      text: 'none',
      reason: 'this browser will not give the page a clipboard',
    })
  })

  // A DOMException with an empty message, and a rejection with a non-Error,
  // both reach the user as a sentence rather than as "" or "undefined".
  it.each([
    [new Error(''), 'Error'],
    ['NotAllowedError', 'NotAllowedError'],
    [undefined, 'undefined'],
  ])('always produces a reason (%s)', async (thrown, want) => {
    const writeText = () => Promise.reject(thrown)
    const got = await writeTerminalClipboard('x', { writeText })
    expect(got?.reason).toBe(want)
  })

  // It is called from a WebSocket callback, where a rejected promise is an
  // unhandled rejection on a path that is allowed to fail.
  it('never rejects', async () => {
    const onRejected = vi.fn()
    await writeTerminalClipboard('x', {
      writeText: () => Promise.reject(new Error('nope')),
    }).catch(onRejected)
    expect(onRejected).not.toHaveBeenCalled()
  })
})
