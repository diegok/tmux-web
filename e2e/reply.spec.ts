/**
 * The reply box, judged by what the pane's shell did -- never by what the
 * browser drew.
 *
 * ## Why this file has to exist at all
 *
 * `<Terminal>` exposes two functions a reply could plausibly be handed to, and
 * they are one keystroke apart:
 *
 *   - `TerminalSession.write` puts a data frame on the WebSocket, and the PTY
 *     on the far end feeds it to the pane's program. This is the wire.
 *   - `useTerminal().write` -- bound as `paint` in that component precisely so
 *     that the name `write` no longer resolves there -- draws the characters
 *     into the local grid and sends nothing. This is the display.
 *
 * Bind `TerminalHandle.send` to the second and the reply appears in the
 * terminal character for character, looking exactly like it worked, while the
 * agent waits forever. `renderToStaticMarkup` produces identical markup either
 * way, so no vitest test can separate them; the only instrument that can is a
 * real tmux server, asked what its pane holds.
 *
 * ## How every assertion here stays on the tmux side of that line
 *
 * Nothing below asserts on `.term-row`, on `getByText`, or on anything else the
 * browser rendered. Every claim is read back out of tmux with `capture-pane`,
 * and each one is written so that the characters merely *appearing* cannot
 * satisfy it. Two habits, both borrowed from
 * `internal/front/reply_integration_test.go`, which solves the same problem on
 * the Go side:
 *
 *   - **Markers are split across a quote.** The reply is `echo REP""LY`, so the
 *     line the shell echoes back holds `REP""LY` and only the command's own
 *     output is `REPLY`. A canvas that painted the keystrokes -- or a pane that
 *     received them without running them -- has the wrong string.
 *   - **Executed is proved by the *next* command.** The shell is serial, so a
 *     second command asking for `$?` cannot answer until it has finished with
 *     the reply. That probe is both the synchronisation point (nothing here
 *     sleeps) and the evidence that the reply was run rather than typed.
 *
 * The probe deliberately travels a different road from the reply: it is typed
 * into the terminal, whose keystroke path already has a passing test in
 * `terminal.spec.ts`. So when the reply box is broken the probe still arrives,
 * the shell still answers, and the failure is a specific "the shell finished
 * and your reply was not in what it ran" rather than a timeout.
 *
 * ## What the harness already guarantees, so this file does not
 *
 * The Go test respawns the pane onto `/bin/sh` before it measures anything,
 * because its fixture inherits whatever login shell the developer has. The e2e
 * harness starts its base session as `sh` with `SHELL=/bin/sh` in the server's
 * environment, so the pane here is already the controlled one and a
 * `respawn-pane` would only be an extra chance to disturb a pane a browser is
 * attached to.
 *
 * Every test below opens the box first, through the header toggle, because that
 * is now the only way it exists. See `openReplyBox`.
 */

import type { Page } from '@playwright/test'

import { enroll, expect, focusTerminal, openReplyBox, pill, replyBox, test } from './harness'
import type { TmuxWeb } from './harness'

/**
 * The pane the box says it will send to, once the tab has learned it.
 *
 * Waiting for it is not just synchronisation. `replyFrames` omits the
 * `end-mode` frame entirely when the pane is null, so a copy-mode test that
 * replied too early would be testing a code path with no `end-mode` in it and
 * would pass for the wrong reason. Reading the id from the hint also ties every
 * `capture-pane` below to the pane the product itself claims to be addressing,
 * rather than to a target this file guessed.
 */
async function replyTarget(page: Page): Promise<string> {
  const hint = page.getByText(/Enter sends to/)
  await expect(hint, 'the reply box never named a pane, so no end-mode frame would be sent')
    .toContainText(/%\d+/)
  const text = await hint.innerText()
  return /%\d+/.exec(text)![0]
}

/** What tmux says is on the pane, as one string. */
function capture(tmuxWeb: TmuxWeb, pane: string): string {
  return tmuxWeb.tmux('capture-pane', '-p', '-t', pane)
}

/**
 * Type into the terminal, which is the path `terminal.spec.ts` already proves.
 *
 * The click is not ceremony: the caret is wherever the last interaction left
 * it, and after a reply that is the reply box. See the note on `focusTerminal`.
 */
