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

	e.endMode(t, e.a)

	// The reply itself, typed straight after. It reaches the tab's own pane,
	// which is %b -- proving both that the refusal did not close the socket and
	// that the handler went on to the write it exists to protect.
	wsWrite(t, e.c, ptybridge.EncodeData([]byte("echo reply-still-arrives\r")))
	wsReadUntil(t, e.c, "reply-still-arrives", 10*time.Second)

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
