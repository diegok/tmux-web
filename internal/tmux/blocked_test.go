package tmux

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
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

// --- opencode ---------------------------------------------------------------

// opencode's box shares nothing with Claude Code's: no rules, no numbered
// options, no question mark -- a left gutter and a header that says outright
// what it wants. These pin that it is matched on those terms.
func TestIsBlockedOpencode(t *testing.T) {
	for _, tc := range []struct {
		file string
		want bool
	}{
		{"opencode-idle.txt", false},
		{"opencode-blocked.txt", true},
	} {
		if got := IsBlocked("opencode", readFixture(t, tc.file)); got != tc.want {
			t.Errorf("IsBlocked(opencode, %s) = %v, want %v", tc.file, got, tc.want)
		}
	}
}

// dropGutterLine removes the one line containing the given text and nothing
// else. dropLine cannot serve here: it also swallows the unnumbered line below
// the one it removes, which is Claude Code's wrapped continuation and
// opencode's request line. It fails the test if nothing matched, so it cannot
// quietly leave the assertions below running against an unmodified screen.
func dropGutterLine(t *testing.T, screen, text string) string {
	t.Helper()
	lines := strings.Split(screen, "\n")
	for i, line := range lines {
		if !strings.Contains(line, text) {
			continue
		}
		return strings.Join(append(append([]string{}, lines[:i]...), lines[i+1:]...), "\n")
	}
	t.Fatalf("no line containing %q to remove", text)
	return ""
}

// The gutter is not the signal. opencode draws every block the same way -- the
// idle input box and each user message included -- so a rule that fired on the
// gutter would light the badge on an agent sitting at its prompt.
//
// The screen here is the real dialog with its header line taken out and nothing
// else changed: the gutter, the diff preview and the whole "Allow once / Allow
// always / Reject" row survive it.
func TestIsBlockedOpencodeWantsTheHeader(t *testing.T) {
	dialog := readFixture(t, "opencode-blocked.txt")
	headless := dropGutterLine(t, dialog, "△ Permission required")
	if headless == dialog {
		t.Fatal("transform changed nothing")
	}
	if !strings.Contains(headless, "Allow once") {
		t.Fatal("stripped screen lost the option row it is supposed to keep")
	}
	if IsBlocked("opencode", headless) {
		t.Error("a guttered block with options but no header is not a permission box")
	}
}

// The header has to be the block's own title, not a line inside it.
//
// The input box is a guttered block, and text pasted into it is that block's
// content -- so an operator pasting a log that carries the words on a line of
// its own would light the badge on an agent that is waiting for the enter key.
// Matching the header anywhere in the block passes every other test in this
// file, this one included until the pasted line was a whole line.
//
// Composed from the idle capture's own bytes: its gutter, its text column, its
// box, with two lines at the prompt instead of one. The situation is real and
// was not on screen when the fixture was taken, and the fixture's text is a
// redacted placeholder already.
func TestIsBlockedOpencodeWantsTheHeaderAtTheTopOfTheBlock(t *testing.T) {
	idle := readFixture(t, "opencode-idle.txt")
	var pasted []string
	for _, line := range strings.Split(idle, "\n") {
		if col := strings.Index(line, "Ask anything…"); col >= 0 {
			gutter := line[:col]
			pasted = append(pasted, gutter+"why does this fail", gutter+"Permission required")
			continue
		}
		pasted = append(pasted, line)
	}
	screen := strings.Join(pasted, "\n")
	if screen == idle {
		t.Fatal("transform changed nothing")
	}
	if !strings.Contains(screen, "Permission required") {
		t.Fatal("composed screen lost the words it is supposed to contain")
	}
	if IsBlocked("opencode", screen) {
		t.Error("the words pasted into the prompt box are content, not a title")
	}
}

// The bottommost block is the live one, for the same reason the bottommost
// rule-delimited region is for Claude Code -- and here the point is sharper,
// because opencode's input box is itself a guttered block, so an answered
// permission box sits directly above one.
//
// Composed from two real captures: the situation is real but was not on screen
// when either fixture was taken.
func TestIsBlockedOpencodeWantsTheBottommostBlock(t *testing.T) {
	answered := readFixture(t, "opencode-blocked.txt") + readFixture(t, "opencode-idle.txt")
	if !strings.Contains(answered, "△ Permission required") {
		t.Fatal("composed screen lost the dialog it is supposed to contain")
	}
	if IsBlocked("opencode", answered) {
		t.Error("a permission box above a live input box is answered, not blocked")
	}
}

