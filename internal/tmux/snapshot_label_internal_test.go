package tmux

import (
	"context"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/diegok/tmux-web/internal/tmux/testutil"
)

// The parser alone must not lose a pane, with the tmux-side substitution taken
// away.
//
// labelField is the first of the three defences and the only one that can fail
// open: tmux answers a pattern it cannot compile by echoing the value
// unchanged and exiting 0 (measured on 3.7b with a deliberately broken "["
// pattern), and a tmux too old for the s/// modifier expands the whole
// expression to "". So this test runs the real format with labelField swapped
// back for a bare #{@tmux_web_label} -- exactly what the daemon would be reading
// if that layer stopped working -- and requires that ParseRows still returns
// every pane.
//
// It is an internal test because that swap needs labelField, and it uses a real
// server because the point is the bytes tmux actually emits: it asserts they
// arrived raw before parsing them, so it cannot quietly become a test of a
// value tmux had already defanged.
func TestParserAloneSurvivesARawLabelFromRealTmux(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "work", "-x", "80", "-y", "24")
	srv.Run(t, "split-window", "-t", "work")
	srv.Run(t, "set", "-p", "-t", "work:0.1", LabelOption, "neighbour")

	rawFormat := strings.Replace(Format, labelField, "#{"+LabelOption+"}", 1)
	if rawFormat == Format {
		t.Fatalf("labelField is not in Format; this test is reading a format nobody uses")
	}

	c := NewClient(srv.Args())
	for _, tc := range []struct {
		name    string
		label   string
		want    string
		dropped int
	}{
		{"a separator adds fields to the record", "EV" + Sep + "IL", "EV IL", 0},
		// A newline is the case position buys and rejoining cannot: the tail of
		// the label is lost with the line it started, and the pane -- whose
		// every other field is already on the first line -- is not.
		{"a newline cuts the record short", "EV\nIL", "EV", 1},
		{"both", "a" + Sep + "b\nc", "a b", 1},
		{"nothing but dangerous bytes", Sep + "\n" + Sep, "", 1},
		{"invalid UTF-8", "a" + string([]byte{0xff}) + "b", "a\ufffdb", 0},
		{"a very long label", strings.Repeat("\U0001f30d", 2000), strings.Repeat("\U0001f30d", MaxLabel), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv.Run(t, "set", "-p", "-t", "work:0.0", LabelOption, tc.label)
			out, err := c.Run(context.Background(), "list-panes", "-a", "-F", rawFormat)
			if err != nil {
				t.Fatal(err)
			}
			// Without this the test could be passing because tmux sanitised the
			// value, which is the assumption that produced the bug.
			if !strings.Contains(out, tc.label) {
				t.Fatalf("tmux did not emit the label verbatim; there is nothing dangerous to parse")
			}

			rows, dropped, err := ParseRows(out)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 2 {
				t.Fatalf("want both panes, got %d: %+v", len(rows), rows)
			}
			if dropped != tc.dropped {
				t.Errorf("dropped = %d, want %d", dropped, tc.dropped)
			}
			if rows[0].Label != tc.want {
				t.Errorf("Label = %q, want %q", rows[0].Label, tc.want)
			}
			if !utf8.ValidString(rows[0].Label) {
				t.Errorf("Label %q is not valid UTF-8", rows[0].Label)
			}
			// The neighbour is the pane a forged or shifted record would eat.
			if rows[1].Label != "neighbour" {
				t.Errorf("the neighbour's row = %+v, want its own label intact", rows[1])
			}
			if rows[0].PaneID == rows[1].PaneID {
				t.Errorf("both rows are the same pane: %+v", rows)
			}
			for _, r := range rows {
				if !strings.HasPrefix(r.PaneID, "%") || !strings.HasPrefix(r.WindowID, "@") ||
					!strings.HasPrefix(r.SessionID, "$") || r.Command == "" {
					t.Errorf("a shifted record survived as a row: %+v", r)
				}
			}
		})
	}
}

// The format alone must produce one well-formed record per pane, whatever the
// label holds.
//
// This is the first defence on its own terms, and the only test that can fail
// when it weakens: with a tolerant parser behind it, dropping either byte from
// labelField's pattern produces exactly the same rows, so nothing downstream
// notices. Here the raw bytes tmux prints are what is asserted on -- one line
// per pane, fieldCount fields on each -- so a pattern that stops covering the
// separator, or the newline, or the value entirely, fails immediately.
func TestFormatAloneKeepsEveryRecordWellFormed(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "work", "-x", "80", "-y", "24")
	srv.Run(t, "split-window", "-t", "work")
	srv.Run(t, "set", "-p", "-t", "work:0.1", LabelOption, "neighbour")

	c := NewClient(srv.Args())
	for _, tc := range []struct{ name, label string }{
		{"a separator", "EV" + Sep + "IL"},
		{"a newline", "EV\nIL"},
		{"both", "a" + Sep + "b\nc"},
		{"nothing but dangerous bytes", Sep + "\n" + Sep + "\n"},
		// A whole forged record: every field of one, so a format that let it
		// through would produce a plausible extra pane rather than a mess.
		{"a forged record", strings.Join([]string{"", "grp", "$9", "sess", "%99", "0", "@9", "0", "w", "1", "sh", "t", "x"}, Sep) + "\n"},
		{"500 separators", strings.Repeat(Sep, 500)},
		{"500 newlines", strings.Repeat("\n", 500)},
		// The control: a format that answered "" for every label would pass
		// every case above.
		{"a plain label", "plain"},
	} {
		label := tc.label
		t.Run(tc.name, func(t *testing.T) {
			srv.Run(t, "set", "-p", "-t", "work:0.0", LabelOption, label)
			out, err := c.Run(context.Background(), "list-panes", "-a", "-F", Format)
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(out, "\n")
			if len(lines) != 2 {
				t.Fatalf("%d lines for 2 panes: the label split or forged a record: %q", len(lines), out)
			}
			var labels []string
			for i, line := range lines {
				f := strings.Split(line, Sep)
				if len(f) != fieldCount {
					t.Fatalf("line %d has %d fields, want %d: %q", i, len(f), fieldCount, line)
				}
				labels = append(labels, f[fieldCount-1])
			}
			// The neighbour's label proves the field is still being read at
			// all: a labelField that expanded to "" for every pane would
			// otherwise satisfy every assertion above.
			if !slices.Contains(labels, "neighbour") {
				t.Errorf("the neighbour's label is not in the output: %q", labels)
			}
		})
	}
}
