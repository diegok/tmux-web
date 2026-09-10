package tmux_test

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/diegok/tmux-web/internal/tmux"
	"github.com/diegok/tmux-web/internal/tmux/testutil"
)

// Management verbs against a real tmux. Every one of these is a mutation the
// browser can ask for, and tmux's failure mode for a bad target is almost never
// an error -- it is a successful operation on something else -- so nothing here
// can be established with a fake.

// c1 is U+009F, a C1 control. tmux's own control-character check is
// byte-oriented -- below 0x20 plus 0x7f -- so this one is accepted and stored
// by rename-session, rename-window and set-option alike, and would ride the
// poll into the DOM.
const c1 = "\u009f"

// manageFixture starts a server with one detached session and returns the
// client plus the session, window and pane ids of what it created.
type manageFixture struct {
	srv     *testutil.Server
	c       *tmux.Client
	session string // $N
	window  string // @N
	pane    string // %N
}

func newManageFixture(t *testing.T) *manageFixture {
	t.Helper()
	srv := testutil.NewServer(t)
	f := &manageFixture{srv: srv, c: tmux.NewClient(srv.Args())}
	f.session = srv.Run(t, "new-session", "-d", "-s", "work", "-x", "80", "-y", "24",
		"-P", "-F", "#{session_id}")
	f.window = srv.Run(t, "list-windows", "-t", f.session, "-F", "#{window_id}")
	f.pane = srv.Run(t, "list-panes", "-t", f.session, "-F", "#{pane_id}")
	return f
}

// paneRow returns the snapshot row for a pane, so assertions are made on what
// the sidebar will actually receive rather than on a fresh tmux query.
func paneRow(t *testing.T, c *tmux.Client, paneID string) tmux.Row {
	t.Helper()
	rows, err := c.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	for _, r := range rows {
		if r.PaneID == paneID {
			return r
		}
	}
	t.Fatalf("pane %s is missing from the snapshot: %+v", paneID, rows)
	return tmux.Row{}
}

// realPath is what tmux will report for a directory: it reads the pane's cwd
// out of /proc, which is fully resolved, while t.TempDir() may hand back a path
// containing a symlink.
func realPath(t *testing.T, dir string) string {
	t.Helper()
	p, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolve %s: %v", dir, err)
	}
	return p
}

