package main

import (
	"context"
	"flag"
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
	// The DETACHED window and split verbs, and not their siblings: every pane
	// these subcommands create is created without moving the owner's view, so
	// the attached pair -- which is what the browser's dialogs drive -- is not
	// on this interface at all.
	NewWindowDetached(ctx context.Context, sessionID, name, fromPane string) (string, error)
	SplitPaneDetached(ctx context.Context, paneID, direction string) (string, error)
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
// longer.
//
// WHAT IS POSITIONAL AND WHAT IS NOT, exactly, because five verbs and the skill
// that teaches them quote this:
//
//   - FIELDS 1 TO 5 -- pane id, address, command, agent, state -- are
//     positional. Every one of them is a value whose spelling this file controls
//     or tmux generates: an id, an address with its whitespace collapsed (see
//     paneAddress), a command name, a word from the Agents list, a word from
//     ParseReport's three. None can contain a space, so `awk '{print $5}'` reads
//     the state of every pane on the server.
//   - THE TRAILING label AND title ARE FOR READING, NOT PARSING. Both are
//     free-form -- the owner types the label, and whatever runs in the pane
//     rewrites the title -- so either may contain a space, two spaces, or
//     something that looks exactly like a field boundary. A parser cannot split
//     that trailing pair unambiguously and must not try: everything a verb needs
//     is in the first five.
//
// The separator being two spaces rather than one is what makes the boundary
// visible to a person in the trailing pair as well; it is not what makes the
// first five safe. Collapsing the address is.
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

// stateSettle is the gap `state` leaves between its two captures.
//
// 1.5s, which is the daemon's poll interval (front.DefaultPollInterval in
// internal/front/server.go; not imported, to keep this file off that package),
// and that is the reason rather than
// a coincidence: LiveState borrows settleAfter from the classifier to decide
// how many identical captures mean "still", and that count was chosen for
// captures 1.5s apart. A shorter gap would let a pane that redraws once a
// second -- a spinner, a status line ticking -- read still through this verb
// and working through the browser, which is a disagreement about the same pane
// that no test of either would find. A longer one buys nothing the classifier
// does not already have and is paid for by an agent mid-turn.
//
// Budget: one gap, two captures and a show-options read. The forks are
// milliseconds, so the verb is about 1.5s of a controlTimeout of 10s, and a
// slow server has the other 8s before the deadline cuts the wait short.
//
// It is passed to LiveState for every pane, but only the screen path waits: a
// fresh `working` report returns before the first capture, and a blocked one
// takes one capture and no gap. Nothing here may change that -- `panes` prints
// only what `state` answers without a fork, and a corroboration added on top
// of the report would make the two verbs disagree.
const stateSettle = 1500 * time.Millisecond

func cmdState(args []string, stdout, stderr io.Writer) int {
	return runState(args, stdout, stderr, os.Getenv, dialControl)
}

// runState answers what one pane is doing now: one word on the first line, and
// for a blocked pane the question after it.
//
// The pane is an operand and is named, so the caller's-session default that
// scopes `panes` does not apply: %7 in another session is as explicit a
// target as %1, and refusing it would stop the one thing this verb is for,
// which is one agent asking about another.
func runState(args []string, stdout, stderr io.Writer, getenv func(string) string, dial func(socket string) controlClient) int {
	fset := newFlagSet("state", stderr, "tmux-web state <pane-id>")
	operands, code, ok := parseFlags(fset, args)
	if !ok {
		return code
	}
	if len(operands) != 1 {
		return usageError(stderr, fset, "state takes exactly one pane id (see tmux-web panes), got %d arguments", len(operands))
	}
	target := operands[0]
	// Before the environment and before any fork: tmux resolves a malformed
	// -t to whatever is current and exits 0, so a typo here would be answered
	// confidently about some other pane.
	if err := tmux.ValidatePaneID(target); err != nil {
		return usageError(stderr, fset, "%v -- pass the %%N from tmux-web panes, not a session:window.pane", err)
	}
	socket, _, ok := tmuxTarget(getenv)
	if !ok {
		return notInsideTmux(stderr, "state")
	}
	ctx, cancel := context.WithTimeout(context.Background(), controlTimeout)
	defer cancel()
	c := dial(socket)

	rows, err := c.Snapshot(ctx)
	if err != nil {
		return controlFail(stderr, "state", err)
	}
	// The row, and not just the id, because LiveState decides by the pane's
	// command which vocabulary it speaks. A pane absent from the snapshot is
	// refused here with its own message: asking tmux about it would put tmux's
	// stderr in the agent's transcript instead of a sentence naming the pane.
	var row *tmux.Row
	for i := range rows {
		if rows[i].PaneID == target {
			row = &rows[i]
			break
		}
	}
	if row == nil {
		return controlFail(stderr, "state", fmt.Errorf("no pane %s on this tmux server (list them with tmux-web panes --all)", target))
	}

	v, err := tmux.LiveState(ctx, c, *row, time.Now(), stateSettle)
	if err != nil {
		// The word still goes to stdout: a reader that takes the first line
		// gets "unknown", the one thing that must never be mistaken for quiet.
		fmt.Fprintln(stdout, v.State)
		return controlFail(stderr, "state", err)
	}
	fmt.Fprintln(stdout, v.State)
	switch {
	case v.Dropped:
		fmt.Fprintln(stdout, "note: a blocked report was standing and no dialog is on screen, so it was dropped; this idle rests on one capture")
	case v.State == tmux.StateBlocked:
		// The label names where the TEXT came from (QuestionFrom), not what
		// decided the state: a state-only report corroborated by a screen
		// has a question a grammar read off the pixels, and calling that
		// "from the report" would let an agent quote a guess as the
		// integration's own statement. When the two differ both are said.
		switch {
		case v.Question == nil:
			fmt.Fprintf(stdout, "from the %s:\n", v.From)
		case v.QuestionFrom == v.From:
			fmt.Fprintf(stdout, "from the %s:\n", v.From)
		default:
			fmt.Fprintf(stdout, "state from the %s, question read from the %s:\n", v.From, v.QuestionFrom)
		}
		if q := v.Question; q != nil {
			fmt.Fprintln(stdout, display(q.Text, maxQuestion))
			// Numbered here because the grammar hands the choices back with
			// their numbers stripped, and the number is what the agent that
			// answers the dialog will type.
			for i, ch := range q.Choices {
				fmt.Fprintf(stdout, "  %d. %s\n", i+1, display(ch, maxQuestion))
			}
		} else {
			fmt.Fprintln(stdout, "(no question could be read from the pane)")
		}
	}
	return 0
}

func cmdPeek(args []string, stdout, stderr io.Writer) int {
	return runPeek(args, stdout, stderr, os.Getenv, dialControl)
}

// runPeek prints what one pane shows: its visible screen, or with --lines N
// that many lines of scrollback behind it.
//
// What comes out is text another program wrote and it can say anything,
// instructions included. It is printed as it is, deliberately not filtered
// (unlike `state`, whose lines are cut down to one printable line each): a
// screen is multi-line and its layout is the content. Nothing here makes it
// safe to obey; it is data, never instructions.
//
// As in `state`, the pane is a named operand, so another session's pane is as
// legitimate a target as one's own.
func runPeek(args []string, stdout, stderr io.Writer, getenv func(string) string, dial func(socket string) controlClient) int {
	fset := newFlagSet("peek", stderr, "tmux-web peek [--lines N] <pane-id>")
	lines := fset.Int("lines", 0, "lines of scrollback to read behind the visible screen (default: the visible screen only)")
	operands, code, ok := parseFlags(fset, args)
	if !ok {
		return code
	}
	if len(operands) != 1 {
		return usageError(stderr, fset, "peek takes exactly one pane id (see tmux-web panes), got %d arguments", len(operands))
	}
	// Zero is "not given", so a typed 0 or a negative must be refused: either
	// would otherwise fall back to the bare screen or be clamped to one line,
	// and answer a question the caller did not ask.
	given := false
	fset.Visit(func(*flag.Flag) { given = true })
	if given && *lines < 1 {
		return usageError(stderr, fset, "--lines must be at least 1, got %d", *lines)
	}
	target := operands[0]
	if err := tmux.ValidatePaneID(target); err != nil {
		return usageError(stderr, fset, "%v -- pass the %%N from tmux-web panes, not a session:window.pane", err)
	}
	socket, _, ok := tmuxTarget(getenv)
	if !ok {
		return notInsideTmux(stderr, "peek")
	}
	ctx, cancel := context.WithTimeout(context.Background(), controlTimeout)
	defer cancel()
	c := dial(socket)

	// Checked against the snapshot for the reason `state` does: a missing pane
	// gets a sentence naming it, not tmux's own stderr in a transcript.
	rows, err := c.Snapshot(ctx)
	if err != nil {
		return controlFail(stderr, "peek", err)
	}
	found := false
	for i := range rows {
		if rows[i].PaneID == target {
			found = true
			break
		}
	}
	if !found {
		return controlFail(stderr, "peek", fmt.Errorf("no pane %s on this tmux server (list them with tmux-web panes --all)", target))
	}

	if *lines == 0 {
		text, err := c.Capture(ctx, target)
		if err != nil {
			return controlFail(stderr, "peek", err)
		}
		fmt.Fprint(stdout, text)
		return 0
	}
	text, truncated, err := c.CaptureRange(ctx, target, *lines)
	if err != nil {
		return controlFail(stderr, "peek", err)
	}
	fmt.Fprint(stdout, text)
	if truncated {
		// On stderr with exit 0: the verb did its job and the text is on
		// stdout, so this is not a failure, and a note on stdout would end up
		// inside a file the caller redirected it to. It must be said at all
		// because the cut is from the top: a reader that does not know the
		// start is missing treats a dialog's tail as the whole of it and
		// answers the wrong question.
		fmt.Fprintf(stderr, "tmux-web peek: output truncated to the last %d bytes; the start of the %d lines requested is missing (ask for fewer lines)\n", tmux.MaxCaptureBytes, *lines)
	}
	return 0
}

// maxQuestion bounds one printed line of a question or a choice. Screen text is
// untrusted in the way every pane's content is, and display is what keeps an
// escape sequence in it from being replayed on the caller's terminal; the cap
// is generous because a dialog's wrapped line is the width of a screen.
const maxQuestion = 500

func cmdOpen(args []string, stdout, stderr io.Writer) int {
	return runOpen(args, stdout, stderr, os.Getenv, dialControl)
}

// runOpen makes an empty pane beside the agent's work and prints its id.
//
// AN EMPTY SHELL, AND NOTHING IS RUN IN IT. That is what the two tmux verbs
// underneath are for -- "new window means an empty shell to all four of its
// callers" (internal/tmux/manage.go) -- and it is what makes this verb worth
// having: a command the owner can see, scroll back through and re-run is a
// command in a pane, where a command hidden in a background task is a thing he
// is told about afterwards. Nothing here hands tmux a command, and nothing
// later may: `run` is this verb followed by typing the command into the shell
// it created.
//
// BOTH MODES CREATE THE PANE DETACHED, which is why they call the Detached
// siblings. tmux's default is to make the new window or pane current, so the
// plain verbs would move the owner's terminal off whatever he was reading the
// moment another agent opened something -- a default, not a choice, and the
// first thing this design set out not to do. The flag is asserted in
// control_integration_test.go, which is the only place it can be seen.
//
// stdout carries the new pane id and nothing else, so `$(tmux-web open
// --split)` is a target the other verbs take.
func runOpen(args []string, stdout, stderr io.Writer, getenv func(string) string, dial func(socket string) controlClient) int {
	fset := newFlagSet("open", stderr, "tmux-web open (--split | --window [name]) [--label TEXT]")
	split := fset.Bool("split", false, "put the new pane beside the calling pane, in the same window")
	window := fset.Bool("window", false, "put the new pane in a new window of the session this pane is in")
	label := fset.String("label", "", "a label for the new pane, shown by tmux-web panes and in the sidebar")
	operands, code, ok := parseFlags(fset, args)
	if !ok {
		return code
	}
	// Exactly one mode, and no default: the two differ in what the owner sees
	// -- a pane appears next to his work, a window does not -- and guessing
	// which he wanted is not this verb's call to make.
	if *split == *window {
		return usageError(stderr, fset, "choose exactly one of --split (a pane beside this one) or --window (a new window in this session)")
	}
	name := ""
	if len(operands) > 1 {
		return usageError(stderr, fset, "open takes at most one positional argument, the new window's name, got %d", len(operands))
	}
	if len(operands) == 1 {
		if *split {
			return usageError(stderr, fset, "open --split takes no positional arguments, got %q: a name belongs to a window (did you mean --window %s?)", operands[0], operands[0])
		}
		name = operands[0]
		// Validated here, before the environment and before any fork: a name
		// tmux would refuse is a malformed command line, which exits 2, and
		// finding that out from the server would cost a snapshot first and
		// report it as a failed operation.
		if err := tmux.ValidateWindowName(name); err != nil {
			return usageError(stderr, fset, "%v", err)
		}
	}
	socket, caller, ok := tmuxTarget(getenv)
	if !ok {
		return notInsideTmux(stderr, "open")
	}
	ctx, cancel := context.WithTimeout(context.Background(), controlTimeout)
	defer cancel()
	c := dial(socket)

	pane, err := openPane(ctx, c, *split, caller, name)
	if err != nil {
		return controlFail(stderr, "open", err)
	}
	// The id goes out before the label is set, and it goes out even if that
	// fails below. The pane exists by now, and a caller that was not given its
	// id has leaked a pane it cannot name -- the same rule `state` follows when
	// it prints its word before reporting a read it could not finish.
	fmt.Fprintln(stdout, pane)
	if *label != "" {
		if err := c.SetLabel(ctx, pane, *label); err != nil {
			return controlFail(stderr, "open", fmt.Errorf("pane %s was opened but its label was not set: %w", pane, err))
		}
	}
	return 0
}

// openPane creates the pane and returns its id: a split of the caller's own
// pane, or the single pane of a new window in the caller's session.
func openPane(ctx context.Context, c controlClient, split bool, caller, name string) (string, error) {
	if split {
		// The CALLER's pane, from $TMUX_PANE, and never the window's active
		// one: tmux's default target is whatever is current, which in a window
		// the owner is working in is somebody else's pane.
		//
		// Rightwards, with no flag to choose otherwise. "A pane next to this
		// one" is what this verb is for, and a direction no caller asks for is
		// a flag with nobody behind it; SplitPaneDetached takes tmux.SplitDown
		// just as well, so the day something wants it, it is a flag and not a
		// redesign.
		return c.SplitPaneDetached(ctx, caller, tmux.SplitRight)
	}
	// The session the OWNER keeps, not the _web-* session a browser tab added
	// over the same pane. CallerSession is that preference and it is not
	// re-derived here; it is answered from the snapshot already in hand, so the
	// window mode costs one fork rather than two.
	rows, err := c.Snapshot(ctx)
	if err != nil {
		return "", err
	}
	session, err := tmux.CallerSession(ctx, snapshotOf(rows), caller)
	if err != nil {
		return "", err
	}
	// Refused rather than opened, and this is not a matter of ownership
	// etiquette: an app-owned session is a browser tab's own and carries
	// destroy-unattached, so the window would be destroyed the moment the tab
	// closes. CallerSession answers such a session when it is the only one
	// holding the pane -- it says deciding whether that is good enough is the
	// caller's job, and this is that decision.
	//
	// ONLY THE WINDOW MODE ASKS, and not because a split is safe from the tab.
	// It is not: in exactly the case refused here -- an app-owned session as
	// the last member of its group -- that session is the only one linking the
	// caller's own window, so destroy-unattached takes the window and every
	// pane in it, a new split included.
	//
	// What makes the question pointless for a split is that the new pane SHARES
	// THE CALLER'S OWN FATE. It lands in the caller's window, so if that window
	// goes, the agent that asked for the pane goes with it and there is nobody
	// left to have been misled. A window is somewhere else: it dies while the
	// agent lives on, holding an id for a pane that no longer exists. That
	// asymmetry is the whole of why one mode checks and the other cannot need
	// to -- the saved fork is a consequence, not the reason.
	if app, owned := appSessionName(rows, session); owned {
		return "", fmt.Errorf("pane %s is only in session %s (%s), which belongs to a browser tab: it is torn down when that tab closes and a window opened in it would die with it while this pane carried on -- use --split, which puts the pane in this pane's own window, where it lives and dies with the pane asking for it",
			caller, session, app)
	}
	// fromPane is the caller's pane, which is what gives the new window the
	// directory the agent is working in -- resolved by tmux from the pane
	// itself, so no path crosses from here.
	windowID, err := c.NewWindowDetached(ctx, session, name, caller)
	if err != nil {
		return "", err
	}
	// A new window answers with a WINDOW id (#{window_id}) and every verb here
	// takes a pane, so the window's own pane has to be read back. Validated
	// before it becomes a target: an empty or malformed id is the one tmux
	// resolves to "whatever is current", which would print a pane this verb did
	// not create.
	if err := tmux.ValidateWindowID(windowID); err != nil {
		return "", fmt.Errorf("new window: %w", err)
	}
	pane, err := c.Run(ctx, "display-message", "-p", "-t", windowID, "#{pane_id}")
	if err != nil {
		return "", err
	}
	if err := tmux.ValidatePaneID(pane); err != nil {
		// display-message prints an empty expansion and exits 0 for a target it
		// cannot find, so an unusable answer has to be caught rather than
		// printed. The window is named, because it was created and is still
		// there for the owner to find.
		return "", fmt.Errorf("window %s was created but its pane could not be read: %w", windowID, err)
	}
	return pane, nil
}

// appSessionName returns the name of this session if it belongs to the app, and
// reports whether it does.
//
// By the option Row.AppOwned carries, never by the "_web-" name: that prefix is
// a convention the app follows and not a namespace it owns, so a session the
// owner called "_web-notes" is his and stays operable. It is the same identity
// check internal/tmux's own refusals make (refuseAppSession), read off a
// snapshot this verb already has instead of costing another fork.
func appSessionName(rows []tmux.Row, sessionID string) (string, bool) {
	for _, r := range rows {
		if r.SessionID == sessionID && r.AppOwned {
			return r.SessionName, true
		}
	}
	return "", false
}

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
//
// EVERY RUN OF WHITESPACE IN THE NAME BECOMES ONE "_", and that is what keeps
// fields 1 to 5 positional. A session name may contain spaces: ValidateSessionName
// refuses them only for the sessions tmux-web itself creates, and it refuses
// nothing about the ones that were already on the server -- `tmux new -s "my
// work"` is ordinary. Printed raw, that name adds a field to its own line, and
// `awk '{print $5}'` then reads a pane's COMMAND out of the state column while
// every other line reads correctly. A mangled name is loud; a column that is
// right eleven times out of twelve is not.
//
// The session ID would also have been unambiguous and is deliberately not used:
// "$3:0.0" orients nobody, and orienting the owner is this field's entire job.
// A name made unambiguous keeps that, and nothing is lost by it, because the
// thing a verb takes is the pane id in field 1.
func paneAddress(r tmux.Row) string {
	// Fields splits on every kind of unicode whitespace, which is wider than
	// the space that survives `field`. It is belt-and-braces: `field` has
	// already turned every non-printable rune, U+00A0 included, into a '?'
	// (unicode.IsPrint rejects it), so the only whitespace left to collapse is
	// the plain space, and a run of those is what this join exists for.
	name := strings.Join(strings.Fields(field(r.SessionName)), "_")
	if name == "" {
		// A name of nothing but whitespace, which tmux accepts and this would
		// otherwise render as a bare ":0.0".
		name = "-"
	}
	return name + ":" + strconv.Itoa(r.WindowIndex) + "." + strconv.Itoa(r.PaneIndex)
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
