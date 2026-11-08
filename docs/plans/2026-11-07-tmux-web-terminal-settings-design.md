# tmux-web — terminal settings: size and colours

Date: 2026-11-07
Status: implemented

## Purpose

The terminal's cell is fixed. `@wterm/dom` ships `--term-font-size: 14px` and
`--term-row-height: 17px`, this app overrides neither, and there is no control
anywhere that changes them. That became a user-visible problem the day
`GRID_MAX_COLS` landed: the grid cannot draw past 256 columns, so on a wide
screen the only way to use the whole width is a bigger cell, and there is no
way to ask for one.

The terminal's colours are fixed too, and wrongly: `@wterm/react` applies a
palette by class and this app passes no `theme`, so the terminal is wterm's
default dark **even when the interface is in light mode**. The app's
light/dark choice does not reach the thing that fills most of the screen.

Give the user both, in one place: **Terminal…** in the user menu, opening a
dialog with a font size and a terminal theme.

## Approaches

The question was how to model colours, because in wterm light/dark and palette
are **the same axis**: it ships four themes and exactly one is light (default
dark, `theme-solarized-dark`, `theme-monokai`, `theme-light`).

- **A — one list, "Theme".** Follow interface / Dark / Light / Solarized Dark /
  Monokai. One control, no CSS of our own, and "Follow interface" is what makes
  the light-background request work without being asked twice. Costs: no
  combinations.
- **B — two axes.** Palette (Default / Solarized / Monokai) × background
  (Light / Dark / Follow interface). To be true it needs the missing halves
  written by us: Solarized Light is canonical, but a light Monokai would be our
  invention — a palette nobody has ever seen, presented beside three real ones.

**Chosen: A** (the owner picked it). B promises a grid of options two thirds of
which would be made up here.

A third option — ship nothing but light/dark now — was dropped: the palettes
are four lines of mapping once the control exists, and the owner asked for them
in the same breath.

## Decisions

1. **Two menu entries, not one.** `Terminal…` opens the dialog; `Appearance`
   stays exactly as it is and goes on being the *interface* theme. Folding the
   app's theme into something called "Terminal" would be a lie, and so would
   calling the dialog "Settings" while the app's own theme sits outside it.
   This is my proposal, not the owner's words: he said "settings de terminal".

2. **Font size is two CSS variables, set inline on the `WTermView`, beside
   `--term-font-family`.** `--term-font-size: <n>px` and
   `--term-row-height: <round(n * 1.2)>px`. Both, never one: `.term-row` is
   `height: var(--term-row-height)`, a hard pixel box that does not follow the
   font, so a size raised alone clips its own text. The ratio reproduces the
   shipped default exactly — `round(14 * 1.2) = 17` — so the factory setting is
   byte-for-byte what wterm ships today. Inline for the same reason
   `--term-font-family` is: wterm defines these on `.wterm` itself, and an
   inline custom property is beyond any specificity argument.

   `WTerm._setRowHeight()` runs once at init and sets `--term-row-height` from a
   `.term-row` probe — whose height *is* that variable. It therefore re-affirms
   whatever we set and cannot disagree with it. MEASURED claim; the
   implementation verifies it rather than trusting this paragraph.

