package tmux

import (
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Sep is the field separator used in tmux -F format strings. It is a literal
// 0x1f byte: tmux does not expand "\x1f" inside a format string, and a tab is
// unsafe because window names may contain one.
const Sep = "\x1f"

// snapshotTag and reportTag label the two blocks of the one batched read.
//
// A literal constant in each format string, so nothing a writer controls can
// forge one: a report containing a newline would have to survive tmux's
// substitution first, and that substitution turns it into a space. Telling the
// blocks apart by counting fields or by trusting their order would both be
// guesses about a value somebody else writes.
const (
	snapshotTag = "S"
	reportTag   = "A"
)

// fieldCount is how many fields a record must have to be read. It is a
// minimum, not an equality: the label is the last field and may contain the
// separator, so a record can legitimately arrive with more. See ParseRows.
//
// Fourteen rather than thirteen since the batched read: the block tag is field
// 0, which every positional index below is offset by.
const fieldCount = 14

// MaxTitle bounds a pane title. tmux normalises control bytes out of titles but
// does not cap length; an 8KB title was observed stored and reported in full,
// and it would ride a 1.5s poll into the DOM.
const MaxTitle = 256

// Row is one pane as reported by tmux, before deduplication.
//
// The JSON names are the wire contract with the frontend; without the tags Go
// would marshal the exported Go names instead.
type Row struct {
	GroupKey string `json:"groupKey"` // session_group, falling back to session_name
	// SessionID and SessionName are separate from GroupKey because they differ
	// after a rename: session_group keeps the pre-rename name, so the group key
	// is not an address and not a display name. Operations target the id.
	SessionID   string `json:"sessionId"`   // $N; what management operations target
	SessionName string `json:"sessionName"` // live name, for display
	PaneID      string `json:"paneId"`      // e.g. "%3", stable for the pane's lifetime
	PaneIndex   int    `json:"paneIndex"`   // position within the window, in layout order
	AppOwned    bool   `json:"appOwned"`    // set from the @wterm_web user option
	// Label is @wterm_label, and it may be "": the user never set one, or what
	// they set sanitised away to nothing. It is the only field here whose value
	// tmux hands over exactly as written, by anything holding the socket --
	// from v2 that includes third-party agent integrations -- so it is the only
	// one ParseRows repairs rather than trusts.
	Label string `json:"label"`
	// WindowID is @N, and it is what window operations target -- the same
	// reason SessionID is here rather than a name. WindowIndex is a position,
	// not an address: tmux renumbers indices on move-window and reuses them
	// after a kill, so a rename or a kill addressed by index can land on a
	// different window than the one the sidebar was showing. Without this
	// field the browser cannot name a window at all, and PATCH/DELETE
	// /api/windows/{id} -- which validate an @N id -- are unreachable.
	WindowID    string `json:"windowId"`
	WindowIndex int    `json:"windowIndex"` // display order within the session
	WindowName  string `json:"windowName"`
	PaneActive  bool   `json:"paneActive"`
	Command     string `json:"command"`
	Title       string `json:"title"` // tmux-sanitised, truncated
	// Activity is what the agent's own integration says it is doing. "" when no
	// integration is installed, when its report is stale, and for every pane
	// that is not an agent. Sanitised and capped on write and again on read.
	Activity string `json:"activity"`
	// StateSource is which authority decided AgentState: "event", "screen", or
	// "" when nothing did.
	//
	// It is on the wire mainly so that tests can see it. A report and the
	// classifier agreeing on "working" is indistinguishable from the precedence
	// being backwards, and a test that cannot distinguish them is a test that
	// will stay green through the rewrite that breaks it -- exactly as v2's
	// blocked override hid finishedAt.
	//
	// The UI may use it for a tooltip and must NOT branch the row's appearance
	// on it: two visibly different kinds of state dot teach the user to trust
	// one and ignore the other. That prohibition has a test of its own; see
	// Task 11.
	StateSource string `json:"stateSource"`
	// AgentState is "working", "idle" or "blocked", and "" for anything the
	// daemon did not compute a state for: a pane that is not a known agent, a
	// poll taken with no browser connected, and a capture that failed. The
	// frontend never decides what counts as an agent -- it renders what is
	// here, and "" is not a state.
	AgentState string `json:"agentState"`
	// FinishedAt is the unix-ms timestamp of this pane's most recent
	// working->idle edge, or 0 if it has not had one under the current
	// classifier. The browser compares it against its own per-device memory of
	// what it has already looked at, which is why it is a timestamp rather than
	// a "done" flag: "done" would have to be cleared by somebody.
	FinishedAt int64 `json:"finishedAt"`
	// Question is what a blocked agent is waiting on. It is a pointer and
	// omitempty because it is absent far more often than present -- every
	// non-agent pane, every agent that is not blocked, and every blocked agent
	// whose dialog the grammar could not read. Its absence never means the
	// pane is not blocked: AgentState is the state, this is a convenience.
	Question *Question `json:"question,omitempty"`
}

// labelField is #{@wterm_label} with the two bytes that break this wire format
// substituted out by tmux, before the value ever reaches Go.
//
// tmux's s/// modifier is a POSIX regex substitution over the variable's value,
// applied to every match, and its pattern can carry the raw bytes: probed on
// tmux 3.7b, a label of "a\x1fb\nc" reports as "a b c" through this expression
// and as itself through a bare #{@wterm_label}. The pattern is a bracket SET of
// the two literal bytes rather than a range or a class, deliberately:
//
//   - [[:cntrl:]] does not survive tmux's own parse. The modifier's variable is
//     introduced by ":", so the ":" inside the class terminates the pattern
//     early; measured, the whole expression then expands to "" for every value,
//     including a label with nothing wrong with it.
//   - A range such as [\x0a-\x1f] compiles, but POSIX leaves the endpoints of a
//     bracket range to the locale's collation order, and the tmux server's
//     locale is whatever started it. A set of literal bytes has no such
//     freedom.
//
// A space rather than "": "EV\x1fIL" reads as "EV IL", which shows the label
// was tampered with, where "EVIL" would read as a label somebody chose.
//
// This is the first of three defences, and the only one that can fail open: a
// pattern that stopped compiling would leave tmux echoing the value untouched
// and exiting 0 (measured with a deliberately broken "[" pattern). That is why
// the label also sits in the last field, and why ParseRows sanitises what
// arrives. TestFormatAloneKeepsEveryRecordWellFormed pins this layer on its own
// terms, against a real server -- with a tolerant parser behind it, a pattern
// that quietly stopped covering one of the two bytes produces identical rows,
// so no test downstream of the parser can see it weaken.
const labelField = "#{s/[\n" + Sep + "]/ /:" + LabelOption + "}"

// formatFields are the -F fields in the order ParseRows indexes them. It is a
// slice rather than one concatenated constant so that the field count is
// something a test can count directly: labelField contains a raw Sep of its
// own, inside a regex, so counting separators in the finished string no longer
// tells you how many fields there are.
//
// pane_current_path is deliberately absent. tmux sanitizes session and window
// names but not the path, so a pane sitting in a directory whose name contains
// a 0x1f or a newline can forge a whole extra record or swallow the following
// pane's -- either way the sidebar shows something other than the truth, and a
// pane that exists can vanish from it. Being the last field would not bound the
// damage: a newline simply starts a fresh line whose every field is
// attacker-controlled. tmux's #{q:} modifier does not escape either byte.
// Nothing in v1 renders the path; the deferred git panel can query it per pane,
// where a single-pane result needs no field splitting to interpret.
//
// pane_current_command is a theoretical residual: it is not known to be
// sanitized either, and two attempts to make tmux report a command containing a
// newline failed, but that is not a proof that it cannot happen.
//
// pane_title is safe for the opposite reason to the path: tmux normalises a
// title through its own OSC parser, so a title set to "EVIL\x1fFORGED\nMORE"
// reads back as one line with the control bytes gone. It used to hold the last
// slot for that reason; the label needs it more.
//
// @wterm_label is the one field tmux will hand over exactly as somebody wrote
// it, and from v2 that somebody includes third-party agent integrations. It is
// last so that the damage a raw byte can do is bounded by arithmetic rather
// than by the sanitiser holding: a 0x1f only adds fields past the end, which
// ParseRows rejoins, and a newline only truncates the last field, leaving the
// twelve before it -- the whole identity of the pane -- already complete on the
// line. Any other position turns both into a shifted or unparseable record,
// which is a pane missing from the sidebar.
var formatFields = []string{
	snapshotTag,
	"#{?#{session_group},#{session_group},#{session_name}}",
	"#{session_id}",
	"#{session_name}",
	"#{pane_id}",
	"#{pane_index}",
	"#{@wterm_web}",
	"#{window_id}",
	"#{window_index}",
	"#{window_name}",
	"#{pane_active}",
	"#{pane_current_command}",
	"#{pane_title}",
	labelField,
}

// Format is the -F argument producing rows this package can parse.
var Format = strings.Join(formatFields, Sep)

// ParseRows parses raw `tmux list-panes` output into one Row per line.
//
// Any line that is not a well-formed record -- too few fields, or a non-numeric
// index -- is skipped and counted in dropped. Lines are independent: a
// malformed one never merges into or alters a neighbouring row. dropped is
// returned rather than logged so the caller can surface a snapshot that is
// quietly losing panes instead of it passing unnoticed.
//
// More fields than expected is NOT malformed. The surplus can only come from a
// separator inside a field value, and the label -- the one field tmux does not
// sanitize -- is last, so the surplus is rejoined into it. Dropping the row
// instead would lose the pane, which is worse than showing it with a label that
// has a stray byte in it; and the alternative reading, that some earlier field
// grew a separator, would shift every field after it and mislabel the pane
// anyway.
//
// The error is always nil today. It is part of the signature because Snapshot
// calls this in a context where an error is the natural shape.
func ParseRows(out string) (rows []Row, dropped int, err error) {
	out = strings.TrimSuffix(out, "\n")
	if out == "" {
		return nil, 0, nil
	}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Split(line, Sep)
		// The other block of the batched read. Skipped rather than counted:
		// ParseReports owns those lines, and counting them as malformed would
		// log "skipped malformed rows" once per pane per poll forever.
		if len(fields) > 0 && fields[0] == reportTag {
			continue
		}
		// The tag is the discriminator, not the field count. A line that is
		// neither block is malformed however many fields it happens to have.
		if len(fields) < fieldCount || fields[0] != snapshotTag {
			dropped++
			continue
		}
		// Distinct names: shadowing the named err return here would be
		// harmless today only because it is always nil.
		pidx, perr := strconv.Atoi(fields[5])
		widx, werr := strconv.Atoi(fields[8])
		if perr != nil || werr != nil {
			dropped++
			continue
		}
		rows = append(rows, Row{
			GroupKey:    fields[1],
			SessionID:   fields[2],
			SessionName: fields[3],
			PaneID:      fields[4],
			PaneIndex:   pidx,
			AppOwned:    fields[6] == "1",
			WindowID:    fields[7],
			WindowIndex: widx,
			WindowName:  fields[9],
			PaneActive:  fields[10] == "1",
			Command:     fields[11],
			Title:       truncateAtRuneBoundary(fields[12], MaxTitle),
			// Rejoined with the separator it was split on, so a label that
			// arrived with a raw 0x1f in it is reconstructed rather than
			// silently reassembled into something else.
			Label: sanitizeLabel(strings.Join(fields[fieldCount-1:], Sep)),
		})
	}
	return rows, dropped, nil
}

