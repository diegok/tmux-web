# tmux-web v3 Implementation Plan — agent-side reporting

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** An agent tells tmux-web what it is doing, instead of tmux-web guessing from pixels. Each supported agent runs a small integration that writes one tmux pane option; the daemon reads it in the poll it already makes, and prefers it over the screen classifier when it is fresh.

**Architecture:** Integrations write `@wterm_agent = "1;<state>;<unix-ms>[;<activity text>]"` on their own pane via a new `wterm-web report` subcommand — one Go binary so the sanitizer exists once. The snapshot poller's single `list-panes` fork gains a second `list-panes` command in the same invocation, tagged, reading that option through its own format string. A fresh report outranks `internal/tmux/state.go`'s churn classifier and suppresses the pane's `capture-pane` entirely; three evidence rules buy some of that capture back to check the two *resting* states against the screen while a client is connected.

**Tech Stack:** Go 1.26 (stdlib only), React 19 + shadcn/ui, vitest, Playwright. Integrations: one TypeScript file (pi), one JavaScript file (opencode), one `settings.json` block plus one shell line (Claude Code).

**Design document:** `docs/plans/2026-09-10-tmux-web-agent-reporting-design.md`, **revision 6**. Revision 5 is the version this plan was written against, pinned at commit `201fa3d`; revision 6 landed after it and moves no mechanism — it withdraws the exposure argument that revisions 1–5 used to justify the label rules, keeps the decisions, and **deletes the first-word reduction of shell commands** (Task 15 below carries the corrected version). **Read all of it before starting.** It is long because it records six revisions and three adversarial review rounds; the revision sections at the top are the fastest way to learn which plausible-looking simplifications have already been tried and are wrong.

**It supersedes part of v2.** `docs/plans/2026-09-10-tmux-web-v2-design.md` Decision 1 (the title as a description of current work) and v2's rule that `AgentState` is empty whenever no browser is connected. Everything else in v2 stands — the churn classifier, the blocked grammars, `finishedAt` on the server and `done` in the browser, and the rule that only a positive match sets `blocked`.

---

## Before you start

### The failure mode of this project is plausible reasoning that tests green and is false

Not bugs. Every defect in v1's 23 tasks was in the plan, not in the code written from it, and the ones this design records are all of the same shape:

- `capture-pane -S -8` was assumed to mean "the last 8 lines". On a 6-row pane it returns 14.
- A dialog detector was keyed on a box corner the real dialog does not contain. Every test passed.
- The whole v2 design rested on Claude rewriting its pane title as it works. It does not; the title is frozen at the first turn.
- Revision 2 of *this* design specified a rule-2 test as "`N` settled captures drop, `N-1` do not" — a test of a counter over a premise its own whitelist had made false.

Assume this plan contains some too. **If something looks wrong, say so rather than implementing it faithfully.**

Where a task rests on an external behaviour — tmux's, an agent's, a runtime's — the task says **"verify this by running it, do not assume"** and gives the command. Those steps are not optional and their output is not predictable from the docs.

### Mutation testing is the verification standard

Every task in this repo has been mutation-tested by its implementer, and **it has found a real gap every single time** — twelve vacuous tests so far. One of them was `expect(cls).toContain('disabled')` on a shadcn button, which passes unconditionally because the Tailwind class list contains `disabled:pointer-events-none`.

So every task below ends with a **mutation step naming specific mutants**, chosen against that task's own logic. Apply the mutant, run the named test, watch it go red, revert. A surviving mutant is a missing test, not a curiosity. Record the survivors in the commit message.

### Hard safety rules

These govern you as well as the code.

1. **Never run `pkill`, `killall`, or any pattern-matching process kill.** The owner has live tmux sessions with real work and three running coding agents in them. An agent once killed its own shell this way.
2. **Never run a mutating tmux command without `-L <your own socket>` and `-f /dev/null`.** The owner's `~/.tmux.conf` sets non-default options, so a test server started without `-f /dev/null` is not the server your test thinks it is. `internal/tmux/testutil`'s `Args()` already includes both — use it. Read-only commands against the live server (`list-panes`, `show-options`, `display-message`) are fine.
3. **Do not inspect or decompile the agents' binaries or bundled JS.** Everything this plan needs about an agent's events comes from its documentation or from running it and recording what arrives.
4. **Never copy text out of the owner's live panes.** This repo is public and those panes hold private client work. Every fixture is captured from *your own* content in *your own* isolated session, or masked.
5. **Do not run `prettier`.** None is configured, and it rewrites unrelated files — it has already happened once and had to be unpicked by hand. Match the surrounding style.
6. **Each task's implementer works in its own scratchpad subdirectory.** Nothing temporary goes in the repo, in `/tmp` directly, or in another task's directory.

### Run both suites after every task

`go test ./...` alone is not enough. The frontend carries a contract test that parses the Go `Row` struct out of `internal/tmux/snapshot.go` and compares its json tags against the TypeScript keys — including an exact field *count* — so a Go-side wire change turns the frontend suite red while Go stays green. That has already happened in this repo, and the frontend suite was red for four tasks before anyone ran it.

```bash
make test          # test-go (go test ./... -count=1 -race) then test-web (pnpm vitest run)
```

Per-package during a task: `go test ./internal/tmux/ -run TestName -v`, and `cd web && pnpm test <file>`.

**Typechecking is a third check, and neither of the other two performs it.** vitest does not typecheck, so the
suite stays green over TypeScript that will not build. Run `pnpm typecheck` **from the repo root** after any
change to a `.ts`/`.tsx` file -- it runs `tsc -b` inside `web/`, exits 2 on a type error and 0 when clean
(both verified).

**Do not run `npx tsc -b` from the repo root.** The root package has no TypeScript dependency, so npx resolves
a decoy package that prints *"This is not the tsc command you are looking for"* and **exits 0**. It compiles
nothing and reports success. Task 4's file list was missing two test files that build a complete `SnapshotRow`;
the suite passed because vitest does not typecheck, and the typecheck "passed" because it was not running --
two green checks and nothing looking. `cd web && npx tsc -b` is correct; `pnpm typecheck` from the root is
shorter and harder to get wrong.

### Six things here are counter-intuitive. Do not "simplify" them back

1. **The report is not a fourteenth snapshot field.** `Format`'s last slot belongs to `@wterm_label`, and the last slot is the only position layer 2 of the label hardening protects. A second unsanitized field in the middle of the record fails *worse* than the label ever did: a surplus separator shifts every field after it and the row parses successfully **with another pane's values in it**. The report gets its own `list-panes` command inside the same fork.
2. **A trailing `;` does not survive `set-option`, and `--` does not help.** tmux's own command parser eats it. So a state-only report is written `1;idle;<ts>` with three fields, and **the reader accepts three parts or four**. A reader demanding four rejects every Claude report.
3. **The bracket set in the format string must be copied from source, not from prose.** `labelField` at `internal/tmux/snapshot.go:108` holds a *literal* newline byte and a *literal* `0x1f` byte. Written as a two-character `\n`, it leaves real newlines alive **and** puts a literal `n` in the set, so every lowercase `n` in a benign value becomes a space (`SECOnD` → `SECO D`). `[[:cntrl:]]` is worse: the `:` terminates the modifier's pattern and the whole expression expands to `""` for *every* value, good ones included.
4. **`N_idle` and `N_blocked` are different numbers, and neither is ever written as a literal.** Both are expressed against `settleAfter` in code. A test hard-coding `3` or `4` passes straight through the next change to `settleAfter` — which is exactly how revision 3 shipped an off-by-one. **But writing a test entirely against the constant is the opposite failure and is just as silent:** if a test's fixtures *and* its assertions are both written against the constant a mutant retargets, retargeting moves both sides at once and the mutant survives — a test that cannot fail is not caution, it is a false kill waiting to be recorded. The rule that satisfies both: **build the fixture from the constant the mutant does not touch (`settleAfter`), assert against the one it does (`NIdle`, `NBlocked`, `workingTTL`, `lateRepaintDwell`), and where a constant has a stated *relationship* rather than a measured value, assert that relationship on its own line.** `internal/tmux/report_test.go:56` is the model — one line, `MaxActivity != MaxLabel`, doing what none of the cap assertions around it can do. Where there is no relationship to assert, only a budget (`MaxReportBytes`, `reportFutureSkew`), literal fixtures either side of the boundary are the pattern, and Task 2 says so where they live.
5. **Every write is fire-and-forget, and `wterm-web report` exits 0 unconditionally.** All three agents block on the hook: pi and opencode await handlers with no timeout at all, and Claude adds the full hook duration to the turn. A reporting integration that can stop an agent from working is worse than no reporting integration.
6. **`asyncRewake` is never set on any hook this project installs.** An `"async": true` command hook has its exit code ignored *including exit 2* — unless `asyncRewake` is also set, which is documented as the thing that adds "exit code 2 wakes Claude". Setting it would re-arm the one sharp edge the exit-0 rule exists to blunt.

### The turn-start invariant

Every integration **must write `working` at turn start** — Claude's `UserPromptSubmit`, opencode's `session.status busy`, pi's `input`. This is not a nicety and it is not only a keepalive: the three turn-end events are treated as *edges* (they write unconditionally, without reading the standing option) and that is only safe because a new turn writes a non-resting state before its turn end can fire. A turn that reaches its end with no `working` written before it has that turn's end **suppressed**, and loses that turn's badge. It gets its own test (Task 14).

### The traps, collected

Each is carried into the task that would hit it, and repeated here so you meet them once before you meet them under time pressure.

| Trap | Where it bites |
| --- | --- |
| A retyped `\n` in the bracket set kills every `n` and leaves newlines alive | Task 3 |
| `[[:cntrl:]]` blanks every value, benign ones included | Task 3 |
| A trailing `;` is eaten; a value of exactly `;` errors and **keeps the previous value** | Tasks 2, 3, 6 |
| Constants written as literals rather than against `settleAfter` | Tasks 7, 8, 21 |
| pi's `ctx.mode` is presence-coded and fails **closed**; Claude's `agent_id` and opencode's `parentID` are absence-coded and fail **open** | Task 15 |
| A turn end with no preceding `working` loses its badge | Task 14 |
| A rule-2 test that tests the counter over a false premise | Task 8 |
| Mid-turn stillness makes the classifier agree with an idle report before the turn ended | Tasks 7, 9 |
| The classifier's tests run on a clock that starts at `time.Unix(0, 0)`, so "now minus the zero value is obviously huge" is false — a duration guard needs an explicit zero check | Task 21 |
| A test fixture that does not exercise the mutant it names (`"01x"` against a `HasPrefix` mutant, an `ok` lookup of a key the mutant never produces) | Tasks 2, 3 |
| **A self-referential assertion**: fixture and assertion both written against the constant the mutant retargets, so changing it moves both sides and the mutant survives | Tasks 5, 7, 8, 21 |
| A boundary mutant (`>` against `>=`) asserted at a fixture that is not on the boundary — the two operators agree everywhere else | Tasks 5, 21 |
| A **presence** assertion (`if _, ok := m[k]; !ok`) where the mutant changes the **value**, on a map every key is already in | Task 6 |
| A one-pane tmux fixture against an option-scope mutant — tmux resolves `#{@wterm_agent}` pane → window → session → global, so a session-level `set` reads back through the *pane* format on every pane of the session | Task 6 |

---

## The shape of the work, and why it is in this order

Twenty-one tasks in seven phases. The ordering is driven by one constraint above all: **the reader must be able to defend itself before any writer exists.**

- **Phase A (1–3) builds the value and the transport.** Pure sanitizing and parsing first, then the format string and the batched read against a real tmux server. Nothing else can be tested honestly until a report can survive the round trip.
- **Phase B (4–6) makes the daemon read one.** Wire fields, then minimal precedence, then a `wterm-web report` skeleton that takes flags rather than payloads. **Task 6 is the first end-to-end demonstration**: one command in a tmux pane, and the row changes. Everything after it refines something that already works.
- **Phase C (7–10) is the evidence machinery** — the three rules, the rejection slot, and `finishedAt` derivation. It lands **before any integration is installable**, which is the whole reason for the ordering: between Task 6 and Task 10 a reported `idle` is believed without being checked, and the failure that would cause — a false `done` badge — is the one v2 spent most of its complexity avoiding. Task 6's writer is a manual command nobody leaves running; Phase F's writers report on every turn. The gap closes before the risk arrives.
- **Phase D (11) is the row in the browser**, so that everything built so far is visible to a person and not only to a test.
- **Phase E (12–15) gives the writer its brains** — recorded payloads first, then the event tables, the whitelist, the edge/re-assertion split and the subagent filters. All of it in Go, in `wterm-web report`, so that it is one table with one test rather than three integrations that drift.
- **Phase F (16–20) ships the three integrations and the installer.** The shared single-slot queue is tested first, as a pure TypeScript module, because today that logic has no test story at all.
- **Phase G (21) is the late-repaint dwell in `state.go`.** The design hands this to the plan as a task rather than an open question, and it is deliberately last: it is a v2 classifier defect this design *inherits*, it touches a file nothing else here touches, it gates nothing, and its constant is a number nobody has measured.

Every task is independently committable and reviewable. Where a task changes the wire, the TypeScript mirror moves in the same commit.

### Task list

| # | Task | One line |
| --- | --- | --- |
| 1 | The activity sanitizer | Escape sequences as sequences, then controls including C1, collapse, trim, rune-boundary cap at 128 |
| 2 | The report value | `ParseReport`: version, closed state set, decimal ms, three parts or four, 1 KiB cap, future timestamps discarded whole; empty text yields a three-part report, never an unset |
| 3 | The report format string and the batched read | `reportField` built from `labelField`'s own constants, two tagged blocks in one tmux fork, stdout parsed on its own terms and not gated on exit status |
| 4 | `activity` and `stateSource` on the wire | Two `Row` fields plus their TypeScript mirror and `rowsEqual` |
| 5 | Precedence and freshness | A fresh report wins and suppresses the capture; `working` expires against an injected clock; `blocked` and `idle` do not; the command check; the ordering filter |
| 6 | `wterm-web report`, skeleton | `--state`/`--text`, `$TMUX`/`$TMUX_PANE`, one `set-option`, exit 0 always. **First end-to-end demo** |
| 7 | Evidence rule 3, the idle verification window | `NIdle = settleAfter + 2`, the window closes at the verdict, and a capture-skipped pane is a first sight |
| 8 | Evidence rules 1 and 2, and the form registry | `blockedRules` holds several named forms per agent; rule 2 drops only when **no** registered form matches; `NBlocked = settleAfter + 1` |
| 9 | The rejection slot | One semantic for all three rules; a classifier `idle` does not clear one; only a strictly newer report does, and a delayed older write clears nothing |
| 10 | `finishedAt` and the authority switch | Derived statelessly from a resting report; report→classifier stamps nothing, classifier→report does |
| 11 | The row in the browser | `paneText` becomes question → label → activity → title → command; the label joins the first line when both exist; `stateSource` must not change a row's appearance |
| 12 | Recorded hook payloads as fixtures | One file per agent per event, captured from your own content; the two that matter most are a real `agent_settled` and a real `session.idle` taken while a subagent runs |
| 13 | The event tables and the `notification_type` whitelist | Twelve documented types plus invented ones; an unknown one writes **nothing**; every `blocked` entry names a registered form |
| 14 | Edge versus re-assertion | The criterion, the per-(event, state) table, one `show-options` read before a re-assertion write, and the turn-start invariant test |
| 15 | Activity text and the subagent filters | Basename for a path, a shell command whole, tool name alone; per-agent filters with their failure directions stated and tested |
| 16 | The single-slot queue | A pure TypeScript module with the spawn injected, under `web/`'s vitest |
| 17 | The pi extension | `session_start`, `input`, `tool_execution_start`, `ui_prompt_start`, `agent_settled` |
| 18 | The opencode plugin | `chat.message`, `session.status`, tool events, `permission.asked`, `todo.updated`, `session.idle` |
| 19 | The Claude hooks | Four hooks, `"async": true`, never `asyncRewake`, and a shell line that spawns and does not wait |
| 20 | `wterm-web install-integration` | Managed header with a schema line, a JSON merge that refuses what it does not recognise, and the `.gitignore` warning |
| 21 | The late-repaint dwell | `lateRepaintDwell` in `Observe`, against the constant, with the short-turn sibling test that keeps the wrong fix out |

### Which tasks depend on an unresolved open question

The design's open questions are open, and several of them are open because measuring them is work nobody has done rather than because nobody thought about them. **Do not invent answers.** Each row below says what to measure and what to do until somebody has.

| Task | Open question | Standing instruction |
| --- | --- | --- |
| 19 | **1 — `PreToolUse` fork volume under a burst.** Latency is off the critical path because the hook is async; what is unmeasured is process volume, and it decides a *trade*, not a threshold: the activity line and the disconnected-case keepalive against a 4.5-second faster blocked badge whenever somebody is watching | Ship `PreToolUse`. The state-only fallback is **deleting one entry** from the generated block, and Task 19 spells out both the measurement and the one-line retreat |
| 15, 19 | **9 and 11 — does a teammate, background-session or nested-CLI hook process carry `TMUX_PANE`, and is it its own or a stale one?** No `TMUX_PANE` and the whole class is out of scope for free. A stale one and it is the nested-CLI door, which no field test can close because both processes are legitimate roots | Measure it (Task 19, step 3). Write **no** guard on the strength of a guess. Record the answer in the task's commit message either way |
| 5 | **2 — the 60-second `working` window.** Nobody has measured inter-event gaps during real work. The case that matters is a single long tool call emitting no sub-events, with no client connected | One named constant, changeable by editing one line, with the measurement written beside it |
| 8 | **3 — `N_blocked`.** `N_idle` is measured and closed; `N_blocked` is a derived floor with an unmeasured value, and measuring it needs the real dialog screens question 10 is also waiting on | `settleAfter + 1`, expressed against `settleAfter`. **Do not carry `4` across from `N_idle`** |
| 13 | **10 — four screen captures the whitelist is waiting on.** An 88-turn, 3-agent run met none of them and did not provoke them, so nobody will capture one by accident | Those four types stay *ignored*. Each promotes the day somebody manufactures the screen **and writes a grammar** — a code change, not a data change |
| 18, 20 | **5, 6, 8 — opencode's global plugin directory, how often the todo rung is empty, nested project installs** | `--global` is **not** offered for opencode. The todo rung is written as rung 2 with the tool-call rung underneath it, which is correct either way |
| 15, 17 | **7 — what pi's `tool_execution_start.args` actually contains, per tool** | Task 12 captures it. The basename default — and whether pi hands a shell command over as one string or as a structured object — is written against what the fixture shows, not against the guess in the design |
| 21 | **13 — how late a late repaint can be.** Two observations, 5.0 s and 9.0 s, both on one claude prompt shape. A bound, not a distribution | `lateRepaintDwell = 15 * time.Second`, biased long, flagged in the comment as a guess of the same standing as the 60-second window |

---

## Phase A — the value and the transport

### Task 1: The activity sanitizer

**Files:**
- Create: `internal/tmux/report.go`, `internal/tmux/report_test.go`

This is the security-critical part of the whole feature, and it exists once, in Go, precisely so that three integrations cannot each have their own version of it.

**The design underspecifies one step and this task decides it.** The design's step 2 says "drop every remaining control character", and its step 3 says "collapse whitespace runs to one space". Those two compose badly: a newline is a control, so dropping it welds `line one` and `line two` into `line onetwo`, and step 3 then has nothing to collapse. `sanitizeLabel` (`internal/tmux/snapshot.go:249`) — which the design names as the model to reuse — turns a control rune into a **space** instead, and layer 1's tmux-side substitution does the same thing to the two record-breaking bytes, deliberately, so that a tampered value reads as tampered with rather than as a name somebody chose. So: **control runes become spaces, then whitespace runs collapse, then trim.** Put the `line onetwo` reason in the comment, or the next reader will "fix" it back to a drop.

**Order matters and the first step is not optional.** Removing the ESC byte alone leaves the literal `[31m` in the text — a thing that has already been observed in this project. Strip whole sequences first, then handle whatever bytes are left.

**Every control byte in this task is written as a Go escape, never as a literal byte in the file.** That is not a style preference: it is the discipline `labelField` follows, and a literal `0x1f` pasted into a source file is invisible in every diff it ever appears in.

**Step 1: Write the failing test**

```go
package tmux

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// The sanitizer as data. Every row is a trap somebody has already fallen into,
// here or in the design.
func TestSanitizeActivity(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		// Sequences go as sequences. Dropping the ESC byte alone leaves the
		// literal "[31m" behind, which has been observed in this project.
		{"csi colour", "\x1b[31mred\x1b[0m", "red"},
		{"csi cursor move", "a\x1b[2Kb", "ab"},
		{"osc title set, BEL terminated", "x\x1b]0;title\x07y", "xy"},
		{"osc, ST terminated", "x\x1b]0;title\x1b\\y", "xy"},
		// The two bytes that break a snapshot record. tmux substitutes both to
		// spaces on the way out; this is the layer that does not depend on that
		// pattern still compiling.
		{"unit separator", "EV\x1fIL", "EV IL"},
		{"a newline welds two words if it is dropped rather than spaced",
			"line one\nline two", "line one line two"},
		// C1, built from its code point rather than typed. tmux's own checks
		// elsewhere are byte-oriented and let these through, which validateLabel
		// already had to learn.
		{"c1 control", "a" + string(rune(0x9f)) + "b", "a b"},
		{"del", "a\x7fb", "a b"},
		// Collapse and trim, after the substitutions, or a line of controls
		// becomes a row of blanks.
		{"collapse and trim", "  a\t\t\tb  ", "a b"},
		{"nothing but controls", "\x1b[0m\x00\x1f\n", ""},
		{"benign text is untouched", "running go test ./internal/tmux", "running go test ./internal/tmux"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := SanitizeActivity(tc.in); got != tc.want {
				t.Errorf("SanitizeActivity(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSanitizeActivityBounds(t *testing.T) {
	// Runes, not bytes, and the rune has to be one the cap does not divide
	// evenly or the test is vacuous. MaxActivity is 128 and the star is 3
	// bytes, so a byte cap lands mid-rune.
	got := SanitizeActivity(strings.Repeat("✳", 4000))
	if n := utf8.RuneCountInString(got); n != MaxActivity {
		t.Fatalf("kept %d runes, want exactly %d", n, MaxActivity)
	}
	if !utf8.ValidString(got) {
		t.Fatal("truncation split a rune")
	}
	// Invalid UTF-8 becomes U+FFFD rather than riding onto the wire, exactly as
	// sanitizeLabel does it -- ranging over a string yields RuneError per bad
	// byte and WriteRune re-encodes it, so what the browser is told is what the
	// daemon holds. encoding/json would make the same substitution later, where
	// nothing bounds it.
	if got := SanitizeActivity("a\xffb"); got != "a�b" {
		t.Errorf("invalid UTF-8 = %q, want a U+FFFD in the middle", got)
	}
	// A 10 KiB value is bounded by the same cap. It cannot reach here from our
	// own writer; it can from anything else holding the tmux socket.
	if n := utf8.RuneCountInString(SanitizeActivity(strings.Repeat("x", 10<<10))); n != MaxActivity {
		t.Errorf("10 KiB input kept %d runes, want %d", n, MaxActivity)
	}
}
```

**Step 2: Run it, expect FAIL**

```bash
go test ./internal/tmux/ -run TestSanitizeActivity -v
```
Expected: `undefined: SanitizeActivity`, `undefined: MaxActivity`.

**Step 3: Implement**

```go
// MaxActivity bounds an agent's activity line, in runes.
//
// It matches MaxLabel because it is the same row: PaneLines' second line, whose
// width budget MaxLabel was chosen for. Deliberately not herdr's 80, which is a
// column budget for a different row -- copying a magic number is copying the
// answer to somebody else's question. Deliberately not MaxTitle's 256 either,
// and note that the units differ: MaxTitle is a *byte* cap, because a title has
// been through tmux's own OSC parser before we see it, and this has been
// through nothing. A smaller cap on the less trustworthy field is the right way
// round.
const MaxActivity = MaxLabel

// ansiSequence matches the escape sequences an agent's own strings carry.
//
// Whole sequences, not the ESC byte: dropping the byte alone leaves "[31m" in
// the text as literal characters, which has been observed in this project. The
// three alternatives are CSI (ESC [ ... final byte), OSC (ESC ] ... BEL or ST),
// and the two-byte Fe forms. Anything this misses is still a control byte when
// SanitizeActivity reaches it, so the worst failure is a stray "[" on a row and
// never a live escape sequence on the wire.
var ansiSequence = regexp.MustCompile(
	"\x1b\\[[0-9;?]*[ -/]*[@-~]" + // CSI
		"|\x1b\\][^\x07\x1b]*(?:\x07|\x1b\\\\)" + // OSC, BEL- or ST-terminated
		"|\x1b[@-Z\\\\-_]") // two-byte Fe

// SanitizeActivity makes an agent's activity line safe to put in a tmux option,
// on the wire, and in the DOM.
//
// sanitizeLabel is the model and the rules are deliberately its rules, with two
// differences, both of them because this is a machine-written field rather than
// a human-typed one:
//
//   - Escape sequences are stripped first, as sequences. A label is typed; an
//     activity line is built from an agent's own output, which carries colour.
//   - Whitespace runs collapse. A control rune becomes a space here exactly as
//     it does in sanitizeLabel, and exactly as it does in tmux's own
//     substitution -- dropping a newline instead would weld "line one" and
//     "line two" into "line onetwo" -- so a multi-line fragment arrives as a
//     run of spaces and has to be closed up.
//
// An empty result is a legitimate outcome and means a state-only report, NOT an
// unset option. See FormatReport.
func SanitizeActivity(s string) string {
	s = ansiSequence.ReplaceAllString(s, "")

	var b strings.Builder
	b.Grow(len(s))
	pending := false // a whitespace run waiting to become one space
	runes := 0
	for _, r := range s {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			// Never a leading space: `runes > 0` is the trim of the front, and
			// a run that reaches the end is simply never written, which is the
			// trim of the back.
			pending = runes > 0
			continue
		}
		if pending {
			if runes == MaxActivity {
				break
			}
			b.WriteRune(' ')
			runes++
			pending = false
		}
		if runes == MaxActivity {
			break
		}
		b.WriteRune(r)
		runes++
	}
	return b.String()
}
```

Imports: `regexp`, `strings`, `unicode`. No `TrimSpace` call is needed and none should be added — the two comments above are what replace it, and a trailing `TrimSpace` would make the rune count come out short of the cap for an input ending in whitespace.

**Step 4: Run it, expect PASS**

```bash
go test ./internal/tmux/ -run TestSanitizeActivity -v
```

**Step 5: Mutation testing**

Apply each, run the named test, watch it go red, revert. A survivor is a missing test.

| Mutant | Killed by |
| --- | --- |
| Drop the `ansiSequence` pass entirely | `csi colour` — the `[31m` survives |
| Replace it with `strings.ReplaceAll(s, "\x1b", "")` | same row. **This is the exact bug the step exists for** |
| `unicode.IsControl(r)` to `r < 0x20` | `c1 control` and `del` |
| Control runes dropped rather than spaced (`continue` without setting `pending`) | `a newline welds two words` |
| `runes == MaxActivity` to `runes > MaxActivity` | `TestSanitizeActivityBounds`, on the exact count |
| Cap in bytes (`b.Len() == MaxActivity`) | same, on the star repeat |
| `MaxActivity = MaxTitle` | same |
| Drop the collapse (write every space through) | `collapse and trim` |
| Drop the `runes > 0` guard, so a leading run writes a space | `collapse and trim` |
| Return `""` unconditionally | `benign text is untouched` — make sure that row exists before you start, because without it a great many mutants live |

**Step 6: Commit**

```bash
git add internal/tmux/report.go internal/tmux/report_test.go
git commit -m "feat: sanitize an agent's activity line once, in Go"
```

---

### Task 2: The report value

**Files:**
- Modify: `internal/tmux/report.go`, `internal/tmux/report_test.go`

```
@wterm_agent = "1;working;1789075200000;running go test ./internal/tmux"
                | |       |             '- activity text, may contain ";"
                | |       '- unix ms when the agent produced this
                | '- working | blocked | idle
                '- schema version
```

**The two things that will be got wrong here:**

1. **Three fields is a valid report and it is the common one.** tmux **strips a trailing `;` from an option value** — measured on 3.7b, and `--` does not help, because the `;` is eaten by tmux's own command parser rather than by a shell. A writer that emits `1;idle;<ts>;` gets `1;idle;<ts>` stored anyway, so a reader demanding four parts rejects **every Claude report and every turn-end report from all three agents**. The writer omits the separator; the reader accepts three parts or four.
2. **A value failing any shape check is discarded whole, never repaired.** In particular a timestamp more than a few seconds in the **future** is discarded rather than treated as stale: a resting `idle` derives `finishedAt` from its own timestamp, and a `finishedAt` in the future is a `done` badge the browser's `seen` marker can never catch up with.

**Step 1: Write the failing test**

