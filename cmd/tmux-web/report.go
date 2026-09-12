package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/diegok/tmux-web/internal/report"
	"github.com/diegok/tmux-web/internal/tmux"
)

// report is what the three integrations run. It is the only subcommand that
// does not talk to the admin socket: the state lives in the pane, so a daemon
// restart loses nothing, and nothing here ever waits on a network.
//
// It exits 0 on every path, including a malformed command line -- which
// deliberately breaks this CLI's own convention that a usage error is exit 2.
// The reason is narrow and worth stating rather than generalising: for a Claude
// Code PreToolUse hook, exit code 2 specifically blocks the tool call (every
// other nonzero code is a non-blocking error and the action proceeds), and the
// distance between `exit 1` and `exit 2` is one character in a wrapper nobody
// will re-read. A reporting integration that can stop an agent from working is
// worse than no reporting integration.
func cmdReport(args []string, stdout, stderr io.Writer) int {
	return runReport(args, os.Stdin, stdout, stderr, os.Getenv, dialReal)
}

// tmuxRunner is the little of tmux.Client this subcommand uses.
//
// Injected from the first version rather than retrofitted. Task 14 adds a
// `show-options` read before a re-assertion write and tests it by counting the
// reads and the writes a given event makes -- which needs a recording stub in
// this position, and a hardcoded tmux.NewClient(...) here would force that task
// to refactor this one. It takes no new parameter then; only the stub changes.
type tmuxRunner interface {
	Run(ctx context.Context, args ...string) (string, error)
}

// dialReal is the production dial: one client per socket, built after the
// environment has been read, because the socket comes from $TMUX.
func dialReal(socket string) tmuxRunner { return tmux.NewClient([]string{"-S", socket}) }

// reportTimeout bounds the one tmux call. A set-option fork takes a couple of
// milliseconds; this exists so that a wedged tmux server makes the hook return
// rather than hold an agent's turn open. Every one of these events is delivered
// inside something the agent is waiting on -- pi and opencode await handlers
// with NO timeout at all -- so the integration also spawns and never waits.
const reportTimeout = 5 * time.Second

// maxPayloadBytes bounds what the integration form reads from stdin.
//
// WHAT IT IS FOR. A hook process that buffers whatever it is handed is a hook
// process that can be made to buffer a gigabyte, and this one runs inside
// something the agent is waiting on. So there is a ceiling. What the ceiling is
// NOT is a bound on a real payload: everything over it is refused (see
// readPayload), and a refusal is a badge that never appears.
//
// THE MEASUREMENT. Of the thirty-six recorded payloads in testdata/hooks the
// largest is 1253 bytes; the median is under 550. The one unbounded field is
// opencode's permission.asked `metadata.diff` for an edit, a full unified diff,
// and the recorded sample of that is 775 bytes whole. The field's real ceiling
// is not the file's size but the MODEL'S OUTPUT: an edit's diff is the changed
// hunks plus context, so a whole-file rewrite at the top of a 64k-token output
// budget is on the order of 256 KB of text, and a diff carrying both the - and
// the + side of it perhaps twice that. Half a megabyte is therefore the largest
// payload anything here can honestly claim to have reasoned about.
//
// THE NUMBER. 4 MiB: eight times that worst case, three thousand times the
// largest payload ever recorded, and the same cap cli.go already puts on the
// admin socket's JSON. The old 1 MiB was inside the range a real edit can
// reach, which is what made the hole reachable rather than theoretical.
//
// WHAT IT COSTS. Nothing in the ordinary case -- the cap is a ceiling on
// allocation, not a reservation, and a 550-byte payload allocates 550 bytes.
// In the pathological case a short-lived process buffers 4 MiB and the parsers
// copy the big string once or twice more; tens of megabytes, for milliseconds,
// on a path that is already refusing the payload.
const maxPayloadBytes = 4 << 20

