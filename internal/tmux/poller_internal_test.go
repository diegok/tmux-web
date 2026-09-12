package tmux

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
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

// --- the poll loop -----------------------------------------------------------
//
// These drive the ticker instead of waiting on one, which is why they are
// internal: newTicker is the seam, and like nowFn it is deliberately not in
// Options, because no caller outside this package has a reason to move the
// poller's clock.
//
// The version of this that waited on a real 1ms ticker was flaky, and not for
// want of a wider margin. Once a poll outlasts the interval -- which is what a
// loaded machine does to a 1ms ticker -- a tick is always waiting in the
// channel, so the select at the top of the loop finds BOTH ctx.Done and a tick
// ready and Go picks between them at random. The loop then keeps servicing
// ticks after cancel, geometrically often: measured with a 1.2ms poll, 47% of
// runs took at least two more polls and one took ten. No allowance short of
// "any number" survives that, and every allowance that does tests nothing.

// pollLoopFixture is a poller whose ticks the test sends by hand.
//
// The tick channel is the handshake: a send on an unbuffered channel completes
// only once the loop has received it, so "the loop is running" is an answer
// rather than an interval to wait out. polled is the other half, one value per
// poll. It is buffered so that a poller that polls when it should not fails the
// assertion below instead of deadlocking on a channel nobody is draining.
type pollLoopFixture struct {
	tick    chan time.Time
	polled  chan struct{}
	stopped chan struct{} // closed by the loop's deferred stop, i.e. on its exit
	p       *Poller
}

func newPollLoopFixture(t *testing.T, tick chan time.Time) *pollLoopFixture {
	t.Helper()
	f := &pollLoopFixture{tick: tick, polled: make(chan struct{}, 8), stopped: make(chan struct{})}
	f.p = NewPollerFunc(time.Hour, func(context.Context) ([]Row, error) {
		f.polled <- struct{}{}
		return nil, nil
	})
	f.p.newTicker = func(time.Duration) (<-chan time.Time, func()) {
		return f.tick, func() { close(f.stopped) }
	}
	return f
}

// The timeouts below are deadlock guards, not waits: on the passing path every
// one of them returns as soon as the other goroutine gets scheduled. A test
// that is right never spends them, so they can be generous without being slow.
const pollLoopGuard = 10 * time.Second

func (f *pollLoopFixture) sendTick(t *testing.T, what string) {
	t.Helper()
	select {
	case f.tick <- time.Now():
	case <-time.After(pollLoopGuard):
		t.Fatalf("the poll loop never took a tick: %s", what)
	}
}

func (f *pollLoopFixture) awaitPoll(t *testing.T, what string) {
	t.Helper()
	select {
	case <-f.polled:
	case <-time.After(pollLoopGuard):
		t.Fatal(what)
	}
}

// Cancelling the context is the only way to stop the ticker. If it does not,
// every daemon that ever built a Poller keeps forking tmux forever.
func TestPollerStopsOnContextCancel(t *testing.T) {
	f := newPollLoopFixture(t, make(chan time.Time))
	ctx, cancel := context.WithCancel(context.Background())
	f.p.Start(ctx)
	f.awaitPoll(t, "Start's initial poll never ran")

	// Ticks must reach the poll first, or "no polls after cancel" would be
	// satisfied by a poller that never polls at all.
	for i := 0; i < 3; i++ {
		f.sendTick(t, "before cancel")
		f.awaitPoll(t, "a tick before cancel produced no poll")
	}

	cancel()

	// The loop must RETURN, which is what stops the ticker -- checked by the
	// exit itself rather than by watching a counter sit still, so there is no
	// duration whose passing is the evidence. A loop that does not select on
	// ctx.Done parks on the tick channel forever and nothing closes this.
	select {
	case <-f.stopped:
	case <-time.After(pollLoopGuard):
		t.Fatal("the poll loop never returned after cancel -- the ticker outlives its context")
	}

	// Returned, so there is nobody left to receive: this send has to fail. It
	// is a non-blocking send precisely because the loop is already known to be
	// gone -- waiting on it would be back to timing the absence of an event.
	select {
	case f.tick <- time.Now():
		t.Fatal("something is still taking ticks after cancel")
	default:
	}
	if n := len(f.polled); n != 0 {
		t.Fatalf("%d polls ran after cancel", n)
	}
}