3. **A size change must nudge the box, because nothing else makes wterm
   re-measure.** `_measureCharSize()` runs at init and once per
   `ResizeObserver` delivery, and the observer watches the terminal element's
   content box. Changing the font changes no box, so without help the cell stays
   measured at the old size and tmux is told the old column count — the exact
   class of bug `GRID_MAX_COLS` was just written to kill.

   So, after the new size is committed: set `padding-right: 1px` on the host
   (ours, not wterm's DOM), hold it for a whole animation frame, and clear it in
   the frame after. The observer fires on the shrink, re-measures with the new
   font and resizes; if the 1px crossed a cell boundary it fires again on the
   restore and lands on the true width. Two local grid resizes at most,
   collapsed into a single `resize` frame to tmux by the 150 ms debounce in
   `noteResize`.

   **Corrected while implementing, both halves measured.** The first draft of
   this decision said "clear it on the next animation frame" and "the restore —
   not the shrink — arms `noteLayoutChange()`". Both were wrong:

   - A `ResizeObserver` is broadcast *after* every animation-frame callback of
     the same frame, so a padding cleared in the next frame's callback is
     cleared before the observer has ever looked: net zero change, nothing
     fires, nothing re-measures. Measured in Chromium — the font reached the
     element (20px, 24px rows, a 12px cell in a 1320px box = 110 columns) and
     tmux stayed at 165. Hence the extra frame.
   - `@wterm/dom` calls `onResize` only when the column or row count actually
     changed. A font size moves both, so the delivery that fires is the one for
     the *shrunk* box; by the restore the counts usually match what it already
     has and nothing is delivered. An arming placed on the restore is therefore
     spent on a resize that never comes and left standing for the whole
     `LAYOUT_CHANGE_GRACE_MS` — a second in which a phone's keyboard rising
     would be pushed to the shared window, which is the failure suppression
     exists to prevent. The arming goes on **before the box moves at all**.

   And suppression is *not* reliably off while the dialog is open: the theme
   control is a native radio group, and `suppressesResize` treated every
   `INPUT` as a keyboard. That predicate now excludes the input types that
   raise no soft keyboard (radio, checkbox, button, range, …), which is the
   more honest reading of what it is for. The arming remains the insurance for
   any path that does focus a text control.

   Rejected: computing the cell ourselves and calling the public `resize()` —
   it leaves wterm's own `_charWidth`/`_rowHeight` stale, which silently breaks
   mouse cell mapping and the `CSI 14 t` pixel report until the next real
   resize. Rejected: remounting the terminal — it tears down the WASM grid and
   the socket to change a font.

4. **Theme maps to the `theme` prop, which is reactive.** `@wterm/react`
   recomputes `theme-${theme}` on every render, so no remount:
   `light` → `light`, `solarized-dark` → `solarized-dark`, `monokai` →
   `monokai`, `dark` → `undefined` (wterm's default *is* the absence of a
   class). `system` resolves through next-themes' `resolvedTheme`.

5. **`system` is the default, and it is a behaviour change.** A terminal that
   stays dark while the interface is light is the bug this fixes, so the
   default has to be the fixed behaviour, not the old one. Anyone in dark or
   system-dark — which is this app's own default — sees nothing change.

6. **Settings are per device, in `localStorage`.** Key `tmux-web:terminal`,
   JSON `{ fontSize, theme }`. A phone and a 27" screen want different sizes,
   and the daemon has no per-device preference store to put this in. Same
   contract as `readSeen`/`readReplyOpen`: absent, corrupt, foreign or
   *throwing* storage all read as the defaults, and a failed write is dropped.
   Values are clamped and validated on read, never trusted.

7. **10 to 24 px, whole numbers.** Below 10 the frames stop being legible on a
   phone; above 24 nothing is gained that a browser zoom would not do better,
   and the ceiling matters because a grouped session shares its window — a
   browser tab that asks for a very large cell is asking the owner's own
   terminal to get narrower. On a 2560px viewport the 256-column cap stops
   binding at about 16px, so the stated purpose is reached well inside the
   range. Whole pixels only: `round(n * 1.2)` and wterm's `Math.ceil` of the
   row probe agree cleanly only for integers, and the picker in decision 8 has
   to enumerate them anyway. The clamp on read is to this range; anything
   outside it, or not an integer, reads as the default.

8. **A size *picker*, not a slider, and applied live with no OK button.** Each
   choice is one change: picking 18 from a list resizes once, where dragging
   14 → 18 would have sent four distinct sizes. That matters more here than in
   most apps, because the tab's session is grouped — every size it sends
   reaches the window the owner's own client is attached to, and reflows every
   TUI in it. The owner's call, and the right one: a slider would have had to
   choose between a burst of resizes and a blind drag that only lands on
   release.

   Live, because once each choice is a single commit there is nothing left to
   confirm: the preview *is* the result. The dialog is a modal over the very
   thing being previewed, which is one more reason not to make the user hold a
   control down while they look.

   Every whole size in range is offered, with no gaps: a curated list would be
   picking favourites among numbers that are all equally real. The terminal
   theme stays a radio group — five named choices worth seeing at once, the
   way `Appearance` shows its three.

9. **The readout is what `noteResize` recorded, and it says when the clamp is
   biting.** "232 × 64", from the clamped pair `noteResize` stores — not from
   the send, which is debounced, held entirely while suppression is on, and
   goes nowhere with the socket down. The recorded pair is what this app
   believes the size to be; a readout wired to the send would go blank or stale
   for reasons that have nothing to do with the font.

   One number cannot answer the question the readout exists for: `256` reads
   the same whether the box would give 285 columns or genuinely gives 256. So
   when the clamp is biting the dialog says so, from the unclamped count wterm
   hands to `handleResize` on the very same callback. That is not the "second
   measuring path" this decision rejects — it is the other half of one
   measurement, and without it the user cannot tell when the font has finally
   become the binding constraint.

   With no terminal mounted, or before the first resize, the readout says so
   rather than inventing a size.

10. **One font family, still.** The size control ships; the family does not.
   Of the families measured when `monoFont.ts` was written, only Geist Mono
   covers `U+2500–259F`, so today's picker would read "Geist Mono / the
   device's". Adding real choices means measuring glyph advances against the
   cell for each candidate and paying bundle weight per survivor. Deferred
   deliberately, with the owner's agreement; the dialog gains one row when
   there is measurement behind it.

11. **Built from what is already installed, and with no new wrappers either.**
    `Dialog` and `DevicesDialog` are the precedent for the modal. The two
    controls are *native*: a `<select>` for the size and a
    `<fieldset>`/`<input type="radio">` group for the theme, styled with the
    app's own tokens. `radix-ui` would have served (it carries select and
    radio-group), and two shadcn wrappers were the first plan — but on a phone
    a native `<select>` is the OS picker, which beats anything a listbox can
    do with fifteen numbers, and this app is used from a phone. No new
    package, no new file under `components/ui`.

