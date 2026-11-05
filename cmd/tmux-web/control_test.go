package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/diegok/tmux-web/internal/tmux"
)

// The field order of one `panes` record, named so an assertion says which
// column it is about. Five later verbs and the skill quote this order, so a
// test that referred to fields[4] would be the only thing holding it.
const (
	fPaneID = iota
	fAddress
	fCommand
	fAgent
	fState
	fLabel
	fTitle
	fieldsPerLine
)

func TestPanesListsTheCallersSessionByDefault(t *testing.T) {
	stub := &controlStub{rows: twoSessions()}
	var dialled []string
	dial := func(socket string) controlClient {
		dialled = append(dialled, socket)
		return stub
	}

	var out, errb bytes.Buffer
	code := runPanes(nil, &out, &errb, callerEnv("%1"), dial)
	if code != 0 {
		t.Fatalf("exited %d: %s", code, errb.String())
	}
	// The socket comes from $TMUX and not from a bare `tmux`: the caller's
	// server is not necessarily the default one.
	if len(dialled) != 1 || dialled[0] != "/tmp/sock" {
		t.Fatalf("dialled %q, want one client on the socket named by $TMUX", dialled)
	}
	for _, pane := range []string{"%1", "%2", "%3"} {
		if paneFields(t, out.String(), pane) == nil {
			t.Errorf("%s is in the caller's session and is missing from:\n%s", pane, out.String())
		}
	}
	// $1 and $2 are other sessions nobody asked about. Listing them is not a
	// leak, it is noise that makes the default listing stop being the caller's
	// own.
	for _, pane := range []string{"%7", "%8"} {
		if paneFields(t, out.String(), pane) != nil {
			t.Errorf("%s belongs to another session and must not be listed by default:\n%s", pane, out.String())
		}
	}
}

func TestPanesWithAllListsEverySession(t *testing.T) {
	stub := &controlStub{rows: twoSessions()}
	var out, errb bytes.Buffer
	code := runPanes([]string{"--all"}, &out, &errb, callerEnv("%1"), stubDial(stub))
	if code != 0 {
		t.Fatalf("exited %d: %s", code, errb.String())
	}
	for _, pane := range []string{"%1", "%2", "%3", "%7", "%8"} {
		if paneFields(t, out.String(), pane) == nil {
			t.Errorf("--all must list %s:\n%s", pane, out.String())
		}
	}
}

