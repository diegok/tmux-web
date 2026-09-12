package tmux

import (
	"context"
	"errors"
	"fmt"
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
	// fn produces one poll's worth of tmux state: the rows, and -- when the
	// poller was built over a batched read -- every pane's raw @tmux_web_agent
	// value and the server's generation from the same tmux invocation. A poller
	// built from a bare Snapshot has those halves wrapped away: a nil map is no
	// report for every pane, and no generation is what the v1 path means
	// anyway.
	fn func(context.Context) (Poll, error)
	// startFn reads the tmux server's generation in a fork of its own, and is
	// the fallback for a poller whose read does not carry one. It is optional,
	// and mutually exclusive with a batched Poll: see NewPollerWith.
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
	// timeout bounds one whole poll -- the snapshot, and every capture it
	// leads to -- and is what keeps a wedged tmux from parking the poll
	// goroutine forever. See pollTimeout for the value and refresh for what a
	// poll that hits it looks like to the owner.
	//
	// A field rather than a constant so that it is derived from the interval,
	// and unexported beside nowFn and newTicker for the same reason: the
	// interval is the caller's business, the machinery behind it is not. The
	// tests that have to wait a deadline out set it directly, because the real
	// value is seconds.
	timeout time.Duration
	// newTicker is the poll loop's ticker, returning its channel and the
	// function that stops it. Beside nowFn, and unexported for the same reason:
	// the interval is the caller's business, the machinery behind it is not.
	//
	// The tests drive the tick rather than wait for one. A loop test that waits
	// cannot say anything an interval cannot take back -- "no poll happened
	// after cancel" is only ever "none yet" -- and at the millisecond intervals
	// such a test needs to stay fast, "yet" expires while it is still running.
	// See TestPollerStopsOnContextCancel.
	newTicker func(time.Duration) (<-chan time.Time, func())

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

	// The forced-poll machinery. PollNow does not poll; it asks the loop to,
	// and waits. See PollNow for why it must not simply call refresh.
	//
	// force carries one BATCH of waiting callers to the loop. Buffered at one,
	// which is enough forever: a batch is created only when pending is nil, and
	// pending is cleared only by the loop at the moment it takes the batch off
	// this channel -- so between a send and its receive no second send can be
	// started, and the send never blocks.
	force chan chan struct{}
	// fmu guards pending and loopDone. Its own mutex rather than mu: mu is held
	// across a read of the published snapshot, and a caller waiting to be told
	// a poll has happened has no business queueing behind readers.
	fmu sync.Mutex
	// pending is the batch of callers waiting for a poll that STARTS AFTER they
	// asked, or nil when there is none. Closed by the loop when that poll has
	// published.
	pending chan struct{}
	// loopDone is the poll loop's context, so a forced poll on a poller that
	// has stopped is an error rather than a wait. nil until Start, which is the
	// answer for a poller that was never started: such a caller waits out its
	// own context, which is the honest thing to do -- nothing is coming.
	loopDone <-chan struct{}

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
	// Snapshot produces the rows. Exactly one of Snapshot and Poll is required.
	Snapshot func(context.Context) ([]Row, error)
	// Poll produces the rows AND, from the same tmux invocation, every pane's
	// raw @tmux_web_agent value keyed by pane id and the tmux server's
	// generation. Set this INSTEAD of Snapshot: it is the batched read, it
	// turns agent reporting on, and it makes ServerStart both unnecessary and
	// refused.
	Poll func(context.Context) (Poll, error)
	// ServerStart reads the tmux server's generation in a fork of its own, once
	// per poll. Optional, and only for a Snapshot poller: without it such a
	// poller reports no generation rather than inventing one, and beside Poll
	// it is a wiring mistake rather than an option -- see NewPollerWith.
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
	if (o.Snapshot == nil) == (o.Poll == nil) {
		// Half-wired reporting is silent: with neither there is nothing to
		// poll, and with both there is no way to say which fork happens.
		panic("tmux: exactly one of Options.Snapshot and Options.Poll must be set")
	}
	if o.Poll != nil && o.ServerStart != nil {
		// The batched read already carries the generation. A second reader
		// beside it forks tmux twice a poll, forever, for one value both
		// answers agree on -- so nothing would ever look wrong, which is
		// exactly why this is worth failing at startup rather than leaving to
		// be noticed.
		panic("tmux: Options.ServerStart is the fork Options.Poll removes; set one or the other")
	}
	fn := o.Poll
	if fn == nil {
		fn = func(ctx context.Context) (Poll, error) {
			rows, err := o.Snapshot(ctx)
			// No map and no generation rather than empty ones, and nothing
			// downstream needs a branch for either: reports[id] on a nil map is
			// "", which is exactly what no report means, and HaveServerStart
			// false is what sends refresh to startFn.
			return Poll{Rows: rows}, err
		}
	}
	p := &Poller{
		interval:  o.Interval,
		timeout:   pollTimeout(o.Interval),
		force:     make(chan chan struct{}, 1),
		fn:        fn,
		startFn:   o.ServerStart,
		nowFn:     time.Now,
		newTicker: realTicker,
	}
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

// pollTimeoutFactor is how many intervals one poll may take before it is
// abandoned.
//
// Not one. A deadline equal to the interval means a tmux that answers in 1.6s
// on a 1.5s interval -- slow, loaded, but working -- never completes a poll,
// and the daemon reports a healthy server as unreachable forever. Four leaves
// room for a machine having a bad minute while still being far outside
// anything a working server does: a poll's tmux read is milliseconds, so a
// poll that has taken four intervals is not slow, it is stuck.
//
// The cost of the headroom is bounded and mild: the tree can be up to one
// deadline behind before the daemon says so, and the browser waits for two
// troubled polls before it tells the user anyway (TROUBLE_BEFORE_STALE).
const pollTimeoutFactor = 4

// minPollTimeout is the floor under that multiple.
//
// Two jobs. It keeps a short interval from tightening the deadline below what a
// single tmux command is allowed to take anywhere else in this daemon -- the
// socket path bounds each of its own at 5s (front.wsTmuxTimeout) against the
// same wedged server. And it makes an unset interval safe: NewPollerFunc(0,
// ...) is a real construction, and context.WithTimeout(ctx, 0) is a context
// that has already expired, which would fail every poll such a poller makes.
const minPollTimeout = 5 * time.Second

// pollTimeout is how long one poll may take, given the interval it runs on.
func pollTimeout(interval time.Duration) time.Duration {
	return max(interval*pollTimeoutFactor, minPollTimeout)
}

// realTicker is what every poller outside a test polls on.
func realTicker(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTicker(d)
	return t.C, t.Stop
}

// Start polls once synchronously -- so the first tab to connect does not see an
// empty sidebar -- then keeps polling until ctx is cancelled.
//
// The ticker is built here rather than inside the goroutine so that the
// interval starts running when Start returns, not whenever the scheduler gets
// round to the goroutine.
//
// Cancellation is not instant and cannot be: a tick already in the channel when
// ctx is cancelled leaves the select with two ready cases, and Go picks between
// them at random. That costs at most one more poll -- the ticker is stopped on
// the way out, so no further tick can arrive to be picked.
func (p *Poller) Start(ctx context.Context) {
	// Published before the first poll, so a PollNow that arrives during it can
	// already tell a running poller from one that was never started.
	p.fmu.Lock()
	p.loopDone = ctx.Done()
	p.fmu.Unlock()

	p.refresh(ctx)
	tick, stop := p.newTicker(p.interval)
	go func() {
		defer stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick:
				p.refresh(ctx)
			case batch := <-p.force:
				// Cleared BEFORE the poll, not after. A caller that arrives
				// while this poll is running asked after it started, so this
				// poll cannot answer them: they open the next batch and get
				// their own poll. Clearing afterwards would fold them into a
				// read that predates the change they are waiting to see, which
				// is the whole bug being fixed.
				p.fmu.Lock()
				p.pending = nil
				p.fmu.Unlock()
				// ctx, not the caller's. The poll is the daemon's work and
				// carries the poll deadline; a browser that gave up must not
				// abort a poll every other reader is about to be served.
				p.refresh(ctx)
				close(batch)
			}
		}
	}()
}