// Words on a screen are not a dialog. Without the gutter there is no block, and
// without a block there is nothing to be the title of.
func TestIsBlockedOpencodeIsStrict(t *testing.T) {
	for _, screen := range []string{
		"",
		"just some output\nnothing to see",
		"△ Permission required\n→ Edit fixture.txt\n  Allow once   Allow always   Reject",
		"Permission required",
	} {
		if IsBlocked("opencode", screen) {
			t.Errorf("false positive on %q", screen)
		}
	}
}

// A block's first line is its title only when the whole of it is the title.
//
// The input box is a guttered block like any other, and whatever the operator
// has typed into it is that block's first line -- so words matched loosely
// inside it would light the badge on an agent that is waiting for nothing but
// the enter key. This is the screen that costs, and the reason the header is
// anchored at both ends.
//
// The idle capture with something else typed at its prompt: the fixture's text
// is a redacted placeholder already, so one placeholder becomes another and
// nothing structural moves.
func TestIsBlockedOpencodeWantsTheWholeLine(t *testing.T) {
	idle := readFixture(t, "opencode-idle.txt")
	typed := strings.Replace(idle,
		`Ask anything… "Fix a TODO in the codebase"`,
		`Ask anything… "Permission required for the deploy"`, 1)
	if typed == idle {
		t.Fatal("transform changed nothing")
	}
	if IsBlocked("opencode", typed) {
		t.Error("words typed into the prompt box are not a permission box")
	}
}

// Each agent's rules stay on that agent's screen. Run any fixture through
// another's grammar and nothing must match -- which is the whole reason the
// detector keeps a table per agent instead of one heuristic broad enough to
// cover every shape, and broad enough to fire on prose.
//
// Every ordered pair, not a chosen few: pi's overlay carries 319 of the rune
// Claude Code draws its rules with, and opencode's request line begins with the
// arrow pi highlights an option with, so the pairs that could plausibly cross
// are not the ones that look alike from a distance.
func TestBlockedRulesDoNotCrossAgents(t *testing.T) {
	agents := Agents
	screens := map[string]string{}
	for _, a := range agents {
		screens[a] = readFixture(t, a+"-blocked.txt")
		if !IsBlocked(a, screens[a]) {
			t.Fatalf("%s-blocked.txt does not match its own agent, so this test proves nothing", a)
		}
	}
	for _, a := range agents {
		for _, b := range agents {
			if a == b {
				continue
			}
			if IsBlocked(a, screens[b]) {
				t.Errorf("IsBlocked(%s, %s-blocked.txt) = true: that is a guess, not a match", a, b)
			}
			if q := ExtractQuestion(a, screens[b]); q != nil {
				t.Errorf("ExtractQuestion(%s, %s-blocked.txt) = %+v, want nil", a, b, q)
			}
		}
	}
	// The idle captures too: an agent sitting at its prompt is not blocked
	// under anybody's rules, and pi's idle screen carries 200 of Claude Code's
	// rule rune.
	for _, file := range []string{"claude-idle.txt", "opencode-idle.txt", "pi-idle.txt"} {
		screen := readFixture(t, file)
		for _, a := range agents {
			if IsBlocked(a, screen) {
				t.Errorf("IsBlocked(%s, %s) = true on an idle screen", a, file)
			}
		}
	}
}

