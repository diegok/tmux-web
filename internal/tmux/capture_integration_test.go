package tmux_test

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/diegok/tmux-web/internal/tmux"
	"github.com/diegok/tmux-web/internal/tmux/testutil"
)

// captureFixture starts a pane whose every byte is a byte the fixture printed:
// no shell prompt, because the pane runs one `sh -c` and nothing else, and no
// terminal echo, because a test that pads the capture with send-keys must know
// exactly how many bytes each pad adds.
//
// The order of the three commands is load-bearing and was measured on 3.7b:
//
//   - `set-option -g history-limit` BEFORE any session fails -- there is no
//     server yet to set an option on -- and after `new-session` it does apply
//     to the pane that already exists (#{history_limit} reads the new value,
//     asserted below).
//   - the pane therefore starts as `exec cat`, which prints nothing, and the
//     script is respawned into it once the limit is raised. Starting the
//     script in `new-session` and raising the limit afterwards races the
//     script's own output against the option.
func captureFixture(t *testing.T, cols, rows, history int, script string) (*testutil.Server, string) {
	t.Helper()

	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "cap",
		"-x", strconv.Itoa(cols), "-y", strconv.Itoa(rows), "exec cat")

	if history > 0 {
		srv.Run(t, "set-option", "-g", "history-limit", strconv.Itoa(history))
		if got := srv.Run(t, "display-message", "-p", "-t", "cap", "#{history_limit}"); got != strconv.Itoa(history) {
			t.Fatalf("history_limit = %s, want %d: a raised limit no longer reaches an "+
				"existing pane, so this fixture cannot hold enough scrollback", got, history)
		}
	}

	pane := srv.Run(t, "list-panes", "-t", "cap", "-F", "#{pane_id}")
	srv.Run(t, "respawn-pane", "-k", "-t", pane, "sh -c 'stty -echo; "+script+"'")
	return srv, pane
}

// TestCaptureRangeReturnsScrollbackAndTheScreen pins the difference between the
// two calls rather than the output of one: Capture returns the visible screen
// and CaptureRange returns strictly more, including a line that has scrolled
// off. A test that only asserted "CaptureRange returned something" would pass
// against a CaptureRange that ignored its argument.
//
// It also carries the assertions for the three flags that are otherwise
// invisible: -J is kept (a wrapped line comes back whole), -N is rejected (no
// line is padded out to the pane width) and -e is off (no escape sequence
// survives coloured output).
func TestCaptureRangeReturnsScrollbackAndTheScreen(t *testing.T) {
	// 95 characters on a 40-column pane: three rows tmux flags as wrapped, and
	// one line again once -J has joined them.
	long := strings.Repeat("W", 95)

	srv, pane := captureFixture(t, 40, 5, 0,
		`printf "\033[31mCOLOURED\033[0m\n"; printf "%s\n" "`+long+`"; `+
			`i=1; while [ $i -le 40 ]; do printf "line%02d\n" $i; i=$((i+1)); done; exec cat`)

	c := tmux.NewClient(srv.Args())
	ctx := context.Background()

	// Wait on the FIXTURE and not on the call under test: a CaptureRange that
	// returned nothing would otherwise spend the whole timeout here and be
	// reported as a slow pane rather than as the assertion it actually broke.
	waitFor(t, 5*time.Second, func() bool {
		out, err := srv.TryRun("capture-pane", "-p", "-J", "-S", "-200", "-t", pane)
		return err == nil && strings.Contains(out, "line40")
	}, "the pane never printed its last line")

	out, truncated, err := c.CaptureRange(ctx, pane, 200)
	if err != nil {
		t.Fatal(err)
	}

	// The pre-state: the screen does NOT hold the oldest lines. Without this
	// the two assertions below could both be satisfied by a five-row pane that
	// never scrolled, and the -S flag would be doing nothing.
	screen, err := c.Capture(ctx, pane)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(screen, "line01") || strings.Contains(screen, "COLOURED") {
		t.Fatalf("the visible screen already holds the oldest lines, so nothing here "+
			"tests the start line:\n%s", screen)
	}

	if !strings.Contains(out, "line01") {
		t.Errorf("CaptureRange did not reach into scrollback: line01 is missing from\n%s", out)
	}
	if !strings.Contains(out, "COLOURED") {
		t.Errorf("CaptureRange did not reach the top of the history: COLOURED is missing")
	}
	if len(out) <= len(screen) {
		t.Errorf("CaptureRange returned %d bytes and Capture %d; it must return strictly more",
			len(out), len(screen))
	}
	// A 40x5 pane's whole history is a few hundred bytes, nowhere near the cap.
	if truncated {
		t.Errorf("truncated = true for a capture of %d bytes", len(out))
	}

	// -J: the wrapped line comes back as one string.
	if !strings.Contains(out, long) {
		t.Errorf("a 95-character line did not come back whole from a 40-column pane:\n%s", out)
	}
	// -e: no escape sequence, even though the fixture printed colour.
	if strings.ContainsRune(out, 0x1b) {
		t.Errorf("the capture carries an escape sequence; -e must stay off:\n%q", out)
	}
	// -N is NOT asserted here, and that is a finding rather than an omission:
	// on 3.7b, -J already preserves trailing spaces, and adding -N to a -J
	// capture produces byte-identical output (286 bytes both ways on this
	// fixture; without -J it is 284 against 452). The flag is unobservable in
	// the output while -J is passed, so the only place it can be pinned is the
	// argument list -- see TestCaptureRangeArgsCarryNoPaddingOrEscapes.
}

