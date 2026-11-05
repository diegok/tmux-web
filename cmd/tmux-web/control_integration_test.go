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
//     scroll back through, and that rests on the pane's own process being a
//     SHELL that outlives the command it ran -- which is what makes
//     remain-on-exit irrelevant here rather than what depends on it. A stub
//     cannot run a command.

// TestOpenLeavesTheOwnersViewWhereItWas is the -d assertion, for BOTH modes.
//
// A session's current window and a window's active pane are both server state,
// so they move whether or not a client is attached -- which is why this works
// on a headless test server and why dropping the flag would be felt
// immediately by an owner who is attached.
//
// Both modes, because the flag is passed twice and nothing else pins either
// argv: internal/tmux has no test naming the Detached verbs, and the stub seam
// cannot see a flag at all. A test for the window mode alone leaves a
// SplitPaneDetached that quietly stopped passing -d green everywhere -- and
// --split is the mode in the `$(tmux-web open --split)` contract and the one
// `run` is built on.
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

	// The window mode: the session's current window must not move.
	var out, errb bytes.Buffer
	if code := runOpen([]string{"--window", "api"}, &out, &errb, paneEnv(srv, caller), dialControl); code != 0 {
		t.Fatalf("open --window exited %d: %s", code, errb.String())
	}
	pane := strings.TrimSuffix(out.String(), "\n")

	// The window was really created, and the printed id really names a pane in
	// it. Without this half, an `open` that quietly did nothing would pass the
	// assertion below.
	if got := srv.Run(t, "display-message", "-p", "-t", pane, "#{window_name}"); got != "api" {
		t.Fatalf("pane %q is in window %q, want the new window \"api\"", pane, got)
	}
	if got := srv.Run(t, "display-message", "-p", "-t", "work", "#{window_index}"); got != "0" {
		t.Errorf("the session's current window moved from 0 to %s: `open --window` must pass -d, or every window an agent opens yanks the owner's terminal off whatever he was reading", got)
	}

	// The split mode: the ACTIVE PANE must not move either. Same flag, a
	// different tmux command, and this is the finer-grained half of the
	// assertion -- a split cannot change which window is current, so the window
	// index above would stay 0 for a split that stole the owner's cursor.
	//
	// The caller's pane is the session's active one, which is both what a real
	// caller's pane usually is and what makes the assertion mean anything:
	// splitting a pane that was not active could not take the cursor from it.
	if active := srv.Run(t, "display-message", "-p", "-t", "work", "#{pane_id}"); active != caller {
		t.Fatalf("setup: the session's active pane is %q, want the caller %q", active, caller)
	}
	out.Reset()
	errb.Reset()
	if code := runOpen([]string{"--split"}, &out, &errb, paneEnv(srv, caller), dialControl); code != 0 {
		t.Fatalf("open --split exited %d: %s", code, errb.String())
	}
	split := strings.TrimSuffix(out.String(), "\n")

	// The pane was really created, and in the caller's own window -- the same
	// did-anything-happen half the window mode gets above.
	if got := srv.Run(t, "display-message", "-p", "-t", split, "#{window_index}"); got != "0" {
		t.Fatalf("the new pane %q is in window %q, want the caller's window 0", split, got)
	}
	if got := srv.Run(t, "display-message", "-p", "-t", "work", "#{pane_id}"); got != caller {
		t.Errorf("the session's active pane moved from the caller %s to %s: `open --split` must pass -d, or a pane opened beside the owner's work steals his cursor into it", caller, got)
	}
}

