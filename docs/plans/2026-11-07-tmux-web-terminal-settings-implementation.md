# Terminal settings Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A `Terminal…` dialog in the user menu that sets the terminal's font
size and colour theme, per device, applied live.

**Architecture:** One module owns the settings and the reported grid as two
module-level stores read through `useSyncExternalStore`, so the dialog (inside
`UserMenu`) and the terminal (a sibling subtree) share them with no prop
threading and no context provider. The terminal turns the size into two inline
CSS custom properties and the theme into `WTermView`'s reactive `theme` prop;
a size change nudges the host's box by 1px for one animation frame, which is
the only thing that makes `@wterm/dom` re-measure its cell.

**Tech Stack:** React 19, TypeScript, Tailwind 4, `@wterm/react`/`@wterm/dom`,
`next-themes`, vitest (node + a jsdom file), Playwright.

**Spec:** `docs/plans/2026-11-07-tmux-web-terminal-settings-design.md`

## Global Constraints

- Font size range **10 to 24 px**, whole numbers only; default **14**.
- `--term-row-height` is always `Math.round(fontSize * 1.2)` px. `14 → 17`,
  which is byte-for-byte what `@wterm/dom` ships.
- Terminal theme values are exactly `system | dark | light | solarized-dark |
  monokai`; default **`system`**. `dark` maps to **no** wterm theme class.
- `localStorage` key is exactly `tmux-web:terminal`, holding
  `{"fontSize":<int>,"theme":"<value>"}`.
- Absent, corrupt, foreign, out-of-range or **throwing** storage reads as the
  defaults; a failing write is dropped, never surfaced.
- `GRID_MAX_COLS` is not changed by this work.
- `Appearance` in the user menu is not touched: it stays the *interface* theme.
- No new npm package. No new `components/ui/*` wrapper either: the size uses a
  native `<select>` (on a phone that is the OS picker) and the theme a native
  `<fieldset>`/`<input type="radio">` group, both styled with the app's tokens.
- The vitest suite runs under `environment: 'node'`; anything needing a DOM
  goes in a file whose first line is `// @vitest-environment jsdom`.
- Do not run `prettier`. `git add` explicit paths only.

## Review Focus

1. **Storage that throws** (Safari private mode) on `getItem` *and* on
   `setItem` — the app must render at the defaults and go on working. Task 1.
2. **A stored value from another version**: unknown theme string, `fontSize` as
   a string, fractional, negative, `NaN`, JSON that is not an object, JSON that
   is an array — each reads as the default, nothing throws. Task 1.
3. **A size change while the socket is down or suppression is on** — tmux must
   learn the new size when the socket returns, from the pair `noteResize`
   recorded. Task 3.
4. **The nudge with no element, or a terminal unmounted between the change and
   the animation frame** — must not throw. Task 3.
5. **The readout with no terminal mounted and before the first resize** — says
   so rather than inventing a size; and when the clamp is biting it says that
   too, since `256` alone cannot distinguish a 256-column box from a clamped
   285-column one. Tasks 1 and 2.

---

### Task 1: The settings module

**Files:**
- Create: `web/src/lib/terminalSettings.ts`
- Test: `web/src/lib/terminalSettings.test.ts`

**Interfaces:**
- Consumes: nothing.
- Produces:
  ```ts
  export const TERMINAL_SETTINGS_KEY = 'tmux-web:terminal'
  export const FONT_SIZE_MIN = 10
  export const FONT_SIZE_MAX = 24
  export const DEFAULT_FONT_SIZE = 14
  export const FONT_SIZES: readonly number[]            // 10..24, ascending
  export const ROW_HEIGHT_RATIO = 1.2
  export type TerminalTheme = 'system' | 'dark' | 'light' | 'solarized-dark' | 'monokai'
  export const TERMINAL_THEMES: readonly { value: TerminalTheme; label: string }[]
  export interface TerminalSettings { fontSize: number; theme: TerminalTheme }
  export const DEFAULT_TERMINAL_SETTINGS: TerminalSettings
  export interface SettingsStore { getItem(k: string): string | null; setItem(k: string, v: string): void }
  export function parseTerminalSettings(raw: string | null): TerminalSettings
  export function readTerminalSettings(store: SettingsStore | null): TerminalSettings
  export function writeTerminalSettings(s: TerminalSettings, store: SettingsStore | null): void
  export function rowHeightFor(fontSize: number): number
  export function wtermThemeFor(theme: TerminalTheme, resolved: string | undefined): string | undefined
  export interface TerminalGrid { cols: number; rows: number; capped: boolean }
  export function useTerminalSettings(): TerminalSettings
  export function applyTerminalSettings(patch: Partial<TerminalSettings>): void
  export function resetTerminalSettings(store?: SettingsStore | null): void   // tests only
  export function useTerminalGrid(): TerminalGrid | null
  export function reportTerminalGrid(grid: TerminalGrid | null): void
  ```
  `TERMINAL_THEMES` labels, exactly: `Follow interface`, `Dark`, `Light`,
  `Solarized Dark`, `Monokai`, in that order, for the values in the order
  declared on `TerminalTheme`.

