package tmux

import (
	"context"
	"errors"
	"strconv"
	"testing"
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
