package tmux

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// rec builds one snapshot record from its fields, joined by the real separator.
// Tests use Sep rather than a private copy of the byte so that a change to the
// separator cannot leave the suite passing against a stale literal.
func rec(fields ...string) string { return strings.Join(fields, Sep) }

// A valid record, as a named baseline the malformed cases can be varied from.
// Field order matches Format: group, session id, session name, pane id, pane
// index, app marker, label, window index, window name, pane active, command,
// title.
func goodRow(paneID, paneIndex, windowIndex string) string {
	return rec("work", "$0", "work", paneID, paneIndex, "", "", windowIndex, "api", "1", "claude", "a title")
}

func TestParseRows(t *testing.T) {
	t.Run("one well formed row", func(t *testing.T) {
		got, dropped, err := ParseRows(rec("work", "$0", "work", "%3", "0", "", "", "1", "api", "1", "claude", "a title"))
		if err != nil {
			t.Fatal(err)
		}
		if dropped != 0 {
			t.Fatalf("dropped = %d, want 0", dropped)
		}
		if len(got) != 1 {
			t.Fatalf("want 1 row, got %d", len(got))
		}
		r := got[0]
		if r.GroupKey != "work" || r.PaneID != "%3" || r.PaneIndex != 0 || r.AppOwned {
			t.Fatalf("bad row: %+v", r)
		}
		if r.WindowIndex != 1 || r.WindowName != "api" || !r.PaneActive {
			t.Fatalf("bad row: %+v", r)
		}
		if r.Command != "claude" {
			t.Fatalf("bad row: %+v", r)
		}
	})

	// Pins the false side of both booleans: a parser hardcoding either to true
	// passes every other subtest.
	t.Run("app owned row with an inactive pane", func(t *testing.T) {
		got, dropped, err := ParseRows(rec("work", "$0", "work", "%3", "2", "1", "", "0", "w", "0", "zsh", "t"))
		if err != nil || dropped != 0 || len(got) != 1 {
			t.Fatalf("ParseRows = %+v, %d, %v", got, dropped, err)
		}
		if !got[0].AppOwned {
			t.Fatal("expected AppOwned")
		}
		if got[0].PaneActive {
			t.Fatal("expected PaneActive false")
		}
		if got[0].PaneIndex != 2 {
			t.Fatalf("PaneIndex = %d, want 2", got[0].PaneIndex)
		}
	})

	// Every other subtest parses a single record, which lets a parser that only
	// ever returns one row, or one that mixes fields between rows, survive.
	t.Run("several rows are returned in input order", func(t *testing.T) {
		out := strings.Join([]string{
			goodRow("%1", "0", "0"),
			goodRow("%4", "1", "0"),
			goodRow("%2", "2", "3"),
		}, "\n")
		got, dropped, err := ParseRows(out)
		if err != nil || dropped != 0 {
			t.Fatalf("dropped = %d, err = %v", dropped, err)
		}
		if len(got) != 3 {
			t.Fatalf("want 3 rows, got %d: %+v", len(got), got)
		}
		for i, want := range []struct {
			paneID             string
			paneIdx, windowIdx int
		}{
			{"%1", 0, 0},
			{"%4", 1, 0},
			{"%2", 2, 3},
		} {
			if got[i].PaneID != want.paneID || got[i].PaneIndex != want.paneIdx ||
				got[i].WindowIndex != want.windowIdx {
				t.Fatalf("row %d = %+v, want %+v", i, got[i], want)
			}
		}
	})

	// A record split across lines used to be rejoined into the previous row's
	// path. There is no path field any more, so a line that does not parse is
	// dropped -- it must never merge into, or corrupt, a neighbouring row.
	t.Run("malformed lines are dropped and counted", func(t *testing.T) {
		out := strings.Join([]string{
			goodRow("%1", "0", "0"),
			"nonsense", // too few fields
			// Numeric indices, so that only the field count can reject it.
			rec("work", "$0", "work", "%7", "0", "", "", "0", "w", "1", "zsh", "t", "extra"),
			rec("work", "$0", "work", "%9", "0", "", "", "notanint", "w", "1", "zsh", "t"), // bad window index
			rec("work", "$0", "work", "%8", "notanint", "", "", "0", "w", "1", "zsh", "t"), // bad pane index
			goodRow("%2", "1", "0"),
		}, "\n")
		got, dropped, err := ParseRows(out)
		if err != nil {
			t.Fatal(err)
		}
		if dropped != 4 {
			t.Fatalf("dropped = %d, want 4", dropped)
		}
		if len(got) != 2 {
			t.Fatalf("want the 2 good rows, got %d: %+v", len(got), got)
		}
		if got[0].PaneID != "%1" || got[1].PaneID != "%2" {
			t.Fatalf("wrong rows survived: %+v", got)
		}
	})

	// tmux output arrives newline-terminated. Client.Run trims it today, but
	// that contract lives in another file and a control-mode caller would not
	// go through it.
	t.Run("a trailing newline does not produce a phantom row", func(t *testing.T) {
		got, dropped, err := ParseRows(goodRow("%1", "0", "0") + "\n")
		if err != nil || dropped != 0 || len(got) != 1 {
			t.Fatalf("ParseRows = %+v, %d, %v; want 1 row, 0 dropped", got, dropped, err)
		}
	})

	t.Run("empty output yields no rows and drops nothing", func(t *testing.T) {
		got, dropped, err := ParseRows("")
		if err != nil || got != nil || dropped != 0 {
			t.Fatalf("ParseRows(\"\") = %v, %d, %v; want nil, 0, nil", got, dropped, err)
		}
	})

	// The error is always nil today. The return exists because Task 5 calls this
	// where an error is the natural shape; this pins the current contract so a
	// caller that ignores it is not silently wrong later.
	t.Run("error is always nil", func(t *testing.T) {
		for _, in := range []string{"", "nonsense", goodRow("%1", "0", "0"), "\n\n"} {
			if _, _, err := ParseRows(in); err != nil {
				t.Fatalf("ParseRows(%q) returned %v, want nil", in, err)
			}
		}
	})

	t.Run("carries session identity, label and title", func(t *testing.T) {
		// The group key and the live session name are DELIBERATELY different.
		// tmux keeps the pre-rename name in session_group, so a fixture where they
		// match would pass against an implementation that reads the group key --
		// which is exactly the bug this field exists to fix.
		line := rec("work3", "$3", "api", "%1", "0", "", "reviewer", "1", "win", "1", "claude", "✳ writing tests")
		got, dropped, err := ParseRows(line)
		if err != nil || dropped != 0 || len(got) != 1 {
			t.Fatalf("got %+v dropped=%d err=%v", got, dropped, err)
		}
		r := got[0]
		if r.GroupKey != "work3" {
			t.Errorf("GroupKey = %q, want the group name work3", r.GroupKey)
		}
		if r.SessionName != "api" {
			t.Errorf("SessionName = %q, want the live name api: reading the group key "+
				"here is the pre-rename bug this field exists to fix", r.SessionName)
		}
		if r.SessionID != "$3" || r.Label != "reviewer" || r.Title != "✳ writing tests" {
			t.Errorf("bad row: %+v", r)
		}
	})

	t.Run("a huge title is truncated on a rune boundary", func(t *testing.T) {
		// Multi-byte runes straddling the cap: a byte slice would cut one in half
		// and put invalid UTF-8 into the DOM.
		//
		// Both widths are here on purpose. MaxTitle is 256, which is divisible by
		// 2, so 2-byte runes land exactly on the cap and s[:MaxTitle] happens to
		// be valid -- a byte-slicing implementation passes the "é" case and fails
		// only on a width that does not divide the cap. "✳" is 3 bytes, and it is
		// also what Claude Code actually puts at the head of a title.
		for _, r := range []string{"é", "✳"} {
			huge := strings.Repeat(r, 4000)
			line := rec("w", "$0", "w", "%1", "0", "", "", "1", "win", "1", "claude", huge)
			got, _, _ := ParseRows(line)
			if len(got[0].Title) > MaxTitle {
				t.Fatalf("%q title kept %d bytes, want <= %d", r, len(got[0].Title), MaxTitle)
			}
			if !utf8.ValidString(got[0].Title) {
				t.Fatalf("%q: truncation split a rune; the title is not valid UTF-8", r)
			}
			// A cap that threw the title away entirely would satisfy both
			// assertions above.
			if len(got[0].Title) < MaxTitle-utf8.RuneLen([]rune(r)[0]) {
				t.Fatalf("%q title kept only %d bytes; truncation must keep what fits", r, len(got[0].Title))
			}
		}
	})

	t.Run("a title that fits is not touched", func(t *testing.T) {
		title := "✳ writing tests"
		line := rec("w", "$0", "w", "%1", "0", "", "", "1", "win", "1", "claude", title)
		got, _, _ := ParseRows(line)
		if got[0].Title != title {
			t.Fatalf("Title = %q, want %q unchanged", got[0].Title, title)
		}
	})

	// tmux sanitises titles but NOT user option values, so a label is the one
	// new field that can carry a separator or a newline.
	t.Run("a label containing control bytes cannot remove a pane", func(t *testing.T) {
		line := rec("w", "$0", "w", "%1", "0", "", "EV"+Sep+"IL", "1", "win", "1", "claude", "t")
		got, dropped, _ := ParseRows(line)
		if dropped == 0 {
			t.Fatal("a malformed record must be counted")
		}
		if len(got) != 0 {
			t.Fatalf("a malformed record must not produce a row: %+v", got)
		}
		// The point is that it is DROPPED AND COUNTED, never merged into a
		// neighbour and never silently ignored.
	})
}

