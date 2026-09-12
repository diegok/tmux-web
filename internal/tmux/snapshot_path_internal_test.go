package tmux

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/diegok/tmux-web/internal/tmux/testutil"
)

// The three defences the path gets are the label's three, and this file tests
// each of them on its own terms:
//
//	layer 1  tmux substitutes the two record-breaking bytes out inside the
//	         format string          -- TestFormatAlonePrintsOnePathRecordPerPane
//	layer 2  the path is the last and only variable field of a format string
//	         of its own             -- TestPathFormatShape
//	layer 3  the parser repairs what arrives, because layer 1 can fail open
//	         -- TestParserAloneSurvivesARawPathFromRealTmux
//
// and then the whole stack, against real tmux and real directories, in
// TestSnapshotHostilePaneDirectoryCannotRemoveAPane.

// --- layer 1, as source ------------------------------------------------------

// The pattern must be labelField's pattern, character for character.
//
// Written out in prose it looks like `#{s/[\n\x1f]/ /:…}`, and a bracket set
// RETYPED from that rendering is two characters -- a backslash and an 'n' --
// which leaves real newlines alive and turns every lowercase "n" in a path into
// a space. It works in Go because "\n" in a source literal is the byte, so the
// check is that the byte is there and the two-character spelling is not.
//
// The equality against labelField is what makes this more than a spelling test:
// the two fields defend the same wire against the same two bytes, and a fix
// applied to one must not leave the other behind.
func TestPathFieldIsLabelFieldsPatternOverThePaneDirectory(t *testing.T) {
	if !strings.Contains(pathField, "\n") {
		t.Errorf("pathField = %q: no real newline byte in the bracket set", pathField)
	}
	if !strings.Contains(pathField, Sep) {
		t.Errorf("pathField = %q: no real 0x1f byte in the bracket set", pathField)
	}
	if strings.Contains(pathField, `\n`) {
		t.Errorf("pathField = %q: the bracket set was retyped as a two-character "+
			`backslash-n, which leaves newlines alive and eats every "n"`, pathField)
	}
	if !strings.Contains(pathField, PathVariable) {
		t.Errorf("pathField = %q: does not read %s", pathField, PathVariable)
	}
	if want := strings.Replace(labelField, LabelOption, PathVariable, 1); pathField != want {
		t.Errorf("pathField  = %q\nwant       = %q\nthe two fields must carry the "+
			"same substitution; a pattern fixed on one and not the other is the bug back",
			pathField, want)
	}
}

// --- layer 2, as source ------------------------------------------------------

// The path gets a format string of its own, and is last in it.
//
// Adding it to Format instead would fail WORSE than the original bug: a surplus
// separator in a middle field shifts every later field, the greedy last field
// absorbs the overflow, and the row parses successfully carrying another pane's
// values -- a pane wearing another pane's title, which ParseRows cannot detect.
func TestTheMainFormatStillCarriesNoPathField(t *testing.T) {
	if strings.Contains(Format, PathVariable) {
		t.Fatalf("Format = %q carries the path: the main record may hold exactly "+
			"one unsanitised field, and the label holds it", Format)
	}
	if len(formatFields) != fieldCount {
		t.Errorf("formatFields has %d entries but fieldCount is %d", len(formatFields), fieldCount)
	}
	if formatFields[len(formatFields)-1] != labelField {
		t.Errorf("the last field of Format is %q, want labelField: the one field "+
			"tmux hands over as written has to be last", formatFields[len(formatFields)-1])
	}
}

func TestPathFormatShape(t *testing.T) {
	if got := len(pathFormatFields); got != 3 {
		t.Fatalf("pathFormatFields has %d entries, want 3 (tag, pane id, path)", got)
	}
	if pathFormatFields[0] != pathTag {
		t.Errorf("field 0 = %q, want the block tag %q", pathFormatFields[0], pathTag)
	}
	if pathFormatFields[len(pathFormatFields)-1] != pathField {
		t.Errorf("the path is not the last field of PathFormat: %q", pathFormatFields)
	}
	// Nothing a writer controls can forge a tag, but two blocks sharing one
	// would make every path line look like a report and vice versa.
	if pathTag == snapshotTag || pathTag == reportTag {
		t.Errorf("pathTag %q collides with snapshotTag %q or reportTag %q",
			pathTag, snapshotTag, reportTag)
	}
	if PathFormat != strings.Join(pathFormatFields, Sep) {
		t.Errorf("PathFormat = %q, want the fields joined by the separator", PathFormat)
	}
}

