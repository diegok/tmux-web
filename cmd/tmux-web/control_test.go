package main

import (
	"bytes"
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"maps"
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

// The report read is the one fork this listing makes, and when it fails the
// listing STILL PRINTS -- with a line on stderr saying that none of the dashes
// is a claim about its pane.
//
// That note is the whole of this test. A dash means "ask `tmux-web state`" and
// never "nothing is happening" (freeStates' own invariant), but a reader
// handed a wall of them with no explanation reads the wall as the latter, and
// here EVERY pane is a dash including the ones that do have a fresh working
// report. Losing the reports must not lose the listing either: the pane ids,
// addresses and commands came from the snapshot and are all still true.
func TestPanesStillListsWhenTheReportsCannotBeRead(t *testing.T) {
	stub := &controlStub{
		rows: twoSessions(),
		reports: map[string]string{
			// A report that WOULD have printed `working`, so the dash below is
			// the lost read and not an expired value.
			"%1": tmux.FormatReport(tmux.StateWorking, time.Now().UnixMilli(), "running the tests"),
		},
	}
	var out, errb bytes.Buffer
	code := runPanes(nil, &out, &errb, callerEnv("%1"), func(string) controlClient {
		return &failingRun{controlStub: stub}
	})
	if code != 0 {
		t.Fatalf("exited %d, want 0: the snapshot worked, so there is a listing to print: %s", code, errb.String())
	}
	if f := paneFields(t, out.String(), "%1"); f == nil {
		t.Fatalf("no record for %%1, so the listing was lost with the reports:\n%s", out.String())
	} else if f[fState] != "-" {
		t.Errorf("%%1: state = %q, want \"-\": nothing was read, so nothing may be claimed", f[fState])
	}
	// The note, and the two things it has to carry: that the reports are what
	// failed, and the cause.
	if !strings.Contains(errb.String(), "could not read") || !strings.Contains(errb.String(), "list-panes exploded") {
		t.Errorf("stderr = %q, want a note naming the failure and its cause", errb.String())
	}
}

// failingRun is a stub whose tmux command runs fail and whose snapshot works,
// which controlStub's single err field cannot express -- it would fail the
// snapshot too, and then there would be no listing to print. The sibling of
// failingCapture below, for the other half of the interface.
type failingRun struct{ *controlStub }

func (f *failingRun) Run(context.Context, ...string) (string, error) {
	return "", errors.New("list-panes exploded")
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
	// "unknown", NOT nothing. This assertion was the other way round and the
	// contract it pinned changed on purpose: the first line of `state` is one
	// word on every path out of the verb, failures included, because a caller
	// reading `tmux-web state %7 | head -1` has no exit code left to consult
	// and a pane vanishing between a `panes` and a `state` is the ordinary
	// race in a feature whose own verbs create and destroy panes. "unknown"
	// means the read failed, which is the truth about a pane that is not on
	// the server, and it is never mistaken for quiet.
	if out != "unknown\n" {
		t.Errorf("stdout = %q, want \"unknown\\n\": the first line is one word on every path", out)
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

// The third of the three failure paths, and the first of them a caller meets:
// the snapshot read itself fails, so there is not even a row to ask about.
//
// Together with the two tests above this is the whole of `state`'s contract
// that the first line is one word -- a snapshot that failed, a pane that is
// not there, and a screen that could not be read all print "unknown".
func TestStateSaysUnknownWhenTheSnapshotCannotBeRead(t *testing.T) {
	stub := &controlStub{rows: twoSessions(), err: errors.New("snapshot exploded")}
	code, out, errs := stateRun(stub, "%1")
	if code != 1 {
		t.Errorf("exited %d, want 1", code)
	}
	if out != "unknown\n" {
		t.Errorf("stdout = %q, want \"unknown\\n\"", out)
	}
	if !strings.Contains(errs, "snapshot exploded") {
		t.Errorf("stderr = %q, want the cause", errs)
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

// A bad --label is a malformed command line, so NOTHING HAPPENS -- and for
// `run` that is the whole of the point.
//
// The label was validated only by tmux.SetLabel, which runs after the pane has
// been created, so `open` made a pane, was refused the label, exited 1 and left
// the pane behind, while `run` did worse: openLabelled answers a label failure
// with the new pane id, which runRun reads as "the pane is there and the
// command was NOT typed". A cosmetic name therefore cancelled the command the
// verb exists to run. Both contradict the doctrine in runType's header --
// everything decided without asking the server exits 2 and nothing is
// attempted -- so checkLabel applies tmux.ValidateLabel's three rules on the
// command line, before the environment is even read.
//
// ZERO SNAPSHOTS AS WELL AS ZERO RUNS, which is the part a weaker assertion
// would miss: a refusal that cost a fork would still be a refusal decided by
// asking the server.
func TestOpenAndRunRefuseABadLabelBeforeAnythingIsOpened(t *testing.T) {
	labels := map[string]string{
		"too long":          strings.Repeat("x", tmux.MaxLabel+1),
		"a control rune":    "tests\nand more",
		"invalid utf-8":     "tests\xff",
		"a C1 control rune": "tests\u0085here",
	}
	verbs := map[string]func(*controlStub, ...string) (int, string, string){
		"open": openRun,
		"run":  runRun_,
	}
	for verb, drive := range verbs {
		for name, label := range labels {
			t.Run(verb+"/"+name, func(t *testing.T) {
				stub := &controlStub{rows: twoSessions(), newPane: "%42"}
				args := []string{"--split", "--label", label}
				if verb == "run" {
					args = append(args, "make test")
				}
				code, out, errs := drive(stub, args...)
				if code != 2 {
					t.Errorf("exit %d, want 2: a label is decided without asking the server", code)
				}
				if out != "" {
					t.Errorf("stdout = %q, want nothing: no pane id, because no pane", out)
				}
				if errs == "" {
					t.Error("stderr is empty: the caller is owed the reason")
				}
				if stub.snapshots != 0 || len(stub.calls) != 0 || len(stub.runs) != 0 {
					t.Errorf("snapshots %d, calls %v, runs %v: nothing may be touched", stub.snapshots, stub.calls, stub.runs)
				}
			})
		}
	}
}

// The other half of the same rule: a label tmux WOULD take is not refused
// here, so the command line cannot grow a stricter notion of a label than the
// pane has. The whitespace-only case is SetLabel's own exception -- it clears
// the option rather than storing one -- and it is mirrored, not tightened.
func TestOpenAcceptsEveryLabelThePaneWouldTake(t *testing.T) {
	for name, label := range map[string]string{
		"at the limit":    strings.Repeat("\U0001f30d", tmux.MaxLabel),
		"leading dash":    "-x",
		"a colon and dot": "web:server.1",
		"whitespace only": "   ",
		"a newline alone": "\n",
	} {
		t.Run(name, func(t *testing.T) {
			stub := &controlStub{rows: twoSessions(), newPane: "%42"}
			code, out, errs := openRun(stub, "--split", "--label", label)
			if code != 0 {
				t.Errorf("exit %d, want 0: stderr %q", code, errs)
			}
			if out != "%42\n" {
				t.Errorf("stdout = %q, want the pane id", out)
			}
		})
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

// dispatchedVerbCount is how many case VALUES cli.go's dispatch switch has --
// the thirteen subcommands plus the four spellings of `help`.
//
// IT IS THE ONLY NUMBER IN THIS TEST, and it is a review gate rather than a
// second list. The derivation below decides agent-facing by what a verb IS,
// which catches one wherever its code lives -- but a verb that reached tmux by
// some seam other than a controlClient would be invisible to it, and then
// nothing would ever ask whether the skill should teach it. A total trips on a
// verb added ANYWHERE, so that question gets asked once, of the person adding
// it.
//
// A count of AGENT-FACING verbs would not do: a verb the derivation misses is
// exactly a verb that does not change it, so the pin has to be on the switch's
// own size.
const dispatchedVerbCount = 17

// TestTheSkillNamesEveryVerbTheCLIDispatches is the drift guard: the
// documentation and the dispatcher cannot disagree about what exists.
//
// THERE IS NO SECOND LIST OF VERBS HERE, and that is the point -- a literal
// {"panes", "state", ...} in this file would be exactly the thing that drifts.
// Both halves are derived from the source:
//
//   - WHAT EXISTS is the case clauses of the switch in cli.go's run(), parsed
//     out of the file, and pinned by count (dispatchedVerbCount).
//   - WHICH OF THEM ARE THE AGENT'S is decided by WHAT DRIVES THE VERB, which
//     is a property of the verb rather than of the file it sits in. Every verb
//     an agent calls from inside its own pane is driven through a
//     controlClient -- control.go's tmux seam -- handed to it as a `dial`
//     parameter. `report`'s seam is a tmuxRunner, and serve, enroll, devices,
//     revoke and install-integration have no tmux seam at all, so the
//     separation is exact and a file move cannot disturb it.
//
// AN EARLIER VERSION OF THIS TEST ASKED WHERE THE HANDLER WAS DECLARED, and
// that had a silent-pass path: a new agent-facing verb whose handler landed in
// a NEW file was neither required in the skill nor flagged, and an author who
// documented it anyway was told to delete it. The seam is the fix -- such a
// verb is agent-facing wherever it lives -- and the count above is the
// backstop for the one case the seam cannot see.
func TestTheSkillNamesEveryVerbTheCLIDispatches(t *testing.T) {
	dispatched := dispatchedVerbs(t)
	funcs := packageFuncs(t)

	agentFacing := map[string]bool{}
	for verb, handler := range dispatched {
		if drivenByAControlClient(handler, funcs) {
			agentFacing[verb] = true
		}
	}
	// The derivation itself, checked before anything is concluded from it: a
	// parse that silently matched nothing would make every assertion below
	// vacuously true, and a parse that matched everything would demand the
	// skill teach `serve`.
	if len(dispatched) == 0 {
		t.Fatal("no verbs at all were parsed out of cli.go's dispatch switch -- the parse, not the switch, is what broke")
	}
	if len(agentFacing) == 0 || len(agentFacing) == len(dispatched) {
		t.Fatalf("%d of %d dispatched verbs came out agent-facing (%v): the controlClient seam has stopped telling them apart", len(agentFacing), len(dispatched), agentFacing)
	}
	// Errorf and not Fatalf: when a verb has been added, the forward
	// requirement below is the half that says what to do about it, and
	// stopping here would hide it.
	if len(dispatched) != dispatchedVerbCount {
		t.Errorf("cli.go's dispatch switch has %d case values and this test is pinned to %d (%v).\n"+
			"A verb was added, removed or renamed. Decide whether it is one an agent calls from inside its own pane:\n"+
			"  if it is, teach it in %s and the check below will stop complaining;\n"+
			"  if it is not, leave the skill alone.\n"+
			"Either way, update dispatchedVerbCount -- that decision is what this number exists to force.",
			len(dispatched), dispatchedVerbCount, slices.Sorted(maps.Keys(dispatched)), skillSource)
	}

	skill := readSkill(t)
	for verb := range agentFacing {
		if !strings.Contains(skill, "tmux-web "+verb) {
			t.Errorf("the skill never names `tmux-web %s`, which cli.go dispatches to a verb driven through a controlClient -- an agent that read the skill does not know the verb exists", verb)
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
		t.Errorf("the skill names `tmux-web %s`, which is dispatched to %s and is not driven through a controlClient -- this skill is about the verbs an agent calls from inside its own pane", verb, dispatched[verb])
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
	if m[1] != strconv.Itoa(skillSchema) {
		t.Errorf("the skill's managed header says schema %s, want the skill's %d", m[1], skillSchema)
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

// packageFuncs returns every top-level function this package's non-test files
// declare, by name. The whole package and not one file: which file a handler
// sits in is precisely what this test stopped depending on.
func packageFuncs(t *testing.T) map[string]*ast.FuncDecl {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the package directory: %v", err)
	}
	fset := token.NewFileSet()
	funcs := map[string]*ast.FuncDecl{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		for _, decl := range f.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil {
				funcs[fn.Name.Name] = fn
			}
		}
	}
	if len(funcs) == 0 {
		t.Fatal("no top-level functions found in the package's non-test files")
	}
	return funcs
}

// drivenByAControlClient reports whether the named handler is a verb an agent
// calls from inside its own pane, by the one thing all of those have in common:
// a controlClient, control.go's tmux seam, reaching them as a dial parameter.
//
// TWO PLACES ARE LOOKED AT, because the handler itself is a two-liner. cmdState
// and its six siblings pass os.Getenv and dialControl to a run* function that
// holds the seam in its own signature, so the dial is on the DELEGATE; a verb
// written without that seam would hold it directly. Either counts, and nothing
// deeper does -- a transitive search would eventually reach tmux from anywhere.
func drivenByAControlClient(handler string, funcs map[string]*ast.FuncDecl) bool {
	fn := funcs[handler]
	if fn == nil {
		// `help` and its flag spellings are handled inline, with no handler.
		return false
	}
	if dialsAControlClient(fn) {
		return true
	}
	if fn.Body == nil {
		return false
	}
	driven := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); ok && dialsAControlClient(funcs[id.Name]) {
			driven = true
		}
		return true
	})
	return driven
}

// dialsAControlClient reports whether fn takes a parameter that is a function
// producing a controlClient. The result type is what is matched and not the
// parameter's name: `dial` is a convention, and `func(socket string)
// controlClient` is the contract.
func dialsAControlClient(fn *ast.FuncDecl) bool {
	if fn == nil || fn.Type.Params == nil {
		return false
	}
	for _, p := range fn.Type.Params.List {
		ft, ok := p.Type.(*ast.FuncType)
		if !ok || ft.Results == nil || len(ft.Results.List) != 1 {
			continue
		}
		if id, ok := ft.Results.List[0].Type.(*ast.Ident); ok && id.Name == "controlClient" {
			return true
		}
	}
	return false
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
	// No \n exclusion in the span pattern, and the prose is scanned WHOLE
	// rather than line by line: a markdown code span may wrap across a line
	// break, and this skill has one -- `p=$(tmux-web open --split)` is broken
	// after `open` to fit the column. A per-line pass cannot see that span at
	// all, which makes a wrapped command the one place a bad verb could hide.
	inlineSpan := regexp.MustCompile("`([^`]*)`")
	// [ \t] and not \s: the spans are collected one per line below, and a
	// `\s` would walk off the end of one span onto the start of the next --
	// "`tmux-web` does not check ... from the `command` column" would be read
	// as a verb called `command`.
	command := regexp.MustCompile(`\btmux-web[ \t]+([a-z][a-z-]*)`)
	// A wrapped span's own line break, flattened so the pattern above can see
	// across it. Within one span that is safe: a span is a single command.
	wrap := regexp.MustCompile(`\s+`)

	var code, prose strings.Builder
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
		prose.WriteString(line)
		prose.WriteByte('\n')
	}
	if fenced {
		t.Error("the skill has an unclosed ``` fence")
	}
	// One span per line, so the command pattern still cannot run from the end
	// of one span onto the start of the next.
	for _, m := range inlineSpan.FindAllStringSubmatch(prose.String(), -1) {
		code.WriteString(wrap.ReplaceAllString(m[1], " "))
		code.WriteByte('\n')
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

// The seven verbs never reach the daemon, and this pins it from both ends.
//
// IMPORTS, PARSED. There is no HTTP client in these verbs to inject, so a
// "fails if called" stub would assert nothing. What can be said is that the
// file has no way to build one: it imports neither net/http, nor internal/auth
// (the daemon's credentials), nor internal/front (its routes and the poll loop
// the browser rides). Parsed with go/parser and not grepped, so a comment or a
// string that mentions them cannot satisfy or break it.
func TestControlImportsNothingThatReachesTheDaemon(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "control.go", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parsing control.go: %v", err)
	}
	forbidden := []string{
		"net/http",
		"github.com/diegok/tmux-web/internal/auth",
		"github.com/diegok/tmux-web/internal/front",
	}
	if len(f.Imports) == 0 {
		t.Fatal("control.go parsed with no imports; this test would pass on nothing")
	}
	for _, imp := range f.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			t.Fatalf("unquoting import %s: %v", imp.Path.Value, err)
		}
		if slices.Contains(forbidden, path) {
			t.Errorf("control.go imports %s: these verbs fork tmux and never talk to the daemon, so they work with none running", path)
		}
	}
}

// ... and the other end: every verb, driven, dials the socket from $TMUX and
// nothing else. The daemon's address, a different socket, an empty one -- any
// of them fails the test.
func TestNoVerbDialsAnythingButTheCallersSocket(t *testing.T) {
	const want = "/tmp/sock" // callerEnv's $TMUX is "/tmp/sock,7,0"
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
			stub := &controlStub{rows: twoSessions()}
			var out, errb bytes.Buffer
			v.run(v.args, &out, &errb, callerEnv("%1"), func(socket string) controlClient {
				dialled++
				if socket != want {
					t.Errorf("dialled %q, want only the socket from $TMUX (%q)", socket, want)
				}
				return stub
			})
			if dialled == 0 {
				t.Error("never dialled; the test is not exercising the verb")
			}
		})
	}
}
