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
			"1;idle;" + at(-2*time.Second)},
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

// What TIMESTAMP a repair writes, which is a different question from whether it
// writes at all and is settled per event by internal/report's `dating` column.
//
// THE CASE THE TIMESTAMP EXISTS FOR cannot be told apart by the comparison. A
// re-assertion that sees `working;<older>` is looking at one of two panes and
// the bytes are identical:
//
//   - a LOST STOP -- the turn ended, nothing wrote it, and the daemon is still
//     holding that same working report. The repair must land.
//   - JITTER -- the Stop landed and every device badged, and then a working
//     write from earlier in the turn arrived late and overwrote the option. The
//     daemon refused it and still holds the idle; the option lies. A repair
//     dated now is strictly newer than that idle, so the daemon takes it,
//     finishedAt moves, and every device badges a finish it already cleared.
//
// Dating the write one millisecond after the report the writer READ makes it a
// compare-and-swap: it is newer than the standing report and nothing else, so
// the daemon takes it exactly when the standing report is still what the daemon
// holds. reassertion_integration_test.go runs both interleavings end to end
// against a real server and the real filter; this is the writer's own half,
// where the value on the wire can be read directly.
func TestAReassertionDatedFromTheStandingReportWritesItsTimestamp(t *testing.T) {
	standing := time.Now().Add(-2 * time.Second).UnixMilli()

	for _, tc := range []struct {
		name, event, stdin string
		wantStamp          func(before, after int64) (int64, string)
	}{
		{
			"idle_prompt dates from the standing report",
			"Notification", `{"hook_event_name":"Notification","notification_type":"idle_prompt"}`,
			func(int64, int64) (int64, string) {
				return standing + 1, "one millisecond after the report it read: the smallest stamp " +
					"the daemon's strictly-newer filter will take over that report, and one that " +
					"anything the daemon accepted since will outrank"
			},
		},
		{
			"quota_auto_resume_disabled dates from the standing report",
			"Notification", `{"hook_event_name":"Notification","notification_type":"quota_auto_resume_disabled"}`,
			func(int64, int64) (int64, string) {
				return standing + 1, "same event class as idle_prompt: it re-asserts a rest that " +
					"something else began, and it does not know when"
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &recordingTmux{standing: "1;working;" + strconv.FormatInt(standing, 10)}
			runReportWith(t, r, []string{"--agent", "claude", "--event", tc.event}, withStdin(tc.stdin))
			if r.sets != 1 {
				t.Fatalf("made %d writes, want 1: the standing report disagrees and is older", r.sets)
			}
			rep := wroteReport(t, r)
			want, why := tc.wantStamp(0, 0)
			if rep.Timestamp != want {
				t.Errorf("wrote timestamp %d, want %d -- %s", rep.Timestamp, want, why)
			}
			if rep.State != tmux.StateIdle {
				t.Errorf("wrote %q, want idle", rep.State)
			}
		})
	}
}

// opencode's session.idle is the one re-assertion dated from NOW, and it needs
// its own test because it is the exception the whole column exists to preserve.
//
// It can fire twice inside one resting period -- measured on opencode 1.18.30,
// on a turn that died at the provider -- which makes it a re-assertion. But the
// FIRST time it fires the turn really has just ended, and this event is the
// thing that knows. Dated from the standing report its finish would carry that
// turn's last working write, which is its last tool call: up to a minute early,
// on every opencode turn, every time. That is the certain regression this
// column refuses to trade for a rare race.
func TestOpencodesTurnEndIsStillDatedNow(t *testing.T) {
	standing := time.Now().Add(-30 * time.Second).UnixMilli()
	before := time.Now().UnixMilli()

	r := &recordingTmux{standing: "1;working;" + strconv.FormatInt(standing, 10)}
	runReportWith(t, r, []string{"--agent", "opencode", "--event", "session.idle"},
		withStdin(`{"type":"session.idle","properties":{"sessionID":"ses_root"}}`))
	after := time.Now().UnixMilli()

	if r.shows != 1 {
		t.Errorf("read the standing option %d times, want 1: it is still a re-assertion", r.shows)
	}
	if r.sets != 1 {
		t.Fatalf("made %d writes, want 1: the standing working disagrees and is older", r.sets)
	}
	rep := wroteReport(t, r)
	if rep.Timestamp < before || rep.Timestamp > after {
		t.Errorf("wrote timestamp %d, want one from this turn end itself (between %d and %d). "+
			"Dated from the standing report it would read %d -- thirty seconds before the turn "+
			"actually ended, which is a visible regression on every opencode turn",
			rep.Timestamp, before, after, standing+1)
	}
}

