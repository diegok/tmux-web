/**
 * Installing the service worker, which is one call and one thing to swallow.
 *
 * What the worker is and what it deliberately is not are documented where it
 * lives, in `web/public/sw.js`. It sits in `public/` rather than in `src/`
 * because a worker has to be served from the origin root under a stable name to
 * claim the whole scope, and Vite would otherwise give it a content hash and
 * put it under `/assets/` -- which is both the wrong path and, in this app, a
 * path behind the device cookie.
 */

/**
 * Register `/sw.js`, or do nothing where that is not possible.
 *
 * The container is a parameter so the decision is testable without a DOM; in
 * the app it is `navigator.serviceWorker`, which is `undefined` both in a
 * browser without support and in one that has it but is not in a secure
 * context.
 */
export function registerServiceWorker(
  container: ServiceWorkerContainer | undefined = globalThis.navigator?.serviceWorker,
): Promise<void> {
  if (container === undefined) return Promise.resolve()
  return container.register('/sw.js', { scope: '/' }).then(
    () => undefined,
    (err: unknown) => {
      // A daemon reached over plain HTTP on a LAN address is not a secure
      // context and `register` rejects there. The app is complete without the
      // worker -- it only ever supplies a page for a failed navigation -- so
      // this is a fact about the deployment, not an error, and it must not
      // reach the console as an unhandled rejection in an app that is working.
      console.warn('tmux-web: no service worker (the app works without it):', err)
    },
  )
}
