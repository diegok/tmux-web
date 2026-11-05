package main

import (
	"bytes"
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/diegok/tmux-web/internal/integrations"
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
		{"open", []string{"--split"}, runOpen},
		{"type", []string{"--text", "hi", "%2"}, runType},
		{"close", []string{"%2"}, runClose},
		{"run", []string{"--split", "make test"}, runRun},
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
	// newPane is what a split answers with, and what a window's pane resolves
	// to through windowPanes. newWindow is the WINDOW id a new window answers
	// with -- a different kind of id, which is the whole reason `open --window`
	// needs a second read before it can print anything.
	newPane     string
	newWindow   string
	windowPanes map[string]string
	err         error

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
	// The window-to-pane read `open --window` makes. Answered from a map keyed
	// by the target, so a read that lost its -t or carried the wrong id comes
	// back empty -- which is exactly what tmux does: display-message prints an
	// empty expansion and exits 0 for a target it cannot find.
	if len(args) > 0 && args[0] == "display-message" {
		return s.windowPanes[argAfter(args, "-t")], nil
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

// The two detached verbs, which are the only ones the CLI has: whether -d is on
// the command line is invisible through this seam, and the integration test is
// where that is established.
func (s *controlStub) NewWindowDetached(_ context.Context, sessionID, name, fromPane string) (string, error) {
	s.calls = append(s.calls, []string{"new-window", sessionID, name, fromPane})
	return s.newWindow, s.err
}

func (s *controlStub) SplitPaneDetached(_ context.Context, paneID, direction string) (string, error) {
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

// argAfter returns the value following flag in an argv, or "" if it is absent.
func argAfter(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
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

// -- open -------------------------------------------------------------------

// openRun runs `open` as the caller %1 and returns its exit code and streams.
func openRun(stub *controlStub, args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := runOpen(args, &out, &errb, callerEnv("%1"), stubDial(stub))
	return code, out.String(), errb.String()
}

// callerWithABrowserTab is twoSessions plus the row an open browser tab adds:
// the caller's own pane, reported a second time in a grouped app-owned session.
//
// It is the fixture `open --window` has to be right about. Both rows name the
// same physical pane and only one of them is a usable answer -- a _web-* session
// carries destroy-unattached, so a window created there dies when the tab
// closes. A snapshot holding only the owner's row cannot tell a resolution that
// prefers correctly from one that returns whichever row it saw first.
func callerWithABrowserTab() []tmux.Row {
	tab := paneFixture("$9", "_web-ab12cd", "%1", 0, 0, "claude")
	tab.AppOwned = true
	return append(twoSessions(), tab)
}

// windowStub is a stub whose new window is @7 and whose @7 holds one pane, %42:
// the shape `open --window` reads back through.
func windowStub(rows []tmux.Row) *controlStub {
	return &controlStub{rows: rows, newWindow: "@7", windowPanes: map[string]string{"@7": "%42"}}
}

func TestOpenWithAWindowNameUsesTheCallersSession(t *testing.T) {
	stub := windowStub(callerWithABrowserTab())
	code, out, errs := openRun(stub, "--window", "api")
	if code != 0 || errs != "" {
		t.Fatalf("exit %d, stderr %q", code, errs)
	}
	if out != "%42\n" {
		t.Errorf("stdout = %q, want the new pane id", out)
	}
	// $0 is the session the owner keeps; $9 is the browser tab's. The name is
	// passed through, and fromPane is the CALLER's pane -- that is what gives
	// the new window the directory the agent is working in.
	want := []string{"new-window", "$0", "api", "%1"}
	if len(stub.calls) == 0 || !slices.Equal(stub.calls[0], want) {
		t.Errorf("calls = %v, want the first to be %v", stub.calls, want)
	}
}

func TestOpenWithSplitSplitsTheCallersOwnPane(t *testing.T) {
	stub := &controlStub{rows: twoSessions(), newPane: "%42"}
	code, out, errs := openRun(stub, "--split")
	if code != 0 || errs != "" {
		t.Fatalf("exit %d, stderr %q", code, errs)
	}
	if out != "%42\n" {
		t.Errorf("stdout = %q, want the new pane id", out)
	}
	// %1 is $TMUX_PANE. tmux's own default target is whatever pane is current,
	// which in a window the owner is working in is somebody else's, so the
	// caller's id has to be passed explicitly.
	want := []string{"split-pane", "%1", tmux.SplitRight}
	if len(stub.calls) != 1 || !slices.Equal(stub.calls[0], want) {
		t.Errorf("calls = %v, want exactly %v", stub.calls, want)
	}
	// The caller's pane comes from the environment, so a split needs nothing
	// read off the server: no snapshot, and the session is never resolved.
	if stub.snapshots != 0 {
		t.Errorf("%d snapshots for a split, want none: $TMUX_PANE is the target", stub.snapshots)
	}
}

func TestOpenPrintsNothingButThePaneID(t *testing.T) {
	t.Run("split", func(t *testing.T) {
		stub := &controlStub{rows: twoSessions(), newPane: "%42"}
		code, out, errs := openRun(stub, "--split")
		if code != 0 || out != "%42\n" || errs != "" {
			t.Errorf("exit %d, stdout %q, stderr %q; want just the pane id on stdout", code, out, errs)
		}
	})
	t.Run("window", func(t *testing.T) {
		// A new window answers with a WINDOW id, and @7 is not a pane: a verb
		// handed it would target the window containing some other pane, or
		// nothing at all.
		stub := windowStub(callerWithABrowserTab())
		code, out, errs := openRun(stub, "--window", "api")
		if code != 0 || errs != "" {
			t.Fatalf("exit %d, stderr %q", code, errs)
		}
		if out != "%42\n" {
			t.Errorf("stdout = %q, want %q -- the window's pane, resolved, not the window id %q",
				out, "%42\n", stub.newWindow)
		}
		// Resolved by asking about the window that was just created, which is
		// the only id that answers with a pane this verb made.
		var reads [][]string
		for _, r := range stub.runs {
			if len(r) > 0 && r[0] == "display-message" {
				reads = append(reads, r)
			}
		}
		if len(reads) != 1 {
			t.Fatalf("display-message reads = %v, want exactly one", reads)
		}
		if argAfter(reads[0], "-t") != "@7" {
			t.Errorf("display-message target = %q, want the new window @7: %v", argAfter(reads[0], "-t"), reads[0])
		}
	})
}

func TestOpenAppliesALabel(t *testing.T) {
	stub := &controlStub{rows: twoSessions(), newPane: "%42"}
	code, out, errs := openRun(stub, "--split", "--label", "web server")
	if code != 0 || errs != "" {
		t.Fatalf("exit %d, stderr %q", code, errs)
	}
	if out != "%42\n" {
		t.Errorf("stdout = %q, want the pane id and nothing else -- the label is not printed", out)
	}
	// On the NEW pane, never on the caller's: %1 is the agent's own pane and
	// relabelling it would rename the row the owner uses to find the agent.
	want := []string{"set-label", "%42", "web server"}
	if len(stub.calls) != 2 || !slices.Equal(stub.calls[1], want) {
		t.Errorf("calls = %v, want %v after the split", stub.calls, want)
	}
}

// failingLabel opens panes and then cannot label them, which `err` alone
// cannot do: it would fail the split as well, and the question here is what
// happens to a pane that exists.
type failingLabel struct{ *controlStub }

func (f *failingLabel) SetLabel(context.Context, string, string) error {
	return errors.New("set label: can't find pane")
}

// The pane id reaches stdout even though the verb exits 1, and that is the
// decision being pinned: the pane is already open, and a caller that was not
// given its id has leaked a pane it cannot name, kill or look at. `state`
// prints its word before reporting a failed read for the same reason.
func TestOpenPrintsThePaneIDEvenWhenTheLabelFails(t *testing.T) {
	stub := &controlStub{rows: twoSessions(), newPane: "%42"}
	var out, errb bytes.Buffer
	code := runOpen([]string{"--split", "--label", "web server"}, &out, &errb,
		callerEnv("%1"), func(string) controlClient { return &failingLabel{stub} })
	if code != 1 {
		t.Errorf("exit %d, want 1: the label was asked for and not set", code)
	}
	if out.String() != "%42\n" {
		t.Errorf("stdout = %q, want the pane id: it exists whether or not it got its label", out.String())
	}
	if !strings.Contains(errb.String(), "%42") {
		t.Errorf("stderr = %q, want the pane named so the caller knows what was left behind", errb.String())
	}
}

func TestOpenRefusesAnAppOwnedSession(t *testing.T) {
	// The caller's ONLY row is the browser tab's session, which is what a
	// snapshot looks like once the owner has killed the base session with a tab
	// still open: the group survives with app-owned members only, so
	// CallerSession answers one -- it says deciding is the caller's job.
	only := paneFixture("$9", "_web-ab12cd", "%1", 0, 0, "claude")
	only.AppOwned = true
	stub := windowStub([]tmux.Row{only})
	code, out, errs := openRun(stub, "--window", "api")
	if code != 1 {
		t.Errorf("exit %d, want 1: the verb could not do its job", code)
	}
	if out != "" {
		t.Errorf("stdout = %q, want nothing: no pane was opened", out)
	}
	for _, want := range []string{"$9", "--split"} {
		if !strings.Contains(errs, want) {
			t.Errorf("stderr = %q, want %q in it", errs, want)
		}
	}
	// Refused before the server was asked to do anything, not undone after.
	if len(stub.calls) != 0 {
		t.Errorf("calls = %v, want none", stub.calls)
	}
}

func TestOpenRefusesAMalformedCommandLine(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"no mode", nil},
		{"both modes", []string{"--split", "--window"}},
		{"a name for a split", []string{"--split", "api"}},
		{"two names", []string{"--window", "api", "www"}},
		// Refused here rather than by tmux, so that a name the server would
		// reject costs no fork and exits 2 like the command-line error it is.
		{"an unusable window name", []string{"--window", "a:b"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := windowStub(twoSessions())
			code, out, _ := openRun(stub, tc.args...)
			if code != 2 {
				t.Errorf("exit %d, want 2: a malformed command line", code)
			}
			if out != "" {
				t.Errorf("stdout = %q, want nothing", out)
			}
			if len(stub.calls) != 0 || stub.snapshots != 0 {
				t.Errorf("calls = %v and %d snapshots; nothing may be touched", stub.calls, stub.snapshots)
			}
		})
	}
}

// -- type -------------------------------------------------------------------

// typeRun runs `type` as the caller %1 and returns its exit code and streams.
func typeRun(stub *controlStub, args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := runType(args, &out, &errb, callerEnv("%1"), stubDial(stub))
	return code, out.String(), errb.String()
}

// copyModeRows is twoSessions with %2 -- the pane every test here types into --
// sitting in copy mode, which is the one pane state `send-keys` cannot type
// into at all.
func copyModeRows() []tmux.Row {
	rows := twoSessions()
	rows[1].PaneMode = "copy-mode"
	return rows
}

// The Return is a KEY of its own and not a byte inside the text.
//
// Measured on 3.7b: `send-keys -l -- "... Enter"` puts the five characters
// "Enter" on the pane's screen, and `send-keys -t %2 Enter` submits. So the
// two go out as two commands -- one literal, one a key lookup -- and a
// "\n" appended to the text would be 0x0a, which is C-j and not the Return an
// agent's prompt answers to.
func TestTypeSendsTheTextAndThenEnter(t *testing.T) {
	stub := &controlStub{rows: twoSessions()}
	code, out, errs := typeRun(stub, "--text", "make test", "--enter", "%2")
	if code != 0 || errs != "" {
		t.Fatalf("exit %d, stderr %q", code, errs)
	}
	if out != "" {
		t.Errorf("stdout = %q, want nothing: the answer is in the pane", out)
	}
	if len(stub.runs) != 2 {
		t.Fatalf("runs = %v, want two: the literal text, then the Return", stub.runs)
	}
	wantText := []string{"send-keys", "-l", "-t", "%2", "--", "make test"}
	if !slices.Equal(stub.runs[0], wantText) {
		t.Errorf("text argv = %v, want %v", stub.runs[0], wantText)
	}
	wantEnter := []string{"send-keys", "-t", "%2", "Enter"}
	if !slices.Equal(stub.runs[1], wantEnter) {
		t.Errorf("Return argv = %v, want %v -- a key lookup, since -l would type the word \"Enter\"", stub.runs[1], wantEnter)
	}
	// The text itself carries no line ending: a "\n" in it is 0x0a (C-j), and a
	// "\r" would submit from inside the text, where nothing bounds what was
	// submitted.
	if last := stub.runs[0][len(stub.runs[0])-1]; strings.ContainsAny(last, "\r\n") {
		t.Errorf("the text sent was %q, want no line ending in it", last)
	}
}

// Review Focus 4. The text is characters, whatever it looks like: tmux's own
// `send-keys` reads a bare "-l" as its flag and a bare "Enter" as a key name,
// so -l and -- are what stand between the caller's text and tmux's parser.
func TestTypeSendsTextAsTextEvenWhenItLooksLikeOptions(t *testing.T) {
	for _, text := range []string{"--help", "-l", "-t %9 rm -rf /"} {
		t.Run(text, func(t *testing.T) {
			stub := &controlStub{rows: twoSessions()}
			code, _, errs := typeRun(stub, "--text", text, "%2")
			if code != 0 || errs != "" {
				t.Fatalf("exit %d, stderr %q", code, errs)
			}
			if len(stub.runs) != 1 {
				t.Fatalf("runs = %v, want one", stub.runs)
			}
			argv := stub.runs[0]
			if last := argv[len(argv)-1]; last != text {
				t.Errorf("last argument = %q, want the text %q unchanged", last, text)
			}
			dashL := slices.Index(argv, "-l")
			term := slices.Index(argv, "--")
			if dashL < 0 || term != len(argv)-2 {
				t.Errorf("argv = %v, want -l present and -- immediately before the text", argv)
			}
			if dashL > term {
				t.Errorf("argv = %v, want -l before the -- terminator", argv)
			}
		})
	}
}

// An agent typing into its own pane is a loop, and no agent means it: the keys
// land in the agent's own prompt, which is the one place its author cannot
// have intended to reach by asking tmux.
func TestTypeRefusesTheCallersOwnPane(t *testing.T) {
	stub := &controlStub{rows: twoSessions()}
	code, out, errs := typeRun(stub, "--text", "hello", "%1")
	if code != 2 {
		t.Errorf("exit %d, want 2: the command line names the caller itself", code)
	}
	if out != "" {
		t.Errorf("stdout = %q, want nothing", out)
	}
	if !strings.Contains(errs, "%1") {
		t.Errorf("stderr = %q, want the caller's own pane named in it", errs)
	}
	// Decided from $TMUX_PANE alone, so the server is never asked anything.
	if len(stub.runs) != 0 || stub.snapshots != 0 {
		t.Errorf("runs = %v and %d snapshots, want none of either", stub.runs, stub.snapshots)
	}
}

// THE GUARD WITH THE WORST FAILURE BEHIND IT. `send-keys` against a pane in
// copy mode does not type: it dispatches copy-mode commands, and on 3.7b
// against such a pane with a client attached the command DID NOT RETURN. `q`
// is cancel in both copy-mode key tables, so what a pane in copy mode does to
// prose is worse than swallowing it -- everything after the first `q` leaves
// copy mode and reaches the shell as a command.
//
// ZERO RUNS is the assertion that matters. An exit of 1 after a send that
// already went out is not a refusal, and the send is the thing that hangs.
// This verb refuses rather than cancelling first, as the browser does: the
// browser owns the pane's attach, and a cancel here would change what the
// owner is looking at.
// BOTH WAYS IN, because the refusal is a property of the pane and not of the
// payload. --key is the half that looks harmless and is not: `q` is cancel in
// both copy-mode key tables, so `--key q` would pop the mode the owner is
// reading in -- the exact thing this verb refuses to decide for him, and the
// thing its own message tells the caller to ask HIM to do. The guard holds
// today only by sitting above the --key branch, and a refactor that moved it
// into the text path would leave that open with nothing going red.
func TestTypeRefusesAPaneInCopyMode(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"text", []string{"--text", "quit now", "--enter", "%2"}},
		{"key", []string{"--key", "q", "%2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := &controlStub{rows: copyModeRows()}
			code, out, errs := typeRun(stub, tc.args...)
			if code != 1 {
				t.Errorf("exit %d, want 1: the verb could not do its job", code)
			}
			if out != "" {
				t.Errorf("stdout = %q, want nothing", out)
			}
			if len(stub.runs) != 0 {
				t.Errorf("runs = %v, want NONE: a send-keys against a pane in copy mode can hang, so this must be refused before it goes out, not reported after", stub.runs)
			}
			for _, want := range []string{"copy mode", "%2"} {
				if !strings.Contains(errs, want) {
					t.Errorf("stderr = %q, want %q in it", errs, want)
				}
			}
		})
	}
}

