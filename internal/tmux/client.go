package tmux

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
)

// Client runs tmux commands against one server.
type Client struct {
	// base is prepended to every invocation, e.g. []string{"-L", "sockname"}.
	// Empty means the user's default server.
	base []string
}

func NewClient(base []string) *Client { return &Client{base: base} }

func (c *Client) command(ctx context.Context, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, "tmux", append(append([]string{}, c.base...), args...)...)
}

// Run executes a tmux command and returns trimmed stdout.
//
// stdout and stderr are captured separately, exactly as in the test harness and
// for the same reason: Snapshot splits this return value on 0x1f and indexes
// fields positionally, so a single diagnostic line merged into it would produce
// a malformed row and fail far from its cause. stderr goes into the error,
// where it is useful, rather than into data that gets parsed.
func (c *Client) Run(ctx context.Context, args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := c.command(ctx, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimRight(stderr.String(), "\n"); msg != "" {
			return "", fmt.Errorf("tmux %s: %w: %s", strings.Join(args, " "), err, msg)
		}
		return "", fmt.Errorf("tmux %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimRight(stdout.String(), "\n"), nil
}

// Snapshot returns one row per pane, deduplicated across session groups.
func (c *Client) Snapshot(ctx context.Context) ([]Row, error) {
	out, err := c.Run(ctx, "list-panes", "-a", "-F", Format)
	if err != nil {
		// No server running is not an error condition for the UI. tmux reports
		// this on stderr, which Run folds into the error message.
		if noServer(err.Error()) {
			return nil, nil
		}
		return nil, err
	}
	rows, dropped, err := ParseRows(out)
	if err != nil {
		return nil, err
	}
	if dropped > 0 {
		// A pane silently missing from the sidebar is the worst failure this
		// project has, so an unparseable row is logged rather than swallowed.
		// It is not promoted to an error: the panes that did parse are still
		// worth showing.
		slog.Warn("tmux snapshot: skipped malformed rows", "dropped", dropped)
	}
	return Dedupe(rows), nil
}

// noServer reports whether a tmux failure means "there is no server on this
// socket" rather than a real fault.
//
// tmux 3.7b says this two different ways, depending on what is left of the
// socket. Once the server has exited but its socket file survives, connect
// fails with ECONNREFUSED and tmux prints "no server running on <path>". When
// the file was never created -- a machine where tmux has not been started since
// boot, and the state of every fresh test server -- connect fails with ENOENT
// and tmux prints "error connecting to <path> (No such file or directory)".
// Both mean the same thing to the sidebar: no panes.
//
// The ENOENT text is matched alongside the prefix rather than the prefix alone,
// because "error connecting to" also covers failures the operator must see --
// a permission error on a socket owned by another user, say, which as an empty
// sidebar would be indistinguishable from a machine with no tmux running.
//
// Matching on message text is fragile: tmux exits 1 for this and for genuine
// errors alike, so there is nothing else to match on. TestSnapshotWithNoServer*
// pin both strings against a real tmux.
func noServer(msg string) bool {
	return strings.Contains(msg, "no server running") ||
		(strings.Contains(msg, "error connecting to") &&
			strings.Contains(msg, "No such file or directory"))
}