// A third block costs no extra fork: it rides the invocation the poller already
// makes. A separate c.Run would be a second fork per poll, which is the cost
// this whole shape exists to avoid.
func TestBatchArgsReadsThePathInTheSameFork(t *testing.T) {
	args := batchArgs()
	if !slices.Contains(args, PathFormat) {
		t.Fatalf("batchArgs = %q does not read PathFormat", args)
	}
	var commands int
	for _, a := range args {
		if a == "list-panes" {
			commands++
		}
	}
	if commands != 3 {
		t.Errorf("batchArgs runs %d list-panes commands, want 3 in the one invocation: %q",
			commands, args)
	}
	if got := strings.Count(strings.Join(args, " "), " ; "); got != 2 {
		t.Errorf("batchArgs has %d command separators, want 2: %q", got, args)
	}
}

// The snapshot has to be the FIRST command in the list, and that is not a
// stylistic preference: tmux runs a command list in order and stops at the
// first failure. The second half of this test measures that against a real
// server rather than asserting it from memory -- with the path block moved
// ahead of the snapshot, a path read that fails takes the rows with it and the
// sidebar goes blank, which is precisely the failure
// TestABrokenPathReadStillYieldsTheSnapshot exists to prevent and cannot see,
// because it replaces the whole argv.
func TestTheSnapshotBlockRunsFirst(t *testing.T) {
	args := batchArgs()
	if len(args) < 4 || args[0] != "list-panes" || args[3] != Format {
		t.Errorf("the first command of batchArgs is %q, want the snapshot block: "+
			"a later block's failure must cost that block and nothing before it", args[:min(4, len(args))])
	}

	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "probe", "-x", "80", "-y", "24")
	out, err := NewClient(srv.Args()).runKeepingOutput(context.Background(),
		"list-panes", "-t", "nosuch", "-F", PathFormat,
		";", "list-panes", "-a", "-F", Format)
	if err == nil {
		t.Fatal("the probe command list exited 0; it is supposed to fail on its first command")
	}
	if out != "" {
		t.Fatalf("tmux ran a command after a failed one and printed %q; this test's "+
			"premise -- that a command list stops at the first failure -- no longer holds", out)
	}
}

// --- layer 3, as parser ------------------------------------------------------

func TestParsePaths(t *testing.T) {
	line := func(fields ...string) string {
		return strings.Join(append([]string{pathTag}, fields...), Sep)
	}

	t.Run("one path per pane", func(t *testing.T) {
		got := ParsePaths(strings.Join([]string{
			line("%0", "/tmp/one"),
			line("%3", "/tmp/two"),
		}, "\n"))
		want := map[string]string{"%0": "/tmp/one", "%3": "/tmp/two"}
		if len(got) != len(want) {
			t.Fatalf("ParsePaths = %v, want %v", got, want)
		}
		for id, w := range want {
			if got[id] != w {
				t.Errorf("path for %s = %q, want %q", id, got[id], w)
			}
		}
	})

	// The tag is the discriminator. Without the check a report line -- three
	// fields, same shape -- would be read as a pane's working directory, and
	// the daemon would hand an agent's own text to `split-window -c`.
	t.Run("another block's lines are not paths", func(t *testing.T) {
		out := strings.Join([]string{
			goodRow("%1", "0", "0"),
			strings.Join([]string{reportTag, "%1", "1;working;123"}, Sep),
			line("%1", "/tmp/real"),
		}, "\n")
		got := ParsePaths(out)
		if len(got) != 1 || got["%1"] != "/tmp/real" {
			t.Fatalf("ParsePaths = %v, want only the path block's own line", got)
		}
	})

	// The surplus can only come from a separator inside the value, and the path
	// is the last field, so it is rejoined rather than dropped -- the same rule
	// ParseRows applies to the label.
	t.Run("a separator inside the path is rejoined", func(t *testing.T) {
		got := ParsePaths(line("%1", "/tmp/ev"+Sep+"il"))
		if got["%1"] != "/tmp/ev"+Sep+"il" {
			t.Errorf("path = %q, want the value rejoined, not truncated", got["%1"])
		}
	})

	t.Run("empty output is no paths", func(t *testing.T) {
		if got := ParsePaths(""); len(got) != 0 {
			t.Errorf("ParsePaths(\"\") = %v, want empty", got)
		}
	})

	t.Run("a short line is skipped, not indexed", func(t *testing.T) {
		if got := ParsePaths(pathTag + Sep + "%1"); len(got) != 0 {
			t.Errorf("ParsePaths = %v, want nothing for a line with no path field", got)
		}
	})
}

