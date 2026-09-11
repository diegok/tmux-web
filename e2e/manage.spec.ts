/**
 * Managing tmux from the browser: the menus that open, and what tmux looks like
 * afterwards.
 *
 * Every rule about *what is offered* is `lib/manage.ts`'s and is table-tested;
 * every rule about what the daemon does with a request is Go's, against a real
 * tmux server. Two things are left over, and they are what is here:
 *
 *  - **The menus themselves.** Radix portals a context menu's content, and the
 *    vitest suite renders with `react-dom/server`, so a static render can only
 *    pin that each row carries a trigger. Whether right-clicking one actually
 *    opens a menu with rename and kill in it is unreachable there. So is the
 *    palette's `onIntent`, and so is every handler in `App.tsx`, which that
 *    suite has no test for at all.
 *  - **Whether the thing that died is the thing that was named.** Each test
 *    below reads tmux back with `list-windows`, `list-panes` or
 *    `display-message` rather than trusting the sidebar to have told the truth
 *    about a call it made itself.
 */

import {
  BASE_SESSION,
  BASE_WINDOW,
  breadcrumb,
  enroll,
  expect,
  focusTerminal,
  pill,
  sessionLabel,
  test,
  windowRow,
} from './harness'
import type { TmuxWeb } from './harness'
import type { Page } from '@playwright/test'

/** `tmux list-sessions`, tolerating there being no server at all. */
function sessions(tmuxWeb: TmuxWeb): string[] {
  try {
    return tmuxWeb.tmux('list-sessions', '-F', '#{session_name}').split('\n')
  } catch {
    return []
  }
}

/** The windows of the base session, by name. */
function windows(tmuxWeb: TmuxWeb): string[] {
  return tmuxWeb.tmux('list-windows', '-t', BASE_SESSION, '-F', '#{window_name}').split('\n')
}

/** Open a row's context menu, over its text rather than its padding. */
async function rightClick(row: import('@playwright/test').Locator, text: string): Promise<void> {
  await row.getByText(text, { exact: true }).click({ button: 'right' })
}

/** The dialog a prompt or a kill opens. */
function dialog(page: Page) {
  return page.getByRole('dialog')
}

test('renaming a session from the browser renames it in tmux and in the sidebar', async ({
  page,
  tmuxWeb,
}) => {
  await enroll(page, tmuxWeb, 'laptop')

  // Grouped, which is the whole point of renaming *this* session: the tab is
  // attached through a throwaway session in `e2e`'s group, and tmux freezes
  // `session_group` at the name the group was created under. On an ungrouped
  // session the group field is already the live name, and a sidebar keyed and
  // labelled on it would pass this test while renaming looked like it did
  // nothing for every real user.
  expect(tmuxWeb.tmux('list-sessions', '-F', '#{session_name} #{session_group}').split('\n')).toEqual(
    expect.arrayContaining([expect.stringMatching(/^_web-\w+ e2e$/)]),
  )

  await rightClick(sessionLabel(page, BASE_SESSION), BASE_SESSION)
  await page.getByRole('menuitem', { name: `Rename session "${BASE_SESSION}"…` }).click()

  // It opens on the current name, which is the rule that keeps a reopened
  // dialog from renaming one session to another one's name.
  const name = dialog(page).getByLabel('Name')
  await expect(name).toHaveValue(BASE_SESSION)
  await name.fill('renamed')
  await dialog(page).getByRole('button', { name: 'Rename' }).click()

  // tmux, first and for real.
  await expect.poll(() => sessions(tmuxWeb)).toContain('renamed')
  // And tmux's group is still the old name, so the sidebar below can only have
  // got "renamed" from the live session name.
  expect(tmuxWeb.tmux('list-sessions', '-F', '#{session_name} #{session_group}')).toContain(
    `renamed ${BASE_SESSION}`,
  )
  await expect(sessionLabel(page, 'renamed')).toBeVisible()
  await expect(sessionLabel(page, BASE_SESSION)).toHaveCount(0)

  // The socket is untouched by a rename -- the group key it is addressed by did
  // not change -- so the terminal is still the same live terminal.
  await expect(pill(page)).toHaveCount(0)
  await focusTerminal(page)
  await page.keyboard.type("printf 'still-%s\\n' attached\n")
  await expect
    .poll(() => tmuxWeb.tmux('capture-pane', '-p', '-t', `renamed:${BASE_WINDOW}`))
    .toContain('still-attached')
})

