package integrations

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// Claude Code's integration is the only one of the three with no runtime to
// live in: every hook is a fresh process, so there is no module scope to hold a
// queue and no `ctx` to read a filter out of. What is left is a settings block
// and a three-line wrapper, and both of them are tested here because both of
// them are the kind of thing that fails silently.
//
// What is NOT tested here, and where it is instead:
//
//   - What each hook MEANS -- the state, the whitelist, the edge/re-assertion
//     split, the absence-coded agent_id filter -- is cmd/tmux-web/events.go's
//     table over the eleven recorded payloads in
//     cmd/tmux-web/testdata/hooks/claude/. This package has no opinion about
//     any of it, which is the whole point of the split.
//   - That the four registered names are names that table knows is
//     cmd/tmux-web/integration_claude_test.go: this package cannot import
//     package main, and a hook registered under a name events.go does not map
//     reports NOTHING while passing every assertion in this file.

// -- the settings block -----------------------------------------------------

// hookJSON is the generated block decoded as JSON RATHER THAN as the structs
// that produced it. A struct-shaped assertion can only see the fields the
// struct declares: add `AsyncRewake bool` to the writer and a test decoding
// into the writer's own type would go on passing, because both sides moved
// together. Decoding into maps means the test sees the KEYS that were written.
type hookMatcherJSON struct {
	Hooks []map[string]any `json:"hooks"`
}

type hookJSON struct {
	Hooks map[string][]hookMatcherJSON `json:"hooks"`
}

func TestGeneratedClaudeHooks(t *testing.T) {
	block := ClaudeHookBlock("/opt/tmux-web/report.sh")

	// A string search over the marshalled JSON, deliberately: a struct-field
	// assertion cannot see a key somebody adds to a map later.
	if bytes.Contains(block, []byte("asyncRewake")) {
		t.Fatal("asyncRewake must never be set: it re-arms exit-code handling, " +
			"including the exit 2 that blocks a PreToolUse tool call")
	}
	if bytes.Contains(block, []byte("SubagentStop")) {
		t.Fatal("SubagentStop must never be registered: not registering it is the " +
			"whole of the structural guard against Task-tool subagents")
	}

	// Exactly one top-level key, and it is "hooks". A block that carried
	// anything else would be merged into the user's settings.json by Task 20's
	// installer, which is not a place to discover a surprise.
	var top map[string]json.RawMessage
	if err := json.Unmarshal(block, &top); err != nil {
		t.Fatalf("the generated block is not JSON: %v\n%s", err, block)
	}
	if len(top) != 1 || top["hooks"] == nil {
		t.Fatalf("top-level keys = %v, want exactly [hooks]", sortedKeys(top))
	}

	var got hookJSON
	if err := json.Unmarshal(block, &got); err != nil {
		t.Fatalf("decoding the generated block: %v\n%s", err, block)
	}

	// Exactly the four, counted and named. The count is what a fifth hook
	// trips; the names are what a renamed one trips.
	want := []string{"Notification", "PreToolUse", "Stop", "UserPromptSubmit"}
	if names := sortedKeys(got.Hooks); !equalStrings(names, want) {
		t.Fatalf("registered hooks = %v, want %v. The set is small and CLOSED: SubagentStop in particular is absent by design, and a fifth hook is a new decision, not a new line",
			names, want)
	}

	for _, event := range want {
		matchers := got.Hooks[event]
		if len(matchers) != 1 || len(matchers[0].Hooks) != 1 {
			t.Fatalf("%s: %d matcher(s) holding %v hooks, want exactly one hook",
				event, len(matchers), hookCounts(matchers))
		}
		h := matchers[0].Hooks[0]

		// The key set, exactly. This is the second, independent kill for
		// asyncRewake -- the string search above is the first -- and it is the
		// one that also catches a timeout, a matcher, or anything else added
		// to a hook object without a decision behind it.
		if names := sortedKeys(h); !equalStrings(names, []string{"async", "command", "type"}) {
			t.Errorf("%s hook keys = %v, want exactly [async command type]", event, names)
		}
		// `true`, and not merely `present`: "async": false is the mutant that
		// puts the hook back on the turn's critical path, and a presence check
		// cannot see it. Measured on Claude Code 2.1.267: a turn costing
		// 2771 ms baseline took 7735 ms with a `sleep 5` sync hook and
		// 2251 ms with the same hook async.
		if h["async"] != true {
			t.Errorf("%s async = %v, want true. A sync hook adds its whole duration to the turn",
				event, h["async"])
		}
		if h["type"] != "command" {
			t.Errorf("%s type = %v, want \"command\"", event, h["type"])
		}
		// The event name reaches the wrapper as $1 and reaches `report` as
		// --event. Asserting the whole command line rather than a substring is
		// what catches the two ways this goes wrong quietly: the wrong event
		// name on the wrong hook, and an unquoted path.
		if wantCmd := "'/opt/tmux-web/report.sh' " + event; h["command"] != wantCmd {
			t.Errorf("%s command = %v, want %q", event, h["command"], wantCmd)
		}
	}
}

