package ptybridge

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/creack/pty"
	"github.com/diegok/tmux-web/internal/tmux"
)

// outputBuffer bounds how much PTY output may queue for a slow client, in
// reads rather than bytes. 256 reads of at most 32KB is roughly 8MB of slack --
// far more than a redraw burst, and small enough that a tab which stopped
// reading is noticed rather than buffered indefinitely.
const outputBuffer = 256

// killTimeout bounds the teardown tmux call. Close runs on the pump goroutine
// on the overflow path, and the output channel is not closed until Close
// returns, so a wedged tmux server must not be able to park teardown forever
// with a reader still waiting to learn the session ended.
const killTimeout = 5 * time.Second

type Config struct {
	TmuxArgs []string // server selection, e.g. {"-L", "sock"}; nil for default
	Base     string   // the real session to group onto
	Cols     uint16
	Rows     uint16
}

// Session is one browser tab's tmux client: a throwaway session grouped onto a
// real one, attached under a PTY.
type Session struct {
	name string
	cmd  *exec.Cmd
	pty  *os.File
	out  chan []byte
	tm   *tmux.Client

	closeOnce sync.Once
}

// Open starts the attach and begins pumping its output.
//
// ctx is not wired to the process lifetime: the session outlives whatever
// request opened it and is ended by Close, not by a cancelled context. It is
// taken so the signature stays honest as an I/O-performing constructor and so
// callers pass one habitually.
func Open(ctx context.Context, cfg Config) (*Session, error) {
	// Idempotent, and cheap: one `show` per attach, and a `set` only the very
	// first time. Done here rather than only at daemon startup because the tmux
	// server may not have existed then -- the user can start one at any point.
	// A failure costs clickable links, not the terminal, so it is logged.
	if err := tmux.NewClient(cfg.TmuxArgs).EnableHyperlinks(ctx); err != nil {
		slog.Warn("ptybridge: could not enable tmux hyperlinks", "err", err)
	}

	name := tmux.NewSessionName()
	args := append(append([]string{}, cfg.TmuxArgs...), tmux.AttachArgs(cfg.Base, name)...)

	cmd := exec.Command("tmux", args...)
	// TERM must match what wterm emulates, whatever the daemon inherited --
	// it is started from a login session, a service manager or a cron-like
	// context, and tmux refuses to attach under some of those (TERM=dumb
	// fails with "terminal does not support clear"). tmux advertises
	// tmux-256color to programs inside the session, which is expected and
	// separate from this.
	//
	// TermName rather than xterm-256color so tmux will send OSC 8 hyperlinks
	// here without also sending them to the user's own terminal; see terminfo.go.
	// If the entry cannot be written, fall back rather than refuse to open a
	// terminal: losing clickable links is worth far less than losing the shell.
	env := append(os.Environ(), "TERM="+tmux.TermName)
	if dir, err := TerminfoDir(); err == nil {
		env = append(env, "TERMINFO_DIRS="+dir+":")
	} else {
		slog.Warn("ptybridge: no terminfo for "+tmux.TermName+", links will not be clickable", "err", err)
		env = append(env[:len(env)-1], "TERM=xterm-256color")
	}
	cmd.Env = env

	f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: cfg.Cols, Rows: cfg.Rows})
	if err != nil {
		return nil, err
	}

	s := &Session{
		name: name,
		cmd:  cmd,
		pty:  f,
		out:  make(chan []byte, outputBuffer),
		tm:   tmux.NewClient(cfg.TmuxArgs),
	}
	go s.pump()
	return s, nil
}

// SessionName is the name of this tab's throwaway tmux session.
func (s *Session) SessionName() string { return s.name }

// Output yields PTY reads until the session ends, then closes. A closed channel
// is the only end-of-session signal: readers must handle it rather than block.
func (s *Session) Output() <-chan []byte { return s.out }

// pump reads the PTY into the output channel. It is the only writer to out and
// closes it on the way out, so a reader always observes the end of the session.
//
// If the channel is full the session is closed rather than blocking. Blocking
// here would stall a tmux client that shares the server with the user's local
// session, so a wedged browser tab could freeze the real terminal they are
// working in. Dropping bytes is equally unacceptable: a truncated escape
// sequence corrupts the terminal permanently, and unlike a dropped frame it
// never heals. Closing is safe because tmux redraws the whole screen on
// reattach, so the tab reconnects and loses nothing.
func (s *Session) pump() {
	defer close(s.out)
	buf := make([]byte, 32*1024)
	for {
		n, err := s.pty.Read(buf)
		if n > 0 {
			b := make([]byte, n)
			copy(b, buf[:n])
			select {
			case s.out <- b:
			default:
				s.Close()
				return
			}
		}
		if err != nil {
			// Includes the expected ending: Close closes the PTY, which
			// unblocks this read with EIO or "file already closed".
			return
		}
	}
}

// Write sends keystrokes to the tmux client.
func (s *Session) Write(b []byte) (int, error) { return s.pty.Write(b) }

// Resize tells the tmux client the browser terminal changed size.
func (s *Session) Resize(cols, rows uint16) error {
	return pty.Setsize(s.pty, &pty.Winsize{Cols: cols, Rows: rows})
}

// SelectPane navigates this tab's own session only.
func (s *Session) SelectPane(ctx context.Context, paneID string) error {
	return s.tm.SelectPane(ctx, s.name, paneID)
}

// Close ends the session. It is safe to call more than once and from several
// goroutines: the websocket handler calls it when the socket goes away, and the
// pump calls it on overflow.
//
// The order is local-then-remote. Closing the PTY cannot fail or block, and it
// is what unblocks a pump parked in Read, so the reader learns the session
// ended without waiting on tmux. Reaping the client keeps a long-lived daemon
// from collecting one zombie and one open PTY per closed tab.
//
// No test pins the kill between them, and none can: closing the master hangs
// the client up, and the kernel delivers that even to a process stopped by a
// signal, so tmux exits on its own in every state reachable from here. It stays
// because Wait has no other guarantee of returning -- teardown runs on the pump
// goroutine, and a client that somehow outlived the hangup would park it there
// with a reader still waiting on Output().
//
// kill-session then usually loses a race it is not meant to win: the client is
// already gone, so destroy-unattached has collected the session and tmux
// answers "can't find session". The error is discarded for exactly that reason.
// It still has to be sent, because destroy-unattached is the crash net and may
// not be in force -- the session was created and configured in one command, and
// a `set` that failed would leave a session nothing else ever reaps.
func (s *Session) Close() {
	s.closeOnce.Do(func() {
		_ = s.pty.Close()
		if s.cmd.Process != nil {
			_ = s.cmd.Process.Kill()
			// Wait after the PTY is closed and the client killed, never
			// before: waiting first parks teardown until the client exits by
			// itself, and at that point nothing has told it to.
			_ = s.cmd.Wait()
		}
		ctx, cancel := context.WithTimeout(context.Background(), killTimeout)
		defer cancel()
		_ = s.tm.KillSession(ctx, s.name)
	})
}