- [ ] **Step 1: Write the failing tests**

```ts
describe('parseTerminalSettings', () => {
  it('reads a well-formed value', () => {
    expect(parseTerminalSettings('{"fontSize":18,"theme":"monokai"}'))
      .toEqual({ fontSize: 18, theme: 'monokai' })
  })
  // Review Focus 2: one case per way a stored value can be wrong.
  it.each([
    null, '', 'not json', '[]', '"14"', '42',
    '{"fontSize":9,"theme":"dark"}',          // under the floor
    '{"fontSize":25,"theme":"dark"}',         // over the ceiling
    '{"fontSize":14.5,"theme":"dark"}',       // not a whole pixel
    '{"fontSize":"14","theme":"dark"}',       // wrong type
    '{"fontSize":null,"theme":"dark"}',
    '{"theme":"dark"}',                       // size missing
    '{"fontSize":18,"theme":"hotdog"}',       // foreign theme
    '{"fontSize":18}',                        // theme missing
  ])('falls back on %s', (raw) => {
    const got = parseTerminalSettings(raw as string | null)
    expect(got.fontSize).toBe(14)
    expect(got.theme).toBe('system')
  })
  it('keeps the half it can read', () => {
    expect(parseTerminalSettings('{"fontSize":20,"theme":"hotdog"}'))
      .toEqual({ fontSize: 20, theme: 'system' })
  })
})

// Review Focus 1.
it('reads the defaults from storage that throws', () => {
  const store = { getItem() { throw new Error('blocked') }, setItem() {} }
  expect(readTerminalSettings(store)).toEqual(DEFAULT_TERMINAL_SETTINGS)
})
it('drops a write to storage that throws', () => {
  const store = { getItem: () => null, setItem() { throw new Error('blocked') } }
  expect(() => writeTerminalSettings({ fontSize: 20, theme: 'dark' }, store)).not.toThrow()
})
it('reads the defaults with no storage at all', () => {
  expect(readTerminalSettings(null)).toEqual(DEFAULT_TERMINAL_SETTINGS)
})

describe('rowHeightFor', () => {
  // The first is the one that matters: it is what @wterm/dom ships.
  it('reproduces wterm’s own default', () => expect(rowHeightFor(14)).toBe(17))
  it.each([[10, 12], [12, 14], [16, 19], [18, 22], [20, 24], [24, 29]])(
    '%ipx rows are %ipx', (size, want) => expect(rowHeightFor(size)).toBe(want))
})

describe('wtermThemeFor', () => {
  it.each([
    ['dark', 'dark', undefined],          // wterm's default IS the absent class
    ['light', 'dark', 'light'],
    ['solarized-dark', 'light', 'solarized-dark'],
    ['monokai', 'dark', 'monokai'],
    ['system', 'light', 'light'],
    ['system', 'dark', undefined],
    ['system', undefined, undefined],     // before next-themes has resolved
  ])('%s under a %s interface', (theme, resolved, want) => {
    expect(wtermThemeFor(theme as TerminalTheme, resolved as string | undefined)).toBe(want)
  })
})

it('offers every whole size in range exactly once', () => {
  expect(FONT_SIZES).toEqual([10,11,12,13,14,15,16,17,18,19,20,21,22,23,24])
})

// The stores are module-level, so every test here calls resetTerminalSettings()
// in a beforeEach -- without it one test's choice is the next one's start state.
it('notifies subscribers on a change and persists it', () => {
  // applyTerminalSettings({fontSize: 20}) -> subscriber ran once, the snapshot
  // carries 20 AND the old theme, and the store holds the serialised pair.
})
it('returns the same snapshot by identity when nothing changed', () => {
  // useSyncExternalStore re-renders forever otherwise.
})
it('has no grid until one is reported', () => {
  // the grid snapshot is null before reportTerminalGrid, and null again after
  // reportTerminalGrid(null) -- which is what unmounting sends.
})
```

- [ ] **Step 2: Run them and watch them fail**

Run: `cd web && pnpm vitest run src/lib/terminalSettings.test.ts`
Expected: FAIL — the module does not exist.