// The tick already waiting in the channel when cancel lands. select picks
// between two ready cases at random, so the loop may well serve that tick --
// but it must serve at most that one and then go, and it must go without
// another tick to wake it. This is the case the old timing-based version was
// trying to express with a "+1" allowance it could not enforce.
func TestPollerStopsWithATickAlreadyPending(t *testing.T) {
	f := newPollLoopFixture(t, make(chan time.Time, 1))
	ctx, cancel := context.WithCancel(context.Background())
	f.p.Start(ctx)
	f.awaitPoll(t, "Start's initial poll never ran")

	f.tick <- time.Now() // buffered: in flight, not yet received
	cancel()

	select {
	case <-f.stopped:
	case <-time.After(pollLoopGuard):
		t.Fatal("a tick in flight kept the poll loop alive past cancel")
	}
	if n := len(f.polled); n > 1 {
		t.Fatalf("%d polls after cancel with one tick in flight, want at most 1", n)
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

// reportPoller builds a poller whose snapshot, standing @tmux_web_agent values and
// captures are all supplied by the test, with agent reporting turned on.
//
// captured records the panes capture-pane was actually forked for, which is the
// only way to see the win: a skipped capture and a capture whose verdict was
// overruled produce identical rows.
func reportPoller(rows *[]Row, reports, screens map[string]string, captured *[]string, connected *bool) *Poller {
	return NewPollerWith(Options{
		Poll: func(context.Context) (Poll, error) {
			// Copied, as the real one is: the poller must not be handed the
			// test's live map.
			out := make(map[string]string, len(reports))
			for id, v := range reports {
				out[id] = v
			}
			return Poll{Rows: append([]Row{}, *rows...), Reports: out}, nil
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
	//
	// With nobody connected, which is the half of this claim Task 7 left
	// standing: evidence rule 3 now checks a resting IDLE against the screen
	// while a client is watching, so with one connected this same report opens
	// the verification window and is captured for NIdle polls. See
	// TestIdleWindowSurvivesTheTurnEndRepaint and the rule-3 tests below.
	connected = false
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
	batched := func(context.Context) (Poll, error) { return Poll{}, nil }
	for _, tc := range []struct {
		name string
		o    Options
	}{
		{"neither", Options{}},
		{"both", Options{Snapshot: snap, Poll: batched}},
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

// --- evidence rule 3: the idle verification window ---------------------------
//
// The measured fixture, and the constants are the point of it. A test spelling
// 3 and 4 out would pass straight through the next change to settleAfter --
// which is exactly how revision 3 of the design shipped an off-by-one.
//
// EVERYTHING here is built from settleAfter and asserted against settleAfter,
// and NIdle appears nowhere in these tests except inside a failure message.
// That is not style. NIdle is the constant the headline mutant retargets, so a
// fixture driven by NIdle and an assertion made against NIdle move TOGETHER
// when it is retargeted and the mutant survives them both: with
// NIdle = settleAfter + 1 the window rejects at poll 3, the fixture stops
// there, `captures == NIdle == 3`, green. Build from the constant the mutant
// does not touch; assert against the one it does. The model is
// TestSanitizeActivityBounds' `MaxActivity != MaxLabel` line
// (internal/tmux/report_test.go:56).

// maxWindowPolls bounds the loops below so a broken window fails the test
// rather than hanging it. It is deliberately wider than any window this task
// can produce: the window has to close on its own, so that the capture counts
// measure the window and not the loop.
const maxWindowPolls = settleAfter + 6

func TestIdleWindowSurvivesTheTurnEndRepaint(t *testing.T) {
	// Poll 1: a first sight of the PRE-FINAL screen. The turn-end event fired
	// 7-52 ms before the agent's last repaint, so the report is written to a
	// screen that is not yet final.
	// Poll 2: the repaint -- one changed capture.
	// Polls 3..settleAfter+2: identical.
	// The classifier says idle at poll settleAfter+2, the report stands, and
	// finishedAt is derived from the REPORT's timestamp, not from now.
	reportTS := time.Now().Add(-3 * time.Second).UnixMilli()
	rows := []Row{{PaneID: "%1", Command: "claude"}}
	screens := map[string]string{"%1": "the pre-final screen"}
	reports := map[string]string{"%1": FormatReport(StateIdle, reportTS, "")}
	var captured []string
	connected := true
	p := reportPoller(&rows, reports, screens, &captured, &connected)
	ctx := context.Background()

	// The loop condition is also the assertion that nothing is derived while
	// the window is open: an implementation that stamps finishedAt while the
	// window is pending leaves the loop at poll 1 and fails the count below.
	var row Row
	for polls := 0; row.FinishedAt == 0; {
		polls++
		if polls > maxWindowPolls {
			t.Fatalf("the window never closed in %d polls", maxWindowPolls)
		}
		if polls >= 2 {
			screens["%1"] = "the final screen"
		}
		p.refresh(ctx)
		row = stateOf(t, p, "%1")
	}

	// settleAfter + 2, spelled out, NOT NIdle. This is the assertion that pins
	// the constant, and it can only pin it by being written in something else.
	if len(captured) != settleAfter+2 {
		t.Fatalf("took %d captures, want settleAfter+2 = %d (NIdle is %d)", len(captured), settleAfter+2, NIdle)
	}
	// And this is the assertion that actually kills NIdle = settleAfter + 1:
	// the shortened window rejects the report at poll settleAfter+1, one poll
	// before the classifier settles, so the pane falls through to the
	// classifier and the row's finishedAt is no longer the report's own.
	if row.FinishedAt != reportTS {
		t.Fatalf("finishedAt = %d, want the report's own timestamp %d", row.FinishedAt, reportTS)
	}
	if row.AgentState != StateIdle || row.StateSource != SourceEvent {
		t.Errorf("the verified report = %+v, want idle from the event source", row)
	}
}

// The same fixture one poll short of the window: the classifier has NOT agreed
// yet, so a window that short drops a true turn end. Written against
// settleAfter -- a loop over settleAfter+1 polls -- and NOT over NIdle-1: a
// bound written as NIdle-1 tracks the very constant the headline mutant
// retargets, so it holds for settleAfter+1, settleAfter+2 and settleAfter+3
// alike and proves nothing on its own. This test is a sibling of the one
// above, not a substitute for it.
func TestAWindowOnePollShortWouldDropATrueTurnEnd(t *testing.T) {
	reportTS := time.Now().Add(-3 * time.Second).UnixMilli()
	rows := []Row{{PaneID: "%1", Command: "claude"}}
	screens := map[string]string{"%1": "the pre-final screen"}
	reports := map[string]string{"%1": FormatReport(StateIdle, reportTS, "")}
	var captured []string
	connected := true
	p := reportPoller(&rows, reports, screens, &captured, &connected)
	ctx := context.Background()

	for poll := 1; poll <= settleAfter+1; poll++ {
		if poll >= 2 {
			screens["%1"] = "the final screen"
		}
		p.refresh(ctx)
	}

	row := stateOf(t, p, "%1")
	// The turn really has ended -- this is the measured fixture -- and after
	// settleAfter+1 polls the classifier still says working. A window closing
	// here would discard it.
	if row.AgentState != StateIdle || row.StateSource != SourceEvent {
		t.Fatalf("after settleAfter+1 = %d polls the row is %+v, want the true turn end still "+
			"standing: the classifier cannot have settled yet", settleAfter+1, row)
	}
	if row.FinishedAt != 0 {
		t.Errorf("finishedAt = %d one poll before the classifier can settle", row.FinishedAt)
	}
	if len(captured) != settleAfter+1 {
		t.Errorf("took %d captures in settleAfter+1 = %d polls", len(captured), settleAfter+1)
	}
}

// A screen that changes at every poll for the whole window: dropped, no
// finishedAt derived, and the pane goes back to the classifier. That is the
// unfiltered-subagent case caught by evidence rather than by a discriminator
// holding -- which matters most on pi and opencode, whose turn-end filters are
// absence-coded and fail open.
//
// The poll at which the drop happens is pinned exactly, which is what kills
// `windowPolls >= NIdle` written as `>`.
func TestAnIdleReportOverAChurningScreenIsDropped(t *testing.T) {
	reportTS := time.Now().Add(-3 * time.Second).UnixMilli()
	rows := []Row{{PaneID: "%1", Command: "claude"}}
	screens := map[string]string{"%1": "frame 0"}
	reports := map[string]string{"%1": FormatReport(StateIdle, reportTS, "")}
	var captured []string
	connected := true
	p := reportPoller(&rows, reports, screens, &captured, &connected)
	ctx := context.Background()

	for poll := 1; poll <= settleAfter+2; poll++ {
		screens["%1"] = "frame " + strconv.Itoa(poll)
		p.refresh(ctx)
		row := stateOf(t, p, "%1")
		if poll < settleAfter+2 {
			if row.AgentState != StateIdle || row.StateSource != SourceEvent {
				t.Fatalf("poll %d of settleAfter+2 = %d: %+v, want the report still standing "+
					"while the window is open", poll, settleAfter+2, row)
			}
			if row.FinishedAt != 0 {
				t.Fatalf("poll %d: finishedAt = %d while the window was still open", poll, row.FinishedAt)
			}
			continue
		}
		// settleAfter+2 polls of `working`, so the report is dropped on this
		// poll and not on any other one.
		if row.AgentState != StateWorking || row.StateSource != SourceScreen {
			t.Fatalf("poll %d: %+v, want the pane back on the classifier (NIdle is %d)", poll, row, NIdle)
		}
		if row.FinishedAt != 0 {
			t.Errorf("a dropped report derived finishedAt = %d", row.FinishedAt)
		}
	}
	if len(captured) != settleAfter+2 {
		t.Errorf("took %d captures, want settleAfter+2 = %d", len(captured), settleAfter+2)
	}

	// The drop is remembered rather than re-litigated: the standing option
	// still holds the same value, and Observe reads it again on the very next
	// poll. Task 9 owns the full semantics of that memory.
	//
	// The probe is a DIALOG, because that is what separates the two: a pane
	// that is genuinely back on the classifier takes the whole classifier path,
	// grammars included, and one that is merely being re-dropped every poll
	// never reaches IsBlocked at all -- the window asks the classifier one
	// question only. Asserting StateSource here cannot see the difference:
	// windowPolls stays past the count, so the re-read report is dropped again
	// on arrival and the row reads from the screen either way.
	screens["%1"] = readFixture(t, "claude-blocked.txt") + "\nframe after the drop"
	p.refresh(ctx)
	row := stateOf(t, p, "%1")
	if row.AgentState != StateBlocked || row.StateSource != SourceScreen {
		t.Errorf("the poll after the drop = %+v, want the pane fully back on the classifier, "+
			"blocked grammars and all", row)
	}
	if row.Question == nil || row.Question.Text == "" {
		t.Errorf("the poll after the drop carries no question: %+v", row.Question)
	}
}

// The window closes at the VERDICT, not at the count. Asserted on the captures
// the poller makes and not only on the state it ends with: a report accepted at
// poll settleAfter+1 and one accepted at poll NIdle look identical from
// outside, and the difference is a capture-pane fork per poll per agent.
func TestTheWindowClosesAtTheVerdict(t *testing.T) {
	reportTS := time.Now().Add(-3 * time.Second).UnixMilli()
	rows := []Row{{PaneID: "%1", Command: "claude"}}
	// A screen already still at the first window poll settles at settleAfter+1.
	screens := map[string]string{"%1": "a screen that is already still"}
	reports := map[string]string{"%1": FormatReport(StateIdle, reportTS, "")}
	var captured []string
	connected := true
	p := reportPoller(&rows, reports, screens, &captured, &connected)
	ctx := context.Background()

	var row Row
	for polls := 0; row.FinishedAt == 0; {
		polls++
		if polls > maxWindowPolls {
			t.Fatalf("the window never closed in %d polls", maxWindowPolls)
		}
		p.refresh(ctx)
		row = stateOf(t, p, "%1")
	}
	if len(captured) != settleAfter+1 {
		t.Fatalf("took %d captures, want %d: the window must close at the verdict", len(captured), settleAfter+1)
	}
	if row.FinishedAt != reportTS {
		t.Errorf("finishedAt = %d, want the report's own timestamp %d", row.FinishedAt, reportTS)
	}

	// And it stays closed. A verified report costs no further forks, which is
	// the whole point of closing at the verdict.
	before := len(captured)
	p.refresh(ctx)
	p.refresh(ctx)
	if len(captured) != before {
		t.Errorf("took %d more captures after the verdict", len(captured)-before)
	}
}

// With no client connected there is nothing to verify, so the derivation is
// immediate -- which is the case the app exists for.
func TestWithNoClientTheDerivationIsImmediate(t *testing.T) {
	reportTS := time.Now().Add(-3 * time.Second).UnixMilli()
	rows := []Row{{PaneID: "%1", Command: "claude"}}
	screens := map[string]string{"%1": "a screen nobody is connected to see"}
	reports := map[string]string{"%1": FormatReport(StateIdle, reportTS, "")}
	var captured []string
	connected := false
	p := reportPoller(&rows, reports, screens, &captured, &connected)
	p.refresh(context.Background())

	row := stateOf(t, p, "%1")
	if row.AgentState != StateIdle || row.StateSource != SourceEvent {
		t.Fatalf("a resting report with nobody connected = %+v, want idle from the event source", row)
	}
	if row.FinishedAt != reportTS {
		t.Errorf("finishedAt = %d on the first poll, want the report's own %d: with no screen to "+
			"check there is nothing to wait for", row.FinishedAt, reportTS)
	}
	if len(captured) != 0 {
		t.Errorf("captured %v with no client connected", captured)
	}
}

// The premise the arithmetic rests on: window poll 1 is a FIRST SIGHT, because
// a capture-skipped pane was never passed to Retain. So the settle count the
// window measures is the window's own -- `still` starts at 0 here -- and not a
// leftover from a baseline taken minutes ago, which is what NIdle = settleAfter
// + 2 is counting.
//
// Note what is NOT asserted here: everChanged staying false through the window.
// It is false in this very fixture -- the repaint at window poll 2 differs from
// poll 1's capture, and a differing capture sets everChanged, so at the
// settling poll the classifier does stamp its own finishedAt = now
// (state.go:123). That stamp is not what the row carries: while the report is
// in force the row's FinishedAt is the report's derivation.
func TestTheFirstWindowPollIsAFirstSight(t *testing.T) {
	now := time.Now()
	rows := []Row{{PaneID: "%1", Command: "claude"}}
	screens := map[string]string{"%1": "mid-run, frame 1"}
	reports := map[string]string{}
	var captured []string
	connected := true
	p := reportPoller(&rows, reports, screens, &captured, &connected)
	ctx := context.Background()

	// A classifier run with a real change in it, left one poll short of
	// settling. If this baseline survived into the window, window poll 1 would
	// compare equal, settle on the spot and verify the report against a settle
	// count taken before the window ever opened.
	p.refresh(ctx)
	screens["%1"] = "mid-run, frame 2"
	p.refresh(ctx)
	for i := 0; i < settleAfter-1; i++ {
		p.refresh(ctx)
	}
	if got := stateOf(t, p, "%1"); got.AgentState != StateWorking {
		t.Fatalf("setup: %+v, want a run one poll short of settling", got)
	}

	// The turn's working report: the capture is skipped, so this pane is not
	// passed to Retain and the classifier entry goes.
	reports["%1"] = FormatReport(StateWorking, now.Add(-time.Second).UnixMilli(), "run go")
	p.refresh(ctx)
	base := len(captured)
	if base != settleAfter+1 {
		t.Fatalf("setup: %d captures before the window, want settleAfter+1 = %d", base, settleAfter+1)
	}

	// The turn end, on a screen byte-identical to the last one the classifier
	// saw. A retained baseline settles on window poll 1; a first sight takes
	// settleAfter+1 polls to settle.
	reportTS := now.UnixMilli()
	reports["%1"] = FormatReport(StateIdle, reportTS, "")
	var row Row
	for polls := 0; row.FinishedAt == 0; {
		polls++
		if polls > maxWindowPolls {
			t.Fatalf("the window never closed in %d polls", maxWindowPolls)
		}
		p.refresh(ctx)
		row = stateOf(t, p, "%1")
	}
	if got := len(captured) - base; got != settleAfter+1 {
		t.Fatalf("the window took %d captures, want settleAfter+1 = %d: window poll 1 must be a "+
			"FIRST SIGHT, so a capture-skipped pane must not be passed to Retain", got, settleAfter+1)
	}
	if row.FinishedAt != reportTS {
		t.Errorf("finishedAt = %d, want the report's own timestamp %d", row.FinishedAt, reportTS)
	}
}

// --- evidence rules 1 and 2 --------------------------------------------------

// blockedReportPoller is one claude pane reporting blocked, with a client
// connected and the screen under the test's control.
func blockedReportPoller(screen string, screens *map[string]string, captured *[]string) (*Poller, context.Context) {
	rows := []Row{{PaneID: "%1", Command: "claude"}}
	*screens = map[string]string{"%1": screen}
	reports := map[string]string{"%1": FormatReport(StateBlocked, time.Now().Add(-3*time.Second).UnixMilli(), "")}
	connected := true
	return NewPollerWith(Options{
		Poll: func(context.Context) (Poll, error) {
			out := make(map[string]string, len(reports))
			for id, v := range reports {
				out[id] = v
			}
			return Poll{Rows: append([]Row{}, rows...), Reports: out}, nil
		},
		Capture: func(_ context.Context, paneID string) (string, error) {
			*captured = append(*captured, paneID)
			s, ok := (*screens)[paneID]
			if !ok {
				return "", errors.New("no such pane: " + paneID)
			}
			return s, nil
		},
		Connected: func() bool { return connected },
	}), context.Background()
}

// Rule 2 needs a TRUE PREMISE, not a counter.
//
// The fixtures are the real captured claude permission screen with the dialog
// present, and the same screen after the dialog is answered. Revision 2 of the
// design specified this test as "N settled captures drop, N-1 do not", which
// tests the counter and cannot see the blocker: its premise -- that a reported
// blocked has a matchable dialog -- was false for four fifths of that
// revision's notification whitelist.
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
		t.Fatalf("NBlocked = %d, want settleAfter+1 = %d: rule 1 drops anything that moves before "+
			"rule 2 sees it, so rule 2 never has to absorb a repaint and needs no R -- it is NOT "+
			"NIdle (%d), and 4 must not be carried across", NBlocked, settleAfter+1, NIdle)
	}

	blocked := readFixture(t, "claude-blocked.txt")
	answered := readFixture(t, "claude-idle.txt")
	if IsBlocked("claude", answered) {
		t.Fatal("the answered fixture still matches the grammar, so rule 2 is never reached here")
	}

	// A settled screen with the dialog still on it never drops, at any count.
	// Bound written off settleAfter, like everything else here.
	var screens map[string]string
	var captured []string
	p, ctx := blockedReportPoller(blocked, &screens, &captured)
	for i := 1; i <= (settleAfter+1)*3; i++ {
		p.refresh(ctx)
		row := stateOf(t, p, "%1")
		if row.AgentState != StateBlocked || row.StateSource != SourceEvent {
			t.Fatalf("poll %d of (settleAfter+1)*3 = %d: %+v, want the report still standing on a "+
				"screen that still shows the dialog", i, (settleAfter+1)*3, row)
		}
	}
	if len(captured) != (settleAfter+1)*3 {
		t.Errorf("took %d captures in %d polls: a standing blocked report is checked every poll, "+
			"because rule 1's evidence can arrive at any one of them", len(captured), (settleAfter+1)*3)
	}

	// The same screen answered drops at exactly settleAfter+1 settled polls,
	// and not at settleAfter. Loop and boundary both built from settleAfter, so
	// that they stay where they are when NBlocked is retargeted.
	captured = nil
	p, ctx = blockedReportPoller(answered, &screens, &captured)
	for i := 1; i <= settleAfter; i++ {
		p.refresh(ctx)
		row := stateOf(t, p, "%1")
		if row.AgentState != StateBlocked || row.StateSource != SourceEvent {
			t.Fatalf("poll %d of settleAfter = %d: %+v, want the report still standing -- a window "+
				"this short drops a true blocked on a slow-repainting screen", i, settleAfter, row)
		}
	}
	p.refresh(ctx)
	row := stateOf(t, p, "%1")
	if row.StateSource != SourceScreen {
		t.Fatalf("after settleAfter+1 = %d settled polls with no registered form on screen: %+v, "+
			"want the report dropped and the pane back on the classifier", settleAfter+1, row)
	}
	if row.AgentState == StateBlocked {
		t.Errorf("the badge survived the drop on a screen with no dialog on it: %+v", row)
	}
	if row.Question != nil {
		t.Errorf("a dropped report quoted a question: %+v", row.Question)
	}
}

// Rule 2 asks "no registered form for this agent", not "not the one grammar
// this agent has". Same question today; a different question the day a second
// claude form is registered, and the day it stops being the same question is
// the day a standing permission report would be wrongly dropped on a screen
// showing an elicitation form.
func TestRule2AsksAboutEveryRegisteredForm(t *testing.T) {
	registerTestAgent(t, "twoform",
		form{ID: "twoform/permission", dialog: markerDialog{marker: "FIRST FORM"}},
		form{ID: "twoform/elicitation", dialog: markerDialog{marker: "SECOND FORM"}})

	// The screen shows the SECOND form only, while the standing report is the
	// one the first form's whitelist entry wrote.
	screen := "an elicitation form\nSECOND FORM\nwaiting on you"
	if blockedRules["twoform"][0].dialog.isBlocked(screen) {
		t.Fatal("the first form matches this screen, so the test proves nothing")
	}

	rows := []Row{{PaneID: "%1", Command: "twoform"}}
	screens := map[string]string{"%1": screen}
	reports := map[string]string{"%1": FormatReport(StateBlocked, time.Now().Add(-3*time.Second).UnixMilli(), "")}
	var captured []string
	connected := true
	p := reportPoller(&rows, reports, screens, &captured, &connected)
	ctx := context.Background()

	for i := 1; i <= (settleAfter+1)*3; i++ {
		p.refresh(ctx)
		row := stateOf(t, p, "%1")
		if row.AgentState != StateBlocked || row.StateSource != SourceEvent {
			t.Fatalf("poll %d: %+v, want the report standing -- the agent is waiting, and WHICH "+
				"form it waits at is not rule 2's business", i, row)
		}
	}

	// And the test is not vacuously "never drops": take both forms off the
	// screen and rule 2 does its job.
	screens["%1"] = "back at the prompt, nothing to decide"
	for i := 1; i <= settleAfter+1; i++ {
		p.refresh(ctx)
	}
	if row := stateOf(t, p, "%1"); row.StateSource != SourceScreen {
		t.Errorf("a screen matching NO registered form = %+v, want the report dropped", row)
	}
}

// Rule 1 needs a CHANGED HASH, not a working verdict. state.go reports working
// on a first sight and on any poll before settleAfter, so a rule keyed on the
// verdict would fire on an ordinary settle -- which is every blocked report's
// first poll, since a capture-skipped pane comes back as a first sight.
func TestRule1DropsOnAChangedHashOnly(t *testing.T) {
	blocked := readFixture(t, "claude-blocked.txt")

	// The premise, checked against the classifier itself rather than assumed:
	// on the first settleAfter polls of a static screen the verdict really is
	// working. Without this the test cannot see the mutant it exists for.
	probe := NewClassifier()
	for i := 1; i <= settleAfter; i++ {
		st := probe.Observe("%probe", blocked, time.Now(), true)
		if st.State != StateWorking {
			t.Fatalf("premise: the classifier's verdict at poll %d is %q, want working", i, st.State)
		}
		if st.Changed {
			t.Fatalf("premise: Changed at poll %d of an unchanging screen", i)
		}
	}

	var screens map[string]string
	var captured []string
	p, ctx := blockedReportPoller(blocked, &screens, &captured)
	for i := 1; i <= settleAfter; i++ {
		p.refresh(ctx)
		row := stateOf(t, p, "%1")
		if row.AgentState != StateBlocked || row.StateSource != SourceEvent {
			t.Fatalf("poll %d of settleAfter = %d: %+v, want the report standing. The classifier "+
				"says working on this poll and nothing moved, so a rule 1 keyed on the verdict "+
				"drops a true blocked here", i, settleAfter, row)
		}
	}

	// One capture that differs, and the report goes on that very poll -- not
	// NBlocked polls later, which is rule 2's timing. The dialog is gone from
	// this screen, so rule 2's counter is at zero and cannot be what dropped it.
	screens["%1"] = "the agent is running again"
	p.refresh(ctx)
	row := stateOf(t, p, "%1")
	if row.StateSource != SourceScreen || row.AgentState != StateWorking {
		t.Fatalf("the poll after the screen moved = %+v, want the report dropped on the spot and "+
			"the pane back on the classifier", row)
	}
}

// Composed: a late repaint on a pane reporting blocked is a changed hash, so
// rule 1 drops the report -- and the badge does not go with it, because a
// dropped report hands the pane to the grammars and the dialog is still on the
// screen for IsBlocked to match positively. Both rules require a connected
// client, so there is no case where the drop happens and the grammar is not
// there to catch it.
func TestARule1DropKeepsTheBadgeWhenTheDialogIsStillThere(t *testing.T) {
	blocked := readFixture(t, "claude-blocked.txt")
	var screens map[string]string
	var captured []string
	p, ctx := blockedReportPoller(blocked, &screens, &captured)

	p.refresh(ctx)
	if row := stateOf(t, p, "%1"); row.AgentState != StateBlocked || row.StateSource != SourceEvent {
		t.Fatalf("setup: %+v, want the blocked report in force", row)
	}

	// The repaint: a spinner still turning under a box the user has not
	// answered. TestRefreshBlockedOverridesChurn shows this is a real shape.
	screens["%1"] = blocked + "\nthe spinner turned"
	p.refresh(ctx)
	row := stateOf(t, p, "%1")
	if row.StateSource != SourceScreen {
		t.Fatalf("a changed capture left the report standing: %+v", row)
	}
	if row.AgentState != StateBlocked {
		t.Fatalf("the badge went with the report: %+v, want blocked from the screen -- the dialog "+
			"is still there for the grammar to match", row)
	}
	if row.Question == nil || row.Question.Text == "" {
		t.Errorf("the pane went back to the grammars with no question read: %+v", row.Question)
	}
	if row.FinishedAt != 0 {
		t.Errorf("a pane held at a dialog stamped a finish edge at %d", row.FinishedAt)
	}
}

// A pane held at a dialog must stamp no finish edge, and the report standing
// over it does not excuse the poller from saying so.
//
// Not in the plan's mutant table, and it survived every mutant that is:
// `IsBlocked`'s answer is fed to the classifier on this path as well as to rule
// 2, and a poller that passed `false` there looks correct from every other test
// in this file. The classifier's `blocked` argument exists precisely because a
// held permission box is byte-identical poll after poll, which is the shape
// stillness has -- so without it the box settles into a working->idle edge for a
// run that never finished.
//
// What hides it here is that while a blocked report stands the row's FinishedAt
// is never read from the classifier, so the bad stamp is invisible at the poll
// it happens. It surfaces later, on the poll rule 1 hands the pane back, as a
// done badge on an agent that is mid-run: the same badge-integrity failure v2
// spent most of its complexity avoiding, arriving through the new authority.
func TestAPaneHeldAtADialogUnderAReportStampsNoFinishEdge(t *testing.T) {
	dialog := readFixture(t, "claude-blocked.txt")
	first := time.Now().Add(-10 * time.Second)
	rows := []Row{{PaneID: "%1", Command: "claude"}}
	screens := map[string]string{"%1": dialog}
	reports := map[string]string{"%1": FormatReport(StateBlocked, first.UnixMilli(), "")}
	var captured []string
	connected := true
	p := reportPoller(&rows, reports, screens, &captured, &connected)
	ctx := context.Background()

	// A capture that really moves, so that everChanged is set. Without it no
	// poll can stamp at all and the test proves nothing -- which is also rule
	// 1's drop, with the badge kept by the grammar.
	p.refresh(ctx)
	screens["%1"] = dialog + "\nthe spinner turned"
	p.refresh(ctx)
	if row := stateOf(t, p, "%1"); row.StateSource != SourceScreen || row.AgentState != StateBlocked {
		t.Fatalf("setup: %+v, want rule 1's drop with the badge kept", row)
	}

	// The integration re-asserts over the same, still-held dialog, and the
	// newer report is back in force. settleAfter identical captures follow,
	// which is the classifier's one chance to stamp: `still` passes through
	// settleAfter exactly once per edge.
	reports["%1"] = FormatReport(StateBlocked, first.Add(5*time.Second).UnixMilli(), "")
	for i := 1; i <= settleAfter; i++ {
		p.refresh(ctx)
		if row := stateOf(t, p, "%1"); row.StateSource != SourceEvent || row.AgentState != StateBlocked {
			t.Fatalf("poll %d of settleAfter = %d: %+v, want the newer report in force over the "+
				"dialog", i, settleAfter, row)
		}
	}

	// The probe: one more capture that moves, which hands the pane back to the
	// classifier and with it whatever finishedAt the classifier has been
	// keeping.
	screens["%1"] = dialog + "\nthe spinner turned again"
	p.refresh(ctx)
	row := stateOf(t, p, "%1")
	if row.StateSource != SourceScreen || row.AgentState != StateBlocked {
		t.Fatalf("%+v, want rule 1's drop with the badge kept", row)
	}
	if row.FinishedAt != 0 {
		t.Errorf("finishedAt = %d on a pane that has been held at a dialog throughout: the poller "+
			"must pass IsBlocked's answer to the classifier on the report path too, or a run "+
			"interrupted by a question earns a done badge it never finished for", row.FinishedAt)
	}
}

// --- the rejection slot ------------------------------------------------------

// rejectedPane is one claude pane whose report has been overturned on evidence,
// with a client connected and the screen, the standing report and the tmux
// server's generation all under the test's control.
type rejectedPane struct {
	p          *Poller
	ctx        context.Context
	screens    map[string]string
	reports    map[string]string
	reportTS   int64
	generation *string
}

// rejectedReportPoller drives a pane to a rejection by `rule`.
//
// The three setups differ only in how they reach the rejection; what happens
// afterwards is one semantic and is asserted once, by the caller.
func rejectedReportPoller(t *testing.T, rule string) rejectedPane {
	t.Helper()
	reportTS := time.Now().Add(-3 * time.Second).UnixMilli()
	rows := []Row{{PaneID: "%1", Command: "claude"}}
	screens := map[string]string{}
	reports := map[string]string{}
	var captured []string
	connected := true
	p := reportPoller(&rows, reports, screens, &captured, &connected)
	ctx := context.Background()
	// A generation from the first poll on, so that nothing below is a
	// generation CHANGE: the reset is keyed on the value changing, and a test
	// that establishes it mid-run would reset the very memory it is building.
	generation := "100"
	p.startFn = func(context.Context) (string, error) { return generation, nil }

	switch rule {
	case "rule 1, a capture that moved":
		reports["%1"] = FormatReport(StateBlocked, reportTS, "")
		screens["%1"] = readFixture(t, "claude-blocked.txt")
		p.refresh(ctx)
		if row := stateOf(t, p, "%1"); row.StateSource != SourceEvent {
			t.Fatalf("setup: %+v, want the blocked report in force", row)
		}
		// The dialog is gone and the agent is visibly running again, so rule 1
		// drops it on this poll -- and no grammar matches, so the badge goes
		// with it.
		screens["%1"] = "the agent is running again"
		p.refresh(ctx)
	case "rule 2, settled with no registered form":
		reports["%1"] = FormatReport(StateBlocked, reportTS, "")
		screens["%1"] = readFixture(t, "claude-idle.txt")
		for polls := 1; ; polls++ {
			if polls > maxWindowPolls {
				t.Fatalf("setup: rule 2 never dropped the report in %d polls", maxWindowPolls)
			}
			p.refresh(ctx)
			if stateOf(t, p, "%1").StateSource == SourceScreen {
				break
			}
		}
	case "rule 3, a screen that never settled":
		reports["%1"] = FormatReport(StateIdle, reportTS, "")
		for polls := 1; ; polls++ {
			if polls > maxWindowPolls {
				t.Fatalf("setup: rule 3 never dropped the report in %d polls", maxWindowPolls)
			}
			// A capture that moves at every poll, for the whole window.
			screens["%1"] = "frame " + strconv.Itoa(polls)
			p.refresh(ctx)
			if stateOf(t, p, "%1").StateSource == SourceScreen {
				break
			}
		}
	default:
		t.Fatalf("unknown rule %q", rule)
	}

	row := stateOf(t, p, "%1")
	if row.StateSource != SourceScreen {
		t.Fatalf("setup for %s: %+v, want the report dropped and the pane back on the classifier", rule, row)
	}
	if row.FinishedAt == reportTS {
		t.Fatalf("setup for %s: a dropped report derived finishedAt %d", rule, reportTS)
	}
	return rejectedPane{p: p, ctx: ctx, screens: screens, reports: reports, reportTS: reportTS, generation: &generation}
}

// One semantic for all three rules: a rejection is NOT cleared by the
// classifier later reporting idle.
//
// Written as the negative, which is the only way it can hold. Revision 4 of the
// design made a rule-3 rejection provisional and cleared it "the moment the
// classifier reports idle"; revision 5 withdrew that on two measurements. The
// case it was built for is measurably empty -- at NIdle = settleAfter + 2,
// polls-to-settle was 3 or 4 in every one of 1,440 phase replays and never 5.
// And the trigger is not safe: 4 of 88 measured turns went still while waiting
// on the model, so mid-turn stillness on a root that is genuinely working can
// clear a rejection that was CORRECT -- resurrecting a subagent's false idle,
// deriving finishedAt from it, and landing the false done badge the rule exists
// to prevent.
//
// The standing option still holds the overturned value on every one of these
// polls, which is the whole reason the slot exists: without it the pane is
// re-accepted the moment the screen settles and never returns to the grammars.
func TestARejectionIsNotClearedByAClassifierIdle(t *testing.T) {
	for _, rule := range []string{
		"rule 1, a capture that moved",
		"rule 2, settled with no registered form",
		"rule 3, a screen that never settled",
	} {
		t.Run(rule, func(t *testing.T) {
			f := rejectedReportPoller(t, rule)
			p, ctx, screens, reportTS := f.p, f.ctx, f.screens, f.reportTS

			// The screen settles, so the classifier -- which owns the pane now
			// -- reaches an idle verdict and goes on reporting one.
			screens["%1"] = "a settled screen with nothing left to decide"
			var sawIdle bool
			for poll := 1; poll <= maxWindowPolls; poll++ {
				p.refresh(ctx)
				row := stateOf(t, p, "%1")
				if row.StateSource != SourceScreen {
					t.Fatalf("poll %d after the drop = %+v, want the pane still on the classifier: "+
						"a classifier idle is not evidence that the overturned report was right, "+
						"and clearing on one resurrects a subagent's false idle", poll, row)
				}
				if row.FinishedAt == reportTS {
					t.Fatalf("poll %d after the drop derived finishedAt from the overturned "+
						"report's own timestamp %d", poll, reportTS)
				}
				if row.AgentState == StateIdle {
					sawIdle = true
				}
			}
			// Without this the test is vacuous: if the classifier never said
			// idle, "a classifier idle changes nothing" was never exercised.
			if !sawIdle {
				t.Fatalf("the classifier never reached an idle verdict in %d polls, so the "+
					"trigger this test exists to refuse was never pulled", maxWindowPolls)
			}
		})
	}
}

// The claim that makes a permanent-sounding rejection affordable, and the
// reason it has to be in the comment as well as in a test: a rejected report
// hands the pane back to the classifier, and the classifier is an authority
// that can stamp a finish.
//
// For rule 3 to have fired at all the screen must have churned through the
// whole window, which sets everChanged; when it finally settles, Observe stamps
// finishedAt = now in the ordinary way. A wrongly-dropped true turn end does
// not lose its badge -- it gets one dated by when the daemon NOTICED instead of
// by when the agent finished. Those are two different facts and the test says
// which one it got.
func TestARule3RejectionDoesNotCostTheBadge(t *testing.T) {
	f := rejectedReportPoller(t, "rule 3, a screen that never settled")
	p, ctx, screens, reportTS := f.p, f.ctx, f.screens, f.reportTS

	before := time.Now().UnixMilli()
	screens["%1"] = "the run really has ended now"
	var row Row
	for polls := 0; row.FinishedAt == 0; {
		polls++
		if polls > maxWindowPolls {
			t.Fatalf("the classifier never stamped a finish in %d polls: a wrongly-dropped turn "+
				"end must still earn a badge", maxWindowPolls)
		}
		p.refresh(ctx)
		row = stateOf(t, p, "%1")
	}
	if row.AgentState != StateIdle || row.StateSource != SourceScreen {
		t.Errorf("%+v, want idle decided by the classifier", row)
	}
	if row.FinishedAt == reportTS {
		t.Fatalf("finishedAt = the overturned report's own timestamp %d: the badge must be dated "+
			"by when the daemon noticed, not by a claim the evidence overturned", reportTS)
	}
	if row.FinishedAt < before {
		t.Errorf("finishedAt = %d, want a stamp dated now (at or after %d)", row.FinishedAt, before)
	}
}

// The question Task 5's implementer carried forward, settled here rather than
// left as a derivation the next reader has to redo.
//
// refresh resets p.classifier on a tmux-server generation change; p.reports is
// now reset beside it. The derivation that it was safe without one holds as far
// as it goes -- a restarted server's panes carry no options, so the raw value
// is "", ParseReport fails and Observe deletes the entry -- but it makes the
// daemon's report memory depend for its correctness on what tmux happens to be
// holding, and "across a restart both slots are empty" is a claim about the
// DAEMON. The fixture makes the difference visible by keeping the option value
// standing across the generation change, which the derivation assumes cannot
// happen: with the reset the standing report is a first sight and re-enters the
// window; without it, a rejection recorded against a dead server's %1 silently
// condemns the report of a fresh agent that reused the id.
//
// Cheap, too: a pane id that really is gone is pruned by Retain one poll later
// anyway, so the reset costs at most one re-verification of a live report.
func TestAServerRestartEmptiesTheReportMemory(t *testing.T) {
	f := rejectedReportPoller(t, "rule 3, a screen that never settled")
	p, ctx, screens, reportTS := f.p, f.ctx, f.screens, f.reportTS
	// The reset fires on the generation CHANGING, not on having read one. One
	// more poll on the same server, to say so: a reset that fired on every poll
	// would make every poll a first sight, and no report could ever be verified
	// and no pane could ever earn an edge again.
	p.refresh(ctx)
	if row := stateOf(t, p, "%1"); row.StateSource != SourceScreen {
		t.Fatalf("setup: %+v, want the overturned report still refused on the same server", row)
	}

	// The server restarts. %1 is a different pane on a different server now,
	// and the report standing on it is one the daemon has never seen.
	*f.generation = "200"
	screens["%1"] = "new server, a fresh agent"
	p.refresh(ctx)
	row := stateOf(t, p, "%1")
	if row.AgentState != StateIdle || row.StateSource != SourceEvent {
		t.Fatalf("the first poll on a new tmux server = %+v, want the standing report accepted as "+
			"a FIRST SIGHT: the rejection was recorded against the dead server's %%1", row)
	}
	if row.FinishedAt != 0 {
		t.Errorf("finishedAt = %d on the first poll after a restart, want the report to ENTER the "+
			"verification window rather than re-derive: a pre-restart report that is 'almost "+
			"certainly a real turn end' is the subagent false-idle waved through by an adverb",
			row.FinishedAt)
	}

	// Verified from scratch, on this server's screen, and only then does it
	// derive. This is also what keeps the reset from being fired every poll --
	// a memory reset on every poll would restart the window every poll and
	// nothing could ever be verified.
	for polls := 0; row.FinishedAt == 0; {
		polls++
		if polls > maxWindowPolls {
			t.Fatalf("the window never closed in %d polls after the restart", maxWindowPolls)
		}
		p.refresh(ctx)
		row = stateOf(t, p, "%1")
	}
	if row.FinishedAt != reportTS || row.StateSource != SourceEvent {
		t.Errorf("%+v, want the report verified from scratch and finishedAt = its own timestamp %d",
			row, reportTS)
	}
}

// --- finishedAt, and the switch between authorities --------------------------
//
// The two authorities date a finish differently and that difference is the
// whole of this section. A REPORT carries the time the agent actually finished,
// so it can be believed on sight. The CLASSIFIER has no clock of its own: its
// only way to date a finish is time.Now() at the moment it first noticed, so a
// first sight there must stamp nothing -- everChanged -- or every restart lights
// a done badge on every pane that happens to be sitting still.
//
// Which is why the guard is asymmetric, and why that reads like a contradiction
// until the dates are compared: the classifier's first sight of a pane idle
// since yesterday invents finishedAt = now, which beats every browser's stored
// `seen` and lights every device for a fiction. A report's first sight says
// "this agent finished at 21:20", and the browser badges only if that device has
// not looked since 21:20 -- which is exactly what the badge is supposed to mean.

// The derivation is STATELESS: a pane whose accepted report is a resting idle
// carries that report's own timestamp, read back out of tmux, with no memory of
// a previous report and no edge anywhere in it.
//
// The restart is the assertion revision 1 of the design claimed and revision 1's
// mechanism could not have satisfied. An implementation that remembered the
// previous report and stamped on the working -> idle EDGE answers 0 here, on
// both pollers: neither has ever seen a working report for this pane, and after
// a restart neither ever will.
func TestFinishedAtIsDerivedFromTheReportWithNoDaemonMemory(t *testing.T) {
	// Dated long before this daemon started, and past workingTTL -- a resting
	// report is not re-asserted and does not expire on a clock. It is also what
	// makes the assertion below able to tell the report's timestamp from a
	// clock read at the poll.
	reportTS := time.Now().Add(-workingTTL - 7*time.Minute).UnixMilli()
	rows := []Row{{PaneID: "%1", Command: "claude"}}
	screens := map[string]string{"%1": "a screen nobody is connected to see"}
	reports := map[string]string{"%1": FormatReport(StateIdle, reportTS, "")}
	connected := false

	var captured []string
	p := reportPoller(&rows, reports, screens, &captured, &connected)
	p.refresh(context.Background())
	row := stateOf(t, p, "%1")
	if row.AgentState != StateIdle || row.StateSource != SourceEvent {
		t.Fatalf("a standing resting idle = %+v, want idle from the event source", row)
	}
	if row.FinishedAt != reportTS {
		t.Errorf("finishedAt = %d, want the report's own timestamp %d exactly: the report carries "+
			"the time the agent finished, and a clock read at the poll would date a run this "+
			"daemon never watched to the moment it first looked", row.FinishedAt, reportTS)
	}
	if len(captured) != 0 {
		t.Errorf("captured %v with no client connected", captured)
	}

	// The daemon restarts -- a brand new Reports and a brand new Classifier over
	// the same standing option -- and answers identically, because the fact
	// lives in tmux.
	var captured2 []string
	p2 := reportPoller(&rows, reports, screens, &captured2, &connected)
	p2.refresh(context.Background())
	restarted := stateOf(t, p2, "%1")
	if restarted.AgentState != StateIdle || restarted.StateSource != SourceEvent {
		t.Fatalf("after a restart = %+v, want idle from the event source", restarted)
	}
	if restarted.FinishedAt != reportTS {
		t.Errorf("finishedAt = %d after a restart, want the report's own timestamp %d: an edge "+
			"needs daemon memory, and a fresh Reports has none, so a badge derived from an edge "+
			"would not survive this", restarted.FinishedAt, reportTS)
	}
}

// Both directions, because they are deliberately asymmetric and the next reader
// will assume they are not.
func TestAuthoritySwitchStamping(t *testing.T) {
	t.Run("report to classifier stamps no edge", func(t *testing.T) {
		rows := []Row{{PaneID: "%1", Command: "claude"}}
		screens := map[string]string{"%1": "mid-run, frame 1"}
		reports := map[string]string{}
		var captured []string
		connected := true
		p := reportPoller(&rows, reports, screens, &captured, &connected)
		// A frozen clock, advanced by hand: the working report ages out because
		// TIME passed, and a report already in force can only be replaced by a
		// NEWER one, so there is no value the test could write that would make
		// it stale instead.
		clock := time.Now()
		p.nowFn = func() time.Time { return clock }
		ctx := context.Background()

		// A classifier run with a real change in it -- so the entry carries
		// everChanged, the flag that licenses a finish stamp -- left one poll
		// short of settling. Built from settleAfter, which no mutant in this
		// task retargets.
		p.refresh(ctx)
		screens["%1"] = "mid-run, frame 2"
		p.refresh(ctx)
		for i := 0; i < settleAfter-1; i++ {
			p.refresh(ctx)
		}
		if got := stateOf(t, p, "%1"); got.AgentState != StateWorking || got.StateSource != SourceScreen {
			t.Fatalf("setup: %+v, want a classifier run one poll short of settling", got)
		}

		// The turn's working report lands and takes the pane. The capture is
		// skipped, so this pane is not passed to Retain and the classifier's
		// entry -- hash, still count and everChanged together -- goes with it.
		reports["%1"] = FormatReport(StateWorking, clock.UnixMilli(), "run go")
		before := len(captured)
		p.refresh(ctx)
		if got := stateOf(t, p, "%1"); got.StateSource != SourceEvent {
			t.Fatalf("setup: %+v, want the working report in force", got)
		}
		if len(captured) != before {
			t.Fatalf("setup: a capture was taken under a fresh working report")
		}

		// The integration dies mid-turn. Nothing re-asserts, the report ages
		// out past workingTTL, and the pane goes back to the authority with no
		// clock -- on a screen byte-identical to the last one the classifier
		// saw, which is the shape that makes this dangerous.
		clock = clock.Add(workingTTL + time.Second)
		p.refresh(ctx)
		row := stateOf(t, p, "%1")
		if row.StateSource != SourceScreen {
			t.Fatalf("%+v, want the aged-out report handed back to the classifier", row)
		}
		if row.AgentState != StateWorking || row.FinishedAt != 0 {
			t.Fatalf("the first capture after the switch = %+v, want a FIRST SIGHT: working, no "+
				"edge. A retained entry compares equal on this very poll, against a hash taken "+
				"before the report ever stood, and stamps a finish for a run it never saw start",
				row)
		}

		// And it settles, from nothing. everChanged is false on a first sight,
		// so the settle stamps nothing either: this authority's run began when
		// it first looked, and it has no way to date anything before that.
		for i := 1; i <= settleAfter; i++ {
			p.refresh(ctx)
		}
		row = stateOf(t, p, "%1")
		if row.AgentState != StateIdle || row.StateSource != SourceScreen {
			t.Fatalf("%+v, want the classifier settled after settleAfter = %d identical captures",
				row, settleAfter)
		}
		if row.FinishedAt != 0 {
			t.Errorf("finishedAt = %d after a report -> classifier switch: the classifier can only "+
				"date a finish `now`, so a run it did not watch start must earn no edge -- "+
				"otherwise every integration that dies mid-turn lights a done badge on every "+
				"device ~3s later", row.FinishedAt)
		}
	})

	t.Run("classifier to report does produce one", func(t *testing.T) {
		rows := []Row{{PaneID: "%1", Command: "claude"}}
		screens := map[string]string{"%1": "mid-run, frame 1"}
		reports := map[string]string{}
		var captured []string
		connected := true
		p := reportPoller(&rows, reports, screens, &captured, &connected)
		clock := time.Now()
		p.nowFn = func() time.Time { return clock }
		ctx := context.Background()

		// The classifier owns the pane and sees real work, so everChanged is
		// set and this authority is licensed to stamp -- with the only date it
		// has, which is now.
		p.refresh(ctx)
		screens["%1"] = "mid-run, frame 2"
		p.refresh(ctx)
		if got := stateOf(t, p, "%1"); got.StateSource != SourceScreen || got.AgentState != StateWorking {
			t.Fatalf("setup: %+v, want the classifier watching a run", got)
		}

		// The turn ends. The integration's event fires 7-52 ms BEFORE the last
		// repaint -- measured, over 88 turns on three agents -- so the report is
		// dated slightly earlier than the screen that follows it, and the two
		// dates are distinguishable for exactly that reason.
		reportTS := clock.Add(-50 * time.Millisecond).UnixMilli()
		reports["%1"] = FormatReport(StateIdle, reportTS, "")
		screens["%1"] = "the turn has ended"
		var row Row
		for polls := 0; row.FinishedAt == 0; {
			polls++
			if polls > maxWindowPolls {
				t.Fatalf("no stamp in %d polls: a classifier -> report switch must produce one, "+
					"and the report is the authority that can date it", maxWindowPolls)
			}
			p.refresh(ctx)
			row = stateOf(t, p, "%1")
		}
		if row.AgentState != StateIdle || row.StateSource != SourceEvent {
			t.Fatalf("%+v, want idle from the event source", row)
		}
		if row.FinishedAt != reportTS {
			t.Errorf("finishedAt = %d, want the REPORT's own timestamp %d. The classifier settled "+
				"inside this very window and stamped its own finishedAt = %d; the row must carry "+
				"the agent's date for the finish, not the daemon's date for noticing it",
				row.FinishedAt, reportTS, clock.UnixMilli())
		}
	})
}

// A mid-install badge storm is not possible, and the reason is the difference
// between the two authorities rather than a guard.
//
// The install lands in the middle of a run, which is the worst case: the first
// report this daemon ever sees for the pane comes from the middle of a turn
// whose start it never saw. A `working` report derives nothing -- only a RESTING
// one does -- and when the turn really ends the badge it produces is dated by
// the agent. One badge, for a run that really finished, at the time it really
// finished.
func TestInstallingMidRunProducesOneTrueBadgeAtMost(t *testing.T) {
	rows := []Row{{PaneID: "%1", Command: "claude"}}
	screens := map[string]string{"%1": "mid-run, frame 0"}
	reports := map[string]string{}
	var captured []string
	connected := true
	p := reportPoller(&rows, reports, screens, &captured, &connected)
	clock := time.Now()
	p.nowFn = func() time.Time { return clock }
	ctx := context.Background()

	// Every distinct non-zero finishedAt this pane ever shows. A storm is
	// several of them; a badge for a run nobody watched is one with the wrong
	// date on it. Counting distinct values sees both.
	stamps := map[int64]bool{}
	poll := func() Row {
		p.refresh(ctx)
		row := stateOf(t, p, "%1")
		if row.FinishedAt != 0 {
			stamps[row.FinishedAt] = true
		}
		return row
	}

	// Before the install: an agent mid-run, watched by the classifier alone. It
	// never settles, so it never stamps -- and everChanged is true throughout,
	// so nothing below is protected by the flag being false.
	for i := 1; i <= settleAfter+2; i++ {
		clock = clock.Add(time.Second)
		screens["%1"] = "mid-run, frame " + strconv.Itoa(i)
		if row := poll(); row.StateSource != SourceScreen || row.AgentState != StateWorking {
			t.Fatalf("poll %d before the install = %+v, want the classifier watching a run", i, row)
		}
	}

	// The integration is installed and the agent's next event lands: a working
	// report from the middle of a turn. It takes the pane and stamps nothing.
	clock = clock.Add(time.Second)
	reports["%1"] = FormatReport(StateWorking, clock.UnixMilli(), "run go")
	if row := poll(); row.StateSource != SourceEvent || row.AgentState != StateWorking {
		t.Fatalf("the first report after the install = %+v, want working from the event source", row)
	}

	// The turn ends for real. The report is dated by the agent, just before the
	// final repaint.
	clock = clock.Add(2 * time.Second)
	finish := clock.Add(-50 * time.Millisecond).UnixMilli()
	reports["%1"] = FormatReport(StateIdle, finish, "")
	screens["%1"] = "the turn has ended"
	var row Row
	for polls := 0; row.FinishedAt == 0; {
		polls++
		if polls > maxWindowPolls {
			t.Fatalf("the turn end earned no badge in %d polls", maxWindowPolls)
		}
		row = poll()
	}
	// And it is not re-dated afterwards: a badge that moved forward on every
	// poll could never be cleared by looking at the pane.
	for i := 0; i < settleAfter+2; i++ {
		clock = clock.Add(time.Second)
		poll()
	}

	if len(stamps) != 1 || !stamps[finish] {
		t.Errorf("finishedAt took %d distinct values %v across the install, want exactly one: the "+
			"agent's own %d", len(stamps), stamps, finish)
	}
}

// The full precedence table: report present/absent x fresh/stale x screen
// available/unavailable, asserting AgentState, StateSource AND FinishedAt
// together.
//
// Asserting the state alone cannot see a precedence bug at all -- it is how v2's
// blocked override hid a finish stamp for a run that never finished -- and
// FinishedAt is the field the badge is actually made of. "Screen available" is a
// connected client: the capture is what a resting report is checked against, and
// with nobody connected there is no screen to have an opinion.
//
// Every row builds its own poller. The derivation is stateless, so a table
// sharing one would be testing a sequence instead of a precedence.
func TestPrecedenceTable(t *testing.T) {
	now := time.Now()
	fresh := now.Add(-time.Second).UnixMilli()
	// Past the working window, written against the constant.
	stale := now.Add(-workingTTL - time.Minute).UnixMilli()

	for _, tc := range []struct {
		name       string
		report     string // "" for an unset option
		connected  bool
		state      string
		source     string
		finishedAt int64
	}{
		{
			name: "no report, screen available", connected: true,
			state: StateWorking, source: SourceScreen,
		},
		{
			// Not frozen at a last value: state that is minutes old presented
			// as current is worse than none.
			name: "no report, no screen",
		},
		{
			name:   "a fresh working report, screen available",
			report: FormatReport(StateWorking, fresh, "run go"), connected: true,
			state: StateWorking, source: SourceEvent,
		},
		{
			name:   "a fresh working report, no screen",
			report: FormatReport(StateWorking, fresh, "run go"),
			state:  StateWorking, source: SourceEvent,
		},
		{
			// The classifier's first sight, and no edge with it.
			name:   "a stale working report, screen available",
			report: FormatReport(StateWorking, stale, "run go"), connected: true,
			state: StateWorking, source: SourceScreen,
		},
		{
			// Nothing left: the report has expired and there is no screen.
			name:   "a stale working report, no screen",
			report: FormatReport(StateWorking, stale, "run go"),
		},
		{
			// The derivation is SUPPRESSED while the verification window is
			// open, not stamped and retracted: a done badge that has landed on
			// three devices does not un-land.
			name:   "a fresh idle report, screen available",
			report: FormatReport(StateIdle, fresh, ""), connected: true,
			state: StateIdle, source: SourceEvent,
		},
		{
			name:   "a fresh idle report, no screen",
			report: FormatReport(StateIdle, fresh, ""),
			state:  StateIdle, source: SourceEvent, finishedAt: fresh,
		},
		{
			// A resting report does not expire on a clock: the agent said it
			// has stopped, and nothing further happens until the user acts. An
			// hour-old idle is still in force, and still dates its own finish.
			name:   "an hour-old idle report, no screen",
			report: FormatReport(StateIdle, stale, ""),
			state:  StateIdle, source: SourceEvent, finishedAt: stale,
		},
		{
			// Resting, in force, and deriving nothing. An agent waiting at a
			// dialog has not finished.
			name:   "a fresh blocked report, screen available",
			report: FormatReport(StateBlocked, fresh, "Approve?"), connected: true,
			state: StateBlocked, source: SourceEvent,
		},
		{
			name:   "a fresh blocked report, no screen",
			report: FormatReport(StateBlocked, fresh, "Approve?"),
			state:  StateBlocked, source: SourceEvent,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows := []Row{{PaneID: "%1", Command: "claude"}}
			// No registered form on it, so a blocked report is neither
			// corroborated nor dropped on this first poll.
			screens := map[string]string{"%1": "a screen with no dialog on it"}
			reports := map[string]string{}
			if tc.report != "" {
				reports["%1"] = tc.report
			}
			var captured []string
			connected := tc.connected
			p := reportPoller(&rows, reports, screens, &captured, &connected)
			p.refresh(context.Background())

			row := stateOf(t, p, "%1")
			if row.AgentState != tc.state || row.StateSource != tc.source || row.FinishedAt != tc.finishedAt {
				t.Errorf("%+v\nwant state %q, source %q, finishedAt %d",
					row, tc.state, tc.source, tc.finishedAt)
			}
		})
	}
}

// --- the poll's deadline -----------------------------------------------------
//
// The poll is the daemon's hottest tmux path and was the only one with no
// bound on it. What that costs is not a slow sidebar: refresh runs on the one
// poll goroutine, so a wedged tmux parks it forever, every later tick is
// dropped by the select, and err stays nil -- the tree freezes WHILE THE DAEMON
// GOES ON SAYING IT IS FRESH. A dropped poll that says "stale" is the honest
// shape and the one these pin.
//
// timeout is set directly rather than through the interval in the tests below
// that have to wait one out, for the same reason nowFn and newTicker are
// seams: the real value is seconds, and a test that waits seconds to prove a
// deadline exists is a test nobody runs.

func TestThePollDeadlineHasHeadroomOverTheInterval(t *testing.T) {
	for _, tc := range []struct {
		name     string
		interval time.Duration
		want     time.Duration
	}{
		{
			// The daemon's own interval. Four of them, not one: a deadline
			// equal to the interval means a server that answers in 1.6s --
			// slow, loaded, but WORKING -- never completes a poll, and the
			// sidebar reports itself broken forever. The other direction costs
			// at most 6s of a tree that is behind before it says so, and the
			// browser needs two troubled polls (~3s) to say it anyway.
			"the default interval, four times over", 1500 * time.Millisecond, 6 * time.Second,
		},
		{
			// It scales, so an operator who slows the poll down on a loaded
			// box does not thereby tighten the deadline on it.
			"a long interval scales", 10 * time.Second, 40 * time.Second,
		},
		{
			// Below the floor the multiple would be tighter than one tmux
			// command is allowed to take anywhere else in this daemon (the
			// socket path bounds each of its own at 5s), and a poll dropped
			// while tmux was about to answer is a sidebar that says stale
			// about a healthy server.
			"a short interval gets the floor", 100 * time.Millisecond, 5 * time.Second,
		},
		{
			// The one that would break everything. NewPollerFunc(0, ...) is a
			// real construction -- every v1 test uses it -- and
			// context.WithTimeout(ctx, 0) is a context that has ALREADY
			// expired, so a floorless deadline would fail every poll a
			// zero-interval poller ever makes.
			"an unset interval is not an already-expired context", 0, 5 * time.Second,
		},
		{"nor is a negative one", -time.Second, 5 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := pollTimeout(tc.interval); got != tc.want {
				t.Errorf("pollTimeout(%v) = %v, want %v", tc.interval, got, tc.want)
			}
		})
	}
}

// The deadline is on the POLL, not on one command inside it: the snapshot and
// every capture it leads to run under the same one, so a poll cannot outlive it
// however many agent panes are on the server.
func TestTheWholePollRunsUnderOneDeadline(t *testing.T) {
	rows := []Row{{PaneID: "%1", Command: "claude"}}
	var snapshotDeadline, captureDeadline time.Time
	var snapshotOK, captureOK bool
	connected := true
	p := NewPollerWith(Options{
		Interval: 2 * time.Second,
		Snapshot: func(ctx context.Context) ([]Row, error) {
			snapshotDeadline, snapshotOK = ctx.Deadline()
			return append([]Row{}, rows...), nil
		},
		Capture: func(ctx context.Context, _ string) (string, error) {
			captureDeadline, captureOK = ctx.Deadline()
			return "a screen", nil
		},
		Connected: func() bool { return connected },
	})

	p.refresh(context.Background())

	if !snapshotOK {
		t.Fatal("the snapshot ran on a context with no deadline: a wedged tmux would park the poll goroutine forever")
	}
	if !captureOK {
		t.Fatal("the capture ran on a context with no deadline")
	}
	if !captureDeadline.Equal(snapshotDeadline) {
		t.Errorf("the capture's deadline %v is not the snapshot's %v; the whole poll must be bounded once, "+
			"or N agent panes multiply the bound by N", captureDeadline, snapshotDeadline)
	}
	// 4 * 2s, measured from the top of refresh: anything under three intervals
	// means the headroom is gone.
	if left := time.Until(snapshotDeadline); left < 7*time.Second || left > 8*time.Second {
		t.Errorf("the poll has %v left on its deadline, want ~8s (four 2s intervals)", left)
	}
}

// What a wedged tmux must look like: the tree that was there stays on screen,
// and the daemon says out loud that it is not the current one. Err() is what
// /api/snapshot turns into `stale` plus the sentence the sidebar prints under
// "Showing the last good state" -- see TestSnapshotServesStaleRowsWithAFlag in
// internal/front.
func TestAWedgedPollIsDroppedAndSaysSo(t *testing.T) {
	var polls int
	p := NewPollerFunc(time.Hour, func(ctx context.Context) ([]Row, error) {
		polls++
		if polls == 1 {
			return []Row{{PaneID: "%0"}}, nil
		}
		// A tmux that never answers. The real one is killed by
		// exec.CommandContext when the deadline fires and returns "signal:
		// killed"; what reaches refresh either way is an error and an expired
		// context.
		//
		// The second arm is what a poller with no deadline gets, and it is
		// there so that such a poller FAILS AN ASSERTION rather than hanging
		// the test binary: it answers late, with rows no dropped poll may ever
		// publish.
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(3 * time.Second):
			return []Row{{PaneID: "%9"}}, nil
		}
	})
	p.timeout = 40 * time.Millisecond

	p.refresh(context.Background())
	if got := p.Latest(); len(got) != 1 || p.Err() != nil {
		t.Fatalf("the first poll did not land: rows %+v, err %v", got, p.Err())
	}

	start := time.Now()
	p.refresh(context.Background())
	if took := time.Since(start); took > time.Second {
		t.Fatalf("refresh took %v to give up on a tmux that never answered, want ~40ms", took)
	}

	if rows := p.Latest(); len(rows) != 1 || rows[0].PaneID != "%0" {
		t.Errorf("Latest() = %+v after a dropped poll, want the last good tree", rows)
	}
	err := p.Err()
	if err == nil {
		t.Fatal("a poll that hit its deadline reported no error: the sidebar would freeze while calling itself fresh")
	}
	if !strings.Contains(err.Error(), "did not answer within 40ms") {
		t.Errorf("Err() = %q; the owner reads this sentence in the sidebar, so it has to say tmux did not answer and for how long", err)
	}
}

// A daemon shutting down cancels the poll's parent. That is not a wedged tmux
// and must not be dressed up as one -- an operator reading "tmux did not
// answer" out of their own Ctrl-C would go looking at tmux.
func TestAPollCutShortByShutdownIsNotReportedAsATimeout(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := NewPollerFunc(time.Hour, func(ctx context.Context) ([]Row, error) {
		cancel()
		<-ctx.Done()
		return nil, ctx.Err()
	})
	p.timeout = time.Hour

	p.refresh(ctx)

	err := p.Err()
	if err == nil {
		t.Fatal("a cancelled poll reported no error")
	}
	if strings.Contains(err.Error(), "did not answer within") {
		t.Errorf("Err() = %q, want the cancellation itself: only an expired DEADLINE is a wedged tmux", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Err() = %v, want context.Canceled", err)
	}
}

// --- the generation, from the batch ------------------------------------------

// The batched read carries the generation, so the poll asks for it in the fork
// it was already making. startFn stays for the poller that has no batch --
// NewPoller's -- and must not run beside one that has.
func TestRefreshTakesTheGenerationFromTheBatch(t *testing.T) {
	batch := Poll{Rows: []Row{{PaneID: "%1"}}, ServerStart: "100", HaveServerStart: true}
	p := NewPollerWith(Options{Poll: func(context.Context) (Poll, error) { return batch, nil }})

	p.refresh(context.Background())
	if got := p.ServerStart(); got != "100" {
		t.Fatalf("ServerStart() = %q, want the generation the batch carried", got)
	}

	// A restart, seen through the same one fork.
	batch.ServerStart = "200"
	p.refresh(context.Background())
	if got := p.ServerStart(); got != "200" {
		t.Errorf("ServerStart() = %q after a restart, want 200", got)
	}

	// A read whose generation block failed leaves the last known one in place,
	// exactly as a failed second fork used to: blanking it makes every
	// browser's per-pane memory miss for a poll.
	batch.ServerStart, batch.HaveServerStart = "", false
	p.refresh(context.Background())
	if got := p.ServerStart(); got != "200" {
		t.Errorf("ServerStart() = %q after a read that carried no generation, want the last known 200", got)
	}
}

// The fork this commit removes must not be wireable back in by accident: a
// poller given both the batch and a generation reader would fork twice per poll
// forever, and every test would stay green because the value is the same.
func TestNewPollerWithRefusesASecondGenerationFork(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("a poller wired with both the batch and ServerStart was accepted; that is two forks a poll " +
				"for one value, which is the thing the batch exists to stop")
		}
	}()
	NewPollerWith(Options{
		Poll:        func(context.Context) (Poll, error) { return Poll{}, nil },
		ServerStart: func(context.Context) (string, error) { return "100", nil },
	})
}

// -- the forced poll ---------------------------------------------------------
//
// "The sidebar refreshes immediately" was a promise only the browser kept: it
// re-fetched /api/snapshot, which serves Latest(), and nothing re-polled. A
// killed row lingered up to an interval, and a fresh split's pane id was absent
// from the very tree the browser had just asked for.

// gatedPoller is a poller whose every poll blocks until the test lets it
// through, so that "a poll is in flight" and "no poll has started" are both
// answers rather than intervals to wait out.
type gatedPoller struct {
	p     *Poller
	tick  chan time.Time
	gate  chan struct{} // one receive per poll; close it to let them all run
	polls atomic.Int64
	// inFlight is how many polls are running at this instant, and peak is the
	// most there have ever been. Peak above one is the failure PollNow exists
	// to avoid: refresh owns the classifier and the report memory with no lock,
	// so two overlapping polls corrupt a finish edge rather than crashing.
	inFlight atomic.Int64
	peak     atomic.Int64
}

func newGatedPoller() *gatedPoller {
	g := &gatedPoller{tick: make(chan time.Time), gate: make(chan struct{})}
	g.p = NewPollerFunc(time.Hour, func(context.Context) ([]Row, error) {
		n := g.polls.Add(1)
		if in := g.inFlight.Add(1); in > g.peak.Load() {
			g.peak.Store(in)
		}
		defer g.inFlight.Add(-1)
		<-g.gate
		// One row per poll, named after it, so a caller can say WHICH poll the
		// cached snapshot came from rather than only that it changed.
		return []Row{{PaneID: "%" + strconv.FormatInt(n, 10)}}, nil
	})
	g.p.newTicker = func(time.Duration) (<-chan time.Time, func()) { return g.tick, func() {} }
	return g
}

// latestPane is the pane id the cached snapshot carries, or "" if there is not
// one.
func (g *gatedPoller) latestPane() string {
	rows := g.p.Latest()
	if len(rows) == 0 {
		return ""
	}
	return rows[0].PaneID
}

// The promise itself: when PollNow returns, the poll it forced has already been
// published. Anything weaker -- a poll merely requested, or one still running --
// leaves the browser's next request served from the tree it was trying to get
// past, which for a split is a pane id that does not exist yet.
func TestPollNowPublishesBeforeItReturns(t *testing.T) {
	// In a bubble so that a PollNow nobody ever answers fails here and now --
	// synctest panics the moment every goroutine is blocked with no way to
	// proceed -- rather than parking the test binary until its timeout.
	synctest.Test(t, func(t *testing.T) {
		g := newGatedPoller()
		close(g.gate) // nothing to hold back in this one
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		g.p.Start(ctx)
		if got := g.latestPane(); got != "%1" {
			t.Fatalf("after Start the cache holds %q, want the first poll's row", got)
		}
		if err := g.p.PollNow(ctx); err != nil {
			t.Fatalf("PollNow: %v", err)
		}
		if got := g.latestPane(); got != "%2" {
			t.Fatalf("PollNow returned with %q cached, want the row of the poll it forced -- "+
				"a caller that has to poll again for it has been told nothing", got)
		}
		// Again, because a one-shot would pass the assertion above.
		if err := g.p.PollNow(ctx); err != nil {
			t.Fatalf("second PollNow: %v", err)
		}
		if got := g.latestPane(); got != "%3" {
			t.Fatalf("the second PollNow left %q cached, want a third poll", got)
		}
	})
}

// A poll that was ALREADY RUNNING when a caller asked does not answer that
// caller. This is the whole reason the loop clears the pending batch before it
// polls rather than after: a read taken before the caller's change cannot show
// it, and a PollNow satisfied by one has told the browser nothing while looking
// exactly like success.
func TestPollNowIsNotSatisfiedByAPollThatStartedFirst(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newGatedPoller()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		go g.p.Start(ctx)
		synctest.Wait()
		g.gate <- struct{}{} // poll 1, Start's own
		synctest.Wait()

		g.tick <- time.Now() // poll 2, in flight and gated
		synctest.Wait()

		var aDone, bDone bool
		go func() { _ = g.p.PollNow(ctx); aDone = true }()
		synctest.Wait()

		g.gate <- struct{}{} // poll 2 finishes; the loop takes A's batch, poll 3 starts
		synctest.Wait()
		if n := g.polls.Load(); n != 3 || aDone {
			t.Fatalf("polls=%d aDone=%v, want A's poll running and A still waiting", n, aDone)
		}

		// B asks DURING poll 3. Poll 3 began before B did, so it cannot be B's.
		go func() { _ = g.p.PollNow(ctx); bDone = true }()
		synctest.Wait()

		g.gate <- struct{}{} // poll 3 finishes: A is answered, B must not be
		synctest.Wait()
		if !aDone {
			t.Fatal("A was not answered by the poll the loop started for it")
		}
		if bDone {
			t.Fatalf("B was answered by a poll that started before B asked: the tree it got was "+
				"read at %q, which is before whatever B did", g.latestPane())
		}

		g.gate <- struct{}{} // poll 4, which is B's
		synctest.Wait()
		if !bDone {
			t.Fatal("B never got a poll of its own")
		}
		if got := g.latestPane(); got != "%4" {
			t.Fatalf("B was answered from %q, want the fourth poll's row", got)
		}
	})
}

