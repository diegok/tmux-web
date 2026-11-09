package tmux_test

import (
	"strings"
	"testing"
	"time"

	"github.com/diegok/tmux-web/internal/tmux"
	"github.com/diegok/tmux-web/internal/tmux/testutil"
)

// The two facts that decide who receives a wheel notch, read off a real server.
//
// THIS IS THE ONLY TEST THAT CAN SEE A MISTYPED FORMAT VARIABLE. Measured on
// 3.7b: an unrecognised variable expands to the EMPTY STRING rather than
// failing or dropping the field --
//
//	display-message -p '[#{alternate_on}][#{mouse_any_flag}][#{no_such_xyz}]'
//	[1][1][]
//
// -- so `#{alternate_onn}` produces a full-length, well-formed record whose
// fact is false for every pane forever. Every unit test downstream of the
// parser stays green, and the browser's swipe quietly goes back to assuming
// tmux's five lines in the panes this app exists to drive. The only way to
// catch it is to turn the facts ON in a real pane and read them back.
//
// The fixture is two panes with the target INACTIVE, for the reason
// snapshot_mode_integration_test.go gives: on a single-pane window tmux's
// default pane IS the target, so a format string that resolved against the
// wrong pane -- or against no pane -- would satisfy every assertion here.
func TestSnapshotCarriesTheWheelFactsTmuxReports(t *testing.T) {
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
	rowOf := func(t *testing.T) tmux.Row {
		t.Helper()
		for _, r := range settledSnapshot(t, c) {
			if r.PaneID == target {
				return r
			}
		}
		t.Fatalf("pane %s is not in the snapshot", target)
		return tmux.Row{}
	}

	// A shell at a prompt: tmux's own scrollback is what a wheel notch scrolls,
	// which is the case the gesture's five lines are right for.
	if r := rowOf(t); r.AlternateOn || r.MouseAnyFlag {
		t.Fatalf("a plain shell pane reports AlternateOn=%v MouseAnyFlag=%v, want both false",
			r.AlternateOn, r.MouseAnyFlag)
	}

	// And now the regime every coding agent runs in: the alternate screen
	// (1049) and any-event mouse tracking (1003). Written by the pane itself,
	// so tmux learns it the way it learns it from an agent -- by parsing the
	// pane's output -- rather than from anything this test asserts with.
	srv.Run(t, "send-keys", "-t", target, `printf '\033[?1049h\033[?1003h'`, "Enter")
	waitFor(t, 3*time.Second, func() bool {
		return srv.Run(t, "display-message", "-p", "-t", target,
			"#{alternate_on}#{mouse_any_flag}") == "11"
	}, "tmux never saw the pane turn on the alternate screen and mouse tracking")

	if r := rowOf(t); !r.AlternateOn || !r.MouseAnyFlag {
		t.Errorf("AlternateOn=%v MouseAnyFlag=%v on a pane tmux reports as 11: the "+
			"record is not carrying what tmux knows -- a mistyped variable name "+
			"expands to empty, which reads as false here and nowhere else",
			r.AlternateOn, r.MouseAnyFlag)
	}
}
