/**
 * The service worker's routing table, and the one call that installs it.
 *
 * `web/public/sw.js` is shipped to the browser verbatim -- it is not built, not
 * bundled and not imported by anything in `src`, because a worker has to sit at
 * the origin root under a stable name to claim the whole scope. So this suite
 * reads that file off disk and evaluates it in a `node:vm` context with a
 * stubbed worker global, the same way the wire-contract tests read the Go
 * source: the rules asserted here are the rules that ship, rather than a second
 * copy of them that can drift.
 *
 * The routing decision is a pure function inside that file, which is what makes
 * the table below a table. Everything else -- `skipWaiting`, `clients.claim` --
 * is wiring the stub can watch but not really test; see the note on those two.
 */

import { readFileSync } from 'node:fs'
import { createContext, runInContext } from 'node:vm'

import { describe, expect, it } from 'vitest'

import { registerServiceWorker } from './registerSW'

const source = readFileSync(new URL('../../public/sw.js', import.meta.url), 'utf8')

/** The origin the stubbed worker believes it is installed on. */
const ORIGIN = 'https://tmux.example.com'

type Mode = 'navigate' | 'other'
type Route = 'bypass' | 'fallback-on-failure'

/** What a `FetchEvent` carries that this worker reads. */
interface StubRequest {
  url: string
  mode: string
  method: string
}

/** One `Cache`, as a map from request URL to the response stored under it. */
class StubCache {
  readonly entries = new Map<string, Response>()

  put(request: string | StubRequest, response: Response): Promise<void> {
    this.entries.set(typeof request === 'string' ? new URL(request, ORIGIN).href : request.url, response)
    return Promise.resolve()
  }

  match(request: string | StubRequest): Promise<Response | undefined> {
    const key = typeof request === 'string' ? new URL(request, ORIGIN).href : request.url
    return Promise.resolve(this.entries.get(key))
  }
}

/** `caches`, remembering what was opened and what was deleted. */
class StubCacheStorage {
  readonly opened = new Map<string, StubCache>()

  constructor(preexisting: string[]) {
    for (const name of preexisting) this.opened.set(name, new StubCache())
  }

  open(name: string): Promise<StubCache> {
    const existing = this.opened.get(name)
    if (existing !== undefined) return Promise.resolve(existing)
    const fresh = new StubCache()
    this.opened.set(name, fresh)
    return Promise.resolve(fresh)
  }

  keys(): Promise<string[]> {
    return Promise.resolve([...this.opened.keys()])
  }

  delete(name: string): Promise<boolean> {
    return Promise.resolve(this.opened.delete(name))
  }
}

interface Worker {
  swRoute(url: URL, mode: Mode): Route
  /** Run the install handler to completion, precache included. */
  install(): Promise<void>
  /** Run the activate handler to completion. */
  activate(): Promise<void>
  /**
   * Hand the fetch handler one request. `null` means the worker did not call
   * `respondWith` at all, which is a bypass: the browser does what it would
   * have done with no worker installed.
   */
  request(path: string, mode?: Mode): Promise<Response | null>
  /** What the network answers next. Throwing is "the network is not there". */
  network: (request: StubRequest) => Promise<Response>
  /** Every URL the worker asked the network for. */
  readonly asked: string[]
  readonly caches: StubCacheStorage
  readonly skipWaiting: () => number
  readonly claimed: () => number
}

