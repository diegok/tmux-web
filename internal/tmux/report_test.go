package tmux

import (
	"strings"
	"testing"
	"time"
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

func TestParseReport(t *testing.T) {
	now := time.UnixMilli(1789075200000)
	const ts = "1789075200000"

	for _, tc := range []struct {
		name string
		in   string
		want Report
		ok   bool
	}{
		{"four parts", "1;working;" + ts + ";run go", Report{StateWorking, 1789075200000, "run go"}, true},
		// The common shape. A reader demanding four parts rejects every Claude
		// report there has ever been.
		{"three parts is a state-only report", "1;idle;" + ts, Report{StateIdle, 1789075200000, ""}, true},
		{"blocked", "1;blocked;" + ts + ";Approve?", Report{StateBlocked, 1789075200000, "Approve?"}, true},
		// Only the first three separators are structural.
		{"semicolons in the text survive", "1;working;" + ts + ";a;b;c", Report{StateWorking, 1789075200000, "a;b;c"}, true},
		{"the text is sanitized on the way in", "1;working;" + ts + ";\x1b[31mred\x1fx", Report{StateWorking, 1789075200000, "red x"}, true},

		{"unset", "", Report{}, false},
		{"two parts", "1;idle", Report{}, false},
		{"unknown version", "2;idle;" + ts, Report{}, false},
		{"empty version", ";idle;" + ts, Report{}, false},
		// The fixture must START WITH "1", or it does not exercise the mutant it
		// is here for: strings.HasPrefix("01x", "1") is false, so a prefix-match
		// mutant rejects "01x" exactly as correct code does and survives the
		// whole table. Schema 10 is the case this will really be: it is a
		// different schema and must not be read as this one.
		{"a version that merely starts with ours", "10;idle;" + ts, Report{}, false},
		{"a version with a suffix", "1x;idle;" + ts, Report{}, false},
		{"unknown state", "1;thinking;" + ts, Report{}, false},
		{"empty state", "1;;" + ts, Report{}, false},
		// A state differing only in case is not the state. The writer is ours;
		// a value that is not exactly what we write did not come from us.
		{"state case", "1;Idle;" + ts, Report{}, false},
		{"non-decimal timestamp", "1;idle;later", Report{}, false},
		{"zero timestamp", "1;idle;0", Report{}, false},
		{"negative timestamp", "1;idle;-5", Report{}, false},
		// strconv does NOT refuse this on its own: ParseInt accepts a sign
		// prefix for every base, exactly as Atoi does. Measured, Go 1.26. The
		// plan claimed base 10 refused it; it does not, and this row was red
		// against the plan's own implementation.
		{"timestamp with a plus", "1;idle;+1789075200000", Report{}, false},
		// Discarded whole, not treated as stale: a future finishedAt is a done
		// badge `seen` can never catch up with.
		{"far future", "1;idle;1789075999000", Report{}, false},
		// A second of clock jitter on the one machine involved is not an attack.
		{"a moment in the future is tolerated", "1;idle;1789075201000", Report{StateIdle, 1789075201000, ""}, true},
		// The pair above leaves reportFutureSkew free to be anything from one
		// second to thirteen minutes -- neither row moves when the constant is
		// retargeted anywhere inside that range, so neither pins it. These two
		// are literal offsets from `now` either side of the boundary, and they
		// are the only thing that does. Found by mutation; change them when the
		// constant changes, deliberately.
		{"exactly the skew ahead is tolerated", "1;idle;1789075205000", Report{StateIdle, 1789075205000, ""}, true},
		{"one millisecond past the skew is discarded", "1;idle;1789075205001", Report{}, false},
		{"the past is fine -- freshness is not this function's job",
			"1;working;1000", Report{StateWorking, 1000, ""}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseReport(tc.in, now)
			if ok != tc.ok {
				t.Fatalf("ParseReport(%q) ok = %v, want %v (got %+v)", tc.in, ok, tc.ok, got)
			}
			if ok && got != tc.want {
				t.Errorf("ParseReport(%q) = %+v, want %+v", tc.in, got, tc.want)
			}
		})
	}
}

