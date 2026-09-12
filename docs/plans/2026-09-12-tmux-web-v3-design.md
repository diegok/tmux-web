# tmux-web — the capture panel, the reply box, and the phone

Date: 2026-09-12
Status: design, not agreed. Revision 1. Six features in the owner's priority
order, three of which press against a decision already recorded: item 1 against
v2's "a general parsed-screen panel" being out of scope, item 6 against v1
decision 5, and item 6's service worker against v2 decision 6. Each of those is
argued against the stated reason rather than around it, and one of them —
item 5 — is argued *for* the recorded posture and comes out mostly rejected.
Follows: `2026-09-09-tmux-web-design.md` (v1, shipped),
`2026-09-10-tmux-web-v2-design.md` (v2, shipped),
`2026-09-10-tmux-web-agent-reporting-design.md` (revision 7, shipped).
Depends on: `bd7d8f3` (the snapshot poll no longer parks while hidden — item 2
is designed around what that commit left in place), `14c11dd` (`Row.Path` —
item 3 exists only because that landed), `6308919` (`internal/report` — the
precedent item 3 and item 5 are placed against).

**On the name.** `2026-09-10-tmux-web-agent-reporting-design.md` also calls
itself "v3". That document is revision 7 of the *agent-reporting* line and it
shipped; this one takes the `v3-design` filename the owner asked for and is the
next batch. Where the two need distinguishing below, the older one is called
"the reporting design" and never "v3".

## Purpose

Six features. Four of them are small and independent, one is medium and mostly
gets rejected here, and one is large and reverses a decision.

What ties the first four together is not a subsystem. It is that each one closes
a gap between what the README's first paragraph promises — *pick up the coding
agents running there from a laptop or a phone somewhere else* — and what the app
does when the phone is the device you actually have. You cannot read scrollback
without dragging the owner's local terminal through it. You cannot copy anything
at all. You cannot reply without a soft keyboard the app has never thought
about. Come back after an hour and the terminal is a corpse until a timer that
was suspended finally fires.

## Decisions

1. **A capture panel shows plain text and is never an authority.** Escapes off,
   no parsing, nothing it reads feeds a state, a badge or a row.
2. **It is a snapshot and says so.** No auto-refresh; a manual recapture and a
   visible capture time.
3. **The socket wakes on foreground, and the wake carries a liveness probe** —
   because the failure that never heals is a socket that claims to be open.
4. **The branch is read off `.git/HEAD` from the daemon, off the poll
   goroutine.** A hung `stat` must cost a branch, never a sidebar.
5. **Dirty state is out of scope**, and the reason is `index.lock`, not cost.
6. **The reply box writes bytes to the PTY**, over the socket that already
   exists. Not `send-keys`.
7. **The reply box is always present** and never appears or disappears on agent
   state.
8. **Session history ships as `--resume` and nothing else.** The list of past
   sessions is deferred, with its costs written down.
9. **v1 decision 5 is replaced**, narrowly: still one layout, but it must work
   with a soft keyboard open.
10. **Keyboard geometry is CSS custom properties, never React state** — because
    in this app a re-render can resize the owner's local terminal.

## Sequencing

| Item | Size | Depends on | Could ship alone |
| --- | --- | --- | --- |
| 2 — wake the socket | ~40 lines + tests | nothing | yes, first |
| 1 — capture panel | small | nothing | yes |
| 3 — the branch | small | `14c11dd` (landed) | yes |
| 4a — the reply box, desktop | small | nothing; *pleasant* only after 1 | yes |
| 5a — resume | tiny | nothing | yes |
| 4b — choice chips, keyboard placement | small | **6** | no |
| 6 — the phone | large | a device measurement task first | yes, but not blind |
| 5b — the session list | medium | its own design document | deferred |

Nothing in 1–4 depends on anything in 1–4 to *compile*. Two dependencies are
real and are about whether the feature is any good rather than whether it
builds:

- **1 earns 4.** A pane in copy mode silently swallows everything sent to it
  (measured; see item 4). Today the only way to read scrollback in the browser
  is to scroll the pane into copy mode, so the reply box would routinely be
  aimed at a pane that cannot hear it. The capture panel removes the reason to
  be in copy mode at all, which is worth more to item 4 than any code in it.
- **6 gates 4b.** A reply box is a soft keyboard by construction. Shipping the
  phone half of item 4 while v1 decision 5 stands is shipping the thing the
  decision says nobody has thought about.

**Item 6 must start with a measurement task, not an implementation task.** Nine
of its facts are borrowed from another project's source and none of them can be
verified in this environment: there is no iOS device, no Android device, and
Playwright's Chromium `isMobile` emulation has no soft keyboard — which is
exactly why the one phone-sized e2e test we have (`e2e/terminal.spec.ts:271`)
asserts focus-trap behaviour and nothing about the keyboard. Nine borrowed
constants that test green on a desktop Chromium is the precise shape of this
project's stated failure mode.

---

## 1. The capture panel

A full-screen panel showing a pane's scrollback as plain text, in a read-only
word-wrapped box, with a pane selector, a recapture button and copy-all.

### What v2 rejected, and why this is not it

v2's `## Out of scope` lists "a general parsed-screen panel". The word doing the
work in that sentence is **parsed**. Everything v2 and the reporting design are
about is the discipline of screen *interpretation*: strict blocked matching,
"only a positive match sets `blocked`", three evidence rules, a prohibition on
a row's appearance branching on which authority decided its state. A panel that
parses the screen would be a fourth authority, arriving with no rules, in the
one place where a wrong answer trains the owner to ignore the badge.

This panel parses nothing. `capture-pane -p` in, text out, into a `<pre>`.
It sets no state, feeds no row, decides no badge, and its output never reaches
`internal/tmux`'s classifier or `internal/report`'s table. **v2's rejection
stands for what it rejected**, and this is a different object that happens to
read the same bytes.

### What it actually solves, which is not copy mode

v1 records three window-level properties grouped sessions cannot isolate — size,
copy mode, active pane — and books them as accepted limitations. This does not
fix copy mode. tmux still has no per-client copy mode and this design does not
pretend otherwise.

What it does is **remove the reason to enter copy mode** in the read-and-copy
case, which is most of supervising an agent. Of v1's three shared properties,
copy mode is the only one the user enters *deliberately*, by scrolling; size and
active pane are consequences of acting at all. So it is the only one of the
three that a feature can retire by making the deliberate act unnecessary. Today
scrolling back in the browser drags the local terminal's view with it, and the
local terminal's live tail freezes while the browser reads. After this, reading
back is a panel and the pane is untouched.

It is also the only way to copy on a phone. wterm renders to the DOM, but the
terminal's selection is mouse-driven and a long press on it produces nothing; a
`<pre>` in a normal document is selectable and long-pressable by the platform,
and the copy-all button covers the case where it is not.

### The command, and the flag the brief is ambiguous about

```
tmux capture-pane -p -J -S -<N> -t %<id>
```

Measured on tmux 3.7b against an isolated socket:

- **`-p` alone is the visible screen only** — 24 lines on a 24-row pane, whatever
  the history holds. This is what `Client.Capture` passes today, deliberately,
  and the reporting design's reason for it is that scrollback is where a
  just-answered approval box lives and it produces false `blocked`. That reason
  is about the classifier, not about a panel.
- **`-S -<N>` takes N lines of scrollback plus the visible screen**, clamped to
  the available history: on a pane with 19 lines of history, `-S -100`,
  `-S -1000` and `-S -` all returned the same 43 lines.
- **`-N` is a different flag** and means "preserve trailing spaces". The brief
  writes `capture-pane -p -S -N`, which reads either as `-S` with a numeric start
  line — the intended reading — or as `-S` followed by the `-N` flag. Take the
  first. **The `-N` flag is rejected**: it pads every line to the pane width,
  which measured 206 852 bytes against 180 140 for the same capture without it
  (+15%), and padding is precisely what defeats a word-wrapped box.
- **`-J` is kept.** It rejoins a line the pane wrapped, so a URL split across two
  rows copies as one string and the browser re-wraps it at the phone's width.
  Its cost is that it also joins a line that merely happened to end at the pane
  width, which is a formatting loss in a panel whose job is copying. Accepted.
