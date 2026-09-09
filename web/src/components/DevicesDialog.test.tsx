/**
 * The devices API as this app speaks it, the rules around a minted link, and
 * the two pieces of the dialog that can be rendered on their own.
 *
 * The wire shape is implemented twice, in two languages, so the field names and
 * the enrollment TTL are pinned against the Go source rather than against a
 * copy of themselves -- the same approach useSnapshot.test.ts takes. A Go-side
 * rename then fails here instead of rendering `undefined` in a browser.
 */

import { readFileSync } from 'node:fs'

import { renderToStaticMarkup } from 'react-dom/server'
import { describe, expect, it, vi } from 'vitest'

import {
  DEVICES_URL,
  DeviceApiError,
  DeviceRow,
  ENROLL_TTL_MS,
  MintedPanel,
  fetchDevices,
  formatLastSeen,
  linkLife,
  mintLink,
  parseDevices,
  qrDataUrl,
  revokeAndPrune,
  revokeDevice,
  withoutDevice,
} from './DevicesDialog'
import type { DeviceInfo } from './DevicesDialog'

const goSource = (path: string) => readFileSync(new URL(`../../../${path}`, import.meta.url), 'utf8')

function device(over: Partial<DeviceInfo> = {}): DeviceInfo {
  return {
    id: 'dev-1',
    name: 'laptop',
    user_agent: 'Mozilla/5.0',
    created_at: '2026-09-01T10:00:00Z',
    last_seen: '2026-09-09T10:00:00Z',
    current: false,
    ...over,
  }
}

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

// --- the contract with the daemon -------------------------------------------

describe('contract with the daemon', () => {
  it('calls the routes the daemon registers', () => {
    const server = goSource('internal/front/server.go')
    expect(server).toContain(`"GET ${DEVICES_URL}"`)
    expect(server).toContain(`"POST ${DEVICES_URL}"`)
    expect(server).toContain(`"DELETE ${DEVICES_URL}/{id}"`)
  })

  it('uses the json names deviceJSON marshals', () => {
    const struct = goSource('internal/front/server.go').match(
      /type deviceJSON struct \{([\s\S]*?)\n\}/,
    )
    if (!struct) throw new Error('type deviceJSON not found in internal/front/server.go')
    const tags = [...struct[1].matchAll(/json:"([^",]+)/g)].map((m) => m[1])
    expect(tags).toHaveLength(6)
    expect(Object.keys(device()).sort()).toEqual(tags.sort())
  })

  it('counts a link down against the TTL the daemon enforces', () => {
    const m = goSource('internal/auth/enroll.go').match(
      /EnrollTTL\s*=\s*(\d+)\s*\*\s*time\.Minute/,
    )
    if (!m) throw new Error('EnrollTTL not found in internal/auth/enroll.go')
    // A countdown that disagrees with the daemon tells the user their link is
    // still good when the daemon has already dropped it.
    expect(ENROLL_TTL_MS).toBe(Number(m[1]) * 60_000)
  })
})

// --- parseDevices -----------------------------------------------------------

describe('parseDevices', () => {
  it('reads the list', () => {
    const parsed = parseDevices({ devices: [{ ...device(), current: true }] })
    expect(parsed).toEqual([device({ current: true })])
  })

  it('reads a null list as no devices rather than an error', () => {
    expect(parseDevices({ devices: null })).toEqual([])
    expect(parseDevices({})).toEqual([])
  })

  it('refuses a body where the list belongs', () => {
    expect(() => parseDevices({ devices: 'nope' })).toThrow(DeviceApiError)
    expect(() => parseDevices(null)).toThrow(DeviceApiError)
  })

  it('drops a row with no id, because its revoke button could not work', () => {
    expect(parseDevices({ devices: [{ name: 'ghost' }, null, device()] })).toEqual([device()])
  })

  it('falls back to the id when a device has no name', () => {
    expect(parseDevices({ devices: [{ id: 'dev-9' }] })[0].name).toBe('dev-9')
  })
})

// --- requests ---------------------------------------------------------------

describe('fetchDevices', () => {
  it('sends the cookie and asks for json', async () => {
    const fetchImpl = vi.fn(async (_url: string, _init: RequestInit) => response(200, { devices: [device()] }))
    await fetchDevices(undefined, fetchImpl)
    expect(fetchImpl.mock.calls[0][0]).toBe(DEVICES_URL)
    expect(fetchImpl.mock.calls[0][1].credentials).toBe('same-origin')
  })

  it('reports the sentence the daemon sent', async () => {
    const fetchImpl = vi.fn(async (_url: string, _init: RequestInit) => response(503, { error: 'cannot read the store' }))
    await expect(fetchDevices(undefined, fetchImpl)).rejects.toThrow('cannot read the store')
  })

  it('survives a body that is not json', async () => {
    const fetchImpl = vi.fn(async (_url: string, _init: RequestInit) => response(401, null, false))
    await expect(fetchDevices(undefined, fetchImpl)).rejects.toThrow('(401)')
  })
})

