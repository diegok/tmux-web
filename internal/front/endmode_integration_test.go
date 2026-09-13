package front_test

import (
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/diegok/tmux-web/internal/ptybridge"
)

// --- end-mode ---------------------------------------------------------------
//
// The reply box types into a pane through the PTY, and a pane in copy mode does
// not swallow that reply -- it truncates it. Measured on 3.7b through a real
// client PTY: with the pane in copy mode, typing `echo quit PARTIAL` and Enter
// cancelled copy mode on the `q` and delivered `uit PARTIAL` to the shell,
// which ran it. Prose contains `q` and Escape and every other cancel key,
// usually early, so popping the copy layer first is what makes a reply either
// arrive whole or not arrive at all.
//
// Every fixture in this file is a window with TWO panes, the second one active
// and every assertion aimed at the first. That is not decoration. On a
// single-pane window tmux's default pane *is* the target, so an expression that
// resolves against the wrong pane -- or against no pane at all -- passes every
// assertion here while doing nothing the feature needs. The arrangement itself
// is asserted before anything else for the same reason: without that, the whole
// file rests on a split-window side effect nobody checked.

// endModeFixture is a wsFixture whose "work" window has been split, with the
// tab attached and its active pane pinned to the one nothing is aimed at.
type endModeFixture struct {
	*wsFixture
	c    *websocket.Conn
	a, b string // a: the target, inactive. b: active, and never a target.
}

func newEndModeFixture(t *testing.T) *endModeFixture {
	t.Helper()
	f := defaultWSFixture(t)
	f.srv.Run(t, "split-window", "-d", "-t", "=work:0")
	panes := strings.Split(f.srv.Run(t, "list-panes", "-t", "=work:0", "-F", "#{pane_id}"), "\n")
	if len(panes) != 2 {
		t.Fatalf("want a split window, got panes %q", panes)
	}
	// Both panes run a known shell rather than whatever tmux would have started.
	// That is $SHELL -- the developer's login shell, rc files and all -- and the
	// no-mode case below reads a command's OUTPUT back out of a pane to prove the
	// pane's program received the reply. A login shell that prints a banner,
	// takes seconds to reach a prompt, or (as one has been observed to do under
	// a sandbox) spins at full tilt and never reaches one at all, would decide
	// what that case measures on a machine nobody can look at from here.
	// /bin/sh reads no rc file when it is interactive.
	for _, pane := range panes {
		f.srv.Run(t, "respawn-pane", "-k", "-t", pane, "/bin/sh")
	}

	// The active pane belongs to the window, and a grouped session shares its
	// windows, so this is the pane the tab lands on too -- which is exactly the
	// pane a default-target expression would reach for.
	f.srv.Run(t, "select-pane", "-t", panes[1])

	e := &endModeFixture{wsFixture: f, a: panes[0], b: panes[1]}
	e.c = f.dial(t, wsCanonical, "?session=work")
	f.tabSession(t)

	// Asserted after the attach, because the attach is what could move it.
	// Reversed silently -- %a active instead of %b -- every case below would
	// still pass against an expression aimed at tmux's default pane, so this is
	// the assertion the rest of the file depends on.
	if got := f.srv.Run(t, "display-message", "-p", "-t", e.a, "#{pane_active}"); got != "0" {
		t.Fatalf("the target pane %s is active (%s): every assertion here would be "+
			"satisfied by an expression that hit the default pane instead", e.a, got)
	}
	if got := f.srv.Run(t, "display-message", "-p", "-t", e.b, "#{pane_active}"); got != "1" {
		t.Fatalf("the other pane %s is not active (%s): the fixture never took", e.b, got)
	}
	return e
}

// mode reads a pane's mode stack back. pane_mode names only the top layer and
// pane_in_mode is a COUNT of layers, so both are needed to tell "out of copy
// mode, still in the tree" from "out of everything".
func (e *endModeFixture) mode(t *testing.T, pane string) (string, string) {
	t.Helper()
	out := e.srv.Run(t, "display-message", "-p", "-t", pane, "#{pane_mode} #{pane_in_mode}")
	name, count, _ := strings.Cut(out, " ")
	return name, count
}

