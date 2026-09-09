package tmux_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/diegok/tmux-web/internal/tmux"
	"github.com/diegok/tmux-web/internal/tmux/testutil"
)

func TestSnapshotAgainstRealTmux(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "work", "-x", "80", "-y", "24")
	srv.Run(t, "new-window", "-t", "work")

	// A user session whose name starts with the app's prefix. It must never be
	// hidden or swept: only the @wterm_web option marks an app session.
	srv.Run(t, "new-session", "-d", "-s", "_web-notes")

	// Simulate an open browser tab: a grouped, app-marked session.
	srv.Run(t, "new-session", "-d", "-t", "work", "-s", "_web-sim")
	srv.Run(t, "set", "-t", "_web-sim", "@wterm_web", "1")

	c := tmux.NewClient(srv.Args())
	panes, err := c.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	byPane := map[string]tmux.Row{}
	for _, p := range panes {
		if _, dup := byPane[p.PaneID]; dup {
			t.Fatalf("pane %s reported twice: %+v", p.PaneID, panes)
		}
		byPane[p.PaneID] = p
	}
	if len(byPane) != 3 {
		t.Fatalf("want 3 panes (work x2, _web-notes x1), got %d: %+v", len(byPane), panes)
	}

	var sawNotes bool
	for _, p := range panes {
		if p.GroupKey == "_web-notes" {
			sawNotes = true
		}
	}
	if !sawNotes {
		t.Fatal("a user session named _web-notes must not be hidden")
	}
}

// Regression: killing the base session must not blank the sidebar.
func TestSnapshotSurvivesBaseSessionKill(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "work", "-x", "80", "-y", "24")
	srv.Run(t, "new-window", "-t", "work")
	srv.Run(t, "new-session", "-d", "-t", "work", "-s", "_web-sim")
	srv.Run(t, "set", "-t", "_web-sim", "@wterm_web", "1")

	srv.Run(t, "kill-session", "-t", "work")

	c := tmux.NewClient(srv.Args())
	panes, err := c.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(panes) != 2 {
		t.Fatalf("agents must stay visible after the base session dies, got %+v", panes)
	}
	for _, p := range panes {
		if p.GroupKey != "work" {
			t.Fatalf("panes should still be labelled by group %q: %+v", "work", p)
		}
	}
}

func TestSnapshotWithNoServerIsNotAnError(t *testing.T) {
	rows, err := tmux.NewClient(testutil.NewServer(t).Args()).Snapshot(context.Background())
	if err != nil || rows != nil {
		t.Fatalf("Snapshot on a dead server = %v, %v; want nil, nil", rows, err)
	}
}

// The socket file outlives the server, and tmux then words the same condition
// differently -- "no server running on <path>" instead of the connect error a
// never-started socket gives. Without this the ECONNREFUSED half of noServer is
// unexercised, which is the half a long-running deployment actually hits: the
// user quits their last session while the sidebar is polling.
func TestSnapshotWithNoServerAfterExitIsNotAnError(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "work")
	srv.Run(t, "kill-server")
	if _, err := os.Stat(srv.SocketPath()); err != nil {
		t.Fatalf("socket file must outlive the server, else this duplicates the ENOENT test: %v", err)
	}

	// kill-server returns before the server has finished exiting, and a command
	// that lands in that window fails with "server exited unexpectedly". Poll
	// until it settles rather than asserting on the transient.
	c := tmux.NewClient(srv.Args())
	var err error
	var rows []tmux.Row
	deadline := time.Now().Add(5 * time.Second)
	for {
		rows, err = c.Snapshot(context.Background())
		if err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err != nil || rows != nil {
		t.Fatalf("Snapshot on an exited server = %v, %v; want nil, nil", rows, err)
	}
}
