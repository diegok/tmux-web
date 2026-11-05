# Agents driving tmux — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Seven `tmux-web` subcommands that let an agent running in a pane see what other panes are doing and act on them, with the semantics the daemon already computes and guards against surprising the owner.

**Architecture:** Each verb is a subcommand that forks tmux directly, the way `report` does — `$TMUX` for the socket, `$TMUX_PANE` for the caller — so nothing needs the daemon running and authorization is tmux's own. Two new `internal/tmux` functions carry the thinking (`CallerSession`, `LiveState`); the rest is CLI plumbing over functions that already exist. `run` is `open` then `type --enter`, so no code hands a command to `new-window`.

**Tech Stack:** Go 1.26, `log/slog`, the existing `internal/tmux` client, `embed.FS` in `internal/integrations`. Tests: Go table tests with recording stubs for the unit layer, the repo's `-integration` tests against a throwaway tmux socket for the rest.

**Spec:** `docs/plans/2026-11-05-tmux-web-agent-control-design.md` (revision 4, approved 2026-11-05)

## Global Constraints

- **Transport: tmux, never the daemon.** Every verb forks tmux. No new HTTP route, no new admin-socket route. `cli.go:35`'s argument for `report` is the argument for all of these.
- **Targets are pane ids (`%N`) only**, validated with `tmux.ValidatePaneID`. tmux prefix-matches session and window *names* without reporting ambiguity.
- **The caller is `$TMUX` + `$TMUX_PANE`.** Reuse `tmuxTarget(getenv) (socket, pane string, ok bool)` from `cmd/tmux-web/report.go:371`.
- **Nothing hands a command to `new-window` or `split-window`.** `manage.go:84` forbids it; `open` creates an empty shell and the command arrives as keystrokes.
- **No `remain-on-exit`.** The shell survives the command and keeps its output; measured on 3.7b.
- **Two vocabularies, never mixed.** A known agent (`tmux.Agents`: `claude`, `opencode`, `pi`, matched exactly) answers `working`/`idle`/`blocked`. Anything else answers `changing`/`quiet`. Either can answer `unknown`.
- **The browser's behaviour does not change.** No snapshot field, no new state dot, no route. A reviewer's test for this whole plan is `git diff --stat internal/front web/` being empty.
- **Scope default:** the caller's session. A pane outside it must be named explicitly.
- **Exit codes:** `2` for usage, as `cli.go` does for an unknown command. `1` for a verb that could not do its job. `0` on success. `report`'s exit-code exception does not apply here.
- **The writers that put something somewhere else refuse app-owned sessions** — `open --window`, `run --window` (the same `openPane` branch) and `close` — via the existing `Row.AppOwned` (`@tmux_web_owned`). `type` and `run --split` do **not**, and must not: their pane is in the caller's own window, so it shares the caller's fate and there is nobody left to mislead, while an app-owned row survives `Dedupe` only when that session is the sole link to the window, which is precisely the case where the pane is live and typeable. An earlier wording of this line said "writers refuse app-owned sessions" flatly; the design's decision 6 (revision 6) records why that was wrong.

## Review Focus

Five things the spec implies and no task below would otherwise exercise. Each one's test is added to the task that owns the code.

1. **The caller is not in tmux at all** — `$TMUX` or `$TMUX_PANE` absent or malformed. Every verb must say so and exit 1, never dereference a zero target. (Task 3)
2. **The target pane dies between resolve and act.** An ordinary race, not a fault: the verb must report that the pane is gone, not crash or act on a different pane. (Task 7)
3. **A pane id that is well-formed but belongs to no pane on this server.** `%999` must be refused by the verb, not by tmux's stderr leaking through. (Task 4)
4. **Text that looks like options.** `type %1 -- --help` and text beginning with `-`: the keystrokes must reach the pane as text. `send-keys -l --` discipline. (Task 7)
5. **A capture that fails.** `LiveState` must answer `unknown` rather than let a failed capture read as "the screen did not change", which would report `quiet` for a pane it never saw. (Task 2)

---

## File Structure

**Create:**
- `internal/tmux/caller.go` — `CallerSession`: which session a pane belongs to, once, with the group resolved.
- `internal/tmux/caller_test.go`
- `internal/tmux/livestate.go` — `LiveState`: the on-demand state, borrowing the report rules and the churn rule.
- `internal/tmux/livestate_test.go`
- `cmd/tmux-web/control.go` — the seven subcommands and their shared plumbing.
- `cmd/tmux-web/control_test.go`
- `cmd/tmux-web/control_integration_test.go` — the writers, against a throwaway socket.
- `internal/integrations/claude-tmux-control.md` — the skill, embedded.

**Modify:**
- `cmd/tmux-web/cli.go` — seven cases in the switch, and `printUsage`.
- `cmd/tmux-web/install.go` — `agentSpec` carries N owned files; `tmuxWebSchema` 1 → 2.
- `internal/integrations/claude_hooks_test.go` — the embed-reachability test covers the new file.