```go
func TestParseReport(t *testing.T) {
	now := time.UnixMilli(1789075200000)
	const ts = "1789075200000"

	for _, tc := range []struct {
		name string
		in   string
		want Report
		ok   bool
	}{
		{"four parts", "1;working;" + ts + ";run go", Report{StateWorking, 1789075200000, "run go"}, true},
		// The common shape. A reader demanding four parts rejects every Claude
		// report there has ever been.
		{"three parts is a state-only report", "1;idle;" + ts, Report{StateIdle, 1789075200000, ""}, true},
		{"blocked", "1;blocked;" + ts + ";Approve?", Report{StateBlocked, 1789075200000, "Approve?"}, true},
		// Only the first three separators are structural.
		{"semicolons in the text survive", "1;working;" + ts + ";a;b;c", Report{StateWorking, 1789075200000, "a;b;c"}, true},
		{"the text is sanitized on the way in", "1;working;" + ts + ";\x1b[31mred\x1fx", Report{StateWorking, 1789075200000, "red x"}, true},

		{"unset", "", Report{}, false},
		{"two parts", "1;idle", Report{}, false},
		{"unknown version", "2;idle;" + ts, Report{}, false},
		{"empty version", ";idle;" + ts, Report{}, false},
		// The fixture must START WITH "1", or it does not exercise the mutant it
		// is here for: strings.HasPrefix("01x", "1") is false, so a prefix-match
		// mutant rejects "01x" exactly as correct code does and survives the
		// whole table. Schema 10 is the case this will really be: it is a
		// different schema and must not be read as this one.
		{"a version that merely starts with ours", "10;idle;" + ts, Report{}, false},
		{"a version with a suffix", "1x;idle;" + ts, Report{}, false},
		{"unknown state", "1;thinking;" + ts, Report{}, false},
		{"empty state", "1;;" + ts, Report{}, false},
		// A state differing only in case is not the state. The writer is ours;
		// a value that is not exactly what we write did not come from us.
		{"state case", "1;Idle;" + ts, Report{}, false},
		{"non-decimal timestamp", "1;idle;later", Report{}, false},
		{"zero timestamp", "1;idle;0", Report{}, false},
		{"negative timestamp", "1;idle;-5", Report{}, false},
		// strconv does NOT refuse this on its own: ParseInt accepts a sign
		// prefix for every base, exactly as Atoi does. What refuses it is the
		// digits-only loop in the implementation below. Measured, Go 1.26.
		{"timestamp with a plus", "1;idle;+1789075200000", Report{}, false},
		// Discarded whole, not treated as stale: a future finishedAt is a done
		// badge `seen` can never catch up with.
		{"far future", "1;idle;1789075999000", Report{}, false},
		// A second of clock jitter on the one machine involved is not an attack.
		{"a moment in the future is tolerated", "1;idle;1789075201000", Report{StateIdle, 1789075201000, ""}, true},
		// The pair above leaves reportFutureSkew free to be anything from one
		// second to thirteen minutes -- neither row moves when the constant is
		// retargeted anywhere inside that range, so neither pins it. Both rows
		// are correctly CONSTRUCTED against the constant and survive any
		// retargeting, which is why they say nothing about its value; and
		// unlike MaxActivity/MaxLabel there is no relationship to another
		// constant to assert instead. So the two rows below are literal
		// offsets from `now` either side of the boundary, and they are the only
		// thing that pins the number. Keep them literal; do not "simplify" them
		// back to expressions in reportFutureSkew.
		{"exactly the skew ahead is tolerated", "1;idle;1789075205000", Report{StateIdle, 1789075205000, ""}, true},
		{"one millisecond past the skew is discarded", "1;idle;1789075205001", Report{}, false},
		{"the past is fine -- freshness is not this function's job",
			"1;working;1000", Report{StateWorking, 1000, ""}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseReport(tc.in, now)
			if ok != tc.ok {
				t.Fatalf("ParseReport(%q) ok = %v, want %v (got %+v)", tc.in, ok, tc.ok, got)
			}
			if ok && got != tc.want {
				t.Errorf("ParseReport(%q) = %+v, want %+v", tc.in, got, tc.want)
			}
		})
	}
}

// The rune cap only applies to a field we have successfully parsed out, and
// nothing stops anything holding the tmux socket from storing a megabyte the
// daemon would then carry through every 1.5s poll.
func TestParseReportRefusesAnOversizeValue(t *testing.T) {
	now := time.UnixMilli(1789075200000)
	head := "1;working;1789075200000;"
	if _, ok := ParseReport(head+strings.Repeat("x", MaxReportBytes), now); ok {
		t.Fatal("a value over the byte cap must be discarded whole, before parsing")
	}
	// And the boundary is not off by one: a value at exactly the cap parses.
	// Both halves above are written against MaxReportBytes, so they hold for
	// any value of it -- retarget the constant at 128 or at 64 KiB and they
	// both still pass. They test the > against the >=, and nothing else.
	if _, ok := ParseReport(head+strings.Repeat("x", MaxReportBytes-len(head)), now); !ok {
		t.Fatal("a value at exactly the cap must still parse")
	}
	// So the size itself is pinned here, with literals, and this is the only
	// place it is. Nothing above pins MaxReportBytes = 1024 the way
	// TestSanitizeActivityBounds pins MaxActivity with `MaxActivity !=
	// MaxLabel`: there is no relationship to another constant to assert, only a
	// budget -- 1 KiB for an option value the daemon carries through every
	// poll, for every pane. Literal fixtures either side of it are the pattern
	// for a constant like that. Do not "simplify" them into MaxReportBytes.
	if _, ok := ParseReport(head+strings.Repeat("x", 1024), now); ok {
		t.Error("a 1048-byte value parsed: MaxReportBytes has been widened past 1 KiB")
	}
	if _, ok := ParseReport(head+strings.Repeat("x", 1024-len(head)), now); !ok {
		t.Error("a 1024-byte value was refused: MaxReportBytes has been narrowed below 1 KiB")
	}
}

func TestFormatReport(t *testing.T) {
	// A state-only report carries no trailing separator, because tmux would
	// strip it anyway and a reader written to expect it would then see three
	// parts where it wanted four.
	if got := FormatReport(StateIdle, 1789075200000, ""); got != "1;idle;1789075200000" {
		t.Errorf("state-only = %q, want no trailing separator", got)
	}
	if got := FormatReport(StateWorking, 1789075200000, "run go"); got != "1;working;1789075200000;run go" {
		t.Errorf("with text = %q", got)
	}
	// Text that sanitises to nothing is a state-only report, NOT an unset
	// option. Revision 1 of the design conflated those and thereby deleted the
	// whole Claude integration: Claude ships state-only, so every one of its
	// reports has an empty text field.
	if got := FormatReport(StateIdle, 1789075200000, "\x1b[0m\n"); got != "1;idle;1789075200000" {
		t.Errorf("empty-after-sanitising = %q, want a three-part report", got)
	}
	// Whatever it writes, it can read back. The writer checks its own shape
	// before the write, because a botched write does not clear a report: a
	// value of exactly ";" is refused by tmux with "empty value" and the option
	// KEEPS ITS PREVIOUS CONTENTS, which is the more dangerous of the two
	// outcomes -- a stale report preserved rather than a missing one.
	for _, text := range []string{"", "run go", "a;b", "trailing;", "✳ wide", "\x1b[31mred"} {
		v := FormatReport(StateWorking, 1789075200000, text)
		if _, ok := ParseReport(v, time.UnixMilli(1789075200000)); !ok {
			t.Errorf("FormatReport(%q) produced %q, which ParseReport rejects", text, v)
		}
	}
}
```

**Step 2: Run, expect FAIL** — `undefined: Report`, `ParseReport`, `FormatReport`, `MaxReportBytes`.

**Step 3: Implement**

```go
// AgentOption is the per-pane tmux option an agent's integration writes.
//
// Deliberately NOT @wterm_label. That option is the *user's* field: PATCH
// /api/panes/{id} writes it, and AppSidebar.tsx's own comment on paneText says
// a label "wins outright, including over a title a program is rewriting
// underneath it -- that is the whole point of having one". An integration
// writing it would put a program back underneath the one field defined as being
// above programs: a rename would survive until the agent's next tool call, the
// agent's report would survive until the next rename, and both features would
// look intermittently broken with neither at fault.
const AgentOption = "@wterm_agent"

// ReportVersion is the schema this daemon understands.
//
// A report carrying anything else is ignored and the pane falls back to the
// screen classifier, which is the correct degradation and costs nothing: the
// integration is a file the user installed once and may not have updated, and
// the daemon reading it may be newer or older than it.
const ReportVersion = "1"

// MaxReportBytes bounds the whole option value, before parsing.
//
// The MaxActivity rune cap only applies to a field we have already parsed out,
// and nothing stops anything holding the tmux socket -- the same uid that can
// drive tmux directly -- from storing a megabyte the daemon would then carry
// through every 1.5s poll.
const MaxReportBytes = 1024

// reportFutureSkew is how far ahead of the daemon a report may be dated.
//
// It covers the clock jitter available on a single machine, which is the only
// machine involved. Over it the report is discarded WHOLE rather than treated
// as stale: stale is the right answer for a working report and the wrong one
// for a resting one, because a resting idle derives finishedAt from its own
// timestamp and a finishedAt in the future is a done badge no browser's `seen`
// marker can ever catch up with.
const reportFutureSkew = 5 * time.Second

// Report is one agent's statement about its own pane.
type Report struct {
	State     string // StateWorking, StateBlocked or StateIdle
	Timestamp int64  // unix ms; when the agent ENTERED this state, not when it wrote
	Activity  string // "" for a state-only report
}

// ParseReport reads an @wterm_agent value. ok is false for anything that is not
// a report this daemon wrote and understands -- including the empty value: an
// unset option and one set to "" both render as "" through #{@wterm_agent}, so
// the daemon cannot tell them apart and does not try. Both mean no report.
//
// Every field before the text has a shape that can be checked, and a value that
// fails any of those checks is discarded whole rather than repaired. A report
// we cannot parse is not a report we wrote.
func ParseReport(v string, now time.Time) (Report, bool) {
	if v == "" || len(v) > MaxReportBytes {
		return Report{}, false
	}
	// SplitN with 4: only the first three separators are structural, so the
	// text may contain semicolons freely.
	//
	// Three parts is valid and is the common case: tmux strips a trailing ";"
	// from an option value (measured on 3.7b; "--" does not help, because the
	// ";" is eaten by tmux's own command parser), so a state-only report is
	// stored as "1;idle;<ts>" however it was written.
	parts := strings.SplitN(v, ";", 4)
	if len(parts) < 3 || parts[0] != ReportVersion {
		return Report{}, false
	}
	switch parts[1] {
	case StateWorking, StateBlocked, StateIdle:
	default:
		return Report{}, false
	}
	// Digits and nothing else, checked before strconv sees it. strconv does NOT
	// do this for us: ParseInt accepts a sign prefix for every base, and Atoi
	// is literally ParseInt(s, 10, 0), so both spell "+1789075200000" as a
	// valid timestamp. Measured on Go 1.26. This loop is what actually holds
	// the rule that the only thing that parses is the only thing our own writer
	// emits, and it takes "-5" and the empty string with it.
	for i := 0; i < len(parts[2]); i++ {
		if parts[2][i] < '0' || parts[2][i] > '9' {
			return Report{}, false
		}
	}
	// ParseInt rather than Atoi for the explicit BIT SIZE, not for the base:
	// Atoi is ParseInt at the width of an int, and on a 32-bit build every
	// 13-digit unix-ms value is out of range there.
	ms, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || ms <= 0 || ms > now.Add(reportFutureSkew).UnixMilli() {
		return Report{}, false
	}
	r := Report{State: parts[1], Timestamp: ms}
	if len(parts) == 4 {
		// Re-sanitised on read, on the standing assumption that a writer's
		// promise is not a guarantee. This is the third of the same three
		// layers @wterm_label has: tmux's substitution takes the two
		// record-breaking bytes, the field's position as the only variable one
		// in its own format string bounds what a survivor could do, and this
		// repairs everything neither of those is a promise about -- C1
		// controls, a lone DEL, invalid UTF-8, and length.
		r.Activity = SanitizeActivity(parts[3])
	}
	return r, true
}

// FormatReport builds the value a writer stores.
//
// It never emits a value ending in a bare ";". Escaping it as "\;" would also
// work and is rejected: it puts a shell-shaped escape into a value that is not
// going through a shell, and the next person to read it will not know whether
// the backslash is data.
func FormatReport(state string, ms int64, activity string) string {
	v := ReportVersion + ";" + state + ";" + strconv.FormatInt(ms, 10)
	if a := SanitizeActivity(activity); a != "" {
		v += ";" + a
	}
	return v
}
```

**Step 4: Run, expect PASS.**

**Step 5: Mutation testing**

| Mutant | Killed by |
| --- | --- |
| `SplitN(v, ";", 4)` to `Split(v, ";")` with `len(parts) != 4` | `three parts is a state-only report` **and** `semicolons in the text survive` — two halves of one mistake |
| `len(parts) < 3` to `len(parts) < 4` | `three parts is a state-only report`. **Care about this one**: it is the shape revision 1 of the design shipped |
| Drop the version check | `unknown version` |
| `parts[0] != ReportVersion` to `!strings.HasPrefix(parts[0], ReportVersion)` | `a version that merely starts with ours` |
| Drop the state switch, or give it an accepting `default` | `unknown state`, `empty state`, `state case` |
| Drop the digits-only loop | `timestamp with a plus`. This is the row that refuses a sign prefix, and **strconv does not**: `ParseInt` accepts `+`/`-` for every base and `Atoi` is `ParseInt(s, 10, 0)` |
| `strconv.Atoi` in place of `ParseInt(..., 10, 64)` | **nothing — equivalent, and record it as equivalent rather than as a kill.** With the digits-only loop in front of it the two differ only in bit size, which is unobservable on a 64-bit build. It is still written as `ParseInt(..., 10, 64)`, for the 32-bit build where `Atoi` would reject every 13-digit unix-ms value |
| Drop `ms <= 0` | `zero timestamp`. (Not `negative timestamp` — the digits-only loop takes that one first, and both mutants have to be tried separately) |
| Drop the future check | `far future` |
| Future check without the skew (`ms > now.UnixMilli()`) | `a moment in the future is tolerated` |
| `reportFutureSkew` retargeted (1s, 13min, anything) | `exactly the skew ahead is tolerated` and `one millisecond past the skew is discarded` — the two literal rows. Every other future row is constructed against the constant and survives any retargeting |
| `MaxReportBytes` retargeted (128, 64 KiB) | the two literal rows in `TestParseReportRefusesAnOversizeValue`. The `MaxReportBytes`-relative halves survive it |
| Drop `MaxReportBytes`, **or apply it after parsing** | `TestParseReportRefusesAnOversizeValue`. The second form is the subtle one: it still truncates the text through `MaxActivity`, so a test asserting only on `Activity` would not see it |
| `len(v) > MaxReportBytes` to `>=` | the boundary half of the same test |
| Drop the `SanitizeActivity` call inside `ParseReport` | `the text is sanitized on the way in` |
| Drop the `if a != ""` guard in `FormatReport` | `empty-after-sanitising` — the mutant that puts a trailing `;` on the wire for tmux to eat |

**Step 6: Commit**

```bash
git add internal/tmux/report.go internal/tmux/report_test.go
git commit -m "feat: parse and format the @wterm_agent report value"
```

---

### Task 3: The report format string, and the batched read

**Files:**
- Modify: `internal/tmux/snapshot.go` (`formatFields`, `fieldCount`, `ParseRows`) and `internal/tmux/snapshot_test.go` (`rec`)
- Modify: `internal/tmux/report.go`, `internal/tmux/report_test.go`
- Modify: `internal/tmux/client.go`
- Create: `internal/tmux/report_integration_test.go` (package `tmux_test`, real tmux)
- Create: `internal/tmux/report_internal_test.go` (package `tmux`, real tmux — it swaps `batchArgs`, which is unexported; `snapshot_label_internal_test.go` is the precedent for both)

**The report is not a fourteenth snapshot field, and this is the task where somebody will try to make it one.** `Format`'s last slot belongs to `@wterm_label`, and the last slot is the only position layer 2 of the hardening protects. A second unsanitized field in the *middle* of the record fails worse than the label ever did: a surplus separator at index *k* shifts every field after it, the greedy last field absorbs the overflow, and the row **parses successfully with another pane's values in it**. `ParseRows` cannot detect that. So the report is read by a **second `list-panes`, with its own format string, in the same tmux invocation**.

**This does add a field to the snapshot record — but a constant one.** Both blocks arrive on the same stdout, and the design requires them to be told apart "by a literal tag field rather than by counting fields or by trusting the order", because a tag that is a constant in the format string is a tag nothing a writer controls can forge. So `formatFields` gains a literal `"S"` at index 0, `fieldCount` goes 13 → 14, and **every positional index in `ParseRows` shifts by one** (`fields[4]`, `fields[7]`, `fields[11]`, and the `fieldCount-1` rejoin). The existing fixtures do **not** change, because `rec` absorbs it:

```go
// rec builds one snapshot record from its fields, joined by the real separator.
// The block tag is prepended here rather than written into every fixture: it is
// a constant of the format, not data a test varies, and a suite that spelled it
// out 40 times would be 40 places to update.
func rec(fields ...string) string {
	return strings.Join(append([]string{snapshotTag}, fields...), Sep)
}
```

**Step 1: Verify the three tmux behaviours this rests on. Do not assume them.**

On your own socket, never the default one:

```bash
S=wterm-plan-t3
tmux -L $S -f /dev/null new-session -d -s probe -x 80 -y 24
tmux -L $S -f /dev/null set -p -t probe @wterm_agent '1;working;1789075200000;run go'

# (a) Does a lone ";" argv element separate two commands in one invocation?
#     The precedent is internal/tmux/session.go:34 (AttachArgs), which already
#     chains set-option calls this way.
tmux -L $S -f /dev/null list-panes -a -F 'S#{pane_id}' \; list-panes -a -F 'A#{pane_id}#{@wterm_agent}'
# expect: the S line(s), then the A line(s), in command order

# (b) Does a pane with the option UNSET still get a line?
tmux -L $S -f /dev/null split-window -t probe
tmux -L $S -f /dev/null list-panes -a -F 'A|#{pane_id}|#{@wterm_agent}'
# expect: a line per pane, the new one ending in "||" -- an unset option renders
# as the empty string, which is the same thing as no report

# (c) Does the FIRST command's output survive a failure in the second?
tmux -L $S -f /dev/null list-panes -a -F 'S#{pane_id}' \; list-panes -t nosuch -F 'A#{pane_id}'; echo "exit=$?"
# expect: the S block on stdout, the error on stderr, a nonzero exit

# (d) The trailing-semicolon measurement, because everything in Task 2 rests on it
tmux -L $S -f /dev/null set -p -t probe @wterm_agent 'a;' ; tmux -L $S -f /dev/null show -p -t probe -v @wterm_agent
# expect: a          (the ";" is stripped)
tmux -L $S -f /dev/null set -p -t probe @wterm_agent ';' ; echo "exit=$?"; tmux -L $S -f /dev/null show -p -t probe -v @wterm_agent
# expect: an "empty value" error, a nonzero exit, and the PREVIOUS value still there

tmux -L $S -f /dev/null kill-server
```

Record what each one actually printed in the commit message. If (a) or (c) disagrees with the expectation, **stop and say so** — the batching decision rests on them, and the fallback (two separate forks) is a design change, not an implementation detail.

**Step 2: Write the failing tests**

Pure, in `report_test.go`:

```go
// The report format string is built the same way labelField is, from the same
// two constants. It is not a fourteenth snapshot field: Format's last slot is
// the label's, and a second unsanitized field anywhere but last produces a row
// that parses successfully with another pane's values in it.
func TestReportFormat(t *testing.T) {
	if strings.Contains(Format, AgentOption) {
		t.Fatal("@wterm_agent must not be in the snapshot format string: the last " +
			"slot is @wterm_label's, and any other slot shifts the record")
	}
	// The option is the last and only variable field of its own format, so it
	// gets the same three layers the label has.
	if reportFormatFields[len(reportFormatFields)-1] != reportField {
		t.Fatal("the report must be the last field of its own format")
	}
	// Built from the constants rather than retyped: a bracket set retyped from
	// a rendered "\n" covers neither target byte and turns every lowercase "n"
	// into a space.
	if !strings.Contains(reportField, "\n") || !strings.Contains(reportField, Sep) {
		t.Fatal("reportField's bracket set must hold the real newline and the real separator")
	}
	if strings.Contains(reportField, `\n`) {
		t.Fatal("reportField contains a two-character backslash-n: that leaves real " +
			"newlines alive AND puts a literal 'n' in the set, so every 'n' in a " +
			"benign value becomes a space")
	}
}

func TestParseReports(t *testing.T) {
	// One batch output: the snapshot block, then the report block. Each parser
	// owns one tag and ignores the other's lines.
	out := strings.Join([]string{
		rec("work", "$0", "work", "%1", "0", "", "@1", "1", "win", "1", "claude", "t", ""),
		rec("work", "$0", "work", "%2", "1", "", "@1", "1", "win", "0", "zsh", "t", ""),
		reportTag + Sep + "%1" + Sep + "1;working;1789075200000;run go",
		reportTag + Sep + "%2" + Sep + "",
	}, "\n")

	rows, dropped, err := ParseRows(out)
	if err != nil || dropped != 0 || len(rows) != 2 {
		t.Fatalf("ParseRows over batch output = %d rows, dropped %d, err %v; the "+
			"report block must be skipped, not counted as malformed", len(rows), dropped, err)
	}
	reports := ParseReports(out)
	if got := reports["%1"]; got != "1;working;1789075200000;run go" {
		t.Errorf("reports[%%1] = %q", got)
	}
	// Every pane gets a line, including panes with no integration. An empty
	// value is the same thing as no report -- the design's revision 1 said "a
	// pane with no line in the second call has no report", which described a
	// case that does not occur.
	if got, ok := reports["%2"]; !ok || got != "" {
		t.Errorf("reports[%%2] = %q, ok=%v; want an empty value present", got, ok)
	}
	// The exact key set, and it has to be the key SET.
	//
	// `if _, ok := reports["S"]; ok` is the assertion this wants to be and it
	// cannot fail: drop the tag check from ParseReports and a snapshot line
	// splits SplitN(line, Sep, 3) into ("S", "work", <the rest>), so it is keyed
	// "work" -- parts[1] -- and never "S". The mutant it exists for survives it.
	if len(reports) != 2 {
		t.Errorf("reports = %v, want exactly two entries, for %%1 and %%2: a third "+
			"entry keyed by a snapshot line's SECOND field is what a missing tag "+
			"check looks like", reports)
	}
}

// ParseRows' own tag check, which the batch fixture above cannot see: with the
// reportTag `continue` sitting above it, a report line is skipped either way.
// What the check is really for is a line that is neither block, and the only
// way to produce one is to write it.
func TestParseRowsRefusesALineWithAnUnknownTag(t *testing.T) {
	// A record with the full fieldCount fields and the tag "X". It must be
	// counted dropped and must not become a Row -- otherwise the tag is not the
	// discriminator, the field count is, which is the thing this whole design
	// refuses to rely on.
}

// A report value that arrived with a separator in it -- which needs layer 1 to
// have failed open -- costs that one report and nothing else.
func TestParseReportsRejoinsASurplusSeparator(t *testing.T) {
	out := reportTag + Sep + "%1" + Sep + "1;working;1789075200000;a" + Sep + "b"
	if got := ParseReports(out)["%1"]; got != "1;working;1789075200000;a"+Sep+"b" {
		t.Errorf("got %q, want the value rejoined rather than truncated", got)
	}
	// ...and the daemon's own sanitizer is what makes it harmless.
	r, ok := ParseReport(ParseReports(out)["%1"], time.UnixMilli(1789075200000))
	if !ok || r.Activity != "a b" {
		t.Errorf("parsed %+v ok=%v, want the separator repaired to a space", r, ok)
	}
}
```

Real tmux, in `report_integration_test.go`:

```go
package tmux_test

// The benign fixture is the point of this test and it has TWO required
// properties, because two different mistakes hide behind a fixture without
// them:
//
//   - It contains a lowercase "n". A bracket set retyped from a rendered "\n"
//     puts a literal 'n' in the set, so every "n" in a perfectly good value
//     becomes a space ("SECOnD" -> "SECO D"). A fixture with no "n" in it
//     passes that mutant.
//   - It contains a real newline byte. The same mistake leaves real newlines
//     alive, which is what splits a record in two.
//
// And it asserts on the BENIGN value surviving, not only on the hostile one
// being cleaned: [[:cntrl:]] expands to "" for EVERY value, good ones included,
// and a hostile-input-only test passes that mutant too.
//
// The content is the implementer's own. Nothing in this repo's fixtures is ever
// copied out of a live pane: this repository is public and those panes hold
// private client work.
func TestReportFormatRoundTripsThroughRealTmux(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "probe", "-x", "80", "-y", "24")
	c := tmux.NewClient(srv.Args())

	const benign = "running go test in the second window\nnext line"
	const wantBenign = "running go test in the second window next line"

	srv.Run(t, "set", "-p", "-t", "probe", tmux.AgentOption, benign)
	_, reports, err := c.SnapshotAndReports(context.Background())
	if err != nil {
		t.Fatalf("SnapshotAndReports: %v", err)
	}
	// Exactly one pane, so the map has exactly one entry.
	for id, got := range reports {
		if got != wantBenign {
			t.Fatalf("pane %s: got %q, want %q -- every 'n' intact and the newline "+
				"a single space", id, got, wantBenign)
		}
	}

	// The hostile half. A separator and a newline both become spaces, so the
	// value reads as tampered with rather than as something somebody chose.
	srv.Run(t, "set", "-p", "-t", "probe", tmux.AgentOption, "EV\x1fIL\nMORE")
	_, reports, err = c.SnapshotAndReports(context.Background())
	if err != nil {
		t.Fatalf("SnapshotAndReports: %v", err)
	}
	for id, got := range reports {
		if got != "EV IL MORE" {
			t.Fatalf("pane %s: got %q, want both bytes substituted to spaces", id, got)
		}
	}
}

// Sibling to TestSnapshotHostileLabelCannotRemoveAPane. It should pass
// trivially, because @wterm_agent is not in Format at all -- and it is worth
// having precisely so that the day somebody appends it there, this goes red.
//
// Worth recording, because the batched read opens a direction the label
// hardening did not have to think about: the two blocks share one stdout, so if
// layer 1 ever failed open, a newline inside a hostile LABEL could forge a
// whole REPORT-block line -- "...\nA<Sep>%2<Sep>1;idle;<ts>" -- and thereby set
// another pane's state. It is not worth a code change: the value has to be
// written through the tmux socket, and anyone holding that socket can
// `set -p -t %2 @wterm_agent` directly with no forgery at all. It is worth
// writing down so nobody rediscovers it years from now and reads it as a hole.
// What bounds it is unchanged and is layer 1 plus layer 3: the substitution
// turns both record-breaking bytes into spaces, and ParseReport re-sanitises
// whatever arrives.
func TestHostileAgentReportCannotRemoveAPane(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "probe", "-x", "80", "-y", "24")
	srv.Run(t, "split-window", "-t", "probe")
	c := tmux.NewClient(srv.Args())

	before, _, err := c.SnapshotAndReports(context.Background())
	if err != nil || len(before) != 2 {
		t.Fatalf("baseline: %d rows, err %v", len(before), err)
	}
	srv.Run(t, "set", "-p", "-t", "probe", tmux.AgentOption, "X\x1fY\nZ\x1fW")
	after, _, err := c.SnapshotAndReports(context.Background())
	if err != nil {
		t.Fatalf("SnapshotAndReports: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("a hostile report changed the pane count: %d -> %d", len(before), len(after))
	}
	for i := range after {
		// The identity block must be byte-identical: the report is not in it.
		if after[i].PaneID != before[i].PaneID || after[i].Command != before[i].Command ||
			after[i].WindowID != before[i].WindowID || after[i].Title != before[i].Title {
			t.Fatalf("a hostile report changed a snapshot row:\n%+v\n%+v", before[i], after[i])
		}
	}
}

```

And one more against real tmux, in **`internal/tmux/report_internal_test.go`** — `package tmux`, not `tmux_test`, for the reason given below. The precedent for both the file and the technique is `snapshot_label_internal_test.go`, which is `package tmux`, imports `testutil` (no cycle), and drives a real server with one internal constant swapped out:

```go
package tmux

// A failure in the second command must cost the reports and never the sidebar.
// Measured in the design and re-measured in step 1: the first command's output
// is complete on stdout before the error, so the daemon parses stdout on its
// own terms and does NOT gate on the exit status. "Check the error first" is
// the reflex, and here the reflex trades a degraded feature for a blank
// sidebar.
//
// THE SEAM IS THE POINT OF THIS TEST. SnapshotAndReports takes no arguments and
// calls batchArgs() itself, so there is no way in from outside: a test that
// drove runKeepingOutput directly with a broken argv would assert that
// runKeepingOutput keeps its output -- which it plainly does -- and would NOT
// kill the mutant this test exists for, "SnapshotAndReports returns early on
// err != nil". So batchArgs is declared as a package-level var holding a func,
// and this test replaces it for the duration. That is the whole reason it is a
// var rather than a func; say so at the declaration.
//
// Not parallel, and it restores the var with t.Cleanup: it is process-wide
// state for as long as it is swapped.
func TestABrokenReportReadStillYieldsTheSnapshot(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "probe", "-x", "80", "-y", "24")

	orig := batchArgs
	t.Cleanup(func() { batchArgs = orig })
	batchArgs = func() []string {
		return []string{
			"list-panes", "-a", "-F", Format,
			// The report command, pointed at a target that does not exist.
			";", "list-panes", "-t", "nosuch", "-F", ReportFormat,
		}
	}

	rows, reports, err := NewClient(srv.Args()).SnapshotAndReports(context.Background())
	if err != nil {
		t.Fatalf("err = %v; a failed report read must not fail the poll", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want the snapshot block intact despite the nonzero exit", len(rows))
	}
	if len(reports) != 0 {
		t.Fatalf("reports = %v, want none", reports)
	}

	// The sibling that keeps the rule from becoming "ignore the error": with
	// NOTHING usable on stdout the error must come back. Kill the server and
	// re-run -- noServer() must still be the only silent path.
	// ...
}
```

**Step 3: Run, expect FAIL.**

```bash
go test ./internal/tmux/ -run 'TestReportFormat|TestParseReports' -v
go test ./internal/tmux/ -run 'Report.*Tmux|HostileAgentReport|ABrokenReportRead' -v
```

**Step 4: Implement**

In `snapshot.go`:

```go
// snapshotTag and reportTag label the two blocks of the one batched read.
//
// A literal constant in each format string, so nothing a writer controls can
// forge one: a report containing a newline would have to survive tmux's
// substitution first, and that substitution turns it into a space. Telling the
// blocks apart by counting fields or by trusting their order would both be
// guesses about a value somebody else writes.
const (
	snapshotTag = "S"
	reportTag   = "A"
)