- [ ] **Step 3: Implement `web/src/lib/terminalSettings.ts`**

Two module-level stores behind `useSyncExternalStore`, each with
`subscribe(cb)`, `getSnapshot()` and a setter; `getServerSnapshot` is the same
function as `getSnapshot`, because neither snapshot touches the DOM — the
settings snapshot is seeded lazily on first read via `readTerminalSettings`,
and the grid snapshot starts `null`. Settings snapshots are returned by
identity when nothing changed, or `useSyncExternalStore` re-renders forever.
Storage defaults to `globalThis.localStorage` behind a try/catch, the same
contract as `readSeen` in `lib/useSnapshot.ts`.

Write the file's doc comment the way this repo writes them: why the stores are
module-level rather than a context (one producer, one consumer, two subtrees),
why `rowHeightFor` is `round(n * 1.2)` and not something prettier (it has to
reproduce 17px at 14px exactly), and why `dark` maps to `undefined`.

- [ ] **Step 4: Run them and watch them pass**

Run: `cd web && pnpm vitest run src/lib/terminalSettings.test.ts`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add web/src/lib/terminalSettings.ts web/src/lib/terminalSettings.test.ts
git commit -m "feat: the terminal's own settings, with the storage contract the rest of the app uses"
```

---

### Task 2: The dialog and the menu entry

**Files:**
- Create: `web/src/components/TerminalSettingsDialog.tsx`
- Create: `web/src/components/TerminalSettingsDialog.test.tsx`
- Modify: `web/src/components/UserMenu.tsx` (the menu items around line 290 and
  the dialog mount around line 304; `AppearanceMenu` at 357 is **not** touched)
- Test: `web/src/components/UserMenu.test.tsx`

**Interfaces:**
- Consumes: everything Task 1 produces.
- Produces:
  ```ts
  export interface TerminalSettingsDialogProps {
    open: boolean
    onOpenChange: (open: boolean) => void
  }
  export function TerminalSettingsDialog(props: TerminalSettingsDialogProps): ReactElement
  export function gridLabel(grid: TerminalGrid | null): string
  ```

- [ ] **Step 1: Write the failing tests**

`gridLabel` is the whole of Review Focus 5 and is tested directly:

```ts
it.each([
  [null, 'no terminal attached'],
  [{ cols: 232, rows: 64, capped: false }, '232 × 64'],
  [{ cols: 256, rows: 78, capped: true }, '256 × 78 (capped — the grid cannot draw wider)'],
])('says %j as %s', (grid, want) => expect(gridLabel(grid as TerminalGrid | null)).toBe(want))
```

Markup, via `renderToStaticMarkup` the way `DevicesDialog.test.tsx` does it:

```ts
it('offers every size in range, with the stored one selected', () => { /* 15 <option>s, selected === stored */ })
it('offers the five themes, with the stored one checked', () => { /* labels verbatim from TERMINAL_THEMES */ })
it('shows the reported grid', () => { /* reportTerminalGrid then render */ })
it('is closed when open is false', () => { /* nothing rendered */ })
```

And in `UserMenu.test.tsx`:

```ts
it('offers Terminal settings and leaves Appearance alone', () => {
  // The menu carries BOTH: 'Terminal…' and 'Appearance'. The second is the
  // interface theme and this feature does not touch it.
})
```

- [ ] **Step 2: Run them and watch them fail**

Run: `cd web && pnpm vitest run src/components/TerminalSettingsDialog.test.tsx src/components/UserMenu.test.tsx`
Expected: FAIL — no such module; the menu has no `Terminal…`.

- [ ] **Step 3: Implement the dialog**

`Dialog`/`DialogContent`/`DialogHeader`/`DialogTitle`/`DialogDescription` from
`@/components/ui/dialog`, laid out like `DevicesDialog`. Title `Terminal`,
description one sentence saying these are this device's settings. The size is a
native `<select>` over `FONT_SIZES` labelled `Font size`; the theme is a
`<fieldset>` with a `<legend>` of `Colours` and one `<input type="radio">` per
`TERMINAL_THEMES` entry. Both call `applyTerminalSettings` on change — there is
no OK button and nothing to confirm. The grid readout sits under the size as
`gridLabel(useTerminalGrid())`.

- [ ] **Step 4: Add the menu entry**

In `UserMenu`'s `DropdownMenuContent`, a `DropdownMenuItem` reading
`Terminal…` with lucide's `SquareTerminal`, below `Devices…` and above
`AppearanceMenu`, opening the dialog from a `terminalOpen` state; mount
`<TerminalSettingsDialog>` beside `<DevicesDialog>`. Extend `AppearanceMenu`'s
doc comment: its first line currently says font size is deliberately absent and
why, and that reasoning is now spent — say where the setting went and that
`Appearance` is the *interface's* theme.

- [ ] **Step 5: Run them and watch them pass**

Run: `cd web && pnpm vitest run src/components`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add web/src/components/TerminalSettingsDialog.tsx web/src/components/TerminalSettingsDialog.test.tsx web/src/components/UserMenu.tsx web/src/components/UserMenu.test.tsx
git commit -m "feat: a terminal settings dialog, reached from the user menu"
```