// Multi-line text goes out BRACKETED and unsubmitted.
//
// `ESC[200~` ... `ESC[201~` is what tells the receiving agent a paste
// happened; all three agents set DECSET 2004 at their prompt and honour the
// wrappers, so the lines land as one draft. Without them each newline is a
// submit, and "twelve submitted turns is not an acceptable failure".
func TestTypeBracketsMultiLineTextAndSendsNoReturn(t *testing.T) {
	stub := &controlStub{rows: twoSessions()}
	code, out, errs := typeRun(stub, "--text", "first line\nsecond line", "%2")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	if out != "" {
		t.Errorf("stdout = %q, want nothing", out)
	}
	if len(stub.runs) != 1 {
		t.Fatalf("runs = %v, want exactly one: no Return may follow a paste", stub.runs)
	}
	argv := stub.runs[0]
	want := "\x1b[200~first line\nsecond line\x1b[201~"
	if last := argv[len(argv)-1]; last != want {
		t.Errorf("sent %q, want it wrapped: %q", last, want)
	}
	if slices.Contains(argv, "Enter") {
		t.Errorf("argv = %v, want no Return: the wrappers end the paste and the owner submits it", argv)
	}
	// Said out loud, because a caller that thinks it submitted twelve lines
	// will sit waiting for an answer that nobody asked the agent for. On
	// stderr with exit 0: the verb did what it was asked.
	if !strings.Contains(errs, "submit") {
		t.Errorf("stderr = %q, want it to say the text was not submitted and has to be submitted in the pane", errs)
	}
}

