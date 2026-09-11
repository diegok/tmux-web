package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/diegok/tmux-web/internal/tmux/testutil"
)

// Layer 3 of Task 18's test story: the WIRING, against a real opencode. Not
// what an event means -- that is events_test.go's table over the recorded
// fixtures -- and not the child-session filter, which opencode.test.ts drives
// directly with a fake report and which THIS FILE CANNOT REACH AT ALL: a
// subagent needs a working model, and this test deliberately has no
// credentials. What only a real opencode can answer is whether the plugin is
// loaded, whether an async default export returning a hook object is still the
// form, whether `event` / `chat.message` / `tool.execute.before` are still the
// hook names, and -- the one that cannot be faked -- what the real event bus
// does to a plugin that forwards the wrong things.
//
// That last one is why these tests exist even though the gates are covered in
// vitest. opencode's bus is CHATTY: one `message.part.updated` per streamed
// chunk, 45 `plugin.added` at startup, a `session.updated` on every turn of
// the crank. A fixture cannot show you that; a real run can, and does.
//
// A recording stub named `tmux-web` on PATH stands in for the subcommand, so
// nothing here writes a tmux option or touches a real pane. Everything runs
// under an isolated HOME and isolated XDG directories -- the owner's
// ~/.config/opencode/opencode.jsonc must not be read, let alone written -- and
// with no provider credential at all: the turn is EXPECTED TO FAIL at the
// provider, and it does not matter, because every event asserted on here fires
// before or after the model call rather than because of it. Measured: a run
// that died on `ProviderAuthError` still produced chat.message, two
// session.status(busy), a session.error and two session.idle.
//
// If opencode is not installed, these SKIP LOUDLY.

// opencodeReportedEvents is every event name this plugin is allowed to spawn a
// report for, and it is the whole of Task 18's forwarding whitelist held up
// against a live bus. The six are exactly eventRules["opencode"]'s keys, which
// is the point: a seventh here would be a fork for something the Go table
// ignores, and dropping the whitelist entirely is dozens of forks per turn.
var opencodeReportedEvents = map[string]bool{
	"chat.message":        true,
	"session.status":      true,
	"tool.execute.before": true,
	"todo.updated":        true,
	"permission.asked":    true,
	"session.idle":        true,
}

