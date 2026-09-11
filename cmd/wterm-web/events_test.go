package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/diegok/tmux-web/internal/tmux"
)

// The whitelist, one row per documented notification_type plus invented ones.
//
// It drives the whole subcommand rather than calling lookupMapping, because the
// claim under test is not "the table says idle" but "NOTHING IS WRITTEN". A
// test that only compared states would pass against a mutant that defaulted an
// unknown type to the previous state, or to working -- both of which write.
func TestClaudeNotificationWhitelist(t *testing.T) {
	for _, tc := range []struct {
		typ   string
		state string // "" means: write nothing at all
	}{
		{"permission_prompt", tmux.StateBlocked},
		{"quota_auto_resume_fired", tmux.StateWorking},
		{"idle_prompt", tmux.StateIdle},                // as a REPAIR only; Task 14
		{"quota_auto_resume_disabled", tmux.StateIdle}, // same
		// Ignored PENDING A CAPTURE: a real wait the screen cannot see, and no
		// registered form can confirm it. Open question 10.
		{"quota_auto_resume_stale", ""},
		{"elicitation_dialog", ""},
		{"elicitation_url_dialog", ""},
		{"agent_needs_input", ""},
		// Ignored outright: not a state of this session.
		{"auth_success", ""},
		{"elicitation_complete", ""},
		{"elicitation_response", ""},
		// "a background session finishes or fails" -- NOT this session's turn
		// end. Reported as idle it is a false done badge on a working agent.
		{"agent_completed", ""},
		// The default, and it matters more than the table.
		{"a_type_nobody_has_documented_yet", ""},
		{"PERMISSION_PROMPT", ""}, // the whitelist is not case-folded
		{"", ""},
	} {
		t.Run(tc.typ, func(t *testing.T) {
			payload, err := json.Marshal(map[string]string{
				"hook_event_name":   "Notification",
				"message":           "Claude needs your permission",
				"notification_type": tc.typ,
			})
			if err != nil {
				t.Fatal(err)
			}
			got, writes := reportOne(t, []string{"--agent", "claude", "--event", "Notification"}, string(payload))
			if tc.state == "" {
				if writes != 0 {
					t.Fatalf("notification_type %q wrote %q in %d tmux command(s); an unrecognised or "+
						"ignored type must write NOTHING -- a blocked or idle it cannot justify never expires",
						tc.typ, got, writes)
				}
				return
			}
			if writes != 1 || got != tc.state {
				t.Fatalf("notification_type %q wrote %q in %d tmux command(s), want %q in exactly 1",
					tc.typ, got, writes, tc.state)
			}
		})
	}
}

// The same message on two different types, and one type under two different
// messages. Nothing may key off `message`: Task 12 recorded one
// permission_prompt reading "Claude needs your permission" and another reading
// "Claude Code needs your approval for the plan", so a reader that had learned
// the first string would have missed the plan dialog entirely.
func TestNotificationMessageIsNotAProxyForType(t *testing.T) {
	const msg = "Claude needs your permission"
	for _, tc := range []struct{ typ, want string }{
		{"permission_prompt", tmux.StateBlocked},
		{"auth_success", ""},
	} {
		payload := `{"hook_event_name":"Notification","message":"` + msg + `","notification_type":"` + tc.typ + `"}`
		got, writes := reportOne(t, []string{"--agent", "claude", "--event", "Notification"}, payload)
		if tc.want == "" && writes != 0 {
			t.Errorf("%q under the permission message wrote %q; the message must decide nothing", tc.typ, got)
		}
		if tc.want != "" && got != tc.want {
			t.Errorf("%q wrote %q, want %q", tc.typ, got, tc.want)
		}
	}
}

// The composition test. The two tables live in different files, each is correct
// alone, and the contradiction is only visible from above: revision 2 of the
// design mapped five types to blocked, four of which had no screen form any
// grammar could match -- and evidence rule 2 deletes exactly those. Those
// badges would have stood until a client connected and then been erased about
// 4.5 seconds later while Claude was still waiting.
//
// It is specified at FORM granularity on purpose. "Every blocked entry names an
// agent with a grammar" is vacuous -- claude IS in the map -- so the three types
// revision 3 demoted would all have passed it, which is to say the test written
// to stop round two's blocker recurring could not have seen it.
func TestEveryBlockedMappingNamesARegisteredForm(t *testing.T) {
	blocked := 0
	for _, m := range allMappings() {
		if m.state != tmux.StateBlocked {
			// Only a blocked mapping rests forever on a claim nothing can
			// check, so only a blocked mapping needs a form. The others must
			// not carry one, or the test above starts passing by accident.
			if m.form != "" {
				t.Errorf("%s reports %q but names form %q; a form is a blocked mapping's evidence and means nothing here",
					m.name, m.state, m.form)
			}
			continue
		}
		blocked++
		if !tmux.RegisteredForm(m.form) {
			t.Errorf("%s maps to blocked but names form %q, which no grammar confirms: "+
				"evidence rule 2 would delete that badge as soon as a client connected", m.name, m.form)
		}
	}
	// Without this the whole test is vacuous the day somebody empties the
	// table: ranging over nothing passes.
	if blocked == 0 {
		t.Fatal("no mapping reports blocked at all; this test then proves nothing")
	}
}

