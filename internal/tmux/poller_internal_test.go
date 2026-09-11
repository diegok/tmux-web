package tmux

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The generation's two failure branches, which no test with a real tmux server
// can reach on cue: it is skipped when the snapshot itself failed, and a failure
// of its own leaves the last known generation in place.
//
// This is an internal test because both branches are decided inside refresh from
// functions only the constructors supply.
func TestRefreshKeepsTheLastKnownServerGeneration(t *testing.T) {
	var startCalls int
	var rowsErr, startErr error
	generation := "100"

	p := NewPollerFunc(0, func(context.Context) ([]Row, error) {
		if rowsErr != nil {
			return nil, rowsErr
		}
		return []Row{{PaneID: "%0"}}, nil
	})
	p.startFn = func(context.Context) (string, error) {
		startCalls++
		if startErr != nil {
			return "", startErr
		}
		return generation, nil
	}
	ctx := context.Background()

	p.refresh(ctx)
	if got := p.ServerStart(); got != "100" {
		t.Fatalf("ServerStart() = %q, want 100", got)
	}
	if startCalls != 1 {
		t.Fatalf("startFn called %d times in one poll, want 1", startCalls)
	}

	// A failed snapshot must not fork tmux a second time to ask a question the
	// same fault would fail, and must not disturb what is already known.
	rowsErr = errors.New("server exited unexpectedly")
	p.refresh(ctx)
	if startCalls != 1 {
		t.Errorf("startFn ran %d times; a failed poll must not ask for the generation", startCalls)
	}
	if got := p.ServerStart(); got != "100" {
		t.Errorf("a failed poll changed the generation to %q", got)
	}

	// A generation read that fails on its own keeps the previous value rather
	// than blanking it: every browser keys its per-pane memory on this, and an
	// empty generation for one poll makes all of it miss.
	rowsErr, startErr = nil, errors.New("nope")
	generation = "200" // must not be seen: this call errors
	p.refresh(ctx)
	if got := p.ServerStart(); got != "100" {
		t.Errorf("ServerStart() = %q after a failed read, want the last known 100", got)
	}

	// ... and a later success does replace it, or "keep the previous value"
	// would be indistinguishable from "never update".
	startErr = nil
	p.refresh(ctx)
	if got := p.ServerStart(); got != "200" {
		t.Errorf("ServerStart() = %q, want the new generation 200", got)
	}
}

// A poller built from a bare snapshot function has no tmux server to ask, and
// must report no generation rather than panicking on a nil reader.
func TestRefreshWithoutAServerGenerationReader(t *testing.T) {
	p := NewPollerFunc(0, func(context.Context) ([]Row, error) { return []Row{{PaneID: "%0"}}, nil })
	p.refresh(context.Background())
	if got := p.ServerStart(); got != "" {
		t.Fatalf("ServerStart() = %q, want empty", got)
	}
}

// --- agent classification ---------------------------------------------------
//
// These drive refresh directly rather than Start. Classification spans polls --
// two identical captures mean idle, a third does not re-stamp -- so a test has
// to control exactly how many polls happen and in what order, which a ticker
// cannot offer. Everything they touch is package-private for the same reason
// the generation tests above are.

// agentPoller builds a poller whose snapshot and captures are supplied by the
// test, with agent classification turned on.
func agentPoller(rows *[]Row, screens map[string]string, captures *int, connected *bool) *Poller {
	return NewPollerWith(Options{
		Snapshot: func(context.Context) ([]Row, error) {
			return append([]Row{}, *rows...), nil
		},
		Capture: func(_ context.Context, paneID string) (string, error) {
			*captures++
			s, ok := screens[paneID]
			if !ok {
				return "", errors.New("no such pane: " + paneID)
			}
			return s, nil
		},
		Connected: func() bool { return *connected },
	})
}

func stateOf(t *testing.T, p *Poller, paneID string) Row {
	t.Helper()
	for _, r := range p.Latest() {
		if r.PaneID == paneID {
			return r
		}
	}
	t.Fatalf("pane %s missing from the snapshot", paneID)
	return Row{}
}

