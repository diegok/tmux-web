package tmux

import (
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Sep is the field separator used in tmux -F format strings. It is a literal
// 0x1f byte: tmux does not expand "\x1f" inside a format string, and a tab is
// unsafe because window names may contain one.
const Sep = "\x1f"

const fieldCount = 13

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
	Label       string `json:"label"`       // @wterm_label; user-set, may be ""
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

// Format is the -F argument producing rows this package can parse.
//
// pane_current_path is deliberately absent. tmux sanitizes session and window
// names but not the path, so a pane sitting in a directory whose name contains
// a 0x1f or a newline can forge a whole extra record or swallow the following
// pane's -- either way the sidebar shows something other than the truth, and a
// pane that exists can vanish from it. Being the last field does not bound the
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
// reads back as one line with the control bytes gone.
//
// @wterm_label is NOT safe in the same way: tmux does not sanitize user option
// values, so a label carrying a 0x1f or a newline forges or splits a record and
// makes its pane vanish from the sidebar. It is validated on write, and ParseRows
// drops what it cannot parse -- one row missing until the option is cleared,
// rather than a neighbouring pane's record silently rewritten.
const Format = "#{?#{session_group},#{session_group},#{session_name}}" + Sep +
	"#{session_id}" + Sep +
	"#{session_name}" + Sep +
	"#{pane_id}" + Sep +
	"#{pane_index}" + Sep +
	"#{@wterm_web}" + Sep +
	"#{" + LabelOption + "}" + Sep +
	"#{window_id}" + Sep +
	"#{window_index}" + Sep +
	"#{window_name}" + Sep +
	"#{pane_active}" + Sep +
	"#{pane_current_command}" + Sep +
	"#{pane_title}"

// ParseRows parses raw `tmux list-panes` output into one Row per line.
//
// Any line that is not a well-formed record -- wrong field count, or a
// non-numeric index -- is skipped and counted in dropped. Lines are independent:
// a malformed one never merges into or alters a neighbouring row. dropped is
// returned rather than logged so the caller can surface a snapshot that is
// quietly losing panes instead of it passing unnoticed.
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
		if len(fields) != fieldCount {
			dropped++
			continue
		}
		// Distinct names: shadowing the named err return here would be
		// harmless today only because it is always nil.
		pidx, perr := strconv.Atoi(fields[4])
		widx, werr := strconv.Atoi(fields[8])
		if perr != nil || werr != nil {
			dropped++
			continue
		}
		rows = append(rows, Row{
			GroupKey:    fields[0],
			SessionID:   fields[1],
			SessionName: fields[2],
			PaneID:      fields[3],
			PaneIndex:   pidx,
			AppOwned:    fields[5] == "1",
			Label:       fields[6],
			WindowID:    fields[7],
			WindowIndex: widx,
			WindowName:  fields[9],
			PaneActive:  fields[10] == "1",
			Command:     fields[11],
			Title:       truncateAtRuneBoundary(fields[12], MaxTitle),
		})
	}
	return rows, dropped, nil
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
