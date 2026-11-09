# Scroll Gain Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** One wheel notch buys the number of lines its actual receiver scrolls, so a swipe over an agent pane tracks the finger instead of moving a fifth as far.

**Architecture:** The daemon publishes the two tmux facts that decide tmux's own `WheelUpPane` fork (`#{alternate_on}`, `#{mouse_any_flag}`); they ride the snapshot record and the pane tree to `App`, which reports the active pane's facts — plus the mode it has *decided* the pane is in — to a module-level store; `installSwipeScroll` reads lines-per-notch from that store through an injected getter at the start of each gesture and multiplies the row height by it.

**Tech Stack:** Go (`internal/tmux`), React + TypeScript (`web/src`), vitest, Playwright.

**Spec:** `docs/plans/2026-11-09-tmux-web-scroll-gain-design.md`

## Global Constraints

- `@tmux_web_label` **stays the last field** of the snapshot record; `#{pane_mode}` and the two new fields go before it (`snapshot.go` documents why at length).
- Both new fields are tmux-generated `0`/`1` and get **no sanitising** — unlike the label, nothing but tmux can write them.
- Measured on tmux 3.7b: an unrecognised format variable expands to **empty**, not to nothing (`display-message -p '[#{alternate_on}][#{mouse_any_flag}][#{no_such_xyz}]'` prints `[1][1][]`), so a typo produces a full-length row whose fact is silently `false` forever. Task 1's integration test is the only thing that can see that.
- The quantum table: `claude` → 1, `pi` → 1, `opencode` → 3, anything else → **3**. Keyed on `pane_current_command` and nothing else.
- No user-visible control, no new setting, no change to `~/.tmux.conf`, the `-N 5` binding or the `mouse` option.
- The five existing tests in `e2e/touchscroll.spec.ts` run over a shell pane and must keep passing **untouched**.
- Do not run `prettier`. `git add` explicit paths only.
- Commands: `make test-go`, `cd web && pnpm test`, `cd web && pnpm build` (the only typecheck), `make test-e2e`.

## Review Focus

- **A typo'd format variable** expands to empty, parses as `false`, and the gain silently stays at 5 for every pane forever — no unit test can see it. Pinned by Task 1's integration test, which flips both facts on a real pane and reads them back off a real snapshot.
- **A daemon too old to send the keys at all**: `row.alternateOn` is `undefined`, which must read as the tmux quantum rather than `NaN` pixels. Pinned in Task 2, the precedent being `windowId` being `""`.
- **A pane in a mode that is not a copy mode** (`tree-mode` on an agent pane): tmux handles that wheel itself, so the gain must be 5 even though `alternate_on` is 1 — the unrecoverable direction. Pinned in Task 3.
- **`rowHeight` of 0**, before the terminal has laid out: the fallback must be `FALLBACK_ROW_HEIGHT_PX × the receiver's lines`, not × 5. Pinned in Task 4.
- **A font size other than 14**: `pxPerNotch` multiplies the measured row, and the terminal settings feature now makes that variable; 20px text must move 20px rows. Pinned in Task 4.

---

### Task 1: The daemon publishes the two facts

**Files:**
- Modify: `internal/tmux/snapshot.go` (`Row`, `formatFields`, `fieldCount`, `ParseRows`)
- Test: `internal/tmux/snapshot_test.go`, new `internal/tmux/snapshot_wheel_integration_test.go`
- Fix up: `internal/tmux/report_test.go`, `internal/tmux/serverstart_internal_test.go:167,211`, `internal/tmux/snapshot_path_internal_test.go:420`

**Interfaces:**
- Produces: `tmux.Row.AlternateOn bool` (json `alternateOn`) and `tmux.Row.MouseAnyFlag bool` (json `mouseAnyFlag`); `fieldCount = 17`; record order `… #{pane_title}, #{pane_mode}, #{alternate_on}, #{mouse_any_flag}, labelField`.

- [ ] **Step 1: Write the failing test** — in `snapshot_test.go`, a `rec(...)` line carrying `"1"` and `"0"` in the two new slots:

```go
func TestParseRowsReadsTheWheelFacts(t *testing.T) {
	line := rec("w", "$0", "w", "%1", "0", "", "@1", "1", "win", "1", "claude", "t", "", "1", "0", "lbl")
	got, dropped, _ := ParseRows(line)
	// The facts that decide whether tmux scrolls or the application does.
	if len(got) != 1 || !got[0].AlternateOn || got[0].MouseAnyFlag {
		t.Fatalf("rows = %+v, dropped = %d", got, dropped)
	}
	if got[0].Label != "lbl" {
		t.Errorf("Label = %q: the label must still be the last field", got[0].Label)
	}
	// A record one field short is dropped, not read with the fields shifted.
	if _, d, _ := ParseRows(rec("w", "$0", "w", "%1", "0", "", "@1", "1", "win", "1", "claude", "t", "", "1", "0")); d != 1 {
		t.Errorf("a short record was accepted")
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./internal/tmux -run TestParseRowsReadsTheWheelFacts -count=1`
Expected: FAIL — `got[0].AlternateOn` undefined (the fields do not exist).

- [ ] **Step 3: Add the fields**

In `snapshot.go`: two `bool` fields on `Row` after `PaneMode` with the json names above and a comment saying what each decides (tmux's `WheelUpPane` fork, and that a `1` in either means the application receives the notch); `"#{alternate_on}"` and `"#{mouse_any_flag}"` into `formatFields` after `"#{pane_mode}"`; `fieldCount` 15 → 17; `AlternateOn: fields[14] == "1"` and `MouseAnyFlag: fields[15] == "1"` in `ParseRows`. The label keeps `fields[fieldCount-1:]`.

- [ ] **Step 4: Extend `TestFormatFieldCount`**

Add, in the style of the `#{pane_mode}` assertion already there, that each of `"#{alternate_on}"` and `"#{mouse_any_flag}"` is present at an index `< fieldCount-1`.

- [ ] **Step 5: Fix up every hand-built snapshot record**

The ~20 `rec(...)` calls in `snapshot_test.go` and `report_test.go` and the three literal lines in `serverstart_internal_test.go` and `snapshot_path_internal_test.go` each gain two `"0"` fields in the new slots, before the label.

Run: `go test ./internal/tmux -count=1`
Expected: PASS, with no row counted as dropped by a test that did not intend one.

- [ ] **Step 6: Write the failing integration test** — `snapshot_wheel_integration_test.go`, package `tmux_test`, following `snapshot_mode_integration_test.go` (a `testutil.NewServer`, a two-pane window with the target **inactive**, and `settledSnapshot` from `snapshot_label_integration_test.go`, same package):

The shell pane's row reads `AlternateOn == false && MouseAnyFlag == false`. Then `send-keys` into the target pane the line `printf '\033[?1049h\033[?1003h'` plus `Enter`, poll `display-message -p -t <target> '#{alternate_on}#{mouse_any_flag}'` until it is `"11"`, and assert the target's row in a fresh snapshot now reads `AlternateOn == true && MouseAnyFlag == true`. The test's comment says what it is for: a mistyped variable name expands to empty, so both facts would read `false` here and nowhere else.

- [ ] **Step 7: Run the suite**

Run: `make test-go`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/tmux/snapshot.go internal/tmux/snapshot_test.go internal/tmux/snapshot_wheel_integration_test.go internal/tmux/report_test.go internal/tmux/serverstart_internal_test.go internal/tmux/snapshot_path_internal_test.go
git commit -m "feat: the snapshot carries the two facts that decide who gets the wheel"
```

---

### Task 2: The facts reach the pane tree

**Files:**
- Modify: `web/src/lib/useSnapshot.ts` (`SnapshotRow`, `PaneNode`, `groupRows`, the `rowsEqual` comparison)
- Test: `web/src/lib/useSnapshot.test.ts`
- Fix up: `web/src/lib/tabBadge.test.ts`, `web/src/lib/manage.test.ts`, `web/src/components/Palette.test.tsx`, `web/src/components/CapturePanel.test.tsx`, `web/src/components/Terminal.test.tsx`, `web/src/components/AppSidebar.test.tsx`

**Interfaces:**
- Consumes: Task 1's json names `alternateOn`, `mouseAnyFlag`.
- Produces: `SnapshotRow.alternateOn: boolean`, `SnapshotRow.mouseAnyFlag: boolean`, and the same two on `PaneNode`.

- [ ] **Step 1: Write the failing tests** — add `alternateOn: false` and `mouseAnyFlag: false` to the `row()` fixture in `useSnapshot.test.ts`, which arms the existing `Object.keys`-driven "compares every field the wire carries" loop for free, and add two tests:

```ts
it('carries the wheel facts onto the pane', () => {
  const [session] = groupRows([row({ alternateOn: true, mouseAnyFlag: true })])
  expect(session.windows[0].panes[0].alternateOn).toBe(true)
  expect(session.windows[0].panes[0].mouseAnyFlag).toBe(true)
})

it('reads a daemon too old to send them as neither', () => {
  // The `windowId === ""` case, one type later: the row is cast, not
  // validated, so a missing key arrives as undefined and must not reach the
  // gesture as one.
  const { alternateOn, ...older } = row()
  const [session] = groupRows([older as SnapshotRow])
  expect(session.windows[0].panes[0].alternateOn).toBe(false)
})
```

- [ ] **Step 2: Run them and watch them fail**

Run: `cd web && pnpm vitest run src/lib/useSnapshot.test.ts`
Expected: FAIL — the new assertions, plus `a change to alternateOn was swallowed` and `a change to mouseAnyFlag was swallowed` from the exhaustive loop.

- [ ] **Step 3: Carry the fields through**

Two documented fields on `SnapshotRow` and two on `PaneNode`; `groupRows` maps them with `row.alternateOn === true` (the coercion is the degrade, and says so in a comment); `rowsEqual` compares both, each with the one-line reason the neighbouring fields carry — an agent starting or exiting moves these with the command and nothing else, and a comparison that skips them leaves the previous tree object and the previous gain.

- [ ] **Step 4: Run them and watch them pass**

Run: `cd web && pnpm vitest run src/lib/useSnapshot.test.ts`
Expected: PASS.

- [ ] **Step 5: Fix up the `PaneNode` fixtures and typecheck**

Run: `cd web && pnpm build`
Expected: PASS once each fixture named in **Files** carries the two fields. The typecheck is what enumerates them; add `alternateOn: false, mouseAnyFlag: false` beside each `paneMode:`.

- [ ] **Step 6: Commit**

```bash
git add web/src/lib/useSnapshot.ts web/src/lib/useSnapshot.test.ts web/src/lib/tabBadge.test.ts web/src/lib/manage.test.ts web/src/components/Palette.test.tsx web/src/components/CapturePanel.test.tsx web/src/components/Terminal.test.tsx web/src/components/AppSidebar.test.tsx
git commit -m "feat: the pane tree carries the wheel facts"
```

---

### Task 3: The gain, and the store that carries it

**Files:**
- Create: `web/src/lib/scrollGain.ts`
- Test: `web/src/lib/scrollGain.test.ts`

**Interfaces:**
- Consumes: `TMUX_LINES_PER_NOTCH` from `@/lib/touchScroll`.
- Produces:
  - `DEFAULT_APP_LINES_PER_NOTCH = 3`
  - `APP_LINES_PER_NOTCH: Readonly<Record<string, number>>`
  - `appLinesPerNotch(command: string): number`
  - `interface ScrollTarget { command: string; inMode: boolean; alternateOn: boolean; mouseAnyFlag: boolean }`
  - `linesPerNotch(target: ScrollTarget | null): number`
  - `reportScrollTarget(target: ScrollTarget | null): void`
  - `getLinesPerNotch(): number`
  - `resetScrollGain(): void` (tests only, the idiom `resetTerminalSettings` established)

- [ ] **Step 1: Write the failing tests** — `scrollGain.test.ts`:

```ts
describe('appLinesPerNotch', () => {
  it('is what each program was measured to scroll', () => {
    expect(appLinesPerNotch('claude')).toBe(1)
    expect(appLinesPerNotch('pi')).toBe(1)
    expect(appLinesPerNotch('opencode')).toBe(3)
  })
  it('assumes the conventional three for anything it has not measured', () => {
    // Heavy is recoverable, lost is not: too large a quantum makes an unknown
    // TUI feel heavy, too small flies past what the user was reading.
    expect(appLinesPerNotch('vim')).toBe(DEFAULT_APP_LINES_PER_NOTCH)
    expect(appLinesPerNotch('')).toBe(DEFAULT_APP_LINES_PER_NOTCH)
  })
})

describe('linesPerNotch', () => {
  const agent: ScrollTarget = { command: 'claude', inMode: false, alternateOn: true, mouseAnyFlag: true }

  it('is tmux’s five with no pane to go on', () => {
    expect(linesPerNotch(null)).toBe(TMUX_LINES_PER_NOTCH)
  })
  it('is tmux’s five for a plain shell pane', () => {
    expect(linesPerNotch({ command: 'zsh', inMode: false, alternateOn: false, mouseAnyFlag: false })).toBe(TMUX_LINES_PER_NOTCH)
  })
  it('is the application’s quantum on the alternate screen', () => {
    expect(linesPerNotch(agent)).toBe(1)
    expect(linesPerNotch({ ...agent, mouseAnyFlag: false })).toBe(1)
    expect(linesPerNotch({ ...agent, alternateOn: false })).toBe(1)
  })
  it('is tmux’s five whenever the pane is in a mode, alternate screen or not', () => {
    // The unrecoverable direction: in a mode tmux handles the wheel itself, so
    // reading the application's quantum here would scroll five times too far.
    expect(linesPerNotch({ ...agent, inMode: true })).toBe(TMUX_LINES_PER_NOTCH)
  })
})

describe('the store', () => {
  afterEach(() => resetScrollGain())
  it('is tmux’s five until a pane is reported, and again once it is not', () => {
    expect(getLinesPerNotch()).toBe(TMUX_LINES_PER_NOTCH)
    reportScrollTarget({ command: 'opencode', inMode: false, alternateOn: true, mouseAnyFlag: true })
    expect(getLinesPerNotch()).toBe(3)
    reportScrollTarget(null)
    expect(getLinesPerNotch()).toBe(TMUX_LINES_PER_NOTCH)
  })
})
```

- [ ] **Step 2: Run them and watch them fail**

Run: `cd web && pnpm vitest run src/lib/scrollGain.test.ts`
Expected: FAIL — cannot resolve `./scrollGain`.

- [ ] **Step 3: Write `scrollGain.ts`**

The predicate, in this order: no target → tmux's; `inMode` → tmux's; `alternateOn || mouseAnyFlag` → `appLinesPerNotch(command)`; otherwise tmux's. The store is a module-level `let` plus the two accessors, with no subscribers and no `useSyncExternalStore` — the value is a number read at the start of a gesture, not something React renders.

The file's header comment carries the design's reasoning, because none of it is visible in the code: tmux's own `WheelUpPane` binding and its `-N 5`, the measured per-program quanta (and that they were measured with notches ≥60ms apart, `claude` roughly doubling above that), why the table is keyed on the command rather than on `agentState` (which is `""` whenever the poll ran with no browser connected) and why that is not the frontend deciding what counts as an agent, and why `inMode` is the app's decided value rather than the raw `#{pane_mode}`.

- [ ] **Step 4: Run them and watch them pass**

Run: `cd web && pnpm vitest run src/lib/scrollGain.test.ts`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add web/src/lib/scrollGain.ts web/src/lib/scrollGain.test.ts
git commit -m "feat: one notch is as many lines as its receiver scrolls"
```

---

### Task 4: The gesture asks who is receiving

**Files:**
- Modify: `web/src/lib/touchScroll.ts` (`pxPerNotch`, `SwipeScroller.glide`, `installSwipeScroll`), `web/src/components/Terminal.tsx:1646-1649`, `web/src/App.tsx` (beside `copyControl`, line ~551)
- Test: `web/src/lib/touchScroll.test.ts`

**Interfaces:**
- Consumes: `getLinesPerNotch`, `reportScrollTarget`, `ScrollTarget` from Task 3; `PaneNode.alternateOn`/`mouseAnyFlag` from Task 2.
- Produces: `pxPerNotch(rowHeight: number, linesPerNotch?: number): number` (defaulting to `TMUX_LINES_PER_NOTCH`, so the existing tests and their meaning are untouched) and `installSwipeScroll(host: HTMLElement, linesPerNotch: () => number): () => void`.

- [ ] **Step 1: Write the failing tests** — in `touchScroll.test.ts`:

```ts
it('sizes a notch for the receiver, and for the font', () => {
  expect(pxPerNotch(17, 1)).toBe(17)
  expect(pxPerNotch(17, 3)).toBe(51)
  // The terminal settings feature makes the row variable: 20px text must move
  // 20px rows. The two features meet here for the first time.
  expect(pxPerNotch(20, 1)).toBe(20)
  // Before the terminal has laid out, the fallback row times the RECEIVER's
  // lines -- not tmux's five.
  expect(pxPerNotch(0, 1)).toBe(FALLBACK_ROW_HEIGHT_PX)
})

it('pays the finger every notch it earns and the glide one per frame', () => {
  const s = new SwipeScroller()
  s.start(100, 100, 17)
  s.move(100, 100 + SWIPE_THRESHOLD_PX)
  // A drag that crosses four notches in one move delivers four: one notch per
  // frame is 1018px/s of finger at this size, past any thumb, so a cap on the
  // finger would only ever lag a fast drag.
  expect(Math.abs(s.move(100, 100 + SWIPE_THRESHOLD_PX + 4 * 17))).toBe(4)
  // A glide owing several in one frame delivers one and DISCARDS the rest: a
  // tight burst is worth less than the same notches spread over frames
  // (measured, 16 lines against 19), and an unbounded glide would queue ~94
  // notches it cannot deliver inside its own ~600ms and keep scrolling after
  // the gesture visibly stopped.
  expect(s.glide(5 * 17)).toBe(-1)
  expect(s.glide(-5 * 17)).toBe(1)
})
```

- [ ] **Step 2: Run them and watch them fail**

Run: `cd web && pnpm vitest run src/lib/touchScroll.test.ts`
Expected: FAIL — `pxPerNotch` takes one argument; `glide` returns -5.

- [ ] **Step 3: Make the module take the gain**

`pxPerNotch` gains the second parameter. `SwipeScroller.glide` clamps `#drain()`'s result to ±1 and returns it, the overflow discarded with `#drain` having already taken it off the accumulator. `installSwipeScroll` takes the getter as a second parameter and `onDown` calls `scroller.start(x, y, pxPerNotch(rowHeight(), linesPerNotch()))` — read at `start`, so one gesture keeps one gain from beginning to end, and a getter rather than a value because the listeners are installed once with `[]` deps and must not be torn down mid-gesture. The module header gains the measured table and the reason the cap applies to the glide only.

- [ ] **Step 4: Run them and watch them pass**

Run: `cd web && pnpm vitest run src/lib/touchScroll.test.ts`
Expected: PASS, including the five existing tests unchanged.

- [ ] **Step 5: Wire the two ends**

`Terminal.tsx`: `installSwipeScroll(el, getLinesPerNotch)`. `App.tsx`, in an effect beside the `copyControl` call, report the active pane:

```ts
useEffect(() => {
  reportScrollTarget(
    located ? {
      command: located.pane.command,
      // The app's DECIDED mode, not the raw field: a pane put into copy mode
      // from this app's own button is still alternate_on = 1, and the poll is
      // up to 1.5s behind, so a raw read would pick the application's quantum
      // and scroll five times too far through what tmux is now handling.
      inMode: located.pane.paneMode !== '' || copy.inCopy,
      alternateOn: located.pane.alternateOn,
      mouseAnyFlag: located.pane.mouseAnyFlag,
    } : null,
  )
}, [located, copy.inCopy])
```

(`copy` is one of two frozen objects returned by reference, so `copy.inCopy` is stable across renders.)

- [ ] **Step 6: Typecheck and run both suites**

Run: `cd web && pnpm build && pnpm test`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add web/src/lib/touchScroll.ts web/src/lib/touchScroll.test.ts web/src/components/Terminal.tsx web/src/App.tsx
git commit -m "feat: a swipe scrolls as far as its receiver moves"
```

---

### Task 5: The regime no existing test can reach

**Files:**
- Modify: `e2e/touchscroll.spec.ts`

**Interfaces:**
- Consumes: everything above, through a real browser, a real daemon and a real tmux.

- [ ] **Step 1: Write the failing test**

A sixth test in the existing file, reusing its `swipe`/`slowDrag`/`ask`/`enroll` helpers. Every test in this file today runs over a shell pane, which is the tmux regime; this one makes the pane a receiver with a **known quantum and no agent in it**:

```
stty raw -echo; printf '\033[?1049h\033[?1003h\033[?1006h'; cat -v
```

`cat` is not in the table, so the gain is `DEFAULT_APP_LINES_PER_NOTCH` (3) and each report it receives prints as visible `^[[<64;c;rM` text on the alternate screen. The test:

1. sends that line, then polls `ask(tmuxWeb, '#{alternate_on}#{mouse_any_flag}')` until it is `11`;
2. polls, from the page, that the browser's own wire carries it — `fetch('/api/snapshot', { credentials: 'same-origin' })` and `panes.some((p) => p.alternateOn)` — which is both the wait for the ≤1.5s poll and an end-to-end check of Task 1's fields;
3. reads the row height from the DOM (`document.querySelector('.term-row')!.getBoundingClientRect().height`);
4. `slowDrag`s a measured distance (300px, which is well clear of the threshold and below `MIN_FLICK_PX_PER_MS`, so nothing glides);
5. counts the reports in `tmuxWeb.tmux('capture-pane', '-p', '-t', pane)` with `(capture.match(/<64;/g) ?? []).length` and asserts it is within 1 of `300 / (rowHeight * 3)` — the ±1 absorbing the priming notch.

The comment says what the number is for: with the shipped gain the count is about 6, and with the pre-change gain it is 2, so this is the only test in the suite that fails on the bug it was written for.

No teardown: the `tmuxWeb` fixture starts a tmux server and a daemon per test and disposes of both, so the pane left on the alternate screen dies with them.

- [ ] **Step 2: Run it and watch it fail**

Check out the receiver line first — run it in the e2e harness' own pane and confirm the reports print — then:

Run: `pnpm exec playwright test e2e/touchscroll.spec.ts -g 'alternate screen'`
Expected: FAIL on the count before Task 4's commit is in the build, and PASS after. Run it against `HEAD~1` of Task 4 if the failure needs demonstrating.

- [ ] **Step 3: Run the whole file**

Run: `make test-e2e` (or `pnpm exec playwright test e2e/touchscroll.spec.ts` while iterating)
Expected: PASS — the five existing tests unmodified, the new one with them.

- [ ] **Step 4: Commit**

```bash
git add e2e/touchscroll.spec.ts
git commit -m "test: a swipe over an application, where tmux is not the one scrolling"
```

---

## After the last task

- `make test-go && cd web && pnpm build && pnpm test`, then `make test-e2e`.
- Then the phone, which is the only thing that can settle decision 7: in an agent pane, a slow drag should track the finger and a throw should carry without flying. Try `claude` at 1 and then at 2 in `APP_LINES_PER_NOTCH`, in that order, and keep whichever feels like the shell pane does today. It is a one-line change.
- `docs/plans/2026-11-09-tmux-web-scroll-gain-design.md` goes in the first commit's `git add` as `Status: approved`.
