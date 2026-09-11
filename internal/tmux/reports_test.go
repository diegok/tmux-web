package tmux

import (
	"testing"
	"time"
)

func TestReportsInForce(t *testing.T) {
	now := time.UnixMilli(1789075200000)
	r := NewReports()
	ms := func(d time.Duration) int64 { return now.Add(d).UnixMilli() }

	// A first sight is accepted: the daemon has no better information than the
	// value tmux is holding.
	got, ok := r.Observe("%1", FormatReport(StateWorking, ms(0), "run go"), "claude", now)
	if !ok || got.State != StateWorking || got.Activity != "run go" {
		t.Fatalf("first sight = %+v, %v", got, ok)
	}

	// The SAME value on the next poll is the same report, still in force. A
	// filter written as "strictly newer than the last accepted" applied to the
	// standing value would drop the pane's state on every second poll.
	if got, ok := r.Observe("%1", FormatReport(StateWorking, ms(0), "run go"), "claude", now.Add(1500*time.Millisecond)); !ok || got.State != StateWorking {
		t.Fatalf("re-reading the standing value = %+v, %v", got, ok)
	}

	// A newer one supersedes.
	if got, _ := r.Observe("%1", FormatReport(StateBlocked, ms(time.Second), "Approve?"), "claude", now.Add(time.Second)); got.State != StateBlocked {
		t.Fatalf("newer report = %+v", got)
	}

	// An OLDER one is refused and the accepted one stays in force. This is
	// ordinary scheduling jitter -- the writes are fire-and-forget, so a
	// working write from an earlier PreToolUse can land after a blocked write
	// from a later Notification -- not a broken integration.
	if got, ok := r.Observe("%1", FormatReport(StateWorking, ms(500*time.Millisecond), ""), "claude", now.Add(2*time.Second)); !ok || got.State != StateBlocked {
		t.Fatalf("a late older write = %+v, %v; want the blocked report still standing", got, ok)
	}
}

func TestReportsFreshness(t *testing.T) {
	now := time.UnixMilli(1789075200000)
	r := NewReports()
	w := FormatReport(StateWorking, now.UnixMilli(), "run go")

	// Written against the constant, never against 60. The number is a guess
	// (open question 2) and will change.
	//
	// The in-force assertion is at EXACTLY workingTTL, not one millisecond
	// short of it, and that is the whole point of it. `TTL - 1ms` is inside the
	// window under `> workingTTL` and under `>= workingTTL` alike, so a fixture
	// there cannot see the difference between the two operators and the `>=`
	// mutant survives it. At exactly the boundary `>` keeps the report and `>=`
	// expires it, and the fixture stays on the boundary whatever the constant
	// becomes. Found by mutation.
	if _, ok := r.Observe("%1", w, "claude", now.Add(workingTTL)); !ok {
		t.Fatal("a working report at exactly workingTTL must still be in force: the comparison is `>`, not `>=`")
	}
	if _, ok := r.Observe("%1", w, "claude", now.Add(workingTTL+time.Second)); ok {
		t.Fatal("a working report past the window must expire: a crashed agent must not show as busy")
	}

	// A resting state does not expire on a clock. The agent said it has
	// stopped, and by definition nothing further happens until the user acts --
	// which may be tomorrow, which is the case the app exists for.
	i := FormatReport(StateIdle, now.UnixMilli(), "")
	r2 := NewReports()
	if _, ok := r2.Observe("%2", i, "claude", now.Add(24*time.Hour)); !ok {
		t.Fatal("a resting idle report must not expire on a clock")
	}
	b := FormatReport(StateBlocked, now.UnixMilli(), "Approve?")
	r3 := NewReports()
	if _, ok := r3.Observe("%3", b, "claude", now.Add(24*time.Hour)); !ok {
		t.Fatal("a resting blocked report must not expire on a clock")
	}
}

func TestReportsAreDroppedWhenTheAgentIsGone(t *testing.T) {
	now := time.UnixMilli(1789075200000)
	r := NewReports()
	v := FormatReport(StateBlocked, now.UnixMilli(), "Approve?")
	if _, ok := r.Observe("%1", v, "claude", now); !ok {
		t.Fatal("setup")
	}
	// The command check: the pane is no longer running a known agent, so the
	// agent exited and the report is dropped unconditionally. This catches the
	// crash case, which is the case a clock was supposed to catch.
	if _, ok := r.Observe("%1", v, "zsh", now.Add(time.Second)); ok {
		t.Fatal("a report on a pane that is no longer an agent must be dropped")
	}
	// There is deliberately no third assertion here. See "What dropping does
	// NOT mean" in the task: a claude -> zsh -> claude pane IS a first sight,
	// and a first sight is accepted.
}

