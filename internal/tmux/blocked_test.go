package tmux

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// readFixture returns a real captured pane, byte for byte.
//
// The fixtures are captures, never hand-written screens: a detector tuned
// against an imagined dialog matches nothing that occurs in life, and every one
// of its tests passes. The two negatives are redacted -- every word of the
// developer's content replaced by a placeholder of the same length, so the
// borders, column alignment, wrapping and UI chrome survive intact while the
// content does not.
func readFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return string(b)
}

func TestIsBlocked(t *testing.T) {
	for _, tc := range []struct {
		file string
		want bool
	}{
		{"claude-idle.txt", false},
		{"claude-working.txt", false},
		{"claude-blocked.txt", true},
	} {
		screen := readFixture(t, tc.file)
		if got := IsBlocked("claude", screen); got != tc.want {
			t.Errorf("IsBlocked(%s) = %v, want %v", tc.file, got, tc.want)
		}
	}
}

// The reason blocked detection is strict: a false positive trains the owner to
// ignore the badge, which is worse than never having built it.
func TestIsBlockedIsStrict(t *testing.T) {
	for _, screen := range []string{
		"",
		"just some output\nnothing to see",
		// Prose that mentions the words but is not a dialog.
		"I could delete build/ but I will ask first. Do you want that?",
	} {
		if IsBlocked("claude", screen) {
			t.Errorf("false positive on %q", screen)
		}
	}
}

// The bottommost rule-delimited region is the live one. A dialog sitting above
// a fresh input box has been answered already, and reporting it as blocked
// would leave a badge lit on an agent nobody has to attend to.
//
// Composed from two real captures rather than invented: the situation is real
// but was not on any screen when the fixtures were taken, and composing real
// bytes is the closest honest approximation. Matching the topmost region
// instead of the bottommost passes every other test in this file.
func TestIsBlockedWantsTheBottommostRegion(t *testing.T) {
	answered := readFixture(t, "claude-blocked.txt") + readFixture(t, "claude-idle.txt")
	if !strings.Contains(answered, "❯ 1. ") {
		t.Fatal("composed screen lost the dialog it is supposed to contain")
	}
	if IsBlocked("claude", answered) {
		t.Error("a dialog above a live input box is answered, not blocked")
	}
}

// The choices must sit inside a rule-delimited region. Numbered lines and a
// question mark on their own are prose -- the idle fixture has three numbered
// items and a question in exactly that shape.
func TestIsBlockedWantsTheBorderedRegion(t *testing.T) {
	// The real dialog with its rules taken out, and nothing else changed.
	var kept []string
	for _, ln := range strings.Split(readFixture(t, "claude-blocked.txt"), "\n") {
		if trimmed := strings.TrimSpace(ln); trimmed != "" && strings.Trim(trimmed, "─╌") == "" {
			continue
		}
		kept = append(kept, ln)
	}
	unbordered := strings.Join(kept, "\n")
	if !strings.Contains(unbordered, "❯ 1. ") {
		t.Fatal("stripped screen lost the choices it is supposed to keep")
	}
	if IsBlocked("claude", unbordered) {
		t.Error("choices with no rule-delimited region are prose, not a dialog")
	}
}

// Rules are per agent. Claude Code's dialog is the only one anybody has
// captured, so it is the only one that can be recognised; running its rules
// against another agent's screen would be a guess wearing a badge.
func TestIsBlockedIsPerAgent(t *testing.T) {
	blocked := readFixture(t, "claude-blocked.txt")
	if !IsBlocked("claude", blocked) {
		t.Fatal("fixture must match for claude, or this test proves nothing")
	}
	for _, agent := range []string{"opencode", "pi", "zsh", ""} {
		if IsBlocked(agent, blocked) {
			t.Errorf("IsBlocked(%q, claude-blocked.txt) = true, want false: "+
				"no dialog rules exist for %q", agent, agent)
		}
	}
}

// dropLine removes the first line whose trimmed form has the given prefix, plus
// the wrapped continuation that follows it -- Claude Code's second choice runs
// onto a line that is not itself numbered.
//
// It fails the test if nothing matched, so a helper that quietly stops working
// cannot leave the assertions below passing against an unmodified screen.
func dropLine(t *testing.T, screen, prefix string) string {
	t.Helper()
	lines := strings.Split(screen, "\n")
	for i, line := range lines {
		if !strings.HasPrefix(strings.TrimSpace(line), prefix) {
			continue
		}
		end := i + 1
		for end < len(lines) {
			next := strings.TrimSpace(lines[end])
			if next == "" || choiceLine.MatchString(next) {
				break
			}
			end++ // a wrapped continuation belongs to the choice being removed
		}
		return strings.Join(append(append([]string{}, lines[:i]...), lines[end:]...), "\n")
	}
	t.Fatalf("no line starting %q to remove", prefix)
	return ""
}

var choiceLine = regexp.MustCompile(`^(?:❯ )?\d+\. `)

// Each marker the detector requires is load-bearing: take one away from the
// real dialog and the badge must go out. Without these, dropping any single
// requirement leaves every other test in this file passing.
//
// The screens are the real capture with lines removed, never lines invented --
// a restyle that drops a marker is exactly the shape these guard against.
func TestIsBlockedRequiresEveryMarker(t *testing.T) {
	dialog := readFixture(t, "claude-blocked.txt")

	noQuestion := dropLine(t, dialog, "Do you want")
	noCursor := strings.Replace(dialog, "❯ 1. ", "  1. ", 1)
	oneChoice := dropLine(t, dropLine(t, dialog, "2."), "3.")
	twoChoices := dropLine(t, dialog, "2.")

	for _, tc := range []struct {
		name   string
		screen string
		want   bool
	}{
		{"the capture itself", dialog, true},
		// A dialog with no question is a menu the user opened, not an agent
		// waiting on a decision it raised.
		{"no question line", noQuestion, false},
		// Numbered lines with no selection cursor are a list, not a menu. This
		// is also what an answered dialog looks like once it is re-rendered.
		{"no selection cursor", noCursor, false},
		// A decision has alternatives. One option is a list item.
		{"a single option", oneChoice, false},
		// Two options is a real permission dialog -- the third only appears
		// when "accept edits" applies -- so the cursor line must count as a
		// choice, or a yes/no prompt is missed.
		{"two options", twoChoices, true},
	} {
		// A transform that silently stopped working would leave the
		// assertion below running against the untouched capture.
		if tc.name != "the capture itself" && tc.screen == dialog {
			t.Fatalf("%s: transform changed nothing", tc.name)
		}
		if got := IsBlocked("claude", tc.screen); got != tc.want {
			t.Errorf("%s: IsBlocked = %v, want %v", tc.name, got, tc.want)
		}
	}
}
