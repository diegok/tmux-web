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

// NBlocked is how many consecutive settled polls with no registered form on
// screen drop a reported blocked.
//
// A DIFFERENT number from NIdle, and nobody should carry 4 across. Rule 1 drops
// anything that moves before rule 2 sees it, so rule 2 never has to absorb a
// repaint and needs no R: its floor is settleAfter + 1.
//
// The value is a guess of the same standing as workingTTL -- open question 3 in
// the design -- and measuring it needs the dialog screens question 10 is also
// waiting on. Too small and a true blocked is dropped on a slow-repainting
// screen; too large and the overnight case takes longer to correct itself.
// Expressed against settleAfter, in one place, so that changing it is one line.
const NBlocked = settleAfter + 1

// Reports decides, per pane, whether the standing @tmux_web_agent value is the
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
	// settledPolls is rule 2's counter for a reported blocked: consecutive
	// polls whose capture did not move and showed no registered form. Reset by
	// a form on screen, and reset with the window whenever a newer report is
	// accepted.
	//
	// Its own field rather than windowPolls reused. A report is idle or
	// blocked, never both, so one counter would work -- and would make the two
	// constants that count it look like one number, which is the mistake this
	// whole task exists to keep from being made again.
	settledPolls int
	// rejected is the timestamp of the report the evidence overturned, or 0 for
	// none.
	//
	// It is what stops a dropped report coming straight back: the standing
	// option still holds the same value, and Observe reads it again on the very
	// next poll. Without a memory of the drop the report is re-accepted the
	// moment the screen settles again, the pane stays on the report path
	// forever, and the screen grammars never run on it again -- so a dialog
	// raised after a drop would never be seen.
	//
	// ONE SLOT, not a set, and one semantic for all three evidence rules: the
	// option holds exactly one value, so the only report that can be re-seen is
	// the current one. Whichever rule condemned it, the report in force is
	// re-rejected WITHOUT re-evaluating the evidence for as long as its own
	// timestamp matches this -- the evidence that condemned it was a screen
	// that has since moved on, and re-running the test against a screen that
	// has since settled is exactly how a dropped report comes back to life.
	//
	// Do not make it provisional. Revision 4 of the design cleared a rule-3
	// rejection "the moment the classifier reports idle" and revision 5
	// withdrew that on two measurements: the case it was built for is
	// measurably empty (polls-to-settle was 3 or 4 in every one of 1,440 phase
	// replays and never 5), and the trigger is not safe (4 of 88 measured turns
	// went still while waiting on the model, so mid-turn stillness on a root
	// that is genuinely working would clear a rejection that was CORRECT --
	// resurrecting a subagent's false idle and landing the false done badge the
	// rule exists to prevent).
	//
	// What a permanent-sounding rejection actually costs is much less than it
	// sounds. A rejected report hands the pane back to the classifier, and the
	// classifier is an authority that can stamp a finish: for rule 3 to have
	// fired the screen must have churned through the whole window, which sets
	// everChanged, so when it does settle Observe stamps finishedAt = now in
	// the ordinary way. A wrongly-dropped true turn end does not lose its badge
	// -- it gets one dated by when the daemon noticed instead of by when the
	// agent finished. And the rejection lasts only until the agent next does
	// anything: the next turn's start writes working, which is an edge, which
	// is a newer value, which clears the slot.
	//
	// It lives here and not in @tmux_web_agent. The daemon READS that option, it
	// does not write it; a reader that edits the channel it reads cannot be
	// reasoned about when two of them run, and nothing promises tmux-web is a
	// singleton. The price is that the slot does not survive a restart -- see
	// Observe's first sight, and Poller.refresh, which empties both slots on a
	// tmux-server generation change for the same reason.
	rejected int64
}

// reject records that the evidence overturned the report in force, so that
// reading the same standing value again does not put it back.
//
// A rejection changes what is BELIEVED, not what was accepted, and the two are
// easy to conflate. A report reaches the evidence rules only by passing the
// ordering filter first, so the condemned report IS st.accepted and stays
// there: the slot marks it as not in force, it does not rewind the filter. The
// filter therefore goes on measuring against that same timestamp, which is what
// makes a delayed older write a non-event -- it is refused for being older, and
// refusing it must not clear the rejection.
func (st *reportState) reject() { st.rejected = st.accepted.Timestamp }

// NewReports returns a report memory that has seen nothing.
func NewReports() *Reports { return &Reports{panes: make(map[string]*reportState)} }