---

## Task 1: `CallerSession` — which session a pane is in, once

**Files:**
- Create: `internal/tmux/caller.go`
- Test: `internal/tmux/caller_test.go`

**Interfaces:**
- Consumes: `Snapshot(ctx) ([]Row, error)`, `Dedupe([]Row) []Row` (`snapshot.go:479`), `Row` (`snapshot.go:51`).
- Produces: `func CallerSession(ctx context.Context, c Snapshotter, paneID string) (sessionID string, err error)` and `type Snapshotter interface { Snapshot(context.Context) ([]Row, error) }`.

Why it exists: every open browser tab adds a grouped `_web-*` session, so a pane is reported once per group member and `list-panes -a` gives the caller's own pane two or three rows. The `_web-*` answer is the wrong one — those sessions carry `destroy-unattached on`, so a window created in one dies with the tab. `Dedupe` already prefers the non-app row; this is that preference applied to one pane id.

- [ ] **Step 1: Write the failing test**

```go
// internal/tmux/caller_test.go
func TestCallerSessionPrefersTheSessionTheOwnerKeeps(t *testing.T)
//   rows: the same PaneID "%5" in {SessionID:"$0", AppOwned:false} and
//         {SessionID:"$7", AppOwned:true}; want "$0".
func TestCallerSessionAnswersWithOnlyAnAppSession(t *testing.T)
//   rows: "%5" only in {SessionID:"$7", AppOwned:true}; want "$7" and no error
//   -- refusing is the writer's job, not this function's.
func TestCallerSessionSaysSoForAPaneThatIsNotThere(t *testing.T)
//   rows without "%5"; want an error naming the pane.
func TestCallerSessionRefusesAMalformedPaneID(t *testing.T)
//   paneID "5"; want an error and NO call to Snapshot (count it).
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./internal/tmux/ -run TestCallerSession -v`
Expected: FAIL — undefined: `CallerSession`.

- [ ] **Step 3: Implement `CallerSession` in `internal/tmux/caller.go`**

Validate with `ValidatePaneID` before the snapshot — a bad id must cost no fork. Then `Dedupe` the snapshot and return the matching row's `SessionID`.

- [ ] **Step 4: Run it and watch it pass**

Run: `go test ./internal/tmux/ -run TestCallerSession -v` → PASS

- [ ] **Step 5: Commit**

```bash
git add internal/tmux/caller.go internal/tmux/caller_test.go
git commit -m "feat: a pane resolves to the session the owner keeps, not the tab's"
```

---

## Task 2: `LiveState` — the state, computed now, with no poller

**Files:**
- Create: `internal/tmux/livestate.go`
- Test: `internal/tmux/livestate_test.go`

**Interfaces:**
- Consumes: `ParseReport(v string, now time.Time) (Report, bool)` (`report.go:138`), `Report{State,Timestamp,Activity}` (`report.go:124`), `workingTTL` (`reports.go:24`), `settleAfter` (`state.go`), `IsBlocked(agent, screen string) bool` (`blocked.go:145`), `ExtractQuestion(agent, screen string) *Question` (`blocked.go:191`), `KnownAgent(command string) string` (`agent.go`), `Capture(ctx, paneID) (string, error)`.
- Produces:

```go
type Verdict struct {
    State    string    // StateWorking/StateIdle/StateBlocked, or LiveChanging/LiveQuiet, or LiveUnknown
    Question *Question // nil unless State is StateBlocked
    From     string    // VerdictReport or VerdictScreen -- which authority answered
    Dropped  bool      // a standing blocked report no capture corroborated
}

const (
    LiveChanging = "changing"
    LiveQuiet    = "quiet"
    LiveUnknown  = "unknown"
    VerdictReport = "report"
    VerdictScreen = "screen"
)

func LiveState(ctx context.Context, c LiveReader, row Row, now time.Time, settle time.Duration) (Verdict, error)

type LiveReader interface {
    Run(ctx context.Context, args ...string) (string, error)
    Capture(ctx context.Context, paneID string) (string, error)
}
```

Why it is not the poller's cache: `poller.go:562-610` empties the classifier and gates the captures off whenever no browser is connected, which is exactly the owner's away case. And a report is not self-sufficient — `reports.go` makes a resting report survive multi-poll evidence, and `reports.go:252` records that *"a blocked report costs a fork per poll — the case the app exists for is exactly the one where the report is wrong."* A one-shot call has no history, so it corroborates instead.

The rules, from the spec's decision 3:

| authority | what `LiveState` does |
| --- | --- |
| `working` report, age < `workingTTL` (60s) | trust it. No fork. `From: report` |
| `blocked` report | one capture + `IsBlocked`. Corroborated → `blocked`, `Activity` as the question, `From: report`. Not corroborated → `idle` with `Dropped: true` |
| `idle` report, or no report | two captures `settle` apart. Identical → `idle`/`quiet`. Different → `working`/`changing`. `From: screen` |
| known agent, screen matches `IsBlocked` | `blocked` with `ExtractQuestion`, `From: screen` — churn can never see a dialog (`state.go`: a held dialog is byte-identical, the same shape as stillness) |
| any capture fails | `LiveUnknown`, and the error |

