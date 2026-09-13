/**
 * Agent state, end to end: a real pane running a real process, captured and
 * classified by a real daemon, drawn by a real browser into a real tab.
 *
 * Everything each layer *decides* is already tested where it lives -- the
 * classifier's hashes and settle counter in Go, the sidebar's dots and the
 * badge's arithmetic under vitest. What no other layer can reach is the wiring
 * between them, and it is exactly the wiring these three tests exercise:
 *
 *  - that a screen which stops changing on a real tmux server settles to idle,
 *    and starts reading working again when it moves,
 *  - that `useTabBadge`'s two effects actually write `document.title` and
 *    `<link rel="icon">`, and read the icon once rather than once per poll,
 *  - that `useSeenPanes`'s effect persists to localStorage, so a device that
 *    has looked at a finished run does not get told about it again on reload.
 *
 * The agent is `internal/tmux/testutil.FakeAgent`'s trick in TypeScript: a copy
 * of `cat` named `claude`, which the *real* `tmux.Agents` list matches on
 * `pane_current_command`. The approval box is the captured screen in
 * `internal/tmux/testdata`, so what the daemon's grammar reads here is a
 * verbatim Claude Code dialog and not a shape invented to match the detector.
 */

import * as path from 'node:path'

import {
  BASE_SESSION,
  BASE_WINDOW,
  enroll,
  expect,
  pill,
  repoRoot,
  sessionLabel,
  stateDot,
  test,
  windowRow,
} from './harness'
import type { TmuxWeb } from './harness'
import type { Locator, Page } from '@playwright/test'

/** A verbatim Claude Code permission dialog, as captured on a live machine. */
const BLOCKED_SCREEN = path.join(repoRoot, 'internal', 'tmux', 'testdata', 'claude-blocked.txt')
/** The line that dialog asks, which is what the daemon lifts out of it. */
const QUESTION = 'Do you want to create fixture.txt?'

/** The icon `index.html` links, and the one `useTabBadge` draws its dot on. */
const FAVICON = '/favicon.svg'
/** `#f59e0b` and `#10b981` as they survive `encodeURIComponent`. */
const AMBER = '%23f59e0b'
const EMERALD = '%2310b981'

/** The tab's `<link rel="icon">`, which the badge effect rewrites in place. */
function icon(page: Page): Locator {
  return page.locator('link[rel="icon"]')
}

/** Everything under one session's heading in the sidebar. */
function group(page: Page, session: string): Locator {
  return page.locator('[data-slot="sidebar-group"]').filter({ hasText: session })
}

/**
 * Make an agent pane redraw the way a working one does, until the returned
 * function is called.
 *
 * The fake agent is `cat`, so a `send-keys` puts a new line on its screen --
 * which is precisely the signal the daemon classifies: a capture that differs
 * from the previous poll's. Faster than the 1.5s poll on purpose, so that no
 * poll can land between two redraws and read the pane as still.
 */
function churn(tmuxWeb: TmuxWeb, target: string): () => void {
  let n = 0
  const timer = setInterval(() => {
    try {
      tmuxWeb.tmux('send-keys', '-t', target, `tick-${n++}`, 'Enter')
    } catch {
      // The pane went away, which is a teardown race and not a failure. The
      // assertions say whether that mattered.
    }
  }, 300)
  return () => clearInterval(timer)
}