describe('mintLink', () => {
  it('posts the name and returns the link with the moment it was minted', async () => {
    const fetchImpl = vi.fn(async (_url: string, _init: RequestInit) =>
      response(200, { name: 'phone', url: 'https://box/enroll#tok' }),
    )
    const minted = await mintLink('phone', fetchImpl, () => 1_000)
    const [url, init] = fetchImpl.mock.calls[0]
    expect(url).toBe(DEVICES_URL)
    expect(init.method).toBe('POST')
    expect(JSON.parse(init.body as string)).toEqual({ name: 'phone' })
    // No CSRF token: the daemon checks Origin, which the browser sets on this
    // request and a page on another host cannot forge.
    expect(init.credentials).toBe('same-origin')
    expect(minted).toEqual({ name: 'phone', url: 'https://box/enroll#tok', mintedAt: 1_000 })
  })

  it('refuses a response with no link in it', async () => {
    const fetchImpl = vi.fn(async (_url: string, _init: RequestInit) => response(200, { name: 'phone' }))
    await expect(mintLink('phone', fetchImpl)).rejects.toThrow(/no url/)
  })

  it('reports a refusal', async () => {
    const fetchImpl = vi.fn(async (_url: string, _init: RequestInit) =>
      response(400, { error: 'a device needs a name you will recognise in the list' }),
    )
    await expect(mintLink('', fetchImpl)).rejects.toThrow(/needs a name/)
  })
})

describe('revokeDevice', () => {
  it('deletes the device by id', async () => {
    const fetchImpl = vi.fn(async (_url: string, _init: RequestInit) => response(204, null, false))
    await revokeDevice('dev 1/2', fetchImpl)
    const [url, init] = fetchImpl.mock.calls[0]
    expect(url).toBe(`${DEVICES_URL}/dev%201%2F2`)
    expect(init.method).toBe('DELETE')
  })

  it('treats a 401 as done, because revoking yourself kills your own cookie', async () => {
    const fetchImpl = vi.fn(async (_url: string, _init: RequestInit) => response(401, null, false))
    await expect(revokeDevice('me', fetchImpl)).resolves.toBeUndefined()
  })

  it('reports a 404 with its status, so a sign-out can tell it apart', async () => {
    const fetchImpl = vi.fn(async (_url: string, _init: RequestInit) => response(404, { error: 'no such device' }))
    await expect(revokeDevice('gone', fetchImpl)).rejects.toMatchObject({
      status: 404,
      message: 'no such device',
    })
  })
})

// --- the list ---------------------------------------------------------------

describe('withoutDevice', () => {
  it('takes the revoked row out and leaves the rest', () => {
    const list = [device({ id: 'a' }), device({ id: 'b' }), device({ id: 'c' })]
    expect(withoutDevice(list, 'b').map((d) => d.id)).toEqual(['a', 'c'])
  })

  it('is a copy, so React sees a new list', () => {
    const list = [device({ id: 'a' })]
    expect(withoutDevice(list, 'zzz')).not.toBe(list)
  })
})

describe('revokeAndPrune', () => {
  it('takes the row out once the daemon has actually revoked it', async () => {
    const fetchImpl = vi.fn(async (_url: string, _init: RequestInit) => response(204, null, false))
    const list = [device({ id: 'a' }), device({ id: 'b' })]
    expect((await revokeAndPrune('a', list, fetchImpl)).map((d) => d.id)).toEqual(['b'])
  })

  it('leaves the list alone when the revoke failed', async () => {
    // A row removed on a failed revoke tells the owner a lost laptop was cut
    // off while it still has a shell. The throw is what keeps the list intact.
    const fetchImpl = vi.fn(async (_url: string, _init: RequestInit) => response(500, { error: 'could not revoke that device' }))
    const list = [device({ id: 'a' })]
    await expect(revokeAndPrune('a', list, fetchImpl)).rejects.toThrow(/could not revoke/)
    expect(list.map((d) => d.id)).toEqual(['a'])
  })
})

describe('formatLastSeen', () => {
  const at = Date.parse('2026-09-09T12:00:00Z')

  it('reads Go zero time as never, not as the year 1', () => {
    // A device that enrolled and has not made a request since.
    expect(formatLastSeen('0001-01-01T00:00:00Z', at)).toBe('never')
    expect(formatLastSeen('', at)).toBe('never')
  })

  it('counts up in the unit that fits', () => {
    expect(formatLastSeen('2026-09-09T11:59:30Z', at)).toBe('just now')
    expect(formatLastSeen('2026-09-09T11:57:00Z', at)).toBe('3m ago')
    expect(formatLastSeen('2026-09-09T09:00:00Z', at)).toBe('3h ago')
    expect(formatLastSeen('2026-09-07T12:00:00Z', at)).toBe('2d ago')
  })
})