// No two polls ever overlap, whoever asked for them.
//
// This is the reason PollNow is a request to the poll goroutine rather than a
// call to refresh. refresh owns the classifier and the report memory outright
// and neither has a lock; two concurrent polls do not crash, they mis-stamp a
// finish edge, which is a done badge on every device for work that did not
// finish. A bubble is what makes "the forced poll has NOT started" an assertion
// instead of a sleep: Wait returns only once every goroutine is durably blocked.
func TestPollNowNeverRunsBesideTheTicker(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newGatedPoller()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		go g.p.Start(ctx) // Start's first poll is synchronous and gated
		synctest.Wait()
		g.gate <- struct{}{} // let poll 1 through
		synctest.Wait()

		// A tick is in the loop's hands and its poll is blocked in fn.
		g.tick <- time.Now()
		synctest.Wait()
		if n := g.polls.Load(); n != 2 {
			t.Fatalf("%d polls, want the tick's to be in flight", n)
		}

		var forced error
		go func() { forced = g.p.PollNow(ctx) }()
		synctest.Wait()

		// The whole assertion: everything is blocked, and the forced poll has
		// not begun. A PollNow that called refresh itself would be inside fn
		// right now, beside the tick's.
		if n := g.polls.Load(); n != 2 {
			t.Fatalf("%d polls started while the tick's was still running: a forced poll must wait for it", n)
		}

		close(g.gate)
		synctest.Wait()
		if forced != nil {
			t.Fatalf("PollNow: %v", forced)
		}
		if n := g.polls.Load(); n != 3 {
			t.Fatalf("%d polls in all, want the initial one, the tick's and the forced one", n)
		}
		if peak := g.peak.Load(); peak != 1 {
			t.Fatalf("%d polls were in flight at once; refresh owns the classifier and the report "+
				"memory with no lock, so two at a time mis-stamp a finish edge", peak)
		}
	})
}