/** Evaluate `sw.js` against a stubbed worker global. */
function loadWorker(preexistingCaches: string[] = []): Worker {
  const listeners = new Map<string, (event: unknown) => void>()
  const storage = new StubCacheStorage(preexistingCaches)
  const asked: string[] = []
  let skipWaitingCalls = 0
  let claimCalls = 0

  const worker: Worker = {
    swRoute: () => 'bypass',
    install: () => fire('install'),
    activate: () => fire('activate'),
    request,
    network: () => Promise.reject(new TypeError('Failed to fetch')),
    asked,
    caches: storage,
    skipWaiting: () => skipWaitingCalls,
    claimed: () => claimCalls,
  }

  const self = {
    addEventListener(type: string, fn: (event: unknown) => void) {
      listeners.set(type, fn)
    },
    skipWaiting() {
      skipWaitingCalls += 1
    },
    clients: {
      claim() {
        claimCalls += 1
        return Promise.resolve()
      },
    },
    location: new URL(ORIGIN + '/sw.js'),
  }

  const context = createContext({
    self,
    caches: storage,
    console,
    URL,
    Response,
    fetch(request: StubRequest) {
      asked.push(request.url)
      return worker.network(request)
    },
  })

  // The trailing expression runs in the script's own scope, so it can see
  // top-level `const` bindings that never reach the context's globals. That is
  // what lets sw.js stay a plain worker script with nothing exported for the
  // benefit of a test.
  const exported = runInContext(source + '\n;({ swRoute })', context) as {
    swRoute: (url: URL, mode: Mode) => Route
  }
  worker.swRoute = exported.swRoute

  async function fire(type: string): Promise<void> {
    const waits: unknown[] = []
    listeners.get(type)?.({ waitUntil: (p: unknown) => waits.push(p) })
    await Promise.all(waits)
  }

  async function request(path: string, mode: Mode = 'navigate'): Promise<Response | null> {
    let answered: Promise<Response> | null = null
    listeners.get('fetch')?.({
      request: {
        url: new URL(path, ORIGIN).href,
        // A navigation is the only mode this worker treats specially; `cors`
        // stands in for every subresource fetch that is not one.
        mode: mode === 'navigate' ? 'navigate' : 'cors',
        method: 'GET',
      },
      respondWith(p: Promise<Response>) {
        answered = p
      },
    })
    return answered === null ? null : await answered
  }

  return worker
}

describe('swRoute', () => {
  const rows: [path: string, mode: Mode, want: Route][] = [
    // The app itself, and any client-side route under it.
    ['/', 'navigate', 'fallback-on-failure'],
    ['/anything/else', 'navigate', 'fallback-on-failure'],

    // The enrollment page. It is public, it is served by the daemon rather
    // than by the SPA, and it reads its token out of location.hash -- so a
    // cached page here would be a page with nobody's token in it.
    ['/enroll', 'navigate', 'bypass'],
    ['/enroll#Ck9tR2p', 'navigate', 'bypass'],
    ['/enroll', 'other', 'bypass'],

    // The data. A cached snapshot is worse than no snapshot.
    ['/api/snapshot', 'navigate', 'bypass'],
    ['/api/snapshot', 'other', 'bypass'],
    ['/api/panes/%250/capture?lines=200', 'other', 'bypass'],
    ['/ws', 'navigate', 'bypass'],
    ['/ws', 'other', 'bypass'],

    // The bundles, which are behind the device cookie, and the shell, which
    // names them. Either one cached is a stale app against a new daemon.
    ['/assets/index-abc123.js', 'other', 'bypass'],
    ['/assets/index-abc123.js', 'navigate', 'bypass'],
    ['/index.html', 'navigate', 'bypass'],
    ['/index.html', 'other', 'bypass'],

    // Not a navigation, so not this worker's business however ordinary the
    // path looks. The fallback is a page, and only a page load can show one.
    ['/', 'other', 'bypass'],
    ['/favicon.svg', 'other', 'bypass'],

    // The exclusion is the exact path and what is under it, not a prefix
    // match: a client-side route that merely begins with those letters is the
    // app, and the app gets the fallback.
    ['/enrollment-status', 'navigate', 'fallback-on-failure'],
  ]

  const worker = loadWorker()
  for (const [path, mode, want] of rows) {
    it(`${mode} ${path} -> ${want}`, () => {
      expect(worker.swRoute(new URL(path, ORIGIN), mode)).toBe(want)
    })
  }
})