- **`-e` stays off**, which is the whole point of "plain text". With it the
  capture carries SGR sequences that a `<pre>` renders as garbage and a clipboard
  carries into whatever you paste into. Measured cost of leaving it off on this
  fixture: 54 bytes out of 180 140 — the escapes are not the reason.

### Size, and what the cap is on

Measured on a 200×50 pane with a 5 000-line history of ~88-character lines:

| Capture | Bytes | Fork cost (50 iterations × 3 rounds) |
| --- | --- | --- |
| `-p` (visible) | 3 904 | 2.6 ms |
| `-p -S -2000` | 167 904 | 5.1–5.7 ms |
| `-p -S -` (all 5 000) | 327 227 | 7.6–8.0 ms |

Two things follow. **Depth is not what costs — the fork is**, up to a point:
tripling the payload roughly triples the call, and the floor is the same 2.6 ms
a poll pays. And **the cap must be on bytes, not lines**, because a 200-column
pane makes a line worth twice what an 80-column pane's line is worth. A
full-width 5 000-line history at 200 columns is about 1 MB.

So: the request carries `lines` (default 1 000, maximum 5 000, validated as an
integer before it reaches tmux), and the daemon caps the response at
`MaxCaptureBytes = 256 KiB`, **truncating from the top** — the newest lines are
the ones you opened the panel for — on a rune boundary, with `truncated: true`
in the response and a line in the panel saying so. `truncateAtRuneBoundary`
already exists in `internal/tmux` for `MaxTitle` and is the same instrument.

`Client.Capture` gains a start line and this cap. Note that today it has **no
output bound at all** — every other value that reaches the DOM has one
(`MaxTitle`, `MaxLabel`, `MaxActivity`, `MaxQuestion`, `MaxReportBytes`) and the
capture is the exception, justified by the classifier needing the whole screen
to hash. That justification does not extend to a scrollback capture, and giving
the panel its own bounded call is better than widening the classifier's.

### It is a snapshot, and the recapture affordance follows from that

The panel shows what the pane looked like at one instant. Everything else in
this app is either live (the terminal) or refreshed on a cadence (the sidebar),
so a third temporal behaviour has to be visible or it will be misread.

- The panel header shows the capture time and ages it — `captured 14s ago`.
- A **Recapture** button, and that is the only refresh.
- Opening the panel captures. Changing pane in the selector captures.
- **No auto-refresh, and this is a decision rather than an omission.** Three
  reasons, in the order they matter: a re-render destroys a selection in
  progress, and this panel exists to be selected from; an interval is a second
  poll at a cadence nobody chose, against a fork that costs 2–8 ms rather than
  the snapshot's shared one; and a panel that keeps up with the pane is a
  terminal, and there is one of those on the other side of the screen already.
  If the owner wants to watch, the answer is the terminal.

### Where it lives

- **Wire.** `GET /api/panes/{id}/capture?lines=N`, behind `Auth.Protect` like
  `/api/snapshot`. `{id}` arrives percent-encoded, as the management routes'
  already do, because a pane id contains `%`. It is a GET because it is a read;
  v1's exact-Origin rule is scoped to state-changing requests and a cross-origin
  page cannot read a GET's body without CORS, which is not enabled. Response:
  `{ "paneId": "%3", "text": "…", "lines": 1000, "truncated": false,
  "capturedAt": 1789075200000 }`.
- **Go.** The bound and the start line go on `Client.Capture` in
  `internal/tmux/client.go`; the handler in `internal/front/server.go` beside
  `s.snapshot`. No judgement lives in the frontend: the truncation, the cap and
  the rune boundary are Go's, because they are the same judgement `MaxTitle`
  already encodes and it should exist once.
- **Frontend.** A shadcn `Dialog` at full height, `<pre>` with
  `white-space: pre-wrap; overflow-wrap: anywhere; user-select: text`, monospace,
  the terminal's own palette tokens. **Not a `<textarea>`** — a textarea is a
  focusable text field and focusing it opens the soft keyboard, which is the
  opposite of what a read-and-copy surface wants on the device it exists for.
- **Copy-all** uses `navigator.clipboard.writeText`, which needs a secure
  context — the device cookie already requires one, so every real deployment
  has it and `--dev` on loopback is a secure context too. **Unverified here:**
  iOS Safari rejects a `writeText` that is not synchronously inside a user
  gesture, so the button must copy from state that is already in memory and must
  never fetch first. The design is written that way; whether it is sufficient
  needs a device.
- **The pane selector** reuses the palette's row source (`Palette.tsx` already
  fuzzy-matches `session/window/pane` over the snapshot), so there is one notion
  of "the list of panes" and not two.

---

## 2. Reconnect when the tab comes back to the foreground

### Two corrections to the premise, both in our favour and one against

The brief's reasoning is mtmux's and most of it transfers. Two parts do not, and
the second is the whole feature.

**Our backoff ceiling is 15 seconds, not an hour.** `BACKOFF_MAX_MS = 15_000`
in `web/src/components/Terminal.tsx:94`; the un-jittered ladder is
500 → 1 000 → 2 000 → 4 000 → 8 000 → 15 000 with ±25% jitter, and the attempt
counter resets to 0 on every successful open. So the "backoff is already at its
ceiling on resume" failure costs at most 15 seconds of dead terminal. That is
worth fixing and it is not the interesting half.

**We have no zombie detector at all.** There is no interval-based liveness check
anywhere in `web/src`; the only `setInterval` in the frontend is a countdown
clock in the devices dialog. The nearest thing is `#checkGone()`, which fires
**on close** — beside the backoff, not in front of it — and asks
`/api/snapshot` whether the session still exists, to tell "killed" from "blip".
It is not a timer and it cannot fire without a close event.

So the brief's argument — *the zombie detector cannot help because it is an
interval and intervals are what got suspended* — is right about mtmux and
understates our case. **Ours cannot help because it does not exist.** And that
matters, because it changes what the wake handler has to do.

### The two failures, and only one of them heals on its own

**(a) The socket is closed and a backoff timer is pending.** On resume the timer
fires, possibly late, and the app reconnects. Cost: up to 15 s of dead terminal,
plus however long the platform takes to run a suspended timer. Bounded, and it
self-heals.

**(b) The socket claims `OPEN` and is dead.** `readyState === WebSocket.OPEN`,
no `close` event ever fired, so `#closed` never ran, so no backoff timer exists,
so nothing in the app will ever try again. The server pings every 20 s
(`wsPingInterval`, `internal/front/ws.go:27`) with a 10 s timeout, and the
browser's network stack answers pongs below JavaScript — which is exactly why a
backgrounded tab keeps a socket alive, and exactly why the *server* noticing is
no help to the *client*: the server tears down its half and the client never
learns, because the path that would carry the close is the path that is gone.
This one does not heal. It is the case that produces "I opened my phone and the
terminal was frozen and stayed frozen".

`online` does not fire for either: it tracks interface transitions, not
wake-ups, and on a captive portal it lies. `visibilitychange` is the trigger for
both. `pageshow` with `persisted: true` is the trigger for a bfcache restore,
which on iOS may not fire `visibilitychange` at all — **documented behaviour,
not verified here**, and the handler is written to tolerate firing twice rather
than to depend on which one arrives.

### The design

One handler, registered by `TerminalSession`, on `visibilitychange` and
`pageshow`. It is idempotent and cheap enough to run on a spurious wake.

```
wake():
  if stopped or phase is 'ended' or 'gone': return
  if the socket is not open:            // case (a)
      cancel the pending backoff timer
      attempt = 0
      connect now                        // this is retryNow(), which exists
      return
  if now - lastRecvAt <= LIVENESS_SLACK_MS: return   // it has been talking
  send a `where` control frame            // case (b): probe
  arm a PROBE_TIMEOUT_MS timer
  on any inbound frame:   cancel the timer
  on timeout:             close(4001, 'socket did not answer'); reconnect
```

Three things about it are load-bearing.

**The probe reuses machinery that is already on the wire.** `where` is the one
control message the daemon *answers*: the client asks and the daemon replies
with a `wsPaneMessage` naming the pane the tab landed on
(`internal/front/ws.go`, and `0ded35a`). So the liveness probe costs one round
trip, no new frame type, no new endpoint, and no server change. Its answer is
also *useful* — after an hour away, which pane the tab is on is a thing worth
re-reading anyway.