// The path block's lines are not malformed rows. Counting them would log
// "skipped malformed rows" once per pane per poll forever, which trains the
// operator to ignore the one warning that means a pane is missing.
func TestParseRowsIgnoresThePathBlock(t *testing.T) {
	out := strings.Join([]string{
		goodRow("%1", "0", "0"),
		strings.Join([]string{reportTag, "%1", "1;idle;123"}, Sep),
		strings.Join([]string{pathTag, "%1", "/tmp/one"}, Sep),
	}, "\n")
	rows, dropped, err := ParseRows(out)
	if err != nil {
		t.Fatal(err)
	}
	if dropped != 0 {
		t.Errorf("dropped = %d, want 0: the path block is another block, not a broken row", dropped)
	}
	if len(rows) != 1 || rows[0].PaneID != "%1" {
		t.Fatalf("rows = %+v, want the one snapshot row", rows)
	}
}

// --- layer 1, against real tmux ---------------------------------------------

// The format alone must print exactly one well-formed record per pane, whatever
// the pane's directory is called.
//
// This is the only test that fails when layer 1 weakens: with a tolerant parser
// behind it, a pattern that stopped covering one of the two bytes produces the
// same rows and nothing downstream notices. tmux answers a pattern it cannot
// compile by echoing the value and exiting 0 (measured on 3.7b), so "it still
// exits 0" is not evidence of anything.
func TestFormatAlonePrintsOnePathRecordPerPane(t *testing.T) {
	srv := testutil.NewServer(t)
	base := t.TempDir()
	// "plain" is the control, and it carries an "n": a bracket set retyped as a
	// literal backslash-n turns every "n" in the value into a space, which
	// every hostile case below would otherwise still pass.
	plain := mkdir(t, base, "plain-notes")
	srv.Run(t, "new-session", "-d", "-s", "work", "-x", "200", "-y", "200", "-c", plain)

	for _, dir := range []string{"ev\nil", "ev" + Sep + "il", "a" + Sep + "b\nc", "n\nn"} {
		srv.Run(t, "split-window", "-t", "work", "-c", mkdir(t, base, dir))
	}

	c := NewClient(srv.Args())
	out, err := c.Run(context.Background(), "list-panes", "-a", "-F", PathFormat)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(out, "\n")
	if len(lines) != 5 {
		t.Fatalf("%d lines for 5 panes: a directory name split or forged a record:\n%q", len(lines), out)
	}
	var seen []string
	for i, line := range lines {
		f := strings.Split(line, Sep)
		if len(f) != 3 {
			t.Fatalf("line %d has %d fields, want 3: %q", i, len(f), line)
		}
		if f[0] != pathTag {
			t.Errorf("line %d is tagged %q, want %q", i, f[0], pathTag)
		}
		seen = append(seen, f[2])
	}
	// The control proves the field is being read at all and read INTACT: an
	// expression that expanded to "" for every pane, or one that ate its "n"s,
	// would satisfy every assertion above.
	if !slices.Contains(seen, resolved(t, plain)) {
		t.Errorf("the plain pane's directory is not in the output: %q", seen)
	}
}

