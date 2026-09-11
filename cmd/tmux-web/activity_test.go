package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/diegok/tmux-web/internal/tmux"
)

// The object of a tool call, reduced to what fits and says something.
//
// THE BUDGET IS 128 RUNES ON ONE SIDEBAR LINE, and that is what every row below
// is about. It is not about secrecy: anything that can read @tmux_web_agent can
// already capture-pane the same pane, and the browser side has one reader, the
// machine's owner. So the command goes through whole -- `rm -rf build` and
// `git push origin main` must not render alike -- an absolute path loses its
// directories because they spend the line on what the window name already says,
// and a tool whose argument shape nobody has looked at is its own name because
// a guess at which field matters is worse than the name.
func TestActivityFromToolCall(t *testing.T) {
	for _, tc := range []struct {
		name, tool, want string
		input            map[string]any
	}{
		{"a read becomes its basename", "Read", "read snapshot.go",
			map[string]any{"file_path": "/home/x/clients/acme/internal/tmux/snapshot.go"}},
		{"a bash command is carried whole", "Bash", "run go test ./internal/tmux -run TestX",
			map[string]any{"command": "go test ./internal/tmux -run TestX"}},
		// The row the first-word rule would have broken, and the reason it is
		// gone: two commands a person needs to tell apart, especially at a
		// permission prompt, where the command IS the question.
		{"a destructive command keeps its arguments", "Bash", "run rm -rf build",
			map[string]any{"command": "rm -rf build"}},
		{"and so does an irreversible one", "Bash", "run git push origin main",
			map[string]any{"command": "git push origin main"}},
		{"an unknown tool is its own name", "SomeMcpTool", "SomeMcpTool",
			map[string]any{"whatever": "..."}},
		{"a tool with no recognised field is its own name", "Read", "Read", map[string]any{}},

		// -- rows the plan's table did not have ------------------------------

		// The plan's mutant row asked what a leading `env FOO=1` should do.
		// With the command sent whole the question dissolves: there is no
		// shell parsing to get wrong, and no wrapper -- env, sudo, nice,
		// timeout, a leading "(" -- that needs a rule of its own.
		{"a wrapper and its assignments are part of the command", "Bash", "run env FOO=1 go test ./...",
			map[string]any{"command": "env FOO=1 go test ./..."}},
		{"and so is a program invoked by absolute path", "Bash", "run /home/user/bin/deploy --to prod",
			map[string]any{"command": "/home/user/bin/deploy --to prod"}},
		// The unknown-tool answer does not depend on the arguments being
		// unrecognisable: an MCP tool taking a `command` is still its own name,
		// because two gates have to open and only one of them did. A `command`
		// on a tool nobody has looked at might be a subcommand name, a config
		// block or a list, and the tool name is the readable true answer.
		{"an unknown tool with a command argument is still its own name", "mcp__shell__exec", "mcp__shell__exec",
			map[string]any{"command": "deploy --to prod"}},
		// The three path keys the three agents use, each on its own agent's
		// spelling. opencode's is camelCase and pi's is bare `path`; a reader
		// that knew only claude's would publish the tool name for two agents
		// out of three and nobody would notice, because that is a legal answer.
		{"opencode spells it filePath", "write", "write out.txt",
			map[string]any{"filePath": "/tmp/tmux-web-capture/oc/proj/out.txt"}},
		{"pi spells it path", "read", "read sample.txt",
			map[string]any{"path": "sample.txt"}},
		{"opencode's edit permission spells it filepath", "edit", "edit note.txt",
			map[string]any{"filepath": "/tmp/tmux-web-capture/oc/proj/note.txt"}},
		// A file's contents are unbounded, so they are never the object: a
		// write whose `content` became the label would blow MaxReportBytes and
		// the daemon would refuse the whole report, leaving the row blank.
		// Same reason as the edit permission's diff.
		{"a write names the file, not its contents", "write", "write out.txt",
			map[string]any{"path": "out.txt", "content": "a whole file, of no bounded length"}},
		// A directory-shaped or empty argument reduces to nothing, and nothing
		// falls back to the tool name rather than to "read ." or "read /".
		{"a path that reduces to nothing falls back to the tool name", "Read", "Read",
			map[string]any{"file_path": "/"}},
		{"an empty command falls back to the tool name", "Bash", "Bash",
			map[string]any{"command": "   "}},
		// Belt and braces with SanitizeActivity, which would trim this anyway
		// on the way out -- but reduceToolCall's own answer is what the rest of
		// this table reads, so it normalises here too.
		{"a command is trimmed of its surrounding whitespace", "Bash", "run go test",
			map[string]any{"command": "  go test  "}},
		// A bare leading assignment, for the same reason as the wrapper row:
		// it is part of the command and needs no rule of its own.
		{"a bare leading assignment is part of the command too", "Bash", "run FOO=1 go test ./...",
			map[string]any{"command": "FOO=1 go test ./..."}},
		// A non-string argument is not a path and not a command. Reading it as
		// one would be a type assertion nobody wrote a test for.
		{"a path key holding an object is not a path", "Read", "Read",
			map[string]any{"file_path": map[string]any{"nested": "/etc/passwd"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args, err := json.Marshal(tc.input)
			if err != nil {
				t.Fatal(err)
			}
			if got := reduceToolCall(tc.tool, args); got != tc.want {
				t.Errorf("reduceToolCall(%q, %s) = %q, want %q", tc.tool, args, got, tc.want)
			}
		})
	}
}

// Arguments this reducer was never handed at all: a missing `args` key, args
// that are not an object, args that are a JSON array. Each is an ordinary
// shape for some tool somewhere and none of them may become a label.
func TestActivityFromAnUnreadableArgument(t *testing.T) {
	for _, tc := range []struct{ name, tool, args, want string }{
		{"no args at all", "Bash", "", "Bash"},
		{"args that are a string", "Bash", `"rm -rf /"`, "Bash"},
		{"args that are an array", "Bash", `["rm","-rf","/"]`, "Bash"},
		{"args that are null", "Bash", `null`, "Bash"},
		{"a tool with no name", "", `{"command":"go test"}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := reduceToolCall(tc.tool, []byte(tc.args)); got != tc.want {
				t.Errorf("reduceToolCall(%q, %q) = %q, want %q", tc.tool, tc.args, got, tc.want)
			}
		})
	}
}

// The cap is the bound on a command, so the cap gets a test.
//
// Sending a command whole moves the length question onto SanitizeActivity and
// MaxActivity, which is where the reductions above stopped standing in for it.
// Driven end to end, because the cap is applied by FormatReport on the way out
// and not by the reducer: what has to be true is that a 4 KiB one-liner
// produces a value the DAEMON CAN STILL READ BACK, not merely one the reducer
// returned.
func TestALongCommandIsBoundedByTheCap(t *testing.T) {
	long := "go test " + strings.Repeat("./internal/verylongpackagename ", 200)
	payload, err := json.Marshal(map[string]any{
		"hook_event_name": "PreToolUse",
		"tool_name":       "Bash",
		"tool_input":      map[string]any{"command": long},
	})
	if err != nil {
		t.Fatal(err)
	}
	value, writes := reportValue(t, []string{"--agent", "claude", "--event", "PreToolUse"}, string(payload))
	if writes != 1 {
		t.Fatalf("wrote %d times, want 1", writes)
	}
	if len(value) > tmux.MaxReportBytes {
		t.Fatalf("the value is %d bytes, over the %d-byte cap: the daemon discards it whole and the "+
			"pane loses its state, not just its label", len(value), tmux.MaxReportBytes)
	}
	rep, ok := tmux.ParseReport(value, time.Now())
	if !ok {
		t.Fatalf("wrote %q, which the daemon cannot read back", value)
	}
	// EXACTLY the cap. Written against tmux.MaxActivity and never a literal
	// 128, and the fixture is built from neither -- 4 KiB of command is far
	// over any value that constant could take, so retargeting it moves the
	// assertion without moving the fixture.
	//
	// The plan's table expects this row to also kill "apply the cap before the
	// `run ` prefix is added". IT DOES NOT, and that was measured rather than
	// assumed: with the final cap still in place that mutant is unobservable,
	// because the assembled line is re-cut to the same 128 runes either way.
	// See firstLine. It is killed only by removing the final cap, which is the
	// mutant on the row above it.
	if n := len([]rune(rep.Activity)); n != tmux.MaxActivity {
		t.Errorf("activity is %d runes, want exactly %d -- the input is far longer, so anything "+
			"else means something other than the cap did the cutting", n, tmux.MaxActivity)
	}
	// And what survives is the front of the command, which is the part that
	// says what is happening.
	if !strings.HasPrefix(rep.Activity, "run go test ") {
		t.Errorf("activity = %q, want it to start with the command", rep.Activity)
	}
}

// A newline-bearing command is one value, not two fields.
//
// A heredoc or a multi-line script is an ordinary bash argument, and the
// separator this wire format uses is ";" while tmux's own substitution takes the
// newline. SanitizeActivity collapses the run to one space, which is what keeps
// a two-line command one readable line rather than two welded words.
func TestAMultiLineCommandCollapses(t *testing.T) {
	payload, err := json.Marshal(map[string]any{
		"tool_name":  "Bash",
		"tool_input": map[string]any{"command": "set -e\n\ngit add -A\ngit commit -m 'x'"},
	})
	if err != nil {
		t.Fatal(err)
	}
	rep, writes := reportOne(t, []string{"--agent", "claude", "--event", "PreToolUse"}, string(payload))
	if writes != 1 {
		t.Fatalf("wrote %d times, want 1", writes)
	}
	if want := "run set -e git add -A git commit -m 'x'"; rep.Activity != want {
		t.Errorf("activity = %q, want %q", rep.Activity, want)
	}
}

// The prompt is not the label.
//
// TWO REASONS, BOTH ABOUT THE ROW AND NEITHER ABOUT SECRECY. The first is that
// it answers the wrong question: this line exists to say what the agent is
// doing NOW, and twenty minutes into a task the prompt is history. That is
// exactly the owner's complaint about opencode's frozen title, and reproducing
// it here with better plumbing would be reproducing it.
//
// The second is MaxActivity. A prompt is prose and 128 runes cuts it mid-
// sentence, so what the row would carry is not the request but the first third
// of it -- which reads like an answer and is not one. A tool call and a todo
// entry are already short enough to survive the cut whole.
//
// Driven through the whole subcommand, over the real recorded payloads, and
// asserted on the VALUE THAT REACHES TMUX rather than on the parsed activity,
// because a parse is a second pass through SanitizeActivity and could hide a
// value that still carried the fragment.
func TestThePromptIsNotTheLabel(t *testing.T) {
	for _, tc := range []struct {
		fixture, agent, event string
		// forbidden is a distinctive run of the prompt in that fixture. Each
		// is copied from the file, not from the design.
		forbidden string
		// want is the activity the event is allowed to publish instead.
		want string
	}{
		{"claude/user_prompt_submit.json", "claude", "UserPromptSubmit", "haiku", ""},
		// The prompt handed to a subagent is the same kind of text and arrives
		// through a different door: a tool argument. The Agent tool has no
		// path and no command, so it is its own name.
		{"claude/pre_tool_use_agent.json", "claude", "PreToolUse", "two-line poem", "Agent"},
		{"opencode/chat_message.json", "opencode", "chat.message", "haiku", ""},
		{"opencode/tool_execute_before_task.json", "opencode", "tool.execute.before", "two-line poem", "task"},
		{"pi/input.json", "pi", "input", "haiku", ""},
		{"pi/tool_execution_start_subagent.json", "pi", "tool_execution_start", "two-line poem", "subagent"},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			payload := readFixture(t, tc.fixture)
			if !strings.Contains(payload, tc.forbidden) {
				t.Fatalf("%q is not in %s at all; this row proves nothing", tc.forbidden, tc.fixture)
			}
			value, writes := reportValue(t, []string{"--agent", tc.agent, "--event", tc.event}, payload)
			if writes != 1 {
				t.Fatalf("wrote %d times, want 1", writes)
			}
			if strings.Contains(value, tc.forbidden) {
				t.Fatalf("the value written to tmux was %q, and it carries the prompt (%q). "+
					"The line says what the agent is doing now, and a prompt cut at 128 runes "+
					"says neither that nor the request", value, tc.forbidden)
			}
			rep, ok := tmux.ParseReport(value, time.Now())
			if !ok {
				t.Fatalf("wrote %q, which is not a readable report", value)
			}
			if rep.Activity != tc.want {
				t.Errorf("activity = %q, want %q", rep.Activity, tc.want)
			}
		})
	}
}

// The subagent filters, tested for the DIRECTION they fail in rather than for a
// claim that they fail closed.
//
// A test asserting "no agent_id means silence" would be asserting the opposite
// of what ships -- and would silence all normal reporting. Claude documents
// agent_id as present only inside a subagent call and opencode's root is the
// session with no parentID: for both, the root is coded by an ABSENCE, and an
// absence-coded filter can be tightened but not inverted.
func TestSubagentFilters(t *testing.T) {
	t.Run("claude: a payload carrying agent_id is refused", func(t *testing.T) {
		// The real recorded subagent PreToolUse. Its table entry is identical
		// to the root's -- same event, same tool, same state -- so the table
		// cannot tell them apart and the filter is the only thing that can.
		_, writes := reportValue(t, []string{"--agent", "claude", "--event", "PreToolUse"},
			readFixture(t, "claude/pre_tool_use_subagent_bash.json"))
		if writes != 0 {
			t.Fatalf("wrote %d times; a subagent's event must not describe the root's pane", writes)
		}
	})

	t.Run("claude: a payload with no agent_id is the root and reports", func(t *testing.T) {
		// This is the fails-closed mutant's victim. Inverting the filter to
		// "report only when agent_id is present" silences the whole
		// integration, and every other test in this package that asserts a
		// state would still pass -- they would just all be testing nothing.
		for _, tc := range []struct{ fixture, event, want string }{
			{"claude/user_prompt_submit.json", "UserPromptSubmit", tmux.StateWorking},
			{"claude/pre_tool_use_bash.json", "PreToolUse", tmux.StateWorking},
			{"claude/notification_permission_prompt.json", "Notification", tmux.StateBlocked},
			{"claude/stop.json", "Stop", tmux.StateIdle},
			// claude/stop_subagent_running.json used to be here, reporting
			// idle, and it is deliberately NOT here now: that Stop reports
			// nothing, because background_tasks says a subagent is still
			// running under the root's turn. THE FILTER UNDER TEST STILL
			// ACCEPTS IT -- there is no agent_id anywhere in that payload and
			// TestTheFilterOverEveryRecordedPayload asserts it passes -- so
			// keeping it in this list would make this subtest pass or fail on
			// a decision that has nothing to do with the subagent filter. See
			// TestClaudeStopWithWorkStillRunningUnderIt.
		} {
			rep, writes := reportOne(t, []string{"--agent", "claude", "--event", tc.event},
				readFixture(t, tc.fixture))
			if writes != 1 || rep.State != tc.want {
				t.Errorf("%s wrote %q in %d command(s), want %q in exactly 1",
					tc.fixture, rep.State, writes, tc.want)
			}
		}
	})

	t.Run("claude: a payload that will not parse is refused", func(t *testing.T) {
		// The tightening an absence-coded filter CAN have: the payload must
		// have parsed and must be the shape a hook sends, so a wholesale
		// schema change is caught rather than read as a root event.
		//
		// Stop is the event to test it on, because before this filter existed
		// it wrote idle on any stdin at all, including none -- the reading "a
		// turn ended, whatever else is on stdin", which is what an
		// absence-coded filter cannot afford. Stop reads its payload for
		// background_tasks now as well, but this gate runs first and these
		// payloads never reach it.
		for _, stdin := range []string{
			"",
			"not json at all",
			`{"hook_event_name":"Stop"`, // truncated
			`["hook_event_name"]`,       // an array is not a hook payload
			`"Stop"`,
			`null`,
		} {
			_, writes := reportValue(t, []string{"--agent", "claude", "--event", "Stop"}, stdin)
			if writes != 0 {
				t.Errorf("stdin %q wrote %d times; a payload this subcommand could not read "+
					"is not evidence that this pane's root session did anything", stdin, writes)
			}
		}
	})

	t.Run("a subagent key that says nothing is not evidence of a subagent", func(t *testing.T) {
		// The other edge of the same filter, and the dangerous one: refusing a
		// root event does not fail closed, it stops the integration reporting
		// anything at all, silently, with every table test still green. A
		// serialiser that emits every field -- `"agent_id": ""`, or null --
		// would trip a filter that tested only for the key's presence.
		//
		// Found by mutation testing: `isPresentValue` reduced to `v != nil`
		// survived every other test in this package.
		for _, stdin := range []string{
			`{"hook_event_name":"Stop","agent_id":""}`,
			`{"hook_event_name":"Stop","agent_id":"   "}`,
			`{"hook_event_name":"Stop","agent_id":null}`,
		} {
			rep, writes := reportOne(t, []string{"--agent", "claude", "--event", "Stop"}, stdin)
			if writes != 1 || rep.State != tmux.StateIdle {
				t.Errorf("stdin %q wrote %q in %d command(s), want idle in exactly 1: the field is "+
					"present and saying nothing, which is not a subagent", stdin, rep.State, writes)
			}
		}
		// A value that says something is refused whatever its type. A non-string
		// agent_id is a schema change, and a schema change is the one thing an
		// absence-coded filter is allowed to be suspicious of.
		for _, stdin := range []string{
			`{"hook_event_name":"Stop","agent_id":17}`,
			`{"hook_event_name":"Stop","agent_id":{"id":"a18dabd7c4f03b2f2"}}`,
		} {
			_, writes := reportValue(t, []string{"--agent", "claude", "--event", "Stop"}, stdin)
			if writes != 0 {
				t.Errorf("stdin %q wrote %d times, want 0", stdin, writes)
			}
		}
	})

	t.Run("opencode: a payload carrying parentID is refused", func(t *testing.T) {
		// Go re-checks whatever it can see in any payload it is handed: a
		// parentID present in the JSON is refused whoever sent it. The plugin
		// keeps the child-session map -- it is the only thing that saw the
		// session.created naming the parent -- and this is the second gate.
		for _, tc := range []struct{ name, stdin string }{
			{"at the top level", `{"type":"session.idle","parentID":"ses_root","properties":{"sessionID":"ses_child"}}`},
			{"under properties", `{"type":"session.idle","properties":{"sessionID":"ses_child","parentID":"ses_root"}}`},
			{"nested, where session.created really carries it",
				`{"type":"session.idle","properties":{"sessionID":"ses_child","info":{"parentID":"ses_root"}}}`},
		} {
			_, writes := reportValue(t, []string{"--agent", "opencode", "--event", "session.idle"}, tc.stdin)
			if writes != 0 {
				t.Errorf("%s: wrote %d times, want 0", tc.name, writes)
			}
		}
	})

	t.Run("the filter is not a licence to claim the doors are shut", func(t *testing.T) {
		// The honest half, and it is here so that nobody reads the rows above
		// as "subagents are handled". opencode's session.idle carries NO
		// parentID -- Task 12 measured a child's arriving 2.05 s before the
		// root's, while the root was still busy -- and pi's agent_settled
		// payload is {"type":"agent_settled"} entire. Both still report from
		// here; what keeps them off the pane is the integration's own state
		// (opencode's child-session map, pi's ctx.mode !== "tui" gate), and a
		// Go-side test that pretended otherwise would be describing code that
		// does not exist.
		for _, tc := range []struct{ fixture, agent, event string }{
			{"opencode/session_idle_child.json", "opencode", "session.idle"},
			{"pi/agent_settled.json", "pi", "agent_settled"},
		} {
			rep, writes := reportOne(t, []string{"--agent", tc.agent, "--event", tc.event},
				readFixture(t, tc.fixture))
			if writes != 1 || rep.State != tmux.StateIdle {
				t.Errorf("%s wrote %q in %d command(s); this payload is INDISTINGUISHABLE from the "+
					"root's and Go reports it. If that changed, the integration's filter is what changed",
					tc.fixture, rep.State, writes)
			}
		}
	})
}

// Every recorded payload, through the filter, with the answer stated per file.
//
// The sweep matters more than any single row: the filter refuses on a key it
// finds ANYWHERE in the JSON, and a scan that eager can silence a legitimate
// root event without anybody noticing -- the integration simply stops
// reporting, and every table test still passes because the tables are fine. So
// the claim is two-sided: exactly these three files are refused, and exactly
// the other thirty-three are not.
func TestTheFilterOverEveryRecordedPayload(t *testing.T) {
	refused := map[string]bool{
		// agent_id, on a PreToolUse otherwise identical to the root's.
		"claude/pre_tool_use_subagent_bash.json": true,
		// agent_id again. Its hook is not registered either, so this one is
		// refused twice over -- structurally and by the filter.
		"claude/subagent_stop.json": true,
		// properties.info.parentID, nested two deep. The only opencode event
		// that names a parent at all.
		"opencode/session_created_child.json": true,
	}
	seen, denied := 0, 0
	for _, name := range allFixtures(t) {
		root, why := payloadIsRoot([]byte(readFixture(t, name)))
		seen++
		if refused[name] {
			denied++
			if root {
				t.Errorf("%s is a subagent's payload and the filter accepted it", name)
			} else if why == "" {
				t.Errorf("%s was refused with no reason given", name)
			}
			continue
		}
		if !root {
			t.Errorf("%s is a root session's payload and the filter refused it (%s). "+
				"An absence-coded filter that refuses a root event does not fail closed, "+
				"it stops the integration reporting anything at all", name, why)
		}
	}
	if seen == 0 || denied != len(refused) {
		t.Fatalf("swept %d fixtures and refused %d of an expected %d; this test proves nothing",
			seen, denied, len(refused))
	}
}

// Which mappings publish text, and from where, written out in full.
//
// The roll call the other tables in this package get, for the same reason:
// every entry is a decision about what one line of the sidebar says, and there
// is only one such line per pane. A mapping that quietly gained a text source
// would start overwriting a better answer with a worse one -- and "publish the
// prompt by default" is one table-row edit away in each of the three turn-start
// rows below, which is how the row would come to show history instead of work.
func TestTheTextSourcesAreExactlyThese(t *testing.T) {
	want := map[string]textSource{
		// The tool call, rung 3, on all three agents.
		"claude/PreToolUse":            textClaudeTool,
		"opencode/tool.execute.before": textOpencodeTool,
		"pi/tool_execution_start":      textPiTool,
		// Rung 1, the question when blocked. Claude's Notification carries a
		// message rather than a question; pi's ui_prompt_start.title is the
		// literal question; opencode's permission.asked carries neither and
		// has to be reduced from its metadata, per class.
		"claude/Notification(permission_prompt)": textClaudeNotification,
		"pi/ui_prompt_start":                     textPiPrompt,
		"opencode/permission.asked":              textOpencodePermission,
		// Rung 2, and only opencode has one.
		"opencode/todo.updated": textOpencodeTodo,
	}
	got := map[string]textSource{}
	for _, m := range allMappings() {
		if m.text != textNone {
			got[m.name] = m.text
		}
		// Rung 4 is "nothing -- an empty text field, not an absent report", and
		// a mapping that writes nothing has nothing to describe.
		if m.state == "" && m.text != textNone {
			t.Errorf("%s writes no state but carries a text source", m.name)
		}
	}
	for name, src := range want {
		if got[name] != src {
			t.Errorf("%s has text source %v, want %v", name, got[name], src)
		}
	}
	for name, src := range got {
		if _, ok := want[name]; !ok {
			t.Errorf("%s publishes text from source %v and this test did not know about it. "+
				"There is one activity line per pane and every event competes for it, so adding "+
				"a source decides what the row says at that moment -- make it here, next to the "+
				"reason, and check it still fits in MaxActivity", name, src)
		}
	}
	// The three turn starts are where the prompt lives, on every agent.
	for _, name := range []string{"claude/UserPromptSubmit", "opencode/chat.message", "pi/input"} {
		if _, ok := got[name]; ok {
			t.Errorf("%s publishes text, and the only text it has is the prompt -- which is "+
				"history by the second tool call and a sentence fragment at 128 runes", name)
		}
	}
}

// -- helpers ----------------------------------------------------------------

func readFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "hooks", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// allFixtures is every recorded payload, as "<agent>/<file>.json".
func allFixtures(t *testing.T) []string {
	t.Helper()
	var names []string
	for _, agent := range []string{"claude", "opencode", "pi"} {
		entries, err := os.ReadDir(filepath.Join("testdata", "hooks", agent))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".json") {
				names = append(names, agent+"/"+e.Name())
			}
		}
	}
	if len(names) == 0 {
		t.Fatal("no fixtures at all")
	}
	return names
}
