package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/diegok/tmux-web/internal/tmux"
)

// The verbs an agent running inside a pane gets, so that several agents can see
// what each other are doing and act on them.
//
// They fork tmux directly and never talk to the daemon, which is `report`'s
// exception (cli.go) applied to more verbs for the same three reasons: the
// authorization is tmux's own -- whoever can drive the socket can already run
// these commands by hand -- the pane's own option and screen are the state, so
// there is nothing a daemon knows that these cannot read, and an agent calling
// them on a machine with no tmux-web running still gets an answer. The one
// thing they do NOT inherit is `report`'s always-exit-0 rule: that exists
// because a reporting failure must never stop an agent mid-turn, where here the
// caller is waiting on the answer and a silent failure would be read as a fact
// about the pane. So these follow the CLI's convention -- 0 success, 1 a failed
// operation, 2 a malformed command line.

// controlClient is the little of tmux.Client these subcommands use.
//
// Injected from the first version, as report.go's tmuxRunner was and for the
// same reason stated more sharply: six later verbs land in this file, and what
// each of them must be tested on is the CALLS IT MAKES -- a listing that reads
// no screens, a `type` that refuses a pane rather than sending to it, a `run`
// that opens before it types. None of that is visible in the output, so every
// one of those tests needs a recording stub in this position, and a hardcoded
// tmux.NewClient(...) here would make each of them refactor this one first.
//
// It is also a superset of tmux.LiveReader -- Run and Capture, with the same
// signatures -- so `state` hands this very client to LiveState without an
// adapter.
type controlClient interface {
	Run(ctx context.Context, args ...string) (string, error)
	Snapshot(ctx context.Context) ([]tmux.Row, error)
	Capture(ctx context.Context, paneID string) (string, error)
	CaptureRange(ctx context.Context, paneID string, lines int) (string, bool, error)
	NewWindow(ctx context.Context, sessionID, name, fromPane string) (string, error)
	SplitPane(ctx context.Context, paneID, direction string) (string, error)
	SetLabel(ctx context.Context, paneID, label string) error
	KillPane(ctx context.Context, paneID string) error
}

// The LiveReader claim above, held by the compiler rather than by the comment:
// `state` hands this very interface to tmux.LiveState, and a later edit that
// renamed Capture or gave Run a different signature would otherwise be found by
// whoever was mid-task on that verb.
var _ tmux.LiveReader = controlClient(nil)

// dialControl is the production dial: one client per socket, built after the
// environment has been read, because the socket comes from $TMUX.
func dialControl(socket string) controlClient { return tmux.NewClient([]string{"-S", socket}) }

// controlTimeout bounds one verb, every fork and every deliberate wait inside
// it included.
//
// Twice `report`'s 5s, and the extra is not generosity: a report writes one
// option in a couple of milliseconds, while these verbs fork two or three times
// and one of them -- `state` -- waits between captures on purpose. The bound is
// here for the same reason report's is, though: every one of these runs inside
// something an agent is waiting on, so a wedged tmux server must make the
// command return rather than hold a turn open indefinitely. A verb that grows a
// wait has to fit it inside this, not widen it.
const controlTimeout = 10 * time.Second

// paneFieldSep separates the fields of one `panes` record.
//
// Two spaces, and no alignment padding: this listing is grepped, cut and read
// by an agent, and a padded column moves every time a neighbour's value gets
// longer. Two rather than one because a session name and a title may each
// contain a space, so a single space would not look like a boundary at all --
// and two do not make it parseable either, which is why the pane id a verb
// takes is the FIRST field and the two free-form ones are last.
const paneFieldSep = "  "

// maxPaneField bounds one printed field.
//
// It re-caps nothing in practice: tmux's own values arrive bounded -- the title
// at MaxTitle by ParseRows, the label at MaxLabel by sanitizeLabel -- and this
// is the widest of those. What the trip through `display` is actually for is
// the other two things neither of those bounds promises: a non-printable rune
// becomes a visible '?', because a title is rewritten constantly by whatever
// runs in the pane and an ANSI escape in one could hide the row above it; and
// an empty value becomes "-", so a record always has every field and a reader
// counting columns is never handed a short line.
const maxPaneField = tmux.MaxTitle

func cmdPanes(args []string, stdout, stderr io.Writer) int {
	return runPanes(args, stdout, stderr, os.Getenv, dialControl)
}