- [ ] **Step 1: Write the failing test**

```go
// internal/tmux/livestate_test.go -- a stub LiveReader scripting captures and
// the @tmux_web_agent read, counting both.
func TestAFreshWorkingReportIsTrustedWithoutACapture(t *testing.T)
//   report working at now-10s; want StateWorking, From report, captures == 0.
func TestAnExpiredWorkingReportFallsBackToTheScreen(t *testing.T)
//   report working at now-61s, two identical captures; want StateIdle, From screen.
func TestABlockedReportIsCorroboratedBeforeItIsBelieved(t *testing.T)
//   claude pane, report blocked with Activity "Allow?", capture holding a real
//   claude dialog; want StateBlocked, Question.Text "Allow?", From report.
func TestAnUncorroboratedBlockedReportIsDroppedAndReadsIdle(t *testing.T)
//   report blocked, capture holding an ordinary prompt; want StateIdle,
//   Dropped true -- the one direction the owner's loop cannot absorb.
func TestAKnownAgentWithNoReportReachesBlockedFromItsScreen(t *testing.T)
//   no report, capture holding a claude dialog; want StateBlocked and the
//   question from ExtractQuestion, From screen.
func TestAPaneThatIsNotAnAgentGetsTheOtherVocabulary(t *testing.T)
//   command "zsh", two identical captures; want LiveQuiet. Two different: LiveChanging.
//   Assert the result is never one of StateWorking/StateIdle/StateBlocked.
func TestAFailedCaptureIsUnknownAndNotQuiet(t *testing.T)   // Review Focus 5
//   first capture errors; want LiveUnknown and the error, NOT LiveQuiet.
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./internal/tmux/ -run 'TestA(Fresh|nExpired|Blocked|nUncorrob|KnownAgent|PaneThat|FailedCapture)' -v`
Expected: FAIL — undefined: `LiveState`.

- [ ] **Step 3: Implement `LiveState` and its constants in `internal/tmux/livestate.go`**

Read the report with one `show-options -p -v @tmux_web_agent` on the pane, as `report.go:278` does, and `ParseReport` it. The settle wait is the caller's parameter so a test can pass zero; production passes the value Task 4 chooses.

- [ ] **Step 4: Run it and watch it pass**

Run: `go test ./internal/tmux/ -v -run 'TestA' && go test -race ./internal/tmux/` → PASS

- [ ] **Step 5: Commit**

```bash
git add internal/tmux/livestate.go internal/tmux/livestate_test.go
git commit -m "feat: a pane's state, computed on demand and corroborated"
```

---

## Task 3: The subcommand plumbing, and `panes`

**Files:**
- Create: `cmd/tmux-web/control.go`, `cmd/tmux-web/control_test.go`
- Modify: `cmd/tmux-web/cli.go` (the switch, `printUsage`)

**Interfaces:**
- Consumes: `tmuxTarget(getenv) (socket, pane string, ok bool)` (`report.go:371`), `CallerSession` (Task 1), `Dedupe`, `Snapshot`, `ParseReport`.
- Produces:

```go
func cmdPanes(args []string, stdout, stderr io.Writer) int
func runPanes(args []string, stdout, stderr io.Writer, getenv func(string) string, dial func(socket string) controlClient) int

// controlClient is the little of tmux.Client these subcommands use. Injected
// from the first version, as report.go's tmuxRunner was, so no later task has
// to refactor this one to get a recording stub into position.
type controlClient interface {
    Run(ctx context.Context, args ...string) (string, error)
    Snapshot(ctx context.Context) ([]tmux.Row, error)
    Capture(ctx context.Context, paneID string) (string, error)
    CaptureRange(ctx context.Context, paneID string, lines int) (string, bool, error)
    NewWindow(ctx context.Context, sessionID, name, fromPane string) (string, error)
    SplitPane(ctx context.Context, paneID, direction string) (string, error)
    SetLabel(ctx context.Context, paneID, label string) error
    KillPane(ctx context.Context, paneID string) error
}

func dialControl(socket string) controlClient  // tmux.NewClient([]string{"-S", socket})
```

`panes` prints one line per pane and **only the state it gets for free**: the report-derived state where a parseable, unexpired report exists, and `-` everywhere else. Computing `LiveState` per pane would be two or three forks each plus a settle wait, on a server with a dozen panes, and two verbs that disagree about the same pane in the same second is an anti-goal. A `-` means "ask `state`".

- [ ] **Step 1: Write the failing test**