// Concurrent callers share one poll. Ten management requests landing together
// must not cost ten polls -- each is a tmux fork plus a capture per agent pane,
// and the whole point of the cached snapshot is that its cost is per interval
// and not per reader.
func TestPollNowCoalescesConcurrentCallers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newGatedPoller()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		go g.p.Start(ctx)
		synctest.Wait()
		g.gate <- struct{}{} // poll 1, Start's own
		synctest.Wait()

		// A tick first, so the loop is BUSY when the callers arrive. That is
		// what makes the count below exact rather than a race: a batch is
		// handed to the loop the moment the first caller opens it, so without
		// this the first caller might be dequeued before the tenth has joined
		// and the answer would be two polls on some runs and one on others.
		// Both are coalescing; only one of them is an assertion.
		g.tick <- time.Now()
		synctest.Wait()

		const callers = 10
		errs := make([]error, callers)
		var wg sync.WaitGroup
		for i := range callers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs[i] = g.p.PollNow(ctx)
			}()
		}
		synctest.Wait()

		// All ten are in ONE batch: the loop is inside the tick's poll, so it
		// has taken nothing off the force channel, so pending was non-nil for
		// every caller after the first.
		close(g.gate)
		wg.Wait()

		for i, err := range errs {
			if err != nil {
				t.Fatalf("caller %d: %v", i, err)
			}
		}
		// ONE poll for the ten callers, on top of Start's and the tick's.
		// Twelve is what one poll each would look like.
		if n := g.polls.Load(); n != 3 {
			t.Fatalf("%d polls for Start, one tick and %d concurrent callers; want 3, which is "+
				"one poll shared by all ten", n, callers)
		}
	})
}

