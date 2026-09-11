# tmux-web v2 Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Show which agent needs you, and let you create, rename, split, kill and zoom without leaving the browser.

**Architecture:** The snapshot poller gains a second, narrower job. For panes whose command is a known agent, and only while a browser is connected, each poll also captures the pane and hashes it: a changed hash means working, an unchanged one for two polls means idle, and a matched approval box means blocked. Everything else in the poll is unchanged. Management is a set of REST endpoints behind the existing cookie plus exact-Origin middleware, addressing tmux objects by id.

**Tech Stack:** Go 1.26 (stdlib only for the new server work), React 19 + shadcn/ui, vitest, Playwright.

**Design document:** `docs/plans/2026-09-10-tmux-web-v2-design.md` (revision 2). Read it before starting. It records two wrong turns and why they were wrong; both are things you would otherwise re-derive incorrectly.

---

## Before you start

**v1's lesson, which held for all 23 of its tasks: every defect was in the plan, not in the code written from it.** Two rounds of adversarial review on the v2 *design* found six more, including one that would have rebuilt the exact defect the revision existed to remove. Assume this plan contains some too. If something looks wrong, say so rather than implementing it faithfully.

**Five things here are counter-intuitive. Do not "simplify" them back.**

1. **State comes from the screen, not the pane title.** The title is a *label*. Claude Code rewrites it when the task summary changes and leaves it byte-identical through minutes of work — verified twice on a live machine. opencode's title is a static `OpenCode`. Inferring work from title changes was the design's first version and it is wrong.
2. **`capture-pane -S -8` does not mean "the last 8 lines".** On a 6-row pane it returns 14: the visible screen *plus* 8 lines of scrollback, which is where a just-answered approval box lives. Use `capture-pane -p -J` and slice in Go.
3. **Sessions have `$N` ids, and `session_group` keeps the pre-rename name.** Addressing sessions by name means rename appears to do nothing, because the sidebar keys on the group.
4. **tmux sanitises pane *titles* but not user *option values*.** `@tmux_web_label` can contain a `0x1f` or a newline and make a pane vanish from the sidebar.
5. **Blocked is checked on every capture, with no idle gate.** An agent can raise an approval box while background work continues.

**Run both suites after every task.** `go test ./...` alone is not enough: the
frontend carries a contract test that parses the Go `Row` struct, so a Go-side
wire change turns `pnpm test` red while Go stays green. That happened here —
Task 2's new fields were never mirrored into TypeScript and the frontend suite
was red for four tasks before anyone ran it.

