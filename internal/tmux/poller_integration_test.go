package tmux_test

import (
	"context"
	"testing"
	"time"

	"github.com/diegok/tmux-web/internal/tmux"
	"github.com/diegok/tmux-web/internal/tmux/testutil"
)

// NewPoller must actually poll the client it was handed: the pure tests all
// supply their own function, so nothing else pins the wiring, and a sidebar fed
// by a poller that never reaches tmux would be empty forever.
func TestNewPollerCachesRealSnapshots(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "work", "-x", "80", "-y", "24")

	p := tmux.NewPoller(20*time.Millisecond, tmux.NewClient(srv.Args()))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx)

	rows := p.Latest()
	if len(rows) != 1 || rows[0].GroupKey != "work" {
		t.Fatalf("Latest() = %+v, want the one pane of the running server", rows)
	}

	// A pane opened after Start must reach the cache without anyone asking.
	srv.Run(t, "new-window", "-t", "=work")
	waitFor(t, 3*time.Second, func() bool { return len(p.Latest()) == 2 }, "a new pane never appeared in the cached snapshot")
	if err := p.Err(); err != nil {
		t.Fatalf("Err() = %v, want nil", err)
	}
}

// The generation is read per poll, not once at construction, because the whole
// point of carrying it is that the tmux server can restart underneath a running
// daemon -- which is exactly when pane ids go back to %0 and a browser's "done"
// memory would otherwise be applied to the wrong panes.
func TestNewPollerTracksTheServerGeneration(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "work", "-x", "80", "-y", "24")

	p := tmux.NewPoller(20*time.Millisecond, tmux.NewClient(srv.Args()))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx)

	first := p.ServerStart()
	if first == "" {
		t.Fatal("ServerStart() is empty while a tmux server is running")
	}

	// A restart. tmux stamps start_time in whole seconds, so a server restarted
	// inside the same second reports the same generation: sleep past the tick
	// rather than pretend the resolution is finer than it is.
	srv.Run(t, "kill-server")
	time.Sleep(1100 * time.Millisecond)
	srv.Run(t, "new-session", "-d", "-s", "work", "-x", "80", "-y", "24")

	waitFor(t, 3*time.Second, func() bool {
		s := p.ServerStart()
		return s != "" && s != first
	}, "the poller kept reporting the old tmux server's generation after a restart")
}
