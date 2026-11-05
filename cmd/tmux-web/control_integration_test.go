package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/diegok/tmux-web/internal/tmux/testutil"
)

// The control verbs against a real tmux server, on a socket of this test's own.
//
// Against testutil's server and never the developer's: these verbs CREATE
// windows and panes, and a test that let $TMUX leak through from the
// environment it runs in would make them in the owner's live session, which
// holds real work and running agents.
//
// Two things about `open` can only be established here, and they are the two
// the verb exists for:
//
//   - IT MUST NOT MOVE THE OWNER'S VIEW. Both tmux commands underneath make the
//     new window or pane current unless they are given -d, and whether that flag
//     was passed is invisible through the controlClient seam the stub tests
//     drive -- the method name is the same either way.
//   - THE PANE MUST HOLD A SHELL. `open` creates an empty shell precisely so
//     that a command typed into it is a command the owner can see, re-run and
//     scroll back through, and that rests on tmux's remain-on-exit default
//     rather than on anything this code does. A stub cannot run a command.

// TestOpenLeavesTheOwnersViewWhereItWas is the -d assertion.
//
// A session's current window is server state, so it moves whether or not a
// client is attached -- which is why this works on a headless test server and
// why dropping the flag would be felt immediately by an owner who is attached.
func TestOpenLeavesTheOwnersViewWhereItWas(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "work", "-x", "80", "-y", "24")
	// A second window, so that "still on window 0" is a fact about where the
	// session was left and not the only window there is.
	srv.Run(t, "new-window", "-d", "-t", "work")
	caller := srv.Run(t, "display-message", "-p", "-t", "work:0", "#{pane_id}")
	if before := srv.Run(t, "display-message", "-p", "-t", "work", "#{window_index}"); before != "0" {
		t.Fatalf("setup: the session's current window is %q, want 0", before)
	}

	var out, errb bytes.Buffer
	if code := runOpen([]string{"--window", "api"}, &out, &errb, paneEnv(srv, caller), dialControl); code != 0 {
		t.Fatalf("open exited %d: %s", code, errb.String())
	}
	pane := strings.TrimSuffix(out.String(), "\n")

	// The window was really created, and the printed id really names a pane in
	// it. Without this half, an `open` that quietly did nothing would pass the
	// assertion below.
	if got := srv.Run(t, "display-message", "-p", "-t", pane, "#{window_name}"); got != "api" {
		t.Fatalf("pane %q is in window %q, want the new window \"api\"", pane, got)
	}
	if got := srv.Run(t, "display-message", "-p", "-t", "work", "#{window_index}"); got != "0" {
		t.Errorf("the session's current window moved from 0 to %s: `open` must pass -d, or every pane an agent opens yanks the owner's terminal off whatever he was reading", got)
	}
}

// TestOpenGivesAPaneAShellThatOutlivesACommand is the measurement that kept
// remain-on-exit out of this design.
//
// A pane whose command exits is destroyed, and a pane holding a SHELL is one
// whose shell outlives the command it ran -- so the output stays on screen, the
// scrollback stays readable, and the owner can type the next thing. Setting
// remain-on-exit would have been the other way to keep the text, and it leaves
// a dead pane that no one can type into. This test is what says the design does
// not need it.
func TestOpenGivesAPaneAShellThatOutlivesACommand(t *testing.T) {
	srv := testutil.NewServer(t)
	// /bin/sh for both panes -- named on the session's own pane, and set as the
	// server's default-shell before the split so the pane this test types into
	// gets it too. testutil's -f /dev/null keeps the developer's tmux.conf out
	// of the server, but nothing keeps a shell's rc files out of a pane, and
	// this test is about tmux's remain-on-exit default rather than about
	// anyone's zshrc.
	srv.Run(t, "new-session", "-d", "-s", "work", "-x", "80", "-y", "24", "/bin/sh")
	srv.Run(t, "set-option", "-g", "default-shell", "/bin/sh")
	caller := srv.Run(t, "display-message", "-p", "-t", "work:0", "#{pane_id}")

	var out, errb bytes.Buffer
	if code := runOpen([]string{"--split"}, &out, &errb, paneEnv(srv, caller), dialControl); code != 0 {
		t.Fatalf("open exited %d: %s", code, errb.String())
	}
	pane := strings.TrimSuffix(out.String(), "\n")

	// `sh -c '...'` and NOT a bare `exit 7`: a bare exit ends the pane's own
	// shell and closes the pane, which measures something else entirely -- a
	// mistake made once already while measuring this.
	//
	// The marker is split across printf's arguments so that finding it in the
	// capture is the command's OUTPUT and not tmux's echo of the line that was
	// typed into the pane.
	srv.Run(t, "send-keys", "-t", pane, `sh -c 'printf "OUT%s\n" LIVED; exit 7'`, "Enter")
	waitForScreen(t, srv, pane, "OUTLIVED")

	if dead := srv.Run(t, "display-message", "-p", "-t", pane, "#{pane_dead}"); dead != "0" {
		t.Fatalf("pane_dead = %q after a command exited 7; the pane holds a shell, and the shell is what survives the command", dead)
	}
	// And it is still a shell afterwards, not just a pane that happens to
	// exist: a second command runs in it.
	srv.Run(t, "send-keys", "-t", pane, `sh -c 'printf "SEC%s\n" OND'`, "Enter")
	screen := waitForScreen(t, srv, pane, "SECOND")
	if !strings.Contains(screen, "OUTLIVED") {
		t.Errorf("the first command's output is gone from the screen:\n%s", screen)
	}
}

// waitForScreen blocks until a pane's visible screen contains want, and returns
// that screen.
//
// Polled rather than slept: a pane's shell has to start, read the keys tmux
// wrote into the pty and run the command, and every one of those is a few
// milliseconds that varies with load. A sleep long enough to be safe would cost
// seconds per assertion, and a shorter one would fail on a loaded machine.
func waitForScreen(t *testing.T, srv *testutil.Server, paneID, want string) string {
	t.Helper()
	var screen string
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		screen, _ = srv.TryRun("capture-pane", "-p", "-t", paneID)
		if strings.Contains(screen, want) {
			return screen
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("pane %s never showed %q. Its screen:\n%s", paneID, want, screen)
	return ""
}