// TestCaptureRangeCapsBytesAndTruncatesFromTheTop is the test that owns the
// direction of the cut. The newest lines are the ones the panel was opened for,
// so the OLD end is what goes.
//
// Every size here is a LITERAL and not derived from MaxCaptureBytes: a fixture
// sized from the constant moves with a mutant that retargets the constant, and
// then neither side can fail. 4000 lines of 146 bytes is ~584 000 against a
// 262 144-byte cap, and the assertions below name 262 144 in figures for the
// same reason.
func TestCaptureRangeCapsBytesAndTruncatesFromTheTop(t *testing.T) {
	const (
		head = "HEAD_MARKER_OLDEST"
		tail = "TAIL_MARKER_NEWEST"
		// The exact value of MaxCaptureBytes, written out. A mutant that
		// retargets the constant fails here instead of moving the goalposts.
		capBytes = 262144
	)

	// A DEFAULT pane cannot reach the cap: under -f /dev/null the history-limit
	// is tmux's own 2000 and a new-session pane is 80 columns, so the whole
	// history is at most 160 000 bytes -- and far less in practice, because -J
	// with no -N strips the trailing spaces. Hence 200 columns and a 10 000
	// line history.
	pad := strings.Repeat(".", 140)
	srv, pane := captureFixture(t, 200, 50, 10000,
		`printf "`+head+`\n"; `+
			`i=1; while [ $i -le 4000 ]; do printf "L%04d`+pad+`\n" $i; i=$((i+1)); done; `+
			`printf "`+tail+`\n"; exec cat`)

	c := tmux.NewClient(srv.Args())
	ctx := context.Background()

	// The uncapped capture, run exactly as CaptureRange runs it, so that the
	// guard below measures the same bytes the implementation sees.
	var uncapped string
	waitFor(t, 20*time.Second, func() bool {
		out, err := srv.TryRun("capture-pane", "-p", "-J", "-S", "-5000", "-t", pane)
		uncapped = out
		return err == nil && strings.Contains(out, tail)
	}, "the pane never printed its tail marker")

	// The fixture must genuinely exceed the cap, or this test is true by
	// accident forever after someone shrinks it. 400 000 is a literal floor
	// well above the 262 144 cap and well below the ~584 000 the fixture
	// prints.
	if len(uncapped) < 400000 {
		t.Fatalf("the uncapped capture is %d bytes, which does not exceed the cap by a "+
			"usable margin: the fixture needs more lines or a wider pane", len(uncapped))
	}
	if !strings.Contains(uncapped, head) {
		t.Fatalf("the head marker never reached the history, so its absence below would " +
			"prove nothing")
	}

	out, truncated, err := c.CaptureRange(ctx, pane, 5000)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated {
		t.Errorf("truncated = false after dropping %d bytes", len(uncapped)-len(out))
	}
	if !strings.Contains(out, tail) {
		t.Error("the newest line is missing: the cut took the tail, and the tail is the " +
			"answer the panel was opened to read")
	}
	if strings.Contains(out, head) {
		t.Error("the oldest line survived: the capture was not truncated from the top")
	}
	if len(out) > tmux.MaxCaptureBytes {
		t.Errorf("len = %d, over MaxCaptureBytes = %d", len(out), tmux.MaxCaptureBytes)
	}
	// ASCII only, so the rune walk moves the cut by nothing at all and the
	// length is exact.
	if len(out) != capBytes {
		t.Errorf("len = %d, want exactly %d: an all-ASCII capture cuts on a rune boundary "+
			"without moving", len(out), capBytes)
	}
}

