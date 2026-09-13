package tmux

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/diegok/tmux-web/internal/tmux/testutil"
)

// "Resume here": the table that says what each agent's own resume is, and the
// verb that runs it in a window of its own.
//
// NOTHING IN THIS FILE MAY EXECUTE A REAL AGENT. Every test that reaches tmux
// registers a throwaway entry (see registerResumeAgent) instead, because the
// shipped table's entries are `claude`, `pi` and `opencode` -- all three
// installed on the machine this suite runs on -- and a test that launched one
// would start an interactive session in somebody's account, write to their
// config and cost them a turn. The shipped argvs are asserted as data, which is
// what they are.

// --- the table ---------------------------------------------------------------

// The three invocations, spelled out, and each one MEASURED rather than read
// off a website.
//
// Written literally rather than ranged over from the map, for the reason
// AgentIcon.test.tsx writes its paths out: a test that asks the table what the
// table says passes however the table drifts, including two agents swapping
// commands -- and the cost of a wrong entry here is a command running in the
// owner's repository.
//
// How each was established, on 2026-09-13:
//
//   - `claude --resume`: `claude --help` lists "-r, --resume [value]  Resume a
//     conversation by session ID, or ..." -- with no value it opens Claude's
//     own picker.
//   - `pi --resume`: `pi --help` (pi 0.85.1) lists "--resume, -r  Select a
//     session to resume". Running it in a throwaway directory with a throwaway
//     PI_CODING_AGENT_DIR printed pi's own "Resume Session (Current Folder)"
//     list, so it is a picker and it is already scoped to the directory.
//   - `opencode --continue`: `opencode --help` lists "-c, --continue  continue
//     the last session" and "-s, --session  session id to continue". There is
//     no picker flag and the `session` subcommand only lists and deletes, so
//     opencode resumes the most recent session with no choice offered. The
//     menu wording is held to that in web/src/lib/manage.test.ts.
func TestTheThreeResumeInvocations(t *testing.T) {
	want := map[string][]string{
		"claude":   {"claude", "--resume"},
		"opencode": {"opencode", "--continue"},
		"pi":       {"pi", "--resume"},
	}
	if !reflect.DeepEqual(resumeCommands, want) {
		t.Errorf("the resume table is %v, want %v", resumeCommands, want)
	}
	// Stated separately because it is the one entry whose flag is easy to
	// "fix" into the wrong thing: opencode has no --resume, and giving it one
	// would fail at the agent rather than here.
	if got := resumeCommands["opencode"]; len(got) != 2 || got[1] != "--continue" {
		t.Errorf("opencode resumes with %v; it takes --continue and has no --resume", got)
	}
}

// The shipped table passes its own checks.
//
// checkResume runs in init and panics, so on the shipped data this can never
// fail -- the package would not load. It is here so the positive case is
// stated, exactly as internal/report states its own.
func TestTheShippedResumeTablePassesItsOwnChecks(t *testing.T) {
	if err := checkResume(resumeCommands, Agents); err != nil {
		t.Fatal(err)
	}
}

// Every way this table and the agent list could drift apart, handed to the
// checker. THIS IS THE TEST THAT MATTERS: the checker is the only thing between
// a drifted table and a shipped binary, and a checker that accepted everything
// would pass the test above.
func TestCheckResumeRefusesADriftedTable(t *testing.T) {
	agents := []string{"claude", "pi"}
	ok := func() map[string][]string {
		return map[string][]string{
			"claude": {"claude", "--resume"},
			"pi":     {"pi", "--resume"},
		}
	}
	for _, tc := range []struct {
		name  string
		table map[string][]string
		why   string
	}{
		{
			"an agent with no entry",
			map[string][]string{"claude": {"claude", "--resume"}},
			"pi would be offered a menu entry the daemon then refuses",
		},
		{
			"an entry for something that is not an agent",
			func() map[string][]string { m := ok(); m["vim"] = []string{"vim"}; return m }(),
			"a command reachable by name that nothing else in the app treats as an agent",
		},
		{
			"an empty argv",
			func() map[string][]string { m := ok(); m["pi"] = nil; return m }(),
			"new-window with no command opens a shell and calls it a resume",
		},
		{
			"an argv that runs something else",
			func() map[string][]string { m := ok(); m["pi"] = []string{"sh", "-c", "pi -r"}; return m }(),
			"the whole point of the table is that it can only run the agent itself",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := checkResume(tc.table, agents); err == nil {
				t.Errorf("checkResume accepted %v: %s", tc.table, tc.why)
			}
		})
	}
}

// A key tmux would read as a flag is refused, because ResumeAgent passes the
// key to `new-window -n`.
func TestCheckResumeRefusesAKeyTmuxWouldReadAsAFlag(t *testing.T) {
	agents := []string{"-r"}
	table := map[string][]string{"-r": {"-r"}}
	if err := checkResume(table, agents); err == nil {
		t.Error("checkResume accepted an agent named \"-r\"; tmux reads that as a flag on -n")
	}
}

