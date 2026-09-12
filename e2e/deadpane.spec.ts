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
import { BASE_SESSION, breadcrumb, enroll, expect, selectedRow, test } from './harness'
import type { Locator, Page } from '@playwright/test'

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

/**
 * ## The same two deaths, ordered from the sidebar
 *
 * The two tests above kill from tmux, which is the shape of a pane dying on its
 * own -- an agent exiting, a shell that got `exit` typed into it. The owner hit
 * this the other way: he right-clicked the row, armed the dialog and pressed
 * the red button. That is not the same code. It goes through `runManage` ->
 * `DELETE` -> `refresh()`, so the first snapshot without the pane arrives
 * sooner, and from a different call, than the poll the tests above wait out.
 *
 * They also assert something the two above do not. The breadcrumb is one
 * readout of `activePane`; the sidebar highlight is the other, and it is the
 * one a user reads to answer "which of these am I in". A row carries
 * `data-active` when it is the selected one, so **nothing is selected** is
 * `toHaveCount(0)` here -- a state no assertion about breadcrumb text can tell
 * apart from a breadcrumb that has simply not caught up.
 */

/** Open a row's context menu, over its text rather than its padding. */
async function rightClick(row: Locator, text: string): Promise<void> {
  await row.getByText(text, { exact: true }).click({ button: 'right' })
}

/** Arm the kill dialog and press the red button. */
async function confirmKill(page: Page, label: string): Promise<void> {
  const dialog = page.getByRole('dialog')
  await dialog.getByRole('checkbox').check()
  await dialog.getByRole('button', { name: label }).click()
}

test('killing the selected pane from the sidebar leaves the successor selected', async ({
  page,
  tmuxWeb,
}) => {
  await enroll(page, tmuxWeb, 'laptop')

  tmuxWeb.tmux('new-window', '-d', '-t', BASE_SESSION, '-n', 'api', tmuxWeb.fakeAgent('survivor'))
  tmuxWeb.tmux('split-window', '-d', '-t', `${BASE_SESSION}:api`, tmuxWeb.fakeAgent('doomed'))
  const panes = () =>
    tmuxWeb.tmux('list-panes', '-t', `${BASE_SESSION}:api`, '-F', '#{pane_id}').split('\n')
  await expect.poll(panes).toHaveLength(2)

  await page.getByRole('button', { name: /^1: api/ }).click()
  const doomedRow = page.getByRole('button', { name: /pane 1/ })
  await doomedRow.click()

  // The starting point, in the words the assertion after the kill uses: one row
  // selected, and it is the pane about to die. `survivor` is deliberately not on
  // this row, so nothing below can pass on what is already on screen.
  await expect(selectedRow(page)).toHaveCount(1)
  await expect(selectedRow(page)).toContainText('doomed')

  await rightClick(doomedRow, 'pane 1')
  await page.getByRole('menuitem', { name: 'Kill pane…' }).click()
  await confirmKill(page, 'Kill pane')

  await expect.poll(panes).toHaveLength(1)

  // Down to one pane, so the window row is where the highlight goes -- and that
  // row names the pane that is left.
  //
  // The text assertion comes first and carries the message, deliberately.
  // `toHaveCount(1)` is satisfied by the *pre-kill* highlight on its very first
  // try, so leading with it would report a pass before anything had happened --
  // the same trap the header of this file describes, in its other shape.
  // `survivor` appears on no row that was selected a moment ago, so this one
  // cannot be. It fails with "element(s) not found" when nothing is selected at
  // all, which is the bug being guarded.
  await expect(
    selectedRow(page),
    'the sidebar highlights nothing, or the wrong row: the user is looking at a pane ' +
      'with nothing saying which',
  ).toContainText('survivor')
  await expect(selectedRow(page)).toHaveCount(1)
})

/**
 * The other shape the owner named, "pane/tab": the whole window goes at once.
 *
 * The window has to be split, because a one-pane window row offers a *pane*
 * menu -- `rowTargetForWindow` -- so "Kill window…" is only reachable from a
 * window that has more than one pane. Both of its panes die together, which is
 * the case `succeedPane`'s first choice cannot answer: the window they were in
 * is gone too.
 */
