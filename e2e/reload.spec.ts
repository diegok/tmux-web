/**
 * What a page load costs the daemon, counted rather than argued.
 *
 * A reload used to open two terminal sockets. The tab remembered its session as
 * a group key, mounted the terminal on the key, and the first poll turned the
 * key into `$N` -- a different address, so `<Terminal>`'s effect tore the
 * socket down and opened another. Per load that was two `has-session` calls,
 * two `ptybridge.Open`s, and a `_web-*` tmux session created and immediately
 * destroyed, plus the remembered pane filed under two `sessionStorage` keys.
 *
 * Only a count can see it. Every behavioural assertion in this suite stays
 * green with the second socket back: both addresses name the same session, the
 * terminal ends up on the right pane, and the throwaway session is collected by
 * `destroy-unattached` before anything looks. So this test counts the tmux
 * sessions the server ever created -- ids are monotonic within one server's
 * life, so the highest one is a creation counter -- and the `sessionStorage`
 * keys the tab left behind.
 */

import { BASE_SESSION, breadcrumb, enroll, expect, test, windowRow } from './harness'
import type { TmuxWeb } from './harness'

/**
 * How many sessions this tmux server has ever created.
 *
 * `$N` is handed out in order and never reused while the server lives, so the
 * highest id counts creations including the ones already destroyed -- which is
 * exactly what a `_web-*` session opened and thrown away in the same second is.
 */
function sessionsEverCreated(tmuxWeb: TmuxWeb): number {
  const ids = tmuxWeb.tmux('list-sessions', '-F', '#{session_id}').split('\n')
  return Math.max(...ids.map((id) => Number(id.replace('$', '')))) + 1
}

test('a reload opens one terminal socket, not two', async ({ page, tmuxWeb }) => {
  tmuxWeb.tmux('new-window', '-t', BASE_SESSION, '-n', 'notes')

  await enroll(page, tmuxWeb, 'laptop')
  await expect(page.locator('.term-row').first()).toBeVisible()
  await expect(breadcrumb(page)).toContainText(BASE_SESSION)

  // Click into the second window, so the tab has a remembered pane that is not
  // the one it would land on by default. The reload has to bring it back.
  await windowRow(page, 'notes').click()
  await expect(breadcrumb(page)).toContainText('notes')

  // Settle first: the count below must not include a socket the *first* load
  // was still in the middle of opening.
  await page.waitForTimeout(3000)
  const before = sessionsEverCreated(tmuxWeb)

  await page.reload()
  await expect(page.locator('.term-row').first()).toBeVisible()
  // The remembered pane, restored by the first socket the reload opens. It is
  // filed per address, so a tab that mounts on the wrong address restores
  // nothing and shows the default pane until the poll rebuilds the terminal.
  await expect(breadcrumb(page)).toContainText('notes')

  // Two poll intervals: the second socket, when there was one, was opened by
  // the first poll that landed after the mount.
  await page.waitForTimeout(4000)
  await expect(breadcrumb(page)).toContainText('notes')
  expect(sessionsEverCreated(tmuxWeb) - before).toBe(1)

  // The other half of the same bug: the pane is filed under the address, so a
  // second address is a second key, and the one left behind is never read again.
  const paneKeys = await page.evaluate(() =>
    Object.keys(sessionStorage).filter((k) => k.startsWith('tmux-web:pane:')),
  )
  expect(paneKeys).toHaveLength(1)
})
