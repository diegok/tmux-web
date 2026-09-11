package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

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
// The payload is a hook's own JSON and is normally a few hundred bytes, but
// opencode's permission.asked for an edit carries a full unified diff and one
// captured sample is already over the 1 KiB report cap on its own. There is no
// upper bound on a file, so there is no upper bound on that field, and a hook
// process that buffers whatever it is handed is a hook process that can be made
// to buffer a gigabyte. Nothing here needs more than the first few keys.
const maxPayloadBytes = 1 << 20

// runReport is cmdReport with the environment and the tmux client injected, so
// a test can drive it without setting process-wide variables -- which would
// race every other test in the package and, if $TMUX leaked through, would
// write into the developer's live tmux session.
func runReport(args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string, dial func(socket string) tmuxRunner) int {
	fset := newFlagSet("report", stderr,
		"wterm-web report --state working|blocked|idle [--text TEXT]\n"+
			"   or: wterm-web report --agent claude|opencode|pi --event NAME   (payload on stdin)")
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
		fmt.Fprintf(stderr, "wterm-web report: --state/--text and --agent/--event are two different callers; pass one form or the other\n")
		return 0
	case integration && (*agent == "" || *event == ""):
		fmt.Fprintf(stderr, "wterm-web report: --agent and --event are meaningless apart; pass both\n")
		return 0
	}

	if integration {
		m, known := lookupMapping(*agent, *event, readPayload(stdin))
		if !known {
			// A misconfigured integration, which is worth a line: an event
			// name nobody recognises will never report anything, and silence
			// would make that indistinguishable from an agent that is idle.
			fmt.Fprintf(stderr, "wterm-web report: nothing known about %q's %q event\n", *agent, *event)
			return 0
		}
		if m.state == "" {
			// Expected traffic the table deliberately ignores -- an
			// unrecognised notification_type, auth_success, a session.status
			// that is not busy. No write, no state change, no timestamp
			// refresh, not an error, and no noise either: claude fires
			// Notification for things that are none of our business often
			// enough that a line each would be a log of nothing.
			return 0
		}
		*state = m.state
		// *text is empty by construction here -- --text belongs to the manual
		// form and the two forms cannot be combined -- so the integration form
		// reports state only. That is a complete report, not a degraded one: a
		// three-part value is the ordinary claude case and the reader accepts
		// three parts or four. Task 15 fills it in from m.text.
	}

	switch *state {
	case tmux.StateWorking, tmux.StateBlocked, tmux.StateIdle:
	default:
		fmt.Fprintf(stderr, "wterm-web report: unknown state %q\n", *state)
		return 0
	}
	socket, pane, ok := tmuxTarget(getenv)
	if !ok {
		// Running an agent outside tmux is not an error, it is a no-op.
		return 0
	}
	value := tmux.FormatReport(*state, ms, *text)
	// The writer checks its own shape before the write. A botched write does
	// not clear a report: a value of exactly ";" is refused by tmux with "empty
	// value" and the option KEEPS its previous contents, which is the more
	// dangerous of the two outcomes.
	if _, ok := tmux.ParseReport(value, time.Now()); !ok {
		fmt.Fprintf(stderr, "wterm-web report: refusing to write a value it could not read back\n")
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), reportTimeout)
	defer cancel()
	// No "--": an option value is the second positional argument and tmux never
	// re-scans it for flags -- verified for @wterm_label in SetLabel, and the
	// same command.
	if _, err := dial(socket).Run(ctx, "set", "-p", "-t", pane, tmux.AgentOption, value); err != nil {
		fmt.Fprintf(stderr, "wterm-web report: %v\n", err)
	}
	return 0
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

// readPayload reads the hook's JSON from stdin, bounded.
//
// Errors are dropped on purpose. A payload that cannot be read is a payload
// whose discriminator reads as "", which is on no whitelist and so writes
// nothing -- the same fail-closed answer an unknown notification_type gets.
// Whatever arrived before the error is still parsed, because a truncated read
// that happens to contain the key is more useful than a refusal, and one that
// does not contain it is already handled.
func readPayload(r io.Reader) []byte {
	if r == nil {
		return nil
	}
	b, _ := io.ReadAll(io.LimitReader(r, maxPayloadBytes))
	return b
}