// --- layer 3, against real tmux ---------------------------------------------

// The parser alone must not lose a pane, with the tmux-side substitution taken
// away.
//
// Layer 1 is the only one of the three that can fail open -- a pattern that
// stops compiling leaves tmux echoing the value untouched and exiting 0 -- so
// this runs the real batch with pathField swapped back for a bare
// #{pane_current_path} and requires every pane to survive it with its identity
// intact. The path itself is damaged there, and that is the accepted cost: a
// truncated directory fails the stat at the point of use, where a missing pane
// would simply be gone from the sidebar.
func TestParserAloneSurvivesARawPathFromRealTmux(t *testing.T) {
	srv := testutil.NewServer(t)
	base := t.TempDir()
	srv.Run(t, "new-session", "-d", "-s", "work", "-x", "80", "-y", "24",
		"-c", mkdir(t, base, "plain-notes"))
	srv.Run(t, "split-window", "-t", "work", "-c", mkdir(t, base, "neighbour"))

	rawFormat := strings.Replace(PathFormat, pathField, "#{"+PathVariable+"}", 1)
	if rawFormat == PathFormat {
		t.Fatalf("pathField is not in PathFormat; this test is reading a format nobody uses")
	}

	c := NewClient(srv.Args())
	for _, dir := range []string{"ev\nil", "ev" + Sep + "il", "a" + Sep + "b\nc"} {
		t.Run(strings.ReplaceAll(strings.ReplaceAll(dir, "\n", "<nl>"), Sep, "<us>"), func(t *testing.T) {
			id := srv.Run(t, "split-window", "-t", "work", "-c", mkdir(t, base, dir),
				"-P", "-F", "#{pane_id}")
			t.Cleanup(func() { _, _ = srv.TryRun("kill-pane", "-t", id) })

			out, err := c.Run(context.Background(),
				"list-panes", "-a", "-F", Format, ";", "list-panes", "-a", "-F", rawFormat)
			if err != nil {
				t.Fatal(err)
			}
			// Without this the test could be passing because tmux defanged the
			// value, which is the assumption that produced the bug.
			if !strings.Contains(out, dir) {
				t.Fatalf("tmux did not emit the directory verbatim; there is nothing dangerous to parse")
			}

			rows, _, err := ParseRows(out)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 3 {
				t.Fatalf("want 3 panes, got %d: a directory name removed or forged a pane: %+v",
					len(rows), rows)
			}
			for _, r := range rows {
				if !strings.HasPrefix(r.PaneID, "%") || !strings.HasPrefix(r.WindowID, "@") ||
					!strings.HasPrefix(r.SessionID, "$") || r.SessionName != "work" || r.Command == "" {
					t.Errorf("a shifted or forged record survived as a row: %+v", r)
				}
			}
			// The map may hold a truncated path for the hostile pane; what it
			// must never hold is another pane's line read as one of its own.
			for id, p := range ParsePaths(out) {
				if !strings.HasPrefix(id, "%") {
					t.Errorf("ParsePaths keyed %q on a line that is not a path record", id)
				}
				if strings.Contains(p, "\n") {
					t.Errorf("path for %s spans a line break: %q", id, p)
				}
			}
		})
	}
}

// --- the whole stack, against real tmux -------------------------------------