**`lastRecvAt` is a new field on `Transport` and it must count every inbound
frame, not only data.** It cannot count pongs, because those are below the
JavaScript API — which is the reason the threshold cannot be tight. An idle pane
sends nothing for minutes and that is normal. `LIVENESS_SLACK_MS` must therefore
be well above the server's ping interval and below anything a person would call
frozen; **45 000** is the proposed value, and it is a guess, not a measurement
(open question 2).

**It does not reconnect on every wake, and the reason is a tmux cost.** Every
reconnect creates a fresh throwaway session grouped onto the real one — one
fork for `new-session` chained with its four `set`s, one PTY, one `tmux attach`
process, a full redraw of the pane, and a teardown of the previous one. A
handler that reconnects unconditionally turns every glance at the phone into
that. The probe exists to make the common case free.

### How this interacts with `bd7d8f3`

They are different mechanisms and both wake, and the commit is what makes saying
so necessary.

`bd7d8f3` removed the snapshot poller's outright parking while
`document.hidden`, replaced it with `HIDDEN_POLL_INTERVAL_MS = 60_000`, moved
the visibility test from poll-time to schedule-time, and rewrote `wake()` into
three cases — stopped, in-flight (remember and honour when it settles),
hidden-armed (cancel the timer and poll now). The poller registers its **own**
`visibilitychange` listener, at `web/src/lib/useSnapshot.ts:1435`.

The socket has none. So after this item there are two listeners, and that is the
decision rather than an accident:

- **Two listeners, not one hoisted into `App.tsx`.** `SnapshotPoller` owning its
  own listener is the existing convention, and the two objects share no state:
  the poller never learns about the socket, and the socket borrows only the
  snapshot *endpoint* through its own one-shot fetch with its own AbortController
  and its own 5 s timeout. Hoisting both into `App.tsx` would give a component
  lifecycle responsibility for two classes that manage their own, to make an
  ordering guarantee neither needs.
- **They can both hit `/api/snapshot` on the same wake** — the poller's
  wake-poll and, if the socket also closed, the terminal's `#checkGone()` probe.
  Harmless: `s.snapshot` reads the poller's in-memory cache and forks nothing.
  Worth one comment so nobody later "fixes" it by making them share.
- **The poll waking is not evidence the socket is alive, and vice versa.** The
  poll is HTTP against a cached value; the socket is a PTY. A wake in which the
  sidebar comes back current and the terminal stays dead is exactly what happens
  today, and it is the most confusing possible symptom — because the sidebar
  looks fine.

One more thing the reporting design ties to this: the daemon's capture cadence
is gated on `Registry.Live`, a live `/ws` socket. So a zombie socket that the
server has already torn down means the daemon has **stopped capturing**, and the
sidebar's agent states go empty — while the poll keeps returning happily. Waking
the socket is therefore also what restores the states, which is the second
reason the two mechanisms are worth describing together even though they share
no code.

---

## 3. Git context: the branch, on the row

`Row.Path` carries each pane's working directory since `14c11dd`, sanitized
through the same three layers as the label and rendered by nothing. This is what
it was for.

### Read `.git/HEAD` — and it is not always one file

Measured here, with a throwaway repository:

| Case | What is on disk | What the row shows |
| --- | --- | --- |
| Normal checkout | `.git/HEAD` → `ref: refs/heads/feat/x` | `feat/x` |
| Detached HEAD | `.git/HEAD` → a 40-hex line | `@f2aa39a` |
| Unborn (fresh `git init`) | `.git/HEAD` → `ref: refs/heads/master` | `master` |
| Worktree | `.git` is a **file**: `gitdir: /abs/…/.git/worktrees/wt`; HEAD is there | `wtbranch` |
| Submodule | `.git` is a **file**: `gitdir: ../../.git/modules/vendor/sub` | that module's branch |
| Bare repo | no `.git`; `HEAD` at the root | nothing |
| Not a repo | nothing, up to the root | nothing |

Four things this table settles.

**"One file, no fork" is right about the fork and wrong about the file.** A
worktree and a submodule both put a `gitdir:` pointer in a `.git` *file*, so
those cases are two reads, and the pointer may be absolute (worktree) or
relative (submodule) — it must be resolved against the directory holding the
`.git` file, not against the pane's cwd. Still no fork.

**A detached HEAD is prefixed.** `@f2aa39a`, seven characters after an `@`, so
it cannot be read as a branch someone named `f2aa39a`.

**An unborn HEAD is indistinguishable from a normal one** without reading refs,
and it shows `master`, which is what `git branch --show-current` says too. Not a
bug; recorded so nobody discovers it and "fixes" it by adding a refs read.

**Bare repositories are not supported.** A pane whose cwd is a bare repo has no
worktree, and the branch of a bare repo is not a fact about anything the person
watching is doing. Detecting one costs testing for `HEAD` + `objects/` +
`refs/` at every level of the walk, on every non-repo path, forever.

The walk itself: from the pane's path upward, testing for `.git`, stopping at
the filesystem root, with `maxGitWalk = 40` levels so a pathological path
terminates. Measured on the owner's live server: **maximum path depth 8**, so
the uncached worst case is 8 `stat`s.

### Where it goes on a row that is already full

A pane row today carries: the mark with the state dot hung on its corner, the
name, an optional label chip, the tmux-active marker, a right-aligned command
capsule, and a second line with the activity under a hover marquee. There is no
free space.

**The branch takes the command capsule's slot.** Right-aligned on the first
line, the same `Badge` geometry and `max-w-24`, an outline variant rather than
secondary so the two are not confusable.

The slot is usually free exactly when the branch is most wanted: the capsule is
rendered only when `paneText` fell all the way to `fromCommand`, which is the
case where the row had nothing better to say. An agent pane with an activity has
no capsule. **When both want the slot, the command wins** — a row that cannot
say what is running is worse than one that cannot say the branch — and the
collision is therefore the shell rows, where the branch is least missed because
the prompt in the pane beside it usually shows it.

That tie-break is the weakest part of this item and it is an open question, not
a finding: the shell-in-a-repo row is the one where a person might well prefer
the branch to the word `zsh`. It is answerable by using it for a week and not by
argument, so it ships the conservative way round.

**Rejected: the second line, prefixed** — `feat/x · run go test`. It costs the
activity ten characters on a row that already ellipsises it, and the activity is
the thing the last seven revisions of the reporting design were about.
**Rejected: a third line.** Rows are two lines and the sidebar is a tree; a third
line is a third fewer panes on a phone.

The branch has exactly one authority, so nothing here brushes the rule that a
row's appearance must not branch on which authority decided its state.

### Cadence, caching, and the goroutine it must not run on

Measured on the owner's live server: **12 panes, 3 distinct paths.** That ratio
is the whole design. The cache is keyed by **directory**, not by pane:

```
gitEntry{ gitDir, branch, headMtime, headSize, checkedAt }
```

Per distinct path per pass: one `stat` of the resolved HEAD; re-read only when
mtime or size differs. A miss — no repository above this directory — is cached
too, with `gitMissTTL = 30s`, and **the negative cache is the part that
matters**, because most panes are not in repositories and re-walking eight
levels every 1.5 s for a shell sitting in `$HOME` is pure waste.

**It does not run on the poll goroutine.** Every other read the daemon makes
goes through `exec.CommandContext` and therefore has a deadline; `os.Stat` and
`os.ReadFile` have none, and a `stat` on a wedged NFS or sshfs mount blocks
uninterruptibly. The poll is the sidebar. So the git reader is its own goroutine
with its own map and mutex, handed the distinct path set after each poll; the
poller reads whatever the map holds when it assembles rows. A hung filesystem
then costs a missing or stale branch and nothing else.

That also makes the branch's staleness the same shape as `Path`'s, which
`14c11dd` already documented as "up to a poll interval stale, never current" —
one consistent claim rather than two.

### What v1 and v2 say about reading the filesystem, checked