// TestCaptureRangeCutsOnARuneBoundary sizes multi-byte content so that the cap
// lands INSIDE a rune, and asserts both halves: that a naive slice at that
// offset would be invalid UTF-8, and that what comes back is valid anyway.
// Without the first half the test passes against `s[len(s)-MaxCaptureBytes:]`.
func TestCaptureRangeCutsOnARuneBoundary(t *testing.T) {
	// 90 three-byte runes is 180 columns on a 200-column pane, so nothing
	// wraps, and 271 bytes per line. 1500 lines is ~406 500 bytes, over the
	// 262 144-byte cap and inside tmux's own 2000-line default history.
	line := strings.Repeat("漢", 90)
	srv, pane := captureFixture(t, 200, 50, 0,
		`i=1; while [ $i -le 1500 ]; do printf "%s\n" "`+line+`"; i=$((i+1)); done; exec cat`)

	c := tmux.NewClient(srv.Args())
	ctx := context.Background()

	read := func() string {
		t.Helper()
		var out string
		waitFor(t, 20*time.Second, func() bool {
			var err error
			out, err = srv.TryRun("capture-pane", "-p", "-J", "-S", "-5000", "-t", pane)
			return err == nil && len(out) > 400000
		}, "the pane never printed enough multi-byte content")
		return out
	}

	uncapped := read()
	// The fixture must genuinely exceed the cap, or the offset below is
	// negative and this test panics instead of reporting anything.
	if len(uncapped) <= tmux.MaxCaptureBytes {
		t.Fatalf("the uncapped capture is %d bytes against a cap of %d: nothing is "+
			"truncated, so there is no cut to land inside a rune",
			len(uncapped), tmux.MaxCaptureBytes)
	}
	// Where the cap falls. The fixture is padded a byte at a time until it
	// falls inside a rune rather than on one: each pad is one printable
	// character echoed back by the pane's cat, which grows the capture by
	// exactly two bytes (a newline and the character), so three tries cover
	// all three positions in a three-byte rune.
	for try := 0; ; try++ {
		if !utf8.RuneStart(uncapped[len(uncapped)-tmux.MaxCaptureBytes]) {
			break
		}
		if try == 2 {
			t.Fatalf("could not place the cap inside a rune after %d pads; the fixture "+
				"cannot prove anything about rune boundaries", try)
		}
		before := len(uncapped)
		srv.Run(t, "send-keys", "-t", pane, "x", "Enter")
		waitFor(t, 5*time.Second, func() bool {
			out, err := srv.TryRun("capture-pane", "-p", "-J", "-S", "-5000", "-t", pane)
			uncapped = out
			return err == nil && len(out) > before
		}, "the pad never reached the pane")
	}

	offset := len(uncapped) - tmux.MaxCaptureBytes
	if utf8.ValidString(uncapped[offset:]) {
		t.Fatalf("the naive slice at %d is already valid UTF-8; this fixture would pass "+
			"against a plain s[len(s)-MaxCaptureBytes:]", offset)
	}

	out, truncated, err := c.CaptureRange(ctx, pane, 5000)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated {
		t.Fatalf("truncated = false for a %d-byte capture", len(uncapped))
	}
	if !utf8.ValidString(out) {
		t.Error("the capture is not valid UTF-8: the cut split a rune")
	}
	if !strings.HasSuffix(uncapped, out) {
		t.Error("what came back is not the tail of the capture: the wrong end was kept")
	}
	// Cutting forward past the 1 or 2 remaining bytes of a split rune leaves
	// strictly fewer than the cap, and at most three fewer. 262 144 is
	// MaxCaptureBytes written out; see the byte-cap test.
	if len(out) >= 262144 || len(out) < 262141 {
		t.Errorf("len = %d, want just under 262144: the cut should walk forward to the "+
			"next rune and no further", len(out))
	}
}

// TestCaptureRangeRefusesABadPaneID: tmux resolves an empty target to "whatever
// is current" and exits 0, so an unvalidated id captures some other pane and
// the panel shows a plausible screen belonging to the wrong row.
func TestCaptureRangeRefusesABadPaneID(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "cap", "-x", "40", "-y", "5", "exec cat")

	// The premise: tmux itself would have succeeded. Without this the test
	// passes against a tmux that rejects empty targets on its own.
	if _, err := srv.TryRun("capture-pane", "-p", "-t", ""); err != nil {
		t.Fatalf("tmux refused an empty target on its own (%v); the guard under test is "+
			"no longer the thing standing between the browser and the wrong pane", err)
	}

	c := tmux.NewClient(srv.Args())
	for _, id := range []string{"", "3", "%", "%3x", "@3", "%1;kill-server"} {
		out, truncated, err := c.CaptureRange(context.Background(), id, 100)
		if err == nil {
			t.Errorf("CaptureRange(%q) succeeded, returning %q", id, out)
		}
		if out != "" || truncated {
			t.Errorf("CaptureRange(%q) = %q, %v on error; want empty and false", id, out, truncated)
		}
	}
}
