package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/diegok/tmux-web/internal/tmux"
)

func TestTmuxTarget(t *testing.T) {
	for _, tc := range []struct {
		name, tmuxEnv, pane, wantSock, wantPane string
		ok                                      bool
	}{
		// $TMUX is <socket path>,<server pid>,<session id>. The first field is
		// the socket, and a bare `tmux` can reach a different server than the
		// one the agent is running inside.
		{"a real TMUX", "/tmp/tmux-1000/default,4242,0", "%3", "/tmp/tmux-1000/default", "%3", true},
		{"a socket with no commas", "/tmp/sock", "%3", "/tmp/sock", "%3", true},
		// Not an error: running an agent outside tmux is an ordinary thing to do.
		{"no TMUX", "", "%3", "", "", false},
		{"no TMUX_PANE", "/tmp/sock,1,0", "", "", "", false},
		{"an empty socket field", ",1,0", "%3", "", "", false},
		// The pane id reaches a command line, so it is validated with the same
		// rule everything else in this repo uses.
		{"a pane id that is not one", "/tmp/sock,1,0", "not-a-pane", "", "", false},
		{"a pane id with a flag in it", "/tmp/sock,1,0", "-x", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sock, pane, ok := tmuxTarget(mapEnv(map[string]string{
				"TMUX":      tc.tmuxEnv,
				"TMUX_PANE": tc.pane,
			}))
			if ok != tc.ok || sock != tc.wantSock || pane != tc.wantPane {
				t.Errorf("got (%q, %q, %v), want (%q, %q, %v)", sock, pane, ok, tc.wantSock, tc.wantPane, tc.ok)
			}
		})
	}
}

// Exit 0, whatever happens. The one nonzero code that matters is 2, which
// blocks a Claude PreToolUse tool call; keeping every path at 0 means nobody
// has to remember which.
func TestReportAlwaysExitsZero(t *testing.T) {
	for _, args := range [][]string{
		{"report", "--state", "working"},       // no TMUX in the environment
		{"report", "--state", "nonsense"},      // not a state
		{"report"},                             // no state at all
		{"report", "--nosuchflag"},             // a malformed command line
		{"report", "--state", "idle", "extra"}, // a stray operand
		{"report", "-h"},                       // help, which the rest of the CLI answers on stderr
	} {
		var out, errb bytes.Buffer
		if code := runReport(args[1:], &out, &errb, envWithout("TMUX"), dialReal); code != 0 {
			t.Errorf("%v exited %d, want 0 (stderr: %s)", args, code, errb.String())
		}
		if out.Len() != 0 {
			t.Errorf("%v wrote to stdout: %q -- stdout carries the answer and there is none", args, out.String())
		}
	}
}

// The tmux seam is a parameter, and the subcommand really goes through it.
//
// It exists from the first version because Task 14 adds a `show-options` read
// before a re-assertion write and tests it by counting the reads and writes one
// event makes, which needs a recording stub in this position. A seam nothing
// drives is a seam that has quietly stopped working by the time that task
// arrives, so this is what keeps it honest: the socket reaching dial comes from
// $TMUX, and the value reaching tmux reads back as the report that was asked
// for.
//
// It deliberately does NOT assert the argv word for word. What the write must
// actually achieve -- a PANE option on the pane named by $TMUX_PANE and on no
// other pane -- is a claim about tmux's option hierarchy, which only a real
// server can answer; see report_integration_test.go.
func TestReportWritesThroughTheInjectedDial(t *testing.T) {
	rec := &recordingRunner{}
	var dialled []string
	dial := func(socket string) tmuxRunner {
		dialled = append(dialled, socket)
		return rec
	}

	var out, errb bytes.Buffer
	code := runReport([]string{"--state", "blocked", "--text", "may I run rm -rf"},
		&out, &errb, mapEnv(map[string]string{"TMUX": "/tmp/sock,7,0", "TMUX_PANE": "%12"}), dial)
	if code != 0 {
		t.Fatalf("exited %d: %s", code, errb.String())
	}
	if len(dialled) != 1 || dialled[0] != "/tmp/sock" {
		t.Fatalf("dialled %q, want one client on the socket named by $TMUX", dialled)
	}
	if len(rec.calls) != 1 {
		t.Fatalf("ran %d tmux commands, want exactly 1: %v", len(rec.calls), rec.calls)
	}
	args := rec.calls[0]
	value := args[len(args)-1]
	rep, ok := tmux.ParseReport(value, time.Now())
	if !ok || rep.State != tmux.StateBlocked || rep.Activity != "may I run rm -rf" {
		t.Fatalf("wrote %q -> %+v, %v; want a blocked report reading %q", value, rep, ok, "may I run rm -rf")
	}
	// The option the pane carries, not some other one: @wterm_label is the
	// user's field and an integration writing it would put a program back
	// underneath the one field defined as being above programs.
	if !slices.Contains(args, tmux.AgentOption) {
		t.Fatalf("tmux %v does not name %s", args, tmux.AgentOption)
	}
}

// A tmux failure is not this subcommand's problem to solve: it is reported on
// stderr, where the agent's own log may or may not keep it, and the exit code
// stays 0.
func TestATmuxFailureIsStillExitZero(t *testing.T) {
	rec := &recordingRunner{err: errTmuxFailed}
	var out, errb bytes.Buffer
	code := runReport([]string{"--state", "idle"}, &out, &errb,
		mapEnv(map[string]string{"TMUX": "/tmp/sock,7,0", "TMUX_PANE": "%12"}),
		func(string) tmuxRunner { return rec })
	if code != 0 {
		t.Fatalf("exited %d, want 0", code)
	}
	if out.Len() != 0 {
		t.Errorf("wrote to stdout: %q", out.String())
	}
	if !strings.Contains(errb.String(), errTmuxFailed.Error()) {
		t.Errorf("stderr = %q, want the tmux failure in it", errb.String())
	}
}

// -- helpers ----------------------------------------------------------------

var errTmuxFailed = errors.New("no server running on /tmp/sock")

// recordingRunner is the tmuxRunner stub. Task 14 grows it into one that
// answers a `show-options` read as well; today it records and returns.
type recordingRunner struct {
	calls [][]string
	err   error
}

func (r *recordingRunner) Run(_ context.Context, args ...string) (string, error) {
	r.calls = append(r.calls, append([]string(nil), args...))
	return "", r.err
}

// mapEnv reads the environment from a map, so a test never sets a process-wide
// variable. That is not tidiness: TMUX is what decides which tmux server is
// written to, so a test that set it for real would race every other test in
// this package, and one that let the developer's own $TMUX through would write
// into their live session.
func mapEnv(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// envWithout is the process environment with some keys masked out. Masking
// TMUX is enough on its own to make a write impossible -- the socket has no
// other source, and tmuxTarget refuses an empty one -- so the cases that use
// it cannot reach any tmux server, the developer's included.
func envWithout(keys ...string) func(string) string {
	return func(k string) string {
		for _, masked := range keys {
			if k == masked {
				return ""
			}
		}
		return os.Getenv(k)
	}
}