- **The paths rule does not forbid this, and citing it as though it does is
  specifically prohibited.** v1 keeps `pane_current_path` off the wire because
  tmux does not sanitize it and a raw newline forges a sidebar row while a raw
  `0x1f` makes a live pane vanish — both verified in v1. The reporting design's
  revision 6 states it plainly: *"It is a **parsing** rule about hostile bytes,
  not a secrecy rule."* `14c11dd` solved the parsing with a tagged block, which
  is why `Path` is on the wire now.
- **The daemon already reads the filesystem from a pane-supplied path.**
  `checkDir` (`internal/tmux/manage.go:377`) `stat`s the path before it becomes
  `split-window -c`. The precedent exists; this widens it from one stat on a
  click to a few stats on a cadence, off the hot goroutine.
- **`ParsePaths` takes the path verbatim and does not repair it**, deliberately,
  because a repaired path no longer names the directory. Layer 1 has already
  turned any newline or `0x1f` in the real path into a space, so such a path
  simply will not resolve and the walk finds nothing. It fails closed, showing no
  branch, which is the correct failure.
- The only actor who can shape these paths is whoever can create directories as
  the owner's uid, which is the owner. Not a boundary, and not treated as one.

### Dirty state is out of scope, and the reason is not cost

The obvious reason is that `git status` is a fork and a walk of the worktree,
unbounded in a large repository, per distinct directory, every poll. True, and
not the reason.

**The reason is `index.lock`.** `git status` refreshes the index as a side
effect and takes a lock to do it. So a sidebar drawing itself would be *writing
into the owner's repository*, on a cadence, in the same directory where a coding
agent is running `git commit` — and the visible failure is not in tmux-web, it
is the agent's commit dying on "Unable to create '.git/index.lock': File
exists". A sidebar that can make someone else's commit fail is not a sidebar.

`git status --no-optional-locks` avoids that specific lock and is the right flag
if this is ever built. It still forks and still walks. So the door is left open
in one shape only: **an on-demand dirty summary, in a panel, on a button, never
on the poll.** Ahead/behind is the same class and worse — it needs `rev-list` or
two refs and a merge base.

---

## 4. A text box for replying, aimed at the phone

### The brief's transport suggestion solves a problem we do not have

The brief says to note that `send-keys -H <hex>` in chunks sidesteps every
quoting and encoding problem. It does — measured below — and **we do not need
it, because there is no `SendKeys` in this daemon and there never was.**

Keystrokes reach tmux through a real PTY. The browser sends a `FrameData`
(`0x00`) frame, `internal/front/ws.go` hands the payload to
`ptybridge.Session.Write`, which is `s.pty.Write(b)`. Nothing the user types is
ever an argv element, so there is no quoting problem, no encoding problem, no
`--` needed, no length limit worth naming (`wsReadLimit` is 1 MiB, sized for a
paste), and no chunking.

**The decision: the reply box writes bytes to the PTY over the socket that
already exists.** No new endpoint, no new tmux verb for the payload, and the
bytes land on the pane the tab's client currently has focused — which is the
pane the sidebar selected and the terminal is showing.

`send-keys -H` was measured anyway, because it is the right answer to a
different question and the numbers should be recorded before somebody re-derives
them:

- **It round-trips arbitrary bytes exactly.** `68 c3 a9 3b 20 2d 6e 20 24 28 69
  64 29 22 27` arrived at a `cat` as `h`, U+00E9, and `; -n $(id)"'` — multibyte
  UTF-8, a semicolon, a quote pair and a `$(…)` all intact.
- **`0d` is what you send for Enter.** It arrived as `\n` at the program, because
  the pty translates CR. A raw `0a` is `C-j`, which is not the same key.
- **It has a hard length limit and it is on the command line, not the argument
  count.** `send-keys -H` with 5 444 hex arguments (16 331 bytes of command line)
  succeeded; 5 452 arguments (16 355 bytes) failed with `command too long`. So a
  `-H` path is capped at roughly 5.4 KiB of payload per invocation and would need
  chunking well below that — 1 KiB per call is the safe number.

The one thing `-H` buys is replying to a pane the tab is **not** attached to,
from a sidebar row, without switching. Rejected for now on v1's active-pane
reason: you have to select the pane to know what you are answering, and
selecting it is the act that moves the active pane for every client anyway. It
is recorded as the upgrade path if "reply from the row" is ever wanted, and if
it is built it needs the chunking above and a `ValidatePaneID` on the target,
which `internal/tmux/target.go` already provides.

### Copy mode swallows the reply, and this is the finding that ties 1 to 4

Measured on 3.7b: with `pane_in_mode = 1`, sending `echo CMTEST` and CR to the
pane produced **zero** occurrences of `CMTEST` anywhere in the pane's history,
and left `pane_in_mode = 0`. The keys were consumed by copy mode's key table.
Cancelling copy mode first and sending the same bytes produced the expected
output.

This is tmux's key dispatch, not the transport, so it is equally true of a PTY
write and of `send-keys`. A person who scrolled back to read what the agent
asked — the only way to read scrollback in the browser today — is in copy mode
by definition, and their reply would vanish with no error anywhere.

**So the reply path cancels copy mode first.** `copy-mode -q -t %<id>` does it
with no attached client (measured: `pane_in_mode` 1 → 0), and it is a no-op on a
pane not in a mode, so it is unconditional and idempotent. `send-keys -X cancel`
also works but wants a current client and printed `no current client` in one of
the probes, so `-q` is the one to use.

That is one extra fork per reply. A reply is a keystroke-initiated action at
human cadence; one fork is free next to the 1.5 s budget this project counts
forks against.

And it is why item 1 earns item 4: with a capture panel, the pane is far less
often in copy mode to begin with.

### The wire for that, and a paper cut it closes

A new control message type, sibling to the existing `copy-mode` one:

```go
{ "type": "end-mode", "pane": "%3" }
```

Handled beside `copy-mode` in `internal/front/ws.go`, reusing
`tmux.ValidatePaneID` for the same reason that one does — `copy-mode -t work`
would freeze the user's own pane. The client sends `end-mode` and then the
`FrameData`; the socket orders them.

It also closes an existing gap: there is an "enter copy mode" action in the
header and the palette, and no way out of it from the browser.

### Enter, paste, and the two buttons

**Enter sends the text and a CR (`0x0d`), then clears the box.** CR and not LF,
for the reason measured above. **Shift+Enter inserts a newline** and sends
nothing.

**A second, smaller button sends the text with no CR.** Answering a `y/n` prompt
or a single-key menu is one character and no return, and a box that always
appends CR cannot express it.

**A multi-line paste is wrapped in bracketed paste** — `ESC[200~` … `ESC[201~`
— with the interior newlines intact and **no trailing CR**. The reason is the
single most damaging thing this box can do: all three agents' input boxes treat
a bare newline as *submit*, so pasting a twelve-line stack trace unwrapped
submits twelve turns to the agent, in order, with no way to stop it.

**This is unverified and must be measured before it is implemented.** Two links
in it are assumptions: that all three agents honour bracketed paste, and that
the pane's program has requested it — `DECSET 2004` is set by the program, not
by us, and sending the wrappers to a program that has not asked for them makes
it see the literal characters `[200~`. If measurement says any of it fails, the
fallback is to **refuse the paste and say why**, which is a worse feature and a
far better outcome than twelve turns.

### Always present, and what `blocked` does and does not change

**The box is always there**, one line high, at the foot of the terminal, and it
does not take focus on mount. Three reasons, in the order they matter:

1. **A box that appears and disappears on agent state is a fourth thing
   branching on that state**, and the app has a rule about that. The rule's exact
   wording is about a row's appearance and its reason generalises cleanly: a
   control that vanishes under you while you are typing into it teaches you not
   to trust it. Worse than the rule's case, in fact, because a badge you learn to
   ignore costs attention and a box that vanishes costs the sentence.
2. **`blocked` is 1.5–6 s late** — 1.5 s on the screen path, about 6 s on the
   report path, per the README. A box gated on it arrives after you wanted it.
3. **The box is also how you answer what is not detected.** The README lists four
   Claude notifications that produce no badge on either authority. Those are
   exactly the cases where you are looking at the pane, understand what it wants,
   and need to type.

