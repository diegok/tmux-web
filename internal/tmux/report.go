package tmux

import (
	"regexp"
	"strconv"
	"strings"
	"time"
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

// AgentOption is the per-pane tmux option an agent's integration writes.
//
// Deliberately NOT @wterm_label. That option is the *user's* field: PATCH
// /api/panes/{id} writes it, and AppSidebar.tsx's own comment on paneText says
// a label "wins outright, including over a title a program is rewriting
// underneath it -- that is the whole point of having one". An integration
// writing it would put a program back underneath the one field defined as being
// above programs: a rename would survive until the agent's next tool call, the
// agent's report would survive until the next rename, and both features would
// look intermittently broken with neither at fault.
const AgentOption = "@wterm_agent"

// ReportVersion is the schema this daemon understands.
//
// A report carrying anything else is ignored and the pane falls back to the
// screen classifier, which is the correct degradation and costs nothing: the
// integration is a file the user installed once and may not have updated, and
// the daemon reading it may be newer or older than it.
const ReportVersion = "1"

// MaxReportBytes bounds the whole option value, before parsing.
//
// The MaxActivity rune cap only applies to a field we have already parsed out,
// and nothing stops anything holding the tmux socket -- the same uid that can
// drive tmux directly -- from storing a megabyte the daemon would then carry
// through every 1.5s poll.
const MaxReportBytes = 1024

// reportFutureSkew is how far ahead of the daemon a report may be dated.
//
// It covers the clock jitter available on a single machine, which is the only
// machine involved. Over it the report is discarded WHOLE rather than treated
// as stale: stale is the right answer for a working report and the wrong one
// for a resting one, because a resting idle derives finishedAt from its own
// timestamp and a finishedAt in the future is a done badge no browser's `seen`
// marker can ever catch up with.
const reportFutureSkew = 5 * time.Second

// Report is one agent's statement about its own pane.
type Report struct {
	State     string // StateWorking, StateBlocked or StateIdle
	Timestamp int64  // unix ms; when the agent ENTERED this state, not when it wrote
	Activity  string // "" for a state-only report
}

// ParseReport reads an @wterm_agent value. ok is false for anything that is not
// a report this daemon wrote and understands -- including the empty value: an
// unset option and one set to "" both render as "" through #{@wterm_agent}, so
// the daemon cannot tell them apart and does not try. Both mean no report.
//
// Every field before the text has a shape that can be checked, and a value that
// fails any of those checks is discarded whole rather than repaired. A report
// we cannot parse is not a report we wrote.
func ParseReport(v string, now time.Time) (Report, bool) {
	if v == "" || len(v) > MaxReportBytes {
		return Report{}, false
	}
	// SplitN with 4: only the first three separators are structural, so the
	// text may contain semicolons freely.
	//
	// Three parts is valid and is the common case: tmux strips a trailing ";"
	// from an option value (measured on 3.7b; "--" does not help, because the
	// ";" is eaten by tmux's own command parser), so a state-only report is
	// stored as "1;idle;<ts>" however it was written.
	parts := strings.SplitN(v, ";", 4)
	if len(parts) < 3 || parts[0] != ReportVersion {
		return Report{}, false
	}
	switch parts[1] {
	case StateWorking, StateBlocked, StateIdle:
	default:
		return Report{}, false
	}
	// Digits and nothing else, checked before strconv sees it. The plan for
	// this task asserted that ParseInt with an explicit base 10 "refuses a
	// leading +"; it does not, and neither does Atoi -- strconv accepts a sign
	// prefix for every base, so both spell "+1789075200000" as a valid
	// timestamp. Measured on Go 1.26. This loop is what actually holds the
	// rule that the only thing that parses is the only thing our own writer
	// emits, and it takes "-5" and the empty string with it.
	for i := 0; i < len(parts[2]); i++ {
		if parts[2][i] < '0' || parts[2][i] > '9' {
			return Report{}, false
		}
	}
	// ParseInt rather than Atoi for the explicit bit size: Atoi is ParseInt at
	// the width of an int, and on a 32-bit build every 13-digit unix-ms value
	// is out of range there.
	ms, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || ms <= 0 || ms > now.Add(reportFutureSkew).UnixMilli() {
		return Report{}, false
	}
	r := Report{State: parts[1], Timestamp: ms}
	if len(parts) == 4 {
		// Re-sanitised on read, on the standing assumption that a writer's
		// promise is not a guarantee. This is the third of the same three
		// layers @wterm_label has: tmux's substitution takes the two
		// record-breaking bytes, the field's position as the only variable one
		// in its own format string bounds what a survivor could do, and this
		// repairs everything neither of those is a promise about -- C1
		// controls, a lone DEL, invalid UTF-8, and length.
		r.Activity = SanitizeActivity(parts[3])
	}
	return r, true
}

// FormatReport builds the value a writer stores.
//
// It never emits a *structural* trailing ";" -- the separator before an empty
// text field is left off rather than written for tmux to eat. (An activity line
// that itself ends in ";" still can, and it costs nothing: tmux strips that one
// byte and the value parses on the other side with one character less of text.)
// Escaping the separator as "\;" would also work and is rejected: it puts a
// shell-shaped escape into a value that is not going through a shell, and the
// next person to read it will not know whether the backslash is data.
func FormatReport(state string, ms int64, activity string) string {
	v := ReportVersion + ";" + state + ";" + strconv.FormatInt(ms, 10)
	if a := SanitizeActivity(activity); a != "" {
		v += ";" + a
	}
	return v
}

// reportField is #{@wterm_agent} with the two bytes that break this wire format
// substituted out by tmux before the value reaches Go.
//
// It is labelField's pattern, built from the same two constants -- COPIED FROM
// internal/tmux/snapshot.go, NOT RETYPED FROM ANY RENDERING OF IT. Three ways
// to get this wrong, all measured:
//
//   - [[:cntrl:]] does not survive tmux's own parse: the modifier's variable is
//     introduced by ":", so the ":" inside the class ends the pattern early and
//     the whole expression expands to "" for EVERY value, including good ones.
//   - A range such as [\x0a-\x1f] compiles but depends on the locale's
//     collation order, and the tmux server's locale is whatever started it.
//   - A bracket set retyped from a rendered "\n" is two characters, so it
//     leaves real newlines alive AND puts a literal 'n' in the set: every
//     lowercase "n" in a benign value becomes a space.
//
// The Go source works because "\n" in a Go string literal IS the byte.
const reportField = "#{s/[\n" + Sep + "]/ /:" + AgentOption + "}"

// reportFormatFields is the second -F of the batched read. The option is the
// last and only variable field, so it gets all three of the label's defences;
// #{pane_id} is %N and cannot carry anything.
var reportFormatFields = []string{reportTag, "#{pane_id}", reportField}

// ReportFormat is the -F argument for the report block.
var ReportFormat = strings.Join(reportFormatFields, Sep)

// ParseReports pulls the raw @wterm_agent value of every pane out of the
// batched read, keyed by pane id.
//
// Every pane gets a line, including one with no integration, whose value is the
// empty string -- which is the same thing as no report. There is no dropped
// count and there should not be one: the blast radius here is a report, never a
// pane, so a line that will not parse costs one pane its state and nothing
// costs the sidebar a row.
func ParseReports(out string) map[string]string {
	reports := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		// SplitN with 3 for the same reason ParseRows rejoins its surplus: a
		// separator that survived the substitution belongs to the value.
		parts := strings.SplitN(line, Sep, 3)
		if len(parts) != 3 || parts[0] != reportTag {
			continue
		}
		reports[parts[1]] = parts[2]
	}
	return reports
}
