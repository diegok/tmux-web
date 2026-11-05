package tmux

import (
	"context"
	"time"
)

// The vocabulary a pane that is NOT a known agent gets, plus the one answer
// that is not a state at all.
//
// Deliberately not StateIdle and StateWorking. Those three are an agent's
// states, one list gates whether a pane gets one at all (agent.go: "a pane can
// never show a badge for a state nothing computed"), and telling a coordinating
// agent that a shell is "idle" would hand it a claim the daemon itself refuses
// to make. "quiet" and "changing" say only what two captures showed.
const (
	LiveChanging = "changing"
	LiveQuiet    = "quiet"
	LiveUnknown  = "unknown"
)

// Which authority answered.
//
// Not SourceEvent/SourceScreen, whose "event" names a hook report that was
// DELIVERED to a running daemon. Nothing is delivered here: the option is read
// on demand, and these two values are part of what a subcommand prints, so
// they are named for the one-shot read rather than borrowed from the poller's
// row.
const (
	VerdictReport = "report"
	VerdictScreen = "screen"
)

// Verdict is what one on-demand look at a pane concluded.
type Verdict struct {
	State    string    // StateWorking/StateIdle/StateBlocked, or LiveChanging/LiveQuiet, or LiveUnknown
	Question *Question // nil unless State is StateBlocked, and nil-able even then
	// From names what decided the STATE: VerdictReport or VerdictScreen.
	From string
	// QuestionFrom names where the TEXT of Question came from, and is "" when
	// Question is nil. It can differ from From, and the case where it does is
	// the reason it exists: a state-only blocked report (no Activity) that a
	// screen corroborated. The report decided the state; the question was read
	// off the screen by a grammar. A caller that labelled that text with From
	// would present a grammar's reading as the integration's own statement.
	QuestionFrom string
	// Dropped says a standing blocked report was read and then not believed,
	// because no registered form was on screen to corroborate it.
	//
	// It is a separate field and not a fourth state because the State is
	// honest on its own: no dialog is on screen, so nothing is waiting on the
	// owner. What Dropped adds is that this `idle` rests on ONE capture of a
	// pane whose own agent claims otherwise -- the thinnest evidence this
	// function ever returns, and the only verdict a caller might want to treat
	// as a reason to look again rather than as an answer.
	Dropped bool
}

// LiveReader is the slice of Client that LiveState needs, so a test can script
// both reads and count both without a tmux server.
type LiveReader interface {
	Run(ctx context.Context, args ...string) (string, error)
	Capture(ctx context.Context, paneID string) (string, error)
}

// LiveState computes a pane's state now, from the pane, with no poller.
//
// It does not read the poller's cache and must not be changed to. The poller
// empties its classifier and gates its captures off whenever no browser is
// connected (poller.go), keeping only the hooks' reports -- which is precisely
// the case this function exists for: an agent asking about another agent while
// the owner is away from the tab. The cache is unavailable exactly when it
// would matter most.
//
// Nor does it take a report at face value. reports.go makes a resting report
// survive multi-poll evidence, and records why: "a blocked report costs a fork
// per poll -- the case the app exists for is exactly the one where the report is
// wrong." A single call has no history to weigh, so it corroborates a blocked
// report against one capture instead. That is the one direction the owner's
// loop cannot absorb: the loop reads a pane's screen when it hears blocked, so
// a false blocked spends a read per pass on a pane with nothing to answer, for
// as long as the stale report stands.
//
// settle is the caller's, not a constant here: this is not running on the
// poller's 1.5s grid, so the gap between the two captures is a policy decision
// belonging to whoever is paying for the wait. A test passes zero.
//
// A LiveUnknown verdict always comes with the error that caused it.
func LiveState(ctx context.Context, c LiveReader, row Row, now time.Time, settle time.Duration) (Verdict, error) {
	// Validated before anything forks: tmux resolves an empty -t to "whatever
	// is current" and exits 0, so a malformed id would answer confidently
	// about some other pane.
	if err := ValidatePaneID(row.PaneID); err != nil {
		return Verdict{State: LiveUnknown}, err
	}
	agent := KnownAgent(row.Command)
	// The report is read for a known agent only, because only a known agent
	// has one: Reports.Observe drops any standing value on a pane whose
	// command is not on the Agents list -- the agent exited and the value is
	// its leftover. Reading it here would spend a fork on an answer that has
	// to be thrown away, and a leftover `working` would come back out as
	// `changing` on a shell that is doing nothing.
	if agent != "" {
		if rep, ok := standingReport(ctx, c, row.PaneID, now); ok {
			switch rep.State {
			case StateWorking:
				// In force AT exactly workingTTL and expired past it, which is
				// the comparison Reports.Observe makes. The same pane must not
				// read working through the browser and idle through a
				// subcommand a millisecond apart.
				if now.Sub(time.UnixMilli(rep.Timestamp)) <= workingTTL {
					return Verdict{State: StateWorking, From: VerdictReport}, nil
				}
				// Expired. working asserts that something is happening NOW and
				// nothing re-asserted it, so a crashed agent must not read
				// busy; the screen is the only authority left.
			case StateBlocked:
				return corroborateBlocked(ctx, c, agent, row.PaneID, rep)
			case StateIdle:
				// An idle report earns nothing here. It is a resting state
				// that reports.go only believes once the classifier has agreed
				// within NIdle polls, and a single call has no window to open,
				// so the screen answers unaided. It is also the cheap mistake:
				// a wrongly idle pane costs one wasted screen read, where a
				// wrongly blocked one costs a read per pass forever.
			}
		}
	}
	return fromScreen(ctx, c, agent, row.PaneID, settle)
}