// TestOpencodePluginWiresTheBusToTheReportArgv runs a real opencode in a
// throwaway tmux session and asserts on what the plugin spawned.
//
// It asserts the TURN-START INVARIANT as a sequence. opencode's turn end is an
// edge -- it writes a resting state without reading what is standing -- and
// that is only safe if a working-producing event was written first. opencode
// has two of those, `chat.message` and `session.status(busy)`, and the queue
// collapses a burst to the newest, so which of them survives into the log on
// any given run is not fixed. What is fixed is that the turn start is IN FRONT
// OF the turn end, and that is what is checked.
func TestOpencodePluginWiresTheBusToTheReportArgv(t *testing.T) {
	oc := requireOpencode(t)
	dir := t.TempDir()
	log := filepath.Join(dir, opencodeEventsFile)

	// A launcher rather than a long tmux command line: tmux would otherwise
	// re-split the whole thing through a shell of its own, and the env has to
	// be exact. stdin is closed because `opencode run` waits on it otherwise
	// and the turn never starts.
	script := filepath.Join(dir, "run-opencode.sh")
	writeExecutable(t, script, "#!/bin/sh\n"+
		exports(opencodeEnv(t, dir, log))+
		"exec "+oc+" "+shellArgs(opencodeArgs(t, dir))+" </dev/null\n")

	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "ocwire", "-x", "120", "-y", "40", "-c", dir, script)

	// The turn start. It is the first thing the plugin can report, and the
	// plugin is only loaded at all because the pane's TMUX_PANE reached it.
	waitForCalls(t, log, 1, opencodeColdStart, "opencode to start and report its chat.message")
	first := readCalls(t, log)[0]
	if first.argv != "report --agent opencode --event chat.message" {
		t.Errorf("first call argv = %q, want the argv spawnReport builds for opencode's chat.message", first.argv)
	}
	// The turn start is rung 4 of the activity ladder -- a state-only report --
	// and the only string in chat.message's payload that is not an id is
	// output.parts[].text, the user's raw prompt. This assertion is what keeps
	// it out.
	if first.stdin != "{}" {
		t.Errorf("chat.message stdin = %q, want {} -- this mapping has no activity text, and the payload's only real string is the prompt the user typed",
			first.stdin)
	}

	// The turn end, which arrives whether the turn succeeded or failed.
	waitForEvent(t, log, "session.idle", opencodeTurn, "the turn to end and report session.idle")

	calls := readCalls(t, log)
	var events []string
	for _, c := range calls {
		name := eventOf(c.argv)
		events = append(events, name)
		// The reductions, the whitelist and the 1 KiB cap live in Go. An
		// integration that put a payload on the command line would bypass all
		// three -- and would put a user's prompt in the process table.
		if strings.Contains(c.argv, "--text") {
			t.Errorf("call %q carries --text: the activity text is derived in `tmux-web report`, never passed in by an integration", c.argv)
		}
		if !strings.HasPrefix(c.argv, "report --agent opencode --event ") {
			t.Errorf("call argv = %q, want exactly `report --agent opencode --event <name>`", c.argv)
		}
		// The live-bus half of the forwarding whitelist. Around sixty events
		// reached the `event` hook in the run this was written against, and
		// only the ones below may become a process.
		if !opencodeReportedEvents[name] {
			t.Errorf("the plugin reported %q, which eventRules[\"opencode\"] does not map. Forwarding the bus unfiltered is a fork per streamed chunk and per plugin.added; the payload was %.120q",
				name, c.stdin)
		}
	}

	// The sequence, not a set. A turn end with no working in front of it is
	// suppressed by `report`, and the pane loses that turn's badge.
	start, end := indexOfEvent(events, "chat.message"), indexOfEvent(events, "session.idle")
	if start < 0 || end < 0 || start > end {
		t.Errorf("recorded events = %v; want a turn start before the turn end. opencode's session.idle is a RE-ASSERTION -- measured firing twice inside one resting period, so it reads the standing report and writes only on a disagreement -- and with no working reported in front of it there is nothing to disagree with and the finish is suppressed",
			events)
	}
	if last := events[len(events)-1]; last != "session.idle" {
		t.Errorf("last recorded event = %q, want session.idle: the turn end must be the last thing written, or the state standing on the pane is not the state the agent is in", last)
	}

	// And the turn end goes over whole, because Task 18's filter needs the
	// session id and the Go table reads `properties`.
	idle := calls[len(calls)-1]
	var p struct {
		Type       string `json:"type"`
		Properties struct {
			SessionID string `json:"sessionID"`
		} `json:"properties"`
	}
	if err := json.Unmarshal([]byte(idle.stdin), &p); err != nil {
		t.Fatalf("session.idle stdin is not the event object: %v (%.200q)", err, idle.stdin)
	}
	if p.Type != "session.idle" || !strings.HasPrefix(p.Properties.SessionID, "ses_") {
		t.Errorf("session.idle stdin = %.200q, want the bus event whole -- {type, properties.sessionID} is what the child filter and the Go table both read", idle.stdin)
	}
}

// TestOpencodePluginReportsNothingOutsideATmuxPane is the TMUX_PANE guard,
// against a real opencode that really has no pane.
//
// This is the `opencode serve` case. The plugin runs IN-PROCESS, so in the
// ordinary TUI it inherits the pane's TMUX_PANE -- but a server started
// somewhere else and attached to from a pane puts this same plugin in the
// server's process, where either there is no pane to report on or there is a
// STALE TMUX_PANE from whatever shell started the server, and this session's
// state would be written onto somebody else's row. The guard is why this test
// asserts on silence rather than on a state.
func TestOpencodePluginReportsNothingOutsideATmuxPane(t *testing.T) {
	oc := requireOpencode(t)
	dir := t.TempDir()
	log := filepath.Join(dir, opencodeEventsFile)

	ctx, cancel := context.WithTimeout(context.Background(), opencodeColdStart+opencodeTurn)
	defer cancel()
	cmd := exec.CommandContext(ctx, oc, opencodeArgs(t, dir)...)
	cmd.Dir = dir
	// Built, not inherited, so TMUX_PANE is absent however this test was run.
	cmd.Env = opencodeEnv(t, dir, log)
	out, err := cmd.CombinedOutput()
	// The exit status is not the assertion and must not be one: there is no
	// credential, so this run fails, and it would fail differently on a machine
	// with no network. What is asserted is that opencode loaded the plugin and
	// the plugin said nothing.
	t.Logf("opencode run exited (%v); its whole event bus fired and nothing reported: %s", err, firstOutputLine(out))

	if calls := readCalls(t, log); len(calls) != 0 {
		t.Fatalf("an opencode with no TMUX_PANE reported %d time(s): %+v. There is no pane behind those writes, or worse, somebody else's",
			len(calls), calls)
	}
}

// -- fixture ----------------------------------------------------------------

// opencodeEventsFile is the file the stub appends to. Same name and same
// format as the pi wiring test's, because readCalls and waitForCalls are
// shared with it.
const opencodeEventsFile = "calls.log"

