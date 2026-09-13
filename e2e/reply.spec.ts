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
 */

import type { Locator, Page } from '@playwright/test'

import { enroll, expect, focusTerminal, test } from './harness'
import type { TmuxWeb } from './harness'

/** The box itself. */
function replyBox(page: Page): Locator {
  return page.getByRole('textbox', { name: 'Reply to this pane' })
}

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
