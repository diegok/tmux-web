/**
 * The journeys that only a real browser can prove.
 *
 * Everything below needs the daemon, a tmux server and a rendering engine at
 * the same time; anything that does not is a Go or vitest test, and is left
 * out on purpose. The selection is deliberately small -- see the notes on each
 * test for what it catches that a unit test cannot.
 *
 * The screen assertions are ordinary text assertions because wterm renders
 * rows as DOM nodes: `.term-row` per line inside `.term-grid`. A canvas
 * terminal would have made this file impossible to write, which is the payoff
 * for that choice in the design.
 *
 * Note on the plan's example: it uses `[data-wterm-row]` and
 * `[data-wterm-screen]`. `@wterm/dom` 0.5 emits neither -- its rows are
 * `.term-row` inside `.term-grid`, with no data attributes anywhere. The
 * selectors here are the ones that exist.
 */

import { BASE_SESSION, BASE_WINDOW, enroll, expect, focusTerminal, pill, test } from './harness'
import type { Page, Locator } from '@playwright/test'

/** `session › window › command`. */
function breadcrumb(page: Page): Locator {
  return page.getByRole('navigation', { name: 'Location' })
}

/** The sidebar row for a window, labelled `<index>: <name>`. */
function windowRow(page: Page, name: string): Locator {
  return page.getByRole('button', { name: new RegExp(`\\d+: ${name}`) })
}

test('enrolls a browser and round-trips a keystroke through real tmux', async ({ page, wterm }) => {
  // The core promise, and four things at once that no unit test covers: the
  // enroll page's inline script survives its own CSP nonce, a `__Host-` cookie
  // marked Secure is accepted by a browser over plain http on localhost (it is,
  // because localhost is a trustworthy origin -- but that is a browser rule, so
  // only a browser can say so), the SPA's assets load from behind the same
  // cookie, and a keystroke reaches a real pane.
  await enroll(page, wterm, 'laptop')

  await expect(page).toHaveURL(wterm.baseURL + '/')
  await expect(page.locator('.term-row').first()).toBeVisible()
  await expect(breadcrumb(page)).toContainText(BASE_SESSION)

  const cookies = await page.context().cookies()
  expect(cookies.map((c) => c.name)).toContain('__Host-wterm_device')

  // `printf` rather than the plan's `echo e2e-ok`: the shell echoes what is
  // typed, so `echo e2e-ok` puts the marker on screen whether or not anything
  // ran. This way the marker can only come from the pane's own output.
  await page.keyboard.type("printf 'e2e-%s\\n' ok\n")
  await expect(page.getByText('e2e-ok')).toBeVisible()

  // And the other direction: what tmux itself thinks is on that pane.
  await expect
    .poll(() => wterm.tmux('capture-pane', '-p', '-t', `${BASE_SESSION}:0`))
    .toContain('e2e-ok')
})

test('the sidebar follows real tmux, and clicking a window moves the terminal', async ({
  page,
  wterm,
}) => {
  // The sidebar is a poller over `tmux list-panes -a`, and the click path goes
  // through a *grouped* session: `select-window -t '=<throwaway>:@id'`. Both
  // ends of that are mocked in the vitest suite. This is the only test that
  // watches a window created outside the app appear in the tree and then moves
  // to it.
  await enroll(page, wterm, 'laptop')
  await expect(windowRow(page, 'second')).toHaveCount(0)

  // `-d`, so the base session stays on its own window. That is what makes the
  // last assertion in this test mean anything: without it tmux would have moved
  // `e2e` to the new window itself, and a click that moved nothing would pass.
  wterm.tmux('new-window', '-d', '-t', BASE_SESSION, '-n', 'second', 'sh')
  wterm.tmux('send-keys', '-t', `${BASE_SESSION}:second`, "printf 'in-the-second-window\\n'", 'Enter')

  // Within one poll interval, without a reload.
  await expect(windowRow(page, 'second')).toBeVisible()
  await expect(breadcrumb(page)).not.toContainText('second')

  await windowRow(page, 'second').click()

  await expect(breadcrumb(page)).toContainText('second')
  await expect(page.getByText('in-the-second-window').first()).toBeVisible()

  // The click must move *this tab's* grouped session and not the base session
  // the user is sitting in front of locally: that is the whole reason the
  // daemon attaches to a throwaway session per tab.
  const current = wterm.tmux('list-sessions', '-F', '#{session_name} #{window_name}').split('\n')
  expect(current).toContain(`${BASE_SESSION} ${BASE_WINDOW}`)
  expect(current.filter((s) => s.startsWith('_web-'))).toEqual([
    expect.stringMatching(/^_web-\w+ second$/),
  ])
})

