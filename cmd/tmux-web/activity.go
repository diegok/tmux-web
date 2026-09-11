package main

import (
	"encoding/json"
	"path"
	"strings"
)

// This file is the activity line and the subagent filters: what one event is
// allowed to SAY about a pane, as against events.go, which is what it is
// allowed to CLAIM about it.
//
// WHAT DECIDES THE REDUCTIONS HERE IS THE 128-RUNE BUDGET AND WHAT IS USEFUL IN
// IT -- NOT SECRECY. An earlier draft of this file justified every rule below as
// a privacy boundary, and that reason does not hold: anything that can read
// @tmux_web_agent off the tmux server can already run capture-pane on the same
// pane and read the whole screen, and the browser side is HTTPS with device
// enrolment where the only reader is the machine's owner. There is no second
// actor to keep anything from. A comment giving a reason that does not hold is
// worse than no comment, because the next reader inherits the wrong constraint
// and designs around it -- so the reasons here are the real ones, and where a
// real one runs out the text is sent whole.
//
// The budget is MaxActivity, 128 runes, on PaneLines' second line in a sidebar
// about 16rem wide. Everything that arrives goes through SanitizeActivity and
// that cap, which is what they are for. The rules:
//
//   - a SHELL COMMAND IS SENT WHOLE. `rm -rf build` and `git push origin main`
//     must not both render as a bare verb, and at a permission prompt the
//     command IS the question the row exists to answer.
//   - a PATH argument is reduced to its BASENAME, on display grounds: an
//     absolute path eats most of 128 runes and wraps badly at 16rem, and the
//     window and session names above the row already say which repo this is.
//     It is a default, not a rule -- where a short relative path says more than
//     a bare name, send it.
//   - anything else is the TOOL NAME alone, because for a tool whose argument
//     shape nobody has looked at there is no field we know to be the useful
//     one, and an arbitrary serialised argument object is unbounded and
//     unreadable.
//
// The one thing that is genuinely excluded is opencode's edit-permission `diff`,
// and the reason is size: it is a whole unified diff, unbounded, and over the
// 1 KiB MaxReportBytes cap on its own in the single sample recorded.

// pathKeys is every key, across all three agents, whose value is a filesystem
// path this reducer will publish the basename of.
//
// One key per agent and they are all spelled differently -- claude's file_path,
// opencode's camelCase filePath, pi's bare path -- plus the lowercase filepath
// that opencode's edit PERMISSION metadata uses, which is a fourth spelling on
// an agent that already had one. They are matched exactly and not
// case-insensitively: encoding/json's own field matching is case-insensitive,
// which would make filePath and filepath the same key, and a whitelist that
// cannot tell two keys apart is not a whitelist.
var pathKeys = []string{"file_path", "filePath", "filepath", "path"}

// commandKeys is every key whose value is a shell command line.
//
// All three agents agree on this one, and so does opencode's bash permission
// metadata. Its value is published whole; see firstLine.
var commandKeys = []string{"command"}

// toolVerbs maps a tool name, case-folded, to the verb the activity line uses.
//
// It is small on purpose, and it is small because of what it is FOR: a verb is
// only ever used next to an object, and a tool with no path and no command has
// no object, so an entry for it would never be reached. These four are every
// tool across the three agents that takes one. A tool that is not here is its
// own name, which is the third rule above.
//
// Case-folded because the same tool is spelled three ways: claude's Bash,
// opencode's bash, pi's bash. The fold is on the LOOKUP only; the name that
// reaches the wire when there is no object is the one the agent sent, so an
// MCP tool keeps its own capitalisation.
var toolVerbs = map[string]string{
	"bash":  "run",
	"read":  "read",
	"write": "write",
	"edit":  "edit",
}

// reduceToolCall is the verb-plus-object rule, for a tool call on any agent.
//
// Two gates have to open for an object to be published: the tool has to be one
// of the four this file knows a verb for, AND its arguments have to contain a
// key this file knows the kind of. A tool that fails either gate is its own
// name -- so an MCP tool taking a `command` gets the same answer as one taking
// nothing. That is deliberate and it is a usefulness argument rather than a
// cautious one: the tool table is what says "this key on THIS tool is a shell
// command", and a `command` on a tool nobody has looked at might be a subcommand
// name, a config block or a list. The tool name is short, true and readable;
// a guess at which field matters is none of those.
func reduceToolCall(tool string, rawArgs []byte) string {
	tool = strings.TrimSpace(tool)
	if tool == "" {
		return ""
	}
	verb, known := toolVerbs[strings.ToLower(tool)]
	if !known {
		return tool
	}
	// map[string]any rather than a struct with the four path tags on it,
	// precisely because encoding/json matches field tags case-insensitively
	// and two of those four differ only in case. Explicit lookups also mean
	// the unbounded fields -- `content`, `diff`, `todos` -- are never asked
	// for, which is what keeps a report inside MaxReportBytes.
	var args map[string]any
	if err := json.Unmarshal(rawArgs, &args); err != nil || args == nil {
		return tool
	}
	obj := basename(stringArg(args, pathKeys))
	if obj == "" {
		obj = firstLine(stringArg(args, commandKeys))
	}
	if obj == "" {
		return tool
	}
	return verb + " " + obj
}

