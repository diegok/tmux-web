// Package integrations holds the files `wterm-web install-integration` writes
// into a user's agent configuration, and the little code that has to generate
// rather than copy.
//
// It is deliberately thin, and the shape of it is the same for all three
// agents: the integration transports an event name and the hook's own payload
// to `wterm-web report`, and every judgement about what that event MEANS lives
// in one Go table (cmd/wterm-web/events.go) instead of being spread across a
// TypeScript extension, a JavaScript plugin and a settings.json the user owns.
// Three copies of a judgement are three copies that drift.
//
// This file is also the package's //go:embed host, and that is not an aside.
// //go:embed reads only from the directory of the file that declares it and its
// subtree: cmd/wterm-web CANNOT embed ../../internal/integrations, and a path
// with ".." in it is a compile error rather than a lookup that fails at run
// time. So the bytes the installer writes can only be reached from a .go file
// sitting here, next to them, and this is it.
package integrations

import (
	"embed"
	"encoding/json"
	"strings"
)

// files is everything the installer writes.
//
// queue.ts is in the list and is easy to drop by accident: it is not an
// integration, it is the single-slot spawn queue that BOTH pi.ts and
// opencode.js import, so writing the two integrations without it writes two
// files that throw on load. Its body is plain JavaScript with JSDoc types and a
// `// @ts-nocheck` line for that reason -- the installer ships it into a `.js`
// context -- and nothing here cares, because nothing here transforms it.
//
//go:embed pi.ts opencode.js queue.ts claude-report.sh
var files embed.FS

// Files is the embedded set, sorted, so a test can hold the //go:embed
// directive above against something. A name dropped from that directive is not
// a compile error: it is a ReadFile returning fs.ErrNotExist at install time,
// on somebody else's machine.
var Files = []string{"claude-report.sh", "opencode.js", "pi.ts", "queue.ts"}

// File returns one embedded file's bytes.
func File(name string) ([]byte, error) { return files.ReadFile(name) }

// ClaudeBinPlaceholder is the token claude-report.sh ships with in place of the
// wterm-web path, which is not known until install time.
//
// It sits INSIDE the quotes of the BIN assignment rather than in place of the
// whole line, so that the file as shipped is still a valid shell script and
// still fails the way the installed one does: the placeholder is not an
// executable path, so the `[ -x "$BIN" ]` guard catches it and the script exits
// 0 having said nothing.
const ClaudeBinPlaceholder = "__WTERM_BIN__"

// ClaudeReportScript is claude-report.sh with BIN resolved to a real path.
//
// The path is shell-quoted because the script is a shell script and an install
// prefix containing a space is ordinary on macOS. Everything else in the file
// is left exactly as it is: it is the source of truth, not a template.
func ClaudeReportScript(bin string) []byte {
	b, err := files.ReadFile("claude-report.sh")
	if err != nil {
		// Unreachable: the //go:embed directive above is checked at compile
		// time against this same name.
		panic(err)
	}
	return []byte(strings.Replace(string(b), "'"+ClaudeBinPlaceholder+"'", shellQuote(bin), 1))
}

// ClaudeHookEvents is the whole hook set, in the order a turn fires them.
//
// IT IS SMALL AND IT IS CLOSED, and the closure is the interesting half:
//
//   - SubagentStop is absent, and its absence is the ENTIRE structural guard
//     against the Task-tool subagent class. A subagent's finish arriving as a
//     Stop would write idle onto a pane whose root agent is still working --
//     the false `done` badge this design spends most of its complexity
//     avoiding. Claude's own subagent-launching tool is `Agent`, not `Task`,
//     and its payloads carry `agent_id`; but agent_id is ABSENCE-coded, so the
//     payload filter fails OPEN and not registering the hook is the only part
//     of this that fails closed. cmd/wterm-web/events.go leaves SubagentStop
//     out of its table as well, so even a hand-edited settings.json that
//     registered it writes nothing.
//   - PreToolUse is the hot one, and it is a TRADE rather than an obvious win.
//     It is the keepalive and the activity line, and it is also what makes the
//     connected-case blocked badge about six seconds slow instead of about
//     1.5, because its fresh `working` suppresses the capture IsBlocked would
//     have read. The disconnected case is the product, so it ships. The
//     retreat, if the fork volume ever justifies one, is deleting this one
//     entry: Claude then reports state only and nothing else changes.
//   - Notification is not a state on its own. Its notification_type is a
//     whitelist in events.go, an unknown one writes nothing, and this package
//     has no opinion about any of it.
var ClaudeHookEvents = []string{"UserPromptSubmit", "PreToolUse", "Notification", "Stop"}

// claudeHook is one hook entry.
//
// Async is not a pointer and is never conditional: `"async": true` is what
// keeps the hook off the turn's critical path (measured on Claude Code 2.1.267:
// a 2771 ms turn became 7735 ms with a `sleep 5` sync hook and 2251 ms with the
// same hook async), and a sync hook adds its whole duration to the turn.
//
// THERE IS NO AsyncRewake FIELD AND THERE NEVER WILL BE. An async command
// hook's exit code is ignored, including the exit 2 that blocks a PreToolUse
// tool call; asyncRewake is documented as the thing that puts that handling
// back. It is belt to the exit-0 rule's braces -- two independent things would
// have to change before an exit code could hurt anybody, and the second of them
// is a line in a file the user owns -- and adding this field would take the
// belt off.
type claudeHook struct {
	Type    string `json:"type"`
	Async   bool   `json:"async"`
	Command string `json:"command"`
}

// claudeMatcher is Claude's matcher-plus-hooks wrapper. No `matcher` key: an
// absent one means every tool, which is what PreToolUse wants, and the other
// three hooks take no matcher at all.
type claudeMatcher struct {
	Hooks []claudeHook `json:"hooks"`
}

// ClaudeHookBlock is the settings fragment the installer merges into the user's
// settings.json, given the resolved path of the installed claude-report.sh.
//
// It is generated rather than embedded because one thing in it -- that path --
// is not knowable until install time, and a settings.json with a token in it is
// a settings.json that silently does nothing.
func ClaudeHookBlock(script string) []byte {
	hooks := make(map[string][]claudeMatcher, len(ClaudeHookEvents))
	for _, event := range ClaudeHookEvents {
		hooks[event] = []claudeMatcher{{Hooks: []claudeHook{{
			Type:  "command",
			Async: true,
			// The event name reaches the wrapper as "$1" and `report` as
			// --event. The script path is quoted because Claude runs this
			// through a shell.
			Command: shellQuote(script) + " " + event,
		}}}}
	}
	b, err := json.MarshalIndent(map[string]map[string][]claudeMatcher{"hooks": hooks}, "", "  ")
	if err != nil {
		// Unreachable: every value in here is a string, a bool or a slice of
		// them.
		panic(err)
	}
	return b
}

// shellQuote wraps s so a POSIX shell sees exactly one word.
//
// Single quotes, because inside them a shell interprets nothing at all; the one
// character that cannot appear there is the single quote itself, which is
// closed, escaped and reopened.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