// TestClaudeHookBlockQuotesTheScriptPath: the installer resolves the script
// path at install time and Claude runs the command through a shell, so a path
// carrying a space -- an ordinary thing on macOS, and possible anywhere -- must
// not split into two words.
func TestClaudeHookBlockQuotesTheScriptPath(t *testing.T) {
	var got hookJSON
	if err := json.Unmarshal(ClaudeHookBlock("/home/a b/it's/report.sh"), &got); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	cmd, _ := got.Hooks["Stop"][0].Hooks[0]["command"].(string)
	if want := `'/home/a b/it'\''s/report.sh' Stop`; cmd != want {
		t.Errorf("command = %q, want %q", cmd, want)
	}
	// And the quoting has to be quoting rather than decoration: a real shell
	// has to see one word.
	out, err := exec.Command("/bin/sh", "-c", "set -- "+cmd+`; printf '%s\n' "$#" "$1"`).Output()
	if err != nil {
		t.Fatalf("running the generated command line through a shell: %v", err)
	}
	if want := "2\n/home/a b/it's/report.sh\n"; string(out) != want {
		t.Errorf("the shell split the command line into %q, want %q", out, want)
	}
}

// -- the embed --------------------------------------------------------------

// TestEveryEmbeddedFileIsReachable is the guard on the //go:embed directive,
// and it is here rather than in Task 20 because Task 20 cannot fail without it:
// a name dropped from the directive is not a compile error, it is a ReadFile
// that returns fs.ErrNotExist at install time, on a user's machine.
//
// queue.ts is in the list for a reason that is easy to undo: neither pi.ts nor
// opencode.js works without it -- both import it -- so an installer that writes
// the two integrations and not the queue writes two files that throw on load.
func TestEveryEmbeddedFileIsReachable(t *testing.T) {
	want := []string{"claude-report.sh", "opencode.js", "pi.ts", "queue.ts"}
	if !equalStrings(Files, want) {
		t.Fatalf("Files = %v, want %v", Files, want)
	}
	for _, name := range Files {
		b, err := File(name)
		if err != nil {
			t.Errorf("File(%q): %v -- the //go:embed directive in this file is the only way these bytes reach the installer", name, err)
			continue
		}
		if len(b) == 0 {
			t.Errorf("File(%q) is empty", name)
		}
		// The bytes are the repo's own file, not a stale copy: the embed reads
		// from disk at build time, and this is the assertion that says so.
		on, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("reading %s from disk: %v", name, err)
		}
		if !bytes.Equal(b, on) {
			t.Errorf("File(%q) does not match the file on disk", name)
		}
	}
	if _, err := File("nonexistent.sh"); err == nil {
		t.Error("File on an unembedded name returned no error")
	}
}

// -- the wrapper ------------------------------------------------------------

// The wrapper's whole job is to FAIL QUIETLY, so every row of every test below
// asserts the same three things: exit 0, nothing on stdout, and promptly.
//
// Why stdout and not just the exit code: a PreToolUse hook's stdout is READ,
// and a JSON object with a permissionDecision in it is how a hook denies a tool
// call. A wrapper that echoed anything would be a wrapper that can, on the
// right day, produce something Claude parses.

