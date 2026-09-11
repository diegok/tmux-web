package main

import (
	"encoding/json"

	"github.com/diegok/tmux-web/internal/tmux"
)

// This file is the whole of what an agent's event means, for all three agents.
//
// It lives here, in Go, rather than in the three integrations, because it is
// the one part of this feature that is neither transport nor plumbing: it is a
// judgement about what a hook firing tells you about a person's pane, and a
// judgement spread across a TypeScript extension, a JavaScript plugin and a
// settings.json the user owns is a judgement that drifts. One table, one test.
//
// Two costs of Claude's half of it, stated here rather than discovered:
//
//   - Claude's event-sourced blocked is about SIX SECONDS LATE by construction,
//     because permission_prompt waits about six seconds before it fires. That
//     is four polls.
//   - Installing the integration makes Claude's blocked detection SLOWER in the
//     connected case. A non-integrated pane is captured every poll and
//     IsBlocked matches the permission dialog in about 1.5 s; an integrated one
//     takes about six, because PreToolUse's fresh working report suppressed the
//     capture that would have found it. The trade is accepted -- the
//     integration's value is the DISCONNECTED case, where it is the only source
//     of anything at all -- but it is a trade, and its lever is dropping
//     PreToolUse, which restores the 1.5-second connected badge and costs the
//     activity line. That is what makes open question 1 a design question and
//     not only a fork count.

// eventKind is whether a mapping's write is an edge or a repair.
//
// THE CRITERION, because this is a safety property and "these are the ones I
// thought of" is not one:
//
//	A write is a RE-ASSERTION if the state it would write is a resting state
//	and the event that produces it can fire more than once within one resting
//	period. Everything else is an EDGE. An edge writes unconditionally; a
//	re-assertion reads the standing option first and writes only if the
//	standing report's state differs from the one it would write.
//
// It is per (event, state) pair and not per event: an event that writes working
// on one branch and a resting state on another is an edge on the first and a
// re-assertion on the second. pi's session_start is exactly that and is the
// reason the criterion cannot be written per event.
//
// Why it exists at all. Stop at T writes 1;idle;T, a device glances, its `seen`
// marker stores T, the badge clears. Sixty seconds later idle_prompt fires --
// it does that after EVERY turn the user does not type into -- and would write
// 1;idle;T+60. It is strictly newer, so the daemon's ordering filter takes it;
// the screen has been settled for a minute, so evidence rule 3 has nothing to
// object to; finishedAt is derived statelessly from the report's own timestamp,
// so it becomes T+60. Every device that saw the finish re-badges, one minute
// after every turn, on a pane nothing happened to.
//
// What this costs, stated rather than discovered: A STALE OR BUGGY INTEGRATION
// STILL STORMS. The daemon cannot tell a re-assertion from a genuine finish --
// both are 1;idle;<ts> -- so this is a promise the writer keeps and the reader
// cannot check. It is the one place in this design where a reader-side
// invariant rests on writer-side behaviour.
type eventKind int

const (
	// kindUnclassified is the zero value and means nobody decided. It is not
	// an alias for edge, and that is deliberate: revision 3 of the design
	// carried pi's session_start without classifying it, and a zero value that
	// meant "edge" would have turned that omission into a badge storm silently.
	// reassertsFor gives it the design's stated default instead -- a resting
	// state whose cardinality is unknown is a re-assertion -- and
	// TestEveryMappingIsClassified stops the table leaning on it.
	kindUnclassified eventKind = iota
	// kindEdge writes unconditionally, without reading the standing option.
	// For the three turn-end events that is safe only because of the turn-start
	// invariant: every integration writes working at turn start, so a turn end
	// always has a non-resting state in front of it, and the end cannot fire
	// twice inside one resting period. A turn that reached its end with no
	// working before it would have that end suppressed and lose its badge.
	kindEdge
	// kindReassertion writes only when the standing report disagrees.
	kindReassertion
)

