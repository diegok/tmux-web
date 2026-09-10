package tmux_test

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/diegok/tmux-web/internal/tmux"
	"github.com/diegok/tmux-web/internal/tmux/testutil"
)

func TestSnapshotAgainstRealTmux(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "work", "-x", "80", "-y", "24")
	srv.Run(t, "new-window", "-t", "work")

	// A user session whose name starts with the app's prefix. It must never be
	// hidden or swept: only the @wterm_web option marks an app session.
	srv.Run(t, "new-session", "-d", "-s", "_web-notes")

	// Simulate an open browser tab: a grouped, app-marked session.
	srv.Run(t, "new-session", "-d", "-t", "work", "-s", "_web-sim")
	srv.Run(t, "set", "-t", "_web-sim", "@wterm_web", "1")

	c := tmux.NewClient(srv.Args())
	panes, err := c.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	byPane := map[string]tmux.Row{}
	for _, p := range panes {
		if _, dup := byPane[p.PaneID]; dup {
			t.Fatalf("pane %s reported twice: %+v", p.PaneID, panes)
		}
		byPane[p.PaneID] = p
	}
	if len(byPane) != 3 {
		t.Fatalf("want 3 panes (work x2, _web-notes x1), got %d: %+v", len(byPane), panes)
	}

	var sawNotes bool
	for _, p := range panes {
		if p.GroupKey == "_web-notes" {
			sawNotes = true
		}
	}
	if !sawNotes {
		t.Fatal("a user session named _web-notes must not be hidden")
	}
}

// The reason SessionID and SessionName exist at all, pinned against a real tmux
// because no fixture can prove it: tmux keeps the PRE-RENAME name in
// session_group. Rename work3 to api and every member still reports
// group=work3, so a sidebar keyed on the group shows the old name forever and
// `kill-session -t '=work3'` fails while the session lives on as api.
//
// It also pins the two other new fields against tmux's real format vocabulary:
// a mistyped #{@wterm_label} or #{pane_title} expands to empty rather than
// erroring, so the field count -- and every unit test -- stays happy.
func TestSnapshotCarriesLiveSessionIdentityAfterRename(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "work3", "-x", "80", "-y", "24")
	// A grouped, app-marked member: what an open browser tab creates, and what
	// puts a session_group on the base session in the first place.
	srv.Run(t, "new-session", "-d", "-t", "work3", "-s", "_web-sim")
	srv.Run(t, "set", "-t", "_web-sim", "@wterm_web", "1")
	srv.Run(t, "rename-session", "-t", "work3", "api")
	srv.Run(t, "set", "-p", "-t", "api:0.0", "@wterm_label", "reviewer")

	panes, err := tmux.NewClient(srv.Args()).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(panes) != 1 {
		t.Fatalf("want the one shared pane, got %d: %+v", len(panes), panes)
	}
	r := panes[0]
	if r.GroupKey != "work3" {
		t.Errorf("GroupKey = %q, want the stale group name work3; if tmux now "+
			"renames the group, the SessionName field is no longer load-bearing "+
			"and this design decision should be revisited", r.GroupKey)
	}
	if r.SessionName != "api" {
		t.Errorf("SessionName = %q, want the live name api", r.SessionName)
	}
	if !strings.HasPrefix(r.SessionID, "$") {
		t.Errorf("SessionID = %q, want a $N session id", r.SessionID)
	}
	// Addressability is the point of carrying the id: the name-based target
	// that v1 would have used is now broken, and the id must not be.
	if _, err := srv.TryRun("kill-session", "-t", "="+r.GroupKey); err == nil {
		t.Error("kill-session by group key succeeded; the premise of this test is gone")
	}
	if out, err := srv.TryRun("display-message", "-p", "-t", r.SessionID, "#{session_name}"); err != nil ||
		strings.TrimSpace(out) != "api" {
		t.Errorf("the session id did not address the session: %q, %v", out, err)
	}
	if r.Label != "reviewer" {
		t.Errorf("Label = %q, want reviewer from @wterm_label", r.Label)
	}
	// tmux defaults a pane title to the hostname, so a working #{pane_title} is
	// never empty -- which is what makes an empty one evidence of a typo.
	if r.Title == "" {
		t.Error("Title is empty; #{pane_title} did not expand")
	}
}

