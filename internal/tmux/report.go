package tmux

import (
	"regexp"
	"strings"
	"unicode"
)

// MaxActivity bounds an agent's activity line, in runes.
//
// It matches MaxLabel because it is the same row: PaneLines' second line, whose
// width budget MaxLabel was chosen for. Deliberately not herdr's 80, which is a
// column budget for a different row -- copying a magic number is copying the
// answer to somebody else's question. Deliberately not MaxTitle's 256 either,
// and note that the units differ: MaxTitle is a *byte* cap, because a title has
// been through tmux's own OSC parser before we see it, and this has been
// through nothing. A smaller cap on the less trustworthy field is the right way
// round.
const MaxActivity = MaxLabel

// ansiSequence matches the escape sequences an agent's own strings carry.
//
// Whole sequences, not the ESC byte: dropping the byte alone leaves "[31m" in
// the text as literal characters, which has been observed in this project. The
// three alternatives are CSI (ESC [ ... final byte), OSC (ESC ] ... BEL or ST),
// and the two-byte Fe forms. Anything this misses is still a control byte when
// SanitizeActivity reaches it, so the worst failure is a stray "[" on a row and
// never a live escape sequence on the wire.
var ansiSequence = regexp.MustCompile(
	"\x1b\\[[0-9;?]*[ -/]*[@-~]" + // CSI
		"|\x1b\\][^\x07\x1b]*(?:\x07|\x1b\\\\)" + // OSC, BEL- or ST-terminated
		"|\x1b[@-Z\\\\-_]") // two-byte Fe

// SanitizeActivity makes an agent's activity line safe to put in a tmux option,
// on the wire, and in the DOM.
//
// sanitizeLabel is the model and the rules are deliberately its rules, with two
// differences, both of them because this is a machine-written field rather than
// a human-typed one:
//
//   - Escape sequences are stripped first, as sequences. A label is typed; an
//     activity line is built from an agent's own output, which carries colour.
//   - Whitespace runs collapse. A control rune becomes a space here exactly as
//     it does in sanitizeLabel, and exactly as it does in tmux's own
//     substitution -- dropping a newline instead would weld "line one" and
//     "line two" into "line onetwo" -- so a multi-line fragment arrives as a
//     run of spaces and has to be closed up.
//
// An empty result is a legitimate outcome and means a state-only report, NOT an
// unset option. See FormatReport.
func SanitizeActivity(s string) string {
	s = ansiSequence.ReplaceAllString(s, "")

	var b strings.Builder
	b.Grow(len(s))
	pending := false // a whitespace run waiting to become one space
	runes := 0
	for _, r := range s {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			// Never a leading space: `runes > 0` is the trim of the front, and
			// a run that reaches the end is simply never written, which is the
			// trim of the back.
			pending = runes > 0
			continue
		}
		if pending {
			if runes == MaxActivity {
				break
			}
			b.WriteRune(' ')
			runes++
			pending = false
		}
		if runes == MaxActivity {
			break
		}
		b.WriteRune(r)
		runes++
	}
	return b.String()
}