// reassertsFor reports whether this mapping's write must read the standing
// report first.
//
// The default for an unclassified mapping is the design's: an event whose
// cardinality within a resting period is unknown, and which writes a resting
// state, is treated as a re-assertion. The lists are not claimed to be
// exhaustive -- the event inventories are per-agent documentation we do not
// control -- so they ship with a default rather than a guarantee, and the
// asymmetry that settles which default is the same one that settles the
// unknown-notification_type case: an unnecessary read costs one fork on a path
// nobody is waiting on, and a missing one costs a badge storm.
//
// working is not resting, so an unclassified working mapping stays an edge. It
// is transient and cannot badge, and the worst a redundant one does is refresh
// the 60-second expiry -- which is what the keepalive wants anyway.
func reassertsFor(m mapping) bool {
	switch m.kind {
	case kindEdge:
		return false
	case kindReassertion:
		return true
	}
	return m.state == tmux.StateIdle || m.state == tmux.StateBlocked
}

// textSource is where a mapping's activity text comes from.
//
// It is declared here, next to the states, for the same reason eventKind is:
// what an event MEANS and what it is allowed to SAY are one decision per row,
// and separating them into two tables is how a row ends up accounted for by one
// and not the other. activity.go holds the readers themselves -- the basename
// default, the command sent whole, the tool name alone, and the per-class
// reduction that keeps opencode's unbounded edit diff inside MaxReportBytes.
//
// THE LADDER, and which rung each source is. The label is the first of these
// that is available, per agent:
//
//  1. The question, when blocked -- pi's ui_prompt_start.title, claude's
//     Notification message, and for opencode a reduction of permission.asked,
//     which carries no question at all.
//  2. The current step, when opencode reports one: todo.updated's in_progress
//     entry. Better than a tool call when it exists, because it is the agent's
//     own statement of intent rather than a mechanical trace.
//  3. The current tool call, verb plus object -- the command whole, a path
//     reduced to its basename so the directories do not eat the line.
//  4. Nothing -- AN EMPTY TEXT FIELD, NOT AN ABSENT REPORT. At turn end the
//     integration reports idle with no text; the row falls back to the title
//     and then to the command, and the STATE is still the agent's own.
//
// Rung 4 is why textNone is the zero value and why most rows carry it: a
// three-part report is the ordinary claude case and the reader accepts three
// parts or four.
type textSource int

const (
	// textNone is the zero value: rung 4, a state-only report.
	textNone textSource = iota
	// textClaudeTool reads tool_name and tool_input.
	textClaudeTool
	// textClaudeNotification reads `message`. It is the only rung-1 field
	// claude has, and it is not a question; see activityText.
	textClaudeNotification
	// textOpencodeTool reads input.tool and output.args -- the tool's
	// arguments are under the SECOND of the hook's two arguments.
	textOpencodeTool
	// textOpencodeTodo reads the in_progress entry of properties.todos.
	textOpencodeTodo
	// textOpencodePermission reduces properties.metadata PER PERMISSION CLASS,
	// because its keys vary by class and one of them is a full unified diff.
	textOpencodePermission
	// textPiTool reads toolName and args.
	textPiTool
	// textPiPrompt reads `title`, which pi is the only agent to send and which
	// one captured kind omits entirely.
	textPiPrompt
)

// mapping is what one (agent, event) means. The zero state is "ignore": no
// write, no state change, no timestamp refresh, and not an error.
type mapping struct {
	name  string
	state string
	// kind is edge or reassertion, and every mapping that writes a state names
	// one: the zero value is "nobody decided", not "edge". See eventKind for
	// the criterion and reassertsFor for what an undecided one falls back to.
	kind eventKind
	// form is the registered screen form a blocked mapping names. A blocked
	// mapping may exist ONLY where a grammar can confirm it: the event says the
	// agent is waiting and the grammar says the screen shows it waiting, and a
	// resting claim for which no evidence can exist does not get to rest
	// indefinitely.
	form string
	// text is which of activity.go's readers, if any, supplies this mapping's
	// activity line. The zero value is rung 4 of the ladder -- no text -- and
	// it is the right default for the three turn starts, whose only string is
	// the prompt: history by the second tool call, and a fragment at 128
	// runes. TestTheTextSourcesAreExactlyThese is the roll call that keeps a
	// new one from being added without a reason next to it.
	text textSource
}

