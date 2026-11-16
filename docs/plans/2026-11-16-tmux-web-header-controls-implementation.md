# Header controls Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give the phone's header back to the breadcrumb: delete the connection
dot, move Scrollback to the palette, show the copy-mode control only as the way
out, shrink the two survivors to icons below `sm`, and make the two optional
ones switchable per device.

**Architecture:** Nothing moves between layers. The three-state decision for
the copy control becomes a pure function in `lib/copyMode.ts`, where that
module's own header says rules belong; the two new flags become
`lib/headerSettings.ts`, a module-level store built exactly like
`lib/terminalSettings.ts` (which exists because the user menu and the terminal
are sibling subtrees); and `App.tsx`'s header becomes a reader of both.

**Tech Stack:** React 19 + TypeScript, Tailwind v4, Radix (`DropdownMenuSub`,
`DropdownMenuCheckboxItem`), lucide-react, vitest, Playwright.

**Spec:** `docs/plans/2026-11-16-tmux-web-header-controls-design.md`

## Global Constraints

- **No device sniffing.** Width rules are CSS (`hidden sm:inline`), and both
  new flags default **on, on every device**.
- **Accessible names, fixed** (D4): jump → `Jump to…`, reply → `Reply box`,
  copy → `copy.label` (`Copy mode` / `Exit copy`). Width-independent, not
  mode-independent.
- **Glyphs already chosen** (`Palette.tsx:503-513`): reply `MessageSquare`,
  exit copy `CopySlash`, scrollback `ScrollText`.
- **The flag gates the button, not the box:** `showReplyBox` (`ReplyBox.tsx:261`)
  is not touched.
- **Turning the reply flag off calls `showReply(false)` synchronously from the
  store's setter**, never from a `useEffect` — `noteLayoutChange` must arm
  before React is told (`LAYOUT_CHANGE_GRACE_MS = 1_000`, `Terminal.tsx:144`).
- **A palette row that does not apply is absent, not disabled** (that group's
  own stated rule).
- Commands: `cd web && pnpm test` · `cd web && pnpm build` (the only
  typecheck) · `make test-e2e` · `make test-go`. Do not run prettier.
  `git add` explicit paths only.
- `pnpm build` deletes `internal/front/dist/.gitkeep`; `make dist-keep`
  restores it.

## Review Focus

Five conditions the design implies that no task's main test exercises. Each has
a test pinned to the task that owns the code; a reviewer checks each
deliberately.

1. **`localStorage` blocked or throwing** (private window, embedded webview):
   both controls must render *on*, nothing may throw. → Task 4.
2. **A stored `tmux-web:header` from another version** (`{"showJump":"yes"}`,
   extra keys, not an object): each field falls back to on, field by field, so
   one bad field never costs the other. → Task 4.
3. **The reply toggle switched off while the box is open and the pane is in a
   copy mode**: the box closes, tmux gets that resize, and the copy control
   stays where it is. → Task 5.
4. **Both toggles off**: the header's right-hand group leaves no gap and the
   breadcrumb takes the full width. → Task 5.
5. **A pane in `tree-mode` or `clock-mode`**: the copy control stays absent —
   those are modes `send-keys -X cancel` cannot pop. → Task 1.

---

## File Structure

| File | Responsibility |
|---|---|
| `web/src/lib/copyMode.ts` | +`copyControlState`: the header's three states, as a function |
| `web/src/lib/headerSettings.ts` | **new** — the two flags, their storage, their store |
| `web/src/components/Palette.tsx` | + the `open-capture` action and its row |
| `web/src/components/UserMenu.tsx` | + the `Header` submenu |
| `web/src/App.tsx` | the header: dot deleted, two icon buttons, both flags read |
| `e2e/capture.spec.ts`, `e2e/terminal.spec.ts`, `e2e/sizing.spec.ts` | the locators that moved, and the width this is all for |

---

### Task 1: The copy control's three states

