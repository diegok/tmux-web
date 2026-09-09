package tmux

import (
	"strings"
	"testing"
)

func TestAttachArgs(t *testing.T) {
	args := AttachArgs("work", "_web-abc")
	joined := strings.Join(args, " ")

	if strings.Contains(joined, "-d") {
		t.Fatal("must not be detached: -d creates a session with zero clients, " +
			"and destroy-unattached then destroys it before any attach lands")
	}
	for _, want := range []string{
		"new-session", "-t", "work", "-s", "_web-abc",
		"destroy-unattached", "status", "mouse", "@wterm_web",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in %q", want, joined)
		}
	}
	// Every `set` must come after new-session so it runs in the attached
	// client's context.
	if strings.Index(joined, "new-session") > strings.Index(joined, "destroy-unattached") {
		t.Fatal("new-session must come first")
	}
}

// Two tabs opened at the same moment must not land on one session: a colliding
// name makes the second new-session fail outright.
func TestNewSessionNameIsUniqueAndPrefixed(t *testing.T) {
	seen := make(map[string]bool)
	for range 100 {
		name := NewSessionName()
		if !strings.HasPrefix(name, "_web-") {
			t.Fatalf("NewSessionName() = %q, want the _web- prefix", name)
		}
		if seen[name] {
			t.Fatalf("NewSessionName() repeated %q", name)
		}
		seen[name] = true
	}
}