// Observe reads one pane's standing @tmux_web_agent value and reports which
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
		// must clear what we accepted -- `tmux set -p -u @tmux_web_agent` is the
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
		st.windowPolls, st.verified, st.settledPolls = 0, false, 0
		// A newer value is a different report and earns its own verdict.
		st.rejected = 0
	}
	// Anything not newer is either the same report we already hold or an
	// out-of-order write -- ordinary scheduling jitter between two
	// fire-and-forget writes, not a broken integration -- and either way what
	// stands is st.accepted.
	//
	// The guard is on what is ABOUT TO GO INTO FORCE, which is st.accepted --
	// never on `parsed` at the top of the function. A delayed older write does
	// not match the rejection slot (it carries its own, earlier timestamp), so
	// an entry guard waves it through; the ordering filter then refuses it for
	// being older, leaves st.accepted alone -- and st.accepted IS the report
	// that was rejected on evidence, which the function would then return with
	// ok = true. The rejected report would come back in force on the strength
	// of an unrelated late write. Written here, the same poll returns nothing
	// in force, whichever value tmux happened to be holding.
	if st.accepted.Timestamp == st.rejected {
		// The evidence overturned this report and the option still holds it.
		return Report{}, false
	}
	if st.accepted.State == StateWorking &&
		now.Sub(time.UnixMilli(st.accepted.Timestamp)) > workingTTL {
		return Report{}, false
	}
	return st.accepted, true
}

// NeedsScreen reports whether this pane must be captured despite a report being
// in force. Only the two RESTING states ask for one, and they ask over
// different spans.
//
// A working report skips the capture entirely, which is the win of the feature:
// it is re-asserted by its own writer and expires on a clock, so a capture has
// nothing to add.
func (r *Reports) NeedsScreen(paneID string) bool {
	st := r.panes[paneID]
	if st == nil {
		return false
	}
	switch st.accepted.State {
	case StateIdle:
		// Rule 3's window, open until the classifier agrees. It cannot still be
		// open past NIdle polls: the poll that reaches the count rejects the
		// report, and a rejected report is not in force, so this is never asked
		// about one.
		return !st.verified
	case StateBlocked:
		// Rules 1 and 2, for as long as the report stands. There is no verdict
		// that closes this one: rule 1's evidence is a capture that moved and
		// can arrive at any poll, and the dialog can be answered at the
		// terminal at any poll, which is the poll rule 2 starts counting from.
		// So a blocked report costs a fork per poll -- the case the app exists
		// for is exactly the one where the report is wrong.
		return true
	}
	return false
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
		st.reject()
		return false
	}
	return true
}

// CorroborateBlocked feeds one poll's evidence about a reported blocked into
// rules 1 and 2, and reports whether the report survives.
//
// changed is Status.Changed -- whether THIS capture differed from the previous
// one -- and not the classifier's verdict: the verdict is working on a first
// sight and on every poll before settleAfter, so a rule keyed on it would drop
// a true blocked on an ordinary settle.
//
// formOnScreen is IsBlocked's answer for this agent, which asks every
// registered form. "Not the one grammar this agent has": the day a second
// claude form is promoted, a standing permission report on a screen showing an
// elicitation form must not be dropped.
//
// Rule 1 first, and it returns: a screen that moved is positive evidence the
// agent is running, and a poll that moved is not a settled poll, so rule 2's
// counter must not advance on it. That ordering is invisible from outside
// today -- rule 1 drops the report there and then, so the counter's value never
// gets to matter -- and it is written this way so it stays true if rule 1 ever
// becomes something softer than a drop.
func (r *Reports) CorroborateBlocked(paneID string, changed, formOnScreen bool) bool {
	st := r.panes[paneID]
	if st == nil {
		return false
	}
	if changed {
		// Rule 1. The integration died at a dialog the user then answered, and
		// the pane visibly resumed work. The badge does not necessarily go with
		// the report: a dropped report hands the pane back to the grammars on
		// this same capture, and a dialog still on screen is matched there.
		st.reject()
		return false
	}
	if formOnScreen {
		// The agent really is waiting. The run of settled polls rule 2 counts
		// starts again from the poll the form leaves the screen.
		st.settledPolls = 0
		return true
	}
	st.settledPolls++
	if st.settledPolls >= NBlocked {
		// Rule 2. The user answered at the terminal with no client connected
		// and the agent finished before anyone reconnected: nothing ever
		// changes again, so rule 1 never fires and without this the blocked
		// rests forever on a finished agent.
		st.reject()
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
