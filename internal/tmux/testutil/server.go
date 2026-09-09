// Package testutil starts throwaway tmux servers for tests.
package testutil

import (
	"crypto/rand"
	"encoding/hex"
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

// Args prefixes tmux arguments with this server's socket.
func (s *Server) Args(args ...string) []string {
	return append([]string{"-L", s.Socket}, args...)
}

// Run executes a tmux command against this server and returns its stdout.
func (s *Server) Run(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("tmux", s.Args(args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("tmux %v: %v\n%s", args, err, out)
	}
	return strings.TrimRight(string(out), "\n")
}