What `blocked` *does* change is one thing, and it is additive: when the visible
pane is blocked and the snapshot carries a `question` with `choices`, the box
grows a row of chips, one per choice, each sending that choice's number and CR.
That is the phone case the item exists for — answering a numbered permission
prompt without a keyboard at all — and it is a shortcut, not a mode: the box
underneath is unchanged and still there when the chips are not.

### Focus, and two traps

The box is an ordinary form control outside the terminal's DOM, so focusing it
takes focus off wterm by ordinary rules and Escape blurs it back.

- **The palette must still work from inside the box.** `Ctrl+Alt+K` is
  registered in the capture phase ahead of wterm's handler
  (`Palette.tsx:85`); the box must not swallow it.
- **`Ctrl+C` in the box is copy, not interrupt.** There is no fixing that — it is
  the platform's — so the interrupt stays a thing you send by focusing the
  terminal, and the box's placeholder should not imply otherwise.

### The interaction with item 6 that is easy to miss

The terminal's size is a tmux **window** property shared with every client
viewing that window, and *a resize counts as acting*: v1 measured that a bare
`SIGWINCH` makes a client the most recent one and drags the shared window to its
dimensions, in both directions.

So if opening the soft keyboard shrinks the terminal element, the
`ResizeObserver` fires, the debounced resize goes out, and **the owner's local
terminal is yanked to the size of a phone with a keyboard open.** That is v1's
accepted limitation being triggered by tapping a text box, which is a great deal
more annoying than the co-viewing case it was accepted for.

**So while the keyboard is open, the terminal does not resize.** The box is laid
over the terminal rather than above it, and the resize path is suppressed for
the duration. This is the sharpest single reason item 6's geometry must not be
React state, and it is developed there.

---

## 5. Session history, and resuming one

### What is actually on disk, measured on this machine

| Agent | Store | Scale here |
| --- | --- | --- |
| Claude | `~/.claude/projects/<slug>/*.jsonl` | **10 directories, 13 155 files, 3.11 GB**, largest single file **212 MB** |
| pi | `~/.pi/agent/sessions/<slug>/<ISO-ts>_<uuid>.jsonl` | 13 directories, 19 files, largest 290 KB |
| opencode | `~/.local/share/opencode/opencode.db` — **one SQLite database**, `journal_mode = wal` | 43 MB; 79 sessions, 2 095 messages, 8 972 parts |

Shapes, read as key names only:

- **Claude.** The first record of the sampled file was `type: "user"`, with keys
  `agentId, cwd, entrypoint, gitBranch, isSidechain, message, parentUuid,
  promptId, sessionId, timestamp, type, userType, uuid, version`. So the first
  user message is often line 1 — and `isSidechain` exists, so subagent
  transcripts live in the same tree and must be excluded or every list is
  polluted with them. The first five lines of that one file were **36 499
  bytes**, which sets the scale of "read the head".
- **pi.** First record `type: "session"`, keys `cwd, id, timestamp, type,
  version` — the header the brief describes, with no title in it. Later record
  types seen: `model_change`, `thinking_level_change`, `custom`, `message`. So
  the title needs a scan for the first `message`, not a header read.
- **opencode.** No per-session files at all. `session(id, project_id, parent_id,
  slug, directory, title, version, share_url, …, time_created, time_updated,
  time_archived, workspace_id, path)` — **it already has a `title` column and a
  `directory` column**, which is one SQL query and no parser.

### The appeal is real and so are three costs, one of which is structural

The appeal is exactly as the brief states: it needs no integration and it works
for sessions that predate installing anything. That is a genuinely different
kind of value from everything the reporting design built.

**Cost 1, and it is a blocker for opencode: this binary is `CGO_ENABLED=0` by
deliberate decision.** The Makefile says why — a cgo build links the build
machine's libc for DNS and user lookups and then fails on a box with an older
glibc, "which defeats the reason this is written in Go at all". So
`mattn/go-sqlite3` is out. What is left is `modernc.org/sqlite`, a transpiled
SQLite that is a large new dependency in a `go.mod` whose direct requires today
number **two**; or shelling out to the `sqlite3` CLI, which breaks the README's
"The only runtime dependency is `tmux`" and would be the first time this project
has required a program it does not ship. Neither is small, and both are being
paid for one agent's session list.

**Cost 2: three bespoke parsers against three undocumented private formats.**
opencode's `title` column is a gift; Claude's and pi's titles have to be
reconstructed from the transcript. The directory-slug schemes are already
different — Claude's `-home-diegok-devel-project-supers`, pi's
`--home-diegok-devel-project-supers--` — so even mapping a pane's `Row.Path` to
its session directory is per-agent guesswork. This project has been bitten by
exactly this class already: pi 0.85.1 reformatting a `settings.json` it was
merely asked to add a key to.

**Cost 3: it reads the owner's conversations off disk, and the argument that
retired the last privacy objection does not transfer.** The reporting design's
revision 6 withdrew the exposure argument as unsound, and its reasoning was
specific: *"A pane option is not a boundary. Anything that can talk to the tmux
server can already `capture-pane` the whole screen."* That is true of a value
derived from the current turn and visible on the current screen. A transcript on
disk is a different fact: it is not on any screen, it outlives the pane, it
outlives the tmux server, and it is every turn of every conversation in that
project going back as far as the store does. The withdrawal licensed putting
*what is already on the screen* on the wire. It does not license opening 3.1 GB
of history.

### The decision: ship resume, defer the list

**In scope: resume, driven by the agent.** A "resume here" action that opens a
tmux window in the pane's directory running the agent's own resume command —
`claude --resume`, and the equivalent for the other two. The agent's own picker
is a TUI that already lists its sessions with titles and dates, and it is the
authority on its own format by construction. Cost: one `new-window -c <path>`,
a verb `internal/tmux/manage.go` already has, with the path resolved daemon-side
from `Row.Path` exactly as `SplitPane` already does. **Zero parsers, zero disk
reads, zero drift, and the whole "works for sessions that predate installing
anything" property is preserved** — it is the agent's own history, so of course
it is.

**Out of scope: the list.** The list is what costs three parsers, a SQLite
dependency, a scan over 13 155 files, and a posture the project has never taken.
It is not rejected on principle; it is rejected on the ratio, and the numbers
above are recorded so it can be revisited against them rather than re-argued.

If it is later wanted, the controls are these and they should be written now
rather than invented under pressure:

- Read at most `readHeadBytes = 64 KiB` from the front of a `.jsonl`. Calibrated
  against the measurement above: five records of one real Claude file were 36 KB,
  so 64 KiB is roughly five records — **and a single record can exceed it**, so
  the reader must tolerate finding no user message in the head and fall back to
  the timestamp in the filename rather than reading further.
- Cache keyed by `(path, mtime, size)`. Never re-read an unchanged file.
- Never read a file whose mtime is outside a window the owner chose.
- Skip `isSidechain: true`. Subagent transcripts are not sessions.
- Send **only** the first line, truncated to `MaxTitle`, and a timestamp. Never a
  message body, never a tool call, never a path from inside the transcript.
- Never write any of it to the state file, a log, or an error message — the rule
  `internal/tmux` already applies to captured text.
- Read the SQLite database read-only and expect WAL: `mode=ro` against a
  database another process has open, with the `-wal` and `-shm` files present.
- **Write it as its own design document.** It is not a section of this one.

---

## 6. Mobile, properly

### Reversing v1 decision 5, against its stated reason

v1 decision 5: *"Laptop first, phone as a bonus. No design effort spent on soft
keyboards."*

The stated reason is in the frontend section: shadcn's `Sidebar` "collapses to
icons on desktop and becomes a `Sheet` drawer on mobile by itself. That
satisfies the phone-as-a-bonus decision with no second layout to maintain."

**That reason is still correct and it is not what is broken.** One layout is the
right call and this design keeps it. What the sentence covers is navigation, and
navigation on a phone works. What it does not cover is everything shadcn does
not do: the viewport under a keyboard, the safe area, touch targets,
installability, and the gesture layer. Those were never traded away in v1 —
they were not considered, and the decision's second sentence has been read ever
since as though it disposed of them.

Three things have changed since:

- **The README's first paragraph now sells the phone**, and the agent-reporting
  work spent seven revisions on the case where nobody is watching — which is the
  phone case, arriving at a locked device an hour later.
