package front_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/diegok/tmux-web/internal/ptybridge"
)

// WHY THIS EXISTS. The owner loses what he has typed. Measured from the
// daemon's own log, on a connection that died while he was typing into an
// agent's prompt:
//
//	frames_in=289 bytes_in=2219 since_last_read=3.223s slowest_write=0s
//	cause="failed to ping: failed to wait for pong: context deadline exceeded"
//
// 7.7 bytes per inbound frame -- keystrokes -- and a server that was never once
// blocked writing. The silence was in the browser-to-daemon direction, and a
// browser hands send() to a socket that is still OPEN on a stalled link, where
// those bytes live nowhere else. Two other connections the same afternoon show
// the window: one read nothing for 13s and recovered, one for 1m09.9s and was
// reset.
//
// So the browser has to be able to tell which of its bytes actually reached the
// pty. Nothing in the protocol said so before: a frame left and was never
// mentioned again. The ack is that mention, and these tests are the Go half of
// the contract -- web/src/lib/transport.test.ts is the other.

// The ack counts bytes the PTY accepted, cumulatively over the connection, and
// cumulative is the point: it means an ack may be dropped under load without
// costing anything, because the next one supersedes it.
func TestInputIsAcknowledgedByTheBytesThatReachedThePty(t *testing.T) {
	f := defaultWSFixture(t)
	c := f.dial(t, wsCanonical, "?session=work")

	wsWrite(t, c, ptybridge.EncodeData([]byte("hello")))
	if got := wsReadAck(t, c, 15*time.Second, 5); got != 5 {
		t.Errorf("after 5 bytes the daemon acknowledged %d, want 5", got)
	}

	// Cumulative, not per frame: the second ack counts the first frame too, so
	// one ack is enough to free everything older than it.
	wsWrite(t, c, ptybridge.EncodeData([]byte("xyz")))
	if got := wsReadAck(t, c, 15*time.Second, 8); got != 8 {
		t.Errorf("after 3 more bytes the daemon acknowledged %d, want 8", got)
	}
}

// A control frame is not input. Counting one would tell the browser that
// keystrokes had landed when what landed was a resize, and the browser would
// free bytes the pty never saw.
func TestOnlyInputIsAcknowledgedNotControlMessages(t *testing.T) {
	f := defaultWSFixture(t)
	c := f.dial(t, wsCanonical, "?session=work")

	wsWrite(t, c, ptybridge.EncodeData([]byte("ab")))
	if got := wsReadAck(t, c, 15*time.Second, 2); got != 2 {
		t.Fatalf("after 2 bytes the daemon acknowledged %d, want 2", got)
	}
	// Resizes and selects travel on the same socket and must not move the count.
	wsWrite(t, c, ptybridge.EncodeControl([]byte(`{"type":"resize","cols":100,"rows":30}`)))
	wsWrite(t, c, ptybridge.EncodeControl([]byte(`{"type":"where"}`)))
	wsWrite(t, c, ptybridge.EncodeData([]byte("c")))

	if got := wsReadAck(t, c, 15*time.Second, 3); got != 3 {
		t.Errorf("the count reached %d after 3 bytes of input and two control messages, want 3", got)
	}
}

// An empty data frame is legal in this protocol (Decode yields an empty
// payload) and carries no input, so it must not produce an ack the browser
// would read as progress.
func TestAnEmptyDataFrameIsNotAcknowledged(t *testing.T) {
	f := defaultWSFixture(t)
	c := f.dial(t, wsCanonical, "?session=work")

	wsWrite(t, c, ptybridge.EncodeData(nil))
	wsWrite(t, c, ptybridge.EncodeData([]byte("z")))
	// The first ack to arrive must already be the one for "z": an ack of 0
	// would mean the browser was told something landed when nothing did.
	if got := wsReadAck(t, c, 15*time.Second, 1); got != 1 {
		t.Errorf("first ack was %d, want 1 -- an empty frame was acknowledged", got)
	}
}

// wsReadAck reads until an ack of at least min arrives, skipping the pty output
// and any other control message. Returns the count it carried.
func wsReadAck(t *testing.T, c *websocket.Conn, d time.Duration, min int64) int64 {
	t.Helper()
	deadline := time.Now().Add(d)
	var seen []string
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		typ, msg, err := c.Read(ctx)
		cancel()
		if err != nil {
			t.Fatalf("read: %v (control messages seen: %v)", err, seen)
		}
		if typ != websocket.MessageBinary {
			t.Fatalf("message type = %v, want binary", typ)
		}
		kind, payload, err := ptybridge.Decode(msg)
		if err != nil {
			t.Fatalf("undecodable frame %v: %v", msg, err)
		}
		if kind != ptybridge.FrameControl {
			continue
		}
		var got struct {
			Type  string `json:"type"`
			Bytes int64  `json:"bytes"`
		}
		if err := json.Unmarshal(payload, &got); err != nil {
			t.Fatalf("control message is not JSON: %q", payload)
		}
		seen = append(seen, string(payload))
		// Pinned literally on both sides of the wire, like "pane".
		if got.Type != "ack" {
			continue
		}
		if got.Bytes >= min {
			return got.Bytes
		}
	}
	t.Fatalf("no ack of %d or more arrived; control messages seen: %s", min, strings.Join(seen, " "))
	return 0
}