**Files:**
- Modify: `web/src/lib/copyMode.ts`
- Test: `web/src/lib/copyMode.test.ts`

**Interfaces:**
- Consumes: nothing.
- Produces: `export type CopyControlState = 'absent' | 'disabled' | 'live'` and
  `export function copyControlState(inCopy: boolean, ready: boolean): CopyControlState`.
  `inCopy` is `copy.inCopy` from `copyControl` (the flip-aware value, D2), and
  `ready` is `status?.phase === 'ready'`.

- [ ] **Step 1: Write the failing tests**

```ts
describe('copyControlState', () => {
  it('is absent on a pane that is not in a copy mode', () => {
    expect(copyControlState(false, true)).toBe('absent')
  })
  it('is live on a pane in a copy mode with a ready socket', () => {
    expect(copyControlState(true, true)).toBe('live')
  })
  // The state the browser cannot reach cheaply: the mode comes from the
  // daemon's poll, so it outlives this tab's socket, and `endMode` would fail
  // silently on a button that still looked pressable.
  it('is disabled on a pane in a copy mode with a socket that is not ready', () => {
    expect(copyControlState(true, false)).toBe('disabled')
  })
  it('is absent on a pane that is not in a copy mode and has no socket', () => {
    expect(copyControlState(false, false)).toBe('absent')
  })
})
```

- [ ] **Step 2: Add the Review Focus #5 test, in the same file**

`inCopyMode` is a closed set, so the guard is that the *caller's* value is
false for the other modes. The test states it at this seam:

```ts
it('is absent for every mode cancel cannot pop', () => {
  for (const mode of ['tree-mode', 'clock-mode', 'options-mode', 'client-mode', '']) {
    expect(copyControlState(inCopyMode(mode), true)).toBe('absent')
  }
})
```

- [ ] **Step 3: Run them and watch them fail**

Run: `cd web && pnpm test copyMode`
Expected: FAIL — `copyControlState is not a function`.

- [ ] **Step 4: Implement `copyControlState` in `web/src/lib/copyMode.ts`**

Beside `inCopyMode`, with a doc comment saying why it is a function and not a
ternary in the JSX: the module header's own argument, plus D8 — the state it
protects is the one the browser cannot reach cheaply.

- [ ] **Step 5: Run them and watch them pass**

Run: `cd web && pnpm test copyMode`
Expected: PASS, and no other file's tests touched.

- [ ] **Step 6: Commit**

```bash
git add web/src/lib/copyMode.ts web/src/lib/copyMode.test.ts
git commit -m "feat: the copy control's three states, where a test can reach them"
```

---

### Task 2: Scrollback moves to the palette

**Files:**
- Modify: `web/src/components/Palette.tsx`, `web/src/App.tsx`
- Modify: `e2e/capture.spec.ts:149-152` (the `openPanel` helper)
- Test: `web/src/components/Palette.test.tsx`

**Interfaces:**
- Consumes: nothing.
- Produces: `PaletteAction` gains `| { kind: 'open-capture' }`; `PaletteProps`
  gains `onOpenCapture: () => void` and `hasPane: boolean`. `App` passes
  `() => setCaptureOpen(true)` and `activePane !== null`.

- [ ] **Step 1: Write the failing tests**

In `Palette.test.tsx`, in the style that file already uses for the Terminal
group's rows:

```ts
it('offers the scrollback row when a pane is attached', () => {
  // renders the palette with hasPane: true
  expect(html).toContain('Scrollback')
})
it('leaves the scrollback row out when there is no pane', () => {
  // hasPane: false -- absent, not disabled, like every row in this group
  expect(html).not.toContain('Scrollback')
})
```

- [ ] **Step 2: Run them and watch them fail**

Run: `cd web && pnpm test Palette`
Expected: FAIL — the row is in neither render.

- [ ] **Step 3: Add the row to the `Terminal` group in `Palette.tsx`**