- **The gap is measurable and one-sided.** Today: `h-svh` in exactly one place
  (`App.tsx:504`); no `dvh`, no `env(safe-area-inset-*)`, no `touch-action`, no
  `overscroll-behavior`, no `viewport-fit=cover`, no `interactiveWidget`, no
  manifest, no service worker, no orientation handling, no pointer-coarse query.
  One e2e test at 390×844, which asserts that three stacked Radix focus traps do
  not leave `body { pointer-events: none }` — a real bug worth a test, and
  nothing about the keyboard.
- **Item 4 forces it.** A reply box is a soft keyboard by construction. You
  cannot ship the phone half of item 4 and keep "no design effort spent on soft
  keyboards".

**So the replacement is narrow and should be stated narrowly. v1 decision 5
becomes: one layout, and it must work with a soft keyboard open.** Not "phone
first". Not a second layout. Not a native shell.

### The leads, each with the bug it prevents and how to check it

These come from the mtmux study (Apache-2.0 source read; nothing copied, and
nothing here is a file from it). **None of them can be verified in this
environment** — no iOS device, no Android device, and Playwright's Chromium
`isMobile` emulation has no soft keyboard at all. They are leads with sources.

1. **`100dvh` is right on Chromium and wrong on iOS Safari, where the footer
   ends up under the keyboard; drive height from `visualViewport`.** Our version
   of this is different and worth stating precisely: we already use `h-svh`, the
   *small* viewport, which is the conservative one — it does not change when the
   URL bar collapses, which is why `App.tsx` uses it and why the terminal does
   not grow-and-never-shrink. What `svh` also does not do is shrink when the
   keyboard opens. So `svh` is right for the no-keyboard case and wrong for the
   keyboard case, and `visualViewport.height` is the only value that is right for
   both. *Check:* open the reply box on a real iPhone and a real Android and see
   whether the box is above the keyboard.
2. **`interactiveWidget: "resizes-content"` makes Android usable and, as a side
   effect, makes the naive keyboard-height calculation read ~0 — so detection
   needs a per-orientation baseline.** The bug it prevents: a keyboard that opens
   and the app never notices, on the platform where the setting was added to
   help. *Check:* on Android, log `innerHeight − visualViewport.height` with and
   without the meta value.
3. **A pinch-zoom reads as a ~400px keyboard unless samples with
   `|scale − 1| > 0.05` are dropped.** The bug: the layout collapses to
   keyboard-open geometry because somebody zoomed to read a stack trace — which
   is a thing people do constantly in a terminal on a phone. *Check:* pinch and
   watch the custom property.
4. **Hysteresis: 120px to open, 80px to close.** 120 clears Android's URL-bar
   collapse and sits under the shortest real keyboard. The bug: the layout
   oscillating on a scroll. *Check:* scroll a long sidebar on Android and watch
   for flapping.
5. **Orientation from `screen.orientation.type`, not from comparing width and
   height.** The bug: with a keyboard open in portrait the viewport can be wider
   than it is tall, so a width-vs-height test reports landscape and picks the
   wrong baseline — which then makes lead 2 wrong as well. *Check:* rotate with
   the keyboard open.
6. **Open immediately, close debounced ~150ms**, because iOS reports
   intermediate heights on the way down. The bug: a flash of an intermediate
   layout every time the keyboard dismisses.
7. **Write the result as CSS custom properties, not React state.** mtmux's reason
   is that the terminal re-renders underneath. **In this app the reason is
   sharper and it is not about rendering.** A React state change re-renders
   `App`, which changes the terminal element's box, which fires `Terminal`'s
   `ResizeObserver`, which sends a debounced resize — and a resize is what makes
   this tab the client that acted most recently, which takes the shared tmux
   window's size for every client watching it, *including the owner's local
   terminal* (v1, measured). So keyboard geometry in React state does not merely
   re-render; it can resize a terminal in another room. A custom property on the
   root element changes layout without a React render and without touching the
   terminal's box.
8. **A tap must never be claimed by the gesture layer, and a claimed gesture's
   `touchend` must always be prevented.** The bug: a tap that opens a pane also
   fires a synthetic click somewhere else 300ms later.
9. **`touch-action: none` intersects with ancestors and cannot be given back to
   a descendant.** The consequence here: it must never be set on `body` or
   `SidebarInset`, or the terminal's own scrolling and the reply box's caret
   placement die with it, in a way that looks like a wterm bug.

To that list, two more from this codebase rather than from mtmux:

10. **`overscroll-behavior: contain`** on the sidebar sheet and on any scrolling
    region. Pull-to-refresh on a page whose terminal is a live socket costs a
    reload, a reconnect, a new throwaway tmux session and a full redraw — the
    most expensive accidental gesture available.
11. **The reply box must be laid over the terminal, not above it**, so the
    keyboard opening never changes the terminal's box at all. See item 4.

### PWA: a manifest yes, and a service worker only in one shape

This presses on **v2 decision 6** — *"Notification is a tab badge, not Web
Push"*, elaborated as *"No push, no service worker, no subscription."*

**v2 rejected a service worker as a notification mechanism.** The sentence lives
inside the paragraph about the tab badge and every reason around it is about
push: subscriptions, endpoints, permission prompts, server-side state. A worker
that precaches one self-contained offline page and does nothing else takes on
none of that. **The rejection stands for what it rejected.**

That said, a service worker is not free and the costs here are specific:

- **A worker is a cache that outlives a deploy, and this app ships its frontend
  inside the binary.** A stale worker serving a stale shell against a new daemon
  is the classic failure and it is *invisible*, because the page loads. So: never
  cache `index.html`, never cache `/api/*`, never cache `/ws`, `skipWaiting` +
  `clients.claim`, and precache exactly one route.
- **`/assets/` is behind `Auth.Protect`** in this app, and the route "must 404,
  never fall back to HTML". A worker caching assets would be caching
  authenticated responses into a store that survives a revoke — and v1 says
  revocation "must sever live connections", not merely fail the next request. The
  shell is not the data, so this is small, but it is a hole in a property v1
  states absolutely. One more reason the worker precaches **one self-contained
  page** — inline CSS, inline SVG, no `/assets/*` — and uses it only as the
  navigation fallback when the network fails.
- **The manifest costs the tab badge.** `tabBadge.ts` writes `(2) tmux-web` into
  the title and draws a dot on the favicon. In `display: standalone` there is no
  tab and no favicon, so **installing the app silently disables the notification
  feature.** The answer is not Web Push — v2's rejection of that stands on its
  own reasons — and it is not to skip the manifest. It is to say so: the
  installed case is the one where you have the app in front of you, and the
  sidebar carries the same states the badge summarised. It should be a line in
  the README, not a surprise.

Manifest: `display: standalone`, `start_url: "/"`, `theme_color` from the
existing token, icons derived from the existing `web/public/favicon.svg`.
`viewport-fit=cover` on the viewport meta, plus `interactiveWidget`, both of
which are one-line changes to a twelve-line `index.html`.

---

## What crosses the wire

Consolidated, because three of the six touch it.

**`Row` gains one field**, taking it from 19 json tags to 20:

```go
Branch string `json:"branch"` // "" when the path is not in a work tree;
                              // "@<7-hex>" when HEAD is detached.
                              // Read off .git/HEAD off the poll goroutine,
                              // so up to a poll interval stale, like Path.
```

**The contract test's literal must move.** `web/src/lib/useSnapshot.test.ts:107`
extracts `type Row struct` from `internal/tmux/snapshot.go`, collects its json
tags and asserts `toHaveLength(19)` against a hand-written literal — deliberately
not `Object.keys(row()).length`, "because a count derived from the TypeScript
side would agree with itself forever". That test is *designed* to fail here.
It becomes 20 and stays a literal.

**One new HTTP route:**

```
GET /api/panes/{id}/capture?lines=N     Auth.Protect
  -> { paneId, text, lines, truncated, capturedAt }
```

**One new WebSocket control type**, pinned by `transport.test.ts`, which already
pins `wsControlMessage`'s field names against `internal/front/ws.go`:

```go
{ "type": "end-mode", "pane": "%3" }    // copy-mode -q -t %3
```