// A caller that runs out of time is answered, and the poll it asked for still
// happens: it was requested, every other reader is about to be served by it,
// and the fork it costs has already been paid.
func TestPollNowGivesUpOnItsOwnDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newGatedPoller()
		loopCtx, cancel := context.WithCancel(context.Background())
		defer cancel()

		go g.p.Start(loopCtx)
		synctest.Wait()
		g.gate <- struct{}{}
		synctest.Wait()

		callerCtx, callerCancel := context.WithTimeout(loopCtx, time.Second)
		defer callerCancel()
		var err error
		go func() { err = g.p.PollNow(callerCtx) }()
		synctest.Wait() // the poll is in flight and the caller is waiting on it

		time.Sleep(2 * time.Second) // the bubble's clock, not the test's
		synctest.Wait()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("PollNow after its deadline = %v, want the deadline", err)
		}
		if n := g.polls.Load(); n != 2 {
			t.Fatalf("%d polls, want the abandoned caller's to have started anyway", n)
		}

		// And it finishes and publishes, on the poll context rather than the
		// caller's: a browser that went away must not abort a poll every other
		// reader is about to be served.
		close(g.gate)
		synctest.Wait()
		if got := g.latestPane(); got != "%2" {
			t.Fatalf("the abandoned poll published %q, want its own row", got)
		}
	})
}

