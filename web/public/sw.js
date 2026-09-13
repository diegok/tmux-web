/**
 * The service worker: a navigation fallback, and nothing else.
 *
 * ## Why there is one at all
 *
 * v2 decided that "notification is a tab badge, not Web Push", and spelled that
 * out as "no push, no service worker, no subscription". That sentence rejected
 * a worker as a *notification mechanism*, and every reason around it -- an
 * endpoint, a subscription kept on the daemon, a permission prompt, a VAPID key
 * pair -- is a reason about push. None of them is here. This worker owns one
 * page it writes itself, answers with it only when the network is not there,
 * and talks to nothing.
 *
 * ## The three rules, and what each one is protecting
 *
 * **Never `index.html`, never `/assets/*`.** The frontend is embedded in the Go
 * binary and the shell names content-hashed bundles, so a worker holding
 * yesterday's shell would ask a new daemon for bundles that no longer exist --
 * and the failure is invisible, because the page loads. `skipWaiting` and
 * `clients.claim` are the other half of the same rule: a replaced worker takes
 * over on the next load rather than after every tab has been closed.
 *
 * **Never `/api/*`, never `/ws`.** A cached snapshot is worse than no snapshot,
 * and there is nothing to cache about a socket.
 *
 * **Never `/enroll`.** It is public, it is rendered by the daemon rather than
 * by the SPA, and it reads its one-time token out of `location.hash`. A cached
 * page served there would be an enrollment page belonging to nobody's link.
 *
 * `/assets/` is behind the device cookie, which is the sharper reason the cache
 * holds no bundle: a Cache Storage entry survives a revoke, and revocation in
 * this app severs live connections rather than merely failing the next request.
 * The shell is not the data, so the hole would be small -- but it would be a
 * hole in a property stated without qualification, and one page this worker
 * generates itself has no such edge at all.
 *
 * The fallback is on *failure*, not on error status. A revoked device gets a
 * 401 from a daemon that is right there, and that 401 has to reach the app.
 */

/**
 * The one cache, versioned in its name so a worker that changes what it holds
 * starts from an empty store and sweeps what came before on activate.
 */
const CACHE = 'tmux-web-offline-v1'

/**
 * The only URL this worker ever caches. Nothing is served from this path by the
 * daemon; it is a name for the page below, chosen so that the entry is legible
 * in a browser's storage inspector.
 */
const OFFLINE_URL = '/offline.html'

/**
 * The offline page, entire. Inline CSS and inline SVG with no `src` and no
 * `href` anywhere in it, because every one of those would be a request from a
 * browser that has just been told the network is not there.
 */
const OFFLINE_HTML = `<!doctype html>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>tmux-web is not reachable</title>
<style>
  :root { color-scheme: light dark; --fg: #1c1917; --muted: #78716c; --bg: #fafaf9; --line: #e7e5e4 }
  @media (prefers-color-scheme: dark) {
    :root { --fg: #fafaf9; --muted: #a8a29e; --bg: #1c1917; --line: #44403c }
  }
  body { margin: 0; min-height: 100vh; display: grid; place-items: center; padding: 2rem;
         background: var(--bg); color: var(--fg);
         font: 16px/1.6 ui-sans-serif, system-ui, -apple-system, sans-serif }
  main { max-width: 30rem; text-align: center }
  svg { color: var(--muted) }
  h1 { font-size: 1.25rem; font-weight: 600; margin: 1rem 0 .5rem }
  p { color: var(--muted); margin: 0 0 1.5rem }
  button { font: inherit; color: inherit; background: none; cursor: pointer;
           border: 1px solid var(--line); border-radius: .5rem; padding: .45rem 1.1rem }
  button:hover { border-color: var(--muted) }
</style>
<main>
  <svg width="44" height="44" viewBox="0 0 24 24" fill="none" stroke="currentColor"
       stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
    <rect x="2" y="4" width="20" height="16" rx="2" />
    <path d="m6 9 3 3-3 3" />
    <path d="M13 15h4" />
    <path d="m2 2 20 20" />
  </svg>
  <h1>tmux-web is not reachable</h1>
  <p>This browser could not reach the daemon. Nothing is lost: the tmux server is
     on the other machine, with every pane exactly where you left it.</p>
  <button onclick="location.reload()">Try again</button>
</main>
`