```go
// cmd/tmux-web/control_test.go
func TestPanesListsTheCallersSessionByDefault(t *testing.T)
//   snapshot spanning $0 (caller's) and $1; want only $0's rows.
func TestPanesWithAllListsEverySession(t *testing.T)
func TestPanesPrintsThePaneIDAVerbCanTake(t *testing.T)
//   every line holds a %N and the human-readable session:window.pane.
func TestPanesStateColumnIsTheFreeOneOrADash(t *testing.T)
//   a pane with a fresh working report prints "working"; a pane with none, and
//   an agent pane whose report is 61s old, both print "-". Assert the stub
//   took ZERO captures.
func TestEveryVerbSaysSoWhenItIsNotRunningInsideTmux(t *testing.T)  // Review Focus 1
//   getenv returns "" for TMUX and TMUX_PANE; want exit 1 and a message naming
//   both variables. Table-driven over all seven runX functions as they land;
//   starts with runPanes and grows with each task.
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./cmd/tmux-web/ -run 'TestPanes|TestEveryVerbSaysSo' -v`
Expected: FAIL — undefined: `runPanes`.

- [ ] **Step 3: Implement the plumbing and `panes` in `cmd/tmux-web/control.go`; wire `case "panes"` into `cli.go` and add its usage line**

Output format: one pane per line, fields in a fixed order separated by two spaces, `devices`' output as the precedent. No alignment padding — a reader greps it.

- [ ] **Step 4: Run it and watch it pass**

Run: `go test ./cmd/tmux-web/ -run 'TestPanes|TestEveryVerbSaysSo' -v` → PASS

- [ ] **Step 5: Commit**

```bash
git add cmd/tmux-web/control.go cmd/tmux-web/control_test.go cmd/tmux-web/cli.go
git commit -m "feat: tmux-web panes, the caller's session and what it is running"
```

---

## Task 4: `state`

**Files:**
- Modify: `cmd/tmux-web/control.go`, `cmd/tmux-web/control_test.go`, `cmd/tmux-web/cli.go`

**Interfaces:**
- Consumes: `LiveState` (Task 2), the plumbing (Task 3).
- Produces: `func runState(args []string, stdout, stderr io.Writer, getenv func(string) string, dial func(string) controlClient) int`

Prints one word on the first line. When blocked, the question follows, and the line says which authority answered — a report's `Activity` and a screen's `ExtractQuestion` are different texts and must not be presented as one field. A dropped stale report says so, because an agent that reads `idle` where a report said `blocked` deserves to know why.

- [ ] **Step 1: Write the failing test**

```go
func TestStatePrintsOneWordAndThenTheQuestion(t *testing.T)
func TestStateNamesWhichAuthorityAnswered(t *testing.T)
//   "report" vs "screen" appears in the output for the two blocked paths.
func TestStateSaysAStandingReportWasDropped(t *testing.T)
func TestStateRefusesAPaneThatDoesNotExist(t *testing.T)   // Review Focus 3
//   "%999", well-formed and absent from the snapshot; want exit 1 and a message
//   naming the pane, with tmux's own stderr not leaking into stdout.
func TestStateRefusesAPaneOutsideTheCallersSessionUnlessNamed(t *testing.T)
//   a bare pane id from another session is accepted (it IS named); the test
//   pins that `panes` scoping does not leak into a verb's targeting.
```

- [ ] **Step 2: Run it and watch it fail** — `go test ./cmd/tmux-web/ -run TestState -v` → undefined: `runState`
- [ ] **Step 3: Implement `runState`, wire `case "state"`, add the usage line.** The settle it passes `LiveState` is a named constant in `control.go` with a comment saying why that value; `settleAfter`'s neighbour in `state.go` is the starting point.
- [ ] **Step 4: Run it and watch it pass** — `go test ./cmd/tmux-web/ -run TestState -v` → PASS
- [ ] **Step 5: Commit**

```bash
git add cmd/tmux-web/control.go cmd/tmux-web/control_test.go cmd/tmux-web/cli.go
git commit -m "feat: tmux-web state, and which authority said so"
```

---

## Task 5: `peek`

**Files:**
- Modify: `cmd/tmux-web/control.go`, `cmd/tmux-web/control_test.go`, `cmd/tmux-web/cli.go`

**Interfaces:**
- Produces: `func runPeek(args []string, stdout, stderr io.Writer, getenv func(string) string, dial func(string) controlClient) int`

`Capture` for the screen, `CaptureRange(paneID, lines)` for `--lines N`. When `CaptureRange` reports truncation, say so on stderr — a reader that does not know its tail was cut will answer the wrong question.

- [ ] **Step 1: Write the failing test**

```go
func TestPeekPrintsTheScreen(t *testing.T)
func TestPeekWithLinesReadsTheScrollback(t *testing.T)   // CaptureRange, not Capture
func TestPeekSaysWhenTheScrollbackWasTruncated(t *testing.T)
//   truncated=true -> a note on stderr, the text still on stdout, exit 0.
```

