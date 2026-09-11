package tmux

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// The sanitizer as data. Every row is a trap somebody has already fallen into,
// here or in the design.
func TestSanitizeActivity(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		// Sequences go as sequences. Dropping the ESC byte alone leaves the
		// literal "[31m" behind, which has been observed in this project.
		{"csi colour", "\x1b[31mred\x1b[0m", "red"},
		{"csi cursor move", "a\x1b[2Kb", "ab"},
		{"osc title set, BEL terminated", "x\x1b]0;title\x07y", "xy"},
		{"osc, ST terminated", "x\x1b]0;title\x1b\\y", "xy"},
		// The third alternative in ansiSequence has to have a row too, or
		// deleting it from the pattern changes nothing any test can see. A
		// two-byte Fe goes whole: without the branch the ESC survives as a
		// control and spaces the word open ("a b"), which is the wrong answer
		// for a non-printing control function.
		{"two-byte fe", "a\x1bMb", "ab"},
		// The two bytes that break a snapshot record. tmux substitutes both to
		// spaces on the way out; this is the layer that does not depend on that
		// pattern still compiling.
		{"unit separator", "EV\x1fIL", "EV IL"},
		{"a newline welds two words if it is dropped rather than spaced",
			"line one\nline two", "line one line two"},
		// C1, built from its code point rather than typed. tmux's own checks
		// elsewhere are byte-oriented and let these through, which validateLabel
		// already had to learn.
		{"c1 control", "a" + string(rune(0x9f)) + "b", "a b"},
		{"del", "a\x7fb", "a b"},
		// Collapse and trim, after the substitutions, or a line of controls
		// becomes a row of blanks.
		{"collapse and trim", "  a\t\t\tb  ", "a b"},
		{"nothing but controls", "\x1b[0m\x00\x1f\n", ""},
		{"benign text is untouched", "running go test ./internal/tmux", "running go test ./internal/tmux"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := SanitizeActivity(tc.in); got != tc.want {
				t.Errorf("SanitizeActivity(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSanitizeActivityBounds(t *testing.T) {
	// The cap assertions below all compare against MaxActivity, so they say
	// nothing about what MaxActivity *is*: retargeting the constant at
	// MaxTitle moves both sides of every one of them and the suite stays
	// green. Found by mutation. This is the only line that pins the choice.
	if MaxActivity != MaxLabel {
		t.Fatalf("MaxActivity = %d, want MaxLabel (%d): it is a rune budget for the same sidebar row, not MaxTitle's %d-byte budget for a field tmux has already parsed", MaxActivity, MaxLabel, MaxTitle)
	}
	// Runes, not bytes, and the rune has to be one the cap does not divide
	// evenly or the test is vacuous. MaxActivity is 128 and the star is 3
	// bytes, so a byte cap lands mid-rune.
	got := SanitizeActivity(strings.Repeat("✳", 4000))
	if n := utf8.RuneCountInString(got); n != MaxActivity {
		t.Fatalf("kept %d runes, want exactly %d", n, MaxActivity)
	}
	if !utf8.ValidString(got) {
		t.Fatal("truncation split a rune")
	}
	// Invalid UTF-8 becomes U+FFFD rather than riding onto the wire, exactly as
	// sanitizeLabel does it -- ranging over a string yields RuneError per bad
	// byte and WriteRune re-encodes it, so what the browser is told is what the
	// daemon holds. encoding/json would make the same substitution later, where
	// nothing bounds it.
	if got := SanitizeActivity("a\xffb"); got != "a�b" {
		t.Errorf("invalid UTF-8 = %q, want a U+FFFD in the middle", got)
	}
	// The same cap, against an input whose whitespace falls exactly on it.
	// Every other input here is one unbroken run, which leaves the cap check
	// inside the collapse branch untested -- and without that check the space
	// is written as rune 129, the equality below it never matches again, and
	// the result runs to the end of the input with no bound at all. Found by
	// mutation.
	//
	// Built rather than repeated, and against the constant rather than a
	// literal: whether a repeated "ab " happens to put its gap on the cap
	// depends on MaxActivity modulo 3, so that version would stop exercising
	// this branch, silently, the day somebody changes the cap. This one puts
	// the gap on the boundary whatever the cap is.
	boundary := strings.Repeat("a", MaxActivity) + " " + strings.Repeat("b", MaxActivity)
	if got := SanitizeActivity(boundary); got != strings.Repeat("a", MaxActivity) {
		t.Errorf("whitespace on the cap kept %d runes (%.20q...), want exactly %d and nothing after them", utf8.RuneCountInString(got), got, MaxActivity)
	}
	// A 10 KiB value is bounded by the same cap. It cannot reach here from our
	// own writer; it can from anything else holding the tmux socket.
	if n := utf8.RuneCountInString(SanitizeActivity(strings.Repeat("x", 10<<10))); n != MaxActivity {
		t.Errorf("10 KiB input kept %d runes, want %d", n, MaxActivity)
	}
}