// paneInDir creates a pane whose working directory is dir and waits until tmux
// agrees, so a test that then deletes dir is deleting a directory the pane is
// really sitting in rather than racing its startup.
func paneInDir(t *testing.T, f *manageFixture, dir string) string {
	t.Helper()
	id := f.srv.Run(t, "split-window", "-h", "-t", f.pane, "-c", dir, "-P", "-F", "#{pane_id}")
	want := realPath(t, dir)
	for i := 0; i < 100; i++ {
		got := f.srv.Run(t, "list-panes", "-t", id, "-f", "#{==:#{pane_id},"+id+"}",
			"-F", "#{pane_current_path}")
		if got == want {
			return id
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("pane %s never reported %s as its working directory", id, want)
	return ""
}

func TestNewSession(t *testing.T) {
	f := newManageFixture(t)

	id, err := f.c.NewSession(context.Background(), "notes", "")
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if err := tmux.ValidateSessionID(id); err != nil {
		t.Fatalf("NewSession returned %q, want a session id: %v", id, err)
	}
	// The id must address the session that was just created, not merely look
	// like an id: name it back out of tmux through that id.
	if got := f.srv.Run(t, "display-message", "-p", "-t", id, "#{session_name}"); got != "notes" {
		t.Errorf("session %s is named %q, want %q", id, got, "notes")
	}
}

// The name is the one input on this verb, and tmux accepts names it cannot
// afterwards address -- "" above all, which exits 0 and creates a session that
// kill-session -t "=" cannot find.
func TestNewSessionRejectsAnUnaddressableName(t *testing.T) {
	f := newManageFixture(t)

	for _, tc := range []struct{ name, why string }{
		{"", `new-session -s "" exits 0 and creates a session no name can address`},
		{"a:b", `":" is a target separator; kill-session -t "=a:b" answers "can't find session: a"`},
		{"-x", `getopt eats it as -s's value, creating a session rename-session cannot name`},
		{"a\x1fb", "0x1f is the snapshot field separator; the row would forge a record"},
		{"a" + c1 + "b", "tmux's control-byte check lets C1 through, into the DOM"},
	} {
		before := f.srv.Run(t, "list-sessions", "-F", "#{session_id}")
		if _, err := f.c.NewSession(context.Background(), tc.name, ""); err == nil {
			t.Errorf("NewSession(%q) = nil, want an error: %s", tc.name, tc.why)
		}
		if after := f.srv.Run(t, "list-sessions", "-F", "#{session_id}"); after != before {
			t.Errorf("NewSession(%q) created a session anyway: %q -> %q", tc.name, before, after)
		}
	}
}

// tmux exits 0 for `new-session -c /gone` and silently starts the shell
// somewhere else, so the owner's typo becomes a session in the wrong place.
func TestNewSessionRejectsAMissingPath(t *testing.T) {
	f := newManageFixture(t)

	gone := filepath.Join(t.TempDir(), "not-there")
	_, err := f.c.NewSession(context.Background(), "notes", gone)
	if err == nil {
		t.Fatal("NewSession with a missing path = nil, want an error")
	}
	if !strings.Contains(err.Error(), gone) {
		t.Errorf("error %v does not name the path %q", err, gone)
	}
	if out := f.srv.Run(t, "list-sessions", "-F", "#{session_name}"); strings.Contains(out, "notes") {
		t.Errorf("NewSession created the session despite the bad path: %q", out)
	}
}

// A path that exists but is not a directory is the same failure wearing a
// different hat: tmux exits 0 and lands elsewhere.
func TestNewSessionRejectsAPathThatIsNotADirectory(t *testing.T) {
	f := newManageFixture(t)
	file := filepath.Join(t.TempDir(), "notes.md")
	if err := os.WriteFile(file, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := f.c.NewSession(context.Background(), "notes", file); err == nil {
		t.Error("NewSession with a file as its path = nil, want an error")
	}
	if out := f.srv.Run(t, "list-sessions", "-F", "#{session_name}"); strings.Contains(out, "notes") {
		t.Errorf("NewSession created the session anyway: %q", out)
	}
}

func TestNewSessionStartsInTheGivenPath(t *testing.T) {
	f := newManageFixture(t)
	dir := t.TempDir()

	id, err := f.c.NewSession(context.Background(), "notes", dir)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if got := f.srv.Run(t, "display-message", "-p", "-t", id, "#{pane_current_path}"); got != realPath(t, dir) {
		t.Errorf("new session started in %q, want %q", got, realPath(t, dir))
	}
}

func TestNewWindowLandsInTheNamedSession(t *testing.T) {
	f := newManageFixture(t)
	other := f.srv.Run(t, "new-session", "-d", "-s", "other", "-P", "-F", "#{session_id}")
	// A decoy created after the target, so that "the current session" -- what
	// tmux falls back to when -t is missing or unusable -- is neither `work`
	// nor `other`. Without it, a new-window with no target at all would land in
	// `other` by accident and this test would pass while proving nothing.
	decoy := f.srv.Run(t, "new-session", "-d", "-s", "decoy", "-P", "-F", "#{session_id}")

	id, err := f.c.NewWindow(context.Background(), other, "api", "")
	if err != nil {
		t.Fatalf("NewWindow: %v", err)
	}
	if err := tmux.ValidateWindowID(id); err != nil {
		t.Fatalf("NewWindow returned %q, want a window id: %v", id, err)
	}
	// The window must belong to the session that was asked for. tmux resolves
	// an unusable target to "current", so a broken -t would put the window in
	// `work` and still exit 0.
	got := f.srv.Run(t, "list-windows", "-t", other, "-F", "#{window_id} #{window_name}")
	if !strings.Contains(got, id+" api") {
		t.Errorf("window %s named api is not in session %s: %q", id, other, got)
	}
	for _, elsewhere := range []string{f.session, decoy} {
		if out := f.srv.Run(t, "list-windows", "-t", elsewhere, "-F", "#{window_id}"); strings.Contains(out, id) {
			t.Errorf("window %s landed in session %s instead: %q", id, elsewhere, out)
		}
	}
}

// A window name is a browser input on two verbs, and tmux stores every one of
// these without complaint.
func TestNewWindowRejectsAnUnusableName(t *testing.T) {
	f := newManageFixture(t)

	for _, tc := range []struct{ name, why string }{
		{"w.y", `"." is a target separator, so the window can never be addressed by name`},
		{"a:b", `":" is the other one`},
		{"a" + c1 + "b", "tmux's control-byte check is byte-oriented and lets C1 through"},
		{"a\x1fb", "0x1f would split the snapshot record and drop the pane from the sidebar"},
	} {
		before := f.srv.Run(t, "list-windows", "-t", f.session, "-F", "#{window_id}")
		if _, err := f.c.NewWindow(context.Background(), f.session, tc.name, ""); err == nil {
			t.Errorf("NewWindow(name=%q) = nil, want an error: %s", tc.name, tc.why)
		}
		if after := f.srv.Run(t, "list-windows", "-t", f.session, "-F", "#{window_id}"); after != before {
			t.Errorf("NewWindow(name=%q) created a window anyway: %q -> %q", tc.name, before, after)
		}
	}
}

// An omitted name is not a rejected one: the dialog's name field is optional,
// and tmux then applies its own automatic name.
func TestNewWindowWithoutANameIsAllowed(t *testing.T) {
	f := newManageFixture(t)

	id, err := f.c.NewWindow(context.Background(), f.session, "", "")
	if err != nil {
		t.Fatalf("NewWindow with no name: %v", err)
	}
	if got := f.srv.Run(t, "list-windows", "-t", f.session, "-f", "#{==:#{window_id},"+id+"}",
		"-F", "#{window_name}"); got == "" {
		t.Error("the new window has an empty name, which is a blank sidebar row")
	}
}

// The path is resolved from the pane server-side and never crosses the wire.
func TestNewWindowInheritsThePaneWorkingDirectory(t *testing.T) {
	f := newManageFixture(t)
	dir := t.TempDir()
	src := paneInDir(t, f, dir)

	id, err := f.c.NewWindow(context.Background(), f.session, "", src)
	if err != nil {
		t.Fatalf("NewWindow: %v", err)
	}
	if got := f.srv.Run(t, "list-panes", "-t", id, "-F", "#{pane_current_path}"); got != realPath(t, dir) {
		t.Errorf("new window opened in %q, want the source pane's %q", got, realPath(t, dir))
	}
}

func TestSplitPaneInheritsThePaneWorkingDirectory(t *testing.T) {
	f := newManageFixture(t)
	dir := t.TempDir()
	src := paneInDir(t, f, dir)

	id, err := f.c.SplitPane(context.Background(), src, "down")
	if err != nil {
		t.Fatalf("SplitPane: %v", err)
	}
	if err := tmux.ValidatePaneID(id); err != nil {
		t.Fatalf("SplitPane returned %q, want a pane id: %v", id, err)
	}
	got := f.srv.Run(t, "list-panes", "-t", f.session, "-f", "#{==:#{pane_id},"+id+"}",
		"-F", "#{pane_current_path}")
	if got != realPath(t, dir) {
		t.Errorf("split opened in %q, want the source pane's %q", got, realPath(t, dir))
	}
}

// `split-window -c /gone` exits 0 and lands somewhere else entirely, so a pane
// whose directory was deleted would silently open in the wrong place. Linux
// reports such a cwd as "<path> (deleted)", which is what the stat catches.
func TestSplitPaneReportsAWorkingDirectoryThatIsGone(t *testing.T) {
	f := newManageFixture(t)
	dir := filepath.Join(t.TempDir(), "scratch")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	src := paneInDir(t, f, dir)
	want := realPath(t, dir)
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}

	before := f.srv.Run(t, "list-panes", "-t", f.session, "-F", "#{pane_id}")
	_, err := f.c.SplitPane(context.Background(), src, "right")
	if err == nil {
		t.Fatal("SplitPane from a deleted directory = nil, want an error")
	}
	// The message has to name the directory: "no such file or directory" alone
	// tells the owner nothing about which one.
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error %v does not name the directory %q it could not use", err, want)
	}
	if after := f.srv.Run(t, "list-panes", "-t", f.session, "-F", "#{pane_id}"); after != before {
		t.Errorf("SplitPane split anyway: %q -> %q", before, after)
	}
}

func TestNewWindowReportsAWorkingDirectoryThatIsGone(t *testing.T) {
	f := newManageFixture(t)
	dir := filepath.Join(t.TempDir(), "scratch")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	src := paneInDir(t, f, dir)
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}

	before := f.srv.Run(t, "list-windows", "-t", f.session, "-F", "#{window_id}")
	if _, err := f.c.NewWindow(context.Background(), f.session, "api", src); err == nil {
		t.Fatal("NewWindow from a deleted directory = nil, want an error")
	}
	if after := f.srv.Run(t, "list-windows", "-t", f.session, "-F", "#{window_id}"); after != before {
		t.Errorf("NewWindow created a window anyway: %q -> %q", before, after)
	}
}

