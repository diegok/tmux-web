package tmux

import (
	"strings"
	"testing"
	"time"
)

// The states a pane can be in. "" is not a state: it means nothing computed one.
func TestClassifierWorkingAndIdle(t *testing.T) {
	c := NewClassifier()
	now := time.Unix(0, 0)

	// First sight reports working -- there is no previous capture to compare,
	// and the design accepts a settle rather than guessing. What it must NOT do
	// is stamp a finish edge when it later settles.
	st := c.Observe("%1", "screen A", now, false)
	if st.State != StateWorking {
		t.Fatalf("first observation = %q, want working", st.State)
	}
	if st.FinishedAt != 0 {
		t.Fatal("a first observation must not stamp a finish edge")
	}

	// Two identical captures settle to idle.
	now = now.Add(1500 * time.Millisecond)
	c.Observe("%1", "screen A", now, false)
	now = now.Add(1500 * time.Millisecond)
	st = c.Observe("%1", "screen A", now, false)
	if st.State != StateIdle {
		t.Fatalf("state after two identical captures = %q, want idle", st.State)
	}
	// ... and the run began at first sight, so it produced no finish edge.
	if st.FinishedAt != 0 {
		t.Fatal("a working run that began at first sight must not stamp a finish edge; " +
			"otherwise every daemon restart lights up a done badge on every device")
	}

	// A real change, then stillness, IS a finish edge.
	now = now.Add(1500 * time.Millisecond)
	c.Observe("%1", "screen B", now, false)
	now = now.Add(1500 * time.Millisecond)
	c.Observe("%1", "screen B", now, false)
	now = now.Add(1500 * time.Millisecond)
	st = c.Observe("%1", "screen B", now, false)
	if st.State != StateIdle || st.FinishedAt == 0 {
		t.Fatalf("observed work then stillness = %+v, want idle with a finish edge", st)
	}
	// The unit is unix MILLISECONDS, stamped at the poll that observed the
	// settle. The browser compares this against a localStorage `seen` value, so
	// seconds or nanoseconds here make every badge decision wrong.
	if st.FinishedAt != now.UnixMilli() {
		t.Fatalf("finish edge = %d, want %d (unix ms of the settling poll)", st.FinishedAt, now.UnixMilli())
	}
	first := st.FinishedAt

	// A SECOND run must stamp a NEW edge. Guarding the stamp with
	// "finishedAt == 0" makes the done badge work exactly once per pane for
	// the life of the daemon, and the earlier assertions cannot see it.
	now = now.Add(1500 * time.Millisecond)
	c.Observe("%1", "screen C", now, false)
	now = now.Add(1500 * time.Millisecond)
	c.Observe("%1", "screen C", now, false)
	now = now.Add(1500 * time.Millisecond)
	st = c.Observe("%1", "screen C", now, false)
	if st.FinishedAt <= first {
		t.Fatalf("second finish edge = %d, want later than the first (%d): "+
			"FinishedAt is the LAST working->idle edge, not the first", st.FinishedAt, first)
	}

	// ... but an idle pane that keeps sitting still does not keep re-stamping,
	// or the badge could never be cleared by looking at it.
	now = now.Add(1500 * time.Millisecond)
	again := c.Observe("%1", "screen C", now, false)
	if again.FinishedAt != st.FinishedAt {
		t.Fatal("a pane that is merely still must not re-stamp its finish edge")
	}
	// An idle pane also stays idle rather than lapsing back to working.
	if again.State != StateIdle {
		t.Fatalf("a pane still sitting still = %q, want idle", again.State)
	}

	// A new run does not erase the last edge. FinishedAt is a fact about the
	// pane's history, not about its current state: if it dropped back to 0 the
	// moment the screen moved again, a done badge would clear for a reason
	// other than its owner looking at the pane.
	now = now.Add(1500 * time.Millisecond)
	resumed := c.Observe("%1", "screen D", now, false)
	if resumed.State != StateWorking {
		t.Fatalf("a changed capture = %q, want working", resumed.State)
	}
	if resumed.FinishedAt != again.FinishedAt {
		t.Fatalf("a resumed run reports FinishedAt %d, want the last edge %d",
			resumed.FinishedAt, again.FinishedAt)
	}
}

// Each pane settles on its own evidence. Sharing one hash across panes would let
// a busy neighbour hold an idle pane's badge open, or vice versa.
func TestClassifierKeepsPanesIndependent(t *testing.T) {
	c := NewClassifier()
	now := time.Unix(0, 0)

	c.Observe("%1", "still", now, false)
	now = now.Add(1500 * time.Millisecond)
	c.Observe("%1", "still", now, false)
	c.Observe("%2", "churn 1", now, false)

	now = now.Add(1500 * time.Millisecond)
	if st := c.Observe("%1", "still", now, false); st.State != StateIdle {
		t.Fatalf("%%1 = %q, want idle: its own captures never changed", st.State)
	}
	if st := c.Observe("%2", "churn 2", now, false); st.State != StateWorking {
		t.Fatalf("%%2 = %q, want working: its own capture changed", st.State)
	}
}