**Testing.** Same as v1: integration against a real tmux on an isolated socket (`internal/tmux/testutil`, whose `Args()` includes `-f /dev/null` — that flag is load-bearing, the developer's `~/.tmux.conf` sets non-default options). Pure logic gets table tests. Never mock tmux.

**Safety.** Never run `pkill`, `killall`, or any pattern-matching process kill. The developer has a live tmux session with real work and running agents in it. Every mutating tmux command in a test goes through `testutil.Server`.

**Do not run `prettier`.** This repo has no formatter configured, so prettier
applies its own defaults — double quotes and semicolons — and rewrites whole
files, including ones you are not working on. It has already happened once and
had to be unpicked by hand. Match the surrounding style instead. (`prettier
--check` flags 32 files even at the repo's own settings, most of them generated
shadcn components, so adding a config would trade this problem for a worse one.)

**Commit after every task.**

---

## Phase A — agent state on the server

### Task 1: Recognising an agent

**Files:**
- Create: `internal/tmux/agent.go`, `internal/tmux/agent_test.go`

**Step 1: Write the failing test**

```go
package tmux

import "testing"

func TestKnownAgent(t *testing.T) {
	for _, tc := range []struct {
		cmd  string
		want string
	}{
		{"claude", "claude"},
		{"opencode", "opencode"},
		{"pi", "pi"},
		// Everything the developer actually runs alongside agents. A shell is
		// not idle or working; it is a shell, and a state badge on it is a lie.
		{"zsh", ""},
		{"bash", ""},
		{"nvim", ""},
		{"go", ""},
		{"", ""},
		// Case and paths: pane_current_command is a bare name, but be explicit
		// that we do not do prefix or substring matching -- "claude-helper"
		// is not claude.
		{"claude-helper", ""},
		{"CLAUDE", ""},
	} {
		if got := KnownAgent(tc.cmd); got != tc.want {
			t.Errorf("KnownAgent(%q) = %q, want %q", tc.cmd, got, tc.want)
		}
	}
}
```

**Step 2: Run it, expect FAIL**

Run: `go test ./internal/tmux/ -run TestKnownAgent -v`
Expected: FAIL, `undefined: KnownAgent`

**Step 3: Implement**

```go
package tmux

// Agents is the set of pane_current_command values treated as coding agents.
//
// One list gates three things -- whether a pane is captured, whether it gets a
// state, and whether it gets a logo -- so that a pane can never show a badge
// for a state nothing computed, or a logo for something we do not track.
//
// Matching is exact. "claude-helper" is not claude: a prefix match would put a
// state badge on whatever a user happens to name a script.
var Agents = []string{"claude", "opencode", "pi"}

// KnownAgent returns the agent name for a pane command, or "" if it is not one.
func KnownAgent(command string) string {
	for _, a := range Agents {
		if command == a {
			return a
		}
	}
	return ""
}
```

**Step 4: Run it, expect PASS. Commit.**

```bash
git add internal/tmux/agent.go internal/tmux/agent_test.go
git commit -m "feat: recognise which pane commands are coding agents"
```

---

### Task 2: Session identity, title and label in the snapshot

**Files:**
- Modify: `internal/tmux/snapshot.go` (`Format`, `fieldCount`, `Row`, `ParseRows`)
- Modify: `internal/tmux/snapshot_test.go` — **every existing fixture moves from
  8 fields to 13**; there is no way to add fields without touching them all
- Modify: `internal/tmux/client.go` (`ServerStart`), `internal/tmux/poller.go`
  (read it per refresh) and their tests — "read once per poll" cannot be
  satisfied without the poller
- Modify: `internal/front/server.go` (`snapshotResponse`, and the
  `SnapshotSource` interface the poller satisfies)

Five new fields, all free in the existing `list-panes` call, plus the tmux server
generation on the response envelope.

**There is exactly one authoritative field list. It has 13 fields.** An earlier
draft of this task printed two different lists and described a third in prose;
if you find yourself reconciling versions, you are reading a stale copy.

| # | format field | Row field |
| --- | --- | --- |
| 1 | `#{?#{session_group},#{session_group},#{session_name}}` | `GroupKey` |
| 2 | `#{session_id}` | `SessionID` |
| 3 | `#{session_name}` | `SessionName` |
| 4 | `#{pane_id}` | `PaneID` |
| 5 | `#{pane_index}` | `PaneIndex` |
| 6 | `#{@tmux_web_owned}` | `AppOwned` |
| 7 | `#{window_id}` | `WindowID` |
| 8 | `#{window_index}` | `WindowIndex` |
| 9 | `#{window_name}` | `WindowName` |
| 10 | `#{pane_active}` | `PaneActive` |
| 11 | `#{pane_current_command}` | `Command` |
| 12 | `#{pane_title}` | `Title` |
| 13 | `#{s/[\n\x1f]/ /:@tmux_web_label}` | `Label` |

**Field 13 changed shape and position in the label-hardening task; this table is
the current one.** The label used to sit at field 7 as a bare
`#{@tmux_web_label}`, which is how a hostile or buggy writer of that option could
remove a pane from the sidebar. It is now (a) stripped of the two bytes that
break the record by tmux's own `s///` substitution — the pattern is a bracket
set of the literal `0x1f` and newline, and `[[:cntrl:]]` cannot be used because
the `:` ends the modifier's pattern — and (b) last, so that a raw byte getting
through can only add fields after the final one or cut the line short after
every other field is already on it. `ParseRows` accepts **13 or more** fields
and rejoins the surplus into the label. `Format` is therefore built by joining
`formatFields` rather than concatenating a constant: `labelField` contains a
`0x1f` of its own, inside the regex, so counting separators no longer counts
fields.

The JSON names on `Row` did not change, so the frontend contract test is
untouched.

`fieldCount` is **13**. Field 1 stays the group key and field 3 is the live
session name: they are separate fields *because they differ after a rename*, and
that difference is the entire point of carrying session identity.

Field 8 is there for the same reason as field 2, and revision 1 of this task
missed it exactly as it missed the session id. **A window index is a position,
not an address**: tmux renumbers indices on `move-window` and reuses them after
a kill, and Phase B addresses windows by `@N` throughout — `ValidateWindowID`
refuses anything else. Without this field `PATCH /api/windows/{id}` and
`DELETE /api/windows/{id}` exist on the server and are unreachable from the
browser, because nothing on the wire names a window.

**Step 1: Write the failing tests**

`rec(...)` already exists in `snapshot_test.go:11` — use it, do not add a second
join helper.

```go
t.Run("carries session identity, label and title", func(t *testing.T) {
	// The group key and the live session name are DELIBERATELY different.
	// tmux keeps the pre-rename name in session_group, so a fixture where they
	// match would pass against an implementation that reads the group key --
	// which is exactly the bug this field exists to fix.
	line := rec("work3", "$3", "api", "%1", "0", "", "reviewer", "@7", "1", "win", "1", "claude", "✳ writing tests")
	got, dropped, err := ParseRows(line)
	if err != nil || dropped != 0 || len(got) != 1 {
		t.Fatalf("got %+v dropped=%d err=%v", got, dropped, err)
	}
	r := got[0]
	if r.GroupKey != "work3" {
		t.Errorf("GroupKey = %q, want the group name work3", r.GroupKey)
	}
	if r.SessionName != "api" {
		t.Errorf("SessionName = %q, want the live name api: reading the group key "+
			"here is the pre-rename bug this field exists to fix", r.SessionName)
	}
	if r.SessionID != "$3" || r.Label != "reviewer" || r.Title != "✳ writing tests" {
		t.Errorf("bad row: %+v", r)
	}
})

t.Run("a huge title is truncated on a rune boundary", func(t *testing.T) {
	// The rune has to be one the cap does NOT divide evenly, or the test is
	// vacuous: é is 2 bytes and MaxTitle is 256, so a naive title[:256] lands
	// exactly on a boundary and passes. ✳ is 3 bytes (256 % 3 == 1), so a byte
	// slice genuinely splits one -- and it is the character Claude Code puts at
	// the head of every title.
	huge := strings.Repeat("✳", 4000)
	line := rec("w", "$0", "w", "%1", "0", "", "", "@1", "1", "win", "1", "claude", huge)
	got, _, _ := ParseRows(line)
	if len(got[0].Title) > MaxTitle {
		t.Fatalf("title kept %d bytes, want <= %d", len(got[0].Title), MaxTitle)
	}
	if !utf8.ValidString(got[0].Title) {
		t.Fatal("truncation split a rune; the title is not valid UTF-8")
	}
	// A lower bound too, or "throw the title away" is a passing truncation.
	if len(got[0].Title) < MaxTitle-4 {
		t.Fatalf("truncation kept only %d bytes of a %d-byte cap", len(got[0].Title), MaxTitle)
	}
})

// tmux sanitises titles but NOT user option values, so a label is the one
// new field that can carry a separator or a newline.
t.Run("a label containing control bytes cannot remove a pane", func(t *testing.T) {
	line := rec("w", "$0", "w", "%1", "0", "", "EV"+Sep+"IL", "@1", "1", "win", "1", "claude", "t")
	got, dropped, _ := ParseRows(line)
	if dropped == 0 {
		t.Fatal("a malformed record must be counted")
	}
	if len(got) != 0 {
		t.Fatalf("a malformed record must not produce a row: %+v", got)
	}
	// The point is that it is DROPPED AND COUNTED, never merged into a
	// neighbour and never silently ignored.
})
```

**Step 2: Run, expect FAIL** (`undefined: MaxTitle`, unknown fields, wrong count).

**Step 3: Implement**

Set `fieldCount = 13`, write `Format` from the table above, add to `Row`:

```go
SessionID   string `json:"sessionId"`   // $N; what management operations target
SessionName string `json:"sessionName"` // live name, for display
WindowID    string `json:"windowId"`    // @N; what window operations target
Label       string `json:"label"`       // @tmux_web_label; user-set, may be ""
Title       string `json:"title"`       // tmux-sanitised, truncated
```

```go
// MaxTitle bounds a pane title. tmux normalises control bytes out of titles but
// does not cap length; an 8KB title was observed stored and reported in full,
// and it would ride a 1.5s poll into the DOM.
const MaxTitle = 256
```

Truncate on a rune boundary.

**Step 4: The tmux server generation**

`snapshotResponse` gains `ServerStart string \`json:"serverStart"\``, read once per
poll with `display-message -p '#{start_time}'`. Two consequences to accept
rather than engineer around: this is a **second tmux fork per poll**, where the
poller's own doc comment currently promises one; and `start_time` has
whole-second resolution, so two tmux servers started within the same second
share a generation. Task 10 keys its `seen` map with
it: pane ids restart at `%0` when the tmux server restarts, so without it a stale
`seen["%3"]` silently suppresses the badge on an unrelated new pane.

**Step 5: Run the package tests, expect PASS. Commit.**

```bash
git add internal/tmux internal/front/server.go
git commit -m "feat: carry session identity, label and title in the snapshot"
```

### Task 3: Classifying a capture

Pure functions. No tmux, no I/O.

**Files:**
- Create: `internal/tmux/state.go`, `internal/tmux/state_test.go`

**Step 1: Write the failing tests**

```go
// The states a pane can be in. "" is not a state: it means nothing computed one.
func TestClassifierWorkingAndIdle(t *testing.T) {
	c := NewClassifier()
	now := time.Unix(0, 0)

	// First sight reports working -- there is no previous capture to compare,
	// and the design accepts a settle rather than guessing. What it must NOT do
	// is stamp a finish edge when it later settles.
	st := c.Observe("%1", "screen A", now)
	if st.State != StateWorking {
		t.Fatalf("first observation = %q, want working", st.State)
	}
	if st.FinishedAt != 0 {
		t.Fatal("a first observation must not stamp a finish edge")
	}

	// Two identical captures settle to idle.
	now = now.Add(1500 * time.Millisecond)
	c.Observe("%1", "screen A", now)
	now = now.Add(1500 * time.Millisecond)
	st = c.Observe("%1", "screen A", now)
	if st.State != StateIdle {
		t.Fatalf("state after two identical captures = %q, want idle", st.State)
	}
	// ... and the run began at first sight, so it produced no finish edge.
	if st.FinishedAt != 0 {
		t.Fatal("a working run that began at first sight must not stamp a finish edge; " +
			"otherwise every daemon restart lights up a done badge on every device")
	}

	// A real change, then stillness, IS a finish edge.
	now = now.Add(1500 * time.Millisecond)
	c.Observe("%1", "screen B", now)
	now = now.Add(1500 * time.Millisecond)
	c.Observe("%1", "screen B", now)
	now = now.Add(1500 * time.Millisecond)
	st = c.Observe("%1", "screen B", now)
	if st.State != StateIdle || st.FinishedAt == 0 {
		t.Fatalf("observed work then stillness = %+v, want idle with a finish edge", st)
	}
	first := st.FinishedAt

	// A SECOND run must stamp a NEW edge. Guarding the stamp with
	// "finishedAt == 0" makes the done badge work exactly once per pane for
	// the life of the daemon, and the earlier assertions cannot see it.
	now = now.Add(1500 * time.Millisecond)
	c.Observe("%1", "screen C", now)
	now = now.Add(1500 * time.Millisecond)
	c.Observe("%1", "screen C", now)
	now = now.Add(1500 * time.Millisecond)
	st = c.Observe("%1", "screen C", now)
	if st.FinishedAt <= first {
		t.Fatalf("second finish edge = %d, want later than the first (%d): "+
			"FinishedAt is the LAST working->idle edge, not the first", st.FinishedAt, first)
	}

	// ... but an idle pane that keeps sitting still does not keep re-stamping,
	// or the badge could never be cleared by looking at it.
	now = now.Add(1500 * time.Millisecond)
	again := c.Observe("%1", "screen C", now)
	if again.FinishedAt != st.FinishedAt {
		t.Fatal("a pane that is merely still must not re-stamp its finish edge")
	}
}

func TestClassifierForgetsClosedPanes(t *testing.T) {
	c := NewClassifier()
	now := time.Unix(0, 0)

	// Both panes get a REAL change first, so both have everChanged set. That is
	// what makes kept and forgotten distinguishable: a retained pane settles
	// with a finish edge, a forgotten one comes back as first sight and settles
	// without. Asserting only that both report working -- or only Len() -- is
	// vacuous, because a forgotten pane reports working too.
	for _, id := range []string{"%1", "%2"} {
		c.Observe(id, "a", now)
		now = now.Add(1500 * time.Millisecond)
		c.Observe(id, "b", now) // a real change: everChanged
	}

	c.Retain([]string{"%2"})

	// %2 was kept: its run continues and settling stamps an edge.
	now = now.Add(1500 * time.Millisecond)
	c.Observe("%2", "b", now)
	now = now.Add(1500 * time.Millisecond)
	if st := c.Observe("%2", "b", now); st.State != StateIdle || st.FinishedAt == 0 {
		t.Fatalf("retained pane = %+v, want idle with a finish edge", st)
	}

	// %1 was dropped: it is first sight again, so settling stamps nothing.
	now = now.Add(1500 * time.Millisecond)
	c.Observe("%1", "b", now)
	now = now.Add(1500 * time.Millisecond)
	c.Observe("%1", "b", now)
	now = now.Add(1500 * time.Millisecond)
	if st := c.Observe("%1", "b", now); st.FinishedAt != 0 {
		t.Fatalf("forgotten pane = %+v, want no finish edge: it must come back "+
			"as first sight, not resume its old run", st)
	}
}

**Step 2: Run, expect FAIL.**

**Step 3: Implement `state.go`**

```go
// States a known agent pane can be in. The zero value, "", means no state was
// computed -- a pane that is not an agent, or one observed with no client
// connected. It is never displayed as a state.
const (
	StateWorking = "working"
	StateIdle    = "idle"
	StateBlocked = "blocked"
)

// settleAfter is how many consecutive identical captures mean idle. Two, so
// ~3s at the 1.5s poll: long enough that a redraw landing between polls does
// not flicker, short enough to feel live.
const settleAfter = 2
```

`Classifier` holds `map[string]*paneState` where `paneState` is `{hash uint64, still int, finishedAt int64, everChanged bool}`.

`Observe(paneID, capture string, now time.Time) Status`:

- hash the capture (FNV-64a is fine and stdlib)
- unknown pane: record, `still=0`, `everChanged=false`, return working
- hash differs: `still=0`, `everChanged=true`, return working
- hash same: `still++`; return idle once `still >= settleAfter` — and stamp
  `finishedAt = now` **at the moment `still` first reaches `settleAfter`**, and
  only if `everChanged`

The stamp is **per transition, not per lifetime**. Guarding it with
`finishedAt == 0` would make the done badge fire exactly once per pane for the
life of the daemon; guarding it with nothing would re-stamp on every idle poll,
so the badge could never be cleared by looking at it. Stamp on the edge:
`still == settleAfter` exactly.

`everChanged` is the whole of the restart-storm fix: without it, a map rebuilt
on restart or on client reconnect synthesises a working run on every agent pane,
every run settles, every settle stamps an edge newer than every browser's `seen`,
and every device lights up. Write the comment saying so.

**Step 4: Run, expect PASS. Commit.**

```bash
git add internal/tmux/state.go internal/tmux/state_test.go
git commit -m "feat: classify agent panes by capture churn"
```

---

### Task 4: Detecting a blocked agent

**Files:**
- Create: `internal/tmux/blocked.go`, `internal/tmux/blocked_test.go`, `internal/tmux/testdata/*.txt`

Ship `blocked` as a *state* only. Question extraction is Task 5, separately, so parser rot degrades to a missing quote rather than a wrong state.

**Step 1: Capture real fixtures**

Do **not** invent screens. Record real ones into `internal/tmux/testdata/`:
`claude-idle.txt`, `claude-working.txt`, and — if one can be produced — `claude-blocked.txt`. If you cannot produce a real blocked screen, **say so and stop**: a fixture invented from a description is how a detector ends up matching nothing real. Ask, and one will be captured for you.

**Step 2: Write the failing test**

```go
func TestIsBlocked(t *testing.T) {
	for _, tc := range []struct{ file string; want bool }{
		{"claude-idle.txt", false},
		{"claude-working.txt", false},
		{"claude-blocked.txt", true},
	} {
		screen := readFixture(t, tc.file)
		if got := IsBlocked("claude", screen); got != tc.want {
			t.Errorf("IsBlocked(%s) = %v, want %v", tc.file, got, tc.want)
		}
	}
}

// The reason blocked detection is strict: a false positive trains the owner to
// ignore the badge, which is worse than never having built it.
func TestIsBlockedIsStrict(t *testing.T) {
	for _, screen := range []string{
		"",
		"just some output\nnothing to see",
		// Prose that mentions the words but is not a dialog.
		"I could delete build/ but I will ask first. Do you want that?",
	} {
		if IsBlocked("claude", screen) {
			t.Errorf("false positive on %q", screen)
		}
	}
}
```

**Step 3: Implement**

Match the **bottommost** box-drawing dialog in the capture, and require the structural markers of a prompt — a bordered region containing numbered choices — not merely a question mark or the word "yes". The rules live in one table per agent so a rule change is a data change.

**Step 4: Run, expect PASS. Commit.**

**What shipped, per agent.** Three captures, and the table per agent earned
itself: no two of them share a structural signal.

| | corners | rules `─` | gutter `┃` | box `│` | numbered |
| --- | --- | --- | --- | --- | --- |
| claude | 0 | 100 | 0 | 0 | 3 |
| opencode | 0 | 0 | 18 | 0 | 0 |
| pi | 4 | 319 | 0 | 60 | 4 |

claude is a region between horizontal rules holding a cursored numbered choice
under a line that asks. opencode is a left-guttered block whose first line says
"Permission required". **pi is different in kind: it has no permission dialog at
all.** It asks through a tool, and the tool renders a two-pane selector inside a
closed box — the only closed box on pi's screen, and the only corners any of the
three agents draw. It is matched on that: a numbered option highlighted inside
the bottommost fully drawn box. The overlay is drawn *over* the transcript
rather than replacing it, so every line carries background text on both sides of
the box and only the box's own first column is read.

Two judgement calls on pi, both argued in `blocked.go`:

- **A picker the operator opened themselves lights the badge too.** The pane
  really is waiting on a keystroke. The only thing that would separate the two
  is the tool name pi writes into the top border, and that name is not stable —
  the capture says `ask_user`, the extension is `pi-ask-user`, this document
  says `ask_question` — so matching it would miss every question raised by a
  tool named differently.
- **Nothing reads the key hint line.** It is the most style-volatile line on the
  screen, it adds nothing over the highlight, and matching it loosely would fire
  on any pane whose output quotes pi's own help.

**pi's spinner keeps animating behind the overlay**, so churn never settles
while it waits. Confirmed rather than assumed: blocked is decided on every
capture with no idle gate and overrides churn's verdict (Task 6), and a poller
test drives pi's own spinner frames through the real capture to pin that the
badge survives a screen that never stops changing — and that no finish edge is
stamped underneath it.

---

### Task 5: The blocked question

Only after Task 4 is green and committed, and revertible on its own.

**Files:**
- Modify: `internal/tmux/blocked.go`, `internal/tmux/blocked_test.go`
- Modify: `internal/tmux/snapshot.go` (`Row` gains `Question`)

**Fixtures: the same rule as Task 4.** This needs a real captured screen whose
question text and choices are known. If you cannot produce one, stop and ask. A
grammar tuned against an invented dialog matches nothing that occurs in life,
and every test passes.

**`claude-blocked-unparseable.txt` does not exist and cannot be recorded.**
Step 2 below asks for a screen that IsBlocked still matches but extraction
cannot read. Task 4's detector needs a cursored choice, two lines matching
`^(?:❯ )?\d+\. ` and a line ending in "?" -- and that trailing space means a
line stops counting as a choice as soon as its text is taken away. The one "?"
on the captured screen is the question itself, above the choices. So every
deletion that defeats extraction (the question, an option, an option's text)
defeats detection too, and there is no shape left to record.

What shipped instead is the same test name over a table of transforms of the
real capture, each with the `blocked` verdict spelled out as a claim. It pins
the coincidence rather than inventing a screen, so the day the detector's
grammar changes the test says so and the fixture becomes recordable. The
property the plan actually wanted -- a blocked state surviving a failed
extraction -- is structural in Task 6's poller: the state comes from IsBlocked
and the question is assigned separately, and a nil question changes nothing.

**opencode and pi both produce that screen for real**, from opposite directions:
opencode's badge comes from its header and its quote from the request line
below, so losing the second leaves the first standing; and **pi scrolls the
prompt inside its own box** -- the capture carries the `↓` indicator that proves
it -- so a question long enough to need reading can be off the top of the box
while its options are still on screen.

**pi's grammar**, shipped as its own commit after detection: the options are the
numbered lines of the box's first column, in the order drawn, and the question
is the nearest line ending in "?" *above* the first option. Above, because the
box holds a whole prompt -- preamble, context, a bulleted summary -- any line of
which can end in one, and because the option text comes from the tool, so an
option can end in one too. The unnumbered "Type something" escape hatch is not
an option and is never quoted. One wrong quote it will produce, stated in the
comment rather than papered over: the filter line sits above the options, so a
filter query typed with a question mark on the end is what gets quoted.

**Step 1: The wire shape**

```go
// Question is the request a blocked agent is waiting on. Present only when
// AgentState is blocked, and omitted entirely when extraction failed -- the
// state is load-bearing, the text is a convenience.
type Question struct {
	Text    string   `json:"text"`
	Choices []string `json:"choices,omitempty"`
}
```

`Row` gains `Question *Question \`json:"question,omitempty"\``.

**Step 2: Write the failing tests**

```go
func TestExtractQuestion(t *testing.T) {
	q := ExtractQuestion("claude", readFixture(t, "claude-blocked.txt"))
	if q == nil {
		t.Fatal("no question extracted from a screen that IsBlocked matches")
	}
	if q.Text == "" || len(q.Choices) == 0 {
		t.Fatalf("extracted %+v, want text and choices", q)
	}
}

// Extraction failing must not take the state with it.
func TestExtractionFailureKeepsTheState(t *testing.T) {
	// A screen that matches the box structure but whose inner text we cannot
	// parse -- the shape a restyle produces.
	screen := readFixture(t, "claude-blocked-unparseable.txt")
	if !IsBlocked("claude", screen) {
		t.Fatal("fixture must still match as blocked")
	}
	if q := ExtractQuestion("claude", screen); q != nil {
		t.Fatalf("want no question rather than a wrong one, got %+v", q)
	}
}
```

**Step 3–5:** implement, run, commit separately from Task 4.

---

### Task 6: Capturing, and only when it is worth it

**Files:**
- Modify: `internal/tmux/client.go` (add `Capture`)
- Modify: `internal/tmux/poller.go`
- Modify: `internal/tmux/snapshot.go` — **`Row` gains `AgentState` and
  `FinishedAt`.** The design lists both and no other task claims them; without
  them there is nowhere for a classified state to go.
- Modify: `web/src/lib/useSnapshot.ts` and its test — the frontend contract test
  parses `type Row` out of the Go source and compares its json names against the
  TypeScript fixture's keys, so any field added here turns `pnpm test` red until
  it is mirrored. `rowsEqual` has to compare the new fields too, or a state that
  changes in tmux never reaches the DOM; `question` is the first non-scalar on
  the wire and needs a comparison of its own rather than `===`.
- Test: `internal/tmux/poller_test.go`, `internal/tmux/client_integration_test.go`

**Step 1: `Capture`**

```go
// Capture returns a pane's visible screen.
//
// -p writes to stdout, -J rejoins a line the pane wrapped. Deliberately NOT
// -S -8: a negative -S counts back into scrollback from the top of the visible
// screen, so on a 6-row pane `-S -8` returns 14 lines -- the screen plus 8 lines
// of history, which is exactly where a just-answered approval box lives.
func (c *Client) Capture(ctx context.Context, paneID string) (string, error)
```

Validate `paneID` with the existing `isPaneID` before it reaches tmux.

**Step 2: Wire into the poller**

The poller gains a `Classifier`, a `func() bool` reporting whether any client is connected, and a capture function. Per refresh, after the snapshot:

- if no client is connected: skip captures entirely, leave every `AgentState` empty, and **reset the classifier** so the next connection settles from scratch rather than resuming a stale run
- otherwise, for each row where `KnownAgent(row.Command) != ""`: capture, `Observe`, and if `IsBlocked` matches, override the state with `blocked`
- `Retain` **only the ids of currently-known-agent panes**, not every pane in the
  snapshot. A pane that goes claude → zsh → claude would otherwise keep its old
  hash; the relaunched agent's differing capture sets `everChanged`, and the next
  settle stamps a finish edge for an agent that just started

**Step 2b: The wiring, which is part of this task**

None of this works until something answers "is a client connected", and no
existing type does. This task owns all three edits:

- `internal/front/registry.go` gains `func (r *Registry) Live() bool`, true when
  any closer is registered. The design defines connected as **live terminal
  sockets** — the `/ws` registrations — so if a non-socket registration is ever
  added it must not count.
- `internal/tmux/poller.go` takes the classifier, the capture func, and the
  liveness func. Prefer an options struct or a second constructor over widening
  `NewPoller`, whose existing callers should not have to care.
- `internal/front/server.go`'s `newDaemon` (~line 918) builds the poller, so it
  passes `registry.Live` in. The registry is constructed there already.

**Step 3: Tests**

- table test: no client connected → every state empty, no captures attempted (count calls)
- integration test against real tmux: one agent pane **reads as working** while
  its screen keeps changing, and a still one **settles to idle**.

Three things about that integration test, all found the hard way:

- **`sh -c 'while :; do date; sleep 0.2; done'` is not a usable agent pane.**
  `pane_current_command` reports whatever is in the foreground at the instant
  the poll lands -- `sh`, `sleep` or `date` -- so the pane drifts on and off the
  agent list between polls. A single long-lived binary is what is needed.
- **Do not append a fake name to `Agents`.** It is a package-level variable that
  `KnownAgent` reads from the poll goroutine, and restoring it in a cleanup
  races that goroutine: cancelling the poller's context does not wait for a poll
  already in flight. Copy `cat` to a file named `claude` instead --
  `pane_current_command` comes from the kernel's process name, which is the
  basename of the file that was exec'd, so the pane is indistinguishable from a
  real one and the real `Agents` list is what decides. `testutil.FakeAgent`
  does this.
- **Type something different each time** into the pane that is supposed to read
  working. Sending the same character repeatedly fills the pane with identical
  lines, and scrolling one more onto a screen of identical lines leaves the
  capture byte for byte the same -- the pane reads idle while it is being typed
  into.

- wiring test: none of Step 2b is reachable from a test that does not go through
  the daemon. A poller built without `Capture`/`Connected` classifies nothing
  while every test in `tmux` and `front` stays green, so build a daemon with
  `newDaemon` against a throwaway tmux server and assert that an agent pane has
  no state until something is registered in the registry -- and none again once
  it is removed.

**Step 4: Commit.**

---

### Task 6b: Show the title, before anything else lands

The design says the title as a sidebar label "ships regardless of everything
below", and it is true: it is useful with no state detection at all. Phase C is
otherwise the first point where anything is demonstrable, which is a long way to
go on trust.

**Files:** `web/src/components/AppSidebar.tsx` and its test, **plus
`web/src/lib/useSnapshot.ts`** — two dependencies the first draft of this task
missed, both of them correctness rather than plumbing:

- `SnapshotRow` must mirror Task 2's five new wire fields. The contract test in
  `useSnapshot.test.ts` parses the Go `Row` struct and compares its json tags
  against the TypeScript keys, so it goes red the moment the Go side moves.
- `rowsEqual` must compare the title. It decides whether the previous tree
  object survives a poll, and React reconciles nothing when it does — so a title
  that changes while nothing else about the pane does would never reach the DOM.
  The feature would appear to work at first paint and then freeze.

Show `title` in place of `command` for panes where a title exists and differs
from the command. Nothing else — no state, no dot, no logo. One commit, and the
sidebar is better than it was.

---

## Phase B — management on the server

### Task 7: Target validation

**Files:** `internal/tmux/target.go`, `internal/tmux/target_test.go`

Validators for `%N`, `@N`, `$N`, and **names**.

**Names are not "for create only"** — an earlier draft said so and it is wrong.
`RenameSession`, `RenameWindow` and `NewWindow` all take a *new* name from the
browser, so a name is an input on four verbs, not one. Window names carry the
identical hazards (verified: tmux accepts `-n ''`, `-n 'w.y'` and `-n '-z'`), so
either `ValidateSessionName` is reused for both or a sibling is added — decide
and say which.

Rejections, each verified against tmux 3.7b rather than assumed:

- **Empty.** `new-session -s ''` **exits 0** and creates a session whose name is
  the empty string; `kill-session -t '='` then answers `no mouse target`. An
  invisible session that cannot be addressed by name. This is the most dangerous
  input in the task.
- **`:` and `.`** are target separators split off *before* exact matching, so
  `kill-session -t '=a:b'` answers `can't find session: a`.
- **A leading `-`** is read as a flag in the positional slot
  (`rename-session -t =base -x` → `unknown flag -x`). Note `new-session -s -x`
  *succeeds*, because getopt eats it as the flag's value — so tmux will create a
  name it can never rename.
- **Control characters, including C1.** tmux rejects C0 and DEL by a
  byte-oriented check but **accepts U+009F**, which would then ride the poll into
  the DOM. `unicode.IsControl` is load-bearing here, not decoration.
- **Invalid UTF-8**, because `encoding/json` silently rewrites it to U+FFFD and
  the sidebar would show a name tmux does not hold.

Table-test every rejection with a comment naming what it prevents.

---

### Task 8: Management verbs

**Files:** `internal/tmux/manage.go`, `internal/tmux/manage_integration_test.go`

`NewSession`, `NewWindow`, `SplitPane`, `RenameSession`, `RenameWindow`,
`SetLabel`, `ToggleZoom`, `KillSessionID`, `KillWindow`, `KillPane`.

Every one takes an id (`$N`, `@N`, `%N`) except `NewSession`, which takes a name
and an optional path.

**`KillSession` already exists and must not be reused.**
`internal/tmux/client.go:163` has `KillSession(ctx, name string)`, called from
`internal/ptybridge/session.go:188` to tear down a tab's throwaway session by
name. Same receiver, same name, different contract: a new id-taking
`KillSession` will not compile. Keep the existing one as the app-session
teardown path and name the new one `KillSessionID`.

**`NewSession` takes the optional path** the design specifies
(`POST /api/sessions {name, path?}`), typed by the owner in the dialog, and
stats it first for the same reason splits do.

**Names come from the browser on four verbs**, not one: `NewSession`,
`NewWindow`, `RenameSession`, `RenameWindow`. Every one validates before the
name reaches a command line, using Task 7's validator.

Requirements each needing its own test against real tmux:

- **`SetLabel` rejects control bytes and caps length.** A label with a `0x1f` or a newline used to make the pane vanish from the snapshot; tmux does not sanitise option values. Since the label-hardening task the snapshot no longer depends on this validator holding — see Task 2's field table — but it still keeps the app's own writes honest and gives the browser an error instead of a label that changes shape on the way back.
- **`SplitPane` and `NewWindow` resolve the working directory server-side** from `#{pane_current_path}` and pass `-c`. The path never comes from the browser.
- **A path that no longer exists is reported, not silently ignored.** `tmux split-window -c /gone` exits 0 and lands in `$HOME`. Stat first.
- **Kill refuses an `@tmux_web_owned` session** on the direct session path.
- **Rename is visible in a subsequent snapshot** — the test that revision 1's design would have failed. **Rename twice.** A single rename against a grouped session is vacuous, measured: an implementation that addresses the session by its group key still renames the right session the first time, because the group key and the live name are equal until the first rename lands. Mutation-tested — rename-by-group-name survives the one-rename version and dies on the two-rename one.

**Decided in implementation, since the plan left it open:** window names get a
sibling, `ValidateWindowName`, sharing one `validateName(kind, name)` body with
`ValidateSessionName`. Not a reuse, because the message reaches the owner in a
toast and "invalid session name" on a window rename is a lie about what went
wrong. The rules are identical and were re-probed for windows; one hazard is
worse there — `rename-window -t @1 ''` exits 0 and *stores* the empty name,
leaving a blank sidebar row immediately, where `new-window -n ''` is quieter
because automatic-rename fills one in.

Also measured while implementing, and worth knowing for Task 9's error mapping:
tmux's stale-target message is not uniform. `kill-pane`, `split-window` and
`list-panes` say `can't find pane: %99`; `set-option` says `no such pane: %99`.
Any test pinning one string must pin the one that command emits.

---

### Task 9: The endpoints

**Files:** `internal/front/manage.go`, `internal/front/manage_test.go`, and
`internal/front/server.go` (the routes and `HandlerConfig.Manage`, which is
required: a daemon built without a manager would 404 every context-menu action
and say nothing at startup).

The routes from the design, all `cfg.Auth.Protect(...)`. Every `DELETE` requires `{"confirm": true}` in the body and returns 400 without it.

**Delete the third copy of the pane-id rule.** `wsIsPaneID` in
`internal/front/ws.go` is byte-for-byte the same check that Task 7 replaced in
`internal/tmux/client.go`. `front` already imports `tmux`, so it becomes
`tmux.ValidatePaneID`. Three copies of a validation rule is how one of them
drifts.

**Pane and session ids must be percent-encoded in the path.** `%3` is an invalid
percent-escape, so `DELETE /api/panes/%3` is rejected by the mux before routing.
The browser sends `encodeURIComponent("%3")` → `%253`, and `r.PathValue` decodes
it back to `%3`. Pin the encoded form in the tests, or the frontend will be
written against a route that cannot be reached.

Tests: each route rejects a foreign Origin; each `DELETE` rejects a missing
confirm; a stale id returns the tmux error rather than a 500; an id arriving
un-encoded is refused rather than mis-routed.

**Decided in implementation, and measured rather than assumed:**

- **Only a *pane* id needs encoding, and the un-encoded one is refused by
  net/http rather than by anything in this package.** `@7` and `$1` are legal
  raw in a path and route unchanged; `%3` is not, and `http.ReadRequest` fails
  the request line with "invalid URL escape" before the mux is consulted. There
  is therefore no code here to test, and `httptest.NewRequest` cannot even
  express the case — it parses the target and panics. The test serves the real
  route table over an in-memory `net.Pipe` (no port bound) with full
  credentials and `{"confirm": true}`, and asserts a 400 with the pane still
  alive; the encoded spelling of the same id over the same wire kills it.
- **Every failure from a management verb answers 400, never 500.** Telling a
  stale target apart from a genuinely broken tmux would mean matching on tmux's
  message text, which is exactly what this task was warned is not uniform, so
  one code is used and tmux's own words are passed through for the toast.
- **`resize-pane -Z` on a single-pane window exits 0 and does nothing** (tmux
  3.7b). A zoom test seeded with one pane therefore reports "not zoomed" no
  matter what the handler did — vacuous, and the one found in this task. The
  test splits first and asserts the flag in both directions.
- **Ids and names are not re-validated in `front`.** Task 8's verbs validate
  per kind, and a second copy here is the copy that drifts — the same reasoning
  that deleted `wsIsPaneID`.
- Mutation-tested: 17 mutants, all killed. Including a route registered without
  `Protect`, the confirm check removed, the confirm check accepting any body
  that parses, an id taken from `r.URL.EscapedPath()` rather than `PathValue`,
  an Origin comparison relaxed to a suffix match, a tmux error surfacing as a
  500, and each optional input (`path`, `fromPane`, `name`, `direction`)
  dropped on the way to tmux — the last four were survivors until tests were
  added for them.

---

## Phase C — the browser

### Task 10: State and identity in the sidebar

**Files:** `web/src/lib/useSnapshot.ts`, `web/src/components/AppSidebar.tsx`, tests.

- Row shows `label`, else `title`, else `command` — but a title has to earn the
  row. **The design says only *agent* panes show a title**, while this task
  originally said all panes unconditionally; the reconciliation shipped in
  Task 6b is a rule rather than a list: show a title when it is non-empty, is
  not the command, and is not a single bare hostname-shaped token. That keeps
  useful `vim`/`ssh` titles, drops the hostname every plain shell carries, and
  avoids a second copy of the Go `Agents` list in TypeScript.
- **Session rows display `sessionName`, not `groupKey`.** `useSnapshot.ts:222-225`
  groups and labels by `groupKey` today, and tmux keeps the *pre-rename* name
  there forever — so without this change, renaming from the browser still appears
  to do nothing and Task 8's rename is invisible. Keep `groupKey`/`sessionId` for
  identity; change only what is displayed.
- State dot per pane; roll-up `blocked > done > working > idle` to window and
  session.
- `done` from `finishedAt` vs `localStorage`, **keyed `${serverStart}:${paneId}`**
  using the `serverStart` Task 2 puts on the response. Pane ids restart at `%0`
  when the tmux server restarts.
- Blocked rows show the question text truncated, with the choices in the tooltip
  (skip until Task 5 ships; the state alone is useful).

### Task 11: Agent logos

One inline monochrome SVG per agent, ~16px, source recorded beside each file. An
agent with no mark available falls back to a two-letter monogram — never an
invented logo for someone else's project. Driven by the same `Agents` list, so a
pane with no state never gets a logo.

### Task 12: Context menus, dialogs, and failures

**Files:** `web/src/components/AppSidebar.tsx`, a new `web/src/components/KillDialog.tsx`, `web/src/components/Palette.tsx`, `web/src/App.tsx`.

- Right-click and long-press per row type: rename, new window, split right/down,
  zoom, then a separated red kill.
- **The same actions in the `Ctrl+Alt+K` palette.** `Palette.tsx` exists and
  carries panes and copy-mode today; these are added entries, not a new surface.
- **A `+` on the session row**, for the one case with nothing to right-click: an
  empty tmux server, which v1 cannot fix from the browser at all.
- **Kill dialog**: names exactly what dies including any running agent
  (`kill window "api" — 3 panes, one running claude`), a toggle to arm, then a
  red button; dismissing resets the toggle. **When the target is the session this
  tab is attached to, or its last window or pane, the copy says it closes the
  session and disconnects this tab.**
- **Failures surface as a `sonner` toast** naming the attempt and tmux's own
  message, and the sidebar refreshes immediately rather than waiting for the next
  poll. Nothing is retried.
- **One line of copy where zoom is offered**, saying it zooms the window for
  every client — zoom is a window property, so it moves the local terminal too,
  joining v1's shared-property family.

### Task 13: Reconnect when the session is gone

**Files:** `web/src/components/Terminal.tsx`.

Today a dropped socket reconnects to the same session forever. After a kill that
destroyed the group, that session no longer exists and the retry can never
succeed. Detect it, stop retrying, and report it with the session list offered —
the design's defined behaviour for "my group is gone".

### Task 14: Tab badge

`(2) tmux-web` in the title and a favicon dot when anything is blocked or done.
Note in a comment that a hidden tab's polling is throttled and a locked phone's
stops, so the badge is late or absent exactly while you are away — the accepted
consequence of choosing a badge over push.

### Task 15: End to end

Extend `e2e/`:

- a scripted fake agent (a copy of `sh` on `PATH` named so `pane_current_command`
  matches an entry added to `Agents`) shows working while it redraws and idle
  when it stops
- the two-step kill: the red button does nothing until the toggle is armed
- rename a session from the browser and assert the sidebar shows the new name —
  **against a grouped session**, i.e. with a tab attached, since on an ungrouped
  session field 1 is already the live name and the test would pass even under the
  group-key bug
- the roll-up: a blocked pane makes its window and session read blocked

Four things this took that are not obvious from the list above, recorded because
each of them is a test that passes for the wrong reason if you get it wrong:

1. **The agent is a copy of `cat`, not of `sh`,** and it is named for an agent
   already on `Agents` -- `internal/tmux/testutil.FakeAgent`'s trick, in
   TypeScript, on the harness. `cat` holds the pane open and echoes what is sent
   to it, so a `send-keys` is a redraw; adding a fake name to `Agents` for the
   duration of a test races the poll goroutine, which is why that list is left
   alone.
2. **The first observation of a pane always reports working** -- there is
   nothing to compare it against -- so "create an agent pane, see working" is
   vacuous. The test waits for *idle* first, then churns, then waits for the
   state to come back. Only that order proves a capture was taken twice and
   compared.
3. **`window-size latest` sizes windows from the most recent client on the
   server, whatever session it is attached to.** A session created with `-x/-y`
   and never attached to still gets resized to the headless browser's terminal
   the moment a tab connects, and a 31-row approval box in a 10-row pane has
   scrolled off the screen `capture-pane` returns -- so the pane reads idle and
   the roll-up test quietly tests nothing. The blocked test sets
   `window-size manual` and resizes the window itself.
4. **A reload wipes the daemon's `finishedAt`.** The classifier is emptied
   whenever the last browser disconnects, so a single-tab reload clears a `done`
   badge whether or not anything was persisted. The persistence test keeps a
   second tab open in the same browser context -- same localStorage, its own
   sessionStorage -- so the daemon never sees zero clients and the badge's
   absence after the reload can only come from `useSeenPanes` having written the
   map.

## Definition of done

- `make test` and `make test-e2e` pass.
- A pane running a real agent shows working while it works and idle when it stops, observed by hand.
- Renaming a session from the browser changes the sidebar.
- Killing a window says what it will destroy before it can be armed.
- No state, capture, or logo appears on a zsh or nvim pane.
