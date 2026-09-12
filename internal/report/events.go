package report

import (
	"encoding/json"
	"fmt"

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
// WHY A PACKAGE OF ITS OWN, and not package main where it started. Everything
// here is DOMAIN JUDGEMENT: nothing in this file parses a command line, opens a
// socket or writes a tmux option. Sitting in package main it could import
// internal/tmux and nothing could import it, and that one-way wall is where a
// whole family of duplicate lists came from -- the agents were written out in
// tmux.Agents, in this table's keys, in a turn-start table beside it, in the
// installer's map and in two test literals, with nothing holding any of them
// together; the grammars that confirm a blocked badge could only be named by
// string; and each integration's event list was tied to this table by a test
// that skipped, or by nothing at all. As a package it can hold a *tmux.Form
// instead of an id, its keys can be checked against tmux.Agents at load, and
// the tests that hold the three integrations' lists to it can live beside it
// (integrations_test.go) instead of in the one package that happened to be able
// to see both.
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
//	standing report both differs in state from the one it would write AND is
//	older than it. See reportSupersedes: the second half is what keeps a
//	re-assertion that was descheduled past a later edge from overwriting fresh
//	news with stale news.
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
func reassertsFor(m Mapping) bool {
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

// Mapping is what one (agent, event) means. The zero state is "ignore": no
// write, no state change, no timestamp refresh, and not an error.
type Mapping struct {
	name  string
	state string
	// kind is edge or reassertion, and every mapping that writes a state names
	// one: the zero value is "nobody decided", not "edge". See eventKind for
	// the criterion and reassertsFor for what an undecided one falls back to.
	kind eventKind
	// form is the screen form a blocked mapping's badge rests on: THE GRAMMAR
	// ITSELF, not its name. A blocked mapping may exist ONLY where a grammar
	// can confirm it -- the event says the agent is waiting and the grammar
	// says the screen shows it waiting -- and a resting claim for which no
	// evidence can exist does not get to rest indefinitely.
	//
	// It was a string until this package existed, and it could not be anything
	// else: the grammars live in internal/tmux and package main held the table,
	// so the only thing a mapping could carry was an id and the only check
	// available was RegisteredForm, a membership test over every agent's forms
	// at once. Two lies fit through that. A mapping could name a form NOBODY
	// HAS -- a typo, or a form deleted from the registry -- and a mapping could
	// name ANOTHER AGENT'S form, which is the same badge with no grammar behind
	// it, because rule 2 only ever asks this agent's forms about this screen. A
	// pointer closes the first at compile time and checkRules closes the second
	// at process start. What neither closes is a mapping naming its own agent's
	// real form for a screen that form does not draw; that one is a judgement,
	// and TestTheBlockedMappingsAreExactlyThese is where it is made.
	form *tmux.Form
	// text is which of activity.go's readers, if any, supplies this mapping's
	// activity line. The zero value is rung 4 of the ladder -- no text -- and
	// it is the right default for the three turn starts, whose only string is
	// the prompt: history by the second tool call, and a fragment at 128
	// runes. TestTheTextSourcesAreExactlyThese is the roll call that keeps a
	// new one from being added without a reason next to it.
	text textSource
}

// State is what this mapping puts on the pane, or "" for an event the table
// deliberately ignores. It is the whole of what the writer needs to decide
// whether there is anything to write at all.
func (m Mapping) State() string { return m.state }

// Reasserts reports whether this mapping's write must read the standing report
// first. See reassertsFor for the default an unclassified mapping gets.
func (m Mapping) Reasserts() bool { return reassertsFor(m) }

// Text is the activity line this mapping publishes, read out of the payload by
// whichever of activity.go's readers the table named. "" is rung 4 of the
// ladder -- a state-only report -- and is the ordinary answer.
func (m Mapping) Text(payload []byte) string { return activityText(m.text, payload) }

// eventRule is one row of an agent's event table.
//
// Two events do not decide alone -- claude's Notification and opencode's
// session.status both carry the thing that matters inside the payload -- so a
// rule either holds a mapping or holds a discriminator and a table keyed by
// what it reads.
type eventRule struct {
	mapping      Mapping
	discriminate func(payload []byte) string
	byValue      map[string]Mapping
}

// agentTable is everything this package knows about one agent: its events and
// which of them is its turn start.
//
// The two used to be separate maps keyed by the same names, and that is the
// shape the drift came in through -- an agent could be in one and not the
// other, and the only thing holding either to the list of agents the daemon
// actually recognises was a literal in a test. One table means one key set, and
// checkRules holds that key set to tmux.Agents at process start.
type agentTable struct {
	// turnStart names the event whose write is this agent's turn start -- and,
	// where the event is discriminated, the discriminator value that means the
	// turn started.
	//
	// It exists so the turn-start invariant can be asserted rather than
	// believed. The three turn-end events are edges, which means they write a
	// resting state without looking first, and that is only safe because a new
	// turn has already written a non-resting one: with no working in front of
	// it, a turn end would be the second idle of one resting period and the
	// criterion would have called it a re-assertion. An agent whose turn start
	// stopped writing working would still pass every other test in this
	// package, and would lose one badge per turn in production.
	turnStart turnStartRef
	// events is what each of this agent's own event names means.
	events map[string]eventRule
}

// turnStartRef points at a row of the agent's own events table, rather than
// repeating what that row says. A mapping that changed underneath it is then
// visible to turnStart instead of being shadowed by a copy.
type turnStartRef struct{ event, value string }

// rules is the whole table, per agent.
//
// The agent names are tmux.Agents, which is what the daemon derives from
// pane_current_command, and checkRules refuses to let this process start if the
// two lists disagree. An agent that is not here reports nothing, which is the
// same answer an unknown event gets: this is a fail-closed table on every axis,
// because every way of being wrong here writes a state onto somebody's pane and
// two of the three states never expire.
var rules = map[string]agentTable{
	"claude": {turnStart: turnStartRef{event: "UserPromptSubmit"}, events: map[string]eventRule{
		// The turn start. Load-bearing beyond "the agent is working": the
		// three turn-end events are edges, and an edge is only safe because a
		// new turn wrote a non-resting state before its end could fire.
		"UserPromptSubmit": {mapping: Mapping{
			name: "claude/UserPromptSubmit", state: tmux.StateWorking, kind: kindEdge}},
		// The hot hook. It is also the keepalive -- a long turn of many small
		// tool calls keeps refreshing the 60-second working window -- and it is
		// what the activity line comes from. It is also, see the file comment,
		// what makes the connected-case blocked badge slower.
		"PreToolUse": {mapping: Mapping{name: "claude/PreToolUse", state: tmux.StateWorking,
			kind: kindEdge, text: textClaudeTool}},
		// Not a state on its own. See claudeNotifications.
		"Notification": {discriminate: claudeNotificationType, byValue: claudeNotifications},
		// The turn end, and not always. See claudeStops: a root Stop fires
		// while a subagent it launched is still working.
		"Stop": {discriminate: claudeStopSubagentRunning, byValue: claudeStops},
		// SubagentStop is deliberately absent, and its absence is structural
		// rather than a filter: the installer does not register that hook, so
		// Stop is root-only against the Task-tool subagent class. Leaving the
		// table without an entry means that even a hand-edited settings.json
		// that did register it cannot write idle onto a pane whose root agent
		// is still working.
		//
		// What that does NOT buy, and claudeStops does: the ROOT's own Stop
		// fires while a subagent is still running. Not registering
		// SubagentStop keeps the CHILD from reporting; it says nothing about
		// the parent reporting too early.
	}},
	"opencode": {turnStart: turnStartRef{event: "session.status", value: "busy"}, events: map[string]eventRule{
		// The turn start, with the prompt text on it.
		"chat.message": {mapping: Mapping{name: "opencode/chat.message", state: tmux.StateWorking, kind: kindEdge}},
		// busy is the turn start proper, and it fires repeatedly within one
		// turn -- 17 times in the three-tool turn Task 12 captured -- so
		// nothing here may assume it arrives once.
		"session.status": {discriminate: opencodeStatusType, byValue: opencodeStatuses},
		"tool.execute.before": {mapping: Mapping{name: "opencode/tool.execute.before",
			state: tmux.StateWorking, kind: kindEdge, text: textOpencodeTool}},
		// The todo rung of the activity ladder. Rung 2, with the tool call
		// underneath it, which is correct whether or not the list is empty.
		"todo.updated": {mapping: Mapping{name: "opencode/todo.updated", state: tmux.StateWorking,
			kind: kindEdge, text: textOpencodeTodo}},
		// The one blocked event any agent has that arrives with no delay.
		// There is no question string on it -- the design says there is and the
		// recorded payload says there is not -- but there does not need to be
		// one for the state, and the text is reduced from `metadata` per
		// permission class, whose keys vary and one of which is a whole diff.
		"permission.asked": {mapping: Mapping{name: "opencode/permission.asked", state: tmux.StateBlocked,
			kind: kindEdge, form: tmux.OpencodePermissionForm, text: textOpencodePermission}},
		// The turn end. It carries a sessionID, and a subagent's arrives
		// BEFORE the root's -- 2.05 s before, measured -- which is what Task
		// 15's parentID filter is for. That filter lives in the plugin,
		// because only the plugin saw the session.created that named the
		// parent.
		//
		// A RE-ASSERTION, and the only turn end on any agent that is one.
		// MEASURED on opencode 1.18.30: a turn that DIED AT THE PROVIDER fired
		// session.idle, then message.updated, then session.idle again, about a
		// second apart, with no session.status(busy) between them. That is the
		// criterion exactly -- an event that can recur inside one resting
		// period and writes a resting state -- and the fact that it is named
		// like a turn end has no vote. As an edge the second idle re-dates a
		// finish the first already dated, and finishedAt is derived from the
		// report's own timestamp, so every device that had seen the finish
		// badges again about a second later. Bounded and small: failed turns
		// only, and about a second of re-dating.
		//
		// WHAT IT COSTS opencode, which had paid nothing until now: ONE
		// show-options fork per turn end. Not per event -- the hot paths
		// (session.status busy, 17 times in one three-tool turn, and
		// tool.execute.before) are working edges and still read nothing.
		//
		// WHAT IT COSTS THE BADGE: nothing the turn-start invariant does not
		// already cover. session.status(busy) writes working before this can
		// fire, so the standing state disagrees and the finish is written. The
		// one case it loses is a turn whose START write also failed, and that
		// is a pane that never showed working either.
		"session.idle": {mapping: Mapping{name: "opencode/session.idle", state: tmux.StateIdle,
			kind: kindReassertion}},
		// session.created is absent on purpose: it is the plugin's own
		// bookkeeping, the event that establishes parentage, and not a state
		// of the pane.
	}},
	"pi": {turnStart: turnStartRef{event: "input"}, events: map[string]eventRule{
		// Not a state on its own, and the only event on any agent that is an
		// edge on one branch and a re-assertion on the other. See
		// piSessionStarts.
		"session_start": {discriminate: piSessionStartIdle, byValue: piSessionStarts},
		// The turn start.
		"input": {mapping: Mapping{name: "pi/input", state: tmux.StateWorking, kind: kindEdge}},
		"tool_execution_start": {mapping: Mapping{name: "pi/tool_execution_start",
			state: tmux.StateWorking, kind: kindEdge, text: textPiTool}},
		// Every kind of prompt, not only the numbered selector pi/selector was
		// written against: one captured kind is "custom", an extension's own
		// overlay with no title at all, and there are certainly more. Claiming
		// blocked for all of them is safe precisely because the mapping names
		// a form -- evidence rule 2 drops a blocked whose settled screen
		// matches no registered form for the agent, so a custom overlay that
		// pi/selector cannot read costs a badge that lasts N_blocked polls,
		// not one that lasts forever.
		"ui_prompt_start": {mapping: Mapping{name: "pi/ui_prompt_start", state: tmux.StateBlocked,
			kind: kindEdge, form: tmux.PiSelectorForm, text: textPiPrompt}},
		// The turn end. Its entire payload is {"type":"agent_settled"} -- no
		// session id, no agent id, no parent -- so nothing downstream of here
		// can tell a root settle from an async subagent's, and Task 15's pi
		// filter has to work from ctx at registration time instead.
		"agent_settled": {mapping: Mapping{name: "pi/agent_settled", state: tmux.StateIdle, kind: kindEdge}},
	}},
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
var claudeNotifications = map[string]Mapping{
	// The only claude form any grammar can confirm, and the one claudeDialog
	// was written against, so evidence rule 2 can adjudicate this badge rather
	// than merely erase it.
	"permission_prompt": {name: "claude/Notification(permission_prompt)", state: tmux.StateBlocked,
		kind: kindEdge, form: tmux.ClaudePermissionForm, text: textClaudeNotification},
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

// claudeStops is what a root Stop means, and it is the one event in this table
// whose own name is not the whole answer.
//
// MEASURED TWICE against a real Claude Code 2.1.267: the ROOT's Stop fires
// while a subagent it launched is still working, and a new turn starts when
// that subagent returns. 13.2 s and 6.2 s of reported idle IN THE MIDDLE OF
// WORK, on two separate runs.
//
// This is not the nested-CLI hole and agent_id does not filter it: there is no
// agent_id anywhere in the payload, because the payload really is the root's,
// and the root filter waves it through correctly. What separates the two
// captured Stops is `background_tasks` and nothing else -- stop.json carries
// `[]`, stop_subagent_running.json carries one {id, type:"subagent",
// status:"running", description, agent_type}, and every other difference
// between the two files is the turn's own content.
//
// WHAT IT REPORTS INSTEAD: NOTHING. Not working. The pane keeps the working
// report this turn already put on it, which expires on its own 60 s after the
// last root tool call and then falls back to the screen classifier -- A LATE
// TRANSITION RATHER THAN A FALSE RESTING STATE, which is the direction this
// project takes everywhere else. Reporting working is defensible too and would
// refresh the keepalive, but it would overwrite the activity line the Agent
// tool call put there with nothing, and it would assert a state for a moment
// when the root model genuinely is not generating. Silence claims nothing and
// keeps the line.
//
// WHAT THE SILENCE COSTS, in the case it does not cover: a turn whose LAST act
// is the subagent and where no further event ever arrives -- the subagent is
// killed, the user presses escape, claude exits. That turn's finish badge is
// lost: the pane holds working to the 60-second expiry and then lives where a
// pane with no integration at all lives, on the classifier. One badge on an
// abandoned turn, against 13 seconds of a false DONE badge on every turn that
// ends with a subagent still running.
//
// BOUND THE CLAIM, so nobody simplifies this away for being unreproducible:
// WITH A BROWSER ATTACHED, evidence rule 3 already drops that reported idle,
// because the screen is still churning under the subagent -- so someone
// watching the app sees no bad badge and cannot reproduce this at all. The
// damage is unbounded only when NOBODY IS WATCHING, which is the case this
// whole feature exists for. Failing to reproduce it with the app open is not
// evidence that it is not there.
var claudeStops = map[string]Mapping{
	// The ordinary turn end, and still an edge: the turn-start invariant put a
	// working in front of it, and a Stop cannot fire twice inside one resting
	// period.
	"settled": {name: "claude/Stop(settled)", state: tmux.StateIdle, kind: kindEdge},
	// Listed rather than omitted, like the ignored notification types, so that
	// the next person to read this table finds the case already considered.
	"subagent_running": {name: "claude/Stop(subagent_running)"},
}

// claudeStopSubagentRunning reads Stop's background_tasks.
//
// WHY ONLY type "subagent". The documented type list is `shell`, `subagent`,
// `monitor`, `workflow`, `teammate`, `cloud session` and `MCP task`; ONLY
// `subagent` has ever been observed, and the other six are deliberately not
// filtered. A backgrounded SHELL is precisely the case where the root really
// is idle -- the user has the prompt back, the turn is over, and the shell
// re-wakes claude later -- so suppressing there would cost a badge on an
// ordinary turn. What was measured is narrower than "work is running": a
// returning SUBAGENT resumes the root's turn. An unknown type reports idle,
// which is where this row already was.
//
// WHY status must say "running", which is the opposite asymmetry. No capture
// shows what a FINISHED background task reads -- subagent_stop.json still
// lists its own entry as "running" at the subagent's own SubagentStop -- so a
// completed task lingering in the array is a thing this evidence cannot rule
// out. Matching the type alone would then mean ONE subagent anywhere in a
// session silences every Stop for the rest of it: permanent, silent badge
// loss, which is far worse than the 13 seconds this filter exists to remove.
// Matching the one value that was actually observed fails toward today's
// behaviour instead.
//
// THE FAIL DIRECTION for a background_tasks this cannot read at all -- renamed,
// re-nested, turned into an object -- is "settled", for the same reason: a
// bounded false idle is the failure this project can see, and silencing every
// Stop on every pane forever with every other test still passing is the one it
// cannot.
func claudeStopSubagentRunning(payload []byte) string {
	var p struct {
		BackgroundTasks []struct {
			Type   string `json:"type"`
			Status string `json:"status"`
		} `json:"background_tasks"`
	}
	// Dropped on purpose, and in the other direction from every other
	// discriminator here: a payload whose background_tasks is a shape this
	// struct cannot hold says nothing about whether a subagent is running, and
	// the answer to "nothing known" on this event is the turn end it has always
	// been. The payload gate has already refused anything that is not the JSON
	// object a hook sends.
	_ = json.Unmarshal(payload, &p)
	for _, task := range p.BackgroundTasks {
		// Both fields, exactly as spelled, uncased. A whitelist of one.
		if task.Type == "subagent" && task.Status == "running" {
			return "subagent_running"
		}
	}
	return "settled"
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
var piSessionStarts = map[string]Mapping{
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
var opencodeStatuses = map[string]Mapping{
	"busy": {name: "opencode/session.status(busy)", state: tmux.StateWorking, kind: kindEdge},
}

// Lookup is what one (agent, event, payload) means.
//
// ok reports whether the agent and the event are known at all. A known event
// whose payload discriminator is not on its whitelist returns ok=true with the
// zero state: that is the whitelist doing its job on traffic we expect to see,
// not a misconfigured integration, and the caller keeps quiet about it.
func Lookup(agent, event string, payload []byte) (m Mapping, ok bool) {
	rule, ok := rules[agent].events[event]
	if !ok {
		return Mapping{}, false
	}
	if rule.discriminate == nil {
		return rule.mapping, true
	}
	// A missing key, a wrong-shaped value and an unparseable payload all read
	// as "", which is on no whitelist. Fail-closed, as everywhere else here.
	return rule.byValue[rule.discriminate(payload)], true
}

// EventsFor is the event names this table maps for one agent, unsorted.
//
// It exists so that a test outside this package does not have to write the list
// out again. The live-bus wiring test is the caller: it watches what a real
// opencode spawns and has to say which names are legitimate, and a literal
// there was a fifth copy of a list this package already holds.
func EventsFor(agent string) []string {
	events := make([]string, 0, len(rules[agent].events))
	for event := range rules[agent].events {
		events = append(events, event)
	}
	return events
}

// turnStart is what that agent's turn start writes, resolved through the same
// tables everything else goes through, so a mapping that changed underneath it
// is visible here.
func turnStart(agent string) (Mapping, bool) {
	table, ok := rules[agent]
	if !ok {
		return Mapping{}, false
	}
	return table.resolveTurnStart()
}

// resolveTurnStart is turnStart for a table that is not necessarily the shipped
// one, which is what lets checkRules be handed a drifted table and answer about
// THAT table rather than about this package's own.
func (at agentTable) resolveTurnStart() (Mapping, bool) {
	rule, ok := at.events[at.turnStart.event]
	if !ok {
		return Mapping{}, false
	}
	if rule.discriminate == nil {
		return rule.mapping, at.turnStart.value == ""
	}
	m, ok := rule.byValue[at.turnStart.value]
	return m, ok
}

// allMappings is every mapping in this file, for the tests that have to hold
// this table against another one.
func allMappings() []Mapping {
	var all []Mapping
	for _, table := range rules {
		for _, rule := range table.events {
			all = append(all, rule.mappings()...)
		}
	}
	return all
}

// init refuses to let a process start on a table that has drifted.
//
// A PANIC AT LOAD AND NOT A TEST, and the difference is the whole point of
// moving this table into a package of its own. The lists it checks are static
// data with no input: whatever checkRules says here, it says the same on every
// machine, at every startup, forever. So a drifted table cannot reach a user's
// machine and misbehave there -- it panics on the FIRST run of anything that
// links this package, which is `go test`, long before it is a binary -- and
// that is the difference between drift being impossible to ship and being
// visible to whoever reads a test failure.
//
// It is not a compile error, and the distinction is worth keeping straight: `go
// build` does not run init, so the wall is `go test ./...` and the first
// `tmux-web serve`, not the build. Every package here is under test, so the
// suite is the wall in practice.
//
// What it does NOT check is anything requiring judgement. Whether a blocked
// mapping's form is the form that agent actually draws for that event, and
// whether a mapping is an edge or a repair, are decisions; they live in the
// roll-call tests, spelled out next to their reasons.
func init() {
	if err := checkRules(rules, tmux.Agents); err != nil {
		panic("internal/report: " + err.Error())
	}
}

// checkRules is what init asserts, as a function taking its inputs, so that a
// test can hand it a drifted table and see it complain. A checker that is only
// ever called on the shipped data proves nothing about what it would reject.
//
// THREE INVARIANTS, one per way these lists have drifted or could:
//
//  1. The agents are exactly tmux.Agents. That list gates whether a pane is
//     captured, whether it gets a state and whether it gets a logo; an agent
//     with a table here and no entry there is a table nothing can ever reach,
//     and one there with no table here reports nothing with no sign that it
//     was meant to.
//  2. A blocked mapping's form is one of THIS agent's registered forms, and no
//     other mapping carries one at all. Rule 2 in the daemon only ever asks
//     the pane's own agent's forms about the pane's screen, so a mapping
//     holding another agent's grammar is a badge with nothing behind it --
//     which is exactly what a membership test over every agent's forms at once
//     could not see.
//  3. Every agent's turnStart resolves, through the events table, to a working
//     EDGE. The three turn-end events write a resting state without reading
//     what is standing, and that is safe only because a turn start put a
//     non-resting state in front of them.
func checkRules(table map[string]agentTable, agents []string) error {
	known := make(map[string]bool, len(agents))
	for _, a := range agents {
		known[a] = true
		if _, ok := table[a]; !ok {
			return fmt.Errorf("the daemon treats %q as a coding agent and this table has no events for it: "+
				"every event it sends would be answered with `nothing known`", a)
		}
	}
	for agent, at := range table {
		if !known[agent] {
			return fmt.Errorf("this table maps events for %q, which is not one of tmux.Agents: "+
				"the daemon never derives that name from pane_current_command, so no row of it can ever be reached", agent)
		}
		for event, rule := range at.events {
			for _, m := range rule.mappings() {
				if err := checkForm(agent, m); err != nil {
					return fmt.Errorf("%s's %s event: %w", agent, event, err)
				}
			}
		}
		m, ok := at.resolveTurnStart()
		if !ok || m.state != tmux.StateWorking || m.kind != kindEdge {
			return fmt.Errorf("%q has no turn-start working edge (%+v, ok=%v): its turn end is an edge "+
				"only because one exists in front of it", agent, m, ok)
		}
	}
	return nil
}

// checkForm is invariant 2, for one mapping.
func checkForm(agent string, m Mapping) error {
	if m.state != tmux.StateBlocked {
		// A form is a blocked mapping's evidence and means nothing anywhere
		// else. Carrying one elsewhere would also make the check above pass by
		// accident on a row that never rests.
		if m.form != nil {
			return fmt.Errorf("%s reports %q and names form %q; a form is a blocked mapping's evidence",
				m.name, m.state, m.form.ID)
		}
		return nil
	}
	if m.form == nil {
		return fmt.Errorf("%s reports blocked and names no form: blocked never expires, so a badge "+
			"no grammar can confirm stands until a client connects and is then erased while the agent waits", m.name)
	}
	for _, f := range tmux.FormsFor(agent) {
		if f == m.form {
			return nil
		}
	}
	return fmt.Errorf("%s names form %q, which is not one of %s's own: rule 2 asks only the pane's "+
		"agent's grammars about the pane's screen, so nothing can ever confirm that badge", m.name, m.form.ID, agent)
}

// mappings is every Mapping one rule can produce, discriminated or not. The
// discriminated tables are the half that matters: claude's only blocked mapping
// lives inside claudeNotifications, so a walk that read rule.mapping alone would
// check everything except the table most in need of it.
func (r eventRule) mappings() []Mapping {
	if r.discriminate == nil {
		return []Mapping{r.mapping}
	}
	all := make([]Mapping, 0, len(r.byValue))
	for _, m := range r.byValue {
		all = append(all, m)
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