// With nobody watching there is nothing to capture for, and the design is
// explicit that the states go EMPTY rather than frozen at their last value:
// state presented as current when it is minutes old is worse than none.
//
// The classifier is reset too, so the next connection settles from scratch. A
// resumed run would settle two polls after reconnect and stamp a finish edge,
// which is a done badge on every device for work that finished while nobody was
// connected -- the same storm everChanged exists to prevent.
func TestRefreshSkipsCapturesWithNoClient(t *testing.T) {
	rows := []Row{{PaneID: "%1", Command: "claude"}}
	screens := map[string]string{"%1": "screen A"}
	captures := 0
	connected := true
	p := agentPoller(&rows, screens, &captures, &connected)
	ctx := context.Background()

	// Connected: a real change, then a settle, so there is a live run to lose.
	p.refresh(ctx)
	screens["%1"] = "screen B"
	p.refresh(ctx)
	p.refresh(ctx)
	p.refresh(ctx)
	if got := stateOf(t, p, "%1"); got.AgentState != StateIdle || got.FinishedAt == 0 {
		t.Fatalf("with a client connected = %+v, want idle with a finish edge", got)
	}
	before := captures
	if before == 0 {
		t.Fatal("no captures were attempted with a client connected")
	}

	connected = false
	p.refresh(ctx)
	if got := captures; got != before {
		t.Errorf("%d captures with no client connected, want none", got-before)
	}
	if got := stateOf(t, p, "%1"); got.AgentState != "" || got.FinishedAt != 0 {
		t.Errorf("with no client = %+v, want an empty state: a frozen last value "+
			"is stale state presented as current", got)
	}

	// Reconnect. The pane has not changed, so a classifier that kept its old
	// entry settles immediately and stamps an edge; one that was reset reports
	// working first and settles with nothing to stamp.
	connected = true
	if got := stateOf(t, p, "%1"); got.AgentState != "" {
		t.Fatal("the previous poll's rows changed under us")
	}
	p.refresh(ctx)
	if got := stateOf(t, p, "%1"); got.AgentState != StateWorking {
		t.Errorf("first poll after reconnect = %+v, want working: the classifier "+
			"must start from nothing, not resume a run nobody was watching", got)
	}
	p.refresh(ctx)
	p.refresh(ctx)
	if got := stateOf(t, p, "%1"); got.AgentState != StateIdle || got.FinishedAt != 0 {
		t.Errorf("settling after reconnect = %+v, want idle with NO finish edge", got)
	}
}

// Only known agents are captured. A shell, an editor or a build is never
// forked for, never classified, and never badged -- the whole cost of this
// feature is bounded by that list.
func TestRefreshCapturesOnlyKnownAgents(t *testing.T) {
	rows := []Row{
		{PaneID: "%1", Command: "claude"},
		{PaneID: "%2", Command: "zsh"},
		{PaneID: "%3", Command: "claude-helper"},
	}
	var asked []string
	connected := true
	p := NewPollerWith(Options{
		Snapshot: func(context.Context) ([]Row, error) { return append([]Row{}, rows...), nil },
		Capture: func(_ context.Context, paneID string) (string, error) {
			asked = append(asked, paneID)
			return "a screen", nil
		},
		Connected: func() bool { return connected },
	})
	p.refresh(context.Background())

	if len(asked) != 1 || asked[0] != "%1" {
		t.Errorf("captured %v, want only the claude pane %%1", asked)
	}
	if got := stateOf(t, p, "%1"); got.AgentState == "" {
		t.Error("the agent pane got no state")
	}
	for _, id := range []string{"%2", "%3"} {
		if got := stateOf(t, p, id); got.AgentState != "" {
			t.Errorf("pane %s = %q, want no state: it is not a known agent", id, got.AgentState)
		}
	}
}