func TestTypeRefusesEnterWithMultiLineText(t *testing.T) {
	stub := &controlStub{rows: twoSessions()}
	code, out, errs := typeRun(stub, "--text", "first\nsecond", "--enter", "%2")
	if code != 2 {
		t.Errorf("exit %d, want 2: --enter and multi-line text are a contradiction in the command line", code)
	}
	if out != "" {
		t.Errorf("stdout = %q, want nothing", out)
	}
	if len(stub.runs) != 0 || stub.snapshots != 0 {
		t.Errorf("runs = %v and %d snapshots, want none of either", stub.runs, stub.snapshots)
	}
	for _, want := range []string{"--enter", "submit"} {
		if !strings.Contains(errs, want) {
			t.Errorf("stderr = %q, want %q in it -- the caller has to be told where the submit happens instead", errs, want)
		}
	}
}

// The cap is a refusal that names the file route, and deliberately not
// chunking: see maxTypeText.
func TestTypeRefusesTextOverTheCapAndNamesTheFileRoute(t *testing.T) {
	t.Run("over", func(t *testing.T) {
		stub := &controlStub{rows: twoSessions()}
		code, out, errs := typeRun(stub, "--text", strings.Repeat("x", maxTypeText+1), "%2")
		if code != 2 {
			t.Errorf("exit %d, want 2", code)
		}
		if out != "" {
			t.Errorf("stdout = %q, want nothing", out)
		}
		if len(stub.runs) != 0 || stub.snapshots != 0 {
			t.Errorf("runs = %v and %d snapshots, want none of either", stub.runs, stub.snapshots)
		}
		// The way out has to be in the message: an agent told only "too long"
		// will split the text and send it in pieces, which is the second
		// transport this design refused to invent.
		for _, want := range []string{"file", strconv.Itoa(maxTypeText)} {
			if !strings.Contains(errs, want) {
				t.Errorf("stderr = %q, want %q in it", errs, want)
			}
		}
	})
	// The boundary is a cap and not an approximation: exactly the cap goes.
	t.Run("exactly the cap", func(t *testing.T) {
		stub := &controlStub{rows: twoSessions()}
		code, _, errs := typeRun(stub, "--text", strings.Repeat("x", maxTypeText), "%2")
		if code != 0 || errs != "" {
			t.Errorf("exit %d, stderr %q; %d bytes is within the cap", code, errs, maxTypeText)
		}
	})
}

