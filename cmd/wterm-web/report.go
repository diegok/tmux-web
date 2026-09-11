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
	return runReport(args, stdout, stderr, os.Getenv, dialReal)
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

// runReport is cmdReport with the environment and the tmux client injected, so
// a test can drive it without setting process-wide variables -- which would
// race every other test in the package and, if $TMUX leaked through, would
// write into the developer's live tmux session.
func runReport(args []string, stdout, stderr io.Writer, getenv func(string) string, dial func(socket string) tmuxRunner) int {
	fset := newFlagSet("report", stderr, "wterm-web report --state working|blocked|idle [--text TEXT]")
	state := fset.String("state", "", "working, blocked or idle")
	text := fset.String("text", "", "what the agent is doing; omitted for a state-only report")
	if _, _, ok := parseFlags(fset, args); !ok {
		return 0 // see the comment on cmdReport
	}
	// Stamped from the moment this process starts, never from when set-option
	// returned. The timestamp means "when the agent entered this state", and
	// stamping at write time would reintroduce exactly the reordering the
	// daemon's ordering filter exists to undo.
	ms := time.Now().UnixMilli()

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
