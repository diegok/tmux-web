package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/diegok/tmux-web/internal/tmux/testutil"
)

// Layer 3 of Task 17's test story: the WIRING. Not what an event means -- that
// is events_test.go's table over the recorded fixtures -- and not the three
// filters, which pi.test.ts drives directly with a fake report. What only a
// real pi can answer is whether the extension is loaded at all, whether
// `pi.on(name, fn)` is still the registration form, whether the five names are
// still the five names, and what argv and stdin the queue's spawn actually
// produces.
//
// A recording stub named `wterm-web` on PATH stands in for the subcommand, so
// nothing here writes a tmux option or touches a real pane. Everything runs
// under an isolated HOME and an isolated PI_CODING_AGENT_DIR, with an API key
// that is deliberately invalid: the turn is expected to fail at the provider,
// and it does not matter, because every event this test asserts on fires
// before or after the model call rather than because of it.
//
// If pi is not installed, these SKIP LOUDLY. A wiring test against a fake
// runtime would test the fake.

// piEvents is the file the stub appends to: one line per invocation, the argv
// it was given, then the JSON it was handed on stdin.
const piEventsFile = "calls.log"

// TestPiExtensionWiresTheFiveEventsToTheReportArgv drives a real pi TUI in a
// throwaway tmux session and asserts on what the extension spawned.
//
// It asserts the TURN-START INVARIANT as a sequence, which is the thing no
// other test in this repo can see end to end: pi's turn end is an edge -- it
// writes a resting state without reading what is standing -- and that is only
// safe if a working-producing event was written first. Drop the `input`
// handler and the recorded sequence loses its middle line, which is exactly
// what a pane losing one badge per turn looks like from here.
func TestPiExtensionWiresTheFiveEventsToTheReportArgv(t *testing.T) {
	pi := requirePi(t)
	dir := t.TempDir()
	log := filepath.Join(dir, piEventsFile)

	// A launcher rather than a long tmux command line: tmux would otherwise
	// re-split the whole thing through a shell of its own, and the env has to
	// be exact.
	script := filepath.Join(dir, "run-pi.sh")
	writeExecutable(t, script, "#!/bin/sh\n"+
		exports(piEnv(t, dir, log))+
		"exec "+pi+" "+strings.Join(piArgs(t), " ")+"\n")

	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "wire", "-x", "120", "-y", "40", "-c", dir, script)

	// Startup. session_start is the first thing the extension can report and
	// it is the only one that carries a payload of our own making.
	waitForCalls(t, log, 1, coldStart, "pi to start and report its session_start")
	first := readCalls(t, log)[0]
	if first.argv != "report --agent pi --event session_start" {
		t.Errorf("first call argv = %q, want the argv spawnReport builds for pi's session_start", first.argv)
	}
	// The Task 14 contract, and the one thing in this whole feature that a
	// recorded fixture cannot carry: ctx is not part of pi's event object, so
	// piSessionStartIdle in events.go reads a key the EXTENSION has to add.
	// Its failure direction is deliberately the opposite of every other
	// discriminator -- absent or wrong-typed reads as `working` -- so a
	// misspelling here does not fail loudly anywhere: the idle branch simply
	// stops existing.
	if first.stdin != `{"wterm_is_idle":true}` {
		t.Errorf("session_start stdin = %q, want %q -- events.go discriminates pi's session_start on exactly this key, and reads anything else as `working`",
			first.stdin, `{"wterm_is_idle":true}`)
	}

	// One turn. The prompt is written for this test and goes nowhere: the
	// provider refuses the key, and `input` fires before the request either
	// way.
	srv.Run(t, "send-keys", "-t", "wire", "write a haiku about tmux panes", "Enter")
	waitForCalls(t, log, 2, warm, "the turn start to report `input`")
	// Escape aborts whatever the turn is doing, so the settle does not depend
	// on how the provider fails or on there being a network at all.
	srv.Run(t, "send-keys", "-t", "wire", "Escape")
	waitForCalls(t, log, 3, warm, "the turn end to report `agent_settled`")

	calls := readCalls(t, log)
	var events []string
	for _, c := range calls {
		events = append(events, eventOf(c.argv))
		// The reductions, the whitelist and the 1 KiB cap live in Go. An
		// integration that put a payload on the command line would bypass all
		// three -- and would put a user's prompt in the process table.
		if strings.Contains(c.argv, "--text") {
			t.Errorf("call %q carries --text: the activity text is derived in `wterm-web report`, never passed in by an integration", c.argv)
		}
		if !strings.HasPrefix(c.argv, "report --agent pi --event ") {
			t.Errorf("call argv = %q, want exactly `report --agent pi --event <name>`", c.argv)
		}
	}
	// The sequence, not a set: the turn start has to be IN FRONT OF the turn
	// end, which is the whole of why the edge write at the end is safe.
	want := "session_start input agent_settled"
	if got := strings.Join(events, " "); got != want {
		t.Errorf("recorded events = %q, want %q. A missing `input` in the middle is the turn-start invariant broken: agent_settled's write is an edge, and with no working in front of it the daemon suppresses the finish and the pane loses that turn's badge",
			got, want)
	}
	// And the two state-only events send the event name and nothing else.
	for _, c := range calls[1:] {
		if c.stdin != "{}" {
			t.Errorf("%s stdin = %q, want {} -- neither event's mapping has an activity text, and `input`'s only string is the user's raw prompt",
				eventOf(c.argv), c.stdin)
		}
	}
}

