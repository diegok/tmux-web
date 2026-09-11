package main

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/diegok/tmux-web/internal/integrations"
)

// Claude's integration is split across two packages that cannot see each other,
// and this file is the seam.
//
// internal/integrations owns WHICH HOOKS ARE REGISTERED -- it generates the
// settings.json block -- and it cannot import package main, so nothing over
// there can tell whether the names it registers are names anything maps.
// events.go owns WHAT AN EVENT MEANS and never learns which of its rows a user
// actually has installed. Between the two sits a mutant neither side can see
// and the plan's own mutation table does not name: a hook registered under a
// name events.go does not know.
//
// It is the quietest failure in this whole feature. The settings block is still
// four hooks, every one still `"async": true`, still no asyncRewake and still no
// SubagentStop -- every assertion in claude_hooks_test.go passes. The hook still
// fires, the wrapper still runs, `report` still exits 0. It just prints
// `nothing known about "claude"'s "PreToolCall" event` to a stderr nobody reads
// and writes no state at all. The pi and opencode equivalents cannot happen
// quietly, because their wiring tests drive a REAL runtime and watch what it
// spawns; Claude has no such test, because driving a real Claude Code needs the
// owner's credentials and spends the owner's quota.
//
// So the two lists are held against each other here, in the only package that
// can see both.

// TestTheRegisteredClaudeHooksAreExactlyTheEventsTheTableKnows binds the
// installer's hook set to events.go's claude table, in both directions.
//
// Both directions, because each is a different defect:
//
//   - A registered hook with no mapping fires into nothing. That is the quiet
//     one above.
//   - A mapped event nobody registers is a row that can never run. Harmless in
//     itself, and a reliable sign that somebody added an event to the table and
//     forgot that Claude has no runtime to discover it -- unlike pi and
//     opencode, where the integration subscribes by name, Claude only ever
//     sends what settings.json asked for.
func TestTheRegisteredClaudeHooksAreExactlyTheEventsTheTableKnows(t *testing.T) {
	registered := append([]string(nil), integrations.ClaudeHookEvents...)
	sort.Strings(registered)

	mapped := make([]string, 0, len(eventRules["claude"]))
	for event := range eventRules["claude"] {
		mapped = append(mapped, event)
	}
	sort.Strings(mapped)

	if strings.Join(registered, " ") != strings.Join(mapped, " ") {
		t.Errorf("the installer registers %v; events.go maps %v.\n"+
			"A registered hook events.go does not map fires into nothing -- `report` prints one line to a stderr nobody reads and writes no state -- and every assertion in internal/integrations/claude_hooks_test.go passes while it does.\n"+
			"A mapped event nobody registers can never fire: Claude sends only what settings.json asked for.",
			registered, mapped)
	}
}

// TestSubagentStopIsInNeitherHalf is the structural guard, asserted from the
// side that can see both halves of it.
//
// It is worth its own test rather than a line in the one above, because it is
// the one name whose absence is a SAFETY property and not merely a consistency
// one. Claude's subagent payloads are distinguished by `agent_id`, which is
// ABSENCE-coded: a root payload legitimately lacks it, so the payload filter
// fails OPEN, and anything it cannot see is a subagent's turn end writing idle
// onto a pane whose root agent is still working. Not registering the hook is
// the half of this that fails closed; leaving it out of the table is the half
// that survives a hand-edited settings.json.
func TestSubagentStopIsInNeitherHalf(t *testing.T) {
	for _, event := range integrations.ClaudeHookEvents {
		if event == "SubagentStop" {
			t.Error("the installer registers SubagentStop. Not registering it is the whole of the structural guard against the Task-tool subagent class: agent_id is absence-coded, so the payload filter fails open")
		}
	}
	if _, ok := eventRules["claude"]["SubagentStop"]; ok {
		t.Error("events.go maps SubagentStop. Its absence is what makes even a hand-edited settings.json that registered the hook write nothing")
	}
	// And the recorded payload proves the fixture is a real one rather than a
	// name nobody ever saw: a SubagentStop that DID arrive is refused twice
	// over -- once for the unknown event, once for the agent_id it carries.
	m, known := lookupMapping("claude", "SubagentStop", []byte(readFixture(t, "claude/subagent_stop.json")))
	if known {
		t.Errorf("lookupMapping knows claude/SubagentStop: %+v", m)
	}
}

// TestTheGeneratedBlockNamesTheEventArgumentTheWrapperPassesOn follows one
// event name the whole way: out of the generated command line, through the
// wrapper's "$1", into `report`'s --event, and into the table.
//
// The three hops are in three packages and each one is separately asserted
// somewhere; what nobody asserts is that they are the SAME STRING. A settings
// block that put the event somewhere other than the last argument -- before the
// script path, say, or joined to it -- would still be four async hooks with the
// right names in the right places.
func TestTheGeneratedBlockNamesTheEventArgumentTheWrapperPassesOn(t *testing.T) {
	var block struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(integrations.ClaudeHookBlock("/opt/wterm/report.sh"), &block); err != nil {
		t.Fatalf("decoding the generated block: %v", err)
	}
	for event, matchers := range block.Hooks {
		// The wrapper is `exec "$BIN" report --agent claude --event "$1"`, so
		// the event is whatever the LAST word of the command line is.
		fields := strings.Fields(matchers[0].Hooks[0].Command)
		arg := fields[len(fields)-1]
		if arg != event {
			t.Errorf("the %s hook's command ends in %q: the wrapper passes its $1 on as --event, so that word is what reaches the table", event, arg)
			continue
		}
		if _, known := lookupMapping("claude", arg, []byte(`{}`)); !known {
			t.Errorf("the %s hook would send --event %q, which events.go does not map", event, arg)
		}
	}
}