async function typeIntoPane(page: Page, text: string): Promise<void> {
  await focusTerminal(page)
  await page.keyboard.type(text)
  await page.keyboard.press('Enter')
}

/**
 * Wait until the pane's shell is reading commands, rather than until its
 * process exists.
 *
 * `READY` can only come from the command's output: the input line the shell
 * echoes holds `REA""DY`.
 */
async function waitForShell(page: Page, tmuxWeb: TmuxWeb, pane: string): Promise<void> {
  await typeIntoPane(page, 'echo REA""DY')
  await expect
    .poll(() => capture(tmuxWeb, pane), {
      message: `pane ${pane} never printed READY, so its shell is not running commands`,
    })
    .toContain('READY')
}

/**
 * Make the shell report the exit status of whatever it last did, and wait for
 * the answer.
 *
 * This is the only wait in the file that has to exist, and it waits on the
 * shell rather than on the clock: the probe is a second command down the same
 * tty, so an answer to it means the shell is done with everything before it.
 * Returns the status and the capture it was read from.
 */
async function status(
  page: Page,
  tmuxWeb: TmuxWeb,
  pane: string,
): Promise<{ status: string; capture: string }> {
  await typeIntoPane(page, 'echo "S"TATUS=$?')
  await expect
    .poll(() => capture(tmuxWeb, pane), {
      message:
        `the shell in pane ${pane} never reported an exit status, so nothing here can be ` +
        `read as finished -- not even as failed`,
    })
    .toMatch(/^STATUS=\d+\s*$/m)

  const out = capture(tmuxWeb, pane)
  return { status: /^STATUS=(\d+)/m.exec(out)![1], capture: out }
}

/** The lines of a capture, trimmed -- a command's output is a line of its own. */
function lines(out: string): string[] {
  return out.split('\n').map((line) => line.trim())
}

test('a reply typed into the box runs in the pane, not merely on the screen', async ({
  page,
  tmuxWeb,
}) => {
  // Mutant E, and the reason this file was written: `TerminalHandle.send` bound
  // to the local painter instead of the transport. Under that mutation the
  // browser shows `echo REP""LY` in the terminal and tmux has never heard of
  // it, so every assertion below is on the tmux side.
  await enroll(page, tmuxWeb, 'laptop')
  await openReplyBox(page)

  const pane = await replyTarget(page)
  await waitForShell(page, tmuxWeb, pane)

  await replyBox(page).fill('echo REP""LY')
  await replyBox(page).press('Enter')

  const done = await status(page, tmuxWeb, pane)

  // `REPLY` on a line of its own is the command's output. The echo of the input
  // line cannot be it twice over: it carries the quotes, and it carries the
  // prompt in front of them.
  expect(
    lines(done.capture),
    `pane ${pane} never ran the reply. The shell answered the probe with ` +
      `STATUS=${done.status}, so it was reading the tty and moved on -- the bytes never ` +
      `arrived. This is exactly what a send() bound to the canvas painter looks like ` +
      `from tmux's side: the browser shows the reply and the pane never sees it. ` +
      `Pane holds:\n${done.capture}`,
  ).toContain('REPLY')

  // Typed is not run. The shell exits 0 for the echo it executed; a pane that
  // had only been painted at, or that had the line sitting unsent on its input
  // line, would answer for something else.
  expect(
    done.status,
    `the shell reported ${done.status} for the reply, want 0: something other than the ` +
      `whole line ran in pane ${pane}:\n${done.capture}`,
  ).toBe('0')
})

