package tmux

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"
)

// liveStub scripts the two reads LiveState makes -- the standing
// @tmux_web_agent value and the captures -- and counts both.
//
// The counts are the point and not bookkeeping. Two rows of the rules table are
// claims about FORKS rather than about the verdict: "trust it. No fork." is
// satisfied by nothing a State assertion can see, and an implementation that
// captured anyway would pass every other check in this file while spending the
// fork the row exists to refuse. Nothing here talks to tmux, so counting is the
// only way those rows get tested.
type liveStub struct {
	report   string      // what show-options returns; "" means the option is unset
	screens  []string    // one per capture, in call order
	errs     []error     // errs[i], when non-nil, is capture i's failure instead
	reads    int         // show-options calls
	captures int         // Capture calls
	ran      [][]string  // every Run argv, so a test can check the target
	shot     []string    // every Capture's pane id, for the same reason
	at       []time.Time // when each capture happened, so a test can measure the gap
}

func (s *liveStub) Run(ctx context.Context, args ...string) (string, error) {
	s.reads++
	s.ran = append(s.ran, args)
	if len(args) == 0 || args[0] != "show-options" {
		return "", fmt.Errorf("unexpected tmux command %q", args)
	}
	if s.report == "" {
		// tmux exits 1 on an unset user option, and that is the ordinary case
		// for a pane with no integration installed. A test that returned ""
		// with a nil error would let an implementation that treats a failed
		// option read as unknown pass.
		return "", fmt.Errorf("invalid option: %s", AgentOption)
	}
	return s.report, nil
}

func (s *liveStub) Capture(ctx context.Context, paneID string) (string, error) {
	i := s.captures
	s.captures++
	s.shot = append(s.shot, paneID)
	s.at = append(s.at, time.Now())
	if i < len(s.errs) && s.errs[i] != nil {
		return "", s.errs[i]
	}
	if i >= len(s.screens) {
		// A capture the test did not script is a capture the rules table did
		// not ask for, and saying so here names the extra fork rather than
		// letting it read as an empty screen.
		return "", fmt.Errorf("capture %d: only %d scripted", i+1, len(s.screens))
	}
	return s.screens[i], nil
}

// liveNow is a fixed clock: every report in this file is dated against it, and
// ParseReport refuses a timestamp more than reportFutureSkew ahead of it.
func liveNow() time.Time { return time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC) }

func claudeRow() Row { return Row{PaneID: "%1", Command: "claude"} }

func TestAFreshWorkingReportIsTrustedWithoutACapture(t *testing.T) {
	now := liveNow()
	s := &liveStub{report: FormatReport(StateWorking, now.Add(-10*time.Second).UnixMilli(), "running tests")}
	v, err := LiveState(context.Background(), s, claudeRow(), now, 0)
	if err != nil {
		t.Fatalf("LiveState: %v", err)
	}
	if v.State != StateWorking || v.From != VerdictReport {
		t.Errorf("State/From = %q/%q, want %q/%q", v.State, v.From, StateWorking, VerdictReport)
	}
	if s.captures != 0 {
		t.Errorf("captures = %d, want 0: a working report inside its TTL re-asserts itself and a capture has nothing to add", s.captures)
	}
}

func TestAnExpiredWorkingReportFallsBackToTheScreen(t *testing.T) {
	now := liveNow()
	idle := readFixture(t, "claude-idle.txt")
	s := &liveStub{
		report:  FormatReport(StateWorking, now.Add(-61*time.Second).UnixMilli(), "running tests"),
		screens: []string{idle, idle},
	}
	v, err := LiveState(context.Background(), s, claudeRow(), now, 0)
	if err != nil {
		t.Fatalf("LiveState: %v", err)
	}
	if v.State != StateIdle || v.From != VerdictScreen {
		t.Errorf("State/From = %q/%q, want %q/%q", v.State, v.From, StateIdle, VerdictScreen)
	}
	if s.captures != 2 {
		t.Errorf("captures = %d, want 2: stillness is two identical captures, not one", s.captures)
	}
	// Both reads have to name the pane. tmux resolves an empty -t to whatever
	// pane is current and exits 0, so an unaddressed read would answer
	// plausibly about somebody else's work; -p keeps the option lookup off the
	// window, session and global scopes, which nothing here writes.
	want := []string{"show-options", "-p", "-t", "%1", "-v", AgentOption}
	if len(s.ran) != 1 || !slices.Equal(s.ran[0], want) {
		t.Errorf("Run argv = %q, want one call %q", s.ran, want)
	}
	if !slices.Equal(s.shot, []string{"%1", "%1"}) {
		t.Errorf("captured panes = %q, want both captures on %%1", s.shot)
	}
}

