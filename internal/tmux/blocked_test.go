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

// --- question extraction ----------------------------------------------------

// The exact text and the exact choices, against the real capture. Asserting
// only "not nil" would pass for a parser that returned the footer as the
// question and the file preview as a choice.
func TestExtractQuestion(t *testing.T) {
	q := ExtractQuestion("claude", readFixture(t, "claude-blocked.txt"))
	if q == nil {
		t.Fatal("no question extracted from a screen that IsBlocked matches")
	}
	if want := "Do you want to create fixture.txt?"; q.Text != want {
		t.Errorf("Text = %q, want %q", q.Text, want)
	}
	// The second choice wraps onto a line that is NOT numbered. Counting
	// numbered lines makes that invisible to IsBlocked; extraction has to
	// rejoin it, or the tooltip shows a sentence cut off mid-clause and a
	// phantom fourth choice made of its tail.
	want := []string{
		"Yes",
		"Yes, and switch to accept edits (auto-approve file edits and common file commands) for this session (shift+tab)",
		"No",
	}
	if len(q.Choices) != len(want) {
		t.Fatalf("Choices = %q, want %q", q.Choices, want)
	}
	for i := range want {
		if q.Choices[i] != want[i] {
			t.Errorf("Choices[%d] = %q, want %q", i, q.Choices[i], want[i])
		}
	}
}

// dropBlankBefore removes the blank line immediately above the first line whose
// trimmed form has the given prefix. It fails the test if there was not one, so
// it cannot quietly stop modifying the screen.
func dropBlankBefore(t *testing.T, screen, prefix string) string {
	t.Helper()
	lines := strings.Split(screen, "\n")
	for i, line := range lines {
		if !strings.HasPrefix(strings.TrimSpace(line), prefix) {
			continue
		}
		if i == 0 || strings.TrimSpace(lines[i-1]) != "" {
			t.Fatalf("no blank line above %q to remove", prefix)
		}
		return strings.Join(append(append([]string{}, lines[:i-1]...), lines[i:]...), "\n")
	}
	t.Fatalf("no line starting %q", prefix)
	return ""
}

// The footer is not part of the last choice.
//
// On the real capture a blank line separates them, so the join rule is never
// asked the question. Take the blank away -- one line, and exactly what a
// dialog drawn one row shorter looks like -- and the only thing between "No"
// and "Esc to cancel · Tab to amend" is the column a wrapped continuation has
// to reach. Recognising the footer by its wording instead is not an option:
// blocked.go calls it the most style-volatile line on the screen and the
// detector deliberately refuses to depend on it.
func TestExtractQuestionDoesNotSwallowTheFooter(t *testing.T) {
	tight := dropBlankBefore(t, readFixture(t, "claude-blocked.txt"), "Esc to cancel")
	q := ExtractQuestion("claude", tight)
	if q == nil {
		t.Fatal("removing one blank line must not stop the dialog parsing")
	}
	if n := len(q.Choices); n != 3 {
		t.Fatalf("Choices = %q, want the same three", q.Choices)
	}
	if last := q.Choices[2]; last != "No" {
		t.Errorf("last choice = %q, want %q: the footer is chrome, not an option", last, "No")
	}
}

// Extraction failing must not take the state with it: the badge is
// load-bearing and this text is a convenience, so a grammar that stops matching
// a restyled dialog has to yield nothing rather than something wrong.
//
// The plan named a `claude-blocked-unparseable.txt` fixture -- a screen that
// IsBlocked still matches but extraction cannot read -- and no such screen can
// be derived from the capture. IsBlocked needs a cursored choice, two lines
// matching `^(?:❯ )?\d+\. ` and a line ending in "?"; that trailing space means
// a line only counts as a choice while it still has text on it, and this
// capture's only "?" is the question itself, sitting above the choices. So
// every deletion that defeats extraction -- of the question, of the options, of
// their text -- defeats detection too. Rather than invent a screen, this pins
// that coincidence: the `blocked` column is the claim, and if a future edit to
// the detector ever makes one of these blockable, this test says so and the
// missing fixture becomes recordable.
func TestExtractionFailureKeepsTheState(t *testing.T) {
	dialog := readFixture(t, "claude-blocked.txt")

	// The options with their text taken away: the numbers, the cursor and the
	// question all survive, which is the shape a capture landing mid-redraw
	// has. IsBlocked stops matching because a bare "1." is not a choice.
	emptied := regexp.MustCompile(`(?m)^(\s*(?:❯ )?\d+\. ).*$`).ReplaceAllString(dialog, "$1")

	for _, tc := range []struct {
		name    string
		screen  string
		blocked bool
	}{
		{"choices with no text", emptied, false},
		{"no question line", dropLine(t, dialog, "Do you want"), false},
		{"a single option", dropLine(t, dropLine(t, dialog, "2."), "3."), false},
		// Numbered lines and a question with no rule above them are prose.
		{"no dialog region at all", "Do you want to?\n1. Yes\n2. No", false},
		// An answered dialog above a live input box. Extraction reads the same
		// bottommost region the detector does, so it quotes nothing here --
		// reporting the lower box's state while quoting the upper box's text
		// would be worse than quoting nothing at all.
		{"a dialog already answered", dialog + readFixture(t, "claude-idle.txt"), false},
	} {
		if tc.screen == dialog {
			t.Fatalf("%s: transform changed nothing", tc.name)
		}
		if got := IsBlocked("claude", tc.screen); got != tc.blocked {
			t.Errorf("%s: IsBlocked = %v, want %v", tc.name, got, tc.blocked)
		}
		if q := ExtractQuestion("claude", tc.screen); q != nil {
			t.Errorf("%s: want no question rather than a wrong one, got %+v", tc.name, q)
		}
	}
}

// Extraction is per agent for the same reason detection is: the grammar was
// written against one agent's screen and running it on another's is a guess.
func TestExtractQuestionIsPerAgent(t *testing.T) {
	blocked := readFixture(t, "claude-blocked.txt")
	for _, agent := range []string{"opencode", "pi", "zsh", ""} {
		if q := ExtractQuestion(agent, blocked); q != nil {
			t.Errorf("ExtractQuestion(%q) = %+v, want nil: no rules exist for %q", agent, q, agent)
		}
	}
}
