package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
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
//
// Every refusal is also asserted to have written NOTHING. Exit 0 on its own
// does not distinguish "refused" from "wrote something wrong and said nothing",
// and a wrong resting state written in silence is the failure this whole
// feature is most exposed to. The rows that carry a full $TMUX are the ones
// where that assertion means something: with TMUX masked no write is possible
// anyway, so a zero-write assertion there would be vacuous.
func TestReportAlwaysExitsZero(t *testing.T) {
	full := map[string]string{"TMUX": "/tmp/sock,7,0", "TMUX_PANE": "%1"}
	for _, tc := range []struct {
		name string
		args []string
		// stderr, when set, must appear in the commentary. It is what
		// distinguishes two refusals that write nothing for different reasons
		// -- "you forgot a flag" reads very differently from "nobody has ever
		// heard of that event", and only one of them is the reader's fault.
		stderr string
		stdin  string
		env    func(string) string
	}{
		// Task 6's rows.
		{"no TMUX in the environment", []string{"--state", "working"}, "", "", envWithout("TMUX")},
		{"not a state", []string{"--state", "nonsense"}, "unknown state", "", mapEnv(full)},
		{"no state at all", []string{}, "unknown state", "", mapEnv(full)},
		{"a malformed command line", []string{"--nosuchflag"}, "", "", mapEnv(full)},
		// A stray operand is ignored rather than refused -- parseFlags hands
		// the operands back and report has no use for them. This row is here
		// as Task 6 left it, and the missing $TMUX is what makes it write
		// nothing, not a refusal.
		{"a stray operand", []string{"--state", "idle", "extra"}, "", "", envWithout("TMUX")},
		{"help, which the rest of the CLI answers on stderr", []string{"-h"}, "Usage:", "", mapEnv(full)},

		// Task 13's mode matrix. Neither half of the integration form means
		// anything alone, and neither may be combined with the manual form:
		// a precedence is a rule somebody has to remember, and the wrong
		// branch of it writes a state from a hook that thought it was passing
		// something else.
		{"--agent without --event", []string{"--agent", "claude"}, "meaningless apart", "", mapEnv(full)},
		{"--event without --agent", []string{"--event", "Stop"}, "meaningless apart", "", mapEnv(full)},
		{"--state with the integration form", []string{"--state", "idle", "--agent", "claude", "--event", "Stop"},
			"two different callers", "", mapEnv(full)},
		{"--text with the integration form", []string{"--text", "hi", "--agent", "claude", "--event", "Stop"},
			"two different callers", "", mapEnv(full)},
		{"an agent nothing knows", []string{"--agent", "nosuchagent", "--event", "Stop"},
			"nothing known about", "{}", mapEnv(full)},
		{"an event that agent does not have", []string{"--agent", "claude", "--event", "NoSuchEvent"},
			"nothing known about", "{}", mapEnv(full)},
		// Silent on purpose: claude fires Notification for things that are
		// none of our business often enough that a line each would be a log
		// of nothing, and a line here would make a routine event look like a
		// misconfiguration.
		{"an ignored notification_type", []string{"--agent", "claude", "--event", "Notification"},
			"", `{"notification_type":"auth_success"}`, mapEnv(full)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recordingTmux{}
			var out, errb bytes.Buffer
			code := runReport(tc.args, strings.NewReader(tc.stdin), &out, &errb, tc.env,
				func(string) tmuxRunner { return rec })
			if code != 0 {
				t.Errorf("exited %d, want 0 (stderr: %s)", code, errb.String())
			}
			if out.Len() != 0 {
				t.Errorf("wrote to stdout: %q -- stdout carries the answer and there is none", out.String())
			}
			if len(rec.calls) != 0 {
				t.Errorf("ran %v; a refused command line must leave the option exactly as it found it", rec.calls)
			}
			if tc.stderr != "" && !strings.Contains(errb.String(), tc.stderr) {
				t.Errorf("stderr = %q, want %q in it", errb.String(), tc.stderr)
			}
			if tc.stderr == "" && errb.Len() != 0 && len(tc.args) > 0 && tc.args[0] == "--agent" {
				t.Errorf("stderr = %q, want silence: the table ignoring an event it knows is not a complaint", errb.String())
			}
		})
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
	rec := &recordingTmux{}
	var dialled []string
	dial := func(socket string) tmuxRunner {
		dialled = append(dialled, socket)
		return rec
	}

	var out, errb bytes.Buffer
	code := runReport([]string{"--state", "blocked", "--text", "may I run rm -rf"},
		strings.NewReader(""), &out, &errb,
		mapEnv(map[string]string{"TMUX": "/tmp/sock,7,0", "TMUX_PANE": "%12"}), dial)
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
	// The option the pane carries, not some other one: @tmux_web_label is the
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
	rec := &recordingTmux{err: errTmuxFailed}
	var out, errb bytes.Buffer
	code := runReport([]string{"--state", "idle"}, strings.NewReader(""), &out, &errb,
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

// recordingTmux is the tmuxRunner stub: it records every command, answers a
// `show-options` read with the report already standing on the pane, and counts
// the two kinds of call separately.
//
// The two counters are what Task 14's tests assert on, and they assert on them
// rather than on the value that ends up stored because "wrote nothing" and
// "wrote the same state again under a newer timestamp" store values that look
// alike and badge differently. Only the call log can tell them apart.
type recordingTmux struct {
	// standing is what a read answers with: the value already in
	// @tmux_web_agent. The empty string is the unset option -- which is also what
	// a real tmux read of an unset user option amounts to, since it exits 1
	// with "invalid option" (measured on 3.7b) and Run returns "" on error.
	standing string
	calls    [][]string
	shows    int
	sets     int
	err      error
}

func (r *recordingTmux) Run(_ context.Context, args ...string) (string, error) {
	r.calls = append(r.calls, append([]string(nil), args...))
	// Matched on the command word rather than on a flag, because the flags are
	// what the integration test checks against a real server and a stub that
	// guessed at them here would be checking itself. "show" is tmux's own
	// alias for "show-options"; both count as a read.
	if len(args) > 0 && (args[0] == "show-options" || args[0] == "show") {
		r.shows++
		return r.standing, r.err
	}
	r.sets++
	return "", r.err
}

// lastSet is the last write this stub was asked to make, or nil if there was
// none. A re-assertion's read is in calls too, so "the last call" is not the
// same thing as "the write".
func (r *recordingTmux) lastSet() []string {
	for i := len(r.calls) - 1; i >= 0; i-- {
		if r.calls[i][0] != "show-options" && r.calls[i][0] != "show" {
			return r.calls[i]
		}
	}
	return nil
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

// -- the stdin cap ----------------------------------------------------------

// The cap's boundary, which is the only place a cap can be wrong. A test that
// feeds a small payload proves nothing about it.
//
// The read is maxPayloadBytes+1 rather than maxPayloadBytes, and that one extra
// byte is the whole mechanism: an io.LimitReader(r, cap) that comes back with
// exactly cap bytes cannot say whether stdin held one more, so "a payload of
// exactly the cap" and "the first cap bytes of something longer" arrive
// identical. One is a whole hook payload and the other is a prefix of one, and
// this subcommand answers them differently -- so the difference has to survive
// the read.
func TestReadPayloadKnowsWhenStdinRanPastTheCap(t *testing.T) {
	for _, tc := range []struct {
		name     string
		n        int
		wantLen  int
		wantOver bool
	}{
		{"one byte under the cap", maxPayloadBytes - 1, maxPayloadBytes - 1, false},
		// The row the off-by-one lives on: a payload of exactly the cap is a
		// whole payload, not a truncated one.
		{"exactly the cap", maxPayloadBytes, maxPayloadBytes, false},
		{"one byte over it", maxPayloadBytes + 1, maxPayloadBytes + 1, true},
		// The probe byte bounds what is buffered however much is offered.
		{"far over it", 4 * maxPayloadBytes, maxPayloadBytes + 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, over := readPayload(io.LimitReader(filler{}, int64(tc.n)))
			if len(b) != tc.wantLen || over != tc.wantOver {
				t.Errorf("read %d bytes, over=%v; want %d bytes, over=%v", len(b), over, tc.wantLen, tc.wantOver)
			}
		})
	}
}

// No stdin at all is not a crash and not a truncation: it is an empty payload,
// which payloadIsRoot already refuses for the ordinary reason.
func TestReadPayloadOnNoStdin(t *testing.T) {
	if b, over := readPayload(nil); b != nil || over {
		t.Errorf("readPayload(nil) = %q, %v; want nil, false", b, over)
	}
}

// The coverage the cap exists to preserve: an ordinary opencode edit permission
// whose diff is far bigger than anything recorded still badges the pane.
//
// This is the case the old 1 MiB cap dropped in silence -- an approval prompt
// on a big diff produced no badge at all, and the fresh working report that
// preceded it had already suppressed the screen capture that would have caught
// it. The two guards below are what stop this fixture being true by accident:
// it must be bigger than the cap that used to fail, and smaller than the one
// that must not.
func TestALargeEditPermissionStillReportsBlocked(t *testing.T) {
	payload := editPermissionWithDiff(t, 1_500_000)
	if len(payload) <= 1<<20 {
		t.Fatalf("payload is %d bytes: it has to exceed the 1 MiB cap that dropped it to prove anything", len(payload))
	}
	if len(payload) >= maxPayloadBytes {
		t.Fatalf("payload is %d bytes, which is over the cap: this test is about the case UNDER it", len(payload))
	}

	rec := &recordingTmux{}
	var out, errb bytes.Buffer
	code := runReport([]string{"--agent", "opencode", "--event", "permission.asked"},
		strings.NewReader(payload), &out, &errb,
		mapEnv(map[string]string{"TMUX": "/tmp/sock,7,0", "TMUX_PANE": "%12"}),
		func(string) tmuxRunner { return rec })
	if code != 0 {
		t.Fatalf("exited %d: %s", code, errb.String())
	}
	set := rec.lastSet()
	if set == nil {
		t.Fatalf("wrote nothing (stderr: %s); a permission prompt with a big diff is exactly the badge this feature is for", errb.String())
	}
	value := set[len(set)-1]
	rep, ok := tmux.ParseReport(value, time.Now())
	if !ok || rep.State != tmux.StateBlocked {
		t.Fatalf("wrote %q -> %+v, %v; want a blocked report", value, rep, ok)
	}
	// The diff is read past, never published. A reader that took "metadata,
	// serialised" would write a value the daemon then refuses to parse at all.
	if strings.Contains(rep.Activity, "padding") {
		t.Errorf("activity %q carries the diff; only the filepath's basename belongs in it", rep.Activity)
	}
}

// "It ran past the cap" and "it is not JSON" are different facts about a
// payload, and until now they were the same sentence and the same silence.
//
// Both still refuse, and that is deliberate: a truncated payload is precisely
// the one where agent_id or parentID may lie past the cut, so accepting it
// would write a state -- blocked, which never expires -- on evidence nobody
// has. What changes is that the refusal says which of the two happened, so the
// reader of a missing badge is not sent looking for a JSON bug that is not
// there.
func TestATruncatedPayloadAndGarbageAreRefusedForDifferentReasons(t *testing.T) {
	over := editPermissionWithDiff(t, maxPayloadBytes)
	if len(over) <= maxPayloadBytes {
		t.Fatalf("payload is %d bytes, which is not over the %d-byte cap", len(over), maxPayloadBytes)
	}
	for _, tc := range []struct {
		name, stdin, want, notWant string
	}{
		{
			name:    "a whole payload that is not JSON",
			stdin:   "this is not JSON at all",
			want:    "not the JSON object every hook sends",
			notWant: "cap",
		},
		{
			name:  "a real edit permission cut off by the cap",
			stdin: over,
			want:  "cap",
			// The parse did fail, but saying so is the wrong diagnosis: the
			// payload is fine and the reader is what gave out.
			notWant: "not the JSON object every hook sends",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recordingTmux{}
			var out, errb bytes.Buffer
			code := runReport([]string{"--agent", "opencode", "--event", "permission.asked"},
				strings.NewReader(tc.stdin), &out, &errb,
				mapEnv(map[string]string{"TMUX": "/tmp/sock,7,0", "TMUX_PANE": "%12"}),
				func(string) tmuxRunner { return rec })
			if code != 0 {
				t.Errorf("exited %d, want 0", code)
			}
			if len(rec.calls) != 0 {
				t.Errorf("ran %v; a payload this subcommand could not read is not evidence that anything happened", rec.calls)
			}
			if !strings.Contains(errb.String(), tc.want) {
				t.Errorf("stderr = %q, want %q in it", errb.String(), tc.want)
			}
			if strings.Contains(errb.String(), tc.notWant) {
				t.Errorf("stderr = %q, and %q in it is the other refusal's words", errb.String(), tc.notWant)
			}
		})
	}
}

// editPermissionWithDiff is the recorded opencode edit permission with its
// unified diff padded out to roughly n bytes.
//
// Built from the real fixture rather than hand-written, so the payload that
// exercises the cap is the same shape as the one the subagent filter, the event
// table and the text reader all run on -- a synthetic `{"diff": "..."}` would
// pass the cap and prove nothing about the path behind it.
func editPermissionWithDiff(t *testing.T, n int) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "hooks", "opencode", "permission_asked_edit.json"))
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]any
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}
	meta, ok := top["properties"].(map[string]any)["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("%s no longer carries properties.metadata; this helper pads its diff", "permission_asked_edit.json")
	}
	meta["diff"] = meta["diff"].(string) + strings.Repeat("+padding\n", n/9)
	out, err := json.Marshal(top)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// filler is an endless reader of one repeated byte, so a test can offer stdin
// more than the cap without holding it in memory first.
type filler struct{}

func (filler) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}