After the reply row. `<ScrollText aria-hidden />`, the label `Scrollback`, a
`value` carrying the words a person would type for it (`scrollback history
capture read output`), and a `CommandShortcut` saying what it is for — it reads
the pane without putting it in copy mode. Rendered only when `hasPane`.

- [ ] **Step 4: Wire it in `App.tsx`**

`onOpenCapture` on `<Palette>`, and the `open-capture` case in the palette's
action handler, which closes the palette and opens the panel — the same
`setCaptureOpen(true)` the header button called.

- [ ] **Step 5: Delete the `Scrollback` button from the header**

`App.tsx:752-760`. The feature now has exactly one door, and it is in the
palette; two doors was never the plan and zero would strand the panel.

- [ ] **Step 6: Point the e2e helper at the new door**

`e2e/capture.spec.ts`'s `openPanel` opens the palette (`getByRole('button',
{ name: /Jump to/ })`) and clicks `getByRole('option', { name: /Scrollback/ })`,
then waits for `pre(page)` as it does today. One call site.

- [ ] **Step 7: Run the unit tests, the typecheck and the capture spec**

Run: `cd web && pnpm test Palette && pnpm build && cd .. && make dist-keep`
Expected: PASS, and `tsc -b` clean.
Run: `npx playwright test e2e/capture.spec.ts`
Expected: PASS — every test in the file, since they all go through `openPanel`.

- [ ] **Step 8: Commit**

```bash
git add web/src/components/Palette.tsx web/src/components/Palette.test.tsx web/src/App.tsx e2e/capture.spec.ts
git commit -m "feat: scrollback is a palette row, not a header button"
```

---

### Task 3: The header's final shape

**Files:**
- Modify: `web/src/App.tsx` (the header, `App.tsx:684-763`, and `ConnectionDot`)
- Modify: `e2e/terminal.spec.ts:64-101` and `:187`
- Test: `e2e/sizing.spec.ts` (new test), `e2e/terminal.spec.ts`

**Interfaces:**
- Consumes: `copyControlState(inCopy, ready)` from Task 1.
- Produces: a header whose right side is two icon buttons below `sm`.

- [ ] **Step 1: Write the failing e2e, in `e2e/sizing.spec.ts`**

The one assertion the whole change is for. Name:
`'the breadcrumb is readable on a phone, not squeezed by the controls'`.

```ts
await page.setViewportSize({ width: 390, height: 844 })
// a pane whose command is long enough to be worth reading
const command = page.getByLabel('Location').locator('span').last()
const fits = await command.evaluate((el) => el.scrollWidth <= el.clientWidth)
expect(fits).toBe(true)
```

- [ ] **Step 2: Run it and watch it fail**

Run: `npx playwright test e2e/sizing.spec.ts -g breadcrumb`
Expected: FAIL — the command segment is clipped at 390px. **Record the measured
`scrollWidth`/`clientWidth` in the test's comment**: that pair is the mutation
check for this task, and it is the owner's complaint in numbers.

- [ ] **Step 3: Delete `ConnectionDot`**

The function (`App.tsx:936-951`) and its use in the header. D1's table is the
argument; put its one-line version in the header's module comment, which
currently ends "The dot is the socket, not tmux: green only while input is
actually going somewhere" and must not go on saying so.

- [ ] **Step 4: Rebuild the copy control on Task 1's function**

A `switch` over `copyControlState(copy.inCopy, status?.phase === 'ready')`:
`absent` renders nothing, `disabled` and `live` render the button with
`disabled` set accordingly. `aria-label={copy.label}`, `<CopySlash aria-hidden />`,
and the label in `<span className="hidden sm:inline">{copy.label}</span>`.
Keep the existing comment about why there is no `aria-pressed`.

- [ ] **Step 5: Shrink the reply toggle the same way**

`aria-label="Reply box"` (fixed — `e2e/harness.ts:459` locates it by that
exact name), `<MessageSquare aria-hidden />`, and
`<span className="hidden sm:inline">Reply box</span>`. `aria-pressed` and the
`title` stay as they are.

- [ ] **Step 6: Run the new e2e and watch it pass**

Run: `npx playwright test e2e/sizing.spec.ts -g breadcrumb`
Expected: PASS. Record the new `clientWidth` in the comment beside the old one.

- [ ] **Step 7: Fix the copy-control tests in `e2e/terminal.spec.ts`**

- `:64-101`: the entering half goes through the palette row the file already
  clicks at `:196`; the in-mode half keeps asserting `toHaveText('Exit copy')`,
  which holds because that test runs at the default (desktop) viewport.
- `:187`: delete the "disabled on a revoked device" assertion. Task 1's unit
  tests cover that guard; left here it would assert absence on a pane that is
  never in a mode, which holds with or without the guard. Say so in a comment
  where the assertion was, so the next reader does not restore it.

- [ ] **Step 8: Run the whole suite and the typecheck**

Run: `cd web && pnpm test && pnpm build && cd .. && make dist-keep`
Expected: PASS, `tsc -b` clean.
Run: `make test-e2e`
Expected: PASS, every spec.

- [ ] **Step 9: Commit**

```bash
git add web/src/App.tsx e2e/terminal.spec.ts e2e/sizing.spec.ts
git commit -m "feat: the header gives the phone back its breadcrumb"
```

---

### Task 4: The two flags and their store

**Files:**
- Create: `web/src/lib/headerSettings.ts`
- Test: `web/src/lib/headerSettings.test.ts`

**Interfaces:**
- Consumes: nothing. Mirrors `lib/terminalSettings.ts` — read that file first;
  this one is the same shape with two booleans instead of a size and a theme.
- Produces:
  ```ts
  export const HEADER_SETTINGS_KEY = 'tmux-web:header'
  export interface HeaderSettings { showJump: boolean; showReply: boolean }
  export const DEFAULT_HEADER_SETTINGS: HeaderSettings = { showJump: true, showReply: true }
  export function parseHeaderSettings(raw: string | null): HeaderSettings
  export function readHeaderSettings(store?: SettingsStore | null): HeaderSettings
  export function writeHeaderSettings(s: HeaderSettings, store?: SettingsStore | null): void
  export function setHeaderSettings(patch: Partial<HeaderSettings>): void
  export function useHeaderSettings(): HeaderSettings
  export function onHeaderSettingsChange(fn: (s: HeaderSettings) => void): () => void
  ```
  `SettingsStore` is imported from `lib/terminalSettings.ts` rather than
  redeclared. `onHeaderSettingsChange`'s callbacks run **synchronously inside
  `setHeaderSettings`, before the React notification** — that ordering is what
  Task 5's D6 close depends on, and it is the reason this is not a `useEffect`.

- [ ] **Step 1: Write the failing tests**

```ts
describe('parseHeaderSettings', () => {
  it('reads both flags on when there is nothing stored', () => {
    expect(parseHeaderSettings(null)).toEqual({ showJump: true, showReply: true })
  })
  it('reads what was stored', () => {
    expect(parseHeaderSettings('{"showJump":true,"showReply":false}'))
      .toEqual({ showJump: true, showReply: false })
  })
  // Review Focus #2: a value from a version that is not this one.
  it('falls back field by field, so one bad field does not cost the other', () => {
    expect(parseHeaderSettings('{"showJump":"yes","showReply":false}'))
      .toEqual({ showJump: true, showReply: false })
  })
  it('reads the defaults from anything that is not an object of flags', () => {
    for (const raw of ['', 'null', '[]', '{', '"on"', '7'])
      expect(parseHeaderSettings(raw)).toEqual(DEFAULT_HEADER_SETTINGS)
  })
})