// runReport is cmdReport with the environment and the tmux client injected, so
// a test can drive it without setting process-wide variables -- which would
// race every other test in the package and, if $TMUX leaked through, would
// write into the developer's live tmux session.
func runReport(args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string, dial func(socket string) tmuxRunner) int {
	fset := newFlagSet("report", stderr,
		"tmux-web report --state working|blocked|idle [--text TEXT]\n"+
			"   or: tmux-web report --agent claude|opencode|pi --event NAME   (payload on stdin)")
	state := fset.String("state", "", "working, blocked or idle")
	text := fset.String("text", "", "what the agent is doing; omitted for a state-only report")
	agent := fset.String("agent", "", "the agent whose event this is; requires --event")
	event := fset.String("event", "", "the agent's own name for the event; requires --agent")
	if _, _, ok := parseFlags(fset, args); !ok {
		return 0 // see the comment on cmdReport
	}
	// Stamped from the moment this process starts, never from when set-option
	// returned. The timestamp means "when the agent entered this state", and
	// stamping at write time would reintroduce exactly the reordering the
	// daemon's ordering filter exists to undo.
	ms := time.Now().UnixMilli()

	// The two forms, and the rule between them. --state (with its optional
	// --text) is the manual form a person or a script uses: the state is taken
	// literally, no table is consulted and stdin is not read. --agent with
	// --event is the integration form: the table decides.
	//
	// Half of either form is a usage error, and so is both forms at once. The
	// second refusal is the one worth spelling out: it is refused rather than
	// given a precedence, because a precedence is a rule somebody has to
	// remember and neither caller has any reason to send both. A silent winner
	// would be a wrong state written from a hook that was passing an argument
	// it believed was doing something.
	manual, integration := *state != "" || *text != "", *agent != "" || *event != ""
	switch {
	case manual && integration:
		fmt.Fprintf(stderr, "tmux-web report: --state/--text and --agent/--event are two different callers; pass one form or the other\n")
		return 0
	case integration && (*agent == "" || *event == ""):
		fmt.Fprintf(stderr, "tmux-web report: --agent and --event are meaningless apart; pass both\n")
		return 0
	}

	// Whether this write has to read the standing report first. The manual
	// form is always an edge: a person or a script typing --state means it,
	// there is no table entry to carry a kind, and inferring one from the state
	// would make `report --state idle` silently do nothing on a pane that is
	// already idle.
	reassert := false
	// And, for a re-assertion, whether its write is dated from the standing
	// report rather than from now. Meaningless unless reassert; false for the
	// manual form for the same reason as above, and because a person typing
	// --state is describing the pane at the moment they type.
	fromStanding := false

	if integration {
		payload, overCap := readPayload(stdin)
		// The subagent filter, before the table. A report must only ever
		// describe the ROOT session in its pane: TMUX_PANE is the same for a
		// root session and its children on all three agents, so an unfiltered
		// child event overwrites the root's state, and the specific damage is
		// a child's turn end firing while the root is still working -- a false
		// done badge, which is the failure v2 spent most of its complexity
		// avoiding.
		//
		// It runs first because it is a claim about the PAYLOAD rather than
		// about the event: a payload this subcommand could not read is not
		// evidence that anything happened, whatever event name the caller put
		// on the command line.
		if usable, why := payloadIsUsable(payload, overCap); !usable {
			// Worth a line, unlike an ignored notification_type. This is a
			// subagent whose integration should not have spawned us, a payload
			// that did not arrive, or one that did not fit -- and all three are
			// things somebody debugging a missing badge needs to see, which is
			// why `why` says which.
			fmt.Fprintf(stderr, "tmux-web report: refusing this %s %s payload: %s\n", *agent, *event, why)
			return 0
		}
		m, known := report.Lookup(*agent, *event, payload)
		if !known {
			// A misconfigured integration, which is worth a line: an event
			// name nobody recognises will never report anything, and silence
			// would make that indistinguishable from an agent that is idle.
			fmt.Fprintf(stderr, "tmux-web report: nothing known about %q's %q event\n", *agent, *event)
			return 0
		}
		if m.State() == "" {
			// Expected traffic the table deliberately ignores -- an
			// unrecognised notification_type, auth_success, a session.status
			// that is not busy. No write, no state change, no timestamp
			// refresh, not an error, and no noise either: claude fires
			// Notification for things that are none of our business often
			// enough that a line each would be a log of nothing.
			return 0
		}
		*state = m.State()
		reassert = m.Reasserts()
		fromStanding = m.DatesFromStandingReport()
		// *text was empty by construction -- --text belongs to the manual form
		// and the two forms cannot be combined -- so the table's own reader is
		// the only thing that can fill it. Most mappings have none, and that is
		// rung 4 of the ladder: a three-part value is the ordinary claude case
		// and the reader accepts three parts or four. What no reader ever
		// returns is the user's prompt; see activity.go.
		*text = m.Text(payload)
	}

	switch *state {
	case tmux.StateWorking, tmux.StateBlocked, tmux.StateIdle:
	default:
		fmt.Fprintf(stderr, "tmux-web report: unknown state %q\n", *state)
		return 0
	}
	socket, pane, ok := tmuxTarget(getenv)
	if !ok {
		// Running an agent outside tmux is not an error, it is a no-op.
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), reportTimeout)
	defer cancel()
	// One client for both calls. The timeout covers the pair: it is there so a
	// wedged tmux server makes the hook return rather than hold an agent's turn
	// open, and two calls that could each wait the full 5 s would be twice the
	// bound the number was chosen for.
	tm := dial(socket)

	// The re-assertion read. It happens on this path only -- PreToolUse, the
	// hot hook and the whole subject of open question 1, pays nothing, and
	// opencode pays it once per turn, on session.idle, and on nothing else:
	// its two hot events (session.status busy, 17 times in one three-tool
	// turn, and tool.execute.before) are working edges and read nothing.
	//
	// It decides two things from the one fork, not one: whether to write at
	// all, and what timestamp to write under. The second is the mapping's own
	// decision -- see internal/report's `dating` -- and it is the writer's half
	// of it, because the standing report is the only thing here that knows when
	// the state was entered.
	if reassert {
		stamp, write := reassertedStamp(ctx, tm, pane, *state, ms, fromStanding)
		if !write {
			return 0
		}
		ms = stamp
	}

	value := tmux.FormatReport(*state, ms, *text)
	// The writer checks its own shape before the write. A botched write does
	// not clear a report: a value of exactly ";" is refused by tmux with "empty
	// value" and the option KEEPS its previous contents, which is the more
	// dangerous of the two outcomes.
	//
	// After the re-assertion read rather than before it, because that read can
	// change the timestamp: a shape check on a value the writer then edits
	// checks a value nobody stores.
	if _, ok := tmux.ParseReport(value, time.Now()); !ok {
		fmt.Fprintf(stderr, "tmux-web report: refusing to write a value it could not read back\n")
		return 0
	}

	// No "--": an option value is the second positional argument and tmux never
	// re-scans it for flags -- verified for @tmux_web_label in SetLabel, and the
	// same command.
	if _, err := tm.Run(ctx, "set", "-p", "-t", pane, tmux.AgentOption, value); err != nil {
		fmt.Fprintf(stderr, "tmux-web report: %v\n", err)
	}
	return 0
}