// TestPiExtensionReportsNothingFromANonTUIProcess is the mode gate, against a
// real pi process that really is not the TUI.
//
// This is the async-subagent case exactly. A pi subagent launched with
// `async: true` is a SEPARATE OS PROCESS: it discovers and loads the same
// extension, inherits TMUX_PANE unchanged, runs its own full event stream and
// emits an agent_settled byte-identical to the root's -- measured at 7.0s
// before the root's real one -- with ctx.isIdle() true in it as well. Nothing
// in any payload separates it from the root. `ctx.mode` does, and it is the
// only thing that does, which is why the gate fails closed and why this test
// asserts on SILENCE rather than on a state.
//
// `pi -p` is that process: measured on pi 0.85.1, it reports mode "print" and
// hasUI false and emits session_start, input, agent_start, turn_end and
// agent_settled just like the root.
func TestPiExtensionReportsNothingFromANonTUIProcess(t *testing.T) {
	pi := requirePi(t)
	dir := t.TempDir()
	log := filepath.Join(dir, piEventsFile)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, pi, append(piArgs(t), "-p", "say hi")...)
	cmd.Dir = dir
	cmd.Env = piEnv(t, dir, log)
	out, err := cmd.CombinedOutput()
	// The exit status is not the assertion and must not be one: the provider
	// refuses the key, so this run fails, and it would fail differently on a
	// machine with no network. What is asserted is that a non-TUI pi ran the
	// extension and the extension said nothing.
	t.Logf("pi -p exited (%v); its five events fired and none of them reported: %s", err, firstOutputLine(out))

	if calls := readCalls(t, log); len(calls) != 0 {
		t.Fatalf("a non-TUI pi reported %d time(s): %+v. The mode gate is the ONLY thing standing between an async subagent's settle and a false `done` badge on the owner's pane",
			len(calls), calls)
	}
}

// -- fixture ----------------------------------------------------------------

type piCall struct{ argv, stdin string }

// requirePi finds pi or skips the test by name. Loudly: a suite that silently
// skips its only wiring test looks green while testing nothing.
func requirePi(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("pi")
	if err != nil {
		t.Skipf("SKIPPING %s: pi is not installed (%v). The pi extension's wiring -- that it registers five events and spawns `wterm-web report --agent pi --event <name>` -- is NOT covered on this machine. Its filters still are, in internal/integrations/pi.test.ts",
			t.Name(), err)
	}
	return path
}

// piArgs is how this test runs pi, minus the mode and the prompt.
//
// -ne plus an explicit -e loads THE REPO'S OWN FILE, not a copy: a copy is a
// second source that drifts, and the relative `./queue.ts` import has to
// resolve out of internal/integrations/ anyway. --provider and --model pin the
// run to the credential that piEnv deliberately breaks, so that no real
// provider key can be picked out of the environment.
func piArgs(t *testing.T) []string {
	t.Helper()
	ext, err := filepath.Abs(filepath.Join("..", "..", "internal", "integrations", "pi.ts"))
	if err != nil {
		t.Fatalf("resolving the extension path: %v", err)
	}
	if _, err := os.Stat(ext); err != nil {
		t.Fatalf("the extension under test is missing: %v", err)
	}
	return []string{"-ne", "-e", ext, "--no-session", "--provider", "openai", "--model", "gpt-4o-mini"}
}