describe('readHeaderSettings', () => {
  // Review Focus #1: site data blocked outright -- reaching for localStorage
  // throws, and the header must still come up with both controls.
  it('reads the defaults from a store that throws', () => {
    const store = { getItem() { throw new Error('blocked') }, setItem() {} }
    expect(readHeaderSettings(store)).toEqual(DEFAULT_HEADER_SETTINGS)
  })
})

describe('writeHeaderSettings', () => {
  it('swallows a store that refuses the write', () => {
    const store = { getItem: () => null, setItem() { throw new Error('quota') } }
    expect(() => writeHeaderSettings(DEFAULT_HEADER_SETTINGS, store)).not.toThrow()
  })
})

describe('setHeaderSettings', () => {
  it('tells its subscribers synchronously, before it returns', () => {
    const seen: HeaderSettings[] = []
    const off = onHeaderSettingsChange((s) => seen.push(s))
    setHeaderSettings({ showReply: false })
    expect(seen).toEqual([{ showJump: true, showReply: false }])  // already, not later
    off()
    setHeaderSettings({ showReply: true })
    expect(seen).toHaveLength(1)
  })
})
```

- [ ] **Step 2: Run them and watch them fail**

Run: `cd web && pnpm test headerSettings`
Expected: FAIL — the module does not exist.

- [ ] **Step 3: Write `web/src/lib/headerSettings.ts`**

The `terminalSettings.ts` shape: `defaultStore()` guarded by try/catch, a
field-by-field `parse`, a module-level snapshot returned **by identity** when
nothing changed (`useSyncExternalStore` compares with `Object.is`, and a fresh
object every call is an infinite render loop — that file's own warning), and a
subscriber set that `setHeaderSettings` walks before notifying React. A header
comment saying why these two flags are not in `TerminalSettings`: that module
is about the thing drawing *inside* the terminal.

- [ ] **Step 4: Run them and watch them pass**

Run: `cd web && pnpm test headerSettings`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add web/src/lib/headerSettings.ts web/src/lib/headerSettings.test.ts
git commit -m "feat: two header flags, per device, defaulting to on"
```

