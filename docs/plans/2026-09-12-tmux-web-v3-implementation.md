# tmux-web v3 Implementation Plan — the capture panel, the reply box, and the phone

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Close the gap between what the README's first paragraph promises — *pick up the coding agents running there from a laptop or a phone somewhere else* — and what the app actually does when the phone is the device you have. Six features: a read-only scrollback panel you can copy from, a socket that wakes when the tab does, the branch on a sidebar row, a text box that replies to a pane, a "resume here" action, and a layout that survives a soft keyboard.

**Architecture:** Nothing here adds a per-poll fork. The capture panel is one bounded `capture-pane` behind a new authenticated GET. The wake is a client-side handler that, on a socket claiming `OPEN` while quiet, sends the `select` + `where` pair the reconnect path already sends and reconnects only when nobody answers. The branch is `.git/HEAD` read from a **separate goroutine** with its own cache, handed the distinct path set after each poll. The reply is bytes down the PTY the socket already owns, preceded by one `send-keys -X -t %<id> cancel`. Resume is one `new-window -c`. The phone is CSS custom properties, a viewport meta, a manifest and a navigation-fallback service worker — and a measurement session before any of it.

**Tech Stack:** Go 1.26 (stdlib only), React 19 + shadcn/ui, vitest, Playwright.

**Design document:** `docs/plans/2026-09-12-tmux-web-v3-design.md`, **Revision 3**, pinned at commit `8da5750`. **Read all of it before starting**, including the three revision sections at the top. They are the fastest way to learn which plausible-looking simplifications have already been tried and are wrong — two adversarial review rounds have already deleted the obvious version of four of the mechanisms below, and the deleted versions are the ones a competent engineer reaches for first.

**It replaces one recorded decision and narrows another.** v1 decision 5 (*"Laptop first, phone as a bonus. No design effort spent on soft keyboards"*) becomes **one layout, and it must work with a soft keyboard open** — not "phone first", not a second layout. v2 decision 6 (*"No push, no service worker, no subscription"*) stands for what it rejected: the service worker here carries no subscription and the Badging API is not push.

---

## Before you start

### The failure mode of this project is plausible reasoning that tests green and is false

Not bugs. Every defect this design records was in a *plan*, not in code written from one:

- Revision 2's `end-mode` guard read `#{pane_mode}` with no `-t` of its own, so it evaluated against tmux's default pane — failing in both directions, and in one of them closing the owner's `choose-tree`, which is the exact thing the guard existed to prevent. **And the test revision 2 specified for it would have passed**, because a single-pane fixture makes the default pane the target.
- Revision 2's wake probe was idempotent "because a second wake finds a connecting socket or a fresh `lastRecvAt`". For an `OPEN` socket — the case the whole feature exists for — that is false, and the test revision 2 specified could not have caught it.
- Revision 1 booked an accepted cost for `-J` joining lines that merely ended at the pane width. Measured twice: it does not.
- Revision 1's branch chip took the command capsule's slot, which made a row element appear and disappear on **agent state** — the one thing this codebase has a written rule about.
- Revision 2 wrote that the row's width cost "is paid by the name". `Badge` is `shrink-0`; the name cannot pay. The row overflows and the branch is silently clipped instead.

Assume this plan contains some too. **If something looks wrong, say so rather than implementing it faithfully.**

Where a task rests on an external behaviour — tmux's, a browser's, an agent's, a phone's — the task says **"verify this by running it, do not assume"** and gives the command. Those steps are not optional and their output is not predictable from the docs.

### Mutation testing is the verification standard

Every one of the thirty-plus tasks executed in this repo so far has been mutation-tested by its implementer, and **every single one found a real defect**. So every task below ends with a **mutation step naming specific mutants for that task's own logic**. Generic "mutate something and see" is not enough and does not count as having done the step.

Apply each mutant, run the named test, watch it go red, revert. **A surviving mutant is a missing test, not a curiosity.** Record every survivor in the commit message, with why it survived.

**The four recurring shapes, all of which have bitten in this repository:**

| Shape | What it looks like | How to avoid scoring it as a kill |
| --- | --- | --- |
| **A self-referential assertion** | The test's fixtures *and* its assertions are written against the very constant the mutant retargets, so both sides move together and the mutant survives — or worse, the test can never fail | Build the fixture from a constant the mutant does not touch; assert against the one it does. Where a constant has a stated *relationship* rather than a measured value, assert the relationship on its own line |
| **A fixture true by accident** | The pre-action state already satisfies the assertion, so the action under test is never exercised. The `end-mode` single-pane fixture is exactly this | Assert the pre-state is **not** the post-state before acting. Every mode test below opens with that assertion |
| **A bogus kill** | The mutant fails to compile, or hangs, and the nonzero exit is scored as the test having caught it | A kill counts only when the **named test** fails with a **named assertion**. If the package will not build, the mutant told you nothing — fix the mutant, not the score |
| **A test that cannot fail** (and its mirror, one that always fails) | `expect(cls).toContain('disabled')` on a shadcn button passes unconditionally, because the Tailwind class list contains `disabled:pointer-events-none` | Before running the mutants, break the *implementation* trivially (return a constant, return early) and confirm the test goes red. If it does not, the test is decorative |

### Two of this design's tests are already known to be vacuous as naively written

Both are called out again in their tasks. They are here as well because meeting them twice is cheaper than meeting one of them at review.

1. **The `end-mode` guard test on a single-pane fixture (Task 14).** "Put a pane in copy mode, send `end-mode`, assert the mode is gone" is what the test naturally becomes, and on a single-pane fixture it **passes with the wrong instrument** — tmux's default pane *is* the target there, so an expression that resolves against the wrong pane resolves against the right one by accident. The fixture must be **two panes in one window, with the target pane not the active one**, and every assertion aimed at the inactive one.
2. **The wake idempotency test as "assert exactly one connect attempt" (Task 4).** That covers only case (a), the timer case, where a second wake genuinely finds nothing to do. Against an **OPEN stale socket** it passes with two probes and two armed timers and never notices the orphan. It must assert **exactly one probe frame and exactly one armed timer**, then let the answer land, advance past `PROBE_TIMEOUT_MS`, and assert **no reconnect**.

### Hard safety rules

These govern you as well as the code. Each one is here because it has already gone wrong on this machine.

1. **Never run `pkill`, `killall`, or any pattern-matching process kill.** The owner has live tmux sessions with real work and three running coding agents in them. An agent once killed its own shell this way.
2. **Never run a mutating tmux command without `-L <your own socket>` and `-f /dev/null`.** The owner's `~/.tmux.conf` sets non-default options, so a test server started without `-f /dev/null` is not the server your test thinks it is. In tests, go through `internal/tmux/testutil` — its `Args()` already includes both. Read-only commands against the live server (`list-panes`, `show-options`, `display-message`) are fine.
3. **Do not inspect or decompile the coding agents' binaries or bundled JS.** Everything this plan needs about an agent comes from its documentation or from running it and recording what arrives.
4. **Never copy text out of the owner's live panes.** This repo is **public** and those panes hold private client work. Every fixture is your own content in your own isolated session, or it is masked.
5. **Do not run `prettier`.** None is configured, and it rewrites unrelated files — it has already happened once and had to be unpicked by hand. Match the surrounding style.
6. **`git add` explicit paths only, and run `git status` before staging.** Never `git add -A`, never `git add .`. One agent already swept another agent's uncommitted work into a commit in this repository.
7. **Never revert a mutation experiment with `git checkout -- <path>`.** It has wiped an implementer's uncommitted work here. Revert a mutant by editing the line back, or by keeping the original in your own scratchpad and copying it in.
8. **Each task's implementer works in its own scratchpad subdirectory.** Nothing temporary goes in the repo, in `/tmp` directly, or in another task's directory.

### The verification commands, and the one that lies

```bash
make test              # test-go (go test ./... -count=1 -race) then test-web (pnpm vitest run)
npx playwright test    # the e2e suite, 32 tests
pnpm typecheck         # FROM THE REPO ROOT
```

Run **both** suites after every task. `go test ./...` alone is not enough: the frontend carries a contract test that parses the Go `Row` struct out of `internal/tmux/snapshot.go` and compares its json tags against the TypeScript keys, **including an exact field count**, so a Go-side wire change turns the frontend suite red while Go stays green. Task 12 is that change in this batch.

Per-package during a task: `go test ./internal/tmux/ -run TestName -v`, and `cd web && pnpm test <file>`.

**Typechecking is a third check and neither of the other two performs it.** vitest does not typecheck, so the suite stays green over TypeScript that will not build. Run `pnpm typecheck` from the repo root after any change to a `.ts`/`.tsx` file — it runs `tsc -b` inside `web/`, exits 2 on a type error and 0 when clean.

**Do not run `npx tsc -b` from the repo root.** The root package has no TypeScript dependency, so npx resolves a decoy that prints *"This is not the tsc command you are looking for"* and **exits 0**. It compiles nothing and reports success. `cd web && npx tsc -b` is correct; `pnpm typecheck` from the root is shorter and harder to get wrong.

Baseline before you start: **568 unit tests, 32 e2e, all green**, on a clean `main`.

### Six things here are counter-intuitive. Do not "simplify" them back

1. **The mode cancel is `send-keys -X -t %<id> cancel`, and it is not `copy-mode -q`.** A reader who half-remembers this design will reach for `copy-mode -q`, because it is the obvious command and because revision 2's document contains it. `send-keys -X` dispatches into the **copy-mode command table**, which is why it pops only the copy layer of a stacked mode, refuses on `choose-tree` and `clock-mode` with `not in a mode` and exit 1, needs no format guard, and is **one fork rather than two**. `copy-mode -q` cancels `clock-mode` and `choose-tree` as well, and a pane's mode is shared with the owner's local client — so on the owner's machine that is their session tree closing because somebody typed into a text box on a phone. Revision 2's earlier rejection of `send-keys -X cancel` ("it wants a current client") was **measured without `-t`** and is withdrawn: with an explicit `-t %<id>` it succeeds against a specific pane with no client attached to the server at all.
2. **The non-zero exit of that cancel is the common case and is not an error.** Most replies go to a pane in no mode, where it exits 1 with `not in a mode` on stderr. The daemon runs it, **ignores the exit status**, never surfaces it to the browser, and writes the bytes anyway. A version that propagated the error would fail every ordinary reply. `ValidatePaneID` is what guards the target; the exit status is not doing that job and must not be read as though it were.
3. **The wake probe `select`s before it asks.** `where` answers a different question depending on how it is asked: after a `select` it names the tab's own pane, and with no `select` it names the shared window's active pane — which the owner may have moved while the phone slept. Adopting that clears a finish badge on a pane nobody read. *Discarding* it, which is what revision 2 proposed, closes the badge bug and leaves the two wake paths ending somewhere different: the terminal shows the owner's pane and the reply box writes to it while the sidebar, the capture panel and `useSeenPanes` all name the tab's. The probe therefore does exactly what reconnect does — `select(#pane)`, then `where`, adopting through the existing `#awaitingWhere`.
4. **That makes an existing green test assert the opposite of the design, and moving it is Task 1.** `Terminal.test.tsx:792` asserts a socket *"accepts one answer per socket and no more"*. A socket now asks `where` once per open **plus once per stale wake**. The invariant becomes **one answer per outstanding question**, which is what `#awaitingWhere` has always actually enforced. `wsControlBuffer`'s comment (`internal/front/ws.go:38-43`) and the handler comment (`ws.go:477-479`) both say "one `where` per socket" and move with it.
5. **The branch chip's presence is a fact about the pane's directory and about nothing else.** Not "when the command capsule is absent" — the capsule's presence tracks `paneText`'s ladder, whose top rung fires on `agentState === 'blocked'`, so a chip that yielded to it would appear and disappear as a pane blocks and unblocks. That is the rule this codebase has about rows, broken by the item that looked least likely to break it. The two chips share a **width** budget, which is a fact about the sidebar; they never share a presence rule.
6. **Keyboard geometry is CSS custom properties, never React state.** Not for rendering reasons. A React state change re-renders `App`, which changes the terminal element's box, which fires `Terminal`'s `ResizeObserver`, which sends a debounced resize — and a resize makes this tab the client that acted most recently, which takes the shared tmux window's size **for every client watching it, including the owner's local terminal in another room** (v1, measured). Keyboard height in React state can resize a terminal somebody else is looking at.

---

## The shape of the work, and why it is in this order

Twenty-six tasks in seven phases, one phase per design item, in the owner's priority order corrected by two real dependencies and one gate.

- **Phase A (1–4) wakes the socket** — item 2. It goes first because it depends on nothing, it is the smallest of the six, and it is the only one that fixes a failure that **never heals**: a socket that claims `OPEN` and is dead has no backoff timer pending, because no `close` event ever fired, so nothing in the app will ever try again. Task 1 moves an invariant before anything depends on the new one.
- **Phase B (5–9) is the capture panel** — item 1. It goes second because **it earns item 4**. A pane in copy mode does not merely eat a reply sent down our PTY path: it truncates it at the first cancel key and delivers the remainder to the program as input (measured — `echo quit PARTIAL` cancels on the `q` of `quit` and runs `uit PARTIAL`). Today the only way to read scrollback in the browser is to scroll the pane into copy mode. The panel removes the reason to be in copy mode at all, which is worth more to item 4 than any code in it.
- **Phase C (10–13) is the branch** — item 3. Independent of everything; placed third because it is the last of the small ones and because Task 12 is the wire change that turns the frontend contract test red, and it is cleaner to do that while nothing else is in flight.
- **Phase D (14–18) is the reply box on the desktop** — item 4a. The Go half first (the cancel and its two-pane fixture, which is the sharpest trap in this plan), then the transport, then the copy-mode regression through `ptybridge`, then the box, then the bracketed-paste measurement.
- **Phase E (19) is resume** — item 5a. One task, and its first step is a measurement (pi's invocation) rather than an implementation.
- **Phase F (20–25) is the phone** — item 6. **It opens with a device-measurement task and that is a gate, not a formality.** Nine of its facts come from another project's source, verified on their layout and their device matrix. There is no iOS device here, no Android device, and Playwright's Chromium `isMobile` emulation **has no soft keyboard at all** — which is why the one phone-sized e2e test we have asserts focus-trap behaviour and nothing about the keyboard. Nine borrowed constants that test green on a desktop Chromium is the precise shape of this project's stated failure mode. Task 21 (resize suppression) lands before Task 22 (the viewport meta) and Task 23 (the geometry properties), because both of those can change the terminal's box and the suppression is what keeps that off the owner's terminal.
- **Phase G (26) is the choice chips** — item 4b — and it is last because it is gated on Phase F. A reply box is a soft keyboard by construction; shipping the phone half of item 4 while v1 decision 5 stands is shipping the thing the decision says nobody has thought about.

Every task is independently committable and reviewable. Where a task changes the wire, the TypeScript mirror moves in the same commit.

### Task list

| # | Task | One line |
| --- | --- | --- |
| 1 | The invariant moves | `where` is asked once per **outstanding question**, not once per socket; `Terminal.test.tsx:792` and both `ws.go` comments move together, with no behaviour change |
| 2 | `lastRecvAt`, and the three constants | Every inbound frame stamps it — pongs are invisible to JavaScript and anchor nothing; `LIVENESS_SLACK_MS`, `PROBE_TIMEOUT_MS`, `CONNECT_STALL_MS` land as named guesses |
| 3 | The wake handler, cases (a) and (c) | One handler on `visibilitychange` **and** `pageshow`; no transport → `retryNow()`; `CONNECTING` past `CONNECT_STALL_MS` → `discard()`, which `retryNow()` cannot do |
| 4 | The probe, case (b) | `select` then `where` on an OPEN quiet socket, single-flighted through `#wakeProbe`; the idempotency test asserts one probe frame **and** one timer |
| 5 | `CaptureRange` and the byte cap | A new bounded call beside `Capture`, `-S -<N>`, `MaxCaptureBytes = 256 KiB`, truncated **from the top** on a rune boundary |
| 6 | `GET /api/panes/{id}/capture` | Percent-decoded id, `ValidatePaneID`, `lines` validated as an integer before it reaches tmux, behind `Auth.Protect` |
| 7 | The panel | Full-height `Dialog`, `<pre>` and not a `<textarea>`, `onOpenAutoFocus` prevented, capture time aged, Recapture, copy-all from memory |
| 8 | The panel's selector is the tab's selection | Reuses the palette's row source across every session, goes through `handleSelectPane`, carries the palette's `unreachable` flag, then captures |
| 9 | The panel under Playwright | Selectable text and a working copy-all at the default viewport, with the clipboard permission granted in the context |
| 10 | `.git/HEAD`, parsed | Eight cases including a **64**-hex detached line and the two `gitdir:` indirections; `maxGitWalk = 40` |
| 11 | The git reader goroutine | A one-slot mailbox that drops rather than queues, one unit of work **per directory** with its own in-flight flag, `gitMissTTL = 30s`, and a test that the poll is not on it |
| 12 | `Branch` on the wire | `Row` 19 tags → 20; the contract test's literal at `useSnapshot.test.ts:132` moves and stays a literal |
| 13 | The chip, and the width budget | Its own slot, presence on `Branch != ""` alone; a shared shrinkable 80 px pair overriding `Badge`'s `shrink-0`, branch capped at 64 px |
| 14 | `end-mode` | `send-keys -X -t %<id> cancel`, exit status ignored, **two-pane fixture with the target not active**, four cases |
| 15 | `endMode()` on the transport | The control message and its pin in `transport.test.ts`, beside the existing `copy-mode` one |
| 16 | The truncated-remainder regression | Through `ptybridge` and not `send-keys`, with a payload that **contains a cancel key**: without `end-mode` the pane runs `uit PARTIAL` |
| 17 | The reply box | Always present, no focus on mount, Enter sends text + `0x0d` then clears, Shift+Enter newlines, a second button sends with no CR, an empty reply sends nothing |
| 18 | Bracketed paste, measured | Three agents, three answers; wrap with `ESC[200~`…`ESC[201~` and no trailing CR, or **refuse the paste and say why** |
| 19 | Resume here | pi's invocation measured first, then one `new-window -c <path>`; the control must not promise opencode a picker it does not have |
| 20 | **The device measurement session** | Owner-in-the-loop, a real iPhone and a real Android, the reply box on screen. Nine leads, each with the value it produces. **No production code in this task** |
| 21 | Resize suppression | The terminal does not resize while an app-owned text control has focus — this is what keeps a phone's keyboard off the owner's terminal, and it lands before anything that can change the box |
| 22 | The viewport meta and the manifest | `viewport-fit=cover` + `interactive-widget=resizes-content` in one content string, a manifest, and `navigator.setAppBadge` beside the two calls `tabBadge.ts` already makes |
| 23 | Keyboard geometry as custom properties | `visualViewport`-driven, per-orientation baseline from `screen.orientation.type`, pinch samples dropped, hysteresis, open immediate and close debounced — every number from Task 20 |
| 24 | The gesture and safe-area layer | `overscroll-behavior: contain`, `env(safe-area-inset-*)`, and the `touch-action` rule: never above the element that needs it |
| 25 | The service worker | One precached self-contained page as a navigation fallback; never `index.html`, `/api/*`, `/ws` or `/assets/*`; `/enroll` excluded explicitly |
| 26 | The choice chips | On `blocked` with a `question` carrying `choices`, one chip per choice sending its number and CR — a shortcut, not a mode |

### Which tasks depend on an unresolved open question

The design's ten open questions are open. **Do not invent answers.** Each row says what to measure and what to do until somebody has.

| Task | Open question | Standing instruction |
| --- | --- | --- |
| 2, 3, 4 | **2 — `LIVENESS_SLACK_MS`, `PROBE_TIMEOUT_MS`, `CONNECT_STALL_MS`.** All guesses. The slack is anchored at one end only: the server's 20 s ping is exactly the traffic `lastRecvAt` **cannot see** (pongs are below the JavaScript API), so it never refreshes the field and anchors nothing | Ship `45_000`, `5_000`, `10_000` as three named constants with the measurement written in the comment beside each. Wrong high costs a zombie that survives one wake; wrong low costs three forks per glance at the phone. Measurable by backgrounding a real phone for an hour and recording how long a wake takes to produce a frame — **fold this into Task 20's device session** |
| 6 | **5 — what `lines` should default to.** 1 000 is sized against a measured ~5 KB per 1 000 lines at 80 columns. The real question is how far back a person scrolls to answer an agent, and nobody has measured it | One named constant, `defaultCaptureLines = 1000`, changeable by editing one line. Maximum stays 5 000, validated server-side |
| 7 | **8 — whether the panel should offer the visible screen as a distinct choice.** `-p` alone is 2.6 ms and 4 KB against 5.1 ms and 168 KB, which is a real difference on a phone on a train | Ship the depth default and **no control**. Record the numbers in the panel's comment so the control can be argued for against them rather than added because it seems nice |
| 7, 9 | **7 — whether iOS Safari's clipboard rule is satisfied by copying from memory inside the handler.** The design assumes yes and is written so that it can be: the button copies from state already in memory and never fetches first | Implement it that way and **do not claim it works on iOS**. It is a line in Task 20's device session |
| 11 | **6 — how often a pane's directory actually changes.** The cache's whole design rests on it being rare (3 distinct paths for 12 panes, once, on one machine) | Build the cache as designed and add one counter behind a debug log. If agents `cd` constantly, the negative cache and the mtime check are both sized wrong — but nothing about the *structure* changes, only two numbers |
| 13 | **4 — the name floor.** 50 px is asserted from where "pane 3" stops being readable at `text-sm` and has not been looked at | **This one needs no device.** Task 13 step 1 is: open the app on a desktop with a split window in a repository and look. If 50 px is wrong the group cap moves, not the slot rule |
| 18 | **3 — whether each agent enables `DECSET 2004` and honours it.** The tmux half is settled: tmux mediates the wrappers client-side and strips them for a program that never asked, so there is no literal-`[200~` failure to design around. What remains is three programs and three answers | Task 18 **is** the measurement, and the design already writes down the fallback: refuse the paste and say why. Twelve submitted turns is not an acceptable failure and "it probably works" is not a measurement |
| 19 | **10 — pi's exact resume invocation.** Claude's `--resume` and opencode's `--continue` / `-s <id>` are known; pi's cell is taken from documentation rather than measured | Task 19 step 1 measures it, in a throwaway directory, before anything is written into a button. If it cannot be established, ship Claude and opencode and leave pi's control absent rather than wrong |
| 20, 22, 23, 24 | **1 — every one of item 6's nine leads.** Another project's constants, verified on their layout and their device matrix, not ours | Task 20 is the gate. **No constant from leads 1–9 is written into the app before Task 20 has produced a number for it**, and Task 20 records what it could not establish as loudly as what it could |
| 25 | **9 — what a service worker does to the enrolment flow.** `/enroll` is public and reads `location.hash`; a navigation fallback that served a cached page for it would be a bad failure | Exclude `/enroll` explicitly in the fetch handler, and then **check it against a real install** — an excluded route that is still intercepted is a thing the code cannot tell you about itself |

### One environment fact that shapes four tasks

`web/vitest.config.ts` sets **`environment: 'node'`**, deliberately — "the transport's only DOM dependency is the WebSocket constructor, which its tests replace anyway. A component test added later can opt in per file with `// @vitest-environment jsdom`." So `document` and `window` **do not exist** in `Terminal.test.tsx`.

The consequence, and it applies to Tasks 3, 4, 21 and 23: **the logic is tested by calling the method, and the wiring is tested in its own small jsdom file.** `TerminalSession.wake()` is public and every behavioural test calls it directly under the existing node-environment harness; one new file, `web/src/components/TerminalWake.dom.test.ts`, opens with `// @vitest-environment jsdom` and asserts only that `start()` registered the two listeners and `stop()` removed them with the same references. Do not convert `Terminal.test.tsx` to jsdom to save that file — it would move several hundred passing tests onto a different environment to test two `addEventListener` calls.

---

## Phase A — wake the socket (design item 2)

### Task 1: The invariant moves — one answer per outstanding question

**Files:**
- Modify: `internal/front/ws.go`, `web/src/components/Terminal.test.tsx`

**No behaviour changes in this task.** It exists because Task 4 makes an existing green test assert the opposite of the design, and a plan that leaves that for an implementer to trip over has handed them a test they will "fix" in the wrong direction.

Three places encode "a tab sends one `where` per socket":

- `internal/front/ws.go:38-43` — `wsControlBuffer`'s comment: *"A tab sends one `where` per socket and the answer is one small frame, so this is slack rather than capacity"*.
- `internal/front/ws.go:477-479` — the `where` case: *"Which pane did I land on?", asked once per socket, immediately after the tab has replayed the pane it remembered.*
- `web/src/components/Terminal.test.tsx:792` — `it('accepts one answer per socket and no more', …)`.

After Task 4 a socket asks `where` **once per open plus once per stale wake**. The buffer is 4 and does not overflow, so nothing breaks — but all three state an invariant that is no longer true, and the test states it as an assertion. The real invariant, and the one `#awaitingWhere` has always actually enforced, is **one answer per outstanding question**.

**Step 1: Rewrite the test's name and its comment, and add the assertion the new name needs**

`Terminal.test.tsx:792` becomes:

```ts
  it('accepts one answer per outstanding question and drops every unasked-for one', () => {
    const { term } = makeSession({ storage: memoryStorage() })
    term.start()
    const ws = MockWebSocket.last
    ws.open()

    // One question was asked on open (`#opened` selects nothing here, having
    // nothing remembered, and then asks `where`), so the first answer is
    // adopted.
    serverControl(ws, { type: 'pane', pane: '%5' })
    expect(term.status.pane).toBe('%5')

    // A second, unasked-for answer -- a daemon that decided to announce every
    // move it saw -- must not be able to move the tab behind the user's back.
    // This is what `#awaitingWhere` is for, and it is not a count of sockets:
    // a socket may ask `where` more than once (the wake probe does), and each
    // question gets exactly one answer.
    serverControl(ws, { type: 'pane', pane: '%6' })
    expect(term.status.pane).toBe('%5')
  })
