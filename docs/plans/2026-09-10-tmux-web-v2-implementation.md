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
4. **tmux sanitises pane *titles* but not user *option values*.** `@wterm_label` can contain a `0x1f` or a newline and make a pane vanish from the sidebar.
5. **Blocked is checked on every capture, with no idle gate.** An agent can raise an approval box while background work continues.

**Testing.** Same as v1: integration against a real tmux on an isolated socket (`internal/tmux/testutil`, whose `Args()` includes `-f /dev/null` — that flag is load-bearing, the developer's `~/.tmux.conf` sets non-default options). Pure logic gets table tests. Never mock tmux.

**Safety.** Never run `pkill`, `killall`, or any pattern-matching process kill. The developer has a live tmux session with real work and running agents in it. Every mutating tmux command in a test goes through `testutil.Server`.

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

### Task 2: The title as a label

**Files:**
- Modify: `internal/tmux/snapshot.go` (`Format`, `fieldCount`, `Row`, `ParseRows`)
- Modify: `internal/tmux/snapshot_test.go`

Adds three fields to the snapshot: `SessionID`, `SessionName`, `Title`. All three come free in the existing `list-panes` call.

**Step 1: Write the failing tests**

Add to `TestParseRows`:

```go
t.Run("carries session identity and title", func(t *testing.T) {
	line := join("work", "$3", "%1", "0", "", "1", "api", "1", "claude", "✳ writing tests")
	got, dropped, err := ParseRows(line)
	if err != nil || dropped != 0 || len(got) != 1 {
		t.Fatalf("got %+v dropped=%d err=%v", got, dropped, err)
	}
	r := got[0]
	if r.SessionID != "$3" || r.SessionName != "work" || r.Title != "✳ writing tests" {
		t.Fatalf("bad row: %+v", r)
	}
})

// tmux sanitises titles, but not length: an 8KB title was observed stored and
// reported in full, and it would ride a 1.5s poll into the DOM.
t.Run("a huge title is truncated", func(t *testing.T) {
	huge := strings.Repeat("x", 5000)
	line := join("work", "$0", "%1", "0", "", "1", "api", "1", "claude", huge)
	got, _, _ := ParseRows(line)
	if len(got[0].Title) > MaxTitle {
		t.Fatalf("title kept %d bytes, want <= %d", len(got[0].Title), MaxTitle)
	}
})
```

Add a `join` helper next to the tests:

```go
// join builds a record in the wire order, so a field-order change breaks one
// helper rather than every fixture.
func join(fields ...string) string { return strings.Join(fields, Sep) }
```

**Step 2: Run, expect FAIL** (`undefined: MaxTitle`, unknown fields).

**Step 3: Implement**

`fieldCount` becomes 10. `Format` gains three fields — **note the order, and that `session_id` and `session_name` come from the row's own session:**

```go
const Format = "#{?#{session_group},#{session_group},#{session_name}}" + Sep +
	"#{session_id}" + Sep +
	"#{pane_id}" + Sep +
	"#{pane_index}" + Sep +
	"#{@wterm_web}" + Sep +
	"#{window_index}" + Sep +
	"#{window_name}" + Sep +
	"#{pane_active}" + Sep +
	"#{pane_current_command}" + Sep +
	"#{pane_title}"
```

`SessionName` is **not** a new format field: it is `#{session_name}`, which the existing first field already falls back to. Keep the group key as field 0 and add `session_name` as its own field so the two are independent — the group name and the live name differ after a rename, and that difference is the whole point.

So `Format` is the above with `"#{session_name}"` inserted after `session_id`, and `fieldCount` is 11.

`Row` gains:

```go
SessionID   string `json:"sessionId"`   // $N; what management operations target
SessionName string `json:"sessionName"` // live name, for display
Title       string `json:"title"`       // tmux-sanitised, truncated
```

```go
// MaxTitle bounds a pane title. tmux normalises control bytes out of titles but
// does not cap length; an 8KB title was observed stored and reported in full.
const MaxTitle = 256
```

Truncate in `ParseRows` on a rune boundary, not a byte one, or a multi-byte character can be cut in half and reach the DOM as invalid UTF-8.

**Step 4: Run the package tests, expect PASS. Commit.**

```bash
git add internal/tmux
git commit -m "feat: carry session identity and the pane title in the snapshot"
```

---

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

	// First sight of a pane cannot be a transition: the previous capture is
	// unknown, not different. It settles to idle without ever claiming work.
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
}

