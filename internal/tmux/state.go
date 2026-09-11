package tmux

import (
	"hash/fnv"
	"time"
)

// States a known agent pane can be in. The zero value, "", means no state was
// computed -- a pane that is not an agent, or one observed with no client
// connected. It is never displayed as a state.
const (
	StateWorking = "working"
	StateIdle    = "idle"
	StateBlocked = "blocked"
)

// Which authority decided a pane's AgentState. "" means nothing did.
const (
	SourceEvent  = "event"  // the agent's own report, from @wterm_agent
	SourceScreen = "screen" // the churn classifier and the blocked grammars
)

// settleAfter is how many consecutive identical captures mean idle. Two, so
// ~3s at the 1.5s poll: long enough that a redraw landing between polls does
// not flicker, short enough to feel live.
const settleAfter = 2

// lateRepaintDwell is how long after a stamped finish another one is refused.
//
// It exists for a measured failure: on 2 of 30 claude turns a single line near
// the input box repainted 5.0 s and 9.0 s after everything else had stopped.
// That produces a second working->idle edge about 13 s after the real turn end,
// and since a browser badges on finishedAt > the VALUE it was last shown, the
// second stamp re-lights a done badge the user has already cleared.
//
// It is NOT symmetric with the other two guards and should not be described as
// one: they decide whether THIS run earned an edge, and this decides whether a
// second edge so soon after the first can be a different finish at all.
//
// The constant is a GUESS, of the same standing as the 60-second working
// window. It is floored by the measurement (the second edge landed 12 to 13.5 s
// after the first) and that floor rests on a sample of TWO events, which is a
// bound and not a distribution. 15 s, biased long. Biasing long costs two
// genuine finishes inside the dwell collapsing to one badge -- for which the
// user would have to have looked at the pane between them and then walked away
// within seconds -- against a failure measured at 2 of 30 claude turns.
//
// It applies to the classifier's stamp ONLY, never to a report's derivation. A
// report dates its own finish, and a resting state is never re-asserted with a
// later timestamp, so there is nothing there for a dwell to protect against and
// adding one would silently swallow a genuine second turn end.
const lateRepaintDwell = 15 * time.Second

// Status is what one observation of a pane concluded.
type Status struct {
	State      string // StateWorking or StateIdle; blocked is decided elsewhere
	FinishedAt int64  // unix ms of the last working->idle edge, 0 if there was none
	// Changed reports whether THIS capture differed from the previous one.
	//
	// Separate from State because they answer different questions: State says
	// working on a single changed hash and on every poll before settleAfter,
	// which is precisely what settleAfter exists to declare is noise. Evidence
	// rule 1 wants the raw fact -- a changed hash is positive evidence the
	// agent is running -- and a rule keyed on the verdict would fire on an
	// ordinary settle.
	//
	// False on a first sight, which has nothing to compare against. That is the
	// first poll of every blocked report's evidence, and calling it a change
	// would drop every one of them on arrival.
	Changed bool
}

// paneState is what the classifier remembers between polls for one pane.
type paneState struct {
	hash  uint64 // FNV-64a of the previous capture
	still int    // consecutive polls whose capture was identical
	// finishedAt is the last working->idle edge, in unix ms, and is reported
	// until a new edge replaces it. The browser compares it against its own
	// per-device `seen` value to decide whether to show a done badge.
	finishedAt int64
	// everChanged records whether this pane's capture has actually been
	// observed to change since the entry was created.
	//
	// It is the whole of the restart-storm fix. The map is rebuilt from nothing
	// when the daemon restarts and whenever the last browser client
	// disconnects, and a first sight has nothing to compare against, so it
	// reports working. Without this flag every one of those synthesised runs
	// would settle two polls later and stamp a finish edge newer than every
	// browser's stored `seen` -- so every device would light up with done
	// badges on every agent pane after every restart or reconnect. A run only
	// earns a finish edge if a real change was seen during it.
	everChanged bool
}

// Classifier decides whether an agent pane is working or idle from whether its
// screen is still changing.
//
// The screen is the signal, not the pane title: Claude Code rewrites its title
// when the task summary changes and then leaves it byte-identical through
// minutes of work, while a working agent redraws its spinner and elapsed-time
// counter at least once a second.
//
// It is pure: it has no clock and no I/O, and the caller passes `now`. It is
// not safe for concurrent use -- the poller goroutine owns it.
type Classifier struct {
	panes map[string]*paneState
}

// NewClassifier returns a classifier that has seen nothing.
func NewClassifier() *Classifier {
	return &Classifier{panes: make(map[string]*paneState)}
}