```

**This test passes on today's code, and that is expected.** Its failing evidence is the mutation step, not a red run. The half of the new name that today's code cannot demonstrate — that a *second* question gets a second answer — cannot be written until the probe exists, so **Task 4 adds it** and this task's step 4 says so. Do not skip it there: a renamed test whose file does not back the new name is a claim nobody checked.

**Step 2: Move both Go comments**

`internal/front/ws.go`, the `wsControlBuffer` block:

```go
// wsControlBuffer bounds the queue of control messages waiting to go out to the
// browser. A tab asks "where" once on open and again on any wake that finds
// this socket claiming OPEN while quiet, and each answer is one small frame, so
// this is slack rather than capacity; the send is non-blocking so that a
// browser which has stopped reading cannot park the read goroutine, and the
// ping loop is what eventually collects such a peer.
const wsControlBuffer = 4
```

And the `where` case:

```go
	case "where":
		// "Which pane did I land on?", asked once per outstanding question
		// rather than once per socket: on open, immediately after the tab has
		// replayed the pane it remembered, and again on a wake that found this
		// socket claiming OPEN while quiet. Both ask it the same way -- select
		// first, then where -- which is what makes one answer cover both cases:
		// the remembered pane when it is still alive, and the pane tmux
		// actually left the tab on when the select failed because that pane had
		// died. The browser drops an answer to a question it did not ask.
```

**Step 3: Run both suites, expect PASS**

```bash
make test
pnpm typecheck
```

**Step 4: Mutation testing**

The behaviour is unchanged, so the mutants are aimed at the guard the renamed test now names explicitly. Both live in `Terminal.tsx`'s `#landed`.

| Mutant | Killed by |
| --- | --- |
| Delete `if (!this.#awaitingWhere) return` (`Terminal.tsx:620`) | the new test's second assertion — `%6` is adopted. **If this survives, the test is decorative and the rename made it worse, because the name now promises more than it checks** |
| Delete `this.#awaitingWhere = false` on the line after it | same assertion: the flag stays true and the second answer is adopted |
| Invert it to `if (this.#awaitingWhere) return` | the first assertion — `pane` stays null |
| Change `wsControlBuffer` from 4 to 1 | **nothing, and it should not be** — say so in the commit. The buffer is slack, the send is non-blocking, and one outstanding answer is all there ever is. It is here so that nobody records it as a kill |

**Step 5: Commit**

```bash
git status
git add internal/front/ws.go web/src/components/Terminal.test.tsx
git commit -m "refactor: a where is answered once per outstanding question, not once per socket"
```

---

### Task 2: `lastRecvAt`, `connectStartedAt`, and the three constants

**Files:**
- Modify: `web/src/lib/transport.ts`, `web/src/lib/transport.test.ts`, `web/src/components/Terminal.tsx`

**What `lastRecvAt` cannot see, and why that changes the threshold.** The server pings every 20 s (`wsPingInterval`, `internal/front/ws.go:27`) and the browser's network stack answers pongs **below the JavaScript API**. That is exactly why a backgrounded tab keeps a socket alive, and it means the ping never touches `lastRecvAt`. Revision 1 of the design wrote that the threshold "must sit above the server's 20 s ping interval"; it is anchored to nothing of the sort. Nothing on a healthy but idle socket refreshes this field at all — an idle pane can send nothing for an hour and be perfectly alive.

So the threshold is anchored at **one end only** — below what a person would call frozen — and its real job is to keep the probe off the common case of a tab that was hidden for a moment.

**The initial value is a decision, not a default.** `#lastRecvAt` is stamped at **construction** and re-stamped on **open** and on **every inbound frame**. It answers "when did we last have evidence this peer was alive", and a socket that has just been created is evidence as fresh as evidence gets. Initialising it to `0` instead would make every socket permanently stale until its first frame, so a glance at the phone two seconds after a connect would cost three forks on a pane that has simply not printed anything. That alternative is a named mutant below.

**Step 1: Write the failing tests**

In `web/src/lib/transport.test.ts`:

```ts
describe('lastRecvAt', () => {
  it('is stamped at construction, so a socket that has said nothing is not instantly stale', () => {
    let now = 1_000
    const t = new Transport({ url: 'ws://x/ws', now: () => now })
    expect(t.lastRecvAt).toBe(1_000)
    // A fixture true by accident is the trap here: assert the clock has moved
    // and the field has not, so a `now()` getter would be caught.
    now = 9_000
    expect(t.lastRecvAt).toBe(1_000)
  })

  it('counts a control frame, not only data', () => {
    let now = 1_000
    const t = new Transport({ url: 'ws://x/ws', now: () => now })
    MockWebSocket.last.open()
    now = 2_000
    // A `pane` answer is the only inbound control frame the daemon sends, and
    // it is the whole point: a probe's answer is what proves the socket alive.
    serverControl(MockWebSocket.last, { type: 'pane', pane: '%3' })
    expect(t.lastRecvAt).toBe(2_000)
  })

  it('counts a data frame', () => { /* … now = 3_000, receive FRAME_DATA … */ })

  it('counts a frame the transport goes on to reject as unusable', () => {
    // An empty frame, or a non-binary message, is still a peer that is talking.
    // Liveness is about the socket, not about the payload being useful.
  })

  it('does not count a frame that arrives after close(), which suppresses everything', () => {
    // `#receive` returns early on `#closedByCaller`; the stamp is *inside* that
    // guard, not in front of it.
  })
})

describe('connectStartedAt', () => {
  it('is the construction time and never moves', () => { /* … */ })
})
```

`MockWebSocket` and a `serverControl` helper already exist in this file — check their exact names before writing; `Terminal.test.tsx`'s `serverControl` is a separate copy and this file may spell it differently.

**Step 2: Run, expect FAIL** — `Property 'lastRecvAt' does not exist on type 'Transport'`.

**Step 3: Implement**

`TransportOptions` gains one field:

```ts
  /** Injectable clock, for `lastRecvAt` and `connectStartedAt`. Defaults to Date.now. */
  now?: () => number
```

`Transport` gains:

```ts
  /**
   * When this Transport was constructed, by the injected clock.
   *
   * The wake handler's third case needs it: a socket left in CONNECTING by a
   * suspend has no open event, no close event and a transport object that
   * exists, so the only thing that distinguishes "give it another moment" from
   * "this handshake is never completing" is how long it has been trying.
   */
  readonly connectStartedAt: number

  /**
   * When a frame last arrived from the server.
   *
   * Every inbound frame counts, not only data -- but pongs do not, because they
   * are answered below the JavaScript API and are invisible here. That cuts
   * both ways: the server's 20s ping never refreshes this field, so it anchors
   * no threshold, and nothing on a healthy but idle socket refreshes it at all.
   * An idle pane can be silent for an hour and be perfectly alive. This is
   * evidence of life, never proof of death.
   *
   * Stamped at construction so that a socket which has not spoken yet is not
   * instantly stale: a probe two seconds after a connect is three forks spent
   * on a pane that has simply printed nothing.
   */
  get lastRecvAt(): number
```

The stamp goes in `#receive`, **after** the `#closedByCaller` guard and **before** the `ArrayBuffer` check:

```ts
  #receive(event: MessageEvent): void {
    if (this.#closedByCaller) return
    // Before the validity checks, deliberately. A frame we cannot parse is
    // still a peer that is talking, and this field is about the socket rather
    // than the payload.
    this.#lastRecvAt = this.#now()
    …
```

and in `ws.onopen`, beside the existing `onOpen` call.

Then in `web/src/components/Terminal.tsx`, beside `BACKOFF_MAX_MS`:

```ts
/**
 * How long a socket may have been silent before a wake will probe it.
 *
 * A GUESS -- design open question 2. Anchored at one end only: it must sit
 * below what a person would call frozen. It cannot be anchored at the other
 * end, because the traffic that would anchor it -- the server's 20s ping -- is
 * answered below the JavaScript API and never touches `lastRecvAt`.
 *
 * Wrong high costs a zombie socket that survives one wake and waits for the
 * next. Wrong low costs three tmux forks per glance at a phone. Measurable by
 * backgrounding a real phone for an hour and recording how long a wake takes to
 * produce a frame.
 */
export const LIVENESS_SLACK_MS = 45_000

/**
 * How long the wake probe waits for an answer before deciding the socket is
 * dead and reconnecting.
 *
 * A GUESS -- design open question 2. Wrong low costs one unnecessary reconnect:
 * a fork, a PTY, a redraw, and the remembered pane re-selected. Annoying, not
 * destructive.
 */
export const PROBE_TIMEOUT_MS = 5_000

/**
 * How long a socket may sit in CONNECTING before a wake gives up on it.
 *
 * A GUESS -- design open question 2. This case is not established to be
 * permanent: a stalled handshake eventually fails at the TCP or proxy layer and
 * fires onclose, which puts it back on the backoff ladder. What is established
 * is the timescale -- that failure is minutes of somebody else's timeout, and a
 * wake is about the seconds a person will look at a frozen terminal.
 */
export const CONNECT_STALL_MS = 10_000
```

`TerminalSessionOptions` also gains `now?: () => number`, defaulted to `Date.now`, and passes it into every `new Transport`. One clock, injected in one place.

**Step 4: Run both suites, expect PASS.** Then `pnpm typecheck` from the root.

**Step 5: Mutation testing**

| Mutant | Killed by |
| --- | --- |
| Initialise `#lastRecvAt = 0` instead of `this.#now()` | `is stamped at construction` — **the headline mutant.** It is the version that makes every first wake after a connect cost three forks |
| Make `lastRecvAt` a getter returning `this.#now()` | the same test's second assertion, which is why the clock is advanced there. A test that only checked the value once would score this as a kill |
| Stamp only when `frame[0] === FRAME_DATA` | `counts a control frame, not only data` — the probe's own answer would stop counting, which makes the probe unable to prove anything |
| Move the stamp after the `instanceof ArrayBuffer` check | `counts a frame the transport goes on to reject as unusable` |
| Move the stamp in front of the `#closedByCaller` guard | `does not count a frame that arrives after close()` |
| `LIVENESS_SLACK_MS = 45` (a units slip: seconds for milliseconds) | **nothing in this task**, and that is correct — it is killed in Task 4, by a test whose fixture is built from a literal duration and whose assertion is on the constant. Do not write a Task 2 test against the constant's *value*; a test asserting `LIVENESS_SLACK_MS === 45_000` is the self-referential shape and cannot fail |
| `connectStartedAt` re-stamped on open | Task 3's case (c) tests. Note it here and confirm the kill there |

**Step 6: Commit**

```bash
git status
git add web/src/lib/transport.ts web/src/lib/transport.test.ts web/src/components/Terminal.tsx
git commit -m "feat: a transport remembers when it last heard anything and when it started connecting"
```

---

### Task 3: The wake handler, cases (a) and (c)

**Files:**
- Modify: `web/src/components/Terminal.tsx`, `web/src/components/Terminal.test.tsx`
- Create: `web/src/components/TerminalWake.dom.test.ts`

**Two things in the existing code make the obvious implementation not work**, and both were found by review rather than by writing it:

- **`retryNow()` returns early whenever `#transport` is non-null** (`Terminal.tsx:538-543`: `if (this.#stopped || this.#transport) return`). It covers case (a) — no transport, a backoff timer pending — and **nothing else**. A socket left in `CONNECTING` by a suspend still *has* a transport, so `retryNow()` is a no-op on exactly the state it looks like the tool for.
- **`Transport.close()` suppresses its own `onClose`.** It sets `#closedByCaller` and the close handler returns early on that flag (`transport.ts:266-270`, `:165-166`). So "close it and let the reconnect path fire" fires **nothing at all**. Whatever decides a socket is dead has to drop the transport and start the connect itself.

**Step 1: Write the failing tests**

In `Terminal.test.tsx`, a new `describe('wake')`. Every one of these calls `term.wake()` directly — there is no `document` in this environment.