// The whole capture is the signal, not a slice of it. The design considered
// hashing only the tail of the screen and rejected it; hashing only the head
// would be the same mistake mirrored. Both ends are pinned, because a mutant
// that drops either one passes a test that only exercises the other.
func TestClassifierHashesTheWholeCapture(t *testing.T) {
	middle := "\n" + strings.Repeat("unchanged line\n", 60)
	for _, tc := range []struct{ name, before, after string }{
		{"the top line changed", "top A" + middle + "bottom", "top B" + middle + "bottom"},
		{"the bottom line changed", "top" + middle + "bottom A", "top" + middle + "bottom B"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := NewClassifier()
			now := time.Unix(0, 0)

			c.Observe("%1", tc.before, now, false)
			now = now.Add(1500 * time.Millisecond)
			c.Observe("%1", tc.before, now, false)
			now = now.Add(1500 * time.Millisecond)
			if st := c.Observe("%1", tc.before, now, false); st.State != StateIdle {
				t.Fatalf("an unchanging screen = %q, want idle", st.State)
			}

			now = now.Add(1500 * time.Millisecond)
			if st := c.Observe("%1", tc.after, now, false); st.State != StateWorking {
				t.Fatalf("%s, but the pane read %q: the whole capture is the signal, "+
					"not a slice of it", tc.name, st.State)
			}
		})
	}
}

func TestClassifierForgetsClosedPanes(t *testing.T) {
	c := NewClassifier()
	now := time.Unix(0, 0)

	// Both panes are given a REAL change first, so both are mid-run with a
	// finish edge to earn. That is what makes "kept" and "forgotten"
	// distinguishable: asserting only that each pane reads working afterwards
	// -- or only that one entry survived -- passes just as happily when Retain
	// kept the WRONG pane, because a forgotten pane also reports working.
	c.Observe("%1", "a1", now, false)
	c.Observe("%2", "b1", now, false)
	now = now.Add(1500 * time.Millisecond)
	c.Observe("%1", "a2", now, false)
	c.Observe("%2", "b2", now, false)

	c.Retain([]string{"%2"})

	// %2 was retained mid-run, so it settles two polls from here and stamps the
	// edge its earlier change earned.
	now = now.Add(1500 * time.Millisecond)
	if st := c.Observe("%2", "b2", now, false); st.State != StateWorking {
		t.Fatalf("retained pane %%2 = %q, want its run to have continued", st.State)
	}
	now = now.Add(1500 * time.Millisecond)
	st := c.Observe("%2", "b2", now, false)
	if st.State != StateIdle {
		t.Fatalf("retained pane %%2 = %q, want idle", st.State)
	}
	if st.FinishedAt != now.UnixMilli() {
		t.Fatalf("retained pane %%2 stamped %d, want %d: Retain dropped the run it was "+
			"told to keep", st.FinishedAt, now.UnixMilli())
	}

	// %1 was dropped, so the identical sequence produces NO edge: it is first
	// sight again, which is how we know it was genuinely forgotten rather than
	// resumed.
	now = now.Add(1500 * time.Millisecond)
	if st := c.Observe("%1", "a2", now, false); st.State != StateWorking {
		t.Fatalf("dropped pane %%1 = %q, want first-sight working", st.State)
	}
	now = now.Add(1500 * time.Millisecond)
	c.Observe("%1", "a2", now, false)
	now = now.Add(1500 * time.Millisecond)
	if st := c.Observe("%1", "a2", now, false); st.FinishedAt != 0 {
		t.Fatalf("dropped pane %%1 stamped %d, want no edge: Retain kept the pane it was "+
			"told to forget", st.FinishedAt)
	}
}

// Retain with nothing to keep is how the poller resets when no browser client is
// connected: the next connection settles from scratch instead of resuming a run
// whose intervening polls nobody captured.
func TestClassifierRetainNothingForgetsEverything(t *testing.T) {
	c := NewClassifier()
	now := time.Unix(0, 0)
	c.Observe("%1", "a", now, false)
	now = now.Add(1500 * time.Millisecond)
	c.Observe("%1", "b", now, false) // a real change: this run could stamp an edge

	c.Retain(nil)

	// Back to first sight: three identical captures settle with no edge.
	now = now.Add(1500 * time.Millisecond)
	if st := c.Observe("%1", "b", now, false); st.State != StateWorking {
		t.Fatalf("after a reset, %%1 = %q, want first-sight working", st.State)
	}
	now = now.Add(1500 * time.Millisecond)
	c.Observe("%1", "b", now, false)
	now = now.Add(1500 * time.Millisecond)
	st := c.Observe("%1", "b", now, false)
	if st.State != StateIdle {
		t.Fatalf("after a reset, %%1 = %q, want idle", st.State)
	}
	if st.FinishedAt != 0 {
		t.Fatal("a reset classifier must not carry the pre-reset run's change into a finish edge")
	}
}

