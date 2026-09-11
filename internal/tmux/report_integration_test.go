package tmux_test

import (
	"context"
	"testing"

	"github.com/diegok/tmux-web/internal/tmux"
	"github.com/diegok/tmux-web/internal/tmux/testutil"
)

// The benign fixture is the point of this test and it has TWO required
// properties, because two different mistakes hide behind a fixture without
// them:
//
//   - It contains a lowercase "n". A bracket set retyped from a rendered "\n"
//     puts a literal 'n' in the set, so every "n" in a perfectly good value
//     becomes a space ("SECOnD" -> "SECO D"). A fixture with no "n" in it
//     passes that mutant.
//   - It contains a real newline byte. The same mistake leaves real newlines
//     alive, which is what splits a record in two.
//
// And it asserts on the BENIGN value surviving, not only on the hostile one
// being cleaned: [[:cntrl:]] expands to "" for EVERY value, good ones included,
// and a hostile-input-only test passes that mutant too.
//
// The content is the implementer's own. Nothing in this repo's fixtures is ever
// copied out of a live pane: this repository is public and those panes hold
// private client work.
func TestReportFormatRoundTripsThroughRealTmux(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "probe", "-x", "80", "-y", "24")
	c := tmux.NewClient(srv.Args())

	const benign = "running go test in the second window\nnext line"
	const wantBenign = "running go test in the second window next line"

	srv.Run(t, "set", "-p", "-t", "probe", tmux.AgentOption, benign)
	_, reports, err := c.SnapshotAndReports(context.Background())
	if err != nil {
		t.Fatalf("SnapshotAndReports: %v", err)
	}
	// Exactly one pane, so the map has exactly one entry. Asserted rather than
	// assumed: every check below is inside a range, so an empty map would make
	// the whole test vacuous -- which is what dropping the ";" from batchArgs
	// produces.
	if len(reports) != 1 {
		t.Fatalf("reports = %v, want exactly one entry for the one pane", reports)
	}
	for id, got := range reports {
		if got != wantBenign {
			t.Fatalf("pane %s: got %q, want %q -- every 'n' intact and the newline "+
				"a single space", id, got, wantBenign)
		}
	}

	// The hostile half. A separator and a newline both become spaces, so the
	// value reads as tampered with rather than as something somebody chose.
	// Two substitutions in one value: tmux's s/// modifier replaces every
	// match, not the first -- measured on 3.7b on an isolated socket.
	srv.Run(t, "set", "-p", "-t", "probe", tmux.AgentOption, "EV\x1fIL\nMORE")
	_, reports, err = c.SnapshotAndReports(context.Background())
	if err != nil {
		t.Fatalf("SnapshotAndReports: %v", err)
	}
	if len(reports) != 1 {
		t.Fatalf("reports = %v, want exactly one entry for the one pane", reports)
	}
	for id, got := range reports {
		if got != "EV IL MORE" {
			t.Fatalf("pane %s: got %q, want both bytes substituted to spaces", id, got)
		}
	}
}

// Sibling to TestSnapshotHostileLabelCannotRemoveAPane. It should pass
// trivially, because @wterm_agent is not in Format at all -- and it is worth
// having precisely so that the day somebody appends it there, this goes red.
//
// Worth recording, because the batched read opens a direction the label
// hardening did not have to think about: the two blocks share one stdout, so if
// layer 1 ever failed open, a newline inside a hostile LABEL could forge a
// whole REPORT-block line -- "...\nA<Sep>%2<Sep>1;idle;<ts>" -- and thereby set
// another pane's state. It is not worth a code change: the value has to be
// written through the tmux socket, and anyone holding that socket can
// `set -p -t %2 @wterm_agent` directly with no forgery at all. It is worth
// writing down so nobody rediscovers it years from now and reads it as a hole.
// What bounds it is unchanged and is layer 1 plus layer 3: the substitution
// turns both record-breaking bytes into spaces, and ParseReport re-sanitises
// whatever arrives.
func TestHostileAgentReportCannotRemoveAPane(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "probe", "-x", "80", "-y", "24")
	srv.Run(t, "split-window", "-t", "probe")
	c := tmux.NewClient(srv.Args())

	// A pane's #{pane_current_command} reads "tmux" for the first few
	// milliseconds of its life, before the shell has exec'd, so a baseline taken
	// straight after split-window disagrees with the next read for a reason that
	// has nothing to do with reports. Same helper, same reason, as
	// TestSnapshotHostileLabelCannotRemoveAPane.
	settledSnapshot(t, c)
	before, _, err := c.SnapshotAndReports(context.Background())
	if err != nil || len(before) != 2 {
		t.Fatalf("baseline: %d rows, err %v", len(before), err)
	}
	srv.Run(t, "set", "-p", "-t", "probe", tmux.AgentOption, "X\x1fY\nZ\x1fW")
	after, _, err := c.SnapshotAndReports(context.Background())
	if err != nil {
		t.Fatalf("SnapshotAndReports: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("a hostile report changed the pane count: %d -> %d", len(before), len(after))
	}
	for i := range after {
		// The identity block must be byte-identical: the report is not in it.
		if after[i].PaneID != before[i].PaneID || after[i].Command != before[i].Command ||
			after[i].WindowID != before[i].WindowID || after[i].Title != before[i].Title {
			t.Fatalf("a hostile report changed a snapshot row:\n%+v\n%+v", before[i], after[i])
		}
	}
}