// What a verb takes and what a human reads are two different strings, and the
// listing carries both: tmux prefix-matches session and window NAMES without
// reporting the ambiguity, so every verb here takes a %N -- and a %N is not
// something the owner can find on his own screen.
func TestPanesPrintsThePaneIDAVerbCanTake(t *testing.T) {
	rows := twoSessions()
	// A label and a title on one row, so the two free-form fields are exercised
	// by something other than the empty string.
	rows[1].Label = "deploy"
	rows[1].Title = "~/devel/sandbox"

	stub := &controlStub{
		rows: rows,
		// On %8, whose session name has a space in it, so the positional
		// assertion below is made against a line where getting it wrong moves
		// the state column rather than merely looking odd.
		reports: map[string]string{"%8": tmux.FormatReport(tmux.StateWorking, time.Now().UnixMilli(), "")},
	}
	var out, errb bytes.Buffer
	if code := runPanes([]string{"--all"}, &out, &errb, callerEnv("%1"), stubDial(stub)); code != 0 {
		t.Fatalf("exited %d: %s", code, errb.String())
	}

	for _, want := range []struct{ pane, address string }{
		{"%1", "work:0.0"},
		{"%2", "work:1.0"},
		{"%3", "work:1.1"},
		{"%7", "other:0.0"},
		// Every run of whitespace in a session name collapses to one "_". The
		// name is mangled visibly on purpose: printed raw it would make this one
		// line carry eight fields where every other line carries seven, and the
		// reader counting columns has no way to notice.
		{"%8", "my_work:0.0"},
	} {
		f := paneFields(t, out.String(), want.pane)
		if f == nil {
			t.Fatalf("no record for %s:\n%s", want.pane, out.String())
		}
		if f[fPaneID] != want.pane {
			t.Errorf("%s: first field = %q, want the pane id a verb can take", want.pane, f[fPaneID])
		}
		if f[fAddress] != want.address {
			t.Errorf("%s: address = %q, want %q", want.pane, f[fAddress], want.address)
		}
		// Every record has every field, so a reader counting columns is never
		// handed a short line: an unset label or title prints as "-".
		if len(f) != fieldsPerLine {
			t.Errorf("%s: %d fields (%q), want %d", want.pane, len(f), f, fieldsPerLine)
		}
	}
	f := paneFields(t, out.String(), "%2")
	if f[fLabel] != "deploy" || f[fTitle] != "~/devel/sandbox" {
		t.Errorf("%%2: label = %q, title = %q, want the two values the row carries", f[fLabel], f[fTitle])
	}
	if un := paneFields(t, out.String(), "%1"); un[fLabel] != "-" || un[fTitle] != "-" {
		t.Errorf("%%1 has neither label nor title: got %q and %q, want \"-\" for both", un[fLabel], un[fTitle])
	}

	// FIELDS 1 TO 5 ARE POSITIONAL, and this is the line that proves it: a
	// session named "my work" is the one value in the first five that could
	// carry a space, and the skill tells an agent to read the state out of
	// position 5. Asserted the way such a reader would do it -- on whitespace,
	// not on the separator -- because `awk '{print $5}'` is what will be run
	// against this, and paneFields' own split would not notice an extra field.
	line := paneLineFor(t, out.String(), "%8")
	if cols := strings.Fields(line); len(cols) != fieldsPerLine || cols[fState] != tmux.StateWorking {
		t.Errorf("splitting %q on whitespace gave %d columns with state = %q; want %d columns and %q in position 5",
			line, len(cols), cols[fState], fieldsPerLine, tmux.StateWorking)
	}
}

