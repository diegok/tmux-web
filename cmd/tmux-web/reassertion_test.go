package main

import (
	"strconv"
	"testing"
	"time"

	"github.com/diegok/tmux-web/internal/tmux"
)

// What a re-assertion does about the report already on the pane, when the two
// of them disagree about WHICH IS THE NEWER NEWS.
//
// events_test.go's TestEdgeAndReassertion settles the state word: a standing
// report whose state matches suppresses the write, and one whose state differs
// is repaired. Every fixture there is dated two days into the past, so none of
// them says anything about the timestamp, and until this file the writer did
// not look at it.
//
// The writer and the daemon had two different definitions of "standing". The
// daemon's is the last report it ACCEPTED, and it refuses anything not strictly
// newer than that. The writer's was whatever byte string the option happened to
// hold. Those diverge on exactly the interleaving the ordering filter exists to
// absorb -- two fire-and-forget writes landing out of order -- and the half the
// writer can do something about is the half where THE OPTION IS NEWER THAN THE
// REPORT THIS EVENT WOULD WRITE. There the old rule overwrote fresh news with
// stale news: the daemon refuses the write for being older, so nothing moves
// today, and the pane is left holding a resting report that the next first
// sight -- a daemon restart, a tmux-server generation change, a pane that left
// `keep` and came back -- reads as a finish and badges. See
// reassertion_integration_test.go for that sequence end to end.
func TestAReassertionComparesTheTimestampNotOnlyTheState(t *testing.T) {
	// Dated against the same clock runReport stamps its own write from, a few
	// microseconds before it does. Two seconds is far larger than that gap and
	// far inside ParseReport's five-second future skew, so "before this event"
	// and "after this event" are unambiguous in both directions.
	at := func(d time.Duration) string {
		return strconv.FormatInt(time.Now().Add(d).UnixMilli(), 10)
	}

	const idlePrompt = `{"hook_event_name":"Notification","notification_type":"idle_prompt"}`

	for _, tc := range []struct {
		name     string
		standing string
		wantSets int
		why      string
	}{
		{
			"a standing working from before this event is repaired",
			"1;working;" + at(-2*time.Second), 1,
			"the repair is the whole reason idle_prompt is mapped at all: a Stop that never landed " +
				"leaves the pane on working until the 60-second expiry, and with no client connected " +
				"the expiry hands it to a classifier with no screen to read",
		},
		{
			"a standing working from after this event is left alone",
			"1;working;" + at(2*time.Second), 0,
			"the pane holds news this event predates -- the turn started again while this " +
				"notification's process was descheduled. Writing puts an idle the daemon will refuse " +
				"onto a pane that is working, and the next first sight reads it as a finish",
		},
		{
			"a standing blocked from after this event is left alone",
			"1;blocked;" + at(2*time.Second), 0,
			"same rule, and the state that would be worst to lose: blocked never expires, so a " +
				"stale idle left on the option outlives the dialog it was written over",
		},
		{
			"a standing idle from after this event is left alone twice over",
			"1;idle;" + at(2*time.Second), 0,
			"both clauses agree here; the row is what keeps a mutant that swapped the two apart",
		},
		{
			"a standing report more than five seconds ahead is no report at all",
			"1;working;" + at(30*time.Second), 1,
			"ParseReport discards it, so the DAEMON has no report for this pane either. The writer " +
				"answers with the same function rather than a second opinion, and the write is what " +
				"repairs an option some other clock poisoned",
		},
		{
			"an unset option is no report at all",
			"", 1,
			"tmux answers an unset user option with exit 1, and Run returns \"\" to that. It is the " +
				"ordinary case for the first report a pane ever gets",
		},
		{
			"a value from another schema is no report at all",
			"2;idle;" + at(2*time.Second), 1,
			"unreadable is unreadable however new it claims to be: a timestamp this writer trusts " +
				"has to have come out of a report this writer could read",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &recordingTmux{standing: tc.standing}
			runReportWith(t, r, []string{"--agent", "claude", "--event", "Notification"},
				withStdin(idlePrompt))
			if r.shows != 1 {
				t.Errorf("read the standing option %d times, want exactly 1: the decision is made "+
					"from one show-options fork and nothing else", r.shows)
			}
			if r.sets != tc.wantSets {
				t.Errorf("made %d writes over standing %q, want %d -- %s",
					r.sets, tc.standing, tc.wantSets, tc.why)
			}
			if tc.wantSets == 1 {
				if got := wroteState(t, r); got != tmux.StateIdle {
					t.Errorf("wrote %q, want idle", got)
				}
			}
		})
	}
}