test('a reply into a pane in copy mode arrives whole', async ({ page, tmuxWeb }) => {
  // The `end-mode` frame, and the survivor where it is a no-op. A pane in copy
  // mode does not swallow a reply, it TRUNCATES it: `q` is cancel in both
  // copy-mode key tables, so the pane leaves the mode the moment the browser
  // types one and everything after it goes to the shell as a command. The
  // payload therefore puts a `q` early, the way prose does.
  //
  // `internal/front/reply_integration_test.go` pins both halves of this through
  // a PTY. What it cannot show, and this can, is that the browser sends the
  // frame at all and sends it first.
  await enroll(page, tmuxWeb, 'laptop')
  await openReplyBox(page)

  const pane = await replyTarget(page)
  await waitForShell(page, tmuxWeb, pane)

  // Driven with tmux rather than the app's own Copy mode button: the
  // precondition should not depend on the feature next door, and this is how
  // the pane gets there when the agent's own output puts it there.
  tmuxWeb.tmux('copy-mode', '-t', pane)
  expect(
    tmuxWeb.tmux('display-message', '-p', '-t', pane, '#{pane_in_mode}'),
    `pane ${pane} is not in copy mode, so this test is about nothing`,
  ).toBe('1')

  await replyBox(page).fill('echo qu""it PARTIAL')
  await replyBox(page).press('Enter')

  const done = await status(page, tmuxWeb, pane)

  // Whole, not its tail. Without the `end-mode` frame the pane's copy layer
  // eats `echo ` and is cancelled by the `q`, and the shell runs `uit PARTIAL`
  // -- which leaves `PARTIAL` on the screen and 127 in `$?`, so an assertion
  // about the fragment would pass in both worlds. Only the full output line
  // separates them.
  expect(
    lines(done.capture),
    `pane ${pane} never printed the whole reply on a line of its own (the shell ` +
      `answered STATUS=${done.status}, so it has finished). Either no end-mode frame ` +
      `preceded the bytes, or it did nothing: the copy layer read the reply's own \`q\` ` +
      `as cancel and the shell got the tail of the sentence as a command. Pane holds:\n` +
      `${done.capture}`,
  ).toContain('quit PARTIAL')

  expect(
    done.status,
    `the shell reported ${done.status} for the reply, want 0: a fragment ran in pane ` +
      `${pane} rather than the line that was typed:\n${done.capture}`,
  ).toBe('0')
})

test('the no-Return button leaves the line unsubmitted', async ({ page, tmuxWeb }) => {
  // The third survivor: the small button wired to the sender that appends CR.
  // Answering a `y/n` prompt is one character and no Return, and a Return that
  // should not be there is a second keystroke into whatever the prompt did next
  // -- which is unobservable if you only ask whether the characters arrived.
  //
  // So the reply is deliberately half a command. Only a pane that is still
  // holding it on an unsubmitted input line can be completed into something
  // that prints the marker; a pane that already ran `echo NOCR_JOI` printed the
  // wrong string and then failed to find a command called `NED`.
  await enroll(page, tmuxWeb, 'laptop')
  await openReplyBox(page)

  const pane = await replyTarget(page)
  await waitForShell(page, tmuxWeb, pane)

  await replyBox(page).fill('echo NOCR_JOI')
  await page.getByRole('button', { name: 'No ⏎' }).click()

  await typeIntoPane(page, '""NED')
  const done = await status(page, tmuxWeb, pane)

  expect(
    lines(done.capture),
    `pane ${pane} never printed the joined marker (the shell answered ` +
      `STATUS=${done.status}). The no-Return button submitted the line by itself, so the ` +
      `shell ran half a command and the rest of it became a command of its own. Pane ` +
      `holds:\n${done.capture}`,
  ).toContain('NOCR_JOINED')

  expect(
    done.status,
    `the shell reported ${done.status} for the joined line, want 0:\n${done.capture}`,
  ).toBe('0')
})

/**
 * Task 23: the box is off until a person asks for it, it takes its own space
 * when it is on, and the browser remembers which.
 *
 * The owner's words, after using the shape Tasks 17 and 21 shipped: "en desktop
 * no tiene NINGUN sentido ese componente. No debería mostrarse a menos que yo
 * decida verlo y cuando decido verlo, la consola debe adaptarse para que se siga
 * viendo entera."
 *
 * Both halves of that sentence are here, and the second one is the reason these
 * tests are in a browser rather than in vitest: "se siga viendo entera" is a
 * claim about pixels, and reading it off the class attribute -- "there is no
 * `absolute` in there" -- would be asserting the fix rather than the property.
 */