// The tmux server's generation. Pane ids restart at %0 when the server
// restarts, so the browser keys its per-pane "done" memory on this; a wrong or
// constant value silently suppresses badges on unrelated new panes.
func TestServerStartAgainstRealTmux(t *testing.T) {
	srv := testutil.NewServer(t)

	c := tmux.NewClient(srv.Args())
	// No server is not an error, exactly as for Snapshot: a machine where tmux
	// has never started answers with no panes and no generation.
	if got, err := c.ServerStart(context.Background()); got != "" || err != nil {
		t.Fatalf("ServerStart with no server = %q, %v; want \"\", nil", got, err)
	}

	srv.Run(t, "new-session", "-d", "-s", "work", "-x", "80", "-y", "24")
	first, err := c.ServerStart(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Pinned as a number rather than merely non-empty: a mistyped format
	// expands to the empty string, and a literal one to itself, so shape is the
	// only thing separating a working #{start_time} from a typo.
	if _, err := strconv.Atoi(first); err != nil {
		t.Fatalf("ServerStart = %q, want a unix timestamp: %v", first, err)
	}
	if again, err := c.ServerStart(context.Background()); err != nil || again != first {
		t.Fatalf("ServerStart changed without a restart: %q then %q (%v)", first, again, err)
	}
}

// Regression: killing the base session must not blank the sidebar.
func TestSnapshotSurvivesBaseSessionKill(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "work", "-x", "80", "-y", "24")
	srv.Run(t, "new-window", "-t", "work")
	srv.Run(t, "new-session", "-d", "-t", "work", "-s", "_web-sim")
	srv.Run(t, "set", "-t", "_web-sim", "@wterm_web", "1")

	srv.Run(t, "kill-session", "-t", "work")

	c := tmux.NewClient(srv.Args())
	panes, err := c.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(panes) != 2 {
		t.Fatalf("agents must stay visible after the base session dies, got %+v", panes)
	}
	for _, p := range panes {
		if p.GroupKey != "work" {
			t.Fatalf("panes should still be labelled by group %q: %+v", "work", p)
		}
	}
}

func TestSnapshotWithNoServerIsNotAnError(t *testing.T) {
	rows, err := tmux.NewClient(testutil.NewServer(t).Args()).Snapshot(context.Background())
	if err != nil || rows != nil {
		t.Fatalf("Snapshot on a dead server = %v, %v; want nil, nil", rows, err)
	}
}

// The socket file outlives the server, and tmux then words the same condition
// differently -- "no server running on <path>" instead of the connect error a
// never-started socket gives. Without this the ECONNREFUSED half of noServer is
// unexercised, which is the half a long-running deployment actually hits: the
// user quits their last session while the sidebar is polling.
func TestSnapshotWithNoServerAfterExitIsNotAnError(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "work")
	srv.Run(t, "kill-server")
	if _, err := os.Stat(srv.SocketPath()); err != nil {
		t.Fatalf("socket file must outlive the server, else this duplicates the ENOENT test: %v", err)
	}

	// kill-server returns before the server has finished exiting, and a command
	// that lands in that window fails with "server exited unexpectedly". Poll
	// until it settles rather than asserting on the transient.
	c := tmux.NewClient(srv.Args())
	var err error
	var rows []tmux.Row
	deadline := time.Now().Add(5 * time.Second)
	for {
		rows, err = c.Snapshot(context.Background())
		if err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err != nil || rows != nil {
		t.Fatalf("Snapshot on an exited server = %v, %v; want nil, nil", rows, err)
	}
}

// Two browser tabs are two sessions grouped onto the same base, and they share
// its window list. Clicking a pane in one tab must move that tab and nothing
// else: not the other tab, and above all not the user's own session, which is
// attached to a terminal they are looking at.
func TestSelectPaneMovesOnlyTheGivenSession(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "work", "-x", "80", "-y", "24")
	srv.Run(t, "new-window", "-t", "work")
	srv.Run(t, "new-window", "-t", "work")
	srv.Run(t, "split-window", "-t", "=work:2")
	srv.Run(t, "new-session", "-d", "-t", "work", "-s", "_web-a")
	srv.Run(t, "new-session", "-d", "-t", "work", "-s", "_web-b")

	// Park every session on window 0 and the split window on its first pane,
	// so any movement below is the one this test asked for.
	for _, s := range []string{"work", "_web-a", "_web-b"} {
		srv.Run(t, "select-window", "-t", "="+s+":0")
	}
	srv.Run(t, "select-pane", "-t", "=work:2.0")
	target := paneAt(t, srv, "=work:2", 1)

	if err := tmux.NewClient(srv.Args()).SelectPane(context.Background(), "_web-a", target); err != nil {
		t.Fatal(err)
	}

	if got := currentWindow(t, srv, "_web-a"); got != "2" {
		t.Errorf("_web-a is on window %q, want 2 -- the click did not move the tab that made it", got)
	}
	if got := currentWindow(t, srv, "_web-b"); got != "0" {
		t.Errorf("_web-b is on window %q, want 0 -- the click moved another tab", got)
	}
	if got := currentWindow(t, srv, "work"); got != "0" {
		t.Errorf("work is on window %q, want 0 -- the click moved the user's own session", got)
	}
	if got := activePane(t, srv, "=work:2"); got != target {
		t.Errorf("active pane in the window is %q, want %q -- select-window ran but select-pane did not", got, target)
	}
}

