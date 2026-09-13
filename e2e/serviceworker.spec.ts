/**
 * The service worker, in a real browser against a real daemon.
 *
 * Its routing table is a pure function with a suite of its own in
 * `web/src/lib/registerSW.test.ts`, which evaluates the shipped `sw.js` rather
 * than a copy of it. What only a browser can show is the wiring around that
 * table: that the app registers the worker at all, that the daemon serves
 * `/sw.js` out of the embedded frontend, that the store really does end up
 * holding one page and not a bundle -- and that `/enroll` is left to the
 * browser even when there is no network to reach it with, which is the one
 * question the design left open for a real install to answer.
 */

import { enroll, expect, test } from './harness'
import type { Page } from '@playwright/test'

/** Every URL in Cache Storage, from the page's own origin. */
async function cachedURLs(page: Page): Promise<string[]> {
  return await page.evaluate(async () => {
    await navigator.serviceWorker.ready
    const urls: string[] = []
    for (const name of await caches.keys()) {
      for (const request of await (await caches.open(name)).keys()) urls.push(request.url)
    }
    return urls.sort()
  })
}

test('the worker installs, and what it holds is one page it wrote itself', async ({
  page,
  tmuxWeb,
}) => {
  await enroll(page, tmuxWeb, 'laptop')

  const script = await page.evaluate(async () => {
    const registration = await navigator.serviceWorker.ready
    return registration.active?.scriptURL ?? ''
  })
  expect(script).toBe(`${tmuxWeb.baseURL}/sw.js`)

  // Exactly one entry, by count and by name. The shell and the bundles are the
  // things that must not be here: a cached shell survives a redeploy and a
  // cached bundle survives a revoke, and both are invisible when they bite.
  expect(await cachedURLs(page)).toEqual([`${tmuxWeb.baseURL}/offline.html`])
})

test('with no network the app gets the offline page, and /enroll does not', async ({
  page,
  tmuxWeb,
}) => {
  await enroll(page, tmuxWeb, 'laptop')
  await page.evaluate(() => navigator.serviceWorker.ready)

  await page.context().setOffline(true)

  // The app's own route: the worker answers, because the network did not.
  await page.goto(`${tmuxWeb.baseURL}/`)
  await expect(page.getByRole('heading')).toHaveText('tmux-web is not reachable')

  // Enrollment: the worker stands aside, so this is the browser's own network
  // error rather than a page that would read a token out of a fragment it has
  // never seen. A rejected navigation is the whole assertion -- had the worker
  // answered, `goto` would have resolved with a 200.
  await expect(page.goto(`${tmuxWeb.baseURL}/enroll#Ck9tR2p`)).rejects.toThrow(/net::ERR_/)

  await page.context().setOffline(false)
})
