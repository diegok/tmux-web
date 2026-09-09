package ptybridge

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/diegok/tmux-web/internal/tmux"
)

// terminfoFS carries a compiled terminfo entry: xterm-256color plus the Hls and
// Hlr capabilities. It is compiled rather than sourced so the daemon does not
// need tic at runtime, and self-contained (tic expands use=) so it does not need
// xterm-256color to be installed either. The source is kept alongside it for
// anyone who needs to rebuild it.
//
//go:embed terminfo/w/wterm-256color
var terminfoFS embed.FS

var (
	terminfoOnce sync.Once
	terminfoDir  string
	terminfoErr  error
)

// TerminfoDir writes the embedded entry somewhere ncurses will find it and
// returns the directory to put in TERMINFO_DIRS.
//
// It writes to a temp path rather than ~/.terminfo on purpose: the entry belongs
// to this binary, so installing it into the user's home would outlive the daemon
// and quietly shadow a real one if the name ever collided.
func TerminfoDir() (string, error) {
	terminfoOnce.Do(func() {
		dir := filepath.Join(os.TempDir(), fmt.Sprintf("wterm-web-terminfo-%d", os.Getuid()))
		if err := os.MkdirAll(filepath.Join(dir, "w"), 0o700); err != nil {
			terminfoErr = err
			return
		}
		terminfoDir = dir
	})
	if terminfoErr != nil {
		return "", terminfoErr
	}
	// Rewritten rather than written once: /tmp is swept on some systems, and a
	// missing entry makes tmux refuse to attach at all.
	dst := filepath.Join(terminfoDir, "w", tmux.TermName)
	want, err := terminfoFS.ReadFile("terminfo/w/" + tmux.TermName)
	if err != nil {
		return "", err
	}
	if got, err := os.ReadFile(dst); err == nil && len(got) == len(want) {
		return terminfoDir, nil
	}
	if err := os.WriteFile(dst, want, 0o600); err != nil {
		return "", fmt.Errorf("ptybridge: writing terminfo: %w", err)
	}
	return terminfoDir, nil
}