// A command with no rules entry is never blocked.
//
// `pi` used to be the case this test was written for: it had no capture, so it
// had no rules. It has both now, so what remains is the general property --
// nothing that is not in the table can ever be reported blocked, whatever is on
// its screen.
//
// Every agent on the Agents list must have an entry or be named here, so that
// an agent added to that list without rules is a decision somebody makes rather
// than a badge that silently never lights.
func TestAgentWithNoRulesIsNeverBlocked(t *testing.T) {
	for _, agent := range Agents {
		if _, ok := blockedRules[agent]; !ok {
			t.Errorf("%s is a known agent with no blocked rules: it can never report "+
				"blocked. That may be right -- inventing a grammar for an uncaptured "+
				"screen is worse -- but it should be a deliberate gap, not a surprise.", agent)
		}
	}
	for _, agent := range []string{"zsh", "nvim", "claude-helper", ""} {
		for _, file := range []string{"claude-blocked.txt", "opencode-blocked.txt", "pi-blocked.txt"} {
			screen := readFixture(t, file)
			if IsBlocked(agent, screen) {
				t.Errorf("IsBlocked(%q, %s) = true, want false: no rules exist for %q", agent, file, agent)
			}
			if q := ExtractQuestion(agent, screen); q != nil {
				t.Errorf("ExtractQuestion(%q, %s) = %+v, want nil", agent, file, q)
			}
		}
	}
}

// The request, exactly, and no choices at all.
//
// Asserting "not nil" would pass for a parser that quoted the header, the diff
// preview or the key hints. The empty Choices is the deliberate half of this:
// opencode's options share their row with "⇆ select  enter confirm" and nothing
// but a wider run of spaces separates them, on the one capture that exists.
func TestExtractQuestionOpencode(t *testing.T) {
	q := ExtractQuestion("opencode", readFixture(t, "opencode-blocked.txt"))
	if q == nil {
		t.Fatal("no question extracted from a screen that IsBlocked matches")
	}
	if want := "Edit fixture.txt"; q.Text != want {
		t.Errorf("Text = %q, want %q", q.Text, want)
	}
	if len(q.Choices) != 0 {
		t.Errorf("Choices = %q, want none: the option row cannot be told from the "+
			"key hints beside it, and no choices beats wrong ones", q.Choices)
	}
}

// Extraction failing must not take the state with it -- and unlike Claude Code,
// opencode has a screen that shows it. Its badge comes from the header and its
// quote from the request line below, so losing the second leaves the first
// standing: the blocked-but-unquotable capture the plan called for and the
// Claude fixture could not produce.
//
// The composed screen also carries an arrow inside the file preview, which is
// what a grammar that read past the request line would quote in its place.
func TestExtractQuestionOpencodeFailureKeepsTheState(t *testing.T) {
	dialog := readFixture(t, "opencode-blocked.txt")
	restyled := dropGutterLine(t, dialog, "→ Edit fixture.txt")
	restyled = strings.Replace(restyled, "1 + hello", "1 + → an arrow in the file", 1)

	for _, tc := range []struct {
		name    string
		screen  string
		blocked bool
	}{
		// A restyled arrow, or a capture landing mid-redraw. The header is
		// untouched, so the badge is untouched.
		{"no request line", restyled, true},
		// An answered box above a live input box. Extraction reads the same
		// bottommost block the detector does, so it quotes nothing here.
		{"a box already answered", dialog + readFixture(t, "opencode-idle.txt"), false},
	} {
		if tc.screen == dialog {
			t.Fatalf("%s: transform changed nothing", tc.name)
		}
		if got := IsBlocked("opencode", tc.screen); got != tc.blocked {
			t.Errorf("%s: IsBlocked = %v, want %v", tc.name, got, tc.blocked)
		}
		if q := ExtractQuestion("opencode", tc.screen); q != nil {
			t.Errorf("%s: want no question rather than a wrong one, got %+v", tc.name, q)
		}
	}
}