// reassertedStamp is the one read a re-assertion pays for, and both halves of
// its decision come out of it: whether to write at all, and what timestamp to
// write under.
//
// Everything unreadable answers "write, dated now" -- no standing report, so
// nothing to defer to and nothing to date from, so the repair happens as it
// always did. That covers more cases than it looks:
//
//   - The option is unset, which is the ordinary case for the first report a
//     pane ever gets. tmux does not answer that with an empty string: `show
//     -p -v @tmux_web_agent` on an unset user option exits 1 with "invalid
//     option" (measured on 3.7b), and Client.Run returns "" on error.
//   - The value is from another schema version, or truncated, or something
//     else entirely put there by hand. ParseReport refuses it whole.
//   - The value is dated more than reportFutureSkew ahead. ParseReport refuses
//     that too, and the writer asks THE SAME FUNCTION rather than holding a
//     second opinion about which clocks are believable: a report the daemon
//     cannot read is a pane the daemon has no report for, and the write is what
//     repairs an option some other clock poisoned.
//
// DATING FROM NOW IS THE RIGHT ANSWER IN ALL THREE, and it is the daemon's own
// rule that makes it so rather than a convenience. Observe DELETES its memory
// of a pane whose standing value it cannot parse, so the next value it sees is
// a FIRST SIGHT and is accepted whatever its timestamp. There is nothing to
// compare-and-swap against, because the daemon is not holding anything either.
// Both ends reach that conclusion through ParseReport, so they cannot disagree
// about which of these three cases they are in.
//
// -p is pane-scoped and, unlike the daemon's #{@tmux_web_agent} format read, does
// NOT walk up to the window, session and global options (measured: with only a
// session-level value set, this read still says "invalid option"). Nothing this
// project installs writes at those scopes, and the direction of the difference
// is safe -- a value the writer cannot see reads as no report, so it writes a
// pane-level report, which is what the daemon's own lookup then finds first.
//
// `now` for the parse is the report's OWN stamp rather than a second clock
// read. The two are microseconds apart and nothing observable turns on the
// difference; what it buys is that the skew window and the comparison below are
// measured from one instant, so the function cannot decide that a value is both
// believable and from the future.
func reassertedStamp(ctx context.Context, tm tmuxRunner, pane, state string, ms int64, fromStanding bool) (stamp int64, write bool) {
	out, err := tm.Run(ctx, "show-options", "-p", "-t", pane, "-v", tmux.AgentOption)
	if err != nil {
		return ms, true
	}
	rep, ok := tmux.ParseReport(out, time.UnixMilli(ms))
	if !ok {
		return ms, true
	}
	if reportSupersedes(rep, state, ms) {
		return 0, false
	}
	if !fromStanding {
		return ms, true
	}
	// One millisecond after the report this writer read, which makes the write
	// a COMPARE-AND-SWAP against the daemon's ordering filter: the daemon takes
	// it exactly when what it has accepted is still what the option holds, and
	// refuses it when the daemon knows something newer that the option has
	// since lost. See internal/report's `dating` for the two interleavings that
	// separates, and for why the same timestamp rather than one past it would
	// make the repair a no-op in the case it exists for.
	//
	// It cannot be in the future: reportSupersedes has already returned above
	// for any standing report not older than this event, so rep.Timestamp < ms
	// and rep.Timestamp+1 <= ms.
	return rep.Timestamp + 1, true
}

