# tmux-web header controls — design

**Status:** approved
**Date:** 2026-11-16

## Intent

The header's right side carries four controls and a dot: `Jump to…`, the
copy-mode control, `Reply box`, `Scrollback`, and `ConnectionDot`. Three of
them are text buttons that never shrink, so on a 390px phone the right side is
roughly 300px of fixed content and the breadcrumb — the one thing that says
*which pane you are looking at* — is left with about 30px. The owner's words:
"en movil entre la lupa y el resto de elementos no deja lugar para ver en que
estás".

On the desktop the problem is different and smaller: two of the four are
controls he never uses, in a header that has room for them.

## What the owner actually uses

From the conversation, not inferred:

- **On the phone:** switching pane/agent, and the reply box *sometimes*.
- **On the desktop:** never the reply box.
- **Scrollback:** was essential while copying out of a pane was broken; since
  OSC 52 works it is occasional.
- **The dot:** he could not say what it was for, which is the finding.

## Goals

1. On a phone, the breadcrumb gets the width back.
2. Nothing he uses gets further away, on the device where he uses it.
3. Each remaining control is one tap on that device.
4. No device sniffing: width rules in CSS, and defaults that do not guess.

## Non-goals

- No new header layout, no tab bar, no overflow `…` menu.
- Scrollback gets no sidebar-row entry: `rowMenu` (`lib/manage.ts:437`) is
  tmux verbs, and a UI action there would need a new intent kind.
- `ConnectionPill`, `NoSession` and the user menu's worded connection state are
  untouched — they are what makes the dot redundant.
- The reply box's own geometry does not change: it stays a flex sibling of the
  terminal rather than an overlay, which the owner decided in Task 21.

## Anti-goals

Two states this must not be able to reach, even though no current test would
catch either:

- An open reply box with nothing left that can close it.
- A pane in copy mode on a phone with no visible way out of the mode.

## Approaches

Three were on the table; the owner picked **B**.

- **A — CSS only.** Icon below `sm`, text above, which is the pattern the
  search button in this same header already uses. One `<span>` per button, no
  new state. Cheapest; leaves the desktop carrying a reply-box button he never
  uses, where there is room for it.
- **B — A, plus a toggle per control (chosen).** `Jump to…` and `Reply box`
  become optional per device, so the phone can have one, both or neither and
  the desktop can be just the breadcrumb and the magnifier.
- **C — An overflow `…` on mobile.** One fixed icon, everything inside.
  Rejected: it makes the reply box two taps, and the reply box is one of the
  two things he actually uses on the phone.

**One deviation from what the owner asked for, and his call to take:** he asked
for defaults *by device* — both controls on for a phone, only the magnifier on
a desktop. A default by device has to guess the device, which is exactly what
the reply box's current comment refuses ("no `pointer: coarse` branch and no
viewport-width one"), and the guess is wrong on a touchscreen laptop and on a
tablet with a keyboard. Since these settings are already per-device
`localStorage`, the same end state costs **one click, once, on the laptop**. So:
both default **on**, everywhere. He accepted this when choosing B.

## The decisions

### D1 — The connection dot is deleted

Every state it names is stated louder somewhere else:

| phase | what already says it |
|---|---|
| connecting, reconnecting, ended, gone | `ConnectionPill`, over the terminal (`Terminal.tsx:1851`) |
| ready but keystrokes were dropped | the same pill, with escalated wording |
| closed | there is no terminal — `NoSession` renders instead |
| all of them, in words | `UserMenu`'s footer line (`connectionLabel`) |

Below `sm` the dot's own word is hidden, so on the phone it is a green dot that
is green nearly always. Deleted, not hidden: a control with no information is
not a control that belongs on a narrower screen either.

### D2 — The copy-mode control shows only the way *out*

Rendered only when the pane's top mode is a copy mode (`inCopyMode`, which the
app already computes). Why the way *in* does not earn header space:

- In a **shell** pane the wheel and the swipe already enter copy mode, through
  the owner's own `~/.tmux.conf` binding.
- In an **agent** pane (`alternate_on`) the wheel goes to the application
  instead — that is the scroll-gain feature we shipped a week ago — and tmux
  keeps no scrollback behind an alternate screen, so copy mode there shows
  nothing worth entering for.

The deliberate way in stays the palette's `toggle-copy` row, which already
shows whichever direction applies. The control keeps
`disabled={status?.phase !== 'ready'}`: the pane's mode comes from the daemon's
poll, not from this tab's socket, so "in copy mode with a dead socket" is
reachable and `endMode` would fail silently. What covers that guard is D8.

