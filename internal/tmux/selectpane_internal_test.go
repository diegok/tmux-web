package tmux

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/diegok/tmux-web/internal/tmux/testutil"
)

// selectFixture is a base session with three windows and one browser tab
// grouped onto it, parked on window 0 -- so any movement below is the one the
// test asked for.
type selectFixture struct {
	srv   *testutil.Server
	panes []string // one pane per window, index == window index
	wins  []string // the window id of each of those panes
}

func newSelectFixture(t *testing.T) *selectFixture {
	t.Helper()
	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "work", "-x", "80", "-y", "24")
	srv.Run(t, "new-window", "-t", "work")
	srv.Run(t, "new-window", "-t", "work")
	// Window 2 is split, so the read in the chain reports its window id once
	// per pane. A test whose every window held one pane would pass with the
	// answer taken as the whole of stdout rather than its first line.
	srv.Run(t, "split-window", "-t", "=work:2")
	srv.Run(t, "new-session", "-d", "-t", "work", "-s", "_web-a")
	f := &selectFixture{srv: srv}
	for _, w := range []string{"=work:0", "=work:1", "=work:2"} {
		// First line: window 2 lists two panes, and this test wants one of them.
		pane, _, _ := strings.Cut(srv.Run(t, "list-panes", "-t", w, "-F", "#{pane_id}"), "\n")
		window, _, _ := strings.Cut(srv.Run(t, "list-panes", "-t", w, "-F", "#{window_id}"), "\n")
		f.panes = append(f.panes, pane)
		f.wins = append(f.wins, window)
	}
	srv.Run(t, "select-window", "-t", "=_web-a:0")
	return f
}

// window is the window the tab's session is currently on, read back through the
// server rather than through the code under test.
func (f *selectFixture) window(t *testing.T) string {
	t.Helper()
	return f.srv.Run(t, "display-message", "-p", "-t", "=_web-a:", "#{window_id}")
}

// countForks puts a counting shim ahead of tmux on PATH and returns how many
// times the body forked it. The fixture is seeded before it is installed, so
// only the body is counted.
func countForks(t *testing.T, body func()) int {
	t.Helper()
	real, err := exec.LookPath("tmux")
	if err != nil {
		t.Fatalf("tmux not found: %v", err)
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "invocations")
	shim := "#!/bin/sh\necho x >> " + log + "\nexec " + real + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(shim), 0o755); err != nil {
		t.Fatalf("write shim: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	body()
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("the shim was never run, so PATH did not reach the client: %v", err)
	}
	return strings.Count(string(b), "\n")
}

// The claim, measured rather than argued: a sidebar click with the window id
// the snapshot already carries forks tmux ONCE.
//
// Every other assertion about SelectPane stays green if the window id is read
// back from tmux anyway and the answers agree, which is exactly the regression
// this guards: the three-fork version and the one-fork version leave the server
// in the same state. Only the count can tell them apart, and the fork is the
// cost -- measured on this machine, a click cost 10.1-11.3ms as three
// invocations against 3.3-4.2ms as one.
func TestSelectPaneWithAWindowIDForksTmuxOnce(t *testing.T) {
	f := newSelectFixture(t)
	c := NewClient(f.srv.Args())

	got := countForks(t, func() {
		if err := c.SelectPane(context.Background(), "_web-a", f.panes[2], f.wins[2]); err != nil {
			t.Fatalf("SelectPane: %v", err)
		}
	})
	if got != 1 {
		t.Errorf("one click forked tmux %d times, want 1: the read, the select-window and the "+
			"select-pane are one invocation, and a second one costs a fork on every click forever", got)
	}
	if w := f.window(t); w != f.wins[2] {
		t.Errorf("the tab is on window %q, want %q", w, f.wins[2])
	}
}

// Without a window id there is nothing to chain the selects onto, so the read
// is a fork of its own -- but the two selects still share one. Two, not three.
func TestSelectPaneWithoutAWindowIDForksTmuxTwice(t *testing.T) {
	f := newSelectFixture(t)
	c := NewClient(f.srv.Args())

	got := countForks(t, func() {
		if err := c.SelectPane(context.Background(), "_web-a", f.panes[2], ""); err != nil {
			t.Fatalf("SelectPane: %v", err)
		}
	})
	if got != 2 {
		t.Errorf("a click with no window id forked tmux %d times, want 2: the read, then the two "+
			"selects chained into one invocation", got)
	}
	if w := f.window(t); w != f.wins[2] {
		t.Errorf("the tab is on window %q, want %q", w, f.wins[2])
	}
}

// A window id the caller supplies is a HINT, never an authority. It comes from
// a snapshot up to a poll interval old, and a pane that has been moved between
// windows since -- break-pane, join-pane -- would otherwise navigate the tab to
// a window that no longer holds the pane it clicked, silently and with no error
// to notice. The same invocation reads back the pane's real window, so a stale
// hint is caught and corrected instead of believed.
func TestSelectPaneCorrectsAStaleWindowID(t *testing.T) {
	f := newSelectFixture(t)
	c := NewClient(f.srv.Args())

	// The hint names window 1; the pane clicked lives in window 2.
	if err := c.SelectPane(context.Background(), "_web-a", f.panes[2], f.wins[1]); err != nil {
		t.Fatalf("SelectPane: %v", err)
	}
	if w := f.window(t); w != f.wins[2] {
		t.Errorf("the tab is on window %q, want %q -- a stale hint was believed", w, f.wins[2])
	}
	if got := f.srv.Run(t, "list-panes", "-t", "=work:2", "-F", "#{pane_id}", "-f", "#{pane_active}"); got != f.panes[2] {
		t.Errorf("the active pane of the pane's own window is %q, want %q", got, f.panes[2])
	}
}

// A window id that is not one is not passed to tmux as a target: it falls back
// to reading the pane's real window, exactly as an absent hint does. tmux
// resolves a great many strings to "whatever is current", so a frontend bug
// that put a session name here must not become a navigation.
func TestSelectPaneIgnoresAWindowIDThatIsNotOne(t *testing.T) {
	f := newSelectFixture(t)
	c := NewClient(f.srv.Args())

	for _, bad := range []string{"@", "2", "work", "@1;kill-server", "@-1"} {
		f.srv.Run(t, "select-window", "-t", "=_web-a:0")
		if err := c.SelectPane(context.Background(), "_web-a", f.panes[2], bad); err != nil {
			t.Fatalf("SelectPane(window=%q): %v", bad, err)
		}
		if w := f.window(t); w != f.wins[2] {
			t.Errorf("SelectPane(window=%q) left the tab on %q, want %q", bad, w, f.wins[2])
		}
	}
	// The kill-server hint above must not have been run as a command.
	if out := f.srv.Run(t, "list-sessions", "-F", "#{session_name}"); !strings.Contains(out, "work") {
		t.Fatalf("the server is gone: a window id was interpreted as a command list")
	}
}

// A dead pane is still an error, and still moves nothing. The one-fork chain
// reads the pane first, and tmux abandons the rest of a command list once one
// of them fails -- so a click on a pane that died between the poll and the
// click cannot half-navigate the tab.
func TestSelectPaneWithAWindowIDStillFailsOnADeadPane(t *testing.T) {
	f := newSelectFixture(t)
	c := NewClient(f.srv.Args())

	if err := c.SelectPane(context.Background(), "_web-a", "%9999", f.wins[2]); err == nil {
		t.Error("SelectPane on a pane that does not exist = nil, want an error")
	}
	if w := f.window(t); w != f.wins[0] {
		t.Errorf("a failed select moved the tab to %q, want it left on %q", w, f.wins[0])
	}
}