// Every blocked mapping there is, by name and written out in full.
//
// Two things need a literal list, and neither is served by the test above.
//
// The first is that RegisteredForm is a membership test, so naming an existing
// form is enough to satisfy it -- promoting elicitation_dialog to blocked under
// form "claude/permission" would pass, and it is a lie: the MCP form does not
// draw claude's permission dialog and rule 2 would delete the badge. A blocked
// mapping is the one entry in this file that can rest forever on a false claim,
// so adding one has to be a deliberate edit HERE, next to the reason, and not a
// line in a table that quietly satisfies a predicate.
//
// The second is that allMappings has to reach inside the discriminated tables.
// claude's only blocked mapping lives in claudeNotifications, not in eventRules
// directly, so an allMappings that returned each rule's own mapping and skipped
// byValue would hide the whole whitelist from the consistency test -- which
// would still pass, on opencode's and pi's entries, proving nothing about the
// one table that most needs proving.
func TestTheBlockedMappingsAreExactlyThese(t *testing.T) {
	want := map[string]bool{
		"claude/Notification(permission_prompt)": true,
		"opencode/permission.asked":              true,
		"pi/ui_prompt_start":                     true,
	}
	got := map[string]bool{}
	for _, m := range allMappings() {
		if m.state == tmux.StateBlocked {
			got[m.name] = true
		}
	}
	for name := range want {
		if !got[name] {
			t.Errorf("%s no longer reports blocked -- or allMappings cannot see it, which is worse: "+
				"the consistency test would then pass without ever looking at it", name)
		}
	}
	for name := range got {
		if !want[name] {
			t.Errorf("%s reports blocked and this test did not know about it. A blocked report never "+
				"expires, so adding one is a decision, not a table row: name the screen form that "+
				"confirms it, check the grammar really matches that screen, and add it here", name)
		}
	}
}

// Which mappings are repairs, written out in full for the same reason the
// blocked ones are.
//
// Nothing reads `kind` yet -- Task 14 adds the show-options read that makes a
// re-assertion behave differently from an edge -- so this asserts a decision
// rather than a behaviour, and it is the decision that matters most in the
// table: idle_prompt fires about 60 SECONDS AFTER EVERY TURN, and finishedAt is
// derived from the report's own timestamp, so writing it as an edge re-dates a
// finish the Stop before it already dated and re-badges every enrolled device,
// once per turn, forever. An edge here is not a missing optimisation, it is a
// notification storm with a clock on it.
func TestTheRepairsAreExactlyThese(t *testing.T) {
	want := map[string]bool{
		"claude/Notification(idle_prompt)":                true,
		"claude/Notification(quota_auto_resume_disabled)": true,
	}
	for _, m := range allMappings() {
		if got := m.kind == kindReassertion; got != want[m.name] {
			t.Errorf("%s has kind reassertion = %v, want %v", m.name, got, want[m.name])
		}
	}
}

// The three agents' turn events, from the recorded fixtures rather than from
// the design's prose. Every file under testdata/hooks appears here exactly
// once, and fixtureSweepIsComplete keeps it that way.
func TestEventMappings(t *testing.T) {
	for _, tc := range eventFixtures {
		t.Run(tc.fixture, func(t *testing.T) {
			payload, err := os.ReadFile(filepath.Join("testdata", "hooks", tc.fixture))
			if err != nil {
				t.Fatal(err)
			}
			got, writes := reportOne(t, []string{"--agent", tc.agent, "--event", tc.event}, string(payload))
			if tc.state == "" {
				if writes != 0 {
					t.Fatalf("%s/%s on %s wrote %q; want no write at all", tc.agent, tc.event, tc.fixture, got)
				}
				return
			}
			if writes != 1 || got != tc.state {
				t.Fatalf("%s/%s on %s wrote %q in %d tmux command(s), want %q in exactly 1",
					tc.agent, tc.event, tc.fixture, got, writes, tc.state)
			}
		})
	}
}

