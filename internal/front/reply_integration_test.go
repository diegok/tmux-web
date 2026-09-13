package front_test

import (
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/diegok/tmux-web/internal/ptybridge"
)

// --- a reply typed into a pane that is in copy mode -------------------------
//
// This is the regression the "end-mode" control message exists for, and the
// only test that can show it: everything else in this suite can prove a frame
// was sent, not what tmux did with it.
//
// A pane in copy mode does not swallow a reply, it TRUNCATES it. `q` is cancel
// in both copy-mode key tables, so the moment the browser types one the pane
// leaves copy mode and every keystroke after it goes to the shell -- as a
// command, ending at the Enter the reply's own line ends with. Prose contains
// `q` early and often, so the ordinary failure is that the tail of a sentence
// runs in the agent's pane.
//
// Two choices here are load-bearing and neither is obvious:
//
//   - The keystrokes go through ptybridge, which is a real client PTY, and that
//     is the only path production has: ws.go hands a FrameData payload to
//     Session.Write and there has never been a send-keys path for typing. It is
//     also the only instrument that can show this at all. send-keys does not
//     type into a pane in copy mode, it dispatches copy-mode commands, so
//     nothing it sends reaches the shell for either case to tell apart -- and
//     on 3.7b, against a pane in copy mode with a client attached, the command
//     did not return at all.
//
//   - The payload contains a cancel key. With `echo hello` nothing arrives
//     without the cancel and everything arrives with it, which looks like a
//     difference but is the WRONG one: it is the difference between a swallowed
//     reply and a delivered one, and no assertion about a fragment can be
//     written at all. `echo quit PARTIAL` is what reproduces the fragment.
//
// The payload ends in CR (0x0d), not LF: the pty translates CR into the line
// ending the shell wants, while a raw LF is C-j.

// The reply this file types. `q` is the fifth character, which is why the shell
// sees `uit PARTIAL` when nothing pops the copy layer first.
const (
	replyPayload   = "echo quit PARTIAL\r"
	replyWhole     = "quit PARTIAL"
	replyRemainder = "uit PARTIAL"
)

// replyFixture is a tab attached to a pane running a known shell, with that
// shell's prompt already up.
type replyFixture struct {
	*wsFixture
	c    *websocket.Conn
	pane string
}

func newReplyFixture(t *testing.T) *replyFixture {
	t.Helper()
	f := defaultWSFixture(t)

	// The pane's shell is replaced before anything attaches. tmux starts the
	// developer's login shell otherwise, which drags in their rc files: the
	// assertions below read the shell's own output and its exit status, and a
	// prompt, a command-not-found handler or a slow startup would each change
	// what this test is measuring on a machine nobody can inspect from here.
	// /bin/sh reads no rc for an interactive shell and reports 127 for a
	// command it cannot find, which is all this file needs from it.
	f.srv.Run(t, "respawn-pane", "-k", "-t", "=work:0", "/bin/sh")

	r := &replyFixture{wsFixture: f}
	r.c = f.dial(t, wsCanonical, "?session=work")
	f.tabSession(t)

	// The pane the PTY types into is the tab's own -- one pane on purpose,
	// because a reply goes wherever the keystrokes go and there is nowhere else
	// for them to land. Asking the daemon rather than assuming %0 also means a
	// wrong answer here shows up as the copy mode being applied to some other
	// pane, which the first case below would catch.
	wsWrite(t, r.c, ptybridge.EncodeControl([]byte(`{"type":"where"}`)))
	r.pane = wsReadPane(t, r.c, 15*time.Second)

	// Wait for the shell by making it run something, which is the only signal
	// that says "reading and executing" rather than "process exists". It also
	// leaves $? at 0, and the exit status of the *previous* command is how the
	// cases below tell a reply that ran from one that was only typed.
	//
	// The marker is split so the echo of the input line cannot satisfy the
	// wait: the pane shows `echo REA""DY` and only the command's output is
	// READY. Every marker in this file is split for that reason.
	wsWrite(t, r.c, ptybridge.EncodeData([]byte("echo REA\"\"DY\r")))
	wsReadUntil(t, r.c, "READY", 15*time.Second)
	return r
}

func (r *replyFixture) endMode(t *testing.T) {
	t.Helper()
	// Same socket as the payload below, and the handler's read loop runs a
	// control message to completion before it decodes the next frame, so this
	// has been applied before a byte of the reply is written.
	wsWrite(t, r.c, ptybridge.EncodeControl([]byte(`{"type":"end-mode","pane":"`+r.pane+`"}`)))
}