// pi is the case that hazard was written for, and it is worth pinning on its
// own screen rather than trusting the argument.
//
// pi's spinner keeps animating BEHIND the overlay while the question waits, so
// every capture differs from the last and churn never settles: a detector gated
// on idle would leave this pane reading "working" for as long as it waited,
// which is the whole failure. The frames here are pi's own, substituted into
// the real capture at the column the capture has them in -- so this also pins
// that a screen changing behind the box does not disturb reading the box.
func TestRefreshBlockedOverridesChurnForPi(t *testing.T) {
	overlay := readFixture(t, "pi-blocked.txt")
	if !strings.Contains(overlay, "⠋") {
		t.Fatal("the capture no longer carries the spinner this test animates")
	}
	rows := []Row{{PaneID: "%1", Command: "pi"}}
	screens := map[string]string{"%1": overlay}
	captures := 0
	connected := true
	p := agentPoller(&rows, screens, &captures, &connected)
	ctx := context.Background()

	// Long enough that a pane which settled would have settled several times
	// over: settleAfter is small, and the point is that it never gets there.
	for _, frame := range []string{"⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇"} {
		p.refresh(ctx)
		next := strings.Replace(overlay, "⠋", frame, 1)
		if next == overlay {
			t.Fatal("the spinner substitution changed nothing")
		}
		screens["%1"] = next
		if got := stateOf(t, p, "%1"); got.AgentState != StateBlocked {
			t.Fatalf("a screen churning behind the overlay = %q, want blocked", got.AgentState)
		}
	}
	// And no finish edge underneath it: nothing has finished, and an edge would
	// outlive the overlay as a done badge on a pane that never ran to an end.
	if got := stateOf(t, p, "%1").FinishedAt; got != 0 {
		t.Errorf("a waiting overlay stamped a finish edge at %d", got)
	}
	// The question rides the same poll. A screen changing behind the box does
	// not disturb reading the box: this is the last frame's capture, not the
	// first one's.
	if got := stateOf(t, p, "%1").Question; got == nil || got.Text == "" {
		t.Errorf("blocked row carries no question: %+v", got)
	}

	// The overlay goes away: whatever churn says is what the pane reads, which
	// is what makes the assertions above a decision rather than a constant.
	screens["%1"] = readFixture(t, "pi-idle.txt")
	p.refresh(ctx)
	if got := stateOf(t, p, "%1").AgentState; got == StateBlocked {
		t.Error("the overlay is gone and the pane still reads blocked")
	}
}

// Blocked is not gated on idle and is not a verdict churn can outvote. An agent
// can raise an approval box while background work carries on, so the box wins
// on any poll it is on screen.
func TestRefreshBlockedOverridesChurn(t *testing.T) {
	dialog := readFixture(t, "claude-blocked.txt")
	rows := []Row{{PaneID: "%1", Command: "claude"}}
	screens := map[string]string{"%1": dialog + "\nspinner 1"}
	captures := 0
	connected := true
	p := agentPoller(&rows, screens, &captures, &connected)
	ctx := context.Background()

	// Churning: the screen differs every poll, so churn says working.
	for i := 2; i < 5; i++ {
		p.refresh(ctx)
		screens["%1"] = dialog + "\nspinner " + strconv.Itoa(i)
	}
	got := stateOf(t, p, "%1")
	if got.AgentState != StateBlocked {
		t.Errorf("a changing screen showing a dialog = %q, want blocked", got.AgentState)
	}
	if got.Question == nil || got.Question.Text == "" {
		t.Errorf("blocked row carries no question: %+v", got.Question)
	}

	// Still: churn says idle, and the box still wins.
	//
	// And -- the half this test used to leave out -- the settle stamps NO
	// finish edge. A held box is byte-identical between polls, so churn cannot
	// tell it from a finished run; the classifier has to be told. Asserting
	// only AgentState here hides that completely, because the override makes
	// the row read `blocked` whether or not an edge was stamped underneath it.
	p.refresh(ctx)
	p.refresh(ctx)
	p.refresh(ctx)
	if got := stateOf(t, p, "%1"); got.AgentState != StateBlocked {
		t.Errorf("a still screen showing a dialog = %q, want blocked", got.AgentState)
	}
	if got := stateOf(t, p, "%1"); got.FinishedAt != 0 {
		t.Errorf("a held dialog stamped a finish edge at %d: this pane has not "+
			"finished anything, and the edge outlives the box", got.FinishedAt)
	}

	// The box goes away: the state goes back to what churn says, and the
	// question goes with it rather than being left on a row nobody is asking
	// about.
	//
	// This is where a stamp made under the box does its damage. The row is
	// working again, so the state is right, and the stale edge rides along
	// underneath it -- newer than any `seen` on a device that has not viewed
	// the pane, which shows `done` on an agent that is mid-run.
	screens["%1"] = "answered, working again"
	p.refresh(ctx)
	if got := stateOf(t, p, "%1"); got.AgentState != StateWorking || got.Question != nil {
		t.Errorf("after the dialog was answered = %+v, want working with no question", got)
	}
	if got := stateOf(t, p, "%1"); got.FinishedAt != 0 {
		t.Errorf("a resumed agent carries a finish edge of %d, stamped while it was "+
			"waiting on the owner: every unviewed device now shows `done` on a "+
			"pane that is working", got.FinishedAt)
	}
}