// runPanes is cmdPanes with the environment and the tmux client injected, so a
// test can drive it without setting process-wide variables -- which would race
// every other test in the package and, if $TMUX leaked through, would act on
// the developer's live tmux session.
func runPanes(args []string, stdout, stderr io.Writer, getenv func(string) string, dial func(socket string) controlClient) int {
	fset := newFlagSet("panes", stderr, "tmux-web panes [--all]")
	all := fset.Bool("all", false, "every session on the server, not just the one this pane is in")
	operands, code, ok := parseFlags(fset, args)
	if !ok {
		return code
	}
	if len(operands) > 0 {
		// Not a filter and not a target: `panes` answers "what is there", and
		// the verb that answers "what about this one" is the next line.
		return usageError(stderr, fset, "panes takes no positional arguments, got %q (to ask about one pane: tmux-web state %s)", operands[0], operands[0])
	}
	// Both variables, including under --all, which never looks at the pane id:
	// a listing that worked outside a tmux pane while every other verb here
	// refused would be a difference nobody could act on.
	socket, pane, ok := tmuxTarget(getenv)
	if !ok {
		return notInsideTmux(stderr, "panes")
	}
	ctx, cancel := context.WithTimeout(context.Background(), controlTimeout)
	defer cancel()
	c := dial(socket)

	rows, err := c.Snapshot(ctx)
	if err != nil {
		return controlFail(stderr, "panes", err)
	}
	if !*all {
		// The caller's session is the one the OWNER keeps, not the _web-*
		// session a browser tab added over the same pane; CallerSession is that
		// preference and it is not re-derived here. It is answered from the
		// snapshot already in hand, so the scoping costs no second fork.
		session, err := tmux.CallerSession(ctx, snapshotOf(rows), pane)
		if err != nil {
			return controlFail(stderr, "panes", err)
		}
		rows = inSession(rows, session)
	}
	// Dedupe has already ordered the rows the way the owner sees them -- group,
	// then window, then pane index -- and this prints them in that order rather
	// than sorting again by anything of its own: a listing an agent quotes to
	// the owner must be in the order on his screen.
	states := freeStates(ctx, c, rows, time.Now(), stderr)
	for _, r := range rows {
		fmt.Fprintln(stdout, paneLine(r, states[r.PaneID]))
	}
	return 0
}

// paneLine is one record: the pane id a verb takes, the address a human reads,
// what is running, whether that is an agent, the free state, the owner's label
// and the pane's title.
//
// The agent column is KnownAgent(command), which today is either the command
// repeated or "-". It is still a column of its own because it answers a
// different question: WHICH VOCABULARY the state column speaks. `state` answers
// a known agent with working/idle/blocked and anything else with
// changing/quiet, and the Agents list decides which -- not the reader, who
// would otherwise have to know that "pi" is an agent and "python" is not.
func paneLine(r tmux.Row, state string) string {
	if state == "" {
		state = "-"
	}
	return strings.Join([]string{
		r.PaneID,
		paneAddress(r),
		field(r.Command),
		field(tmux.KnownAgent(r.Command)),
		state,
		field(r.Label),
		field(r.Title),
	}, paneFieldSep)
}

// paneAddress is session:window.pane, which is how the owner finds a pane on
// his own screen and how tmux's own status line names one.
//
// It is NOT a target: tmux prefix-matches session and window names without
// reporting the ambiguity, and the indices are positions that renumbering moves
// (snapshot.go says so of WindowIndex). Everything here takes the %N instead.
// The name goes through `field` for the same reason the title does -- a session
// name may contain anything the owner typed, spaces included.
func paneAddress(r tmux.Row) string {
	return field(r.SessionName) + ":" + strconv.Itoa(r.WindowIndex) + "." + strconv.Itoa(r.PaneIndex)
}

// field makes one value safe to print and never empty. See maxPaneField.
func field(s string) string { return display(s, maxPaneField) }