// --key is the other half of the verb: one tmux key name, looked up by tmux,
// and therefore without -l. It is how a pane gets a C-c or an Escape, neither
// of which is a character anyone can type as text.
func TestTypeWithAKeySendsOneKey(t *testing.T) {
	stub := &controlStub{rows: twoSessions()}
	code, out, errs := typeRun(stub, "--key", "C-c", "%2")
	if code != 0 || errs != "" {
		t.Fatalf("exit %d, stderr %q", code, errs)
	}
	if out != "" {
		t.Errorf("stdout = %q, want nothing", out)
	}
	want := []string{"send-keys", "-t", "%2", "C-c"}
	if len(stub.runs) != 1 || !slices.Equal(stub.runs[0], want) {
		t.Errorf("runs = %v, want exactly one %v -- no -l, or tmux would type the three characters", stub.runs, want)
	}
}

// deadPane is a pane that is in the snapshot and gone by the time the keys go
// out, which `err` alone cannot express: that would fail the snapshot too, and
// the question here is what happens to a send that tmux refuses.
type deadPane struct{ *controlStub }

func (d *deadPane) Run(ctx context.Context, args ...string) (string, error) {
	// Recorded through the stub, so the assertions below can still see every
	// argv this verb put on the wire.
	_, _ = d.controlStub.Run(ctx, args...)
	return "", errors.New("can't find pane: %2")
}