// reportSupersedes is the comparison itself: does this standing report leave a
// re-assertion of `state`, stamped at `ms`, with nothing to say?
//
// TWO CLAUSES, AND THE SECOND ONE IS THE REPAIR THIS FUNCTION EXISTS FOR.
//
//   - The STATE already matches. The original rule, and unchanged: a
//     re-assertion that agrees writes nothing, because a resting report's own
//     timestamp is what finishedAt is derived from, so writing the same state
//     under a newer timestamp is a second finish on every device that had
//     already seen the first.
//
//   - The standing report is NOT OLDER THAN THIS EVENT. That is the half the
//     writer used to get wrong, and it is a disagreement with the daemon rather
//     than with the option: the daemon's "standing" is the last report it
//     ACCEPTED and it refuses anything not strictly newer, while the writer's
//     was whatever byte string the option happened to hold. They diverge on
//     exactly the interleaving the ordering filter exists to absorb. A
//     re-assertion stamps when its process starts and can be descheduled for
//     any length of time before it reaches tmux, so an edge from a LATER event
//     -- a turn start, the user typing -- can land in between and be sitting
//     there when the read finally happens. The old rule saw a state it
//     disagreed with and overwrote fresh news with stale news. Nothing moves
//     that day: the daemon refuses the older value. What it leaves behind is a
//     resting report standing on a working pane, and the next FIRST SIGHT --
//     a restart, a tmux-server generation change, a pane that left `keep` and
//     came back -- has no filter to protect it, accepts what the option holds,
//     and with no client connected derives finishedAt from it immediately. See
//     TestALateReassertionLeavesNoStaleFinishOnAWorkingPane.
//
// NOT-OLDER rather than strictly-newer, and the equal case is not a rounding
// detail: a report stamped in the same millisecond is one the daemon has
// already accepted and would refuse this write against, so writing can only
// swap the option's contents for a value no reader will take. There is nothing
// to win at equality and a stale first sight to lose.
//
// What it deliberately does NOT do is compare timestamps for an EDGE. An edge
// reads nothing at all, so no standing report of any date can suppress a turn
// end: a second turn's Stop still writes over the previous turn's idle, which
// is the case the edge/re-assertion split was scoped around.
func reportSupersedes(rep tmux.Report, state string, ms int64) bool {
	return rep.State == state || rep.Timestamp >= ms
}