// The state column is ONLY WHAT `state` WOULD ANSWER WITHOUT A FORK -- an
// unexpired `working` report -- and a dash for every other pane, including the
// ones holding a resting report.
//
// Four of the five rows below are dashes, and the two resting ones are the
// point: they are not missing information, they are the design refusing to hold
// a second opinion. `state` corroborates a standing `blocked` report with a
// capture and answers `idle` when no dialog is on screen, and it gives an
// `idle` report nothing at all and asks the screen, which can come back
// `working`. A listing that printed either word would contradict `state` about
// the same pane in the same second, which this design calls a failure even with
// every test green.
//
// The zero-capture assertion is the other half of the same claim: a listing
// that computed LiveState per pane would print the same word for %1, so the
// output alone cannot tell the two apart. What tells them apart is the cost --
// two or three forks and a settle wait per agent pane -- and a dash is this
// verb saying "ask `state`".
func TestPanesStateColumnIsTheFreeOneOrADash(t *testing.T) {
	now := time.Now()
	fresh := now.UnixMilli()
	// Past workingTTL (60s), so the claim that something is happening NOW has
	// nothing standing behind it.
	stale := now.Add(-61 * time.Second).UnixMilli()

	rows := []tmux.Row{
		paneFixture("$0", "work", "%1", 0, 0, "claude"),
		paneFixture("$0", "work", "%2", 1, 0, "zsh"),
		paneFixture("$0", "work", "%3", 1, 1, "pi"),
		paneFixture("$0", "work", "%4", 2, 0, "zsh"),
		paneFixture("$0", "work", "%5", 3, 0, "claude"),
		paneFixture("$0", "work", "%6", 4, 0, "opencode"),
	}
	stub := &controlStub{
		rows: rows,
		reports: map[string]string{
			"%1": tmux.FormatReport(tmux.StateWorking, fresh, "running the tests"),
			// %2 has none at all, which is every pane with no integration
			// installed and every pane that is not an agent.
			"%3": tmux.FormatReport(tmux.StateWorking, stale, ""),
			// A shell holding a fresh report: the agent that wrote it exited and
			// the option outlived it. The command is what says so, and it is the
			// same rule the poller applies.
			"%4": tmux.FormatReport(tmux.StateWorking, fresh, "a report its writer left behind"),
			// A standing blocked report, in force as far as the clock is
			// concerned -- resting states do not expire on one. It still prints
			// "-": `state` spends a capture asking whether the dialog is
			// actually on screen and answers `idle` when it is not, so printing
			// `blocked` here would be the disagreement the design forbids, on
			// the value reports.go names as the one most likely to be wrong.
			"%5": tmux.FormatReport(tmux.StateBlocked, now.Add(-10*time.Minute).UnixMilli(), "may I run rm -rf"),
			// An idle report, fresh, and also "-". LiveState gives an idle
			// report nothing and asks the screen instead, whose two captures
			// can come back `working`; this listing has taken no captures and
			// has no business guessing which way that went.
			"%6": tmux.FormatReport(tmux.StateIdle, fresh, ""),
		},
	}

	var out, errb bytes.Buffer
	if code := runPanes(nil, &out, &errb, callerEnv("%1"), stubDial(stub)); code != 0 {
		t.Fatalf("exited %d: %s", code, errb.String())
	}
	for pane, want := range map[string]string{
		"%1": tmux.StateWorking, // fresh working report: the one state printed here
		"%2": "-",               // no report at all
		"%3": "-",               // a working report past workingTTL
		"%4": "-",               // a shell holding a report its writer left behind
		"%5": "-",               // a standing blocked report, which `state` may drop
		"%6": "-",               // an idle report, which `state` lets the screen overrule
	} {
		f := paneFields(t, out.String(), pane)
		if f == nil {
			t.Fatalf("no record for %s:\n%s", pane, out.String())
		}
		if f[fState] != want {
			t.Errorf("%s: state = %q, want %q", pane, f[fState], want)
		}
	}
	// The agent column says which vocabulary the state column speaks, and a
	// shell is not an agent however fresh the option on it is.
	if f := paneFields(t, out.String(), "%4"); f[fAgent] != "-" {
		t.Errorf("%%4 runs a shell: agent = %q, want \"-\"", f[fAgent])
	}

	if stub.captures != 0 {
		t.Errorf("took %d captures; the listing must cost no screen reads at all", stub.captures)
	}
	if stub.snapshots != 1 {
		t.Errorf("took %d snapshots, want 1: resolving the caller's session must reuse the one this verb already has", stub.snapshots)
	}
	if len(stub.runs) != 1 {
		t.Errorf("ran %v, want one command: every pane's report arrives in a single read", stub.runs)
	}
}

// Review Focus 1. Every verb needs the two variables tmux sets in a pane, and
// an agent that was started outside tmux has neither. Saying which variables
// are missing is the difference between a fixable message and "it did not
// work": there is nothing in the agent's own environment that would tell it.
//
// One line per verb. Tasks 4-9 add theirs as they land.
func TestEveryVerbSaysSoWhenItIsNotRunningInsideTmux(t *testing.T) {
	for _, v := range []struct {
		name string
		args []string
		run  controlVerb
	}{
		{"panes", nil, runPanes},
		{"state", []string{"%2"}, runState},
		{"peek", []string{"%2"}, runPeek},
	} {
		t.Run(v.name, func(t *testing.T) {
			dialled := 0
			var out, errb bytes.Buffer
			code := v.run(v.args, &out, &errb, envWithout("TMUX", "TMUX_PANE"),
				func(string) controlClient { dialled++; return &controlStub{} })
			if code != 1 {
				t.Errorf("exited %d, want 1: this is a failed operation, not a malformed command line", code)
			}
			if out.Len() != 0 {
				t.Errorf("wrote to stdout: %q -- stdout carries the answer and there is none", out.String())
			}
			for _, want := range []string{"TMUX", "TMUX_PANE"} {
				if !strings.Contains(errb.String(), "$"+want) {
					t.Errorf("stderr = %q, want $%s named in it", errb.String(), want)
				}
			}
			if dialled != 0 {
				t.Errorf("dialled %d clients; with no socket there is nothing to dial", dialled)
			}
		})
	}
}