// The two waits, and they are different numbers on purpose: a cold opencode
// has a runtime to start, a project to scan and a model catalog to resolve,
// where a turn that dies at the provider is a couple of seconds. Measured on
// 1.18.30: 7.4 s for a whole warm run, well inside both. Keeping them apart is
// not tidiness -- a dropped hook is a mutant whose only symptom here is a wait
// that never finishes, and minutes spent proving that is how a mutation run
// stops being something anyone does.
const (
	opencodeColdStart = 90 * time.Second
	opencodeTurn      = 30 * time.Second
)

// requireOpencode finds opencode or skips the test by name. Loudly: a suite
// that silently skips its only wiring test looks green while testing nothing.
func requireOpencode(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("opencode")
	if err != nil {
		t.Skipf("SKIPPING %s: opencode is not installed (%v). The plugin's wiring -- that it loads, registers three hooks and spawns `tmux-web report --agent opencode --event <name>` for six events and no others -- is NOT covered on this machine. Its two gates still are, in internal/integrations/opencode.test.ts",
			t.Name(), err)
	}
	return path
}

// opencodePrompt is written for this test and goes nowhere: there is no
// credential, and the turn start fires before the request either way. It holds
// no shell metacharacter, because the tmux launcher puts it through a shell.
const opencodePrompt = "write a haiku about tmux panes"

// opencodeArgs is how these tests run opencode: one non-interactive turn, on a
// model pinned to a provider the environment has no key for.
func opencodeArgs(t *testing.T, dir string) []string {
	t.Helper()
	installPluginShim(t, dir)
	return []string{"run", "--model", "google/gemini-2.5-flash-lite", opencodePrompt}
}

// installPluginShim puts a one-line re-export of THE REPO'S OWN FILE in the
// project's plugin directory.
//
// A copy would be a second source that drifts, and it would break the relative
// `./queue.ts` import besides -- the queue has to resolve out of
// internal/integrations/, which is the directory the installer //go:embed's.
func installPluginShim(t *testing.T, dir string) {
	t.Helper()
	plugin, err := filepath.Abs(filepath.Join("..", "..", "internal", "integrations", "opencode.js"))
	if err != nil {
		t.Fatalf("resolving the plugin path: %v", err)
	}
	if _, err := os.Stat(plugin); err != nil {
		t.Fatalf("the plugin under test is missing: %v", err)
	}
	pluginDir := filepath.Join(dir, ".opencode", "plugin")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", pluginDir, err)
	}
	// Only the default export: queue.ts has none, and a plugin directory that
	// contained it would be asked to load it as a plugin of its own.
	if err := os.WriteFile(filepath.Join(pluginDir, "tmux-web.js"),
		[]byte("export { default } from \""+plugin+"\"\n"), 0o644); err != nil {
		t.Fatalf("writing the plugin shim: %v", err)
	}
}

// opencodeEnv is the whole environment the opencode process gets. It is built
// rather than inherited, and that is a safety property, not tidiness:
//
//   - HOME and all four XDG directories point into the test's own directory,
//     so this test cannot read or write the developer's
//     ~/.config/opencode/opencode.jsonc, their auth.json or their session
//     storage.
//   - No provider credential of any kind is passed, and the model is pinned to
//     one provider, so no key from the developer's environment can be picked
//     up. The turn fails at the provider on purpose.
//   - PATH carries the recording stub FIRST, which is how `tmux-web` resolves
//     to something that records instead of to something that writes a tmux
//     option.
//
// TMUX_PANE is deliberately NOT here. In the tmux test it comes from the pane
// the launcher runs in, which is the real path; in the guard test its absence
// is the whole point.
func opencodeEnv(t *testing.T, dir, log string) []string {
	t.Helper()
	stub := filepath.Join(dir, "bin")
	if err := os.MkdirAll(stub, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", stub, err)
	}
	// One line per invocation: the argv, then whatever arrived on stdin. `cat`
	// first, because the shell must read the pipe before the parent's write
	// can complete.
	writeExecutable(t, filepath.Join(stub, "tmux-web"),
		"#!/bin/sh\npayload=$(cat)\nprintf '%s | %s\\n' \"$*\" \"$payload\" >> \"$TMUX_WEB_STUB_LOG\"\n")
	home := filepath.Join(dir, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", home, err)
	}
	return []string{
		"PATH=" + stub + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + home,
		"TERM=" + os.Getenv("TERM"),
		"XDG_CONFIG_HOME=" + filepath.Join(dir, "xdg-config"),
		"XDG_DATA_HOME=" + filepath.Join(dir, "xdg-data"),
		"XDG_CACHE_HOME=" + filepath.Join(dir, "xdg-cache"),
		"XDG_STATE_HOME=" + filepath.Join(dir, "xdg-state"),
		"TMUX_WEB_STUB_LOG=" + log,
	}
}