// --- the verb ----------------------------------------------------------------

// registerResumeAgent installs a throwaway agent and its resume argv for one
// test, and returns the file the fake agent writes what it saw into.
//
// A throwaway rather than one of the three, and this is not tidiness: the real
// entries are the real agents, they are installed here, and a test that ran one
// would open an interactive session in the owner's account. The same reasoning
// as registerTestAgent in blocked_test.go, and the same mechanics -- package
// vars, restored on the way out, no test in this package runs in parallel.
//
// The entry deliberately does NOT satisfy checkResume: argv[0] is an absolute
// path to a script rather than the agent's own name. That rule is about the
// shipped table, which is what init checks; this is a fixture standing in for
// one so the verb can be driven against a real tmux at all.
func registerResumeAgent(t *testing.T, name string) (argvFile string) {
	t.Helper()
	if _, taken := resumeCommands[name]; taken {
		t.Fatalf("%q is a real agent: pick a name nothing ships", name)
	}
	dir := t.TempDir()
	argvFile = filepath.Join(dir, "argv")
	script := filepath.Join(dir, name)
	// pwd -P rather than $PWD: tmux sets the process's working directory, but
	// the PWD in the inherited environment is whatever the daemon's was.
	body := "#!/bin/sh\n{ pwd -P; printf '%s\\n' \"$@\"; } > " + argvFile + "\nsleep 30\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake agent: %v", err)
	}
	resumeCommands[name] = []string{script, "--resume"}
	t.Cleanup(func() { delete(resumeCommands, name) })
	return argvFile
}

// resumeFixture is a server with one session whose pane sits in a directory of
// its own -- the "here" a resume is supposed to land in.
type resumeFixture struct {
	srv     *testutil.Server
	c       *Client
	session string
	pane    string
	dir     string
}

func newResumeFixture(t *testing.T) *resumeFixture {
	t.Helper()
	srv := testutil.NewServer(t)
	f := &resumeFixture{srv: srv, dir: mkdir(t, t.TempDir(), "project")}
	f.session = srv.Run(t, "new-session", "-d", "-s", "work", "-x", "80", "-y", "24",
		"-c", f.dir, "-P", "-F", "#{session_id}")
	f.pane = srv.Run(t, "list-panes", "-t", f.session, "-F", "#{pane_id}")
	f.c = NewClient(srv.Args())
	return f
}

// windows is every window id on the server, so "nothing was created" is an
// assertion rather than a hope.
func (f *resumeFixture) windows(t *testing.T) string {
	t.Helper()
	return f.srv.Run(t, "list-windows", "-a", "-F", "#{window_id}")
}