// The rune cap only applies to a field we have successfully parsed out, and
// nothing stops anything holding the tmux socket from storing a megabyte the
// daemon would then carry through every 1.5s poll.
func TestParseReportRefusesAnOversizeValue(t *testing.T) {
	now := time.UnixMilli(1789075200000)
	head := "1;working;1789075200000;"
	if _, ok := ParseReport(head+strings.Repeat("x", MaxReportBytes), now); ok {
		t.Fatal("a value over the byte cap must be discarded whole, before parsing")
	}
	// And the boundary is not off by one: a value at exactly the cap parses.
	// Both halves above are written against MaxReportBytes, so they hold for
	// any value of it -- retarget the constant at 128 or at 64 KiB and they
	// both still pass. They test the > against the >=, and nothing else.
	if _, ok := ParseReport(head+strings.Repeat("x", MaxReportBytes-len(head)), now); !ok {
		t.Fatal("a value at exactly the cap must still parse")
	}
	// So the size itself is pinned here, with literals, and this is the only
	// place it is. 1 KiB is the budget: an option value the daemon carries
	// through every poll, for every pane. Found by mutation.
	if _, ok := ParseReport(head+strings.Repeat("x", 1024), now); ok {
		t.Error("a 1048-byte value parsed: MaxReportBytes has been widened past 1 KiB")
	}
	if _, ok := ParseReport(head+strings.Repeat("x", 1024-len(head)), now); !ok {
		t.Error("a 1024-byte value was refused: MaxReportBytes has been narrowed below 1 KiB")
	}
}

func TestFormatReport(t *testing.T) {
	// A state-only report carries no trailing separator, because tmux would
	// strip it anyway and a reader written to expect it would then see three
	// parts where it wanted four.
	if got := FormatReport(StateIdle, 1789075200000, ""); got != "1;idle;1789075200000" {
		t.Errorf("state-only = %q, want no trailing separator", got)
	}
	if got := FormatReport(StateWorking, 1789075200000, "run go"); got != "1;working;1789075200000;run go" {
		t.Errorf("with text = %q", got)
	}
	// Text that sanitises to nothing is a state-only report, NOT an unset
	// option. Revision 1 of the design conflated those and thereby deleted the
	// whole Claude integration: Claude ships state-only, so every one of its
	// reports has an empty text field.
	if got := FormatReport(StateIdle, 1789075200000, "\x1b[0m\n"); got != "1;idle;1789075200000" {
		t.Errorf("empty-after-sanitising = %q, want a three-part report", got)
	}
	// Whatever it writes, it can read back. The writer checks its own shape
	// before the write, because a botched write does not clear a report: a
	// value of exactly ";" is refused by tmux with "empty value" and the option
	// KEEPS ITS PREVIOUS CONTENTS, which is the more dangerous of the two
	// outcomes -- a stale report preserved rather than a missing one.
	for _, text := range []string{"", "run go", "a;b", "trailing;", "✳ wide", "\x1b[31mred"} {
		v := FormatReport(StateWorking, 1789075200000, text)
		if _, ok := ParseReport(v, time.UnixMilli(1789075200000)); !ok {
			t.Errorf("FormatReport(%q) produced %q, which ParseReport rejects", text, v)
		}
	}
}

// The report format string is built the same way labelField is, from the same
// two constants. It is not a fourteenth snapshot field: Format's last slot is
// the label's, and a second unsanitized field anywhere but last produces a row
// that parses successfully with another pane's values in it.
func TestReportFormat(t *testing.T) {
	if strings.Contains(Format, AgentOption) {
		t.Fatal("@wterm_agent must not be in the snapshot format string: the last " +
			"slot is @wterm_label's, and any other slot shifts the record")
	}
	// The option is the last and only variable field of its own format, so it
	// gets the same three layers the label has.
	if reportFormatFields[len(reportFormatFields)-1] != reportField {
		t.Fatal("the report must be the last field of its own format")
	}
	// Built from the constants rather than retyped: a bracket set retyped from
	// a rendered "\n" covers neither target byte and turns every lowercase "n"
	// into a space.
	if !strings.Contains(reportField, "\n") || !strings.Contains(reportField, Sep) {
		t.Fatal("reportField's bracket set must hold the real newline and the real separator")
	}
	if strings.Contains(reportField, `\n`) {
		t.Fatal("reportField contains a two-character backslash-n: that leaves real " +
			"newlines alive AND puts a literal 'n' in the set, so every 'n' in a " +
			"benign value becomes a space")
	}
}