// -h and -v are not interchangeable, and neither is the word the browser sends.
func TestSplitPaneDirections(t *testing.T) {
	f := newManageFixture(t)

	right, err := f.c.SplitPane(context.Background(), f.pane, "right")
	if err != nil {
		t.Fatalf("SplitPane right: %v", err)
	}
	// Geometry, not "a pane appeared": a right split puts the new pane beside
	// the source, on the same top row; a down split puts it below. Only the
	// geometry tells -h from -v.
	if top := f.srv.Run(t, "list-panes", "-t", f.session, "-f", "#{==:#{pane_id},"+right+"}",
		"-F", "#{pane_top}"); top != "0" {
		t.Errorf("right split has pane_top %q, want 0: it split downwards", top)
	}

	down, err := f.c.SplitPane(context.Background(), f.pane, "down")
	if err != nil {
		t.Fatalf("SplitPane down: %v", err)
	}
	if top := f.srv.Run(t, "list-panes", "-t", f.session, "-f", "#{==:#{pane_id},"+down+"}",
		"-F", "#{pane_top}"); top == "0" {
		t.Errorf("down split has pane_top %q, want a row below 0: it split sideways", top)
	}
}

func TestSplitPaneRejectsAnUnknownDirection(t *testing.T) {
	f := newManageFixture(t)

	for _, dir := range []string{"", "left", "up", "-h", "RIGHT", "horizontal"} {
		if _, err := f.c.SplitPane(context.Background(), f.pane, dir); err == nil {
			t.Errorf("SplitPane(direction=%q) = nil, want an error", dir)
		}
	}
	if out := f.srv.Run(t, "list-panes", "-t", f.session, "-F", "#{pane_id}"); out != f.pane {
		t.Errorf("a rejected direction still split the window: %q", out)
	}
}

