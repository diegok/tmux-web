package tmux

import (
	"context"
	"sync"
	"time"
)

// Poller refreshes a snapshot on a ticker and hands the cached value to any
// number of readers.
//
// The cost is per interval, never per reader: ten tabs polling /api/snapshot
// re-read the same cached bytes. What the interval costs is the snapshot, the
// server generation beside it, and -- only while a browser is connected -- one
// capture per pane running a known agent. Three agents is two forks a second,
// which is the trade the design makes and the reason both of those conditions
// are checked rather than assumed.
type Poller struct {
	interval time.Duration
	fn       func(context.Context) ([]Row, error)
	// startFn reads the tmux server's generation. It is optional: a poller built
	// from a bare snapshot function has no tmux server to ask, and reports an
	// empty generation rather than inventing one.
	startFn func(context.Context) (string, error)

	// The poller's second, narrower job: for panes running a known agent, and
	// only while a browser is holding a terminal socket, each poll also
	// captures the pane and decides what state it is in. All three are nil
	// together on a poller that was not built for it, which is v1's poller and
	// still the one every NewPollerFunc caller gets.
	//
	// classifier is touched only from refresh, which never runs concurrently
	// with itself: Start polls once synchronously and then from a single
	// goroutine, so this needs no lock of its own. The mutex below guards the
	// cached results, not the machinery that produces them.
	capture    func(ctx context.Context, paneID string) (string, error)
	connected  func() bool
	classifier *Classifier

	mu          sync.RWMutex
	latest      []Row
	err         error
	serverStart string
}

// Options is everything a Poller can be given. Snapshot is the only required
// field; the rest each turn something on.
//
// It exists so that agent classification could be added without widening
// NewPoller and NewPollerFunc, whose callers have no reason to care that the
// poll now has a second job.
type Options struct {
	Interval time.Duration
	// Snapshot produces the rows. Required.
	Snapshot func(context.Context) ([]Row, error)
	// ServerStart reads the tmux server's generation, once per poll. Optional:
	// without it the poller reports no generation rather than inventing one.
	ServerStart func(context.Context) (string, error)
	// Capture and Connected turn on agent classification, and must be given
	// together -- see NewPollerWith.
	//
	// Connected reports whether a browser is holding a live terminal socket.
	// Not whether anyone is looking at a particular pane: that would leave the
	// sidebar blank for exactly the agents the user is not watching, which is
	// what the sidebar is for.
	Capture   func(ctx context.Context, paneID string) (string, error)
	Connected func() bool
}

// NewPollerWith builds a poller from Options.
//
// It panics if exactly one of Capture and Connected is set. Half-wired
// classification is silent: captures with no liveness check fork tmux for
// nobody, and a liveness check with no capture computes nothing, and either way
// every state on the wire is empty forever with every test still green. That is
// a wiring mistake worth failing loudly at startup, where it is one line to fix.
func NewPollerWith(o Options) *Poller {
	if (o.Capture == nil) != (o.Connected == nil) {
		panic("tmux: Options.Capture and Options.Connected must be set together")
	}
	p := &Poller{interval: o.Interval, fn: o.Snapshot, startFn: o.ServerStart}
	if o.Capture != nil {
		p.capture, p.connected = o.Capture, o.Connected
		// Built here rather than taken from the caller: it is the poll
		// goroutine's private memory, and two pollers sharing one would report
		// each other's panes as having settled.
		p.classifier = NewClassifier()
	}
	return p
}

// NewPollerFunc builds a poller over an arbitrary snapshot function. It reports
// no server generation and classifies nothing; see NewPollerWith for both.
func NewPollerFunc(interval time.Duration, fn func(context.Context) ([]Row, error)) *Poller {
	return NewPollerWith(Options{Interval: interval, Snapshot: fn})
}

// NewPoller builds a poller over a real tmux server, without classification.
func NewPoller(interval time.Duration, c *Client) *Poller {
	return NewPollerWith(Options{Interval: interval, Snapshot: c.Snapshot, ServerStart: c.ServerStart})
}

// Start polls once synchronously -- so the first tab to connect does not see an
// empty sidebar -- then keeps polling until ctx is cancelled.
func (p *Poller) Start(ctx context.Context) {
	p.refresh(ctx)
	go func() {
		t := time.NewTicker(p.interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				p.refresh(ctx)
			}
		}
	}()
}