**What gates its presence is `copy.inCopy` — the flip-aware value from
`copyControl` (`App.tsx:552`), not raw `paneMode`.** The optimistic flip exists
because the label would otherwise rename itself up to a poll after the click,
"which reads as a control that did nothing and then did something on its own"
(`COPY_FLIP_MS`'s own comment). Presence inherits that argument whole: pressing
`Exit copy` unmounts it at once, which is the honest answer to a press that
means *get me out*, and entering from the palette row makes it appear at once
rather than a poll later. The cost is the refused-`cancel` case, where it
vanishes and comes back when the flip expires three seconds later; that is the
same three seconds the label has always been able to be wrong for, and raw
`paneMode` would trade it for a wrong answer on every ordinary click.

### D3 — Scrollback moves to the palette

A new palette action `{ kind: 'open-capture' }` and one row in the existing
`Terminal` group, beside the copy-mode and reply rows. **Absent** rather than
disabled when there is no active pane, following that group's own stated rule
("the row that does not apply is not disabled, it is absent").

### D4 — Icon below `sm`, text at `sm` and up

The pattern is already in this header: `<Search/>` plus
`<span className="hidden sm:inline">Jump to…</span>`. Applied to the reply
toggle and the copy control, with the glyphs the palette already uses for the
same two actions (`MessageSquare`, `CopySlash` — `Palette.tsx:503-513`), so the
button and the row that do one thing carry one picture.

**The accessible name must not change with the width.** `hidden` is
`display:none`, and `display:none` text is not in the accessible name, so a
button whose name comes from its text is a button with two different names at
two widths — and that is what the e2e suite locates these controls by. Each of
the three gets an explicit `aria-label` that does not depend on the width, and
the visible text stays the same string.

The strings, chosen so that every existing locator keeps working:

| control | `aria-label` | why |
|---|---|---|
| jump | `Jump to…` | `e2e/terminal.spec.ts:194` matches `/Jump to/` |
| reply | `Reply box` | `e2e/harness.ts:459` matches it exactly |
| copy | `copy.label` — `Copy mode` / `Exit copy` | `e2e/terminal.spec.ts:73` matches `/^(Copy mode\|Exit copy)$/` |

So **width-independent does not mean mode-independent**: the copy control's
name still follows the mode, because the mode *is* that control's name — the
decision `copyMode.ts` was written to hold. The reply button's name stays the
noun and does not become "Show/Hide…", with `aria-pressed` carrying the state,
which is what its current comment already argues for.

One consequence for the plan: an assertion on *visible text*
(`toHaveText('Exit copy')`) only holds at `sm` and up, so it stays in the
desktop-viewport tests. A phone-viewport test asserts the accessible name
instead.

### D5 — Two toggles, in a `Header` submenu of the user menu

A `DropdownMenuSub` named **Header** beside `Appearance`, mirroring
`AppearanceMenu`'s shape, holding two `DropdownMenuCheckboxItem`s: `Jump to…`
and `Reply box`. Both default on, on every device (see Approaches).

They live in a **new** module, `lib/headerSettings.ts`, under its own key
`tmux-web:header`, built the same way as `lib/terminalSettings.ts`: an
injectable store, a parse that falls back field by field, and a module-level
`useSyncExternalStore`. Not folded into `TerminalSettings`, whose own header
says it is about the thing drawing *inside* the terminal; a header button is
not that.

**The copy-mode control gets no toggle.** On a phone it is the only obvious way
out of the mode, and a switch that can hide it is a switch that can trap him.

### D6 — Turning the reply toggle off closes the box

The box has no close affordance of its own (`ReplyBox.tsx:471-510`): it is
closed by the header button or by the palette row. So hiding the button while
the box is open leaves the box standing, and with the magnifier off too there
is nothing left that can close it. Turning the setting off therefore goes
through the same `showReply(false)` the button calls — the one that tells tmux
about the resize first (`noteLayoutChange`), so the owner's own client does not
get a size nobody asked for.

**The flag gates the button, not the box.** `showReplyBox` (`ReplyBox.tsx:261`)
is untouched, so the palette's reply row still opens a box on a device whose
header does not carry the button — which is exactly the desktop case: the owner
never uses it, so he wants no button, not a feature he cannot reach the day he
wants it. If the flag gated the box instead, the palette row would be a control
that silently does nothing.

**How it is wired, and why not the obvious way.** `showReply` lives in `App`;
the submenu lives in `UserMenu`, mounted from `AppSidebar` — a sibling subtree
with no callback channel, which is the same split `terminalSettings` exists to
bridge and the reason it is a module-level store rather than a context. So
`headerSettings` notifies its subscribers **synchronously inside the setter**,
and `App` registers one that calls `showReply(false)` when the flag goes false.
Not a `useEffect` watching the flag: an effect runs after the commit that has
already unmounted the box, so `noteLayoutChange` would arm *after* the resize
it exists to claim — and with `LAYOUT_CHANGE_GRACE_MS` at one second that would
usually work, which is the worst kind of wrong. Not a prop threaded
`App → AppSidebar → UserMenu` either: two component APIs widened for one
boolean.

### D7 — What hiding the magnifier on a phone costs, and why that is fine

The palette has no other door on a phone: `Ctrl+Alt+K` needs a keyboard. So
with the magnifier off, Scrollback (D3) and the deliberate copy-mode entry (D2)
are unreachable on that device, and pane switching falls to the sidebar — which
is what he uses there anyway. Accepted: it is on by default and one tap to put
back. The sidebar is never hideable, so no device can lose pane switching.

### D8 — The copy control's three states become a function, so they can be tested

D2 keeps `disabled` for "in copy mode with a dead socket", and the rewrite this
document first prescribed for `e2e/terminal.spec.ts:187` — assert the button is
absent on a revoked device — is **vacuous** once the button exists only in copy
mode: that test's pane is never in a mode, so the assertion holds whether the
guard is there or not. Reaching the real state from the browser means entering
copy mode and *then* killing the socket, which is an expensive fixture for one
attribute.

So the decision moves out of the JSX, exactly as `copyMode.ts` already says the
rules must ("the rules live here, outside the components, because
`renderToStaticMarkup` fires no handlers: a rule written inside an `onClick` is
a rule no test in this suite can reach"). A pure function in that module takes
the control's state and the socket's phase and returns which of three things
the header does: **absent**, **disabled**, or **live**. Its unit tests in
`copyMode.test.ts` cover all three, including the pair the browser cannot cheaply
reach, and the component becomes a `switch` over its answer.

## What it measures out to

A 390px phone, both toggles on, pane not in copy mode. Estimated from the class
lists, to be confirmed by the e2e that the plan will add:

| | fixed right side | breadcrumb gets |
|---|---|---|
| today | search 30 + copy 78 + reply 72 + scrollback 78 + dot 8 + gaps ≈ **300** | **≈ 30px** |
| after | search 30 + reply 30 + gap ≈ **68** (+38 while in copy mode) | **≈ 260px** |

On a desktop with the reply toggle off, the right side is the magnifier alone.

**What the e2e asserts is not these numbers.** A pixel figure is brittle
against glyph-only button sizing and would have to be rewritten the next time
an icon changes; and "the command is visible" alone would pass at 180px too.
The assertion is the complaint: at a 390px viewport, on a pane whose command is
long enough to be worth reading, the breadcrumb's command segment is **not
clipped** — its `scrollWidth` fits its `clientWidth`. Non-vacuous by
construction, and the plan verifies that by mutation: the same assertion must
fail against today's header, which is the state the owner is complaining about.
If the measured width then comes back below the estimate while that assertion
passes, the table is corrected, not the design.

## Tests this breaks, which the plan must carry

- `e2e/capture.spec.ts` reaches the `Scrollback` button through one helper
  (`openPanel`, `:150`) → must go through the palette. One call site.
- `e2e/terminal.spec.ts:64-101` drives the header copy control in **both**
  directions → the entering half moves to the palette row, which that same
  file already clicks at `:196`; the in-mode half stays, at desktop width.
- `e2e/terminal.spec.ts:187` asserts `Copy mode` is disabled on a dead socket →
  the guard it was covering moves to D8's unit tests; absence here is vacuous
  and must not be left standing as if it covered anything.
- `e2e/harness.ts:459` clicks `Reply box` → survives unchanged, which is what
  D4's table is for.
- **Nothing in `web/src/App.test.tsx` asserts the dot or the phase word**, and
  the harness's pill locator (`[role="status"][aria-live="polite"]`) does not
  match `ConnectionDot`'s bare `role="status"`. D1 is test-neutral; an earlier
  draft of this list claimed otherwise.

## Left to the builder

- Scrollback's glyph (`ScrollText` is the obvious one).
- Whether `headerSettings` exposes one hook returning both flags or two, and
  the name of the pure function D8 asks for.
- The `Header` submenu's two item labels, which must read as the buttons they
  govern — D4's table fixes those names.