// TestClaudeWrapperHandsOverArgvAndStdinUntouched is the one positive
// assertion: everything else here is about silence.
//
// Stdin is the assertion that matters. `report` refuses a payload it could not
// parse -- Task 15 made that a refusal rather than a fall-through, because a
// payload nobody could read is one where claude's `agent_id` is absent for the
// worst reason -- so a wrapper that loses stdin does not fail loudly. It makes
// every Stop write nothing, and the pane sits out the 60-second working expiry
// instead of badging a finish. `cmd &` in a non-interactive sh is exactly that
// mutant: POSIX redirects a background command's stdin from /dev/null.
func TestClaudeWrapperHandsOverArgvAndStdinUntouched(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")
	script := installScript(t, dir, recordingStub(t, dir, log))

	// A real payload, not a token: newlines and quotes are what a wrapper that
	// round-trips stdin through a shell variable would eat.
	payload, err := os.ReadFile(filepath.Join("..", "..", "cmd", "tmux-web", "testdata", "hooks", "claude", "stop.json"))
	if err != nil {
		t.Fatalf("reading the recorded Stop payload: %v", err)
	}

	run(t, script, []string{"Stop"}, string(payload))

	got, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("the wrapper never ran the binary: %v", err)
	}
	argv, stdin, _ := strings.Cut(string(got), "\n--stdin--\n")
	if want := "report --agent claude --event Stop"; argv != want {
		t.Errorf("argv = %q, want %q", argv, want)
	}
	if stdin != string(payload) {
		t.Errorf("stdin arrived as %q, want the payload byte for byte (%d bytes, got %d)",
			stdin, len(payload), len(stdin))
	}
}

