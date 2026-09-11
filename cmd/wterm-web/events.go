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
// Task 14 owns it and adds the read-before-write that makes a re-assertion
// different from an edge. It is declared here because two of the whitelist's
// entries -- idle_prompt and quota_auto_resume_disabled -- are specified as
// repairs *in this table*, and a table that cannot say so would have to be
// restructured rather than filled in. Today every write is unconditional.
type eventKind int

const (
	// kindEdge writes unconditionally, without reading the standing option.
	// That is safe only because of the turn-start invariant: every integration
	// writes working at turn start, so a turn end always has a non-resting
	// state in front of it. Task 14 tests that.
	kindEdge eventKind = iota
	// kindReassertion writes only when the standing report disagrees. It is
	// what stops Notification(idle_prompt), which fires about 60 s after every
	// turn, from re-badging every device once per turn: finishedAt is derived
	// from the report's own timestamp, so a fresh idle over a standing idle is
	// a new finish as far as the browser is concerned.
	kindReassertion
)

// textSource is where a mapping's activity text comes from.
//
// Task 15 owns it: the basename rule, the first-word rule, the tool name alone,
// and the per-agent reductions that keep opencode's edit diff out of a 1 KiB
// option. Declared here for the same reason as eventKind. Until it lands, the
// integration form reports state only, which is a complete report -- a
// three-part report is the ordinary Claude case and the reader accepts it.
type textSource int

// textNone is the zero value and today the only one: no activity text.
const textNone textSource = 0

// mapping is what one (agent, event) means. The zero state is "ignore": no
// write, no state change, no timestamp refresh, and not an error.
type mapping struct {
	name  string
	state string
	kind  eventKind // edge or reassertion; Task 14
	// form is the registered screen form a blocked mapping names. A blocked
	// mapping may exist ONLY where a grammar can confirm it: the event says the
	// agent is waiting and the grammar says the screen shows it waiting, and a
	// resting claim for which no evidence can exist does not get to rest
	// indefinitely.
	form string
	text textSource // Task 15
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
		"UserPromptSubmit": {mapping: mapping{name: "claude/UserPromptSubmit", state: tmux.StateWorking}},
		// The hot hook. It is also the keepalive -- a long turn of many small
		// tool calls keeps refreshing the 60-second working window -- and it is
		// what the activity line comes from. It is also, see the file comment,
		// what makes the connected-case blocked badge slower.
		"PreToolUse": {mapping: mapping{name: "claude/PreToolUse", state: tmux.StateWorking}},
		// Not a state on its own. See claudeNotifications.
		"Notification": {discriminate: claudeNotificationType, byValue: claudeNotifications},
		// The turn end.
		"Stop": {mapping: mapping{name: "claude/Stop", state: tmux.StateIdle}},
		// SubagentStop is deliberately absent, and its absence is structural
		// rather than a filter: the installer does not register that hook, so
		// Stop is root-only against the Task-tool subagent class. Leaving the
		// table without an entry means that even a hand-edited settings.json
		// that did register it cannot write idle onto a pane whose root agent
		// is still working.
	},
	"opencode": {
		// The turn start, with the prompt text on it.
		"chat.message": {mapping: mapping{name: "opencode/chat.message", state: tmux.StateWorking}},
		// busy is the turn start proper, and it fires repeatedly within one
		// turn -- 17 times in the three-tool turn Task 12 captured -- so
		// nothing here may assume it arrives once.
		"session.status": {discriminate: opencodeStatusType, byValue: opencodeStatuses},
		"tool.execute.before": {mapping: mapping{
			name: "opencode/tool.execute.before", state: tmux.StateWorking}},
		// The todo rung of the activity ladder. Rung 2, with the tool call
		// underneath it, which is correct whether or not the list is empty.
		"todo.updated": {mapping: mapping{name: "opencode/todo.updated", state: tmux.StateWorking}},
		// The one blocked event any agent has that arrives with no delay.
		// There is no question string on it -- see Task 15 -- but there does
		// not need to be one for the state.
		"permission.asked": {mapping: mapping{
			name: "opencode/permission.asked", state: tmux.StateBlocked, form: "opencode/permission"}},
		// The turn end. It carries a sessionID, and a subagent's arrives
		// BEFORE the root's -- 2.05 s before, measured -- which is what Task
		// 15's parentID filter is for. That filter lives in the plugin,
		// because only the plugin saw the session.created that named the
		// parent.
		"session.idle": {mapping: mapping{name: "opencode/session.idle", state: tmux.StateIdle}},
		// session.created is absent on purpose: it is the plugin's own
		// bookkeeping, the event that establishes parentage, and not a state
		// of the pane.
	},
	"pi": {
		// pi has no separate "the CLI started" state, and a session_start that
		// reported nothing would leave a freshly launched pi looking like
		// whatever the classifier made of its splash screen. working expires
		// on its own after the window, so the cost of being wrong is bounded
		// in the one direction that matters.
		"session_start": {mapping: mapping{name: "pi/session_start", state: tmux.StateWorking}},
		// The turn start.
		"input":                {mapping: mapping{name: "pi/input", state: tmux.StateWorking}},
		"tool_execution_start": {mapping: mapping{name: "pi/tool_execution_start", state: tmux.StateWorking}},
		// Every kind of prompt, not only the numbered selector pi/selector was
		// written against: one captured kind is "custom", an extension's own
		// overlay with no title at all, and there are certainly more. Claiming
		// blocked for all of them is safe precisely because the mapping names
		// a form -- evidence rule 2 drops a blocked whose settled screen
		// matches no registered form for the agent, so a custom overlay that
		// pi/selector cannot read costs a badge that lasts N_blocked polls,
		// not one that lasts forever.
		"ui_prompt_start": {mapping: mapping{
			name: "pi/ui_prompt_start", state: tmux.StateBlocked, form: "pi/selector"}},
		// The turn end. Its entire payload is {"type":"agent_settled"} -- no
		// session id, no agent id, no parent -- so nothing downstream of here
		// can tell a root settle from an async subagent's, and Task 15's pi
		// filter has to work from ctx at registration time instead.
		"agent_settled": {mapping: mapping{name: "pi/agent_settled", state: tmux.StateIdle}},
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
	"permission_prompt": {name: "claude/Notification(permission_prompt)",
		state: tmux.StateBlocked, form: "claude/permission"},
	// Claude Code is continuing the task.
	"quota_auto_resume_fired": {name: "claude/Notification(quota_auto_resume_fired)",
		state: tmux.StateWorking},
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

// opencodeStatuses is session.status's discriminator table, and it is a
// whitelist for the same reason claudeNotifications is.
//
// Only busy reports. status idle fires in the same millisecond as session.idle,
// which is opencode's turn end and carries the sessionID that Task 15's filter
// needs; reporting both would write idle twice under two timestamps, and the
// second one re-dates a finish the first already dated.
var opencodeStatuses = map[string]mapping{
	"busy": {name: "opencode/session.status(busy)", state: tmux.StateWorking},
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