test('an agent pane reads working while it redraws and idle when it stops', async ({
  page,
  tmuxWeb,
}) => {
  await enroll(page, tmuxWeb, 'laptop')
  tmuxWeb.tmux('new-window', '-d', '-t', BASE_SESSION, '-n', 'agent', tmuxWeb.fakeAgent('claude'))

  const row = windowRow(page, 'agent')
  await expect(row).toBeVisible()
  // tmux's own answer before any assertion about the browser: this pane really
  // is running something the daemon's Agents list matches, so what the dot says
  // below is about a genuine agent pane and not about a row that happens to
  // carry a badge.
  expect(
    tmuxWeb.tmux('list-panes', '-t', `${BASE_SESSION}:agent`, '-F', '#{pane_current_command}'),
  ).toBe('claude')

  // Idle FIRST, and the order is the whole test. A pane's first observation
  // always reports working -- there is nothing to compare it against -- so
  // "create an agent pane, see working" would pass against a daemon that never
  // captured a screen at all. Waiting for idle is what proves a capture was
  // taken twice and compared.
  await expect(stateDot(row)).toHaveAttribute('data-agent-state', 'idle')

  const stop = churn(tmuxWeb, `${BASE_SESSION}:agent`)
  try {
    await expect(stateDot(row)).toHaveAttribute('data-agent-state', 'working')
  } finally {
    stop()
  }
  // And back to still. `done` rather than `idle` because this device has not
  // been shown that finish yet -- it is the daemon's idle, refined by the
  // browser's own memory (the third test in this file is about that half). It
  // is the stronger assertion of the two: a pane can only read `done` if the
  // daemon settled it *and* stamped a working -> idle edge for the run above.
  await expect(stateDot(row)).toHaveAttribute('data-agent-state', 'done')

  // The rule the whole feature rests on: a shell is not idle, it is a shell.
  // The base window runs `sh` and carries no dot at all.
  await expect(stateDot(windowRow(page, BASE_WINDOW))).toHaveCount(0)
})

/**
 * The row layout, which nothing under vitest can see.
 *
 * `react-dom/server` runs no effects and gives nothing a size, so every rule
 * this rework rests on -- that the title is cut, that it is quieter than the
 * name, that it moves on hover, and above all that it moves *only when it is
 * too long* -- is invisible there. The unit tests pin which text lands on which
 * line; this pins what the browser does with it.
 *
 * The marquee is checked by seeking its animation rather than by waiting for
 * it: a screenshot-timed assertion on a 5s pendulum is a flake generator, and
 * the interesting value is the one at the far end of the travel, which is
 * exactly what `currentTime` can be moved to.
 */