// TestClaudeWrapperIsQuietOnEveryFailure is the table the plan names, plus the
// two rows that come from the placeholder.
func TestClaudeWrapperIsQuietOnEveryFailure(t *testing.T) {
	bin := buildTmuxWeb(t)

	cases := []struct {
		name  string
		bin   string
		argv  []string
		stdin string
	}{
		// The reason the -x guard exists. Without it the shell's own "not
		// found" reaches the hook's stderr on every tool call, and the exit
		// status is 127.
		{"BIN missing", filepath.Join(t.TempDir(), "gone", "tmux-web"), []string{"Stop"}, "{}"},
		// Half-installed: the file is there and is not runnable.
		{"BIN not executable", writeFile(t, filepath.Join(t.TempDir(), "tmux-web"), "#!/bin/sh\n", 0o644), []string{"Stop"}, "{}"},
		// The file as it ships, before any installer has touched it. The
		// placeholder is not a path, so the same guard catches it -- which is
		// why the placeholder lives inside the quotes and not in place of the
		// whole assignment.
		{"placeholder unsubstituted", ClaudeBinPlaceholder, []string{"Stop"}, "{}"},
		// From here down the binary is the real one, so these rows are the
		// wrapper and `report` together.
		{"no arguments", bin, nil, "{}"},
		{"garbage on stdin", bin, []string{"Stop"}, "\x00\x01 not json at all {"},
		{"empty stdin", bin, []string{"PreToolUse"}, ""},
		{"an event nothing knows", bin, []string{"SubagentStop"}, `{"agent_id":"a"}`},
		// $TMUX is unset for every row (see run), so this is the ordinary
		// outside-tmux case with a payload that would otherwise write.
		{"a real payload outside tmux", bin, []string{"UserPromptSubmit"}, `{"prompt":"hi"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			script := installScript(t, t.TempDir(), tc.bin)
			run(t, script, tc.argv, tc.stdin)
		})
	}
}

// -- fixture ----------------------------------------------------------------

// run executes the wrapper and asserts the three properties. The environment is
// BUILT rather than inherited: $TMUX leaking through from the developer's shell
// would point `report` at the developer's live tmux server, which holds real
// work and running agents.
func run(t *testing.T, script string, argv []string, stdin string) {
	t.Helper()
	cmd := exec.Command("/bin/sh", append([]string{script}, argv...)...)
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	start := time.Now()
	err := cmd.Run()
	elapsed := time.Since(start)

	if err != nil {
		t.Errorf("exit status %v (stderr: %s). Every path out of this wrapper is exit 0: only exit 2 blocks a PreToolUse tool call, and the distance between 1 and 2 is one character in a file nobody re-reads",
			err, strings.TrimSpace(stderr.String()))
	}
	if stdout.Len() != 0 {
		t.Errorf("wrote %q to stdout. A PreToolUse hook's stdout is READ -- a JSON permissionDecision on it denies the tool call -- so this wrapper writes nothing there, ever",
			stdout.String())
	}
	// "Promptly" is the third property and it is not decoration: the hook is
	// async, but the process still exists, and a wrapper that blocked would
	// hold one per tool call.
	if elapsed > 5*time.Second {
		t.Errorf("took %s; the wrapper does nothing that can block", elapsed)
	}
	if s := strings.TrimSpace(stderr.String()); s != "" {
		t.Logf("stderr (allowed, and this is where `report` says why it did nothing): %s", s)
	}
}

// installScript writes claude-report.sh into dir with BIN resolved, exactly as
// Task 20's installer will. The file under test is the EMBEDDED one, so a
// change to the script that this package did not rebuild cannot pass here.
func installScript(t *testing.T, dir, bin string) string {
	t.Helper()
	body := ClaudeReportScript(bin)
	if bin == ClaudeBinPlaceholder {
		// The as-shipped file: no substitution at all.
		var err error
		if body, err = File("claude-report.sh"); err != nil {
			t.Fatalf("reading the embedded script: %v", err)
		}
	}
	return writeFile(t, filepath.Join(dir, "claude-report.sh"), string(body), 0o755)
}

// recordingStub is a `tmux-web` that writes down what it was given instead of
// touching a tmux server. Its own stdout stays empty on purpose: the wrapper's
// stdout is the child's, so a stub that printed would make the stdout assertion
// pass for the wrong reason -- or fail for one.
func recordingStub(t *testing.T, dir, log string) string {
	t.Helper()
	return writeFile(t, filepath.Join(dir, "tmux-web"),
		"#!/bin/sh\n{ printf '%s\\n--stdin--\\n' \"$*\"; cat; } > "+shellQuote(log)+"\n", 0o755)
}

func writeFile(t *testing.T, path, body string, mode os.FileMode) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	return path
}

// buildTmuxWeb builds the real binary once for the whole package, because the
// rows that matter most -- garbage on stdin, no arguments, outside tmux -- are
// claims about the wrapper AND `report` together, and a stub would let a
// wrapper that mangles argv pass them all.
var (
	buildOnce sync.Once
	builtBin  string
	buildErr  error
	buildLog  string
)

func buildTmuxWeb(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "tmux-web-build")
		if err != nil {
			buildErr = err
			return
		}
		builtBin = filepath.Join(dir, "tmux-web")
		cmd := exec.Command("go", "build", "-o", builtBin, "../../cmd/tmux-web")
		out, err := cmd.CombinedOutput()
		buildErr, buildLog = err, string(out)
	})
	if buildErr != nil {
		t.Fatalf("building cmd/tmux-web: %v\n%s", buildErr, buildLog)
	}
	return builtBin
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func hookCounts(matchers []hookMatcherJSON) []int {
	out := make([]int, len(matchers))
	for i, m := range matchers {
		out[i] = len(m.Hooks)
	}
	return out
}

// TestTheOneExitCodeThisWrapperCannotMake0 records the single path out of
// claude-report.sh that is not exit 0, because it is better recorded than
// rediscovered.
//
// `exec` is what hands the hook's stdin socket over untouched, and it is also
// what gives the wrapper no say in what happens next: if BIN passes `[ -x ]`
// and then fails to exec -- a half-written binary, the wrong architecture, a
// dead interpreter after a partial upgrade -- the shell exits 126 or 127 and no
// line in this file can intercept it. Measured: 126 with a bad interpreter.
//
// It is left as it is rather than replaced with `"$BIN" …; exit 0`, and the
// reasoning is what makes this a decision and not an oversight:
//
//   - Every hook this project installs is `"async": true`, and an async command
//     hook's exit code is IGNORED. Nothing reads this number at all.
//   - If somebody deleted `async` from their own settings.json, exit 2 is the
//     only code that blocks a PreToolUse tool call. 126 and 127 are non-blocking
//     errors that surface stderr, and neither can become 2: they come from the
//     shell's own exec failure, not from anything `report` returns.
//
// So what is asserted here is what actually matters on this path -- nothing on
// stdout -- plus the exit code as a recorded fact. If this ever needs to be an
// unconditional 0, the change is to drop `exec` and add `exit 0`, and the cost
// is one shell process alive for the ~8 ms the report takes.
func TestTheOneExitCodeThisWrapperCannotMake0(t *testing.T) {
	dir := t.TempDir()
	bin := writeFile(t, filepath.Join(dir, "tmux-web"), "#!/nonexistent/interp\n", 0o755)
	script := installScript(t, dir, bin)

	cmd := exec.Command("/bin/sh", script, "Stop")
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	cmd.Stdin = strings.NewReader("{}")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()

	var code int
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	}
	if code != 126 && code != 127 {
		t.Errorf("an unrunnable BIN exited %d (%v); the recorded values are 126 and 127. If this is now 0 the wrapper stopped using exec, and stdin forwarding needs re-checking; if it is 2, a PreToolUse tool call can be BLOCKED and that is the one outcome this whole design forbids",
			code, err)
	}
	if stdout.Len() != 0 {
		t.Errorf("wrote %q to stdout even on the exec-failure path", stdout.String())
	}
	t.Logf("exec failure: exit %d, stdout empty, stderr %q", code, strings.TrimSpace(stderr.String()))
}
