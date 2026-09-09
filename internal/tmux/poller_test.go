package tmux_test

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/diegok/tmux-web/internal/tmux"
)

func TestPollerSharesOnePollAcrossReaders(t *testing.T) {
	var calls int32
	p := tmux.NewPollerFunc(50*time.Millisecond, func(context.Context) ([]tmux.Row, error) {
		atomic.AddInt32(&calls, 1)
		return []tmux.Row{{PaneID: "%0"}}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx)

	// Ten concurrent readers inside one interval must not cause ten polls.
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = p.Latest() }()
	}
	wg.Wait()

	if n := atomic.LoadInt32(&calls); n > 2 {
		t.Fatalf("want at most 2 polls, got %d -- readers are each forking tmux", n)
	}
}

// The first snapshot must be in the cache by the time Start returns, or the
// first browser tab to connect gets an empty sidebar until the first tick.
// The interval is long enough that only the synchronous initial poll can have
// run when this reads.
func TestPollerPollsOnceBeforeStartReturns(t *testing.T) {
	p := tmux.NewPollerFunc(time.Hour, func(context.Context) ([]tmux.Row, error) {
		return []tmux.Row{{PaneID: "%4", WindowName: "agents"}}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx)

	rows := p.Latest()
	if len(rows) != 1 || rows[0].PaneID != "%4" || rows[0].WindowName != "agents" {
		t.Fatalf("Latest() = %+v, want the rows the poll returned", rows)
	}
	if err := p.Err(); err != nil {
		t.Fatalf("Err() = %v, want nil after a successful poll", err)
	}
}

// The cache has to keep moving on its own: a poller that only ever runs its
// initial poll serves a snapshot that is minutes stale and never notices a
// pane appearing.
func TestPollerRefreshesOnTheTicker(t *testing.T) {
	var calls int32
	p := tmux.NewPollerFunc(2*time.Millisecond, func(context.Context) ([]tmux.Row, error) {
		n := atomic.AddInt32(&calls, 1)
		return []tmux.Row{{PaneID: "%" + strconv.Itoa(int(n))}}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx)

	// Each poll reports a different pane id, so "the cache changed" is proof
	// that a later poll reached it. Waiting for one specific id instead would
	// pass only when the reader happens to sample that interval.
	if first := p.Latest(); len(first) != 1 || first[0].PaneID != "%1" {
		t.Fatalf("Latest() = %+v after the initial poll, want %%1", first)
	}
	waitFor(t, 3*time.Second, func() bool {
		rows := p.Latest()
		return len(rows) == 1 && rows[0].PaneID != "%1"
	}, "the cached snapshot never advanced past the initial poll")
}

// A transient tmux failure -- "server exited unexpectedly" is emitted for a few
// milliseconds while a server shuts down -- must not blank a sidebar that was
// correct an interval ago. The error is still visible so a persistent failure
// can be surfaced rather than served as a snapshot that is stale forever.
func TestPollerKeepsLastGoodSnapshotOnError(t *testing.T) {
	boom := errors.New("server exited unexpectedly")

	var mu sync.Mutex
	mode := "ok"
	p := tmux.NewPollerFunc(time.Millisecond, func(context.Context) ([]tmux.Row, error) {
		mu.Lock()
		defer mu.Unlock()
		switch mode {
		case "ok":
			return []tmux.Row{{PaneID: "%7"}}, nil
		case "fail":
			return nil, boom
		default:
			return []tmux.Row{{PaneID: "%9"}}, nil
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx)

	if rows := p.Latest(); len(rows) != 1 || rows[0].PaneID != "%7" {
		t.Fatalf("Latest() = %+v before any failure, want the first good snapshot", rows)
	}

	mu.Lock()
	mode = "fail"
	mu.Unlock()
	waitFor(t, 3*time.Second, func() bool { return p.Err() != nil }, "a failed poll never surfaced an error")

	if rows := p.Latest(); len(rows) != 1 || rows[0].PaneID != "%7" {
		t.Fatalf("Latest() = %+v after a failed poll, want the last good snapshot kept", rows)
	}
	if err := p.Err(); !errors.Is(err, boom) {
		t.Fatalf("Err() = %v, want the failure that the poll returned", err)
	}

	// Recovery: a later success must both replace the rows and clear the error,
	// or the UI keeps an error banner up forever.
	mu.Lock()
	mode = "recovered"
	mu.Unlock()
	waitFor(t, 3*time.Second, func() bool {
		rows := p.Latest()
		return p.Err() == nil && len(rows) == 1 && rows[0].PaneID == "%9"
	}, "the poller never recovered after a transient failure")
}

// Cancelling the context is the only way to stop the ticker. If it does not,
// every daemon that ever built a Poller keeps forking tmux forever.
func TestPollerStopsOnContextCancel(t *testing.T) {
	var calls int32
	p := tmux.NewPollerFunc(time.Millisecond, func(context.Context) ([]tmux.Row, error) {
		atomic.AddInt32(&calls, 1)
		return nil, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	p.Start(ctx)
	waitFor(t, 3*time.Second, func() bool { return atomic.LoadInt32(&calls) > 3 }, "the ticker never fired")

	cancel()
	// One poll may already be in flight when cancel lands, hence the +1.
	settled := atomic.LoadInt32(&calls) + 1
	time.Sleep(50 * time.Millisecond) // ~50 intervals
	if n := atomic.LoadInt32(&calls); n > settled {
		t.Fatalf("polls went from %d to %d after cancel -- the ticker outlives its context", settled, n)
	}
}