// TestOpenGivesAPaneAShellThatOutlivesACommand is the measurement that kept
// remain-on-exit out of this design.
//
// WHAT IT RESTS ON IS THE SHELL, not remain-on-exit's default. tmux destroys a
// pane when the pane's own PROCESS exits, and here that process is a shell that
// outlives every command typed into it -- so the option never comes up: the
// output stays on screen, the scrollback stays readable, and the owner can type
// the next thing. remain-on-exit would have been the other way to keep the
// text, for a design where the command WAS the pane's process, and it leaves a
// dead pane nobody can type into. This test is what says this design needs
// neither.
func TestOpenGivesAPaneAShellThatOutlivesACommand(t *testing.T) {
	srv := testutil.NewServer(t)
	// /bin/sh for both panes -- named on the session's own pane, and set as the
	// server's default-shell before the split so the pane this test types into
	// gets it too. testutil's -f /dev/null keeps the developer's tmux.conf out
	// of the server, but nothing keeps a shell's rc files out of a pane, and
	// this test is about what a pane's own process is rather than about
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

// TestTypeRunsACommandInARealShell is the end-to-end proof that `open` and
// `type` together are what a person does: a pane appears, a command is typed
// into it, and its output is on the pane's screen.
//
// It can only be established here. The stub seam records the argv and nothing
// more, so every way of getting this wrong -- "Enter" typed as five characters
// rather than looked up as a key, a "\n" in the text where tmux wants 0x0d, a
// -l that lost its text to tmux's own parser -- records an argv that looks
// plausible and types nothing a shell would run. Task 9's `run` is this verb
// after `open`, so it rests on this being true.
func TestTypeRunsACommandInARealShell(t *testing.T) {
	srv := testutil.NewServer(t)
	// /bin/sh on both panes, for the reason the test above names: testutil
	// keeps the developer's tmux.conf out of the server, and nothing keeps a
	// shell's rc files out of a pane.
	srv.Run(t, "new-session", "-d", "-s", "work", "-x", "80", "-y", "24", "/bin/sh")
	srv.Run(t, "set-option", "-g", "default-shell", "/bin/sh")
	caller := srv.Run(t, "display-message", "-p", "-t", "work:0", "#{pane_id}")

	var out, errb bytes.Buffer
	if code := runOpen([]string{"--split"}, &out, &errb, paneEnv(srv, caller), dialControl); code != 0 {
		t.Fatalf("open exited %d: %s", code, errb.String())
	}
	pane := strings.TrimSuffix(out.String(), "\n")

	// The marker is split across printf's arguments so that finding it in the
	// capture is the command's OUTPUT and not tmux's echo of the typed line --
	// which is the one way this test could pass with nothing having run.
	out.Reset()
	errb.Reset()
	code := runType([]string{"--text", `printf "TYPED%s\n" THIS`, "--enter", pane}, &out, &errb, paneEnv(srv, caller), dialControl)
	if code != 0 {
		t.Fatalf("type exited %d: %s", code, errb.String())
	}
	if out.String() != "" || errb.String() != "" {
		t.Errorf("stdout %q, stderr %q; a submitted single line says nothing", out.String(), errb.String())
	}
	waitForScreen(t, srv, pane, "TYPEDTHIS")

	// The caller's own pane was not typed into. tmux's default target is
	// whatever pane is current, and the caller is it, so a send that lost its
	// -t would have landed here -- and this is the pane holding the agent.
	if screen := srv.Run(t, "capture-pane", "-p", "-t", caller); strings.Contains(screen, "TYPED") {
		t.Errorf("the caller's own pane shows the text:\n%s", screen)
	}
}

// TestRunStartsSomethingThePanesListingThenShows pins the observation `run`
// rests on: pane_current_command reports the CHILD the shell started, so after
// a run the `panes` listing names the program and not the shell. That is what
// makes panes useful afterwards.
func TestRunStartsSomethingThePanesListingThenShows(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "work", "-x", "80", "-y", "24", "/bin/sh")
	srv.Run(t, "set-option", "-g", "default-shell", "/bin/sh")
	caller := srv.Run(t, "display-message", "-p", "-t", "work:0", "#{pane_id}")

	var out, errb bytes.Buffer
	if code := runRun([]string{"--split", "sleep 30"}, &out, &errb, paneEnv(srv, caller), dialControl); code != 0 {
		t.Fatalf("run exited %d: %s", code, errb.String())
	}
	pane := strings.TrimSuffix(out.String(), "\n")

	var listing string
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(25 * time.Millisecond) {
		out.Reset()
		errb.Reset()
		if code := runPanes(nil, &out, &errb, paneEnv(srv, caller), dialControl); code != 0 {
			t.Fatalf("panes exited %d: %s", code, errb.String())
		}
		listing = out.String()
		if strings.Contains(paneLineFor(t, listing, pane), "sleep") {
			return
		}
	}
	t.Fatalf("panes never showed %s running sleep:\n%s", pane, listing)
}
