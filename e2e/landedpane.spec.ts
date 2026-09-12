/**
 * Which pane the tab says it is showing before anybody has clicked anything.
 *
 * A tab that has just loaded is attached and live on some pane. It used to have
 * no idea which one: `TerminalSession` wrote its `#pane` only from `select()`,
 * so the sidebar highlighted nothing and the breadcrumb was a bare session name
 * over a terminal full of somebody's work -- "veo algo que no sé lo que es".
 *
 * ## Why the snapshot cannot answer this, and why these fixtures look odd
 *
 * The obvious fix is to read it off the poll: the active pane of the active
 * window. It gives the wrong window. This app attaches each tab through a
 * throwaway session *grouped onto* the user's real one precisely so that the
 * two have independent current windows -- that is what lets a browser and the
 * terminal beside it look at different things -- and `#{window_active}` is
 * per-session. The snapshot is deduplicated to one row per pane preferring the
 * user's own session's copy, so what a tab would read there is the pane the
 * *local terminal* is on.
 *
 * So every test here parks the user's own session on a window the tab is not
 * on, and gives that window a differently-named command. An app reading the
 * snapshot passes nothing below; it names `elsewhere` in the breadcrumb and
 * highlights that row.
 *
 * The second oddity: the tab lands on the group's **first** window, not on the
 * base session's current one. Measured on tmux 3.7b -- `new-session -t base`
 * starts its session at the top of the shared window list, wherever `base` is
 * looking.
 *
 * ## What could make these pass by accident
 *
 * Everything asserted here has to name a string that could not already be on
 * screen, for the reason `deadpane.spec.ts` spells out at length: Playwright
 * retries, so "something is selected on load" is satisfied by the very state
 * this file exists to rule out being satisfied by. Each test therefore leads
 * with the *identity* of the expected pane -- a command name that appears on
 * exactly one row -- and only then counts highlights.
 */
import {
  BASE_SESSION,
  BASE_WINDOW,
  breadcrumb,
  enroll,
  expect,
  pill,
  selectedRow,
  stateDot,
  test,
  windowRow,
} from './harness'

test('a freshly loaded tab names the pane it landed on', async ({ page, tmuxWeb }) => {
  // Window 0 is split and its *second* pane is the active one, so "the pane the
  // tab landed on" is not "the first pane of the first window" either.
  tmuxWeb.tmux('split-window', '-d', '-t', `${BASE_SESSION}:${BASE_WINDOW}`, tmuxWeb.fakeAgent('landed'))
  tmuxWeb.tmux('select-pane', '-t', `${BASE_SESSION}:${BASE_WINDOW}.1`)

  // And the user's own session is looking at a different window entirely. Its
  // pane is the answer a snapshot-derived "active pane of the active window"
  // would give.
  tmuxWeb.tmux('new-window', '-d', '-t', BASE_SESSION, '-n', 'local', tmuxWeb.fakeAgent('elsewhere'))
  tmuxWeb.tmux('select-window', '-t', `${BASE_SESSION}:local`)

  await enroll(page, tmuxWeb, 'laptop')

  // Nothing has been clicked. `landed` is on exactly one row and in no
  // breadcrumb any earlier state could have produced -- before this worked, the
  // breadcrumb here read "e2e" and nothing else.
  await expect(
    breadcrumb(page),
    'a fresh tab does not say which pane it is showing: the user is looking at a ' +
      'live terminal with nothing anywhere naming it',
  ).toContainText('landed')
  await expect(breadcrumb(page)).toContainText(`0: ${BASE_WINDOW}`)
  await expect(
    breadcrumb(page),
    'the tab is describing the window the *local* terminal is on, not its own',
  ).not.toContainText('elsewhere')

  await expect(selectedRow(page), 'the sidebar highlights nothing on load').toContainText('landed')
  await expect(selectedRow(page)).toHaveCount(1)

  // And it is not a guess that happens to be right: tmux agrees that this is
  // the pane the tab's own session is on. `_web-` is the throwaway session this
  // browser tab attached through; there is exactly one, because there is one
  // tab.
  const tab = tmuxWeb
    .tmux('list-sessions', '-F', '#{session_name}')
    .split('\n')
    .find((name) => name.startsWith('_web-'))
  expect(tab, 'the browser tab has no tmux session').toBeDefined()
  const onScreen = tmuxWeb.tmux('list-panes', '-t', `=${tab}:`, '-F', '#{pane_id}', '-f', '#{pane_active}')
  expect(
    tmuxWeb.tmux('display-message', '-p', '-t', onScreen, '#{pane_current_command}'),
    'the pane the tab is actually attached to is not the one it named',
  ).toBe('landed')
})

