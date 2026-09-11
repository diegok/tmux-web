package ptybridge_test

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/diegok/tmux-web/internal/ptybridge"
	"github.com/diegok/tmux-web/internal/tmux"
	"github.com/diegok/tmux-web/internal/tmux/testutil"
)

func TestSessionEchoesTypedInput(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "work", "-x", "80", "-y", "24")

	s := open(t, srv, ptybridge.Config{TmuxArgs: srv.Args(), Base: "work", Cols: 80, Rows: 24})

	if _, err := s.Write([]byte("echo hello-bridge\r")); err != nil {
		t.Fatal(err)
	}

	var buf strings.Builder
	deadline := time.After(5 * time.Second)
	for {
		select {
		case b, ok := <-s.Output():
			if !ok {
				t.Fatalf("output closed early; got %q", buf.String())
			}
			buf.Write(b)
			if strings.Contains(buf.String(), "hello-bridge") {
				return
			}
		case <-deadline:
			t.Fatalf("never saw echoed output; got %q", buf.String())
		}
	}
}

func TestSessionKillsItsTmuxSessionOnClose(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "work", "-x", "80", "-y", "24")

	s := open(t, srv, ptybridge.Config{TmuxArgs: srv.Args(), Base: "work", Cols: 80, Rows: 24})
	name := s.SessionName()
	s.Close()

	waitFor(t, 3*time.Second, func() bool {
		return !strings.Contains(sessions(t, srv), name)
	}, "throwaway session outlived the bridge")
}

// Close must kill the session itself, not merely leave it unattached for
// destroy-unattached to collect. With the crash net turned off for this session
// -- the same state a session reaches if the option was never applied, e.g. a
// tmux that rejected it -- the explicit kill is the only thing that reaps it.
func TestCloseKillsTheSessionDestroyUnattachedWouldNotCollect(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "work", "-x", "80", "-y", "24")

	s := open(t, srv, ptybridge.Config{TmuxArgs: srv.Args(), Base: "work", Cols: 80, Rows: 24})
	name := s.SessionName()
	// "=name:" is a window target: set-option resolves a bare -t as a pane
	// target, and `-t =name` fails outright with "no such session".
	srv.Run(t, "set-option", "-t", "="+name+":", "destroy-unattached", "off")

	s.Close()

	waitFor(t, 3*time.Second, func() bool {
		return !strings.Contains(sessions(t, srv), name)
	}, "session survived Close: it was only detached, never killed")
}

// A reader parked on Output() is the websocket handler's write loop. It must
// learn that the session ended by seeing the channel close, not by hanging.
func TestCloseEndsTheOutputStream(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "work", "-x", "80", "-y", "24")

	s := open(t, srv, ptybridge.Config{TmuxArgs: srv.Args(), Base: "work", Cols: 80, Rows: 24})

	closed := make(chan struct{})
	go func() {
		defer close(closed)
		for range s.Output() {
		}
	}()

	s.Close()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Output() never closed after Close; a reader would hang forever")
	}
}

// A browser tab that stops reading must cost the tab, not the tmux server.
// Blocking the pump would stall a client sharing the server with the user's own
// session, and dropping bytes would corrupt the terminal, so the session dies.
func TestSlowClientClosesTheSessionInsteadOfBlocking(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "work", "-x", "80", "-y", "24")

	// Deliberately never read Output(): this is the wedged tab.
	s := open(t, srv, ptybridge.Config{TmuxArgs: srv.Args(), Base: "work", Cols: 80, Rows: 24})
	name := s.SessionName()

	// A session target needs the trailing ":" to be read as one: tmux
	// resolves a bare -t as a pane target, and "=work" is not a pane.
	srv.Run(t, "send-keys", "-t", "=work:", "seq 1 2000000", "Enter")

	waitFor(t, 20*time.Second, func() bool {
		return !strings.Contains(sessions(t, srv), name)
	}, "flooded session was never closed: the pump blocked or dropped bytes")

	// The overflow path goes through Close, so the stream ends too.
	deadline := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-s.Output():
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("Output() never closed after the overflow")
		}
	}
}

func TestConfiguredSizeAndResizeReachTheTmuxClient(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "work", "-x", "80", "-y", "24")

	s := open(t, srv, ptybridge.Config{TmuxArgs: srv.Args(), Base: "work", Cols: 90, Rows: 31})
	name := s.SessionName()

	if got := client(t, srv, name, "#{client_width}x#{client_height}"); got != "90x31" {
		t.Fatalf("client size = %s, want 90x31", got)
	}

	if err := s.Resize(100, 37); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool {
		return client(t, srv, name, "#{client_width}x#{client_height}") == "100x37"
	}, "Resize never reached the tmux client")
}

