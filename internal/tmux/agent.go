package tmux

// Agents is the set of pane_current_command values treated as coding agents.
//
// One list gates three things -- whether a pane is captured, whether it gets a
// state, and whether it gets a logo -- so that a pane can never show a badge
// for a state nothing computed, or a logo for something we do not track.
//
// Matching is exact. "claude-helper" is not claude: a prefix match would put a
// state badge on whatever a user happens to name a script.
var Agents = []string{"claude", "opencode", "pi"}

// KnownAgent returns the agent name for a pane command, or "" if it is not one.
func KnownAgent(command string) string {
	for _, a := range Agents {
		if command == a {
			return a
		}
	}
	return ""
}