test('a pane title gets a line of its own, cut, and scrolled only when it is too long', async ({
  page,
  tmuxWeb,
}) => {
  await enroll(page, tmuxWeb, 'laptop')
  const claude = tmuxWeb.fakeAgent('claude')

  // Long enough to be cut in a 16rem sidebar at 12px, and short enough to be a
  // title a coding agent would really write.
  const LONG = '✳ Categorización de productos de southafrica en el catálogo'
  // What the row is expected to *show*: claude's own glyph comes off the front,
  // because the row is already saying which agent this is in the mark beside
  // the text -- see `AGENT_TITLE_PREFIXES` in AppSidebar. tmux is still told
  // the whole thing, so this also pins that the strip is display only.
  const LONG_SHOWN = 'Categorización de productos de southafrica en el catálogo'
  // Not hostname-shaped -- it has a space -- so it is shown, and comfortably
  // narrower than the row.
  const SHORT = 'in tests'

  tmuxWeb.tmux('new-window', '-d', '-t', BASE_SESSION, '-n', 'catalog', claude)
  tmuxWeb.tmux('select-pane', '-t', `${BASE_SESSION}:catalog`, '-T', LONG)
  tmuxWeb.tmux('new-window', '-d', '-t', BASE_SESSION, '-n', 'unit', claude)
  tmuxWeb.tmux('select-pane', '-t', `${BASE_SESSION}:unit`, '-T', SHORT)
  // A split window, whose pane rows are a different button with a different
  // set of shadcn defaults -- notably no width of its own, since a `<button>`
  // is shrink-to-fit.
  tmuxWeb.tmux('new-window', '-d', '-t', BASE_SESSION, '-n', 'split', claude)
  tmuxWeb.tmux('select-pane', '-t', `${BASE_SESSION}:split.0`, '-T', LONG)
  tmuxWeb.tmux('split-window', '-d', '-v', '-t', `${BASE_SESSION}:split`, 'sh')
  // A second session, so there are two blocks to keep apart.
  // Named `scratch`, not `shell`: `windowRow` finds a row by its name, and a
  // second window called `shell` would make the base one ambiguous.
  tmuxWeb.tmux('new-session', '-d', '-s', 'notes', '-n', 'scratch', 'sh')

  const long = windowRow(page, 'catalog')
  const short = windowRow(page, 'unit')
  await expect(long).toContainText(LONG_SHOWN)
  await expect(long).not.toContainText('✳')
  await expect(short).toContainText(SHORT)

  // A second line, not the capsule the command has. The capsule is `h-5` and
  // sits on the first line; the title's box starts below the name's.
  const name = long.locator('.truncate').first()
  const line = long.locator('.row-line')
  const nameBox = (await name.boundingBox())!
  const lineBox = (await line.boundingBox())!
  expect(lineBox.y, 'the title is under the name, not beside it').toBeGreaterThanOrEqual(
    nameBox.y + nameBox.height,
  )
  // And the row grew to hold it. shadcn pins these buttons to a fixed height
  // and hides their overflow, so a row that did not give that up would draw the
  // second line into a box that clips it -- visible to a person and to nothing
  // else.
  const rowBox = (await long.boundingBox())!
  expect(lineBox.y + lineBox.height, 'the row is tall enough for the line it drew').toBeLessThanOrEqual(
    rowBox.y + rowBox.height + 0.5,
  )
  // The line has the row to itself, rather than the leftovers of the name's
  // width: that width is the entire reason the title left the capsule.
  expect(lineBox.width).toBeGreaterThan(rowBox.width * 0.7)

  // Quieter than the name: a step smaller, and a different colour. If the
  // dimming were dropped the line would simply inherit the row's own colour,
  // which is the name's -- so this is an equality that has to fail.
  const tone = await long.evaluate((row) => {
    const label = row.querySelector('.truncate')!
    const title = row.querySelector('.row-line')!
    const a = getComputedStyle(label)
    const b = getComputedStyle(title)
    return {
      name: { size: parseFloat(a.fontSize), color: a.color },
      title: { size: parseFloat(b.fontSize), color: b.color },
    }
  })
  expect(tone.title.size).toBeLessThan(tone.name.size)
  expect(tone.title.color).not.toBe(tone.name.color)

  // Cut with an ellipsis: overflowing, clipped, on one line, and told to draw
  // the mark. All four, because any one of them alone is satisfiable by a row
  // that does not show an ellipsis at all.
  const cut = await line.evaluate((el) => {
    const inner = el.firstElementChild as HTMLElement
    const cs = getComputedStyle(inner)
    return {
      overflowing: inner.scrollWidth - inner.clientWidth,
      overflow: cs.overflow,
      whiteSpace: cs.whiteSpace,
      textOverflow: cs.textOverflow,
    }
  })
  expect(cut.overflowing).toBeGreaterThan(0)
  expect(cut.overflow).toBe('hidden')
  expect(cut.whiteSpace).toBe('nowrap')
  expect(cut.textOverflow).toBe('ellipsis')

  /**
   * Hover the row, seek its marquee to the far end of the travel, and report
   * how far it went -- and how far it had to go.
   */
  async function travel(row: Locator): Promise<{ moved: number; needed: number }> {
    await row.hover()
    return await row.locator('.row-line').evaluate((el) => {
      const inner = el.firstElementChild as HTMLElement
      const anims = inner.getAnimations()
      if (anims.length === 0) return { moved: NaN, needed: NaN }
      for (const a of anims) {
        a.pause()
        // Past the 400ms delay and a whole 5s iteration: the end of the first
        // pass, which `alternate` makes the far end of the swing.
        a.currentTime = 5_400
      }
      const moved = new DOMMatrixReadOnly(getComputedStyle(inner).transform).m41
      // The clamp is dropped while hovering, so this is now the natural width.
      return { moved, needed: el.clientWidth - inner.getBoundingClientRect().width }
    })
  }

  // The long one travels, and travels exactly as far as it is over -- which is
  // the whole of the `min(0px, 100cqw - 100%)` trick working in a real engine.
  const far = await travel(long)
  expect(far.moved, 'a cut title scrolls').toBeLessThan(-20)
  expect(far.moved).toBeCloseTo(far.needed, 0)

  // And the short one does not move at all, though it is running the same
  // animation. This is the assertion the whole approach exists for: a marquee
  // that ran regardless would wobble every short title in the tree.
  const near = await travel(short)
  expect(near.moved, 'the short title is animating too').not.toBeNaN()
  expect(near.moved, 'a title that fits does not move').toBe(0)

  // Reduced motion gets no marquee -- and still gets the whole title, on the
  // native tooltip, which is the reason it can be dropped rather than slowed.
  await page.emulateMedia({ reducedMotion: 'reduce' })
  const still = await travel(long)
  expect(still.moved, 'no marquee under prefers-reduced-motion').toBeNaN()
  await expect(long.locator('.row-line')).toHaveAttribute('title', LONG_SHOWN)
  await page.emulateMedia({ reducedMotion: 'no-preference' })

  // The pane rows of a split window are a different button with different
  // defaults, and the one that matters is that a `<button>` is shrink-to-fit:
  // without a width of its own its second line is only as wide as the words
  // above it, which is the cramping this rework is about.
  const subRow = page.getByRole('button', { name: /pane 0/ })
  const subBox = (await subRow.boundingBox())!
  const subLine = (await subRow.locator('.row-line').boundingBox())!
  expect(subLine.width, 'a pane row gives its title the full row too').toBeGreaterThan(
    subBox.width * 0.7,
  )

  // The blocks. A rule between session groups and none above the first: 1px of
  // height, no width -- which is what makes it affordable on a phone, where an
  // indent or a gutter would not be.
  const rules = await page
    .locator('[data-slot="sidebar-group"]')
    .evaluateAll((groups) => groups.map((g) => getComputedStyle(g).borderTopWidth))
  expect(rules.length).toBeGreaterThan(1)
  expect(rules[0], 'nothing is ruled off above the first group').toBe('0px')
  expect(rules.slice(1).every((w) => w !== '0px'), `border widths: ${rules}`).toBe(true)

  // The other half of the layout, unchanged: a shell says what program it is
  // running, in a capsule, in a monospaced face, on the row's own line. No
  // second line is spent to print `sh`.
  const shell = windowRow(page, BASE_WINDOW)
  await expect(shell.locator('.row-line')).toHaveCount(0)
  // `:not([data-row-branch])` because the row carries a second badge whenever
  // the pane is inside a work tree, and this harness's panes inherit the
  // checkout the suite is run from -- so on this repository it always is one.
  const capsule = shell.locator('[data-slot="badge"]:not([data-row-branch])')
  await expect(capsule).toHaveText('sh')
  expect(await capsule.evaluate((el) => getComputedStyle(el).fontFamily)).toMatch(/mono/i)
})

