package tmux

import (
	"strings"
	"testing"
)

// rec builds one snapshot record from its fields, joined by the real separator.
// Tests use Sep rather than a private copy of the byte so that a change to the
// separator cannot leave the suite passing against a stale literal.
func rec(fields ...string) string { return strings.Join(fields, Sep) }

// A valid record, as a named baseline the malformed cases can be varied from.
// Field order matches Format: group, pane id, pane index, app marker, window
// index, window name, pane active, command.
func goodRow(paneID, paneIndex, windowIndex string) string {
	return rec("work", paneID, paneIndex, "", windowIndex, "api", "1", "claude")
}

func TestParseRows(t *testing.T) {
	t.Run("one well formed row", func(t *testing.T) {
		got, dropped, err := ParseRows(rec("work", "%3", "0", "", "1", "api", "1", "claude"))
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
		got, dropped, err := ParseRows(rec("work", "%3", "2", "1", "0", "w", "0", "zsh"))
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
			rec("work", "%7", "0", "", "0", "w", "1", "zsh", "extra"),
			rec("work", "%9", "0", "", "notanint", "w", "1", "zsh"), // bad window index
			rec("work", "%8", "notanint", "", "0", "w", "1", "zsh"), // bad pane index
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
