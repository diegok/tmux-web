package tmux

import "testing"

const US = "\x1f"

func TestParseRows(t *testing.T) {
	t.Run("one well formed row", func(t *testing.T) {
		line := "work" + US + "%3" + US + "" + US + "1" + US + "api" + US + "1" + US + "claude" + US + "/home/d/api"
		got, err := ParseRows(line)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Fatalf("want 1 row, got %d", len(got))
		}
		r := got[0]
		if r.GroupKey != "work" || r.PaneID != "%3" || r.AppOwned {
			t.Fatalf("bad row: %+v", r)
		}
		if r.WindowIndex != 1 || r.WindowName != "api" || !r.PaneActive {
			t.Fatalf("bad row: %+v", r)
		}
		if r.Command != "claude" || r.Path != "/home/d/api" {
			t.Fatalf("bad row: %+v", r)
		}
	})

	t.Run("app owned row", func(t *testing.T) {
		line := "work" + US + "%3" + US + "1" + US + "0" + US + "w" + US + "0" + US + "zsh" + US + "/tmp"
		got, _ := ParseRows(line)
		if !got[0].AppOwned {
			t.Fatal("expected AppOwned")
		}
	})

	// tmux sanitizes session and window names but NOT pane_current_path.
	// A path containing a newline splits the record across output lines.
	t.Run("path containing a newline is rejoined", func(t *testing.T) {
		line := "work" + US + "%3" + US + "" + US + "0" + US + "w" + US + "0" + US + "zsh" + US + "/tmp/ev\nil/dir"
		got, err := ParseRows(line)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Fatalf("want 1 row, got %d: %+v", len(got), got)
		}
		if got[0].Path != "/tmp/ev\nil/dir" {
			t.Fatalf("path not rejoined: %q", got[0].Path)
		}
	})

	t.Run("empty output yields no rows", func(t *testing.T) {
		got, err := ParseRows("")
		if err != nil || got != nil {
			t.Fatalf("ParseRows(\"\") = %v, %v; want nil, nil", got, err)
		}
	})

	t.Run("garbage before any valid row is dropped", func(t *testing.T) {
		got, err := ParseRows("nonsense")
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Fatalf("want 0 rows, got %+v", got)
		}
	})
}