---

### Task 3: The terminal obeys them

**Files:**
- Modify: `web/src/components/Terminal.tsx` (`TerminalBody` at 1515,
  `handleResize` at 1637, the `style` prop at 1672)
- Test: `web/src/components/Terminal.test.tsx`
- Create: `web/src/components/TerminalNudge.dom.test.ts`

**Interfaces:**
- Consumes: `useTerminalSettings`, `rowHeightFor`, `wtermThemeFor`,
  `reportTerminalGrid`, `TerminalGrid` from Task 1.
- Produces:
  ```ts
  export function nudgeRemeasure(
    host: HTMLElement | null,
    session: { noteLayoutChange(): void } | null,
    raf?: (cb: () => void) => void,
  ): void
  ```

- [ ] **Step 1: Write the failing tests**

In `Terminal.test.tsx` (node), against a fake element and a synchronous
`raf` double, plus Review Focus 4:

```ts
describe('nudgeRemeasure', () => {
  it('widens the box for exactly one frame', () => {
    // paddingRight is '1px' after the call and '' once the frame runs
  })
  it('arms the layout change on the restore, not on the shrink', () => {
    // noteLayoutChange has not been called when paddingRight is '1px';
    // it has been called exactly once by the time it is ''.
    // The arming is one-shot and only the restore carries the true size.
  })
  it.each([null])('does nothing without a host', () => { /* must not throw */ })
  it('survives a session that is gone by the time the frame runs', () => { /* null session */ })
})
```

And Review Focus 3, against the existing `TerminalSession` harness in that
file:

```ts
it('re-sends the size chosen while the socket was down', () => {
  // noteResize(newCols,newRows) with the transport closed, then reopen:
  // the frame that goes out carries the new pair, not the pre-change one.
})
```

- [ ] **Step 2: Run them and watch them fail**

Run: `cd web && pnpm vitest run src/components/Terminal.test.tsx`
Expected: FAIL — `nudgeRemeasure` is not exported.

- [ ] **Step 3: Implement `nudgeRemeasure` and wire the settings in**

`nudgeRemeasure` sets `host.style.paddingRight = '1px'` and, inside one
`raf` callback, calls `session?.noteLayoutChange()` and then clears
`paddingRight`. The order inside the callback is load-bearing: the arming has
to be in place before the box changes back, because the restore is the resize
that carries the true size.

In `TerminalBody`: `const { fontSize, theme } = useTerminalSettings()` and
`const { resolvedTheme } = useTheme()`; add `--term-font-size` and
`--term-row-height` to the existing inline `style` beside
`--term-font-family`; pass `theme={wtermThemeFor(theme, resolvedTheme)}` to
`WTermView`; in `handleResize`, `reportTerminalGrid({ cols: Math.min(cols,
GRID_MAX_COLS), rows, capped: cols > GRID_MAX_COLS })` alongside the existing
`noteResize` — `handleResize` receives wterm's **unclamped** count, which is
the only place both halves of the number exist. Report `null` on unmount.

A `useEffect` on `[fontSize]` calls `nudgeRemeasure(hostRef.current,
sessionRef.current)`, skipping its first run via a ref: the mount already
measured at the right size, and a nudge there would send a second resize for
nothing. Write down in the comment *why* the revert is on the next frame and
not in the same one — reverted within the frame, the observer sees no net
change and never fires, so the cell is never re-measured.

- [ ] **Step 4: Run them and watch them pass**

Run: `cd web && pnpm vitest run src/components/Terminal.test.tsx`
Expected: PASS.

- [ ] **Step 5: Add the jsdom test that the nudge is wired at all**

`web/src/components/TerminalNudge.dom.test.ts`, first line
`// @vitest-environment jsdom`, in the style of `TerminalWake.dom.test.ts`:
render `<Terminal>` with a stub transport, change the size through
`applyTerminalSettings`, and assert the host's `paddingRight` went to `1px`.
Nothing else: what a nudge *does* is already tested in Task 3 Step 1, and this
file exists only because deleting the effect would leave every one of those
tests green.

