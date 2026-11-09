# tmux-web — scroll gain: one notch is not always five lines

Date: 2026-11-09
Status: approved

## Purpose

A swipe on a phone scrolls a pane by emitting wheel events, and
`lib/touchScroll.ts` decides how much finger travel buys one notch:
`pxPerNotch = rowHeight * TMUX_LINES_PER_NOTCH`, with `TMUX_LINES_PER_NOTCH = 5`.

That 5 is tmux's. It is right only while **tmux** is the thing handling the
wheel. Every coding agent in this workflow runs in the alternate screen with
mouse tracking on, which is precisely the case where tmux hands the event to
the application instead -- and the application has its own quantum, measured
below at 1 line for `claude` and `pi` and 3 for `opencode`.

So the gain is wrong by a factor of 5 in the panes this app exists to drive.
Reported from the phone, and reproduced: over a shell pane a swipe feels right,
both slowly and thrown; over an agent pane it is heavy and the throw barely
carries.

Give the gesture the right quantum for whoever is going to receive the notch.

## What was measured

All on this machine on 2026-11-09, against tmux 3.7b.

### 1. The fork is tmux's own binding

```
WheelUpPane  if-shell -F "#{||:#{alternate_on},#{pane_in_mode},#{mouse_any_flag}}" \
               { send-keys -M } { copy-mode -e }
copy-mode    WheelUpPane  select-pane ; send-keys -X -N 5 scroll-up
```

Three cases, and the `-N 5` is where `TMUX_LINES_PER_NOTCH` comes from:

| pane | what one notch does | lines |
| --- | --- | --- |
| already in a mode | the copy-mode table scrolls | 5 |
| no mode, no alternate screen, no tracking | `copy-mode -e`, then the table | 5 (first notch only enters) |
| no mode, **alternate screen or tracking** | `send -M`: the application gets it | the application's own |

### 2. Every agent is in the third case, twice over

```
%1159 claude    alt=1 any=1 all=1 btn=0
%1582 opencode  alt=1 any=1 all=1 btn=0
%1496 pi        alt=1 any=1 btn=1 all=0
%761  tmux-web  alt=0 any=0 all=0 btn=0     (a plain shell: the first two cases)
```

Both `alternate_on` and `mouse_any_flag` are set on all three, so the forwarding
is doubly determined: even an agent that turned mouse tracking off would still
be forwarded for being on the alternate screen -- where tmux has no scrollback
to show anyway.

### 3. The receiver's quantum

Method: a tmux server of its own (`-L scrollprobe -f /dev/null`), the agent
started in a 100x24 pane, and the SGR report injected as pane **input** with
`send-keys -H 1b 5b 3c 36 34 3b 35 30 3b 31 32 4d` -- byte for byte what tmux's
`send -M` forwards. Validated against `cat -v` in the same server, which
received `^[[<64;50;12M` verbatim, so a pane that did not move had received the
report and declined it. Movement counted by the row of a unique marker line
before and after.

| receiver | lines per notch, notches ≥60ms apart |
| --- | --- |
| tmux copy-mode | **5** (the `-N 5` above) |
| `claude` | **1** (1, 1, 1, and 5 notches → 5 lines) |
| `opencode` | **3** (two independent markers) |
| `pi` | **1** (1, 1, and 2 notches → 2 lines) |

And `claude` accelerates when reports arrive faster -- 10 notches, by spacing:

| spacing | 0ms | 10ms | 30ms | 60ms | 120ms |
| --- | --- | --- | --- | --- | --- |
| lines moved | 16 | 19 | 19 | 10 | 10 |

So its accelerator engages somewhere between 30 and 60ms and roughly doubles
the quantum, and it ramps with sustained speed: 30 notches back to back moved
127 lines (4.2 per notch). The 0ms column being *below* the 10ms one is input
coalescing -- a tight burst is read in one chunk and partly merged, so a burst
is worth slightly less than the same notches spaced across frames. `pi` appears
to accelerate too (8 notches at 16ms moved more than 20 lines); not pinned down,
and nothing below depends on the number.

### What that does to the current code