- [ ] **Step 2: Run it and watch it fail** — `go test ./cmd/tmux-web/ -run TestPeek -v`
- [ ] **Step 3: Implement `runPeek`, wire `case "peek"`, add the usage line**
- [ ] **Step 4: Run it and watch it pass**
- [ ] **Step 5: Commit**

```bash
git add cmd/tmux-web/control.go cmd/tmux-web/control_test.go cmd/tmux-web/cli.go
git commit -m "feat: tmux-web peek, the screen and the scrollback behind it"
```

---

## Task 6: `open`

**Files:**
- Modify: `cmd/tmux-web/control.go`, `cmd/tmux-web/control_test.go`, `cmd/tmux-web/cli.go`
- Create: `cmd/tmux-web/control_integration_test.go`

**Interfaces:**
- Consumes: `CallerSession` (Task 1), `NewWindow`, `SplitPane`, `SetLabel`.
- Produces: `func runOpen(args []string, stdout, stderr io.Writer, getenv func(string) string, dial func(string) controlClient) int`

An **empty shell**, which is what `NewWindow` is for: *"New window means an empty shell to all four of its callers"* (`manage.go:84`). Nothing is handed a command here. Prints the new pane id on stdout and nothing else, so `$(tmux-web open --split)` is the pane id.

Two facts this task must handle, both read from the code rather than assumed:

- **Neither helper passes `-d` today.** `manage.go:98` builds `new-window -t <session> -P -F '#{window_id}'` and `manage.go:175` builds `split-window <flag> -t <pane> -c <dir> -P -F '#{pane_id}'`. Without `-d` both make the new thing current, so an agent's `open` would yank the owner's terminal off whatever he was reading — anti-goal 1, reached by a tmux default. Add `-d` **behind a new parameter**, with the browser's four existing callers passing the value that keeps today's behaviour, so no browser-visible change rides along. The constraint that `git diff --stat internal/front web/` stays empty is what checks this.
- **`NewWindow` returns a window id, not a pane id.** Its format is `#{window_id}`. `open --window` must therefore resolve the new window's single pane with one `display-message -p -t <windowID> '#{pane_id}'` before printing. `SplitPane` already returns `#{pane_id}` and needs no second call.

- [ ] **Step 1: Write the failing test**

```go
// control_test.go
func TestOpenWithAWindowNameUsesTheCallersSession(t *testing.T)
func TestOpenWithSplitSplitsTheCallersOwnPane(t *testing.T)
//   the fromPane/paneID argument is $TMUX_PANE, not the window's active pane.
func TestOpenPrintsNothingButThePaneID(t *testing.T)
//   for BOTH modes: --split prints SplitPane's pane id, and --window resolves
//   NewWindow's window id to a pane id rather than printing the window id.
func TestOpenAppliesALabel(t *testing.T)
func TestOpenRefusesAnAppOwnedSession(t *testing.T)
//   caller's only row is AppOwned -- a window created there dies with the tab.

// control_integration_test.go, against a throwaway socket (-L, -f /dev/null)
func TestOpenLeavesTheOwnersViewWhereItWas(t *testing.T)
//   a real server, two windows, the caller in window 0: after `open --window x`
//   the session's active window is still 0. This is the -d assertion, and it is
//   the only place it can be made.
func TestOpenGivesAPaneAShellThatOutlivesACommand(t *testing.T)
//   open, send a command that exits non-zero, then assert pane_dead=0, the
//   output is still in the capture, and a second command runs. This is the
//   measurement that deleted remain-on-exit; it belongs in the suite.
```

- [ ] **Step 2: Run them and watch them fail** — `go test ./cmd/tmux-web/ -run TestOpen -v`
- [ ] **Step 3: Implement `runOpen`, wire `case "open"`, add the usage line**
- [ ] **Step 4: Run them and watch them pass** — `go test ./cmd/tmux-web/ -run TestOpen -v` → PASS
- [ ] **Step 5: Commit**

```bash
git add cmd/tmux-web/control.go cmd/tmux-web/control_test.go cmd/tmux-web/control_integration_test.go cmd/tmux-web/cli.go
git commit -m "feat: tmux-web open, an empty pane the owner can see"
```

---

## Task 7: `type`

**Files:**
- Modify: `cmd/tmux-web/control.go`, `cmd/tmux-web/control_test.go`, `cmd/tmux-web/control_integration_test.go`, `cmd/tmux-web/cli.go`

**Interfaces:**
- Produces: `func runType(args []string, stdout, stderr io.Writer, getenv func(string) string, dial func(string) controlClient) int`

This is `send-keys`, the transport this repo has twice refused, and the spec accepts it knowingly. What the refusals measured, and what this task must therefore do:

- **Copy mode: refuse.** *"`send-keys` does not type into a pane in copy mode, it dispatches copy-mode commands"*, and on 3.7b against such a pane with a client attached the command **did not return**. The guard reads `Row.PaneMode`, which is already on every snapshot row. Not a cancel-first like the browser's: a cancel changes what the owner is looking at.
- **Multi-line: bracket it, send no Return.** `ReplyBox.tsx:18-22` wraps text containing a newline in `ESC[200~`…`ESC[201~` for a measured reason — the alternative is one submitted turn per newline, and *"twelve submitted turns is not an acceptable failure"*. `--enter` together with multi-line text is refused outright and the message says to submit it in the pane.
- **Size: refuse past a cap, and name the file instead.** `send-keys -H` was measured at 16 331 bytes succeeding and 16 355 failing with `command too long` — a limit on the *command line*. Chunking is the other answer and this plan does not take it: the owner's own rule is that long text goes to a file (*"siempre puedes pedirle al agente que te lo escriba en /tmp/xxx.md"*), so `type` caps at 4096 bytes of text and the refusal says to use a file. **Plan-level decision, from the spec's rule rather than from the spec's text.**
- **Text is text.** `send-keys -l --` so that a leading `-` or a tmux key name in the text reaches the pane as characters. `--key` takes exactly one tmux key name and goes without `-l`.

- [ ] **Step 1: Write the failing test**

```go
func TestTypeSendsTheTextAndThenEnter(t *testing.T)
//   --enter produces the Return as its own key, not "\n" inside the text.
func TestTypeSendsTextAsTextEvenWhenItLooksLikeOptions(t *testing.T)  // Review Focus 4
//   text "--help" and "-l": assert `-l` and `--` are in the argv before it.
func TestTypeRefusesTheCallersOwnPane(t *testing.T)
func TestTypeRefusesAPaneInCopyMode(t *testing.T)
//   Row.PaneMode non-empty -> exit 1, a message naming copy mode, and ZERO Runs.
func TestTypeBracketsMultiLineTextAndSendsNoReturn(t *testing.T)
func TestTypeRefusesEnterWithMultiLineText(t *testing.T)
func TestTypeRefusesTextOverTheCapAndNamesTheFileRoute(t *testing.T)
func TestTypeWithAKeySendsOneKey(t *testing.T)   // --key C-c
func TestTypeSaysSoWhenThePaneDiedUnderIt(t *testing.T)   // Review Focus 2
//   the pane is in the snapshot, then Run returns tmux's "can't find pane"
//   -> exit 1 naming the pane, and no other pane was written to.

// control_integration_test.go
func TestTypeRunsACommandInARealShell(t *testing.T)
//   open a pane on a throwaway socket, type a command with --enter, capture and
//   find its output. The end-to-end proof that open+type is what a person does.
```

- [ ] **Step 2: Run them and watch them fail** — `go test ./cmd/tmux-web/ -run TestType -v`
- [ ] **Step 3: Implement `runType`, wire `case "type"`, add the usage line**
- [ ] **Step 4: Run them and watch them pass**, then `go test -race ./cmd/tmux-web/`
- [ ] **Step 5: Commit**

```bash
git add cmd/tmux-web/control.go cmd/tmux-web/control_test.go cmd/tmux-web/control_integration_test.go cmd/tmux-web/cli.go
git commit -m "feat: tmux-web type, with the three limits send-keys actually has"
```

---

## Task 8: `close`

**Files:**
- Modify: `cmd/tmux-web/control.go`, `cmd/tmux-web/control_test.go`, `cmd/tmux-web/cli.go`

**Interfaces:**
- Produces: `func runClose(args []string, stdout, stderr io.Writer, getenv func(string) string, dial func(string) controlClient) int`

The owner put this verb back after revision 2 dropped it: *"La idea es que el agente pueda hacer lo que yo hago. Yo abro una pane/window lo uso y lo cierro cuando quiero."* So it is **not** restricted to panes the agent created. Two guards only: the caller's own pane, because closing it kills the agent mid-turn and no agent means that, and app-owned sessions.

- [ ] **Step 1: Write the failing test**

```go
func TestCloseClosesANamedPane(t *testing.T)
func TestCloseRefusesTheCallersOwnPane(t *testing.T)
func TestCloseRefusesAnAppOwnedSession(t *testing.T)
func TestCloseIsNotLimitedToPanesTheAgentCreated(t *testing.T)
//   a pane with no connection to this caller closes. Pins the owner's decision
//   so a later reader does not "tighten" it back.
```

- [ ] **Step 2: Run them and watch them fail** — `go test ./cmd/tmux-web/ -run TestClose -v`
- [ ] **Step 3: Implement `runClose` over `KillPane`, wire `case "close"`, add the usage line**
- [ ] **Step 4: Run them and watch them pass**
- [ ] **Step 5: Commit**

```bash
git add cmd/tmux-web/control.go cmd/tmux-web/control_test.go cmd/tmux-web/cli.go
git commit -m "feat: tmux-web close, because the owner closes panes too"
```

---

## Task 9: `run` — `open`, then `type --enter`