- [ ] **Step 6: Run the whole suite**

Run: `make test`
Expected: PASS, no new warnings on stdout.

- [ ] **Step 7: Commit**

```bash
git add web/src/components/Terminal.tsx web/src/components/Terminal.test.tsx web/src/components/TerminalNudge.dom.test.ts
git commit -m "feat: the terminal takes its cell and its colours from the settings"
```

---

### Task 4: Proof in a real browser, and the docs

**Files:**
- Create: `e2e/settings.spec.ts`
- Modify: `e2e/harness.ts` (a new exported action beside `openReplyBox` at 459)
- Modify: `README.md` (the "Things worth knowing" paragraph added by
  `GRID_MAX_COLS`, which currently says 256 columns and nothing about how to
  use a wide screen)
- Modify: `docs/plans/2026-11-07-tmux-web-terminal-settings-design.md` (status)

**Interfaces:**
- Consumes: the shipped UI.
- Produces: `export async function openTerminalSettings(page: Page): Promise<void>` in `harness.ts`.

- [ ] **Step 1: Write the failing tests**

```ts
test('a bigger font makes tmux narrower, and the grid still holds all of it', async ({ page, tmuxWeb }) => {
  // 1. Settle, read tmux's window_width and the cell (the hidden-W probe, as
  //    in font.spec.ts). 2. Raise the font to 20 through the real dialog.
  // 3. tmux's window_width must DROP. This assertion is a number on purpose:
  //    it is the ONLY thing that binds the nudge. Without it, a nudge that
  //    never fires leaves tmux at the old column count, the grid still holds
  //    all of it, and the invariant below stays green over a terminal drawing
  //    wider than its own box.
  // 4. The invariant, as in sizing.spec.ts: print a line of exactly `cols` W's
  //    and the widest rendered /^W+$/ row must be `cols` long.
  // 5. The rows really are rowHeightFor(20) = 24px tall -- which is what
  //    proves wterm's init probe took our inline --term-row-height rather
  //    than overwriting it with 17px.
  // 6. tmux's width is min(floor(contentWidth / cell), GRID_MAX_COLS) exactly
  //    -- not one column short. The nudge shrinks the box by 1px before it
  //    restores it, and this is what says the restore landed rather than the
  //    shrink being the last thing tmux heard.
})

test('a light terminal paints a light background', async ({ page, tmuxWeb }) => {
  // Pick Light; getComputedStyle('.wterm').backgroundColor must be the light
  // theme's, and must differ from what it was before.
})
```

- [ ] **Step 2: Run them and watch them fail**

Run: `cd e2e && pnpm exec playwright test settings.spec.ts`
Expected: FAIL — `openTerminalSettings` does not exist.

- [ ] **Step 3: Add the harness action and run them green**

`openTerminalSettings(page)` opens the sidebar footer menu and clicks
`Terminal…`, mirroring `openReplyBox`.

Run: `cd e2e && pnpm exec playwright test settings.spec.ts`
Expected: PASS.

- [ ] **Step 4: Prove the new assertions bite**

Mutate, one at a time, and watch the right test fail: (a) delete the
`useEffect` that nudges — the `window_width` assertion must fail; (b) return
`17` from `rowHeightFor` unconditionally — the row-height assertion must fail;
(c) pass `theme={undefined}` always — the light-background test must fail.
Restore by editing back, never `git restore`.

- [ ] **Step 5: Look at the light terminal and fix what it exposes**

`terminal.css` draws the selection and the scrollbar from `--term-*`, and
nothing has ever rendered this app's terminal on a light background. Screenshot
it, select some text, scroll it. Anything illegible gets a rule of ours in
`index.css` scoped to the light theme class, plus a line in the e2e test that
pins it; anything legible gets no code and a sentence in the commit saying it
was looked at.

- [ ] **Step 6: Update the README and the design doc's status**

The wide-screen paragraph gains the setting: a bigger cell is how you use a
screen wider than 256 columns, and it lives in `Terminal…` in the user menu.
Say the settings are per device and in the browser, so the phone and the
desktop keep their own. Design doc `Status: implemented`.

- [ ] **Step 7: The whole suite, both halves**

Run: `make test` and then the full Playwright run.
Expected: PASS; `gofmt -l` and `go vet ./...` clean; working tree holds only
the intended files.

- [ ] **Step 8: Commit**

```bash
git add e2e/settings.spec.ts e2e/harness.ts README.md docs/plans/2026-11-07-tmux-web-terminal-settings-design.md
git commit -m "test: tmux learns the new cell, and the docs say where the setting is"
```