// Ids are checked per kind because tmux resolves the wrong kind rather than
// refusing it, and an unvalidated empty id means "whatever is current".
func TestManagementVerbsRejectTheWrongKindOfTarget(t *testing.T) {
	f := newManageFixture(t)
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		call func(target string) error
		bad  []string
	}{
		{"SplitPane", func(s string) error { _, err := f.c.SplitPane(ctx, s, "right"); return err },
			[]string{"", f.window, f.session, "work"}},
		{"SetLabel", func(s string) error { return f.c.SetLabel(ctx, s, "x") },
			[]string{"", f.window, f.session}},
		{"ToggleZoom", func(s string) error { return f.c.ToggleZoom(ctx, s) },
			[]string{"", f.window, f.session}},
		{"KillPane", func(s string) error { return f.c.KillPane(ctx, s) },
			[]string{"", f.window, f.session}},
		// kill-window -t %N kills the window CONTAINING pane N, successfully.
		{"KillWindow", func(s string) error { return f.c.KillWindow(ctx, s) },
			[]string{"", f.pane, f.session, "work"}},
		{"RenameWindow", func(s string) error { return f.c.RenameWindow(ctx, s, "api") },
			[]string{"", f.pane, f.session, "work"}},
		{"KillSessionID", func(s string) error { return f.c.KillSessionID(ctx, s) },
			[]string{"", f.pane, f.window, "work"}},
		{"RenameSession", func(s string) error { return f.c.RenameSession(ctx, s, "api") },
			[]string{"", f.pane, f.window, "work"}},
		{"NewWindow", func(s string) error { _, err := f.c.NewWindow(ctx, s, "api", ""); return err },
			[]string{"", f.pane, f.window, "work"}},
		{"NewWindow fromPane", func(s string) error { _, err := f.c.NewWindow(ctx, f.session, "api", s); return err },
			[]string{f.window, f.session, "work"}},
	} {
		for _, bad := range tc.bad {
			if err := tc.call(bad); err == nil {
				t.Errorf("%s(%q) = nil, want an error: it is not that kind of id", tc.name, bad)
			}
		}
	}
	// Nothing above may have taken effect: the fixture is intact.
	if out := f.srv.Run(t, "list-panes", "-t", f.session, "-F", "#{pane_id}"); out != f.pane {
		t.Errorf("panes changed: %q, want %q", out, f.pane)
	}
	if out := f.srv.Run(t, "list-sessions", "-F", "#{session_name}"); out != "work" {
		t.Errorf("sessions changed: %q, want %q", out, "work")
	}
	if out := f.srv.Run(t, "list-windows", "-t", f.session, "-F", "#{window_id}"); out != f.window {
		t.Errorf("windows changed: %q, want %q", out, f.window)
	}
}