// corroborateBlocked spends one capture on a standing blocked report.
//
// One capture and no settle wait: a dialog is either on screen or it is not,
// and stillness is not the question -- a held box is byte-identical anyway.
func corroborateBlocked(ctx context.Context, c LiveReader, agent, paneID string, rep Report) (Verdict, error) {
	screen, err := c.Capture(ctx, paneID)
	if err != nil {
		return Verdict{State: LiveUnknown}, err
	}
	if !IsBlocked(agent, screen) {
		// Including every agent that has no registered form at all: FormsFor's
		// stance is that an agent whose waiting screen nobody has captured can
		// never report blocked, and guessing from a report alone is the badge
		// failure that stance exists to prevent.
		//
		// No churn follows, and that is NOT the contradiction with
		// CorroborateBlocked's "a dropped report hands the pane back to the
		// grammars on this same capture" that it looks like: the hand-back
		// already happened one line up -- the grammar was asked about this very
		// capture and said no. What is skipped is only the second capture and
		// the settle wait, because the table asks for one capture here, and
		// Dropped is what tells the caller this idle rests on it. The poller
		// can afford the pair; it is already capturing every 1.5s. A single
		// call would be buying a working/idle distinction nobody asked for with
		// a fork and a wait, when the question put to it was whether anything
		// is waiting on the owner.
		return Verdict{State: StateIdle, From: VerdictScreen, Dropped: true}, nil
	}
	v := Verdict{State: StateBlocked, From: VerdictReport}
	if rep.Activity != "" {
		// The report's own words win over the grammar's reading of the same
		// screen: the integration named what it is waiting on, where a grammar
		// can only recover an approximation of it from the pixels.
		v.Question = &Question{Text: rep.Activity}
		v.QuestionFrom = VerdictReport
	} else {
		// A state-only report still has a question on screen -- `state` prints
		// the question when there is one, and on this path there is one -- and
		// the capture that corroborated the report is already in hand, so
		// asking the grammar costs no fork. ExtractQuestion fails closed, so
		// the field stays nil when it cannot read the dialog with confidence.
		v.Question = ExtractQuestion(agent, screen)
		if v.Question != nil {
			v.QuestionFrom = VerdictScreen
		}
	}
	// From is VerdictReport either way. It names what decided the STATE, and on
	// this branch that is the report; QuestionFrom says who supplied the text.
	return v, nil
}

// fromScreen answers from captures alone: the dialog first, then churn.
func fromScreen(ctx context.Context, c LiveReader, agent, paneID string, settle time.Duration) (Verdict, error) {
	prev, err := c.Capture(ctx, paneID)
	if err != nil {
		return Verdict{State: LiveUnknown}, err
	}
	// The dialog is checked BEFORE churn because churn can never see one: a
	// held permission box is byte-identical capture after capture, which is
	// exactly the shape stillness has -- the reason Classifier.Observe has to
	// be TOLD whether a pane is blocked. Asked in the other order, every agent
	// waiting on the owner reads idle, which is the verdict that stops the
	// owner's loop from ever looking at the one pane that needs it.
	if agent != "" && IsBlocked(agent, prev) {
		// ExtractQuestion fails closed and may return nil: a grammar that
		// stopped matching a restyled dialog must cost the quote, never the
		// state.
		q := ExtractQuestion(agent, prev)
		qFrom := ""
		if q != nil {
			qFrom = VerdictScreen
		}
		return Verdict{State: StateBlocked, Question: q, QuestionFrom: qFrom, From: VerdictScreen}, nil
	}
	// settleAfter identical captures mean still -- the same number the
	// classifier uses for the same claim, so a pane cannot read still through
	// the browser at one threshold and through a subcommand at another. Only
	// the GAP between them is local: the classifier's is the poll interval,
	// and this one is the caller's.
	for i := 1; i < settleAfter; i++ {
		if settle > 0 {
			select {
			case <-ctx.Done():
				return Verdict{State: LiveUnknown}, ctx.Err()
			case <-time.After(settle):
			}
		}
		next, err := c.Capture(ctx, paneID)
		if err != nil {
			// Never compared against what came before it. An unreadable screen
			// is not a still screen, and reading it as one would be the same
			// false idle the dialog check above exists to prevent.
			return Verdict{State: LiveUnknown}, err
		}
		if next != prev {
			if agent != "" {
				return Verdict{State: StateWorking, From: VerdictScreen}, nil
			}
			return Verdict{State: LiveChanging, From: VerdictScreen}, nil
		}
		prev = next
	}
	// A dialog raised between two captures reads working rather than blocked,
	// and that is left alone: it is the absorbable direction -- the next call a
	// few minutes later sees a screen that has stopped moving and asks the
	// grammar about it.
	if agent != "" {
		return Verdict{State: StateIdle, From: VerdictScreen}, nil
	}
	return Verdict{State: LiveQuiet, From: VerdictScreen}, nil
}

// standingReport reads one pane's own @tmux_web_agent value.
//
// -p, pane-scoped, as cmd/tmux-web's re-assertion read is: unlike the daemon's
// #{@tmux_web_agent} format read it does NOT walk up to the window, session and
// global options, and nothing this project installs writes at those scopes.
//
// A failed read is NO REPORT and never LiveUnknown. `show-options -v` on an
// unset user option exits 1, so the error is the ordinary shape of a pane with
// no integration installed -- and there is a screen to fall back to. A failed
// CAPTURE is unknown because there the screen was the whole of the evidence.
func standingReport(ctx context.Context, c LiveReader, paneID string, now time.Time) (Report, bool) {
	out, err := c.Run(ctx, "show-options", "-p", "-t", paneID, "-v", AgentOption)
	if err != nil {
		return Report{}, false
	}
	return ParseReport(out, now)
}