---

### Task 5: The `Header` submenu, and the close that cannot be lost

**Files:**
- Modify: `web/src/components/UserMenu.tsx`, `web/src/App.tsx`
- Test: `web/src/components/UserMenu.test.tsx`, `e2e/terminal.spec.ts`

**Interfaces:**
- Consumes: `useHeaderSettings`, `setHeaderSettings`, `onHeaderSettingsChange`
  from Task 4.
- Produces: nothing later tasks read.

- [ ] **Step 1: Write the failing unit test**

`UserMenu.test.tsx` asserts the menu's contents the way that file already does:

```ts
it('offers a Header submenu with both controls', () => {
  // the menu's source/render carries a `Header` trigger and the two items
  expect(menu).toContain('Header')
  expect(menu).toContain('Jump to…')
  expect(menu).toContain('Reply box')
})
```

- [ ] **Step 2: Run it and watch it fail**

Run: `cd web && pnpm test UserMenu`
Expected: FAIL — no `Header` item.

- [ ] **Step 3: Add `HeaderMenu` to `UserMenu.tsx`**

A `DropdownMenuSub` beside `AppearanceMenu` (`UserMenu.tsx:376-402` is the
shape to mirror), triggered by `Header` with a `PanelTop` glyph, holding two
`DropdownMenuCheckboxItem`s — `Jump to…` and `Reply box` — whose `checked` comes
from `useHeaderSettings()` and whose `onCheckedChange` calls
`setHeaderSettings`. Placed after `Terminal…`, before `Appearance`.

- [ ] **Step 4: Gate the two buttons in `App.tsx`**

`useHeaderSettings()` in `App`; the jump button renders only when `showJump`,
the reply button only when `showReply`. The copy control is **not** gated (D5:
on a phone it is the only obvious way out of the mode).

- [ ] **Step 5: Register the synchronous close**