test('the box is not there until the toggle asks for it', async ({ page, tmuxWeb }) => {
  await enroll(page, tmuxWeb, 'laptop')

  // Nothing has been decided by this browser, so: closed. Not "hidden on a
  // desktop" -- there is no device branch anywhere, and a phone loads exactly
  // this.
  await expect(
    replyBox(page),
    'the reply box was on screen before anybody asked for it',
  ).toHaveCount(0)

  await openReplyBox(page)
  await expect(replyBox(page)).toBeVisible()

  // Opening it by the toggle is a person saying "I want to type now", so the
  // box takes the keyboard and the user does not have to then click it.
  await expect(
    replyBox(page),
    'the toggle showed the box but left the caret elsewhere, so the press did half its job',
  ).toBeFocused()
})

test('the terminal s last row is still visible with the box open', async ({ page, tmuxWeb }) => {
  await enroll(page, tmuxWeb, 'laptop')
  await openReplyBox(page)

  // A hit test, not a class and not a pair of numbers of this file's own
  // choosing: `elementFromPoint` answers "what would a click at the middle of
  // the terminal's last row land on", which is exactly what "nothing is
  // covered" means. Under Task 21's overlay the answer was the reply box's own
  // translucent strip, and the row underneath it was the prompt.
  const seen = await page.evaluate(() => {
    const rows = [...document.querySelectorAll('.term-row')]
    const last = rows.at(-1)
    const box = document.querySelector('textarea[aria-label="Reply to this pane"]')
    if (!last || !box) return null
    const r = last.getBoundingClientRect()
    const hit = document.elementFromPoint(r.left + r.width / 2, r.top + r.height / 2)
    // The box's whole strip, hint line included -- `border-t` is what the
    // component puts on it and nothing else in the shell has one.
    const strip = box.closest('div.border-t')!.getBoundingClientRect()
    return {
      rows: rows.length,
      bottom: r.bottom,
      viewport: window.innerHeight,
      boxTop: strip.top,
      boxBottom: strip.bottom,
      onTheRow: hit === last || (hit !== null && last.contains(hit)),
      hitBy: hit === null ? 'nothing' : `${hit.tagName}.${hit.className}`,
    }
  })

  expect(seen, 'the page rendered no terminal rows, or no reply box').not.toBeNull()
  expect(
    seen!.onTheRow,
    `the terminal's last row is covered: a click at its centre would land on ${seen!.hitBy}. ` +
      'That row is the prompt and the agent\'s question -- the line you are reading while ' +
      'you type the answer.',
  ).toBe(true)
  expect(
    seen!.bottom,
    `the last row ends at ${seen!.bottom}px, past the ${seen!.viewport}px viewport`,
  ).toBeLessThanOrEqual(seen!.viewport + 1)
  expect(
    seen!.bottom,
    `the last row ends at ${seen!.bottom}px and the box starts at ${seen!.boxTop}px, so the ` +
      'box is lying over the terminal rather than taking space of its own',
  ).toBeLessThanOrEqual(seen!.boxTop + 1)
  // And the trade is honest in the other direction too: the strip the terminal
  // gave its rows to is itself entirely on screen, hint line and all. The shell
  // is `overflow-hidden`, so a box pushed past the fold would simply be cut off
  // with nothing scrolling to reach it.
  expect(
    seen!.boxBottom,
    `the reply box ends at ${seen!.boxBottom}px, past the ${seen!.viewport}px viewport`,
  ).toBeLessThanOrEqual(seen!.viewport + 1)
})