// freeStates is the state this listing gets for free.
//
// THE INVARIANT, and it is the one sentence to check this function against:
// `panes` PRINTS ONLY WHAT `state` WOULD ANSWER WITHOUT A FORK. That is row 1
// of LiveState's own rules table and nothing else -- a parseable `working`
// report younger than workingTTL, which LiveState trusts outright and captures
// nothing for. Every other pane prints "-", which means "ask `state`" and never
// "nothing is happening".
//
// WHY IT IS NOT "the report-derived state", which is the obvious wider reading
// and the one a later reader will be tempted to restore. Two verbs disagreeing
// about the same pane in the same second is an anti-goal of this design, and
// both resting states reach it:
//
//   - A standing `blocked` report whose screen no longer holds a dialog.
//     `state` corroborates such a report with one capture and answers `idle`
//     with Dropped set; a listing that printed `blocked` would contradict it
//     outright, on precisely the value reports.go calls out as the one most
//     likely to be wrong ("a blocked report costs a fork per poll -- the case
//     the app exists for is exactly the one where the report is wrong").
//   - An `idle` report. LiveState gives it nothing and asks the screen, whose
//     two captures can come back `working`.
//
// Restricted to row 1, the two verbs are not unlikely to disagree, they are
// unable to: the one state printed here is the one state `state` reaches
// without looking at anything this listing did not also look at.
//
// ONE FORK FOR THE WHOLE LISTING, and no captures at all. LiveState per pane
// would be two or three forks each plus a settle wait, on a server with a dozen
// panes, and a verb that is multi-second is a verb an agent stops running.
//
// Reports.Observe does the reading, on a memory that has seen nothing, which is
// exactly a first sight: it drops the value on a pane whose command is no
// longer a known agent (the agent exited and the option outlived it), it drops
// anything it cannot parse, and it expires a `working` report at workingTTL.
// Asking THAT function rather than re-implementing its rules is what keeps this
// listing from disagreeing with the poller about the same option, and the TTL
// from existing twice.
func freeStates(ctx context.Context, c controlClient, rows []tmux.Row, now time.Time, stderr io.Writer) map[string]string {
	states := make(map[string]string, len(rows))
	// -a reads every pane on the server, including the ones outside the scope
	// being printed. It is the same single fork either way: tmux takes no list
	// of panes, and a per-pane show-options would be one fork per agent pane.
	out, err := c.Run(ctx, "list-panes", "-a", "-F", tmux.ReportFormat)
	if err != nil {
		// A line on stderr, where the commentary goes, and the listing still
		// prints: a lost report costs a pane its state column, and a reader
		// looking at a wall of dashes deserves to know that none of them is a
		// claim about the pane.
		fmt.Fprintf(stderr, "tmux-web panes: could not read the panes' reports, so every state is \"-\": %v\n", err)
		return states
	}
	raw := tmux.ParseReports(out)
	reports := tmux.NewReports()
	for _, r := range rows {
		rep, ok := reports.Observe(r.PaneID, raw[r.PaneID], r.Command, now)
		// The invariant, in one condition: in force AND working. Observe has
		// already applied the clock, so `working` here means younger than
		// workingTTL; the two resting states are in force and still print "-",
		// because believing one costs the capture this verb does not make.
		if ok && rep.State == tmux.StateWorking {
			states[r.PaneID] = rep.State
		}
	}
	return states
}

// snapshotOf answers CallerSession from a snapshot already taken.
//
// CallerSession takes a Snapshotter so that a test can count its forks; this is
// the production reason the seam is worth having. The listing has just read
// every pane on the server, and asking the client again would fork a second
// list-panes to learn something the rows in hand already say.
type snapshotOf []tmux.Row

func (rows snapshotOf) Snapshot(context.Context) ([]tmux.Row, error) { return rows, nil }

// inSession keeps the rows belonging to one session id.
//
// By SessionID and not by name or by group: a group key is the pre-rename name
// and a name is not an address, while the id is what the caller was resolved to.
func inSession(rows []tmux.Row, sessionID string) []tmux.Row {
	kept := make([]tmux.Row, 0, len(rows))
	for _, r := range rows {
		if r.SessionID == sessionID {
			kept = append(kept, r)
		}
	}
	return kept
}

// notInsideTmux is the refusal every verb here shares.
//
// It names both variables, because an agent started outside tmux has nothing in
// its environment that would tell it which one is missing, and because the fix
// differs: no $TMUX at all means this is not a tmux pane, while a $TMUX_PANE
// that failed validation means something in between has rewritten it.
//
// Exit 1 rather than `report`'s 0: there is an answer the caller is waiting for
// and it was not given, so a success here would be read as "no panes".
func notInsideTmux(stderr io.Writer, verb string) int {
	fmt.Fprintf(stderr, "tmux-web %s: this only works inside tmux -- it reads the server's socket from $TMUX and the calling pane's id from $TMUX_PANE, and one of them is missing or malformed\n", verb)
	return 1
}

// controlFail reports a failed operation: exit 1, with the verb named so that a
// message read out of an agent's transcript says which command produced it.
func controlFail(stderr io.Writer, verb string, err error) int {
	fmt.Fprintf(stderr, "tmux-web %s: %v\n", verb, err)
	return 1
}
