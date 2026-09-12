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
	// hidden or swept: only the @tmux_web_owned option marks an app session.
	srv.Run(t, "new-session", "-d", "-s", "_web-notes")

	// Simulate an open browser tab: a grouped, app-marked session.
	srv.Run(t, "new-session", "-d", "-t", "work", "-s", "_web-sim")
	srv.Run(t, "set", "-t", "_web-sim", "@tmux_web_owned", "1")

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
// a mistyped #{@tmux_web_label} or #{pane_title} expands to empty rather than
// erroring, so the field count -- and every unit test -- stays happy.
func TestSnapshotCarriesLiveSessionIdentityAfterRename(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "work3", "-x", "80", "-y", "24")
	// A grouped, app-marked member: what an open browser tab creates, and what
	// puts a session_group on the base session in the first place.
	srv.Run(t, "new-session", "-d", "-t", "work3", "-s", "_web-sim")
	srv.Run(t, "set", "-t", "_web-sim", "@tmux_web_owned", "1")
	srv.Run(t, "rename-session", "-t", "work3", "api")
	srv.Run(t, "set", "-p", "-t", "api:0.0", "@tmux_web_label", "reviewer")

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
		t.Errorf("Label = %q, want reviewer from @tmux_web_label", r.Label)
	}
	// tmux defaults a pane title to the hostname, so a working #{pane_title} is
	// never empty -- which is what makes an empty one evidence of a typo.
	if r.Title == "" {
		t.Error("Title is empty; #{pane_title} did not expand")
	}
}

