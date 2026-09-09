// Package testutil starts throwaway tmux servers for tests.
package testutil

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Server is a tmux server on a private socket, killed when the test ends.
type Server struct {
	Socket string
}

// NewServer reserves a private tmux socket for a test and registers its
// cleanup. It does not start the server: tmux does that lazily on the first
// command run against the socket.
func NewServer(t *testing.T) *Server {
	t.Helper()

	// Fail rather than skip: a suite that silently skips every tmux test
	// because tmux is missing looks green while testing nothing.
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Fatalf("tmux not found in PATH: %v", err)
	}

	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	s := &Server{Socket: "wterm-test-" + socketSafe(t.Name()) + "-" + hex.EncodeToString(b)}

	t.Cleanup(func() {
		_, _ = s.TryRun("kill-server")
		// tmux does not unlink the socket on shutdown, so without this a dead
		// socket file would accumulate per test.
		_ = os.Remove(s.SocketPath())
	})
	return s
}

// socketSafe reduces a test name to characters that are safe in a socket name,
// truncated to keep the socket path well inside the unix path length limit.
func socketSafe(name string) string {
	const max = 24
	var b strings.Builder
	for _, r := range name {
		if b.Len() >= max {
			break
		}
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

// SocketPath is the filesystem path of this server's socket. tmux places it in
// $TMUX_TMPDIR/tmux-<uid>, falling back to /tmp.
func (s *Server) SocketPath() string {
	dir := os.Getenv("TMUX_TMPDIR")
	if dir == "" {
		dir = "/tmp"
	}
	return filepath.Join(dir, fmt.Sprintf("tmux-%d", os.Getuid()), s.Socket)
}

// Args prefixes tmux arguments with this server's socket. It also passes
// -f /dev/null: a tmux server started on a private socket still reads
// ~/.tmux.conf, and inheriting the developer's options would make tests that
// assert on those same options pass without testing anything.
func (s *Server) Args(args ...string) []string {
	out := make([]string, 0, 4+len(args))
	out = append(out, "-L", s.Socket, "-f", "/dev/null")
	return append(out, args...)
}

// TryRun executes a tmux command against this server and returns its stdout
// and any error. Callers that parse the output need it free of diagnostics, so
// stderr is kept out of the returned value and reported through the error
// instead. It takes no *testing.T so it can be used from cleanup and polling
// helpers, and so tests can assert that a tmux command fails.
func (s *Server) TryRun(args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.Command("tmux", s.Args(args...)...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimRight(stderr.String(), "\n"); msg != "" {
			return "", fmt.Errorf("tmux %v: %w: %s", args, err, msg)
		}
		return "", fmt.Errorf("tmux %v: %w", args, err)
	}
	return strings.TrimRight(stdout.String(), "\n"), nil
}

// Run executes a tmux command against this server and returns its stdout,
// failing the test if the command does not succeed.
func (s *Server) Run(t *testing.T, args ...string) string {
	t.Helper()
	out, err := s.TryRun(args...)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return out
}