// A poller whose context is gone answers rather than waiting: on shutdown an
// in-flight management request would otherwise sit here until its own deadline,
// inside a grace period that is the same five seconds.
func TestPollNowOnAStoppedPollerSaysSo(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newGatedPoller()
		close(g.gate)
		ctx, cancel := context.WithCancel(context.Background())
		g.p.Start(ctx)
		cancel()
		synctest.Wait()

		// context.Background(), so nothing but the poller's own state can end
		// this call. A PollNow that only watched its caller's context would
		// hang here forever.
		if err := g.p.PollNow(context.Background()); !errors.Is(err, errPollerStopped) {
			t.Fatalf("PollNow on a stopped poller = %v, want it to say the poller has stopped", err)
		}
	})
}

// A poller that was never started has no loop to serve a forced poll, and this
// is what that looks like: the caller waits out its own context and is told so.
// Not a hang, and not a silent success either -- a success would be a claim
// that a poll happened.
func TestPollNowOnAPollerThatWasNeverStarted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newGatedPoller()
		close(g.gate)
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := g.p.PollNow(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("PollNow on an unstarted poller = %v, want its caller's deadline", err)
		}
		if n := g.polls.Load(); n != 0 {
			t.Fatalf("%d polls ran without a loop to run them", n)
		}
	})
}

