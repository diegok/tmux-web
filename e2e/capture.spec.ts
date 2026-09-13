/**
 * The capture panel, in a real browser.
 *
 * Everything in `CapturePanel.test.tsx` runs `react-dom/server` in node, where
 * an open Radix dialog is a portal into a `document` that does not exist. So
 * four of this panel's properties are invisible there and are asserted only
 * here: that the text really is selectable, that copy-all really reaches a
 * clipboard, that the header's once-a-second tick does not collapse a
 * selection, and where the focus goes -- on open, and after the selector moves
 * the tab.
 *
 * ## Every fixture line is this test's own
 *
 * The panes here run a copy of `cat` (`fakeAgent`) and are fed their content by
 * `send-keys`, so every line the assertions name was written by this file. The
 * repository is public and the machine running the suite has real work in its
 * own tmux server; nothing from a live pane is ever read, let alone asserted
 * on. The harness's `TMUX_TMPDIR` and `-L` shim are what make that structural
 * rather than a promise.
 *
 * ## The trap this file is written against
 *
 * Three e2e tests in this repository have passed against broken code because
 * the state *before* the action already satisfied the assertion. So the two
 * panes carry deliberately different markers, `ALPHA-...` and `BRAVO-...`, and
 * every assertion after an action names a string that cannot already be on
 * screen: the pane switch is asserted with the marker of the pane that was not
 * being shown, and every focus assertion is made against a describing string
 * whose pre-action value is read and written down first.
 *
 * ## Where the focus actually goes, measured
 *
 * Two things here are not what the plan and the panel's own comment predict,
 * and both were found by writing the assertion the plan asked for and watching
 * it fail on a correct build:
 *
 *   - **On open, focus does not move to the dialog container.** Radix reaches
 *     for the container only when the auto-focus event is *not* prevented, so
 *     with `onOpenAutoFocus` prevented focus simply stays on the header button
 *     that opened the panel. The property the design wants holds either way --
 *     nothing inside the panel has the keyboard -- and that is what is
 *     asserted. Escape still closes the panel from there, and this file proves
 *     it rather than assuming it.
 *   - **After the selector changes pane, the resting focus does not move at
 *     all**, because `selectOption` does not leave a native select focused. So
 *     "focus is still inside the dialog" is false on a *correct* build, and
 *     "focus is outside the dialog" is true on a broken one too -- a focused
 *     terminal is outside as well. The instrument that separates them is a
 *     `focusin` counter on the terminal, which is also the truer statement of
 *     the guarantee: on a phone what raises the keyboard is that input taking
 *     focus at all, even for one turn.
 */

import { BASE_SESSION, breadcrumb, enroll, expect, focusTerminal, test } from './harness'
import type { TmuxWeb } from './harness'
import type { Locator, Page } from '@playwright/test'

/**
 * Chromium refuses `navigator.clipboard.readText()` without this, and the read
 * would then be a rejected promise the assertion reported as "not equal".
 */
test.use({ permissions: ['clipboard-read', 'clipboard-write'] })

/** The two markers. Distinct on purpose; see the header. */
const ALPHA = 'ALPHA-fixture-line-one'
const BRAVO = 'BRAVO-fixture-line-two'

/**
 * A window with one pane running a fake agent, fed one line of this test's own
 * text. Returns the pane id.
 *
 * `cat` echoes what is sent to it, so the marker lands in the pane's scrollback
 * twice -- once from the pty's echo of the keystrokes, once from `cat` writing
 * it back. Both are this file's own bytes.
 */
function fixturePane(tmuxWeb: TmuxWeb, window: string, agent: string, marker: string): string {
  tmuxWeb.tmux('new-window', '-d', '-t', BASE_SESSION, '-n', window, tmuxWeb.fakeAgent(agent))
  const pane = tmuxWeb.tmux('list-panes', '-t', `${BASE_SESSION}:${window}`, '-F', '#{pane_id}')
  tmuxWeb.tmux('send-keys', '-t', pane, marker, 'Enter')
  return pane
}

/** The panel. */
function panel(page: Page): Locator {
  return page.getByRole('dialog')
}