// tmux resolves an empty or absent target to "whatever is current" and exits 0,
// so a pane id the frontend failed to fill in would silently navigate the tab
// to an arbitrary pane. It has to be an error instead.
func TestSelectPaneRejectsBadPaneIDs(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "work", "-x", "80", "-y", "24")
	srv.Run(t, "new-window", "-t", "work")
	srv.Run(t, "new-session", "-d", "-t", "work", "-s", "_web-a")
	srv.Run(t, "select-window", "-t", "=_web-a:0")

	c := tmux.NewClient(srv.Args())
	for _, bad := range []string{"", "%", "0", "work:1", "%999"} {
		if err := c.SelectPane(context.Background(), "_web-a", bad); err == nil {
			t.Errorf("SelectPane(%q) = nil, want an error", bad)
		}
		if got := currentWindow(t, srv, "_web-a"); got != "0" {
			t.Fatalf("SelectPane(%q) moved the session to window %q", bad, got)
		}
	}
}

// A session argument that does not name exactly one session is refused. tmux
// resolves both of these without complaint -- an empty target means "whatever
// is current", and a partial name prefix-matches -- so either would silently
// navigate a session the caller never named, possibly the user's own.
func TestSelectPaneRefusesSessionsItWasNotGiven(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "work", "-x", "80", "-y", "24")
	srv.Run(t, "new-window", "-t", "=work")
	srv.Run(t, "new-session", "-d", "-t", "work", "-s", "_web-abcd")
	srv.Run(t, "select-window", "-t", "=_web-abcd:0")
	srv.Run(t, "select-window", "-t", "=work:0")
	target := paneAt(t, srv, "=work:1", 0)

	c := tmux.NewClient(srv.Args())
	for _, bad := range []string{"", "_web-"} {
		if err := c.SelectPane(context.Background(), bad, target); err == nil {
			t.Errorf("SelectPane(session=%q) = nil, want an error", bad)
		}
		for _, s := range []string{"_web-abcd", "work"} {
			if got := currentWindow(t, srv, s); got != "0" {
				t.Fatalf("SelectPane(session=%q) moved %s to window %q", bad, s, got)
			}
		}
	}
}

func TestKillSession(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "keepalive")
	srv.Run(t, "new-session", "-d", "-s", "_web-abcd")

	c := tmux.NewClient(srv.Args())
	if err := c.KillSession(context.Background(), "_web-abcd"); err != nil {
		t.Fatal(err)
	}
	if out := srv.Run(t, "list-sessions", "-F", "#{session_name}"); strings.Contains(out, "_web-abcd") {
		t.Fatalf("KillSession left the session alive: %q", out)
	}
}

// tmux target matching falls back to a prefix and does not report ambiguity, so
// a partial name would kill some other session and exit 0.
func TestKillSessionRefusesAPartialName(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "keepalive")
	srv.Run(t, "new-session", "-d", "-s", "_web-abcd")

	if err := tmux.NewClient(srv.Args()).KillSession(context.Background(), "_web-"); err == nil {
		t.Error("KillSession with a partial name = nil, want an error")
	}
	if out := srv.Run(t, "list-sessions", "-F", "#{session_name}"); !strings.Contains(out, "_web-abcd") {
		t.Fatalf("KillSession with a partial name killed a session it does not name: %q", out)
	}
}

// currentWindow is the index of the window the session is on. Grouped sessions
// share a window list but not a current window, which is the whole point of
// selecting against the tab's own session.
func currentWindow(t *testing.T, srv *testutil.Server, session string) string {
	t.Helper()
	out := srv.Run(t, "list-windows", "-t", "="+session, "-F", "#{?window_active,#{window_index},}")
	return strings.Join(strings.Fields(out), "")
}

// activePane is the pane id of the active pane in a window. It is a property of
// the window, so every session in the group sees the same one.
func activePane(t *testing.T, srv *testutil.Server, window string) string {
	t.Helper()
	out := srv.Run(t, "list-panes", "-t", window, "-F", "#{?pane_active,#{pane_id},}")
	return strings.Join(strings.Fields(out), "")
}

// paneAt is the pane id at a position in a window. Tests address panes by
// position because pane ids are assigned server-wide and are not predictable.
func paneAt(t *testing.T, srv *testutil.Server, window string, index int) string {
	t.Helper()
	out := srv.Run(t, "list-panes", "-t", window, "-F", "#{pane_index}"+tmux.Sep+"#{pane_id}")
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Split(line, tmux.Sep); len(f) == 2 && f[0] == strconv.Itoa(index) {
			return f[1]
		}
	}
	t.Fatalf("no pane %d in %s: %q", index, window, out)
	return ""
}
