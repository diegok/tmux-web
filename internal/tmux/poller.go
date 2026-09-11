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
	// fn produces the rows and, when reporting is on, every pane's raw
	// @tmux_web_agent value from the same tmux invocation. A poller built from a
	// bare Snapshot has that half wrapped away: a nil map is no report for
	// every pane, which is what the v1 path means anyway.
	fn func(context.Context) ([]Row, map[string]string, error)
	// startFn reads the tmux server's generation. It is optional: a poller built
	// from a bare snapshot function has no tmux server to ask, and reports an
	// empty generation rather than inventing one.
	startFn func(context.Context) (string, error)
	// nowFn is the poll's clock, and the only one either authority ever gets:
	// Classifier and Reports are both pure, and `now` reaches them as a
	// parameter from here.
	//
	// Never nil -- NewPollerWith fills it with time.Now -- and not in Options,
	// because no caller outside this package has a reason to move the clock.
	// The tests do: workingTTL expires because TIME passed, and a report
	// already in force can only be replaced by a NEWER one, so there is no
	// value a test could write to a fixture that would make it stale instead.
	nowFn func() time.Time

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
	// reports is the other authority, and it outranks the classifier: a fresh
	// report decides the pane and its capture is never taken. Owned by the poll
	// goroutine on exactly the same terms as classifier.
	reports *Reports

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
	// Snapshot produces the rows. Exactly one of Snapshot and
	// SnapshotWithReports is required.
	Snapshot func(context.Context) ([]Row, error)
	// SnapshotWithReports produces the rows AND, from the same tmux
	// invocation, every pane's raw @tmux_web_agent value keyed by pane id. Set
	// this INSTEAD of Snapshot to turn agent reporting on.
	SnapshotWithReports func(context.Context) ([]Row, map[string]string, error)
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
	if (o.Snapshot == nil) == (o.SnapshotWithReports == nil) {
		// Half-wired reporting is silent: with neither there is nothing to
		// poll, and with both there is no way to say which fork happens.
		panic("tmux: exactly one of Options.Snapshot and Options.SnapshotWithReports must be set")
	}
	fn := o.SnapshotWithReports
	if fn == nil {
		fn = func(ctx context.Context) ([]Row, map[string]string, error) {
			rows, err := o.Snapshot(ctx)
			// No map at all rather than an empty one, and nothing downstream
			// needs a branch for it: reports[id] on a nil map is "", which is
			// exactly what no report means.
			return rows, nil, err
		}
	}
	p := &Poller{interval: o.Interval, fn: fn, startFn: o.ServerStart, nowFn: time.Now}
	if o.Capture != nil {
		p.capture, p.connected = o.Capture, o.Connected
		// Built here rather than taken from the caller: it is the poll
		// goroutine's private memory, and two pollers sharing one would report
		// each other's panes as having settled.
		p.classifier = NewClassifier()
		// Beside the classifier and for the same reason. A poller built
		// without reporting still gets one; it simply never sees a value.
		p.reports = NewReports()
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
	rows, reports, err := p.fn(ctx)

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
	//
	// The report memory goes with it, and not only by symmetry. It was left out
	// at first on a derivation -- a restarted server's panes carry no options,
	// so the raw value is "", ParseReport fails and Observe deletes the entry --
	// which holds as far as it goes but makes the daemon's memory depend for its
	// correctness on what tmux happens to be holding. Both of its slots are
	// keyed on a pane id the new server has renumbered: an accepted timestamp
	// would be an ordering floor a fresh agent's first report could fall under,
	// and a rejection recorded against the dead server's %1 would silently
	// condemn the report of whatever reused the id. Across a restart both slots
	// are empty and the standing report is a FIRST SIGHT -- accepted by the
	// ordering filter, then verified from scratch, so a resting idle enters the
	// verification window rather than deriving immediately. That is the honest
	// cost of holding the rejection in daemon memory rather than in tmux:
	// bounded, one-shot, and only on a restart.
	if p.classifier != nil && haveStart && start != p.ServerStart() {
		p.classifier.Retain(nil)
		p.reports.Retain(nil)
	}

	// Before publishing, so no reader ever sees a row between its snapshot
	// fields being set and its state being decided.
	if err == nil {
		p.classify(ctx, rows, reports)
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
func (p *Poller) classify(ctx context.Context, rows []Row, reports map[string]string) {
	if p.classifier == nil {
		return
	}
	// Reports are read every poll whether or not anybody is watching: they cost
	// no fork of their own, and with no client connected a report is the only
	// authority there is. Only the captures are gated on a live client.
	//
	// The classifier is emptied when nobody is watching. A run resumed on
	// reconnect would settle two polls later and stamp a finish edge, lighting
	// a done badge on every device for work that finished while nobody was
	// connected; starting from nothing costs ~3s of "working" instead, which is
	// the same deal a daemon restart makes.
	connected := p.connected()
	if !connected {
		p.classifier.Retain(nil)
	}

	// Only the panes that are known agents on THIS poll. A pane that went
	// claude -> zsh -> claude must come back as a first sight: keeping its hash
	// across the shell means the relaunched agent's different screen sets
	// everChanged, and the next settle stamps a finish edge for an agent that
	// has only just started.
	var agents, captured []string
	now := p.nowFn()
	for i := range rows {
		agent := KnownAgent(rows[i].Command)
		if agent == "" {
			continue
		}
		agents = append(agents, rows[i].PaneID)

		// The report is consulted BEFORE the capture, not after it. Two
		// authorities running in parallel can only agree, in which case the
		// second one was pure cost, or disagree, in which case the design has
		// already settled which wins -- so the second fork buys nothing either
		// way, and not taking it is the whole win of the feature.
		if rep, ok := p.reports.Observe(rows[i].PaneID, reports[rows[i].PaneID], rows[i].Command, now); ok {
			// Both RESTING states make a claim about the screen, and while a
			// client is connected each is checked against one: a reported idle
			// over a window of NIdle polls (rule 3), a reported blocked for as
			// long as it stands (rules 1 and 2). A working report -- and every
			// report at all with nobody watching -- skips the capture entirely,
			// which is the win.
			stands := true
			var st Status
			var blocked bool
			var checked string
			if connected && p.reports.NeedsScreen(rows[i].PaneID) {
				screen, err := p.capture(ctx, rows[i].PaneID)
				if err == nil {
					captured = append(captured, rows[i].PaneID)
					checked = screen
					if rep.State == StateBlocked {
						// Evidence rules 1 and 2. The grammars run on this
						// capture whatever the verdict turns out to be: they
						// are rule 2's premise, and on a drop they are what
						// keeps the badge -- see below.
						blocked = IsBlocked(agent, screen)
						st = p.classifier.Observe(rows[i].PaneID, screen, now, blocked)
						// Status.Changed, not st.State: the verdict is working
						// on a first sight and on every poll before settleAfter,
						// and a rule 1 keyed on it would drop a true blocked on
						// an ordinary settle.
						stands = p.reports.CorroborateBlocked(rows[i].PaneID, st.Changed, blocked)
					} else {
						// Evidence rule 3. blocked=false: the window asks the
						// classifier one question only -- has the screen
						// settled. blocked's only effect in Observe is to
						// withhold the finish stamp, and inside the window the
						// row's FinishedAt is the report's derivation either
						// way.
						st = p.classifier.Observe(rows[i].PaneID, screen, now, false)
						stands = p.reports.Corroborate(rows[i].PaneID, st.State == StateIdle)
					}
				}
			}
			if stands {
				rows[i].AgentState = rep.State
				rows[i].Activity = rep.Activity
				rows[i].StateSource = SourceEvent
				if rep.State == StateIdle && p.reports.Confirmed(rows[i].PaneID, connected) {
					// Derived, not stamped: no memory of a previous report, no
					// edge. Read the option, get the answer -- which is why it
					// survives a daemon restart. See Task 10.
					//
					// Suppressed while the window is open rather than stamped
					// and retracted: a done badge that has landed on three
					// devices does not un-land.
					rows[i].FinishedAt = rep.Timestamp
				}
				// Outside the window the capture is skipped entirely, and this
				// pane is then deliberately NOT added to `captured`: see Retain
				// below.
				continue
			}
			// The screen never settled, so the report is dropped and this pane
			// goes back to the classifier -- on the verdict the window's own
			// capture already produced. It is not captured or Observed a second
			// time: two Observes of one capture would count the same screen
			// twice.
			//
			// The blocked grammars are not run on this poll, and that costs at
			// most one poll of latency on the one screen that could have been
			// holding a dialog all through the window -- a box with a spinner
			// churning under it, which TestRefreshBlockedOverridesChurn shows
			// is a real shape. The drop is remembered, so the very next poll
			// takes the full classifier path, grammars included.
			rows[i].AgentState = st.State
			rows[i].FinishedAt = st.FinishedAt
			rows[i].StateSource = SourceScreen
			if blocked {
				// A rule 1 drop with the dialog still on screen: the report
				// goes and the badge does not, because the grammar has just
				// matched this same capture positively. Both rules require a
				// connected client, so there is no case where the drop happens
				// and the grammar is not there to catch it.
				rows[i].AgentState = StateBlocked
				rows[i].Question = ExtractQuestion(agent, checked)
			}
			continue
		}
		if !connected {
			// Nobody is watching and there is no report, so this pane's state
			// stays empty -- not frozen at its last value, which would be stale
			// state presented as current.
			continue
		}
		screen, err := p.capture(ctx, rows[i].PaneID)
		if err != nil {
			continue
		}
		captured = append(captured, rows[i].PaneID)
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
		rows[i].StateSource = SourceScreen

		if blocked {
			rows[i].AgentState = StateBlocked
			// nil when the grammar could not read the dialog. The state is the
			// load-bearing half; the text is a convenience.
			rows[i].Question = ExtractQuestion(agent, screen)
		}
	}
	p.reports.Retain(agents)
	if connected {
		// `captured`, not `agents`. A pane the poller did not capture is not
		// passed to Retain, so its entry is dropped and the first capture
		// whenever one is taken again is a FIRST SIGHT, which Observe answers
		// with working and no comparison at all. Two reasons, and Task 7's
		// arithmetic rests on the second: a retained hash answers a different
		// question from the one Observe asks ("changed since the previous
		// poll", at a fixed interval), and a retained baseline that differs
		// sets everChanged -- the flag that licenses a time.Now() finish stamp.
		p.classifier.Retain(captured)
	}
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

// PathFor is the working directory the most recent poll saw for a pane, and
// whether it saw one at all.
//
// This is what Client.UsePathCache is given, and it is the reason the snapshot
// carries the path: a split or a new window then opens in the right place
// without forking tmux to ask where that is.
//
// ok is false for a pane the poll did not see -- one created since, or one on a
// poller whose snapshot function carries no paths -- and for a row whose path
// is "". Those are the same answer to the caller, which is "ask tmux", and an
// empty path must never reach `-c`: tmux would take it as "start wherever you
// like" and exit 0.
//
// A linear scan rather than a map. The slice is one row per pane on the whole
// server -- tens, not thousands -- and it is read once per split, not per poll;
// a second index would be a second thing for refresh to keep in step with the
// rows for no measurable gain.
func (p *Poller) PathFor(paneID string) (string, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, r := range p.latest {
		if r.PaneID == paneID {
			return r.Path, r.Path != ""
		}
	}
	return "", false
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