// The window id, against a real tmux, because no fixture can prove it: a
// mistyped #{window_id} expands to the empty string rather than erroring, so
// the parser's unit tests -- which are fed a hand-written record -- stay green
// against a format string that reports nothing. Only a live server can tell the
// two apart.
//
// It is also the field the window endpoints are built on. ValidateWindowID is
// the gate PATCH/DELETE /api/windows/{id} put in front of tmux, so what the
// snapshot reports has to be something that gate accepts, or the browser can
// see a window it cannot address.
func TestSnapshotCarriesAddressableWindowIDs(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "work", "-x", "80", "-y", "24")
	srv.Run(t, "new-window", "-t", "work", "-n", "api")
	// Two panes in the second window: panes of one window must report one id,
	// which a parser reading the pane id into this field would fail.
	srv.Run(t, "split-window", "-t", "work:api")

	panes, err := tmux.NewClient(srv.Args()).Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(panes) != 3 {
		t.Fatalf("want 3 panes (one window x1, one window x2), got %d: %+v", len(panes), panes)
	}

	byWindow := map[string][]tmux.Row{}
	for _, p := range panes {
		if err := tmux.ValidateWindowID(p.WindowID); err != nil {
			t.Errorf("pane %s reports WindowID %q: %v -- the snapshot must not "+
				"name a window the endpoints reject", p.PaneID, p.WindowID, err)
		}
		byWindow[p.WindowID] = append(byWindow[p.WindowID], p)
	}
	if len(byWindow) != 2 {
		t.Fatalf("want 2 distinct window ids, got %d: %+v", len(byWindow), byWindow)
	}
	for id, rows := range byWindow {
		for _, r := range rows {
			if r.WindowIndex != rows[0].WindowIndex {
				t.Errorf("window %s reported with indices %d and %d", id, rows[0].WindowIndex, r.WindowIndex)
			}
		}
	}

	// The id addresses the window, and it is not the index in disguise: rename
	// the window through its id and tmux must find it.
	var target string
	for _, p := range panes {
		if p.WindowName == "api" {
			target = p.WindowID
		}
	}
	if target == "" {
		t.Fatal("no pane reported the window named api")
	}
	srv.Run(t, "rename-window", "-t", target, "renamed")
	if got := srv.Run(t, "display-message", "-p", "-t", target, "#{window_name}"); got != "renamed" {
		t.Errorf("the window id did not address the window: #{window_name} = %q", got)
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
	srv.Run(t, "set", "-t", "_web-sim", "@tmux_web_owned", "1")

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

	if err := tmux.NewClient(srv.Args()).SelectPane(context.Background(), "_web-a", target, ""); err != nil {
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
		if err := c.SelectPane(context.Background(), "_web-a", bad, ""); err == nil {
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

	window := srv.Run(t, "list-panes", "-t", target, "-F", "#{window_id}")
	c := tmux.NewClient(srv.Args())
	// Both paths: with a window id the whole click is one chained invocation
	// whose first command is the read, and the session only reaches tmux in the
	// second -- so the refusal has to hold there too, not just on the path that
	// reads the window separately.
	for _, hint := range []string{"", window} {
		for _, bad := range []string{"", "_web-"} {
			if err := c.SelectPane(context.Background(), bad, target, hint); err == nil {
				t.Errorf("SelectPane(session=%q, window=%q) = nil, want an error", bad, hint)
			}
			for _, s := range []string{"_web-abcd", "work"} {
				if got := currentWindow(t, srv, s); got != "0" {
					t.Fatalf("SelectPane(session=%q, window=%q) moved %s to window %q", bad, hint, s, got)
				}
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

// CurrentPane answers for the session it is given and not for the group, which
// is the only reason it can be the thing that tells a browser tab which pane it
// is showing.
//
// The fixture is built so that no other answer coincides with the right one.
// Three sessions share the same windows: the user's own "work", parked on
// window 0, and two tabs, parked on windows 1 and 2. Every candidate wrong
// answer is a *different* pane id from the right one -- the user's current
// pane, the other tab's, the first pane of the window rather than its active
// one -- so a reading that mixes up whose current window is whose cannot pass.
//
// This is the route the previous design took and it is what the test would have
// caught: combining `#{window_active}` and `#{pane_active}` from a snapshot row
// answers with `work`'s pane here, because the snapshot the browser gets keeps
// one row per pane and prefers the user's own session's copy of it.
func TestCurrentPaneAnswersForOneSessionOfAGroupAndNotTheOthers(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "work", "-x", "80", "-y", "24")
	srv.Run(t, "new-window", "-t", "=work")
	srv.Run(t, "new-window", "-t", "=work")
	// Window 2 is split so that "the active pane" and "the first pane" are
	// different panes: without this, dropping the -f filter would still pass.
	srv.Run(t, "split-window", "-t", "=work:2")
	srv.Run(t, "new-session", "-d", "-t", "work", "-s", "_web-a")
	srv.Run(t, "new-session", "-d", "-t", "work", "-s", "_web-b")

	srv.Run(t, "select-window", "-t", "=work:0")
	srv.Run(t, "select-window", "-t", "=_web-a:1")
	srv.Run(t, "select-window", "-t", "=_web-b:2")
	want := paneAt(t, srv, "=work:2", 1)
	srv.Run(t, "select-pane", "-t", want)

	c := tmux.NewClient(srv.Args())
	got, err := c.CurrentPane(context.Background(), "_web-b")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("CurrentPane(_web-b) = %q, want %q (window 2's active pane).\n"+
			"work is on %q, _web-a on %q, and window 2's first pane is %q -- "+
			"a tab told any of those is looking at somebody else's pane",
			got, want,
			activePane(t, srv, "=work:0"), activePane(t, srv, "=work:1"),
			paneAt(t, srv, "=work:2", 0))
	}

	// The same call against each of the others, so the test cannot pass by
	// answering "window 2's active pane" for every session it is handed.
	for _, tc := range []struct{ session, window string }{
		{"work", "=work:0"},
		{"_web-a", "=work:1"},
	} {
		want := activePane(t, srv, tc.window)
		got, err := c.CurrentPane(context.Background(), tc.session)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("CurrentPane(%s) = %q, want %q", tc.session, got, want)
		}
	}
}

// A session that does not exist is an error and not an empty string, because
// ptybridge waits that error out while a freshly spawned attach creates its
// session. `display-message -p -t '=nosuch:' '#{pane_id}'` prints an empty line
// and exits 0 -- measured on 3.7b -- which is why this does not use it.
func TestCurrentPaneFailsForASessionThatIsNotThere(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "work", "-x", "80", "-y", "24")

	c := tmux.NewClient(srv.Args())
	got, err := c.CurrentPane(context.Background(), "_web-nope")
	if err == nil {
		t.Fatalf("CurrentPane for a missing session = %q, want an error", got)
	}
	if !tmux.IsMissingSession(err) {
		t.Errorf("IsMissingSession(%v) = false; the attach race would fail instead of waiting", err)
	}

	// An empty session is the shape a frontend bug takes, and tmux reads it as
	// "whatever is current" -- it must never answer about the user's session.
	if got, err := c.CurrentPane(context.Background(), ""); err == nil {
		t.Errorf(`CurrentPane("") = %q, want an error`, got)
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

// Capture returns the VISIBLE screen and nothing above it.
//
// The distinction is the whole reason this method exists rather than a
// `capture-pane -S -8` inline somewhere. A negative -S counts back from the top
// of the visible screen into scrollback, so it would hand the classifier a
// just-answered approval box out of history and the pane would read blocked
// with nothing on screen to answer.
func TestCaptureAgainstRealTmux(t *testing.T) {
	srv := testutil.NewServer(t)
	// Five rows, forty lines: everything but the last handful is scrollback.
	srv.Run(t, "new-session", "-d", "-s", "work", "-x", "40", "-y", "5",
		"sh -c 'for i in $(seq 1 40); do echo line$i; done; exec cat'")

	c := tmux.NewClient(srv.Args())
	pane := srv.Run(t, "list-panes", "-t", "work", "-F", "#{pane_id}")

	var screen string
	waitFor(t, 3*time.Second, func() bool {
		var err error
		screen, err = c.Capture(context.Background(), pane)
		return err == nil && strings.Contains(screen, "line40")
	}, "the pane never printed its last line")

	if strings.Contains(screen, "line1\n") || strings.Contains(screen, "line20") {
		t.Errorf("capture reached into scrollback:\n%s", screen)
	}
	if n := len(strings.Split(screen, "\n")); n > 5 {
		t.Errorf("capture returned %d lines for a 5-row pane:\n%s", n, screen)
	}
}

// -J rejoins a line the pane wrapped, so a question broken across two rows by a
// narrow pane reads as one -- and, less obviously, so that a pane being resized
// does not change the hash of a screen nothing happened on.
func TestCaptureRejoinsWrappedLines(t *testing.T) {
	long := strings.Repeat("a", 45)

	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "work", "-x", "20", "-y", "10",
		"echo "+long+"; exec cat")

	c := tmux.NewClient(srv.Args())
	pane := srv.Run(t, "list-panes", "-t", "work", "-F", "#{pane_id}")

	waitFor(t, 3*time.Second, func() bool {
		screen, err := c.Capture(context.Background(), pane)
		return err == nil && strings.Contains(screen, long)
	}, "a 45-character line never came back whole from a 20-column pane")
}