// wterm emulates xterm-256color, so the attach must run with that TERM
// regardless of the daemon's own environment. (tmux advertises tmux-256color
// to programs inside the session; that is separate.)
func TestAttachRunsWithTheTerminalWtermEmulates(t *testing.T) {
	t.Setenv("TERM", "screen")
	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "work", "-x", "80", "-y", "24")

	s := open(t, srv, ptybridge.Config{TmuxArgs: srv.Args(), Base: "work", Cols: 80, Rows: 24})

	if got := client(t, srv, s.SessionName(), "#{client_termname}"); got != tmux.TermName {
		t.Fatalf("client TERM = %s, want %s", got, tmux.TermName)
	}

	// The TERM is not cosmetic: it is what scopes tmux's hyperlink support to
	// this client. If the embedded terminfo entry were missing, tmux would
	// refuse to attach at all rather than silently drop the feature.
	if got := client(t, srv, s.SessionName(), "#{client_termfeatures}"); !strings.Contains(got, "hyperlinks") {
		t.Fatalf("client features = %q, want hyperlinks: OSC 8 links will be stripped", got)
	}
}

// A local terminal sharing the server must not start receiving OSC 8 because
// the browser asked for it. This is the whole reason for a separate TERM.
func TestEnablingHyperlinksDoesNotAffectOtherClients(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "work", "-x", "80", "-y", "24")

	s := open(t, srv, ptybridge.Config{TmuxArgs: srv.Args(), Base: "work", Cols: 80, Rows: 24})
	_ = s

	out := srv.Run(t, "show", "-s", "terminal-features")
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "hyperlinks") && !strings.Contains(line, tmux.TermName) {
			t.Fatalf("hyperlinks enabled beyond the web client: %q", line)
		}
	}
}

// Clicking a pane in one tab must move that tab only -- not the other tabs, and
// not the terminal the user is sitting in front of.
func TestSelectPaneMovesOnlyThisTabsSession(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "work", "-x", "80", "-y", "24")
	srv.Run(t, "new-window", "-t", "=work", "-d")
	panes := strings.Split(srv.Run(t, "list-panes", "-s", "-t", "=work", "-F", "#{pane_id}"), "\n")
	if len(panes) != 2 {
		t.Fatalf("want two panes to choose between, got %q", panes)
	}
	other := panes[1]

	s := open(t, srv, ptybridge.Config{TmuxArgs: srv.Args(), Base: "work", Cols: 80, Rows: 24})
	// The current window is per-session even inside a group, and
	// display-message reports it -- unlike list-windows, whose window_active
	// answers for the window's own session. A target it cannot resolve makes
	// it print an empty line and exit 0, hence the trailing ":".
	before := currentWindow(t, srv, "work")

	if err := s.SelectPane(context.Background(), other); err != nil {
		t.Fatal(err)
	}

	moved := currentWindow(t, srv, s.SessionName())
	if moved == before {
		t.Fatalf("tab's session did not move: still on window %s", moved)
	}
	if now := currentWindow(t, srv, "work"); now != before {
		t.Fatalf("the user's own session moved from window %s to %s", before, now)
	}
}

// A daemon serving tabs for weeks must not accumulate one zombie and one open
// PTY per closed tab, so Close reaps the child it killed and releases the
// master. Both survive the tmux session's death otherwise: killing the client
// or the session ends the stream, and would leave these behind.
func TestCloseReleasesTheClientProcessAndItsPTY(t *testing.T) {
	// Only Linux publishes process state this way. The daemon targets Linux;
	// the bridge itself does not.
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("no /proc to read process state from")
	}
	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "work", "-x", "80", "-y", "24")

	before := openPTYs(t)
	s := open(t, srv, ptybridge.Config{TmuxArgs: srv.Args(), Base: "work", Cols: 80, Rows: 24})
	if got := openPTYs(t); got != before+1 {
		t.Fatalf("open PTY masters = %d, want %d: the harness is not measuring what it thinks", got, before+1)
	}

	s.Close()

	waitFor(t, 5*time.Second, func() bool {
		return len(zombieChildren(t)) == 0
	}, "Close left the killed tmux client unreaped")
	if got := openPTYs(t); got != before {
		t.Fatalf("open PTY masters = %d after Close, want %d: Close leaked the PTY", got, before)
	}
}

// Close arrives from two directions -- the websocket handler when the socket
// goes away, and the pump on overflow -- and they can land together.
func TestCloseIsSafeFromConcurrentCallers(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "work", "-x", "80", "-y", "24")

	s := open(t, srv, ptybridge.Config{TmuxArgs: srv.Args(), Base: "work", Cols: 80, Rows: 24})
	name := s.SessionName()

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); s.Close() }()
	}
	wg.Wait()

	for range s.Output() { // drains, then ends when the pump closes it
	}
	waitFor(t, 3*time.Second, func() bool {
		return !strings.Contains(sessions(t, srv), name)
	}, "session outlived a concurrent Close")
}

// open starts a bridge session and waits until tmux reports its client, so that
// tests assert on a session that exists rather than racing its startup. Cleanup
// is registered rather than deferred by the caller: a surviving `tmux attach`
// holds its session open and defeats destroy-unattached, so it must be
// collected even when the test fails early.
func open(t *testing.T, srv *testutil.Server, cfg ptybridge.Config) *ptybridge.Session {
	t.Helper()
	s, err := ptybridge.Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	waitFor(t, 5*time.Second, func() bool {
		out, err := srv.TryRun("list-sessions", "-F", "#{session_name} #{session_attached}")
		return err == nil && strings.Contains(out, s.SessionName()+" 1")
	}, "bridge session never came up attached")
	return s
}

