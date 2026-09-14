package tmux_test

import (
	"strings"
	"testing"

	"github.com/diegok/tmux-web/internal/tmux"
	"github.com/diegok/tmux-web/internal/tmux/testutil"
)

// What tmux actually calls each mode, measured rather than assumed.
//
// Row.PaneMode exists so the browser can tell "this pane is in copy mode" from
// "this pane is in some other mode", and it renames a button on the strength of
// that string. Every spelling below was read off a real 3.7b server before it
// was written down here -- the names are tmux's own mode names, not an API, and
// nothing in the format string would fail if one of them changed.
//
// The stacking cases are the ones the button's rule turns on. pane_in_mode is a
// COUNT of layers and pane_mode names only the TOP one, so a pane that is in
// copy mode on top of a choose-tree reports "copy-mode"/2 -- and a pane that is
// only in a choose-tree reports "tree-mode"/1 while still being "in a mode".
// A rule written against pane_in_mode would call the second one copy mode and
// offer to leave a mode it cannot leave.
//
// The fixture is two panes with the target INACTIVE, for the reason
// endmode_integration_test.go gives at length: on a single-pane window tmux's
// default pane *is* the target, so a format string that resolved against the
// wrong pane -- or against no pane -- would satisfy every assertion here.
func TestSnapshotCarriesThePaneModeTmuxReports(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "work", "-x", "80", "-y", "24")
	srv.Run(t, "split-window", "-d", "-t", "=work:0")
	panes := strings.Split(srv.Run(t, "list-panes", "-t", "=work:0", "-F", "#{pane_id}"), "\n")
	if len(panes) != 2 {
		t.Fatalf("want a split window, got panes %q", panes)
	}
	target, other := panes[0], panes[1]
	srv.Run(t, "select-pane", "-t", other)
	if got := srv.Run(t, "display-message", "-p", "-t", target, "#{pane_active}"); got != "0" {
		t.Fatalf("the target pane %s is active (%s): every assertion here would be "+
			"satisfied by a format string that hit the default pane instead", target, got)
	}

	c := tmux.NewClient(srv.Args())
	// The row for the target pane out of a settled snapshot. Settled because a
	// pane reports #{pane_current_command} as "tmux" for the first few
	// milliseconds of its life, which has nothing to do with modes.
	modeOf := func(t *testing.T) string {
		t.Helper()
		for _, r := range settledSnapshot(t, c) {
			if r.PaneID == target {
				return r.PaneMode
			}
		}
		t.Fatalf("pane %s is not in the snapshot", target)
		return ""
	}

	for _, tc := range []struct {
		name string
		// enter is run against the target pane before the snapshot.
		enter [][]string
		want  string
	}{
		// The pane most replies go to, and the state the button must offer to
		// ENTER copy mode from.
		{"no mode", nil, ""},
		{"copy mode", [][]string{{"copy-mode"}}, "copy-mode"},
		// Scrolling up inside a choose-tree. The top layer is the copy layer,
		// which is the one `send-keys -X cancel` pops, so this is an "Exit
		// copy" -- and leaving it lands back in the tree rather than nowhere.
		{"copy mode stacked on tree mode", [][]string{{"choose-tree"}, {"copy-mode"}}, "copy-mode"},
		// In a mode, but not one this app can leave: cancel refuses here with
		// "not in a mode", exit 1. The button must still say "Copy mode".
		{"tree mode alone", [][]string{{"choose-tree"}}, "tree-mode"},
		{"clock mode", [][]string{{"clock-mode"}}, "clock-mode"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A fresh shell in the pane drops whatever the previous case left
			// on its mode stack, so the cases cannot leak into one another --
			// and `cancel` could not clear them anyway: it refuses on every
			// mode outside the copy family, which is half of this table.
			srv.Run(t, "respawn-pane", "-k", "-t", target, "/bin/sh")
			for _, cmd := range tc.enter {
				srv.Run(t, append(cmd, "-t", target)...)
			}
			// tmux's own answer, read the way endmode_integration_test.go
			// reads it. Asserted first: if tmux stopped calling this mode what
			// the table says, the row below would be "wrong" for a reason that
			// has nothing to do with the snapshot format.
			if got := srv.Run(t, "display-message", "-p", "-t", target, "#{pane_mode}"); got != tc.want {
				t.Fatalf("tmux reports #{pane_mode} = %q for %s, want %q: the "+
					"measurement this test is written from no longer holds",
					got, tc.name, tc.want)
			}
			if got := modeOf(t); got != tc.want {
				t.Errorf("Row.PaneMode = %q, want %q", got, tc.want)
			}
		})
	}
}
