package front_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/diegok/tmux-web/internal/front"
	"github.com/diegok/tmux-web/internal/ptybridge"
)

// --- the forced poll behind the header's one copy-mode button ----------------
//
// The button's label follows #{pane_mode}, which reaches the browser on a poll
// the daemon takes every 1.5s. A control that renames itself a second and a
// half after it was pressed reads as broken, so the browser flips the label on
// click and waits for the poll to confirm it -- and this is what makes that
// wait short: the same forced poll the management verbs already take (see
// `settle` in manage.go), so the cached snapshot the tab re-reads is already
// the one taken after the mode changed.
//
// It is taken only when the command actually did something. `end-mode` is sent
// before EVERY non-empty reply and refuses on the pane most replies go to --
// "not in a mode", exit 1, nothing changed -- and a forced tmux fork per reply
// would buy a poll that re-reads a mode stack nobody touched.

// modePoller records what the daemon's snapshot would have seen at the moment
// the handler forced a poll.
//
// It records the pane's mode rather than only a count, because the order is the
// whole point: a poll forced BEFORE the tmux command caches the mode the pane
// was in a moment ago, which is exactly the stale answer the browser is trying
// to get ahead of, and a call counter cannot tell the two apart.
type modePoller struct {
	mu    sync.Mutex
	modes []string
	read  func() string
}

func (m *modePoller) PollNow(context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.modes = append(m.modes, m.read())
	return nil
}

func (m *modePoller) seen() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.modes...)
}

// modePollFixture is the two-pane end-mode fixture with that hook installed.
func modePollFixture(t *testing.T) (*endModeFixture, *modePoller) {
	t.Helper()
	p := &modePoller{}
	e := newEndModeFixtureWith(t, front.TerminalConfig{AllowedOrigin: wsCanonical, PollNow: p.PollNow})
	// Read through the fixture's own server, and only once the fixture exists:
	// the hook cannot fire before the socket it belongs to is open.
	p.mu.Lock()
	p.read = func() string {
		out, err := e.srv.TryRun("display-message", "-p", "-t", e.a, "#{pane_mode}")
		if err != nil {
			return "display-message failed: " + err.Error()
		}
		return strings.TrimSuffix(out, "\n")
	}
	p.mu.Unlock()
	return e, p
}

func (e *endModeFixture) copyMode(t *testing.T, pane string) {
	t.Helper()
	msg := `{"type":"copy-mode"}`
	if pane != "" {
		msg = `{"type":"copy-mode","pane":"` + pane + `"}`
	}
	wsWrite(t, e.c, ptybridge.EncodeControl([]byte(msg)))
}

func TestCopyModeForcesAPollThatAlreadySeesTheNewMode(t *testing.T) {
	e, p := modePollFixture(t)
	e.wantMode(t, "before", e.a, "", "0")

	e.copyMode(t, e.a)
	e.barrier(t)

	e.wantMode(t, "after", e.a, "copy-mode", "1")
	if got := p.seen(); len(got) != 1 || got[0] != "copy-mode" {
		t.Fatalf("the forced polls saw %q, want exactly one and the pane already in "+
			"copy mode: a poll taken before the command caches the mode the button "+
			"is trying to get ahead of", got)
	}
}

func TestEndModeForcesAPollThatAlreadySeesTheModeGone(t *testing.T) {
	e, p := modePollFixture(t)
	e.srv.Run(t, "copy-mode", "-t", e.a)
	e.wantMode(t, "before", e.a, "copy-mode", "1")

	e.endMode(t, e.a)
	e.barrier(t)

	e.wantMode(t, "after", e.a, "", "0")
	if got := p.seen(); len(got) != 1 || got[0] != "" {
		t.Fatalf("the forced polls saw %q, want exactly one and the pane already out "+
			"of copy mode", got)
	}
}

// The common case, and the reason the poll is conditional. Every non-empty
// reply sends an end-mode first, and most replies go to a pane in no mode at
// all: tmux refuses with "not in a mode" and changes nothing, so there is
// nothing for a poll to re-read.
func TestARefusedEndModeForcesNoPoll(t *testing.T) {
	e, p := modePollFixture(t)
	e.wantMode(t, "before", e.a, "", "0")

	e.endMode(t, e.a)
	e.barrier(t)

	if got := p.seen(); len(got) != 0 {
		t.Fatalf("a refused end-mode forced %d polls (%q): it changed nothing, and "+
			"this message goes out before every reply", len(got), got)
	}
}

// A pane in a mode this app cannot leave is the same case: cancel refuses, the
// tree stays, and the snapshot the browser already has is still correct.
func TestAnEndModeOnTreeModeForcesNoPoll(t *testing.T) {
	e, p := modePollFixture(t)
	e.srv.Run(t, "choose-tree", "-t", e.a)
	e.wantMode(t, "before", e.a, "tree-mode", "1")

	e.endMode(t, e.a)
	e.barrier(t)

	e.wantMode(t, "after", e.a, "tree-mode", "1")
	if got := p.seen(); len(got) != 0 {
		t.Fatalf("a refused end-mode forced %d polls (%q)", len(got), got)
	}
}

// A refused copy-mode is refused before tmux is reached at all -- the target is
// not a pane id -- so there is nothing to re-read there either.
func TestACopyModeOnABadTargetForcesNoPoll(t *testing.T) {
	e, p := modePollFixture(t)

	e.copyMode(t, "work")
	e.barrier(t)

	e.wantMode(t, "after", e.a, "", "0")
	if got := p.seen(); len(got) != 0 {
		t.Fatalf("a refused copy-mode forced %d polls (%q)", len(got), got)
	}
}

// The handler is built without a poller behind it in several tests and in any
// deployment that has none; a nil hook must be a no-op rather than a panic that
// takes the socket down.
func TestCopyModeWithNoPollerConfigured(t *testing.T) {
	e := newEndModeFixtureWith(t, front.TerminalConfig{AllowedOrigin: wsCanonical})

	e.copyMode(t, e.a)
	e.barrier(t)

	// The barrier is the survival check: its answer comes back over the same
	// socket, after the copy-mode was decoded and run, so a handler that
	// panicked on a nil hook could not have produced it.
	e.wantMode(t, "after", e.a, "copy-mode", "1")
}