// sanitizeLabel makes an arbitrary @wterm_label value safe to put on the wire
// and in the DOM, and bounds it.
//
// The last of the three defences, and the only one that runs on bytes that have
// already reached Go. It exists because neither of the others is a promise
// about content: the tmux-side substitution removes exactly the two bytes that
// break the record, and the field's position stops those two from removing a
// pane -- neither says anything about a C1 control, a lone 0x7f, invalid UTF-8,
// or a label of a length that would ride the poll into the sidebar every 1.5s.
//
// The rules are validateLabel's, applied instead of refused: what SetLabel
// rejects on the way in is what this repairs on the way out, so there is one
// notion of a safe label rather than two.
//
//   - Every control rune becomes a space. unicode.IsControl, not a byte range,
//     for the reason validateLabel gives: it is what catches the C1 controls
//     that tmux's own byte-oriented check lets through.
//   - Invalid UTF-8 becomes U+FFFD. Ranging over a string yields RuneError per
//     bad byte and WriteRune re-encodes it, so what the browser is told is what
//     the daemon holds. encoding/json would substitute the same rune silently
//     and later, where nothing bounds it.
//   - MaxLabel runes, counted rather than sliced: a byte cut lands mid-rune for
//     any width that does not divide the cap. Runes, not bytes, because MaxLabel
//     is the writer's cap and it is a column budget.
//
// The result is trimmed, so a label of nothing but dangerous bytes degrades to
// "" -- an unlabelled pane, which is what SetLabel stores for a label of
// whitespace -- rather than to a row titled with blanks.
func sanitizeLabel(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	runes := 0
	for _, r := range s {
		if runes == MaxLabel {
			break
		}
		if unicode.IsControl(r) {
			r = ' '
		}
		b.WriteRune(r)
		runes++
	}
	return strings.TrimSpace(b.String())
}