// errPollerStopped is what a forced poll gets when there is no loop to run it.
var errPollerStopped = errors.New("tmux: the poller has stopped")

// PollNow forces a poll and returns once its result has been published, so that
// the caller's next Latest -- or the browser's next /api/snapshot -- sees a tree
// read after whatever the caller just did.
//
// WHY IT EXISTS. Every management verb changes the tmux tree, and the cached
// snapshot is up to one interval old. A killed row lingering is the mild half;
// the sharp half is a split, whose new pane id is ABSENT from the tree the
// browser asks for immediately afterwards, so the tab it is trying to open
// points at nothing. Both frontend and design promised this was already
// handled. Only the browser's re-fetch was: nothing re-polled.
//
// WHY IT DOES NOT JUST CALL refresh. refresh owns the classifier and the report
// memory outright -- see the field comments -- and that ownership is what lets
// them run without locks of their own. A second caller running refresh on a
// request goroutine would race the ticker's for those maps, and the damage
// would not be a crash but a mis-stamped finish edge, which is a false done
// badge. So a forced poll is a REQUEST to the one poll goroutine, and no two
// polls ever overlap.
//
// WHAT IT COSTS, since a management verb can be held down. One poll: the same
// batched fork the ticker makes, plus a capture per agent pane while a browser
// is connected. Concurrent callers COALESCE -- they share one batch and one
// poll -- so a burst costs one poll for the burst rather than one each, and
// because the polls are serialized on the one goroutine, the worst a caller can
// do is keep that goroutine busy back to back. The ticker is deliberately NOT
// reset: it is a ceiling on staleness rather than a rate limit, and resetting
// it would make a stream of management verbs able to postpone the regular poll
// indefinitely.
//
// A caller that gives up -- its context expired, or the browser went away --
// leaves the poll running. It was requested, other readers are about to be
// served by it, and abandoning it would waste the fork it has already made.
func (p *Poller) PollNow(ctx context.Context) error {
	p.fmu.Lock()
	batch, done := p.pending, p.loopDone
	if batch == nil {
		batch = make(chan struct{})
		p.pending = batch
		// Never blocks; see the field comment on force.
		p.force <- batch
	}
	p.fmu.Unlock()

	select {
	case <-batch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
		// nil for a poller that was never started, which makes this case
		// unreachable and leaves such a caller on its own context.
		return errPollerStopped
	}
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
//
// The whole poll runs under a deadline, and that is what makes the sentence
// above true of a wedged tmux as well as of a failed one. Without it a tmux
// that never answers parks this goroutine: the loop above cannot start another
// poll, every tick is dropped, and err stays nil -- so the sidebar freezes
// while the daemon goes on reporting itself fresh, which is the one failure
// nothing tells the user about. With it, a poll that runs out of time is an
// ordinary failed poll: the last good tree stays on screen and Err() carries a
// sentence saying tmux did not answer, which /api/snapshot serves as `stale`
// and the sidebar prints.
//
// One deadline for the poll rather than one per command, so that the bound does
// not multiply by the number of agent panes. A capture cut short by it is not
// promoted to a failed poll -- classify already treats a failed capture as one
// pane's missing state rather than a blank sidebar -- so a poll whose snapshot
// arrived and whose captures did not is published with fresh rows and no agent
// state, which is what "the capture failed" has always meant here.
func (p *Poller) refresh(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	res, err := p.fn(ctx)
	// Said in the daemon's own words, because tmux does not say it: the error
	// from a command killed by its context is "signal: killed", which wraps
	// nothing a caller can match on and reads, in the sidebar, as though tmux
	// had been shot. Only an expired deadline is rewritten -- a parent
	// cancelled by a shutting-down daemon reports itself as cancelled, since an
	// operator reading "tmux did not answer" out of their own Ctrl-C would go
	// looking in the wrong place.
	if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		err = fmt.Errorf("tmux did not answer within %s", p.timeout)
	}

	// The generation is read per poll rather than once at construction: the tmux
	// server can restart underneath a running daemon, and that is precisely the
	// moment the value matters. It is skipped when the snapshot itself failed --
	// the same fault would fail this too, and a doomed read buys nothing -- and
	// a read that did not carry one leaves the previous generation in place,
	// since blanking it would make every browser's per-pane memory miss for one
	// poll.
	//
	// It rides the batched read where there is one, which is the daemon's own
	// poller: one fork, N format strings, and this value was the last thing
	// breaking that rule.
	var start string
	var haveStart bool
	if err == nil {
		start, haveStart = res.ServerStart, res.HaveServerStart
		if !haveStart && p.startFn != nil {
			var serr error
			if start, serr = p.startFn(ctx); serr == nil {
				haveStart = true
			}
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
		p.classify(ctx, res.Rows, res.Reports)
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
	p.latest = res.Rows
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

// WindowFor is the window the most recent poll saw a pane in, and whether it
// saw one at all.
//
// This is what the terminal handler passes to Client.SelectPane, and it is why
// a sidebar click forks tmux once instead of three times: the id that used to
// be re-read per click is a column of the snapshot that produced the row the
// user clicked on.
//
// A HINT, never an authority, and SelectPane treats it as one -- see there. ok
// is false for a pane the poll did not see and for a row with no window id, and
// both mean "read it": "" handed to `select-window -t "=sess:"` resolves to the
// window the session is already on and exits 0, which is a navigation that
// silently does not happen.
//
// A linear scan, for the reason PathFor gives.
func (p *Poller) WindowFor(paneID string) (string, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, r := range p.latest {
		if r.PaneID == paneID {
			return r.WindowID, r.WindowID != ""
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
