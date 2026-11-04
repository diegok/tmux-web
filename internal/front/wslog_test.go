package front_test

import (
	"bytes"
	"log/slog"
	"regexp"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/diegok/tmux-web/internal/ptybridge"
)

// The daemon's only account of a terminal connection.
//
// WHY THIS EXISTS, and it is not hygiene. After two months of real use the
// owner reported keystrokes that stop arriving and are then either flushed all
// at once or lost outright. The daemon had been up for two and a half weeks;
// its whole log for that period contained not one line about a WebSocket --
// only net/http's own GOAWAY and TLS-handshake noise. There was nothing to
// read, so the one bug that costs the user work was also the one bug the
// daemon could not see.
//
// THE COUNTERS MATTER MORE THAN THEY LOOK. A keystroke that never reaches tmux
// is either a frame the server never received or a frame it received and
// dropped, and those are opposite bugs with opposite fixes. frames_in and
// since_last_read are what separate them from the log alone: a connection that
// dies with frames_in flat and since_last_read long was not being fed, which
// puts the loss in the browser's own socket buffer rather than anywhere in
// this process.
//
// It asserts the shape rather than the numbers, with one exception -- frames
// that this test really did send and really did receive have to be counted, or
// the counters are decoration.
func TestATerminalConnectionIsLoggedWhenItOpensAndWhenItEnds(t *testing.T) {
	logs := captureLogs(t)
	f := defaultWSFixture(t)

	c := f.dial(t, wsCanonical, "?session=work")
	f.tabSession(t)
	// Traffic in both directions, so the four counters cannot all be zero by
	// construction.
	wsWrite(t, c, ptybridge.EncodeData([]byte("echo log-ok\r")))
	wsReadUntil(t, c, "log-ok", 10*time.Second)

	open := waitForLine(t, logs, "terminal: connected")
	for _, key := range []string{"conn=", "session=", "peer="} {
		if !bytes.Contains([]byte(open), []byte(key)) {
			t.Errorf("the connected line has no %s:\n%s", key, open)
		}
	}

	// This connection's id, so the disconnect below is this connection's
	// disconnect and not one still arriving from a connection another test
	// opened before this capture began.
	conn := field(open, "conn")
	if conn == "" {
		t.Fatalf("the connected line carries no conn id:\n%s", open)
	}

	_ = c.Close(websocket.StatusNormalClosure, "test done")

	closed := waitForLine(t, logs, "terminal: disconnected", "conn="+conn)
	// Every field a person would need to tell "the network went away" from
	// "the session ended" from "this handler gave up".
	for _, key := range []string{
		"conn=", "session=", "status=", "graceful=", "age=",
		"frames_in=", "bytes_in=", "frames_out=", "bytes_out=",
		"since_last_read=", "slowest_write=",
	} {
		if !bytes.Contains([]byte(closed), []byte(key)) {
			t.Errorf("the disconnected line has no %s:\n%s", key, closed)
		}
	}

	// The two lines are one connection's, and nothing but the id can say so:
	// with two tabs open their lines interleave.
	if a, b := field(open, "conn"), field(closed, "conn"); a == "" || a != b {
		t.Errorf("connected conn=%q and disconnected conn=%q are not the same connection", a, b)
	}

	// The one pair of numbers this test is entitled to assert on.
	if n := number(t, closed, "frames_in"); n < 1 {
		t.Errorf("frames_in = %d after this test sent a frame; the inbound counter is not counting:\n%s", n, closed)
	}
	if n := number(t, closed, "frames_out"); n < 1 {
		t.Errorf("frames_out = %d after this test read the echo back; the outbound counter is not counting:\n%s", n, closed)
	}
}

// A connection the peer abandons without a close handshake is the shape the
// owner's phone produces on a bad link, and it is the one that has to be
// legible: there is no status to report, so `graceful=false` and the cause are
// the whole account of it.
func TestAnAbandonedTerminalConnectionIsStillLogged(t *testing.T) {
	logs := captureLogs(t)
	f := defaultWSFixture(t)

	c := f.dial(t, wsCanonical, "?session=work")
	f.tabSession(t)
	// Pinned to this connection for the same reason as the test above: another
	// test's teardown can log a disconnect into this buffer.
	conn := field(waitForLine(t, logs, "terminal: connected"), "conn")
	if conn == "" {
		t.Fatal("no connected line to take an id from")
	}
	// CloseNow drops the TCP connection without a close frame, which is what a
	// phone leaving the network looks like from here.
	_ = c.CloseNow()

	closed := waitForLine(t, logs, "terminal: disconnected", "conn="+conn)
	if got := field(closed, "graceful"); got != "false" {
		t.Errorf("graceful = %q, want false: nobody was left to send a status to:\n%s", got, closed)
	}
	if field(closed, "cause") == "" {
		t.Errorf("no cause on an abandoned connection; the read error is the only thing that says how it died:\n%s", closed)
	}
}

// --- fixture ---------------------------------------------------------------

// safeBuf is a buffer a test goroutine may read while slog's handler writes.
// slog's own handlers serialize their writes, but not against a reader.
type safeBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureLogs redirects the default logger for the length of one test and
// returns a reader for what it has collected.
func captureLogs(t *testing.T) func() string {
	t.Helper()
	b := &safeBuf{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(b, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return b.String
}

// waitForLine returns the first logged line containing want, waiting for it:
// the close line is written by the handler's own goroutine after the client has
// already moved on.
// All of want, not any: a disconnect has to be matched on its connection id as
// well as its message.
//
// WHY THAT MATTERS, measured the hard way: `wsConnSeq` numbers connections for
// the whole package, and a connection another test opened before this capture
// started can still be tearing down once it has. Its "connected" line went to
// the previous writer and its "disconnected" line lands in this buffer, so
// taking the first disconnect gave conn=52 against this test's own conn=53 and
// the pairing assertion failed for a reason that had nothing to do with the
// code under test.
func waitForLine(t *testing.T, logs func() string, want ...string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, line := range bytes.Split([]byte(logs()), []byte("\n")) {
			matched := true
			for _, w := range want {
				if !bytes.Contains(line, []byte(w)) {
					matched = false
					break
				}
			}
			if matched {
				return string(line)
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no log line containing all of %q within 10s; the log held:\n%s", want, logs())
	return ""
}

// field reads one slog text-handler attribute out of a line. Quoted values are
// unquoted, which is what `reason="session ended"` needs.
func field(line, key string) string {
	m := regexp.MustCompile(`\b` + regexp.QuoteMeta(key) + `=("(?:[^"\\]|\\.)*"|\S*)`).FindStringSubmatch(line)
	if m == nil {
		return ""
	}
	if s, err := strconv.Unquote(m[1]); err == nil {
		return s
	}
	return m[1]
}

func number(t *testing.T, line, key string) int {
	t.Helper()
	raw := field(line, key)
	n, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatalf("%s=%q is not a number in:\n%s", key, raw, line)
	}
	return n
}