// Nothing on the screen bounds a question. `capture-pane -J` rejoins a question
// wrapped across rows, and the claude extractor rejoins a choice's continuation
// lines the same way, so one line of a wide pane is one long string -- and this
// one rides every 1.5s poll into the sidebar and the tooltip. MaxTitle exists
// for exactly this hazard through the other field; the design's own wire
// contract says "the request, one line, truncated".
//
// The cap counts RUNES, and the padding here is multi-byte on purpose: a byte
// cap cuts a question to half its length in any language that is not English,
// and a byte cap applied without care leaves half a rune on the wire. Both
// assertions below are needed -- the length one alone passes for a naive
// `s[:n]` whenever the bytes happen to land on a boundary, which depends on the
// text and so is right most of the time.
func TestExtractQuestionIsTruncated(t *testing.T) {
	// An odd-length prefix, so that a naive byte slice lands mid-rune rather
	// than getting away with it.
	long := strings.Repeat("ñ", MaxQuestion*2)
	screen := strings.Replace(readFixture(t, "claude-blocked.txt"),
		"Do you want to create fixture.txt?", "Do you want"+long+"?", 1)
	screen = strings.Replace(screen, "1. Yes", "1. "+long, 1)

	q := ExtractQuestion("claude", screen)
	if q == nil {
		t.Fatal("a long question stopped the dialog parsing")
	}
	if n := utf8.RuneCountInString(q.Text); n != MaxQuestion {
		t.Errorf("Text is %d runes, want %d: the cap is a column budget, so it "+
			"counts runes, not bytes", n, MaxQuestion)
	}
	if !utf8.ValidString(q.Text) {
		t.Errorf("Text is not valid UTF-8: the cut fell inside a rune")
	}
	if len(q.Choices) != 3 {
		t.Fatalf("Choices = %q, want the fixture's three", q.Choices)
	}
	// The choices are capped too. They are the same rejoined-line hazard, they
	// ride the same poll, and the tooltip is not a place to put a screenful.
	if n := utf8.RuneCountInString(q.Choices[0]); n != MaxQuestion {
		t.Errorf("Choices[0] is %d runes, want %d", n, MaxQuestion)
	}
	if !utf8.ValidString(q.Choices[0]) {
		t.Errorf("Choices[0] is not valid UTF-8: the cut fell inside a rune")
	}
	// A question that fits is untouched -- a cap that trimmed everything would
	// pass every assertion above.
	if q := ExtractQuestion("claude", readFixture(t, "claude-blocked.txt")); q == nil ||
		q.Text != "Do you want to create fixture.txt?" {
		t.Errorf("the real fixture's question came back as %+v, want it whole", q)
	}
}

// The cap lives in ExtractQuestion, not in one grammar, so that an agent added
// to the rules table later cannot forget it. opencode's extractor is the other
// implementation and never counts anything itself; if the cap were moved into
// the claude one this is the test that says so.
func TestExtractQuestionOpencodeIsTruncated(t *testing.T) {
	long := strings.Repeat("ñ", MaxQuestion*2)
	screen := strings.Replace(readFixture(t, "opencode-blocked.txt"),
		"→ Edit fixture.txt", "→ Edit"+long, 1)

	q := ExtractQuestion("opencode", screen)
	if q == nil {
		t.Fatal("a long request stopped the box parsing")
	}
	if n := utf8.RuneCountInString(q.Text); n != MaxQuestion {
		t.Errorf("Text is %d runes, want %d", n, MaxQuestion)
	}
	if !utf8.ValidString(q.Text) {
		t.Errorf("Text is not valid UTF-8: the cut fell inside a rune")
	}
}

// --- pi ---------------------------------------------------------------------

// pi is different in kind from the other two: it has no permission dialog at
// all. It asks through a tool, and the tool renders a two-pane selector overlay
// -- a closed box, a filter, a numbered list on the left and a description of
// the highlighted option on the right.
//
// Measured over the three captures, no two agents share a structural signal:
//
//	          corners  rules ─  gutter ┃  box │  numbered
//	claude          0      100          0       0          3
//	opencode        0        0         18       0          0
//	pi              4      319          0      60          4
//
// pi is the only one that draws corners, and the only one drawing a closed box;
// claude's 100 rules and pi's 319 are the same rune doing different jobs. That
// is why the detector keeps one rule set per agent rather than one heuristic.
func TestIsBlockedPi(t *testing.T) {
	for _, tc := range []struct {
		file string
		want bool
	}{
		{"pi-idle.txt", false},
		{"pi-blocked.txt", true},
	} {
		if got := IsBlocked("pi", readFixture(t, tc.file)); got != tc.want {
			t.Errorf("IsBlocked(pi, %s) = %v, want %v", tc.file, got, tc.want)
		}
	}
}