// -- helpers ----------------------------------------------------------------

// controlVerb is the shape every verb's testable core has: the command line,
// the two streams, the environment and the dial, all injected. It is a type so
// that the table above is one line per verb.
type controlVerb func(args []string, stdout, stderr io.Writer, getenv func(string) string, dial func(socket string) controlClient) int

// callerEnv is the environment tmux gives a pane: the socket in $TMUX, the
// pane's own id in $TMUX_PANE. Never the real one -- see mapEnv.
func callerEnv(pane string) func(string) string {
	return mapEnv(map[string]string{"TMUX": "/tmp/sock,7,0", "TMUX_PANE": pane})
}

func stubDial(s *controlStub) func(string) controlClient {
	return func(string) controlClient { return s }
}

// twoSessions is a snapshot spanning the caller's session and two others: three
// panes over two windows in $0, where %1 is the caller, one pane in $1, and one
// in a session whose NAME CONTAINS A SPACE.
//
// That last row is not decoration. `tmux new -s "my work"` is an ordinary thing
// for the owner to have done -- ValidateSessionName governs only the sessions
// tmux-web creates -- and a name printed raw would add a field to its own line,
// which is how a state column comes to be read out of the wrong position for
// one pane out of twelve. The fixture is here so that nothing can regress to
// printing it raw without a test going red.
func twoSessions() []tmux.Row {
	return []tmux.Row{
		paneFixture("$0", "work", "%1", 0, 0, "claude"),
		paneFixture("$0", "work", "%2", 1, 0, "zsh"),
		paneFixture("$0", "work", "%3", 1, 1, "pi"),
		paneFixture("$1", "other", "%7", 0, 0, "zsh"),
		paneFixture("$2", "my work", "%8", 0, 0, "claude"),
	}
}

// paneFixture is one snapshot row with the fields a listing reads filled in.
// Deliberately hand-built: the panes on the developer's own server hold private
// work, and a fixture copied out of one would put it in a public repository.
func paneFixture(sessionID, sessionName, paneID string, windowIndex, paneIndex int, command string) tmux.Row {
	return tmux.Row{
		GroupKey:    sessionName,
		SessionID:   sessionID,
		SessionName: sessionName,
		PaneID:      paneID,
		PaneIndex:   paneIndex,
		WindowID:    "@" + strconv.Itoa(windowIndex),
		WindowIndex: windowIndex,
		WindowName:  "w" + strconv.Itoa(windowIndex),
		Command:     command,
	}
}

// paneLineFor returns paneID's whole record, unsplit, for an assertion about
// how a reader would split it.
func paneLineFor(t *testing.T, out, paneID string) string {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		if strings.HasPrefix(line, paneID+paneFieldSep) {
			return line
		}
	}
	t.Fatalf("no record for %s:\n%s", paneID, out)
	return ""
}

// paneFields finds paneID's record in a listing and splits it back into its
// fields, or returns nil if the listing has no record for that pane.
//
// It splits on the separator rather than on whitespace because two of the
// fields -- a session name and a title -- may legitimately contain a space.
func paneFields(t *testing.T, out, paneID string) []string {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		if f := strings.Split(line, paneFieldSep); f[0] == paneID {
			return f
		}
	}
	return nil
}

// controlStub is the controlClient these tests drive.
//
// It counts what it was asked to do, because the expensive way to build a
// listing and the free way print the SAME WORDS: only the call log can tell a
// state the report gave away from one a pair of captures computed. Tasks 4-9
// script the rest of the interface through the same fields.
type controlStub struct {
	rows []tmux.Row
	// reports is each pane's raw @tmux_web_agent value, keyed by pane id, as
	// the option itself holds it.
	reports map[string]string
	screens map[string]string
	// truncated is what CaptureRange reports; rangeLines records the depth it
	// was asked for, so a test can tell the scrollback read from the bare one.
	truncated  bool
	rangeLines []int
	// newPane is what a window or a split answers with.
	newPane string
	err     error

	snapshots int
	runs      [][]string
	captures  int
	// calls is the management half of the interface: one entry per call, the
	// method and its arguments, for the verbs that change something.
	calls [][]string
}

