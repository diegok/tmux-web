package ptybridge

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/diegok/tmux-web/internal/tmux/testutil"
)

// Open's hyperlink check is the one tmux call the terminal path makes before
// the browser has anything at all: it runs before the pty is started, so before
// the read, write and ping loops that would otherwise notice a stall exist, and
// on a context ws.go deliberately hands it unwired from the request -- see
// serve. Unbounded, a wedged tmux server there means a tab that never opens a
// terminal, for a feature whose entire value is clickable links.
//
// These are internal because enableHyperlinks is the seam. Holding up the real
// one takes a wedged tmux server, which nothing can produce on cue; what a test
// can do is watch the deadline this package puts on it.

// recordEnabler replaces the hyperlink check for one test, and returns the
// contexts it was called with.
func recordEnabler(t *testing.T, block bool) *[]context.Context {
	t.Helper()
	var seen []context.Context
	prev := enableHyperlinks
	t.Cleanup(func() { enableHyperlinks = prev })
	enableHyperlinks = func(ctx context.Context, _ []string) error {
		seen = append(seen, ctx)
		if block {
			// A tmux that never answers. The real one is killed by
			// exec.CommandContext when the deadline fires; the second arm is
			// what an unbounded check gets, and it is there so that one fails
			// an assertion below rather than hanging the test binary.
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(3 * time.Second):
				return nil
			}
		}
		return nil
	}
	return &seen
}

func TestOpenBoundsTheHyperlinkCheck(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "work", "-x", "80", "-y", "24")
	seen := recordEnabler(t, false)

	s, err := Open(context.Background(), Config{TmuxArgs: srv.Args(), Base: "work", Cols: 80, Rows: 24})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	if len(*seen) != 1 {
		t.Fatalf("the hyperlink check ran %d times per attach, want 1", len(*seen))
	}
	deadline, ok := (*seen)[0].Deadline()
	if !ok {
		t.Fatal("the hyperlink check ran on a context with no deadline: a wedged tmux would hold the tab " +
			"before the terminal, its loops, or anything that could time it out exists")
	}
	// Spelled out rather than compared against hyperlinkTimeout itself, which
	// would pass whatever that were changed to.
	if left := time.Until(deadline); left <= time.Second || left > 2*time.Second {
		t.Errorf("the hyperlink check had %v, want ~2s: long enough for a healthy `show`, "+
			"short enough that the owner is not left looking at a blank terminal", left)
	}
}

// What the bound is worth: the shell opens anyway. Losing clickable links is a
// warning in the log, which is what Open already decided a failed check costs;
// losing the terminal is the whole product.
func TestATmuxThatNeverAnswersDoesNotCostTheTerminal(t *testing.T) {
	prev := hyperlinkTimeout
	hyperlinkTimeout = 100 * time.Millisecond
	t.Cleanup(func() { hyperlinkTimeout = prev })

	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "work", "-x", "80", "-y", "24")
	recordEnabler(t, true)

	start := time.Now()
	s, err := Open(context.Background(), Config{TmuxArgs: srv.Args(), Base: "work", Cols: 80, Rows: 24})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	if took := time.Since(start); took > time.Second {
		t.Fatalf("Open took %v to get past a hyperlink check that never answered, want ~100ms", took)
	}

	// And it is a working terminal, not just a returned pointer.
	if _, err := s.Write([]byte("echo hello-bridge\r")); err != nil {
		t.Fatal(err)
	}
	var buf strings.Builder
	deadline := time.After(10 * time.Second)
	for {
		select {
		case b, ok := <-s.Output():
			if !ok {
				t.Fatalf("the session ended without echoing; got %q", buf.String())
			}
			buf.Write(b)
			if strings.Contains(buf.String(), "hello-bridge") {
				return
			}
		case <-deadline:
			t.Fatalf("the terminal never echoed; got %q", buf.String())
		}
	}
}