// The classifier is keyed by pane id, and pane ids restart at %0 with the tmux
// server -- the same hazard the browser's `seen` map carries the generation for.
//
// While the server is down the snapshot fails and refresh returns before
// classify, so nothing prunes the dead server's entries. On the first poll
// against the new server a fresh pane reusing %1 is compared against them,
// inherits an everChanged that was set by a run on a machine that no longer
// exists, and stamps a finish edge two polls later. The browser cannot suppress
// it: its map is keyed on the NEW generation and is empty.
func TestRefreshResetsTheClassifierWhenTheServerRestarts(t *testing.T) {
	rows := []Row{{PaneID: "%1", Command: "claude"}}
	screens := map[string]string{"%1": "old server, mid-run"}
	captures := 0
	connected := true
	generation := "100"
	p := agentPoller(&rows, screens, &captures, &connected)
	p.startFn = func(context.Context) (string, error) { return generation, nil }
	ctx := context.Background()

	// A real change, so the old server's entry has everChanged set: that is the
	// thing that must not cross the restart.
	p.refresh(ctx)
	screens["%1"] = "old server, still going"
	p.refresh(ctx)

	// The server restarts. %1 is now a different pane on a different server.
	generation = "200"
	screens["%1"] = "new server, a fresh agent"
	p.refresh(ctx)
	p.refresh(ctx)
	p.refresh(ctx)
	if got := stateOf(t, p, "%1"); got.AgentState != StateIdle || got.FinishedAt != 0 {
		t.Errorf("the first pane on a new tmux server = %+v, want idle with NO "+
			"finish edge: it inherited the dead server's %%1", got)
	}

	// The reset is keyed on the generation CHANGING, not on having read one. A
	// reset that fired every poll -- or on every successful read -- would make
	// every poll a first sight, so no pane could ever earn an edge again and
	// the done badge would be dead. Same server, a real run, a real edge.
	screens["%1"] = "new server, working"
	p.refresh(ctx)
	p.refresh(ctx)
	p.refresh(ctx)
	if got := stateOf(t, p, "%1"); got.AgentState != StateIdle || got.FinishedAt == 0 {
		t.Errorf("a genuine run on the same server = %+v, want idle WITH a finish "+
			"edge: the reset must fire on a restart, not on every poll", got)
	}
}

// Retain takes the panes that are known agents NOW, not every pane in the
// snapshot. A pane that goes claude -> zsh -> claude has to come back as a
// first sight: keeping its hash across the shell means the relaunched agent's
// different screen sets everChanged, and the next settle stamps a finish edge
// for an agent that has only just started.
func TestRefreshForgetsAPaneThatStoppedBeingAnAgent(t *testing.T) {
	rows := []Row{{PaneID: "%1", Command: "claude"}}
	screens := map[string]string{"%1": "claude, mid-run"}
	captures := 0
	connected := true
	p := agentPoller(&rows, screens, &captures, &connected)
	ctx := context.Background()

	// A real change, so everChanged is set on the entry that must not survive.
	p.refresh(ctx)
	screens["%1"] = "claude, still going"
	p.refresh(ctx)

	// The agent exits back to a shell for one poll.
	rows[0].Command = "zsh"
	p.refresh(ctx)

	// A new agent starts in the same pane, with a screen unlike the old one's.
	rows[0].Command = "claude"
	screens["%1"] = "a brand new claude"
	p.refresh(ctx)
	p.refresh(ctx)
	p.refresh(ctx)
	if got := stateOf(t, p, "%1"); got.AgentState != StateIdle || got.FinishedAt != 0 {
		t.Errorf("relaunched agent = %+v, want idle with NO finish edge: its "+
			"predecessor's hash must not have survived the shell", got)
	}
}

// A pane can close between list-panes and capture-pane -- the snapshot is
// milliseconds old by then. That is an ordinary race, not a fault: the pane
// gets no state for that poll, and its neighbours are unaffected.
func TestRefreshSurvivesACaptureThatFails(t *testing.T) {
	rows := []Row{
		{PaneID: "%1", Command: "claude"},
		{PaneID: "%2", Command: "claude"},
	}
	screens := map[string]string{"%2": "a screen"} // %1 is gone
	captures := 0
	connected := true
	p := agentPoller(&rows, screens, &captures, &connected)
	p.refresh(context.Background())

	if got := stateOf(t, p, "%1"); got.AgentState != "" {
		t.Errorf("pane whose capture failed = %q, want no state", got.AgentState)
	}
	if got := stateOf(t, p, "%2"); got.AgentState == "" {
		t.Error("a failed capture on one pane cost its neighbour its state")
	}
	if err := p.Err(); err != nil {
		t.Errorf("Err() = %v; a capture that failed is not a failed poll", err)
	}
}