// A fixture nobody mapped is a fixture nobody looked at. Task 12 landed 35 of
// them and the table above has to account for all of them -- including the ones
// whose answer is "nothing", which are the interesting half.
func TestFixtureSweepIsComplete(t *testing.T) {
	covered := map[string]bool{}
	for _, tc := range eventFixtures {
		if covered[tc.fixture] {
			t.Errorf("%s is in the table twice", tc.fixture)
		}
		covered[tc.fixture] = true
	}
	for _, agent := range []string{"claude", "opencode", "pi"} {
		entries, err := os.ReadDir(filepath.Join("testdata", "hooks", agent))
		if err != nil {
			t.Fatal(err)
		}
		seen := 0
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			seen++
			if name := agent + "/" + e.Name(); !covered[name] {
				t.Errorf("%s is a recorded payload no row in TestEventMappings accounts for", name)
			}
		}
		if seen == 0 {
			t.Errorf("no fixtures found for %s", agent)
		}
	}
}

// eventFixtures is TestEventMappings' table, separate so the completeness test
// can walk it too.
//
// state "" means the event writes nothing: either the table ignores it, or the
// agent's integration does not report it at all.
var eventFixtures = []struct {
	fixture, agent, event, state string
}{
	// -- claude ---------------------------------------------------------
	{"claude/user_prompt_submit.json", "claude", "UserPromptSubmit", tmux.StateWorking},
	{"claude/pre_tool_use_bash.json", "claude", "PreToolUse", tmux.StateWorking},
	{"claude/pre_tool_use_write.json", "claude", "PreToolUse", tmux.StateWorking},
	{"claude/pre_tool_use_agent.json", "claude", "PreToolUse", tmux.StateWorking},
	// Task 15 adds the agent_id filter over PreToolUse, Notification and
	// UserPromptSubmit. Until it lands the table alone decides, and a
	// subagent's PreToolUse says exactly what the root's does. The row is here
	// so that Task 15 changing it is a visible edit rather than a silent one.
	{"claude/pre_tool_use_subagent_bash.json", "claude", "PreToolUse", tmux.StateWorking},
	{"claude/notification_permission_prompt.json", "claude", "Notification", tmux.StateBlocked},
	{"claude/notification_permission_prompt_plan.json", "claude", "Notification", tmux.StateBlocked},
	{"claude/notification_idle_prompt.json", "claude", "Notification", tmux.StateIdle},
	{"claude/stop.json", "claude", "Stop", tmux.StateIdle},
	{"claude/stop_subagent_running.json", "claude", "Stop", tmux.StateIdle},
	// Structural, not a filter: SubagentStop is a different hook and the
	// installer does not register it, so claude's Stop is root-only against
	// the Task-tool subagent class. If it is ever passed anyway -- by hand, or
	// by a settings.json the user edited -- the table still refuses it.
	{"claude/subagent_stop.json", "claude", "SubagentStop", ""},

	// -- opencode -------------------------------------------------------
	{"opencode/chat_message.json", "opencode", "chat.message", tmux.StateWorking},
	{"opencode/chat_message_child.json", "opencode", "chat.message", tmux.StateWorking},
	{"opencode/session_status_busy.json", "opencode", "session.status", tmux.StateWorking},
	// The turn end is session.idle, and one turn end is enough. session.status
	// idle fires in the same millisecond; reporting both would write the same
	// state twice with two timestamps for no gain.
	{"opencode/session_status_idle.json", "opencode", "session.status", ""},
	{"opencode/tool_execute_before_bash.json", "opencode", "tool.execute.before", tmux.StateWorking},
	{"opencode/tool_execute_before_write.json", "opencode", "tool.execute.before", tmux.StateWorking},
	{"opencode/tool_execute_before_todowrite.json", "opencode", "tool.execute.before", tmux.StateWorking},
	{"opencode/tool_execute_before_task.json", "opencode", "tool.execute.before", tmux.StateWorking},
	{"opencode/todo_updated.json", "opencode", "todo.updated", tmux.StateWorking},
	{"opencode/permission_asked_bash.json", "opencode", "permission.asked", tmux.StateBlocked},
	{"opencode/permission_asked_edit.json", "opencode", "permission.asked", tmux.StateBlocked},
	{"opencode/session_idle_root.json", "opencode", "session.idle", tmux.StateIdle},
	// Task 15's parentID filter is what keeps this one off the pane; it needs
	// the session.created that named the parent, which is plugin state and not
	// something this table can see. Same note as claude's subagent PreToolUse.
	{"opencode/session_idle_child.json", "opencode", "session.idle", tmux.StateIdle},
	// session.created establishes parentage inside the plugin. It is not a
	// state of the pane and the plugin never reports it.
	{"opencode/session_created_root.json", "opencode", "session.created", ""},
	{"opencode/session_created_child.json", "opencode", "session.created", ""},

	// -- pi -------------------------------------------------------------
	{"pi/session_start.json", "pi", "session_start", tmux.StateWorking},
	{"pi/input.json", "pi", "input", tmux.StateWorking},
	{"pi/tool_execution_start_bash.json", "pi", "tool_execution_start", tmux.StateWorking},
	{"pi/tool_execution_start_read.json", "pi", "tool_execution_start", tmux.StateWorking},
	{"pi/tool_execution_start_write.json", "pi", "tool_execution_start", tmux.StateWorking},
	{"pi/tool_execution_start_subagent.json", "pi", "tool_execution_start", tmux.StateWorking},
	{"pi/ui_prompt_start_input.json", "pi", "ui_prompt_start", tmux.StateBlocked},
	// kind "custom" is an extension's own overlay and has no title at all. It
	// still reports blocked: whether the screen really shows pi's selector is
	// evidence rule 2's question, and rule 2 drops what no registered form
	// confirms. That is the adjudication the form field exists to enable.
	{"pi/ui_prompt_start_custom.json", "pi", "ui_prompt_start", tmux.StateBlocked},
	{"pi/agent_settled.json", "pi", "agent_settled", tmux.StateIdle},
}