func (e *endModeFixture) wantMode(t *testing.T, when, pane, mode, count string) {
	t.Helper()
	gotMode, gotCount := e.mode(t, pane)
	if gotMode != mode || gotCount != count {
		t.Fatalf("%s: pane %s is %q/%s, want %q/%s", when, pane, gotMode, gotCount, mode, count)
	}
}

func (e *endModeFixture) endMode(t *testing.T, pane string) {
	t.Helper()
	msg := `{"type":"end-mode"}`
	if pane != "" {
		msg = `{"type":"end-mode","pane":"` + pane + `"}`
	}
	wsWrite(t, e.c, ptybridge.EncodeControl([]byte(msg)))
}

// echo types `echo <split>` into the tab's own pane -- %b, which is where the
// keystrokes of a reply land -- and waits for joined to come back out of the
// socket.
//
// split and joined are the same marker written two ways, and the difference is
// what makes this an assertion about the pane's program rather than about the
// terminal. A pty echoes what is typed at it whether or not anything is alive
// to read it: waiting for the characters as they were sent is satisfied by a
// pane running `sleep`, or by no program at all. Splitting the marker with a
// pair of empty quotes means the input line reads `echo REA""DY` while READY
// can only be a line the shell itself printed, so the wait cannot be satisfied
// until the shell has read the line and run it.
//
// The guard is there because "simplifying" the marker back into one piece is
// the natural edit, and it would quietly restore the weaker assertion.
func (e *endModeFixture) echo(t *testing.T, split, joined string) {
	t.Helper()
	if strings.Contains(split, joined) {
		t.Fatalf("marker %q still contains %q, so the tty echo of the input line "+
			"satisfies this wait on its own and the shell need not have run", split, joined)
	}
	wsWrite(t, e.c, ptybridge.EncodeData([]byte("echo "+split+"\r")))
	wsReadUntil(t, e.c, joined, 15*time.Second)
}

// barrier makes every assertion in this file a plain read rather than a poll.
// One loop reads control messages in the order they arrive and each runs its
// tmux command to completion before the next is decoded, so an answer to a
// "where" sent afterwards proves the end-mode before it has already been
// applied -- including the cases whose whole point is that nothing changed,
// which no amount of waiting could establish on its own.
func (e *endModeFixture) barrier(t *testing.T) {
	t.Helper()
	wsWrite(t, e.c, ptybridge.EncodeControl([]byte(`{"type":"where"}`)))
	wsReadPane(t, e.c, 15*time.Second)
}

// The case the feature exists for.
func TestEndModePopsTheCopyLayerOfTheTargetPane(t *testing.T) {
	e := newEndModeFixture(t)
	e.srv.Run(t, "copy-mode", "-t", e.a)
	e.wantMode(t, "before", e.a, "copy-mode", "1")

	e.endMode(t, e.a)
	e.barrier(t)

	e.wantMode(t, "after", e.a, "", "0")
	// The instrument must not have typed anything either: `send-keys` without
	// -X delivers the literal word, which a mode assertion alone cannot see.
	if out := e.srv.Run(t, "capture-pane", "-p", "-t", e.a); strings.Contains(out, "cancel") {
		t.Errorf("the word \"cancel\" was typed into %s rather than dispatched to it:\n%s", e.a, out)
	}
}

// A pane in choose-tree is not this feature's business. `copy-mode -q` would
// close it, and so would a `#{pane_mode}` guard that expanded against the wrong
// pane -- and a phone typing into a text box closing the owner's session tree
// is the failure the guard existed to prevent.
//
// The other pane is put into copy mode on purpose: it is the pane a default
// target resolves to, so an expression that reads its mode instead of the
// target's finds "copy-mode" and fires.
func TestEndModeLeavesAPaneThatIsOnlyInTreeModeAlone(t *testing.T) {
	e := newEndModeFixture(t)
	e.srv.Run(t, "choose-tree", "-t", e.a)
	e.srv.Run(t, "copy-mode", "-t", e.b)
	e.wantMode(t, "before", e.a, "tree-mode", "1")
	e.wantMode(t, "before", e.b, "copy-mode", "1")

	e.endMode(t, e.a)
	e.barrier(t)

	e.wantMode(t, "after", e.a, "tree-mode", "1")
}