**Files:**
- Modify: `cmd/tmux-web/control.go`, `cmd/tmux-web/control_test.go`, `cmd/tmux-web/control_integration_test.go`, `cmd/tmux-web/cli.go`

**Interfaces:**
- Consumes: `runOpen` (Task 6) and `runType` (Task 7) — their logic, not their output formatting.
- Produces: `func runRun(args []string, stdout, stderr io.Writer, getenv func(string) string, dial func(string) controlClient) int`

Sugar, and the plan's only shared-logic task: the command is **one shell-command string**, the owner's decision, typed into the shell `open` created. Nothing reaches `new-window` as an argument. If the open succeeds and the type fails, the pane stays and the message says which half failed — a half-done `run` the agent cannot see would be worse than either outcome.

- [ ] **Step 1: Write the failing test**

```go
func TestRunOpensAPaneAndTypesTheCommandIntoIt(t *testing.T)
//   one call: the NewWindow/SplitPane happens, then send-keys with the string
//   and a Return. Assert new-window's argv carries NO command.
func TestRunPrintsThePaneIDSoTheAgentCanWatchIt(t *testing.T)
func TestRunKeepsThePaneAndSaysWhichHalfFailedWhenTypeFails(t *testing.T)
func TestRunTakesTheCommandAsOneShellString(t *testing.T)
//   `run 'printf a && printf b'` arrives as one argument, unsplit.

// control_integration_test.go
func TestRunStartsSomethingThePanesListingThenShows(t *testing.T)
//   run a sleep on a throwaway socket, then `panes` reports that pane's command
//   as the running program rather than the shell. Pins the spec's observation
//   that pane_current_command reports the child.
```

- [ ] **Step 2: Run them and watch them fail** — `go test ./cmd/tmux-web/ -run TestRun -v`
- [ ] **Step 3: Implement `runRun`, wire `case "run"`, add the usage line**
- [ ] **Step 4: Run them and watch them pass**, then the whole package: `go test -race ./cmd/tmux-web/`
- [ ] **Step 5: Commit**

```bash
git add cmd/tmux-web/control.go cmd/tmux-web/control_test.go cmd/tmux-web/control_integration_test.go cmd/tmux-web/cli.go
git commit -m "feat: tmux-web run, which is open and type in one call"
```

---

## Task 10: The installer carries more than one owned file

**Files:**
- Modify: `cmd/tmux-web/install.go` (`agentSpec`, `installPlan`, `tmuxWebSchema`)
- Test: `cmd/tmux-web/install_test.go` (or wherever the existing install tests live)

**Interfaces:**
- Consumes: `agentSpec{file, source, mode, settings, global, projectWarning}` (`install.go:164`), `managedHeader` (`install.go:59`), `tmuxWebSchema = 1` (`install.go:52`).
- Produces: `agentSpec` whose owned files are a slice rather than one `file`/`source` pair, and `tmuxWebSchema = 2`.

The shape is built for three agents and filled in for one. The owner uses skills in all three — *"yo uso los skills en los 3 para muchas cosas"* — and asked to start with claude: *"puedes empezar por claude asi lo probamos y cuando funcione bien investigamos para los otros dos, pero seguro que se puede."* So opencode and pi keep exactly one owned file today and gain a second without a refactor.

The schema bump is what `install.go:48` describes it as: the shape of what gets installed changed, so an existing file of ours is replaced **with a note** rather than silently.

- [ ] **Step 1: Write the failing test**

```go
func TestAnAgentCanOwnMoreThanOneFile(t *testing.T)
//   claude's spec installs both the wrapper and the skill; both carry the
//   managed header; both are listed in the plan shown before confirmation.
func TestAgentsWithOneOwnedFileAreUnchanged(t *testing.T)
//   opencode and pi still write exactly one file each, same paths as before.
func TestASchemaOneFileIsReplacedWithANote(t *testing.T)
//   an existing file whose header says schema 1 -> replaced, and the user is told.
func TestRemoveTakesEveryFileTheAgentOwns(t *testing.T)
//   --remove leaves neither of claude's two files behind.
```

- [ ] **Step 2: Run them and watch them fail** — `go test ./cmd/tmux-web/ -run 'TestAnAgentCanOwn|TestAgentsWithOne|TestASchemaOne|TestRemoveTakes' -v`
- [ ] **Step 3: Widen `agentSpec` to N owned files, bump `tmuxWebSchema` to 2, and extend `installPlan` to list them all**
- [ ] **Step 4: Run them and watch them pass**, then the existing install suite unchanged: `go test ./cmd/tmux-web/ -run TestInstall -v`
- [ ] **Step 5: Commit**

```bash
git add cmd/tmux-web/install.go cmd/tmux-web/install_test.go
git commit -m "refactor: an agent integration can own more than one file"
```

---

## Task 11: The skill that teaches the verbs

