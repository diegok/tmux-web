package tmux_test

import (
	"context"
	"strconv"
	"strings"
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

// End to end against a real tmux: a pane whose screen keeps changing reads
// working, one that sits still settles to idle, and a pane that is not a known
// agent gets no state at all.
//
// The agent panes are a copy of cat named "claude" -- see testutil.FakeAgent --
// so the real Agents list is what decides, no TUI has to be installed, and the
// pane's command is steady. A `sh -c 'while :; ...'` loop would report sh,
// sleep or date depending on which instant the poll landed in, and the pane
// would drift on and off the agent list between polls.
func TestPollerClassifiesRealAgentPanes(t *testing.T) {
	agent := testutil.FakeAgent(t, "claude")

	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "work", "-x", "40", "-y", "10", agent)
	srv.Run(t, "split-window", "-t", "work", agent)
	// Not an agent: never captured, never classified, never badged.
	srv.Run(t, "split-window", "-t", "work", "sleep 300")

	ids := strings.Split(srv.Run(t, "list-panes", "-t", "work", "-F", "#{pane_id}"), "\n")
	if len(ids) != 3 {
		t.Fatalf("want 3 panes, got %q", ids)
	}
	busy, still, other := ids[0], ids[1], ids[2]

	c := tmux.NewClient(srv.Args())
	p := tmux.NewPollerWith(tmux.Options{
		Interval:    50 * time.Millisecond,
		Snapshot:    c.Snapshot,
		ServerStart: c.ServerStart,
		Capture:     c.Capture,
		Connected:   func() bool { return true },
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx)

	state := func(id string) tmux.Row {
		for _, r := range p.Latest() {
			if r.PaneID == id {
				return r
			}
		}
		return tmux.Row{}
	}

	// Keep typing into one pane while waiting. cat echoes it, so its screen
	// differs at every poll; the other two are untouched.
	//
	// A counter rather than a fixed character: once the pane is full of
	// identical lines, scrolling one more onto it leaves the capture byte for
	// byte the same and the pane reads idle while it is being typed into.
	deadline := time.Now().Add(10 * time.Second)
	for typed := 0; ; typed++ {
		if _, err := srv.TryRun("send-keys", "-t", busy, "line"+strconv.Itoa(typed), "Enter"); err != nil {
			t.Fatalf("send-keys: %v", err)
		}
		if state(busy).AgentState == tmux.StateWorking && state(still).AgentState == tmux.StateIdle {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("never reached working/idle: busy=%+v still=%+v", state(busy), state(still))
		}
		time.Sleep(20 * time.Millisecond)
	}

	// A pane that has never been seen to change has had no working->idle edge,
	// whatever its first poll reported. Otherwise every agent that was already
	// sitting idle when the daemon started lights up a done badge.
	if got := state(still); got.FinishedAt != 0 {
		t.Errorf("a pane that never changed = %+v, want no finish edge", got)
	}
	if got := state(other); got.AgentState != "" || got.FinishedAt != 0 {
		t.Errorf("a `sleep` pane = %+v, want no state: it is not a known agent", got)
	}

	// Stop typing. The pane that WAS working settles, and this settle is a real
	// edge because a real change was observed during the run.
	waitFor(t, 10*time.Second, func() bool {
		got := state(busy)
		return got.AgentState == tmux.StateIdle && got.FinishedAt != 0
	}, "a pane that stopped changing never settled to idle with a finish edge")
}