**No new frame kind.** The reply is `FrameData`, which is what typing already
is.

## Where each thing lives

The conventions this codebase holds, and how each item sits inside them.

**Domain judgement in Go, so it exists once.** `internal/report` was created for
exactly this: a judgement "spread across a TypeScript extension, a JavaScript
plugin and a settings.json the user owns is a judgement that drifts. One table,
one test." Applied here:

- The capture cap, the truncation direction and the rune boundary are Go's, not
  the panel's — they are the same judgement `MaxTitle` and `sanitizeLabel`
  already encode.
- `.git/HEAD` interpretation — `ref:` versus hex, the `gitdir:` indirection, the
  detached prefix, the bare-repo refusal — is one Go function with a table test.
  The browser receives a string it renders.
- Bracketed-paste wrapping is the one candidate that could plausibly live in
  either place, and it goes **in the browser**, because it is a property of what
  the *user's input widget* did (a paste event) and not of the pane. The daemon
  cannot tell a paste from typing, and inventing a flag so it could would be
  putting a UI fact on the wire.

**The wire pinned by a Go↔TypeScript contract test.** All three wire changes
above have a pinning test already in place to extend: the row tag count, the
route string, and the control-message field names.

**`paneText`'s ladder decides what a row says**, and the branch does not enter
it. The ladder is question → label → activity → title → command and it decides
the row's *text*; the branch is a chip in the capsule slot, which is a different
element with a different tie-break. Adding a sixth rung would put a directory
fact into a ladder that is entirely about what the *process* is doing.

**A row's appearance must not branch on which authority decided its state.** The
branch has one authority, and the reply box is not on a row. Neither item comes
near the rule. The item that does come near it is 4's "always present": a box
that appeared on `blocked` would be the same failure reached from a fourth door,
which is why it does not.

**The daemon never installs anything and no route ever writes executable code.**
Item 5's resume runs an agent that is already on the machine, through
`new-window`, which is an existing verb behind the existing middleware. It
writes nothing and installs nothing, so it does not touch that boundary. Worth
stating because "run the user's agent from a web request" sounds like it might.

## The costs, priced

The project counts forks: two recent commits took a poll from 4.9–5.2 ms to
2.5–2.8 ms (`b5518c7`, by folding the generation into the batch) and a sidebar
click from 10.1–11.3 ms to 3.3–4.2 ms (`146fcfc`, by using the window id the
snapshot already carries). Against that budget:

| Item | Forks per poll | Other per-poll cost | Per-action cost | Wire |
| --- | --- | --- | --- | --- |
| 1 capture panel | **0** | none | 1 fork per capture: 2.6 ms visible, 5.1–5.7 ms at 2 000 lines, 7.6–8.0 ms full history (measured, 3.7b, 200×50, 5 000-line history) | up to 256 KiB, once, on demand |
| 2 wake | **0** | none | on a real wake with a live socket: one `where` round trip. On a dead one: a reconnect — 1 fork, 1 PTY, 1 redraw, plus the old session's teardown | one control frame |
| 3 branch | **0** | N `stat`s, N = distinct pane directories (**measured: 3 for 12 panes**), on a separate goroutine | 1 `ReadFile` per changed HEAD | +1 field, ~20 bytes per row |
| 4 reply | **0** | none | 1 fork per reply (`copy-mode -q`) + one PTY write | one control frame + the bytes |
| 5a resume | **0** | none | 1 fork (`new-window -c`) | nothing |
| 6 phone | **0** | none | none | nothing |

Two costs are not forks and are the ones worth watching.

**Re-renders.** Item 6's whole geometry story is that a re-render of `App` can
resize the owner's local terminal, so the keyboard's height is a CSS custom
property. Item 4's box is a sibling of the terminal, not a wrapper, for the same
reason. Item 1's panel is a portal to `<body>`, like every other dialog, so it
does not touch `SidebarInset`'s box at all.

**Syscalls on the poll path.** Item 3 is the first thing in this daemon that
reads the filesystem on a cadence. Three `stat`s is nothing; three `stat`s on a
wedged mount is the sidebar. Hence the separate goroutine, which is the entire
reason that design is not just "read the file in `refresh`".

## Rejected

**Auto-refreshing the capture panel.** A re-render destroys a selection in
progress and the panel exists to be selected from; an interval is a second poll
at a cadence nobody chose against a 2–8 ms fork; and a panel that keeps up with
the pane is a terminal, which is already on screen.

**`send-keys -H` as the reply transport.** Solves a quoting problem this daemon
does not have, since input goes to a PTY and is never an argv element. Kept as
the recorded upgrade for "reply from a sidebar row without switching pane", with
its measured 16 KiB command-line limit so nobody re-derives it.

**Reconnecting on every wake.** Every reconnect is a new throwaway tmux session,
a PTY, an attach and a full redraw. A `where` round trip is the cheap way to ask.

**An `online` handler.** It tracks interface transitions, not wake-ups; it lies
on captive portals; and every case it would catch, the wake handler already
catches. One trigger, not three.

**Dirty state on the row.** Not on cost: on `index.lock`. A sidebar that refreshes
a repository's index on a cadence can make a coding agent's `git commit` fail,
and the error surfaces in the agent, not here.

**Ahead/behind on the row.** Same class as dirty, and it needs `rev-list` or two
refs and a merge base.

**Bare-repository support.** No worktree, so no branch that describes what the
watcher is doing, and detecting one costs three extra tests at every level of
every walk on every non-repo path forever.

**The branch on the second line.** Costs the activity ten characters on a row
that already ellipsises it. **A third line on the row.** A third fewer panes on a
phone.

**A `git rev-parse --abbrev-ref HEAD` fork per directory.** The file read exists
precisely to avoid it, and a fork per distinct directory per poll is the cost
class this project spends commits removing.

**The session list, for now**, with its numbers recorded above so it can be
revisited rather than re-argued. **A SQLite dependency**, whether
`modernc.org/sqlite` in `go.mod` or the `sqlite3` CLI as a runtime requirement —
both paid for one agent's list.

**Web Push, again.** v2 rejected it and none of its reasons have changed. The
service worker here is a navigation fallback and carries no subscription.

**A second layout for the phone.** v1's actual reason for one layout is still
right; only its second sentence is being replaced.

**A `tmux -C` control-mode sidecar.** Named in v1 and again in v2 as the eventual
upgrade when forks stop being free. Nothing in this batch adds a per-poll fork,
so nothing here brings it closer.

## Failure modes