/**
 * What the worker does with one request. Pure, so the rules are a table.
 *
 * The path tests come before the mode test on purpose: `/api/snapshot` typed
 * into an address bar is a navigation, and it must still be none of this
 * worker's business.
 *
 * Everything this worker sees is same-origin -- its scope is the origin root,
 * and a cross-origin subresource arrives with a mode that is not `navigate` --
 * so the origin is not part of the decision.
 *
 * @param {URL} url the request's URL
 * @param {'navigate' | 'other'} mode `navigate` for a page load, `other` for everything else
 * @returns {'fallback-on-failure' | 'bypass'} `bypass` means the worker does not answer at all
 */
function swRoute(url, mode) {
  const path = url.pathname

  // The shell, by the name it is stored under. The SPA is served from "/" and
  // never links to this, but a worker that would cache it if asked is a worker
  // one bookmark away from serving a stale app.
  if (path === '/index.html') return 'bypass'

  // Enrollment, exactly: a client-side route that merely starts with those
  // letters is the app, and the app gets the fallback.
  if (path === '/enroll' || path.startsWith('/enroll/')) return 'bypass'

  // The data and the socket.
  if (path === '/ws' || path.startsWith('/api/') || path.startsWith('/assets/')) return 'bypass'

  // What is left is the app, and the fallback for it is a page -- so only a
  // page load can be answered with one.
  if (mode !== 'navigate') return 'bypass'

  return 'fallback-on-failure'
}

self.addEventListener('install', (event) => {
  // Take over from the previous worker as soon as this one is installed. The
  // pair with clients.claim below is what keeps a deploy from being invisible.
  self.skipWaiting()
  event.waitUntil(
    caches.open(CACHE).then((cache) => cache.put(OFFLINE_URL, offlinePage())),
  )
})

self.addEventListener('activate', (event) => {
  event.waitUntil(
    (async () => {
      // Only this app's own caches, matched by prefix. Deleting by "not mine"
      // would be this worker taking a view on storage it did not write.
      for (const name of await caches.keys()) {
        if (name.startsWith('tmux-web-') && name !== CACHE) await caches.delete(name)
      }
      await self.clients.claim()
    })(),
  )
})

self.addEventListener('fetch', (event) => {
  const request = event.request
  const mode = request.mode === 'navigate' ? 'navigate' : 'other'
  // No respondWith: the browser then does exactly what it would have done with
  // no worker installed, which is the whole of "bypass".
  if (swRoute(new URL(request.url), mode) === 'bypass') return
  event.respondWith(networkOrOfflinePage(request))
})

/** The offline page as a response, built here and never fetched. */
function offlinePage() {
  return new Response(OFFLINE_HTML, {
    headers: {
      'Content-Type': 'text/html; charset=utf-8',
      // It is served from a cache the worker controls; an HTTP cache holding a
      // second copy of it would be a copy no deploy can reach.
      'Cache-Control': 'no-store',
    },
  })
}

/**
 * The network, and the offline page only if the network is not there at all.
 *
 * A rejected fetch is a transport failure -- no route, no listener, TLS refused.
 * Every answer the daemon actually gives, 401 and 404 included, is returned
 * untouched.
 */
async function networkOrOfflinePage(request) {
  try {
    return await fetch(request)
  } catch {
    const cache = await caches.open(CACHE)
    const page = await cache.match(OFFLINE_URL)
    // The fallback of the fallback: an install whose precache did not finish
    // should look like the network error it is, not like a worker bug.
    return page ?? Response.error()
  }
}