// --- the link's ten minutes -------------------------------------------------

describe('linkLife', () => {
  it('counts down from the TTL', () => {
    expect(linkLife(1_000, 1_000).label).toBe('10:00')
    expect(linkLife(1_000, 1_000 + 61_000).label).toBe('8:59')
  })

  it('expires exactly when the daemon stops accepting it', () => {
    expect(linkLife(0, ENROLL_TTL_MS - 1).expired).toBe(false)
    // One millisecond later the token is dead on the server, and a QR still on
    // screen is a thing to scan that cannot work.
    expect(linkLife(0, ENROLL_TTL_MS).expired).toBe(true)
    expect(linkLife(0, ENROLL_TTL_MS).label).toBe('expired')
  })
})

// --- the QR -----------------------------------------------------------------

describe('qrDataUrl', () => {
  it('encodes the link itself, and nothing else', async () => {
    const encode = vi.fn(async (_text: string, _options: { color: unknown }) => 'data:image/png;base64,fake')
    const url = 'https://box.example.com/enroll#Zm9vYmFyYmF6'
    await qrDataUrl(url, encode)
    // The fragment is the token. Encoding the base URL, the device name, or a
    // truncation of the link would produce a QR that scans and then fails, and
    // no test can read that back out of a PNG.
    expect(encode.mock.calls[0][0]).toBe(url)
  })

  it('draws black on white whatever the theme is', async () => {
    const encode = vi.fn(async (_text: string, _options: { color: unknown }) => '')
    await qrDataUrl('https://box/enroll#t', encode)
    // A scanner needs the contrast; an inverted QR is refused by a good half
    // of the phone cameras out there.
    expect(encode.mock.calls[0][1].color).toEqual({ dark: '#000000', light: '#ffffff' })
  })

  it('really encodes, and a different link is a different image', async () => {
    const one = await qrDataUrl('https://box/enroll#aaa')
    const two = await qrDataUrl('https://box/enroll#bbb')
    expect(one.startsWith('data:image/png;base64,')).toBe(true)
    expect(one).not.toBe(two)
  })
})

// --- what the dialog shows --------------------------------------------------

describe('<DeviceRow>', () => {
  function render(over: Partial<Parameters<typeof DeviceRow>[0]> = {}) {
    return renderToStaticMarkup(
      <DeviceRow
        device={device()}
        now={Date.parse('2026-09-09T12:00:00Z')}
        confirming={false}
        busy={false}
        onAsk={() => {}}
        onCancel={() => {}}
        onRevoke={() => {}}
        {...over}
      />,
    )
  }

  it('names the device, when it was last seen, and what it was', () => {
    const markup = render()
    expect(markup).toContain('laptop')
    expect(markup).toContain('last seen 2h ago')
    expect(markup).toContain('Mozilla/5.0')
  })

  it('marks this device', () => {
    expect(render({ device: device({ current: true }) })).toContain('this device')
    expect(render()).not.toContain('this device')
  })

  it('asks before it revokes', () => {
    // One click arms, the second one does it: revoking is not undoable from
    // the revoked device, and on a phone the row is a thumb wide.
    expect(render()).not.toContain('Cancel')
    expect(render({ confirming: true })).toContain('Cancel')
  })

  it('calls revoking this device what it is', () => {
    expect(render({ device: device({ current: true }), confirming: true })).toContain('Sign out')
  })
})

describe('<MintedPanel>', () => {
  const minted = { name: 'phone', url: 'https://box/enroll#tok', mintedAt: 0 }

  function render(qr: string | null) {
    return renderToStaticMarkup(
      <MintedPanel
        minted={minted}
        life={linkLife(0, 60_000)}
        qr={qr}
        onDone={() => {}}
      />,
    )
  }

  it('shows the link, the QR of it, and how long is left', () => {
    const markup = render('data:image/png;base64,fake')
    expect(markup).toContain('https://box/enroll#tok')
    expect(markup).toContain('src="data:image/png;base64,fake"')
    expect(markup).toContain('9:00')
  })

  it('says out loud that the link is a credential', () => {
    expect(render(null)).toMatch(/Anyone who sees it can\s+enroll/)
  })

  it('is still usable when the QR could not be drawn', () => {
    // The URL beside it is the fallback, so a failed encode is a missing
    // convenience rather than a dead dialog.
    expect(render(null)).toContain('https://box/enroll#tok')
  })
})