// Review Focus 2. A pane can be killed between the snapshot and the send --
// the owner closes it, the command in it exits -- and tmux answers that with
// "can't find pane". What must not happen is a confident exit 0, and what must
// not happen is the Return going out anyway.
func TestTypeSaysSoWhenThePaneDiedUnderIt(t *testing.T) {
	stub := &controlStub{rows: twoSessions()}
	var out, errb bytes.Buffer
	code := runType([]string{"--text", "make test", "--enter", "%2"}, &out, &errb,
		callerEnv("%1"), func(string) controlClient { return &deadPane{stub} })
	if code != 1 {
		t.Errorf("exit %d, want 1: the keys did not land", code)
	}
	if out.String() != "" {
		t.Errorf("stdout = %q, want nothing", out.String())
	}
	if !strings.Contains(errb.String(), "%2") {
		t.Errorf("stderr = %q, want the pane named", errb.String())
	}
	if len(stub.runs) != 1 {
		t.Errorf("runs = %v, want exactly one: a Return sent after the text failed would submit whatever was already in that pane's prompt", stub.runs)
	}
	// And nothing was written to any other pane: a failed target must never
	// become tmux's default one, which is whatever pane is current.
	for _, r := range stub.runs {
		if argAfter(r, "-t") != "%2" {
			t.Errorf("argv %v targets %q, want only the pane asked for", r, argAfter(r, "-t"))
		}
	}
}

func TestTypeRefusesAMalformedCommandLine(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"nothing to send", []string{"%2"}},
		{"both text and key", []string{"--text", "hi", "--key", "C-c", "%2"}},
		{"empty text", []string{"--text", "", "%2"}},
		{"no pane", []string{"--text", "hi"}},
		{"two panes", []string{"--text", "hi", "%2", "%3"}},
		// The %N and nothing else, for the reason `state` refuses one: tmux
		// resolves a malformed -t to whatever is current and exits 0.
		{"an address instead of a pane id", []string{"--text", "hi", "work:1.0"}},
		// --enter is about submitting text; --key presses exactly one key, and
		// the key that submits is --key Enter.
		{"enter with a key", []string{"--key", "C-c", "--enter", "%2"}},
		// Two key names in one --key would be two keys, silently.
		{"two keys", []string{"--key", "C-x C-c", "%2"}},
		// tmux would read this one as a flag of its own.
		{"a key that starts with a dash", []string{"--key", "-X", "%2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := &controlStub{rows: twoSessions()}
			code, out, _ := typeRun(stub, tc.args...)
			if code != 2 {
				t.Errorf("exit %d, want 2: a malformed command line", code)
			}
			if out != "" {
				t.Errorf("stdout = %q, want nothing", out)
			}
			if len(stub.runs) != 0 || stub.snapshots != 0 {
				t.Errorf("runs = %v and %d snapshots; nothing may be touched", stub.runs, stub.snapshots)
			}
		})
	}
}

// A pane that is not on the server gets a sentence naming it, as `state` and
// `peek` do, rather than tmux's own stderr in an agent's transcript -- and
// nothing is sent.
func TestTypeRefusesAPaneThatIsNotOnTheServer(t *testing.T) {
	stub := &controlStub{rows: twoSessions()}
	code, out, errs := typeRun(stub, "--text", "hi", "%99")
	if code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	if out != "" {
		t.Errorf("stdout = %q, want nothing", out)
	}
	if !strings.Contains(errs, "%99") {
		t.Errorf("stderr = %q, want the pane named", errs)
	}
	if len(stub.runs) != 0 {
		t.Errorf("runs = %v, want none", stub.runs)
	}
}

func closeRun(stub *controlStub, args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := runClose(args, &out, &errb, callerEnv("%1"), stubDial(stub))
	return code, out.String(), errb.String()
}

func TestCloseClosesANamedPane(t *testing.T) {
	stub := &controlStub{rows: twoSessions()}
	code, out, errs := closeRun(stub, "%2")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q, want 0", code, errs)
	}
	if out != "" {
		t.Errorf("stdout = %q, want nothing: the caller named the pane, so an echo tells it nothing", out)
	}
	if !reflect.DeepEqual(stub.calls, [][]string{{"kill-pane", "%2"}}) {
		t.Errorf("calls = %v, want exactly one kill-pane of %%2", stub.calls)
	}
}

func TestCloseRefusesTheCallersOwnPane(t *testing.T) {
	stub := &controlStub{rows: twoSessions()}
	code, out, errs := closeRun(stub, "%1")
	if code != 2 {
		t.Errorf("exit %d, want 2: the command line names the caller itself", code)
	}
	if out != "" {
		t.Errorf("stdout = %q, want nothing", out)
	}
	if !strings.Contains(errs, "%1") {
		t.Errorf("stderr = %q, want the caller's own pane named in it", errs)
	}
	// Decided from $TMUX_PANE alone: nothing dialled, read or killed.
	if len(stub.runs) != 0 || stub.snapshots != 0 || len(stub.calls) != 0 {
		t.Errorf("runs = %v, %d snapshots, calls = %v, want none of any", stub.runs, stub.snapshots, stub.calls)
	}
}

func TestCloseRefusesAnAppOwnedSession(t *testing.T) {
	rows := twoSessions()
	rows[1].AppOwned = true
	stub := &controlStub{rows: rows}
	code, out, errs := closeRun(stub, "%2")
	if code != 1 {
		t.Errorf("exit %d, want 1: the verb could not do its job", code)
	}
	if out != "" {
		t.Errorf("stdout = %q, want nothing", out)
	}
	if !strings.Contains(errs, "%2") {
		t.Errorf("stderr = %q, want the pane named", errs)
	}
	if len(stub.runs) != 0 || len(stub.calls) != 0 {
		t.Errorf("runs = %v, calls = %v, want NONE: nothing may be killed in a browser tab's session", stub.runs, stub.calls)
	}
}

