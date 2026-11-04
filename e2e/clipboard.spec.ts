/**
 * Copying out of a pane, end to end.
 *
 * This is the only place the whole chain is visible at once, and the chain is
 * the whole point: tmux decides a copy happened, tmux encodes it as OSC 52,
 * the daemon forwards the bytes, `lib/osc52.ts` finds the sequence in the
 * stream, and `lib/clipboard.ts` puts it on the clipboard of a real browser.
 * Every link is unit-tested; none of the unit tests can tell you that tmux is
 * really emitting the sequence, which is the fact the feature rests on.
 *
 * It was reported as "highlighting text does not copy any more". The cause was
 * never the selection: `mouse on` hands the drag to tmux, so the browser gets
 * no DOM selection -- but tmux sends the result on, and the browser was
 * throwing it away because @wterm/dom has no OSC hook.
 */
import { test, expect, enroll, BASE_SESSION } from './harness'

// Chrome gives the focused tab `clipboard-write` and lets a write through with
// no user gesture; that is what makes this feature work at all on a desktop.
// iOS Safari is stricter and can refuse, which is what the toast with a Copy
// button in `Terminal.tsx` is for -- a tap on it IS a gesture. That path cannot
// be driven from here, so it is covered by `lib/clipboard.test.ts` instead.
test.use({ permissions: ['clipboard-read', 'clipboard-write'] })

test('a copy made in tmux lands on the browser clipboard', async ({ page, tmuxWeb }) => {
  await enroll(page, tmuxWeb, 'clipboard')

  // Unique, so a stale clipboard from another test cannot pass this one.
  const probe = `clip-probe-${Date.now()}`
  tmuxWeb.tmux('send-keys', '-t', `${BASE_SESSION}:0`, `printf '${probe}\\n'`, 'Enter')
  await expect
    .poll(() => tmuxWeb.tmux('capture-pane', '-p', '-t', `${BASE_SESSION}:0`), {
      message: 'the probe line never appeared in the pane',
    })
    .toContain(probe)

  // The copy-mode path, not `set-buffer -w`: a selection is what the user
  // makes, and `set-buffer -w` emits OSC 52 even with set-clipboard off, so it
  // would pass without proving anything about copying.
  tmuxWeb.tmux('copy-mode', '-t', `${BASE_SESSION}:0`)
  tmuxWeb.tmux('send-keys', '-X', '-t', `${BASE_SESSION}:0`, 'search-backward', probe)
  tmuxWeb.tmux('send-keys', '-X', '-t', `${BASE_SESSION}:0`, 'select-line')
  tmuxWeb.tmux('send-keys', '-X', '-t', `${BASE_SESSION}:0`, 'copy-selection-and-cancel')

  await expect
    .poll(() => page.evaluate(() => navigator.clipboard.readText()), {
      message: 'tmux copied a line and it never reached the browser clipboard',
      timeout: 15_000,
    })
    .toContain(probe)
})

// The guard on the other side of the same feature. A sequence asking to CLEAR
// the clipboard must not be honoured: a stray one in a pane's output would
// silently wipe whatever the user had copied, and output is not something they
// control.
test('a pane cannot wipe the clipboard', async ({ page, tmuxWeb }) => {
  await enroll(page, tmuxWeb, 'clipboard-clear')

  await page.evaluate(() => navigator.clipboard.writeText('something the user copied earlier'))

  // Written by the pane itself, which is the threat: any program can print this.
  tmuxWeb.tmux('send-keys', '-t', `${BASE_SESSION}:0`, `printf '\\\\033]52;c;\\\\033\\\\\\\\'`, 'Enter')
  await expect
    .poll(() => tmuxWeb.tmux('capture-pane', '-p', '-t', `${BASE_SESSION}:0`), {
      message: 'the clearing sequence was never written',
    })
    .toContain('52;c;')

  // Give the bytes time to arrive and be scanned, then assert nothing changed.
  await page.waitForTimeout(2000)
  expect(await page.evaluate(() => navigator.clipboard.readText())).toBe(
    'something the user copied earlier',
  )
})
