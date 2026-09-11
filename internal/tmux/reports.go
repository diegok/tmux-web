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
