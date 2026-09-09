package tmux_test

import (
	"context"
	"strings"
	"testing"

	"github.com/diegok/tmux-web/internal/tmux"
	"github.com/diegok/tmux-web/internal/tmux/testutil"
)

func TestEnableHyperlinksAppendsWithoutDiscardingDefaults(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "work")

	before := srv.Run(t, "show", "-s", "terminal-features")
	if !strings.Contains(before, "xterm*") {
		t.Fatalf("expected tmux's own defaults to be present, got %q", before)
	}

	c := tmux.NewClient(srv.Args())
	if err := c.EnableHyperlinks(context.Background()); err != nil {
		t.Fatal(err)
	}

	after := srv.Run(t, "show", "-s", "terminal-features")
	if !strings.Contains(after, tmux.TermName+":hyperlinks") {
		t.Fatalf("hyperlinks not enabled for %s: %q", tmux.TermName, after)
	}
	// A bare `set -s` would replace the list; the defaults must survive.
	for _, def := range []string{"xterm*", "screen*", "rxvt*"} {
		if !strings.Contains(after, def) {
			t.Errorf("appending discarded tmux's own %q entry: %q", def, after)
		}
	}
}

// set -sa does not deduplicate, so a per-attach call would grow the option by
// one entry per browser tab.
func TestEnableHyperlinksIsIdempotent(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "work")
	c := tmux.NewClient(srv.Args())

	for i := 0; i < 5; i++ {
		if err := c.EnableHyperlinks(context.Background()); err != nil {
			t.Fatal(err)
		}
	}

	after := srv.Run(t, "show", "-s", "terminal-features")
	if n := strings.Count(after, tmux.TermName+":hyperlinks"); n != 1 {
		t.Fatalf("feature appears %d times after 5 calls, want 1:\n%s", n, after)
	}
}

// The user's own terminal must not start receiving OSC 8 because of this.
func TestEnableHyperlinksLeavesOtherTerminalsAlone(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "work")
	if err := tmux.NewClient(srv.Args()).EnableHyperlinks(context.Background()); err != nil {
		t.Fatal(err)
	}

	after := srv.Run(t, "show", "-s", "terminal-features")
	for _, line := range strings.Split(after, "\n") {
		if strings.Contains(line, "hyperlinks") && !strings.Contains(line, tmux.TermName) {
			t.Fatalf("hyperlinks enabled for a TERM other than the web client's: %q", line)
		}
	}
}

func TestEnableHyperlinksWithNoServerIsNotAnError(t *testing.T) {
	if err := tmux.NewClient(testutil.NewServer(t).Args()).EnableHyperlinks(context.Background()); err != nil {
		t.Fatalf("EnableHyperlinks on a dead server = %v, want nil", err)
	}
}