// An unparseable payload is a silent no-op on a discriminated event, and does
// not stop an undiscriminated one: Stop means the turn ended whatever else is
// on stdin.
func TestAMalformedPayload(t *testing.T) {
	for _, tc := range []struct {
		name, event, stdin, want string
	}{
		{"a discriminated event with junk on stdin", "Notification", "not json at all", ""},
		{"a discriminated event with no stdin", "Notification", "", ""},
		{"a discriminated event with the wrong shape", "Notification", `{"notification_type":{"a":1}}`, ""},
		{"an undiscriminated event with junk on stdin", "Stop", "not json at all", tmux.StateIdle},
		{"an undiscriminated event with no stdin", "Stop", "", tmux.StateIdle},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, writes := reportOne(t, []string{"--agent", "claude", "--event", tc.event}, tc.stdin)
			if tc.want == "" && writes != 0 {
				t.Fatalf("wrote %q, want no write", got)
			}
			if tc.want != "" && got != tc.want {
				t.Fatalf("wrote %q in %d command(s), want %q", got, writes, tc.want)
			}
		})
	}
}

// The manual form does not read stdin, and the table does not touch it. Task 6
// keeps working, including the states the table would never produce from an
// event.
func TestTheManualFormIgnoresTheTable(t *testing.T) {
	rec := &recordingRunner{}
	stdin := &countingReader{Reader: strings.NewReader(`{"notification_type":"permission_prompt"}`)}
	var out, errb bytes.Buffer
	code := runReport([]string{"--state", "idle", "--text", "waiting"}, stdin, &out, &errb,
		mapEnv(map[string]string{"TMUX": "/tmp/sock,7,0", "TMUX_PANE": "%1"}),
		func(string) tmuxRunner { return rec })
	if code != 0 {
		t.Fatalf("exited %d: %s", code, errb.String())
	}
	if stdin.n != 0 {
		t.Errorf("read %d bytes of stdin; the manual form must not read it", stdin.n)
	}
	if len(rec.calls) != 1 {
		t.Fatalf("ran %d tmux commands, want 1", len(rec.calls))
	}
	args := rec.calls[0]
	rep, ok := tmux.ParseReport(args[len(args)-1], time.Now())
	if !ok || rep.State != tmux.StateIdle || rep.Activity != "waiting" {
		t.Fatalf("wrote %+v, %v; want the literal idle/waiting it was given", rep, ok)
	}
}

// -- helpers ----------------------------------------------------------------

// reportOne runs the integration form once against a recording tmux, and
// returns the state written and how many tmux commands it took. A write count
// of zero is the whole point of most of the rows above: "exit 0" alone does not
// distinguish a refusal from a wrong state written in silence.
func reportOne(t *testing.T, args []string, stdin string) (state string, writes int) {
	t.Helper()
	rec := &recordingRunner{}
	var out, errb bytes.Buffer
	code := runReport(args, strings.NewReader(stdin), &out, &errb,
		mapEnv(map[string]string{"TMUX": "/tmp/sock,7,0", "TMUX_PANE": "%1"}),
		func(string) tmuxRunner { return rec })
	if code != 0 {
		t.Fatalf("runReport%v exited %d, want 0 (stderr: %s)", args, code, errb.String())
	}
	if out.Len() != 0 {
		t.Errorf("runReport%v wrote to stdout: %q", args, out.String())
	}
	if len(rec.calls) == 0 {
		return "", 0
	}
	last := rec.calls[len(rec.calls)-1]
	rep, ok := tmux.ParseReport(last[len(last)-1], time.Now())
	if !ok {
		t.Fatalf("runReport%v wrote %q, which is not a readable report", args, last[len(last)-1])
	}
	return rep.State, len(rec.calls)
}

// countingReader records whether stdin was touched at all.
type countingReader struct {
	*strings.Reader
	n int
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.n += n
	return n, err
}
