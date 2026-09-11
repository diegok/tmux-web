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