func TestParseReports(t *testing.T) {
	// One batch output: the snapshot block, then the report block. Each parser
	// owns one tag and ignores the other's lines.
	out := strings.Join([]string{
		rec("work", "$0", "work", "%1", "0", "", "@1", "1", "win", "1", "claude", "t", ""),
		rec("work", "$0", "work", "%2", "1", "", "@1", "1", "win", "0", "zsh", "t", ""),
		reportTag + Sep + "%1" + Sep + "1;working;1789075200000;run go",
		reportTag + Sep + "%2" + Sep + "",
	}, "\n")

	rows, dropped, err := ParseRows(out)
	if err != nil || dropped != 0 || len(rows) != 2 {
		t.Fatalf("ParseRows over batch output = %d rows, dropped %d, err %v; the "+
			"report block must be skipped, not counted as malformed", len(rows), dropped, err)
	}
	reports := ParseReports(out)
	if got := reports["%1"]; got != "1;working;1789075200000;run go" {
		t.Errorf("reports[%%1] = %q", got)
	}
	// Every pane gets a line, including panes with no integration. An empty
	// value is the same thing as no report -- the design's revision 1 said "a
	// pane with no line in the second call has no report", which described a
	// case that does not occur.
	if got, ok := reports["%2"]; !ok || got != "" {
		t.Errorf("reports[%%2] = %q, ok=%v; want an empty value present", got, ok)
	}
	// The exact key set, and it has to be the key SET.
	//
	// `if _, ok := reports["S"]; ok` is the assertion this wants to be and it
	// cannot fail: drop the tag check from ParseReports and a snapshot line
	// splits SplitN(line, Sep, 3) into ("S", "work", <the rest>), so it is keyed
	// "work" -- parts[1] -- and never "S". The mutant it exists for survives it.
	if len(reports) != 2 {
		t.Errorf("reports = %v, want exactly two entries, for %%1 and %%2: a third "+
			"entry keyed by a snapshot line's SECOND field is what a missing tag "+
			"check looks like", reports)
	}
}

// ParseRows' own tag check, which the batch fixture above cannot see: with the
// reportTag `continue` sitting above it, a report line is skipped either way.
// What the check is really for is a line that is neither block, and the only
// way to produce one is to write it.
//
// The fixture has a hidden requirement the plan does not state, and getting it
// wrong makes this test green against the mutant it names. ParseRows drops any
// record whose pane-index or window-index field will not go through Atoi, so a
// full-width record tagged "X" with placeholder text in those two slots is
// dropped under the mutant TOO. The indices have to be real integers, so that
// the tag is the ONLY thing standing between this line and a Row. It also
// cannot use rec, which prepends snapshotTag.
func TestParseRowsRefusesALineWithAnUnknownTag(t *testing.T) {
	// fieldCount fields, tag "X", and valid integers where ParseRows calls
	// Atoi -- field 5 (pane index) and field 8 (window index).
	line := strings.Join([]string{
		"X", "work", "$0", "work", "%1", "0", "", "@1", "1", "win", "1", "claude", "t", "",
	}, Sep)
	if n := len(strings.Split(line, Sep)); n != fieldCount {
		t.Fatalf("the fixture has %d fields, want %d: a record too short to be "+
			"accepted proves nothing about the tag check", n, fieldCount)
	}
	rows, dropped, err := ParseRows(line)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("ParseRows accepted a line tagged %q: %+v -- then the discriminator "+
			"is the field count, not the tag, which is the thing this design refuses "+
			"to rely on", "X", rows)
	}
	if dropped != 1 {
		t.Errorf("dropped = %d, want 1", dropped)
	}
}

// A report value that arrived with a separator in it -- which needs layer 1 to
// have failed open -- costs that one report and nothing else.
func TestParseReportsRejoinsASurplusSeparator(t *testing.T) {
	out := reportTag + Sep + "%1" + Sep + "1;working;1789075200000;a" + Sep + "b"
	if got := ParseReports(out)["%1"]; got != "1;working;1789075200000;a"+Sep+"b" {
		t.Errorf("got %q, want the value rejoined rather than truncated", got)
	}
	// ...and the daemon's own sanitizer is what makes it harmless.
	r, ok := ParseReport(ParseReports(out)["%1"], time.UnixMilli(1789075200000))
	if !ok || r.Activity != "a b" {
		t.Errorf("parsed %+v ok=%v, want the separator repaired to a space", r, ok)
	}
}