/**
 * The remembered pane still wins -- and when it cannot, the tab says where it
 * really is instead of naming a corpse.
 *
 * `sessionStorage` keeps the pane a tab was on across a reload, and that should
 * beat wherever a new attach happens to land: coming back to where you were is
 * the better behaviour. But the pane can die while the tab is away, and then
 * the `select` that replays it fails, tmux leaves the tab where it attached,
 * and before this change the app went on highlighting nothing and printing
 * "%N is gone" over a live pane.
 */
test('a reloaded tab returns to its remembered pane, or to reality if it died', async ({
  page,
  tmuxWeb,
}) => {
  tmuxWeb.tmux('split-window', '-d', '-t', `${BASE_SESSION}:${BASE_WINDOW}`, tmuxWeb.fakeAgent('survivor'))
  tmuxWeb.tmux('select-pane', '-t', `${BASE_SESSION}:${BASE_WINDOW}.1`)
  tmuxWeb.tmux('new-window', '-d', '-t', BASE_SESSION, '-n', 'api', tmuxWeb.fakeAgent('doomed'))

  await enroll(page, tmuxWeb, 'laptop')

  // Put the tab on the pane in window 1, which is what gets remembered.
  await windowRow(page, 'api').click()
  await expect(breadcrumb(page)).toContainText('doomed')
  const doomed = tmuxWeb.tmux('list-panes', '-t', `${BASE_SESSION}:api`, '-F', '#{pane_id}')

  // First half: a reload comes back to it, rather than to wherever the new
  // attach landed. `survivor` is what landing looks like, so this fails loudly
  // if the remembered pane stopped winning.
  await page.reload()
  await page.locator('.term-row').first().waitFor()
  await pill(page).waitFor({ state: 'detached' })
  await expect(breadcrumb(page), 'the reload lost the pane the tab was on').toContainText('doomed')
  await expect(selectedRow(page)).toContainText('doomed')

  // Now the pane dies while the tab is not there to follow it. The tab has to
  // be off the page for that: with the app running, `succeedPane` moves it and
  // rewrites what is remembered, which is a different journey (deadpane.spec).
  await page.goto('about:blank')

  // A second tab, in the same context so it shares the cookie but not
  // `sessionStorage`, purely to watch the daemon. It is what makes the
  // assertion below sharp rather than a race: the snapshot is a 1.5s-old cache,
  // so a tab reloaded straight after the kill is handed a poll that still lists
  // the dead pane, `succeedPane` sees it die a moment later and moves the tab
  // by the pre-existing route -- and this test would then pass without the
  // socket ever being asked anything. Waiting for the row to leave *another*
  // tab's sidebar proves the cache no longer has it before this one loads.
  const watcher = await page.context().newPage()
  await watcher.goto(tmuxWeb.baseURL + '/')
  await watcher.locator('.term-row').first().waitFor()
  tmuxWeb.tmux('kill-window', '-t', `${BASE_SESSION}:api`)
  await expect(windowRow(watcher, 'api')).toHaveCount(0)
  await watcher.close()

  await page.goto(tmuxWeb.baseURL + '/')
  await page.locator('.term-row').first().waitFor()
  await pill(page).waitFor({ state: 'detached' })

  // The fixture, asserted rather than assumed: the tab really did come back
  // remembering the dead pane. Without this the test would pass for the
  // uninteresting reason that nothing was remembered at all.
  const remembered = await page.evaluate(() => ({ ...globalThis.sessionStorage }))
  expect(
    Object.values(remembered),
    `sessionStorage = ${JSON.stringify(remembered)}`,
  ).toContain(doomed)

  // And what it shows is where it is. `survivor` is in no row that was on
  // screen a moment ago and in no breadcrumb the previous state could produce.
  await expect(
    breadcrumb(page),
    'the tab is still naming the pane it remembered, which no longer exists, ' +
      'over a terminal showing a different one',
  ).toContainText('survivor')
  await expect(breadcrumb(page)).not.toContainText('is gone')
  await expect(selectedRow(page)).toContainText('survivor')
  await expect(selectedRow(page)).toHaveCount(1)

  // The remembered pane is left alone: what is stored is where the *user* put
  // themselves, and an observation does not overwrite it. One failed select per
  // reload is the price, and the answer above is what makes it harmless.
  expect(Object.values(await page.evaluate(() => ({ ...globalThis.sessionStorage })))).toContain(
    doomed,
  )
})