// A pane must survive any working directory, whatever bytes are in its name.
//
// This is the bug the path was kept off the wire for, reproduced the only way
// it can be: tmux sanitises session and window names but NOT
// pane_current_path, and #{q:} escapes neither of the two bytes that break this
// format. A directory with a newline in its name forged a whole extra sidebar
// row; one with a 0x1f swallowed the following pane's record and a live pane
// disappeared. A hand-built string proves nothing about that, which is exactly
// how the hole stayed open.
//
// Every directory here is created by this test. None of them comes from the
// machine it runs on.
func TestSnapshotHostilePaneDirectoryCannotRemoveAPane(t *testing.T) {
	srv := testutil.NewServer(t)
	// Named windows running `cat`, not the default shell, because this test
	// compares whole rows against a baseline and a fresh tmux window is not the
	// row it will be a second later. Measured on 3.7b: a window created with no
	// -n is called "tmux" for ~500ms before automatic-rename makes it "zsh", and
	// pane_current_command reads "tmux", then the shell, then briefly whatever
	// the developer's rc forks -- "bash" here -- before settling. settledRows
	// only asks for two identical snapshots 20ms apart, so the baseline lands on
	// a plateau inside that, and every later comparison fails on a field this
	// test is not about. An explicit -n turns automatic-rename off for the
	// window, and a command of our own keeps the machine's login shell, and its
	// rc, out of the fixture entirely.
	srv.Run(t, "new-session", "-d", "-s", "work", "-x", "80", "-y", "24", "-n", "shell", "cat")
	srv.Run(t, "new-window", "-t", "work", "-n", "api", "cat")
	srv.Run(t, "split-window", "-t", "work:api", "cat")
	// A neighbour with a label of its own: a fix that let one pane's fields
	// leak into the next record has to fail here.
	srv.Run(t, "set", "-p", "-t", "work:api.1", LabelOption, "neighbour")

	c := NewClient(srv.Args())
	base := settledRows(t, c)
	if len(base) != 3 {
		t.Fatalf("want 3 panes before any hostile directory, got %d: %+v", len(base), base)
	}
	// The last pane in the last window: splitting from it appends a pane rather
	// than renumbering the ones already there, so the neighbours can be
	// compared field for field.
	source := base[len(base)-1]
	if source.Label != "neighbour" {
		t.Fatalf("the neighbour's label did not survive a clean snapshot: %+v", base)
	}
	for _, r := range base {
		if r.Path == "" {
			t.Fatalf("pane %s has no working directory on the wire: %+v", r.PaneID, r)
		}
	}

	// A directory name that is a whole forged record: if the substitution ever
	// stops covering the newline, this is the shape that puts a pane in the
	// sidebar that does not exist.
	forged := "\n" + strings.Join([]string{
		snapshotTag, "grp", "$9", "sess", "%99", "0", "", "@9", "0", "w", "1", "sh", "t", "x",
	}, Sep)

	for _, tc := range []struct{ name, dir string }{
		// The control, and it carries "n"s: a bracket set retyped as a literal
		// backslash-n maps every "n" to a space, and nothing but an exact
		// comparison against a benign path can see that.
		{"a plain directory", "plain-notes"},
		{"a newline", "ev\nil"},
		{"a separator", "ev" + Sep + "il"},
		{"both", "a" + Sep + "b\nc"},
		{"a forged record", forged},
		{"nothing but dangerous bytes", Sep + "\n" + Sep},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := mkdir(t, t.TempDir(), tc.dir)
			id := srv.Run(t, "split-window", "-t", source.PaneID, "-c", dir,
				"-P", "-F", "#{pane_id}")
			// tmux moves the active pane to the new one, which is a real change
			// to the neighbour's row and would mask the one being looked for.
			srv.Run(t, "select-pane", "-t", source.PaneID)

			rows := settledRows(t, c)
			if len(rows) != 4 {
				t.Fatalf("want 4 panes, got %d: a directory name removed or forged a pane: %+v",
					len(rows), rows)
			}
			var got Row
			seen := map[string]bool{}
			for _, r := range rows {
				seen[r.PaneID] = true
				if r.PaneID == id {
					got = r
					continue
				}
				i := slices.IndexFunc(base, func(b Row) bool { return b.PaneID == r.PaneID })
				if i < 0 {
					t.Fatalf("a pane nothing created is in the snapshot: %+v", r)
				}
				if r != base[i] {
					t.Errorf("another pane's row changed:\n got %+v\nwant %+v", r, base[i])
				}
			}
			if !seen[id] {
				t.Fatalf("pane %s vanished from the snapshot: %+v", id, rows)
			}
			// The identity fields are what the sidebar addresses the pane by; a
			// shifted record keeps the row and ruins them.
			if got.SessionID != source.SessionID || got.WindowID != source.WindowID ||
				got.SessionName != "work" || got.WindowName != "api" || got.Label != "" ||
				got.WindowIndex != source.WindowIndex || got.PaneIndex != source.PaneIndex+1 {
				t.Errorf("the directory shifted the record: %+v", got)
			}
			if want := sanitizedPath(resolved(t, dir)); got.Path != want {
				t.Errorf("Path = %q, want %q", got.Path, want)
			}
			if strings.Contains(got.Path, "\n") || strings.Contains(got.Path, Sep) {
				t.Errorf("Path %q still carries a byte that breaks the record", got.Path)
			}

			srv.Run(t, "kill-pane", "-t", id)
			// Back exactly as it was, so nothing above is a one-way change.
			if after := settledRows(t, c); !slices.Equal(after, base) {
				t.Errorf("after killing the hostile pane the snapshot is %+v, want %+v", after, base)
			}
		})
	}
}