// The owner's decision, pinned. An earlier design closed only panes the agent
// had opened itself, and he reversed it: "the idea is that the agent can do what
// I do. I open a pane/window, use it, and close it when I want." %7 is in
// another session and has no connection to the caller -- it was not opened by
// it, is not in its window, is not even in its session -- and it must close.
// If this test goes red because a restriction was added, the restriction is the
// bug, not the test.
func TestCloseIsNotLimitedToPanesTheAgentCreated(t *testing.T) {
	stub := &controlStub{rows: twoSessions()}
	code, _, errs := closeRun(stub, "%7")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q, want 0", code, errs)
	}
	if !reflect.DeepEqual(stub.calls, [][]string{{"kill-pane", "%7"}}) {
		t.Errorf("calls = %v, want kill-pane of %%7", stub.calls)
	}
}

// -- run --------------------------------------------------------------------

// runRun_ runs `run` as the caller %1 and returns its exit code and streams.
func runRun_(stub *controlStub, args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := runRun(args, &out, &errb, callerEnv("%1"), stubDial(stub))
	return code, out.String(), errb.String()
}

// The composition: the pane is made first, then the command goes into THAT
// pane as keystrokes. The assertion that matters most is what is absent from
// the creating call -- a command in new-window or split-window's argv would
// make it the pane's process, with no shell to type into afterwards and a pane
// that dies with it.
func TestRunOpensAPaneAndTypesTheCommandIntoIt(t *testing.T) {
	for _, mode := range []string{"--split", "--window"} {
		t.Run(mode, func(t *testing.T) {
			stub := windowStub(callerWithABrowserTab())
			stub.newPane = "%42"
			code, _, errs := runRun_(stub, mode, "make test")
			if code != 0 || errs != "" {
				t.Fatalf("exit %d, stderr %q", code, errs)
			}
			// The creating call is the first, and carries no command: the exact
			// argv `open` makes, nothing more.
			var want []string
			if mode == "--split" {
				want = []string{"split-pane", "%1", tmux.SplitRight}
			} else {
				want = []string{"new-window", "$0", "", "%1"}
			}
			if len(stub.calls) == 0 || !slices.Equal(stub.calls[0], want) {
				t.Errorf("calls = %v, want the first to be %v, with no command in it", stub.calls, want)
			}
			for _, call := range stub.calls {
				for _, a := range call {
					if strings.Contains(a, "make test") {
						t.Errorf("%v hands the command to a pane-creating call", call)
					}
				}
			}
			// Then typed, into the new pane and not the caller's, and submitted.
			var sends [][]string
			for _, r := range stub.runs {
				if r[0] == "send-keys" {
					sends = append(sends, r)
				}
			}
			wantSends := [][]string{
				{"send-keys", "-l", "-t", "%42", "--", "make test"},
				{"send-keys", "-t", "%42", "Enter"},
			}
			if len(sends) != 2 || !slices.Equal(sends[0], wantSends[0]) || !slices.Equal(sends[1], wantSends[1]) {
				t.Errorf("send-keys = %v, want %v", sends, wantSends)
			}
		})
	}
}

func TestRunPrintsThePaneIDSoTheAgentCanWatchIt(t *testing.T) {
	stub := &controlStub{rows: twoSessions(), newPane: "%42"}
	code, out, _ := runRun_(stub, "--split", "make test")
	if code != 0 || out != "%42\n" {
		t.Errorf("exit %d, stdout %q, want 0 and the pane id alone", code, out)
	}
}

// A type that fails leaves the pane in place: closing it would be a third
// operation that can itself fail, and the agent can peek the pane and decide.
// The id is printed regardless, and the message says which half it was.
func TestRunKeepsThePaneAndSaysWhichHalfFailedWhenTypeFails(t *testing.T) {
	stub := &controlStub{rows: twoSessions(), newPane: "%42"}
	var out, errb bytes.Buffer
	code := runRun([]string{"--split", "make test"}, &out, &errb,
		callerEnv("%1"), func(string) controlClient { return &deadPane{stub} })
	if code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	if out.String() != "%42\n" {
		t.Errorf("stdout = %q, want the pane id even though the type failed", out.String())
	}
	for _, want := range []string{"%42", "was opened", "not typed", "peek"} {
		if !strings.Contains(errb.String(), want) {
			t.Errorf("stderr = %q, want %q in it", errb.String(), want)
		}
	}
	for _, c := range stub.calls {
		if c[0] == "kill-pane" {
			t.Errorf("run closed the pane it opened: %v", c)
		}
	}
}

func TestRunSaysNothingWasRunWhenTheOpenFails(t *testing.T) {
	stub := &controlStub{rows: twoSessions(), err: errors.New("no server")}
	code, out, errs := runRun_(stub, "--split", "make test")
	if code != 1 || out != "" || !strings.Contains(errs, "nothing was opened") {
		t.Errorf("exit %d, stdout %q, stderr %q", code, out, errs)
	}
	for _, r := range stub.runs {
		if r[0] == "send-keys" {
			t.Errorf("typed %v although nothing was opened", r)
		}
	}
}

func TestRunTakesTheCommandAsOneShellString(t *testing.T) {
	cmd := "make test 2>&1 | tee /tmp/out && printf a && printf b"
	stub := &controlStub{rows: twoSessions(), newPane: "%42"}
	code, _, errs := runRun_(stub, "--split", cmd)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	var text []string
	for _, r := range stub.runs {
		if r[0] == "send-keys" && r[1] == "-l" {
			text = r
		}
	}
	if len(text) == 0 || text[len(text)-1] != cmd {
		t.Errorf("typed %v, want the whole string as the single final argument", text)
	}
}