/**
 * The other thing `activePane` drives: the "finished" badge.
 *
 * Looking at a pane is what marks its finished run seen, and this tab is
 * looking at the pane it landed on -- so the badge for that pane must not be
 * lit. Before this change nothing was being looked at as far as the app knew,
 * and an agent finishing in the pane on screen badged the tab and the row while
 * the user was reading the very output it was badging.
 *
 * The state dot is the assertion, and `done` vs `idle` is the whole difference:
 * `done` is the daemon's idle refined by this device's memory, so a pane can
 * only read `done` if a working -> idle edge was stamped *and* this device has
 * not been shown it.
 */
test('the pane a fresh tab landed on is not badged as an unseen finish', async ({
  page,
  tmuxWeb,
}) => {
  // The landing pane is the agent. It is in window 0 because that is where a
  // new tab lands, and it is the active pane there.
  tmuxWeb.tmux('split-window', '-d', '-t', `${BASE_SESSION}:${BASE_WINDOW}`, tmuxWeb.fakeAgent('claude'))
  tmuxWeb.tmux('select-pane', '-t', `${BASE_SESSION}:${BASE_WINDOW}.1`)

  await enroll(page, tmuxWeb, 'laptop')

  const row = selectedRow(page)
  await expect(row, 'the tab never worked out which pane it landed on').toContainText('claude')

  // A run: the screen moves, then stops. Only a run that was seen to change
  // earns a finish edge, so the churn is what makes the badge possible at all.
  await expect(stateDot(row)).toHaveAttribute('data-agent-state', 'idle')
  const target = `${BASE_SESSION}:${BASE_WINDOW}.1`
  let n = 0
  const timer = setInterval(() => {
    try {
      tmuxWeb.tmux('send-keys', '-t', target, `tick-${n++}`, 'Enter')
    } catch {
      // A teardown race, not a failure; the assertions say whether it mattered.
    }
  }, 300)
  try {
    await expect(stateDot(row)).toHaveAttribute('data-agent-state', 'working')
  } finally {
    clearInterval(timer)
  }

  // Settled, and *not* badged: the finish happened on the pane this tab is
  // showing. A tab that did not know where it was reads `done` here, badges its
  // title, and tells the user about output they are looking straight at.
  await expect(
    stateDot(row),
    'the app badged a finish on the pane the user is reading',
  ).toHaveAttribute('data-agent-state', 'idle')
  await expect(page).toHaveTitle('tmux-web')

  // The control, in the same run: the same finish on a pane nobody is looking
  // at does light up. Without this the assertion above would also pass against
  // an app whose badge never works at all.
  tmuxWeb.tmux('new-window', '-d', '-t', BASE_SESSION, '-n', 'other', tmuxWeb.fakeAgent('claude'))
  const other = windowRow(page, 'other')
  await expect(stateDot(other)).toHaveAttribute('data-agent-state', 'idle')
  let m = 0
  const timer2 = setInterval(() => {
    try {
      tmuxWeb.tmux('send-keys', '-t', `${BASE_SESSION}:other`, `tick-${m++}`, 'Enter')
    } catch {
      /* see above */
    }
  }, 300)
  try {
    await expect(stateDot(other)).toHaveAttribute('data-agent-state', 'working')
  } finally {
    clearInterval(timer2)
  }
  await expect(stateDot(other)).toHaveAttribute('data-agent-state', 'done')
  await expect(page).toHaveTitle('(1) tmux-web')
})