test('a browser that left the box open gets it back, and not the keyboard', async ({
  page,
  tmuxWeb,
}) => {
  await enroll(page, tmuxWeb, 'laptop')
  const pane = await (async () => {
    await openReplyBox(page)
    const hint = page.getByText(/Enter sends to/)
    await expect(hint).toContainText(/%\d+/)
    return /%\d+/.exec(await hint.innerText())![0]
  })()
  await waitForShell(page, tmuxWeb, pane)

  // Every focus this load, in order, recorded from before the app runs.
  //
  // Asking `toBeFocused()` afterwards is not enough and the measurement proved
  // it: `<Terminal>` grabs the keyboard once, when the socket goes live, which
  // is *after* the box has mounted -- so a box that stole focus on mount has
  // already had it taken back by the time any assertion can look, and an
  // `autoFocus` wired to a constant survives. What it cannot survive is the
  // record: a phone would have raised its soft keyboard in that window.
  await page.addInitScript(() => {
    const log: string[] = []
    ;(window as unknown as { __focusLog: string[] }).__focusLog = log
    document.addEventListener('focusin', (event) => {
      const el = event.target as Element | null
      log.push(`${el?.tagName ?? '?'}:${el?.getAttribute?.('aria-label') ?? ''}`)
    })
  })

  await page.reload()
  await page.locator('.term-row').first().waitFor()
  await pill(page).waitFor({ state: 'detached' })

  // The memory is this device's, in localStorage: a phone keeps the box and a
  // laptop keeps it shut.
  await expect(
    replyBox(page),
    'the box was not remembered across a reload, so every visit costs the toggle again',
  ).toBeVisible()

  // And it must not have taken the keyboard on the way in -- not for a moment.
  // Nobody asked for anything this load; on a phone a box that focused itself
  // here would raise the soft keyboard on every single visit.
  await expect(replyBox(page)).not.toBeFocused()
  const focused = await page.evaluate(
    () => (window as unknown as { __focusLog: string[] }).__focusLog,
  )
  expect(
    focused.filter((f) => f.includes('Reply to this pane')),
    `the remembered-open box took focus on load. Focus went, in order, to ${focused.join(' → ')}. ` +
      'It is only not focused now because the terminal took the keyboard back when the socket ' +
      'went live, which on a phone is a keyboard that rose and fell on its own.',
  ).toEqual([])

  // Proved on tmux's side rather than by reading `document.activeElement`: type
  // without clicking anything, and the bytes have to reach the pane. If the box
  // had stolen focus they would be sitting in a textarea instead.
  await page.keyboard.type('echo REST""ORED')
  await page.keyboard.press('Enter')
  await expect
    .poll(() => capture(tmuxWeb, pane), {
      message:
        `pane ${pane} never ran what was typed after the reload: the remembered-open box ` +
        'took the keyboard from the terminal',
    })
    .toContain('RESTORED')
})

test('Escape puts the box away and gives the keyboard back to the pane', async ({
  page,
  tmuxWeb,
}) => {
  await enroll(page, tmuxWeb, 'laptop')
  await openReplyBox(page)
  const pane = await replyTarget(page)
  await waitForShell(page, tmuxWeb, pane)

  await replyBox(page).click()
  await replyBox(page).press('Escape')

  // Gone, not merely blurred: the box owns rows of a window the owner's own
  // client shares, so leaving it on screen with the caret elsewhere leaves his
  // terminal short for nothing.
  await expect(
    replyBox(page),
    'Escape only blurred the box, which still costs the terminal its rows',
  ).toHaveCount(0)

  await page.keyboard.type('echo ESC""APED')
  await page.keyboard.press('Enter')
  await expect
    .poll(() => capture(tmuxWeb, pane), {
      message:
        `pane ${pane} never ran what was typed after Escape: the caret was left on <body>, ` +
        'where nothing the user types goes anywhere',
    })
    .toContain('ESCAPED')
})

test('the palette shows the box too, and hands it the keyboard', async ({ page, tmuxWeb }) => {
  // Every other control in that header row has a palette row, and on a phone
  // the palette is the roomier surface of the two. This one also has to survive
  // the dialog closing over it: Radix restores focus on close, after the box has
  // mounted and taken it, and in this app that restore lands on `<body>` -- so a
  // row that opened a text box and left the caret nowhere would be worse than no
  // row at all. See `keepFocus` in Palette.tsx.
  await enroll(page, tmuxWeb, 'laptop')

  await page.keyboard.press('Control+Alt+k')
  await page.getByPlaceholder('Jump to session/window/pane…').fill('reply box')
  await page.keyboard.press('Enter')

  await expect(replyBox(page), 'the palette row did not show the reply box').toBeVisible()
  await expect(
    replyBox(page),
    'the palette opened the box and then let the dialog take the keyboard back, which ' +
      'leaves the caret on <body> where nothing typed goes anywhere',
  ).toBeFocused()
})