func TestAnUnsetOptionClearsTheMemory(t *testing.T) {
	now := time.UnixMilli(1789075200000)
	r := NewReports()
	if _, ok := r.Observe("%1", FormatReport(StateIdle, now.UnixMilli(), ""), "claude", now); !ok {
		t.Fatal("setup")
	}
	// `tmux set -p -u @wterm_agent` is the documented escape hatch for a stuck
	// report. If the daemon kept serving the last value it accepted, the escape
	// hatch would do nothing.
	if _, ok := r.Observe("%1", "", "claude", now.Add(time.Second)); ok {
		t.Fatal("an unset option must clear the report")
	}
	// A value we cannot parse means the same thing: a report we cannot parse is
	// not a report we wrote.
	r2 := NewReports()
	r2.Observe("%1", FormatReport(StateIdle, now.UnixMilli(), ""), "claude", now)
	if _, ok := r2.Observe("%1", "garbage", "claude", now.Add(time.Second)); ok {
		t.Fatal("an unparseable value must clear the report")
	}
}

// Retain is Classifier.Retain's semantic on the report memory, and the poller
// leans on it every poll: a pane that is no longer a known agent is not in
// `keep`, and its accepted report has to go with it or a relaunched agent
// inherits a predecessor's ordering floor.
func TestReportsRetain(t *testing.T) {
	now := time.UnixMilli(1789075200000)
	r := NewReports()
	old := FormatReport(StateBlocked, now.UnixMilli(), "Approve?")
	for _, id := range []string{"%1", "%2"} {
		if _, ok := r.Observe(id, old, "claude", now); !ok {
			t.Fatal("setup")
		}
	}

	r.Retain([]string{"%1"})
	// %2 was forgotten, so a report OLDER than the one it held is now a first
	// sight and is accepted. On a retained entry the ordering filter would
	// refuse it and keep the blocked one.
	older := FormatReport(StateWorking, now.Add(-time.Minute).UnixMilli(), "")
	if got, ok := r.Observe("%2", older, "claude", now); !ok || got.State != StateWorking {
		t.Errorf("%%2 after Retain = %+v, %v; want a first sight accepting the older report", got, ok)
	}
	if got, ok := r.Observe("%1", older, "claude", now); !ok || got.State != StateBlocked {
		t.Errorf("%%1 after Retain = %+v, %v; want the retained blocked report still standing", got, ok)
	}

	// Retaining nothing resets it entirely, which is what a disconnected client
	// and a tmux server restart both do to the classifier.
	r.Retain(nil)
	if got, ok := r.Observe("%1", older, "claude", now); !ok || got.State != StateWorking {
		t.Errorf("%%1 after Retain(nil) = %+v, %v; want a first sight", got, ok)
	}
}

// The verification window belongs to the REPORT, not to the pane: a newer
// report is a new claim and gets a fresh window. Without the reset, a second
// turn's idle would inherit the first turn's verdict and be believed without
// ever being checked.
func TestANewerReportOpensAFreshWindow(t *testing.T) {
	now := time.UnixMilli(1789075200000)
	r := NewReports()

	if _, ok := r.Observe("%1", FormatReport(StateIdle, now.UnixMilli(), ""), "claude", now); !ok {
		t.Fatal("setup: the first report was not accepted")
	}
	if !r.NeedsScreen("%1") {
		t.Fatal("a resting idle with an open window must be checked against the screen")
	}
	if r.Confirmed("%1", true) {
		t.Fatal("confirmed with a client connected and the window still open")
	}
	if !r.Corroborate("%1", true) {
		t.Fatal("an idle verdict must leave the report standing")
	}
	if r.NeedsScreen("%1") {
		t.Error("the window stayed open after the verdict: the captures must stop")
	}
	if !r.Confirmed("%1", true) {
		t.Error("a corroborated report may derive finishedAt")
	}

	// A second turn in the same pane.
	later := now.Add(time.Minute)
	if _, ok := r.Observe("%1", FormatReport(StateIdle, later.UnixMilli(), ""), "claude", later); !ok {
		t.Fatal("setup: the second report was not accepted")
	}
	if !r.NeedsScreen("%1") || r.Confirmed("%1", true) {
		t.Error("the second report inherited the first one's verdict")
	}
}

