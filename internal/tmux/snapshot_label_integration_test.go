package tmux_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/diegok/tmux-web/internal/tmux"
	"github.com/diegok/tmux-web/internal/tmux/testutil"
)

// A pane must survive any @tmux_web_label, whatever bytes are in it.
//
// This runs against a real server because the bug it guards only exists
// because of what real tmux does with real bytes: tmux normalises a pane title
// through its OSC parser but stores a user option exactly as given, and
// `list-panes -F` prints it exactly as stored -- 0x1f, newline and all. A
// hand-built record proves nothing about that, which is precisely how the hole
// stayed open. Each case re-checks that tmux really did store the value
// verbatim, so a future tmux that starts sanitising options turns this test
// into a failure rather than into a green test of nothing.
//
// From v2 the writers include third-party agent integrations -- a pi extension
// and an opencode plugin -- carrying text derived from prompts and tool calls,
// so "the app validates on write" is no longer the whole story.
func TestSnapshotHostileLabelCannotRemoveAPane(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "work", "-x", "80", "-y", "24")
	srv.Run(t, "new-window", "-t", "work", "-n", "api")
	srv.Run(t, "split-window", "-t", "work:api")
	// A neighbour with a label of its own: a fix that threw all labels away, or
	// that let one pane's label leak into the next record, has to fail here.
	srv.Run(t, "set", "-p", "-t", "work:api.1", tmux.LabelOption, "neighbour")

	c := tmux.NewClient(srv.Args())
	base := settledSnapshot(t, c)
	if len(base) != 3 {
		t.Fatalf("want 3 panes before any hostile label, got %d: %+v", len(base), base)
	}
	target := base[0].PaneID
	want := map[string]tmux.Row{}
	for _, r := range base[1:] {
		want[r.PaneID] = r
	}
	if base[1].Label != "neighbour" && base[2].Label != "neighbour" {
		t.Fatalf("the neighbour's label did not survive a clean snapshot: %+v", base)
	}

	for _, tc := range []struct {
		name  string
		label string
		want  string
	}{
		{"a separator", "EV" + tmux.Sep + "IL", "EV IL"},
		{"a newline", "EV\nIL", "EV IL"},
		{"both", "a" + tmux.Sep + "b\nc", "a b c"},
		{"a forged record", tmux.Sep + "$9" + tmux.Sep + "forged\n%99" + tmux.Sep + "0", "$9 forged %99 0"},
		{"nothing but dangerous bytes", tmux.Sep + "\n" + tmux.Sep + "\n", ""},
		{"a C1 control tmux's own check lets through", "a\u009fb", "a b"},
		{"invalid UTF-8", "a" + string([]byte{0xff}) + "b", "a\ufffdb"},
		{"a very long label", strings.Repeat("\U0001f30d", 2000), strings.Repeat("\U0001f30d", tmux.MaxLabel)},
		{"a long label of separators", strings.Repeat(tmux.Sep, 2000), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv.Run(t, "set", "-p", "-t", target, tmux.LabelOption, tc.label)
			// tmux stored it verbatim, or this test is asserting against a
			// value tmux already defanged and proves nothing. TryRun trims a
			// trailing newline off tmux's stdout, so the expectation is trimmed
			// the same way rather than the check being weakened.
			if got := srv.Run(t, "show", "-p", "-t", target, "-qv", tmux.LabelOption); got != strings.TrimRight(tc.label, "\n") {
				t.Fatalf("tmux did not store the label verbatim: %q", got)
			}

			panes, err := c.Snapshot(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(panes) != 3 {
				t.Fatalf("want 3 panes, got %d: a label removed or forged a pane: %+v", len(panes), panes)
			}
			var got tmux.Row
			for _, r := range panes {
				if r.PaneID == target {
					got = r
					continue
				}
				if w, ok := want[r.PaneID]; !ok || r != w {
					t.Errorf("another pane's row changed: got %+v, want %+v", r, w)
				}
			}
			if got.PaneID != target {
				t.Fatalf("pane %s vanished from the snapshot: %+v", target, panes)
			}
			if got.Label != tc.want {
				t.Errorf("Label = %q, want %q", got.Label, tc.want)
			}
			if !utf8.ValidString(got.Label) {
				t.Errorf("Label %q is not valid UTF-8", got.Label)
			}
			if n := utf8.RuneCountInString(got.Label); n > tmux.MaxLabel {
				t.Errorf("Label kept %d runes, limit is %d", n, tmux.MaxLabel)
			}
			// The identity fields are the ones the sidebar addresses the pane
			// by; a shifted record keeps the row and ruins them.
			if got.SessionID != base[0].SessionID || got.WindowID != base[0].WindowID ||
				got.PaneIndex != base[0].PaneIndex || got.WindowIndex != base[0].WindowIndex ||
				got.SessionName != base[0].SessionName || got.WindowName != base[0].WindowName ||
				got.Command != base[0].Command || got.Title != base[0].Title ||
				got.PaneActive != base[0].PaneActive || got.AppOwned != base[0].AppOwned {
				t.Errorf("the label shifted the record: got %+v, want %+v with a different label", got, base[0])
			}
		})
	}

	// Clearing it puts the pane back exactly as it was, so nothing above is a
	// one-way change to the row.
	srv.Run(t, "set", "-p", "-u", "-t", target, tmux.LabelOption)
	after, err := c.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 3 {
		t.Fatalf("want 3 panes after clearing, got %d", len(after))
	}
	for i := range after {
		if after[i] != base[i] {
			t.Errorf("row %d after clearing = %+v, want %+v", i, after[i], base[i])
		}
	}
}

// settledSnapshot returns a snapshot that two consecutive reads agree on.
//
// A pane's #{pane_current_command} is "tmux" for the first few milliseconds of
// its life, before the shell has exec'd, so a baseline taken immediately after
// new-window disagrees with every later snapshot for a reason that has nothing
// to do with labels. Waiting for the rows to stop moving keeps the
// row-for-row comparison strict instead of dropping the fields that flicker.
func settledSnapshot(t *testing.T, c *tmux.Client) []tmux.Row {
	t.Helper()
	prev, err := c.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		time.Sleep(20 * time.Millisecond)
		cur, err := c.Snapshot(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if slices.Equal(prev, cur) {
			return cur
		}
		if time.Now().After(deadline) {
			t.Fatalf("snapshot never settled: %+v then %+v", prev, cur)
		}
		prev = cur
	}
}