// eventRule is one row of an agent's event table.
//
// Two events do not decide alone -- claude's Notification and opencode's
// session.status both carry the thing that matters inside the payload -- so a
// rule either holds a mapping or holds a discriminator and a table keyed by
// what it reads.
type eventRule struct {
	mapping      mapping
	discriminate func(payload []byte) string
	byValue      map[string]mapping
}

// eventRules is the whole table, per agent.
//
// The agent names are blockedRules' keys, which are what the daemon derives
// from pane_current_command. An agent that is not here reports nothing, which
// is the same answer an unknown event gets: this is a fail-closed table on
// every axis, because every way of being wrong here writes a state onto
// somebody's pane and two of the three states never expire.
var eventRules = map[string]map[string]eventRule{
	"claude": {
		// The turn start. Load-bearing beyond "the agent is working": the
		// three turn-end events are edges, and an edge is only safe because a
		// new turn wrote a non-resting state before its end could fire.
		"UserPromptSubmit": {mapping: mapping{
			name: "claude/UserPromptSubmit", state: tmux.StateWorking, kind: kindEdge}},
		// The hot hook. It is also the keepalive -- a long turn of many small
		// tool calls keeps refreshing the 60-second working window -- and it is
		// what the activity line comes from. It is also, see the file comment,
		// what makes the connected-case blocked badge slower.
		"PreToolUse": {mapping: mapping{name: "claude/PreToolUse", state: tmux.StateWorking,
			kind: kindEdge, text: textClaudeTool}},
		// Not a state on its own. See claudeNotifications.
		"Notification": {discriminate: claudeNotificationType, byValue: claudeNotifications},
		// The turn end.
		"Stop": {mapping: mapping{name: "claude/Stop", state: tmux.StateIdle, kind: kindEdge}},
		// SubagentStop is deliberately absent, and its absence is structural
		// rather than a filter: the installer does not register that hook, so
		// Stop is root-only against the Task-tool subagent class. Leaving the
		// table without an entry means that even a hand-edited settings.json
		// that did register it cannot write idle onto a pane whose root agent
		// is still working.
	},
	"opencode": {
		// The turn start, with the prompt text on it.
		"chat.message": {mapping: mapping{name: "opencode/chat.message", state: tmux.StateWorking, kind: kindEdge}},
		// busy is the turn start proper, and it fires repeatedly within one
		// turn -- 17 times in the three-tool turn Task 12 captured -- so
		// nothing here may assume it arrives once.
		"session.status": {discriminate: opencodeStatusType, byValue: opencodeStatuses},
		"tool.execute.before": {mapping: mapping{name: "opencode/tool.execute.before",
			state: tmux.StateWorking, kind: kindEdge, text: textOpencodeTool}},
		// The todo rung of the activity ladder. Rung 2, with the tool call
		// underneath it, which is correct whether or not the list is empty.
		"todo.updated": {mapping: mapping{name: "opencode/todo.updated", state: tmux.StateWorking,
			kind: kindEdge, text: textOpencodeTodo}},
		// The one blocked event any agent has that arrives with no delay.
		// There is no question string on it -- the design says there is and the
		// recorded payload says there is not -- but there does not need to be
		// one for the state, and the text is reduced from `metadata` per
		// permission class, whose keys vary and one of which is a whole diff.
		"permission.asked": {mapping: mapping{name: "opencode/permission.asked", state: tmux.StateBlocked,
			kind: kindEdge, form: "opencode/permission", text: textOpencodePermission}},
		// The turn end. It carries a sessionID, and a subagent's arrives
		// BEFORE the root's -- 2.05 s before, measured -- which is what Task
		// 15's parentID filter is for. That filter lives in the plugin,
		// because only the plugin saw the session.created that named the
		// parent.
		"session.idle": {mapping: mapping{name: "opencode/session.idle", state: tmux.StateIdle, kind: kindEdge}},
		// session.created is absent on purpose: it is the plugin's own
		// bookkeeping, the event that establishes parentage, and not a state
		// of the pane.
	},
	"pi": {
		// Not a state on its own, and the only event on any agent that is an
		// edge on one branch and a re-assertion on the other. See
		// piSessionStarts.
		"session_start": {discriminate: piSessionStartIdle, byValue: piSessionStarts},
		// The turn start.
		"input": {mapping: mapping{name: "pi/input", state: tmux.StateWorking, kind: kindEdge}},
		"tool_execution_start": {mapping: mapping{name: "pi/tool_execution_start",
			state: tmux.StateWorking, kind: kindEdge, text: textPiTool}},
		// Every kind of prompt, not only the numbered selector pi/selector was
		// written against: one captured kind is "custom", an extension's own
		// overlay with no title at all, and there are certainly more. Claiming
		// blocked for all of them is safe precisely because the mapping names
		// a form -- evidence rule 2 drops a blocked whose settled screen
		// matches no registered form for the agent, so a custom overlay that
		// pi/selector cannot read costs a badge that lasts N_blocked polls,
		// not one that lasts forever.
		"ui_prompt_start": {mapping: mapping{name: "pi/ui_prompt_start", state: tmux.StateBlocked,
			kind: kindEdge, form: "pi/selector", text: textPiPrompt}},
		// The turn end. Its entire payload is {"type":"agent_settled"} -- no
		// session id, no agent id, no parent -- so nothing downstream of here
		// can tell a root settle from an async subagent's, and Task 15's pi
		// filter has to work from ctx at registration time instead.
		"agent_settled": {mapping: mapping{name: "pi/agent_settled", state: tmux.StateIdle, kind: kindEdge}},
	},
}

