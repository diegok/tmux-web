// Package testutil starts throwaway tmux servers for tests.
package testutil

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os/exec"
	"strings"
	"testing"
)

// Server is a tmux server on a private socket, killed when the test ends.
type Server struct {
	Socket string
}

// NewServer starts an isolated tmux server and registers its cleanup.
func NewServer(t *testing.T) *Server {
	t.Helper()

	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	s := &Server{Socket: "wterm-test-" + hex.EncodeToString(b)}

	t.Cleanup(func() {
		_ = exec.Command("tmux", "-L", s.Socket, "kill-server").Run()
	})
	return s
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