// The test revision 1's design would have failed. It renames TWICE on purpose:
// tmux keeps the pre-rename name in session_group forever, so after the first
// rename an implementation addressing the session by name -- its group key, the
// only name the sidebar has -- targets a name that no longer exists. One rename
// passes under that bug; two do not.
func TestRenameSessionIsVisibleInTheNextSnapshot(t *testing.T) {
	f := newManageFixture(t)
	// A grouped session, as any open browser tab creates. Without it the group
	// key is just the live name and the whole hazard is invisible.
	f.srv.Run(t, "new-session", "-d", "-t", "work", "-s", "_web-sim")
	f.srv.Run(t, "set", "-t", "_web-sim", tmux.AppOption, "1")

	for _, want := range []string{"api", "api2"} {
		if err := f.c.RenameSession(context.Background(), f.session, want); err != nil {
			t.Fatalf("RenameSession to %q: %v", want, err)
		}
		row := paneRow(t, f.c, f.pane)
		if row.SessionName != want {
			t.Fatalf("snapshot reports session name %q, want %q", row.SessionName, want)
		}
		// And the reason the id is what gets targeted: the group key is stuck
		// at the session's original name.
		if row.GroupKey != "work" {
			t.Errorf("group key = %q, want %q: tmux keeps the pre-rename name", row.GroupKey, "work")
		}
	}
}

func TestRenameSessionRejectsANameTmuxCannotUndo(t *testing.T) {
	f := newManageFixture(t)

	for _, name := range []string{"", "   ", "a:b", "-x", "a\x1fb", "a" + c1 + "b"} {
		if err := f.c.RenameSession(context.Background(), f.session, name); err == nil {
			t.Errorf("RenameSession(%q) = nil, want an error", name)
		}
	}
	if out := f.srv.Run(t, "list-sessions", "-F", "#{session_name}"); out != "work" {
		t.Errorf("session was renamed anyway: %q", out)
	}
}

func TestRenameWindowIsVisibleInTheNextSnapshot(t *testing.T) {
	f := newManageFixture(t)

	if err := f.c.RenameWindow(context.Background(), f.window, "api"); err != nil {
		t.Fatalf("RenameWindow: %v", err)
	}
	if got := paneRow(t, f.c, f.pane).WindowName; got != "api" {
		t.Errorf("snapshot reports window name %q, want %q", got, "api")
	}
}

func TestRenameWindowRejectsAnUnusableName(t *testing.T) {
	f := newManageFixture(t)
	f.srv.Run(t, "rename-window", "-t", f.window, "keep")

	// tmux accepts every one of these on rename-window and exits 0 -- including
	// "", which leaves a window with no name at all and a blank sidebar row.
	for _, name := range []string{"", "   ", "w.y", "a:b", "-z", "a" + c1 + "b", "a\x1fb"} {
		if err := f.c.RenameWindow(context.Background(), f.window, name); err == nil {
			t.Errorf("RenameWindow(%q) = nil, want an error", name)
		}
	}
	if got := f.srv.Run(t, "list-windows", "-t", f.session, "-F", "#{window_name}"); got != "keep" {
		t.Errorf("window was renamed anyway: %q", got)
	}
}