/** The read-and-copy surface inside it. */
function pre(page: Page): Locator {
  return panel(page).locator('pre')
}

/** `captured 14s ago`, the one thing in here that changes on its own. */
function age(page: Page): Locator {
  return panel(page).locator('[data-slot="dialog-description"]')
}

/**
 * Where the keyboard is, in words.
 *
 * A string rather than a boolean so a failure says what *did* have focus, which
 * is the difference between a readable diff and a `false`. The answers are kept
 * distinct: the dialog container, some control inside it (which is what a
 * dropped `onOpenAutoFocus` produces -- the pane selector), and anything
 * outside it, named. The container case is the one the plan predicted and this
 * build never produces; it is still reported by name rather than lumped in.
 */
async function focusReport(page: Page): Promise<string> {
  return await page.evaluate(() => {
    const el = document.activeElement
    if (!el || el === document.body) return 'nothing'
    const label = el.getAttribute('aria-label') ?? (el.textContent ?? '').trim().slice(0, 20)
    const name = `<${el.tagName.toLowerCase()}${label === '' ? '' : ` ${label}`}>`
    const dialog = document.querySelector('[data-slot="dialog-content"]')
    if (dialog === null) return `no dialog, ${name}`
    if (dialog === el) return 'the dialog container'
    return dialog.contains(el) ? `inside the dialog, ${name}` : `outside the dialog, ${name}`
  })
}

/** Everything the browser thinks is selected right now. */
async function selection(page: Page): Promise<string> {
  return await page.evaluate(() => window.getSelection()?.toString() ?? '')
}

/**
 * Triple-click on a marker, rather than in the middle of the element.
 *
 * A capture is mostly blank lines below the cursor, so a click on the `<pre>`'s
 * centre lands on empty space and selects nothing -- which would make the
 * selection assertion fail against a perfectly good build, and worse, would
 * make `user-select: none` indistinguishable from a badly aimed click. The
 * marker's own rectangle is measured with a `Range` and clicked in the middle.
 */
async function tripleClickOn(page: Page, marker: string): Promise<void> {
  const spot = await pre(page).evaluate((el, text) => {
    const node = el.firstChild
    const at = (el.textContent ?? '').indexOf(text)
    if (node === null || at < 0) throw new Error(`the <pre> does not contain ${text}`)
    const range = document.createRange()
    range.setStart(node, at)
    range.setEnd(node, at + text.length)
    const box = range.getBoundingClientRect()
    return { x: box.x + box.width / 2, y: box.y + box.height / 2 }
  }, marker)
  await page.mouse.click(spot.x, spot.y, { clickCount: 3 })
}

/** Open the panel on the pane the tab is on. */
async function openPanel(page: Page): Promise<void> {
  await page.getByRole('button', { name: 'Scrollback' }).click()
  await pre(page).waitFor()
}