```ts
describe('wake', () => {
  it('case (a): a pending backoff timer is cancelled and the connect happens now', () => {
    const { term } = makeSession()
    term.start()
    MockWebSocket.last.emitClose(1006)
    expect(term.status.phase).toBe('reconnecting')
    // The fixture must not already satisfy the assertion: one socket so far.
    expect(MockWebSocket.instances).toHaveLength(1)

    term.wake()
    expect(MockWebSocket.instances).toHaveLength(2)

    // And the cancelled timer must not later open a third. This is the half a
    // test written as "wake reconnects" would miss, and `retryNow` only gets it
    // right because it calls `#clearTimers`.
    vi.advanceTimersByTime(BACKOFF_MAX_MS * 2)
    expect(MockWebSocket.instances).toHaveLength(2)
  })

  it('case (c): a socket stuck in CONNECTING past CONNECT_STALL_MS is dropped and replaced', () => {
    let now = 1_000
    const { term } = makeSession({ now: () => now })
    term.start()
    const first = MockWebSocket.last
    // Never opened: readyState stays 0, which is CONNECTING.
    expect(first.readyState).toBe(0)
    expect(first.closedWith).toBeNull()

    now += CONNECT_STALL_MS + 1
    term.wake()

    expect(first.closedWith).toEqual({ code: 4001, reason: 'socket did not answer' })
    expect(MockWebSocket.instances).toHaveLength(2)
    // attempt reset to 0, so the pill says "connecting" rather than climbing
    // the ladder from wherever it was.
    expect(term.status.attempt).toBe(0)
  })

  it('case (c), the negative control: a CONNECTING socket inside the window is left alone', () => {
    let now = 1_000
    const { term } = makeSession({ now: () => now })
    term.start()
    const first = MockWebSocket.last

    now += CONNECT_STALL_MS - 1
    term.wake()

    expect(first.closedWith).toBeNull()
    expect(MockWebSocket.instances).toHaveLength(1)
  })

  it('does nothing once stopped, or from ended, or from gone', () => {
    // Three fixtures, three assertions that no socket was constructed. `gone`
    // in particular: `retryNow()` is deliberately allowed from `gone` because
    // the user asked; a wake is not the user asking.
  })
})
```

`makeSession` gains a `now?: () => number` passthrough.

The wiring, in the new `web/src/components/TerminalWake.dom.test.ts`:

```ts
// @vitest-environment jsdom
//
// This file exists only because vitest runs the rest of this suite under
// `environment: 'node'`, where `document` does not exist. Everything about what
// a wake *does* is tested in Terminal.test.tsx by calling `wake()`. What is
// tested here is the two lines that make a real browser call it, and that
// `stop()` takes them back off -- a listener that outlives its session keeps a
// dead TerminalSession alive for the life of the page.
```

It asserts, with `vi.spyOn(document, 'addEventListener')` and `vi.spyOn(window, 'addEventListener')` and their `remove` counterparts, that:

- `start()` adds `visibilitychange` on `document` and `pageshow` on `window`;
- `stop()` removes both, **with the same function reference** (compare the recorded argument identity — `removeEventListener` with a fresh arrow removes nothing, and that is a leak no other test can see);
- a `visibilitychange` dispatched while `document.visibilityState === 'hidden'` does **not** wake, and one dispatched while it is `'visible'` does.

Stub `globalThis.WebSocket` with a ten-line class in this file; do not import the harness from `Terminal.test.tsx`.

**Step 2: Run, expect FAIL** — `term.wake is not a function`.

**Step 3: Implement**

```ts
  /**
   * The tab came back to the foreground. Decide whether this socket is worth
   * keeping, and reconnect if it is not.
   *
   * Registered on both `visibilitychange` and `pageshow`, and written to
   * tolerate firing twice: a bfcache restore on iOS may fire `pageshow` with
   * `persisted: true` and no `visibilitychange` at all -- documented behaviour,
   * not verified here -- so the handler depends on neither arriving rather than
   * on which one does.
   *
   * `online` is deliberately not a trigger. It tracks interface transitions
   * rather than wake-ups, it lies on a captive portal, and every case it would
   * catch this already catches.
   */
  wake(): void {
    if (this.#stopped || this.#phase === 'ended' || this.#phase === 'gone') return

    // (a) No transport: a backoff timer is pending and may be arbitrarily late,
    // because it is a timer and timers are what a suspend suspends.
    if (!this.#transport) {
      this.retryNow()
      return
    }

    // (c) Stuck mid-handshake. `retryNow()` cannot do this -- it returns early
    // while a transport exists -- and no close event is coming.
    if (this.#transport.state === 'connecting') {
      if (this.#now() - this.#transport.connectStartedAt <= CONNECT_STALL_MS) return
      this.#discard()
      return
    }

    // (b) is Task 4.
  }

  /**
   * Throw this socket away and start a new one, now.
   *
   * `Transport.close()` sets `#closedByCaller` and suppresses its own
   * `onClose`, so closing and waiting for the reconnect path to fire would
   * wait forever. The transport is detached first so that a callback which
   * somehow still arrives fails its `transport !== this.#transport` check.
   */
  #discard(): void {
    if (this.#wakeProbe) {
      clearTimeout(this.#wakeProbe)
      this.#wakeProbe = null
    }
    const dead = this.#transport
    this.#transport = null
    this.#awaitingWhere = false
    dead?.close(4001, 'socket did not answer')
    this.#clearTimers()
    this.#attempt = 0
    this.#connect()
  }
```

`#wakeProbe: ReturnType<typeof setTimeout> | null = null` is declared here even though only Task 4 ever sets it, so that `#discard()` is whole in the commit that introduces it.

Registration goes in `start()` and removal in `stop()`, through two stored bound handlers:

```ts
  readonly #onVisible = () => {
    // The event fires on hide as well as on show, and probing a tab that is
    // going away is three forks nobody will see the result of.
    if (globalThis.document?.visibilityState === 'hidden') return
    this.wake()
  }
  readonly #onPageShow = () => this.wake()
```

guarded so the class still constructs under the node environment (`globalThis.document?.addEventListener`, `globalThis.addEventListener`).

**One comment is required, next to the registration**, or somebody will "fix" it later:

```ts
    // There are now two visibilitychange listeners in this app: this one and
    // SnapshotPoller's, at web/src/lib/useSnapshot.ts:1435. That is the
    // decision and not an accident. The poller owning its own listener is the
    // existing convention, the two objects share no state, and they can both
    // hit /api/snapshot on the same wake -- harmlessly, because `s.snapshot`
    // reads the poller's in-memory cache and forks nothing. Do not hoist them
    // into App.tsx to make an ordering guarantee neither of them needs.
    //
    // And note what the poll waking does *not* tell you: the poll is HTTP
    // against a cache, the socket is a PTY. A wake where the sidebar comes back
    // current and the terminal stays dead is exactly today's bug, and it is the
    // most confusing possible symptom because the sidebar looks fine.
```

**Step 4: Run both suites, expect PASS.** Then `pnpm typecheck`.

**Step 5: Mutation testing**

| Mutant | Killed by |
| --- | --- |
| Replace the whole case (c) branch with `this.retryNow()` | `case (c): a socket stuck in CONNECTING…` — `retryNow` is a no-op while `#transport` is non-null, so no second socket is constructed. **The headline mutant: this is the implementation the design says a reader will write** |
| `<= CONNECT_STALL_MS` to `< CONNECT_STALL_MS` | nothing, unless a fixture sits exactly on the boundary. **Add one**: `now += CONNECT_STALL_MS` exactly, and assert the socket is left alone. Without it this is a boundary mutant asserted off the boundary, where the two operators agree |
| `CONNECT_STALL_MS` retargeted to `0` | `case (c), the negative control` — and it is killed only because that test's fixture is built from `CONNECT_STALL_MS - 1` while the *assertion* is "nothing happened". Check that reasoning before scoring it: if you write the fixture as a literal `9_999` the test stops moving with the constant, which is the safer direction here |
| `#discard()` without the `close()` call | add an assertion on `first.closedWith` — which `case (c)` already has. Without the close, the old socket's `tmux attach` leaks until the server's ping collects it |
| `#discard()` closing but not detaching (`this.#transport = null` removed before `close`) | nothing today, because `close()` suppresses the callback. **Reduced to a review check**, not a mutant: the detach is defence against a callback that arrives from a path `close()` does not suppress. Say so in the commit rather than recording a kill |
| Drop `this.#clearTimers()` from `#discard()` | a new assertion in `case (c)`: after the discard, advance past `BACKOFF_MAX_MS * 2` and assert still exactly two sockets. Without it a pending retry opens a third |
| Drop the `phase === 'gone'` guard | `does nothing once stopped, or from ended, or from gone` |
| Register on `visibilitychange` only | the jsdom file's `pageshow` assertion |
| `removeEventListener` with a fresh arrow instead of the stored reference | the jsdom file's identity assertion. **Nothing else in the suite can see this**, which is the reason that file exists |
| Drop the `visibilityState === 'hidden'` filter | the jsdom file's hidden-dispatch assertion |

**Step 6: Commit**

```bash
git status
git add web/src/components/Terminal.tsx web/src/components/Terminal.test.tsx web/src/components/TerminalWake.dom.test.ts
git commit -m "feat: a foregrounded tab reconnects a socket that is stalled or waiting on a suspended timer"
```

---

### Task 4: The probe — case (b), the failure that never heals

**Files:**
- Modify: `web/src/components/Terminal.tsx`, `web/src/components/Terminal.test.tsx`

**This is the case the feature exists for.** `readyState === WebSocket.OPEN`, no `close` event ever fired, so `#closed` never ran, so no backoff timer exists, so **nothing in the app will ever try again**. The server pings every 20 s and the browser's network stack answers pongs below JavaScript — which is why a backgrounded tab keeps a socket alive, and why the *server* noticing is no help to the *client*: the server tears down its half and the client never learns, because the path that would carry the close is the path that is gone. This is "I opened my phone and the terminal was frozen and stayed frozen".

**The probe `select`s before it asks, and that ordering is the design.** `where` answers a different question depending on how it is asked. After a `select` it names the tab's own pane; with no `select` it is what `CurrentPane` actually reads — the active pane of the tab session's current window, which is one of v1's three shared window properties and which the owner has had an hour at the keyboard to move. Adopting *that* would feed `#pane`, which is what `App.tsx:295` computes as `activePane` and hands to `useSeenPanes(serverStart, activePane, rows)` at `App.tsx:385` (`useSnapshot.ts:1476`), which marks it seen — **clearing a finish badge on a pane nobody read**, the exact failure the whole reporting line exists to prevent. Discarding it instead, as revision 2 proposed, closes the badge bug and leaves the two wake paths ending somewhere different: the terminal shows the owner's pane and the reply box writes to it while the sidebar, the capture panel and `useSeenPanes` all name the tab's.

So the probe does what reconnect does — and `#landed` needs no change at all.

**Cost, stated rather than discovered:** the probe is **three forks**, not one. `select` is `Client.SelectPane`, which runs `list-panes` and then `select-window` chained with `select-pane`; `where` is `CurrentPane`, which runs `list-panes`. And **waking the phone now moves the owner's active pane**, because `select-pane` is what pulls it back. That is v1's already-accepted shared-active-pane cost reached through a new occasion, and it is the price of the app agreeing with itself about which pane it is on.

**Step 1: Write the failing tests**

```ts
  it('case (b): a quiet OPEN socket is probed with select-then-where, in that order', () => {
    let now = 1_000
    const { term } = makeSession({ storage: memoryStorage(), now: () => now })
    term.start()
    const ws = MockWebSocket.last
    ws.open()
    serverControl(ws, { type: 'pane', pane: '%5' })
    const before = controls(ws).length

    now += LIVENESS_SLACK_MS + 1
    term.wake()

    // Order is the assertion, not just presence. A refactor that drops the
    // select "because the answer is what we wanted anyway" puts the
    // badge-clearing bug straight back, and only the ordering catches it.
    expect(controls(ws).slice(before).map((f) => f.json)).toEqual([
      { type: 'select', pane: '%5' },
      { type: 'where' },
    ])
  })

  it('case (b): the answer is adopted, because the tab asked the question', () => {
    // …probe as above, then answer with '%5' and assert pane is still '%5'…
  })

  it('case (b), the negative control: an answer naming a different pane is followed', () => {
    // The server's "your pane is gone, you are here now". Answer the probe with
    // { type: 'pane', pane: '%9' } and assert term.status.pane === '%9',
    // exactly as it does on reconnect. A probe that discarded its answer -- the
    // revision 2 design -- passes the previous test and fails this one.
  })

  it('does not probe a socket that has been talking', () => {
    // The positive control for the whole feature. Without it, a handler that
    // always probes (or always reconnects) passes every other test here.
    // Receive a data frame, advance the clock by LIVENESS_SLACK_MS - 1 since
    // that frame, wake, and assert no new control frame and no new socket.
  })

  it('is idempotent on an OPEN stale socket: one probe frame and one armed timer', () => {
    let now = 1_000
    const { term } = makeSession({ storage: memoryStorage(), now: () => now })
    term.start()
    const ws = MockWebSocket.last
    ws.open()
    serverControl(ws, { type: 'pane', pane: '%5' })
    const before = controls(ws).length

    now += LIVENESS_SLACK_MS + 1
    // The pair this handler is explicitly written to tolerate.
    term.wake()
    term.wake()

    // ONE probe: a select and a where, not two of each. Counting connect
    // attempts here -- which is what revision 2 specified -- passes with two
    // probes and two timers and notices nothing.
    expect(controls(ws).slice(before).map((f) => f.json)).toEqual([
      { type: 'select', pane: '%5' },
      { type: 'where' },
    ])
    expect(vi.getTimerCount()).toBe(1)

    // ONE timer: let the answer land, then run the clock well past the timeout
    // and assert no orphan fired a reconnect.
    serverControl(ws, { type: 'pane', pane: '%5' })
    vi.advanceTimersByTime(PROBE_TIMEOUT_MS * 3)
    expect(MockWebSocket.instances).toHaveLength(1)
    expect(ws.closedWith).toBeNull()
  })

  it('reconnects when nobody answers within PROBE_TIMEOUT_MS', () => {
    // …probe, advance PROBE_TIMEOUT_MS, assert closedWith 4001 and a second
    // socket. And the boundary sibling: at PROBE_TIMEOUT_MS - 1, nothing.
  })
```

**`vi.getTimerCount()` needs care.** Other timers may be pending in a given fixture (the resize debounce, a backoff). Assert it against a baseline taken immediately before the wake (`const timersBefore = vi.getTimerCount()`, then `expect(vi.getTimerCount()).toBe(timersBefore + 1)`), or the assertion is measuring the fixture rather than the probe. Check what is pending in your fixture before you decide which form to use — **an assertion that happens to be true because the fixture has no other timers is the "true by accident" shape**.

And add the half Task 1 could not write:

```ts
  it('adopts a second answer, because the probe asked a second question', () => {
    // This is what makes Task 1's rename honest: one answer per outstanding
    // question, and the probe creates a second outstanding question on the same
    // socket. Open, answer '%5', wake past the slack, answer '%9', assert '%9'.
    // Then a third, unasked answer of '%3' is dropped.
  })
```

**Step 2: Run, expect FAIL.**

**Step 3: Implement** — case (b), appended to `wake()`:

```ts
    // (b) The socket says it is open. It may be lying, and nothing else in this
    // app will ever find out: no close event fired, so there is no backoff
    // timer, so there is nothing pending that could discover it.
    if (this.#wakeProbe !== null) return
    if (this.#now() - this.#transport.lastRecvAt <= LIVENESS_SLACK_MS) return

    // Exactly what `#opened` does, in exactly that order. The select is what
    // makes `where`'s answer be about *this tab* rather than about the shared
    // window's active pane, which the owner may have moved while the phone
    // slept -- and an adopted answer feeds `#pane`, which feeds `useSeenPanes`,
    // which would clear a finish badge on a pane nobody read.
    //
    // It costs three tmux forks (SelectPane: list-panes, then select-window
    // chained with select-pane; CurrentPane: list-panes) and it pulls the
    // owner's active pane back to this tab's. Both are the price of the two
    // wake paths ending in the same state, and the `lastRecvAt` gate above is
    // what keeps it off the common case of a tab hidden for a moment.
    if (this.#pane) this.#transport.select(this.#pane)
    this.#awaitingWhere = this.#transport.where()
    this.#wakeProbe = setTimeout(() => {
      this.#wakeProbe = null
      this.#discard()
    }, PROBE_TIMEOUT_MS)
```

and the cancel, in `#landed` — or better, wherever *any* inbound frame lands, since any frame proves the socket alive. Put it in the `onData` and `onControl` callbacks in `#connect()`, through one private method:

```ts
  /** Any inbound frame answers the probe: the question was "is anyone there". */
  #frameArrived(): void {
    if (this.#wakeProbe === null) return
    clearTimeout(this.#wakeProbe)
    this.#wakeProbe = null
  }
```

`#wakeProbe` is nulled in exactly three places — here, in the timeout, and in `#discard()` — and a non-null value is the in-flight flag. This is the same pattern as `#checkGone()`'s `#probing` (`Terminal.tsx:418-419`, `:676-689`) and should be written to look like it.

**Step 4: Run both suites, expect PASS.** Then `pnpm typecheck`.

**Step 5: Mutation testing**

| Mutant | Killed by |
| --- | --- |
| Drop the `select` and send only `where` | `select-then-where, in that order`. **The headline mutant** — it is revision 2's design, and it passes an assertion written as "a `where` was sent" |
| Send `where` before `select` | same test. A `toEqual` on the array catches order; a pair of `toContainEqual`s would not |
| Set `#awaitingWhere = false` after the `where` (revision 2's discard) | `the negative control: an answer naming a different pane is followed`. It **passes** `the answer is adopted`, because the pane was already `%5`. That is the "fixture true by accident" shape and the reason the negative control exists |
| Drop the `if (this.#wakeProbe !== null) return` single-flight guard | `is idempotent on an OPEN stale socket` — at **both** assertions: two select/where pairs, and two timers. Confirm the kill at the frame-array assertion *and* at the timer count; a version that only counted connect attempts would go green |
| Null `#wakeProbe` in `#frameArrived` but not clear the timeout | the same test's tail: the orphan fires `#discard()` after the answer landed and a second socket appears |
| `<= LIVENESS_SLACK_MS` to `< LIVENESS_SLACK_MS` | needs a fixture exactly on the boundary. Add one: silence of exactly `LIVENESS_SLACK_MS` and assert **no** probe |
| `LIVENESS_SLACK_MS` retargeted to `0` | `does not probe a socket that has been talking`. This is the positive control and it is the only test that can kill it — build its fixture from a literal (`now += 1_000`), never from the constant, or fixture and assertion move together |
| Probe unconditionally, ignoring `lastRecvAt` | same test |
| `#discard()` on timeout replaced by `retryNow()` | `reconnects when nobody answers` — `retryNow` is a no-op while the transport exists, so no second socket |
| `PROBE_TIMEOUT_MS` retargeted longer | `reconnects when nobody answers`, whose `advanceTimersByTime` must be a literal-derived duration or the assertion moves with the mutant |
| Cancel the probe only on a control frame, not on data | add a test: probe, then receive a `FRAME_DATA` frame, advance past `PROBE_TIMEOUT_MS`, assert no reconnect. Any frame is an answer to "is anyone there" |
| Run the probe from `phase !== 'ready'` | review check, not a mutant: `wake()`'s guard already excludes `ended` and `gone`, and `connecting` is case (c). Note it |

**Step 6: Commit**

```bash
git status
git add web/src/components/Terminal.tsx web/src/components/Terminal.test.tsx
git commit -m "feat: probe a socket that claims to be open after a wake, and reconnect when it does not answer"
```

---

## Phase B — the capture panel (design item 1)

### Task 5: `CaptureRange` and the byte cap

**Files:**
- Modify: `internal/tmux/client.go`
- Create: `internal/tmux/capture_integration_test.go`

**Do not change `Capture`.** `Client.Capture` (`internal/tmux/client.go:490`) runs `capture-pane -p -J -t <paneID>` and its comment explains at length why it has no `-S`: a negative `-S` reaches into scrollback, where a just-answered approval box lives, and reporting one as a live question is the false positive that would train the owner to ignore the badge. That reasoning is about the classifier. The panel gets **its own bounded call** beside it, which is better than widening the classifier's — and note that `Capture` today has **no output bound at all**, the one exception among `MaxTitle`, `MaxLabel`, `MaxActivity`, `MaxQuestion` and `MaxReportBytes`, justified by the classifier needing the whole screen to hash. That justification does not extend to a scrollback capture.

**The flags, all measured on tmux 3.7b and not to be re-derived:**

- `-p` with no `-S` is **the visible screen only** — 24 lines on a 24-row pane, whatever the history holds.
- `-S -<N>` takes N lines of scrollback **plus** the visible screen, clamped to the available history.
- `-J` is **kept**, not added — `Capture` already passes it. It rejoins a line the pane wrapped, so a URL split across rows copies as one string. It does **not** join two lines that merely each ended at the pane width: tmux joins only lines carrying its own wrapped flag (measured on an 80-column pane; an exactly-80-character line stayed separate).
- **`-N` is rejected.** It is a different flag meaning "preserve trailing spaces" and it pads every line to the pane width: 206 852 bytes against 180 140 for the same capture (+15%), and padding is exactly what defeats a word-wrapped box. The design brief's `capture-pane -p -S -N` reads as `-S` with a numeric start line; take that reading.
- **`-e` stays off.** With it the capture carries SGR sequences that a `<pre>` renders as garbage and a clipboard carries into whatever you paste into. Measured cost of leaving it off: 54 bytes out of 180 140 — the escapes are not the reason, plain text is.

**The cap is on bytes, not lines**, because a 200-column pane's line is worth twice an 80-column pane's, and a full-width 5 000-line history at 200 columns is about 1 MB.

**Step 1: Write the failing integration test** in `internal/tmux/capture_integration_test.go`, against `testutil.NewServer(t)` — which supplies `-L <own socket>` and `-f /dev/null`; never run a mutating tmux command any other way.

```go
func TestCaptureRangeReturnsScrollbackAndTheScreen(t *testing.T) {
	// Print enough numbered lines to overflow the pane, then capture with and
	// without a start line. The assertion is the DIFFERENCE: `Capture` returns
	// the screen and `CaptureRange` returns strictly more, including a line
	// that has scrolled off. A test that only asserted "CaptureRange returned
	// something" would pass against a CaptureRange that ignored its argument.
}

func TestCaptureRangeCapsBytesAndTruncatesFromTheTop(t *testing.T) {
	// The newest lines are the ones you opened the panel for, so the OLD end is
	// what goes. Print a marker at the start of the run and a different marker
	// at the end, overflow MaxCaptureBytes, and assert the tail marker is
	// present, the head marker is not, and truncated is true.
	// The fixture must genuinely exceed the cap -- check len() of the uncapped
	// capture in the test and t.Fatal if it does not, or this is a fixture that
	// is true by accident forever after someone shrinks it.
}

func TestCaptureRangeCutsOnARuneBoundary(t *testing.T) {
	// Multi-byte content sized so the cap lands mid-rune. Assert
	// utf8.ValidString(out) -- which is the whole point -- and assert the
	// uncapped capture was NOT already valid-at-that-offset, or the test passes
	// against a naive slice.
}

func TestCaptureRangeRefusesABadPaneID(t *testing.T) {
	// tmux resolves an empty target to "whatever is current" and exits 0.
}
```

**Step 2: Run, expect FAIL** — `undefined: CaptureRange`, `MaxCaptureBytes`.

**Step 3: Implement**

```go
// MaxCaptureBytes bounds one scrollback capture.
//
// The cap is on BYTES and not on lines, because a line is not a unit of size: a
// 200-column pane's line is worth twice an 80-column pane's, and a full-width
// 5000-line history at 200 columns is about 1 MB. Measured on a 200x50 pane
// with a 5000-line history of ~88-character lines: the visible screen is 3904
// bytes and 2.6 ms, `-S -2000` is 167 904 bytes and 5.1-5.7 ms, and the whole
// history is 327 227 bytes and 7.6-8.0 ms. Depth is not what costs; the fork is.
const MaxCaptureBytes = 256 << 10

// maxCaptureLines is the deepest scrollback a caller may ask for.
const maxCaptureLines = 5000

// CaptureRange returns a pane's scrollback plus its visible screen, bounded.
//
// Separate from Capture, which the classifier owns and which deliberately has
// no -S: a negative start line reaches into scrollback where a just-answered
// approval box lives, and that is a false `blocked`. That reasoning is about
// the classifier and does not transfer to a panel, so the panel gets its own
// call rather than widening the classifier's.
//
// Truncation is from the TOP, on a rune boundary. The newest lines are the ones
// the panel was opened for, and cutting the tail would throw away the answer to
// keep the question.
func (c *Client) CaptureRange(ctx context.Context, paneID string, lines int) (text string, truncated bool, err error)
```

`lines` is clamped to `[1, maxCaptureLines]` here as well as validated in the handler — the handler's validation is about rejecting a bad request, this one is about the function being safe to call from anywhere.

Truncation from the top on a rune boundary is **not** `truncateAtRuneBoundary`, which cuts the tail (`internal/tmux/snapshot.go:416`). Write its sibling and say in the comment that it is the same judgement pointed the other way:

```go
// truncateHeadAtRuneBoundary keeps the LAST maxBytes of s, cutting forward to
// the start of a rune rather than back. Sibling of truncateAtRuneBoundary, and
// the direction is the whole point: see MaxCaptureBytes.
```

**Step 4: Run, expect PASS.**

```bash
go test ./internal/tmux/ -run TestCaptureRange -v
```

**Step 5: Mutation testing**

| Mutant | Killed by |
| --- | --- |
| Drop `-S` from the args | `ReturnsScrollbackAndTheScreen`, at the scrolled-off line. **The headline mutant** |
| `-S -N` written as `-S N` (positive) | same test — a positive start line counts from the top of the history, not back from the screen |
| Add `-N` | add an assertion that no captured line ends in a space. Without it the flag is invisible to every other assertion here |
| Add `-e` | add an assertion that the output contains no `\x1b` after printing coloured text in the fixture |
| Drop `-J` | add a fixture line longer than the pane width and assert it comes back as one line |
| Truncate from the **bottom** (call `truncateAtRuneBoundary`) | `CapsBytesAndTruncatesFromTheTop` — and only because that test asserts **both** markers, one present and one absent. A test asserting only `len(out) <= MaxCaptureBytes` scores this as a kill and is the "cannot fail" shape |
| `truncated` hard-coded `false` | same test's `truncated` assertion |
| `truncated` hard-coded `true` | add an assertion to `ReturnsScrollbackAndTheScreen` that a small capture reports `truncated == false`. Without it, half the flag is untested |
| Plain `s[len(s)-MaxCaptureBytes:]` with no rune walk | `CutsOnARuneBoundary` |
| `MaxCaptureBytes` retargeted to `1 << 30` | `CapsBytesAndTruncatesFromTheTop`, **provided its fixture size is a literal** and not derived from `MaxCaptureBytes`. Derive the fixture from a literal byte count and assert against the constant, never the reverse |
| Clamp `lines` to `[1, 500]` instead of `maxCaptureLines` | nothing here; add a unit test that `CaptureRange(ctx, id, 4000)` passes `-S -4000`, by asserting on a recorded arg list rather than on tmux output |

**Step 6: Commit**

```bash
git status
git add internal/tmux/client.go internal/tmux/capture_integration_test.go
git commit -m "feat: a bounded scrollback capture, separate from the classifier's"
```

---

### Task 6: `GET /api/panes/{id}/capture`

**Files:**
- Modify: `internal/front/server.go`, `internal/front/manage.go` (or a new `internal/front/capture.go`), `internal/front/server_test.go`
- Modify: `web/src/lib/manage.ts` (or a new `web/src/lib/capture.ts`), and its test

**It is a GET and that is deliberate.** It is a read; v1's exact-Origin rule is scoped to state-changing requests, and a cross-origin page cannot read a GET's body without CORS, which is not enabled. It sits behind `cfg.Auth.Protect` like `/api/snapshot`.

**The id arrives percent-encoded**, as every management route's does — `encodeURIComponent("%3")` → `"%253"`, and `r.PathValue("id")` hands back `"%3"`. `internal/front/manage.go:29` states this and `manage_test.go:138` pins the encoded spelling; do the same here.

Route, beside the existing pane routes at `internal/front/server.go:270`:

```go
	mux.Handle("GET /api/panes/{id}/capture", cfg.Auth.Protect(http.HandlerFunc(s.capturePane)))
```

Response:

```json
{ "paneId": "%3", "text": "…", "lines": 1000, "truncated": false, "capturedAt": 1789075200000 }
```

**Step 1: Write the failing tests** in `internal/front/server_test.go`:

- a capture with no `lines` uses `defaultCaptureLines`;
- `?lines=2000` reaches tmux as 2 000 — assert on what the fake manage layer recorded, not on the text;
- `?lines=abc`, `?lines=-1`, `?lines=0`, `?lines=1e3` are **400**, not silently defaulted. A string that is not an integer must never reach tmux: `lines` becomes part of an argv element and the one rule this daemon holds everywhere is that nothing unvalidated does;
- `?lines=999999` is clamped to `maxCaptureLines` (or 400 — **pick one and pin it**; clamping is friendlier and matches `CaptureRange`'s own clamp, so clamp, and say so in the handler comment);
- an unencoded `%3` in the path is a 400 from `net/http` before the mux — pin the encoded spelling in the test the way `manage_test.go:138` does;
- a pane that does not exist returns the error from tmux with a non-200, and the panel is expected to show it;
- no cookie → 401, exactly as `/api/snapshot`;
- `capturedAt` is unix **milliseconds** and is the daemon's clock, not the browser's.

**Step 2: Run, expect FAIL.**

**Step 3: Implement.** One handler in the shape of `zoomPane` (`internal/front/manage.go:246`) — `manageCtx(r)`, the call, `writeManageError` on failure. `defaultCaptureLines = 1000` lives in Go, with the open question in its comment:

```go
// defaultCaptureLines is how far back the panel looks when the caller does not
// say.
//
// A GUESS -- design open question 5. Sized against a measured ~5 KB per 1000
// lines at 80 columns, which puts the default comfortably inside
// MaxCaptureBytes for an ordinary pane. The real question is how far back a
// person actually scrolls to answer an agent, and nobody has measured it.
const defaultCaptureLines = 1000
```

**Step 4: Run both suites, expect PASS.**

**Step 5: Mutation testing**

| Mutant | Killed by |
| --- | --- |
| Parse `lines` with a bare `strconv.Atoi` and ignore the error (0 on failure) | the `?lines=abc` case — **provided that test asserts 400 and not just "did not crash"** |
| Accept a negative `lines` | `?lines=-1` |
| Drop the `Auth.Protect` wrapper | the no-cookie case. Check this one by hand as well: an unprotected route is the single worst defect this task can ship |
| Register the route as `POST` | every test in the file, but confirm the 405 rather than a 404 |
| `defaultCaptureLines` retargeted to 1 | the no-`lines` test, **whose assertion must be on the recorded argument** (`-S -1000`) and not on how much text came back — text length varies with the fixture and would make this a test that cannot fail |
| Return `capturedAt` in seconds | assert the magnitude (`> 1e12`) in the test, or a units slip is invisible |
| Drop `truncated` from the JSON | a field-presence assertion on the decoded map, not on a typed struct that would fill in the zero value |

**Step 6: Commit**

```bash
git status
git add internal/front/server.go internal/front/capture.go internal/front/server_test.go web/src/lib/capture.ts web/src/lib/capture.test.ts
git commit -m "feat: an authenticated route that captures a pane's scrollback once"
```

---

### Task 7: The panel

**Files:**
- Create: `web/src/components/CapturePanel.tsx`, `web/src/components/CapturePanel.test.tsx`
- Modify: `web/src/App.tsx` (mount + the control that opens it)

**Four things about this component are decisions, not styling:**

1. **A `<pre>`, never a `<textarea>`.** A textarea is a focusable text field, and focusing it opens the soft keyboard — the opposite of what a read-and-copy surface wants on the device it exists for. The `<pre>` carries `white-space: pre-wrap; overflow-wrap: anywhere; user-select: text`, monospace, and the terminal's own palette tokens.
2. **`onOpenAutoFocus` must be prevented.** Radix's `Dialog` focuses its first tabbable child on open, and this panel's first tabbable child is the pane selector — so the soft keyboard opens anyway. Focus goes to the dialog container instead, so Escape still closes and the `<pre>` and the buttons are still reachable by tab. **`onOpenAutoFocus` is used nowhere in this app today**, so this is the first dialog that needs it: do not assume the default is harmless, and do not copy the pattern from `DevicesDialog` or `KillDialog`, neither of which has this problem.
3. **Copy-all copies from state already in memory and never fetches first.** iOS Safari rejects a `writeText` that is not synchronously inside a user gesture. `navigator.clipboard.writeText` needs a secure context, which the device cookie already requires — and `--dev` on loopback is a secure context too. **Whether copying from memory inside the handler satisfies iOS is design open question 7 and needs a device**; write it the way that can work and do not claim it does.
4. **No auto-refresh.** Three reasons, in the order they matter: a re-render destroys a selection in progress and this panel exists to be selected from; an interval is a second poll at a cadence nobody chose, against a fork that costs 2–8 ms rather than the snapshot's shared one; and a panel that keeps up with the pane is a second terminal, which is what closing the panel gets you. Note that on a phone the panel is a full-screen dialog and the live terminal is *behind* it, not beside it — so "there is one of those on the other side of the screen already" is false there and is not the reason.

The header shows the capture time and **ages it** — `captured 14s ago` — and a **Recapture** button is the only refresh. Opening captures. Changing pane (Task 8) captures.

**Step 1: Write the failing tests.** This suite runs under `renderToStaticMarkup` like the rest of the component tests here, so **the logic that must be asserted has to live outside the click handlers** — the existing files say so explicitly ("a rule inside a click handler is a rule no test reaches"). Put the decisions in exported pure functions and test those:

- `captureAge(capturedAt, now)` → `'just now' | '14s ago' | '3m ago'` — with a boundary fixture at each transition, and a fixture where `now < capturedAt` (a clock skew) asserting it does not render a negative age;
- `truncationNotice(truncated, lines)` → the line the panel shows when the daemon cut the top, or null;
- and a render test asserting the `<pre>` is a `pre` and not a `textarea`, that it carries `user-select: text` (assert the **computed style property in the class or style attribute you actually set**, not a Tailwind class name that may contain the word for another reason — that is the `disabled:pointer-events-none` trap), and that the dialog's `onOpenAutoFocus` prop is present.

**Step 2–4:** run, implement, run. Then `pnpm typecheck`.

**Step 5: Mutation testing**

| Mutant | Killed by |
| --- | --- |
| Render a `<textarea>` instead of a `<pre>` | the tag assertion. **The headline mutant**, because it is the version that looks better and raises a keyboard |
| Drop `onOpenAutoFocus` | the prop-presence assertion — and note in the commit that this is a *structural* assertion, not a behavioural one: nothing in vitest can prove focus went to the container, and Task 9's Playwright test is where that becomes observable |
| `onOpenAutoFocus` present but not calling `preventDefault` | the same limitation. Assert the handler calls `preventDefault` on a fake event, driving the exported handler directly |
| Copy-all refetches before writing | assert the exported copy handler takes the text as an argument and performs no fetch — make that structural by giving it a signature that has nowhere to fetch from |
| Add a `setInterval` refresh | nothing, and no test can catch it. **Reduced to a review check**: the reasons are in the comment, and a reviewer enforces them |
| `captureAge` off by one at each boundary | the boundary fixtures — which must be literals, not derived from the thresholds |
| `captureAge` returning a negative age on skew | the skew fixture |
| `truncationNotice` returning the notice unconditionally | a `truncated: false` fixture asserting null |

**Step 6: Commit**

```bash
git status
git add web/src/components/CapturePanel.tsx web/src/components/CapturePanel.test.tsx web/src/App.tsx
git commit -m "feat: a read-only scrollback panel you can select and copy from"
```

---

### Task 8: The panel's selector *is* the tab's selection

**Files:**
- Modify: `web/src/components/CapturePanel.tsx`, `web/src/components/CapturePanel.test.tsx`, `web/src/App.tsx`

**This is the decision, and it is worth a shared property to make the alternative unrepresentable.** The panel is a full-screen dialog with a pane selector; the reply box (Phase D) writes to the tab's client's active pane. On a phone the two cannot be seen at once, so a panel-local selection would let you read pane A, close the panel, type into the box and send to pane B — **with nothing on screen having lied to you at any point**. A reply landing on the wrong pane because two controls disagreed is the worst thing this batch can produce.

So changing pane in the panel goes through the same `handleSelectPane` a sidebar row click goes through (`App.tsx:258-283`), and then captures.

**Two qualifications the implementer must carry, both of which the design states carefully:**

- **"The same `select`" is only true within a group.** `handleSelectPane` branches: same group is one `transport.select` on the live socket; a pane in **another** group sets `pendingPane` and `setPicked` and returns without touching the terminal. That changes `session`, which changes the `/ws?session=` URL, which **tears the socket down and brings up a new throwaway tmux session in the new group** — then the effect at `App.tsx:239-256` replays `pendingPane` once the new socket is `ready`, and the new socket's `#opened` selects it again before its own `where`. A cross-group pick from the panel is a teardown, a new session, a PTY and a redraw, **not one fork**. That is the existing cost of a cross-group sidebar click, and the panel inherits rather than adds it — but the panel makes it far easier to reach, because the selector reuses the palette's row source, which **spans every session in the snapshot** (`Palette.tsx:117-171`, `paneEntries`: "Every pane, in snapshot order"; the current session is a flag, not a filter). So the panel **must show the same `unreachable` flag `paneEntries` computes**, and must treat a cross-group capture as the expensive path it is.
- **"Cannot diverge" is about the controls, not about the pane.** One selector means the panel and the reply box always *name* the same pane. What neither controls is that pane moving underneath them: the active pane is shared, `App.tsx:314-321` records that `#pane` is written only by `select` and that nothing corrects it, and `App.tsx:288-295` deliberately declines to read `paneActive` back from the snapshot. That is Phase A's problem, and Task 4's probe is what re-converges it on a wake. Item 1's contribution is only that it does not add a *second* way to get there.

**The cost is stated rather than discovered:** the active pane is a tmux window property shared with every client, so changing pane in the panel moves the owner's local active pane in that window — exactly as a sidebar row click already does. Not a new class of surprise; the existing one reached through a new control.

**Step 1: Write the failing tests.** Again, the logic must be outside the handlers:

- the selector's entries come from `paneEntries` and include a pane from a session other than the current one — assert on the entry list, with a two-session fixture;
- an `unreachable` entry is rendered disabled and its `action` is not dispatched;
- selecting the current pane **does not** re-select and does not re-capture (a fixture that already satisfies the assertion would hide a selector that fires on every render);
- selecting a different pane calls `handleSelectPane` **and then** captures, in that order — assert the order, since a capture that races the select captures the old pane.

**Step 2–4:** run, implement, run. `pnpm typecheck`.

**Step 5: Mutation testing**

| Mutant | Killed by |
| --- | --- |
| Give the panel its own `useState` pane instead of calling `handleSelectPane` | the ordering test — **the headline mutant**, and the one the design spends a section rejecting |
| Filter `paneEntries` to the current session | the two-session entry-list test |
| Drop the `unreachable` flag | the disabled-entry test |
| Capture before selecting | the ordering test, which must be an ordered array assertion and not two independent "was called" checks |
| Re-capture on selecting the pane already selected | the no-op test |

**Step 6: Commit**

```bash
git status
git add web/src/components/CapturePanel.tsx web/src/components/CapturePanel.test.tsx web/src/App.tsx
git commit -m "feat: the capture panel's pane selector moves the tab's selection, not a second one"
```

---

### Task 9: The panel under Playwright

**Files:**
- Create: `e2e/capture.spec.ts`

Two things vitest cannot see: that the text is really selectable in a browser, and that copy-all really writes to a clipboard.

- Grant `clipboard-read` and `clipboard-write` in the Playwright context (`test.use({ permissions: [...] })`) — Chromium refuses the read otherwise and the test would be asserting on a rejected promise.
- Open the panel on a pane with known content **that this test produced** — never anything from a live pane; the repo is public.
- Assert the `<pre>` contains that content, that `window.getSelection()` after a triple-click is non-empty, and that after clicking copy-all `navigator.clipboard.readText()` equals the panel's text.
- Assert the truncation notice is **absent** on a short capture, so the notice's own test is not the only fixture it ever sees.

Run it: `npx playwright test e2e/capture.spec.ts`.

**Mutation testing:** make the `<pre>` `user-select: none` and watch the selection assertion go red; make copy-all write a constant and watch the clipboard assertion go red; make copy-all write nothing and watch it go red for a different reason (empty rather than wrong) — check that both directions are distinguishable in the failure output, because a clipboard left over from a previous test is a classic false pass. Clear the clipboard at the start of the test.

**Commit**

```bash
git status
git add e2e/capture.spec.ts
git commit -m "test: the capture panel's text is selectable and copy-all reaches the clipboard"
```

---

## Phase C — the branch on a row (design item 3)

`Row.Path` has carried each pane's working directory since `14c11dd`, sanitized through the same three layers as the label and rendered by nothing. This is what it was for.

### Task 10: `.git/HEAD`, parsed

**Files:**
- Create: `internal/tmux/git.go`, `internal/tmux/git_test.go`

**"One file, no fork" is right about the fork and wrong about the file.** Measured here with throwaway repositories:

| Case | What is on disk | What the row shows |
| --- | --- | --- |
| Normal checkout | `.git/HEAD` → `ref: refs/heads/feat/x` | `feat/x` |
| Detached HEAD, SHA-1 repo | `.git/HEAD` → a **40**-hex line | `@f2aa39a` |
| Detached HEAD, SHA-256 repo | `.git/HEAD` → a **64**-hex line | `@f2aa39a` |
| Unborn (fresh `git init`) | `.git/HEAD` → `ref: refs/heads/master` | `master` |
| Worktree | `.git` is a **file**: `gitdir: /abs/…/.git/worktrees/wt` | `wtbranch` |
| Submodule | `.git` is a **file**: `gitdir: ../../.git/modules/vendor/sub` | that module's branch |
| Bare repo | no `.git`; `HEAD` at the root | nothing |
| Not a repo | nothing, up to the root | nothing |

Five things that table settles, and each is a row in the test:

1. **A worktree and a submodule put a `gitdir:` pointer in a `.git` *file*.** Those cases are two reads, and the pointer may be **absolute** (worktree) or **relative** (submodule) — it must be resolved against the directory holding the `.git` file, **not** against the pane's cwd. Still no fork.
2. **A detached HEAD is prefixed:** `@f2aa39a`, seven characters after an `@`, so it cannot be read as a branch someone named `f2aa39a`.
3. **A SHA-256 repository writes 64 hex characters, not 40.** `git init --object-format=sha256` has existed since 2.29. A parser accepting only 40 shows no branch there: it fails closed, which is the right direction, but *silently*, and the failure looks like "the branch feature does not work in this repo". **The parser accepts 40 or 64, and the table carries both.**
4. **An unborn HEAD is indistinguishable from a normal one** without reading refs, and it shows `master`, which is what `git branch --show-current` says too. Recorded so nobody "fixes" it by adding a refs read.
5. **Bare repositories are not supported.** A pane whose cwd is a bare repo has no worktree, and detecting one costs testing for `HEAD` + `objects/` + `refs/` at every level of every walk on every non-repo path, forever.

The walk: from the pane's path upward, testing for `.git`, stopping at the filesystem root, with `maxGitWalk = 40` so a pathological path or a symlink loop terminates. **Measured on the owner's live server: maximum path depth 7**, so the uncached worst case is 7 `stat`s.

**Step 1: Write the failing test.** A table test in the house style — anonymous struct inline in the `range`, `tc` and not `tt`, `t.Run(tc.name, …)`, `t.Errorf("F(%q) = %q, want %q", …)`, and **a prose comment per row saying which trap it pins** (`internal/tmux/report_test.go:10-49` is the model).

Build the fixtures with real `git init` in `t.TempDir()`, **not by hand**:

```go
// Built by git rather than written by hand, deliberately. The worktree and the
// submodule are the two cases the "one file" claim is wrong about, and a
// fixture written from the claim would encode the claim rather than test it.
// Skip the whole test if git is not on PATH -- t.Skip, not t.Fatal: this is the
// one test in the package whose subject is another program's on-disk format.
```

Cases, one subtest each: normal checkout; a branch with a slash in it; detached at 40 hex; detached at **64** hex (`git init --object-format=sha256` — if the local git refuses it, write that `.git/HEAD` by hand and say in the comment that this one row is hand-built and why); unborn; worktree via `git worktree add`; submodule via `git submodule add` on a local path; bare via `git init --bare`; a plain empty directory; a directory whose parent is a repo (the walk finds it); a directory 41 levels deep inside a repo (the walk gives up); `.git` present but `HEAD` unreadable (fails closed).

Plus one non-table test:

```go
func TestParseHeadRefusesATornRead(t *testing.T) {
	// `git checkout` rewrites .git/HEAD, and a read that lands mid-write gets a
	// short or torn line. The parser accepts only "ref: refs/..." or exactly 40
	// or 64 hex characters and shows nothing otherwise; the next pass, one poll
	// later, reads the settled file.
	// Rows: "ref: refs/hea", "", "f2aa39", 39 hex, 41 hex, 63 hex, 65 hex,
	// "ref: refs/heads/", and 40 characters that are not all hex.
}
```

**Step 2: Run, expect FAIL** — `undefined: gitBranch`, `maxGitWalk`.

**Step 3: Implement.** Two functions, both pure enough to test:

```go
// maxGitWalk bounds the climb from a pane's directory to the filesystem root.
//
// Measured on the owner's live server: maximum pane path depth 7. Forty is
// slack, and its job is a pathological path or a symlink loop, not a real tree.
const maxGitWalk = 40

// findGitDir climbs from dir looking for .git, and resolves the `gitdir:`
// indirection a worktree or a submodule leaves in a .git FILE.
//
// The pointer is resolved against the directory holding the .git file and never
// against the pane's cwd: a submodule's is RELATIVE (`../../.git/modules/...`)
// and resolving it anywhere else names a directory that does not exist.
func findGitDir(dir string) (gitDir string, ok bool)

// parseHead turns the contents of a HEAD file into what the row shows: a branch
// name, or "@" + the first seven characters of a detached hash, or "" for
// anything it does not recognise.
//
// Accepts 40 OR 64 hex characters. `git init --object-format=sha256` has
// existed since 2.29 and writes 64; a parser accepting only 40 fails closed in
// such a repo, silently, and looks like the feature not working.
func parseHead(contents string) string
```

**Step 4: Run, expect PASS.**

```bash
go test ./internal/tmux/ -run TestGit -v
```

**Step 5: Mutation testing**

| Mutant | Killed by |
| --- | --- |
| Accept only 40 hex | the 64-hex row. **The headline mutant**, because it fails closed and would never be noticed in CI without this row |
| Accept any length of hex | the torn-read rows at 39, 41, 63 and 65 |
| Drop the `@` prefix | the detached rows — assert the exact string `@f2aa39a`, not `strings.Contains(out, "f2aa39a")` |
| Take 8 characters instead of 7 | same, on the exact string |
| Resolve a relative `gitdir:` against the pane's cwd | the **submodule** row. This is why the fixture must be a real `git submodule add` and not a hand-written relative pointer that happens to resolve from either base |
| Ignore the `.git`-is-a-file case entirely | the worktree and submodule rows |
| `maxGitWalk` retargeted to 4 | the 41-levels-deep row — build that fixture from a **literal** 41, not from `maxGitWalk + 1`, or fixture and assertion move together and the mutant survives |
| `maxGitWalk` retargeted to 4 000 | nothing, and it should not be: it is a slack bound, not a measured value. Record it as an honest survivor |
| Treat a bare repo as a repo | the bare row |
| `strings.TrimSpace` dropped after the read | add a row whose HEAD has a trailing newline — which every real one does, so **check the ordinary rows are not already covering this**; if they are, this mutant is killed and say which row did it |
| Return the branch for an unreadable HEAD instead of "" | the unreadable row |

**Step 6: Commit**

```bash
git status
git add internal/tmux/git.go internal/tmux/git_test.go
git commit -m "feat: read a working tree's branch off .git/HEAD, worktrees and submodules included"
```

---

### Task 11: The git reader goroutine

**Files:**
- Modify: `internal/tmux/git.go`, `internal/tmux/poller.go`
- Create: `internal/tmux/git_reader_test.go`

**It does not run on the poll goroutine, and that is the entire design.** Every other read this daemon makes goes through `exec.CommandContext` and therefore has a deadline; `os.Stat` and `os.ReadFile` have **none**, and a `stat` on a wedged NFS or sshfs mount blocks uninterruptibly. The poll is the sidebar. The whole poll runs under one deadline (`poller.go:393-394`), so a wedged `stat` inline would eat the same budget as the captures — and then the sidebar.

So: **its own goroutine, its own map, its own mutex**, handed the distinct path set after each poll; the poller reads whatever the map holds when it assembles rows. A hung filesystem then costs a missing or stale branch and nothing else.

**Measured on the owner's live server: 12 panes, 3 distinct paths.** That ratio is the design. The cache is keyed by **directory**, never by pane:

```go
type gitEntry struct {
	gitDir    string
	branch    string
	headMtime time.Time
	headSize  int64
	checkedAt time.Time
}

// gitMissTTL is how long "there is no repository above this directory" is
// believed.
//
// The negative cache is the part that matters. Most panes are not in
// repositories, and re-walking eight levels every 1.5s for a shell sitting in
// $HOME is pure waste.
const gitMissTTL = 30 * time.Second
```

Per distinct path per pass: one `stat` of the resolved HEAD; **re-read only when mtime or size differs**.

**Two details of the handoff are load-bearing and easy to get wrong:**

- **The reader drops path sets; it never queues them.** The handoff is a one-slot mailbox — a buffered channel of capacity 1 with a **non-blocking send that overwrites**, or a mutex-guarded "latest wanted" field. A queue turns one wedged `stat` into a growing backlog of path sets that are stale before they are read, while the poller keeps producing another every 1.5 s forever. The newest set is the only one worth having.
- **A serial walker makes every directory stale, not only the wedged one.** One goroutine walking three paths in order stops at the first blocked `stat`, and the other two are never refreshed — so a single wedged mount empties the whole sidebar's branches, which is the failure the separate goroutine was supposed to contain, **reintroduced one level down**. Each distinct directory is therefore **its own unit of work with its own in-flight flag**: a directory already being checked is skipped rather than waited on, and the rest proceed.

**Where it plugs in.** `tmux.Options` (`internal/tmux/poller.go:118-142`) is the documented extension point — it exists "so that agent classification could be added without widening `NewPoller` and `NewPollerFunc`", and `Capture`/`Connected` are the precedent for a paired optional feature (`NewPollerWith` panics on half-wiring, `poller.go:152-170`). The enrichment call goes beside `classify` at **`poller.go:468`**, before `p.mu.Lock()` at 470, whose comment is the contract to satisfy: *"Before publishing, so no reader ever sees a row between its snapshot fields being set and its state being decided."*

**And the generation reset.** `poller.go:463-465` clears the classifier's and the reports' pane-keyed memory when the tmux server's start time changes, because a restart renumbers panes from `%0`. The git cache is keyed by **directory**, not by pane, so it needs **no** reset — and that is worth one comment, because the next reader will assume it does.

**Step 1: Write the failing tests**

```go
func TestGitReaderDoesNotRunOnThePollGoroutine(t *testing.T) {
	// Block the reader on a directory that never answers -- a fake filesystem
	// hook or a path whose stat is injected -- and assert the poll still
	// completes within its deadline. Without this test the goroutine is one
	// refactor away from being inlined "for simplicity".
	//
	// The fixture must genuinely block: assert the reader is still blocked at
	// the end of the test, or a stat that returns instantly makes this pass
	// while proving nothing.
}

func TestOneWedgedDirectoryDoesNotStallTheOthers(t *testing.T) {
	// Three directories, one of them blocked. The other two must have branches
	// after one pass. A serial walker fails this and passes the one above it.
}

func TestTheMailboxHoldsOneSetAndDropsTheRest(t *testing.T) {
	// Hand the reader three path sets while it is busy; assert it processes the
	// FIRST (already taken) and then the THIRD, and never the second.
}

func TestAnUnchangedHeadIsNotReRead(t *testing.T) {
	// Count reads through an injected reader. Two passes over an untouched
	// repository is one read, not two. Then touch HEAD (change mtime AND
	// content) and assert the second pass re-reads.
}

func TestAMissIsCachedForGitMissTTL(t *testing.T) {
	// A directory with no repository above it is walked once, not once per
	// poll. Count walk attempts through an injected stat, advance an injected
	// clock past gitMissTTL, assert it walks again.
}

func TestOnePollStillForksTmuxOnce(t *testing.T) {
	// Three panes in three directories. The shim technique in
	// serverstart_internal_test.go is the instrument, and
	// TestOnePollForksTmuxOnce is the model. The git reader must add ZERO
	// forks: it reads files.
}
```

**Step 2–4:** run, implement, run.

**Instrument the injections properly.** Time comes from `p.nowFn` (`poller.go:42`), which already exists; the filesystem needs one small interface (`stat`, `readFile`) so a test can block and count. Do not reach for `os` directly inside the reader.

**Step 5: Mutation testing**

| Mutant | Killed by |
| --- | --- |
| Call the git read inline in `refresh` instead of on the goroutine | `DoesNotRunOnThePollGoroutine`. **The headline mutant** and the whole reason the design is shaped this way |
| Make the mailbox a buffered channel of 16 | `TheMailboxHoldsOneSetAndDropsTheRest` — the second set is processed |
| Make the mailbox send blocking | the same test hangs, and **a hang is not a kill**. If the test times out rather than failing an assertion, that is the "bogus kill" shape: give the test its own short deadline and fail with a message, so the mutant produces a named failure |
| Walk the directories serially in one loop | `OneWedgedDirectoryDoesNotStallTheOthers` |
| Wait on a directory already in flight instead of skipping it | same test, with a second pass arriving while the first is blocked |
| Compare only mtime, not size | add a row: rewrite HEAD to a different branch of the same length within the mtime granularity, and assert the re-read happens. If your filesystem's mtime resolution makes this unreproducible, **say so in the commit** and keep the size check on the reasoning rather than on a flaky test |
| Compare only size, not mtime | a same-length branch change with a new mtime |
| Drop the negative cache | `AMissIsCachedForGitMissTTL` at its walk count |
| `gitMissTTL` retargeted to 0 | the same test — build its clock advance from a literal, not from `gitMissTTL` |
| Key the cache by pane id instead of by directory | `OnePollStillForksTmuxOnce` will not see it. Add an explicit assertion: twelve panes in three directories perform **three** HEAD stats, not twelve. That ratio is the measured design and nothing else asserts it |
| Reset the git cache on a server-start change | nothing, and it should not — the cache is keyed by directory. Record it as an honest survivor with that reasoning |

**Step 6: Commit**

```bash
git status
git add internal/tmux/git.go internal/tmux/git_reader_test.go internal/tmux/poller.go
git commit -m "feat: read branches on a goroutine of their own, one unit of work per directory"
```

---

### Task 12: `Branch` on the wire

**Files:**
- Modify: `internal/tmux/snapshot.go`, `web/src/lib/useSnapshot.ts`, `web/src/lib/useSnapshot.test.ts`, and every TypeScript file that builds a complete `SnapshotRow`

**`Row` gains one field, taking it from 19 json tags to 20:**

```go
	// Branch is the pane's git branch: "" when the path is not in a work tree,
	// and "@<7-hex>" when HEAD is detached.
	//
	// Read off .git/HEAD from a goroutine of its own, never the poll's, so it
	// is up to a poll interval stale in exactly the way Path is. One authority
	// -- the filesystem -- and nothing about the agent enters it.
	Branch string `json:"branch"`
```

**The contract test is designed to fail here and that is the point.** `web/src/lib/useSnapshot.test.ts:122` (*"uses the json names tmux.Row marshals"*) extracts `type Row struct` from `internal/tmux/snapshot.go` at `:123`, collects its json tags, and asserts `toHaveLength(19)` at **`:132`** against a **hand-written literal** — deliberately not `Object.keys(row()).length`, "because a count derived from the TypeScript side would agree with itself forever". **It becomes 20 at `:132` and stays a literal**, and the companion assertion at `:133` means the TypeScript `row()` fixture gains the field in the same change.

**Find every fixture, and do not trust the test suite to find them for you.** vitest does not typecheck, so a fixture missing the new field leaves the suite green over TypeScript that will not build — this exact thing happened in this repo, in Task 4 of the previous plan, where two test files building a complete `SnapshotRow` were missed and both checks came back green because the typecheck was not actually running. So:

```bash
grep -rln 'SnapshotRow' web/src internal/integrations
pnpm typecheck    # FROM THE REPO ROOT, and read the exit code
```

`rowsEqual` (in `useSnapshot.ts`) must compare the new field, or a branch change alone will not re-render the row.

**Step 1–4:** change the count first, watch the frontend suite go red, add the Go field, add the TypeScript field and the fixtures, watch it go green. Run `make test` **and** `pnpm typecheck`.

**Step 5: Mutation testing**

| Mutant | Killed by |
| --- | --- |
| Change the Go json tag to `"gitBranch"` | the contract test's name comparison, not its count |
| Add the Go field with `json:"-"` | the count assertion at `:132` |
| Add the TypeScript field but not the Go one | the count assertion, from the other side |
| Drop `branch` from `rowsEqual` | a test that two rows differing only in `branch` are not equal. **Write it** — nothing else in the suite covers it, and its absence is how a branch change silently fails to repaint |
| Change the count to `20` without the field | the tag-name comparison at `:133` |

**Step 6: Commit**

```bash
git status
git add internal/tmux/snapshot.go web/src/lib/useSnapshot.ts web/src/lib/useSnapshot.test.ts <the fixtures grep found>
git commit -m "feat: the pane's branch crosses the wire"
```

---

### Task 13: The chip, and the width budget

**Files:**
- Modify: `web/src/components/AppSidebar.tsx`, `web/src/components/AppSidebar.test.tsx`

**Step 0, before any code: settle open question 4 by looking.** Open the app on a **desktop** with a split window in a repository. The narrowest row is the desktop one, not the phone's — that is the correction revision 3 made — so this needs no device. Look at where the pane name becomes unreadable and record the number in the commit message. If 50 px is wrong, **the group cap moves and the slot rule does not**.

**The slot rule, which is the part that must not move.** A pane row today carries: the mark with the state dot on its corner, the name, an optional label chip, the tmux-active marker, a right-aligned command capsule, and a second line with the activity under a hover marquee. There is no free space.

**The branch gets its own slot and never shares one.** Right-aligned on the first line, `Badge` geometry, an **outline** variant rather than secondary so it and the command capsule are not confusable, and rendered **whenever `Branch` is non-empty — always, and on nothing else.**

Revision 1 had it take the command capsule's slot with "the command wins" as the tie-break, and that is wrong in the one way this codebase has a rule about. The capsule renders only when `paneText` fell all the way to `fromCommand` and the window name has not already borrowed that same word (`AppSidebar.tsx:1119-1120`); and `paneText`'s ladder takes its top rung, `question`, only when `agentState === 'blocked'`, while the `activity` rung is event-authority only and expires. So a chip rendering *iff the capsule did not* would **appear on an un-integrated Claude pane the moment it blocks and disappear when the block clears**, and would disappear from an integrated pane a minute after its report expires. A row element appearing on agent state is exactly what the rule forbids — and worse, its presence would correlate with *which authority is reporting*, which is the other half of the same rule.

> **The branch chip's presence is a function of the pane's directory and of nothing else.** It changes when the pane changes directory and at no other time, identically whether the pane is blocked, working, done or idle.

**The width budget, because there is not enough room for two `max-w-24` chips.** `Badge`'s base class string is `shrink-0` with `overflow-hidden` (`web/src/components/ui/badge.tsx:7`), so **neither chip yields** — the name cannot pay for something that refuses to be paid, which is what revision 2 got backwards. What actually happens is that the row overflows and `SidebarMenuSubButton`'s own `overflow-hidden` (`ui/sidebar.tsx:667`) clips it, and the branch, being outermost, is the thing that disappears.

Computed from the class chain, for a pane row inside a **split** window — the deepest nesting and the narrowest row:

```
256   --sidebar-width (16rem, ui/sidebar.tsx:27; App.tsx renders a bare provider)
-  1  Sidebar border-r
- 16  SidebarGroup p-2
- 28  SidebarMenuSub mx-3.5
-  1  SidebarMenuSub border-l
- 20  SidebarMenuSub px-2.5
- 16  SidebarMenuSubButton px-2
- 16  RowIcon size-4
-  8  SidebarMenuSubButton gap-2
= 150 px   <- the whole first line
```

Two chips at `max-w-24` are 192 px before the 6 px active dot and three 8 px gaps — **222 px into 150**. **And the phone is the roomier device**: the mobile sheet is `--sidebar-width-mobile`, 18rem (`ui/sidebar.tsx:28`), with no `border-r`, giving **183 px — 33 px more than the desktop sidebar**.

**Three things change together or the budget is decorative:**

1. **The pair becomes one shrinkable flex box**, `flex min-w-0 items-center gap-1`, capped at `max-w-20` (80 px). A cap on the group is what makes the two chips trade against each other instead of each holding 96 px it cannot use.
2. **Each chip overrides `Badge`'s `shrink-0`** inside that box and carries `min-w-0 truncate`. Without this the group cap changes nothing: the children still refuse to compress and the overflow is clipped exactly as today.
3. **The branch is additionally capped at `max-w-16`** (64 px), four-fifths of the group, so a long branch cannot squeeze the capsule out of existence. The capsule's own `max-w-24` (`AppSidebar.tsx:508`) stays and stops binding once the group cap does.

The row's first-line `gap-2` (`AppSidebar.tsx:1123`) drops to `gap-1.5`. The arithmetic it closes:

```
150  available
-  6  active dot (size-1.5, shrink-0)
-  6  gap
- 50  name floor -- below this "pane 3" is not readable  (open question 4)
-  6  gap
- 80  chip pair, max-w-20
=  2  px spare
```

The **shallower** row — a pane that is its own window — sits one nesting level up and already carries the roomier `max-w-32` capsule (`AppSidebar.tsx:451`). The branch gets the same `max-w-16` there and the group cap does not bind.

**Step 1: Write the failing tests.** These run through `renderToStaticMarkup`, so assert on the markup — and **do not assert a Tailwind class name that could be present for another reason**; `expect(cls).toContain('disabled')` passing because of `disabled:pointer-events-none` is the recorded example of that trap here.

- **The presence rule, four fixtures, one assertion each:** the same row with `branch: 'main'` and `agentState` of `''`, `'working'`, `'blocked'` (with a `question`) and `'idle'` — the chip renders in **all four**, identically. This is the test the rule exists for and it must be four fixtures, not one.
- **The absence rule:** `branch: ''` renders no chip, at all four states.
- **The chip and the capsule coexist:** a fixture with both, asserting both are in the markup and the branch is the outermost of the two.
- **The chip is `variant="outline"`** and the capsule is `variant="secondary"` — assert the distinguishing class, and assert they differ from each other on the same line, so a mutant that makes both outline is caught by the relationship rather than by a literal.
- **The pair carries the group cap and both children carry `min-w-0 truncate` and not `shrink-0`.** Assert the absence of `shrink-0` **inside the pair specifically**, since `Badge`'s base string contains it and a naive `toContain` check on the whole row would pass either way. This is the trap in this task.
- **Both row depths:** a pane in a split window and a pane that is its own window.

**Step 2–4:** run, implement, run. Then look at it, with a real repository open.

**Step 5: Mutation testing**

| Mutant | Killed by |
| --- | --- |
| Render the chip only when the capsule does not (`{!showCommand && branch && …}`) | the four-state presence test — it goes red on `blocked`. **The headline mutant**: it is revision 1's design and it is the rule this codebase has |
| Render the chip only when `agentState !== 'blocked'` | same test, more directly |
| Drop the `min-w-0 truncate` override on the chips | the shrink-0 absence test — and **only** if that test scopes itself to the pair. Verify the scoping by making the assertion pass/fail deliberately before you trust it |
| Cap the group at `max-w-48` | nothing, and no vitest test can see a computed width. **Reduced to a review check plus the step-0 look**: record in the commit that the budget is enforced by arithmetic and by eye, not by a test |
| `variant="secondary"` on the branch chip | the variant-difference assertion |
| Put the branch on the second line, prefixed | the first-line structural assertion — make sure one exists, asserting the chip is a sibling of the name inside the first-line span |
| Swap the order so the capsule is outermost | the ordering assertion |
| Leave `gap-2` in place | nothing. Review check; it is arithmetic |
| Render the chip on the split row but not the lone-window row | the two-depth test |

**Step 6: Commit**

```bash
git status
git add web/src/components/AppSidebar.tsx web/src/components/AppSidebar.test.tsx
git commit -m "feat: the branch as its own chip, sharing a width budget and never a presence rule"
```

---

## Phase D — the reply box, on the desktop (design item 4a)

**The transport question is already settled and it is not `send-keys`.** Keystrokes reach tmux through a real PTY: the browser sends a `FrameData` (`0x00`) frame, `internal/front/ws.go` hands the payload to `ptybridge.Session.Write`, which is `s.pty.Write(b)`. Nothing the user types is ever an argv element, so there is **no quoting problem, no encoding problem, no `--`, no length limit worth naming** (`wsReadLimit` is 1 MiB, sized for a paste) and **no chunking**. There is no `SendKeys` in this daemon and there never was.

`send-keys -H` was measured anyway and is recorded so nobody re-derives it: it round-trips arbitrary bytes exactly; `0d` is what you send for Enter (a raw `0a` is `C-j`, a different key); and it has a hard limit on the **command line**, not the argument count — 5 444 hex arguments (16 331 bytes) succeeded and 5 452 (16 355 bytes) failed with `command too long`, so a `-H` path caps at roughly 5.4 KiB per invocation and would need chunking near 1 KiB. It buys exactly one thing — replying to a pane the tab is **not** attached to — and that is out of scope for the reason v1 already gives: you have to select the pane to know what you are answering, and selecting it moves the shared active pane anyway.

### Task 14: `end-mode`

**Files:**
- Modify: `internal/front/ws.go`
- Create: `internal/front/endmode_integration_test.go` (or extend `internal/front/ws_test.go`)

**Copy mode does not swallow a reply on our path — it truncates it and runs the rest.** Measured on 3.7b through a real client PTY, with the pane in copy mode, typing `echo quit PARTIAL` and Enter: the `q` **cancelled copy mode**, and `uit PARTIAL` was delivered to the shell and executed.

| Transport | Pane in copy mode, reply sent |
| --- | --- |
| `send-keys` — *not our path* | Consumed by the mode's key table. Nothing reaches the program. Silent |
| PTY write — **our path** | Truncated at the first key the mode binds to cancel, and **the remainder is delivered to the program as input** |

A reply is prose. Prose contains `q`, and Escape, and every other cancel key, usually early. `"Sorry, quick question: can you retry?"` cancels on the `q` of *quick* and hands `uick question: can you retry?` to whatever is sitting there. **So cancelling first is not a nicety; it is what makes a reply either arrive whole or not arrive, which is the only pair of outcomes a text box is allowed to have.**

**The instrument, and it changed in revision 3:**

```
tmux send-keys -X -t %3 cancel
```

Measured on 3.7b, with and without a client attached, target being the **inactive** pane of a two-pane window:

| Target pane's state | Result |
| --- | --- |
| `copy-mode` alone | cancelled; `pane_in_mode` 1 → 0; exit 0 |
| `copy-mode` stacked on `choose-tree` | **only the copy layer popped**; back to `tree-mode`, `pane_in_mode` 2 → 1; exit 0 |
| `copy-mode` stacked on `clock-mode` | only the copy layer popped; back to `clock-mode`; exit 0 |
| `tree-mode` alone | refused: `not in a mode`, exit 1, **mode unchanged** |
| `clock-mode` alone | refused: `not in a mode`, exit 1, **mode unchanged** |
| no mode | refused: `not in a mode`, exit 1, nothing happens |

`send-keys -X` dispatches into the **copy-mode command table**, so a pane not in a copy-mode-family mode has nowhere to deliver it. That makes the instrument **intrinsically scoped to the mode this feature needs cancelled** — no format to get wrong, no default pane to expand against, and stacking handled by construction rather than by a guard that cannot see it. **One fork**, one fewer than `if-shell` wrapping a command.

**Why not `copy-mode -q`, in the form a reader will reach for.** Bare, it also cancels `clock-mode` and `choose-tree`, and a pane's mode is shared with the owner's local client — so typing into a text box on a phone would close the owner's session tree. Guarded with revision 2's `if-shell -F '#{==:#{pane_mode},copy-mode}'` it is **worse than bare**: `if-shell` takes **its own `-t target-pane`**, and without one the format expands against tmux's default pane. Measured on a throwaway socket with two panes in one window:

| Fixture | Revision 2's form, aimed at `%0` | With `-t %0` on `if-shell` |
| --- | --- | --- |
| `%0` inactive in `copy-mode`, `%1` active in no mode | condition read **false** — `%0` **left in copy mode** | condition true, `%0` cancelled |
| `%0` inactive in `tree-mode`, `%1` active in `copy-mode` | condition read **true** — **`%0`'s `choose-tree` was closed** | condition false, tree untouched |

Failing in both directions, and the second row is precisely the failure the guard existed to prevent, reached *by* the guard. And adding `-t` fixes the target but not the mechanism: **modes stack.** `pane_in_mode` is a **count**, not a flag — `copy-mode` on top of `choose-tree` gives 2 while `pane_mode` names only the top layer — so the corrected guard reads true and **one `copy-mode -q` pops both layers**, leaving the owner's tree gone. Scrolling up inside a `choose-tree` is an ordinary thing to do.

**Revision 2's rejection of `send-keys -X cancel` is withdrawn.** It recorded that the command "wants a current client and printed `no current client`". Re-measured with an explicit `-t %<id>`, it succeeds against a specific pane with **no client attached to the server at all**, and behaves identically with one attached. The earlier probe was measuring the missing target, not a missing client.

**The one cost is the exit status, and it is the common case.** A pane in no mode — most replies — exits 1 with `not in a mode` on stderr. **The daemon runs the cancel, ignores a non-zero exit, never surfaces it to the browser, and proceeds to the PTY write regardless.** A version that propagated the error would fail every ordinary reply. `ValidatePaneID` still runs first and is what guards the target; the exit status is not doing that job.

**Step 1: Verify the six tmux behaviours above by running them. Do not assume them.** On your own socket, through `testutil`-style args (`-L <own socket> -f /dev/null`), never against the live server:

```bash
S=/tmp/…your scratchpad…/sock          # your own scratchpad subdirectory
tmux -L "$S" -f /dev/null new-session -d -s t
tmux -L "$S" -f /dev/null split-window -t t
# note the two pane ids, make the SECOND one active, and drive every command at the FIRST
tmux -L "$S" -f /dev/null list-panes -t t -F '#{pane_id} #{pane_active} #{pane_mode} #{pane_in_mode}'
…
tmux -L "$S" -f /dev/null kill-server
```

Record what each row printed in the commit message, exactly as Task 3 of the previous plan does for tmux. **Never `pkill`, never `killall`** — `kill-server` on your own socket is the only teardown.

**Step 2: Write the failing tests — and the fixture comes before the assertions**

> **This is one of the two tests this plan knows in advance is vacuous as naively written.** "Put a pane in copy mode, send `end-mode`, assert the mode is gone" is what it naturally becomes, and on a **single-pane** fixture tmux's default pane *is* the target — so an expression that resolves against the wrong pane passes every assertion. Revision 2's broken guard would have shipped green.

**The fixture is a window with `%a` and `%b`, `%b` made active, and every assertion aimed at `%a`.** Assert the fixture itself first — that `%b` is active and `%a` is not — or the whole file rests on a `split-window` side effect nobody checked.

Four cases:

| Fixture | Assertion | Which instrument it catches |
| --- | --- | --- |
| `%a` in `copy-mode`, `%b` in no mode | `%a` cancelled | revision 2's expression left it in copy mode |
| `%a` in `tree-mode`, `%b` in `copy-mode` | **`%a` still in `tree-mode`** | revision 2's expression closed it |
| `%a` in `copy-mode` stacked on `tree-mode` | `%a` back to `tree-mode`, not out of both | `copy-mode -q` pops both |
| `%a` in no mode | nothing changes, and the non-zero exit is **swallowed rather than surfaced** | a handler that propagates the error |

Read the state back with `list-panes -t %a -F '#{pane_mode} #{pane_in_mode}'`, and **assert the pre-state as well as the post-state in every case** — that is what stops a fixture being true by accident.

**Step 3: Implement.** A new control type beside `copy-mode` in `internal/front/ws.go`:

```go
	case "end-mode":
		// Pop the copy-mode layer of a pane before writing a reply into it.
		//
		// Not `copy-mode -q`: that cancels clock-mode and choose-tree too, and
		// a pane's mode is shared with the owner's local client, so a phone
		// typing into a text box would close the owner's session tree. And not
		// a `#{pane_mode}` guard around it: if-shell takes its own -t, modes
		// STACK, and pane_in_mode is a count -- see the v3 design.
		//
		// `-X` dispatches into the copy-mode command table, so a pane in any
		// other mode has nowhere to deliver it and refuses with "not in a
		// mode", exit 1, unchanged. That NON-ZERO EXIT IS THE COMMON CASE --
		// most replies go to a pane in no mode -- so it is logged at debug and
		// never surfaced, and the caller writes its bytes regardless. A version
		// that treated it as an error would fail every ordinary reply.
		if err := h.endMode(ctx, m.Pane); err != nil {
			slog.Debug("terminal: end-mode did not apply", "pane", m.Pane, "err", err)
		}
```

and beside `copyMode` (`ws.go:536`):

```go
// endMode pops a pane's copy-mode layer. See the "end-mode" case for why the
// instrument is send-keys -X and why its exit status is not an error signal.
//
// Unlike copyMode, there is no session default: an end-mode with no pane is
// refused rather than aimed at "whatever is current". Entering a mode on the
// tab's own pane is a thing the user asked for; leaving one on an unnamed pane
// is a thing the reply box would do by accident.
func (h *TerminalHandler) endMode(ctx context.Context, pane string) error {
	if err := tmux.ValidatePaneID(pane); err != nil {
		return err
	}
	_, err := h.tm.Run(ctx, "send-keys", "-X", "-t", pane, "cancel")
	return err
}
```

**Step 4: Run, expect PASS.**

**Step 5: Mutation testing**

| Mutant | Killed by |
| --- | --- |
| `copy-mode -q -t <pane>` instead of `send-keys -X -t <pane> cancel` | the `tree-mode` case (it closes the tree) **and** the stacked case (it pops both layers). **The headline mutant, named because a reader who half-remembers this design will write it** |
| Revision 2's `if-shell -F '#{==:#{pane_mode},copy-mode}' 'copy-mode -q -t %a'` | rows 1 and 2, in opposite directions. **Run this one specifically**: it is the mutant the fixture was redesigned for, and if it survives your fixture has one pane |
| Drop the `-t` from `send-keys -X` | row 1 — `%b` is active, so the cancel lands on the wrong pane and `%a` stays in copy mode |
| Drop `-X` (plain `send-keys … cancel`) | row 1 — it types the literal word into the pane. Add an assertion on the pane's content, not only its mode, or this one is invisible |
| Propagate the error to the browser (return the `wsExit`) | the no-mode case. Assert the socket stays open **and** that the subsequent data frame still arrives |
| Drop the `ValidatePaneID` | a case with `pane: "work"` — `tmux send-keys -X -t work cancel` would target the pane the *owner* is sitting in front of. **Write this case**; it is the security-shaped one |
| Default an empty pane to the session, as `copyMode` does | a case with `pane: ""` asserting no fork and no change |
| Log the failure at `Warn` instead of `Debug` | nothing, and it should not be — record it as a survivor with the reasoning that a warn-per-reply would make the log useless |

**Step 6: Commit** — with the step-1 measurements in the message.

```bash
git status
git add internal/front/ws.go internal/front/endmode_integration_test.go
git commit -m "feat: end-mode pops a pane's copy layer and nothing else"
```

---

### Task 15: `endMode()` on the transport

**Files:**
- Modify: `web/src/lib/transport.ts`, `web/src/lib/transport.test.ts`

One line of API and one pin. `ControlMessage` (`transport.ts:68-72`) gains `| { type: 'end-mode'; pane: string }`, and:

```ts
  /**
   * Pop a pane's copy-mode layer before writing into it.
   *
   * Always takes a pane, unlike `copyMode`: the server refuses an unnamed one.
   * See internal/front/ws.go's "end-mode" case.
   */
  endMode(pane: string): boolean {
    return this.sendControl({ type: 'end-mode', pane })
  }
```

`transport.test.ts` already pins `wsControlMessage`'s field names against `internal/front/ws.go`; extend that pin to the new type — the wire contract for this batch is three changes and this is the one with a pinning test already in place.

**Mutation testing:** spell the type `'endmode'` and watch the pin go red; drop `pane` from the message and watch it go red; send it as a `FRAME_DATA` frame and watch the frame-kind assertion go red. If any of those survives, the pin is checking the shape of your own object rather than the string on the wire.

**Commit**

```bash
git status
git add web/src/lib/transport.ts web/src/lib/transport.test.ts
git commit -m "feat: the transport can ask the daemon to end a pane's copy mode"
```

---

### Task 16: The truncated-remainder regression, through `ptybridge`

**Files:**
- Create: `internal/front/reply_integration_test.go`

**Revision 1 specified a test that would have passed while proving nothing**, twice over: with a `send-keys` fixture nothing arrives with or without the cancel, and with a payload containing no `q` everything arrives with or without it. So this test is specified with both halves fixed.

**It runs through `ptybridge`, not `send-keys`, and its payload contains a cancel key.**

- Open a `ptybridge` session attached to a pane, put that pane into copy mode, and write `echo quit PARTIAL` + CR (`0x0d`, not `0x0a` — the pty translates CR, and a raw LF is `C-j`).
- **Without `end-mode`:** assert the pane's history contains the truncated remainder `uit PARTIAL` **and not** the whole line. That is the fragment-executed failure, pinned as the thing that must not happen.
- **With `end-mode` first:** assert it contains `quit PARTIAL` and that the mode is gone.

Both assertions are two-sided on purpose: "contains X" alone passes when *everything* arrived, and "does not contain Y" alone passes when *nothing* did.

**Mutation testing:** send the payload without the leading `end-mode` in the with-cancel case and watch it go red; change the payload to one with no cancel key (`echo hello`) and confirm **both** cases then pass — which is the demonstration that the payload choice is load-bearing, and it belongs in the commit message rather than in the committed test.

**Commit**

```bash
git status
git add internal/front/reply_integration_test.go
git commit -m "test: without end-mode a reply into copy mode runs its own tail"
```

---

### Task 17: The reply box

**Files:**
- Create: `web/src/components/ReplyBox.tsx`, `web/src/components/ReplyBox.test.tsx`
- Modify: `web/src/App.tsx`

**The box is always there**, one line high, at the foot of the terminal, and it **does not take focus on mount**. Three reasons, in the order they matter:

1. **A box that appears and disappears on agent state is a fourth thing branching on that state**, and the app has a rule about that. A control that vanishes under you while you are typing into it teaches you not to trust it — worse than the rule's own case, because a badge you learn to ignore costs attention and a box that vanishes costs the sentence.
2. **`blocked` is 1.5–6 s late** — 1.5 s on the screen path, about 6 s on the report path. A box gated on it arrives after you wanted it.
3. **The box is also how you answer what is not detected.** The README lists four Claude notifications that produce no badge on either authority; those are exactly the cases where you are looking at the pane, understand what it wants, and need to type.

**The behaviours:**

- **Enter sends the text and a CR (`0x0d`), then clears the box.** CR and not LF, measured.
- **Shift+Enter inserts a newline** and sends nothing.
- **A second, smaller button sends the text with no CR.** Answering a `y/n` prompt or a single-key menu is one character and no return, and a box that always appends CR cannot express it.
- **An empty reply sends nothing** — no bytes, no `end-mode`, no fork. An empty send has nothing to protect and every reason not to touch the owner's pane.
- **Every non-empty send is preceded by `end-mode` on the visible pane**, and the socket orders them.
- **`Ctrl+Alt+K` must still open the palette from inside the box.** It is registered in the capture phase ahead of wterm's handler (`Palette.tsx:85`); the box must not swallow it.
- **`Ctrl+C` in the box is the browser's copy, not an interrupt.** Not fixable — it is the platform's — so the interrupt stays a thing you send by focusing the terminal, and **the placeholder must not imply otherwise**.
- **Escape blurs back to the terminal.**
- **Clear the box on a pane change, and say so.** The bytes go to the tab's client, which follows the selection, so a half-typed sentence would silently retarget.

**And `end-mode` remains useful on its own**: it closes an existing gap, since there is an "enter copy mode" action in the header and the palette and no way out of it from the browser. Wire it as a header/palette action in this task.

**Step 1: Write the failing tests.** `renderToStaticMarkup` again, so the decisions go in exported pure functions:

```ts
/**
 * What one press of the send control puts on the wire.
 *
 * Everything the box decides is here rather than in a handler, because
 * `renderToStaticMarkup` fires no handlers and a rule inside one is a rule no
 * test reaches. Returns the frames in order, or [] for a send that must not
 * happen at all.
 */
export function replyFrames(text: string, opts: { pane: string | null; withReturn: boolean }): ReplyFrame[]
```

Cases: empty string → `[]`; whitespace-only → decide and pin it (**it is not empty**: a space is a legitimate answer to some prompts, so send it — say so in the comment, since the opposite is the natural guess); a normal reply with `withReturn` → `[end-mode, data("hi\r")]` **in that order**; `withReturn: false` → `[end-mode, data("y")]`; `pane: null` → no `end-mode` frame but the data still goes (the tab has not landed yet; do not block the reply on a pane id the app has not learned).

Plus render tests: the box is present at all four agent states; it has no `autoFocus`; the placeholder does not mention Ctrl+C.

**Step 2–4:** run, implement, run. `pnpm typecheck`.

**Step 5: Mutation testing**

| Mutant | Killed by |
| --- | --- |
| Append `\n` instead of `\r` | the frame-content assertion, which must be on the exact bytes. **The headline mutant**: LF is `C-j`, a different key |
| Send the data frame before the `end-mode` | the ordering assertion — an ordered array, not two membership checks |
| Send `end-mode` on an empty reply | the empty case, asserting `[]` and not merely "no data frame" |
| Send nothing on a whitespace-only reply | the whitespace case |
| Drop the `end-mode` entirely | the ordering assertion — and note that **nothing in vitest proves the cancel worked**; Task 16 is what proves that, and this only proves the frame is sent |
| `withReturn` inverted | both send cases, which must differ from each other in the assertion |
| Gate the box on `agentState === 'blocked'` | the four-state presence test. Write it as four fixtures, as in Task 13 |
| Add `autoFocus` | the autoFocus assertion — assert the attribute is **absent**, which is a different assertion from asserting it is false |
| Not clearing on a pane change | a test on the exported clear rule, driving a pane change through the same pure function |

**Step 6: Commit**

```bash
git status
git add web/src/components/ReplyBox.tsx web/src/components/ReplyBox.test.tsx web/src/App.tsx
git commit -m "feat: a reply box that cancels copy mode and writes bytes to the pane"
```

---

### Task 18: Bracketed paste — the measurement, then the code

**Files:**
- Modify: `web/src/components/ReplyBox.tsx`, `web/src/components/ReplyBox.test.tsx`, `README.md`

**This task is a measurement first.** Design open question 3 is open, and the thing it decides is whether this feature ships or is refused.

**What is already settled, and it is the dangerous half.** The wrappers never reach a program that did not ask for them: tmux mediates bracketed paste **client-side**, and a pane whose program has not set `DECSET 2004` receives the payload with the wrappers stripped rather than the literal characters `[200~` — measured both directions on 3.7b. There is no literal-`[200~` failure to design around.

**What is unmeasured is the agents.** Three programs, three answers, and the answer can differ between an agent's prompt and the shell behind it:

1. does it enable `2004` in its input box at all, and
2. does it honour the wrapping by treating the interior newlines as **text** rather than as **submit**?

**Why it matters more than anything else in this phase:** all three agents' input boxes treat a bare newline as *submit*, so pasting a twelve-line stack trace unwrapped **submits twelve turns to the agent, in order, with no way to stop it**.

**Step 1: Measure it, per agent, in your own throwaway tmux session.** For each of Claude, pi and opencode: start it in a pane on **your own socket**, and check whether the program has enabled bracketed paste (`tmux show-options -p -t %<id>` and the pane's own reported state, or by writing a wrapped multi-line payload and observing). Then write `ESC[200~` + a three-line payload + `ESC[201~` through the same PTY path the reply box uses, and record whether the three lines land as one message or as three turns. Use your own content — never anything from the owner's live panes; this repo is public.

Record all three answers in the commit message, whatever they are.

**Step 2: Implement against what you measured.**

- **If all three honour it:** a multi-line paste is wrapped in `ESC[200~` … `ESC[201~`, with the interior newlines intact and **no trailing CR**.
- **If any of them does not:** **refuse the paste and say why** — a worse feature and a far better outcome than twelve submitted turns. The refusal is a visible message in the box, naming the agent behaviour, not a silent drop.

The wrapping lives **in the browser**, not in Go. It is a property of what the *user's input widget* did (a paste event) and not of the pane; the daemon cannot tell a paste from typing, and inventing a flag so it could would be putting a UI fact on the wire.

**Step 3: Mutation testing** (whichever branch you took)

| Mutant | Killed by |
| --- | --- |
| Append a trailing CR after `ESC[201~` | the exact-bytes assertion — a trailing CR is the submit this whole feature exists to avoid |
| Wrap a **single-line** paste as well | a single-line fixture asserting no wrappers. Harmless in principle, but it makes the rule untestable if it applies to everything |
| Wrap typed input, not only pasted | drive the exported function from a `paste` origin and a `type` origin and assert they differ |
| Emit `ESC[200~` but not `ESC[201~` | the exact-bytes assertion |
| Refuse every paste (if you took the refusal branch) | a single-line fixture asserting it still goes through |

**Step 4: Commit** — with the three measurements in the message.

```bash
git status
git add web/src/components/ReplyBox.tsx web/src/components/ReplyBox.test.tsx README.md
git commit -m "feat: a multi-line paste is bracketed, or refused with a reason"
```

---

## Phase E — resume (design item 5a)

### Task 19: Resume here

**Files:**
- Modify: `internal/tmux/manage.go`, `internal/front/manage.go`, `internal/front/server.go`, `web/src/lib/manage.ts`, `web/src/components/AppSidebar.tsx` (the row menu), `README.md`
- Plus their tests

**The list of past sessions is out of scope and this is the whole of item 5.** What ships is **resume, driven by the agent**: a "resume here" action that opens a tmux window in the pane's directory running the agent's own resume command. One `new-window -c <path>`, a verb `internal/tmux/manage.go` already has (`NewWindow`, `manage.go:83`), with the path resolved daemon-side from `Row.Path` exactly as `SplitPane` already does through `panePath` (`client.go:342`) and `checkDir` (`manage.go:377`). **Zero parsers, zero disk reads, zero drift** — and the "works for sessions that predate installing anything" property is preserved, because it is the agent's own history.

**"Driven by the agent's own picker" is true for two of the three**, and the third changes what the control may promise:

| Agent | What "resume here" runs | What the user then sees |
| --- | --- | --- |
| Claude | `claude --resume` | the agent's own picker |
| pi | its resume equivalent — **open question 10, measure it** | the agent's own picker |
| opencode | `opencode --continue` | the most recent session in that directory, **with no choice** |

**opencode has no resume picker.** It takes `-c` / `--continue` for the most recent session and `-s <id>` for a named one, and its `session` subcommand only lists and deletes. So the control's label and the README **must not promise a picker on opencode**, and `-s <id>` is out of reach without the session list this design defers.

**Step 1: Measure pi's resume invocation. Do not take it from documentation.** In a throwaway directory with a throwaway `PI_CODING_AGENT_DIR`, never against the owner's own config: run pi's help and whatever subcommand it exposes, and establish the exact argv. Record it in the commit message. **If it cannot be established, ship Claude and opencode and leave pi's control absent** — an absent control is a gap, a wrong one runs the wrong command in the owner's repository.

**Step 2: Write the failing tests**

- the resume command per agent comes from **one table**, in Go, beside the agent list `internal/report` already holds — one source, one test, the same reason `internal/report` exists;
- a pane whose command is not a known agent has **no** resume action;
- the window is created with `-c <the pane's path>`, and the path is resolved daemon-side and `stat`ed first (the existing `checkDir` rule), so a directory that has been removed fails before `new-window`;
- **nothing is installed and no route writes executable code** — resume runs an agent already on the machine, through an existing verb behind existing middleware. Worth an assertion that the argv is a fixed table entry and never anything derived from a request body;
- the opencode entry's label does not contain the word "choose"/"pick"/"select" — pin it, so the promise cannot drift back in.

**Steps 3–4:** run, implement, run. Then run it by hand once per agent and look at the window it opens.

**Step 5: Mutation testing**

| Mutant | Killed by |
| --- | --- |
| Take the command from the request body instead of the table | the fixed-argv assertion. **The headline mutant**: it is the version that turns a management route into "run this for me" |
| Drop `-c` from `new-window` | the path assertion — the window opens in the daemon's cwd and the agent resumes the wrong project |
| Use the row's `Path` from the browser rather than resolving daemon-side | assert the handler ignores a path in the body |
| Skip the `checkDir` stat | a removed-directory fixture |
| Give opencode `--resume` | the per-agent table test, with all three entries asserted, not just one |
| Offer the action on a `zsh` pane | the unknown-agent test |
| Reword the opencode label to promise a picker | the label pin |

**Step 6: Commit** — with pi's measured invocation in the message.

```bash
git status
git add internal/tmux/manage.go internal/front/manage.go internal/front/server.go web/src/lib/manage.ts web/src/components/AppSidebar.tsx README.md <their tests>
git commit -m "feat: resume an agent's own session history in a new window here"
```

---

## Phase F — the phone (design item 6)

**v1 decision 5's stated reason is still correct and it is not what is broken.** shadcn's `Sidebar` "collapses to icons on desktop and becomes a `Sheet` drawer on mobile by itself. That satisfies the phone-as-a-bonus decision with no second layout to maintain." One layout **is** the right call and this phase keeps it. What that sentence covers is navigation, and navigation on a phone works. What it does not cover is everything shadcn does not do: the viewport under a keyboard, the safe area, touch targets, installability and the gesture layer. Those were never traded away — they were not considered, and the decision's second sentence has been read ever since as though it disposed of them.

**The measurable gap, today:** `h-svh` in exactly one place (`App.tsx:505`); no `dvh`, no `env(safe-area-inset-*)`, no `touch-action`, no `overscroll-behavior`, no `viewport-fit=cover`, no `interactive-widget`, no manifest, no service worker, no `navigator.setAppBadge`, no orientation handling, no pointer-coarse query. `web/index.html` is twelve lines and its viewport meta is `width=device-width, initial-scale=1.0`.

### Task 20: The device measurement session — a gate, not a formality

**Files:**
- Create: `docs/measurements/2026-XX-XX-phone.md` (or append a section to the design document — pick one and say which in the commit)
- **No production code in this task.**

**This task requires the owner, a real iPhone and a real Android.** It cannot be done by an agent alone and it cannot be simulated here: there is no iOS device, no Android device, and Playwright's Chromium `isMobile` emulation **has no soft keyboard at all** — which is exactly why the one phone-sized e2e test we have (`e2e/terminal.spec.ts:271`) asserts three-layer Radix focus-trap behaviour and nothing about the keyboard. **Nine borrowed constants that test green on a desktop Chromium is the precise shape of this project's stated failure mode.**

**How to run it.** Build a throwaway branch (not `main`) carrying a **temporary** instrumentation page — or a `?debug=keyboard` overlay in the app — that continuously prints `innerHeight`, `visualViewport.height`, `visualViewport.scale`, `visualViewport.offsetTop`, `screen.orientation.type`, and the computed `innerHeight − visualViewport.height`. Serve it over the real HTTPS path the device cookie needs. **Do not merge that branch**; the numbers are the deliverable.

**Ask the owner to do these, in this order, on each device, and record what the overlay printed:**

| # | Lead | What to do | What to record |
| --- | --- | --- | --- |
| 1 | `100dvh` is right on Chromium and wrong on iOS Safari; drive height from `visualViewport` | Open the reply box | Is the box above the keyboard? `visualViewport.height` with the keyboard open and closed. **Our situation differs from mtmux's**: we already use `h-svh`, the *small* viewport, which is the conservative one — it does not change when the URL bar collapses, which is why `App.tsx` uses it. What `svh` also does not do is shrink when the keyboard opens. So `svh` is right for the no-keyboard case and wrong for the keyboard case, and `visualViewport.height` is the only value right for both |
| 2 | `interactive-widget=resizes-content` makes Android usable and makes the naive keyboard-height calculation read ~0 | On Android, with and without the token in the viewport meta | `innerHeight − visualViewport.height` in both builds. The spelling matters: it is a **token inside the `content` string**, not a camelCase key, and it is **Chromium/Android only** — iOS Safari ignores it, which is why lead 1's `visualViewport` path is not made optional by it |
| 3 | A pinch-zoom reads as a ~400 px keyboard unless samples with `|scale − 1| > 0.05` are dropped | Pinch to read a stack trace in the terminal | `visualViewport.scale` and the apparent keyboard height at each pinch. **Is 0.05 the right threshold on these devices?** |
| 4 | Hysteresis: 120 px to open, 80 px to close | Scroll a long sidebar on Android | Does anything flap at the recorded thresholds? 120 is meant to clear Android's URL-bar collapse and sit under the shortest real keyboard — **measure both of those** |
| 5 | Orientation from `screen.orientation.type`, not from comparing width and height | Rotate **with the keyboard open** | Whether the viewport is wider than tall in portrait-with-keyboard. If it is, a width-vs-height test reports landscape and picks the wrong baseline, which makes lead 2 wrong as well |
| 6 | Open immediately, close debounced ~150 ms | Dismiss the keyboard | The intermediate heights iOS reports on the way down, and how long they last. **Is 150 ms enough?** |
| 7 | Geometry as CSS custom properties, never React state | — | Nothing to measure; it is settled and its reason is in the plan below |
| 8 | A tap must never be claimed by the gesture layer, and a claimed gesture's `touchend` must always be prevented | Tap a sidebar row, tap the reply box | Any 300 ms ghost click landing elsewhere |
| 9 | `touch-action: none` intersects with ancestors and cannot be given back to a descendant | — | Nothing to measure; it is a constraint on where the property may be set |
| 10 | `overscroll-behavior: contain` | Pull down on the terminal page | Whether pull-to-refresh fires. It costs a reload, a reconnect, a **new throwaway tmux session** and a full redraw — the most expensive accidental gesture available |
| 11 | The reply box laid over the terminal | Open the keyboard with the box focused | Whether the terminal element's box changes size, and by how much |

**Fold three more measurements into the same session, from other tasks:**

- **Open question 2** (Phase A): background the phone for an hour with a tab open, then wake it, and record how long it takes to produce a frame. That is what `LIVENESS_SLACK_MS`, `PROBE_TIMEOUT_MS` and `CONNECT_STALL_MS` are guesses about.
- **Open question 7** (Task 7): does the capture panel's copy-all work on iOS Safari, copying from memory inside the handler?
- **Whether the capture panel's `<pre>` is long-pressable** on both platforms, and whether the dialog opens without raising the keyboard (the `onOpenAutoFocus` prevention).

**Record what you could NOT establish as loudly as what you could.** A lead with no number stays a lead, and Tasks 22–24 must not write a constant for it. If the session cannot happen, **stop the phase here** — Tasks 21 and 25 do not depend on it, Tasks 22, 23 and 24 do.

**Commit**

```bash
git status
git add docs/measurements/2026-XX-XX-phone.md
git commit -m "docs: what a real iPhone and a real Android say about the nine leads"
```

---

### Task 21: Resize suppression — and it lands before anything that can change the terminal's box

**Files:**
- Modify: `web/src/components/Terminal.tsx`, `web/src/components/Terminal.test.tsx`, `web/src/App.tsx`

**This is the task that protects somebody else's terminal, and it does not depend on Task 20.**

The terminal's size is a tmux **window** property shared with every client viewing that window, and **a resize counts as acting**: v1 measured that a bare `SIGWINCH` makes a client the most recent one and drags the shared window to its dimensions, in both directions. So if opening the soft keyboard shrinks the terminal element, the `ResizeObserver` fires, the debounced resize goes out, and **the owner's local terminal is yanked to the size of a phone with a keyboard open** — v1's accepted limitation triggered by tapping a text box, which is a great deal more annoying than the co-viewing case it was accepted for.

**The load-bearing half is suppressing the resize path, not the layout.** With `interactive-widget=resizes-content` (Task 22) the *layout* viewport itself shrinks when the keyboard opens, so an element sized from the viewport shrinks whether or not anything is stacked above it. Laying the reply box **over** the terminal rather than above it is still right — it keeps the box out of the terminal's flow, so the box never takes height *from* the terminal as a sibling would — but it is the smaller half, and revision 1 claiming it was sufficient conflicted with its own lead 2.

**The predicate, and why it is this one.** Suppress while **an app-owned text control has focus**. That needs no keyboard detection at all, it is exactly the interval the keyboard is open on a phone, and it therefore lands cleanly **before** Task 20's numbers exist. Task 23 may widen it to "or the keyboard-open custom property is set", and only if the measurement shows focus alone is insufficient — for instance if a device closes the keyboard while focus stays.

`TerminalSession.noteResize` (`Terminal.tsx:488`) is where the suppression goes, not in the `ResizeObserver`: the observer keeps observing and the class keeps its own record of the size wterm reported, so that **when suppression lifts, the current size is sent once** rather than the pre-keyboard size being restored from a stale field.

**Step 1: Write the failing tests**

```ts
  it('sends no resize while suppressed', () => {
    // Fixture must not be true by accident: send one resize first and assert
    // it went out, so the suppressed one is a change from a known state.
  })

  it('sends the CURRENT size once when suppression lifts, not the one it had when suppression began', () => {
    // Two resizes arrive while suppressed. Exactly one frame goes out on lift,
    // and it carries the SECOND size. A version that queued would send two; a
    // version that dropped would send none; a version that restored would send
    // the first.
  })

  it('sends nothing on lift when the size did not change while suppressed', () => {
    // `#sentCols`/`#sentRows` already do this and it must keep working.
  })
```

**Steps 2–4:** run, implement, run.

**Step 5: Mutation testing**

| Mutant | Killed by |
| --- | --- |
| Suppress the `ResizeObserver` instead of the send | `sends the CURRENT size once when suppression lifts` — the class never learned the new size, so it sends nothing or the old one. **The headline mutant** |
| Queue the suppressed resizes and flush them all | the same test's "exactly one frame" |
| Drop them and send nothing on lift | the same test |
| Invert the predicate | `sends no resize while suppressed` |
| Suppress permanently after the first focus | a lift-then-resize case |

**Step 6: Commit**

```bash
git status
git add web/src/components/Terminal.tsx web/src/components/Terminal.test.tsx web/src/App.tsx
git commit -m "feat: a focused text control suppresses the resize, so a phone keyboard cannot resize the owner's terminal"
```

---

### Task 22: The viewport meta, the manifest, and the app badge

**Files:**
- Modify: `web/index.html`, `web/src/lib/tabBadge.ts`, `web/src/lib/tabBadge.test.ts`, `README.md`
- Create: `web/public/manifest.webmanifest`, the icons derived from `web/public/favicon.svg`

**Depends on Task 20 for lead 2**, and on Task 21 having landed, because `interactive-widget=resizes-content` shrinks the layout viewport and therefore can change the terminal's box.

Two one-line changes to a twelve-line `web/index.html`:

```html
    <meta name="viewport" content="width=device-width, initial-scale=1.0, viewport-fit=cover, interactive-widget=resizes-content" />
    <link rel="manifest" href="/manifest.webmanifest" />
```

Manifest: `display: standalone`, `start_url: "/"`, `theme_color` from the existing token, icons derived from `web/public/favicon.svg`.

**A standalone launch costs the tab badge, and the Badging API is the answer — not Web Push, and not a line of apology in the README.** Two corrections to revision 1 that matter here:

1. **A manifest alone changes nothing.** A page opened in a browser tab still has its tab and its favicon; only a launch from the Home Screen in `display: standalone` loses them. Adding the manifest disables nothing.
2. **The choice is not "push, or lose the badge".** `navigator.setAppBadge(n)` / `clearAppBadge()` exists for exactly the installed case — no service worker, no subscription, no endpoint, no VAPID key, no server state, **nothing on the daemon at all**. It is one call in `tabBadge.ts` beside the two it already makes, taking the same count `tabBadge` already computes. On iOS 16.4+ it works for a Home Screen web app **once notification permission has been granted**, which is a prompt this app does not otherwise need and is its one real cost; on Android and desktop Chromium an installed app needs no prompt.

So: keep the title and the favicon for the tab case, add `setAppBadge` for the installed case, **feature-detect rather than branch on display mode**, and let both draw the same number computed once. `setAppBadge` appears nowhere in `web/src` today.

**Step 1: Write the failing tests.** `tabBadge.ts`'s existing tests are the model — the count logic is already pure and tested; this adds one sink.

- the badge count passed to `setAppBadge` is the **same number** `tabTitle` uses — assert the relationship on its own line, not two independent literals;
- a count of 0 calls `clearAppBadge`, not `setAppBadge(0)`;
- with `navigator.setAppBadge` absent, nothing throws and the title and favicon still update — **this is the assertion that matters**, because the feature detection is the whole safety of it;
- a rejected `setAppBadge` promise (iOS with permission refused) is swallowed and does not break the title update.

**Steps 2–4:** run, implement, run. Then **look at it**: `pnpm --dir web build` and load the built page, confirm the manifest parses in devtools and the icons resolve.

**Step 5: Mutation testing**

| Mutant | Killed by |
| --- | --- |
| Branch on `matchMedia('(display-mode: standalone)')` instead of feature-detecting | the absent-API test, if you write it as "the API is absent" rather than "we are in a tab". **The headline mutant**: display mode is not the question, the API's existence is |
| `setAppBadge(0)` instead of `clearAppBadge()` | the zero-count test |
| Compute the app-badge count separately from the title's | the relationship assertion |
| Drop the `.catch()` on the promise | the rejected-promise test |
| Drop `viewport-fit=cover` or the `interactive-widget` token | **nothing in vitest.** Assert the exact `content` string of the meta tag in a test that reads `web/index.html` from disk — the wire-contract tests already read files from disk under `tsconfig.test.json`, so the precedent exists |
| Write `interactiveWidget` as a camelCase key | the same string assertion |

**Step 6: Commit**

```bash
git status
git add web/index.html web/public/manifest.webmanifest web/public/icon-*.png web/src/lib/tabBadge.ts web/src/lib/tabBadge.test.ts README.md
git commit -m "feat: an installable app that badges its own icon, and a viewport that expects a keyboard"
```

---

### Task 23: Keyboard geometry as CSS custom properties

**Files:**
- Create: `web/src/lib/keyboard.ts`, `web/src/lib/keyboard.test.ts`
- Modify: `web/src/App.tsx`, `web/src/index.css`

**Gated on Task 20. No constant from leads 1–6 is written here without a number from that session.**

**Custom properties, never React state, and the reason is not rendering.** mtmux's reason is that the terminal re-renders underneath. **In this app the reason is sharper.** A React state change re-renders `App`, which changes the terminal element's box, which fires `Terminal`'s `ResizeObserver`, which sends a debounced resize — and a resize is what makes this tab the client that acted most recently, which takes the shared tmux window's size for every client watching it, **including the owner's local terminal** (v1, measured). So keyboard geometry in React state does not merely re-render; **it can resize a terminal in another room.** A custom property on the root element changes layout without a React render and without touching the terminal's box.

**The module is pure and the DOM is the sink.** Put the decision in `web/src/lib/keyboard.ts` as a reducer over samples:

```ts
export interface ViewportSample {
  innerHeight: number
  visualHeight: number
  scale: number
  orientation: 'portrait' | 'landscape'
  at: number
}

/**
 * Fold one sample into the keyboard state. Pure, so every lead below is a
 * table row rather than a thing you can only see on a device.
 */
export function nextKeyboardState(prev: KeyboardState, s: ViewportSample): KeyboardState
```

The five rules it encodes, each a row in the table test, each with its measured number from Task 20:

1. **A per-orientation baseline**, taken from `screen.orientation.type` and **never from comparing width and height** — with a keyboard open in portrait the viewport can be wider than it is tall, so a width-vs-height test reports landscape and picks the wrong baseline, which makes rule 2 wrong as well.
2. **Samples with `|scale − 1| > 0.05` are dropped.** A pinch reads as a ~400 px keyboard otherwise, and pinching to read a stack trace is a thing people do constantly in a terminal on a phone.
3. **Hysteresis**, open at one threshold and close at a lower one, so the layout does not oscillate on a scroll.
4. **Open immediately, close debounced**, because iOS reports intermediate heights on the way down and each one is a flash of an intermediate layout.
5. **The output is a number of pixels**, written to a custom property by the one impure function in the file.

**Step 1: Write the failing table test.** Every row is a sequence of samples and an expected state, and **every fixture number is a literal** — deriving a fixture from the threshold it is testing is the self-referential shape and those mutants would survive.

Rows, at minimum: a clean open; a clean close; a pinch mid-read that must not open; a scroll that crosses the open threshold downward and back and must not flap; a rotation with the keyboard open that must not re-baseline from the wrong orientation; an iOS-style descent through three intermediate heights that must produce **one** close, not three.

**Steps 2–4:** run, implement, run. Then have the owner look at it on the two devices — **the table test proves the reducer, not the platform.**

**Step 5: Mutation testing**

| Mutant | Killed by |
| --- | --- |
| Derive orientation from `innerWidth > innerHeight` | the rotation-with-keyboard row. **The headline mutant** and the one that silently corrupts rule 2 as well |
| Drop the scale filter | the pinch row |
| `> 0.05` to `>= 0.05` | a row at exactly 0.05 — add it, or the boundary is asserted off the boundary |
| One threshold instead of two (open == close) | the flap row |
| Close immediately instead of debounced | the three-intermediate-heights row, at "exactly one close" |
| Open debounced as well | a row asserting the open is on the first qualifying sample |
| Write the value into React state | **no unit test can see this.** A review check, plus one Playwright assertion that the custom property changes and the terminal element's `clientHeight` does not — write that assertion even though the emulator has no keyboard, by setting the property directly |
| Any threshold retargeted | its own row, with a literal fixture either side of it |

**Step 6: Commit** — with the Task 20 numbers each constant came from, in the message.

```bash
git status
git add web/src/lib/keyboard.ts web/src/lib/keyboard.test.ts web/src/App.tsx web/src/index.css
git commit -m "feat: keyboard geometry as custom properties, from measured thresholds"
```

---

### Task 24: The gesture and safe-area layer

**Files:**
- Modify: `web/src/index.css`, `web/src/App.tsx`, `web/src/components/ui/sidebar.tsx` (or a wrapper), `web/src/components/CapturePanel.tsx`

Three rules, and the third is a prohibition:

1. **`overscroll-behavior: contain`** on the sidebar sheet, the capture panel and any scrolling region. Pull-to-refresh on a page whose terminal is a live socket costs a reload, a reconnect, **a new throwaway tmux session** and a full redraw — the most expensive accidental gesture available.
2. **`env(safe-area-inset-*)`**, now that `viewport-fit=cover` is on (Task 22). Without it the reply box sits under the home indicator.
3. **`touch-action: none` intersects with ancestors and cannot be given back to a descendant.** So it must **never** be set on `body` or on `SidebarInset`, or the terminal's own scrolling and the reply box's caret placement die with it — in a way that looks like a wterm bug. Put the rule in a comment at the top of the CSS block, because this is a prohibition that only bites the next person.

And **a tap must never be claimed by the gesture layer, and a claimed gesture's `touchend` must always be prevented**, or a tap that opens a pane also fires a synthetic click somewhere else 300 ms later.

**Test story:** most of this is CSS, which vitest cannot see and Playwright can only partly see. Be honest about it:

- assert the CSS rules exist by reading `web/src/index.css` from disk and matching the selectors — a weak test, but it stops a silent deletion;
- assert **`touch-action: none` appears nowhere** in the codebase on `body`, `html` or `[data-slot="sidebar-inset"]`. That is a *prohibition* test and it is the strongest one available here;
- one Playwright test at 390×844 that a tap on a sidebar row selects that pane and nothing else receives a click.

**Mutation testing:** add `touch-action: none` to `body` and watch the prohibition test go red; delete the `overscroll-behavior` rule and watch the CSS-presence test go red; **and then check the prohibition test is not vacuous** by asserting it also passes on a file that legitimately sets `touch-action: none` on a leaf element.

**Commit**

```bash
git status
git add web/src/index.css web/src/App.tsx web/src/components/CapturePanel.tsx e2e/touch.spec.ts
git commit -m "feat: contain the overscroll, respect the safe area, and never claim touch above the terminal"
```

---

### Task 25: The service worker, in one shape only

**Files:**
- Create: `web/public/sw.js`, `web/src/lib/registerSW.ts`, and its test
- Modify: `web/src/main.tsx`, `README.md`

**This presses on v2 decision 6** — *"Notification is a tab badge, not Web Push"*, elaborated as *"No push, no service worker, no subscription."* **v2 rejected a service worker as a notification mechanism.** The sentence lives inside the paragraph about the tab badge and every reason around it is about push: subscriptions, endpoints, permission prompts, server-side state. A worker that precaches one self-contained offline page and does nothing else takes on none of that. **The rejection stands for what it rejected.**

The costs here are specific and each is a rule:

- **A worker is a cache that outlives a deploy, and this app ships its frontend inside the binary.** A stale worker serving a stale shell against a new daemon is the classic failure and it is **invisible**, because the page loads. So: **never cache `index.html`**, never cache `/api/*`, never cache `/ws`, `skipWaiting` + `clients.claim`, and **precache exactly one route**.
- **`/assets/` is behind `Auth.Protect`** in this app, and that route "must 404, never fall back to HTML". A worker caching assets would be caching authenticated responses into a store that **survives a revoke** — and v1 says revocation "must sever live connections", not merely fail the next request. The shell is not the data, so this is small, but it is a hole in a property v1 states absolutely. Hence: the worker precaches **one self-contained page** — inline CSS, inline SVG, no `/assets/*` — and uses it **only** as the navigation fallback when the network fails.
- **`/enroll` is excluded explicitly.** It is public and reads `location.hash`; a navigation fallback serving a cached page for it would be a bad failure. **Design open question 9**, and exclusion in the fetch handler is the design — *whether exclusion is enough needs checking against a real install*, so that check is step 4 of this task and its result goes in the commit message.

**Step 1: Write the failing tests.** The routing decision is pure; make it so:

```ts
/** What the worker does with one request. Pure, so the rules are a table. */
export function swRoute(url: URL, mode: 'navigate' | 'other'): 'network' | 'fallback-on-failure' | 'bypass'
```

Rows: `/` navigate → fallback-on-failure; `/enroll` navigate → **bypass**; `/enroll#token` navigate → bypass; `/api/snapshot` → bypass; `/ws` → bypass; `/assets/index-abc.js` → bypass; `/index.html` → bypass (never cached); an unknown path, navigate → fallback-on-failure.

**Steps 2–4:** run, implement, run. Then **install it for real** — build, serve over the real HTTPS path, install from a browser, revoke the device, and confirm: the app still fails auth; `/enroll` is not served from cache; a redeploy replaces the worker on the next load.

**Step 5: Mutation testing**

| Mutant | Killed by |
| --- | --- |
| Remove the `/enroll` bypass | the `/enroll` rows. **The headline mutant** and the open question this task carries |
| Cache `index.html` | the `/index.html` row, and confirm the manual redeploy check catches it too |
| Fall back for `/api/*` | the API row — a cached snapshot is worse than none |
| Drop `skipWaiting`/`clients.claim` | **nothing testable.** A review check plus the manual redeploy step; say so |
| Precache `/assets/*` | assert the precache list is exactly one entry, by length and by content |
| Fall back on **every** failure rather than navigations only | the `mode: 'other'` rows |

**Step 6: Commit** — with the real-install check's result in the message.

```bash
git status
git add web/public/sw.js web/src/lib/registerSW.ts web/src/lib/registerSW.test.ts web/src/main.tsx README.md
git commit -m "feat: a service worker that is a navigation fallback and nothing else"
```

---

## Phase G — the choice chips (design item 4b)

### Task 26: A row of chips on `blocked`

**Files:**
- Modify: `web/src/components/ReplyBox.tsx`, `web/src/components/ReplyBox.test.tsx`

**Gated on Phase F**, because a reply box is a soft keyboard by construction and this is the phone half of item 4.

**What `blocked` changes is one thing and it is additive.** When the visible pane is blocked and the snapshot carries a `question` with `choices`, the box grows a row of chips, one per choice, each sending that choice's number and CR. That is the phone case the item exists for — answering a numbered permission prompt **without a keyboard at all** — and it is **a shortcut, not a mode**: the box underneath is unchanged and still there when the chips are not.

Keyboard placement of the box comes with this task, using Task 23's custom properties: the box sits above the keyboard, laid **over** the terminal rather than above it in flow, with the safe-area inset from Task 24.

**Step 1: Write the failing tests**

- chips render only when the pane is `blocked` **and** `question.choices` is non-empty — and, crucially, **the box itself renders in all four states with and without chips**, which is the four-fixture presence test from Task 17 re-asserted here so that adding chips cannot have quietly gated the box;
- each chip sends **that choice's number and CR**, through the same `replyFrames` the box uses — so the `end-mode` frame precedes it exactly as it does for typed text;
- a `question` with no `choices` renders no chips;
- a choice whose number would be ambiguous (more than nine choices) — decide and pin it;
- the chips are laid out so they do not push the box off screen at 390 px wide.

**Steps 2–4:** run, implement, run. Then look at it on a phone, if the Task 20 session can be repeated; if not, say in the commit that the chips have been seen only in the emulator.

**Step 5: Mutation testing**

| Mutant | Killed by |
| --- | --- |
| Replace the box with the chips when blocked | the four-state box-presence test. **The headline mutant**: it is the "mode, not shortcut" failure, and it takes the typed reply away exactly when the four undetected Claude notifications need it |
| Send the choice text instead of its number | the frame-content assertion |
| Send the number with no CR | the same assertion — assert the exact bytes |
| Skip the `end-mode` frame for a chip | the ordered-frames assertion |
| Render chips when `agentState !== 'blocked'` but a stale `question` is present | a fixture with a question and a non-blocked state |
| Render chips for an empty `choices` array | that fixture |

**Step 6: Commit**

```bash
git status
git add web/src/components/ReplyBox.tsx web/src/components/ReplyBox.test.tsx
git commit -m "feat: one chip per choice, so a numbered prompt can be answered without a keyboard"
```

---

## Definition of done

- `make test` and `npx playwright test` pass, and `pnpm typecheck` **from the repo root** exits 0.
- Reading an agent's scrollback on a phone, and copying from it, needs no copy mode and does not drag the owner's local terminal through the history.
- A phone left in a pocket for an hour comes back to a live terminal, and the sidebar and the terminal agree about which pane the tab is on.
- Typing a sentence containing the letter `q` into a pane that is in copy mode delivers the sentence, whole, and leaves the owner's `choose-tree` alone if there is one underneath.
- A pane sitting in a git worktree shows its branch, and that chip is present whether the pane is blocked, working, done or idle — observed by hand at all four.
- A wedged mount costs one pane's branch and not the sidebar.
- "Resume here" opens the agent's own history, and the opencode control does not promise a picker.
- The soft keyboard on a phone does not resize the owner's terminal — **checked on a device, not in an emulator.**
- Every task's mutation step has been run and its survivors are recorded in that task's commit message, with why each survived.

## What this plan does not settle

Carried from the design's ten open questions, so that finding one unsolved later is a confirmation rather than a surprise.

- **`LIVENESS_SLACK_MS`, `PROBE_TIMEOUT_MS`, `CONNECT_STALL_MS`** (question 2) are guesses, and the slack is anchored at one end only because the traffic that would anchor the other end — the server's 20 s ping — is invisible to JavaScript. Task 20 is where they get a number.
- **Every one of item 6's nine leads** (question 1) is another project's constant until Task 20 runs. **Tasks 22, 23 and 24 are blocked on it**, and if the session cannot happen the honest outcome is that Phase F stops after Task 21.
- **Whether the three agents honour bracketed paste** (question 3) decides whether Task 18 ships wrapping or refusal. The tmux half is settled; the agents' half is not.
- **The name floor of 50 px** (question 4) is asserted from where "pane 3" stops being readable and has not been looked at. Task 13 step 0 looks. If it is wrong the group cap moves, not the slot rule.
- **`defaultCaptureLines = 1000`** (question 5) is sized against bytes per line, not against how far back a person actually scrolls to answer an agent.
- **How often a pane's directory changes** (question 6) is the premise the git cache rests on, measured once, on one machine, at 3 distinct paths for 12 panes.
- **Whether iOS Safari's clipboard rule is satisfied by copying from memory inside the handler** (question 7). The code is written so that it can be; nobody has held the device.
- **Whether the panel should offer the visible screen as a distinct choice** (question 8). 2.6 ms and 4 KB against 5.1 ms and 168 KB is a real difference on a phone on a train, and no control ships for it.
- **What a service worker does to the enrolment flow** (question 9). `/enroll` is excluded explicitly; whether exclusion is enough is a manual check in Task 25, not something the code can tell you.
- **pi's exact resume invocation** (question 10) is measured in Task 19 step 1, and if it cannot be established pi gets no control rather than a wrong one.

Two more that are not open questions but are worth naming, because they are limits rather than gaps:

- **Nothing in CI can see a soft keyboard.** Playwright's Chromium `isMobile` emulation has none. Everything in Tasks 22–24 that matters is a device check, and the plan says so rather than pretending a green suite covers it.
- **The active pane stays a shared tmux window property.** This batch adds two new occasions on which the app moves it — the capture panel's selector and a stale-socket wake — and both are v1's already-accepted limitation reached through a new door. The alternative in each case was worse and is argued in the design.