// piEnv is the whole environment the pi process gets. It is built rather than
// inherited, and that is a safety property, not tidiness:
//
//   - HOME and PI_CODING_AGENT_DIR point into the test's own directory, so
//     this test cannot read or write the developer's ~/.pi/agent/settings.json.
//   - OPENAI_API_KEY is invalid on purpose and --provider pins pi to it, so no
//     real credential from the developer's environment is ever used.
//   - PATH carries the recording stub FIRST, which is how `wterm-web` resolves
//     to something that records instead of to something that writes a tmux
//     option.
func piEnv(t *testing.T, dir, log string) []string {
	t.Helper()
	stub := filepath.Join(dir, "bin")
	if err := os.MkdirAll(stub, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", stub, err)
	}
	// One line per invocation: the argv, then whatever arrived on stdin. `cat`
	// first, because the shell must read the pipe before the parent's write
	// can complete.
	writeExecutable(t, filepath.Join(stub, "wterm-web"),
		"#!/bin/sh\npayload=$(cat)\nprintf '%s | %s\\n' \"$*\" \"$payload\" >> \"$WTERM_STUB_LOG\"\n")
	home := filepath.Join(dir, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", home, err)
	}
	return []string{
		"PATH=" + stub + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + home,
		"TERM=" + os.Getenv("TERM"),
		"PI_CODING_AGENT_DIR=" + filepath.Join(dir, "pi-config"),
		"PI_OFFLINE=1",
		"OPENAI_API_KEY=wterm-web-test-invalid",
		"WTERM_STUB_LOG=" + log,
	}
}

// exports turns piEnv's pairs into a shell prologue for the tmux launcher.
func exports(env []string) string {
	var b strings.Builder
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		fmt.Fprintf(&b, "export %s='%s'\n", k, v)
	}
	return b.String()
}

func writeExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

// The two waits this test makes, and they are different numbers on purpose.
// coldStart covers a node runtime starting inside a freshly created tmux pane;
// once pi is up and has reported once, every later event is a few hundred
// milliseconds away, so `warm` is the budget for those. Keeping them apart is
// not tidiness: a dropped handler is a mutant whose ONLY symptom here is a
// wait that never finishes, and 60s spent proving that three times over is how
// a mutation run stops being something anyone does.
const (
	coldStart = 60 * time.Second
	warm      = 20 * time.Second
)

// waitForCalls blocks until the stub has recorded at least n invocations.
//
// It fails with the calls it DID see, because that list is the real assertion
// when a handler is missing: "recorded so far: [session_start, agent_settled]"
// names the turn-start invariant broken, where a bare timeout would name
// nothing.
func waitForCalls(t *testing.T, log string, n int, within time.Duration, what string) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		if len(readCalls(t, log)) >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("waited %s for %s; recorded so far: %+v", within, what, readCalls(t, log))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// readCalls parses the stub's log. A missing file is no calls: the stub writes
// it on its first invocation, and "pi reported nothing" is a result this suite
// asserts on rather than an error.
func readCalls(t *testing.T, log string) []piCall {
	t.Helper()
	b, err := os.ReadFile(log)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("reading the stub's log: %v", err)
	}
	var calls []piCall
	for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if line == "" {
			continue
		}
		argv, stdin, _ := strings.Cut(line, " | ")
		calls = append(calls, piCall{argv: argv, stdin: stdin})
	}
	return calls
}

// eventOf is the --event value of a recorded argv, for the sequence assertion.
func eventOf(argv string) string {
	fields := strings.Fields(argv)
	for i, f := range fields {
		if f == "--event" && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	return "<no --event in " + argv + ">"
}

// firstOutputLine is the head of a failed run's output, for the log line that
// records how it failed. (activity.go already has a firstLine, on strings.)
func firstOutputLine(b []byte) string {
	line, _, _ := strings.Cut(strings.TrimSpace(string(b)), "\n")
	return line
}
