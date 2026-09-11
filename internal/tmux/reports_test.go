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

// --- the rejection slot ------------------------------------------------------

// A delayed older write neither wins nor rescues the rejected report.
//
// There is no report "newer than the rejected one but older than the last
// accepted one" to test with: the rejected report IS st.accepted, so that
// interval is empty, and a test written to that wording would have had to
// invent a state the implementation cannot reach. The fixture that matters is
// the one ordinary scheduling jitter actually produces -- every write is
// fire-and-forget, so a working write from an earlier hook can land after the
// turn end that superseded it.
func TestARejectionSurvivesADelayedOlderWrite(t *testing.T) {
	now := time.UnixMilli(1789075200000)
	r := NewReports()
	t0 := now.UnixMilli()
	t1 := now.Add(2 * time.Second).UnixMilli()
	idle := FormatReport(StateIdle, t1, "")

	if _, ok := r.Observe("%1", FormatReport(StateWorking, t0, "run go"), "claude", now); !ok {
		t.Fatal("setup: the turn's working report was not accepted")
	}
	if _, ok := r.Observe("%1", idle, "claude", now.Add(2*time.Second)); !ok {
		t.Fatal("setup: the turn end was not accepted")
	}
	// Rule 3, the screen churning for the whole window. Driven to the verdict
	// rather than counted out, so the fixture does not restate NIdle.
	for polls := 1; r.Corroborate("%1", false); polls++ {
		if polls > maxWindowPolls {
			t.Fatalf("setup: the window never closed in %d polls", maxWindowPolls)
		}
	}
	if _, ok := r.Observe("%1", idle, "claude", now.Add(3*time.Second)); ok {
		t.Fatal("setup: the overturned report is still in force")
	}

	// (a) The late write is refused and NOTHING goes into force on this poll:
	// the pane stays on the classifier. Both halves are asserted, and they
	// catch different mutants.
	late := FormatReport(StateWorking, now.Add(time.Second).UnixMilli(), "an earlier tool call")
	got, ok := r.Observe("%1", late, "claude", now.Add(3*time.Second))
	if ok {
		switch got.State {
		case StateWorking:
			t.Fatalf("a delayed older write went into force: %+v. It is older than the report "+
				"already accepted, and dropping the ordering comparison puts a stale working on "+
				"the row", got)
		case StateIdle:
			t.Fatalf("the rejected report came back in force on the strength of an unrelated late "+
				"write: %+v. The guard belongs on st.accepted at the RETURN, not on parsed at the "+
				"entry -- the late write carries its own earlier timestamp, so it does not match "+
				"the slot, an entry guard waves it through, the ordering filter refuses the write "+
				"and the function then hands back st.accepted, which IS the rejected report", got)
		default:
			t.Fatalf("a delayed older write put %+v in force", got)
		}
	}

	// (b) It cleared nothing, and the only way to see that is to keep polling:
	// the option still holds idle@T1, so the next poll re-reads it, and it must
	// still be refused. A mutant that clears the slot on any parsed value
	// resurrects it here.
	for poll := 1; poll <= 2; poll++ {
		if got, ok := r.Observe("%1", idle, "claude", now.Add(time.Duration(3+poll)*time.Second)); ok {
			t.Fatalf("poll %d after the delayed write re-read the standing option and put %+v back "+
				"in force: refusing an older write must not clear the rejection", poll, got)
		}
	}
}