// truncateAtRuneBoundary cuts s to at most maxBytes without splitting a rune.
//
// s[:maxBytes] is not good enough: a title is arbitrary UTF-8 and a multi-byte
// rune straddling the cap would be halved, putting bytes that are not valid
// UTF-8 on the wire and into the DOM. Whether that happens depends on where the
// runes land, so the naive version is right most of the time -- which is why it
// has to be pinned by a test rather than eyeballed.
func truncateAtRuneBoundary(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	// s[maxBytes] is the first byte dropped. If it does not begin a rune, the
	// cut falls inside one, so walk back to where that rune starts.
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// Dedupe collapses rows to one per pane and puts them in the order the user
// sees on screen.
//
// Grouped sessions share a window list, so `list-panes -a` reports every pane
// once per member of the group: with two browser tabs open on one base session,
// three copies of every pane. A tmux -f filter cannot do this job. Excluding
// app-owned sessions looks equivalent until the user kills the base session
// while a tab is open: the group survives with only app-owned members, so a
// filtered snapshot comes back empty while the agents are still running and
// still visible in the attached tab. Preferring a non-app row keeps the label
// honest; keeping an app-owned row when it is the only one keeps those agents
// in the sidebar.
//
// Panes are keyed by PaneID alone. Pane ids are unique per server, so GroupKey
// adds no discrimination -- only a failure mode, if session_group is ever empty
// for one member of a group and the same pane keys twice.
//
// The returned slice is freshly allocated and never aliases rows. Task 7's
// Poller hands it to concurrent HTTP readers without copying, which is only
// safe because of that. An empty result is nil rather than an empty slice, to
// match Snapshot's no-server path.
func Dedupe(rows []Row) []Row {
	best := make(map[string]Row, len(rows))
	for _, r := range rows {
		if cur, ok := best[r.PaneID]; !ok || (cur.AppOwned && !r.AppOwned) {
			best[r.PaneID] = r
		}
	}
	if len(best) == 0 {
		return nil
	}
	out := make([]Row, 0, len(best))
	for _, r := range best {
		out = append(out, r)
	}
	// Order by what the user sees. PaneIndex, not PaneID: after a split-and-kill
	// cycle the ids run %0 %4 %2 %1 while the layout runs 0 1 2 3, so sorting by
	// id -- lexicographically or numerically -- disagrees with the screen.
	//
	// sort.Slice is explicitly NOT stable and map iteration order is randomised,
	// so this comparator has to be a total order or the output varies run to
	// run. (GroupKey, WindowIndex, PaneIndex) is already one over a correctly
	// deduped set, since pane indices are unique within a window; PaneID is a
	// final tiebreak so that a snapshot violating that assumption degrades to a
	// wrong-but-stable order rather than a sidebar that reshuffles every poll.
	sort.Slice(out, func(i, j int) bool {
		if out[i].GroupKey != out[j].GroupKey {
			return out[i].GroupKey < out[j].GroupKey
		}
		if out[i].WindowIndex != out[j].WindowIndex {
			return out[i].WindowIndex < out[j].WindowIndex
		}
		if out[i].PaneIndex != out[j].PaneIndex {
			return out[i].PaneIndex < out[j].PaneIndex
		}
		return out[i].PaneID < out[j].PaneID
	})
	return out
}
