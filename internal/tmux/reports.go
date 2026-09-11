package tmux

import "time"

// workingTTL is how long a transient working report stands without being
// re-asserted.
//
// working asserts that something is happening NOW and must be re-asserted; a
// crashed agent must not show as busy. blocked and idle are resting: the agent
// said it has stopped, and by definition nothing further happens until the user
// acts, so there is nothing to re-assert and they do not expire on a clock.
// That split is what dissolves the tension a single TTL cannot resolve.
//
// 60 seconds is a GUESS and is open question 2 in the design: nobody has
// measured the distribution of gaps between events during real work. Expiring
// early is the cheap direction but it is not free -- its cost is a false idle
// on a quiet screen, because a long silent tool call with no sub-events is
// exactly the gap this bounds, and v2's own caveat is that a quiet agent reads
// idle. What keeps that from being worse: an authority change stamps no
// finishedAt, so the row is wrong and the badge is not. What would settle the
// number: the inter-event gap distribution on all three agents, measured
// against a single long tool call emitting no sub-events, with no client
// connected.
const workingTTL = 60 * time.Second

// NIdle is the idle verification window, in polls.
//
// Expressed against settleAfter and never written as 4. Two independent
// arguments arrive at settleAfter + 2 and both are worth keeping:
//
// Derived: Observe returns idle only at the settleAfter-th IDENTICAL
// comparison, so a window that must contain one repaint needs settleAfter + 2
// polls -- N_idle >= settleAfter + R + 1, where R is the number of polls inside
// the window whose capture differs from the one before it.
//
// Measured: R = 1, in every one of 1,440 exact-timing replays of the 1.5s grid
// at every phase offset across 88 turns on three agents. Polls-to-settle was 3
// or 4 and never 5, and every turn on every agent had some phase at which it
// took 4. The mechanism is that the agent's turn-end event fires 7-52 ms BEFORE
// its last repaint -- the report always comes first -- so a poll can be spent
// on the pre-final screen.
//
// It is not padded, and that is deliberate. A wider window accepts more
// mid-turn stillness as corroboration: on 4 of 88 measured turns a screen sat
// still long enough while the agent waited on the model for the classifier to
// report idle BEFORE the turn ended. So the verdict this rule accepts means
// "the screen was still for settleAfter polls", not "the turn ended", and those
// are different claims. Widening N trades one failure for another.
const NIdle = settleAfter + 2

// Reports decides, per pane, whether the standing @wterm_agent value is the
// authority for that pane's state.
//
// Like Classifier it is pure -- no clock, no I/O, `now` is a parameter -- and
// it is owned by the poll goroutine.
type Reports struct {
	panes map[string]*reportState
}

// reportState is the report currently in force for one pane.
//
// It holds the report and not merely its timestamp, and that is what makes the
// ordering rule implementable: the standing option holds the same value poll
// after poll, so "refuse anything not strictly newer" applied to the raw value
// would drop the pane's own state on every second poll. What is refused is a
// value OLDER than the one in force, and what stands in its place is this.
type reportState struct {
	accepted Report
	// The idle verification window for `accepted`, reset whenever a newer
	// report is accepted: a new report is a new claim and earns its own window.
	windowPolls int  // captures spent inside the window
	verified    bool // the classifier agreed at some poll inside it
	// rejected is the timestamp of the newest report the evidence overturned.
	//
	// It is what stops a dropped report coming straight back: the standing
	// option still holds the same value, and Observe reads it again on the very
	// next poll. Minimal on purpose -- Task 9 gives the rejection its full
	// semantics across all three evidence rules; all this does is refuse
	// anything not strictly newer than what was overturned.
	rejected int64
}

// NewReports returns a report memory that has seen nothing.
func NewReports() *Reports { return &Reports{panes: make(map[string]*reportState)} }