test('killing the selected window from the sidebar leaves the successor selected', async ({
  page,
  tmuxWeb,
}) => {
  await enroll(page, tmuxWeb, 'laptop')

  tmuxWeb.tmux('new-window', '-d', '-t', BASE_SESSION, '-n', 'api', tmuxWeb.fakeAgent('editor'))
  tmuxWeb.tmux('split-window', '-d', '-t', `${BASE_SESSION}:api`, tmuxWeb.fakeAgent('agent'))
  const windows = () =>
    tmuxWeb.tmux('list-windows', '-t', BASE_SESSION, '-F', '#{window_name}').split('\n')
  await expect.poll(windows).toContain('api')

  const apiRow = page.getByRole('button', { name: /^1: api/ })
  await apiRow.click()
  await page.getByRole('button', { name: /pane 1/ }).click()
  await expect(selectedRow(page)).toHaveCount(1)
  await expect(selectedRow(page)).toContainText('agent')

  await rightClick(apiRow, '1: api')
  await page.getByRole('menuitem', { name: 'Kill window…' }).click()
  await confirmKill(page, 'Kill window')

  await expect.poll(windows).not.toContain('api')

  // Text first, count second -- see the note in the test above.
  await expect(
    selectedRow(page),
    'the sidebar highlights nothing after the window went',
  ).toContainText('0: shell')
  await expect(selectedRow(page)).toHaveCount(1)
})

/**
 * The one the owner actually hit, and the reason this file grew a second half.
 *
 * Closing the last tab of a session destroys the session. The tab does not sit
 * there disconnected: `resolveSession` moves it to another session -- the first
 * one in the sidebar -- and the terminal comes back on a live pane of that one.
 *
 * What used to happen then was the whole bug in one line: `<Terminal>` files its
 * remembered pane under the session *address*, so a new address means a new
 * `TerminalSession` with no remembered pane at all, and `status.pane` went null
 * at the same moment the old pane left the snapshot. Nothing was highlighted,
 * the breadcrumb had only a session name in it, and the successor effect --
 * guarded on `activePane` -- never ran to fill either of them in. The terminal
 * had moved and there was no indication anywhere of where to.
 *
 * The successor here has to be in another session, so it is deliberately not
 * anything that could be left on screen from before: `elsewhere` appears in no
 * row of the session being killed.
 */
test('killing the last tab of a session leaves the tab selected in the next one', async ({
  page,
  tmuxWeb,
}) => {
  // Created before the browser so the sidebar has it from the first poll.
  tmuxWeb.tmux('new-session', '-d', '-s', 'other', '-n', 'work', tmuxWeb.fakeAgent('elsewhere'))
  await enroll(page, tmuxWeb, 'laptop')

  const sessions = () => tmuxWeb.tmux('list-sessions', '-F', '#{session_name}').split('\n')
  await expect.poll(sessions).toContain(BASE_SESSION)

  // `e2e` has exactly one window with one pane, so killing that pane takes the
  // session -- and the group this tab is attached through -- with it.
  const row = page.getByRole('button', { name: /^0: shell/ })
  await row.click()
  await expect(selectedRow(page)).toHaveCount(1)
  await expect(selectedRow(page)).toContainText('0: shell')

  await rightClick(row, '0: shell')
  await page.getByRole('menuitem', { name: 'Kill pane…' }).click()
  await confirmKill(page, 'Kill pane')

  await expect.poll(sessions).not.toContain(BASE_SESSION)

  // The tab landed in `other`, and it says which pane of it. Text first, count
  // second -- see the note two tests up. Against the broken build this is what
  // reported the defect: `toHaveCount(1)` was still passing on the highlight
  // left over from the session that had just been destroyed.
  await expect(
    selectedRow(page),
    'the tab moved to another session and highlighted nothing: the user is looking ' +
      'at a live terminal with no row anywhere saying which pane it is',
  ).toContainText('elsewhere')
  await expect(selectedRow(page)).toHaveCount(1)
  await expect(breadcrumb(page)).toContainText('other')
  await expect(breadcrumb(page)).toContainText('elsewhere')
  // And the toast names the session, which is the part of "where am I" that
  // changed and the part nothing else would have told them.
  await expect(page.locator('[data-sonner-toast]')).toContainText('Moved to other › 0: work')
})