// Only the RESTING states are checked against the screen. A working report is
// re-asserted by its own writer and expires on a clock, so there is nothing for
// a capture to add and taking one would give back the fork the whole feature
// exists to save.
//
// idle and blocked both ask for one, for different reasons and over different
// counts: idle has a window that closes at the classifier's verdict (rule 3),
// blocked is checked for as long as it stands, because rule 1's evidence -- a
// capture that moved -- can arrive at any poll.
func TestOnlyARestingReportNeedsTheScreen(t *testing.T) {
	now := time.UnixMilli(1789075200000)
	for _, tc := range []struct {
		state string
		want  bool
	}{
		{StateWorking, false},
		{StateIdle, true},
		{StateBlocked, true},
	} {
		r := NewReports()
		if _, ok := r.Observe("%1", FormatReport(tc.state, now.UnixMilli(), ""), "claude", now); !ok {
			t.Fatalf("setup: the %s report was not accepted", tc.state)
		}
		if got := r.NeedsScreen("%1"); got != tc.want {
			t.Errorf("NeedsScreen with a %s report in force = %v, want %v", tc.state, got, tc.want)
		}
	}
}

// A blocked report's window never closes on agreement the way an idle one's
// does. There is no verdict that ends the checking: the dialog can be answered
// at the terminal at any poll, and the poll it is answered at is the one rule 2
// starts counting from.
func TestABlockedReportIsCheckedForAsLongAsItStands(t *testing.T) {
	now := time.UnixMilli(1789075200000)
	r := NewReports()
	if _, ok := r.Observe("%1", FormatReport(StateBlocked, now.UnixMilli(), ""), "claude", now); !ok {
		t.Fatal("setup: the blocked report was not accepted")
	}
	// A form on screen every poll: the report stands and the counter never
	// reaches NBlocked, however long it goes on.
	for i := 0; i < (settleAfter+1)*3; i++ {
		if !r.NeedsScreen("%1") {
			t.Fatalf("poll %d: the captures stopped while the report still stood", i+1)
		}
		if !r.CorroborateBlocked("%1", false, true) {
			t.Fatalf("poll %d: a settled screen still showing a form dropped the report", i+1)
		}
	}
	// A pane with no report in force has nothing to check.
	if r.CorroborateBlocked("%2", false, false) {
		t.Error("evidence for a pane with no report in force was accepted")
	}
}

// Rule 2 counts a RUN of consecutive settled polls with no registered form on
// screen, and a form on screen starts the run again.
//
// Driven at the method rather than through the poller, because through the
// poller it is unreachable today: a form arriving on a screen is a capture that
// moved, and rule 1 drops the report on that poll before rule 2 is asked
// anything. So "never drops at any count" -- the kill the plan's mutant table
// names for this -- cannot see it: on a screen that shows the dialog at every
// poll the counter is never incremented, and deleting the reset changes
// nothing. What the reset is for is the contract: the day rule 1 becomes
// something softer than a drop, a total rather than a run would drop a true
// blocked after NBlocked scattered polls spread over an afternoon.
func TestRule2CountsARunAndAFormStartsItAgain(t *testing.T) {
	now := time.UnixMilli(1789075200000)
	r := NewReports()
	if _, ok := r.Observe("%1", FormatReport(StateBlocked, now.UnixMilli(), ""), "claude", now); !ok {
		t.Fatal("setup: the blocked report was not accepted")
	}
	// settleAfter settled polls with nothing on screen: one short of the count,
	// and written off settleAfter so it stays there if NBlocked moves.
	for i := 1; i <= settleAfter; i++ {
		if !r.CorroborateBlocked("%1", false, false) {
			t.Fatalf("poll %d of settleAfter = %d: dropped early", i, settleAfter)
		}
	}
	if !r.CorroborateBlocked("%1", false, true) {
		t.Fatal("a form back on screen dropped the report")
	}
	// The run starts again from here, so settleAfter more polls must not drop.
	for i := 1; i <= settleAfter; i++ {
		if !r.CorroborateBlocked("%1", false, false) {
			t.Fatalf("poll %d after the form: dropped at %d polls since the form, want the run "+
				"counted from the form and not the total since the report", i, i)
		}
	}
	if r.CorroborateBlocked("%1", false, false) {
		t.Errorf("settleAfter+1 = %d settled formless polls after the form left the screen did "+
			"not drop the report", settleAfter+1)
	}
}

// With nobody connected there is no screen to check and the derivation is
// immediate; a pane with no report in force has nothing to derive from either
// way.
func TestConfirmedWithoutAClientOrWithoutAReport(t *testing.T) {
	now := time.UnixMilli(1789075200000)
	r := NewReports()
	if _, ok := r.Observe("%1", FormatReport(StateIdle, now.UnixMilli(), ""), "claude", now); !ok {
		t.Fatal("setup: the report was not accepted")
	}
	if !r.Confirmed("%1", false) {
		t.Error("with no client connected the derivation must be immediate")
	}
	if r.Confirmed("%2", true) || r.Confirmed("%2", false) {
		t.Error("a pane with no report in force confirmed one")
	}
	if r.Corroborate("%2", true) {
		t.Error("a verdict for a pane with no report in force was accepted")
	}
}