// claudeNotifications is the notification_type whitelist.
//
// THE DEFAULT IS THE DECISION HERE, NOT THE ENUMERATION. The documented list is
// not documented as closed, it has plainly grown before, and it will grow
// again, so an unrecognised type writes nothing at all. The asymmetry that
// settles it:
//
//   - Ignoring a notification that should have meant blocked costs a badge that
//     is late and may never come. Not merely "a late badge": at a permission
//     wait the standing report is a fresh working from PreToolUse, and a fresh
//     working SKIPS THE CAPTURE, so IsBlocked is not even running. The true
//     cost is up to 60 s of working expiry and then whatever the grammar can
//     do, which for an uncaptured dialog is nothing.
//   - Defaulting an unknown notification to blocked costs a PERMANENT FALSE
//     BADGE. blocked is resting, so it never expires, and the changed-hash
//     escape hatch cannot save it: about 580 s of post-turn idle across ten
//     runs produced zero hash changes and zero pane output on all three agents.
//     On a finished agent that escape hatch cannot fire at all.
//
// Only permission_prompt and idle_prompt have ever been observed (Task 12); the
// other ten are here on Claude Code's documentation alone. That is deliberate
// and it is why the table is a whitelist: the two that were observed do not
// tell you the set is two, and nothing here may key off `message` -- one
// permission_prompt read "Claude needs your permission" and another "Claude
// Code needs your approval for the plan".
var claudeNotifications = map[string]mapping{
	// The only claude form any grammar can confirm, and the one claudeDialog
	// was written against, so evidence rule 2 can adjudicate this badge rather
	// than merely erase it.
	"permission_prompt": {name: "claude/Notification(permission_prompt)", state: tmux.StateBlocked,
		kind: kindEdge, form: "claude/permission", text: textClaudeNotification},
	// Claude Code is continuing the task.
	"quota_auto_resume_fired": {name: "claude/Notification(quota_auto_resume_fired)",
		state: tmux.StateWorking, kind: kindEdge},
	// "Claude finished responding about 60 seconds ago and you haven't typed
	// since". It re-asserts what Stop already said, so it is a REPAIR: without
	// that, an idle_prompt over a standing idle re-dates the finish and
	// re-badges every device, once per turn, a minute after every turn.
	"idle_prompt": {name: "claude/Notification(idle_prompt)",
		state: tmux.StateIdle, kind: kindReassertion},
	// The wait ended and the task was not continued. Same suppression, same
	// reason.
	"quota_auto_resume_disabled": {name: "claude/Notification(quota_auto_resume_disabled)",
		state: tmux.StateIdle, kind: kindReassertion},

	// Ignored PENDING A CAPTURE, all four. Each is a real wait drawn on this
	// pane's root screen that no registered form can confirm, and a blocked
	// mapping with no possible evidence is a badge that stands until a client
	// connects and is then erased about 4.5 seconds later by evidence rule 2,
	// while Claude is still waiting. Open question 10. Each promotes the day
	// somebody manufactures the screen AND WRITES A GRAMMAR -- a code change,
	// not a data change. The cost of leaving them here, stated plainly so
	// nobody reads it as a bug: an MCP form or a quota banner left overnight
	// produces no badge at all.
	//
	// They are listed rather than omitted so that the next person to read the
	// documented list finds them already considered.
	"quota_auto_resume_stale": {name: "claude/Notification(quota_auto_resume_stale)"},
	"elicitation_dialog":      {name: "claude/Notification(elicitation_dialog)"},
	"elicitation_url_dialog":  {name: "claude/Notification(elicitation_url_dialog)"},
	// Two situations under one type. "A background session or a teammate needs
	// input" is not this pane's root session; but this session asking a
	// teammate's terminal SETUP question is this pane's root session waiting on
	// this pane's screen. So it is pending a capture rather than excluded, and
	// if its form is ever captured, rule 2 keeps the other half honest.
	"agent_needs_input": {name: "claude/Notification(agent_needs_input)"},

	// Ignored outright: not a state of this session.
	"auth_success":         {name: "claude/Notification(auth_success)"},
	"elicitation_complete": {name: "claude/Notification(elicitation_complete)"},
	"elicitation_response": {name: "claude/Notification(elicitation_response)"},
	// "A background session finishes or fails" -- NOT this session's turn end.
	// Reported as idle it is a false done badge on an agent that is still
	// working, which is v2's central failure arriving through another door.
	"agent_completed": {name: "claude/Notification(agent_completed)"},
}

