package tmux

import (
	"sort"
	"strconv"
	"strings"
)

// Sep is the field separator used in tmux -F format strings. It is a literal
// 0x1f byte: tmux does not expand "\x1f" inside a format string, and a tab is
// unsafe because window names may contain one.
const Sep = "\x1f"

const fieldCount = 8

// Row is one pane as reported by tmux, before deduplication.
//
// The JSON names are the wire contract with the frontend; without the tags Go
// would marshal the exported Go names instead.
type Row struct {
	GroupKey    string `json:"groupKey"`  // session_group, falling back to session_name
	PaneID      string `json:"paneId"`    // e.g. "%3", stable for the pane's lifetime
	PaneIndex   int    `json:"paneIndex"` // position within the window, in layout order
	AppOwned    bool   `json:"appOwned"`  // set from the @wterm_web user option
	WindowIndex int    `json:"windowIndex"`
	WindowName  string `json:"windowName"`
	PaneActive  bool   `json:"paneActive"`
	Command     string `json:"command"`
}

// Format is the -F argument producing rows this package can parse.
//
// pane_current_path is deliberately absent. tmux sanitizes session and window
// names but not the path, so a pane sitting in a directory whose name contains
// a 0x1f or a newline can forge a whole extra record or swallow the following
// pane's -- either way the sidebar shows something other than the truth, and a
// pane that exists can vanish from it. Being the last field does not bound the
// damage: a newline simply starts a fresh line whose eight fields are all
// attacker-controlled. tmux's #{q:} modifier does not escape either byte.
// Nothing in v1 renders the path; the deferred git panel can query it per pane,
// where a single-pane result needs no field splitting to interpret.
//
// pane_current_command is a theoretical residual: it is not known to be
// sanitized either, and two attempts to make tmux report a command containing a
// newline failed, but that is not a proof that it cannot happen.
const Format = "#{?#{session_group},#{session_group},#{session_name}}" + Sep +
	"#{pane_id}" + Sep +
	"#{pane_index}" + Sep +
	"#{@wterm_web}" + Sep +
	"#{window_index}" + Sep +
	"#{window_name}" + Sep +
	"#{pane_active}" + Sep +
	"#{pane_current_command}"

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
		pidx, perr := strconv.Atoi(fields[2])
		widx, werr := strconv.Atoi(fields[4])
		if perr != nil || werr != nil {
			dropped++
			continue
		}
		rows = append(rows, Row{
			GroupKey:    fields[0],
			PaneID:      fields[1],
			PaneIndex:   pidx,
			AppOwned:    fields[3] == "1",
			WindowIndex: widx,
			WindowName:  fields[5],
			PaneActive:  fields[6] == "1",
			Command:     fields[7],
		})
	}
	return rows, dropped, nil
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
