/**
 * The capture endpoint as this app speaks it.
 *
 * The wire shape is implemented twice, in two languages, so the route and the
 * field names are pinned against the Go source rather than against a copy of
 * themselves -- the approach manage.test.ts and useSnapshot.test.ts already
 * take. A route renamed on the daemon fails here rather than in a browser.
 */

import { readFileSync } from 'node:fs'

import { describe, expect, it, vi } from 'vitest'

import { captureUrl, fetchCapture } from './capture'

const goSource = (path: string) => readFileSync(new URL(`../../../${path}`, import.meta.url), 'utf8')

function response(status: number, body: unknown, json = true): Response {
  return {
    ok: status >= 200 && status < 300,
    status,
    json: async () => {
      if (!json) throw new SyntaxError('not json')
      return body
    },
  } as unknown as Response
}

const capture = {
  paneId: '%3',
  text: 'a line\nanother line\n',
  lines: 1000,
  truncated: false,
  capturedAt: 1789075200000,
}

describe('contract with the daemon', () => {
  it('calls the route the daemon registers', () => {
    expect(goSource('internal/front/server.go')).toContain('"GET /api/panes/{id}/capture"')
  })

  it('reads the field names the handler writes', () => {
    const handler = goSource('internal/front/capture.go')
    for (const tag of ['paneId', 'text', 'lines', 'truncated', 'capturedAt']) {
      expect(handler).toContain(`json:"${tag}"`)
    }
  })

  it('leaves the default depth to the daemon, which is the only place it is written', () => {
    // This file deliberately has no default of its own: two would eventually
    // disagree, and the panel renders the depth the answer reports.
    expect(goSource('internal/front/capture.go')).toContain('defaultCaptureLines = 1000')
    expect(captureUrl('%3')).not.toContain('lines')
  })
})

describe('captureUrl', () => {
  it('percent-encodes the pane id, because % is not a legal path byte', () => {
    // Sent raw, "%3" is an invalid percent-escape and net/http answers 400
    // before the mux is consulted -- a failure that looks like a broken
    // endpoint rather than a broken caller.
    expect(captureUrl('%3')).toBe('/api/panes/%253/capture')
    expect(captureUrl('%3')).not.toBe('/api/panes/%3/capture')
  })

  it('sends no lines parameter when the caller names no depth', () => {
    expect(captureUrl('%3')).toBe('/api/panes/%253/capture')
  })

  it('sends the depth the caller named', () => {
    expect(captureUrl('%3', 2000)).toBe('/api/panes/%253/capture?lines=2000')
  })
})

describe('fetchCapture', () => {
  it('asks for the pane with the device cookie and reads back the capture', async () => {
    const fetchImpl = vi.fn(async (_url: string, _init: RequestInit) => response(200, capture))
    const result = await fetchCapture('%3', { fetchImpl })

    expect(fetchImpl.mock.calls[0][0]).toBe('/api/panes/%253/capture')
    const init = fetchImpl.mock.calls[0][1]
    expect(init.credentials).toBe('same-origin')
    expect(init.method).toBeUndefined() // a GET, and nothing else
    expect(result).toEqual({ ok: true, capture })
  })

  it('passes the depth and the abort signal through', async () => {
    const fetchImpl = vi.fn(async (_url: string, _init: RequestInit) => response(200, capture))
    const controller = new AbortController()
    await fetchCapture('%3', { lines: 500, signal: controller.signal, fetchImpl })

    expect(fetchImpl.mock.calls[0][0]).toBe('/api/panes/%253/capture?lines=500')
    expect(fetchImpl.mock.calls[0][1].signal).toBe(controller.signal)
  })

  it('reports the depth the daemon used rather than the one that was asked for', async () => {
    // 999999 is clamped by the daemon, and the answer says so. A panel that
    // rendered its own request back would claim a depth nobody captured.
    const fetchImpl = async () => response(200, { ...capture, lines: 5000 })
    const result = await fetchCapture('%3', { lines: 999999, fetchImpl })
    expect(result).toEqual({ ok: true, capture: { ...capture, lines: 5000 } })
  })

  it("quotes tmux's own words on a refusal", async () => {
    const fetchImpl = async () => response(400, { error: "can't find pane: %99" })
    expect(await fetchCapture('%99', { fetchImpl })).toEqual({
      ok: false,
      message: "can't find pane: %99",
    })
  })

  it('falls back to the status when the refusal is not JSON', async () => {
    // Protect answers 401 in plain text.
    const fetchImpl = async () => response(401, null, false)
    expect(await fetchCapture('%3', { fetchImpl })).toEqual({
      ok: false,
      message: 'the daemon answered 401',
    })
  })

  it('does not throw when the fetch itself fails', async () => {
    const fetchImpl = async () => {
      throw new Error('network down')
    }
    expect(await fetchCapture('%3', { fetchImpl })).toEqual({ ok: false, message: 'network down' })
  })

  it('refuses a 200 that is missing a field rather than inventing one', async () => {
    // truncated absent is not truncated false: it is a daemon this build does
    // not understand, and "nothing was dropped" is the one wrong answer.
    const { truncated: _dropped, ...withoutTruncated } = capture
    const fetchImpl = async () => response(200, withoutTruncated)
    const result = await fetchCapture('%3', { fetchImpl })
    expect(result.ok).toBe(false)
  })

  it('refuses a capturedAt that is not a number', async () => {
    const fetchImpl = async () => response(200, { ...capture, capturedAt: '1789075200000' })
    expect((await fetchCapture('%3', { fetchImpl })).ok).toBe(false)
  })
})