test('a blocked agent rolls up to its window and session, and badges the tab', async ({
  page,
  tmuxWeb,
}) => {
  await enroll(page, tmuxWeb, 'laptop')

  // Counted from here, for the "read once" assertion further down. The tab has
  // already fetched the icon to display it; what must not happen is another
  // fetch on every poll for as long as something is blocked.
  let favicons = 0
  page.on('request', (req) => {
    if (new URL(req.url()).pathname === FAVICON) favicons++
  })

  // A session of its own, sized by hand and pinned there.
  //
  // A 100-column, 31-row approval box has to be *on screen* for the daemon to
  // read it: `capture-pane` returns the visible screen and nothing else, on
  // purpose -- a dialog that has scrolled into history is one that has already
  // been answered. And a window's size is not its session's business: with the
  // default `window-size latest` tmux sizes every window on this server from
  // the most recent client, which here is the headless browser, so splitting
  // this window twice would leave 10-row panes with the question scrolled off.
  // `manual` is what makes the screen these tests are about independent of the
  // viewport the suite happens to run at.
  tmuxWeb.tmux('new-session', '-d', '-s', 'agents', '-n', 'shell', '-x', '120', '-y', '120', 'sh')
  tmuxWeb.tmux('set-option', '-t', 'agents', 'window-size', 'manual')
  tmuxWeb.tmux('new-window', '-d', '-t', 'agents', '-n', 'bot', 'sh')
  tmuxWeb.tmux('resize-window', '-t', 'agents:bot', '-x', '120', '-y', '120')
  // The agent is the second pane of the second window, so both roll-ups have
  // something to roll up over: a window that reported its first pane's state,
  // or a session that reported its first window's, would read as nothing here.
  //
  // `cat <dialog>; exec claude` puts the captured screen up and then holds the
  // pane open under the agent's name -- the dialog is what is on the pane when
  // the daemon captures it.
  tmuxWeb.tmux(
    'split-window',
    '-d',
    '-v',
    '-t',
    'agents:bot',
    `sh -c 'cat ${BLOCKED_SCREEN}; exec ${tmuxWeb.fakeAgent('claude')}'`,
  )

  // Again, tmux first: the approval box really is on that pane's screen.
  await expect
    .poll(() => tmuxWeb.tmux('capture-pane', '-p', '-J', '-t', 'agents:bot.1'))
    .toContain(QUESTION)

  const agents = group(page, 'agents')
  const pane = agents.getByRole('button', { name: /pane 1/ })
  await expect(stateDot(pane)).toHaveAttribute('data-agent-state', 'blocked')
  // The roll-up, which is the journey: neither of these rows is the agent.
  await expect(stateDot(windowRow(page, 'bot'))).toHaveAttribute('data-agent-state', 'blocked')
  await expect(stateDot(sessionLabel(page, 'agents'))).toHaveAttribute(
    'data-agent-state',
    'blocked',
  )
  // Its sibling is a plain shell and still has nothing to say.
  await expect(stateDot(agents.getByRole('button', { name: /pane 0/ }))).toHaveCount(0)
  // What it is waiting for, lifted off the screen by the daemon's grammar.
  await expect(pane).toContainText(QUESTION)

  // The tab badge's two effects, which no unit test in this repository can
  // reach: `react-dom/server` runs neither.
  await expect(page).toHaveTitle('(1) tmux-web')
  await expect(icon(page)).toHaveAttribute('href', new RegExp(`^data:image/svg\\+xml,.*${AMBER}`))

  // Read once, and lazily: two fetches at most, the browser's own load of the
  // icon and `readIconSource`'s.
  expect(favicons, 'the icon is fetched once, lazily').toBeLessThanOrEqual(2)
  const readsSoFar = favicons

  // A second agent raises its own box. The count is what moves -- and it is the
  // thing that would re-run a read guarded on the count rather than on "has one
  // been read at all", which is the regression the `source !== null` guard in
  // `useTabBadge` exists to prevent. Several polls also go by in here with the
  // badge up, so a read on every poll would show up too.
  tmuxWeb.tmux(
    'split-window',
    '-d',
    '-v',
    '-t',
    'agents:bot',
    `sh -c 'cat ${BLOCKED_SCREEN}; exec ${tmuxWeb.fakeAgent('claude')}'`,
  )
  await expect
    .poll(() => tmuxWeb.tmux('capture-pane', '-p', '-J', '-t', 'agents:bot.1'))
    .toContain(QUESTION)
  await expect(page).toHaveTitle('(2) tmux-web')
  expect(favicons, 'the icon must not be re-read when the count changes').toBe(readsSoFar)

  // And both halves go back when nothing needs you. The panes are killed rather
  // than answered: an answered dialog leaves a pane that has just changed, and
  // three seconds later that is a finished run with a `done` badge of its own.
  tmuxWeb.tmux('kill-window', '-t', 'agents:bot')
  await expect(windowRow(page, 'bot')).toHaveCount(0)
  await expect(stateDot(sessionLabel(page, 'agents'))).toHaveCount(0)
  await expect(page).toHaveTitle('tmux-web')
  await expect(icon(page)).toHaveAttribute('href', FAVICON)
})