// Observe reads one pane's standing @wterm_agent value and reports which
// report, if any, is in force for it.
//
// command is pane_current_command: if it is no longer a known agent the agent
// exited, and the report is dropped unconditionally. That catches the crash
// case, which is also the case a clock was supposed to catch.
func (r *Reports) Observe(paneID, raw, command string, now time.Time) (Report, bool) {
	if KnownAgent(command) == "" {
		delete(r.panes, paneID)
		return Report{}, false
	}
	parsed, ok := ParseReport(raw, now)
	if !ok {
		// Unset, or a value we did not write. Both mean no report, and both
		// must clear what we accepted -- `tmux set -p -u @wterm_agent` is the
		// documented escape hatch for a stuck report, and a daemon that went on
		// serving its own memory would make that escape hatch do nothing.
		delete(r.panes, paneID)
		return Report{}, false
	}
	st := r.panes[paneID]
	if st == nil {
		// A first sight. Accepted whatever it is: the option holds the last
		// write that landed and the daemon has no better information. Accepted
		// by this filter is not the same as believed -- a resting report still
		// has to earn its way past the evidence rules (Tasks 7 and 8).
		//
		// Including the first sight after claude -> zsh -> claude, whose value
		// the PREVIOUS agent in this pane wrote. That is deliberate and
		// bounded; a tombstone here would not survive Retain, which the poller
		// calls one line later with the shell pane absent from `keep`.
		st = &reportState{}
		r.panes[paneID] = st
	}
	if parsed.Timestamp > st.accepted.Timestamp {
		st.accepted = parsed
		// A newer report is a new claim about the screen, so it is checked
		// afresh. Inheriting the previous report's verdict would let one
		// verified turn end vouch for every turn end after it.
		st.windowPolls, st.verified = 0, false
	}
	if st.accepted.Timestamp <= st.rejected {
		// The evidence overturned this report and the option still holds it.
		return Report{}, false
	}
	// Anything not newer is either the same report we already hold or an
	// out-of-order write -- ordinary scheduling jitter between two
	// fire-and-forget writes, not a broken integration -- and either way what
	// stands is st.accepted.
	if st.accepted.State == StateWorking &&
		now.Sub(time.UnixMilli(st.accepted.Timestamp)) > workingTTL {
		return Report{}, false
	}
	return st.accepted, true
}

// NeedsScreen reports whether this pane must be captured despite a report being
// in force: only for a resting idle whose window is still open.
//
// Every other report skips the capture entirely, which is the win of the
// feature. A working report is re-asserted by its own writer and expires on a
// clock; blocked is evidence rules 1 and 2, which are Task 8's.
func (r *Reports) NeedsScreen(paneID string) bool {
	st := r.panes[paneID]
	if st == nil || st.accepted.State != StateIdle {
		return false
	}
	// Open until the classifier agrees. It cannot still be open past NIdle
	// polls: the poll that reaches the count rejects the report, and a rejected
	// report is not in force, so this is never asked about one.
	return !st.verified
}

// Corroborate feeds one classifier verdict into the open window and reports
// whether the report survives.
//
// The window closes at the first idle verdict rather than running out the
// count: the report is accepted, the derivation happens, and the captures stop.
// It is dropped only when the classifier said working for the whole of it.
//
// The idle verdict CORROBORATES, it does not verify. On 4 of 88 measured turns
// the screen sat still while the agent waited on the model and the classifier
// said idle before the turn had ended. This rule is a backstop with a measured
// leak, not a proof.
func (r *Reports) Corroborate(paneID string, idle bool) bool {
	st := r.panes[paneID]
	if st == nil {
		return false
	}
	st.windowPolls++
	if idle {
		st.verified = true
		return true
	}
	if st.windowPolls >= NIdle {
		r.reject(paneID, st.accepted.Timestamp)
		return false
	}
	return true
}

// Confirmed reports whether a resting idle report may derive finishedAt.
//
// With no client connected there is nothing to verify and the derivation is
// immediate, which is the case the app exists for. With one, it waits for the
// window: suppressed while pending rather than stamped and retracted, because a
// done badge that has landed on three devices does not un-land.
func (r *Reports) Confirmed(paneID string, connected bool) bool {
	st := r.panes[paneID]
	if st == nil {
		return false
	}
	if !connected {
		return true
	}
	return st.verified
}

// reject records that the evidence overturned the report in force, so that
// reading the same standing value again does not put it back.
func (r *Reports) reject(paneID string, ts int64) {
	if st := r.panes[paneID]; st != nil && ts > st.rejected {
		st.rejected = ts
	}
}

// Retain forgets every pane that is not in keep, on the same terms as
// Classifier.Retain: the caller passes the panes that are known agents right
// now, and retaining nothing resets the memory entirely.
func (r *Reports) Retain(keep []string) {
	if len(keep) == 0 {
		clear(r.panes)
		return
	}
	set := make(map[string]struct{}, len(keep))
	for _, id := range keep {
		set[id] = struct{}{}
	}
	for id := range r.panes {
		if _, ok := set[id]; !ok {
			delete(r.panes, id)
		}
	}
}