test('revoking the device over the admin socket severs the live terminal', async ({
  page,
  wterm,
}) => {
  // The security property the whole design rests on. Unit tests cover the
  // registry and the admin mux separately; this is the only place where a real
  // `wterm-web revoke` on a real unix socket is observed to drop a WebSocket a
  // real browser is holding open.
  await enroll(page, wterm, 'lost-laptop')

  const device = wterm.devices().find((d) => d.name === 'lost-laptop')
  expect(device, `devices(): ${JSON.stringify(wterm.devices())}`).toBeDefined()

  wterm.revoke(device!.id)

  // The socket is cut; the tab retries, and every retry is refused, so it stays
  // in "reconnecting" rather than recovering.
  await expect(pill(page)).toContainText(/Reconnecting/)
  // Everything that needs a live socket goes with it.
  await expect(page.getByRole('button', { name: 'Copy mode' })).toBeDisabled()
  // And the poller stops for good rather than 401-ing every 1.5s forever.
  await expect(page.getByText('This device is no longer enrolled')).toBeVisible()

  // The failure branch of the palette's copy-mode command. It exists precisely
  // for this moment, and `react-dom/server` cannot reach it -- a static render
  // runs no handlers -- so it survived task 22's unit tests.
  await page.getByRole('button', { name: /Jump to/ }).click()
  await expect(page.locator('[data-slot="command-input"]')).toBeVisible()
  await page.getByRole('option', { name: /Enter copy mode/ }).click()
  await expect(page.getByRole('alert')).toContainText('copy mode did nothing')
  // Stays open: a palette that closed would look like a command that worked.
  await expect(page.locator('[data-slot="command-input"]')).toBeVisible()
})

test('a restarted daemon reconnects the tab to the same pane', async ({ page, wterm }) => {
  // "Kill the network, restore it: the terminal reconnects to the *same* pane."
  //
  // The network is killed by restarting the daemon rather than with
  // `context.setOffline(true)`, which does not work for this: Chromium's
  // offline emulation stops new requests -- the snapshot poll fails -- but
  // leaves an already-open WebSocket up, so the terminal never notices. A
  // daemon that goes away and comes back on the same port is what the browser
  // sees when a laptop sleeps, and it is what the tab has to survive.
  await enroll(page, wterm, 'laptop')

  wterm.tmux('new-window', '-d', '-t', BASE_SESSION, '-n', 'agent', 'sh')
  wterm.tmux('send-keys', '-t', `${BASE_SESSION}:agent`, "printf 'agent-pane-marker\\n'", 'Enter')
  await windowRow(page, 'agent').click()
  await expect(breadcrumb(page)).toContainText('agent')

  const before = (await breadcrumb(page).innerText()).replace(/\s+/g, ' ')

  await wterm.restart()
  await expect(pill(page)).toContainText(/Reconnecting/)
  await expect(pill(page)).toHaveCount(0)

  // Same pane, not window 0. The pane id is remembered in sessionStorage and
  // re-selected before input is re-enabled.
  await expect(breadcrumb(page)).toContainText('agent')
  expect((await breadcrumb(page).innerText()).replace(/\s+/g, ' ')).toBe(before)
  await expect(page.getByText('agent-pane-marker').first()).toBeVisible()

  // And the first keystroke after the blip lands in that pane, which is the
  // thing the ordering in `#opened` exists to guarantee. The click is not
  // ceremony: selecting a pane in the sidebar leaves focus on the sidebar
  // button, so without it this types into a button. See the note on
  // `focusTerminal`.
  await focusTerminal(page)
  await page.keyboard.type("printf 'after-%s\\n' reconnect\n")
  await expect
    .poll(() => wterm.tmux('capture-pane', '-p', '-t', `${BASE_SESSION}:agent`))
    .toContain('after-reconnect')
  expect(wterm.tmux('capture-pane', '-p', '-t', `${BASE_SESSION}:0`)).not.toContain(
    'after-reconnect',
  )
})