## Non-goals

- The interface's text size. The owner was explicit: these settings are the
  terminal's. (`Appearance` remains the interface's only control.)
- A font family picker — see decision 10.
- Custom colours, or any palette we author ourselves — that was approach B.
- Per-pane or per-session settings. One terminal view, one setting.
- Syncing settings between devices. Decision 6 is deliberate, not a shortcut.
- Raising `GRID_MAX_COLS`. A bigger font is how you use a wide screen; the cap
  is the renderer's and is not ours to move.

## To verify while implementing

- That `_setRowHeight()`'s init probe really takes our inline
  `--term-row-height`, rather than wterm overwriting it with 17px on mount.
  Decision 2 argues it from the CSS; the e2e run is what proves it — a 20px
  font must produce 24px rows on the real page.
- That the nudge lands the *true* column count, not the one-pixel-narrower one,
  and that a reverted nudge in the same frame would not have fired at all.
- That React's `style` prop and wterm's own `element.style.setProperty` do not
  fight over `--term-row-height` across re-renders.
- Whether a light terminal needs anything of ours for the scrollbar and the
  selection colour, which `terminal.css` draws from `--term-*`.
- That the unclamped column count really is available to the readout on the
  same `handleResize` callback as the clamped one (decision 9), rather than
  having to be recovered somewhere else.

## Tests

- **Pure, node:** parse/clamp/default of the stored value — absent, corrupt,
  out-of-range, wrong type, throwing storage; `rowHeightFor(14) === 17`.
- **Markup:** the user menu carries both `Appearance` and `Terminal…`; the
  dialog renders five theme choices and every size in range exactly once; the
  readout shows the reported grid, and says when the clamp is biting.
- **Terminal:** the two custom properties reach the element at the configured
  size; `system` resolves to `light`/`undefined` by `resolvedTheme`; the nudge
  runs once per size change, reverts on the next frame, and does **not** run on
  mount.
- **e2e, two assertions, because one of them cannot fail.** Raising the font
  through the real UI must (a) make tmux's `window_width` **drop** — a number,
  deliberately, because this is the only thing that binds decision 3's nudge:
  if the nudge never fires tmux simply keeps the old column count, the grid
  still holds all of it, and the invariant assertion stays green over a
  terminal drawing wider than its own box — and (b) still render a line of
  exactly `cols` `W`s in full, which is `e2e/sizing.spec.ts`'s existing
  invariant, now at a cell this app chose. `tmuxCols`/`stableCols` in
  `e2e/font.spec.ts` already do the first half's measuring. Plus: a light
  terminal really paints a light background.
