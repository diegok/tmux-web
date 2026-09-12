package report

import (
	"encoding/json"
	"testing"
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
