package main

import (
	"bytes"
	"encoding/json"
	"io"
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
// It is the decision that matters most in the table: idle_prompt fires about 60
// SECONDS AFTER EVERY TURN, and finishedAt is derived from the report's own
// timestamp, so writing it as an edge re-dates a finish the Stop before it
// already dated and re-badges every enrolled device, once per turn, forever. An
// edge here is not a missing optimisation, it is a notification storm with a
// clock on it.
//
// TestEdgeAndReassertion drives the behaviour. This one is the roll call: a
// mapping that quietly changes kind changes how often somebody's phone lights
// up, so it has to be a deliberate edit here, next to the reason.
func TestTheRepairsAreExactlyThese(t *testing.T) {
	want := map[string]bool{
		"claude/Notification(idle_prompt)":                true,
		"claude/Notification(quota_auto_resume_disabled)": true,
		// A pi extension reload re-derives state mid-run. The reload can
		// recur arbitrarily often inside one resting period, which is the
		// criterion, and revision 3 of the design classified this event
		// nowhere at all.
		"pi/session_start(idle)": true,
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
	rec := &recordingTmux{}
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

// The split itself, asserted on the TMUX CALLS MADE rather than only on the
// value that ends up stored: "no write" and "a write of the same state under a
// newer timestamp" store values that look alike and badge differently.
//
// finishedAt is derived statelessly from a resting report's own timestamp, and
// a browser's `seen` marker stores the value it was SHOWN rather than the time
// it looked, so a second idle one minute after the first is a second finish as
// far as every enrolled device is concerned.
func TestEdgeAndReassertion(t *testing.T) {
	// A standing report from a minute ago. Written out rather than built with
	// FormatReport: this is what tmux is holding, and a fixture built by the
	// writer under test could only ever agree with it.
	const standingIdle = "1;idle;1789075200000"
	const standingWorking = "1;working;1789075200000"

	t.Run("an edge writes without reading", func(t *testing.T) {
		// Stop, on a pane already reporting idle -- the stale report of the
		// PREVIOUS turn. It must write anyway: what makes a turn end an edge
		// is the turn-start invariant, and a Stop that stayed silent here
		// would lose this turn's badge.
		r := &recordingTmux{standing: standingIdle}
		runReportWith(t, r, []string{"--agent", "claude", "--event", "Stop"})
		if r.shows != 0 {
			t.Errorf("an edge read the standing option %d times, want 0", r.shows)
		}
		if r.sets != 1 {
			t.Errorf("an edge made %d writes, want 1", r.sets)
		}
	})

	t.Run("a re-assertion reads first and stays silent when it agrees", func(t *testing.T) {
		r := &recordingTmux{standing: standingIdle}
		runReportWith(t, r, []string{"--agent", "claude", "--event", "Notification"},
			withStdin(`{"notification_type":"idle_prompt"}`))
		if r.shows != 1 || r.sets != 0 {
			t.Errorf("shows=%d sets=%d, want 1 and 0: a re-assertion that agrees "+
				"must write NOTHING -- a write of the same state with a newer "+
				"timestamp re-badges every device that had already looked", r.shows, r.sets)
		}
	})

	t.Run("a re-assertion writes when it disagrees, and that repair is the point", func(t *testing.T) {
		// A Stop that never landed -- the attested reason herdr gave up on
		// Claude hooks. The standing report is working; idle_prompt sees the
		// disagreement and repairs the pane from the agent itself.
		r := &recordingTmux{standing: standingWorking}
		runReportWith(t, r, []string{"--agent", "claude", "--event", "Notification"},
			withStdin(`{"notification_type":"idle_prompt"}`))
		if r.sets != 1 {
			t.Errorf("sets=%d, want 1: the repair is what keeps this event mapped at all", r.sets)
		}
		if got := wroteState(t, r); got != tmux.StateIdle {
			t.Errorf("the repair wrote %q, want idle", got)
		}
	})

	t.Run("an unreadable standing value counts as a disagreement", func(t *testing.T) {
		// Every way of having no usable answer: a value from a schema this
		// daemon does not know, a truncated one, and the unset option -- which
		// is what a real tmux read returns for a pane nobody has reported on,
		// since `show-options -v` on an unset user option exits 1 with
		// "invalid option" (measured on 3.7b) and Run answers "" to that.
		//
		// All of them must WRITE. Reading them as agreement would suppress the
		// first report a freshly started agent ever makes.
		for _, standing := range []string{"", "garbage", "2;idle;1789075200000", "1;idle", "1;nonsense;1789075200000"} {
			r := &recordingTmux{standing: standing}
			runReportWith(t, r, []string{"--agent", "claude", "--event", "Notification"},
				withStdin(`{"notification_type":"idle_prompt"}`))
			if r.shows != 1 || r.sets != 1 {
				t.Errorf("standing %q: shows=%d sets=%d, want 1 and 1", standing, r.shows, r.sets)
			}
		}
	})

	t.Run("a re-assertion compares the state and not the whole value", func(t *testing.T) {
		// Same state, different activity text and a different timestamp. It
		// still agrees: the criterion is the STATE, and a resting report's
		// text is not what badges a device.
		r := &recordingTmux{standing: "1;idle;1789075200001;wrote the poem"}
		runReportWith(t, r, []string{"--agent", "claude", "--event", "Notification"},
			withStdin(`{"notification_type":"quota_auto_resume_disabled"}`))
		if r.sets != 0 {
			t.Errorf("sets=%d, want 0", r.sets)
		}
	})

	// Three rows carry more weight than the rest.

	t.Run("pi session_start's idle branch reads before writing", func(t *testing.T) {
		// A pi extension reload re-derives state from ctx.isIdle(), and a
		// reload can happen any number of times while the agent sits idle. As
		// an edge, every reload on an idle pane would write idle;<now> and
		// re-badge every device.
		r := &recordingTmux{standing: standingIdle}
		runReportWith(t, r, []string{"--agent", "pi", "--event", "session_start"},
			withStdin(`{"type":"session_start","reason":"startup","wterm_is_idle":true}`))
		if r.shows != 1 || r.sets != 0 {
			t.Errorf("shows=%d sets=%d, want 1 and 0", r.shows, r.sets)
		}
		// And it repairs, like every other re-assertion: a reload while the
		// pane carries a stale working is how a pi that crashed mid-turn gets
		// its pane back.
		r = &recordingTmux{standing: standingWorking}
		runReportWith(t, r, []string{"--agent", "pi", "--event", "session_start"},
			withStdin(`{"type":"session_start","reason":"startup","wterm_is_idle":true}`))
		if r.sets != 1 {
			t.Errorf("over a standing working: sets=%d, want 1", r.sets)
		}
		if got := wroteState(t, r); got != tmux.StateIdle {
			t.Errorf("wrote %q, want idle", got)
		}
	})

	t.Run("pi session_start's working branch does not", func(t *testing.T) {
		// The same event, the other branch. working is transient and cannot
		// badge -- the worst a redundant one does is refresh the expiry, which
		// is what the keepalive wants -- so it pays no read.
		for _, stdin := range []string{
			`{"type":"session_start","reason":"startup","wterm_is_idle":false}`,
			// The flag missing altogether: an integration that has not been
			// updated, or one whose ctx read failed. It must fall to working,
			// the state that expires on its own, and not to idle, which rests
			// forever.
			`{"type":"session_start","reason":"startup"}`,
			`not json at all`,
			`{"type":"session_start","wterm_is_idle":"yes"}`,
		} {
			r := &recordingTmux{standing: standingIdle}
			runReportWith(t, r, []string{"--agent", "pi", "--event", "session_start"}, withStdin(stdin))
			if r.shows != 0 || r.sets != 1 {
				t.Errorf("stdin %q: shows=%d sets=%d, want 0 and 1", stdin, r.shows, r.sets)
			}
			if got := wroteState(t, r); got != tmux.StateWorking {
				t.Errorf("stdin %q wrote %q, want working", stdin, got)
			}
		}
	})

	t.Run("a turn-end event writes unconditionally", func(t *testing.T) {
		// All three, each against a STALE standing idle left by the previous
		// turn. As re-assertions they would all be silent here and every turn
		// after the first would lose its badge.
		for _, tc := range []struct{ agent, event, stdin string }{
			{"claude", "Stop", `{"hook_event_name":"Stop"}`},
			{"pi", "agent_settled", `{"type":"agent_settled"}`},
			{"opencode", "session.idle", `{"type":"session.idle","properties":{"sessionID":"ses_1"}}`},
		} {
			r := &recordingTmux{standing: standingIdle}
			runReportWith(t, r, []string{"--agent", tc.agent, "--event", tc.event}, withStdin(tc.stdin))
			if r.shows != 0 || r.sets != 1 {
				t.Errorf("%s/%s: shows=%d sets=%d, want 0 and 1", tc.agent, tc.event, r.shows, r.sets)
			}
		}
	})

	t.Run("the hot hooks pay nothing", func(t *testing.T) {
		// Fork volume under a burst is open question 1 and it is about exactly
		// this number. PreToolUse fires on every tool call; opencode's busy
		// fired 17 times in one three-tool turn. Neither may read.
		for _, tc := range []struct{ agent, event, stdin string }{
			{"claude", "PreToolUse", `{"tool_name":"Bash"}`},
			{"claude", "UserPromptSubmit", `{"prompt":"hi"}`},
			{"opencode", "session.status", `{"properties":{"status":{"type":"busy"}}}`},
			{"opencode", "tool.execute.before", `{"input":{"tool":"bash"}}`},
			{"pi", "input", `{"type":"input","text":"hi"}`},
			{"pi", "tool_execution_start", `{"type":"tool_execution_start"}`},
		} {
			r := &recordingTmux{standing: "1;working;1789075200000"}
			runReportWith(t, r, []string{"--agent", tc.agent, "--event", tc.event}, withStdin(tc.stdin))
			if r.shows != 0 {
				t.Errorf("%s/%s read the standing option %d times; it is on the hot path and pays nothing",
					tc.agent, tc.event, r.shows)
			}
			if r.sets != 1 {
				t.Errorf("%s/%s made %d writes, want 1: a working report is also the keepalive",
					tc.agent, tc.event, r.sets)
			}
		}
	})

	t.Run("the manual form is always an edge", func(t *testing.T) {
		// A person typing the command means it. There is no table entry to
		// carry a kind, and inferring one from the state would make a scripted
		// `report --state idle` silently do nothing.
		r := &recordingTmux{standing: standingIdle}
		runReportWith(t, r, []string{"--state", "idle"})
		if r.shows != 0 || r.sets != 1 {
			t.Errorf("shows=%d sets=%d, want 0 and 1", r.shows, r.sets)
		}
	})
}

// The invariant that makes the turn-end rows above safe, and it needs its own
// test because it is a property OF THE TABLE rather than of any one call: a
// turn that could reach its end with no working written before it would have
// that end suppressed and lose the badge.
//
// This is the reader-side rule that rests on writer-side behaviour, so it is
// asserted where the writer's table lives. It cannot be asserted at all in the
// daemon, which sees two identical `1;idle;<ts>` values and cannot tell a
// genuine finish from a re-assertion.
func TestEveryAgentHasATurnStartWorkingEdge(t *testing.T) {
	for _, agent := range []string{"claude", "opencode", "pi"} {
		m, ok := turnStart(agent)
		if !ok || m.state != tmux.StateWorking || m.kind != kindEdge {
			t.Errorf("%s has no turn-start working edge (%+v, ok=%v): its turn-end event "+
				"is only an edge because one exists", agent, m, ok)
		}
	}
}

// Every mapping that writes a state says which kind it is, in the table, next
// to the reason.
//
// The zero value is deliberately NOT edge, and this is why: revision 3 of the
// design carried pi's session_start without classifying it at all, and a table
// whose unclassified default is "edge" turns that omission into a badge storm
// nobody wrote down. The default the design ships instead -- unknown cardinality
// plus a resting state means re-assertion -- lives in reassertsFor, and this
// test is what keeps the table from relying on it: a new entry has to be
// classified here rather than inherit a decision.
func TestEveryMappingIsClassified(t *testing.T) {
	seen := 0
	for _, m := range allMappings() {
		if m.state == "" {
			// An ignored event writes nothing, so there is no write to
			// classify. Carrying a kind here would be as meaningless as
			// carrying a form.
			if m.kind != kindUnclassified {
				t.Errorf("%s writes nothing but carries kind %v; a kind describes a write", m.name, m.kind)
			}
			continue
		}
		seen++
		if m.kind == kindUnclassified {
			t.Errorf("%s writes %q and nobody said whether that is an edge or a re-assertion. "+
				"The criterion: a write is a re-assertion if the state is a resting one AND the event "+
				"can fire more than once inside one resting period; if its cardinality is unknown and "+
				"the state is resting, it is a re-assertion", m.name, m.state)
		}
	}
	if seen == 0 {
		t.Fatal("no mapping writes a state at all; this test then proves nothing")
	}
}

// The default, which no table row can exercise because the test above forbids
// an unclassified row from existing.
//
// It is the same asymmetry that settles the unknown-notification_type default:
// an unnecessary read costs one fork on a path nobody is waiting on, and a
// missing one costs a badge storm.
func TestTheUnclassifiedDefault(t *testing.T) {
	for _, tc := range []struct {
		state string
		want  bool
	}{
		{tmux.StateIdle, true},
		{tmux.StateBlocked, true},
		// working is transient and cannot badge, and suppressing a redundant
		// one would cost the keepalive its expiry refresh.
		{tmux.StateWorking, false},
	} {
		if got := reassertsFor(mapping{state: tc.state}); got != tc.want {
			t.Errorf("an unclassified mapping writing %q reasserts = %v, want %v", tc.state, got, tc.want)
		}
	}
	// And an explicit kind overrides the default in both directions.
	if reassertsFor(mapping{state: tmux.StateIdle, kind: kindEdge}) {
		t.Error("an explicit edge writing idle reasserts; the turn-end events are exactly that case")
	}
	if !reassertsFor(mapping{state: tmux.StateWorking, kind: kindReassertion}) {
		t.Error("an explicit re-assertion writing working does not reassert")
	}
}

// -- helpers ----------------------------------------------------------------

// reportOne runs the integration form once against a recording tmux, and
// returns the state written and how many WRITES it took. A write count of zero
// is the whole point of most of the rows above: "exit 0" alone does not
// distinguish a refusal from a wrong state written in silence.
//
// Reads are deliberately not counted here. Since Task 14 a re-assertion makes
// one before it decides, and folding it into this number would make "wrote
// nothing" and "read, then wrote" indistinguishable in exactly the tests whose
// subject is whether anything was written. TestEdgeAndReassertion is where the
// reads are the subject, and it counts the two separately.
//
// The standing option is left unset, which is the disagreeing answer, so every
// mapping in the table above writes what it would write as an edge. That is the
// right fixture for a table test: it isolates what the table says from what the
// pane happened to be carrying.
func reportOne(t *testing.T, args []string, stdin string) (state string, writes int) {
	t.Helper()
	rec := &recordingTmux{}
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
	last := rec.lastSet()
	if last == nil {
		return "", 0
	}
	rep, ok := tmux.ParseReport(last[len(last)-1], time.Now())
	if !ok {
		t.Fatalf("runReport%v wrote %q, which is not a readable report", args, last[len(last)-1])
	}
	return rep.State, rec.sets
}

// runReportWith runs one report against a recording tmux and asserts only that
// it exited 0. What each caller asserts is the CALL LOG, which is the only
// thing that separates "wrote nothing" from "wrote the same state again".
func runReportWith(t *testing.T, r *recordingTmux, args []string, opts ...reportOption) {
	t.Helper()
	call := reportCall{}
	for _, o := range opts {
		o(&call)
	}
	var errb bytes.Buffer
	code := runReport(args, strings.NewReader(call.stdin), io.Discard, &errb,
		mapEnv(map[string]string{"TMUX": "/tmp/sock,7,0", "TMUX_PANE": "%1"}),
		func(string) tmuxRunner { return r })
	if code != 0 {
		t.Fatalf("runReport%v exited %d, want 0 (stderr: %s)", args, code, errb.String())
	}
}

type reportCall struct{ stdin string }

// reportOption is one adjustment to a runReportWith call. Only the payload
// varies today; it is an option rather than a parameter so that the calls whose
// subject is the table -- the manual form, an undiscriminated event -- do not
// carry an empty string that means nothing.
type reportOption func(*reportCall)

func withStdin(s string) reportOption { return func(c *reportCall) { c.stdin = s } }

// wroteState is the state of the last report written, for the rows where
// "something was written" is not the whole claim.
func wroteState(t *testing.T, r *recordingTmux) string {
	t.Helper()
	last := r.lastSet()
	if last == nil {
		t.Fatal("nothing was written")
	}
	rep, ok := tmux.ParseReport(last[len(last)-1], time.Now())
	if !ok {
		t.Fatalf("wrote %q, which is not a readable report", last[len(last)-1])
	}
	return rep.State
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