// waitFor polls for a file the fake agent writes, because new-window returns as
// soon as tmux has forked the pane.
func waitFor(t *testing.T, path string) string {
	t.Helper()
	for i := 0; i < 200; i++ {
		if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
			return string(b)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s was never written: the resume window ran nothing", path)
	return ""
}

// The whole feature in one assertion: the window runs the agent's own resume
// command, and it runs it in the pane's directory.
//
// Both halves matter and neither implies the other. A window with no -c opens
// in the daemon's own working directory, where the agent's history is somebody
// else's project or nothing at all -- which is a resume that looks like it
// worked.
func TestResumeAgentRunsTheTableCommandInThePaneDirectory(t *testing.T) {
	f := newResumeFixture(t)
	argvFile := registerResumeAgent(t, "fakeagent")

	id, err := f.c.ResumeAgent(context.Background(), f.session, f.pane, "fakeagent")
	if err != nil {
		t.Fatalf("ResumeAgent: %v", err)
	}
	if !strings.HasPrefix(id, "@") {
		t.Fatalf("ResumeAgent returned %q, want a window id", id)
	}

	lines := strings.Split(strings.TrimRight(waitFor(t, argvFile), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("the fake agent recorded %q, want its directory and one argument", lines)
	}
	if lines[0] != resolved(t, f.dir) {
		t.Errorf("the resume ran in %q, want the pane's %q", lines[0], resolved(t, f.dir))
	}
	if lines[1] != "--resume" {
		t.Errorf("the resume ran with %q, want the table's --resume", lines[1])
	}
}

// The window is named after the agent, not after tmux's guess.
//
// Measured: with a shell-command argument and automatic-rename on, tmux names
// the window "tmux". A row reading "tmux" says nothing about what is in it.
func TestResumeAgentNamesTheWindowAfterTheAgent(t *testing.T) {
	f := newResumeFixture(t)
	registerResumeAgent(t, "fakeagent")

	id, err := f.c.ResumeAgent(context.Background(), f.session, f.pane, "fakeagent")
	if err != nil {
		t.Fatalf("ResumeAgent: %v", err)
	}
	if got := f.srv.Run(t, "display-message", "-p", "-t", id, "#{window_name}"); got != "fakeagent" {
		t.Errorf("the resume window is named %q, want %q", got, "fakeagent")
	}
}

// THE HEADLINE RULE: `agent` is a lookup key, never a command.
//
// Every string below is a command this machine would happily run, and none of
// them is a key in the table. If the argv ever came from the caller instead of
// from resumeCommands, the marker file would exist -- so this is an assertion
// about the shell, not about an error string.
func TestResumeAgentRefusesAnythingThatIsNotATableKey(t *testing.T) {
	f := newResumeFixture(t)
	marker := filepath.Join(t.TempDir(), "ran")
	before := f.windows(t)

	// NOTE the absence of a real agent's name in these strings, e.g.
	// "claude; touch <marker>". It is the obvious case to want here and it is
	// deliberately left out: the mutant this test exists to kill is "take the
	// argv from the caller", and running that mutant against such a string
	// would start Claude Code in whoever's account is running the suite. The
	// injection shape is what matters, and `true;` has it.
	for _, agent := range []string{
		"touch " + marker,
		"sh -c 'touch " + marker + "'",
		"true; touch " + marker,
		"CLAUDE",
		"claude-helper",
		"zsh",
		"",
	} {
		if _, err := f.c.ResumeAgent(context.Background(), f.session, f.pane, agent); err == nil {
			t.Errorf("ResumeAgent(%q) = nil, want a refusal", agent)
		}
	}

	// Nothing forked. tmux is asked directly, so a window that opened and then
	// exited would still have been counted by the marker.
	if after := f.windows(t); after != before {
		t.Errorf("a refused resume created a window anyway: %q -> %q", before, after)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatalf("a string from the caller was executed: %s exists", marker)
	}
}

// The directory is stat'd before new-window, so a project that has been removed
// is an error rather than a resume in $HOME.
//
// `new-window -c /gone` exits 0 and starts the shell somewhere else entirely --
// the same measurement checkDir exists for -- and for a resume that is the
// worst version of the bug: the agent starts, finds no history for wherever it
// landed, and offers an empty list.
func TestResumeAgentRefusesADirectoryThatIsGone(t *testing.T) {
	f := newResumeFixture(t)
	registerResumeAgent(t, "fakeagent")
	before := f.windows(t)
	if err := os.RemoveAll(f.dir); err != nil {
		t.Fatal(err)
	}

	if _, err := f.c.ResumeAgent(context.Background(), f.session, f.pane, "fakeagent"); err == nil {
		t.Fatal("ResumeAgent into a removed directory = nil, want an error")
	}
	if after := f.windows(t); after != before {
		t.Errorf("a resume into a removed directory created a window anyway: %q -> %q", before, after)
	}
}

// A resume has to know where "here" is, so the pane is not optional -- unlike
// NewWindow, where an empty fromPane means "let tmux decide".
func TestResumeAgentNeedsAPane(t *testing.T) {
	f := newResumeFixture(t)
	registerResumeAgent(t, "fakeagent")
	before := f.windows(t)

	for _, pane := range []string{"", "nonsense", "%99"} {
		if _, err := f.c.ResumeAgent(context.Background(), f.session, pane, "fakeagent"); err == nil {
			t.Errorf("ResumeAgent(fromPane=%q) = nil, want an error", pane)
		}
	}
	if after := f.windows(t); after != before {
		t.Errorf("a resume with no usable pane created a window anyway: %q -> %q", before, after)
	}
}

// The session id is validated the way every other verb validates it.
func TestResumeAgentRejectsAnUnusableSessionID(t *testing.T) {
	f := newResumeFixture(t)
	registerResumeAgent(t, "fakeagent")

	for _, id := range []string{"", "work", "$1;kill-server"} {
		if _, err := f.c.ResumeAgent(context.Background(), id, f.pane, "fakeagent"); err == nil {
			t.Errorf("ResumeAgent(session=%q) = nil, want an error", id)
		}
	}
}

// NewWindow stays a shell. The comment on it says so; this is the assertion,
// because the temptation the comment guards against is adding one parameter.
func TestNewWindowStillRunsNothing(t *testing.T) {
	f := newResumeFixture(t)
	id, err := f.c.NewWindow(context.Background(), f.session, "", f.pane)
	if err != nil {
		t.Fatalf("NewWindow: %v", err)
	}
	// The pane's command is the shell tmux started, and #{pane_start_command}
	// is empty for a window opened with no shell-command at all.
	if got := f.srv.Run(t, "display-message", "-p", "-t", id, "#{pane_start_command}"); got != "" {
		t.Errorf("NewWindow started %q; it opens a shell and runs nothing", got)
	}
}