// waitForEvent blocks until the stub has recorded a call for the named event.
//
// It is a predicate rather than a count because the queue COLLAPSES a burst to
// the newest item: how many calls a turn produces is not fixed, and a test
// that waited for "five calls" would be waiting on the scheduler.
func waitForEvent(t *testing.T, log, event string, within time.Duration, what string) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		calls := readCalls(t, log)
		for _, c := range calls {
			if eventOf(c.argv) == event {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("waited %s for %s; recorded so far: %+v", within, what, calls)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// shellArgs quotes an argv for the launcher script tmux runs, so that a prompt
// with spaces in it stays one argument.
func shellArgs(args []string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(quoted, " ")
}

// indexOfEvent is the first position of an event name in a recorded sequence,
// or -1.
func indexOfEvent(events []string, name string) int {
	for i, e := range events {
		if e == name {
			return i
		}
	}
	return -1
}

// The post-install oracle, and the only thing in
// this repository that can say a `--global --agent opencode` install actually
// reaches opencode rather than merely reaching a plausible directory. It SKIPS
// LOUDLY without opencode.
//
// `opencode debug config` resolves the whole plugin set and prints
// `plugin_origins`, one entry per plugin with its `spec`, the `source`
// directory it came from and its `scope`. A directory drop shows up there, so
// it answers both halves: the install is `"scope": "global"` with our path in
// it, and `--remove` takes it back out of that array entirely.
//
// This also measures the claim that made the project-scope footprint warning
// wrong for a global install: with only a global plugin present, the project
// directory this runs in must still be EMPTY afterwards.
func TestAGlobalOpencodeInstallIsLoadedAndRemovable(t *testing.T) {
	oc := requireOpencode(t)
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	xdg := filepath.Join(dir, "xdg-config")
	project := filepath.Join(dir, "project")
	for _, d := range []string{home, project} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", xdg)

	installCLI(t, "", "--agent", "opencode", "--global", "--yes").wantCode(t, 0)
	plugin := filepath.Join(xdg, "opencode", "plugin", "tmux-web.js")

	origins := opencodePluginOrigins(t, oc, dir, project)
	found := false
	for _, o := range origins {
		if strings.Contains(o.Spec, plugin) {
			found = true
			if o.Scope != "global" {
				t.Errorf("opencode loaded our plugin at scope %q, want \"global\": a --global install that lands somewhere opencode calls local is an install whose reach depends on where the user runs opencode from", o.Scope)
			}
		}
	}
	if !found {
		t.Fatalf("opencode does not load %s at all. plugin_origins = %+v", plugin, origins)
	}

	// The project stayed empty. This is the measurement behind not printing the
	// repository-footprint warning for a global install: opencode's
	// package.json, node_modules/ and .gitignore land in its OWN config
	// directory, beside the plugin.
	entries, err := os.ReadDir(project)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("a global opencode plugin put %d entries in the project directory: %v. The global install's note tells the user it does not touch their repository", len(entries), entries)
	}
	if _, err := os.Stat(filepath.Join(xdg, "opencode", "node_modules")); err != nil {
		t.Errorf("opencode's bootstrap is not in its own config directory either (%v); the note names a footprint that is not there", err)
	}

	// And back out. `--remove` is one unlink, and the oracle says so.
	installCLI(t, "", "--agent", "opencode", "--global", "--yes", "--remove").wantCode(t, 0)
	for _, o := range opencodePluginOrigins(t, oc, dir, project) {
		if strings.Contains(o.Spec, plugin) {
			t.Errorf("opencode still loads %s after --remove: %+v", plugin, o)
		}
	}
}

// pluginOrigin is one entry of `opencode debug config`'s resolved plugin set.
type pluginOrigin struct {
	Spec   string `json:"spec"`
	Source string `json:"source"`
	Scope  string `json:"scope"`
}

// opencodePluginOrigins runs `opencode debug config` in a throwaway project
// under throwaway XDG directories and returns what it resolved.
func opencodePluginOrigins(t *testing.T, oc, dir, project string) []pluginOrigin {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), opencodeColdStart)
	defer cancel()
	cmd := exec.CommandContext(ctx, oc, "debug", "config")
	cmd.Dir = project
	// Built, not inherited, and the XDG_CONFIG_HOME here is the one the install
	// above was pointed at -- which is the whole assertion.
	cmd.Env = append(opencodeEnv(t, dir, filepath.Join(dir, opencodeEventsFile)),
		"XDG_CONFIG_HOME="+os.Getenv("XDG_CONFIG_HOME"), "HOME="+os.Getenv("HOME"))
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("opencode debug config: %v", err)
	}
	var config struct {
		PluginOrigins []pluginOrigin `json:"plugin_origins"`
	}
	if err := json.Unmarshal(out, &config); err != nil {
		t.Fatalf("opencode debug config did not print the JSON this oracle reads (%v):\n%.400s", err, out)
	}
	return config.PluginOrigins
}