// tmuxTarget resolves the server and the pane from the environment the agent
// handed its hook.
//
// $TMUX is "<socket path>,<server pid>,<session id>" and its first field is the
// socket: a bare `tmux` can reach a different server than the one the agent is
// running inside. $TMUX_PANE is present in every integration's environment on
// all three agents, and for Claude Code there is no alternative -- a hook's
// stdin is a socket and it has no controlling tty, so nothing tty-derived is
// available.
func tmuxTarget(getenv func(string) string) (socket, pane string, ok bool) {
	socket, _, _ = strings.Cut(getenv("TMUX"), ",")
	pane = getenv("TMUX_PANE")
	if socket == "" || tmux.ValidatePaneID(pane) != nil {
		return "", "", false
	}
	return socket, pane, true
}

// readPayload reads the hook's JSON from stdin, bounded, and says whether
// stdin had more to give.
//
// THE EXTRA BYTE. The read is maxPayloadBytes+1, and the byte past the cap is
// the whole point: a LimitReader that comes back with exactly cap bytes cannot
// say whether stdin held one more, so a payload of exactly the cap and the
// first cap bytes of something longer arrive identical. They are not the same
// fact. The first is a whole hook payload; the second is a prefix, and every
// parser downstream will refuse it for a reason that has nothing to do with
// what went wrong.
//
// An earlier comment here claimed a truncated read "is still parsed, because a
// truncated read that happens to contain the key is more useful than a
// refusal". That was false in both halves. A truncated JSON object does not
// parse at all, so no key in it is reachable; and payloadIsRoot then refuses
// the payload wholesale -- deliberately, since a payload nobody could read is
// one where agent_id is absent for the worst possible reason. The net effect
// was that an oversized payload was refused in silence and looked exactly like
// garbage. See payloadIsUsable for what it looks like now.
//
// Read errors are still dropped on purpose: whatever arrived is handed on, and
// an empty or partial object is refused by the parse requirement anyway.
func readPayload(r io.Reader) (payload []byte, overCap bool) {
	if r == nil {
		return nil, false
	}
	b, _ := io.ReadAll(io.LimitReader(r, maxPayloadBytes+1))
	return b, len(b) > maxPayloadBytes
}

// payloadIsUsable is payloadIsRoot plus the one fact only the reader knows:
// whether stdin ran past the cap.
//
// BOTH ANSWERS ARE STILL A REFUSAL, and that is the fail-safe direction this
// project takes everywhere. A truncated payload is exactly the one in which
// agent_id or parentID may lie past the cut, and the state at stake on the
// event that gets big -- opencode's permission.asked -- is blocked, which never
// expires on its own. Writing it on evidence nobody has is the worse trade.
//
// What changes is that the two facts stop being one sentence. "It ran past the
// cap" is a fact about this process's own limit and is actionable: the number
// is in the message and it is one constant away. "It is not the JSON object
// every hook sends" is a fact about the caller. Sending the reader of a missing
// badge to look for a JSON bug that is not there is the part that was wrong.
//
// It lives here rather than beside payloadIsRoot because the cap is this
// subcommand's, not the filter's: payloadIsRoot is a judgement about a payload
// and this is a judgement about a read.
func payloadIsUsable(payload []byte, overCap bool) (ok bool, why string) {
	if overCap {
		return false, fmt.Sprintf("it ran past this hook's %d-byte cap on stdin, so what arrived is a truncated prefix", maxPayloadBytes)
	}
	return report.PayloadIsRoot(payload)
}