test('a finished run badges the tab until this device looks, and stays looked-at', async ({
  page,
  tmuxWeb,
}) => {
  await enroll(page, tmuxWeb, 'laptop')

  // A second tab, in the same context so it shares the cookie and localStorage
  // but not sessionStorage. It exists to hold a terminal socket open while the
  // first tab reloads: the daemon throws its classifier away the moment no
  // browser is connected, and with it the `finishedAt` this test is about. With
  // one tab, a reload would clear the badge whether or not anything was
  // remembered, and the assertion at the end would be vacuous.
  const other = await page.context().newPage()
  await other.goto(tmuxWeb.baseURL + '/')
  await other.locator('.term-row').first().waitFor()
  // Live, not merely loaded: it is the open socket that matters here, and the
  // status pill is rendered only while there is not one.
  await pill(other).waitFor({ state: 'detached' })

  tmuxWeb.tmux('new-window', '-d', '-t', BASE_SESSION, '-n', 'agent', tmuxWeb.fakeAgent('claude'))
  const paneId = tmuxWeb.tmux('list-panes', '-t', `${BASE_SESSION}:agent`, '-F', '#{pane_id}')
  const row = windowRow(page, 'agent')
  await expect(stateDot(row)).toHaveAttribute('data-agent-state', 'idle')

  // A run: the screen moves, then stops. Only a run that was seen to change
  // earns a finish edge, which is why the pane has to churn before it settles.
  const stop = churn(tmuxWeb, `${BASE_SESSION}:agent`)
  try {
    await expect(stateDot(row)).toHaveAttribute('data-agent-state', 'working')
  } finally {
    stop()
  }

  await expect(stateDot(row)).toHaveAttribute('data-agent-state', 'done')
  await expect(page).toHaveTitle('(1) tmux-web')
  await expect(icon(page)).toHaveAttribute('href', new RegExp(`^data:image/svg\\+xml,.*${EMERALD}`))

  // Looking at it is what clears it, and nothing else does -- the daemon is
  // never told, so every device clears its own.
  await row.click()
  await expect(stateDot(row)).toHaveAttribute('data-agent-state', 'idle')
  await expect(page).toHaveTitle('tmux-web')
  await expect(icon(page)).toHaveAttribute('href', FAVICON)

  // The effect behind that: `useSeenPanes` writing the map to localStorage,
  // keyed on the tmux server's generation so a pane id reused after a restart
  // cannot inherit it.
  const stored = JSON.parse(
    (await page.evaluate(() => globalThis.localStorage.getItem('tmux-web:seen'))) ?? '{}',
  ) as Record<string, number>
  const key = Object.keys(stored).find((k) => k.endsWith(`:${paneId}`))
  expect(key, `seen = ${JSON.stringify(stored)}`).toBeDefined()
  expect(key).not.toBe(`:${paneId}`) // a generation, not a bare pane id
  expect(stored[key!]).toBeGreaterThan(0)

  // Move off the pane before reloading. Otherwise the reloaded tab would be
  // looking straight at it again -- which clears the badge on its own, whether
  // or not anything was ever persisted.
  await windowRow(page, BASE_WINDOW).click()
  await page.reload()
  await page.locator('.term-row').first().waitFor()

  // The whole point: this device was shown that finish once and is not shown it
  // again. The other tab kept the daemon's `finishedAt` alive across the
  // reload, so `done` is available to be computed and is not computed.
  await expect(stateDot(row)).toHaveAttribute('data-agent-state', 'idle')
  await expect(page).toHaveTitle('tmux-web')
})
