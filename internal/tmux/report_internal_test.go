package tmux

import (
	"context"
	"testing"

	"github.com/diegok/tmux-web/internal/tmux/testutil"
)

// A failure in the second command must cost the reports and never the sidebar.
// Measured in the design and re-measured for this task: the first command's
// output is complete on stdout before the error, so the daemon parses stdout on
// its own terms and does NOT gate on the exit status. "Check the error first"
// is the reflex, and here the reflex trades a degraded feature for a blank
// sidebar.
//
// THE SEAM IS THE POINT OF THIS TEST. Poll takes no arguments and
// calls batchArgs() itself, so there is no way in from outside: a test that
// drove runKeepingOutput directly with a broken argv would assert that
// runKeepingOutput keeps its output -- which it plainly does -- and would NOT
// kill the mutant this test exists for, "Poll returns early on
// err != nil". So batchArgs is declared as a package-level var holding a func,
// and this test replaces it for the duration.
//
// Not parallel, and it restores the var with t.Cleanup: it is process-wide
// state for as long as it is swapped.
func TestABrokenReportReadStillYieldsTheSnapshot(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "probe", "-x", "80", "-y", "24")

	orig := batchArgs
	t.Cleanup(func() { batchArgs = orig })
	batchArgs = func() []string {
		return []string{
			"list-panes", "-a", "-F", Format,
			// The report command, pointed at a target that does not exist.
			";", "list-panes", "-t", "nosuch", "-F", ReportFormat,
		}
	}

	got, err := NewClient(srv.Args()).Poll(context.Background())
	rows, reports := got.Rows, got.Reports
	if err != nil {
		t.Fatalf("err = %v; a failed report read must not fail the poll", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want the snapshot block intact despite the nonzero exit", len(rows))
	}
	if len(reports) != 0 {
		t.Fatalf("reports = %v, want none", reports)
	}
}

// The sibling that keeps the rule from becoming "ignore the error": with
// NOTHING usable on stdout, the error must come back.
//
// The plan specified this as "kill the server and re-run". That cannot work and
// kills nothing: a killed server makes tmux print "no server running on
// <path>", which noServer() matches, so CORRECT code returns (nil, nil, nil)
// with no error -- the one path this package deliberately defines as silent.
// Asserting an error there fails against the right implementation.
//
// Pointing BOTH commands at a nonexistent session gives what the assertion
// actually needs: empty stdout, a nonzero exit, and a message ("can't find
// window: nosuch") that noServer does not match. So correct code must return
// the error, and the mutant that drops the `err != nil && len(rows) == 0`
// branch dies here.
func TestABatchWithNothingUsableOnStdoutIsAnError(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "probe", "-x", "80", "-y", "24")

	orig := batchArgs
	t.Cleanup(func() { batchArgs = orig })
	batchArgs = func() []string {
		return []string{
			"list-panes", "-t", "nosuch", "-F", Format,
			";", "list-panes", "-t", "nosuch", "-F", ReportFormat,
		}
	}

	got, err := NewClient(srv.Args()).Poll(context.Background())
	rows, reports := got.Rows, got.Reports
	if err == nil {
		t.Fatalf("err = nil with rows %+v and reports %v; a batch that produced "+
			"nothing usable is a failed poll, not a degraded one", rows, reports)
	}
	if noServer(err.Error()) {
		t.Fatalf("err = %v, which noServer() matches; this test would then be "+
			"asserting against the silent path and would pass for the wrong "+
			"reason", err)
	}
	if rows != nil || reports != nil {
		t.Errorf("rows = %+v, reports = %v; want both nil alongside the error", rows, reports)
	}
}
