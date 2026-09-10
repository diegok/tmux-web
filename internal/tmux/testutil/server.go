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

// FakeAgent returns the path to an executable named name that behaves like cat:
// it holds a pane open and echoes whatever is typed into it.
//
// tmux reports #{pane_current_command} from the kernel's idea of a process's
// name, which comes from the basename of the file that was exec'd -- so a copy
// of cat named "claude" produces a pane the snapshot cannot tell from a real
// one. That is how the agent tests get an agent pane without requiring claude
// to be installed.
//
// The alternative was appending a fake name to tmux.Agents for the duration of
// a test. That is a package-level variable read by KnownAgent, which the poll
// goroutine calls, and restoring it in a cleanup races that goroutine -- the
// poller's context being cancelled does not wait for a poll already in flight.
// Copying a binary has no such window, and it exercises the real list.
func FakeAgent(t *testing.T, name string) string {
	t.Helper()
	src, err := exec.LookPath("cat")
	if err != nil {
		t.Fatalf("cat not found in PATH: %v", err)
	}
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, b, 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}