// A poller with no capture function is v1's poller and must stay one: every
// existing caller of NewPollerFunc builds one, and a nil dereference in the
// poll goroutine would take the daemon with it.
func TestRefreshWithoutClassificationLeavesRowsAlone(t *testing.T) {
	p := NewPollerFunc(0, func(context.Context) ([]Row, error) {
		return []Row{{PaneID: "%1", Command: "claude"}}, nil
	})
	p.refresh(context.Background())
	if got := p.Latest(); len(got) != 1 || got[0].AgentState != "" {
		t.Fatalf("Latest() = %+v, want one row with no state", got)
	}
}

// Half-wired classification is a silent no-op: captures with no liveness check
// fork tmux for nobody, and a liveness check with no capture computes nothing.
// Either is a wiring mistake that would leave the whole feature dark with every
// test still green, so it fails at construction.
func TestNewPollerWithRefusesHalfWiredClassification(t *testing.T) {
	for _, tc := range []struct {
		name string
		o    Options
	}{
		{"capture without liveness", Options{
			Snapshot: func(context.Context) ([]Row, error) { return nil, nil },
			Capture:  func(context.Context, string) (string, error) { return "", nil },
		}},
		{"liveness without capture", Options{
			Snapshot:  func(context.Context) ([]Row, error) { return nil, nil },
			Connected: func() bool { return true },
		}},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: built a poller, want a panic", tc.name)
				}
			}()
			NewPollerWith(tc.o)
		}()
	}
}

// --- agent reports ----------------------------------------------------------
//
// The precedence table, at the poller. Every one of these asserts AgentState,
// StateSource and FinishedAt together: a precedence bug is invisible to the
// state alone, because the two authorities agree about the state in exactly the
// case where consulting the wrong one costs the most.

// reportPoller builds a poller whose snapshot, standing @wterm_agent values and
// captures are all supplied by the test, with agent reporting turned on.
//
// captured records the panes capture-pane was actually forked for, which is the
// only way to see the win: a skipped capture and a capture whose verdict was
// overruled produce identical rows.
func reportPoller(rows *[]Row, reports, screens map[string]string, captured *[]string, connected *bool) *Poller {
	return NewPollerWith(Options{
		SnapshotWithReports: func(context.Context) ([]Row, map[string]string, error) {
			// Copied, as the real one is: the poller must not be handed the
			// test's live map.
			out := make(map[string]string, len(reports))
			for id, v := range reports {
				out[id] = v
			}
			return append([]Row{}, *rows...), out, nil
		},
		Capture: func(_ context.Context, paneID string) (string, error) {
			*captured = append(*captured, paneID)
			s, ok := screens[paneID]
			if !ok {
				return "", errors.New("no such pane: " + paneID)
			}
			return s, nil
		},
		Connected: func() bool { return *connected },
	})
}

// A fresh report wins, and the capture is skipped -- which is the win. Two
// authorities running in parallel can only agree, in which case the second one
// was cost, or disagree, in which case we have already decided which wins.
func TestFreshReportSkipsTheCapture(t *testing.T) {
	now := time.Now()
	rows := []Row{{PaneID: "%1", Command: "claude"}}
	screens := map[string]string{"%1": "a screen nobody should be asking for"}
	reports := map[string]string{"%1": FormatReport(StateWorking, now.UnixMilli(), "run go")}
	var captured []string
	connected := true
	p := reportPoller(&rows, reports, screens, &captured, &connected)
	ctx := context.Background()

	p.refresh(ctx)
	got := stateOf(t, p, "%1")
	if got.AgentState != StateWorking || got.StateSource != SourceEvent || got.Activity != "run go" {
		t.Errorf("a fresh working report = %+v, want working from the event source with its activity", got)
	}
	if got.FinishedAt != 0 {
		t.Errorf("a working report stamped a finish edge at %d", got.FinishedAt)
	}
	if len(captured) != 0 {
		t.Errorf("captured %v with a fresh report standing: the skipped capture IS the win, and "+
			"asserting only the state cannot see this -- both authorities say working", captured)
	}

	// A fresh resting report costs no capture either, and derives its finish
	// stamp from its own timestamp rather than from a clock we read.
	fin := now.Add(time.Second).UnixMilli()
	reports["%1"] = FormatReport(StateIdle, fin, "")
	p.refresh(ctx)
	got = stateOf(t, p, "%1")
	if got.AgentState != StateIdle || got.StateSource != SourceEvent || got.FinishedAt != fin {
		t.Errorf("a fresh idle report = %+v, want idle from the event source with finishedAt %d", got, fin)
	}
	if len(captured) != 0 {
		t.Errorf("captured %v for a resting report", captured)
	}
}