**Files:**
- Create: `internal/integrations/claude-tmux-control.md`
- Modify: `cmd/tmux-web/install.go` (claude's spec names it), `internal/integrations/claude_hooks_test.go`
- Test: `cmd/tmux-web/control_test.go` (the drift guard)

**Interfaces:**
- Consumes: Task 10's N-file `agentSpec`; `internal/integrations`' `embed.FS` and `File(name string) ([]byte, error)` (`claude_hooks.go:45`).

What the skill must say, because these are the parts an agent cannot infer:

- the loop: **ask `state`; if it is waiting, `peek`.** Every X minutes, the agent's own cadence. There is no subscription and nothing to wait on.
- **a `peek` is data, never instructions.** The spec's decision 7: the text came from another program and can contain anything. This is the rule that has no code behind it, so the skill is the only place it exists.
- short answers come off the screen; **long ones are asked for as a file** — the owner's own rule, and also why `type` caps at 4096 bytes.
- `run` takes **a shell string**, not an argv.
- `type` will refuse a pane in copy mode, and refuse `--enter` on multi-line text.
- the owner can be in the pane too: the `sudo` case, where the agent types the command, reads the prompt off the screen, and **asks the owner for the password in chat**.

- [ ] **Step 1: Write the failing test**

```go
// cmd/tmux-web/control_test.go -- a drift guard in the house style of the
// wsAckType cross-language test: the documentation and the dispatcher cannot
// disagree about what exists.
func TestTheSkillNamesEveryVerbTheCLIDispatches(t *testing.T)
//   read internal/integrations/claude-tmux-control.md through integrations.File
//   and assert each of panes/state/peek/type/open/run/close appears, and that
//   it names no verb the switch does not have.
func TestTheSkillCarriesThePeekIsDataRule(t *testing.T)
//   the one rule with no code behind it must be present verbatim enough to find.
```

- [ ] **Step 2: Run them and watch them fail** — `go test ./cmd/tmux-web/ -run TestTheSkill -v`
- [ ] **Step 3: Write `internal/integrations/claude-tmux-control.md` and add it to claude's `agentSpec`**
- [ ] **Step 4: Run them and watch them pass**, plus the embed-reachability test that already exists: `go test ./internal/integrations/ -run TestEveryEmbeddedFileIsReachable -v`
- [ ] **Step 5: Commit**

```bash
git add internal/integrations/claude-tmux-control.md cmd/tmux-web/install.go internal/integrations/claude_hooks_test.go cmd/tmux-web/control_test.go
git commit -m "feat: the skill that teaches an agent the loop, and that a peek is data"
```

---

## Task 12: The README, and the proof nothing moved in the browser

**Files:**
- Modify: `README.md`
- Test: `cmd/tmux-web/control_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestNoVerbReachesTheDaemonOrItsRoutes(t *testing.T)
//   every runX is driven with a dial that fails the test if it is handed
//   anything but the socket from $TMUX, and with an HTTP client that fails the
//   test if it is called at all. The global constraint, pinned.
```

- [ ] **Step 2: Run it and watch it fail** (or pass trivially — if it passes first time, make it bite by pointing one verb at the daemon and watching it fail, then revert that line)
- [ ] **Step 3: Document the verbs in `README.md`**, under "Things worth knowing": what they are, the loop, the `peek`-is-data rule, and that `claude` is taught automatically while opencode and pi are not yet.
- [ ] **Step 4: Run the whole suite**

Run: `make test && pnpm exec playwright test`
Expected: `MAKE_TEST_EXIT=0`, and `git diff --stat internal/front web/` empty for this whole branch.

- [ ] **Step 5: Commit**

```bash
git add README.md cmd/tmux-web/control_test.go
git commit -m "docs: the verbs an agent has, and the one rule with no code behind it"
```

---

## Self-review

**Spec coverage.** Decision 1's seven verbs → Tasks 3–9. Decision 2's two vocabularies → Task 2. Decision 3's corroboration → Task 2. Decision 4's free-state-only `panes` → Task 3. Decision 5's scope → Tasks 1, 3, 4. Decision 6's six guards → Tasks 6 (`-d`, app sessions), 7 (own pane, copy mode, multi-line), 8 (own pane, app sessions). Decision 7's rule → Task 11. Decision 8's placement and the installer → Tasks 10, 11. The three sequencing phases are Tasks 1–5, 6–9, 10–12.

**Gaps found and closed while reviewing.** The spec never said what `type` does past `send-keys`' command-line limit; Task 7 settles it as a refusal that names the file route, derived from the owner's own long-answers-to-a-file rule, and flags it as a plan-level decision. The spec's `-d` requirement needed somewhere to live when `NewWindow`/`SplitPane` do not already pass it; Task 6 says how to add it without a browser-visible change.

**Open questions 1–4 from the spec** are the builder's, as it says: the settle constant (Task 4, step 3), the `--split` base pane (Task 6, pinned by a test either way), `--key`'s vocabulary (Task 7), and `panes`' exact format (Task 3, step 3).