func (s *controlStub) Snapshot(context.Context) ([]tmux.Row, error) {
	s.snapshots++
	return s.rows, s.err
}

func (s *controlStub) Run(_ context.Context, args ...string) (string, error) {
	s.runs = append(s.runs, append([]string(nil), args...))
	if s.err != nil {
		return "", s.err
	}
	// The report block of a list-panes read. Everything else answers empty,
	// which is what a tmux read of an option nothing set amounts to.
	if len(args) > 0 && args[0] == "list-panes" {
		return renderReportBlock(s.rows, s.reports), nil
	}
	// One pane's own option, which is what LiveState reads. An unset option is
	// an error, as tmux makes it: exit 1, and the ordinary shape of a pane with
	// no integration installed.
	if len(args) > 0 && args[0] == "show-options" {
		if v := s.reports[args[len(args)-3]]; v != "" {
			return v, nil
		}
		return "", errors.New("invalid option: " + tmux.AgentOption)
	}
	return "", nil
}

func (s *controlStub) Capture(_ context.Context, paneID string) (string, error) {
	s.captures++
	return s.screens[paneID], s.err
}

func (s *controlStub) CaptureRange(_ context.Context, paneID string, lines int) (string, bool, error) {
	s.captures++
	s.rangeLines = append(s.rangeLines, lines)
	return s.screens[paneID], s.truncated, s.err
}

func (s *controlStub) NewWindow(_ context.Context, sessionID, name, fromPane string) (string, error) {
	s.calls = append(s.calls, []string{"new-window", sessionID, name, fromPane})
	return s.newPane, s.err
}

func (s *controlStub) SplitPane(_ context.Context, paneID, direction string) (string, error) {
	s.calls = append(s.calls, []string{"split-pane", paneID, direction})
	return s.newPane, s.err
}

func (s *controlStub) SetLabel(_ context.Context, paneID, label string) error {
	s.calls = append(s.calls, []string{"set-label", paneID, label})
	return s.err
}

func (s *controlStub) KillPane(_ context.Context, paneID string) error {
	s.calls = append(s.calls, []string{"kill-pane", paneID})
	return s.err
}