const fieldCount = 14 // was 13; the block tag is field 0
```

`formatFields` gains `snapshotTag` as its first element. In `ParseRows`, every index moves up by one and the tag is checked first:

```go
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Split(line, Sep)
		// The other block of the batched read. Skipped rather than counted:
		// ParseReports owns those lines, and counting them as malformed would
		// log "skipped malformed rows" once per pane per poll forever.
		if len(fields) > 0 && fields[0] == reportTag {
			continue
		}
		if len(fields) < fieldCount || fields[0] != snapshotTag {
			dropped++
			continue
		}
		pidx, perr := strconv.Atoi(fields[5])
		widx, werr := strconv.Atoi(fields[8])
		...
	}
```

In `report.go`:

```go
// reportField is #{@wterm_agent} with the two bytes that break this wire format
// substituted out by tmux before the value reaches Go.
//
// It is labelField's pattern, built from the same two constants -- COPIED FROM
// internal/tmux/snapshot.go:108, NOT RETYPED FROM ANY RENDERING OF IT. Three
// ways to get this wrong, all measured:
//
//   - [[:cntrl:]] does not survive tmux's own parse: the modifier's variable is
//     introduced by ":", so the ":" inside the class ends the pattern early and
//     the whole expression expands to "" for EVERY value, including good ones.
//   - A range such as [\x0a-\x1f] compiles but depends on the locale's
//     collation order, and the tmux server's locale is whatever started it.
//   - A bracket set retyped from a rendered "\n" is two characters, so it
//     leaves real newlines alive AND puts a literal 'n' in the set: every
//     lowercase "n" in a benign value becomes a space.
//
// The Go source works because "\n" in a Go string literal IS the byte.
const reportField = "#{s/[\n" + Sep + "]/ /:" + AgentOption + "}"

// reportFormatFields is the second -F of the batched read. The option is the
// last and only variable field, so it gets all three of the label's defences;
// #{pane_id} is %N and cannot carry anything.
var reportFormatFields = []string{reportTag, "#{pane_id}", reportField}

// ReportFormat is the -F argument for the report block.
var ReportFormat = strings.Join(reportFormatFields, Sep)

