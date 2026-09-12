package report

import (
	"testing"

	"github.com/diegok/tmux-web/internal/tmux"
)

// The shipped table passes its own checks.
//
// checkRules runs in init and panics, so on the shipped data this test can
// never fail: the package would not load. It is here so that the positive case
// is STATED -- a checker only ever exercised on drifted inputs proves nothing
// about the table it guards -- and so that the failure, if the panic is ever
// weakened to a log, is an assertion rather than a silence.
func TestTheShippedTablePassesItsOwnChecks(t *testing.T) {
	if err := checkRules(rules, tmux.Agents); err != nil {
		t.Fatal(err)
	}
}

// Every way these lists have drifted, or could, handed to the checker.
//
// THIS IS THE TEST THAT MATTERS, because the checker is the only thing standing
// between a drifted table and a shipped binary, and a checker that accepted
// everything would pass the test above. Each row is a table that is wrong in
// exactly one way; the shipped table is the row above.
//
// The two form rows are the ones the old test could not write. The check used
// to be tmux.RegisteredForm, a membership test over EVERY agent's forms at
// once, so a pi mapping naming claude's form passed it -- and rule 2, which
// asks only the pane's own agent's grammars about the pane's screen, would
// delete that badge about 4.5 seconds after a client connected, while the agent
// was still waiting.
func TestCheckRulesRefusesADriftedTable(t *testing.T) {
	agents := []string{"claude", "pi"}
	working := Mapping{name: "claude/start", state: tmux.StateWorking, kind: kindEdge}
	piStart := Mapping{name: "pi/start", state: tmux.StateWorking, kind: kindEdge}
	ok := func() map[string]agentTable {
		return map[string]agentTable{
			"claude": {turnStart: turnStartRef{event: "start"}, events: map[string]eventRule{
				"start": {mapping: working},
			}},
			"pi": {turnStart: turnStartRef{event: "start"}, events: map[string]eventRule{
				"start": {mapping: piStart},
			}},
		}
	}
	// The fixture has to be accepted, or every row below passes for the wrong
	// reason.
	if err := checkRules(ok(), agents); err != nil {
		t.Fatalf("the fixture this test mutates is already refused: %v", err)
	}

	for _, tc := range []struct {
		name string
		// drift returns a table that is wrong in one way.
		drift func() map[string]agentTable
	}{
		{"an agent the daemon knows and this table does not", func() map[string]agentTable {
			table := ok()
			delete(table, "pi")
			return table
		}},
		{"an agent this table maps and the daemon never derives", func() map[string]agentTable {
			table := ok()
			table["opencode"] = table["claude"]
			return table
		}},
		{"a blocked mapping naming ANOTHER agent's form", func() map[string]agentTable {
			table := ok()
			table["pi"].events["ask"] = eventRule{mapping: Mapping{name: "pi/ask",
				state: tmux.StateBlocked, kind: kindEdge, form: tmux.ClaudePermissionForm}}
			return table
		}},
		{"a blocked mapping naming no form at all", func() map[string]agentTable {
			table := ok()
			table["pi"].events["ask"] = eventRule{mapping: Mapping{name: "pi/ask",
				state: tmux.StateBlocked, kind: kindEdge}}
			return table
		}},
		{"a mapping that does not rest carrying a form", func() map[string]agentTable {
			table := ok()
			table["pi"].events["work"] = eventRule{mapping: Mapping{name: "pi/work",
				state: tmux.StateWorking, kind: kindEdge, form: tmux.PiSelectorForm}}
			return table
		}},
		{"a blocked mapping inside a DISCRIMINATED table", func() map[string]agentTable {
			table := ok()
			table["pi"].events["ask"] = eventRule{
				discriminate: func([]byte) string { return "x" },
				byValue: map[string]Mapping{"x": {name: "pi/ask(x)",
					state: tmux.StateBlocked, kind: kindEdge, form: tmux.OpencodePermissionForm}},
			}
			return table
		}},
		{"a turn start naming an event the agent does not have", func() map[string]agentTable {
			table := ok()
			table["pi"] = agentTable{turnStart: turnStartRef{event: "typo"}, events: table["pi"].events}
			return table
		}},
		{"a turn start that does not write working", func() map[string]agentTable {
			table := ok()
			table["pi"].events["start"] = eventRule{mapping: Mapping{name: "pi/start",
				state: tmux.StateIdle, kind: kindEdge}}
			return table
		}},
		{"a turn start that is a re-assertion rather than an edge", func() map[string]agentTable {
			table := ok()
			table["pi"].events["start"] = eventRule{mapping: Mapping{name: "pi/start",
				state: tmux.StateWorking, kind: kindReassertion}}
			return table
		}},
		{"a turn start whose discriminator value is on no whitelist", func() map[string]agentTable {
			table := ok()
			table["pi"] = agentTable{
				turnStart: turnStartRef{event: "start", value: "busy"},
				events: map[string]eventRule{"start": {
					discriminate: func([]byte) string { return "" },
					byValue:      map[string]Mapping{"other": piStart},
				}},
			}
			return table
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := checkRules(tc.drift(), agents); err == nil {
				t.Error("checkRules accepted it. Every list this checker holds together is one " +
					"somebody has to keep in step by hand otherwise, and the failure is silent: " +
					"a badge nothing can confirm, an event nothing can reach, or a turn end that " +
					"suppresses itself")
			}
		})
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
		// opencode's TURN END, which reads like an edge and is not one.
		// MEASURED on opencode 1.18.30: on a turn that died at the provider,
		// session.idle fired TWICE inside one resting period -- idle,
		// message.updated, idle, about a second apart, with no busy between
		// them. An event that can recur inside one resting period and writes a
		// resting state is a re-assertion by the criterion, and the name of
		// the event has no vote.
		"opencode/session.idle": true,
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
//
// Since Task 15 it asserts the ACTIVITY TEXT of every one of them as well, in
// the same table, because the two answers are one decision: a fixture's row
// says both what state that payload puts on somebody's pane and what its one
// line of sidebar then reads. Splitting them into two tables would let a new
// fixture be accounted for by one and not the other.

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
	for _, agent := range tmux.Agents {
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
		if got := reassertsFor(Mapping{state: tc.state}); got != tc.want {
			t.Errorf("an unclassified mapping writing %q reasserts = %v, want %v", tc.state, got, tc.want)
		}
	}
	// And an explicit kind overrides the default in both directions.
	if reassertsFor(Mapping{state: tmux.StateIdle, kind: kindEdge}) {
		t.Error("an explicit edge writing idle reasserts; the turn-end events are exactly that case")
	}
	if !reassertsFor(Mapping{state: tmux.StateWorking, kind: kindReassertion}) {
		t.Error("an explicit re-assertion writing working does not reassert")
	}
}
