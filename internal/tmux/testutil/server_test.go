package testutil_test

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/diegok/tmux-web/internal/tmux/testutil"
)

func TestServerIsIsolatedAndCleansUp(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "probe")

	out := srv.Run(t, "list-sessions", "-F", "#{session_name}")
	if !strings.Contains(out, "probe") {
		t.Fatalf("expected probe session, got %q", out)
	}

	// The socket name must be unique per test, never the default server.
	if srv.Socket == "default" || srv.Socket == "" {
		t.Fatalf("refusing to run against socket %q", srv.Socket)
	}

	// The server must not inherit ~/.tmux.conf: a developer config that already
	// sets mouse/aggressive-resize/automatic-rename would make later assertions
	// about those options vacuous. C-b is the tmux default prefix.
	if got := srv.Run(t, "show", "-g", "-v", "prefix"); got != "C-b" {
		t.Fatalf("prefix = %q, want %q: server inherited a user config", got, "C-b")
	}

	// Sanity: the user's real server must not have gained a "probe" session.
	real, _ := exec.Command("tmux", "list-sessions", "-F", "#{session_name}").Output()
	if strings.Contains(string(real), "probe") {
		t.Fatal("test leaked a session into the user's real tmux server")
	}
}

func TestTryRunReportsFailureWithoutFataling(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "probe")

	out, err := srv.TryRun("has-session", "-t", "missing")
	if err == nil {
		t.Fatalf("expected an error for a missing session, got output %q", out)
	}
	// Diagnostics belong in the error; the returned value carries stdout only,
	// so callers that parse it never see a stray stderr line.
	if out != "" {
		t.Errorf("returned value = %q, want empty: stderr must not be merged into it", out)
	}
	if !strings.Contains(err.Error(), "can't find session") {
		t.Errorf("error should carry tmux's stderr, got %v", err)
	}
}

func TestTryRunReturnsStdoutOnSuccess(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "probe")

	out, err := srv.TryRun("list-sessions", "-F", "#{session_name}")
	if err != nil {
		t.Fatalf("list-sessions: %v", err)
	}
	if out != "probe" {
		t.Errorf("out = %q, want %q", out, "probe")
	}
}