// ParseReports pulls the raw @wterm_agent value of every pane out of the
// batched read, keyed by pane id.
//
// Every pane gets a line, including one with no integration, whose value is the
// empty string -- which is the same thing as no report. There is no dropped
// count and there should not be one: the blast radius here is a report, never a
// pane, so a line that will not parse costs one pane its state and nothing
// costs the sidebar a row.
func ParseReports(out string) map[string]string {
	reports := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		// SplitN with 3 for the same reason ParseRows rejoins its surplus: a
		// separator that survived the substitution belongs to the value.
		parts := strings.SplitN(line, Sep, 3)
		if len(parts) != 3 || parts[0] != reportTag {
			continue
		}
		reports[parts[1]] = parts[2]
	}
	return reports
}
```

In `client.go`:

```go
// runKeepingOutput is Run for a command whose stdout is worth having even when
// it fails.
//
// It exists for exactly one caller. SnapshotAndReports runs two commands in one
// invocation, and a failure in the second leaves the first's output complete on
// stdout -- so a nonzero exit there is a missing REPORT, not a missing
// snapshot, and discarding stdout would trade a degraded feature for a blank
// sidebar.
func (c *Client) runKeepingOutput(ctx context.Context, args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := c.command(ctx, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	out := strings.TrimRight(stdout.String(), "\n")
	if err != nil {
		if msg := strings.TrimRight(stderr.String(), "\n"); msg != "" {
			return out, fmt.Errorf("tmux %s: %w: %s", strings.Join(args, " "), err, msg)
		}
		return out, fmt.Errorf("tmux %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}

// Run executes a tmux command and returns trimmed stdout. (Unchanged contract:
// on failure it returns "" and the error.)
func (c *Client) Run(ctx context.Context, args ...string) (string, error) {
	out, err := c.runKeepingOutput(ctx, args...)
	if err != nil {
		return "", err
	}
	return out, nil
}

// batchArgs is the one tmux invocation the poller makes per refresh: the
// snapshot, then the reports, in command order.
//
// A lone ";" argv element is tmux's own command separator -- the same shape
// AttachArgs already uses. There is no shell here, so it needs no escaping.
//
// It is a var rather than a func for exactly one reason: SnapshotAndReports
// takes no arguments and calls this itself, so this is the only seam through
// which a test can make the SECOND command fail while the first succeeds. That
// is what TestABrokenReportReadStillYieldsTheSnapshot swaps, and without the
// seam the rule "parse stdout on its own terms, do not gate on the exit status"
// has no test that can see it. Nothing in production reassigns it.
var batchArgs = func() []string {
	return []string{
		"list-panes", "-a", "-F", Format,
		";", "list-panes", "-a", "-F", ReportFormat,
	}
}

// SnapshotAndReports returns one row per pane and every pane's raw
// @wterm_agent value, from a single tmux invocation.
//
// The marginal cost of the reports is zero forks: it is one more command inside
// a fork the poller already makes unconditionally, and against it the feature
// removes one capture-pane fork per reporting agent pane per poll. There is no
// poll at which it costs a fork it did not save.
func (c *Client) SnapshotAndReports(ctx context.Context) ([]Row, map[string]string, error) {
	out, err := c.runKeepingOutput(ctx, batchArgs()...)
	if err != nil && noServer(err.Error()) {
		return nil, nil, nil
	}
	rows, dropped, perr := ParseRows(out)
	if perr != nil {
		return nil, nil, perr
	}
	// The exit status is NOT the gate. A nonzero exit with a complete snapshot
	// block is a missing report; only a nonzero exit with nothing usable on
	// stdout is a failed poll.
	if err != nil && len(rows) == 0 {
		return nil, nil, err
	}
	if err != nil {
		slog.Warn("tmux batch: the report read failed; the snapshot is intact", "error", err)
	}
	if dropped > 0 {
		slog.Warn("tmux snapshot: skipped malformed rows", "dropped", dropped)
	}
	return Dedupe(rows), ParseReports(out), nil
}
```

`Client.Snapshot` stays exactly as it is. Nothing calls `SnapshotAndReports` until Task 5, deliberately: there is nowhere for a report to go until `Row` has the fields, and its own integration tests are what exercise it in the meantime.

**Step 5: Run both suites, expect PASS**

```bash
make test
```

The frontend suite must stay green here: no json tag changed, so the contract test is untouched.

**Step 6: Mutation testing**

| Mutant | Killed by |
| --- | --- |
| `reportField` rewritten with a two-character `\n` in the bracket set | `TestReportFormat`'s backslash-n check, **and** the real-tmux benign round trip (`running` loses both its `n`s) |
| `reportField` rewritten as `#{s/[[:cntrl:]]/ /:@wterm_agent}` | the benign round trip — every value comes back `""`. A hostile-only test survives this |
| `reportField` → bare `#{@wterm_agent}` | the hostile half of the round trip |
| Append `reportField` to `formatFields` and drop the second command | `TestReportFormat`'s first assertion, and `TestHostileAgentReportCannotRemoveAPane` |
| Put `reportField` before the label in `formatFields` | `TestFormatFieldCount`'s "label must be last" |
| `ParseRows` counts report lines as `dropped` | `TestParseReports`'s `dropped != 0` |
| `ParseRows` accepts a line without checking `fields[0] == snapshotTag` | `TestParseRowsRefusesALineWithAnUnknownTag`. The batch fixture cannot kill this one: the `reportTag` continue sits above the check, so a report line is skipped either way, and a report line is too short for `fieldCount` regardless. It takes a synthetic full-width record tagged `X` |
| `ParseReports` drops its `parts[0] != reportTag` check | `TestParseReports`'s **key-set** assertion — not an `ok` lookup of `"S"`, which passes under the mutant because the line is keyed `"work"` |
| `ParseReports` uses `Split` rather than `SplitN(…, 3)` | `TestParseReportsRejoinsASurplusSeparator` |
| `ParseReports` skips empty values (`if parts[2] == "" { continue }`) | `TestParseReports`'s `%2` present-and-empty assertion. **This is the tempting "cleanup"** and it is what makes "a pane with no report" and "a pane the batch did not mention" indistinguishable |
| `SnapshotAndReports` returns early on `err != nil` | `TestABrokenReportReadStillYieldsTheSnapshot` |
| `SnapshotAndReports` ignores `err` entirely, even with no rows | the same test's sibling: kill the server and assert an error comes back |
| `batchArgs` drops the `";"` | the round trip returns no reports |

**Step 7: Commit**

```bash
git add internal/tmux/report.go internal/tmux/report_test.go internal/tmux/report_integration_test.go \
        internal/tmux/report_internal_test.go \
        internal/tmux/snapshot.go internal/tmux/snapshot_test.go internal/tmux/client.go
git commit -m "feat: read @wterm_agent in the poll's own tmux invocation"
```

---

## Phase B — the daemon reads one

### Task 4: `activity` and `stateSource` on the wire

**Files:**
- Modify: `internal/tmux/snapshot.go` (`Row`)
- Modify: `web/src/lib/useSnapshot.ts` (`SnapshotRow`, `PaneNode`, `groupRows`, `rowsEqual`)
- Modify: `web/src/lib/useSnapshot.test.ts` (the contract test's field **count**, and its `row()` helper), `web/src/lib/tabBadge.test.ts`, `web/src/lib/manage.test.ts` (their `row()` helpers)

Two fields, nothing else. It is its own task because the frontend contract test parses the Go struct and asserts an exact field count, so this is the commit where Go and TypeScript move together — and because the next task has somewhere to put its answer.

**`StateSource` is on the wire mainly so that tests can see it.** v2 learned this the hard way with `finishedAt`: the blocked override made the row read `blocked` whether or not an edge had been stamped underneath, so a test asserting on the state alone could not see the bug at all. Precedence has exactly that property — a report and the classifier agreeing on `working` is indistinguishable from the precedence being backwards.

**Step 1: Write the failing test**

The Go side has no behaviour to test yet; the contract test is the test.

```ts
// web/src/lib/useSnapshot.test.ts, in the existing describe('contract with the daemon')
    expect(tags).toHaveLength(18)   // was 16
```

and `row()` gains `activity: ''` and `stateSource: ''`.

**Step 2: Run, expect FAIL**

```bash
cd web && pnpm test src/lib/useSnapshot.test.ts
```
Expected: the tag list has 16 entries, the keys do not match.

**Step 3: Implement**

`Row` gains, after `Title`:

```go
	// Activity is what the agent's own integration says it is doing. "" when no
	// integration is installed, when its report is stale, and for every pane
	// that is not an agent. Sanitised and capped on write and again on read.
	Activity string `json:"activity"`
	// StateSource is which authority decided AgentState: "event", "screen", or
	// "" when nothing did.
	//
	// It is on the wire mainly so that tests can see it. A report and the
	// classifier agreeing on "working" is indistinguishable from the precedence
	// being backwards, and a test that cannot distinguish them is a test that
	// will stay green through the rewrite that breaks it -- exactly as v2's
	// blocked override hid finishedAt.
	//
	// The UI may use it for a tooltip and must NOT branch the row's appearance
	// on it: two visibly different kinds of state dot teach the user to trust
	// one and ignore the other. That prohibition has a test of its own; see
	// Task 11.
	StateSource string `json:"stateSource"`
```

and in `state.go`, beside the state constants:

```go
// Which authority decided a pane's AgentState. "" means nothing did.
const (
	SourceEvent  = "event"  // the agent's own report, from @wterm_agent
	SourceScreen = "screen" // the churn classifier and the blocked grammars
)
```

TypeScript: `SnapshotRow` and `PaneNode` gain both fields, `groupRows` copies them into the tree, and **`rowsEqual` compares both** — it decides whether the previous tree object survives a poll, and React reconciles nothing when it does, so an activity line that changes while nothing else about the pane does would never reach the DOM.

**Step 4: Run both suites, expect PASS.**

**Step 5: Mutation testing**

| Mutant | Killed by |
| --- | --- |
| Drop `activity` from `rowsEqual` | needs its own test: two payloads differing only in `activity` must produce a **new** rows array. Add it — this is the vacuity trap, because every other assertion passes with the comparison missing |
| Drop `stateSource` from `rowsEqual` | the sibling of the above |
| Drop either field from `groupRows` | a tree test asserting the pane node carries it |
| `json:"activity"` renamed | the contract test |
| The count assertion left at 16 | it fails already; the point is that changing it to `tags.length` would make it vacuous forever. **Do not** write `expect(tags).toHaveLength(Object.keys(row()).length)` |

**Step 6: Commit**

```bash
git add internal/tmux/snapshot.go internal/tmux/state.go web/src/lib/useSnapshot.ts \
        web/src/lib/useSnapshot.test.ts web/src/lib/tabBadge.test.ts web/src/lib/manage.test.ts
git commit -m "feat: carry the agent's activity and the state's source on the wire"
```

---

### Task 5: Precedence and freshness

**Files:**
- Create: `internal/tmux/reports.go`, `internal/tmux/reports_test.go`
- Modify: `internal/tmux/poller.go`, `internal/tmux/poller_internal_test.go`
- Modify: `internal/front/server.go` (`newDaemon`, one line)

This is the task that makes a report mean something. The rules, from the design's precedence table:

| Report | Screen | Result |
| --- | --- | --- |
| fresh `working` | not taken | The report. **The capture is skipped, so this pane costs no forks at all** |
| fresh `idle` | not taken | The report (the verification window is Task 7) |
| fresh `blocked` | not taken | The report (the evidence rules are Task 8) |
| stale or absent | available | The classifier, exactly as v2 |
| stale or absent | no client connected | Empty, exactly as v2 |
| fresh, any state | no client connected | **The report.** Nothing to verify against, so nothing is verified |

That last row supersedes v2's rule that `AgentState` is empty whenever no browser holds a terminal socket. v2's reasoning — "stale state presented as current is worse than none" — was right *about a hash*: the classifier's memory is a claim that nothing has changed since we last looked, and we stopped looking. A report is different in kind: it is a fact the agent published and tmux is still holding.

**Four rules that are easy to get subtly wrong:**

1. **`working` expires; `blocked` and `idle` do not.** A short TTL is what you want for `working` — a crashed agent must not show as busy — and it is exactly wrong for a resting state, where the user is away, no client is connected, and "this one needs you" is the only thing the app is for.
2. **The ordering filter needs the accepted *report*, not just its timestamp.** The standing option holds the same value poll after poll, so "refuse anything not strictly newer" applied to the raw value would drop the pane's own state on the second poll. What the daemon keeps is the report it accepted; a standing value that is *older* than that (a delayed `working` landing after a `blocked` — ordinary scheduling jitter, not a broken integration) is refused and **the accepted one stays in force**.
3. **An unset or unparseable value clears the memory.** Otherwise `tmux set -p -u @wterm_agent`, the documented escape hatch for a stuck report, does nothing.
4. **A capture-skipped pane is not passed to `Retain`.** Its classifier entry is dropped, so the first capture whenever one is taken again is a **first sight**. Two reasons, and Task 7 rests on the second: `Observe`'s contract is "changed since the previous poll" at a fixed 1.5s interval, and a baseline from minutes ago answers a different question; and a retained baseline that differs sets `everChanged`, which is the flag licensing a `time.Now()` finish stamp.

**What dropping does NOT mean, and this is a bounded exposure the task accepts rather than a gap to close.** `delete(r.panes, paneID)` forgets the pane; it does not tombstone it. So a pane that goes `claude` → `zsh` → `claude` is a **first sight** for the relaunched agent, and the stale `@wterm_agent` value tmux is still holding — tmux options outlive the process that wrote them — is accepted on the terms every first sight is accepted on. **Do not build a tombstone to prevent that.** Three reasons, and the first is decisive on its own:

- A tombstone in `Reports` cannot deliver the property anyway. `classify` never reaches `Observe` for a non-agent pane (it `continue`s on `agent == ""`), and then calls `p.reports.Retain(agents)` — where the `zsh` pane is absent, so the entry *and* any tombstone beside it are deleted one line later. The unit test would be green and the deployed behaviour unchanged: this plan's own named failure mode.
- It is the same exposure the design already accepts and documents elsewhere. Task 9's restart case is identical — both slots empty, the standing report a first sight — and the answer there is not a tombstone either.
- What it costs is bounded and self-clearing: the relaunched agent's row shows the previous agent's resting state, with `finishedAt` derived from the **old** timestamp, until that agent's first turn start writes `working` (the turn-start invariant, which every integration has). With a client connected the `idle` also has to get past the verification window first. A device that had already seen that `finishedAt` does not re-badge, because the browser compares against the value it was shown. The escape hatch is `tmux set -p -u @wterm_agent`, which Task 5 already makes work.

The only complete fix is a writer-side one — the integration clearing the option as the agent exits — and no agent gives a shutdown event any of them can be trusted to deliver. Record this in the commit message; do not implement half of it.

**Step 1: Write the failing tests**

```go
// internal/tmux/reports_test.go
func TestReportsInForce(t *testing.T) {
	now := time.UnixMilli(1789075200000)
	r := NewReports()
	ms := func(d time.Duration) int64 { return now.Add(d).UnixMilli() }

	// A first sight is accepted: the daemon has no better information than the
	// value tmux is holding.
	got, ok := r.Observe("%1", FormatReport(StateWorking, ms(0), "run go"), "claude", now)
	if !ok || got.State != StateWorking || got.Activity != "run go" {
		t.Fatalf("first sight = %+v, %v", got, ok)
	}

	// The SAME value on the next poll is the same report, still in force. A
	// filter written as "strictly newer than the last accepted" applied to the
	// standing value would drop the pane's state on every second poll.
	if got, ok := r.Observe("%1", FormatReport(StateWorking, ms(0), "run go"), "claude", now.Add(1500*time.Millisecond)); !ok || got.State != StateWorking {
		t.Fatalf("re-reading the standing value = %+v, %v", got, ok)
	}

	// A newer one supersedes.
	if got, _ := r.Observe("%1", FormatReport(StateBlocked, ms(time.Second), "Approve?"), "claude", now.Add(time.Second)); got.State != StateBlocked {
		t.Fatalf("newer report = %+v", got)
	}

	// An OLDER one is refused and the accepted one stays in force. This is
	// ordinary scheduling jitter -- the writes are fire-and-forget, so a
	// working write from an earlier PreToolUse can land after a blocked write
	// from a later Notification -- not a broken integration.
	if got, ok := r.Observe("%1", FormatReport(StateWorking, ms(500*time.Millisecond), ""), "claude", now.Add(2*time.Second)); !ok || got.State != StateBlocked {
		t.Fatalf("a late older write = %+v, %v; want the blocked report still standing", got, ok)
	}
}

func TestReportsFreshness(t *testing.T) {
	now := time.UnixMilli(1789075200000)
	r := NewReports()
	w := FormatReport(StateWorking, now.UnixMilli(), "run go")

	// Written against the constant, never against 60. The number is a guess
	// (open question 2) and will change.
	//
	// The in-force assertion is at EXACTLY workingTTL, not one millisecond
	// short of it, and that is the whole point of it. `TTL - 1ms` is inside the
	// window under `> workingTTL` and under `>= workingTTL` alike, so a fixture
	// there cannot see the difference between the two operators and the `>=`
	// mutant survives it. At exactly the boundary `>` keeps the report and `>=`
	// expires it, and the fixture stays on the boundary whatever the constant
	// becomes. Found by mutation.
	if _, ok := r.Observe("%1", w, "claude", now.Add(workingTTL)); !ok {
		t.Fatal("a working report at exactly workingTTL must still be in force: the comparison is `>`, not `>=`")
	}
	if _, ok := r.Observe("%1", w, "claude", now.Add(workingTTL+time.Second)); ok {
		t.Fatal("a working report past the window must expire: a crashed agent must not show as busy")
	}

	// A resting state does not expire on a clock. The agent said it has
	// stopped, and by definition nothing further happens until the user acts --
	// which may be tomorrow, which is the case the app exists for.
	i := FormatReport(StateIdle, now.UnixMilli(), "")
	r2 := NewReports()
	if _, ok := r2.Observe("%2", i, "claude", now.Add(24*time.Hour)); !ok {
		t.Fatal("a resting idle report must not expire on a clock")
	}
	b := FormatReport(StateBlocked, now.UnixMilli(), "Approve?")
	r3 := NewReports()
	if _, ok := r3.Observe("%3", b, "claude", now.Add(24*time.Hour)); !ok {
		t.Fatal("a resting blocked report must not expire on a clock")
	}
}

func TestReportsAreDroppedWhenTheAgentIsGone(t *testing.T) {
	now := time.UnixMilli(1789075200000)
	r := NewReports()
	v := FormatReport(StateBlocked, now.UnixMilli(), "Approve?")
	if _, ok := r.Observe("%1", v, "claude", now); !ok {
		t.Fatal("setup")
	}
	// The command check: the pane is no longer running a known agent, so the
	// agent exited and the report is dropped unconditionally. This catches the
	// crash case, which is the case a clock was supposed to catch.
	if _, ok := r.Observe("%1", v, "zsh", now.Add(time.Second)); ok {
		t.Fatal("a report on a pane that is no longer an agent must be dropped")
	}
	// There is deliberately no third assertion here. See "What dropping does
	// NOT mean" below: a claude -> zsh -> claude pane IS a first sight, and a
	// first sight is accepted.
}

func TestAnUnsetOptionClearsTheMemory(t *testing.T) {
	now := time.UnixMilli(1789075200000)
	r := NewReports()
	if _, ok := r.Observe("%1", FormatReport(StateIdle, now.UnixMilli(), ""), "claude", now); !ok {
		t.Fatal("setup")
	}
	// `tmux set -p -u @wterm_agent` is the documented escape hatch for a stuck
	// report. If the daemon kept serving the last value it accepted, the escape
	// hatch would do nothing.
	if _, ok := r.Observe("%1", "", "claude", now.Add(time.Second)); ok {
		t.Fatal("an unset option must clear the report")
	}
	// A value we cannot parse means the same thing: a report we cannot parse is
	// not a report we wrote.
	r2 := NewReports()
	r2.Observe("%1", FormatReport(StateIdle, now.UnixMilli(), ""), "claude", now)
	if _, ok := r2.Observe("%1", "garbage", "claude", now.Add(time.Second)); ok {
		t.Fatal("an unparseable value must clear the report")
	}
}
```

And in `poller_internal_test.go`, the precedence table the design asks for — asserting `AgentState`, `StateSource` **and** `FinishedAt` together, because asserting the state alone cannot see a precedence bug at all:

```go
// A fresh report wins, and the capture is skipped -- which is the win. Two
// authorities running in parallel can only agree, in which case the second one
// was cost, or disagree, in which case we have already decided which wins.
func TestFreshReportSkipsTheCapture(t *testing.T) {
	var captured []string
	// ... build a poller whose Capture records its calls and whose
	// SnapshotWithReports returns one claude pane with a fresh working report.
	// Assert: AgentState "working", StateSource "event", Activity "run go",
	// and captured is EMPTY.
}

// With no client connected there is no screen to check, and the report is the
// only authority there is -- which is the case the app exists for.
func TestAReportStandsWithNoClientConnected(t *testing.T) { /* ... */ }

// A stale report hands the pane back to the classifier, and the authority
// switch stamps nothing (Task 10 tests the stamping half).
func TestAStaleReportFallsBackToTheClassifier(t *testing.T) { /* StateSource "screen" */ }

// The premise Task 7's arithmetic rests on, asserted at the poller: a pane
// whose capture was skipped is not passed to Retain, so its classifier entry is
// dropped and the next capture is a first sight.
func TestACaptureSkippedPaneIsNotRetained(t *testing.T) { /* ... */ }
```

**Step 2: Run, expect FAIL.**

**Step 3: Implement `reports.go`**

```go
// workingTTL is how long a transient working report stands without being
// re-asserted.
//
// working asserts that something is happening NOW and must be re-asserted; a
// crashed agent must not show as busy. blocked and idle are resting: the agent
// said it has stopped, and by definition nothing further happens until the user
// acts, so there is nothing to re-assert and they do not expire on a clock.
// That split is what dissolves the tension a single TTL cannot resolve.
//
// 60 seconds is a GUESS and is open question 2 in the design: nobody has
// measured the distribution of gaps between events during real work. Expiring
// early is the cheap direction but it is not free -- its cost is a false idle
// on a quiet screen, because a long silent tool call with no sub-events is
// exactly the gap this bounds, and v2's own caveat is that a quiet agent reads
// idle. What keeps that from being worse: an authority change stamps no
// finishedAt, so the row is wrong and the badge is not. What would settle the
// number: the inter-event gap distribution on all three agents, measured
// against a single long tool call emitting no sub-events, with no client
// connected.
const workingTTL = 60 * time.Second

// Reports decides, per pane, whether the standing @wterm_agent value is the
// authority for that pane's state.
//
// Like Classifier it is pure -- no clock, no I/O, `now` is a parameter -- and
// it is owned by the poll goroutine.
type Reports struct {
	panes map[string]*reportState
}

// reportState is the report currently in force for one pane.
//
// It holds the report and not merely its timestamp, and that is what makes the
// ordering rule implementable: the standing option holds the same value poll
// after poll, so "refuse anything not strictly newer" applied to the raw value
// would drop the pane's own state on every second poll. What is refused is a
// value OLDER than the one in force, and what stands in its place is this.
type reportState struct {
	accepted Report
}

func NewReports() *Reports { return &Reports{panes: make(map[string]*reportState)} }

// Observe reads one pane's standing @wterm_agent value and reports which
// report, if any, is in force for it.
//
// command is pane_current_command: if it is no longer a known agent the agent
// exited, and the report is dropped unconditionally. That catches the crash
// case, which is also the case a clock was supposed to catch.
func (r *Reports) Observe(paneID, raw, command string, now time.Time) (Report, bool) {
	if KnownAgent(command) == "" {
		delete(r.panes, paneID)
		return Report{}, false
	}
	parsed, ok := ParseReport(raw, now)
	if !ok {
		// Unset, or a value we did not write. Both mean no report, and both
		// must clear what we accepted -- `tmux set -p -u @wterm_agent` is the
		// documented escape hatch for a stuck report, and a daemon that went on
		// serving its own memory would make that escape hatch do nothing.
		delete(r.panes, paneID)
		return Report{}, false
	}
	st := r.panes[paneID]
	if st == nil {
		// A first sight. Accepted whatever it is: the option holds the last
		// write that landed and the daemon has no better information. Accepted
		// by this filter is not the same as believed -- a resting report still
		// has to earn its way past the evidence rules (Tasks 7 and 8).
		//
		// Including the first sight after claude -> zsh -> claude, whose value
		// the PREVIOUS agent in this pane wrote. That is deliberate and
		// bounded; a tombstone here would not survive Retain. See the task's
		// note on what dropping does not mean.
		st = &reportState{}
		r.panes[paneID] = st
	}
	if parsed.Timestamp > st.accepted.Timestamp {
		st.accepted = parsed
	}
	// Anything not newer is either the same report we already hold or an
	// out-of-order write; either way what stands is st.accepted.
	if st.accepted.State == StateWorking &&
		now.Sub(time.UnixMilli(st.accepted.Timestamp)) > workingTTL {
		return Report{}, false
	}
	return st.accepted, true
}

// Retain forgets every pane not in keep, on the same terms as Classifier.Retain.
func (r *Reports) Retain(keep []string) { /* same shape as Classifier.Retain */ }
```

**Step 4: Wire the poller**

`Options` gains one field and `NewPollerWith` gains one panic:

```go
	// SnapshotWithReports produces the rows AND, from the same tmux
	// invocation, every pane's raw @wterm_agent value keyed by pane id. Set
	// this INSTEAD of Snapshot to turn agent reporting on.
	SnapshotWithReports func(context.Context) ([]Row, map[string]string, error)
```

```go
	if (o.Snapshot == nil) == (o.SnapshotWithReports == nil) {
		// Half-wired reporting is silent: with neither there is nothing to
		// poll, and with both there is no way to say which fork happens.
		panic("tmux: exactly one of Options.Snapshot and Options.SnapshotWithReports must be set")
	}
```

`p.reports` is built beside `p.classifier`, in `NewPollerWith`, and for the same reason: it is the poll goroutine's private memory, and two pollers sharing one would answer each other's panes. A poller built without reporting still gets one — `reports[id]` on a nil map is `""`, which is no report, so the v1 path needs no branch of its own.

`classify` becomes:

```go
func (p *Poller) classify(ctx context.Context, rows []Row, reports map[string]string) {
	if p.classifier == nil {
		return
	}
	// Reports are read every poll whether or not anybody is watching: they cost
	// no fork of their own, and with no client connected a report is the only
	// authority there is. Only the captures are gated on a live client.
	connected := p.connected()
	if !connected {
		p.classifier.Retain(nil)
	}

	var agents, captured []string
	now := time.Now()
	for i := range rows {
		agent := KnownAgent(rows[i].Command)
		if agent == "" {
			continue
		}
		agents = append(agents, rows[i].PaneID)

		if rep, ok := p.reports.Observe(rows[i].PaneID, reports[rows[i].PaneID], rows[i].Command, now); ok {
			rows[i].AgentState = rep.State
			rows[i].Activity = rep.Activity
			rows[i].StateSource = SourceEvent
			if rep.State == StateIdle {
				// Derived, not stamped: no memory of a previous report, no
				// edge. Read the option, get the answer -- which is why it
				// survives a daemon restart. See Task 10.
				rows[i].FinishedAt = rep.Timestamp
			}
			// The capture is skipped entirely, and this pane is deliberately
			// NOT added to `captured`: see Retain below.
			continue
		}
		if !connected {
			continue
		}
		screen, err := p.capture(ctx, rows[i].PaneID)
		if err != nil {
			continue
		}
		captured = append(captured, rows[i].PaneID)
		blocked := IsBlocked(agent, screen)
		st := p.classifier.Observe(rows[i].PaneID, screen, now, blocked)
		rows[i].AgentState = st.State
		rows[i].FinishedAt = st.FinishedAt
		rows[i].StateSource = SourceScreen
		if blocked {
			rows[i].AgentState = StateBlocked
			rows[i].Question = ExtractQuestion(agent, screen)
		}
	}
	p.reports.Retain(agents)
	if connected {
		// `captured`, not `agents`. A pane the poller did not capture is not
		// passed to Retain, so its entry is dropped and the first capture
		// whenever one is taken again is a FIRST SIGHT, which Observe answers
		// with working and no comparison at all. Two reasons, and Task 7's
		// arithmetic rests on the second: a retained hash answers a different
		// question from the one Observe asks ("changed since the previous
		// poll", at a fixed interval), and a retained baseline that differs
		// sets everChanged -- the flag that licenses a time.Now() finish stamp.
		p.classifier.Retain(captured)
	}
}
```

`newDaemon` swaps one line: `Snapshot: tm.Snapshot` becomes `SnapshotWithReports: tm.SnapshotAndReports`.

**Step 5: Run both suites, expect PASS.**

**Step 6: Mutation testing**

| Mutant | Killed by |
| --- | --- |
| `parsed.Timestamp > st.accepted.Timestamp` to `>=` | nothing today — an honest survivor. Say so in the commit: two writes in the same millisecond cannot be ordered by a millisecond timestamp, and the design does not claim they can |
| `>` to `!=`, or dropping the comparison entirely | `TestReportsInForce`'s "a late older write" |
| Return `false` when the standing value equals the accepted one | `TestReportsInForce`'s "re-reading the standing value". **This is the mutant the rule is written to avoid** |
| Drop the `!ok` clear (`return st.accepted, true` on an unset option) | `TestAnUnsetOptionClearsTheMemory` |
| Drop the `KnownAgent` check | `TestReportsAreDroppedWhenTheAgentIsGone` |
| Expire resting states too (drop the `State == StateWorking` guard) | `TestReportsFreshness`'s 24-hour assertions |
| `now.Sub(...) > workingTTL` to `<` | the past-the-window assertion |
| `now.Sub(...) > workingTTL` to `>=` | the assertion at **exactly** `now.Add(workingTTL)`, and only that one. A fixture at `workingTTL - time.Millisecond` does **not** kill it: `TTL-1ms > TTL` and `TTL-1ms >= TTL` are both false, so both operators keep the report and the mutant survives. Exactly the boundary is the only point at which the two differ |
| `workingTTL` written as a literal `60` in the test | not a mutant — a **review check**. If any test in this task contains the number 60, reject it |
| The capture taken *before* the report is consulted | `TestFreshReportSkipsTheCapture`'s empty `captured`. Asserting only on the state cannot see this: both authorities say `working` |
| `p.classifier.Retain(agents)` instead of `Retain(captured)` | `TestACaptureSkippedPaneIsNotRetained` |
| `StateSource` set to `SourceEvent` on the classifier path | the precedence table |
| Reports consulted only when `connected` | `TestAReportStandsWithNoClientConnected` |

**Step 7: Commit**

```bash
git add internal/tmux/reports.go internal/tmux/reports_test.go internal/tmux/poller.go \
        internal/tmux/poller_internal_test.go internal/front/server.go
git commit -m "feat: a fresh agent report outranks the screen classifier"
```

---

### Task 6: `wterm-web report`, the skeleton — and the first end-to-end demonstration

**Files:**
- Create: `cmd/wterm-web/report.go`, `cmd/wterm-web/report_test.go`, `cmd/wterm-web/report_integration_test.go`
- Modify: `cmd/wterm-web/cli.go` (`usageText`, the dispatch switch, and the header comment)

After this task you can type one command into a tmux pane and watch the sidebar row change. Everything after it is refinement of something that already works.

**Two rules that are absolute here:**

1. **`report` exits 0. Always.** Every failure — no `$TMUX`, no `$TMUX_PANE`, tmux missing, the pane gone, unparseable stdin, a state we do not recognise — is a **silent no-op with status 0**. Running an agent outside tmux is not an error, it is a no-op. The precise hazard is narrow — for a Claude `PreToolUse` hook, exit code **2** specifically blocks the tool call, and every other nonzero code is a non-blocking error — but the distance between `exit 1` and `exit 2` is one character in a wrapper nobody will re-read, and a reporting integration that can stop an agent from working is worse than no reporting integration. **This deliberately breaks the CLI's own convention** that a malformed command line exits 2; say so in the comment.
2. **`report` talks to tmux, never to the daemon.** No socket, no auth, no dependency on tmux-web running. That is what makes the state survive a daemon restart, and it is why nothing here can ever wait on a network.

**`--state`/`--text` are permanent, not scaffolding.** Task 13 adds `--agent`/`--event` beside them — the form the integrations use, with the payload on stdin — and does not replace them: this pair is the manual form, the one the demo below uses, the one a user's own script can use, and the only form that exercises the write path without an event table in the way. Task 13 states how the two modes combine; nothing in this task needs to anticipate it beyond not designing them out.

**The server is addressed as `tmux -S "${TMUX%%,*}"`.** `$TMUX`'s first comma-separated field is the socket path, and a bare `tmux` can reach a different server than the one the agent is running inside. The pane is `-t "$TMUX_PANE"`, which held on all three agents. There is no alternative for Claude Code in particular: a hook's stdin is a socket and it has no controlling tty, so no tty-derived pane id is available.

**Step 1: Write the failing tests**

```go
// cmd/wterm-web/report_test.go
func TestTmuxTarget(t *testing.T) {
	for _, tc := range []struct {
		name, tmux, pane, wantSock, wantPane string
		ok                                   bool
	}{
		// $TMUX is <socket path>,<server pid>,<session id>. The first field is
		// the socket, and a bare `tmux` can reach a different server than the
		// one the agent is running inside.
		{"a real TMUX", "/tmp/tmux-1000/default,4242,0", "%3", "/tmp/tmux-1000/default", "%3", true},
		{"a socket with no commas", "/tmp/sock", "%3", "/tmp/sock", "%3", true},
		// Not an error: running an agent outside tmux is an ordinary thing to do.
		{"no TMUX", "", "%3", "", "", false},
		{"no TMUX_PANE", "/tmp/sock,1,0", "", "", "", false},
		{"an empty socket field", ",1,0", "%3", "", "", false},
		// The pane id reaches a command line, so it is validated with the same
		// rule everything else in this repo uses.
		{"a pane id that is not one", "/tmp/sock,1,0", "not-a-pane", "", "", false},
		{"a pane id with a flag in it", "/tmp/sock,1,0", "-x", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sock, pane, ok := tmuxTarget(func(k string) string {
				switch k {
				case "TMUX":
					return tc.tmux
				case "TMUX_PANE":
					return tc.pane
				}
				return ""
			})
			if ok != tc.ok || sock != tc.wantSock || pane != tc.wantPane {
				t.Errorf("got (%q, %q, %v), want (%q, %q, %v)", sock, pane, ok, tc.wantSock, tc.wantPane, tc.ok)
			}
		})
	}
}

// Exit 0, whatever happens. The one nonzero code that matters is 2, which
// blocks a Claude PreToolUse tool call; keeping every path at 0 means nobody
// has to remember which.
func TestReportAlwaysExitsZero(t *testing.T) {
	for _, args := range [][]string{
		{"report", "--state", "working"},        // no TMUX in the environment
		{"report", "--state", "nonsense"},       // not a state
		{"report"},                              // no state at all
		{"report", "--nosuchflag"},              // a malformed command line
		{"report", "--state", "idle", "extra"},  // a stray operand
	} {
		var out, errb bytes.Buffer
		if code := runReport(args[1:], &out, &errb, envWithout("TMUX"), dialReal); code != 0 {
			t.Errorf("%v exited %d, want 0 (stderr: %s)", args, code, errb.String())
		}
		if out.Len() != 0 {
			t.Errorf("%v wrote to stdout: %q -- stdout carries the answer and there is none", args, out.String())
		}
	}
}
```

And the end-to-end one, which is the point of the task:

```go
// cmd/wterm-web/report_integration_test.go
//
// Writer to tmux to reader, with nothing stubbed between them: the real
// subcommand writes the real option on a real tmux server, and the real
// batched read puts it on a real Row.
//
// Against testutil's own server, never the developer's. `report` in production
// addresses whatever $TMUX names, so a test that let $TMUX leak through from
// the environment it runs in would write into the developer's live session --
// which holds real work and running agents.
//
// TWO panes, deliberately, and the second one is load-bearing. `set` without
// `-p` writes a SESSION option, and tmux resolves #{@wterm_agent} up the
// hierarchy -- pane, then window, then session, then global -- so a
// session-level value shows through the PANE format on every pane of that
// session. Measured on an isolated socket: after
// `set -t probe @wterm_agent 'SESSIONLEVEL'`,
// `list-panes -a -F '#{@wterm_agent}'` printed SESSIONLEVEL for both panes. A
// one-pane fixture therefore cannot see the missing `-p` at all.
//
// And the assertion is on the VALUE, never on the presence of the key. Task 3's
// batched read gives every pane a line, empty value and all, so
// `if _, ok := reports[paneID]; !ok` is a check on something Task 3 guarantees
// unconditionally -- it holds for a pane that was never written to, and it held
// for the missing-`-p` mutant too.
func TestReportReachesTheSnapshot(t *testing.T) {
	agent := testutil.FakeAgent(t, "claude")
	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "work", "-x", "80", "-y", "24", agent)
	srv.Run(t, "split-window", "-t", "work", "-d", agent)
	panes := strings.Fields(srv.Run(t, "list-panes", "-t", "work", "-F", "#{pane_id}"))
	if len(panes) != 2 {
		t.Fatalf("setup: %d panes, want 2", len(panes))
	}
	paneID, otherPane := panes[0], panes[1]

	env := map[string]string{
		"TMUX":      srv.SocketPath() + ",1,0",
		"TMUX_PANE": paneID,
	}
	var out, errb bytes.Buffer
	// dialReal, because this test is the end-to-end one: a real client against
	// the real server testutil started.
	if code := runReport([]string{"--state", "working", "--text", "running go test"},
		&out, &errb, mapEnv(env), dialReal); code != 0 {
		t.Fatalf("report exited %d: %s", code, errb.String())
	}

	c := tmux.NewClient(srv.Args())
	rows, reports, err := c.SnapshotAndReports(context.Background())
	if err != nil {
		t.Fatalf("SnapshotAndReports: %v", err)
	}
	// The value, parsed. Not the key.
	rep, ok := tmux.ParseReport(reports[paneID], time.Now())
	if !ok || rep.State != tmux.StateWorking || rep.Activity != "running go test" {
		t.Fatalf("report for %s = %q -> %+v, %v; want a working report reading %q",
			paneID, reports[paneID], rep, ok, "running go test")
	}
	// The pane that was NOT written to must read empty. This is the half that
	// kills the missing-`-p` mutant: a session option shows through the pane
	// format on every pane of the session, so the mutant sets this one too.
	if reports[otherPane] != "" {
		t.Fatalf("the pane nobody reported on reads %q: the write was not scoped to a pane (`set` without `-p` sets a SESSION option, which tmux resolves through #{@wterm_agent} on every pane of the session)",
			reports[otherPane])
	}
	// And through the poller's precedence, which is what the sidebar sees.
	// ... build a poller over this server with Connected: func() bool { return false },
	// assert the row for paneID reads working / event / "running go test" with
	// NO client connected, which is the case the whole feature exists for --
	// and that the row for otherPane carries no agent state at all.
	_ = rows
}
```

**Step 2: Run, expect FAIL.**

**Step 3: Implement**

```go
// cmd/wterm-web/report.go

// report is what the three integrations run. It is the only subcommand that
// does not talk to the admin socket: the state lives in the pane, so a daemon
// restart loses nothing, and nothing here ever waits on a network.
//
// It exits 0 on every path, including a malformed command line -- which
// deliberately breaks this CLI's own convention that a usage error is exit 2.
// The reason is narrow and worth stating rather than generalising: for a Claude
// Code PreToolUse hook, exit code 2 specifically blocks the tool call (every
// other nonzero code is a non-blocking error and the action proceeds), and the
// distance between `exit 1` and `exit 2` is one character in a wrapper nobody
// will re-read. A reporting integration that can stop an agent from working is
// worse than no reporting integration.
func cmdReport(args []string, stdout, stderr io.Writer) int {
	return runReport(args, stdout, stderr, os.Getenv, dialReal)
}

// tmuxRunner is the little of tmux.Client this subcommand uses.
//
// Injected from the first version rather than retrofitted. Task 14 adds a
// `show-options` read before a re-assertion write and tests it by counting the
// reads and the writes a given event makes -- which needs a recording stub in
// this position, and a hardcoded tmux.NewClient(...) here would force that task
// to refactor this one. It takes no new parameter then; only the stub changes.
type tmuxRunner interface {
	Run(ctx context.Context, args ...string) (string, error)
}

// dialReal is the production dial: one client per socket, built after the
// environment has been read, because the socket comes from $TMUX.
func dialReal(socket string) tmuxRunner { return tmux.NewClient([]string{"-S", socket}) }

// runReport is cmdReport with the environment and the tmux client injected, so
// a test can drive it without setting process-wide variables -- which would
// race every other test in the package and, if $TMUX leaked through, would
// write into the developer's live tmux session.
func runReport(args []string, stdout, stderr io.Writer, getenv func(string) string, dial func(socket string) tmuxRunner) int {
	fset := newFlagSet("report", stderr, "wterm-web report --state working|blocked|idle [--text TEXT]")
	state := fset.String("state", "", "working, blocked or idle")
	text := fset.String("text", "", "what the agent is doing; omitted for a state-only report")
	if _, _, ok := parseFlags(fset, args); !ok {
		return 0 // see the comment on cmdReport
	}
	// Stamped from the moment this process starts, never from when set-option
	// returned. The timestamp means "when the agent entered this state", and
	// stamping at write time would reintroduce exactly the reordering the
	// daemon's ordering filter exists to undo.
	ms := time.Now().UnixMilli()

	switch *state {
	case tmux.StateWorking, tmux.StateBlocked, tmux.StateIdle:
	default:
		fmt.Fprintf(stderr, "wterm-web report: unknown state %q\n", *state)
		return 0
	}
	socket, pane, ok := tmuxTarget(getenv)
	if !ok {
		// Running an agent outside tmux is not an error, it is a no-op.
		return 0
	}
	value := tmux.FormatReport(*state, ms, *text)
	// The writer checks its own shape before the write. A botched write does
	// not clear a report: a value of exactly ";" is refused by tmux with "empty
	// value" and the option KEEPS its previous contents, which is the more
	// dangerous of the two outcomes.
	if _, ok := tmux.ParseReport(value, time.Now()); !ok {
		fmt.Fprintf(stderr, "wterm-web report: refusing to write a value it could not read back\n")
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), reportTimeout)
	defer cancel()
	// No "--": an option value is the second positional argument and tmux never
	// re-scans it for flags -- verified for @wterm_label in SetLabel, and the
	// same command.
	if _, err := dial(socket).Run(ctx, "set", "-p", "-t", pane, tmux.AgentOption, value); err != nil {
		fmt.Fprintf(stderr, "wterm-web report: %v\n", err)
	}
	return 0
}

// tmuxTarget resolves the server and the pane from the environment the agent
// handed its hook.
//
// $TMUX is "<socket path>,<server pid>,<session id>" and its first field is the
// socket: a bare `tmux` can reach a different server than the one the agent is
// running inside. $TMUX_PANE is present in every integration's environment on
// all three agents, and for Claude Code there is no alternative -- a hook's
// stdin is a socket and it has no controlling tty, so nothing tty-derived is
// available.
func tmuxTarget(getenv func(string) string) (socket, pane string, ok bool) {
	socket, _, _ = strings.Cut(getenv("TMUX"), ",")
	pane = getenv("TMUX_PANE")
	if socket == "" || tmux.ValidatePaneID(pane) != nil {
		return "", "", false
	}
	return socket, pane, true
}

// reportTimeout bounds the one tmux call. A set-option fork takes a couple of
// milliseconds; this exists so that a wedged tmux server makes the hook return
// rather than hold an agent's turn open. Every one of these events is delivered
// inside something the agent is waiting on -- pi and opencode await handlers
// with NO timeout at all -- so the integration also spawns and never waits.
const reportTimeout = 5 * time.Second
```

`cli.go` gains `case "report": return cmdReport(rest, stdout, stderr)`, a `usageText` line, and an amendment to the header comment, which currently says *"Everything but `serve` is a client of the admin socket, and the socket is the whole authorization argument"* — no longer true, and the exception is worth naming there rather than leaving the comment to be discovered as wrong.

**Step 4: Run, expect PASS.**

**Step 5: See it work, by hand**

Not against the developer's tmux server. Build the binary, start a throwaway server of your own, and run the daemon against it — the e2e harness (`e2e/harness.ts`) already does exactly this and is the shortest path to a browser:

```bash
make build
S=wterm-demo-t6
tmux -L $S -f /dev/null new-session -d -s work -x 120 -y 40
# a pane the daemon will treat as an agent: pane_current_command comes from the
# kernel's process name, which is the basename of the file that was exec'd
D=$(mktemp -d) && cp "$(command -v cat)" "$D/claude"
tmux -L $S -f /dev/null respawn-pane -k -t work "$D/claude"
P=$(tmux -L $S -f /dev/null list-panes -t work -F '#{pane_id}')
TMUX="$(tmux -L $S -f /dev/null display-message -p '#{socket_path}'),1,0" TMUX_PANE=$P \
  ./wterm-web report --state working --text "the first report this project ever read"
tmux -L $S -f /dev/null show -p -t $P -v @wterm_agent
# expect: 1;working;<13 digits>;the first report this project ever read
tmux -L $S -f /dev/null kill-server
```

Then the same value through the reader, which is what `TestReportReachesTheSnapshot` asserts automatically.

**Step 6: Mutation testing**

| Mutant | Killed by |
| --- | --- |
| Any path returning nonzero | `TestReportAlwaysExitsZero` — every row of it |
| `strings.Cut(getenv("TMUX"), ",")` taking the **last** field | `a real TMUX` |
| Using `getenv("TMUX")` whole as the socket | `a real TMUX` |
| Dropping the `ValidatePaneID` call | `a pane id that is not one`, `a pane id with a flag in it` |
| Dropping the `socket == ""` check | `an empty socket field` — tmux would then resolve `-S ""` to something |
| Dropping the state switch | needs its own assertion: after `report --state nonsense`, the option must be **unset**, not merely the exit code 0. Add it, or the mutant that writes `1;nonsense;<ts>` survives |
| Stamping `ms` after the tmux call returns | not observable in a unit test — a **review check**. The comment is the defence; if the `time.Now()` moves below the `Run`, reject it |
| Writing to stdout | `TestReportAlwaysExitsZero`'s stdout assertion |
| `set` without `-p` (a session option rather than a pane one) | `TestReportReachesTheSnapshot`, and **only its `reports[otherPane] == ""` half**. Not "the report never appears against the pane" — it does appear: tmux resolves `#{@wterm_agent}` up the hierarchy (pane, window, session, global), so a session-level value reads back through the *pane* format on **every pane of that session**. Measured on an isolated socket. The mutant is visible only as the value leaking onto the pane nobody reported on, which is why the fixture has two panes. A presence check (`if _, ok := reports[paneID]; !ok`) sees nothing either way — Task 3 puts a line on the map for every pane, empty value and all |

**Step 7: Commit**

```bash
git add cmd/wterm-web/report.go cmd/wterm-web/report_test.go \
        cmd/wterm-web/report_integration_test.go cmd/wterm-web/cli.go
git commit -m "feat: wterm-web report writes one pane option and always exits 0"
```

---

## Phase C — checking a resting report against the screen

A resting report makes a claim about the screen, and while the screen is available the claim is checked. We are never guessing at a state — the house rule that only a positive match sets `blocked` is untouched — we are checking a claim against evidence, and only evidence overturns it.

All three rules require a connected client. With no client there is no screen to check and the report is the only authority there is.

### Task 7: Evidence rule 3, the idle verification window

**Files:**
- Modify: `internal/tmux/reports.go`, `internal/tmux/reports_test.go`, `internal/tmux/poller.go`, `internal/tmux/poller_internal_test.go`

**A reported `idle` is dropped if the screen never settles**, over a window of `N_idle` polls after the report arrives. Within the window the pane is still captured. The report **stands** if the classifier says `idle` at any poll inside the window, and the window **closes at that verdict** — the captures stop rather than running out the count. It is **dropped** only if the classifier says `working` for the whole of it.

**`N_idle = settleAfter + 2`. This is measured, and it is also derived, and they are two different claims that happen to agree.**

- Derived: `Observe` returns idle only at the `settleAfter`-th *identical* comparison, so a window that must contain one repaint needs `settleAfter + 2` polls. `N_idle >= settleAfter + R + 1`, where `R` is the number of polls inside the window at which the capture differs from the one before it.
- Measured: 88 turns across the three agents, 44 counted from the agent's own turn-end event, 1,440 replays of the 1.5s grid at **every phase offset**. Polls-to-settle was **3 or 4 and never anything else**, and every turn on every agent had at least one phase at which it took 4. `R = 1`.
- The mechanism, which is why `settleAfter + 1` was sound reasoning on a false premise: **the turn-end event fires 7–52 ms *before* the agent's last repaint.** The report always comes first. A poll landing in that gap is spent on the pre-final screen, the repaint lands on window poll 2, and `settleAfter` then needs polls 3 and 4.
- The cost of `N = 3` is the invisible kind: about 1 true turn end in 50 discarded on claude and opencode, silently, because the badge merely falls back to the classifier's slower verdict.

**Do not pad it.** A longer window is not free caution: it accepts more mid-turn stillness as corroboration. On **4 of 88 measured turns** the screen sat still long enough *while the agent was waiting on the model* that the classifier reported `idle` **before that turn's turn-end event fired** — in one opencode turn at 31 of 60 poll phases, with a run of two consecutive `idle` verdicts. So an `idle` verdict inside the window is **corroboration, not verification**, and widening `N` trades one failure for another. This rule is a backstop with a measured leak, not a proof; the design says so and the comment must too.

**Step 1: Write the failing tests**

```go
// The measured fixture, and the constants are the point of it. A test spelling
// 3 and 4 out would pass straight through the next change to settleAfter --
// which is exactly how revision 3 of the design shipped an off-by-one.
//
// EVERYTHING here is built from settleAfter and asserted against settleAfter,
// and NIdle appears nowhere in this test. That is not style. NIdle is the
// constant the headline mutant retargets, so a fixture driven by NIdle and an
// assertion made against NIdle move TOGETHER when it is retargeted and the
// mutant survives them both: with NIdle = settleAfter + 1 the window rejects at
// poll 3, the fixture stops there, `captures == NIdle == 3`, green. Build from
// the constant the mutant does not touch; assert against the one it does. The
// model is TestSanitizeActivityBounds' `MaxActivity != MaxLabel` line
// (internal/tmux/report_test.go:56).
func TestIdleWindowSurvivesTheTurnEndRepaint(t *testing.T) {
	// Poll 1: a first sight of the PRE-FINAL screen. The turn-end event fired
	// 7-52 ms before the agent's last repaint, so the report is written to a
	// screen that is not yet final.
	// Poll 2: the repaint -- one changed capture.
	// Polls 3..settleAfter+2: identical.
	// The classifier says idle at poll settleAfter+2, the report stands, and
	// finishedAt is derived from the REPORT's timestamp, not from now.
	//
	// The fixture drives its screens off settleAfter -- repaint at poll 2, then
	// identical captures through poll settleAfter+2 -- and keeps feeding the
	// poller until the window closes on its own.
	...
	// settleAfter + 2, spelled out, NOT NIdle. This is the assertion that pins
	// the constant, and it can only pin it by being written in something else.
	if captures != settleAfter+2 {
		t.Fatalf("took %d captures, want settleAfter+2 = %d (NIdle is %d)", captures, settleAfter+2, NIdle)
	}
	// And this is the assertion that actually kills NIdle = settleAfter + 1:
	// the shortened window rejects the report at poll settleAfter+1, one poll
	// before the classifier settles, so the pane falls through to the
	// classifier and the row's finishedAt is no longer the report's own.
	// Confirm the kill from THIS line. An implementer who writes the count
	// assertion first and reads its green as a survival has learned nothing.
	if row.FinishedAt != reportTS {
		t.Fatalf("finishedAt = %d, want the report's own timestamp %d", row.FinishedAt, reportTS)
	}
}

// The same fixture one poll short of the window drops it. Written against
// settleAfter -- a loop over `settleAfter+1` polls -- and NOT over NIdle-1: a
// bound written as NIdle-1 tracks the very constant the headline mutant
// retargets, so it holds for settleAfter+1, settleAfter+2 and settleAfter+3
// alike and proves nothing on its own. This test is a sibling of the one above,
// not a substitute for it.
func TestAWindowOnePollShortWouldDropATrueTurnEnd(t *testing.T) { ... }

// A screen that changes at every poll for the whole window: dropped, no
// finishedAt derived, and the pane goes back to the classifier. That is the
// unfiltered-subagent case caught by evidence rather than by a discriminator
// holding -- which matters most on pi and opencode, whose turn-end filters are
// absence-coded and fail open.
func TestAnIdleReportOverAChurningScreenIsDropped(t *testing.T) { ... }

// The window closes at the VERDICT, not at the count. Asserted on the captures
// the poller makes and not only on the state it ends with: a report accepted at
// poll settleAfter+1 and one accepted at poll NIdle look identical from
// outside, and the difference is a capture-pane fork per poll per agent.
func TestTheWindowClosesAtTheVerdict(t *testing.T) {
	// A screen already still at the first window poll settles at settleAfter+1.
	if captures != settleAfter+1 {
		t.Fatalf("took %d captures, want %d: the window must close at the verdict", captures, settleAfter+1)
	}
}

// With no client connected there is nothing to verify, so the derivation is
// immediate -- which is the case the app exists for.
func TestWithNoClientTheDerivationIsImmediate(t *testing.T) { ... }

// The premise the arithmetic rests on: window poll 1 is a FIRST SIGHT, because
// a capture-skipped pane was never passed to Retain. So the settle count the
// window measures is the window's own -- `still` starts at 0 here -- and not a
// leftover from a baseline taken minutes ago, which is what NIdle = settleAfter
// + 2 is counting.
//
// Do NOT also assert that everChanged stays false through the window, and do not
// write that sentence into a comment: it is false in this very fixture. The
// repaint at window poll 2 differs from poll 1's capture, which is the whole
// point of the fixture, and a differing capture sets everChanged -- so at the
// settling poll the classifier does stamp its own finishedAt = now
// (state.go:123). That stamp is not what the row carries: while the report is in
// force the row's FinishedAt is the report's derivation, and the window closes
// at the verdict so no further capture is taken. It matters only if the report
// later leaves force, and then it dates the same turn end the report dated,
// within a poll of it. An implementer who asserts everChanged == false here will
// either go red against correct code or "fix" it by withholding the stamp --
// which is the blocked=true mutant that was in this table and has been removed.
func TestTheFirstWindowPollIsAFirstSight(t *testing.T) { ... }
```

**Step 2: Run, expect FAIL.**

**Step 3: Implement**

```go
// NIdle is the idle verification window, in polls.
//
// Expressed against settleAfter and never written as 4. Two independent
// arguments arrive at settleAfter + 2 and both are worth keeping:
//
// Derived: Observe returns idle only at the settleAfter-th IDENTICAL
// comparison, so a window that must contain one repaint needs settleAfter + 2
// polls -- N_idle >= settleAfter + R + 1, where R is the number of polls inside
// the window whose capture differs from the one before it.
//
// Measured: R = 1, in every one of 1,440 exact-timing replays of the 1.5s grid
// at every phase offset across 88 turns on three agents. Polls-to-settle was 3
// or 4 and never 5, and every turn on every agent had some phase at which it
// took 4. The mechanism is that the agent's turn-end event fires 7-52 ms BEFORE
// its last repaint -- the report always comes first -- so a poll can be spent
// on the pre-final screen.
//
// It is not padded, and that is deliberate. A wider window accepts more
// mid-turn stillness as corroboration: on 4 of 88 measured turns a screen sat
// still long enough while the agent waited on the model for the classifier to
// report idle BEFORE the turn ended. So the verdict this rule accepts means
// "the screen was still for settleAfter polls", not "the turn ended", and those
// are different claims. Widening N trades one failure for another.
const NIdle = settleAfter + 2
```

`reportState` gains the window:

```go
	// The idle verification window for `accepted`, reset whenever a newer
	// report is accepted.
	windowPolls int  // captures spent inside the window
	verified    bool // the classifier agreed at some poll inside it
```

```go
// NeedsScreen reports whether this pane must be captured despite a report being
// in force: only for a resting idle whose window is still open.
func (r *Reports) NeedsScreen(paneID string) bool

// Corroborate feeds one classifier verdict into the open window and reports
// whether the report survives.
//
// The window closes at the first idle verdict rather than running out the
// count: the report is accepted, the derivation happens, and the captures stop.
// It is dropped only when the classifier said working for the whole of it.
func (r *Reports) Corroborate(paneID string, idle bool) bool {
	st := r.panes[paneID]
	if st == nil {
		return false
	}
	st.windowPolls++
	if idle {
		st.verified = true
		return true
	}
	if st.windowPolls >= NIdle {
		r.reject(paneID, st.accepted.Timestamp) // Task 9 gives this its memory
		return false
	}
	return true
}

// Confirmed reports whether a resting idle report may derive finishedAt.
//
// With no client connected there is nothing to verify and the derivation is
// immediate, which is the case the app exists for. With one, it waits for the
// window: suppressed while pending rather than stamped and retracted, because a
// done badge that has landed on three devices does not un-land.
func (r *Reports) Confirmed(paneID string, connected bool) bool
```

In the poller's report branch, before setting the row: if `connected && rep.State == StateIdle && p.reports.NeedsScreen(id)`, capture, append to `captured`, `Observe(..., blocked=false)`, and `Corroborate(id, st.State == StateIdle)`; if that returns false, fall through to the classifier path for this pane. Otherwise set the row from the report, and set `FinishedAt` only when `p.reports.Confirmed(id, connected)`.

**Step 4: Run both suites, expect PASS.**

**Step 5: Mutation testing**

| Mutant | Killed by |
| --- | --- |
| `NIdle = settleAfter + 1` | `TestIdleWindowSurvivesTheTurnEndRepaint`, at its **`row.FinishedAt != reportTS`** assertion — the shortened window rejects the report one poll before the classifier settles, the pane falls through to the classifier, and the row's `finishedAt` stops being the report's own. **Confirm the kill from that line, not from the capture count**, and note that the count assertion only helps because it is written `settleAfter+2`: a count compared against `NIdle` would move with the mutant and go green. **The headline mutant**: it is the value revision 3 of the design shipped, and it discards about 1 true turn end in 50 |
| `NIdle = settleAfter + 3` (padding "for safety") | not killed by any test, and it should not be — say so in the commit. It is refused on the measurement, not on a test: a wider window accepts more mid-turn stillness as corroboration |
| `NIdle` written as the literal `4` | a **review check**: change `settleAfter` to 3 locally and re-run the package. If nothing goes red, a literal has crept in |
| `st.windowPolls >= NIdle` to `>` | `TestAWindowOnePollShortWouldDropATrueTurnEnd`'s sibling — add a churning-screen assertion that pins the exact poll at which the drop happens |
| Drop the `if idle` early return (run the count out) | `TestTheWindowClosesAtTheVerdict`'s capture count. Asserting the final state cannot see this |
| Derive `finishedAt` while the window is pending | `TestIdleWindowSurvivesTheTurnEndRepaint` — add an assertion that `FinishedAt` is 0 at every poll before the verdict |
| `Confirmed` returning false when `!connected` | `TestWithNoClientTheDerivationIsImmediate` |
| Capture inside the window but pass the pane to `Retain` **with** the skipped ones | `TestTheFirstWindowPollIsAFirstSight` |
| `Observe(..., blocked=true)` inside the window | **nothing, and the row has been reduced to a review check** rather than left as a mutant somebody will mark killed. `blocked` has exactly one effect in `Observe` — it withholds the stamp (`state.go:123`) — and the row's `FinishedAt` inside the window comes from the report's derivation either way, so no assertion can separate the two. Pass `false`, and pass it because the window asks the classifier one question only ("has the screen settled"); if a reader changes it, reject it on that reasoning, not on a test |

**Step 6: Commit**

```bash
git add internal/tmux/reports.go internal/tmux/reports_test.go internal/tmux/poller.go internal/tmux/poller_internal_test.go
git commit -m "feat: verify a reported idle against the screen for settleAfter+2 polls"
```

---

### Task 8: Evidence rules 1 and 2, and the form registry

**Files:**
- Modify: `internal/tmux/blocked.go`, `internal/tmux/blocked_test.go` (the registry)
- Modify: `internal/tmux/state.go` (`Status` gains `Changed`)
- Modify: `internal/tmux/reports.go`, `internal/tmux/reports_test.go`, `internal/tmux/poller.go`

**Rule 1: a reported `blocked` on a churning screen is dropped.** A capture whose hash changed is positive evidence the agent is running. This covers the integration that died at a dialog the user then answered, on a pane that visibly resumed work.

**Rule 2: a reported `blocked` on a settled screen with no dialog on it is dropped**, after `N_blocked` consecutive polls agree. This is the case the app exists for: the integration dies at a dialog, the user answers at the terminal with **no client connected**, the agent finishes before anyone reconnects. On reconnect the first capture is the baseline, nothing ever changes again, and rule 1 — which needs a *changed* hash — never fires. Without rule 2, `blocked` rests forever on a finished agent.

**`blockedRules` becomes a registry that can hold several forms per agent, and this is a code change rather than the data change revision 3 of the design promised.** Today it is `map[string]dialog` — exactly one grammar per agent — and claude's slot is taken by a grammar requiring horizontal rules, a cursor on a numbered choice, at least two numbered choices, and a line ending in `?`. **Rule 2 drops a report only when *no registered form for that agent* matches**, not when it fails to match the one grammar the agent happens to have. Those are the same question today and stop being the same question the day a second claude form is promoted, at which point a standing `permission` report on a screen showing an elicitation form must **not** be dropped: the agent is waiting, and which form it waits at is not rule 2's business.

Forms carry an **identifier**, because Task 13's whitelist names one and that is what makes the consistency test able to fail. Three exist, one per agent — `claude/permission`, `opencode/permission`, `pi/selector` — and **claude has exactly one**, which is why promoting a second claude screen is a restructuring rather than the data change revision 3 of the design promised.

**`N_blocked = settleAfter + 1`, and it is a different number from `N_idle`.** Rule 1 drops anything that moves before rule 2 sees it, so rule 2 never has to absorb a repaint and needs no `R`. **Do not carry `4` across from `N_idle`.** The value is open question 3 in the design: a derived floor with an unmeasured value, and measuring it needs the real dialog screens question 10 is also waiting on. Too small and rule 2 drops a true `blocked` on a slow-repainting screen; too large and the overnight case takes longer to correct itself. **This is the one-line task the design hands the plan**: write it against `settleAfter`, in one place, with the trade in the comment.

**Step 1: Write the failing tests**

Rule 2's test needs a **true premise**, not a counter. Revision 2 of the design specified it as "`N` settled captures drop, `N-1` do not", which tests the counter and cannot see the blocker: its premise — that a reported `blocked` has a matchable dialog — was false for four fifths of that revision's whitelist.

```go
// The fixtures are the real captured claude permission screen with the dialog
// present, and the same screen after the dialog is answered. The off-by-one is
// still a point; it is no longer the only point.
func TestRule2NeedsATruePremise(t *testing.T) {
	// The relationship, on its own line, first. Every count below is a count of
	// POLLS, and a poll count written against NBlocked moves with NBlocked:
	// retarget the constant at 4 and the fixture drives 4 polls, drops at 4 and
	// not at 3, and the whole test is green on the mutant it exists to kill.
	// Unlike Task 7 there is no second assertion here to catch it -- nothing in
	// this test depends on anything but the count -- so the only thing that can
	// pin the value is a statement of the relationship, in terms of a constant
	// the mutant does not touch. This kills BOTH `NBlocked = NIdle` (which is
	// settleAfter+2) and `NBlocked = settleAfter`. The model is
	// TestSanitizeActivityBounds' `MaxActivity != MaxLabel` line
	// (internal/tmux/report_test.go:56).
	if NBlocked != settleAfter+1 {
		t.Fatalf("NBlocked = %d, want settleAfter+1 = %d: rule 1 drops anything that moves before rule 2 sees it, so rule 2 never has to absorb a repaint and needs no R -- it is NOT NIdle (%d), and 4 must not be carried across",
			NBlocked, settleAfter+1, NIdle)
	}

	blocked := readFixture(t, "claude-blocked.txt")
	answered := readFixture(t, "claude-idle.txt")

	// A settled screen with the dialog still on it never drops, at any count.
	// Bound written off settleAfter, like everything else here.
	for i := 0; i < (settleAfter+1)*3; i++ { /* ... report stands ... */ }

	// The same screen answered drops at exactly settleAfter+1 settled polls,
	// and not at settleAfter. Loop and boundary both built from settleAfter, so
	// that they stay where they are when NBlocked is retargeted:
	//
	//   for i := 0; i < settleAfter+1; i++ { ... feed the answered screen ... }
	//
	// with the report asserted still standing after poll settleAfter and gone
	// after poll settleAfter+1.
}

// Rule 2 asks "no registered form for this agent", not "not the one grammar
// this agent has". Same question today; a different question the day a second
// claude form is registered, and the day it stops being the same question is
// the day a standing permission report would be wrongly dropped on a screen
// showing an elicitation form.
func TestRule2AsksAboutEveryRegisteredForm(t *testing.T) {
	// Register a second, throwaway form for a throwaway agent in the test, put
	// a screen on the table that matches only the second, and assert a report
	// naming the first does NOT drop.
}

// Rule 1 needs a CHANGED HASH, not a working verdict. state.go reports working
// on a single changed hash and also on any poll before settleAfter, so a rule
// keyed on the verdict would fire on an ordinary settle.
func TestRule1DropsOnAChangedHashOnly(t *testing.T) { ... }

// Composed: a late repaint on a pane reporting blocked is a changed hash, so
// rule 1 drops the report -- and the badge does not go with it, because a
// dropped report hands the pane to the grammars and the dialog is still on the
// screen for IsBlocked to match positively. Both rules require a connected
// client, so there is no case where the drop happens and the grammar is not
// there to catch it.
func TestARule1DropKeepsTheBadgeWhenTheDialogIsStillThere(t *testing.T) { ... }
```

**Step 2: Run, expect FAIL.**

**Step 3: Implement**

`state.go`: `Status` gains one field, because rule 1's evidence is not the verdict.

```go
	// Changed reports whether THIS capture differed from the previous one.
	//
	// Separate from State because they answer different questions: State says
	// working on a single changed hash and on every poll before settleAfter,
	// which is precisely what settleAfter exists to declare is noise. Evidence
	// rule 1 wants the raw fact -- a changed hash is positive evidence the
	// agent is running -- and a rule keyed on the verdict would fire on an
	// ordinary settle.
	Changed bool
```

`blocked.go`:

```go
// form is one screen shape an agent draws when it is waiting, with the
// identifier a report can name.
//
// The identifier exists because Claude's notification whitelist maps a
// notification_type to blocked only where a grammar can confirm it, and the
// test that holds those two tables together has to name something. "Every
// blocked entry names an agent with a grammar" is VACUOUS -- claude is in the
// map -- so the three types the design demoted would all have passed it.
type form struct {
	ID     string // e.g. "claude/permission"
	dialog dialog
}

// blockedRules is the whole of the detector's knowledge, per agent.
//
// Several forms per agent, in match order. It used to be one dialog per agent,
// which made "promote a second claude screen" a restructuring rather than the
// data change it should be: an MCP elicitation form and a quota press-Enter
// banner match none of claudeDialog's markers, so each needs its own grammar
// beside it rather than instead of it.
var blockedRules = map[string][]form{
	"claude":   {{ID: "claude/permission", dialog: claudeDialog{ /* unchanged */ }}},
	"opencode": {{ID: "opencode/permission", dialog: opencodeDialog{ /* unchanged */ }}},
	"pi":       {{ID: "pi/selector", dialog: piDialog{ /* unchanged */ }}},
}

// RegisteredForm reports whether an id names a form some agent's grammar can
// confirm. The notification whitelist's consistency test is the caller.
func RegisteredForm(id string) bool
```

`IsBlocked` and `ExtractQuestion` iterate the slice; `IsBlocked` is true if **any** form matches, and `ExtractQuestion` returns the first non-nil extraction from a matching form.

`reports.go`:

```go
// NBlocked is how many consecutive settled polls with no registered form on
// screen drop a reported blocked.
//
// A DIFFERENT number from NIdle, and nobody should carry 4 across. Rule 1 drops
// anything that moves before rule 2 sees it, so rule 2 never has to absorb a
// repaint and needs no R: its floor is settleAfter + 1.
//
// The value is a guess of the same standing as workingTTL -- open question 3 in
// the design -- and measuring it needs the dialog screens question 10 is also
// waiting on. Too small and a true blocked is dropped on a slow-repainting
// screen; too large and the overnight case takes longer to correct itself.
// Expressed against settleAfter, in one place, so that changing it is one line.
const NBlocked = settleAfter + 1
```

The poller captures a pane for as long as a `blocked` report stands, and feeds `(st.Changed, IsBlocked(agent, screen))` back through a `Reports` method that applies rule 1 then rule 2.

**Step 4: Run both suites, expect PASS.**

**Step 5: Mutation testing**

| Mutant | Killed by |
| --- | --- |
| Rule 1 keyed on `st.State == StateWorking` rather than `st.Changed` | `TestRule1DropsOnAChangedHashOnly` |
| Rule 2's counter not reset when a form matches | `TestRule2NeedsATruePremise`'s "never drops at any count" |
| `NBlocked = NIdle` | `TestRule2NeedsATruePremise`'s **`NBlocked != settleAfter+1`** line, and nothing else. Not the exact-count half: every poll count in that test is written against `NBlocked`, so retargeting the constant moves the fixture and the assertion together — at `NBlocked = 4` the test drives 4 polls, drops at 4, does not drop at 3, green. The counts kill it only once they are built from `settleAfter` as well |
| `NBlocked = settleAfter` | the same `NBlocked != settleAfter+1` line. Same reasoning: a `NBlocked-1` assertion tracks the mutant |
| Rule 2 asking `blockedRules[agent][0]` rather than every form | `TestRule2AsksAboutEveryRegisteredForm` |
| `IsBlocked` returning on the first form's verdict rather than any match | the same test |
| Rule 2 applied while the screen is still churning | assert a churning screen with no dialog is rule 1's drop, not rule 2's — the tombstone is the same, but the counter must not advance |
| A form registered without an ID (`""`) | Task 13's consistency test; add a registry test here that every form has a non-empty, unique ID |

**Step 6: Commit**

```bash
git add internal/tmux/blocked.go internal/tmux/blocked_test.go internal/tmux/state.go \
        internal/tmux/state_test.go internal/tmux/reports.go internal/tmux/reports_test.go internal/tmux/poller.go
git commit -m "feat: check a reported blocked against every registered screen form"
```

---

### Task 9: What "dropped" means, against a transport that never forgets

> **Carried from Task 5's implementer, unverified and worth settling here.** `refresh` in `poller.go` resets
> `p.classifier` on a tmux-server generation change, but does **not** reset `p.reports`. That is probably safe
> by derivation: a restarted server's panes carry no options, so the raw value is `""`, `ParseReport` fails and
> `Observe` deletes the entry. But it is a derivation nobody has tested, and this task is the one that decides
> what daemon-side report memory means across a restart -- the rejection slot has exactly the same question.
> Either assert it (a generation change with a standing report, and the entry gone afterwards) or reset
> `p.reports` alongside the classifier and say why. Do not leave it as a derivation a reader has to redo.

**Files:**
- Modify: `internal/tmux/reports.go`, `internal/tmux/reports_test.go`

The evidence rules say a report is "dropped". Left unspecified against an option that still holds the same value on the next poll, the daemon re-reads it, re-evaluates it, and flips it back to accepted the moment the screen settles again.

- **The daemon keeps two timestamps per pane**: the last report it **accepted**, and the standing report if it was **rejected on evidence**. One slot each rather than a set — the option holds exactly one value, so the only report that can be re-seen is the current one.
- **The report in force is re-rejected, without re-evaluating the evidence, for as long as its own timestamp matches the rejection slot**, whichever rule condemned it. The evidence that condemned it was a screen that has since moved on; re-running the test against a screen that has since settled is exactly how a dropped report comes back to life. The comparison is against the report that has passed the ordering filter and is about to go into force — `st.accepted` — and **not** against the raw value just parsed off the option; the difference is not cosmetic and Step 2–4 gives the fixture that separates them.
- **A rejection changes what is believed, not what was accepted.** Be precise about this, because the two are easy to conflate and Task 5's data model settles it: a report reaches the evidence rules only by passing the ordering filter first, so the condemned report **is** `st.accepted` and stays there. The rejection slot marks it as not in force; it does not rewind the filter. The filter therefore goes on measuring against that same timestamp, which is what makes a delayed older write a non-event: it is refused for being older, and refusing it must not clear the rejection.
- **A newer value clears the rejection slot.** It is a different report and earns its own verdict.
- **Across a restart both slots are empty**, so the standing report is a first sight: accepted by the ordering filter and then verified from scratch. A resting `idle` **enters the verification window** rather than being re-derived immediately, and a `blocked` that had been dropped can re-badge for up to `NBlocked` polls. That is the honest cost of holding the rejection in daemon memory rather than in tmux — bounded, one-shot, and only on restart.

**One semantic for all three rules, and this is the part a reader will try to improve.** Revision 4 of the design made a rule-3 rejection *provisional*, cleared "the moment the classifier reports `idle`", and revision 5 **withdrew it on a measurement**. Both premises are gone:

- The case it was built for is measurably empty: at `N_idle = settleAfter + 2`, polls-to-settle was 3 or 4 in every one of 1,440 phase replays and **never 5**. The "window one poll too short" that provisional clearing repairs did not occur once.
- Clearing on a classifier `idle` is no longer a safe trigger: finding B measured 4 of 88 turns going still while waiting on the model, so mid-turn stillness on a root that is genuinely working can clear a rejection that was **correct** — resurrecting a subagent's false `idle`, deriving `finishedAt` from it, and landing the false done badge the rule exists to prevent.

**What a permanent-sounding rejection actually costs is much less than it sounds, and the reason has to be in the comment**: a rejected report hands the pane back to the classifier, and the classifier is an authority that can stamp a finish. For rule 3 to have fired at all the screen must have churned through the whole window, which sets `everChanged`; when it does settle, `Observe` stamps `finishedAt = now` in the ordinary way. A wrongly-dropped true turn end does not lose its badge — **it gets one dated by when the daemon noticed instead of by when the agent finished**. And the rejection lasts only until the agent next does anything: the next turn's start writes `working`, which is an edge, which is a newer value, which clears the slot.

**Step 1: Write the failing tests**

```go
// Three rules, one slot, one test -- because the slot is one field, the
// difference revision 4 wanted was a flag on it, and that flag is now known to
// have a measured failure mode rather than merely an unproven one. Written as
// the negative, which is the only way it can hold:
func TestARejectionIsNotClearedByAClassifierIdle(t *testing.T) {
	for _, rule := range []string{"rule 1", "rule 2", "rule 3"} {
		// ... drive the pane to a rejection by that rule ...
		// A later poll at which the classifier reports idle changes nothing:
		// the report stays rejected and no finishedAt is derived from it.
	}
}

// What DOES clear the slot, and the fixture for it is the next turn's working
// write -- an edge, which writes unconditionally.
func TestANewerValueClearsTheRejection(t *testing.T) { ... }

// A delayed older write neither wins nor rescues the rejected report.
//
// There is no report "newer than the rejected one but older than the last
// accepted one" to test with: the rejected report IS st.accepted, so that
// interval is empty, and a test written to the earlier wording would have had
// to invent a state the implementation cannot reach. The fixture that matters
// is the one ordinary scheduling jitter actually produces.
func TestARejectionSurvivesADelayedOlderWrite(t *testing.T) {
	// accept working@T0; accept idle@T1 (T1 > T0); reject it on evidence.
	// Then a delayed write lands carrying a timestamp T with T0 < T < T1 --
	// a fire-and-forget working write from an earlier hook, arriving late.
	//
	//   (a) It is REFUSED and NOTHING goes into force on that poll: the pane
	//       stays on the classifier. Assert both halves -- that the row does
	//       not read `working` (a mutant that accepts the late write puts a
	//       stale working on the row) AND that it does not read the rejected
	//       idle either. The second half is the one that catches the rejection
	//       guard written on `parsed.Timestamp` at the entry rather than on
	//       `st.accepted.Timestamp` at the return: the late write does not
	//       match the slot, so an entry guard lets the poll through, the
	//       ordering filter refuses the write, and the function then hands back
	//       st.accepted -- the rejected report -- with ok = true.
	//   (b) It does not clear the rejection, and the only way to see that is to
	//       keep polling: the NEXT poll re-reads the standing idle@T1 -- the
	//       option still holds it -- and it must still be refused, with NO
	//       capture taken and no evidence re-run. A mutant that clears the slot
	//       on any parsed value resurrects it here.
}

// The claim that makes a permanent rejection affordable. Same fixture as the
// rule-3 drop, continued: the pane is back on the classifier, everChanged is
// set because the screen churned through the whole window, and when it finally
// settles the classifier stamps its own finishedAt. Assert that it lands, AND
// that it is dated `now` rather than the report's timestamp -- they are two
// different facts and the test should be able to tell which one it got.
func TestARule3RejectionDoesNotCostTheBadge(t *testing.T) { ... }

// A restart empties both slots. The standing report is accepted by the ordering
// filter -- the daemon has no better information than the value tmux holds --
// and then verified from scratch: a resting idle ENTERS the window rather than
// deriving immediately.
func TestAfterARestartAStandingIdleEntersTheWindow(t *testing.T) {
	// "Restart" is a fresh NewReports() over the same tmux state. Revision 2 of
	// the design re-derived immediately here, on the grounds that a pre-restart
	// report "is almost certainly a real turn end" -- which is the subagent
	// false-idle waved through by an adverb.
}
```

**Step 2–4: Run (FAIL), implement, run (PASS).** `reportState` gains `rejected int64`. The guard goes **on `st.accepted` at the return, not on `parsed` at the entry**, and this is the part to get right:

```go
	// ... the ordering filter from Task 5, with one line added:
	if parsed.Timestamp > st.accepted.Timestamp {
		st.accepted = parsed
		// A newer value is a different report and earns its own verdict.
		st.rejected = 0
	}
	// The guard is on what is ABOUT TO GO INTO FORCE, which is st.accepted --
	// never on `parsed` at the top of the function. A delayed older write does
	// not match the rejection slot (it carries its own, earlier timestamp), so
	// an entry guard waves it through; the ordering filter then refuses it for
	// being older, leaves st.accepted alone -- and st.accepted IS the report
	// that was rejected on evidence, which the function would then return with
	// ok = true. The rejected report comes back in force on the strength of an
	// unrelated late write. Written here, the same poll returns nothing in
	// force, whichever value tmux happened to be holding.
	if st.accepted.Timestamp == st.rejected {
		return Report{}, false
	}
	// ... the workingTTL check, then `return st.accepted, true`.
```

Note that this is also what makes the design's own sentence above true — "refusing it must not clear the rejection". Refusing a delayed older write leaves both slots untouched, and the pane stays on the classifier.

**The alternative that must not be built:** writing the rejection back into the option so it survives with the report. Rejected on a rule this design has held since "Not `@wterm_label`" — **the daemon reads that option, it does not write it.** A reader that edits the channel it reads cannot be reasoned about when two of them run, and nothing promises `wterm-web` is a singleton.

**Step 5: Mutation testing**

| Mutant | Killed by |
| --- | --- |
| Clear the rejection on a classifier `idle` verdict (revision 4's behaviour) | `TestARejectionIsNotClearedByAClassifierIdle`. **This is the mutant the test exists for**, and it is the one a reader will re-add as an improvement |
| Give rule 3 a different slot semantic from rules 1 and 2 | the same test's loop over all three |
| Clear `st.rejected` on any parsed value rather than only on a strictly newer one | `TestARejectionSurvivesADelayedOlderWrite`'s half (b) |
| Accept a report older than `st.accepted` after a rejection (dropping the ordering comparison) | the same test's half (a) |
| `parsed.Timestamp > st.accepted.Timestamp` to `>=` | still nothing, exactly as in Task 5 — it changes only what happens to two writes in the same millisecond, and the delayed-older-write fixture (`T < T1`) is false under both operators. Do not list it here as killed by half (a); it is the same honest survivor Task 5 already records |
| `st.accepted.Timestamp == st.rejected` to `>=` | a newer report after a rejection is refused; `TestANewerValueClearsTheRejection`. (**Not `<=`** — that one is *equivalent* and no test can kill it: after a newer value clears the slot `st.rejected` is 0 and `st.accepted.Timestamp <= 0` is false, and whenever the slot is set the two timestamps are equal, so `<=` behaves exactly as `==` everywhere. `>=` is the mutant with a difference) |
| The guard written on `parsed.Timestamp` at the entry instead of on `st.accepted.Timestamp` at the return | `TestARejectionSurvivesADelayedOlderWrite` half (a). The delayed write carries its own earlier timestamp, so it does not match the slot and the entry guard waves it through; the ordering filter then refuses it and the function returns `st.accepted` — which *is* the rejected report — with `ok = true` |
| Persist the rejection across a fresh `Reports` | `TestAfterARestartAStandingIdleEntersTheWindow` |
| Re-evaluate the evidence for a re-seen rejected report | assert the poller takes **no capture** for a pane whose standing report is already rejected and whose fallback classifier path has settled |

**Step 6: Commit**

```bash
git add internal/tmux/reports.go internal/tmux/reports_test.go
git commit -m "feat: a report dropped on evidence stays dropped until a newer one"
```

---

### Task 10: `finishedAt`, and the switch between authorities

**Files:**
- Modify: `internal/tmux/reports_test.go`, `internal/tmux/poller_internal_test.go` (mostly tests), `internal/tmux/poller.go` (if anything is missing)

Almost entirely tests, deliberately. The mechanism landed in Tasks 5 and 7; what it needs is the assertions that keep it from being quietly reversed, and the design names them one by one.

**The rules:**

- **From a report, `finishedAt` is *derived*, not stamped.** A pane whose current accepted report is a resting `idle` has `finishedAt` equal to **that report's own timestamp**. No memory of a previous report, no edge, no daemon state. It survives a daemon restart, a poller restart and a `wterm-web` upgrade, because the fact lives in tmux.
- **From the classifier, unchanged from v2**, `everChanged` and all. That authority has no clock — its only way to date a finish is `time.Now()` at the moment it first noticed — so a first sight must stamp nothing there, or every restart is a badge storm on every pane that happens to be sitting still.
- **The authority-switch guard stays, on the side that needs it.** Classifier → report needs none: the report is timestamped and can date its own finish. Report → classifier keeps it, because that is the direction where a timestamp-less authority starts from nothing.

**Why the mid-install badge storm does not happen**, which is the thing to write into the test's comment because it reads like a contradiction: **a report carries the time the agent actually finished, and the classifier can only carry the time we noticed.** The classifier's first sight of a pane idle since yesterday invents `finishedAt = now`, which beats every browser's `seen` and lights every device — a fiction. A report's first sight says "this agent finished at 21:20", and the browser badges only if that device has not looked since 21:20, which is exactly what the badge is supposed to mean.

**Step 1: Write the failing tests**

```go
// Derived statelessly, and specifically that it survives a restart -- the
// assertion revision 1 of the design claimed and revision 1's mechanism could
// not have satisfied.
func TestFinishedAtIsDerivedFromTheReportWithNoDaemonMemory(t *testing.T) {
	// A fresh Reports, a standing resting idle report, no client connected.
	// finishedAt equals the report's own timestamp, exactly.
}

// Both directions, because they are deliberately asymmetric and the next reader
// will assume they are not.
func TestAuthoritySwitchStamping(t *testing.T) {
	t.Run("report to classifier stamps no edge", func(t *testing.T) {
		// An integration dies mid-turn, the working report ages out, the
		// classifier reports working from churn and settles two polls later.
		// Without the guard it would stamp an edge for a run it never saw
		// start. The guard is structural: the capture was skipped while the
		// report stood, so the pane was never passed to Retain, so the first
		// capture after the switch is a first sight and everChanged is false.
	})
	t.Run("classifier to report does produce one", func(t *testing.T) {
		// ... and it is dated by the REPORT, not by now.
	})
}

// A mid-install badge storm is not possible, and the reason is the difference
// between the two authorities rather than a guard.
func TestInstallingMidRunProducesOneTrueBadgeAtMost(t *testing.T) { ... }

// The full precedence table the design asks for: report present/absent x
// fresh/stale x screen available/unavailable, asserting AgentState,
// StateSource AND FinishedAt together. Asserting the state alone cannot see a
// precedence bug at all, for the same reason v2's blocked override hid
// finishedAt.
func TestPrecedenceTable(t *testing.T) { ... }
```

**Step 2–4: Run (FAIL), fix whatever is missing, run (PASS).** If everything passes on the first run, that is a result and not a waste: say so in the commit, and then run the mutants, because a task whose tests all pass immediately is exactly the task whose tests might be vacuous.

**Step 5: Mutation testing**

| Mutant | Killed by |
| --- | --- |
| `rows[i].FinishedAt = now.UnixMilli()` on the report path | `TestFinishedAtIsDerivedFromTheReportWithNoDaemonMemory` |
| Derive `finishedAt` from a `working` or `blocked` report | the precedence table |
| Remember the previous report and stamp on the working→idle **edge** | `TestFinishedAtIsDerivedFromTheReportWithNoDaemonMemory` — an edge needs daemon memory, and a fresh `Reports` has none, so the badge would not survive a restart |
| Pass a report-owned pane to `Classifier.Retain` | `report to classifier stamps no edge` |
| Drop `everChanged` from the stamp guard | v2's own `state_test.go`; re-run it, and if it stays green, `state_test.go` has the gap |

**Step 6: Commit**

```bash
git add internal/tmux/reports_test.go internal/tmux/poller_internal_test.go internal/tmux/poller.go
git commit -m "test: pin finishedAt's derivation and the authority switch in both directions"
```

---

## Phase D — the row in the browser

### Task 11: What a pane row says, now that the agent can say it

**Files:**
- Modify: `web/src/components/AppSidebar.tsx` (`paneText`, `PaneLines`, and the header comment on `paneText`)
- Modify: `web/src/components/AppSidebar.test.tsx`

**`paneText`'s ladder becomes: question → user label → activity → title → command.**

The user's label stays **above** the activity, for v2's reason: a label is a name the user gave this pane on purpose, and it wins over programs — `AppSidebar.tsx`'s own comment says so, and this design's transport decision rests on the same sentence. But that leaves a labelled pane with no activity readout, which is a real loss, and **the fix is a layout change rather than a precedence change: when both exist, the label joins the window name on the first line and the activity keeps the second.** The label is identity, the activity is description, and `PaneLines` already has exactly that split — it is what the capsule-versus-second-line decision is about. So this is a change to `PaneLines`, not only to `paneText`.

**The UI must not branch a row's appearance on `stateSource`.** Two visibly different kinds of state dot teach the user to trust one and ignore the other, which is the badge-integrity failure arriving through a third door. A tooltip is fine ("reported by the agent" / "from the screen"); a class is not. **A prohibition with no test is a prohibition that gets violated silently.**

Also: the header comment on `paneText` currently says a title is "what it is working on", which is the premise that collapsed. Claude Code's title is generated on the first turn and frozen after it — held byte-identical across three turns with unrelated prompts, and stale while the agent is blocked. opencode's is derived from the first message and never revised. pi's is `π - <cwd basename>` and never mentions the task. **The title's meaning is downgraded from "what it is doing" to "what this pane is"**, and the copy must stop implying otherwise. That is a comment edit and it is part of this task.

**Step 1: Write the failing tests**

```tsx
// No jsdom and no testing library -- follow the existing AppSidebar.test.tsx,
// whose header comment says why: renderToStaticMarkup in the same node
// environment as the rest of the suite. Its `render`, `fromRows` and `row`
// helpers are what these tests use; do not introduce a second rendering style
// in the same file.

it('shows the activity under the name', () => { /* ... */ })

it('puts a user label on the first line when there is an activity for the second', () => {
  // Both present. The label is identity and rides with the name; the activity
  // is description and keeps the second line. Neither is dropped -- which is
  // what the old precedence did to whichever one lost.
})

it('keeps a lone label on the second line, exactly as before', () => {
  // The layout change is scoped to "both exist". A labelled pane with no
  // integration must look exactly as it looks today.
})

it('prefers a blocked question to both', () => { /* unchanged from v2 */ })

it('falls back to the title, then the command, when there is no activity', () => {
  // The whole v2 ladder still has to work: most panes will never have an
  // integration, and that is the floor this design degrades to everywhere.
})

// The prohibition, as a test. Note the POSITIVE CONTROL in the second half:
// without it, "every row renders identically" also passes, and this repo has
// already shipped one assertion that could not fail
// (expect(cls).toContain('disabled') on a shadcn button, which passes
// unconditionally because the Tailwind class list contains
// disabled:pointer-events-none).
// There is no <PaneRow> to render: AppSidebar.tsx exports AppSidebar and
// AGENT_TITLE_PREFIXES, and the row is internal to it. Do NOT extract and
// export one for this test -- that is a refactor of a 1,000-line component in
// a task whose Files list says `paneText`, `PaneLines` and a comment. Use the
// file's own `render(fromRows([...]))` helper, which returns the whole
// sidebar's static markup, and compare the STRINGS. That is a stronger
// assertion than a class list anyway: it covers the dot, the title attribute,
// the aria labels and anything else somebody might key on stateSource.
it('renders a row identically whichever authority decided its state', () => {
  const base = { command: 'claude', agentState: 'idle', activity: 'run go' }
  const fromEvent = render(fromRows([row({ ...base, stateSource: 'event' })]))
  const fromScreen = render(fromRows([row({ ...base, stateSource: 'screen' })]))
  expect(fromEvent).toBe(fromScreen)
  // Not vacuously equal: the row really is in there.
  expect(fromEvent).toContain('run go')

  // Positive control: something the row IS allowed to change on must differ,
  // or the comparison above is measuring nothing.
  const blocked = render(fromRows([row({ ...base, agentState: 'blocked', stateSource: 'event' })]))
  expect(blocked).not.toBe(fromEvent)
})
```

**Step 2: Run, expect FAIL.**

```bash
cd web && pnpm test src/components/AppSidebar.test.tsx
```

**Step 3: Implement**

`PaneText` gains one optional field, and the comment says what it is for:

```ts
interface PaneText {
  text: string
  /**
   * A user label to sit beside the row's name on the first line.
   *
   * Set only when the label and the agent's activity both exist: the label
   * wins the *precedence* -- it is a name the user gave this pane on purpose --
   * but the row has two lines, and making the label take the second one would
   * throw away the activity readout that is the whole point of the feature.
   * Identity on the first line, description on the second.
   */
  label?: string
  tooltip?: string
  fromCommand: boolean
}
```

`PaneLines` renders `label` after `name` on the first line when it is present.

**Step 4: Run both suites, expect PASS. Then look at it**, with the Task 6 demo: a pane with a label and a report shows both, a pane with only a report shows the activity, a `zsh` pane looks exactly as it did.

**Step 5: Mutation testing**

| Mutant | Killed by |
| --- | --- |
| Activity placed **above** the label in the ladder | `puts a user label on the first line...` — the label disappears |
| The label dropped when an activity exists | the same test |
| The label moved to the first line **always** | `keeps a lone label on the second line` |
| Activity rendered even when it is `''` | a row with an empty activity must fall through to the title; add the assertion if it is missing, because an empty second line is a worse row than no second line |
| `stateSource` added to the dot's class list, or to a `title`/`aria-label` | `renders a row identically whichever authority decided its state` — the markup comparison sees all three |
| The row rendered as a constant, or the two renders compared against each other by accident | the **positive control** in that same test. Without the control this mutant lives |
| `rowsEqual` not comparing `activity` (Task 4's mutant, re-run here) | the activity never updates after first paint — worth re-checking from the component side, because that is where it would be noticed |

**Step 6: Commit**

```bash
git add web/src/components/AppSidebar.tsx web/src/components/AppSidebar.test.tsx
git commit -m "feat: show what the agent says it is doing, under the pane's name"
```

---

## Phase E — the writer's brains

All of it in Go, in `wterm-web report`. The whitelist, the edge/re-assertion classification and the text reduction live in **one table with one test**, not in three integration files and a `settings.json` the user owns. That is the whole point of having one binary: when the list grows it grows in one place.

### Task 12: Recorded hook payloads as fixtures

**Files:**
- Create: `cmd/wterm-web/testdata/hooks/{claude,opencode,pi}/*.json`
- Create: `cmd/wterm-web/testdata/hooks/README.md` (how each was captured, and which ones could not be)

**These do not exist yet and capturing them is implementation, not design.** Everything in Tasks 13–15 is written against what these files actually contain, not against the shapes quoted in the design. Where the two disagree, the fixture wins and the disagreement goes in the commit message.

**Safety, and it is not negotiable.** This repository is **public** and the owner's live panes hold private client work.

- Capture in your **own** session, with **your own** content — a throwaway prompt you wrote for the purpose ("write a haiku about tmux", "read this file and tell me its length").
- **Never copy text out of the owner's live panes.** If a payload comes back with anything of the owner's in it, mask it the way the existing screen fixtures are masked: every word replaced by a placeholder of the same length, so structure, alignment and wrapping survive and content does not.
- Each agent runs against an **isolated config directory** and an **isolated tmux socket**, so nothing you install reaches the owner's real setup.

**The two that carry more weight than the rest, and they should be captured first**: a real **`agent_settled`** (pi) and a real **`session.idle`** (opencode) taken **while a subagent is running**. Those are the two turn-end filters that fail open, and this is the only evidence anybody will ever have about what they actually contain. If they turn out to carry a session identifier, say so loudly — it is the measurement open question 9 is waiting for.

**Step 1: Build the recorder**

One script per agent, in your scratchpad, never in the repo. The shape (this is the one used to produce the design's measurements):

```sh
#!/bin/sh
# rec.sh <event-label>: append one hook payload, plus the environment that
# decides whether `report` can even find the pane.
S="$(dirname "$0")"; EV="$1"; IN="$(cat)"
{
  echo "=== $EV at $(date +%s.%N) pid=$$ ppid=$PPID"
  echo "--- STDIN ---";  printf '%s\n' "$IN"
  echo "--- ENV ---";    env | grep -E '^(TMUX|TMUX_PANE|CLAUDE_)' | sort
  echo "--- TTY ---";    tty 2>&1
} >> "$S/out/$EV.log" 2>&1
```

The `--- ENV ---` block is not decoration: **whether a hook process carries `TMUX_PANE`** is what open questions 9 and 11 turn on, and this is the harness that answers them. Record it for every event, on every agent.

**Step 2: Capture, per agent**

- **claude**: `CLAUDE_CONFIG_DIR=<scratch>/cfg claude`, with a `settings.json` registering `rec.sh` on `UserPromptSubmit`, `PreToolUse`, `Notification` and `Stop`, each `"async": true`. **Never `"asyncRewake"`.** One payload per hook, plus one per `notification_type` you can provoke.
- **opencode**: a plugin in `<scratch>/proj/.opencode/plugin/` that writes `JSON.stringify({event})` to a file. Capture `chat.message`, `session.status` (both `busy` and `idle`), `tool.execute.before`, `permission.asked`, `todo.updated`, `session.idle` — and `session.idle` again with a subagent running.
- **pi**: an extension in `<scratch>/proj/.pi/extensions/`. Capture `session_start` (recording `ctx.mode` and `ctx.isIdle()`), `input`, `tool_execution_start` (**with its `args` object in full** — that is open question 7), `ui_prompt_start`, `agent_settled` — and `agent_settled` again with a subagent running.

**Four you will probably not get, and that is a result rather than a failure.** `quota_auto_resume_stale`, `elicitation_dialog`, `elicitation_url_dialog` and `agent_needs_input`'s teammate-setup form are open question 10. An 88-turn, 3-agent measurement run met **none** of them and did not provoke them. Do not spend a day hunting them: record in the README that they are missing, and Task 13 leaves them *ignored*.

**Step 3: Land the fixtures**

Strip each capture to the JSON payload, one file per event, named for it. The README records, per file: which agent version produced it, what prompt provoked it, whether `TMUX_PANE` was present, and whether anything was masked.

**Step 4: Commit**

```bash
git add cmd/wterm-web/testdata/hooks
git commit -m "test: record real hook payloads from all three agents"
```

There is no mutation step here — there is no logic yet. **What replaces it:** re-read each fixture and confirm it is a payload the *agent* produced and not one you shaped to fit the design. A fixture edited to match an expectation is the exact failure mode this project keeps hitting, one level further back.

---

### Task 13: The event tables and the `notification_type` whitelist

**Files:**
- Create: `cmd/wterm-web/events.go`, `cmd/wterm-web/events_test.go`
- Modify: `cmd/wterm-web/report.go` (`--agent`, `--event`, payload on stdin)

**Two modes, and the rule between them is settled here rather than discovered.** `report` accepts either:

| Given | Means |
| --- | --- |
| `--state` (with optional `--text`) | The **manual** form from Task 6. Unchanged, still supported, still what the demo and a user's own script use. The state is taken literally; no table is consulted and stdin is not read |
| `--agent` + `--event` (payload on stdin) | The **integration** form. The table decides the state, the text and the edge/re-assertion kind |
| `--event` without `--agent`, or `--agent` without `--event` | A usage error: **no write**, a line on stderr, exit 0 |
| **both** `--state` and `--agent`/`--event` | A usage error, for the same reason and with the same outcome: **no write**, a line on stderr, exit 0. It is refused rather than given a precedence, because a precedence is a rule somebody has to remember and neither caller has any reason to send both. A silent winner here would be a wrong state written from a hook that was passing an argument it believed was doing something |
| neither | A usage error, as in Task 6 |

Each of those rows gets a row in `TestReportAlwaysExitsZero` (exit 0, nothing on stdout) **and** an assertion that the option is still unset afterwards — "exit 0" alone does not distinguish "refused" from "wrote something wrong and said nothing".

**Step 1: Write the failing tests**

```go
// One row per documented notification_type, plus invented ones, asserting the
// state reported -- and asserting that an unrecognised value produces NO WRITE
// AT ALL, not a write of the previous state.
func TestClaudeNotificationWhitelist(t *testing.T) {
	for _, tc := range []struct {
		typ   string
		state string // "" means: write nothing
	}{
		{"permission_prompt", tmux.StateBlocked},
		{"quota_auto_resume_fired", tmux.StateWorking},
		{"idle_prompt", tmux.StateIdle},                 // as a REPAIR only; Task 14
		{"quota_auto_resume_disabled", tmux.StateIdle},  // same
		// Ignored PENDING A CAPTURE: a real wait the screen cannot see, and no
		// registered form can confirm it. Open question 10.
		{"quota_auto_resume_stale", ""},
		{"elicitation_dialog", ""},
		{"elicitation_url_dialog", ""},
		{"agent_needs_input", ""},
		// Ignored outright: not a state of this session.
		{"auth_success", ""},
		{"elicitation_complete", ""},
		{"elicitation_response", ""},
		// "a background session finishes or fails" -- NOT this session's turn
		// end. Reported as idle it is a false done badge on a working agent.
		{"agent_completed", ""},
		// The default, and it matters more than the table.
		{"a_type_nobody_has_documented_yet", ""},
		{"", ""},
	} {
		...
	}
}

// The composition test. The two tables live in different files, each is correct
// alone, and the contradiction is only visible from above: revision 2 of the
// design mapped five types to blocked, four of which had no screen form any
// grammar could match -- and evidence rule 2 deletes exactly those. Those
// badges would have stood until a client connected and then been erased about
// 4.5 seconds later while Claude was still waiting.
//
// It is specified at FORM granularity on purpose. "Every blocked entry names an
// agent with a grammar" is vacuous -- claude IS in the map -- so the three types
// revision 3 demoted would all have passed it, which is to say the test written
// to stop round two's blocker recurring could not have seen it.
func TestEveryBlockedMappingNamesARegisteredForm(t *testing.T) {
	for _, m := range allMappings() {
		if m.state != tmux.StateBlocked {
			continue
		}
		if !tmux.RegisteredForm(m.form) {
			t.Errorf("%s maps to blocked but names form %q, which no grammar confirms: "+
				"evidence rule 2 would delete that badge as soon as a client connected", m.name, m.form)
		}
	}
}

// The three agents' turn events, from the recorded fixtures rather than from
// the design's prose.
func TestEventMappings(t *testing.T) { /* table over cmd/wterm-web/testdata/hooks */ }
```

**Step 2–4: Run (FAIL), implement, run (PASS)**

```go
// mapping is what one (agent, event) means. The zero state is "ignore": no
// write, no state change, no timestamp refresh, and not an error.
type mapping struct {
	name  string
	state string
	kind  eventKind // edge or reassertion; Task 14
	// form is the registered screen form a blocked mapping names. A blocked
	// mapping may exist ONLY where a grammar can confirm it: the event says the
	// agent is waiting and the grammar says the screen shows it waiting, and a
	// resting claim for which no evidence can exist does not get to rest
	// indefinitely.
	form string
	text textSource // Task 15
}
```

**The default is the decision here, not the enumeration.** The documented list is not documented as closed, it has plainly grown before, and it will grow again. So an unrecognised `notification_type` **writes nothing at all**. The asymmetry that settles it:

- Ignoring a notification that should have meant `blocked` costs a badge that is late and may never come — up to 60 seconds of `working` expiry and then whatever the grammar can do, which for an uncaptured dialog is nothing. (Not merely "a late badge": at a permission wait the standing report is a fresh `working` from `PreToolUse`, and a fresh `working` **skips the capture**, so `IsBlocked` is not running.)
- Defaulting an unknown notification to `blocked` costs a **permanent false badge**. `blocked` is resting, so it never expires — and the changed-hash escape hatch cannot save it: about 580 s of post-turn idle across ten runs produced **zero** hash changes and zero pane output on all three agents. On a finished agent that escape hatch cannot fire at all.

**Two costs to state in the comment rather than discover:** Claude's event-sourced `blocked` is about **six seconds late by construction**, because `permission_prompt` waits about six seconds before firing — four polls. And installing the integration makes Claude's blocked detection **slower in the connected case**: a non-integrated pane is captured every poll and `IsBlocked` matches within about 1.5 s, where an integrated one takes about six, because `PreToolUse`'s fresh `working` suppressed the capture that would have found it. The trade is accepted — the integration's value is the **disconnected** case, where it is the only source of anything at all — but it is a trade, and it has a lever: dropping `PreToolUse` restores the 1.5-second connected badge and costs the activity line. That is what makes open question 1 a design question and not only a fork count.

**Step 5: Mutation testing**

| Mutant | Killed by |
| --- | --- |
| Default an unknown `notification_type` to `blocked` | the two invented rows |
| Default it to the previous state, or to `working` | the same rows — assert **no write**, not merely a different state |
| Map `agent_completed` to `idle` | its row. This is a false `done` badge on a working agent: v2's central failure through a new door |
| Promote any of the four pending-a-capture types to `blocked` | `TestEveryBlockedMappingNamesARegisteredForm` |
| Weaken that test to "names an agent that has a grammar" | nothing — it is **vacuous**, which is the point. Re-check by hand that mapping `elicitation_dialog` to `blocked` turns it red |
| Map `idle_prompt` to `blocked` (revision 1's behaviour) | its row |
| Drop the `form` field and check only the state | the consistency test cannot compile — which is the desired failure |

**Step 6: Commit**

```bash
git add cmd/wterm-web/events.go cmd/wterm-web/events_test.go cmd/wterm-web/report.go
git commit -m "feat: one table decides what each agent event means"
```

---

### Task 14: Edge versus re-assertion

**Files:**
- Modify: `cmd/wterm-web/events.go`, `cmd/wterm-web/events_test.go`, `cmd/wterm-web/report.go`, `cmd/wterm-web/report_test.go`

**The criterion first, because this is a safety property and "these are the ones I thought of" is not one:**

> **A write is a re-assertion if the state it would write is a resting state and the event that produces it can fire more than once within one resting period.** Everything else is an edge. An edge writes unconditionally; a re-assertion **reads the standing option first** and writes only if the standing report's state differs from the one it would write.

**Why this exists at all.** `Stop` at T writes `1;idle;T`; a device glances; `seen = T`; the badge clears. Sixty seconds later `idle_prompt` fires — it does that after **every** turn the user does not type into — and would write `1;idle;T+60`. It is strictly newer, so the ordering filter takes it. The screen has been settled for a minute, so evidence rule 3 has nothing to object to. `finishedAt` is derived statelessly, so it becomes T+60. Each browser's `seen[paneId]` stores the **value it was shown**, not the time it looked. **Every device that saw the finish re-badges, one minute after every Claude turn**, and `quota_auto_resume_disabled` does it again. "A no-op re-assertion" is false under this design's own derivation rule, and a badge that fires once per turn on a pane nothing happened to is the failure that teaches the owner to ignore the badge.

**Three things that fall out, and the third is a bug an earlier revision was carrying:**

1. **The classification is per (event, state) pair, not per event.** An event writing `working` on one branch and a resting state on another is an edge on the first and a re-assertion on the second. `working` is transient and cannot badge — the worst a redundant one does is refresh the expiry, which is what the keepalive wants anyway — so the extra read is paid only where it buys something.
2. **What makes the turn-end events edges is an invariant, not their semantics.** `Stop`, `agent_settled` and `session.idle` all write a resting state, so they are edges only because they cannot fire twice inside one resting period — and that holds **only because a new turn writes a non-resting state before its turn end can fire.** Hence the turn-start invariant. A turn that could end with no `working` written before it would have its turn end suppressed and lose that turn's badge.
3. **pi's `session_start` re-derivation is a re-assertion.** A pi extension reload can replace the extension mid-run, so `session_start` re-derives state from `ctx.isIdle()`. A reload can happen any number of times while the agent sits idle, and the `isIdle()` branch writes a resting state. Implemented as an edge, **every extension reload on an idle pane writes `idle;<now>` and re-badges every device** — the badge storm arriving through an event nobody had classified, which is the whole argument for having a criterion instead of a list. Its `isIdle() === false` branch writes `working`, which is transient, and stays an edge.

**The lists are not claimed to be exhaustive**, so they ship with a **default** instead of a guarantee: **an event whose cardinality within a resting period is unknown, and which writes a resting state, is treated as a re-assertion.** The read costs one fork on a path nobody is waiting on; the alternative costs a badge storm.

| Kind | Events |
| --- | --- |
| Edge | `Stop`, `agent_settled`, `session.idle` (each by the turn-start invariant), `UserPromptSubmit`, pi's `input`, `session.status busy`, `PreToolUse`, tool events, `permission_prompt`, `permission.asked`, `ui_prompt_start`, `quota_auto_resume_fired`, and `session_start`'s working branch |
| Re-assertion | `idle_prompt`, `quota_auto_resume_disabled`, and `session_start`'s **idle** branch |

**Step 1: Write the failing tests**

```go
// Assert on the TMUX CALLS MADE, not only on the value that ends up stored:
// "no write" and "a write of the same state with a newer timestamp" store
// values that look alike and badge differently.
func TestEdgeAndReassertion(t *testing.T) {
	t.Run("an edge writes without reading", func(t *testing.T) {
		r := &recordingTmux{standing: "1;idle;1789075200000"}
		// Stop, on a pane already reporting idle.
		runReportWith(r, "--agent", "claude", "--event", "Stop")
		if r.shows != 0 {
			t.Errorf("an edge read the standing option %d times, want 0", r.shows)
		}
		if r.sets != 1 {
			t.Errorf("an edge made %d writes, want 1", r.sets)
		}
	})

	t.Run("a re-assertion reads first and stays silent when it agrees", func(t *testing.T) {
		r := &recordingTmux{standing: "1;idle;1789075200000"}
		runReportWith(r, "--agent", "claude", "--event", "Notification",
			withStdin(`{"notification_type":"idle_prompt"}`))
		if r.shows != 1 || r.sets != 0 {
			t.Errorf("shows=%d sets=%d, want 1 and 0: a re-assertion that agrees "+
				"must write NOTHING -- a write of the same state with a newer "+
				"timestamp re-badges every device that had already looked", r.shows, r.sets)
		}
	})

	t.Run("a re-assertion writes when it disagrees, and that repair is the point", func(t *testing.T) {
		// A Stop that never landed -- the attested reason herdr gave up on
		// Claude hooks. The standing report is working; idle_prompt sees the
		// disagreement and repairs the pane from the agent itself.
		r := &recordingTmux{standing: "1;working;1789075200000"}
		runReportWith(r, "--agent", "claude", "--event", "Notification",
			withStdin(`{"notification_type":"idle_prompt"}`))
		if r.sets != 1 {
			t.Errorf("sets=%d, want 1: the repair is what keeps this event mapped at all", r.sets)
		}
	})

	// Three rows carry more weight than the rest.
	t.Run("pi session_start's idle branch reads before writing", func(t *testing.T) { ... })
	t.Run("pi session_start's working branch does not", func(t *testing.T) { ... })
	t.Run("a turn-end event writes unconditionally", func(t *testing.T) { ... })
}

// The invariant that makes the row above safe, and it needs its own test
// because it is a property OF THE TABLE rather than of any one call: a turn
// that could reach its end with no working written before it would have that
// end suppressed and lose the badge.
func TestEveryAgentHasATurnStartWorkingEdge(t *testing.T) {
	for _, agent := range []string{"claude", "opencode", "pi"} {
		m, ok := turnStart(agent)
		if !ok || m.state != tmux.StateWorking || m.kind != edge {
			t.Errorf("%s has no turn-start working edge (%+v): its turn-end event "+
				"is only an edge because one exists", agent, m)
		}
	}
}
```

**Step 2–4: Run (FAIL), implement, run (PASS).** A re-assertion runs one `tmux show-options -p -t <pane> -v @wterm_agent` before deciding, **through the same `tmuxRunner` Task 6 injected** — no new parameter, no refactor of `runReport`'s signature. `recordingTmux` implements that one-method interface, counting `show-options` calls as `shows` and `set` calls as `sets`, and answering a `show-options` with its `standing` field; `runReportWith(r, args...)` is the test helper that calls `runReport(args, io.Discard, io.Discard, mapEnv(...), func(string) tmuxRunner { return r })` with a `$TMUX`/`$TMUX_PANE` environment that resolves. `ParseReport` is what reads the answer; a standing value that will not parse counts as disagreeing, so the repair still happens.

**What it costs, stated rather than discovered:**

- **One extra fork, on re-assertion writes only.** Today: two `Notification` types on claude, at most once per turn each, plus pi's `session_start` idle branch, which fires on a reload rather than on a turn. **`PreToolUse` — the hot hook, and the whole subject of open question 1 — pays nothing**, and opencode pays nothing at all. Both re-assertion `Notification`s fire while Claude is already idle and are registered async, so neither is on any turn's critical path.
- **A stale or buggy integration still storms.** The daemon cannot tell a re-assertion from a genuine finish — both are `1;idle;<ts>` — so this is a promise the writer keeps and the reader cannot check. It is the one place in this design where a reader-side invariant rests on writer-side behaviour, and the comment says so.

**Three alternatives that must not be built**, because each contradicts something this design holds:

- **Edge memory in the daemon** (remember the previous accepted report and collapse a resting run to its first timestamp) works while the daemon lives and fails on restart, where the memory is empty and the standing `1;idle;T+60` derives `T+60` all over again. It converts one storm per turn into one storm per restart, and spends the restart story stateless derivation was chosen to buy.
- **Demoting `idle_prompt` to ignored** costs nothing to build and loses the repair — which is the answer to the one attested objection anybody has raised against hook-sourced Claude state.
- **A fifth field carrying the finish time**, so a re-assertion could restate the original timestamp, is the same mechanism in a costume: the writer still has to read the standing report to learn what to restate.

**Step 5: Mutation testing**

| Mutant | Killed by |
| --- | --- |
| Classify `idle_prompt` as an edge | `a re-assertion reads first...` — **the badge storm mutant** |
| Classify pi's `session_start` idle branch as an edge | its row. Revision 3 of the design shipped exactly this by leaving the event unclassified |
| Make a re-assertion write the same state with a fresh timestamp | the `sets != 0` assertion. A test asserting only on the stored *state* passes this |
| Make a re-assertion silent even when it disagrees | `a re-assertion writes when it disagrees` — the missed-`Stop` repair |
| Classify a turn-end event as a re-assertion | `a turn-end event writes unconditionally`, plus: with a stale standing `idle` from the previous turn it would be silent and the turn would lose its badge |
| Remove any agent's turn-start `working` mapping | `TestEveryAgentHasATurnStartWorkingEdge` |
| Classify `PreToolUse` as a re-assertion | assert `shows == 0` for it — this is the fork-volume mutant, and open question 1 is about exactly that number |
| Treat an unparseable standing value as "agrees" | a test with `standing: "garbage"` must still write |

**Step 6: Commit**

```bash
git add cmd/wterm-web/events.go cmd/wterm-web/events_test.go cmd/wterm-web/report.go cmd/wterm-web/report_test.go
git commit -m "feat: a re-assertion reads the standing report before writing one"
```

---

### Task 15: The activity text, and the subagent filters

**Files:**
- Modify: `cmd/wterm-web/events.go`, `cmd/wterm-web/report.go`, and their tests
- Create: `cmd/wterm-web/activity.go`, `cmd/wterm-web/activity_test.go`

**The ladder.** The label is the first of these that is available, per agent:

1. **The question, when blocked.** pi's `ui_prompt_start.title` is the literal question; opencode's `permission.asked` carries the question plus metadata; Claude's `Notification` carries a message.
2. **The current step**, when opencode reports one — `todo.updated`'s `in_progress` entry. Better than a tool call when it exists: `Fix the parser bug` beats `read blocked.go`, because it is the agent's own statement of intent rather than a mechanical trace. **How often it is empty is open question 6** and does not change the ladder: rung 3 sits underneath it either way.
3. **The current tool call, verb plus object.**
4. **Nothing — an empty text field, not an absent report.** At turn end the integration reports `idle` with no text; the row falls back to the title and then to the command, and the *state* is still the agent's own.

**How much of a tool call the label carries.** This is the one rule the design's revision 6 changed, and it changed because the reason under it was false. The old rule reduced a `Bash` command to its first word on the grounds that "a tool's arguments are not a safe thing to publish" — a command line contains paths and occasionally a token, and the label lands in a tmux option and a web UI. **That is not a boundary.** Anything that can talk to the tmux server can already `capture-pane` the whole screen, and the browser at the other end is the owner's, over HTTPS, behind device enrolment. Do not reintroduce it. (v1 and v2 do keep `pane_current_path` out of the snapshot, and that rule is real, but its reason is **parsing**: tmux does not sanitise a path, so a raw newline in one forges a sidebar row and a raw `0x1f` swallows the next pane's record. `SanitizeActivity` is the answer to that, and it is already written.)

The rules, on display and budget grounds:

- A **path** argument is reduced to its **basename**: `read snapshot.go`, not `read /home/…/clients/…/snapshot.go`. The reason is the row, not secrecy — a full path eats most of the 128-rune budget and wraps badly in a 16rem sidebar, and its leading components repeat the window and session names above it. A **default for display**, not a boundary.
- A **shell command goes through whole**, bounded by `SanitizeActivity` and the `MaxActivity` cap and by nothing else. **There is no first-word reduction.** Under one, `rm -rf build` and `git push origin main` both render as a bare verb — and when the pane is sitting on a permission prompt, **the command is the question**, so reducing it destroys the one thing that row exists to answer.
- **Anything else is the tool name alone**, because there is no field here we recognise.
- **opencode's edit tool `metadata.diff` is not sent — on size.** It is unbounded, it would blow `MaxReportBytes`, and an over-cap value is discarded *whole* in `ParseReport`, so sending it costs the pane its entire report and drops it back to the classifier. Size, not secrecy.

The basename default is deliberately lossy and is allowed to be: the window name and the session name above the row already say which repo this is. **The second line describes; the first line identifies.**

**The user's raw prompt is never the label**, and for two reasons, neither of them about exposure:

- **Utility.** It answers the wrong question. Twenty minutes into a task a prompt is history — which is exactly the complaint about opencode's and claude's frozen titles that this whole feature exists to answer.
- **The budget.** `MaxActivity` is 128 runes. A long prompt arrives cut in half and says nothing, where a truncated tool call is still a tool call.

Revisions 1–5 of the design gave a third reason — that a prompt publishes the user's own words into an option anything on the box can read — and cited `pi-subagents` for it (*"raw prompts never enter pane metadata"*). **Revision 6 withdrew it as false; the decision stood, its stated reason did not.** Do not restate it in a comment here. `--label-from-prompt` exists as a flag in the integration and nothing else, default off, for an owner who wants it on their own machine.

**The subagent filters, and the honest version of what they buy.** `TMUX_PANE` is the same for a root session and its children on all three agents, so an unfiltered child event overwrites the root's state — and the specific damage is a child's turn end firing while the root is still working, which becomes a false `done` badge. **A report must only ever describe the root session in its pane.**

| Filter | Coding | Failure direction |
| --- | --- | --- |
| pi: `ctx.mode === "tui"` | presence | **Closed.** A missing or unexpected `mode` reports nothing |
| claude: `agent_id` absent | absence | **Open.** A payload that lost the field — renamed, nested, dropped in a refactor — reads as root |
| opencode: no `parentID` | absence | **Open.** Same |

**"Every one of these filters fails closed" is not implementable, and no comment here may claim it.** For two of the three agents the root session is coded by the *absence* of a field: Claude documents `agent_id` as present only inside a subagent call, and opencode's root is the session with no `parentID`. For those two, "missing means silence" silences all normal reporting. An absence-coded filter can be **tightened** but not inverted: the integration can require that the payload parsed, that it is the shape it expects, and that the other fields it knows about are present, so a wholesale schema change is caught rather than read as a root event. It cannot distinguish "absent because this is the root" from "absent because it moved."

**What lives where** (the same split the three integrations follow in Phase F): a filter needing runtime state or a runtime object stays in the integration — pi's `ctx.mode`/`ctx.isIdle()`, opencode's child-session map, which needs memory across events a fresh process cannot have. **Claude's `agent_id` check lives here, in Go**, because the payload arrives on stdin and each hook is a fresh process anyway. Go additionally re-checks whatever it can see in any payload it is handed: a `parentID` or an `agent_id` present in the JSON is refused whoever sent it. Two gates, both cheap.

**Two doors that are not closed, and no code here may pretend otherwise:**

- **A nested `claude` CLI in the same pane. Certain.** A `claude` spawned from a Bash tool call inherits `TMUX_PANE`, loads the same project `.claude/settings.json`, and is the **root of its own session** — so it fires its own perfectly legitimate `Stop` and writes `idle;<now>` onto a pane whose outer agent is still working. No field test can catch it: both processes are roots, `agent_id` is absent for both, and the two share nothing to compare. This is not hypothetical in this repo's own development. Open question 11.
- **Teammates and background sessions. Unknown.** They are not subagents: they have their own hook events (`TeammateIdle`, `TaskCreated`, `TaskCompleted`) which we do not register, they carry no `agent_id`, and they run under a supervisor with **no terminal attached** whose environment inheritance is undocumented. **No `TMUX_PANE` and the whole class is out of scope for free; a stale one and it is the nested-CLI door again.** Open question 9, measured in Task 19.

One more thing the payload says, because "`Stop` means nothing is happening" is the natural misreading: `Stop`'s input carries a `background_tasks` array. A `Stop` can fire with work still running under the session. It does **not** change what we report — the root session is waiting on the user, which is exactly what `idle` means here and exactly what the sidebar is being asked.

**Step 1: Write the failing tests**

```go
func TestActivityFromToolCall(t *testing.T) {
	for _, tc := range []struct{ name, tool, want string; input map[string]any }{
		{"a read becomes its basename", "Read", "read snapshot.go",
			map[string]any{"file_path": "/home/x/clients/acme/internal/tmux/snapshot.go"}},
		// A command line is carried WHOLE. There is no first-word reduction:
		// on a permission prompt the command is the question, and "run rm"
		// cannot answer "shall I run rm -rf build?".
		{"a bash command is carried whole", "Bash", "run go test ./internal/tmux -run TestX",
			map[string]any{"command": "go test ./internal/tmux -run TestX"}},
		{"a destructive command keeps its arguments", "Bash", "run rm -rf build",
			map[string]any{"command": "rm -rf build"}},
		// The ONLY bound on a command line, and the reason SanitizeActivity
		// and tmux.MaxActivity exist. Written against the constant, never a
		// literal 128.
		{"a long command line is bounded by the cap and nothing else", "Bash",
			"run " + strings.Repeat("x", tmux.MaxActivity-len("run ")),
			map[string]any{"command": strings.Repeat("x", 4<<10)}},
		{"an unknown tool is its own name", "SomeMcpTool", "SomeMcpTool",
			map[string]any{"whatever": "..."}},
		{"a tool with no recognised field is its own name", "Read", "Read", map[string]any{}},
	} { ... }
}

// The prompt is never the label unless the owner asked for it on their own
// machine.
func TestThePromptIsNotTheLabel(t *testing.T) { ... }

// The filters, tested for the DIRECTION they fail in rather than for a claim
// that they fail closed. A test asserting "no agent_id means silence" would be
// asserting the opposite of what ships -- and would silence all normal
// reporting.
func TestSubagentFilters(t *testing.T) {
	t.Run("claude: a payload carrying agent_id is refused", func(t *testing.T) { ... })
	t.Run("claude: a payload with no agent_id is the root and reports", func(t *testing.T) { ... })
	t.Run("claude: a payload that will not parse is refused", func(t *testing.T) {
		// The tightening an absence-coded filter CAN have: a wholesale schema
		// change is caught rather than read as a root event.
	})
	t.Run("opencode: a payload carrying parentID is refused", func(t *testing.T) { ... })
}
```

**Steps 2–4:** run, implement against **the recorded fixtures from Task 12** — particularly pi's `tool_execution_start.args`, whose shape is open question 7 and which the basename default was designed against a guess about — run again.

**Step 5: Mutation testing**

| Mutant | Killed by |
| --- | --- |
| Publish `file_path` whole | `a read becomes its basename` |
| **Reduce `command` to its first word** — the rule revision 6 deleted, and the one a reader who half-remembers the design will re-add | `a bash command is carried whole` **and** `a destructive command keeps its arguments`. Two rows, because the first alone is also killed by unrelated mutants and the second names the case that matters |
| Drop the `MaxActivity` cap on the command path, or apply it before the `run ` prefix is added | `a long command line is bounded by the cap and nothing else` — with the cap gone this is the only bound left, so no other row sees it |
| Fall back to the raw argument for an unknown tool | `an unknown tool is its own name` |
| Publish the prompt by default | `TestThePromptIsNotTheLabel` |
| Invert the claude filter to "report only when `agent_id` is present" | `claude: a payload with no agent_id is the root` — **this is the fails-closed mutant, and it silences the whole integration** |
| Drop the `agent_id` check | `a payload carrying agent_id is refused` |
| Accept a payload that failed to parse | `a payload that will not parse is refused` |

**Step 6: Commit**

```bash
git add cmd/wterm-web/activity.go cmd/wterm-web/activity_test.go cmd/wterm-web/events.go cmd/wterm-web/report.go
git commit -m "feat: verb plus object, a basename for a path, and never a raw prompt"
```

---

## Phase F — the three integrations, and the installer

**The division of labour is the same for all three, and it is the reason there is one binary.** The security-critical part of this feature is the sanitizer, and the naive shape implements it three times — once in TypeScript, once in JavaScript, once in something for claude. Three implementations of one security boundary, drifting apart, is not a thing to ship.

| | Stays in the integration file | Delegated to `wterm-web report` |
| --- | --- | --- |
| **pi** | `ctx.mode !== "tui"`; the root-session flag; the `ctx.isIdle()` re-derivation on `session_start`; the single-slot queue; spawning | which state an event means, the text ladder and its reductions, the sanitizer, edge-versus-re-assertion **including the read of the standing option**, the timestamp, and the tmux write |
| **opencode** | the child-session map (`properties.info.parentID`) and the root-only gate; the queue; spawning | everything else, as above |
| **claude** | four hook entries with `"async": true` and a three-line wrapper that spawns and does not wait | **everything**, including the `agent_id` subagent filter and the whole `notification_type` whitelist |

Only what needs a **runtime object** (pi's `ctx`) or **memory across events** (opencode's child-session map) stays outside Go. Each integration is then short enough for a user to read before trusting it — and reviewability is the whole of the trust model here.

**What an integration is permitted to do is one thing: spawn `wterm-web report`.** No network, no filesystem writes, no reading the repository, no dependencies.

### Task 16: The single-slot queue

**Files:**
- Create: `internal/integrations/queue.ts`, `internal/integrations/queue.test.ts`
- Modify: `web/vitest.config.ts` (one line in `include`)

**This logic has no test story today, which is how it would ship untested.** It goes in a tiny pure module with the spawn injected. Two exports, and Tasks 17 and 18 both import them rather than reimplementing either:

- `makeQueue(spawn)` — the slot. `spawn(item)` returns a promise; the queue never looks inside an item.
- `spawnReport(agent)` — the argv, in one place: it returns a `spawn` that runs `wterm-web report --agent <agent> --event <item.event>` with `JSON.stringify(item.payload ?? {})` on stdin. **Its promise settles when the child exits, and it never rejects** -- a failed report is silence, never a thrown error inside an agent's hook. Do not settle it at spawn time: the queue's whole guarantee is that the collapsed spawn starts only after the in-flight one has *finished*, so that the process it starts stamps a strictly later millisecond. Settle early and "finished" degrades to "spawned", two children can stamp out of order, and the daemon's ordering filter silently drops the newer state -- the exact failure this queue exists to prevent, and one no test here can catch, because the queue's own tests inject the spawn. The queue waits; the handlers never do. Neither integration builds a command line of its own.

**`queue.ts`'s body must be valid JavaScript** -- no type annotations, no TS-only syntax. Both distribution shapes in Task 20 deliver this file into a `.js` context, so TS-only syntax there forces Task 20 to strip types or to reopen a file it has already shipped.

**What it is for:** a burst of tool calls must not become a queue of forks. At most one `report` in flight per pane; if a new state arrives while one is running, keep only the latest and drop what it replaced. Both pi and opencode are long-lived runtimes and can hold the slot in module scope. **Claude Code gets no queue**, because each hook is a fresh process and there is nowhere to put one; the daemon's ordering rule is what stands in for the queue it cannot have.

**The source of truth is `internal/integrations/`,** because that is what the installer `//go:embed`s. The test lives beside it and vitest is pointed at it:

```ts
// web/vitest.config.ts
      include: ['src/**/*.test.ts', 'src/**/*.test.tsx', '../internal/integrations/*.test.ts'],
```

**Verify that vitest actually collects it — do not assume.** `pnpm test` must report the new file by name. If it refuses to reach outside its root, the fallback is a second vitest project rather than a copy of the module: two copies of this file is the thing the task exists to prevent.

**Step 1: Write the failing test**

**The item carries no timestamp, and there is no place in this design for one to come from.** An item is exactly what `report` needs on its command line and its stdin — `{ event, payload }` — and the timestamp is stamped by `report` itself, from `time.Now()` at process start (Task 6, where that is spelled out as deliberate). Neither `pi.ts` nor `opencode.js` has an event timestamp to pass, and nothing downstream would read one. So the queue's ordering guarantee is **serialization, not stamping**: the collapsed spawn starts only after the in-flight one has finished, so the process it starts stamps a strictly later millisecond, and the daemon's ordering filter sees the two states in the order the events happened. Say that in the module comment; a reader who assumes the queue carries times will add a field nothing fills.

```ts
it('keeps exactly one report in flight and collapses a burst to the newest', async () => {
  const q = makeQueue(spy)          // the spawn is injected
  q.push({ event: 'tool_execution_start', payload: { tool: 'read' } })
  q.push({ event: 'tool_execution_start', payload: { tool: 'edit' } })
  q.push({ event: 'tool_execution_start', payload: { tool: 'bash' } })
  q.push({ event: 'tool_execution_start', payload: { tool: 'grep' } })
  q.push({ event: 'ui_prompt_start', payload: { title: 'Approve?' } })
  await settle()
  // Five events, one in flight: exactly one queued, and it is the newest.
  expect(spy.calls).toHaveLength(2)
  expect(spy.calls[1].event).toBe('ui_prompt_start')
})

it('starts the queued spawn only after the in-flight one has finished', async () => {
  // This is what the queue gives the daemon's ordering filter, and it is the
  // whole of it: `report` stamps its own timestamp at process start, so two
  // reports that overlap could be stamped in either order, and the filter
  // refuses anything not newer than what it accepted -- i.e. it would silently
  // drop the newer STATE for having the older stamp.
  //
  // Assert on the spawn's lifecycle, not on a field: resolve the first spawn's
  // promise by hand and check that spy.calls is still length 1 until you do.
})

it('never rejects, whatever the spawn does', async () => {
  // An integration that throws inside an agent's hook is worse than one that
  // reports nothing: pi and opencode await handlers with NO timeout.
})

it('does not await the spawn', async () => {
  // Spawn, do not await. The spawn itself is a few milliseconds and that is
  // the budget. A 3s stall in pi's `input` delayed the turn by 3.03s; 4s in
  // opencode's chain delayed session.status busy to 8956ms.
})
```

**Steps 2–4:** run, implement, run.

**Step 5: Mutation testing**

| Mutant | Killed by |
| --- | --- |
| Queue everything (an array rather than a slot) | the burst test's call count |
| Keep the **oldest** queued rather than the newest | `spy.calls[1].event` |
| Spawn the queued item immediately rather than after the in-flight one resolves | `starts the queued spawn only after the in-flight one has finished` |
| `await` the spawn | `does not await the spawn` |
| Let a spawn failure reject | `never rejects` |
| Drop the newest instead of the in-flight one when both exist | the burst test — the last call must be `ui_prompt_start`, not a `tool_execution_start` |

**Step 6: Commit**

```bash
git add internal/integrations/queue.ts internal/integrations/queue.test.ts web/vitest.config.ts
git commit -m "feat: one report in flight per pane, collapsing a burst to the newest"
```

---

### Task 17: The pi extension

**Files:**
- Create: `internal/integrations/pi.ts`, `internal/integrations/pi.test.ts`
- Create: `cmd/wterm-web/integration_pi_test.go` (the smoke test, against a recording stub)

**Step 1: Verify the extension API by running it. Do not assume it.**

```bash
# In your own scratch project, with pi's own config dir pointed elsewhere.
mkdir -p <scratch>/proj/.pi/extensions
# an extension that logs its own registration surface and one event's shape
```

What must be confirmed before a line of the real file is written: the module's export shape, that `pi.on("<event>", (event, ctx) => …)` is the registration form, that `ctx.mode` and `ctx.isIdle()` exist, and the event names — `session_start`, `input`, `tool_execution_start`, `ui_prompt_start`, `agent_settled`. Record what you saw in the commit message. If any of them differs from the design's table, **the fixture wins and you say so.**

**Step 2: Write it**

```ts
// managed by tmux-web (wterm-schema: 1)
// Reinstalling or updating the integration overwrites this file.
// It does one thing: `tmux set-option -p @wterm_agent`. Nothing else.

import { makeQueue, spawnReport } from "./queue.ts"

// handlers is exported, and that export is what makes this file testable at all.
// Its three filters -- the mode gate, the root flag and agent_settled's
// isIdle check -- are the only logic in the integration, and the Go wiring test
// that would otherwise be their only cover is allowed to t.Skip when pi is not
// installed. Driven here with a fake `report`, they are covered on every run.
export function handlers(report) {
  let root = false

  return {
    session_start: async (event, ctx) => {
      // TUI only. RPC/JSON/print modes are headless, and RPC reports hasUI=true,
      // so `mode` is the reliable gate and `hasUI` is not. This is the ONE filter
      // of the three that fails CLOSED: a missing or unexpected mode reports
      // nothing.
      if (ctx?.mode !== "tui") return
      root = true
      // A reload can replace this extension mid-run without another
      // agent_start, so an extension that only ever sets state on transitions
      // comes back from a reload believing nothing is happening.
      //
      // The idle branch is a RE-ASSERTION, not an edge -- a reload can recur
      // arbitrarily often inside one resting period, so as an edge it would
      // write idle;<now> and re-badge every device on every reload. `report`
      // knows that from its own table; the extension just names the event.
      report({ event: "session_start", payload: { idle: ctx?.isIdle?.() === true } })
    },

    // The turn-start invariant. Without this write, agent_settled's write is
    // suppressed and that turn loses its badge.
    input: () => root && report({ event: "input" }),
    tool_execution_start: (e) => root && report({ event: "tool_execution_start", payload: e }),
    ui_prompt_start: (e) => root && report({ event: "ui_prompt_start", payload: e }),
    agent_settled: (_e, ctx) => root && ctx?.isIdle?.() === true && report({ event: "agent_settled" }),
  }
}

// What pi loads. It owns one thing the factory does not: the real queue.
export default function (pi) {
  const q = makeQueue(spawnReport("pi"))
  for (const [name, fn] of Object.entries(handlers((item) => q.push(item)))) pi.on(name, fn)
}
```

The item shape is Task 16's exactly — `{ event, payload }`, no timestamp — and `spawnReport` owns the argv, so this file builds no command line and carries no state name. **Nothing else is in this file.** No state machine, no mapping table, no string handling.

**Step 3: The test story**

Four layers, and none of them is "read it and hope". Layer 0 is `internal/integrations/pi.test.ts`, named in this task's Files — **it has real content and here is what it is**, because an unspecified test file in a Files list becomes a "registers five handlers" mock test that asserts nothing:

0. **The filters**, in vitest, against the exported `handlers(report)` with a fake `report` that records items. Four cases, and they are the three mutants below that the wiring test can only cover when pi happens to be installed:
   - `session_start` with `ctx.mode = "rpc"` records **nothing**, and the `input` handler that follows it records nothing either — the fails-closed gate and the `root` flag in one fixture.
   - `session_start` with `ctx.mode = "tui"` records a `session_start` item whose payload is `{ idle: … }`, and then `input` records an `input` item.
   - `agent_settled` with `ctx.isIdle() === false` records nothing; with `true` it records.
   - Every recorded item is `{ event, payload? }` and nothing else: no state name, no timestamp, no text. That is the file's one-line contract with Go, and it is the assertion that catches somebody "helpfully" adding `--text` here.

1. **The queue** is Task 16's, already covered.
2. **The mapping** — which state each event means, what text comes out of `tool_execution_start.args`, whether a payload is refused — is a **Go** table test over Task 12's recorded fixtures. That is where it belongs: it is the same table all three agents share.
3. **The wiring** — that the extension registers those five events and spawns with the right argv — is a Go smoke test with a **recording stub named `wterm-web` on `PATH`** that appends its argv and stdin to a file. Run pi in a throwaway project with the extension installed, drive one turn, assert the recorded calls. **If pi is not installed on the machine, skip loudly** (`t.Skip` with a message naming what was not run) rather than silently — but do not fake it: a wiring test against a fake runtime tests the fake.

**What cannot be tested in CI**: that a real agent, running a real integration, produces the events we mapped. That is a manual check per agent per upgrade, and the honest mitigation is that a wrong mapping degrades to no report, which degrades to v2.

**Step 4: Mutation testing** (against layers 0, 2 and 3)

Every row names a test that runs unconditionally where one exists: layer 3 is allowed to skip, so a mutant whose only cover is the wiring test is a mutant nobody verifies on a machine without pi.

| Mutant | Killed by |
| --- | --- |
| Drop the `ctx?.mode !== "tui"` gate | layer 0's `rpc` case; the wiring test driven in a non-TUI mode seconds it |
| Invert it to `=== "tui"` returning early | layer 0's `tui` case — no items recorded |
| Drop the `input` handler | layer 0's `tui` case, and **the turn-start invariant** in the wiring test: drive a turn and assert the recorded sequence *starts* with a `working`-producing event. Without this, `agent_settled` is suppressed |
| Drop the `root` guard on any handler | layer 0's `rpc` case: `input` after a refused `session_start` must record nothing |
| `agent_settled` without the `ctx.isIdle() === true` check | layer 0's `isIdle() === false` case; a settled-child run in the wiring test seconds it |
| Pass the raw payload as `--text` instead of on stdin | layer 0's item-shape assertion **and** the Go smoke test's argv assertion — the reductions and the sanitizer live in Go and must not be bypassed |

**Step 5: Commit**

```bash
git add internal/integrations/pi.ts internal/integrations/pi.test.ts cmd/wterm-web/integration_pi_test.go
git commit -m "feat: a pi extension that maps events and delegates everything else"
```

---

### Task 18: The opencode plugin

**Files:**
- Create: `internal/integrations/opencode.js`, `internal/integrations/opencode.test.ts`
- Create: `cmd/wterm-web/integration_opencode_test.go`

**Step 1: Verify the plugin API by running it. Do not assume it.** What must be confirmed: the export shape (an async factory returning a handler object), that `"chat.message"` and `event` are the hook names, and the event `type` strings — `session.status`, `tool.execute.before`, `permission.asked`, `todo.updated`, `session.idle`. And the one this task turns on: **where `parentID` appears**, and whether `session.idle` carries anything identifying the session.

**Step 2: Write it**

```js
// managed by tmux-web (wterm-schema: 1)
// ...

// Child sessions arrive in every hook and TMUX_PANE is the same for all of
// them, so an unfiltered child event overwrites the root's state -- and the
// damage that matters is a child's turn end firing while the root is still
// working, which becomes a false done badge.
//
// This map is why the filter lives here and not in Go: deciding whether
// session X is a child needs memory of an earlier event, and every `report`
// process is fresh.
//
// It is ABSENCE-CODED and therefore fails OPEN: a payload that lost parentID
// -- renamed, nested, dropped in a refactor -- reads as root. That cannot be
// inverted, because "missing means silence" silences all normal reporting. It
// can only be tightened, and it is: a payload that does not parse, or does not
// carry the fields we expect, is refused.
//
// EXPORTED, and for the same reason pi.ts exports its handlers: this is the
// only real logic in the file, and the Go wiring test that would otherwise be
// its only cover is allowed to t.Skip when opencode is not installed. A
// module-scope `const` inside the plugin factory cannot be reached from
// opencode.test.ts, and the test named in this task's Files is then either not
// written or written against a copy.
export function sessionTree() {
  const parents = new Map() // session id -> parent id, for sessions we have seen

  return {
    // note records what one payload says about parentage. Called on every
    // event, before the root gate.
    note(payload) { /* ... */ },
    // isRoot walks the chain to the top. An id we have never seen is root:
    // absence-coded, failing open, as above.
    isRoot(sessionID) { /* ... */ },
  }
}

// What opencode loads. One tree and one queue per plugin instance.
export default async function () {
  const tree = sessionTree()
  const q = makeQueue(spawnReport("opencode"))
  // ... the handlers, each gated on tree.isRoot(...) and each pushing
  // { event, payload } -- Task 16's item shape, no timestamp, no state name.
}
```

Handlers: `"chat.message"` and `event` → `session.status` (busy is the **turn-start `working` edge**; the invariant), tool events, `permission.asked`, `todo.updated`, `session.idle`. Each one root-only, each one pushed through the queue, each one delegating to `wterm-web report --agent opencode --event <type>` with the payload on stdin.

**Step 3: The test story** — the same four layers as pi, and layer 0 here is the bigger half. The **child-session map is pure and gets its own vitest test** in `internal/integrations/opencode.test.ts`, driving the exported `sessionTree()` directly: a `session.created` with a `parentID` registers a child, `isRoot` is false for that child and true for the root, a chain of nested children resolves to the root, and an id never seen reads as root (the fail-open direction, asserted deliberately so that inverting it goes red). That is the half of the filter that has real logic in it. As with pi, every item the handlers push is `{ event, payload? }` and nothing else.

**The honest statement of what this buys, which goes in the file's comment:** behind this filter there is exactly one thing — evidence rule 3, which needs a connected client, needs the pane to keep churning for a whole window, and leaks at a measured rate even then (4 of 88 turns went still while waiting on the model). **With no client connected, a subagent false-idle on opencode is undefended.** That is the true state of it; it is not two independent mechanisms, it is one mechanism that only runs when somebody is watching.

**Step 4: Mutation testing**

| Mutant | Killed by |
| --- | --- |
| Drop the child-session map | the pure test: a child's `session.idle` reports |
| Register the child but not resolve a nested chain | the nested-chain row |
| Invert the filter to "report only when `parentID` is present" | the root rows — **the fails-closed mutant, which silences everything** |
| Drop the `session.status busy` handler | the turn-start invariant assertion on the recorded sequence |
| Map `session.status idle` to `idle` as well as `session.idle` | decide and assert: two turn-end events inside one resting period would make each a re-assertion rather than an edge |
| Report on `session.error` | not mapped; a payload for it must record no call |

**Step 5: Commit**

```bash
git add internal/integrations/opencode.js internal/integrations/opencode.test.ts cmd/wterm-web/integration_opencode_test.go
git commit -m "feat: an opencode plugin whose only state is which sessions are children"
```

---

> **Carried from Task 18's implementer, measured and not yet fixed.** opencode's `session.idle` fired
> **twice inside one resting period** on a turn that died at the provider: idle, `message.updated`, idle,
> about a second apart, with no `busy` between them. By this design's own criterion -- any event that can
> recur within one resting period and maps to a resting state is a re-assertion -- that makes it a
> re-assertion, not the edge `events.go` currently classifies it as. Cost is small and bounded: a finish
> re-dated by roughly a second, on failed turns only. The fix is one row in `events.go`'s kind table
> (Tasks 13/14's surface), not in the plugin. **Do not fix it inside Task 19** -- it is recorded here
> because this is the next task anyone reads, and it needs its own commit and its own mutant.

> **Also from Task 18, a real coverage loss rather than a defect.** A subagent's `permission.asked` is
> filtered out with the rest of the child's traffic -- but that dialog is drawn on the **root's** screen and
> a human has to answer it. Root-only is what this plan specifies and it is the fail-safe direction, since
> a wrongly-claimed `blocked` never expires. The cost is that the badge is gone unless a client is
> connected for evidence rule 2 to find the dialog on screen. Belongs on the feature backlog beside the
> four uncaptured `notification_type` screens.

### Task 19: The Claude Code hooks

**Files:**
- Create: `internal/integrations/claude-report.sh`, `internal/integrations/claude_hooks.go` (the settings block as data), and their tests

**`claude_hooks.go` is also this directory's `//go:embed` host, and that is not an aside — Task 20 cannot work without it.** `//go:embed` reads only from the directory of the file that declares it and its subtree: `cmd/wterm-web` **cannot** embed `../../internal/integrations`, and a path with `..` in it is a compile error, not a lookup that fails at run time. Until this task there is no `.go` file in `internal/integrations` at all — Tasks 16, 17 and 18 put only `.ts` and `.js` there — so this file is where `package integrations` begins. Give it the directives and the exported accessors the installer will use:

```go
package integrations

// The files the installer writes, embedded here because this is the only
// package that can embed them: //go:embed cannot climb out of its own
// directory, so cmd/wterm-web has no way to reach these bytes. Task 20 is the
// only consumer.
//
//go:embed pi.ts opencode.js queue.ts claude-report.sh
var files embed.FS
```

`queue.ts` is in that list deliberately: both integrations import it (Task 16), so the installer has to write it beside them. Task 20 decides the layout; this task only has to make sure the bytes are reachable.

**The hook set is small and closed**: `UserPromptSubmit`, `PreToolUse`, `Notification`, `Stop`, **and nothing else**. In particular **`SubagentStop` is never registered** — that is the whole of what the structural guard against Task-tool subagents buys, and it buys nothing against the two other doors.

```json
{
  "hooks": {
    "UserPromptSubmit": [{ "hooks": [{ "type": "command", "async": true, "command": "<bin> UserPromptSubmit" }] }],
    "PreToolUse":       [{ "hooks": [{ "type": "command", "async": true, "command": "<bin> PreToolUse" }] }],
    "Notification":     [{ "hooks": [{ "type": "command", "async": true, "command": "<bin> Notification" }] }],
    "Stop":             [{ "hooks": [{ "type": "command", "async": true, "command": "<bin> Stop" }] }]
  }
}
```

**`"asyncRewake"` is never set. On any hook this project installs, ever.** An `"async": true` command hook has its exit code **ignored, including exit 2** — the docs give "exit code 2 wakes Claude" as the thing `asyncRewake` adds, and the timeout section says enforcement is skipped for an async command hook. That is documented *adjacent* rather than verbatim, so it is **belt to the exit-0 rule's braces and not a replacement for it**: two independent things would have to change before an exit code could hurt, and the second is a line in a file the user owns. Setting `asyncRewake` would re-arm the one sharp edge.

The wrapper, and it is three lines because everything else is in Go:

```sh
#!/bin/sh
# managed by tmux-web (wterm-schema: 1)
# Reinstalling or updating the integration overwrites this file.
# It does one thing: `tmux set-option -p @wterm_agent`. Nothing else.
BIN=<resolved at install time>
[ -x "$BIN" ] || exit 0     # moved, uninstalled, or never built: say nothing
exec "$BIN" report --agent claude --event "$1"
```

**Step 1: The test story**

- **Everything with logic is a Go table test** over Task 12's recorded payloads: which hook means which state, the whitelist, the edge/re-assertion split, the `agent_id` filter. That is Tasks 13–15 and it is already written; this task adds the payload rows for each hook.
- **The wrapper gets a shell-level test**, because its whole job is to fail quietly: with `$BIN` missing, with `$TMUX` unset, with garbage on stdin, and with no arguments, it must **exit 0 and write nothing to stdout**, promptly.
- **The settings block gets a JSON test**: exactly four hooks, every one `"async": true`, **no key named `asyncRewake` anywhere in the generated block**, and `SubagentStop` absent.

```go
func TestGeneratedClaudeHooks(t *testing.T) {
	block := claudeHookBlock("/opt/wterm/report.sh")
	// A string search over the marshalled JSON, deliberately: a struct-field
	// assertion cannot see a key somebody adds to a map later.
	if strings.Contains(string(block), "asyncRewake") {
		t.Fatal("asyncRewake must never be set: it re-arms exit-code handling, " +
			"including the exit 2 that blocks a PreToolUse tool call")
	}
	if strings.Contains(string(block), "SubagentStop") {
		t.Fatal("SubagentStop must never be registered: not registering it is the " +
			"whole of the structural guard against Task-tool subagents")
	}
	// Every hook async, and exactly the four.
}
```

**Step 2: Measure open question 1 — `PreToolUse` fork volume. Do not skip this, and do not decide it from the docs.**

What is *not* in question: the payload (`tool_name` plus a structured per-tool `tool_input`) and `"async": true` are both documented, and latency is off the critical path by construction. What is unmeasured is **process volume under a burst** — one hook process plus one tmux client per tool call.

```bash
# Isolated config dir, isolated tmux socket, your own project.
# A prompt that forces a long run of small tool calls, e.g. "read each of these
# 40 files and tell me the line count of each".
# While it runs, in another pane:
while :; do pgrep -c -f 'report --agent claude' 2>/dev/null; sleep 0.2; done | sort -n | tail -1
# and afterwards: how many hook invocations, over what wall-clock window
wc -l <scratch>/out/PreToolUse.log
```

Record the peak concurrent count and the invocations-per-second. **What the answer decides is a trade, not a threshold**: `PreToolUse` is what makes Claude's connected-case blocked badge about six seconds slow instead of about 1.5, because its fresh `working` suppresses the capture `IsBlocked` would have read. So the choice is "the activity line and the disconnected-case keepalive" against "a 4.5-second faster badge whenever somebody is watching". **The recommendation still goes to `PreToolUse`** — the disconnected case is the product. If the volume comes out badly, the fallback is **deleting one entry from the generated block** and nothing else: Claude ships state-only, every other Claude behaviour here is unchanged, and the activity line is what is lost.

**Step 3: Measure open questions 9 and 11 — does a teammate, background-session or nested-CLI hook process carry `TMUX_PANE`?**

This is the one most likely to bite, and it decides whether a whole class needs a guard or is out of scope for free.

```bash
# In the isolated config dir, TEMPORARILY register the recorder on the teammate
# and background-session hooks as well -- TeammateIdle, TaskCreated,
# TaskCompleted -- purely to see the environment. Production registers none of
# them.
# Then, from inside a claude session in a tmux pane:
#   (a) start a background session / a teammate, and let it finish
#   (b) from a Bash tool call, run a nested `claude -p 'say hi'`
# Afterwards, read the --- ENV --- blocks:
grep -A4 'ENV' <scratch>/out/*.log | grep -E 'TMUX_PANE|^--'
```

Three outcomes and what each means:

- **No `TMUX_PANE`** on those processes: `report` no-ops and **the entire teammate/background class is out of scope for free, with no guard to write.** Record it and move on.
- **A stale `TMUX_PANE`** inherited from the dispatching pane: it is the nested-CLI door again, and `Stop` is not the only hook that could come through it. Record it; **do not invent a guard here.** The candidate fixes — a writer-identity field, refusing to report when `pane_current_command` is not the agent doing the reporting, or accepting that the innermost agent owns the pane — all want measuring first, and all are design changes.
- **Its own `TMUX_PANE`**: report that too; it is the least expected of the three.

**The nested-CLI case is already known and is not fixed by anything in this plan.** A `claude` spawned from a Bash tool call inherits `TMUX_PANE`, loads the same project `.claude/settings.json`, and is the root of its own session — so it fires a legitimate `Stop` and writes `idle;<now>` onto a pane whose outer agent is still working. Both processes are roots, so `agent_id` is absent for both and no field test can separate them. It is not hypothetical in this repo's own development. Record it in the commit message as a known hole, with the measurement result beside it.

**Step 4: Mutation testing**

| Mutant | Killed by |
| --- | --- |
| `"asyncRewake": true` added anywhere | the string search |
| `"async"` dropped from any hook | the per-hook assertion |
| `SubagentStop` registered | the string search |
| A fifth hook added | the exact-count assertion |
| The wrapper's `[ -x "$BIN" ]` guard removed | the shell test with `$BIN` missing — it exits 127 |
| The wrapper exiting nonzero on any path | every row of the shell test |
| The wrapper writing to stdout | the shell test. A `PreToolUse` hook's stdout is read for a JSON `permissionDecision` |

**Step 5: Commit** — with the two measurements in the message.

```bash
git add internal/integrations/claude-report.sh internal/integrations/claude_hooks.go internal/integrations/claude_hooks_test.go
git commit -m "feat: four Claude hooks, async, and never asyncRewake"
```

---

### Task 20: `wterm-web install-integration`

**Files:**
- Create: `cmd/wterm-web/install.go`, `cmd/wterm-web/install_test.go`
- Modify: `cmd/wterm-web/cli.go` (dispatch and usage)
- Modify: `internal/front/server.go` (a comment at the mux, and nothing else — see the prohibition test below)

**Installation is a CLI act, and it is never a button in the web UI.** Not a "we detected claude, shall we…" prompt either. The web UI is reachable over the network from a phone, and "write executable code into a repo" is not a thing a network request should be able to do however well authenticated it is. The Origin middleware is the boundary for tmux operations; **this is not a tmux operation.**

```
wterm-web install-integration --agent claude|opencode|pi [dir] [--global] [--yes] [--remove]
```

It prints the exact paths it will write and requires confirmation unless `--yes`.

| Agent | Path | Kind |
| --- | --- | --- |
| pi | `<proj>/.pi/extensions/wterm.ts` | A file we own. TypeScript via jiti, no build step. Needs project trust |
| opencode | `<proj>/.opencode/plugin/wterm.js` | A file we own. Auto-loaded, no config entry needed |
| claude | `<proj>/.claude/settings.json` | **A file the user owns**, merged into |

**Where the bytes come from.** `cmd/wterm-web` imports `internal/integrations` and reads them out of the `embed.FS` Task 19 declares there. It does **not** embed them itself: `//go:embed` cannot reach outside its own directory, so a directive in `cmd/wterm-web` naming `../../internal/integrations/pi.ts` does not compile. If that package or its directives are missing, the task to fix is Task 19, not this one.

**One thing this task has to settle and must not settle by guessing: how the queue module gets to the destination.** `pi.ts` and `opencode.js` both `import { makeQueue, spawnReport } from "./queue.ts"` (Task 16), so writing one file per agent leaves a dangling import. Two shapes, and the choice is a measurement, not a preference:

- **Write `queue.ts` beside the integration** — `.pi/extensions/wterm-queue.ts`, `.opencode/plugin/wterm-queue.js` — with its own managed header, subject to the same ownership rules, and removed by `--remove` along with its sibling. Requires the import specifier in the written file to match the written name, and requires each runtime to accept it.
- **Inline it at install time**, so each agent gets exactly one self-contained file. One source of truth is preserved (the concatenation happens in Go from the embedded bytes), and there is no import to resolve at all.

**Verify before choosing**, because the deciding fact is whether opencode's runtime will load a `.js` plugin that imports a `.ts` sibling — bun will, node will not, and which one opencode uses under the user's install is not something to assume. Both runtimes are on this machine. Record what you ran and what it printed in the commit message, exactly as Task 3 does for tmux. Whichever shape wins, `--remove` must leave nothing of ours behind, and the "not ours, refuse" rule applies to every file written.

**The claude case is different in kind and the installer must treat it that way.** It refuses if the file is not valid JSON, adds only entries whose command is recognisably ours, and removes exactly those on uninstall. What it promises about the rest of the file is that **nothing of the user's is lost or changed in meaning** — not that the bytes are preserved, which `encoding/json` cannot do; see the test below for why that line is drawn there and what is scoped instead.

**Every file we own carries a managed header, and the schema line is what makes `install` safe:**

```
// managed by tmux-web (wterm-schema: 1)
// Reinstalling or updating the integration overwrites this file.
// It does one thing: `tmux set-option -p @wterm_agent`. Nothing else.
```

Three cases it lets `install` tell apart: **ours and current** (overwrite silently), **ours and outdated** (overwrite, say so), **not ours** (refuse, and name the file). The last case is what the header exists for.

**Uninstall is `--remove`**, and for the two file-based agents `rm` also works and is documented as working. An integration you cannot remove with `rm` is one you have to trust more than this one deserves.

**Scope, and one thing that must not be implemented on the strength of a sentence:**

- pi and opencode are project-local by mechanism, so an install is per project; pi additionally requires the project to be trusted.
- Claude Code hooks can also live in `~/.claude/settings.json`, so **`--global` is offered for claude**.
- opencode *may* have a global plugin directory (`~/.config/opencode/plugin/`). That is **unverified** — open question 5 — and **`--global` is not offered for opencode.** Either verify it in this task and record the result, or leave it unimplemented; do not implement it on the strength of the design's sentence, which says in as many words that it must not be.

**One consequence the installer warns about before writing, and it is measured rather than inferred:** opencode auto-creates `.opencode/package.json`, `node_modules/` and a **`.gitignore`** in the project on first plugin load. Installing into a client repository therefore adds files that repository did not have, and a `.gitignore` tmux-web did not write and does not control. Say so before writing, because "why is there a `.gitignore` in my client's repo" is a question the user should be able to answer without archaeology.

**Nested project installs are open question 8** — a repository with an installed integration and a working directory below it that also has one, or a parent directory's plugin being loaded at all. Two integrations writing one pane option is a race nobody has looked at. **Warn when you can detect it** (an ancestor directory already carries one of our managed files) and do not attempt to resolve it.

**The binary's path is resolved at install time** and written into the generated file, with a `PATH` lookup as fallback. If it is missing at run time, the integration does nothing and says nothing.

**Step 1: Write the failing tests**

```go
func TestInstallRefusesAFileItDoesNotOwn(t *testing.T) {
	// A hand-written .pi/extensions/wterm.ts with no managed header.
	// Refused, the file untouched, and the error NAMES the file.
}

func TestInstallOverwritesItsOwnFile(t *testing.T) { /* current: silent; outdated: says so */ }

func TestClaudeMergePreservesTheUsersOwnHooks(t *testing.T) {
	// A settings.json with the user's own PreToolUse entry and an unrelated
	// top-level key. After install: our four entries are present, THEIR entry
	// is still there, the unrelated key is still there with the same value.
	// After --remove: ours are gone, theirs is still there.
	//
	// SEMANTICALLY identical, compared as parsed JSON -- not byte-comparable,
	// and the difference is a scoping decision rather than a slack assertion.
	// encoding/json does not preserve key order or formatting, so a promise
	// that the user's file comes back byte-for-byte is a promise to write a
	// format-preserving JSON editor: a token walk, or json.RawMessage surgery
	// with the key order recovered by a Decoder. That is a real piece of work
	// and it is NOT scoped here. What bounds the damage instead is that we
	// touch this file at all only under the rules above -- valid JSON or
	// refuse, our entries only, named by our command.
}

// The one place a byte assertion belongs, and it is cheap to honour: a merge
// that would change nothing must not write.
func TestAMergeThatChangesNothingDoesNotTouchTheFile(t *testing.T) {
	// Install twice; the second run must leave the file byte-identical (compare
	// the contents, and the mtime if the implementation makes that meaningful).
	// Likewise --remove when none of our entries are present.
	//
	// This is what stops "it only reformats the file when something changed"
	// from quietly becoming "it reformats the file every time anybody runs it",
	// which is the wholesale-rewrite mutant wearing a different hat.
}

func TestClaudeRefusesInvalidJSON(t *testing.T) {
	// Not valid JSON: refuse, do not rewrite, do not "fix". The file is the
	// user's.
}

func TestInstallIsNeverReachableFromHTTP(t *testing.T) {
	// NOT a route-table assertion. http.ServeMux exposes no way to enumerate
	// its patterns, so "no handler anywhere serves installation" is not a
	// question any Go test can ask it.
	//
	// Two things stand in its place, and being honest about which is which
	// matters more than the test:
	//
	//  1. The compiler already enforces the direct half. This installer lives
	//     in `package main`, and a package main cannot be imported -- so
	//     internal/front CANNOT call it, today or ever, without somebody first
	//     moving it. That is a stronger guarantee than any assertion here.
	//  2. This test covers the other half: a REIMPLEMENTATION inside
	//     internal/front. It is a grep -- it reads internal/front/*.go and
	//     fails if the install symbols (install-integration, claudeHookBlock,
	//     wterm-schema, the .pi/.opencode/.claude paths) appear there at all.
	//     A grep is a blunt instrument and this comment says so plainly rather
	//     than dressing it up: what it really buys is that somebody adding an
	//     install route has to delete a test with this comment in it.
	//
	// The prohibition itself also goes where the routes are built, in a comment
	// at internal/front/server.go's mux (server.go:227), because that is where
	// the person who would add the route is reading.
}

func TestNoGlobalForOpencode(t *testing.T) {
	// --global --agent opencode is refused with a message saying it is
	// unverified, not silently ignored.
}
```

**Steps 2–4:** run, implement, run.

**Step 5: Mutation testing**

| Mutant | Killed by |
| --- | --- |
| Overwrite a file with no managed header | `TestInstallRefusesAFileItDoesNotOwn` |
| Match the header by prefix rather than by the schema line | add an outdated-schema fixture; it must be detected as ours-but-outdated, not as not-ours |
| Rewrite claude's `settings.json` wholesale (marshal the parsed map back, dropping the user's other keys) | `TestClaudeMergePreservesTheUsersOwnHooks` on the unrelated top-level key |
| Rewrite it on every run, including one that changes nothing | `TestAMergeThatChangesNothingDoesNotTouchTheFile` |
| `--remove` deleting every hook rather than ours | the same test's second half |
| `--remove` matching by hook *name* rather than by our command | a fixture where the user has their own `PreToolUse` entry: it must survive |
| Accept `--global --agent opencode` | `TestNoGlobalForOpencode` |
| Reimplement any of this inside `internal/front` | `TestInstallIsNeverReachableFromHTTP`'s grep. Apply it for real — paste `claudeHookBlock` into a file under `internal/front` — and watch it go red, because a grep test that matches nothing is the easiest vacuous test in this plan to write by accident |
| Skip the confirmation without `--yes` | a test driving it with no `--yes` and asserting nothing was written |
| Write before printing the paths | the same test |

**Step 6: Commit**

```bash
git add cmd/wterm-web/install.go cmd/wterm-web/install_test.go cmd/wterm-web/cli.go internal/front/server.go
git commit -m "feat: install and remove the three integrations, from the CLI only"
```

---

## Phase G — the defect this design inherits

### Task 21: The late-repaint dwell in `Observe`

**Files:**
- Modify: `internal/tmux/state.go`, `internal/tmux/state_test.go`

**Last, and separable from everything above it.** This is a **v2 classifier defect this design inherits, not one it introduces**, it touches a file nothing else here touches, it gates nothing, and its constant is a number nobody has measured. A design that is otherwise ready should not wait on it.

**What was measured.** On **2 of 30 claude turns**, a single line near the input box repainted **5.0 s and 9.0 s after everything else on the screen had stopped** — both on a "write a file, then reply done" prompt; not periodic, not reproducible on demand. It does not stop the turn settling. What it does is produce a **second working→idle edge and a fresh `finishedAt` about 13 s after the real turn end**, and since `finishedAt` is compared against each browser's stored `seen` **value**, a stamp 13 s later than the one a device was shown **re-lights a done badge the user has already cleared.**

**Where it bites, and where it cannot.** On a pane whose report is in force it reaches nothing, for two independent reasons: a fresh report outranks the classifier for the derivation, and the capture is skipped so the classifier never sees the repaint. Inside the verification window the pane *is* captured and the classifier may well stamp its own `finishedAt` there — window poll 1 is a first sight, but the repaint at poll 2 sets `everChanged` — and it still reaches nothing, for a different reason: while the report is in force the row's `finishedAt` is the report's own, and the window **closes at the verdict**, so there is no later capture for a late repaint to land in. So it bites where the classifier is the authority: **a pane with no integration at all** (the common case, and pure v2), and a pane whose report is not in force — dropped on evidence, expired after a missed turn end, or demoted for want of a grammar.

**The repair that does not work, and this is the thing to write down so nobody tries it.** The obvious fix is to require *more* movement before arming an edge — a run of changed polls rather than a single one, by symmetry with `settleAfter`. **The same measurement refutes it: a genuinely short turn presents exactly one changed poll too.** In 29 of 72 claude turn-phase replays and 19 of 72 opencode ones, the *entire* turn — prompt echo, answer, prompt box redrawn — changed the screen at exactly one poll of the 1.5 s grid before settling. A late repaint and a two-second turn are the same signal at the hash level, and any threshold discarding the first discards the second. That is not a limitation of the threshold; it is what the classifier **is**: it dates our noticing, where a report dates the finish.

**The minimum that closes it is one conjunct**: do not stamp a new finish within `lateRepaintDwell` of the one already recorded on that pane.

**Step 1: Write the failing tests**

```go
// A run, a settle, a stamp; then a SINGLE changed capture and a second settle,
// which must NOT re-stamp; then the same again beyond the dwell, which must.
// Written against the constant, never against 15.
func TestObserveDoesNotRestampWithinTheDwell(t *testing.T) {
	c := NewClassifier()
	now := time.Unix(0, 0)
	// ... a real run: change, change, settle -> first stamp
	first := st.FinishedAt

	// The late repaint: one changed capture, 9s after everything stopped, then
	// a settle. Measured at 5.0s and 9.0s on 2 of 30 claude turns.
	now = now.Add(9 * time.Second)
	// ... one change, then settleAfter identical ...
	if st.FinishedAt != first {
		t.Fatalf("a repaint %v after the finish re-stamped (%d -> %d): that "+
			"re-lights a done badge the user has already cleared",
			9*time.Second, first, st.FinishedAt)
	}

	// A genuine second run whose stamp attempt lands at EXACTLY
	// `first + lateRepaintDwell` still stamps. This is the assertion that kills
	// `>=` -> `>`, and it is the only one that can: seconds past the boundary
	// the two operators agree, so the "beyond the dwell" assertion below --
	// which advances lateRepaintDwell + time.Second and then runs a whole
	// change-and-settle on top of that, landing several more seconds out -- is
	// green under both. Found by mutation.
	//
	// Built backwards from the boundary rather than forwards from `now`, so
	// that it stays exactly on it whatever lateRepaintDwell becomes: the
	// SETTLING poll is placed at time.UnixMilli(first).Add(lateRepaintDwell),
	// and the one changed capture that opens the run is stepped back
	// settleAfter polls of 1.5s from there, so the settleAfter-th identical
	// comparison lands exactly on it. On today's constants (settleAfter 2,
	// dwell 15s) the settling poll is the 10th 1.5s poll after the first stamp.
	settleAt := time.UnixMilli(first).Add(lateRepaintDwell)
	now = settleAt.Add(-1500 * time.Millisecond * time.Duration(settleAfter))
	// ... one changed capture at `now`, then settleAfter identical ones at
	// 1.5s steps, the last of them at exactly settleAt ...
	if st.FinishedAt != settleAt.UnixMilli() {
		t.Fatalf("a finish at exactly lateRepaintDwell after the last one did not stamp (finishedAt %d, want %d): the comparison is `>= lateRepaintDwell`, not `>`",
			st.FinishedAt, settleAt.UnixMilli())
	}

	// And a genuine second run, beyond the dwell, still stamps -- or the badge
	// works once per pane per dwell forever. This one pins `lateRepaintDwell =
	// 0` and the stamp-once-per-pane mutant; it does NOT pin `>=` against `>`.
	second := st.FinishedAt
	now = now.Add(lateRepaintDwell + time.Second)
	// ... change, change, settle ...
	if st.FinishedAt <= second {
		t.Fatalf("a real second finish beyond the dwell did not stamp")
	}
}

// The sibling that keeps the obvious wrong fix out. A turn whose whole visible
// life is ONE changed capture followed by a settle DOES stamp: measured, that
// is 29 of 72 claude turn-phase replays and 19 of 72 opencode ones. A short
// turn and a late repaint are the same signal, and only the dwell separates
// them.
func TestAShortTurnStillStamps(t *testing.T) {
	c := NewClassifier()
	now := time.Unix(0, 0) // the same clock the rest of this file uses
	// first sight, one changed capture, then settleAfter identical ones.
	// It must stamp, because nothing was stamped before it -- the dwell is
	// about the gap since the LAST stamp, not about how much movement this run
	// had. On this clock the stamp lands 4.5s past the epoch, which is
	// INSIDE the dwell: without the `finishedAt == 0` disjunct this test is red,
	// and so is every stamp assertion in TestClassifierWorkingAndIdle.
}
```

**Step 2: Run, expect FAIL.**

**Step 3: Implement**

```go
// lateRepaintDwell is how long after a stamped finish another one is refused.
//
// It exists for a measured failure: on 2 of 30 claude turns a single line near
// the input box repainted 5.0 s and 9.0 s after everything else had stopped.
// That produces a second working->idle edge about 13 s after the real turn end,
// and since a browser badges on finishedAt > the VALUE it was last shown, the
// second stamp re-lights a done badge the user has already cleared.
//
// It is NOT symmetric with the other two guards and should not be described as
// one: they decide whether THIS run earned an edge, and this decides whether a
// second edge so soon after the first can be a different finish at all.
//
// The constant is a GUESS, of the same standing as the 60-second working
// window -- open question 13. It is floored by the measurement (the second edge
// landed 12 to 13.5 s after the first) and that floor rests on a sample of TWO
// events, which is a bound and not a distribution. 15 s, biased long. Biasing
// long costs two genuine finishes inside the dwell collapsing to one badge --
// for which the user would have to have looked at the pane between them and
// then walked away within seconds -- against a failure measured at 2 of 30
// claude turns.
//
// It applies to the classifier's stamp ONLY, never to a report's derivation. A
// report dates its own finish, and a resting state is never re-asserted with a
// later timestamp, so there is nothing there for a dwell to protect against and
// adding one would silently swallow a genuine second turn end.
const lateRepaintDwell = 15 * time.Second
```

and the guard, which needs no new state — `p.finishedAt` is already there — and no clock inside the classifier, because `now` is already a parameter and the purity rule holds:

```go
	// The `p.finishedAt == 0` disjunct is LOAD-BEARING and must be written out.
	// "A pane that has never finished has finishedAt 0, and now - epoch is
	// obviously more than 15s" is true only when `now` is a real wall clock.
	// Every test in this file starts at `now := time.Unix(0, 0)` (state_test.go
	// lines 12, 102, 130, 151, 203, 244) and advances in 1.5s steps, so at the
	// first genuine stamp `now` is 7.5 seconds past the epoch and
	// time.UnixMilli(0) IS the epoch: the subtraction gives 9s, which is less
	// than the dwell, and the first stamp of the existing v2 suite is refused.
	if p.still == settleAfter && p.everChanged && !blocked &&
		(p.finishedAt == 0 || now.Sub(time.UnixMilli(p.finishedAt)) >= lateRepaintDwell) {
		p.finishedAt = now.UnixMilli()
	}
```

**Do not "simplify" the disjunct away and do not repair it by moving the tests' clocks.** The v2 stamp suite in `state_test.go` is not this task's to weaken — it is the suite that pins the behaviour this task is adding one conjunct to — and its epoch-based clock is what makes the disjunct observable rather than decorative: with a `time.Now()` fixture the whole guard would pass for the wrong reason and every mutant below would survive. For the same reason, **both new tests start at `time.Unix(0, 0)` like the rest of the file.**

**Step 4: Run, expect PASS.**

**Step 5: Mutation testing**

| Mutant | Killed by |
| --- | --- |
| Drop the dwell conjunct | `TestObserveDoesNotRestampWithinTheDwell`'s middle assertion |
| Drop the `p.finishedAt == 0` disjunct (the "it is trivially true anyway" simplification) | `TestAShortTurnStillStamps` **and the whole existing v2 stamp suite** — `TestClassifierWorkingAndIdle` first, because on a `time.Unix(0, 0)` clock the first genuine stamp is 7.5 seconds from the epoch (0 -> 1.5 -> 3.0 settles without stamping -> 4.5 changes -> 6.0 -> 7.5 stamps), and the next one 4.5s later is refused too. Run `go test ./internal/tmux/` after applying it: if only the new test goes red, a fixture has drifted onto a wall clock |
| the subtraction reversed (`time.UnixMilli(p.finishedAt).Sub(now)`) | the beyond-the-dwell assertion |
| `>=` to `>` | **only** the assertion at exactly `first + lateRepaintDwell`. Not the beyond-the-dwell one: that fixture advances `lateRepaintDwell + time.Second` and then runs a whole change-and-settle on top, landing seconds past the boundary where `>` and `>=` agree, so the mutant survives it. The boundary is the one point at which they differ, and a stamp attempt has to be placed on it exactly — settle poll at `time.UnixMilli(first).Add(lateRepaintDwell)`, which on today's constants is the 10th 1.5 s poll after the first stamp |
| `lateRepaintDwell = 0` | the middle assertion |
| Guard with `p.finishedAt == 0` instead (stamp once per pane, ever) | the beyond-the-dwell assertion. This is v2's own recorded trap arriving through a new door |
| Require a run of changed polls instead of the dwell | `TestAShortTurnStillStamps` — **the wrong fix the measurement refutes** |
| Apply the dwell to the report derivation too | a `reports_test.go` assertion: two genuine turn ends inside 15 s must both derive |
| `lateRepaintDwell` spelled as a literal in a test | a **review check**: change the constant locally and re-run; nothing should need editing |

**Step 6: Commit**

```bash
git add internal/tmux/state.go internal/tmux/state_test.go
git commit -m "fix: a repaint seconds after a finish does not re-light a cleared badge"
```

---

## Definition of done

- `make test` and `make test-e2e` pass.
- A pane running a real agent with its integration installed shows **what it is doing**, not what it was first asked, observed by hand on all three agents.
- With the browser closed and reopened, a finished agent still badges — the report survives a daemon restart, because the fact lives in tmux.
- A pane with **no** integration behaves exactly as it did under v2: the classifier, the grammars, the same badge.
- `wterm-web install-integration --remove` leaves a user's own `settings.json` hooks untouched, and `rm` works for the other two.
- No integration file contains a sanitizer, a state table, or a whitelist.
- Every task's mutation step has been run and its survivors are recorded in that task's commit message.

## What this plan does not settle

Carried from the design's open questions, and each is recorded here so that finding one unsolved later is a confirmation rather than a surprise:

- **`N_blocked`'s value** (question 3) is a derived floor with no measurement behind it, and measuring it needs the dialog screens question 10 is also waiting on.
- **The 60-second `working` window** (question 2) is a guess biased short. The case to measure is a single long tool call emitting no sub-events, with no client connected.
- **`lateRepaintDwell`** (question 13) rests on a sample of two events.
- **Four `notification_type`s stay ignored** (question 10) for want of a captured screen and a grammar. An 88-turn, 3-agent run met none of them, so somebody has to manufacture each one deliberately. Until then those four waits are invisible to **both** authorities, which is the largest single hole in this feature's coverage.
- **Cross-agent and nested-CLI panes are not handled** (questions 9 and 11). The nested `claude` inside a `claude` is *certain*, not hypothetical, and no field test can close it. Whether teammate and background-session hook processes carry `TMUX_PANE` is measured in Task 19 and decides whether that class needs anything at all.
- **A subagent false-idle on pi or opencode with no client connected is undefended.** The filters are absence-coded and fail open, and behind them there is one mechanism that only runs when somebody is watching — and that leaks at a measured rate even then.
- **What cannot be tested in CI**: that a real agent, running a real integration, produces the events we mapped. That is a manual check per agent per upgrade, and the honest mitigation is that a wrong mapping degrades to no report, which degrades to v2.
