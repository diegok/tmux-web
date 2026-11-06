package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/diegok/tmux-web/internal/report"
	"github.com/diegok/tmux-web/internal/tmux/testutil"
)

// Layer 3 of Task 18's test story: the WIRING, against a real opencode. Not
// what an event means -- that is events_test.go's table over the recorded
// fixtures -- and not the child-session filter, which opencode.test.ts drives
// directly with a fake report and which THIS FILE CANNOT REACH AT ALL: a
// subagent needs a working model, and this test deliberately has no
// credentials. What only a real opencode can answer is whether the plugin is
// loaded, whether a default export of {id, setup} registering
// session.hook / tool.hook / event.subscribe is still the form, whether
// `prompt` / `tool.execute.before` / the bus events are still the names
// things arrive under, and -- the one that cannot be faked -- what the real
// event bus does to a plugin that forwards the wrong things.
//
// That last one is why these tests exist even though the gates are covered in
// vitest. opencode's bus is CHATTY: one delta event per streamed token under
// V2, a usage update per step, an inbox event per queued message. A fixture
// cannot show you that; a real run can, and does.
//
// A recording stub named `tmux-web` on PATH stands in for the subcommand, so
// nothing here writes a tmux option or touches a real pane. Everything runs
// under an isolated HOME and isolated XDG directories -- the owner's
// ~/.config/opencode/opencode.jsonc must not be read, let alone written -- and
// with no provider credential at all: the turn is EXPECTED TO FAIL at the
// provider, and it does not matter, because every event asserted on here fires
// before or after the model call rather than because of it. Measured on
// 1.18.30: a run that died on `ProviderAuthError` still produced chat.message,
// two session.status(busy), a session.error and two session.idle. Measured on
// 2.2.5, where status and idle are off the public bus entirely: the same dead
// run produces the prompt hook, session.step.started, session.step.failed and
// session.execution.failed.
//
// If opencode is not installed, these SKIP LOUDLY.

// opencodeReportedEvents is every event name this plugin is allowed to spawn a
// report for, and it is the whole of Task 18's forwarding whitelist held up
// against a live bus.
//
// TAKEN FROM THE TABLE rather than written out. It used to be a literal of six,
// with a comment saying they were exactly eventRules["opencode"]'s keys and
// nothing making that so; internal/report holds the plugin's own busEvents to
// the same list now, unconditionally, and this is the third end of it -- what a
// REAL opencode actually spawned. A seventh name here would be a fork for
// something the Go table ignores, and dropping the whitelist entirely is dozens
// of forks per turn.
func opencodeReportedEvents() map[string]bool {
	allowed := map[string]bool{}
	for _, event := range report.EventsFor("opencode") {
		allowed[event] = true
	}
	return allowed
}

