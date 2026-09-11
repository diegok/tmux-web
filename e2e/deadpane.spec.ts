/**
 * What the app does when the pane it is pinned to dies under it.
 *
 * The rule -- which pane succeeds a dead one -- is a pure function and is
 * table-tested in `web/src/lib/useSnapshot.test.ts`. What cannot be tested
 * there is the wiring: `App.tsx` has no vitest coverage at all (see the header
 * of `manage.spec.ts`), and the effect under test needs a `TerminalHandle` ref,
 * a live socket, and a snapshot poll noticing the pane has gone. So this file
 * asserts the journey and reads tmux back rather than trusting the UI.
 *
 * ## Why every pane here runs a differently-named command
 *
 * The breadcrumb's last segment is the pane's command, and it is the only thing
 * that distinguishes "the app moved to the right pane" from "the app has not
 * caught up yet". Playwright retries an assertion until it passes, so a test
 * that killed a pane and immediately asserted `toContainText('1: api')` passes
 * on the *pre-kill* breadcrumb, on its first try, before anything has happened
 * -- and goes on passing against an app that never moves at all. Both tests
 * below were written that way first and were checked against a build with the
 * follow disabled; they passed. Naming the commands apart is what fixed them:
 * every assertion now names a string that cannot already be on screen.
 */
import { BASE_SESSION, breadcrumb, enroll, expect, test } from './harness'

test('a pane that dies hands the tab to its neighbour in the same window', async ({
  page,
  tmuxWeb,
}) => {
  await enroll(page, tmuxWeb, 'laptop')

  // Two windows, and the second has two panes. The tab has to be in a window
  // that is *not* the first, or "went back to the same window" and "fell back
  // to window 0" would be the same observation.
  tmuxWeb.tmux('new-window', '-d', '-t', BASE_SESSION, '-n', 'api', tmuxWeb.fakeAgent('survivor'))
  tmuxWeb.tmux('split-window', '-d', '-t', `${BASE_SESSION}:api`, tmuxWeb.fakeAgent('doomed'))
  const panes = () =>
    tmuxWeb.tmux('list-panes', '-t', `${BASE_SESSION}:api`, '-F', '#{pane_id}').split('\n')
  await expect.poll(panes).toHaveLength(2)
  const doomed = panes()[1]

  // Pin the tab to the pane that is about to die. Until something is selected
  // the tab has no pinned pane at all and the breadcrumb names only the
  // session, so without this click there is nothing to go stale.
  await page.getByRole('button', { name: /^1: api/ }).click()
  await page.getByRole('button', { name: /pane 1/ }).click()
  await expect(breadcrumb(page)).toContainText('doomed')

  tmuxWeb.tmux('kill-pane', '-t', doomed)
  await expect.poll(panes).toHaveLength(1)

  // The breadcrumb describes where the terminal is, and the terminal is in the
  // window the user was working in -- beside the pane that died, not back at
  // the top of the session.
  await expect(
    breadcrumb(page),
    'the tab never followed the dead pane to its neighbour',
  ).toContainText('survivor')
  await expect(breadcrumb(page)).toContainText('1: api')
  await expect(
    breadcrumb(page),
    'the breadcrumb still names the dead pane, so the app is pointing at a corpse ' +
      'while the terminal shows a live pane',
  ).not.toContainText('is gone')

  // And it says so, naming the pane that went. A silent move would leave the
  // user wondering what happened to their editor. Scoped to the toast region:
  // unscoped, this matches the breadcrumb of an app that did *not* move.
  await expect(page.locator('[data-sonner-toast]')).toContainText(`${doomed} is gone`)
})

/**
 * The other half of the rule: when the pane took its whole window with it there
 * is no neighbour, and the tab falls back to the session's first window.
 */
test('a pane that takes its window with it falls back to the first window', async ({
  page,
  tmuxWeb,
}) => {
  await enroll(page, tmuxWeb, 'laptop')

  tmuxWeb.tmux('new-window', '-d', '-t', BASE_SESSION, '-n', 'api', tmuxWeb.fakeAgent('doomed'))
  const doomed = tmuxWeb.tmux('list-panes', '-t', `${BASE_SESSION}:api`, '-F', '#{pane_id}')

  await page.getByRole('button', { name: /^1: api/ }).click()
  await expect(breadcrumb(page)).toContainText('doomed')

  tmuxWeb.tmux('kill-window', '-t', `${BASE_SESSION}:api`)

  await expect(breadcrumb(page), 'the tab never left the window that was killed').toContainText(
    '0: ',
  )
  await expect(breadcrumb(page)).not.toContainText('doomed')
  await expect(breadcrumb(page)).not.toContainText('is gone')
  await expect(page.locator('[data-sonner-toast]')).toContainText(`${doomed} is gone`)
})