// Format and fieldCount must agree, or every record is dropped at runtime while
// the parser's own tests keep passing.
func TestFormatFieldCount(t *testing.T) {
	if n := strings.Count(Format, Sep); n != fieldCount-1 {
		t.Fatalf("Format has %d separators (%d fields), want %d (%d fields)",
			n, n+1, fieldCount-1, fieldCount)
	}
	if strings.Contains(Format, "pane_current_path") {
		t.Fatal("pane_current_path must not be in the snapshot: an unsanitized " +
			"field lets one pane's output forge or erase another's record")
	}
}

func TestDedupe(t *testing.T) {
	// The app-owned copies carry different display fields, so this asserts
	// which row's payload survived -- not merely how many rows did. Each pane
	// appears with the app copy on either side of the real one, so both
	// directions of the preference are exercised.
	t.Run("grouped sessions collapse to one row per pane", func(t *testing.T) {
		rows := []Row{
			{PaneID: "%0", GroupKey: "work", PaneIndex: 0, WindowName: "api", Command: "zsh", AppOwned: false},
			{PaneID: "%0", GroupKey: "work", PaneIndex: 0, WindowName: "stale", Command: "stale", AppOwned: true},
			{PaneID: "%1", GroupKey: "work", PaneIndex: 1, WindowName: "stale", Command: "stale", AppOwned: true},
			{PaneID: "%1", GroupKey: "work", PaneIndex: 1, WindowName: "api", Command: "vim", AppOwned: false},
		}
		got := Dedupe(rows)
		if len(got) != 2 {
			t.Fatalf("want 2 panes, got %d: %+v", len(got), got)
		}
		for _, r := range got {
			if r.AppOwned {
				t.Fatalf("should prefer the non-app row: %+v", r)
			}
			if r.WindowName != "api" || r.Command == "stale" {
				t.Fatalf("kept the app row's payload: %+v", r)
			}
		}
	})

	// The regression that motivated dedupe over a tmux filter. Kill the base
	// session while two tabs are open and the group survives with only
	// app-owned members -- so every pane still arrives duplicated, and every
	// copy is AppOwned. There is nothing to prefer, and a filter returns
	// nothing at all while the agents are still running.
	//
	// The fixture must contain duplicates. Two distinct panes with one row each
	// prove only that Dedupe does not filter, which no plausible implementation
	// gets wrong.
	t.Run("duplicated app owned rows collapse but survive", func(t *testing.T) {
		rows := []Row{
			{PaneID: "%0", GroupKey: "work", PaneIndex: 0, Command: "claude", AppOwned: true},
			{PaneID: "%1", GroupKey: "work", PaneIndex: 1, Command: "npm", AppOwned: true},
			{PaneID: "%0", GroupKey: "work", PaneIndex: 0, Command: "claude", AppOwned: true},
			{PaneID: "%1", GroupKey: "work", PaneIndex: 1, Command: "npm", AppOwned: true},
		}
		got := Dedupe(rows)
		if len(got) != 2 {
			t.Fatalf("app-owned panes must survive and collapse, got %d: %+v", len(got), got)
		}
		for _, r := range got {
			if !r.AppOwned {
				t.Fatalf("nothing here to prefer: %+v", r)
			}
		}
	})

	// Every field contradicts the others, so an implementation that sorts by
	// PaneID alone -- or that keeps PaneID as the tiebreak within a window --
	// produces a different answer from the correct one. A fixture whose pane
	// ids happen to already be in the intended order tests nothing.
	t.Run("output is ordered by group, then window, then pane index", func(t *testing.T) {
		rows := []Row{
			{PaneID: "%1", GroupKey: "b", WindowIndex: 0, PaneIndex: 0},
			{PaneID: "%9", GroupKey: "a", WindowIndex: 2, PaneIndex: 0},
			{PaneID: "%4", GroupKey: "a", WindowIndex: 0, PaneIndex: 1},
			{PaneID: "%7", GroupKey: "a", WindowIndex: 0, PaneIndex: 0},
		}
		got := Dedupe(rows)
		want := []string{"%7", "%4", "%9", "%1"}
		if len(got) != len(want) {
			t.Fatalf("want %d rows, got %+v", len(want), got)
		}
		for i, w := range want {
			if got[i].PaneID != w {
				t.Fatalf("order = %+v, want %v", got, want)
			}
		}
	})

	// sort.Slice is explicitly not a stable sort and map iteration order is
	// randomised, so determinism has to be proven rather than assumed. Without
	// a total order, one window with four panes produced four different
	// orderings across 200 runs -- a sidebar that reshuffles every 1.5s poll.
	//
	// The fixture is the verified real case: after a split-and-kill cycle the
	// pane ids run %0 %4 %2 %1 while pane_index runs 0 1 2 3, so this also pins
	// that layout order wins over id order.
	t.Run("repeated calls return the same order", func(t *testing.T) {
		rows := []Row{
			{PaneID: "%0", GroupKey: "w", WindowIndex: 0, PaneIndex: 0},
			{PaneID: "%4", GroupKey: "w", WindowIndex: 0, PaneIndex: 1},
			{PaneID: "%2", GroupKey: "w", WindowIndex: 0, PaneIndex: 2},
			{PaneID: "%1", GroupKey: "w", WindowIndex: 0, PaneIndex: 3},
		}
		want := []string{"%0", "%4", "%2", "%1"}
		for run := 0; run < 200; run++ {
			got := Dedupe(rows)
			for i, w := range want {
				if got[i].PaneID != w {
					t.Fatalf("run %d: order = %+v, want %v", run, got, want)
				}
			}
		}
	})

	// Snapshot returns nil when no server is running. If Dedupe returned an
	// empty slice here the API would emit [] in one case and null in the other,
	// and Task 21's sidebar would map over null.
	t.Run("an empty result is nil, not an empty slice", func(t *testing.T) {
		if got := Dedupe(nil); got != nil {
			t.Fatalf("Dedupe(nil) = %+v, want nil", got)
		}
		if got := Dedupe([]Row{}); got != nil {
			t.Fatalf("Dedupe([]Row{}) = %+v, want nil", got)
		}
	})

	// Task 7's Poller hands this slice to concurrent HTTP readers without
	// copying it, which is only safe if it never aliases the poll's input.
	t.Run("the result does not alias the input", func(t *testing.T) {
		rows := []Row{{PaneID: "%0", GroupKey: "w", Command: "zsh"}}
		got := Dedupe(rows)
		got[0].Command = "mutated"
		if rows[0].Command != "zsh" {
			t.Fatal("Dedupe returned rows aliasing its input")
		}
	})
}