// A held permission dialog must not stamp a finish edge.
//
// This is the one thing the classifier cannot see for itself. A box waiting on
// the owner is byte-identical poll after poll, which is precisely the shape
// stillness has, so a working agent that raises one draws a single change and
// then settles -- stamping a working->idle edge for a pane that was never idle
// on the wire. The damage lands after the box is answered: the agent resumes
// working and that stale edge is still the newest, so every device that has not
// viewed the pane shows `done` on an agent that is mid-run. That is the same
// badge-integrity failure a false `blocked` is, arrived at from the other side.
//
// The settled *state* is not the bug and is asserted as idle here on purpose:
// the caller overrides it with blocked, and an implementation that suppressed
// the stamp by holding `still` at zero would be reporting churn that is not
// there.
func TestClassifierBlockedDoesNotStampAFinishEdge(t *testing.T) {
	c := NewClassifier()
	now := time.Unix(0, 0)
	step := func() { now = now.Add(1500 * time.Millisecond) }

	// A real run, so there is an edge to be wrongly earned: without the change
	// on the second poll everChanged stays false and this test would pass
	// against a classifier that ignores `blocked` entirely.
	c.Observe("%1", "working 1", now, false)
	step()
	c.Observe("%1", "working 2", now, false)

	// The box goes up and holds byte-identical.
	step()
	c.Observe("%1", "a dialog", now, true)
	step()
	c.Observe("%1", "a dialog", now, true)
	step()
	st := c.Observe("%1", "a dialog", now, true)
	if st.State != StateIdle {
		t.Errorf("a held dialog settles to %q, want idle: only the edge is wrong, "+
			"the state is the caller's to override", st.State)
	}
	if st.FinishedAt != 0 {
		t.Fatalf("a held dialog stamped a finish edge at %d: the pane was never "+
			"idle on the wire, and this edge lights `done` on an agent that is "+
			"mid-run as soon as the owner answers", st.FinishedAt)
	}
	// Held longer, in case the suppression were only the settling poll's. The
	// state is asserted again, and it is the assertion that catches the
	// tempting shortcut: suppressing the stamp by holding `still` at zero also
	// works, and then a box held for a third poll reads *working* -- churn
	// invented out of a screen that has not moved a byte since.
	step()
	st = c.Observe("%1", "a dialog", now, true)
	if st.FinishedAt != 0 {
		t.Fatalf("a dialog held one poll longer stamped %d", st.FinishedAt)
	}
	if st.State != StateIdle {
		t.Errorf("a dialog held a third poll = %q, want idle: the screen has not "+
			"changed, so nothing may report churn", st.State)
	}

	// Answered: the agent resumes, and the finish that follows is a real one.
	// Suppression is a delay, not a forfeit -- a mutant that suppressed the
	// stamp for the wrong state loses this edge and never gets it back.
	step()
	c.Observe("%1", "back to work", now, false)
	step()
	c.Observe("%1", "back to work", now, false)
	step()
	st = c.Observe("%1", "back to work", now, false)
	if st.State != StateIdle || st.FinishedAt != now.UnixMilli() {
		t.Fatalf("the finish after the box was answered = %+v, want idle stamped %d",
			st, now.UnixMilli())
	}
}

// Changed is the RAW FACT -- this capture differed from the previous one -- and
// it is not the verdict. Evidence rule 1 rests on the difference: State says
// working on a first sight and on every poll before settleAfter, which is
// precisely what settleAfter exists to declare is noise, so a rule keyed on the
// verdict would drop a true blocked report on an ordinary settle.
func TestChangedIsTheRawFactAndNotTheVerdict(t *testing.T) {
	c := NewClassifier()
	now := time.Unix(0, 0)

	// A first sight has nothing to compare against, so nothing changed -- even
	// though the verdict is working. This is the poll that matters most: it is
	// the first poll of every blocked report's evidence.
	if st := c.Observe("%1", "frame 1", now, false); st.Changed || st.State != StateWorking {
		t.Errorf("a first sight = %+v, want working with Changed false", st)
	}
	if st := c.Observe("%1", "frame 2", now, false); !st.Changed || st.State != StateWorking {
		t.Errorf("a differing capture = %+v, want working with Changed true", st)
	}
	// Identical captures that have not yet settled: still working, still no
	// change. settleAfter-1 of them, written against settleAfter so that they
	// stay put if it moves.
	for i := 1; i < settleAfter; i++ {
		st := c.Observe("%1", "frame 2", now, false)
		if st.State != StateWorking {
			t.Fatalf("poll %d after the change = %q, want working: without this the test "+
				"cannot tell the verdict from the fact", i, st.State)
		}
		if st.Changed {
			t.Errorf("poll %d after the change reported Changed on an identical capture", i)
		}
	}
	if st := c.Observe("%1", "frame 2", now, false); st.Changed || st.State != StateIdle {
		t.Errorf("the settling poll = %+v, want idle with Changed false", st)
	}
}
