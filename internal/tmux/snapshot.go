package tmux

import (
	"strconv"
	"strings"
)

// Sep is the field separator used in tmux -F format strings. It is a literal
// 0x1f byte: tmux does not expand "\x1f" inside a format string, and a tab is
// unsafe because window names and paths may contain one.
const Sep = "\x1f"

const fieldCount = 8

// Row is one pane as reported by tmux, before deduplication.
type Row struct {
	GroupKey    string // session_group, falling back to session_name
	PaneID      string // e.g. "%3", stable for the pane's lifetime
	AppOwned    bool   // set from the @wterm_web user option
	WindowIndex int
	WindowName  string
	PaneActive  bool
	Command     string
	Path        string
}

// Format is the -F argument producing rows this package can parse.
// Path is deliberately last: it is the only field tmux does not sanitize, so a
// raw newline in it can only ever corrupt the tail of a record.
const Format = "#{?#{session_group},#{session_group},#{session_name}}" + Sep +
	"#{pane_id}" + Sep +
	"#{@wterm_web}" + Sep +
	"#{window_index}" + Sep +
	"#{window_name}" + Sep +
	"#{pane_active}" + Sep +
	"#{pane_current_command}" + Sep +
	"#{pane_current_path}"

// ParseRows parses raw `tmux list-panes` output.
//
// A line with the wrong field count is treated as the continuation of the
// previous row's path rather than as a new pane, because pane_current_path may
// contain newlines. Leading garbage with no preceding row is discarded.
func ParseRows(out string) ([]Row, error) {
	var rows []Row
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Split(line, Sep)
		if len(fields) != fieldCount {
			if len(rows) > 0 {
				rows[len(rows)-1].Path += "\n" + line
			}
			continue
		}
		widx, err := strconv.Atoi(fields[3])
		if err != nil {
			continue
		}
		rows = append(rows, Row{
			GroupKey:    fields[0],
			PaneID:      fields[1],
			AppOwned:    fields[2] == "1",
			WindowIndex: widx,
			WindowName:  fields[4],
			PaneActive:  fields[5] == "1",
			Command:     fields[6],
			Path:        fields[7],
		})
	}
	return rows, nil
}