// TestOpencodePluginWiresTheBusToTheReportArgv runs a real opencode in a
// throwaway tmux session and asserts on what the plugin spawned.
//
// It asserts the TURN-START INVARIANT as a sequence. opencode's turn end is an
// edge -- it writes a resting state without reading what is standing -- and
// that is only safe if a working-producing event was written first. opencode
// has several of those -- the `prompt` hook, `session.execution.started`,
// `session.step.started`, `tool.execute.before` -- and the queue collapses a
// burst to the newest, so which of them survives into the log on any given run
// is not fixed. What is fixed is that the turn start is IN FRONT OF the turn
// end, and that is what is checked.
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
	waitForCalls(t, log, 1, opencodeColdStart, "opencode to start and report its prompt")
	first := readCalls(t, log)[0]
	if first.argv != "report --agent opencode --event prompt" {
		t.Errorf("first call argv = %q, want the argv spawnReport builds for opencode's prompt hook", first.argv)
	}
	// The turn start is rung 4 of the activity ladder -- a state-only report --
	// and the only string in the prompt event's payload that is not an id is
	// prompt.text, the user's raw prompt. This assertion is what keeps it out.
	if first.stdin != "{}" {
		t.Errorf("prompt stdin = %q, want {} -- this mapping has no activity text, and the payload's only real string is the prompt the user typed",
			first.stdin)
	}

	// The turn end, which arrives whether the turn succeeded or failed. Under
	// no provider credential this run will fail at the provider, and that is
	// the point: V1 measured the dead turn reporting its end too. What the
	// whitelist below holds the plugin to is the whole set; what this wait
	// needs is the one a dead turn actually fires.
	waitForEvent(t, log, "session.execution.failed", opencodeTurn,
		"the dead turn to report session.execution.failed")

	calls := readCalls(t, log)
	allowed := opencodeReportedEvents()
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
		// The live-bus half of the forwarding whitelist. Several hundred
		// events reached the subscription in the capture this was written
		// against -- one delta event per streamed token -- and only the ones
		// below may become a process.
		if !allowed[name] {
			t.Errorf("the plugin reported %q, which internal/report does not map. Forwarding the bus unfiltered is a fork per streamed chunk and per plugin.added; the payload was %.120q",
				name, c.stdin)
		}
	}

	// The sequence, not a set. A turn end with no working in front of it is
	// suppressed by `report`, and the pane loses that turn's badge.
	endOfTurn := func(name string) bool {
		return name == "session.execution.succeeded" || name == "session.execution.failed" ||
			name == "session.execution.interrupted"
	}
	start, end := indexOfEvent(events, "prompt"), -1
	for i, name := range events {
		if endOfTurn(name) {
			end = i
			break
		}
	}
	if start < 0 || end < 0 || start > end {
		t.Errorf("recorded events = %v; want a turn start before the turn end. A turn end with no working in front of it writes nothing, and the pane loses that turn's badge",
			events)
	}
	if last := events[len(events)-1]; !endOfTurn(last) {
		t.Errorf("last recorded event = %q, want a session.execution turn end: the turn end must be the last thing written, or the state standing on the pane is not the state the agent is in", last)
	}

	// And the turn end goes over whole, because Task 18's filter needs the
	// session id and the Go table reads `data`.
	idle := calls[len(calls)-1]
	var p struct {
		Type string `json:"type"`
		Data struct {
			SessionID string `json:"sessionID"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(idle.stdin), &p); err != nil {
		t.Fatalf("the turn end's stdin is not the event object: %v (%.200q)", err, idle.stdin)
	}
	if !endOfTurn(p.Type) || !strings.HasPrefix(p.Data.SessionID, "ses_") {
		t.Errorf("turn end stdin = %.200q, want the bus event whole -- {type, data.sessionID} is what the child filter and the Go table both read", idle.stdin)
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
	tmp := filepath.Join(dir, "tmp")
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", tmp, err)
	}
	// THE SERVICE PORT, and it is the reason this test once failed on every
	// machine that had opencode open. V2 runs opencode against a managed
	// background service, and the default channel binds a FIXED port (49374)
	// -- the port the developer's own running service is already holding.
	// MEASURED on 2.2.5: an isolated-HOME `opencode run` still tries 49374,
	// fails to bind, and exits 1 before any plugin loads -- which reads as
	// "the plugin did not load" and would have this test asserting on a boot
	// failure. The port is configured through service.json in XDG_CONFIG_HOME
	// (`opencode service set port <port>` writes exactly that file, one
	// "port" key), so the test writes it itself and takes a port in a range
	// nothing else here is told about.
	port := 20000 + (os.Getpid()+time.Now().Nanosecond())%19999
	cfg := filepath.Join(dir, "xdg-config", "opencode", "service.json")
	if err := os.MkdirAll(filepath.Dir(cfg), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(cfg), err)
	}
	if err := os.WriteFile(cfg, []byte(fmt.Sprintf("{\n  \"port\": %d\n}\n", port)), 0o644); err != nil {
		t.Fatalf("writing %s: %v", cfg, err)
	}
	return []string{
		"PATH=" + stub + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + home,
		"TERM=" + os.Getenv("TERM"),
		"XDG_CONFIG_HOME=" + filepath.Join(dir, "xdg-config"),
		"XDG_DATA_HOME=" + filepath.Join(dir, "xdg-data"),
		"XDG_CACHE_HOME=" + filepath.Join(dir, "xdg-cache"),
		"XDG_STATE_HOME=" + filepath.Join(dir, "xdg-state"),
		// TMPDIR, because this env is BUILT: a child given no TMPDIR writes to
		// /tmp, and opencode's runtime extracts a 5,576,816-byte shared object
		// there on EVERY start and never removes it. Measured on this machine:
		// one file per opencode process, 46 of them (245 MB) sitting in a /tmp
		// that is tmpfs -- so that is RAM, held by files nothing will read again.
		// Named here, the extraction lands under dir and belongs to t.TempDir()
		// like everything else this test makes. It is the same failure the build
		// directory in internal/integrations had, and it fills the same /tmp: the
		// visible symptom is this very test's sibling failing to unpack
		// node_modules, which reads as opencode misbehaving rather than as a full
		// disk. The directory has to exist first -- a child is not obliged to
		// create its own TMPDIR.
		"TMPDIR=" + tmp,
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
// V2 retired the V1 oracle's source: `opencode debug config` used to resolve
// the plugin set and print `plugin_origins` with a per-plugin `spec` and
// `scope`, and on 2.2.5 it prints only the plugin DIRECTORIES it scans
// (`[{type: "directory", path: ...}]`) -- which can say a directory is scanned
// but not that OUR file in it was accepted. What V2 does write, for every
// plugin file it accepts, is the server log line
// `loading plugin id=<path>` -- the same line whose absence, with a
// `failed to load plugin` WARN beside it, is how the V1-format file's failure
// was diagnosed. So the oracle boots opencode (three times, below) and reads
// the service's own log: our path under `loading plugin`, and no
// `failed to load plugin` for it.
//
// This also measures the claim that made the project-scope footprint warning
// wrong for a global install: with only a global plugin present, the project
// directory this runs in must still be EMPTY afterwards. Re-measured on 2.2.5
// it is, and so is the configuration directory -- V2 creates no bootstrap at
// all; see install.go's projectWarning.
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

	// Each boot gets its own port, its own XDG_DATA_HOME -- that is where the
	// service log lives, and a phase that asserts an ABSENCE cannot share a
	// log with a phase that asserts a presence -- and its own XDG_STATE_HOME,
	// because that is where the service REGISTRATION lives: a phase whose CLI
	// finds the previous phase's registration connects to that still-running
	// service instead of starting one, and reads a log the new install never
	// reached. `opencode api get /api/plugin` is the call this oracle makes:
	// MEASURED, /api/info boots the service too but never resolves plugins.
	// Every service a phase starts is stopped from t.Cleanup -- not at the end
	// of its own phase, because the phase that asserts a presence polls the log
	// after its CLI has exited -- so all three are running for the back half of
	// this test and none of them outlives it. They are whole opencode servers,
	// and leaving them behind would not be untidiness, it would be a leak.
	boot := func(name string, port int) ocPhase {
		t.Helper()
		data := filepath.Join(dir, name+"-data")
		state := filepath.Join(dir, name+"-state")
		// THE PORT GOES IN AFTER THE ENVIRONMENT IS BUILT, and the order is the
		// whole point: opencodeEnv writes a service.json of its own, with a
		// port of its own, into this very path. Written before it, the phase's
		// port is overwritten and the three boots are kept apart by a random
		// number instead of by construction.
		cfg := filepath.Join(xdg, "opencode", "service.json")
		env := func() []string {
			e := append(opencodeEnv(t, dir, filepath.Join(dir, opencodeEventsFile)),
				"XDG_CONFIG_HOME="+xdg, "HOME="+home, "XDG_DATA_HOME="+data, "XDG_STATE_HOME="+state)
			if err := os.MkdirAll(filepath.Dir(cfg), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(cfg, []byte(fmt.Sprintf("{\n  \"port\": %d\n}\n", port)), 0o644); err != nil {
				t.Fatal(err)
			}
			return e
		}
		run := func(args ...string) {
			t.Helper()
			ctx, cancel := context.WithTimeout(context.Background(), opencodeColdStart)
			defer cancel()
			cmd := exec.CommandContext(ctx, oc, args...)
			cmd.Dir = project
			cmd.Env = env()
			if err := cmd.Run(); err != nil {
				t.Fatalf("opencode %s did not run (%s): %v", strings.Join(args, " "), name, err)
			}
		}
		// The same call, for the answers a phase reads rather than the
		// booting. Errors are swallowed: every caller is a poller that has to
		// cope with an answer that is not there yet anyway, and a hard failure
		// here would turn a settle window into a flake.
		out := func(args ...string) string {
			ctx, cancel := context.WithTimeout(context.Background(), opencodeColdStart)
			defer cancel()
			cmd := exec.CommandContext(ctx, oc, args...)
			cmd.Dir = project
			cmd.Env = env()
			stdout, _ := cmd.Output()
			return string(stdout)
		}
		run("api", "get", "/api/plugin")
		log := filepath.Join(data, "opencode", "log", "opencode.log")
		if _, err := os.Stat(log); err != nil {
			t.Fatalf("the boot (%s) wrote no service log at %s: %v", name, log, err)
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, oc, "service", "stop")
			cmd.Dir = project
			cmd.Env = env()
			_ = cmd.Run()
		})
		return ocPhase{log: log, out: out}
	}

	// THE SKILL ORACLE, AND ITS POSITIVE CONTROL. `GET /api/skill` lists what
	// opencode loaded for a location, with no model and no credentials -- the
	// lever pi does not have. MEASURED on 2.2.5: the first call after a cold
	// start lists NOTHING AT ALL, opencode's own builtins included, so a zero
	// means "ask again" and a test that read it as "our skill is not loaded"
	// would be reading a race and calling it a measurement. This waits for the
	// builtins to appear and only then looks for ours -- which is also what
	// makes the absence asserted after --remove mean anything.
	skills := func(p ocPhase, within time.Duration) []ocSkill {
		t.Helper()
		deadline := time.Now().Add(within)
		for {
			var answer struct {
				Data []ocSkill `json:"data"`
			}
			out := p.out("api", "get", "/api/skill?location[directory]="+project)
			if json.Unmarshal([]byte(out), &answer) == nil && len(answer.Data) > 0 {
				return answer.Data
			}
			if time.Now().After(deadline) {
				t.Fatalf("opencode never listed one skill for %s, its own builtins included, so nothing "+
					"can be read into the absence of ours:\n%.400s", project, out)
			}
			time.Sleep(500 * time.Millisecond)
		}
	}

	// (1) A boot with nothing installed: the log names no plugin of ours.
	before := boot("before", 21000+os.Getpid()%8000)
	beforeLog, err := os.ReadFile(before.log)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(beforeLog), "tmux-web.js") {
		t.Fatalf("%s mentions tmux-web.js with nothing installed:\n%.400s", before.log, beforeLog)
	}
	for _, sk := range skills(before, 30*time.Second) {
		if sk.ID == "tmux-web" {
			t.Fatalf("opencode lists a tmux-web skill at %q with nothing installed", sk.Path)
		}
	}

	installCLI(t, "", "--agent", "opencode", "--global", "--yes").wantCode(t, 0)
	plugin := filepath.Join(xdg, "opencode", "plugin", "tmux-web.js")

	// (2) The boot after the install. The service resolves and loads plugins
	// while -- and briefly after -- the api call is being answered, so the
	// log is POLLED for the outcome rather than read once: the definitive
	// lines are `loading plugin` for our path, or `failed to load plugin`
	// beside it. The api CLI exits when ITS call is answered; the log keeps
	// moving after that.
	after := boot("after", 22000+os.Getpid()%8000)
	outcome := func(log string, within time.Duration) (string, bool) {
		t.Helper()
		deadline := time.Now().Add(within)
		for {
			b, err := os.ReadFile(log)
			if err == nil {
				for _, line := range strings.Split(string(b), "\n") {
					if strings.Contains(line, "tmux-web.js") &&
						(strings.Contains(line, `loading plugin`) || strings.Contains(line, "failed to load plugin")) {
						return line, true
					}
				}
			}
			if time.Now().After(deadline) {
				return "", false
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	line, ok := outcome(after.log, 15*time.Second)
	if !ok {
		afterLog, _ := os.ReadFile(after.log)
		t.Fatalf("opencode's service log never records an outcome for %s:\n%.600s", plugin, afterLog)
	}
	if strings.Contains(line, "failed to load plugin") {
		t.Fatalf("opencode refused our plugin: %.400s", line)
	}

	// And the other half of the install. The plugin reports the pane; the skill
	// is what teaches the agent the verbs, and opencode is the one agent whose
	// loaded-skill list can be read back without a model.
	skill := filepath.Join(xdg, "opencode", "skills", "tmux-web", "SKILL.md")
	loaded := ""
	for _, sk := range skills(after, 30*time.Second) {
		if sk.ID == "tmux-web" {
			loaded = sk.Path
		}
	}
	if loaded != skill {
		t.Errorf("opencode lists the tmux-web skill at %q, want %q: a global install that loads the "+
			"plugin and not the skill is half an install, and the half it loses is the half the agent reads",
			loaded, skill)
	}

	// The project stayed empty. This is the measurement behind not printing the
	// repository-footprint warning for a global install.
	entries, err := os.ReadDir(project)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("a global opencode plugin put %d entries in the project directory: %v. The global install's note tells the user it does not touch their repository", len(entries), entries)
	}

	// And back out. `--remove` is one unlink, and the oracle says so: a boot
	// with the file gone reaches no outcome for it -- give the service the
	// same settle window the positive case needed, then read.
	installCLI(t, "", "--agent", "opencode", "--global", "--yes", "--remove").wantCode(t, 0)
	removed := boot("removed", 23000+os.Getpid()%8000)
	if _, hit := outcome(removed.log, 3*time.Second); hit {
		removedLog, _ := os.ReadFile(removed.log)
		t.Errorf("opencode still loads or refuses %s after --remove:\n%.400s", plugin, removedLog)
	}
	for _, sk := range skills(removed, 30*time.Second) {
		if sk.ID == "tmux-web" {
			t.Errorf("opencode still lists the tmux-web skill at %q after --remove", sk.Path)
		}
	}
}

// ocPhase is one boot of this test: its service log, and a way to ask that
// service a question.
type ocPhase struct {
	log string
	out func(args ...string) string
}

// ocSkill is one entry of `GET /api/skill`, which is opencode's own answer to
// "which skills did you load".
type ocSkill struct {
	ID   string `json:"id"`
	Path string `json:"path"`
}

// pluginOrigin is one entry of V1's `opencode debug config` output, retired
// with the oracle that read it: 2.2.5 prints only the plugin directories it
// scans, with no per-plugin spec or scope, so nothing here can be asserted
// from it any more. See TestAGlobalOpencodeInstallIsLoadedAndRemovable for the
// oracle that replaced it.