// A forced poll runs on the DAEMON's context, not on the context of whoever
// asked for it. Two halves, and this pins the one a surviving mutant found
// unpinned: the poll must stop when the daemon does. The other half -- that a
// caller giving up does not abort it -- is TestPollNowGivesUpOnItsOwnDeadline
// above, and the two are the same decision seen from either end.
//
// Checked from INSIDE the poll, because refresh puts its own deadline on the
// context and cancels it on the way out: after a poll has returned, the context
// it ran on is cancelled whatever it was derived from, so an assertion made
// afterwards would pass on every implementation.
func TestAForcedPollRunsOnTheDaemonsContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newGatedPoller()
		seen := make(chan context.Context, 4)
		inner := g.p.fn
		g.p.fn = func(ctx context.Context) (Poll, error) {
			seen <- ctx
			return inner(ctx)
		}
		loopCtx, cancel := context.WithCancel(context.Background())

		go g.p.Start(loopCtx)
		synctest.Wait()
		g.gate <- struct{}{} // Start's own poll
		synctest.Wait()
		<-seen

		// context.Background(): the caller's context outlives everything here,
		// so nothing but the daemon's own can end this poll.
		go func() { _ = g.p.PollNow(context.Background()) }()
		synctest.Wait()

		forced := <-seen
		if forced.Err() != nil {
			t.Fatalf("the forced poll was handed a context that was already %v", forced.Err())
		}

		cancel()
		synctest.Wait()
		if forced.Err() == nil {
			t.Fatal("the daemon shut down and the forced poll it was running did not notice: " +
				"a poll on a context of its own outlives the daemon that asked for it")
		}

		g.gate <- struct{}{} // let it finish, so the bubble can empty
		synctest.Wait()
	})
}