// --- the path block may fail without costing the sidebar ---------------------

// A failure in the third command must cost the paths and never the rows.
//
// Same rule and same seam as TestABrokenReportReadStillYieldsTheSnapshot: the
// earlier commands' output is complete on stdout before the error, so the
// daemon parses stdout on its own terms and does not gate on the exit status.
// The mutant this exists for is "SnapshotAndReports returns early on err != nil"
// -- with a third block, that is one more way for a whole sidebar to go blank.
func TestABrokenPathReadStillYieldsTheSnapshot(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "probe", "-x", "80", "-y", "24")

	orig := batchArgs
	t.Cleanup(func() { batchArgs = orig })
	batchArgs = func() []string {
		return []string{
			"list-panes", "-a", "-F", Format,
			";", "list-panes", "-a", "-F", ReportFormat,
			// The path command, pointed at a target that does not exist.
			";", "list-panes", "-t", "nosuch", "-F", PathFormat,
		}
	}

	rows, reports, err := NewClient(srv.Args()).SnapshotAndReports(context.Background())
	if err != nil {
		t.Fatalf("err = %v; a failed path read must not fail the poll", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want the snapshot block intact despite the nonzero exit", len(rows))
	}
	if rows[0].Path != "" {
		t.Errorf("Path = %q, want empty: nothing reported one", rows[0].Path)
	}
	if len(reports) != 1 {
		t.Errorf("reports = %v, want the report block intact too", reports)
	}
}

// --- helpers ----------------------------------------------------------------

// mkdir creates one directory under parent, named exactly as given -- newlines,
// 0x1f and all -- and returns its path.
func mkdir(t *testing.T, parent, name string) string {
	t.Helper()
	dir := filepath.Join(parent, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %q: %v", name, err)
	}
	return dir
}

// resolved is what tmux will report for a directory: it reads a pane's cwd out
// of /proc, which is fully resolved, while t.TempDir() may hand back a path
// containing a symlink.
func resolved(t *testing.T, dir string) string {
	t.Helper()
	p, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolve %q: %v", dir, err)
	}
	return p
}

// sanitizedPath is what tmux's own substitution leaves of a path: both bytes
// become a space, wherever in the name they are.
func sanitizedPath(p string) string {
	return strings.NewReplacer("\n", " ", Sep, " ").Replace(p)
}

// settledRows returns a batched snapshot that two consecutive reads agree on.
//
// A pane's #{pane_current_command} is "tmux" for the first few milliseconds of
// its life, before the shell has exec'd, so a baseline taken immediately after
// a split disagrees with every later snapshot for a reason that has nothing to
// do with directories. Waiting for the rows to stop moving keeps the
// row-for-row comparison strict instead of dropping the fields that flicker.
func settledRows(t *testing.T, c *Client) []Row {
	t.Helper()
	prev, _, err := c.SnapshotAndReports(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		time.Sleep(20 * time.Millisecond)
		cur, _, err := c.SnapshotAndReports(context.Background())
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
