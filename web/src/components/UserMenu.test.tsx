/**
 * The footer's two lines, and the one operation in this app that cannot be
 * undone from the browser that ran it.
 *
 * Sign out is the interesting half. The design's rule is that it **revokes this
 * device** rather than clearing a cookie, so most of what follows is there to
 * fail if that request ever stops being a DELETE against this device's own id
 * -- a cookie-only logout would pass a naive "the user is signed out" test
 * while leaving a live, fully privileged credential in the store that no UI can
 * still name.
 */

import { readFileSync } from 'node:fs'

import { renderToStaticMarkup } from 'react-dom/server'
import { describe, expect, it, vi } from 'vitest'

import {
  IDENTITY_URL,
  UserMenu,
  IdentityLines,
  connectionLabel,
  fetchIdentity,
  identityLabel,
  parseIdentity,
  performSignOut,
} from './UserMenu'
import type { Identity } from './UserMenu'
import { SidebarProvider } from '@/components/ui/sidebar'
import { TooltipProvider } from '@/components/ui/tooltip'

const goSource = (path: string) => readFileSync(new URL(`../../../${path}`, import.meta.url), 'utf8')

const me: Identity = { user: 'diegok', host: 'devbox', device: 'laptop', deviceId: 'dev-1' }

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
  it('reads the identity route the daemon serves', () => {
    expect(goSource('internal/front/server.go')).toContain(`"GET ${IDENTITY_URL}"`)
  })

  it('reads the four fields the identity handler writes', () => {
    const handler = goSource('internal/front/server.go').match(
      /func \(s \*server\) identity\([\s\S]*?\n\}/,
    )
    if (!handler) throw new Error('identity handler not found in internal/front/server.go')
    for (const key of Object.keys(me)) {
      expect(handler[0]).toContain(`"${key}"`)
    }
  })
})

// --- reading who this is ----------------------------------------------------

describe('parseIdentity', () => {
  it('reads the four fields', () => {
    expect(parseIdentity({ ...me })).toEqual(me)
  })

  it('refuses a response that names no device', () => {
    // The device id is what Sign out revokes; without it the footer would show
    // a menu whose most important item cannot work.
    expect(() => parseIdentity({ user: 'diegok', host: 'devbox' })).toThrow(/named no device/)
    expect(() => parseIdentity(null)).toThrow()
  })
})

describe('fetchIdentity', () => {
  it('sends the device cookie', async () => {
    const fetchImpl = vi.fn(async (_url: string, _init: RequestInit) => response(200, me))
    expect(await fetchIdentity(undefined, fetchImpl)).toEqual(me)
    expect(fetchImpl.mock.calls[0][0]).toBe(IDENTITY_URL)
    expect(fetchImpl.mock.calls[0][1].credentials).toBe('same-origin')
  })

  it('fails loudly on a refusal', async () => {
    const fetchImpl = vi.fn(async (_url: string, _init: RequestInit) => response(401, null, false))
    await expect(fetchIdentity(undefined, fetchImpl)).rejects.toThrow('(401)')
  })
})

// --- the labels -------------------------------------------------------------

describe('identityLabel', () => {
  it('is user@host: which box am I driving', () => {
    expect(identityLabel(me)).toBe('diegok@devbox')
  })

  it('says something while the answer is in flight', () => {
    expect(identityLabel(null)).toBe('…')
  })
})

describe('connectionLabel', () => {
  it('turns the socket phase into a word', () => {
    expect(connectionLabel('ready')).toBe('connected')
    expect(connectionLabel('reconnecting')).toBe('reconnecting')
    expect(connectionLabel('ended')).toBe('session ended')
    expect(connectionLabel('closed')).toBe('no session')
    expect(connectionLabel(null)).toBe('connecting')
  })
})

// --- signing out ------------------------------------------------------------