// piSessionStarts is pi's re-derivation of state on a session_start, and it is
// the whole reason the edge/re-assertion classification is per (event, state)
// pair rather than per event.
//
// pi has no separate "the CLI started" state, and a session_start that reported
// nothing would leave a freshly launched pi looking like whatever the classifier
// made of its splash screen. But session_start is not only a launch: a pi
// extension reload replaces the extension mid-run without another agent_start,
// so the extension re-derives what the agent is doing from ctx.isIdle() -- an
// extension that only ever set state on transitions would come back from a
// reload believing nothing was happening.
//
//   - The idle branch is a RE-ASSERTION. A reload can happen any number of
//     times while the agent sits idle, and it writes a resting state, which is
//     the criterion exactly. As an edge, every reload on an idle pane would
//     write idle;<now> and re-badge every device -- the badge storm arriving
//     through an event revision 3 of the design had classified nowhere.
//   - The working branch is an EDGE. working is transient, cannot badge, and
//     its redundant writes are the keepalive.
//
// WHAT THE EXTENSION MUST SEND, because this is a contract Task 17 has to keep
// and no recorded payload carries it: ctx is not part of pi's event object, so
// the extension adds `"tmux_web_is_idle": <ctx.isIdle()>` to the JSON it pipes to
// this subcommand. The key is namespaced because it is ours and not pi's.
var piSessionStarts = map[string]mapping{
	"working": {name: "pi/session_start(working)", state: tmux.StateWorking, kind: kindEdge},
	"idle":    {name: "pi/session_start(idle)", state: tmux.StateIdle, kind: kindReassertion},
}

// piSessionStartIdle reads that flag.
//
// Its failure direction is the opposite of every other discriminator in this
// file, and deliberately so: an unreadable payload, a missing key or a
// wrong-shaped value all answer "working" rather than "" -- an unknown
// notification_type must write nothing because the states it might mean rest
// forever, whereas here the two branches are known and only one of them rests.
// Falling to working costs at most 60 seconds of a wrong transient state on a
// pane whose agent has just started or reloaded; falling to idle would rest
// forever, and falling to "" would leave a freshly launched pi to the splash
// screen.
func piSessionStartIdle(payload []byte) string {
	var p struct {
		Idle bool `json:"tmux_web_is_idle"`
	}
	_ = json.Unmarshal(payload, &p)
	if p.Idle {
		return "idle"
	}
	return "working"
}

