package tmux_test

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/diegok/tmux-web/internal/tmux"
	"github.com/diegok/tmux-web/internal/tmux/testutil"
)

func TestAttachLifecycle(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "work", "-x", "80", "-y", "24")

	name, f, cmd := startAttached(t, srv, "work")

	// These assertions are only meaningful because the harness starts tmux
	// with -f /dev/null: the developer's ~/.tmux.conf already sets mouse on
	// globally, which would satisfy the mouse check on its own.
	for _, tc := range []struct{ opt, want string }{
		{"destroy-unattached", "on"},
		{"status", "off"},
		{"mouse", "on"},
		{tmux.AppOption, "1"},
	} {
		got := srv.Run(t, "show", "-t", name, "-v", tc.opt)
		if got != tc.want {
			t.Errorf("%s = %q, want %q", tc.opt, got, tc.want)
		}
	}

	// Closing the PTY detaches the client; destroy-unattached must collect it.
	_ = f.Close()
	_ = cmd.Process.Kill()
	waitFor(t, 3*time.Second, func() bool {
		// The unattached "work" session keeps the server alive, so an error
		// here is never transient: it means the harness broke. Polling through
		// it and then reporting "not reaped" would name the wrong cause.
		// t.Fatalf is safe in this closure because waitFor calls cond() on the
		// test goroutine.
		out, err := srv.TryRun("list-sessions", "-F", "#{session_name}")
		if err != nil {
			t.Fatalf("list-sessions while waiting for reap: %v", err)
		}
		return !strings.Contains(out, name)
	}, "session was not reaped on detach")
}

// Sweep runs at startup, but the daemon can be restarted while a browser tab is
// attached. Collecting orphans must not kill a live session out from under it.
func TestSweepSparesAttachedAppSessions(t *testing.T) {
	srv := testutil.NewServer(t)
	// "work" is not app-owned, so the sweep spares it and it keeps the server
	// alive -- which makes the list-sessions below a real assertion rather than
	// an error whose message would name the wrong cause.
	srv.Run(t, "new-session", "-d", "-s", "work", "-x", "80", "-y", "24")
	live, _, _ := startAttached(t, srv, "work")

	srv.Run(t, "new-session", "-d", "-t", "work", "-s", "_web-orphan")
	srv.Run(t, "set", "-t", "_web-orphan", tmux.AppOption, "1")

	if err := tmux.NewClient(srv.Args()).Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}

	out := srv.Run(t, "list-sessions", "-F", "#{session_name}")
	if !strings.Contains(out, live) {
		t.Fatalf("sweep killed an attached app session, closing a live tab: %q", out)
	}
	if strings.Contains(out, "_web-orphan") {
		t.Fatalf("sweep failed to collect an app-owned orphan: %q", out)
	}
}

func TestSweepSparesUserSessions(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "_web-notes") // user's own, unmarked
	srv.Run(t, "new-session", "-d", "-s", "_web-orphan")
	srv.Run(t, "set", "-t", "_web-orphan", tmux.AppOption, "1")

	if err := tmux.NewClient(srv.Args()).Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}

	out := srv.Run(t, "list-sessions", "-F", "#{session_name}")
	if !strings.Contains(out, "_web-notes") {
		t.Fatal("sweep killed a user session that merely shares the name prefix")
	}
	if strings.Contains(out, "_web-orphan") {
		t.Fatal("sweep failed to collect an app-owned orphan")
	}
}

// startAttached runs the real one-shot attach command under a PTY and returns
// once the session reports a client. Teardown is registered with t.Cleanup
// rather than deferred in the caller, so that a t.Fatal anywhere in the test
// still collects the client: a surviving `tmux attach` holds the session open
// and defeats destroy-unattached.
func startAttached(t *testing.T, srv *testutil.Server, base string) (string, *os.File, *exec.Cmd) {
	t.Helper()
	name := tmux.NewSessionName()
	cmd := exec.Command("tmux", append(srv.Args(), tmux.AttachArgs(base, name)...)...)
	f, err := pty.Start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = f.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	waitFor(t, 3*time.Second, func() bool {
		// Check the error: a failed command yields empty output, and a
		// Contains check on empty output would make this wait vacuous.
		out, err := srv.TryRun("list-sessions", "-F", "#{session_name} #{session_attached}")
		return err == nil && strings.Contains(out, name+" 1")
	}, "session never came up attached")
	return name, f, cmd
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