| Case | Handling |
| --- | --- |
| A capture is opened on a pane that dies between the click and the fork | tmux exits 1, the handler returns the error, the panel shows it and offers the selector. The pane id was validated, so nothing else was targeted |
| A pane with a 200-column, 5 000-line full-width history | ~1 MB uncapped; capped at `MaxCaptureBytes` and truncated from the top, with `truncated: true` and a line saying so. The newest lines are the ones kept |
| Someone reads a badge or a state off the capture panel | Cannot happen from the code — the panel's text reaches no classifier — but it can happen in someone's head. The panel shows a capture time and ages it, which is the only honest defence |
| `-J` joins two lines that merely both ended at the pane width | A formatting loss in a panel whose job is copying. Accepted; `-J` is what makes a wrapped URL copy as one string |
| The wake handler fires twice (`visibilitychange` and `pageshow` both) | Idempotent by construction: the second call finds either a connecting socket or a fresh `lastRecvAt` and returns |
| The wake probe times out on a socket that was actually fine, just slow | Costs one unnecessary reconnect: a fork, a PTY, a redraw, and the pane the tab remembers is re-selected. Annoying, not destructive. `PROBE_TIMEOUT_MS` is a guess and is open question 2 |
| A wake while the daemon is restarting | The reconnect fails, the backoff restarts from attempt 0, and the phase pill says reconnecting — which is the existing behaviour and correct |
| The sidebar comes back current on wake and the terminal stays dead | This is the symptom the item exists to remove, and it is worth naming because it is the most confusing one: the poll is HTTP against a cache and the socket is a PTY, so one recovering says nothing about the other |
| A zombie socket means the daemon stopped capturing | `Registry.Live` gates the capture cadence, so a torn-down socket also empties every agent state. Waking the socket is what restores them; a poll alone cannot |
| `.git/HEAD` is read mid-write by `git checkout` | A short read or a torn line. The parser accepts only `ref: refs/...` or 40 hex characters and shows nothing otherwise; the next pass, one poll later, reads the settled file |
| A pane's path is on a wedged NFS mount | The git goroutine blocks; the poll does not. The branch is missing for that directory until the mount answers. This is the reason for the separate goroutine and the only reason |
| A path containing a raw newline or `0x1f` | Layer 1 has already substituted spaces, so the walk resolves nothing and the row shows no branch. Fails closed |
| A directory 40 levels deep, or a symlink loop | `maxGitWalk = 40` terminates it. Measured maximum depth on the live server is 8 |
| A repository with an unborn HEAD | Shows the branch name that HEAD points at, which is what `git branch --show-current` says too. Not distinguishable without reading refs, and not worth reading refs for |
| A shell pane in a repository | Shows the command capsule, not the branch, because the command wins the slot. The weakest tie-break in this document; open question 4 |
| A reply sent to a pane in copy mode | Cancelled first with `copy-mode -q`. **Without that the reply vanishes with no error anywhere** — measured: zero occurrences of the sent text and copy mode exited |
| A multi-line paste into the reply box | Wrapped in bracketed paste, no trailing CR. **If measurement shows the agents or tmux do not honour it, the paste is refused with a reason** — twelve submitted turns is not an acceptable failure |
| `Ctrl+C` typed in the reply box | The browser's copy. Not fixable; the interrupt stays on the terminal and the placeholder must not imply otherwise |
| The reply box focused while the pane changes underneath | The bytes go to the tab's client, which follows the selection, so the reply lands on the new pane. Clear the box on a pane change and say so, rather than silently retargeting a half-typed sentence |
| The soft keyboard opening resizes the terminal | **Would take the shared tmux window's size for every client, including the owner's local terminal** — v1's measured limitation, triggered by tapping a text box. The box is laid over the terminal and the resize path is suppressed while the keyboard is open |
| Keyboard height written into React state | Same failure by a different route: the re-render changes the terminal's box and the `ResizeObserver` sends the resize. Hence custom properties, and it is a decision rather than a style preference |
| A pinch-zoom during a terminal read on a phone | Reads as a ~400px keyboard unless samples where `scale` differs from 1 by more than 0.05 are dropped. Unverified here |
| A stale service worker after a deploy | Invisible, because the page loads. `index.html` is never cached, `skipWaiting` + `clients.claim`, and exactly one precached self-contained route |
| The app installed as a PWA | **The tab badge stops working**: no tab, no favicon. Not fixed by push (v2's rejection stands); stated in the README instead |
| A revoked device whose worker still has the shell cached | The shell is not the data — every request it makes still fails auth — but it is a small hole in v1's "revocation severs" property. The worker precaches one self-contained page and no `/assets/*`, which is what keeps it small |
| Pull-to-refresh on the terminal page | A reload, a reconnect, a new throwaway session, a redraw. `overscroll-behavior: contain` |
| `touch-action: none` set on an ancestor | Cannot be given back to a descendant, so the terminal's scroll and the box's caret die together and it looks like a wterm bug. Never set it above the element that needs it |
| The resume action on an agent that is not installed | `new-window` succeeds, the command fails inside the pane, and the pane shows the shell's error — which is the right place for it and needs no handling here |

## Testing

Following v1, v2 and the reporting design:

- **Integration against real tmux** (`internal/tmux/testutil`, throwaway socket)
  for: `Capture` with a start line returning scrollback plus the screen; the byte
  cap truncating from the top on a rune boundary; `copy-mode -q` on a pane in a
  mode and on one that is not; **and the one that matters — a reply sent to a
  pane in copy mode without the cancel does not arrive, and with it does.** That
  last one is the whole justification for the `end-mode` message and it is
  invisible to any test that only checks the cancel ran.
- **A fork-counting test for the git reader**: a poll with panes in three
  directories forks tmux **once**, exactly as `TestOnePollForksTmuxOnce` asserts
  today. The shim technique in `serverstart_internal_test.go` is the instrument.
- **A table test for `.git/HEAD`** over the seven cases in the table above,
  built by `git init` in `t.TempDir()` — including the worktree and the submodule,
  because those are the two the "one file" claim is wrong about and a fixture
  written by hand would encode the wrong claim.
- **A test that the git reader is not on the poll goroutine**: block the reader
  and assert the poll still completes within its deadline. Without it the
  goroutine is a refactor away from being inlined "for simplicity".
- **The contract test's row tag count moves to 20**, as a literal, and the new
  route and control type get their pins.
- **A Playwright test that the capture panel's text is selectable and its
  copy-all works**, at the default viewport. The clipboard needs a permission
  grant in the Playwright context.
- **A Playwright test at 390×844 that the reply box is reachable** — which is as
  far as the emulator can go, since it has no keyboard. Everything about the
  keyboard is a device measurement, not a test, and the plan must say so instead
  of pretending a green CI covers it.
- **A regression test that the wake handler is idempotent**: fire
  `visibilitychange` and `pageshow` together and assert exactly one connect
  attempt.
- **A regression test that a healthy socket is not reconnected on wake** — the
  positive control for the probe. Without it a handler that always reconnects
  passes every other test in this list.

## Out of scope

The session list and its three parsers; any SQLite dependency; dirty state,
ahead/behind and anything else needing a `git status`; bare repositories; Web
Push and any notification that is not the tab title and favicon; a second layout
for the phone; a native shell; replying to a pane the tab is not attached to;
a control-mode sidecar; making the capture panel live; and reading anything
under `~/.claude`, `~/.pi` or opencode's database. Each is argued above.

## Open questions that need measurement before a plan can be written

1. **Every one of item 6's nine leads.** They are another project's constants,
   verified on their layout and their device matrix, not ours. The first task of
   any implementation plan for item 6 is a measurement session on a real iPhone
   and a real Android with the reply box on screen — not an implementation task.
   Chromium `isMobile` cannot see any of it.
2. **`LIVENESS_SLACK_MS` and `PROBE_TIMEOUT_MS`.** Both guesses. The slack must
   sit above the server's 20 s ping interval and below what a person calls
   frozen; 45 s and 5 s are placeholders. Measurable by backgrounding a real
   phone for an hour and recording how long a real wake takes to produce a frame.
3. **Whether bracketed paste works end to end** — that tmux forwards it, that
   each agent's input box honours it, and what a program that never enabled
   `DECSET 2004` does with the wrappers. Three agents, three answers, and the
   fallback (refuse the paste) depends on the result.
4. **Whether the command capsule should really beat the branch on a shell row.**
   Answerable by using it, not by argument. Recorded as the conservative choice.
5. **What `lines` should default to.** 1 000 is a guess sized against the
   measured 5 KB-per-1 000-lines-at-80-columns figure. The real question is how
   far back a person actually scrolls to answer an agent, and nobody has measured
   it.
6. **How often a pane's directory actually changes.** The cache's whole design
   rests on it being rare (3 distinct paths for 12 panes, once, on one machine).
   If agents `cd` constantly the negative cache and the mtime check are both
   sized wrong.
7. **Whether `copy-mode -q` is safe to send unconditionally on every reply.** It
   is a no-op on a pane not in a mode against 3.7b, measured. Whether that holds
   for a pane in a *different* mode — `choose-tree`, a menu, `clock-mode` — is
   not measured, and cancelling somebody's `choose-tree` because they typed in a
   text box would be a surprise.
8. **Whether iOS Safari's clipboard rule is satisfied by copying from memory
   inside the handler.** The design assumes yes and is written so that it can be;
   it needs a device.
9. **Whether the panel should offer the visible screen as a distinct choice**
   from a scrollback depth. `-p` alone is 2.6 ms and 4 KB against 5.1 ms and
   168 KB, which is a real difference on a phone on a train, and it is not
   obvious that the depth slider is worth the control.
10. **What a service worker does to the enrollment flow.** `/enroll` is public
    and reads `location.hash`; a navigation fallback that serves a cached page
    for it would be a bad failure. It should be excluded explicitly, and whether
    exclusion is enough needs checking against a real install.