// stringArg is the first of these keys whose value is a non-empty string.
//
// The type assertion is the point. A path key holding an object, a number or
// null is not a path, and a reducer that stringified whatever it found would
// spend the row's 128 runes on `map[nested:/etc/passwd]`, which tells the
// reader less than the tool name would.
func stringArg(args map[string]any, keys []string) string {
	for _, k := range keys {
		if s, ok := args[k].(string); ok && strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// basename reduces a path to the part a person reads.
//
// A DISPLAY DEFAULT, not a rule about what may leave the machine. An absolute
// path spends most of a 128-rune line on directories the row already implies --
// the window name and the session name above it say which repo this is -- and
// wraps badly at 16rem. Where a short relative path would say more than a bare
// name, send that instead; nothing here forbids it.
//
// path.Base and not filepath.Base: the two are the same function on this
// platform, and path.Base is the one whose behaviour does not depend on which
// platform that is. The three answers that are not a name -- ".", "/" and ".."
// -- reduce to nothing, so a tool called on a directory falls back to its own
// name rather than publishing "read .".
func basename(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	switch b := path.Base(v); b {
	case ".", "/", "..":
		return ""
	default:
		return b
	}
}

// firstLine is a shell command, whole.
//
// THE COMMAND IS THE ONE ARGUMENT THAT IS NOT REDUCED, and it is the rule this
// file most recently got wrong. Reducing it to its first word makes
// `rm -rf build` and `git push origin main` render identically, which throws
// away the only thing that distinguishes them; and at a permission prompt --
// opencode's permission.asked, claude's permission dialog -- THE COMMAND IS THE
// QUESTION. A row that says "blocked: run" has erased what it exists to answer.
//
// The bound on it is SanitizeActivity and MaxActivity, applied by FormatReport
// on the way out, which is exactly what those are for: a heredoc collapses to
// spaces, controls and escape sequences go, and 128 runes is the cut. A long
// command arrives truncated, which is a legible partial answer; a first word is
// a complete non-answer.
//
// THE CAP IS NOT APPLIED HERE. One cap, at the end, over the assembled
// "run <command>". A second one inside this function would be UNOBSERVABLE --
// mutation-tested: cutting the command to MaxActivity first and prefixing the
// verb after produces the same 128 runes once SanitizeActivity has run, because
// the final cap catches the overrun either way. So this is not a correctness
// argument and nothing here should be read as one; it is that a second
// truncation point is a second thing to keep in step with SanitizeActivity's
// rune-boundary handling, for no gain.
//
// The trim is the whole of what happens here. It is a named function rather
// than an inline TrimSpace so that the reasoning above has somewhere to live.
func firstLine(cmd string) string { return strings.TrimSpace(cmd) }

// activityText is the text a mapping publishes, read out of that agent's own
// payload shape.
//
// Every branch is a per-agent reader and every one of them names the keys it
// wants, so the default for a payload shape nobody anticipated is no text --
// which is rung 4 of the ladder, a complete report, and not a degraded one.
//
// A note on the ladder, because the design's four rungs read like a precedence
// and here they are not. A precedence needs all four candidates in front of it
// at once, and this writer is a fresh process handling one event: opencode's
// todo.updated (rung 2) and its tool.execute.before (rung 3) are separate
// events and whichever fires later wins. That is the right answer anyway -- the
// later event is the more current description -- but it is an ordering, not a
// precedence, and nothing here should be read as promising otherwise.
func activityText(src textSource, payload []byte) string {
	switch src {
	case textClaudeTool:
		var p struct {
			ToolName  string          `json:"tool_name"`
			ToolInput json.RawMessage `json:"tool_input"`
		}
		if json.Unmarshal(payload, &p) != nil {
			return ""
		}
		return reduceToolCall(p.ToolName, p.ToolInput)

	case textClaudeNotification:
		// The one rung-1 field claude has. It is NOT a question -- one
		// permission_prompt read "Claude needs your permission" and another
		// "Claude Code needs your approval for the plan" -- which is why
		// nothing keys off it for the STATE. As TEXT it is the most the
		// payload has: claude's Notification carries no command, no path and
		// no dialog contents, so the message is the whole of what this event
		// knows. It is short, so the cap never bites.
		var p struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(payload, &p) != nil {
			return ""
		}
		return p.Message

	case textOpencodeTool:
		// tool.execute.before takes two arguments, so the payload is
		// {input, output} -- and the tool's arguments are under output.args,
		// not under input.
		var p struct {
			Input struct {
				Tool string `json:"tool"`
			} `json:"input"`
			Output struct {
				Args json.RawMessage `json:"args"`
			} `json:"output"`
		}
		if json.Unmarshal(payload, &p) != nil {
			return ""
		}
		return reduceToolCall(p.Input.Tool, p.Output.Args)

	case textOpencodeTodo:
		// Rung 2, and only opencode has one: the agent's own statement of
		// intent, which beats a mechanical trace of the tool it is currently
		// inside. "Fix the parser bug" beats "read blocked.go".
		//
		// How often the list is empty is open question 6 and it does not
		// change anything here: an empty list yields no text, and the tool-call
		// rung underneath arrives on its own events either way.
		var p struct {
			Properties struct {
				Todos []struct {
					Content string `json:"content"`
					Status  string `json:"status"`
				} `json:"todos"`
			} `json:"properties"`
		}
		if json.Unmarshal(payload, &p) != nil {
			return ""
		}
		for _, todo := range p.Properties.Todos {
			if todo.Status == "in_progress" {
				return todo.Content
			}
		}
		return ""

	case textOpencodePermission:
		// THE DESIGN IS WRONG ABOUT THIS EVENT and the recorded payload is
		// what settles it: permission.asked carries no question string and no
		// title. It carries the permission CLASS, `patterns`, `always`, `tool`
		// and a `metadata` object WHOSE KEYS DEPEND ON THE CLASS -- {command}
		// for bash, {filepath, diff} for edit. So this reduces per class rather
		// than reading one fixed field, and the class plays the part the tool
		// name plays everywhere else in this file.
		//
		// THE REASON FOR REDUCING THE EDIT CLASS IS SIZE, and it is the one
		// genuine exclusion in this file. `diff` is a whole unified diff --
		// unbounded, and over MaxReportBytes on its own in the single sample
		// recorded -- so a reader that took "the biggest string in metadata"
		// or "metadata, serialised" would write a value the daemon then
		// refuses to parse at all, and the row would show nothing. The
		// filepath's basename is bounded and answers the question the dialog
		// is asking: which file. The bash class needs no such treatment and
		// gets none -- its command goes through whole, because at a permission
		// prompt the command IS the question.
		var p struct {
			Properties struct {
				Permission string          `json:"permission"`
				Metadata   json.RawMessage `json:"metadata"`
			} `json:"properties"`
		}
		if json.Unmarshal(payload, &p) != nil {
			return ""
		}
		return reduceToolCall(p.Properties.Permission, p.Properties.Metadata)

	case textPiTool:
		// Open question 7, answered by the capture: args is the tool's own
		// argument object, unreduced, with a different key per tool --
		// {command} for bash, {path} for read, {path, content} for write,
		// {agent, async, task} for a subagent. `content` is a whole file and
		// is not read, for the size reason; `task` is not read because
		// `subagent` is not in toolVerbs, which is the ordinary
		// unknown-argument-shape answer.
		var p struct {
			ToolName string          `json:"toolName"`
			Args     json.RawMessage `json:"args"`
		}
		if json.Unmarshal(payload, &p) != nil {
			return ""
		}
		return reduceToolCall(p.ToolName, p.Args)

	case textPiPrompt:
		// Rung 1 at its best: pi's title IS the literal question, sent whole.
		// One captured kind ("custom", an extension's own overlay) has no
		// title key at all, which is not an error -- a blocked report with no
		// text is complete, and the row falls back to the pane title and then
		// to the command.
		var p struct {
			Title string `json:"title"`
		}
		if json.Unmarshal(payload, &p) != nil {
			return ""
		}
		return p.Title
	}
	return ""
}

// subagentKeys are the payload keys whose presence says "this event is not
// about the pane's root session".
//
// Spelled exactly as the two agents spell them. claude's agent_id is documented
// as present only inside a subagent call and was absent on every root payload
// and present on every subagent one in the capture; opencode's parentID appears
// on session.created's nested info and names the parent session.
//
// A NOTE ON WHAT THIS BUYS, because "every one of these filters fails closed"
// is not implementable and no comment here may claim it. For both agents the
// ROOT is coded by an ABSENCE: a root claude payload legitimately has no
// agent_id, and opencode's root is the session with no parentID. An
// absence-coded filter cannot be inverted -- "missing means silence" silences
// all normal reporting -- so it fails OPEN: a payload that lost the field to a
// rename, a nesting change or a refactor reads as a root. It can only be
// TIGHTENED, which payloadIsRoot does by requiring that the payload parsed and
// is the shape a hook sends.
var subagentKeys = []string{"agent_id", "parentID"}

// payloadIsRoot is the Go-side half of the subagent filters.
//
// WHAT LIVES WHERE: a filter needing runtime state or a runtime object stays in
// the integration -- pi's ctx.mode and ctx.isIdle(), opencode's child-session
// map, which needs memory across events that a fresh process cannot have.
// Claude's agent_id check lives HERE, in Go, because the payload arrives on
// stdin and each claude hook is a fresh process anyway. Go additionally
// re-checks whatever it can see in any payload it is handed, whichever agent
// sent it: two gates, both cheap.
//
// TWO DOORS THIS DOES NOT CLOSE, and no code here may pretend otherwise:
//
//   - A NESTED claude CLI IN THE SAME PANE. Certain. A claude spawned from a
//     Bash tool call inherits TMUX_PANE, loads the same project settings, and
//     is the root of its own session -- so it fires its own perfectly
//     legitimate Stop and writes idle onto a pane whose outer agent is still
//     working. No field test can catch it: both processes are roots, agent_id
//     is absent for both, and they share nothing to compare. Open question 11.
//   - AN ASYNC pi SUBAGENT. It is a separate OS process that inherits
//     TMUX_PANE=%0, loads the same extension, and emits an agent_settled that
//     is BYTE-IDENTICAL to the root's -- 7.0 s before the root's real one --
//     and ctx.isIdle() is true in it too. Nothing in the payload separates
//     them. What does is pi's ctx.mode !== "tui" gate, in the extension, and it
//     is load-bearing.
//
// Both are why this returns a reason: a refusal that cannot say why is a
// refusal nobody can tell from a bug.
func payloadIsRoot(payload []byte) (root bool, why string) {
	// The parse requirement is the tightening, and it is the only one an
	// absence-coded filter is allowed. A payload nobody could read is a
	// payload in which agent_id is absent FOR THE WORST POSSIBLE REASON, and
	// reading that as a root event is reading a parse failure as evidence. A
	// JSON object is what every hook on all three agents sends; a scalar, an
	// array, a truncated object and an empty stdin are not one.
	var top map[string]any
	if err := json.Unmarshal(payload, &top); err != nil || top == nil {
		return false, "it is not the JSON object every hook sends"
	}
	if key := findSubagentKey(top, 0); key != "" {
		return false, "it carries " + key + ", which only a subagent's payload does"
	}
	return true, ""
}

// maxScanDepth bounds the walk below.
//
// encoding/json has already refused anything nested deeper than its own limit
// by the time we get here, so this is not what stops a hostile payload; it is
// what stops the recursion being unbounded in its own right. Anything deeper
// than this is not a hook payload, and the direction of the answer matters:
// running out of depth means the scan found nothing, so the payload is treated
// as a root. That is the fail-open direction the whole filter has, stated where
// it happens rather than left to be inferred.
const maxScanDepth = 64

// findSubagentKey is the name of the first subagent key anywhere in the
// payload, or "".
//
// ANYWHERE, and not only at the top level, because opencode carries parentID
// two levels down under properties.info. A scan this eager has its own failure
// mode and it is the dangerous direction: refusing a legitimate root event
// stops the integration reporting anything at all, silently, with every table
// test still passing. TestTheFilterOverEveryRecordedPayload is what holds that
// down -- it asserts both halves over all thirty-six recorded payloads.
//
// Only a non-empty scalar counts. An agent_id of "" or null is the field
// present and saying nothing, which is not evidence of a subagent, and
// refusing on it would turn a serialiser's habit into silence.
func findSubagentKey(v any, depth int) string {
	if depth > maxScanDepth {
		return ""
	}
	switch t := v.(type) {
	case map[string]any:
		for _, key := range subagentKeys {
			if inner, ok := t[key]; ok && isPresentValue(inner) {
				return key
			}
		}
		for _, inner := range t {
			if key := findSubagentKey(inner, depth+1); key != "" {
				return key
			}
		}
	case []any:
		for _, inner := range t {
			if key := findSubagentKey(inner, depth+1); key != "" {
				return key
			}
		}
	}
	return ""
}

// isPresentValue reports whether a JSON value says anything at all.
func isPresentValue(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(t) != ""
	default:
		return true
	}
}