func TestSetLabelIsVisibleInTheNextSnapshot(t *testing.T) {
	f := newManageFixture(t)

	if err := f.c.SetLabel(context.Background(), f.pane, "build"); err != nil {
		t.Fatalf("SetLabel: %v", err)
	}
	if got := paneRow(t, f.c, f.pane).Label; got != "build" {
		t.Errorf("snapshot reports label %q, want %q", got, "build")
	}
}

// A label is display text, not a target, so the rules that protect names from
// tmux's own parser do not apply to it: a label may start with a dash or carry
// a colon, and it must still survive the round trip intact.
func TestSetLabelKeepsPunctuationThatNamesCannotHave(t *testing.T) {
	f := newManageFixture(t)

	for _, label := range []string{"-x", "api: build", "1.2.3", "ñandú 🙂"} {
		if err := f.c.SetLabel(context.Background(), f.pane, label); err != nil {
			t.Fatalf("SetLabel(%q): %v", label, err)
		}
		if got := paneRow(t, f.c, f.pane).Label; got != label {
			t.Errorf("label round-tripped as %q, want %q", got, label)
		}
	}
}

func TestSetLabelClears(t *testing.T) {
	f := newManageFixture(t)
	if err := f.c.SetLabel(context.Background(), f.pane, "build"); err != nil {
		t.Fatalf("SetLabel: %v", err)
	}

	if err := f.c.SetLabel(context.Background(), f.pane, ""); err != nil {
		t.Fatalf("SetLabel to empty: %v", err)
	}
	if got := paneRow(t, f.c, f.pane).Label; got != "" {
		t.Errorf("label after clearing = %q, want empty", got)
	}
	// Clearing an already-clear label is what an empty dialog sends twice; it
	// must not fail. `set -pu` on an option that was never set is only quiet
	// under some tmux builds, so pin it rather than assume.
	if err := f.c.SetLabel(context.Background(), f.pane, ""); err != nil {
		t.Errorf("clearing an already-clear label: %v", err)
	}

	// A label of spaces clears too rather than being stored: it is a row title
	// the owner can neither read nor tell apart from an unset one.
	if err := f.c.SetLabel(context.Background(), f.pane, "build"); err != nil {
		t.Fatalf("SetLabel: %v", err)
	}
	if err := f.c.SetLabel(context.Background(), f.pane, "   "); err != nil {
		t.Fatalf("SetLabel to spaces: %v", err)
	}
	if got := paneRow(t, f.c, f.pane).Label; got != "" {
		t.Errorf("label after a whitespace-only write = %q, want empty", got)
	}
}

// The worst failure this project has: tmux does not sanitise user option
// values, so a label carrying 0x1f or a newline splits the snapshot record,
// ParseRows drops the line, and the pane disappears from the sidebar.
func TestSetLabelRejectsControlBytesAndKeepsThePaneVisible(t *testing.T) {
	f := newManageFixture(t)

	for _, tc := range []struct{ label, why string }{
		{"EV\x1fIL", "0x1f is the snapshot field separator; the record gains a field and is dropped"},
		{"EV\nIL", "a newline splits the record in two; both halves are dropped"},
		{"EV" + c1 + "IL", "tmux's own check is byte-oriented and lets C1 controls through"},
		{"EV\tIL", "a tab is a control character and has no place in a one-line row"},
		{"EV\x7fIL", "DEL"},
		{"EV\xffIL", "invalid UTF-8: encoding/json rewrites it and the sidebar shows a lie"},
	} {
		if err := f.c.SetLabel(context.Background(), f.pane, tc.label); err == nil {
			t.Errorf("SetLabel(%q) = nil, want an error: %s", tc.label, tc.why)
		}
		// The point of the rejection: the pane is still in the snapshot at all,
		// and the option was not written.
		if got := paneRow(t, f.c, f.pane).Label; got != "" {
			t.Errorf("label = %q after a rejected write, want empty", got)
		}
	}
}

