package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/diegok/tmux-web/internal/integrations"
	"github.com/diegok/tmux-web/internal/report"
)

// Claude's integration is split across packages, and this file is what is left
// of the seam once internal/report exists.
//
// internal/integrations owns WHICH HOOKS ARE REGISTERED -- it generates the
// settings.json block -- and internal/report owns WHAT AN EVENT MEANS. Between
// the two sits a mutant neither side can see: a hook registered under a name
// the table does not know. THAT TIE NOW LIVES IN internal/report, which can see
// both halves; it used to live here because package main was the only place
// that could. What is left here is the pair of claims that need a recorded
// payload or the generated command line, which are this package's own.
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
	// The other half, through the recorded payload, which also proves the
	// fixture is a real one rather than a name nobody ever saw: a SubagentStop
	// that DID arrive is refused twice over -- once for the unknown event, once
	// for the agent_id it carries. The table's absence is what makes even a
	// hand-edited settings.json that registered the hook write nothing.
	m, known := report.Lookup("claude", "SubagentStop", []byte(readFixture(t, "claude/subagent_stop.json")))
	if known {
		t.Errorf("the table knows claude/SubagentStop: %+v", m)
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
	if err := json.Unmarshal(integrations.ClaudeHookBlock("/opt/tmux-web/report.sh"), &block); err != nil {
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
		if _, known := report.Lookup("claude", arg, []byte(`{}`)); !known {
			t.Errorf("the %s hook would send --event %q, which internal/report does not map", event, arg)
		}
	}
}