// The id is checked before anything forks, as CallerSession checks it: a tmux
// target this malformed is a bug in the caller, and answering it from the
// current pane would be a confident answer about the wrong work.
func TestAMalformedPaneIDIsRefusedBeforeAnyFork(t *testing.T) {
	s := &liveStub{}
	v, err := LiveState(context.Background(), s, Row{PaneID: "1", Command: "claude"}, liveNow(), 0)
	if err == nil {
		t.Fatal("a pane id with no %% sigil must not be addressed")
	}
	if v.State != LiveUnknown {
		t.Errorf("State = %q, want %q", v.State, LiveUnknown)
	}
	if s.reads != 0 || s.captures != 0 {
		t.Errorf("reads/captures = %d/%d, want 0/0", s.reads, s.captures)
	}
}

func TestABlockedReportIsCorroboratedBeforeItIsBelieved(t *testing.T) {
	now := liveNow()
	s := &liveStub{
		report:  FormatReport(StateBlocked, now.Add(-3*time.Minute).UnixMilli(), "Allow?"),
		screens: []string{readFixture(t, "claude-blocked.txt")},
	}
	v, err := LiveState(context.Background(), s, claudeRow(), now, 0)
	if err != nil {
		t.Fatalf("LiveState: %v", err)
	}
	if v.State != StateBlocked || v.From != VerdictReport {
		t.Errorf("State/From = %q/%q, want %q/%q", v.State, v.From, StateBlocked, VerdictReport)
	}
	if v.Dropped {
		t.Error("Dropped = true on a report the screen confirmed")
	}
	if v.QuestionFrom != VerdictReport {
		t.Errorf("QuestionFrom = %q, want %q: the report supplied the text", v.QuestionFrom, VerdictReport)
	}
	if v.Question == nil || v.Question.Text != "Allow?" {
		t.Errorf("Question = %+v, want the report's own Activity %q", v.Question, "Allow?")
	}
	if s.captures != 1 {
		t.Errorf("captures = %d, want 1: one capture corroborates, and a settle wait would answer a question nobody asked", s.captures)
	}
}

func TestAnUncorroboratedBlockedReportIsDroppedAndReadsIdle(t *testing.T) {
	now := liveNow()
	s := &liveStub{
		report:  FormatReport(StateBlocked, now.Add(-3*time.Minute).UnixMilli(), "Allow?"),
		screens: []string{readFixture(t, "claude-idle.txt")},
	}
	v, err := LiveState(context.Background(), s, claudeRow(), now, 0)
	if err != nil {
		t.Fatalf("LiveState: %v", err)
	}
	// The one direction the owner's loop cannot absorb: a standing blocked
	// report no screen confirms would send a coordinating agent to read a pane
	// that has nothing to answer, once per pass, for as long as the stale
	// report stands.
	if v.State != StateIdle {
		t.Errorf("State = %q, want %q", v.State, StateIdle)
	}
	if !v.Dropped {
		t.Error("Dropped = false: the caller cannot tell a dropped report from an agreed idle")
	}
	// The screen answered, not the report -- the report is the thing that was
	// overruled, and From names whoever the verdict came from.
	if v.From != VerdictScreen {
		t.Errorf("From = %q, want %q", v.From, VerdictScreen)
	}
	if v.Question != nil {
		t.Errorf("Question = %+v, want nil on a state that is not blocked", v.Question)
	}
}

// A state-only blocked report -- the integration said it is waiting and did not
// say what for. The design has `state` print the question when there is one, and
// the capture that corroborated the report has one on it, so the grammar reads
// it off the screen already in hand. The report still decided the state, so From
// does not move.
func TestACorroboratedBlockedReportWithNoTextTakesItsQuestionFromTheScreen(t *testing.T) {
	now := liveNow()
	s := &liveStub{
		report:  FormatReport(StateBlocked, now.Add(-3*time.Minute).UnixMilli(), ""),
		screens: []string{readFixture(t, "claude-blocked.txt")},
	}
	v, err := LiveState(context.Background(), s, claudeRow(), now, 0)
	if err != nil {
		t.Fatalf("LiveState: %v", err)
	}
	if v.State != StateBlocked || v.From != VerdictReport {
		t.Errorf("State/From = %q/%q, want %q/%q", v.State, v.From, StateBlocked, VerdictReport)
	}
	// The two fields differ here and only here: the report decided the state
	// and the grammar supplied the text. Collapsing them into one would fail
	// this line.
	if v.QuestionFrom != VerdictScreen {
		t.Errorf("QuestionFrom = %q, want %q: the text was read off the screen", v.QuestionFrom, VerdictScreen)
	}
	if want := "Do you want to create fixture.txt?"; v.Question == nil || v.Question.Text != want {
		t.Errorf("Question = %+v, want the screen's own question %q", v.Question, want)
	}
	if s.captures != 1 {
		t.Errorf("captures = %d, want 1: the grammar reads the capture the corroboration already took", s.captures)
	}
}