func TestClassifierForgetsClosedPanes(t *testing.T) {
	c := NewClassifier()
	c.Observe("%1", "a", time.Unix(0, 0))
	c.Observe("%2", "b", time.Unix(0, 0))
	c.Retain([]string{"%2"})
	if c.Len() != 1 {
		t.Fatalf("classifier holds %d panes after retaining one, want 1", c.Len())
	}
}
```

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
- hash same: `still++`; when `still >= settleAfter`, return idle — and stamp
  `finishedAt = now` **only if `everChanged`** and it is not already stamped

`everChanged` is the whole of the restart-storm fix. Write the comment saying so.

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

---

### Task 5: The blocked question

Only after Task 4 is green and committed.

Extract the request text and the numbered choices from the matched box into `Question{Text, Choices}`. Truncate the text. If extraction fails on a screen that matched as blocked, **return no question and keep the blocked state** — the state is the load-bearing part.

Commit separately so it can be reverted without losing the badge.

---

### Task 6: Capturing, and only when it is worth it

**Files:**
- Modify: `internal/tmux/client.go` (add `Capture`)
- Modify: `internal/tmux/poller.go`
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
- `Retain` the pane ids present, so closed panes are forgotten

**Step 3: Tests**

- table test: no client connected → every state empty, no captures attempted (count calls)
- integration test against real tmux: a pane running `sh -c 'while :; do date; sleep 0.2; done'` **reads as working**, and a pane running a static `cat` of a file **settles to idle**. Use a fake agent name added to `Agents` for the test rather than requiring claude to be installed.

**Step 4: Commit.**

---

## Phase B — management on the server

### Task 7: Target validation

**Files:** `internal/tmux/target.go`, `internal/tmux/target_test.go`

Validators for `%N`, `@N`, `$N`, and a session *name* (for create only). Session names reject a leading `-`, and `:` or `.`, which tmux uses as target separators.

Table-test every rejection with a comment naming what it prevents.

---

### Task 8: Management verbs

**Files:** `internal/tmux/manage.go`, `internal/tmux/manage_integration_test.go`

`NewSession`, `NewWindow`, `SplitPane`, `RenameSession`, `RenameWindow`, `SetLabel`, `ToggleZoom`, `KillSession`, `KillWindow`, `KillPane`.

Every one takes an id (`$N`, `@N`, `%N`) except `NewSession`, which takes a name.

Requirements each needing its own test against real tmux:

- **`SetLabel` rejects control bytes and caps length.** A label with a `0x1f` or a newline makes the pane vanish from the snapshot; tmux does not sanitise option values.
- **`SplitPane` and `NewWindow` resolve the working directory server-side** from `#{pane_current_path}` and pass `-c`. The path never comes from the browser.
- **A path that no longer exists is reported, not silently ignored.** `tmux split-window -c /gone` exits 0 and lands in `$HOME`. Stat first.
- **Kill refuses an `@wterm_web` session** on the direct session path.
- **Rename is visible in a subsequent snapshot** — the test that revision 1's design would have failed.

---

### Task 9: The endpoints

**Files:** `internal/front/manage.go`, `internal/front/manage_test.go`

The routes from the design, all `cfg.Auth.Protect(...)`. Every `DELETE` requires `{"confirm": true}` in the body and returns 400 without it.

Tests: each route rejects a foreign Origin; each `DELETE` rejects a missing confirm; a stale id returns the tmux error rather than a 500.

---

## Phase C — the browser

### Task 10: State in the sidebar

Row shows label, else title, else command. State dot. Roll-up `blocked > done > working > idle`. `done` from `finishedAt` vs `localStorage`, keyed with the tmux server generation so a restarted server's `%0` cannot inherit a stale entry.

### Task 11: Agent logos

One inline monochrome SVG per agent, ~16px, source recorded beside each file. An agent with no mark falls back to a two-letter monogram — never an invented logo.

### Task 12: Context menus and dialogs

Right-click and long-press per row type. Rename and create dialogs. **Kill dialog: names what dies including a running agent, a toggle to arm, then a red button; dismissing resets the toggle.** Copy must say when the kill closes the session and disconnects this tab.

### Task 13: Tab badge

`(2) tmux-web` and a favicon dot when anything is blocked or done.

### Task 14: End to end

Extend `e2e/`: state appears for a scripted fake agent; the two-step kill; rename visible after; the roll-up.

---

## Definition of done

- `make test` and `make test-e2e` pass.
- A pane running a real agent shows working while it works and idle when it stops, observed by hand.
- Renaming a session from the browser changes the sidebar.
- Killing a window says what it will destroy before it can be armed.
- No state, capture, or logo appears on a zsh or nvim pane.