// reply types a payload into the pane and returns the pane's text together with
// the exit status the shell reported for whatever it made of it.
//
// The status probe is what synchronises this file: it is a second command down
// the same tty, so the shell cannot answer it before it has finished with the
// reply. Nothing here waits on the clock. The poll that follows only closes the
// gap between "the shell wrote it" and "tmux has it in the grid", and it is a
// poll for the content itself rather than a guess at how long that takes.
func (r *replyFixture) reply(t *testing.T, payload string) (capture, status string) {
	t.Helper()
	wsWrite(t, r.c, ptybridge.EncodeData([]byte(payload)))
	wsWrite(t, r.c, ptybridge.EncodeData([]byte("echo \"S\"TATUS=$?\r")))

	deadline := time.Now().Add(15 * time.Second)
	for {
		capture = r.srv.Run(t, "capture-pane", "-p", "-t", r.pane)
		for _, line := range strings.Split(capture, "\n") {
			if s, ok := strings.CutPrefix(strings.TrimSpace(line), "STATUS="); ok {
				return capture, s
			}
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("the shell never reported an exit status for %q, so nothing here "+
				"can be read as finished; pane %s holds:\n%s", payload, r.pane, capture)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (r *replyFixture) wantMode(t *testing.T, when, mode, count string) {
	t.Helper()
	// pane_mode names only the top layer and pane_in_mode is a count of layers,
	// so both are needed to tell "out of copy mode" from "out of everything".
	out := r.srv.Run(t, "display-message", "-p", "-t", r.pane, "#{pane_mode} #{pane_in_mode}")
	if want := mode + " " + count; out != want {
		t.Fatalf("%s: pane %s is %q, want %q", when, r.pane, out, want)
	}
}

// replyHasLine reports whether the pane holds want as a line of its own, which
// is where a command's OUTPUT lands. The echo of what was typed is never a line
// of its own: the shell's prompt sits in front of it.
func replyHasLine(capture, want string) bool {
	for _, line := range strings.Split(capture, "\n") {
		if strings.TrimSpace(line) == want {
			return true
		}
	}
	return false
}

// The failure. Pinned because it is what the feature prevents, and because it
// is a great deal worse than a lost reply: the pane belongs to a coding agent
// and the fragment runs there as a command.
func TestAReplyIntoCopyModeLosesItsHeadAndRunsTheRest(t *testing.T) {
	r := newReplyFixture(t)
	r.srv.Run(t, "copy-mode", "-t", r.pane)
	r.wantMode(t, "before", "copy-mode", "1")

	capture, status := r.reply(t, replyPayload)

	// The `q` popped the copy layer on its way past, which is the mechanism
	// the rest of this case is about.
	r.wantMode(t, "after", "", "0")

	// Two-sided on purpose. "Contains the remainder" alone is satisfied when
	// the whole line arrived, since the remainder is a substring of it.
	if !strings.Contains(capture, replyRemainder) {
		t.Fatalf("the reply left no trace in pane %s, so this case is no longer about "+
			"truncation and proves nothing:\n%s", r.pane, capture)
	}
	if strings.Contains(capture, replyWhole) {
		t.Fatalf("the whole reply %q reached pane %s: a pane in copy mode no longer "+
			"truncates, and the end-mode message it is paired with has lost its "+
			"reason to exist:\n%s", replyWhole, r.pane, capture)
	}

	// Typed is not the same as executed, and the echo of the input line holds
	// the remainder either way. These two say the shell ran it: 127 is what a
	// POSIX shell reports for a command it could not find, and the diagnostic
	// naming `uit` without its argument cannot be the input line.
	if status != "127" {
		t.Errorf("the shell reported status %q for the fragment, want 127: it was typed "+
			"into pane %s but there is no sign it ran:\n%s", status, r.pane, capture)
	}
	if !replyDiagnosed(capture) {
		t.Errorf("no line of pane %s reports the fragment as an unknown command, so all "+
			"that is shown here is characters echoed onto the input line:\n%s", r.pane, capture)
	}
}

// The fix. The copy layer is popped first, so no keystroke of the reply is read
// as a cancel and the line arrives whole.
func TestEndModeFirstDeliversTheWholeReply(t *testing.T) {
	r := newReplyFixture(t)
	r.srv.Run(t, "copy-mode", "-t", r.pane)
	r.wantMode(t, "before", "copy-mode", "1")

	r.endMode(t)
	capture, status := r.reply(t, replyPayload)

	r.wantMode(t, "after", "", "0")

	// The command's own output, on a line of its own -- the echo of the input
	// line would contain the same words with the prompt and `echo` in front of
	// them, and would say only that the characters were typed.
	if !replyHasLine(capture, replyWhole) {
		t.Fatalf("pane %s never printed %q on a line of its own, so the reply did not "+
			"arrive whole and run:\n%s", r.pane, replyWhole, capture)
	}
	if status != "0" {
		t.Errorf("the shell reported status %q for the reply, want 0: something other "+
			"than the whole line ran in pane %s:\n%s", status, r.pane, capture)
	}
}

// replyDiagnosed reports whether some line is the shell complaining about the
// fragment rather than the fragment as it was typed. Every POSIX shell names
// the command it could not find and none of them repeat its arguments, so the
// argument is what separates the complaint from the echo.
func replyDiagnosed(capture string) bool {
	const (
		command  = "uit"     // the fragment's first word, the one a shell names
		argument = "PARTIAL" // only ever on the line the fragment was typed on
	)
	for _, line := range strings.Split(capture, "\n") {
		if strings.Contains(line, command) && !strings.Contains(line, argument) {
			return true
		}
	}
	return false
}