// opencodeStatuses is session.status's discriminator table, and it is a
// whitelist for the same reason claudeNotifications is.
//
// Only busy reports. status idle fires in the same millisecond as session.idle,
// which is opencode's turn end and carries the sessionID that Task 15's filter
// needs; reporting both would write idle twice under two timestamps, and the
// second one re-dates a finish the first already dated.
var opencodeStatuses = map[string]mapping{
	"busy": {name: "opencode/session.status(busy)", state: tmux.StateWorking, kind: kindEdge},
}

// lookupMapping is what one (agent, event, payload) means.
//
// ok reports whether the agent and the event are known at all. A known event
// whose payload discriminator is not on its whitelist returns ok=true with the
// zero state: that is the whitelist doing its job on traffic we expect to see,
// not a misconfigured integration, and the caller keeps quiet about it.
func lookupMapping(agent, event string, payload []byte) (m mapping, ok bool) {
	rule, ok := eventRules[agent][event]
	if !ok {
		return mapping{}, false
	}
	if rule.discriminate == nil {
		return rule.mapping, true
	}
	// A missing key, a wrong-shaped value and an unparseable payload all read
	// as "", which is on no whitelist. Fail-closed, as everywhere else here.
	return rule.byValue[rule.discriminate(payload)], true
}

// turnStartEvent names, per agent, the event whose write is that agent's turn
// start -- and, where the event is discriminated, the discriminator value that
// means the turn started.
//
// It exists so the turn-start invariant can be asserted rather than believed.
// The three turn-end events are edges, which means they write a resting state
// without looking first, and that is only safe because a new turn has already
// written a non-resting one: with no working in front of it, a turn end would
// be the second idle of one resting period and the criterion would have called
// it a re-assertion. An agent whose turn start stopped writing working would
// still pass every other test in this package, and would lose one badge per
// turn in production.
var turnStartEvent = map[string]struct{ event, value string }{
	"claude":   {event: "UserPromptSubmit"},
	"opencode": {event: "session.status", value: "busy"},
	"pi":       {event: "input"},
}

// turnStart is what that agent's turn start writes, resolved through the same
// tables everything else goes through, so a mapping that changed underneath it
// is visible here.
func turnStart(agent string) (mapping, bool) {
	ref, ok := turnStartEvent[agent]
	if !ok {
		return mapping{}, false
	}
	rule, ok := eventRules[agent][ref.event]
	if !ok {
		return mapping{}, false
	}
	if rule.discriminate == nil {
		return rule.mapping, ref.value == ""
	}
	m, ok := rule.byValue[ref.value]
	return m, ok
}

// allMappings is every mapping in this file, for the tests that have to hold
// this table against another one.
func allMappings() []mapping {
	var all []mapping
	for _, events := range eventRules {
		for _, rule := range events {
			if rule.discriminate == nil {
				all = append(all, rule.mapping)
				continue
			}
			for _, m := range rule.byValue {
				all = append(all, m)
			}
		}
	}
	return all
}

// claudeNotificationType reads a Notification payload's notification_type.
func claudeNotificationType(payload []byte) string {
	var p struct {
		NotificationType string `json:"notification_type"`
	}
	// The error is deliberately dropped: "" is the answer for a payload we
	// cannot read, and "" is ignored. The message field is not read at all --
	// one type has been seen under two different messages.
	_ = json.Unmarshal(payload, &p)
	return p.NotificationType
}

// opencodeStatusType reads a session.status event's properties.status.type.
func opencodeStatusType(payload []byte) string {
	var p struct {
		Properties struct {
			Status struct {
				Type string `json:"type"`
			} `json:"status"`
		} `json:"properties"`
	}
	_ = json.Unmarshal(payload, &p)
	return p.Properties.Status.Type
}
