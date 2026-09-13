package tmux

import (
	"fmt"
	"slices"
)

// What "resume here" runs, per agent.
//
// One table, in Go, beside the agent list, for the reason internal/report's
// event table gives at length: an agent named in three places is an agent that
// ends up spelled differently in two of them. Its keys are checked against
// Agents at load, so a fourth agent is a binary that will not start until this
// file has heard of it -- rather than a menu entry that silently never appears.
//
// EVERY ENTRY IS A FIXED ARGV WHOSE FIRST WORD IS THE AGENT'S OWN NAME, and
// that is checked at load too. Nothing a browser sends reaches this map except
// as a key; a key that is not here is refused before tmux is spoken to. So the
// complete set of programs this daemon can start on the owner's machine
// through resume is the three the sidebar already draws marks for. That is the
// property `ResumeAgent` exists to make structural instead of remembered, and
// it is why NewWindow was not given a command parameter.
//
// MEASURED, NOT READ OFF A WEBSITE. Each flag below was established by running
// the agent's own help on this machine, 2026-09-13:
//
//   - claude --resume -- `claude --help`: "-r, --resume [value]  Resume a
//     conversation by session ID, or ...". With no value it opens Claude's own
//     picker.
//   - pi --resume -- `pi --help` (pi 0.85.1): "--resume, -r  Select a session
//     to resume". Run in a throwaway directory with a throwaway
//     PI_CODING_AGENT_DIR it printed pi's own "Resume Session (Current
//     Folder)" list, so it is a picker and it is already scoped to the
//     directory the window opens in.
//   - opencode --continue -- `opencode --help`: "-c, --continue  continue the
//     last session", plus "-s, --session  session id to continue". THERE IS NO
//     PICKER. opencode takes the most recent session in the directory with no
//     choice offered, and its `session` subcommand only lists and deletes, so
//     -s is out of reach without a session list this design does not have. The
//     menu wording is held to that by a test, because a control that promises
//     a list the agent will not show is worse than no control.
var resumeCommands = map[string][]string{
	"claude":   {"claude", "--resume"},
	"opencode": {"opencode", "--continue"},
	"pi":       {"pi", "--resume"},
}

// A panic rather than a log, and at load rather than on the first request: the
// failures checkResume catches are all "somebody edited one list and not the
// other", and the moment to find that out is the build, not the evening the
// owner right-clicks a pi pane from a phone.
func init() {
	if err := checkResume(resumeCommands, Agents); err != nil {
		panic("internal/tmux: " + err.Error())
	}
}

// checkResume holds the resume table to the agent list, and to its own rules.
//
// Both directions, because they fail differently. An agent with no entry is a
// menu entry the daemon then refuses -- a control that does nothing. An entry
// for something that is not an agent is worse: it is a program this daemon can
// be asked by name to start, sitting outside the one list that gates capture,
// state and logo together.
func checkResume(table map[string][]string, agents []string) error {
	for _, agent := range agents {
		argv, ok := table[agent]
		if !ok {
			return fmt.Errorf("agent %q has no resume command: it would be offered a "+
				"menu entry the daemon then refuses", agent)
		}
		if len(argv) == 0 {
			return fmt.Errorf("agent %q has an empty resume command: new-window with no "+
				"command opens a shell and would call that a resume", agent)
		}
		if argv[0] != agent {
			return fmt.Errorf("%q's resume command runs %q: the table may only run the "+
				"agent itself, which is what bounds what resume can start", agent, argv[0])
		}
		// The key is also the window name ResumeAgent passes to -n.
		if err := ValidateWindowName(agent); err != nil {
			return fmt.Errorf("agent %q cannot name a window: %w", agent, err)
		}
	}
	for agent := range table {
		if !slices.Contains(agents, agent) {
			return fmt.Errorf("%q has a resume command but is not in Agents: a program "+
				"reachable by name that nothing else in this app treats as an agent", agent)
		}
	}
	return nil
}