describe('what the worker caches', () => {
  it('holds exactly one entry after installing, and it is the offline page', async () => {
    const worker = loadWorker()
    await worker.install()

    const entries = [...worker.caches.opened.values()].flatMap((c) => [...c.entries.keys()])
    expect(entries).toEqual([`${ORIGIN}/offline.html`])
  })

  it('builds the offline page itself rather than fetching anything', async () => {
    const worker = loadWorker()
    await worker.install()

    expect(worker.asked).toEqual([])
  })

  it('caches a page that loads nothing: no bundle, no font, no icon file', async () => {
    const worker = loadWorker()
    await worker.install()

    const [cache] = [...worker.caches.opened.values()]
    const page = cache.entries.get(`${ORIGIN}/offline.html`)
    expect(page?.headers.get('Content-Type')).toMatch(/^text\/html/)

    const html = await page!.text()
    // Any src= or href= at all is a request this page would make from a
    // browser that has just been told the network is not there.
    expect(html).not.toMatch(/\b(?:src|href)\s*=/)
    expect(html).not.toContain('/assets/')
  })

  it('deletes its own older caches on activate, and leaves anything else alone', async () => {
    const worker = loadWorker(['tmux-web-offline-v0', 'someone-elses-cache'])
    await worker.install()
    await worker.activate()

    const names = [...worker.caches.opened.keys()]
    expect(names).not.toContain('tmux-web-offline-v0')
    expect(names).toContain('someone-elses-cache')
  })
})

describe('the fetch handler', () => {
  it('serves the offline page when a navigation cannot reach the network', async () => {
    const worker = loadWorker()
    await worker.install()

    const response = await worker.request('/')
    expect(worker.asked).toEqual([`${ORIGIN}/`])
    expect(response?.status).toBe(200)
    expect(await response!.text()).toContain('tmux-web')
  })

  /**
   * The revoke case, and the reason the fallback is on failure rather than on
   * error. A revoked device gets a 401 from a daemon that is right there, and
   * the app has to see it: replacing it with a cached page would be a worker
   * hiding a credential decision behind an offline story.
   */
  it('passes a 401 straight through instead of covering it with a page', async () => {
    const worker = loadWorker()
    await worker.install()
    worker.network = () => Promise.resolve(new Response('unauthorized', { status: 401 }))

    const response = await worker.request('/')
    expect(response?.status).toBe(401)
  })

  it('does not touch /enroll, even with no network at all', async () => {
    const worker = loadWorker()
    await worker.install()

    expect(await worker.request('/enroll')).toBeNull()
    expect(worker.asked).toEqual([])
  })

  it('does not touch the API, even with no network at all', async () => {
    const worker = loadWorker()
    await worker.install()

    expect(await worker.request('/api/snapshot', 'other')).toBeNull()
    expect(worker.asked).toEqual([])
  })

  /**
   * Not really a test of behaviour: a stub can watch the two calls, but only a
   * real browser can show that they are what replaces a worker on the next
   * load rather than the one after it. The redeploy check in the commit message
   * is the evidence; this is here so that deleting the calls is at least loud.
   */
  it('claims the page it installed into, rather than waiting for every tab to close', async () => {
    const worker = loadWorker()
    await worker.install()
    await worker.activate()

    expect(worker.skipWaiting()).toBe(1)
    expect(worker.claimed()).toBe(1)
  })
})

describe('registerServiceWorker', () => {
  it('registers the worker at the origin root, so it can see every navigation', async () => {
    const registered: [string, RegistrationOptions | undefined][] = []
    await registerServiceWorker({
      register(url: string, options?: RegistrationOptions) {
        registered.push([url, options])
        return Promise.resolve({} as ServiceWorkerRegistration)
      },
    } as ServiceWorkerContainer)

    expect(registered).toEqual([['/sw.js', { scope: '/' }]])
  })

  it('does nothing at all where there is no service worker to register', async () => {
    await expect(registerServiceWorker(undefined)).resolves.toBeUndefined()
  })

  /**
   * A daemon reached over plain HTTP on a LAN address is not a secure context,
   * and `register` rejects there. The app works without the worker, so that
   * rejection is a fact about the deployment rather than an error, and it must
   * not surface as an unhandled rejection in the console of a working app.
   */
  it('swallows a registration that the browser refuses', async () => {
    await expect(
      registerServiceWorker({
        register: () => Promise.reject(new Error('not a secure context')),
      } as unknown as ServiceWorkerContainer),
    ).resolves.toBeUndefined()
  })
})