In `App`, one `useEffect` whose body is `onHeaderSettingsChange` and whose
cleanup is the unsubscribe it returns: when `showReply` arrives false and the
box is open, call `showReply(false)` — the same callback the button calls, so
`noteLayoutChange` arms before React is told. The effect only *registers*; the
callback is what runs synchronously. Mind the name collision between the
`showReply` callback in `App` and the `showReply` flag; rename the flag at the
read site.

- [ ] **Step 6: Write the Review Focus e2e, in `e2e/terminal.spec.ts`**

`'turning the reply box off closes it, even in copy mode'` (#3) — open the box,
put the pane in copy mode, turn the item off through the footer menu
(`openTerminalSettings`'s sibling path: the footer's only button, then the
`Header` submenu), and assert `replyBox(page)` is gone while the `Exit copy`
control is still there.

`'with both header controls off the breadcrumb has the width to itself'` (#4) —
turn both off at a 390px viewport and assert no `button` remains inside the
`banner`/`header` other than the sidebar trigger, and that the breadcrumb's
command still fits (the Task 3 assertion, which must also hold here).

- [ ] **Step 7: Run everything**

Run: `cd web && pnpm test && pnpm build && cd .. && make dist-keep`
Expected: PASS, `tsc -b` clean.
Run: `make test-e2e`
Expected: PASS.

- [ ] **Step 8: Mutation-check the close**

Make the close an ordinary `useEffect` on the flag instead of the subscriber,
run the #3 test, and record what it does. If it passes either way, the test is
not covering the ordering D6 is about and the comment must say so rather than
implying coverage. **Restore by editing the code back, never with
`git checkout --` or `git restore`.**

- [ ] **Step 9: Commit**

```bash
git add web/src/components/UserMenu.tsx web/src/components/UserMenu.test.tsx web/src/App.tsx e2e/terminal.spec.ts
git commit -m "feat: the header's two optional controls, per device"
```

---

### Task 6: What the README says the header is

**Files:**
- Modify: `README.md`

- [ ] **Step 1: Find every line that describes the header's controls**

Run: `grep -n -i "scrollback\|copy mode\|reply box\|jump to\|connection" README.md`
Expected: the bullets that describe the header and the reply box.

- [ ] **Step 2: Rewrite them to the shipped behaviour**

Scrollback is in the palette; the copy-mode control is the way out and only
appears in a mode; the two optional controls are in the user menu under
`Header`, both on by default, remembered per device; there is no connection
dot, because the pill over the terminal and the sidebar footer already say it.

- [ ] **Step 3: Commit**

```bash
git add README.md
git commit -m "docs: the header, as it is now"
```

---

## Self-Review

**1. Spec coverage.** D1 → Task 3 Step 3. D2 → Task 1 + Task 3 Step 4. D3 →
Task 2. D4 → Task 3 Steps 4-5 (strings in Global Constraints). D5 → Task 4 +
Task 5 Steps 3-4. D6 → Task 5 Step 5, mutation-checked at Step 8. D7 → Task 5
Step 6's second test, which is that cost made visible. D8 → Task 1. The
measurement's pass condition → Task 3 Steps 1-2 and 6. The "tests this breaks"
list → Task 2 Step 6, Task 3 Step 7. Non-goals need no task. No gaps.

**2. Step scan.** Every step names a file and a checkable result. The only code
blocks are tests (names and assertions) and one signature block; no function
bodies.

**3. Type consistency.** `copyControlState(inCopy, ready)` is produced in
Task 1 and consumed in Task 3 Step 4 with that argument order.
`HeaderSettings`'s two field names (`showJump`, `showReply`) are produced in
Task 4 and consumed in Task 5 Steps 3-4. `PaletteAction`'s new member
`{ kind: 'open-capture' }` is produced and consumed inside Task 2.
`SettingsStore` is imported, not redeclared.

**4. Review Focus.** #1 and #2 → Task 4 Step 1. #3 and #4 → Task 5 Step 6.
#5 → Task 1 Step 2.

**5. Proportion.** Plan ≈ the design's length, and the code it describes is
larger than both. No transcript.