/**
 * The death that follows a click into *another session*.
 *
 * That click is not the same code path as the four above. It cannot select
 * anything yet -- the pane belongs to a window this tab's socket has no session
 * in -- so `App` re-attaches, holds the clicked pane as `pendingPane` to
 * acknowledge the click immediately, and replays the selection when the
 * replacement socket comes up. Everything downstream reads
 * `pendingPane ?? status.pane`, and the successor effect stands down while a
 * switch is in flight, so an override that is never dropped is invisible until
 * the moment this test exercises: the pane dies, and the tab that clicked it
 * from another session is the one tab that cannot follow.
 *
 * ## What each name in here rules out
 *
 * `crossed` is deliberately **not** where a re-attach lands on its own. It is
 * the second pane of the window and `split-window -d` leaves the first one
 * current, so a tab that switched sessions and never replayed the click ends up
 * on `successor` -- which is what the assertion right after the click is for.
 * Without that, an app that dropped the optimistic override on the way and
 * forgot to select anything would pass this test on tmux's own choice of pane.
 *
 * `successor` in turn is on no row that could be highlighted before the kill --
 * the highlight is on the pane row that is about to die -- so the leading
 * assertion after it cannot pass on the state the kill was supposed to change.
 */
test('a pane clicked from another session is still followed when it dies', async ({
  page,
  tmuxWeb,
}) => {
  // Created before the browser so the sidebar has it from the first poll. Two
  // panes, because the successor has to be a pane the tab was never on.
  tmuxWeb.tmux('new-session', '-d', '-s', 'other', '-n', 'remote', tmuxWeb.fakeAgent('successor'))
  tmuxWeb.tmux('split-window', '-d', '-t', 'other:remote', tmuxWeb.fakeAgent('crossed'))
  const panes = () =>
    tmuxWeb.tmux('list-panes', '-t', 'other:remote', '-F', '#{pane_id}').split('\n')
  await expect.poll(panes).toHaveLength(2)
  const crossed = panes()[1]

  await enroll(page, tmuxWeb, 'laptop')

  // The click that crosses the boundary: `other` is not the session this tab is
  // attached to, so this replaces the socket and replays the selection on the
  // new one. The pane row directly, and only it -- clicking the window row
  // first would be a cross-session click of its own, on the other pane.
  await page.getByRole('button', { name: /pane 1/ }).click()
  await expect(
    breadcrumb(page),
    'the click into another session never landed on the pane it named, so what ' +
      'follows tests nothing',
  ).toContainText('crossed')
  await expect(breadcrumb(page)).toContainText('other')
  await expect(selectedRow(page)).toContainText('crossed')
  await expect(selectedRow(page)).toHaveCount(1)

  tmuxWeb.tmux('kill-pane', '-t', crossed)
  await expect.poll(panes).toHaveLength(1)

  // Text first, count second -- see the note further up this file. Against the
  // unfixed app the highlight is on nothing at all: `activePane` is still
  // pinned to the pane that was clicked, so this fails with "element(s) not
  // found" rather than on the wrong row.
  await expect(
    selectedRow(page),
    'the tab that reached this pane from another session never followed it: the ' +
      'user is looking at a live pane with no row anywhere saying which',
  ).toContainText('successor')
  await expect(selectedRow(page)).toHaveCount(1)
  await expect(breadcrumb(page)).toContainText('successor')
  await expect(breadcrumb(page)).not.toContainText('is gone')
  await expect(page.locator('[data-sonner-toast]')).toContainText(`${crossed} is gone`)
})