// A repair on a pane with no readable standing report is dated NOW, whatever
// its column says, because there is nothing to date from.
//
// It is the same three cases the write/no-write decision already treats as "no
// report" -- unset, another schema, dated past the future skew -- and the
// answer has to be the same one for the same reason: ParseReport refuses the
// value, so the DAEMON has no report for this pane either. Observe deletes its
// memory of a pane whose value it cannot parse, so the next value it sees is a
// first sight and is accepted whatever its timestamp. There is nothing to
// compare-and-swap against, and dating from a report nobody could read is not
// an option that exists.
func TestARepairWithNoReadableStandingReportIsDatedNow(t *testing.T) {
	for _, tc := range []struct {
		name, standing, why string
	}{
		{"an unset option", "",
			"the ordinary case for the first report a pane ever gets"},
		{"a value from another schema", "2;idle;" + strconv.FormatInt(time.Now().UnixMilli(), 10),
			"unreadable is unreadable however new it claims to be"},
		{"a value more than five seconds ahead",
			"1;working;" + strconv.FormatInt(time.Now().Add(30*time.Second).UnixMilli(), 10),
			"ParseReport discards it and so does the daemon; this write is what repairs an " +
				"option some other clock poisoned"},
		{"a truncated value", "1;idle",
			"three fields is the minimum and this has two"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := time.Now().UnixMilli()
			r := &recordingTmux{standing: tc.standing}
			runReportWith(t, r, []string{"--agent", "claude", "--event", "Notification"},
				withStdin(`{"hook_event_name":"Notification","notification_type":"idle_prompt"}`))
			after := time.Now().UnixMilli()
			if r.sets != 1 {
				t.Fatalf("made %d writes over standing %q, want 1 -- %s", r.sets, tc.standing, tc.why)
			}
			if rep := wroteReport(t, r); rep.Timestamp < before || rep.Timestamp > after {
				t.Errorf("wrote timestamp %d, want one from now (between %d and %d): there is no "+
					"readable report to date from, and the daemon has none either -- %s",
					rep.Timestamp, before, after, tc.why)
			}
		})
	}
}

// A read that FAILED is the same answer again, and it has its own test because
// it is a different branch: tmux exits 1 on an unset user option, so this is
// what every pane looks like before its first report.
func TestARepairWhoseReadFailsIsDatedNow(t *testing.T) {
	before := time.Now().UnixMilli()
	r := &recordingTmux{err: errTmuxFailed}
	runReportWith(t, r, []string{"--agent", "claude", "--event", "Notification"},
		withStdin(`{"hook_event_name":"Notification","notification_type":"idle_prompt"}`))
	after := time.Now().UnixMilli()
	if r.sets != 1 {
		t.Fatalf("made %d writes, want 1", r.sets)
	}
	if rep := wroteReport(t, r); rep.Timestamp < before || rep.Timestamp > after {
		t.Errorf("wrote timestamp %d, want one from now (between %d and %d)", rep.Timestamp, before, after)
	}
}

// wroteReport is the last value this stub was asked to store, parsed.
//
// Parsed rather than compared as a string, because the claim in this file is
// about the TIMESTAMP: a test that compared whole values would need to build
// the activity field and the version prefix as well, and would then be
// asserting against its own copy of FormatReport.
func wroteReport(t *testing.T, r *recordingTmux) tmux.Report {
	t.Helper()
	last := r.lastSet()
	if last == nil {
		t.Fatal("nothing was written")
	}
	rep, ok := tmux.ParseReport(last[len(last)-1], time.Now())
	if !ok {
		t.Fatalf("wrote %q, which is not a readable report", last[len(last)-1])
	}
	return rep
}