func TestRunRefusesAMalformedCommandLine(t *testing.T) {
	for name, args := range map[string][]string{
		"no mode":       {"make test"},
		"both modes":    {"--split", "--window", "make test"},
		"no command":    {"--split"},
		"empty command": {"--split", ""},
		"two arguments": {"--split", "make", "test"},
		"multi-line":    {"--split", "a\nb"},
		"over the cap":  {"--split", strings.Repeat("x", maxTypeText+1)},
		"help":          {"--split", "--help"},
	} {
		t.Run(name, func(t *testing.T) {
			stub := &controlStub{rows: twoSessions(), newPane: "%42"}
			code, out, _ := runRun_(stub, args...)
			if code != 2 && !(name == "help" && code == 0) {
				t.Errorf("exit %d, want 2", code)
			}
			if out != "" || len(stub.calls) != 0 || len(stub.runs) != 0 {
				t.Errorf("stdout %q, calls %v, runs %v: nothing may happen", out, stub.calls, stub.runs)
			}
		})
	}
}

// -- the skill --------------------------------------------------------------

// The skill is the only part of this feature that cannot fail loudly. A verb
// renamed in cli.go leaves the file telling an agent to run a command that
// exits 2, and the agent read the file once, minutes ago, and has no way to
// find out. So the dispatcher is the source of truth and the prose is held
// against it, in the house style of this repo's other cross-artefact pins.
//
// skillSource is its name in internal/integrations' embed.FS. The installer
// writes these bytes to .claude/skills/tmux-web/SKILL.md; it is read through
// File rather than off disk so a drop from the //go:embed directive fails here
// the way it would fail an install.
const skillSource = "claude-skill.md"

func readSkill(t *testing.T) string {
	t.Helper()
	b, err := integrations.File(skillSource)
	if err != nil {
		t.Fatalf("reading the embedded %s: %v", skillSource, err)
	}
	return string(b)
}

// TestTheSkillNamesEveryVerbTheCLIDispatches is the drift guard: the
// documentation and the dispatcher cannot disagree about what exists.
//
// THERE IS NO SECOND LIST OF VERBS HERE, and that is the point -- a literal
// {"panes", "state", ...} in this file would be exactly the thing that drifts.
// Both halves are derived from the source:
//
//   - WHAT EXISTS is the case clauses of the switch in cli.go's run(), parsed
//     out of the file.
//   - WHICH OF THEM ARE THE AGENT'S is decided by WHERE EACH HANDLER LIVES.
//     `panes`, `state`, `peek`, `open`, `type`, `run` and `close` dispatch to
//     functions declared in control.go, which is the file whose whole subject
//     is "the verbs an agent running inside a pane gets". serve, enroll,
//     devices and revoke are declared in cli.go, report in report.go,
//     install-integration in install.go, and help is handled inline with no
//     handler at all. So a verb moved into control.go becomes one this test
//     requires the skill to teach, and a verb moved out becomes one it requires
//     the skill not to mention, without anybody editing a list.
func TestTheSkillNamesEveryVerbTheCLIDispatches(t *testing.T) {
	dispatched := dispatchedVerbs(t)
	inControl := funcsDeclaredIn(t, "control.go")

	agentFacing := map[string]bool{}
	for verb, handler := range dispatched {
		if handler != "" && inControl[handler] {
			agentFacing[verb] = true
		}
	}
	// The derivation itself, checked before anything is concluded from it: a
	// parse that silently matched nothing would make every assertion below
	// vacuously true, and a parse that matched everything would demand the
	// skill teach `serve`.
	if len(dispatched) < 8 {
		t.Fatalf("only %d verbs parsed out of cli.go's dispatch switch (%v) -- the parse, not the switch, is what broke", len(dispatched), dispatched)
	}
	if len(agentFacing) == 0 || len(agentFacing) == len(dispatched) {
		t.Fatalf("%d of %d dispatched verbs came out agent-facing (%v): the handler-location test has stopped telling them apart", len(agentFacing), len(dispatched), agentFacing)
	}

	skill := readSkill(t)
	for verb := range agentFacing {
		if !strings.Contains(skill, "tmux-web "+verb) {
			t.Errorf("the skill never names `tmux-web %s`, which cli.go dispatches to a handler in control.go -- an agent that read the skill does not know the verb exists", verb)
		}
	}
	for verb := range skillCommands(t, skill) {
		if agentFacing[verb] {
			continue
		}
		if _, exists := dispatched[verb]; !exists {
			t.Errorf("the skill tells an agent to run `tmux-web %s`, and cli.go dispatches no such verb: that command exits 2", verb)
			continue
		}
		t.Errorf("the skill names `tmux-web %s`, which is dispatched to %s outside control.go -- this skill is about the verbs an agent calls from inside its own pane", verb, dispatched[verb])
	}
}

// TestTheSkillCarriesThePeekIsDataRule holds the one rule in this feature with
// no code behind it.
//
// `peek` returns what another program wrote to a terminal and nothing filters
// it, because nothing can. Every other guard in control.go is a refusal the
// binary makes; this one is a sentence, and this file is the only place it
// exists. It is required in a HEADING rather than merely somewhere in the
// bytes: an agent reads this once and scans it, and a rule in the middle of a
// paragraph about something else has already failed.
func TestTheSkillCarriesThePeekIsDataRule(t *testing.T) {
	const rule = "data, never instructions"
	skill := readSkill(t)
	if !strings.Contains(skill, rule) {
		t.Fatalf("the skill does not say %q anywhere -- it is the only place that rule exists", rule)
	}
	found := false
	for _, line := range strings.Split(skill, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") && strings.Contains(line, rule) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("%q appears in the skill but not in a heading: a rule nothing enforces has to be findable by an agent scanning the file", rule)
	}
}