describe('performSignOut', () => {
  it('revokes this device rather than clearing a cookie', async () => {
    const fetchImpl = vi.fn(async (_url: string, _init: RequestInit) => response(204, null, false))
    const reload = vi.fn()

    expect(await performSignOut('dev-1', { fetchImpl, reload })).toEqual({ ok: true })

    expect(fetchImpl).toHaveBeenCalledTimes(1)
    const [url, init] = fetchImpl.mock.calls[0]
    // The whole design decision, in two assertions: a logout that only dropped
    // the cookie would leave dev-1 enrolled forever, and unidentifiable.
    expect(url).toBe('/api/devices/dev-1')
    expect(init.method).toBe('DELETE')
    // The reload lands on the daemon's own "not enrolled" page, which is the
    // only thing in the system that tells a person how to get back in.
    expect(reload).toHaveBeenCalledTimes(1)
  })

  it('counts a device that is already gone as signed out', async () => {
    const fetchImpl = vi.fn(async (_url: string, _init: RequestInit) => response(404, { error: 'no such device' }))
    const reload = vi.fn()
    expect(await performSignOut('dev-1', { fetchImpl, reload })).toEqual({ ok: true })
    expect(reload).toHaveBeenCalledTimes(1)
  })

  it('counts a 401 as signed out: the cookie died with the device', async () => {
    // Revoking yourself clears your own cookie on the way back, and the
    // response can race that.
    const fetchImpl = vi.fn(async (_url: string, _init: RequestInit) => response(401, null, false))
    const reload = vi.fn()
    expect(await performSignOut('dev-1', { fetchImpl, reload })).toEqual({ ok: true })
    expect(reload).toHaveBeenCalledTimes(1)
  })

  it('does not reload when the revoke failed', async () => {
    const fetchImpl = vi.fn(async (_url: string, _init: RequestInit) => response(500, { error: 'could not revoke that device' }))
    const reload = vi.fn()

    const outcome = await performSignOut('dev-1', { fetchImpl, reload })

    expect(outcome.ok).toBe(false)
    expect(outcome.message).toContain('could not revoke that device')
    // Reloading here would show the "not enrolled" page to a browser that is
    // still, in fact, enrolled: a sign-out the user believes and the store
    // disagrees with.
    expect(reload).not.toHaveBeenCalled()
  })

  it('says so when the daemon never answers, instead of claiming success', async () => {
    // The realistic shape of this: revoking yourself severs this device's
    // sockets, and a response that never lands must not read as a sign-out.
    let fire: () => void = () => {}
    const fetchImpl = vi.fn(
      (_url: string, init: RequestInit) =>
        new Promise<Response>((_resolve, reject) => {
          init.signal?.addEventListener('abort', () => reject(new Error('aborted')))
        }),
    )
    const reload = vi.fn()

    const outcome = await performSignOut('dev-1', {
      fetchImpl,
      reload,
      setTimer: (fn) => {
        fire = fn
        // The timer is fired by hand below; nothing is scheduled.
        queueMicrotask(() => fire())
        return 0 as unknown as ReturnType<typeof setTimeout>
      },
      clearTimer: () => {},
    })

    expect(outcome.ok).toBe(false)
    expect(outcome.message).toContain('may still be enrolled')
    expect(reload).not.toHaveBeenCalled()
  })
})

// --- what the footer says ---------------------------------------------------

describe('<IdentityLines>', () => {
  function render(identity: Identity | null, connection: Parameters<typeof connectionLabel>[0]) {
    return renderToStaticMarkup(
      <SidebarProvider>
        <IdentityLines identity={identity} connection={connection} />
      </SidebarProvider>,
    )
  }

  it('is user@host over device and connection state', () => {
    const markup = render(me, 'ready')
    expect(markup).toContain('diegok@devbox')
    expect(markup).toContain('laptop · connected')
  })

  it('says the device is not typing anywhere when the socket is down', () => {
    expect(render(me, 'reconnecting')).toContain('laptop · reconnecting')
  })

  it('renders before the identity has arrived', () => {
    // The request is an effect, and the first paint happens without it.
    const markup = render(null, null)
    expect(markup).toContain('…')
    expect(markup).toContain('this device · connecting')
  })
})

describe('<UserMenu>', () => {
  it('renders the footer button the way AppSidebar mounts it', () => {
    const markup = renderToStaticMarkup(
      <SidebarProvider>
        <TooltipProvider>
          <UserMenu connection="ready" />
        </TooltipProvider>
      </SidebarProvider>,
    )
    // The menu itself is a portal and is not open, so the button and the two
    // lines are what a static render can see -- which is exactly what a user
    // sees before clicking.
    expect(markup).toContain('data-sidebar="menu-button"')
    expect(markup).toContain('this device · connected')
    expect(markup).not.toContain('Sign out')
  })

  it('needs a tooltip provider in scope, which is why it lives inside the sidebar', () => {
    // The collapsed rail turns this button into a bare avatar, so it carries a
    // `tooltip` -- and a Radix Tooltip with no provider above it throws during
    // render, taking the whole page with it rather than losing a tooltip. This
    // is the failure AppSidebar's own comment warns about; pinning it here
    // means moving the provider is a test failure and not a blank screen.
    expect(() =>
      renderToStaticMarkup(
        <SidebarProvider>
          <UserMenu connection="ready" />
        </SidebarProvider>,
      ),
    ).toThrow(/TooltipProvider/)
  })
})