// refresh replaces the cached snapshot, but only when the poll succeeded.
//
// A failed poll updates err and leaves the rows alone. tmux prints "server
// exited unexpectedly" for a few milliseconds while a server shuts down, and
// noServer deliberately does not match it -- so without this, one poll landing
// in that window would blank the sidebar and the next would silently fix it.
// Keeping the last good snapshot covers that and any other transient fault
// without having to enumerate tmux's error strings, which is exactly the
// fragile thing noServer already has to do.
//
// The stale snapshot is not served silently: err stays set until a poll
// succeeds, so a caller can tell a momentary hiccup from a server that has been
// unreachable for a minute.
func (p *Poller) refresh(ctx context.Context) {
	rows, err := p.fn(ctx)

	// The generation is read per poll rather than once at construction: the tmux
	// server can restart underneath a running daemon, and that is precisely the
	// moment the value matters. It is skipped when the snapshot itself failed --
	// the same fault would fail this too, and a doomed second fork buys nothing --
	// and a failure of its own leaves the previous generation in place, since
	// blanking it would make every browser's per-pane memory miss for one poll.
	var start string
	var haveStart bool
	if err == nil && p.startFn != nil {
		var serr error
		if start, serr = p.startFn(ctx); serr == nil {
			haveStart = true
		}
	}

	// A tmux server restart renumbers panes from %0, so an entry keyed on a
	// pane id survives into a server it says nothing about: a fresh pane
	// reusing %1 is compared against the dead server's hash, inherits its
	// everChanged, and stamps a finish edge two polls later. The browser cannot
	// suppress that one -- its `seen` map is keyed on the new generation, so it
	// is empty -- and an unviewed done badge on an agent that has only just
	// started is the badge-integrity failure the whole feature rests on.
	//
	// Nothing else prunes it: while the server is down the snapshot fails and
	// refresh returns above without reaching classify at all.
	//
	// Only a *change* resets. A poll gap on the same server needs none: an edge
	// after a snapshot outage reflects work that really happened. haveStart
	// gates it so that a poller with no generation reader -- NewPollerFunc's,
	// which is every v1 caller -- does not reset on every poll, and so that a
	// generation read that failed leaves the run alone rather than reading its
	// own empty answer as a restart.
	if p.classifier != nil && haveStart && start != p.ServerStart() {
		p.classifier.Retain(nil)
	}

	// Before publishing, so no reader ever sees a row between its snapshot
	// fields being set and its state being decided.
	if err == nil {
		p.classify(ctx, rows)
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	p.err = err
	if haveStart {
		p.serverStart = start
	}
	if err != nil {
		return
	}
	p.latest = rows
}

// classify captures every known agent pane and writes its state into rows.
//
// Rows are modified in place, which is safe only because this runs before they
// are published: Snapshot's slice comes from Dedupe, which allocates a fresh one
// per call and never aliases the previous poll's.
//
// A capture that fails leaves that pane's state empty rather than failing the
// poll. The snapshot is milliseconds old by the time capture-pane runs, so a
// pane that has closed in between is an ordinary race and not a fault -- and
// blanking a whole sidebar because one pane went away would be a poor trade.
func (p *Poller) classify(ctx context.Context, rows []Row) {
	if p.classifier == nil {
		return
	}
	if !p.connected() {
		// Nobody is watching, so nothing is captured and every state stays
		// empty -- not frozen at its last value, which would be stale state
		// presented as current.
		//
		// The classifier is emptied with it. A run resumed on reconnect would
		// settle two polls later and stamp a finish edge, lighting a done badge
		// on every device for work that finished while nobody was connected;
		// starting from nothing costs ~3s of "working" instead, which is the
		// same deal a daemon restart makes.
		p.classifier.Retain(nil)
		return
	}

	// Only the panes that are known agents on THIS poll. A pane that went
	// claude -> zsh -> claude must come back as a first sight: keeping its hash
	// across the shell means the relaunched agent's different screen sets
	// everChanged, and the next settle stamps a finish edge for an agent that
	// has only just started.
	var agents []string
	now := time.Now()
	for i := range rows {
		agent := KnownAgent(rows[i].Command)
		if agent == "" {
			continue
		}
		agents = append(agents, rows[i].PaneID)

		screen, err := p.capture(ctx, rows[i].PaneID)
		if err != nil {
			continue
		}
		// Decided before the capture is classified, not after, because the
		// classifier needs it: a held dialog is byte-identical between polls
		// and would otherwise settle into a working->idle edge for a run that
		// never finished. See Classifier.Observe.
		//
		// Checked on every capture, with no idle gate: an agent can raise an
		// approval box while background work carries on redrawing, so churn's
		// verdict never outvotes a box that is on screen. Only a positive match
		// changes anything -- no match leaves whatever churn decided.
		blocked := IsBlocked(agent, screen)

		st := p.classifier.Observe(rows[i].PaneID, screen, now, blocked)
		rows[i].AgentState = st.State
		rows[i].FinishedAt = st.FinishedAt

		if blocked {
			rows[i].AgentState = StateBlocked
			// nil when the grammar could not read the dialog. The state is the
			// load-bearing half; the text is a convenience.
			rows[i].Question = ExtractQuestion(agent, screen)
		}
	}
	p.classifier.Retain(agents)
}

// Latest returns the most recent successful snapshot without forking tmux.
//
// The slice is shared with every other reader and must not be modified.
// Snapshot's rows come from Dedupe, which allocates a fresh slice per call and
// never aliases its input, so a refresh replaces this value rather than writing
// through it.
func (p *Poller) Latest() []Row {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.latest
}

// ServerStart is the generation of the tmux server the cached snapshot came
// from, or "" if there is no server or this poller was built without a client.
// See Client.ServerStart for why anything keyed on a pane id needs it.
func (p *Poller) ServerStart() string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.serverStart
}

// Err reports how the most recent poll ended: nil if it succeeded, otherwise
// the failure, in which case Latest is serving the snapshot from before it.
//
// Latest and Err take the lock separately, so a caller reading both across a
// refresh can pair rows with the other poll's error. That is deliberate -- the
// pair only ever straddles one interval, and neither value is ever wrong on its
// own -- and a combined accessor can be added when a caller needs one.
func (p *Poller) Err() error {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.err
}