// With no client connected there is no screen to check, and the report is the
// only authority there is -- which is the case the app exists for. This
// supersedes v2's rule that AgentState is empty whenever no browser holds a
// terminal socket: that rule was right about the classifier's memory, which is
// a claim that nothing has changed since we last looked, and wrong about a fact
// the agent published that tmux is still holding.
func TestAReportStandsWithNoClientConnected(t *testing.T) {
	now := time.Now()
	rows := []Row{
		{PaneID: "%1", Command: "claude"},
		{PaneID: "%2", Command: "claude"}, // no integration, so no report
	}
	screens := map[string]string{"%1": "a screen", "%2": "a screen"}
	reports := map[string]string{"%1": FormatReport(StateBlocked, now.UnixMilli(), "Approve?")}
	var captured []string
	connected := false
	p := reportPoller(&rows, reports, screens, &captured, &connected)
	p.refresh(context.Background())

	if got := stateOf(t, p, "%1"); got.AgentState != StateBlocked || got.StateSource != SourceEvent || got.Activity != "Approve?" {
		t.Errorf("a fresh report with nobody connected = %+v, want blocked from the event source", got)
	}
	// The half that has not changed: with no report and no client there is
	// nothing to say, and a frozen last value would be stale state presented as
	// current.
	if got := stateOf(t, p, "%2"); got.AgentState != "" || got.StateSource != "" {
		t.Errorf("a pane with no report and no client = %+v, want an empty state", got)
	}
	if len(captured) != 0 {
		t.Errorf("captured %v with no client connected", captured)
	}
}

// A stale report hands the pane back to the classifier, and the authority
// switch stamps nothing (Task 10 tests the stamping half).
func TestAStaleReportFallsBackToTheClassifier(t *testing.T) {
	// Past the window by a minute, and written against the constant.
	stale := time.Now().Add(-workingTTL - time.Minute).UnixMilli()
	rows := []Row{{PaneID: "%1", Command: "claude"}}
	screens := map[string]string{"%1": "a screen the classifier does have to read"}
	reports := map[string]string{"%1": FormatReport(StateWorking, stale, "run go")}
	var captured []string
	connected := true
	p := reportPoller(&rows, reports, screens, &captured, &connected)
	p.refresh(context.Background())

	got := stateOf(t, p, "%1")
	if got.StateSource != SourceScreen {
		t.Errorf("a stale report left the row sourced %q, want %q", got.StateSource, SourceScreen)
	}
	if got.AgentState != StateWorking || got.FinishedAt != 0 {
		t.Errorf("a stale report = %+v, want the classifier's first sight with no finish edge", got)
	}
	// The dead report's activity must not ride along on a row the classifier
	// decided: it is a claim about work that stopped being current a minute ago.
	if got.Activity != "" {
		t.Errorf("a stale report left %q on the row", got.Activity)
	}
	if len(captured) != 1 || captured[0] != "%1" {
		t.Errorf("captured %v, want exactly the one pane the classifier had to read", captured)
	}
}