- **Drag.** At today's 85px notch the finger produces notches more than 60ms
  apart at any human speed, so the quantum is the unaccelerated one. (The fix
  moves that boundary, which is decision 7's whole subject.) 85px of travel buys 1 line =
  17px of content: **a fifth of the finger** in `claude` and `pi`.
- **Throw.** `Glide` runs at up to 6px/ms, which at 85px per notch is about one
  notch per frame -- 16ms, inside `claude`'s accelerated zone. So a flick yields
  ~1.9 lines per notch over ~19 notches: about 36 lines where copy-mode gets 94.
  Bad, but less bad than the drag, which is exactly what was reported.

## Approaches

- **A — the gain follows the receiver.** The daemon publishes the two tmux
  facts that decide the fork; the browser picks 5 lines for the tmux cases and
  the application's quantum for the forwarded one, keyed on the agent it already
  knows. One change fixes drag and throw together, because shrinking
  `pxPerNotch` to 17px also makes the same glide ask for 94 notches instead of
  19. Costs a small table of per-agent constants that goes stale if an agent
  changes its own quantum, and leaves fast drags brushing against the
  application's accelerator.
- **B — a scroll-speed setting** in the Terminal dialog. Cheap, and absorbs any
  variation. Rejected as the primary fix: one number is necessarily wrong
  somewhere, because a shell pane wants 5 and `claude` wants 1 **in the same
  session**, and the user would be tuning around a fact the software can read.
- **C — self-calibration.** wterm's rows are in the DOM, so comparing them
  before and after the priming notch would say how many lines actually moved,
  and the gain would need no table. Right for any TUI, and rejected for now:
  the calibration is least trustworthy in exactly the panes that matter, the
  ones repainting on their own while an agent streams output.

**Chosen: A.** It attacks the measured cause with facts the daemon is already
one format field away from having, and the same change repairs both symptoms.
C stays written down as the version that would never need a table.

## Decisions

1. **The predicate is tmux's, restated.** One notch is 5 lines unless the pane
   is in no mode **and** (`alternate_on` **or** `mouse_any_flag`); then it is
   the application's quantum. `pane_mode` is already on the wire. Deriving the
   predicate in the browser rather than publishing a ready-made boolean keeps
   it next to the quantum table it feeds and testable in one place.

2. **Two new snapshot fields:** `#{alternate_on}` and `#{mouse_any_flag}`,
   added after `#{pane_mode}` and before the label, which stays last for the
   reason `snapshot.go` already documents. `fieldCount` goes 15 → 17. Both are
   tmux-generated `0`/`1` and need no sanitising.

3. **The quantum table is keyed on `pane_current_command`**, and on nothing
   else: `claude` → 1, `pi` → 1, `opencode` → 3, anything else → **3**. There is
   no agent name on the wire to key on -- `AppOption` is `@tmux_web_owned`, a
   flag -- and the identity is derived from the command on both sides already
   (`tmux.KnownAgent`, `AGENT_MARKS`). Keying on the command is not the frontend
   deciding what counts as an agent, the rule `useSnapshot.ts` states: the Go
   list still gates state, capture and logo, and this table decides none of
   those. It is a per-program constant, and a command it does not know gets the
   default and scrolls fine. Keying it on `agentState` instead would be worse,
   not better: that field is `""` whenever the poll was taken with no browser
   connected, so the gain would change under a pane that did not.

   3 is the default because it is both the conventional wheel delta and the
   safe side of the error. The content moves at `real / assumed` times the
   finger, so assuming too *large* a quantum makes an unknown TUI feel heavy,
   and assuming too small makes it fly past the place the user was reading.
   Heavy is recoverable; lost is not.

4. **The gain reaches the gesture through a module-level store**, the idiom
   `terminalSettings.ts` just established: `App` holds both the snapshot and
   the active pane id, so it reports the active pane's two facts, and
   `installSwipeScroll` reads the resulting lines-per-notch through an injected
   getter. A getter rather than an argument because the listeners are installed
   once with `[]` deps and must not be torn down mid-gesture; it is read at
   `scroller.start()`, so a gesture keeps one gain from beginning to end.

   **The mode half of the predicate reads the app's decided copy-mode value,
   not the raw field.** `App.tsx` already holds `copyControl(paneMode, …,
   copyFlip, …)` because the poll is up to 1.5s behind and the header's own
   control cannot wait for it, and the store is written in that same component.
   Reading `paneMode` raw instead has one genuinely bad case: an agent pane put
   into copy mode from this app's own button is still `alternate_on = 1`, so the
   raw read says "forwarded", picks quantum 1, and scrolls **five times too
   far** through the one thing tmux is now handling itself -- the unrecoverable
   direction decision 3 is built to avoid. Reusing the decided value costs one
   argument and removes the case.

5. **Two stalenesses are accepted.** The facts are up to 1.5s old, so a pane
   that has just changed regime -- an agent exiting, or copy mode entered from
   the owner's own terminal rather than from this app -- gives the previous gain
   for one gesture before the next poll corrects it. Accepted because nothing
   can know sooner than the poll, it self-corrects, and the app's own copy-mode
   button is covered twice over: by decision 4's decided value, and by the
   forced poll `internal/front/ws.go` already does on `copy-mode`/`end-mode`.

   And **the active pane is what the gain follows**, not the pane under the
   finger.
   The snapshot carries no pane geometry, and tmux routes a wheel report by its
   coordinates, so in a split window a swipe over the inactive half uses the
   active half's quantum. Accepted rather than fixed: a tap selects a pane,
   which is how one starts working with it anyway, and the mismatch costs the
   same wrong gain that ships today. Pane geometry is the fix if it ever bites.

6. **The finger's notches are never dropped; the glide's are capped at one per
   frame.** Two measured reasons. A tight burst is worth less than the same
   notches spread over frames (16 vs 19), and at 17px per notch an unbounded
   glide would queue ~94 notches it cannot deliver in its own ~600ms lifetime
   and would go on scrolling after the gesture visibly stopped. Capping and
   discarding the overflow lands every regime in the same place without
   modelling anybody's accelerator: ~68 lines for `claude` at its mild 1.9x,
   ~93 for `opencode` under the cap, 94 for copy-mode. The cap cannot bite a
   drag -- one notch per frame at 17px is 1018px/s of finger, past any thumb --
   which is why it applies to the glide only and the finger's own travel is
   still exact.

7. **Nobody's acceleration curve is modelled, and the gain is chosen knowing
   the drag now reaches it.** The quanta above were measured with notches ≥60ms
   apart, and the new gain changes which drags qualify: at 17px per notch a
   finger needs only 283px/s to space them 60ms apart, and `MIN_FLICK_PX_PER_MS`
   is 300px/s. So in `claude` a *slow* drag tracks 1:1 and a brisk one enters
   the measured ~1.9 lines per notch and runs at about twice the finger. Two
   zones, by arithmetic, not by accident.

   Shipped as `1` anyway, deliberately. The slow end is where 1:1 earns its
   keep, because that is the drag placing something precisely, and the brisk end
   is covering distance, where overshoot costs less attention. The alternatives
   are both worse: `2` buys 1:1 at the brisk end by making every careful drag
   move at half the finger, and a minimum spacing on the finger's notches
   contradicts decision 6 and would make the content lag a fast drag. This is
   the one constant here that only a thumb can settle, it is a one-line change
   in the table, and the phone measurement below is where it gets settled.

   What stays unmodelled either way is the ramp: 30 notches back to back moved
   127 lines, so a hard throw carries further than the geometry predicts. It is
   a property of a version of someone else's program, reported nowhere, and
   obtained here by injecting bytes into a probe server. Left alone until a
   phone says it is a problem.

## Non-goals

- A user-visible scroll-speed control (approach B). Nothing here asks the user
  for a number.
- Self-calibration (approach C).
- Changing what tmux does, the `-N 5` binding, or the `mouse` option.
- Routing the gesture by pane geometry, and therefore getting a split window
  right per half.
- Chasing the agents' accelerators, or any behaviour that depends on an agent's
  version.
- The horizontal axis, pinch-zoom, and `touch-action`, all unchanged.

## To verify while implementing

- That absent facts degrade to the tmux quantum, and note the shape of this
  failure, which is not the obvious one: an unrecognised format variable
  expands to **empty**, not to nothing -- measured on 3.7b,
  `display-message -p '[#{alternate_on}][#{mouse_any_flag}][#{no_such_xyz}]'`
  prints `[1][1][]` -- so a daemon talking to an older tmux produces a
  full-length row with empty facts and the Go side needs no defence. The
  degrade to test is in the browser, against an older *daemon* that sends no
  such keys at all; the precedent is `windowId` being `""` in `useSnapshot.ts`.
- That the gain tracks the font size: `pxPerNotch` multiplies `rowHeight`, which
  the terminal settings feature now makes variable, so 20px text must move 20px
  rows. Worth one assertion, since the two features met here for the first time.
- End to end on the phone, in an agent pane, after the change: a slow drag
  should track the finger, and a throw should carry without flying. This is the
  only place decision 7 can be falsified, and the only thing that settles
  `claude`'s quantum at 1 or 2 -- try both, in that order, and keep the one
  that feels like the shell pane does today.

## Tests

- **Unit, the predicate:** a table over (mode, alternate, tracking) → 5 or the
  application's quantum, including a pane in copy-mode on the alternate screen,
  which must read 5.
- **Unit, the table:** each named command, an unknown application → 3, and an
  empty command → 3.
- **Unit, the decided mode:** a pane with `alternate_on = 1` that the app
  believes is in copy mode reads 5, which is decision 4's bad case.
- **Unit, the store:** no active pane reads as the tmux quantum; a reported pane
  changes what the getter returns; identity is stable so the gesture does not
  see a new object per poll.
- **Unit, the cap:** a glide owing several notches in one frame delivers one and
  drops the rest; a drag owing several delivers all of them.
- **Go:** the format carries both fields and `fieldCount` matches; `ParseRows`
  reads `0`/`1` into the struct; a row missing them is rejected the way a short
  row already is.
- **e2e, the app regime:** a pane running a 12-line program that turns on
  `1049`/`1003`/`1006` and prints one line per report it receives -- a receiver
  with a known quantum and no agent involved. A swipe of a measured distance
  must produce `distance / (rowHeight * 3)` reports, ±1, which is the assertion
  the current suite cannot make because every existing touch test runs in the
  tmux regime.
- **e2e, unchanged:** the five tests in `e2e/touchscroll.spec.ts` all run over a
  shell pane and must keep passing untouched, which is what pins decision 1's
  "5 lines unless".