// A read that failed is not evidence that the pane agrees.
//
// It has its own test because it is the ordinary case rather than the
// exceptional one: tmux answers `show -p -v` on an unset user option with exit
// 1 and "invalid option" (measured on 3.7b), which is what every pane looks
// like before the first report lands on it. The integration test proves that
// against a real server; this proves the branch on its own, so that a mutant
// which treats an unreadable answer as agreement fails with a sentence about
// the rule instead of with a fixture's own read blowing up two steps later.
func TestAReassertionWhoseReadFailsStillWrites(t *testing.T) {
	r := &recordingTmux{err: errTmuxFailed}
	runReportWith(t, r, []string{"--agent", "claude", "--event", "Notification"},
		withStdin(`{"hook_event_name":"Notification","notification_type":"idle_prompt"}`))
	if r.shows != 1 {
		t.Errorf("read the standing option %d times, want 1", r.shows)
	}
	if r.sets != 1 {
		t.Errorf("made %d writes, want 1: a pane whose standing report could not be read is a "+
			"pane with no standing report, and suppressing here would silence the first report "+
			"a freshly started agent ever makes", r.sets)
	}
}

// The boundary between "older than this event" and "not older", which is the
// one row of the rule no test that goes through runReport can reach: the
// re-assertion's own stamp is taken inside it, from the clock, and a fixture
// cannot name it.
//
// It is a real millisecond and not a rounding detail. Two hook processes
// starting inside one millisecond is ordinary -- claude spawns a fresh process
// per hook and several fire together at a turn boundary -- and the reader's
// filter is STRICTLY newer, so a write stamped at the same millisecond as the
// standing report is one the daemon will refuse. Writing it swaps the option's
// contents for a value no reader will take, which is the stale-first-sight
// failure this rule exists to close, for nothing in return.
func TestTheStandingReportSupersedesFromTheSameMillisecondOn(t *testing.T) {
	const ms = 1789075200000

	for _, tc := range []struct {
		name string
		rep  tmux.Report
		want bool
	}{
		{"a disagreeing report from the millisecond before",
			tmux.Report{State: tmux.StateWorking, Timestamp: ms - 1}, false},
		{"a disagreeing report from the same millisecond",
			tmux.Report{State: tmux.StateWorking, Timestamp: ms}, true},
		{"a disagreeing report from the millisecond after",
			tmux.Report{State: tmux.StateWorking, Timestamp: ms + 1}, true},
		// The state clause, at the same three points, so that neither clause
		// can be dropped without a row noticing.
		{"an agreeing report from the millisecond before",
			tmux.Report{State: tmux.StateIdle, Timestamp: ms - 1}, true},
		{"an agreeing report from the same millisecond",
			tmux.Report{State: tmux.StateIdle, Timestamp: ms}, true},
		{"an agreeing report from the millisecond after",
			tmux.Report{State: tmux.StateIdle, Timestamp: ms + 1}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := reportSupersedes(tc.rep, tmux.StateIdle, ms); got != tc.want {
				t.Errorf("reportSupersedes(%+v, idle, %d) = %v, want %v", tc.rep, ms, got, tc.want)
			}
		})
	}
}

// The other half of the rule, and the half that keeps the badges: AN EDGE NEVER
// READS, so nothing about the standing report's timestamp can suppress one.
//
// This is the case the edge/re-assertion split was scoped around. A second
// turn's Stop, on a pane whose turn-start working never landed, is a genuine
// finish that must badge -- and the two standing reports below are the two ways
// that pane can look: the previous turn's own idle still standing, and a report
// dated AFTER this Stop's stamp, which is what a delayed write from the turn
// that was lost leaves behind. Both write.
func TestAGenuineFinishIsNotSuppressedByAnyStandingReport(t *testing.T) {
	at := func(d time.Duration) string {
		return strconv.FormatInt(time.Now().Add(d).UnixMilli(), 10)
	}

	for _, tc := range []struct {
		name     string
		standing string
	}{
		{"the previous turn's idle, never cleared because this turn's start was lost",
			"1;idle;" + at(-2 * time.Second)},
		{"a report dated after this Stop's own stamp", "1;working;" + at(2*time.Second)},
		{"a report dated after this Stop's own stamp, and resting", "1;idle;" + at(2*time.Second)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &recordingTmux{standing: tc.standing}
			runReportWith(t, r, []string{"--agent", "claude", "--event", "Stop"},
				withStdin(`{"hook_event_name":"Stop","background_tasks":[]}`))
			if r.shows != 0 {
				t.Errorf("an edge read the standing option %d times, want 0: a turn end is a "+
					"transition the agent is telling us about, and it costs no fork", r.shows)
			}
			if r.sets != 1 {
				t.Fatalf("made %d writes over standing %q, want 1: this turn's badge is the whole "+
					"point of the event", r.sets, tc.standing)
			}
			if got := wroteState(t, r); got != tmux.StateIdle {
				t.Errorf("wrote %q, want idle", got)
			}
		})
	}
}