func TestAKnownAgentWithNoReportReachesBlockedFromItsScreen(t *testing.T) {
	s := &liveStub{screens: []string{readFixture(t, "claude-blocked.txt")}}
	v, err := LiveState(context.Background(), s, claudeRow(), liveNow(), 0)
	if err != nil {
		t.Fatalf("LiveState: %v", err)
	}
	if v.State != StateBlocked || v.From != VerdictScreen || v.QuestionFrom != VerdictScreen {
		t.Errorf("State/From/QuestionFrom = %q/%q/%q, want %q/%q/%q", v.State, v.From, v.QuestionFrom, StateBlocked, VerdictScreen, VerdictScreen)
	}
	if want := "Do you want to create fixture.txt?"; v.Question == nil || v.Question.Text != want {
		t.Errorf("Question = %+v, want %q", v.Question, want)
	}
	// Churn cannot see a dialog: a held permission box is byte-identical poll
	// after poll, which is the shape stillness has. So the dialog is read off
	// the first capture, before the settle wait, or this pane reads idle.
	if s.captures != 1 {
		t.Errorf("captures = %d, want 1", s.captures)
	}
}

// An idle report earns nothing on its own: it is the resting state that
// reports.go makes survive a verification window, and a one-shot call has no
// window. So the screen decides, and a screen that is still moving overrules it.
func TestAnIdleReportIsOverruledByAScreenThatIsStillMoving(t *testing.T) {
	now := liveNow()
	s := &liveStub{
		report:  FormatReport(StateIdle, now.Add(-30*time.Second).UnixMilli(), ""),
		screens: []string{readFixture(t, "claude-working.txt"), readFixture(t, "claude-idle.txt")},
	}
	// Small but real: the gap is the only part of this row that cannot be
	// asserted from the verdict, and a multi-second wait in a unit suite buys
	// nothing a millisecond does not.
	const settle = 5 * time.Millisecond
	v, err := LiveState(context.Background(), s, claudeRow(), now, settle)
	if err != nil {
		t.Fatalf("LiveState: %v", err)
	}
	if v.State != StateWorking || v.From != VerdictScreen {
		t.Errorf("State/From = %q/%q, want %q/%q", v.State, v.From, StateWorking, VerdictScreen)
	}
	// "settle apart" is the MECHANISM of this row, not a detail of it. Two
	// captures taken back to back are identical for almost any pane, however
	// hard it is working, so a verdict built on them reads idle or quiet
	// everywhere -- and nothing about the State of this test, or of any other
	// in this file, would change if the wait were deleted. So the gap is
	// measured.
	if len(s.at) != 2 {
		t.Fatalf("captures = %d, want 2", len(s.at))
	}
	if gap := s.at[1].Sub(s.at[0]); gap < settle {
		t.Errorf("captures %v apart, want at least the caller's settle of %v", gap, settle)
	}
}

func TestAPaneThatIsNotAnAgentGetsTheOtherVocabulary(t *testing.T) {
	row := Row{PaneID: "%4", Command: "zsh"}
	for _, tc := range []struct {
		name    string
		screens []string
		want    string
	}{
		// Hand-written screens, unlike every agent case in this file: no
		// grammar runs on a pane that is not an agent, so there is no detector
		// here for a fixture to keep honest.
		{"still", []string{"$ ls\nfile\n$ ", "$ ls\nfile\n$ "}, LiveQuiet},
		{"moving", []string{"$ make\ncc a.c\n", "$ make\ncc a.c\ncc b.c\n"}, LiveChanging},
	} {
		s := &liveStub{screens: tc.screens}
		v, err := LiveState(context.Background(), s, row, liveNow(), 0)
		if err != nil {
			t.Fatalf("%s: LiveState: %v", tc.name, err)
		}
		if v.State != tc.want {
			t.Errorf("%s: State = %q, want %q", tc.name, v.State, tc.want)
		}
		switch v.State {
		case StateWorking, StateIdle, StateBlocked:
			t.Errorf("%s: State = %q: the agent vocabulary means a badge, and nothing computed a state for this pane", tc.name, v.State)
		}
		if s.reads != 0 {
			t.Errorf("%s: reads = %d, want 0: Reports.Observe drops any report on a pane that is not a known agent, so reading one here buys an answer that cannot be used", tc.name, s.reads)
		}
	}
}

func TestAFailedCaptureIsUnknownAndNotQuiet(t *testing.T) {
	boom := errors.New("no server running")
	// ANY capture, not just the first: the second one is the whole of the
	// comparison, and an implementation that compared a screen against "" would
	// call a failed read stillness -- which is the answer that stops the owner's
	// loop from ever looking at the pane.
	for _, tc := range []struct {
		name    string
		screens []string
		errs    []error
	}{
		{"first", []string{""}, []error{boom}},
		{"second", []string{"$ ls\nfile\n$ ", ""}, []error{nil, boom}},
	} {
		s := &liveStub{screens: tc.screens, errs: tc.errs}
		v, err := LiveState(context.Background(), s, Row{PaneID: "%4", Command: "zsh"}, liveNow(), 0)
		if !errors.Is(err, boom) {
			t.Errorf("%s: err = %v, want the capture's own error", tc.name, err)
		}
		if v.State != LiveUnknown {
			t.Errorf("%s: State = %q, want %q -- a screen nobody could read is not a still screen", tc.name, v.State, LiveUnknown)
		}
	}
}
