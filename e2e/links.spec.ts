import { chmodSync, mkdtempSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { BASE_SESSION, enroll, expect, test } from './harness'

const PR = 'https://github.com/example/project/pull/13914'

/** Writes a script that emits `body`, since send-keys mangles ESC \ terminators. */
function emitter(body: string): string {
  const sh = join(mkdtempSync(join(tmpdir(), 'osc8-')), 'emit.sh')
  writeFileSync(sh, `#!/bin/sh\n${body}\n`)
  chmodSync(sh, 0o755)
  return sh
}


/**
 * Click a character inside `needle` on the row containing it.
 *
 * Rows are padded to the full terminal width, so clicking a row's centre lands
 * in trailing whitespace -- which silently makes a link test pass for the wrong
 * reason. The x is computed from the character's position in the row.
 */
async function clickInside(
  page: import('@playwright/test').Page,
  needle: string,
  modifiers: Array<'Shift' | 'Control' | 'Meta'> = [],
) {
  const row = page.locator('.term-row', { hasText: needle.slice(0, 30) }).first()
  await row.waitFor({ state: 'visible', timeout: 8000 })
  const box = await row.boundingBox()
  const text = (await row.textContent()) ?? ''
  const at = text.indexOf(needle)
  if (!box || at < 0) throw new Error(`row not found for ${needle}`)
  // Middle of the character three past the start of the match.
  const x = ((at + 3.5) * box.width) / text.length
  await row.click({ position: { x, y: box.height / 2 }, modifiers })
}

test('an OSC 8 hyperlink becomes a real anchor', async ({ page, tmuxWeb }) => {
  await enroll(page, tmuxWeb, 'laptop')
  const sh = emitter(`printf '\\033]8;id=x;${PR}\\033\\\\#13914\\033]8;;\\033\\\\\\n'`)
  tmuxWeb.tmux('send-keys', '-t', BASE_SESSION, sh, 'Enter')

  const link = page.locator('a.term-link').first()
  await expect(link).toBeVisible({ timeout: 8000 })
  await expect(link).toHaveAttribute('href', PR)
  await expect(link).toHaveAttribute('rel', 'noopener noreferrer')
  // The visible text is what the program chose, not the URL. Deriving the
  // target from the text would open the wrong thing.
  await expect(link).toHaveText('#13914')
})

test('shift+click opens an OSC 8 link', async ({ page, tmuxWeb }) => {
  await enroll(page, tmuxWeb, 'laptop')
  const sh = emitter(`printf '\\033]8;id=x;${PR}\\033\\\\#13914\\033]8;;\\033\\\\\\n'`)
  tmuxWeb.tmux('send-keys', '-t', BASE_SESSION, sh, 'Enter')
  await expect(page.locator('a.term-link').first()).toBeVisible({ timeout: 8000 })

  const [popup] = await Promise.all([
    page.waitForEvent('popup'),
    page.locator('a.term-link').first().click({ modifiers: ['Shift'] }),
  ])
  expect(popup.url()).toBe(PR)
})

test('shift+click opens a plain-text URL that is not a hyperlink', async ({ page, tmuxWeb }) => {
  await enroll(page, tmuxWeb, 'laptop')
  // No OSC 8 at all -- the case tools that do not emit hyperlinks produce.
  const sh = emitter(`printf 'see ${PR} for details\\n'`)
  tmuxWeb.tmux('send-keys', '-t', BASE_SESSION, sh, 'Enter')

  const cell = page.getByText('for details', { exact: false }).first()
  await expect(cell).toBeVisible({ timeout: 8000 })
  await expect(page.locator('a.term-link')).toHaveCount(0)

  const [popup] = await Promise.all([
    page.waitForEvent('popup'),
    clickInside(page, PR, ['Shift']),
  ])
  expect(popup.url()).toBe(PR)
})

test('a plain click still belongs to tmux, not the link opener', async ({ page, tmuxWeb }) => {
  await enroll(page, tmuxWeb, 'laptop')
  const sh = emitter(`printf 'see ${PR} for details\\n'`)
  tmuxWeb.tmux('send-keys', '-t', BASE_SESSION, sh, 'Enter')
  await expect(page.getByText('for details', { exact: false }).first()).toBeVisible({
    timeout: 8000,
  })

  let popped = false
  page.on('popup', () => {
    popped = true
  })
  // Deliberately the same spot the shift+click test uses. Clicking the row's
  // centre would land in trailing padding and pass for the wrong reason.
  await clickInside(page, PR)
  await page.waitForTimeout(500)
  expect(popped, 'an unmodified click must reach tmux, not open a browser tab').toBe(false)
})