// currentWindow is the window one session is looking at.
func currentWindow(t *testing.T, srv *testutil.Server, session string) string {
	t.Helper()
	out := srv.Run(t, "display-message", "-p", "-t", "="+session+":", "#{window_id}")
	if out == "" {
		t.Fatalf("no current window for session %s", session)
	}
	return out
}

// client reports a formatted field of the one client attached to name.
func client(t *testing.T, srv *testutil.Server, name, format string) string {
	t.Helper()
	return srv.Run(t, "list-clients", "-t", "="+name, "-F", format)
}

func sessions(t *testing.T, srv *testutil.Server) string {
	t.Helper()
	// The unattached "work" session keeps the server alive, so a failure here
	// is never transient: polling through it would report the wrong cause.
	out, err := srv.TryRun("list-sessions", "-F", "#{session_name}")
	if err != nil {
		t.Fatalf("list-sessions: %v", err)
	}
	return out
}

// zombieChildren returns the pids of this test binary's unreaped children. Every
// other subprocess a test starts goes through exec.Cmd.Run, which waits, so
// anything listed here was left behind by the bridge.
func zombieChildren(t *testing.T) []int {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatalf("read /proc: %v", err)
	}
	var zombies []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if state, ppid, ok := procStat(pid); ok && state == "Z" && ppid == os.Getpid() {
			zombies = append(zombies, pid)
		}
	}
	return zombies
}

// openPTYs counts this process's open PTY masters, one of which is held per
// live bridge session.
func openPTYs(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatalf("read /proc/self/fd: %v", err)
	}
	n := 0
	for _, e := range entries {
		if target, err := os.Readlink("/proc/self/fd/" + e.Name()); err == nil && target == "/dev/ptmx" {
			n++
		}
	}
	return n
}

// procStat reports a process's state and parent pid, or false if it exited
// between the listing and the read.
func procStat(pid int) (state string, ppid int, ok bool) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", 0, false
	}
	// The comm field is parenthesized and may contain spaces, so the fields
	// after it are counted from the last ')': state, then ppid.
	f := strings.Fields(string(b[strings.LastIndexByte(string(b), ')')+1:]))
	if len(f) < 2 {
		return "", 0, false
	}
	parent, err := strconv.Atoi(f[1])
	if err != nil {
		return "", 0, false
	}
	return f[0], parent, true
}

func waitFor(t *testing.T, d time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal(msg)
}

// The owner's actual shape: a wide local terminal and a narrower browser tab on
// the same grouped window. The tab goes full-screen and comes back, and the
// window has to follow it *down* as well as up.
//
// TestConfiguredSizeAndResizeReachTheTmuxClient only ever grows (90x31 ->
// 100x37) and only ever looks at #{client_width}, so it passes against a Resize
// that refuses to shrink and against one whose ioctl never reaches the window
// at all. Both halves are pinned here instead: the *window* size, which is what
// the pane is actually drawn at, and a shrink made against a larger client that
// is still attached.
//
// This also settles who owns "grows on full-screen, does not shrink coming
// back". tmux's `window-size latest` counts a SIGWINCH as using a client, so a
// resize alone -- with no keystroke -- makes this tab the latest client and the
// shared window follows it in both directions. If that symptom survives, it is
// not the daemon.
func TestResizeShrinksTheSharedWindowEvenWithALargerClientAttached(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "work", "-x", "200", "-y", "60")

	// Stands in for the terminal the owner is sitting at: same group, same
	// window, and wider than the tab in both axes for the whole test.
	local := open(t, srv, ptybridge.Config{TmuxArgs: srv.Args(), Base: "work", Cols: 160, Rows: 50})
	defer local.Close()

	tab := open(t, srv, ptybridge.Config{TmuxArgs: srv.Args(), Base: "work", Cols: 100, Rows: 30})
	name := tab.SessionName()

	// Attaching is itself a claim on the size, so the window is the tab's
	// before anything is resized. Without this the grow below could pass on a
	// window that was already large.
	waitFor(t, 3*time.Second, func() bool {
		return windowSize(t, srv, name) == "100x30"
	}, "the window never took the newest client's size")

	if err := tab.Resize(150, 45); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool {
		return windowSize(t, srv, name) == "150x45"
	}, "going full-screen never widened the shared window")

	// The one that matters. A larger client is still attached, so a window that
	// only ever grows stays at 150x45 here.
	if err := tab.Resize(100, 30); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool {
		return windowSize(t, srv, name) == "100x30"
	}, "coming back from full-screen never shrank the shared window: it stayed at "+
		windowSize(t, srv, name)+" with a 160x50 client attached")
}

// windowSize is the size the pane is actually drawn at, which is the window's
// and not the client's: with another client on the same grouped window the two
// differ, and only this one decides what the user sees.
func windowSize(t *testing.T, srv *testutil.Server, session string) string {
	t.Helper()
	return srv.Run(t, "display-message", "-p", "-t", "="+session+":",
		"#{window_width}x#{window_height}")
}