// The premise Task 7's arithmetic rests on, asserted at the poller: a pane
// whose capture was skipped is not passed to Retain, so its classifier entry is
// dropped and the next capture is a first sight.
func TestACaptureSkippedPaneIsNotRetained(t *testing.T) {
	rows := []Row{{PaneID: "%1", Command: "claude"}}
	screens := map[string]string{"%1": "mid-run, frame 1"}
	reports := map[string]string{}
	var captured []string
	connected := true
	p := reportPoller(&rows, reports, screens, &captured, &connected)
	ctx := context.Background()

	// A run with a real change in it -- so the entry carries everChanged, the
	// flag that licenses a finish stamp -- left one poll short of settling.
	// Built from settleAfter, which no mutant in this task touches.
	p.refresh(ctx)
	screens["%1"] = "mid-run, frame 2"
	p.refresh(ctx)
	for i := 0; i < settleAfter-1; i++ {
		p.refresh(ctx)
	}
	if got := stateOf(t, p, "%1"); got.AgentState != StateWorking {
		t.Fatalf("setup: %+v, want a run one poll short of settling", got)
	}

	// One poll under a report, so the capture is skipped.
	reports["%1"] = FormatReport(StateWorking, time.Now().UnixMilli(), "run go")
	before := len(captured)
	p.refresh(ctx)
	if len(captured) != before {
		t.Fatalf("setup: the capture was taken under a fresh report")
	}

	// The report goes away, and the screen is byte-identical to the last one
	// the classifier saw. A RETAINED entry settles on this very poll and stamps
	// a finish edge -- a done badge on an agent nobody watched stop. A dropped
	// one is a first sight, which Observe answers with working and no
	// comparison at all.
	delete(reports, "%1")
	p.refresh(ctx)
	got := stateOf(t, p, "%1")
	if got.AgentState != StateWorking || got.FinishedAt != 0 {
		t.Errorf("the first capture after a skipped one = %+v, want working with no finish edge: "+
			"a capture-skipped pane must not be passed to Retain", got)
	}
	if got.StateSource != SourceScreen {
		t.Errorf("StateSource = %q, want %q", got.StateSource, SourceScreen)
	}
}

// Half-wired reporting is silent in the same way half-wired classification is:
// with neither snapshot function there is nothing to poll, and with both there
// is no way to say which fork happens.
func TestNewPollerWithRefusesHalfWiredReporting(t *testing.T) {
	snap := func(context.Context) ([]Row, error) { return nil, nil }
	withReports := func(context.Context) ([]Row, map[string]string, error) { return nil, nil, nil }
	for _, tc := range []struct {
		name string
		o    Options
	}{
		{"neither", Options{}},
		{"both", Options{Snapshot: snap, SnapshotWithReports: withReports}},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: built a poller, want a panic", tc.name)
				}
			}()
			NewPollerWith(tc.o)
		}()
	}
}

// The report memory is pruned on the same terms as the classifier's, and the
// task's own argument for NOT building a tombstone rests on it: a tombstone
// "would not survive Retain, which the poller calls one line later with the
// shell pane absent from keep". Nothing else prunes it -- classify continues
// past a non-agent pane before Reports.Observe is ever reached, so Observe's
// own command check never fires from here.
//
// Not in this task's mutant table: dropping p.reports.Retain(agents) survived
// the whole suite without this.
//
// The probe is a standing value OLDER than the one the previous agent left,
// because that is the only way to see the prune from outside: on a retained
// entry the ordering filter refuses it and serves the PREVIOUS agent's state,
// and on a pruned one it is a first sight and is accepted on its own terms.
func TestAPaneThatStoppedBeingAnAgentIsForgottenByTheReportMemory(t *testing.T) {
	now := time.Now()
	rows := []Row{{PaneID: "%1", Command: "claude"}}
	screens := map[string]string{"%1": "a screen"}
	reports := map[string]string{"%1": FormatReport(StateBlocked, now.UnixMilli(), "Approve?")}
	var captured []string
	connected := true
	p := reportPoller(&rows, reports, screens, &captured, &connected)
	ctx := context.Background()

	p.refresh(ctx)
	if got := stateOf(t, p, "%1"); got.AgentState != StateBlocked {
		t.Fatalf("setup: %+v, want the blocked report in force", got)
	}

	// The agent exits back to a shell for one poll. The pane is not an agent,
	// so it is skipped -- and Retain is what has to do the forgetting.
	rows[0].Command = "zsh"
	p.refresh(ctx)

	// A new agent in the same pane, whose standing report predates the one its
	// predecessor left behind.
	rows[0].Command = "claude"
	reports["%1"] = FormatReport(StateWorking, now.Add(-time.Second).UnixMilli(), "run go")
	p.refresh(ctx)
	if got := stateOf(t, p, "%1"); got.AgentState != StateWorking || got.Activity != "run go" {
		t.Errorf("relaunched agent = %+v, want its own report: its predecessor's "+
			"accepted report must not have survived the shell as an ordering floor", got)
	}
}
