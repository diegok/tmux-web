package tmux

import "testing"

func TestKnownAgent(t *testing.T) {
	for _, tc := range []struct {
		cmd  string
		want string
	}{
		{"claude", "claude"},
		{"opencode", "opencode"},
		{"pi", "pi"},
		// Everything the developer actually runs alongside agents. A shell is
		// not idle or working; it is a shell, and a state badge on it is a lie.
		{"zsh", ""},
		{"bash", ""},
		{"nvim", ""},
		{"go", ""},
		{"", ""},
		// Case and paths: pane_current_command is a bare name, but be explicit
		// that we do not do prefix or substring matching -- "claude-helper"
		// is not claude.
		{"claude-helper", ""},
		{"CLAUDE", ""},
	} {
		if got := KnownAgent(tc.cmd); got != tc.want {
			t.Errorf("KnownAgent(%q) = %q, want %q", tc.cmd, got, tc.want)
		}
	}
}