// The cap is in runes, not bytes: it is a column budget for a sidebar row, and
// a byte cap would refuse a perfectly readable label in any language that is
// not English. Every rune here is two bytes, so a byte cap fails the first
// case -- which is the point of choosing "ñ" over "z".
func TestSetLabelCapsLengthInRunes(t *testing.T) {
	f := newManageFixture(t)

	atLimit := strings.Repeat("ñ", tmux.MaxLabel)
	if err := f.c.SetLabel(context.Background(), f.pane, atLimit); err != nil {
		t.Fatalf("SetLabel with %d two-byte runes = %v, want nil", tmux.MaxLabel, err)
	}
	if got := paneRow(t, f.c, f.pane).Label; got != atLimit {
		t.Errorf("label round-tripped as %d bytes, want %d", len(got), len(atLimit))
	}

	tooLong := strings.Repeat("ñ", tmux.MaxLabel+1)
	if err := f.c.SetLabel(context.Background(), f.pane, tooLong); err == nil {
		t.Errorf("SetLabel with %d runes = nil, want an error", tmux.MaxLabel+1)
	}
	if got := paneRow(t, f.c, f.pane).Label; got != atLimit {
		t.Error("an over-long label overwrote the stored one")
	}
}

func TestToggleZoom(t *testing.T) {
	f := newManageFixture(t)
	// tmux refuses to zoom a window with one pane, so the fixture needs two.
	f.srv.Run(t, "split-window", "-h", "-t", f.pane)

	zoomed := func() string {
		return f.srv.Run(t, "display-message", "-p", "-t", f.pane, "#{window_zoomed_flag}")
	}
	if got := zoomed(); got != "0" {
		t.Fatalf("window starts zoomed=%q, want 0", got)
	}
	if err := f.c.ToggleZoom(context.Background(), f.pane); err != nil {
		t.Fatalf("ToggleZoom: %v", err)
	}
	if got := zoomed(); got != "1" {
		t.Errorf("after one toggle zoomed=%q, want 1", got)
	}
	if err := f.c.ToggleZoom(context.Background(), f.pane); err != nil {
		t.Fatalf("ToggleZoom back: %v", err)
	}
	if got := zoomed(); got != "0" {
		t.Errorf("after two toggles zoomed=%q, want 0", got)
	}
}

func TestKillSessionIDKillsTheSessionWithThatID(t *testing.T) {
	f := newManageFixture(t)
	doomed := f.srv.Run(t, "new-session", "-d", "-s", "doomed", "-P", "-F", "#{session_id}")

	if err := f.c.KillSessionID(context.Background(), doomed); err != nil {
		t.Fatalf("KillSessionID: %v", err)
	}
	if out := f.srv.Run(t, "list-sessions", "-F", "#{session_name}"); out != "work" {
		t.Errorf("sessions after the kill = %q, want only %q", out, "work")
	}
}

// A session name is not a session id, and tmux would resolve a name -- by
// prefix, silently -- into a kill of something the browser did not name.
func TestKillSessionIDRefusesAName(t *testing.T) {
	f := newManageFixture(t)
	f.srv.Run(t, "new-session", "-d", "-s", "work-notes")

	for _, target := range []string{"work", "work-", "=work", ""} {
		if err := f.c.KillSessionID(context.Background(), target); err == nil {
			t.Errorf("KillSessionID(%q) = nil, want an error: it is a name, not a $id", target)
		}
	}
	out := f.srv.Run(t, "list-sessions", "-F", "#{session_name}")
	names := strings.Split(out, "\n")
	sort.Strings(names)
	if len(names) != 2 || names[0] != "work" || names[1] != "work-notes" {
		t.Errorf("a name-shaped target killed something: sessions are %q", out)
	}
}