// Observe records one capture of a pane and reports what it means.
//
// The whole capture is hashed, not a tail of it: a redraw at the top of the
// screen is work too.
//
// blocked is the caller's verdict on this same capture -- whether a dialog is
// on screen waiting for the owner. The classifier cannot see it: a held
// permission box is byte-identical poll after poll, which is exactly the shape
// stillness has, so without being told, a working agent that raises a box
// stamps a working->idle edge two polls later for a run that never finished.
// The damage lands after the box is answered: the agent resumes working and
// that stale edge is still the newest, so every device that has not viewed the
// pane shows done on an agent that is mid-run. A false done is the same
// badge-integrity failure a false blocked is.
//
// So while blocked the hash and the still-counter are kept up to date -- the
// screen really is still, and the settled *state* is fine, since the caller
// overrides it with blocked anyway -- and only the stamp is withheld. The run
// is not otherwise disturbed: everChanged survives, so the finish that comes
// after the box is answered stamps normally.
func (c *Classifier) Observe(paneID, capture string, now time.Time, blocked bool) Status {
	h := fnv.New64a()
	h.Write([]byte(capture))
	sum := h.Sum64()

	var changed bool
	p, known := c.panes[paneID]
	switch {
	case !known:
		// Nothing to compare against. Report working and let it settle rather
		// than guessing -- but everChanged stays false, so the settle two polls
		// from now stamps no edge.
		c.panes[paneID] = &paneState{hash: sum}
		return Status{State: StateWorking}
	case p.hash != sum:
		p.hash = sum
		p.still = 0
		p.everChanged = true
		changed = true
	default:
		p.still++
	}

	if p.still < settleAfter {
		return Status{State: StateWorking, FinishedAt: p.finishedAt, Changed: changed}
	}
	// Stamp on the transition, not for as long as the pane is idle. `still`
	// passes through settleAfter exactly once per working->idle edge: guarding
	// this with `finishedAt == 0` instead would let the done badge fire once
	// per pane for the life of the daemon, and guarding it with nothing would
	// re-stamp on every idle poll, so viewing the pane could never clear it.
	//
	// `!blocked` is the third guard and it is not symmetric with the others: a
	// pane held at a dialog passes through settleAfter exactly once too, and
	// that poll is its only chance to stamp. Withholding it there means a run
	// interrupted by a question earns no edge until it really ends.
	//
	// lateRepaintDwell is the fourth, and it answers a different question again:
	// a repaint seconds after a finish draws a whole second working->idle edge,
	// and a stamp for it re-lights a done badge its owner has already cleared.
	//
	// The `p.finishedAt == 0` disjunct is LOAD-BEARING and must be written out.
	// "A pane that has never finished has finishedAt 0, and now - epoch is
	// obviously more than 15s" is true only when `now` is a real wall clock.
	// Every test in this file's suite starts at `now := time.Unix(0, 0)` and
	// advances in 1.5s steps, so at the first genuine stamp `now` is 7.5 seconds
	// past the epoch and time.UnixMilli(0) IS the epoch: the subtraction gives
	// 7.5s, which is less than the dwell, and the first stamp of the existing v2
	// suite is refused.
	if p.still == settleAfter && p.everChanged && !blocked &&
		(p.finishedAt == 0 || now.Sub(time.UnixMilli(p.finishedAt)) >= lateRepaintDwell) {
		p.finishedAt = now.UnixMilli()
	}
	// Changed is always false on this path -- a differing capture resets `still`
	// to 0, which is below settleAfter -- and it is carried anyway rather than
	// left to the zero value, so that the field means the same thing at every
	// return and nobody has to prove that again when settleAfter moves.
	return Status{State: StateIdle, FinishedAt: p.finishedAt, Changed: changed}
}

// Retain forgets every pane that is not in keep.
//
// The caller passes only the ids of panes that are known agents right now, not
// every pane in the snapshot: a pane that goes claude -> zsh -> claude must come
// back as a first sight, or the relaunched agent's differing capture would set
// everChanged and the next settle would stamp a finish edge for an agent that
// has only just started. Retaining nothing resets the classifier entirely.
func (c *Classifier) Retain(keep []string) {
	if len(keep) == 0 {
		clear(c.panes)
		return
	}
	set := make(map[string]struct{}, len(keep))
	for _, id := range keep {
		set[id] = struct{}{}
	}
	for id := range c.panes {
		if _, ok := set[id]; !ok {
			delete(c.panes, id)
		}
	}
}