// A forced poll asked for DURING Start's own first poll is still answerable.
//
// Start polls once synchronously before the loop exists, and on a real daemon
// that poll is a tmux fork -- long enough for a request to land inside it. A
// caller that arrives in that window must still learn that the poller has
// stopped, which it can only do if Start published its context before polling
// rather than after: a caller that read nil there waits on its own context
// forever, and the one this daemon hands it may not have a deadline.
func TestPollNowDuringStartsFirstPoll(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := newGatedPoller()
		ctx, cancel := context.WithCancel(context.Background())

		go g.p.Start(ctx) // blocked in its first poll
		synctest.Wait()

		var err error
		done := make(chan struct{})
		go func() { defer close(done); err = g.p.PollNow(context.Background()) }()
		synctest.Wait()

		// Cancelled with the first poll STILL RUNNING, so the loop does not
		// exist yet and cannot answer the batch. Nothing but loopDone can end
		// this call, which is what makes the assertion below exact rather than
		// a coin toss between the two ready cases of a select.
		cancel()
		<-done
		if !errors.Is(err, errPollerStopped) {
			t.Fatalf("PollNow from inside Start's first poll = %v, want it to say the poller "+
				"has stopped -- on context.Background() there is nothing else to end it", err)
		}

		close(g.gate) // let the first poll, Start and the loop finish and the bubble empty
		synctest.Wait()
	})
}
