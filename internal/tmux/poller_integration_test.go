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