// Modes stack. Scrolling up inside a choose-tree is an ordinary thing to do,
// and it leaves pane_in_mode at 2 with pane_mode naming only the copy layer --
// so a single `copy-mode -q` pops both and the tree is gone.
func TestEndModePopsOnlyTheCopyLayerOffAStack(t *testing.T) {
	e := newEndModeFixture(t)
	e.srv.Run(t, "choose-tree", "-t", e.a)
	e.srv.Run(t, "copy-mode", "-t", e.a)
	e.wantMode(t, "before", e.a, "copy-mode", "2")

	e.endMode(t, e.a)
	e.barrier(t)

	e.wantMode(t, "after", e.a, "tree-mode", "1")
}

// The common case, and the one that would break every ordinary reply.
//
// `send-keys -X` dispatches into the copy-mode command table, so a pane in no
// mode -- where most replies go -- has nowhere to deliver it and refuses with
// "not in a mode", exit 1. That status is swallowed: the socket survives it and
// the keystrokes that follow still land.
func TestEndModeOnAPaneInNoModeIsNotAnError(t *testing.T) {
	e := newEndModeFixture(t)
	e.wantMode(t, "before", e.a, "", "0")

	// The shell in the tab's own pane is made to run something before the
	// refusal, which is the only signal that says "reading and executing"
	// rather than "the process exists". Without it a reply that arrived before
	// the shell did would be indistinguishable from one that never arrived.
	e.echo(t, `REA""DY`, "READY")

	e.endMode(t, e.a)

	// The reply itself, typed straight after. Its output comes back through the
	// same socket the refusal was sent on, which proves three things at once:
	// the refusal did not close the socket, the handler went on to the write it
	// exists to protect, and the shell on the far end read that write and ran
	// it. The last of those is why the marker is split -- see echo.
	e.echo(t, `reply-still-arri""ves`, "reply-still-arrives")

	e.wantMode(t, "after", e.a, "", "0")
	// A `send-keys` missing its -X would have typed the word instead, and on a
	// pane in no mode that reaches the shell.
	if out := e.srv.Run(t, "capture-pane", "-p", "-t", e.a); strings.Contains(out, "cancel") {
		t.Errorf("the word \"cancel\" was typed into %s:\n%s", e.a, out)
	}
}

// `tmux send-keys -X -t work cancel` exits 0 against the pane the *owner* is
// sitting in front of. A frontend bug that sent a session name where a pane id
// belongs would reach across into their terminal from a machine nobody is at,
// so the target is validated before tmux ever sees it.
func TestEndModeRejectsATargetThatIsNotAPaneID(t *testing.T) {
	e := newEndModeFixture(t)
	// "work" resolves to that session's current pane, which is %b.
	e.srv.Run(t, "copy-mode", "-t", e.b)
	e.wantMode(t, "before", e.b, "copy-mode", "1")

	e.endMode(t, "work")
	e.barrier(t)

	e.wantMode(t, "after", e.b, "copy-mode", "1")
}

// Unlike copy-mode, there is no session default here. Entering a mode on the
// tab's own pane is a thing the user asked for; leaving one on an unnamed pane
// is a thing the reply box would do by accident.
func TestEndModeWithNoPaneIsRefusedRatherThanAimedAtTheTabsOwnPane(t *testing.T) {
	e := newEndModeFixture(t)
	e.srv.Run(t, "copy-mode", "-t", e.b)
	e.wantMode(t, "before", e.b, "copy-mode", "1")

	e.endMode(t, "")
	e.barrier(t)

	e.wantMode(t, "after", e.b, "copy-mode", "1")
	e.wantMode(t, "after", e.a, "", "0")
}