// What DOES clear the slot: a strictly newer value. The fixture is the next
// turn's working write -- an edge, written unconditionally by the integration
// without reading the standing option -- which is the thing that ends every
// rejection in practice.
func TestANewerValueClearsTheRejection(t *testing.T) {
	now := time.UnixMilli(1789075200000)
	r := NewReports()
	t1 := now.UnixMilli()
	idle := FormatReport(StateIdle, t1, "")

	if _, ok := r.Observe("%1", idle, "claude", now); !ok {
		t.Fatal("setup: the turn end was not accepted")
	}
	for polls := 1; r.Corroborate("%1", false); polls++ {
		if polls > maxWindowPolls {
			t.Fatalf("setup: the window never closed in %d polls", maxWindowPolls)
		}
	}
	if _, ok := r.Observe("%1", idle, "claude", now.Add(time.Second)); ok {
		t.Fatal("setup: the overturned report is still in force")
	}

	// The next turn starts.
	next := now.Add(time.Minute)
	working := FormatReport(StateWorking, next.UnixMilli(), "run go")
	got, ok := r.Observe("%1", working, "claude", next)
	if !ok || got.State != StateWorking || got.Activity != "run go" {
		t.Fatalf("the next turn's working write = %+v, %v; want it in force -- a newer value is a "+
			"different report and earns its own verdict", got, ok)
	}
	// And it stays in force when the same standing value is re-read, which is
	// what separates a cleared slot from one that merely lost a comparison.
	if got, ok := r.Observe("%1", working, "claude", next.Add(1500*time.Millisecond)); !ok || got.State != StateWorking {
		t.Fatalf("re-reading the newer standing value = %+v, %v; want it still in force", got, ok)
	}

	// The new claim is checked afresh rather than inheriting the overturned
	// one's fate: this turn's own end opens its own window.
	end := next.Add(time.Minute)
	if got, ok := r.Observe("%1", FormatReport(StateIdle, end.UnixMilli(), ""), "claude", end); !ok || got.State != StateIdle {
		t.Fatalf("the next turn's end = %+v, %v; want it accepted on its own terms", got, ok)
	}
	if !r.NeedsScreen("%1") || r.Confirmed("%1", true) {
		t.Error("the second turn's end inherited the first one's verdict instead of earning its own")
	}
}

// A restart empties both slots. The standing report is accepted by the ordering
// filter -- the daemon has no better information than the value tmux holds --
// and then verified from scratch: a resting idle ENTERS the window rather than
// deriving immediately.
//
// Revision 2 of the design re-derived immediately here, on the grounds that a
// pre-restart report "is almost certainly a real turn end" -- which is the
// subagent false-idle waved through by an adverb.
//
// That is the honest cost of holding the rejection in daemon memory rather than
// in tmux: bounded, one-shot, and only on a restart. The alternative -- writing
// the rejection back into @wterm_agent so it survives with the report -- is
// refused on a rule this design has held since "Not @wterm_label": the daemon
// READS that option, it does not write it, and a reader that edits the channel
// it reads cannot be reasoned about when two of them run.
func TestAfterARestartAStandingIdleEntersTheWindow(t *testing.T) {
	now := time.UnixMilli(1789075200000)
	standing := FormatReport(StateIdle, now.UnixMilli(), "")

	r := NewReports()
	if _, ok := r.Observe("%1", standing, "claude", now); !ok {
		t.Fatal("setup: the report was not accepted")
	}
	for polls := 1; r.Corroborate("%1", false); polls++ {
		if polls > maxWindowPolls {
			t.Fatalf("setup: the window never closed in %d polls", maxWindowPolls)
		}
	}
	if _, ok := r.Observe("%1", standing, "claude", now.Add(time.Second)); ok {
		t.Fatal("setup: the overturned report is still in force")
	}

	// The daemon restarts: a fresh memory over the very same tmux state.
	r2 := NewReports()
	got, ok := r2.Observe("%1", standing, "claude", now.Add(time.Hour))
	if !ok || got.State != StateIdle {
		t.Fatalf("the standing report after a restart = %+v, %v; want a first sight, accepted by "+
			"the ordering filter -- the rejection lived in the memory that has just gone", got, ok)
	}
	if !r2.NeedsScreen("%1") {
		t.Error("a resting idle after a restart must enter the verification window")
	}
	if r2.Confirmed("%1", true) {
		t.Error("a resting idle after a restart derived finishedAt immediately, with a client " +
			"connected and the window still open: that is the subagent false-idle waved through")
	}
	// Unchanged by any of this: with nobody connected there is no screen to
	// check and the derivation is immediate, restart or no restart.
	if !r2.Confirmed("%1", false) {
		t.Error("with no client connected the derivation must still be immediate")
	}
}