test('the devices dialog revokes another browser and leaves this page usable', async ({
  page,
  browser,
  wterm,
}) => {
  // Three things a static render cannot reach, in one journey:
  //
  //  1. Radix nesting. `DropdownMenu` -> `Dialog` is the classic
  //     `body { pointer-events: none }` leftover: the menu unmounts while the
  //     dialog opens, and if the cleanup loses the race the page stays inert
  //     after the dialog closes. Only a browser has a hit-test.
  //  2. The revoke handler storing the pruned list. `revokeAndPrune` is unit
  //     tested; the `setList` that consumes it is not, because
  //     `react-dom/server` runs no handlers.
  //  3. Revocation from a *browser* (DELETE /api/devices/{id}) severing another
  //     device -- a different path from the admin socket's, and the plan's last
  //     manual check.
  await enroll(page, wterm, 'laptop')

  const phone = await browser.newContext()
  const phonePage = await phone.newPage()
  try {
    await enroll(phonePage, wterm, 'phone')

    await page.locator('[data-slot="sidebar-footer"]').getByRole('button').first().click()
    await page.getByRole('menuitem', { name: 'Devices…' }).click()

    const dialog = page.getByRole('dialog', { name: 'Devices' })
    await expect(dialog.getByText('phone')).toBeVisible()
    await expect(dialog.getByText('laptop')).toBeVisible()
    // Device JSON is snake_case (`last_seen`), so a row that rendered "last
    // seen never" would mean the frontend and the daemon had drifted apart.
    await expect(dialog.getByText(/last seen just now/).first()).toBeVisible()
    await expect(dialog.getByText('this device')).toBeVisible()

    // Twice: revoking is deliberately two clicks, and the second button is the
    // one that says what it will do.
    const phoneRow = dialog.locator('li').filter({ hasText: 'phone' })
    await phoneRow.getByRole('button', { name: 'Revoke' }).click()
    await phoneRow.getByRole('button', { name: 'Revoke' }).click()

    // The row goes without a refetch: that is the handler storing the pruned
    // list, and nothing else would remove it -- the dialog does not poll.
    await expect(phoneRow).toHaveCount(0)
    await expect(dialog.getByText('laptop')).toBeVisible()
    expect(wterm.devices().map((d) => d.name)).not.toContain('phone')

    // The other browser loses its terminal, which is what revoking is for.
    await expect(pill(phonePage)).toContainText(/Reconnecting/)

    // The footer button, not the header's X: both are named "Close".
    await dialog.locator('[data-slot="dialog-footer"]').getByRole('button', { name: 'Close' }).click()
    await expect(dialog).toHaveCount(0)

    // The page is still interactive. If Radix left `pointer-events: none` on
    // <body>, this click is swallowed and nothing below runs.
    await expect(page.locator('body')).not.toHaveCSS('pointer-events', 'none')
    await page.getByRole('button', { name: /Jump to/ }).click()
    await expect(page.locator('[data-slot="command-input"]')).toBeVisible()
    await page.keyboard.press('Escape')
    await expect(page.locator('[data-slot="command-input"]')).toHaveCount(0)

    // And the terminal still takes input after all that.
    await focusTerminal(page)
    await page.keyboard.type("printf 'still-%s\\n' live\n")
    await expect(page.getByText('still-live')).toBeVisible()
  } finally {
    await phone.close()
  }
})

test.describe('on a phone-sized viewport', () => {
  test.use({ viewport: { width: 390, height: 844 }, hasTouch: true, isMobile: true })

  test('the sidebar sheet can open the devices dialog without wedging the page', async ({
    page,
    wterm,
  }) => {
    // Below the 768px breakpoint the sidebar is a `Sheet`, so the nesting is
    // Sheet -> DropdownMenu -> Dialog: three Radix layers, each with its own
    // focus trap and its own pointer-events bookkeeping. This is the phone the
    // whole product is for, and it is the arrangement most likely to leave the
    // page inert.
    await enroll(page, wterm, 'phone')

    // shadcn's mobile sidebar *is* the sheet content: it overwrites
    // data-slot="sheet-content" with data-slot="sidebar" and marks it mobile.
    await page.getByRole('button', { name: 'Toggle Sidebar' }).first().click()
    const sheet = page.locator('[data-slot="sidebar"][data-mobile="true"]')
    await expect(sheet).toBeVisible()

    await sheet.locator('[data-slot="sidebar-footer"]').getByRole('button').first().click()
    await page.getByRole('menuitem', { name: 'Devices…' }).click()

    const dialog = page.getByRole('dialog', { name: 'Devices' })
    await expect(dialog.getByText('phone')).toBeVisible()
    // The footer button, not the header's X: both are named "Close".
    await dialog.locator('[data-slot="dialog-footer"]').getByRole('button', { name: 'Close' }).click()
    await expect(dialog).toHaveCount(0)

    // Dismiss the sheet, then prove the page underneath still responds.
    await page.keyboard.press('Escape')
    await expect(sheet).toHaveCount(0)
    await expect(page.locator('body')).not.toHaveCSS('pointer-events', 'none')

    await page.getByRole('button', { name: /Jump to/ }).click()
    await expect(page.locator('[data-slot="command-input"]')).toBeVisible()
  })
})