// The app's own sessions belong to the daemon, not the owner: killing one drops
// a live browser tab's socket for no reason the owner could understand. This
// guards the direct path only -- killing a base session's last window still
// destroys the whole group, which is the dialog's job to say, not this one's.
func TestKillSessionIDRefusesAnAppSession(t *testing.T) {
	f := newManageFixture(t)
	app := f.srv.Run(t, "new-session", "-d", "-t", "work", "-s", "_web-sim", "-P", "-F", "#{session_id}")
	f.srv.Run(t, "set", "-t", app, tmux.AppOption, "1")

	err := f.c.KillSessionID(context.Background(), app)
	if err == nil {
		t.Fatal("KillSessionID on an app session = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), tmux.AppOption) {
		t.Errorf("error %v does not say why it refused", err)
	}
	if out := f.srv.Run(t, "list-sessions", "-F", "#{session_name}"); !strings.Contains(out, "_web-sim") {
		t.Errorf("the app session was killed anyway: %q", out)
	}

	// And a user session that merely looks like one is not protected: app
	// sessions are identified by the option, never by name. Taking "_web-"
	// names away from the user is the bug session.go already warns about.
	named := f.srv.Run(t, "new-session", "-d", "-s", "_web-notes", "-P", "-F", "#{session_id}")
	if err := f.c.KillSessionID(context.Background(), named); err != nil {
		t.Errorf("KillSessionID on a user session named _web-notes: %v", err)
	}
}

func TestKillWindow(t *testing.T) {
	f := newManageFixture(t)
	doomed := f.srv.Run(t, "new-window", "-t", f.session, "-P", "-F", "#{window_id}")

	if err := f.c.KillWindow(context.Background(), doomed); err != nil {
		t.Fatalf("KillWindow: %v", err)
	}
	if out := f.srv.Run(t, "list-windows", "-t", f.session, "-F", "#{window_id}"); out != f.window {
		t.Errorf("windows after the kill = %q, want only %q", out, f.window)
	}
}

func TestKillPane(t *testing.T) {
	f := newManageFixture(t)
	doomed := f.srv.Run(t, "split-window", "-h", "-t", f.pane, "-P", "-F", "#{pane_id}")

	if err := f.c.KillPane(context.Background(), doomed); err != nil {
		t.Fatalf("KillPane: %v", err)
	}
	if out := f.srv.Run(t, "list-panes", "-t", f.session, "-F", "#{pane_id}"); out != f.pane {
		t.Errorf("panes after the kill = %q, want only %q", out, f.pane)
	}
}

// A stale id -- the row the poll showed 1.5s ago -- must fail, and fail with
// tmux's own message, because that message is what the toast shows.
func TestVerbsReportAStaleID(t *testing.T) {
	f := newManageFixture(t)
	ctx := context.Background()

	splitErr := func() error { _, err := f.c.SplitPane(ctx, "%99", "right"); return err }()
	windowErr := func() error { _, err := f.c.NewWindow(ctx, "$99", "api", ""); return err }()
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"KillPane", f.c.KillPane(ctx, "%99"), "can't find pane"},
		{"KillWindow", f.c.KillWindow(ctx, "@99"), "can't find window"},
		{"KillSessionID", f.c.KillSessionID(ctx, "$99"), "can't find session"},
		// set-option words it differently from the rest; both are tmux's own
		// text, and the toast shows whichever it gets.
		{"SetLabel", f.c.SetLabel(ctx, "%99", "x"), "no such pane"},
		// Clearing is a separate tmux command (set -u) and has to fail too:
		// -q would make it quiet, and a cleared label on a dead pane would
		// report success.
		{"SetLabel clearing", f.c.SetLabel(ctx, "%99", ""), "no such pane"},
		{"ToggleZoom", f.c.ToggleZoom(ctx, "%99"), "can't find pane"},
		{"RenameWindow", f.c.RenameWindow(ctx, "@99", "api"), "can't find window"},
		{"RenameSession", f.c.RenameSession(ctx, "$99", "api"), "can't find session"},
		{"SplitPane", splitErr, "can't find pane"},
		{"NewWindow", windowErr, "can't find session"},
	} {
		if tc.err == nil {
			t.Errorf("%s with a stale id = nil, want an error", tc.name)
			continue
		}
		if !strings.Contains(tc.err.Error(), tc.want) {
			t.Errorf("%s error = %v, want tmux's %q", tc.name, tc.err, tc.want)
		}
	}
}