test('the capture is selectable and copy-all reaches the clipboard', async ({ page, tmuxWeb }) => {
  fixturePane(tmuxWeb, 'notes', 'scribe', ALPHA)
  await enroll(page, tmuxWeb, 'laptop')

  // A clipboard left over from anything else is the classic false pass here, so
  // it starts empty and the assertion below is against a string that is not.
  await page.evaluate(() => navigator.clipboard.writeText(''))
  expect(await page.evaluate(() => navigator.clipboard.readText())).toBe('')

  await page.getByRole('button', { name: /^1: notes/ }).click()
  await expect(breadcrumb(page)).toContainText('scribe')

  // The pre-state, in the words the post-open assertion uses. Written down
  // rather than assumed: if the panel were already open, or focus already
  // inside it, everything below would be asserting on nothing.
  await expect(panel(page)).toHaveCount(0)
  expect(await focusReport(page)).toContain('no dialog')

  await openPanel(page)

  // Task 7's deferred mutant, come home. `onOpenAutoFocus` is a prop of
  // `DialogContent`, which never renders under vitest, so this is the only
  // assertion in the repository that can see it dropped.
  //
  // Stated as a relationship to the dialog container and to nothing inside it.
  // Which control is first tabbable is Task 8's business and has already
  // changed once; an assertion phrased as "not the pane selector" would have to
  // be edited the next time it does, and would go quietly green if Radix ever
  // picked a different child.
  //
  // MEASURED, and it is not what the plan predicted. With the auto-focus
  // prevented, Radix moves focus nowhere at all -- its focus scope only focuses
  // the container when the mount event is *not* prevented -- so focus stays
  // where the click left it, on the header button that opened the panel. It is
  // outside the dialog rather than on the dialog container. The property that
  // matters is the same either way and is the one asserted: nothing inside the
  // panel has the keyboard, which on a phone is the soft keyboard not rising.
  const onOpen = await focusReport(page)
  expect(
    onOpen,
    'the panel opened with focus on a control inside it: on a phone that is the ' +
      'soft keyboard rising over the panel that exists to be read without one',
  ).toContain('outside the dialog')

  // Focus stays out after the capture lands, which is a second render of the
  // body and of the selector inside it.
  await expect(pre(page)).toContainText(ALPHA)
  expect(await focusReport(page)).toContain('outside the dialog')

  // What focus outside the dialog puts at risk, and the reason the decision
  // records it as safe: Escape still closes the panel, because the key reaches
  // the document from the button that has it. (It does not from the terminal's
  // textarea, which swallows keys -- but nothing here leaves focus there.)
  await page.keyboard.press('Escape')
  await expect(panel(page), 'Escape does not close the panel').toHaveCount(0)
  await openPanel(page)

  await expect(pre(page)).toContainText(ALPHA)
  // A short capture is not truncated, so the notice's own unit test is not the
  // only fixture it ever sees.
  await expect(panel(page)).not.toContainText('Too much to send')

  await tripleClickOn(page, ALPHA)
  const selected = await selection(page)
  expect(
    selected,
    'a triple-click in the capture selected nothing: the text is not selectable',
  ).not.toBe('')
  expect(selected).toContain(ALPHA)

  // What is on screen, which is what the clipboard has to end up holding.
  const shown = (await pre(page).textContent()) ?? ''
  expect(shown).toContain(ALPHA)

  await panel(page).getByRole('button', { name: 'Copy all' }).click()
  await expect(panel(page).getByRole('button', { name: 'Copied' })).toBeVisible()

  const clipboard = await page.evaluate(() => navigator.clipboard.readText())
  expect(clipboard, 'copy-all put nothing on the clipboard').not.toBe('')
  expect(clipboard, 'the clipboard does not hold the capture that was on screen').toBe(shown)
})

test('the header ageing does not collapse a selection', async ({ page, tmuxWeb }) => {
  fixturePane(tmuxWeb, 'notes', 'scribe', ALPHA)
  await enroll(page, tmuxWeb, 'laptop')

  await page.getByRole('button', { name: /^1: notes/ }).click()
  await expect(breadcrumb(page)).toContainText('scribe')
  await openPanel(page)
  await expect(pre(page)).toContainText(ALPHA)

  await tripleClickOn(page, ALPHA)
  const selected = await selection(page)
  expect(selected, 'nothing was selected, so the tick below has nothing to destroy').not.toBe('')

  // The clock has to be seen to tick, or this test passes on a panel whose
  // header never re-rendered at all.
  const before = (await age(page).textContent()) ?? ''
  await expect(age(page), 'the capture age never changed, so nothing re-rendered').not.toHaveText(
    before,
  )
  await expect(age(page)).toContainText('ago')

  expect(
    await selection(page),
    'the once-a-second header tick collapsed the selection: the panel destroys the ' +
      'thing it exists for while the user is reaching for the copy button',
  ).toBe(selected)
})