// Each marker is load-bearing: take one away from the real capture and the
// badge must go out. The screens are the capture with lines removed or one
// substitution made, never lines invented.
func TestIsBlockedPiRequiresEveryMarker(t *testing.T) {
	overlay := readFixture(t, "pi-blocked.txt")

	// The walls without the corners. Every interior line survives -- the
	// filter, the numbered list, the cursor, the key hints -- and pi draws
	// │ down the side of a box that is not this one, so on its own it says
	// nothing.
	noBox := dropGutterLine(t, dropGutterLine(t, overlay, "╭"), "╰")
	// A box whose top is off the screen is not a box this detector reads. It
	// is also what the bottom half of a scrolled overlay looks like.
	noTop := dropGutterLine(t, overlay, "╭")
	noBottom := dropGutterLine(t, overlay, "╰")
	// A list with nothing highlighted is not a menu awaiting a keystroke. The
	// column is preserved so that nothing else on the line moves.
	noCursor := strings.Replace(overlay, "→ 1. PostgreSQL", "  1. PostgreSQL", 1)
	oneChoice := dropGutterLine(t, dropGutterLine(t, dropGutterLine(t,
		overlay, "2. SQLite"), "3. MySQL / MariaDB"), "4. MongoDB")
	twoChoices := dropGutterLine(t, dropGutterLine(t, overlay, "3. MySQL / MariaDB"), "4. MongoDB")

	for _, tc := range []struct {
		name   string
		screen string
		want   bool
	}{
		{"the capture itself", overlay, true},
		{"walls but no corners", noBox, false},
		{"no top border", noTop, false},
		{"no bottom border", noBottom, false},
		{"no selection cursor", noCursor, false},
		{"a single option", oneChoice, false},
		// Two is a real question -- yes/no is the commonest shape there is --
		// so the cursor line has to count as an option or those are missed.
		{"two options", twoChoices, true},
	} {
		if tc.name != "the capture itself" && tc.screen == overlay {
			t.Fatalf("%s: transform changed nothing", tc.name)
		}
		if got := IsBlocked("pi", tc.screen); got != tc.want {
			t.Errorf("%s: IsBlocked = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The key hints are chrome, not the signal.
//
// "type filter • ↑↓ navigate • esc clear/cancel" is the most style-volatile
// line in the overlay and the most tempting thing to match, and matching it
// anywhere on the screen would light the badge on any pane whose output quotes
// pi's own help -- pi's own README, this repository's design document, a
// terminal recording being reviewed. The words are here with the box taken
// away, and with the box present but nothing highlighted.
func TestIsBlockedPiDoesNotMatchTheKeyHints(t *testing.T) {
	hints := "  type filter • PgUp/PgDn prompt • backspace erase • ↑↓ navigate • alt+o hide • enter\n" +
		"  select • esc clear/cancel • ctrl+c cancel"
	for _, screen := range []string{
		hints,
		"→ 1. PostgreSQL\n  2. SQLite\n" + hints,
		readFixture(t, "pi-idle.txt") + "\n" + hints,
	} {
		if IsBlocked("pi", screen) {
			t.Errorf("false positive on a screen quoting the key hints: %q", screen)
		}
	}
}

// The overlay pi draws is the only closed box on its screen, and it is the box
// that is the signal -- not the rune it is drawn with. pi's status bar carries
// a │ of its own, its output routinely carries table borders, and neither is a
// question being asked.
func TestIsBlockedPiIsStrict(t *testing.T) {
	for _, screen := range []string{
		"",
		"just some output\nnothing to see",
		// Walls and numbered options, no corners: a table, or wrapped output.
		"│ → 1. PostgreSQL │\n│   2. SQLite     │",
		// Corners and options but no cursor: a menu with nothing selected, and
		// what an answered overlay looks like on its way out.
		"╭──────╮\n│ 1. PostgreSQL │\n│ 2. SQLite │\n╰──────╯",
		// A box drawn around prose that happens to number things.
		"╭─ notes ─╮\n│ 1. write the parser │\n│ 2. write the tests │\n╰─────────╯",
		// Numbers referred to INSIDE a sentence, one of them after an arrow.
		// A line is an option when the whole line is one, not when it mentions
		// one -- which is why both patterns are anchored. Unanchored, this box
		// reads as a menu with two options and a cursor on the second.
		"╭─ notes ─╮\n│ step 1. install the deps │\n│ then → 2. run the tests │\n╰─────────╯",
	} {
		if IsBlocked("pi", screen) {
			t.Errorf("false positive on %q", screen)
		}
	}
}

// The bottommost box is the live one, for the reason the other two agents have
// the same rule: a screen can hold a box that has been answered above the one
// that has not.
//
// Composed from the two real captures: pi's idle screen has no box at all, so
// the overlay stacked above it is the shape of an overlay that is gone.
func TestIsBlockedPiWantsTheBottommostBox(t *testing.T) {
	idle := readFixture(t, "pi-idle.txt")
	// The idle capture with a box drawn at the bottom of it, so that the
	// screen holds two: the answered overlay above, a boxed thing below.
	below := "╭─ notes ─╮\n│ nothing to decide │\n╰───────────────────╯"
	answered := readFixture(t, "pi-blocked.txt") + idle + "\n" + below
	if !strings.Contains(answered, "→ 1. PostgreSQL") {
		t.Fatal("composed screen lost the overlay it is supposed to contain")
	}
	if IsBlocked("pi", answered) {
		t.Error("an overlay above a later box is answered, not blocked")
	}
}

// The exact text and the exact options, against the real capture.
//
// "Not nil" would pass for a parser that quoted the filter line, the key hints,
// or the description of the highlighted option in the pane beside the list --
// and for one that left "1. " on the front of every option.
func TestExtractQuestionPi(t *testing.T) {
	q := ExtractQuestion("pi", readFixture(t, "pi-blocked.txt"))
	if q == nil {
		t.Fatal("no question extracted from a screen that IsBlocked matches")
	}
	if want := "Which database should this project use?"; q.Text != want {
		t.Errorf("Text = %q, want %q", q.Text, want)
	}
	// Four options, and not the fifth thing on the list: the unnumbered "Type
	// something. — Enter a custom response" entry is an escape hatch with no
	// answer in it, and it wraps onto a second line that a looser rule would
	// quote as a sixth.
	want := []string{"PostgreSQL", "SQLite", "MySQL / MariaDB", "MongoDB"}
	if len(q.Choices) != len(want) {
		t.Fatalf("Choices = %q, want %q", q.Choices, want)
	}
	for i := range want {
		if q.Choices[i] != want[i] {
			t.Errorf("Choices[%d] = %q, want %q", i, q.Choices[i], want[i])
		}
	}
}

// The question is the nearest line that asks above the options, not the first
// one in the box.
//
// pi's box holds a whole prompt -- a preamble, a context section, a bulleted
// trade-off summary -- and any of those lines can end in a question mark. The
// screen here is the real capture with one character changed, a colon into a
// question mark, on a line the prompt already has; taking the first match
// instead of the nearest passes every other test in this file.
func TestExtractQuestionPiTakesTheNearestQuestionAboveTheOptions(t *testing.T) {
	overlay := readFixture(t, "pi-blocked.txt")
	two := strings.Replace(overlay, "Quick trade-off summary:", "Quick trade-off summary?", 1)
	if two == overlay {
		t.Fatal("transform changed nothing")
	}
	q := ExtractQuestion("pi", two)
	if q == nil {
		t.Fatal("a second question mark stopped the box parsing")
	}
	if want := "Quick trade-off summary?"; q.Text != want {
		t.Errorf("Text = %q, want %q: the nearest line above the options is the "+
			"one being answered", q.Text, want)
	}
}

// An option is not the question, wherever its text happens to end.
//
// The question is looked for above the options for a reason, and the option
// text is arbitrary -- pi's tool supplies it -- so an option that ends in a
// question mark is an ordinary screen, not a contrived one. Searching the whole
// box instead of the part above the options passes every other test in this
// file, and quotes "4. MongoDB?" as the question being asked.
//
// One substitution of the capture's own bytes, and the line keeps its length so
// that nothing in the box moves.
func TestExtractQuestionPiDoesNotQuoteAnOption(t *testing.T) {
	overlay := readFixture(t, "pi-blocked.txt")
	asking := strings.Replace(overlay, "4. MongoDB ", "4. MongoDB?", 1)
	if asking == overlay {
		t.Fatal("transform changed nothing")
	}
	q := ExtractQuestion("pi", asking)
	if q == nil {
		t.Fatal("an option ending in a question mark stopped the box parsing")
	}
	if want := "Which database should this project use?"; q.Text != want {
		t.Errorf("Text = %q, want %q", q.Text, want)
	}
	if len(q.Choices) != 4 || q.Choices[3] != "MongoDB?" {
		t.Errorf("Choices = %q, want the same four with the last one asking", q.Choices)
	}
}

// Extraction failing must not take the state with it, and pi has a real screen
// that shows it rather than a composed one: it scrolls the prompt inside its
// own box -- the capture carries the ↓ indicator that proves it -- so a long
// question can be off the top of the box while the options are still on it.
// The badge comes from the menu and the quote from the prompt, so losing the
// second leaves the first standing.
func TestExtractQuestionPiFailureKeepsTheState(t *testing.T) {
	overlay := readFixture(t, "pi-blocked.txt")
	// The prompt scrolled past its question: the line that asks is gone, the
	// context and the options are not.
	scrolled := dropGutterLine(t, overlay, "Which database should this project use?")
	// One option left. The detector says this is a list rather than a menu, and
	// extraction has to agree: quoting a question whose answers were not read
	// puts a decision in the tooltip that the pane is not offering.
	oneChoice := dropGutterLine(t, dropGutterLine(t, dropGutterLine(t,
		overlay, "2. SQLite"), "3. MySQL / MariaDB"), "4. MongoDB")

	// An answered overlay with a later box below it. Extraction reads the same
	// bottommost box the detector does, so it quotes nothing here -- reporting
	// the lower box's state while quoting the upper box's text would be worse
	// than quoting nothing at all.
	answered := overlay + readFixture(t, "pi-idle.txt") +
		"\n╭─ notes ─╮\n│ nothing to decide │\n╰───────────────────╯"

	for _, tc := range []struct {
		name    string
		screen  string
		blocked bool
	}{
		{"the question scrolled out of the box", scrolled, true},
		{"a single option", oneChoice, false},
		// No box at all: nothing to be blocked on and nothing to quote.
		{"the overlay is gone", readFixture(t, "pi-idle.txt"), false},
		{"an overlay already answered", answered, false},
	} {
		if tc.screen == overlay {
			t.Fatalf("%s: transform changed nothing", tc.name)
		}
		if got := IsBlocked("pi", tc.screen); got != tc.blocked {
			t.Errorf("%s: IsBlocked = %v, want %v", tc.name, got, tc.blocked)
		}
		if q := ExtractQuestion("pi", tc.screen); q != nil {
			t.Errorf("%s: want no question rather than a wrong one, got %+v", tc.name, q)
		}
	}
}

// --- the form registry -------------------------------------------------------

// markerDialog is a form nobody has captured: it matches a screen holding a
// literal marker.
//
// Test-only, and deliberately trivial. What the multi-form tests are about is
// the REGISTRY -- that every form an agent has is asked, and that the matching
// one is the one quoted -- and a second real grammar would test a grammar.
type markerDialog struct{ marker string }

func (d markerDialog) isBlocked(screen string) bool { return strings.Contains(screen, d.marker) }

// extractQuestion quotes UNCONDITIONALLY, including on a screen this form does
// not match, and that is the point of it.
//
// Every shipped grammar happens to fail closed on a screen it was not written
// for, so a registry that asked each form for text without first asking whether
// it is the form on screen would look correct against all three -- and would
// quote the wrong dialog the day one of them is less shy. This is the form that
// is less shy.
func (d markerDialog) extractQuestion(string) *Question {
	return &Question{Text: d.marker + " is waiting"}
}

// registerTestAgent installs a throwaway agent and its forms for one test.
//
// A throwaway rather than a second claude form, because claude has exactly one
// and promoting a second is a decision with a capture behind it (open question
// 10), not something a test gets to make. The registry is a package var, so it
// is restored on the way out and no test in this package runs in parallel.
func registerTestAgent(t *testing.T, name string, forms ...*Form) {
	t.Helper()
	if _, taken := blockedRules[name]; taken {
		t.Fatalf("%q is a real agent: pick a name nothing ships", name)
	}
	agents := Agents
	blockedRules[name] = forms
	Agents = append(append([]string{}, Agents...), name)
	t.Cleanup(func() {
		delete(blockedRules, name)
		Agents = agents
	})
}

// Every form carries a non-empty, unique identifier, and FormsFor answers per
// agent.
//
// internal/report's blocked mappings hold these forms by pointer, so a name
// nobody has can no longer be written down at all. What a pointer does NOT
// settle is which agent a form belongs to -- every Form is reachable from every
// package that imports this one -- and rule 2 only ever asks the pane's own
// agent's grammars about the pane's screen. So FormsFor is the check that
// matters now, and this is where it is held to the registry.
func TestEveryRegisteredFormHasAUniqueID(t *testing.T) {
	seen := map[string]string{}
	for agent, forms := range blockedRules {
		if len(forms) == 0 {
			t.Errorf("%s is registered with no forms at all: it can never report blocked", agent)
		}
		for _, f := range forms {
			if f.ID == "" {
				t.Errorf("%s has a form with no id", agent)
				continue
			}
			if other, dup := seen[f.ID]; dup {
				t.Errorf("form id %q is registered for both %s and %s", f.ID, other, agent)
			}
			seen[f.ID] = agent
		}
	}
	// The three the design names, each under ITS OWN agent. A form that moved
	// to another agent's list, or that a mapping reached for across agents, is
	// a badge rule 2 deletes about 4.5 seconds after a client connects.
	for _, tc := range []struct {
		agent string
		form  *Form
	}{
		{"claude", ClaudePermissionForm},
		{"opencode", OpencodePermissionForm},
		{"pi", PiSelectorForm},
	} {
		if !registeredFor(tc.agent, tc.form) {
			t.Errorf("%s is not one of %s's registered forms", tc.form.ID, tc.agent)
		}
		// And not any other agent's, which is the half a membership test over
		// every agent at once could never fail.
		for _, other := range Agents {
			if other != tc.agent && registeredFor(other, tc.form) {
				t.Errorf("%s is registered for %s as well", tc.form.ID, other)
			}
		}
	}
	// An agent nobody has captured a screen for has no forms, and an unknown
	// name is not an error: both answer "nothing can confirm blocked here".
	for _, agent := range []string{"", "claude-helper", "vim"} {
		if got := FormsFor(agent); len(got) != 0 {
			t.Errorf("FormsFor(%q) = %v, want none", agent, got)
		}
	}
}

// registeredFor is FormsFor's answer for one form, by identity.
func registeredFor(agent string, want *Form) bool {
	for _, f := range FormsFor(agent) {
		if f == want {
			return true
		}
	}
	return false
}

// An agent with several forms is blocked when ANY of them matches, and the
// question comes from the one that matched rather than from the first in the
// list.
//
// Today every agent has one form and this is untestable against the shipped
// registry, which is why it is tested against a throwaway one: the day a second
// claude screen is promoted, "does the first grammar match" and "is this agent
// waiting" stop being the same question, and a standing report on the second
// screen must not be treated as a screen with nothing on it.
func TestIsBlockedMatchesAnyRegisteredForm(t *testing.T) {
	registerTestAgent(t, "twoform",
		&Form{ID: "twoform/first", dialog: markerDialog{marker: "FIRST FORM"}},
		&Form{ID: "twoform/second", dialog: markerDialog{marker: "SECOND FORM"}})

	second := "an elicitation-shaped screen\nSECOND FORM\nwaiting on you"
	if blockedRules["twoform"][0].dialog.isBlocked(second) {
		t.Fatal("the first form matches the second form's screen, so this test proves nothing")
	}
	if !IsBlocked("twoform", second) {
		t.Error("IsBlocked asked only the first registered form")
	}
	q := ExtractQuestion("twoform", second)
	if q == nil || q.Text != "SECOND FORM is waiting" {
		t.Errorf("ExtractQuestion = %+v, want the quote from the form that MATCHED", q)
	}
	if !IsBlocked("twoform", "FIRST FORM\nstill on screen") {
		t.Error("IsBlocked stopped matching the first form once a second was registered")
	}
	// And nothing has become loose: a screen matching neither form is not
	// blocked, which is what rule 2 is entitled to act on.
	if IsBlocked("twoform", "an ordinary prompt\nnothing to decide") {
		t.Error("a screen matching no registered form reported blocked")
	}
}
