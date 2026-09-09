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

	// Sanity: the user's real server must not have gained a "probe" session.
	real, _ := exec.Command("tmux", "list-sessions", "-F", "#{session_name}").Output()
	if strings.Contains(string(real), "probe") {
		t.Fatal("test leaked a session into the user's real tmux server")
	}
}
