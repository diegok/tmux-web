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

// Status is what one observation of a pane concluded.
type Status struct {
	State      string // StateWorking or StateIdle; blocked is decided elsewhere
	FinishedAt int64  // unix ms of the last working->idle edge, 0 if there was none
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
	default:
		p.still++
	}

	if p.still < settleAfter {
		return Status{State: StateWorking, FinishedAt: p.finishedAt}
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
	if p.still == settleAfter && p.everChanged && !blocked {
		p.finishedAt = now.UnixMilli()
	}
	return Status{State: StateIdle, FinishedAt: p.finishedAt}
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