test('a new window from the +, a split from the palette, a zoom from the menu', async ({
  page,
  tmuxWeb,
}) => {
  await enroll(page, tmuxWeb, 'laptop')

  // The session row's `+`. tmux names the window itself.
  await page.getByRole('button', { name: `New window in "${BASE_SESSION}"` }).click()
  await expect.poll(() => windows(tmuxWeb)).toHaveLength(2)

  const row = page.getByRole('button', { name: /^1: / })
  await expect(row).toBeVisible()
  // The palette scopes its management entries to the pane this tab is looking
  // at, so the tab has to be looking at the new window.
  await row.click()
  await expect(breadcrumb(page)).toContainText('1: ')

  // Split from the palette. `Palette`'s `onIntent` is a JSX arrow that runs the
  // action and closes the dialog, and no static render runs either half.
  await page.getByRole('button', { name: /Jump to/ }).click()
  await page.getByRole('option', { name: 'Split right' }).click()
  await expect
    .poll(() => tmuxWeb.tmux('list-panes', '-t', `${BASE_SESSION}:1`, '-F', '#{pane_id}').split('\n'))
    .toHaveLength(2)
  // It closed, which is how a palette says the command went through.
  await expect(page.locator('[data-slot="command-input"]')).toHaveCount(0)
  // The second pane is a row of its own now: a window with one pane is the
  // pane, and the sidebar only grows a sub-list when there is more than one.
  await expect(page.getByRole('button', { name: /pane 1/ })).toBeVisible()

  // Zoom from the row's own context menu, on a window that genuinely has two
  // panes: `resize-pane -Z` exits 0 and does nothing on a single-pane window,
  // so the same test on the window before the split would pass without zooming
  // anything.
  const zoomed = () =>
    tmuxWeb.tmux('display-message', '-p', '-t', `${BASE_SESSION}:1`, '#{window_zoomed_flag}')
  expect(zoomed()).toBe('0')
  const label = `1: ${tmuxWeb.tmux('display-message', '-p', '-t', `${BASE_SESSION}:1`, '#{window_name}')}`
  await rightClick(row, label)
  await page.getByRole('menuitem', { name: 'Zoom window' }).click()
  await expect.poll(zoomed).toBe('1')
})

test('the kill dialog does nothing until it is armed', async ({ page, tmuxWeb }) => {
  await enroll(page, tmuxWeb, 'laptop')
  tmuxWeb.tmux('new-window', '-d', '-t', BASE_SESSION, '-n', 'doomed', 'sh')
  tmuxWeb.tmux('split-window', '-d', '-t', `${BASE_SESSION}:doomed`, 'sh')

  const row = windowRow(page, 'doomed')
  await expect(row).toBeVisible()
  await rightClick(row, '1: doomed')
  await page.getByRole('menuitem', { name: 'Kill window…' }).click()

  // What it says it will destroy, before it can be armed.
  await expect(dialog(page)).toContainText('kill window "doomed"')
  await expect(dialog(page)).toContainText('2 panes')

  // `toBeDisabled` reads the property. `expect(class).toContain('disabled')`
  // would pass on any shadcn button, whose Tailwind carries
  // `disabled:pointer-events-none`.
  const red = dialog(page).getByRole('button', { name: 'Kill window' })
  await expect(red).toBeDisabled()
  await red.click({ force: true })
  await page.waitForTimeout(1_000)
  // The assertion that means something: the window is still there. A red button
  // that only *looked* inert would have taken it by now.
  expect(windows(tmuxWeb)).toContain('doomed')
  await expect(dialog(page)).toBeVisible()

  await dialog(page).getByRole('checkbox').check()
  await expect(red).toBeEnabled()
  await red.click()

  await expect.poll(() => windows(tmuxWeb)).not.toContain('doomed')
  await expect(row).toHaveCount(0)
  // The tab is attached to this session, and it survives: the kill took the
  // window it named and nothing else.
  await expect(pill(page)).toHaveCount(0)
})

test('with no tmux server at all, the browser starts a session', async ({ page, tmuxWeb }) => {
  await enroll(page, tmuxWeb, 'laptop')

  // The only session there is, so this takes the whole tmux server with it.
  // That is the state the design points at -- a phone, no shell on this host,
  // an empty server -- and the one v1 could only answer with "go and run tmux
  // over SSH".
  tmuxWeb.tmux('kill-server')
  await expect.poll(() => sessions(tmuxWeb)).toEqual([])

  // The empty state's button, in the main area rather than the sidebar: this is
  // `App`'s own `onNewSession`, which no test in the vitest suite reaches.
  const empty = page.locator('[data-slot="sidebar-inset"]')
  await expect(empty).toContainText('There is no tmux session to attach to')
  await empty.getByRole('button', { name: 'New session' }).click()

  await dialog(page).getByLabel('Name').fill('fresh')
  await dialog(page).getByRole('button', { name: 'Create' }).click()

  await expect.poll(() => sessions(tmuxWeb)).toEqual(['fresh'])
  await expect(sessionLabel(page, 'fresh')).toBeVisible()

  // Attached to it, not merely showing it. The keystroke is the proof: the tab
  // had to notice the new session, pick it, and open a socket into a throwaway
  // session grouped with it, on a tmux server that did not exist a moment ago.
  await page.locator('.term-row').first().waitFor()
  await expect(pill(page)).toHaveCount(0)
  await expect(breadcrumb(page)).toContainText('fresh')
  await focusTerminal(page)
  await page.keyboard.type("printf 'e2e-%s\\n' fresh\n")
  await expect.poll(() => tmuxWeb.tmux('capture-pane', '-p', '-t', 'fresh:0')).toContain('e2e-fresh')
})