// renderReportBlock is what tmux prints for the report read: one line per pane,
// carrying the raw option value.
//
// It is built by SUBSTITUTING INTO tmux.ReportFormat rather than by retyping
// the block's shape. The tag that labels the block is unexported, and a stub
// holding its own copy of it would keep passing if production's ever changed --
// which is the one way this read can break while every assertion below still
// agrees with itself.
//
// Two substitutions, in order, and NOT a split on the separator: the option's
// own format expression contains a literal 0x1f inside its substitution pattern
// (see sanitizedField), so splitting the format into fields cuts that
// expression in half. #{pane_id} is replaced where it stands, and everything
// from the next "#{" to the end of the format is the option -- which holds
// because the option is the LAST field of this format and the only variable one
// left after the pane id.
func renderReportBlock(rows []tmux.Row, reports map[string]string) string {
	var b strings.Builder
	for _, r := range rows {
		line := strings.Replace(tmux.ReportFormat, "#{pane_id}", r.PaneID, 1)
		if i := strings.Index(line, "#{"); i >= 0 {
			line = line[:i] + reports[r.PaneID]
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

// -- state ------------------------------------------------------------------

// fixtureScreen reads a screen from the tmux package's testdata, which is
// hand-built, so that this suite and LiveState's are looking at the same dialog
// rather than at two ideas of one.
func fixtureScreen(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "internal", "tmux", "testdata", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return string(b)
}

// stateRun runs `state` on one pane as the caller %1 and returns its exit code
// and both streams.
func stateRun(stub *controlStub, pane string) (int, string, string) {
	var out, errb bytes.Buffer
	code := runState([]string{pane}, &out, &errb, callerEnv("%1"), stubDial(stub))
	return code, out.String(), errb.String()
}

func TestStatePrintsOneWordAndThenTheQuestion(t *testing.T) {
	stub := &controlStub{
		rows:    twoSessions(),
		screens: map[string]string{"%1": fixtureScreen(t, "claude-blocked.txt")},
	}
	code, out, errs := stateRun(stub, "%1")
	if code != 0 {
		t.Fatalf("exited %d: %s", code, errs)
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	// The first line is one word and nothing else: the owner's loop reads it
	// with `head -1`, and everything after it is for whoever decides to look.
	if lines[0] != tmux.StateBlocked {
		t.Errorf("first line = %q, want exactly %q", lines[0], tmux.StateBlocked)
	}
	if !strings.Contains(out, "Do you want to create fixture.txt?") {
		t.Errorf("the question is missing from:\n%s", out)
	}
	if !strings.Contains(out, "1. Yes") || !strings.Contains(out, "3. No") {
		t.Errorf("the choices are missing from:\n%s", out)
	}

	// A working pane is one word and nothing after it.
	working := &controlStub{
		rows:    twoSessions(),
		reports: map[string]string{"%1": tmux.FormatReport(tmux.StateWorking, time.Now().UnixMilli(), "running the tests")},
	}
	_, out, _ = stateRun(working, "%1")
	if out != "working\n" {
		t.Errorf("a working pane printed %q, want \"working\\n\"", out)
	}
	// And a fresh working report is trusted without a fork: this is half of the
	// pair of facts that keep `panes` and `state` from disagreeing.
	if working.captures != 0 {
		t.Errorf("took %d captures on a fresh working report, want 0", working.captures)
	}
}

// A report's Activity is the integration's own words and ExtractQuestion is a
// grammar's reading of pixels; they are different texts, and printing them in
// one unlabelled field would let an agent quote a guess as a statement.
func TestStateNamesWhichAuthorityAnswered(t *testing.T) {
	blockedAt := time.Now().Add(-3 * time.Minute).UnixMilli()
	for _, tc := range []struct {
		name     string
		reports  map[string]string
		want     string // the authority that must be named
		notWant  string
		question string
	}{
		{"the report", map[string]string{"%1": tmux.FormatReport(tmux.StateBlocked, blockedAt, "Allow rm -rf?")}, "report", "screen", "Allow rm -rf?"},
		{"the screen", nil, "screen", "report", "Do you want to create fixture.txt?"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := &controlStub{
				rows:    twoSessions(),
				reports: tc.reports,
				screens: map[string]string{"%1": fixtureScreen(t, "claude-blocked.txt")},
			}
			code, out, errs := stateRun(stub, "%1")
			if code != 0 {
				t.Fatalf("exited %d: %s", code, errs)
			}
			lines := strings.Split(out, "\n")
			if lines[0] != "blocked" {
				t.Fatalf("first line = %q, want \"blocked\"", lines[0])
			}
			rest := strings.Join(lines[1:], "\n")
			if !strings.Contains(rest, tc.want) || strings.Contains(rest, tc.notWant) {
				t.Errorf("want %q named and %q not named after the first line:\n%s", tc.want, tc.notWant, out)
			}
			if !strings.Contains(rest, tc.question) {
				t.Errorf("want the question %q in:\n%s", tc.question, out)
			}
		})
	}
}

// A state-only report: the integration said blocked and not what for, so the
// question is a grammar's reading of the screen. Labelling it "from the report"
// would present that reading as the integration's own statement.
func TestStateLabelsAScreenReadQuestionAsTheScreensWhenTheReportHadNoText(t *testing.T) {
	stub := &controlStub{
		rows: twoSessions(),
		reports: map[string]string{
			"%1": tmux.FormatReport(tmux.StateBlocked, time.Now().Add(-3*time.Minute).UnixMilli(), ""),
		},
		screens: map[string]string{"%1": fixtureScreen(t, "claude-blocked.txt")},
	}
	code, out, errs := stateRun(stub, "%1")
	if code != 0 {
		t.Fatalf("exited %d: %s", code, errs)
	}
	lines := strings.Split(out, "\n")
	if lines[0] != "blocked" {
		t.Fatalf("first line = %q, want \"blocked\"", lines[0])
	}
	if want := "state from the report, question read from the screen:"; lines[1] != want {
		t.Errorf("label = %q, want %q", lines[1], want)
	}
	if !strings.Contains(out, "Do you want to create fixture.txt?") {
		t.Errorf("the question is missing from:\n%s", out)
	}
}

func TestStateSaysAStandingReportWasDropped(t *testing.T) {
	stub := &controlStub{
		rows: twoSessions(),
		reports: map[string]string{
			"%1": tmux.FormatReport(tmux.StateBlocked, time.Now().Add(-3*time.Minute).UnixMilli(), "Allow?"),
		},
		screens: map[string]string{"%1": fixtureScreen(t, "claude-idle.txt")},
	}
	code, out, errs := stateRun(stub, "%1")
	if code != 0 {
		t.Fatalf("exited %d: %s", code, errs)
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if lines[0] != "idle" {
		t.Errorf("first line = %q, want \"idle\"", lines[0])
	}
	// The reader who sees idle where a report said blocked is owed the reason,
	// and the reason is on the lines after the word, not in place of it.
	if len(lines) < 2 || !strings.Contains(lines[1], "blocked") || !strings.Contains(lines[1], "dropped") {
		t.Errorf("want a line after \"idle\" saying a blocked report was dropped, got:\n%s", out)
	}
	if stub.captures != 1 {
		t.Errorf("took %d captures, want 1: corroboration is one capture", stub.captures)
	}
}

func TestStateRefusesAPaneThatDoesNotExist(t *testing.T) {
	stub := &controlStub{rows: twoSessions()}
	code, out, errs := stateRun(stub, "%999")
	if code != 1 {
		t.Errorf("exited %d, want 1", code)
	}
	if out != "" {
		t.Errorf("stdout = %q, want nothing: there is no answer", out)
	}
	if !strings.Contains(errs, "%999") {
		t.Errorf("stderr = %q, want the pane named", errs)
	}
	if stub.captures != 0 || len(stub.runs) != 0 {
		t.Errorf("forked for a pane that is not there: %d captures, runs %v", stub.captures, stub.runs)
	}

	// A malformed target is a command-line error, and is refused before tmux
	// is asked anything at all.
	for _, bad := range []string{"work:0.0", "3", ""} {
		fresh := &controlStub{rows: twoSessions()}
		if code, _, _ := stateRun(fresh, bad); code != 2 {
			t.Errorf("state %q exited %d, want 2", bad, code)
		}
		if fresh.snapshots != 0 {
			t.Errorf("state %q read a snapshot before refusing it", bad)
		}
	}
}

// `panes` scopes to the caller's session because "everything here" is the wrong
// default for a listing. A verb that takes a pane id has no default to apply:
// the id IS the target, named, and %7 is as good a name as %1.
func TestStateAcceptsAnExplicitlyNamedPaneFromAnotherSession(t *testing.T) {
	stub := &controlStub{
		rows:    twoSessions(),
		screens: map[string]string{"%7": "$ \n"},
	}
	code, out, errs := stateRun(stub, "%7")
	if code != 0 {
		t.Fatalf("exited %d: %s", code, errs)
	}
	// %7 runs zsh, so it gets the vocabulary of a pane that is not an agent.
	if out != "quiet\n" {
		t.Errorf("printed %q, want \"quiet\\n\"", out)
	}
}

func TestStateSaysUnknownWhenTheScreenCannotBeRead(t *testing.T) {
	stub := &controlStub{rows: twoSessions(), err: nil}
	failing := &failingCapture{controlStub: stub}
	var out, errb bytes.Buffer
	code := runState([]string{"%3"}, &out, &errb, callerEnv("%1"), func(string) controlClient { return failing })
	if code != 1 {
		t.Errorf("exited %d, want 1", code)
	}
	if out.String() != "unknown\n" {
		t.Errorf("stdout = %q, want \"unknown\\n\"", out.String())
	}
	if !strings.Contains(errb.String(), "capture exploded") {
		t.Errorf("stderr = %q, want the cause", errb.String())
	}
}

// failingCapture is a stub whose screen reads fail and whose other reads work,
// which controlStub's single err field cannot express.
type failingCapture struct{ *controlStub }

func (f *failingCapture) Capture(context.Context, string) (string, error) {
	return "", errors.New("capture exploded")
}

func peekRun(stub *controlStub, args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := runPeek(args, &out, &errb, callerEnv("%1"), stubDial(stub))
	return code, out.String(), errb.String()
}

func TestPeekPrintsTheScreen(t *testing.T) {
	screen := fixtureScreen(t, "claude-blocked.txt")
	stub := &controlStub{rows: twoSessions(), screens: map[string]string{"%1": screen}}
	code, out, errs := peekRun(stub, "%1")
	if code != 0 || errs != "" {
		t.Fatalf("exit %d, stderr %q", code, errs)
	}
	if out != screen {
		t.Errorf("stdout is not the screen as captured:\n%s", out)
	}
	if len(stub.rangeLines) != 0 {
		t.Errorf("the bare form read scrollback: %v", stub.rangeLines)
	}
}

func TestPeekWithLinesReadsTheScrollback(t *testing.T) {
	stub := &controlStub{rows: twoSessions(), screens: map[string]string{"%1": "history\nscreen\n"}}
	code, out, errs := peekRun(stub, "--lines", "40", "%1")
	if code != 0 || errs != "" {
		t.Fatalf("exit %d, stderr %q", code, errs)
	}
	if out != "history\nscreen\n" {
		t.Errorf("stdout = %q", out)
	}
	if len(stub.rangeLines) != 1 || stub.rangeLines[0] != 40 {
		t.Errorf("CaptureRange calls = %v, want one for 40 lines", stub.rangeLines)
	}
	if stub.captures != 1 {
		t.Errorf("%d captures, want exactly the one range read", stub.captures)
	}
}

func TestPeekSaysWhenTheScrollbackWasTruncated(t *testing.T) {
	stub := &controlStub{rows: twoSessions(), screens: map[string]string{"%1": "tail\n"}, truncated: true}
	code, out, errs := peekRun(stub, "--lines", "5000", "%1")
	if code != 0 {
		t.Fatalf("exit %d, want 0: a truncated read is not a failure", code)
	}
	if out != "tail\n" {
		t.Errorf("stdout = %q, want the text and nothing else", out)
	}
	if !strings.Contains(errs, "truncated") {
		t.Errorf("stderr = %q, want a note saying the read was truncated", errs)
	}
}

func TestPeekRefusesAPaneThatDoesNotExistWithoutReadingIt(t *testing.T) {
	stub := &controlStub{rows: twoSessions()}
	code, out, errs := peekRun(stub, "%999")
	if code != 1 || out != "" || !strings.Contains(errs, "%999") {
		t.Errorf("exit %d, stdout %q, stderr %q", code, out, errs)
	}
	if stub.captures != 0 {
		t.Errorf("%d captures for a pane that is not there", stub.captures)
	}
}

func TestPeekRefusesALinesCountThatIsNotPositive(t *testing.T) {
	for _, n := range []string{"0", "-3"} {
		stub := &controlStub{rows: twoSessions(), screens: map[string]string{"%1": "x"}}
		if code, _, _ := peekRun(stub, "--lines", n, "%1"); code != 2 || stub.captures != 0 {
			t.Errorf("--lines %s: exit %d, %d captures; want 2 and none", n, code, stub.captures)
		}
	}
}