// The derivation's own half, one layer below the poller: a resting idle needs
// no previous report to be in force, and its timestamp is carried through
// unchanged, because that timestamp is the whole of finishedAt.
//
// An implementation that remembered the previous report and fired on the
// working -> idle EDGE would need daemon memory this Reports does not have. It
// would answer nothing here -- and nothing again after every daemon restart, on
// every pane whose turn ended while the daemon was down, which is most of them.
func TestARestingIdleNeedsNoPreviousReport(t *testing.T) {
	now := time.UnixMilli(1789075200000)
	// Dated well before this Reports existed, and past workingTTL: a resting
	// report is not re-asserted and does not expire on a clock, so an agent
	// that finished an hour ago still says so.
	finished := now.Add(-workingTTL - 10*time.Minute).UnixMilli()
	raw := FormatReport(StateIdle, finished, "")

	r := NewReports()
	got, ok := r.Observe("%1", raw, "claude", now)
	if !ok {
		t.Fatalf("a resting idle with no working report before it is not in force")
	}
	if got.State != StateIdle || got.Timestamp != finished {
		t.Errorf("in force: %+v, want idle at the report's own timestamp %d", got, finished)
	}
	if !r.Confirmed("%1", false) {
		t.Error("with nobody connected the derivation must be immediate: there is no screen to " +
			"check and the report is the only authority there is")
	}

	// The restart. A Reports that has seen nothing reads the same standing
	// value and answers identically, because the fact lives in tmux and not in
	// here -- which is what makes it survive a daemon restart, a poller restart
	// and a wterm-web upgrade.
	r2 := NewReports()
	got2, ok2 := r2.Observe("%1", raw, "claude", now)
	if !ok2 || got2 != got {
		t.Errorf("after a restart: %+v, %v, want the identical report %+v back", got2, ok2, got)
	}
}

// lateRepaintDwell is the CLASSIFIER's guard and must never reach a report's
// derivation.
//
// The classifier dates our NOTICING, so two finishes seconds apart are more
// likely one finish plus a stray repaint -- measured on 2 of 30 claude turns --
// than two turns. A report dates the FINISH: the agent says when its turn ended,
// a resting state is never re-asserted with a later timestamp, and two turn ends
// ten seconds apart are simply two turn ends. A dwell here would silently
// swallow the second one, and a swallowed turn end is a badge that never lights.
func TestTwoReportedTurnEndsInsideTheDwellBothDerive(t *testing.T) {
	now := time.UnixMilli(1789075200000)
	r := NewReports()

	if _, ok := r.Observe("%1", FormatReport(StateIdle, now.UnixMilli(), ""), "claude", now); !ok {
		t.Fatal("setup: the first report was not accepted")
	}
	if !r.Confirmed("%1", false) {
		t.Fatal("setup: a resting idle with no client connected derives immediately")
	}

	// A second genuine turn end, well inside the dwell of the first -- a third
	// of it, which on today's constant is the 5.0 s of the shorter measured
	// repaint. Written against the constant: if the dwell moves, so does this.
	gap := lateRepaintDwell / 3
	later := now.Add(gap)
	if _, ok := r.Observe("%1", FormatReport(StateIdle, later.UnixMilli(), ""), "claude", later); !ok {
		t.Fatal("setup: the second report was not accepted")
	}
	if !r.Confirmed("%1", false) {
		t.Fatalf("a second reported turn end %v after the first did not derive "+
			"finishedAt: the dwell applies to the classifier's stamp only", gap)
	}
}
