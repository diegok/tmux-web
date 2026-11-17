package front

import (
	"context"
	"errors"
	"testing"

	"github.com/coder/websocket"
)

// A tab that falls behind is the one case the two sides used to disagree about.
//
// The bridge closes a session whose reader cannot keep up -- blocking would
// stall a tmux client the owner shares the server with, and dropping bytes
// would corrupt the terminal permanently -- and its own comment says the tab
// "reconnects and loses nothing". But the write loop reported that closed
// channel as StatusNormalClosure "session ended", and the browser's
// `shouldReconnect` is `code !== 1000`: the one close it never retries. The
// owner got "Session ended" and a "New session" button with his tmux session
// alive and working. Four connections in one week of his own log ended that
// way, each after megabytes of output and a write that blocked for seconds.
//
// 1013 is RFC 6455's "try again later", which is exactly what this is.
//
// A unit test of the mapping rather than a flood through a real tmux server:
// the bridge's channel holds 8MB and tmux paints at ~135KB/s, so a real
// overflow needs a tab that stops draining for a minute. What the two tests
// either side of this one pin is that the bridge sets the flag (ptybridge's
// TestSlowClientClosesTheSessionInsteadOfBlocking) and that the browser
// reconnects on this code (transport.test.ts).
func TestAnOverflowedSessionIsReportedAsTryAgainLater(t *testing.T) {
	out := make(chan []byte)
	close(out)

	// The conn is never touched: a closed output channel returns before any
	// write. Passing nil is what says so.
	got := wsWriteLoop(context.Background(), nil, out, nil, &wsStats{}, func() bool { return true })

	if got.status != websocket.StatusTryAgainLater {
		t.Errorf("status = %v, want %v", got.status, websocket.StatusTryAgainLater)
	}
	if !got.graceful {
		t.Error("graceful = false; the browser needs the close frame to read the code off")
	}
	if got.reason == "" {
		t.Error("reason is empty; the daemon log is where this gets counted")
	}
}

// The other half of the same fork: a session that really ended -- the user
// typed `exit`, or the base session was killed -- still says so with the one
// code that stops the browser retrying.
func TestASessionThatReallyEndedIsStillReportedAsNormalClosure(t *testing.T) {
	out := make(chan []byte)
	close(out)

	got := wsWriteLoop(context.Background(), nil, out, nil, &wsStats{}, func() bool { return false })

	if got.status != websocket.StatusNormalClosure {
		t.Errorf("status = %v, want %v", got.status, websocket.StatusNormalClosure)
	}
	if got.reason != "session ended" {
		t.Errorf("reason = %q, want %q", got.reason, "session ended")
	}
}

// Strikes have to be CONSECUTIVE, or the tolerance is a delay rather than a
// policy: a tab open all afternoon on a link that drops one ping an hour would
// still be collected, just on the third hour instead of the first. The owner's
// week held 141 of those spread across connections that lived for hours.
//
// A unit test of the rule rather than three real misses through a socket: a
// client can only miss a ping by having no Read parked, and the only way to
// take a Read out of the water in coder/websocket is to cancel its context,
// which closes the connection -- the first draft of this test killed its own
// client and blamed the daemon. What a real socket does prove is the single
// tolerated miss, and TestOneMissedPingDoesNotCollectALiveConnection does it.
func TestKeepaliveStrikesMustBeConsecutive(t *testing.T) {
	boom := errors.New("failed to wait for pong")

	for _, c := range []struct {
		name    string
		err     error
		strikes int
		collect bool
		next    int
	}{
		{"first failure is tolerated", boom, 0, false, 1},
		{"second failure is tolerated", boom, 1, false, 2},
		{"third failure in a row collects", boom, 2, true, 3},
		{"an answered ping resets the count", nil, 2, false, 0},
		{"an answered ping on a clean connection stays at zero", nil, 0, false, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			collect, next := pingVerdict(c.err, c.strikes)
			if collect != c.collect {
				t.Errorf("collect = %v, want %v", collect, c.collect)
			}
			if next != c.next {
				t.Errorf("next = %d, want %d", next, c.next)
			}
		})
	}
}
