package tmux

import (
	"slices"
	"strings"
	"testing"
)

// The clamp is invisible to a capture test: tmux clamps a start line to the
// history it actually has, so asking for 4000 lines of a 40-line pane returns
// the same bytes as asking for 500. Only the argument list can tell the two
// apart, which is why this one test reads the args rather than the output.
func TestCaptureRangeArgsClampTheStartLine(t *testing.T) {
	for _, tc := range []struct {
		name  string
		lines int
		want  string
	}{
		{"a depth inside the range is passed through", 4000, "-4000"},
		{"over the maximum is clamped to it", 9000, "-5000"},
		{"the maximum itself is not clamped", 5000, "-5000"},
		{"zero becomes one line", 0, "-1"},
		{"a negative depth becomes one line", -7, "-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := captureRangeArgs("%3", tc.lines)

			i := slices.Index(args, "-S")
			if i < 0 || i == len(args)-1 {
				t.Fatalf("no -S in %q", args)
			}
			if args[i+1] != tc.want {
				t.Errorf("-S %s, want %s (from lines=%d): %q", args[i+1], tc.want, tc.lines, args)
			}
			// A start line counted from the top of the history rather than
			// back from the screen returns the wrong end of the scrollback,
			// and tmux reports no error for it.
			if !strings.HasPrefix(args[i+1], "-") {
				t.Errorf("-S %s is positive; it must count back from the screen", args[i+1])
			}
		})
	}
}

// truncateHeadAtRuneBoundary keeps the tail, and its sibling keeps the head.
// The two directions are one edit apart, so the boundary cases are pinned here
// where the exact bytes are visible rather than through a tmux fixture.
func TestTruncateHeadAtRuneBoundary(t *testing.T) {
	for _, tc := range []struct {
		name     string
		s        string
		maxBytes int
		want     string
	}{
		{"under the cap is untouched", "abcdef", 6, "abcdef"},
		{"the newest bytes are the ones kept", "abcdef", 3, "def"},
		{"a cut inside a rune walks forward to the next one", "aaéé", 3, "é"},
		{"a cut already on a rune boundary does not move", "aaéé", 4, "éé"},
		{"nothing survives a zero cap", "abc", 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := truncateHeadAtRuneBoundary(tc.s, tc.maxBytes); got != tc.want {
				t.Errorf("truncateHeadAtRuneBoundary(%q, %d) = %q, want %q",
					tc.s, tc.maxBytes, got, tc.want)
			}
		})
	}
}

// The flag list, pinned where the output cannot pin it.
//
// -N is the trap the plan warned about from the other side: it is not a
// numeric start line, it means "preserve trailing spaces" -- and on 3.7b, -J
// already does that, so `-p -J -N` and `-p -J` produce byte-identical output
// (286 bytes both ways on a 40-column fixture; without -J the same capture is
// 284 against 452 padded). A -N smuggled into a -J capture is therefore
// invisible to every assertion about the text, and this is the only test that
// can see it. -e is visible in the output and is asserted there as well; it is
// here so that both halves of "plain text only" read in one place.
func TestCaptureRangeArgsCarryNoPaddingOrEscapes(t *testing.T) {
	args := captureRangeArgs("%3", 1000)
	for _, flag := range []string{"-N", "-e"} {
		if slices.Contains(args, flag) {
			t.Errorf("%s must not be passed: %q", flag, args)
		}
	}
	// -J is the one flag whose presence is required: it rejoins a line the
	// pane wrapped, which is why -N has nothing left to do.
	if !slices.Contains(args, "-J") {
		t.Errorf("-J is missing: %q", args)
	}
}

// The cap's boundary, which no tmux fixture can hit on purpose: a capture is
// whatever tmux happens to hold.
//
// These fixtures ARE sized from MaxCaptureBytes, deliberately and unlike the
// integration test's: what is under test here is the relationship -- at the cap
// nothing goes, one byte over and exactly one byte goes -- which is true at
// whatever value the constant takes. The VALUE is pinned separately, by the
// literal 262 144 in TestCaptureRangeCapsBytesAndTruncatesFromTheTop.
func TestCapCaptureBoundary(t *testing.T) {
	t.Run("exactly at the cap nothing is dropped", func(t *testing.T) {
		s := strings.Repeat("a", MaxCaptureBytes)
		got, truncated := capCapture(s)
		if truncated {
			t.Error("truncated = true for a capture that lost nothing")
		}
		if len(got) != MaxCaptureBytes {
			t.Errorf("len = %d, want %d: the capture was cut at the cap itself",
				len(got), MaxCaptureBytes)
		}
	})

	t.Run("one byte over drops exactly one byte", func(t *testing.T) {
		s := strings.Repeat("a", MaxCaptureBytes) + "z"
		got, truncated := capCapture(s)
		if !truncated {
			t.Error("truncated = false after dropping a byte")
		}
		if len(got) != MaxCaptureBytes {
			t.Errorf("len = %d, want %d", len(got), MaxCaptureBytes)
		}
		if got != s[1:] {
			t.Error("the byte that went was not the oldest one")
		}
	})
}