test('the panel selector moves the header, the capture and nothing else', async ({
  page,
  tmuxWeb,
}) => {
  fixturePane(tmuxWeb, 'notes', 'scribe', ALPHA)
  const bravo = fixturePane(tmuxWeb, 'log', 'ledger', BRAVO)
  await enroll(page, tmuxWeb, 'laptop')

  await page.getByRole('button', { name: /^1: notes/ }).click()
  await expect(breadcrumb(page)).toContainText('scribe')

  // On a phone the guarantee is "nothing raises the keyboard", and where focus
  // is *resting* afterwards turns out not to measure it -- see the header. What
  // the phone reacts to is the terminal's input taking focus **at all**, even
  // for one turn, so that is counted rather than inferred.
  //
  // `focusin` and not `focus`: the element the role query finds is the wterm
  // host `<div role="textbox">`, and the thing that actually takes focus is a
  // hidden `<textarea>` inside it. `focus` does not bubble, so a listener on the
  // host counts nothing -- measured, against the very mutant this exists for.
  //
  // Wired up before the panel opens, and that is not tidiness either: an open
  // Radix modal marks everything behind it `aria-hidden`, so the role query that
  // finds the terminal everywhere else in this suite matches nothing while the
  // panel is up.
  const terminalInput = page.getByRole('textbox', { name: 'Terminal' })
  await expect(
    terminalInput,
    'no terminal input to watch, so the count below is vacuous',
  ).toHaveCount(1)
  await terminalInput.evaluate((el) => {
    const w = window as unknown as { __termFocus?: number }
    w.__termFocus = 0
    el.addEventListener('focusin', () => (w.__termFocus = (w.__termFocus ?? 0) + 1))
  })

  // And the instrument is live. A focus the counter cannot see is a zero that
  // means nothing, which is exactly how the first version of this test passed
  // against the build it was written to catch. `focusTerminal` is a click, so
  // this doubles as the ordinary way in: the user was typing in the pane and
  // then opened the panel over it.
  //
  // The blur is not ceremony. The sidebar click above ends with
  // `handleSelectPane` focusing the terminal, so without it the click below
  // re-focuses an already-focused input, fires no event at all, and the check
  // condemns an instrument that works.
  await page.evaluate(() => (document.activeElement as HTMLElement | null)?.blur())
  await focusTerminal(page)
  expect(
    await page.evaluate(() => (window as unknown as { __termFocus?: number }).__termFocus),
    'the focus counter does not count a focus, so its zero below says nothing',
  ).toBe(1)
  await page.evaluate(() => ((window as unknown as { __termFocus: number }).__termFocus = 0))

  await openPanel(page)

  // Where this starts: ALPHA's capture under ALPHA's pane, and BRAVO nowhere on
  // screen. Every assertion after the switch names BRAVO for that reason.
  await expect(pre(page)).toContainText(ALPHA)
  await expect(panel(page)).not.toContainText(BRAVO)

  // Where the keyboard is before the switch, so the assertion after it can be
  // "nothing moved" rather than a guess at where it ought to end up. See the
  // header for why neither "inside the dialog" nor "outside" can be asserted
  // here.
  const focusBefore = await focusReport(page)

  await panel(page).getByLabel('Pane').selectOption(bravo)

  // (b) The header and the text move together. The fetch effect is keyed on the
  // pane, so a build whose deps dropped it leaves ALPHA's scrollback sitting
  // under BRAVO's title -- the one failure of this panel a user cannot see.
  // BRAVO leads, because it is on no part of the screen the switch did not
  // change.
  await expect(
    pre(page),
    "the capture under the new pane's header is still the old pane's",
  ).toContainText(BRAVO)
  await expect(pre(page)).not.toContainText(ALPHA)
  await expect(panel(page).getByRole('heading')).toContainText(bravo)

  // (a) The selector passes `{ focus: false }` and `handleSelectPane` honours
  // it: the keyboard never went near the terminal, and it is still the panel
  // the user is in.
  expect(
    await page.evaluate(() => (window as unknown as { __termFocus?: number }).__termFocus),
    'changing pane in the panel focused the terminal: on a phone the soft keyboard ' +
      'comes up over the panel, inside the tap that changed the pane',
  ).toBe(0)
  expect(await focusReport(page), 'changing pane moved the keyboard somewhere new').toBe(
    focusBefore,
  )
  await expect(panel(page), 'the panel closed itself when the pane changed').toHaveCount(1)
})