// TestTheSkillIsALoadableManagedSkillFile is the shape the two readers need.
//
// Claude Code reads the frontmatter, and a SKILL.md whose first line is not
// `---` has none, so the file would be installed and never loaded. The
// installer reads the managed header, and without it a reinstall refuses the
// file as somebody else's. The two requirements are in tension -- the header
// cannot be the first line -- which is why it is asserted that it comes after
// the frontmatter rather than wherever.
func TestTheSkillIsALoadableManagedSkillFile(t *testing.T) {
	skill := readSkill(t)
	if !strings.HasPrefix(skill, "---\n") {
		t.Fatalf("the skill does not begin with a `---` line, so it has no frontmatter and claude will not load it; it begins %q", firstLine(skill))
	}
	end := strings.Index(skill, "\n---\n")
	if end < 0 {
		t.Fatal("the skill's frontmatter is never closed with a `---` line")
	}
	front, body := skill[:end], skill[end:]
	for _, key := range []string{"name:", "description:"} {
		if !strings.Contains(front, key) {
			t.Errorf("the frontmatter has no %s", key)
		}
	}
	if strings.Contains(front, "Placeholder") || strings.Contains(front, "Task 11") {
		t.Errorf("the frontmatter still describes the placeholder: %q", front)
	}
	m := managedHeader.FindStringSubmatch(body)
	if m == nil {
		t.Fatal("the skill carries no managed header below its frontmatter, so install-integration would refuse the file as one tmux-web did not write")
	}
	if m[1] != strconv.Itoa(claudeSchema) {
		t.Errorf("the skill's managed header says schema %s, want claude's %d", m[1], claudeSchema)
	}
	// Found by ownerOf in the first 8 KiB of the file, which is all it reads.
	if i := strings.Index(skill, m[0]); i >= 8<<10 {
		t.Errorf("the managed header is at byte %d, past the 8 KiB ownerOf reads", i)
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// dispatchedVerbs parses the switch in cli.go's run() and returns every verb it
// dispatches, mapped to the name of the function that handles it -- "" for the
// ones handled inline, which is `help` and its flag spellings.
//
// Parsed rather than exercised: run() with a bad verb prints usage and exits 2,
// which tells a test nothing about WHICH verbs are good, and calling each
// candidate would run it.
func dispatchedVerbs(t *testing.T) map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "cli.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing cli.go: %v", err)
	}
	var sw *ast.SwitchStmt
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || fn.Name.Name != "run" {
			continue
		}
		ast.Inspect(fn, func(n ast.Node) bool {
			s, ok := n.(*ast.SwitchStmt)
			if !ok || sw != nil {
				return true
			}
			if id, ok := s.Tag.(*ast.Ident); ok && id.Name == "cmd" {
				sw = s
			}
			return true
		})
	}
	if sw == nil {
		t.Fatal("cli.go's run() no longer has a `switch cmd` -- this test reads the verb list out of it")
	}
	verbs := map[string]string{}
	for _, stmt := range sw.Body.List {
		clause, ok := stmt.(*ast.CaseClause)
		if !ok {
			continue
		}
		// The handler is whatever cmd* function the clause calls. Taken from
		// the clause's body rather than from the verb's own spelling, so
		// `install-integration` -> cmdInstall needs no rule.
		handler := ""
		for _, body := range clause.Body {
			ast.Inspect(body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if id, ok := call.Fun.(*ast.Ident); ok && strings.HasPrefix(id.Name, "cmd") {
					handler = id.Name
				}
				return true
			})
		}
		for _, expr := range clause.List {
			lit, ok := expr.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			verb, err := strconv.Unquote(lit.Value)
			if err != nil {
				continue
			}
			verbs[verb] = handler
		}
	}
	return verbs
}

// funcsDeclaredIn returns the names of the top-level functions one file in this
// package declares.
func funcsDeclaredIn(t *testing.T, file string) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", file, err)
	}
	names := map[string]bool{}
	for _, decl := range f.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil {
			names[fn.Name.Name] = true
		}
	}
	return names
}

// skillCommands returns every `tmux-web <word>` the skill names INSIDE CODE --
// a fenced block or an inline backtick span -- and nowhere else.
//
// Prose is excluded deliberately, and not to be lenient. "`tmux-web` does not
// check whether the program set bracketed-paste mode" is a sentence about the
// tool, not a claim that `does` is a verb, and a regexp over the whole file
// cannot tell that from a real command. Code spans can: everything an agent is
// told to RUN is in one, which is also what makes the skill copy-pasteable.
func skillCommands(t *testing.T, skill string) map[string]bool {
	t.Helper()
	inlineSpan := regexp.MustCompile("`([^`\n]*)`")
	// [ \t] and not \s: the spans are collected one per line below, and a
	// `\s` would walk off the end of one span onto the start of the next --
	// "`tmux-web` does not check ... from the `command` column" would be read
	// as a verb called `command`.
	command := regexp.MustCompile(`\btmux-web[ \t]+([a-z][a-z-]*)`)

	var code strings.Builder
	fenced := false
	for _, line := range strings.Split(skill, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			fenced = !fenced
			continue
		}
		if fenced {
			code.WriteString(line)
			code.WriteByte('\n')
			continue
		}
		for _, m := range inlineSpan.FindAllStringSubmatch(line, -1) {
			code.WriteString(m[1])
			code.WriteByte('\n')
		}
	}
	if fenced {
		t.Error("the skill has an unclosed ``` fence")
	}
	found := map[string]bool{}
	for _, m := range command.FindAllStringSubmatch(code.String(), -1) {
		found[m[1]] = true
	}
	if len(found) == 0 {
		t.Fatal("no `tmux-web <verb>` command was found in any code span of the skill: either it teaches nothing or this extraction is broken")
	}
	return found
}
