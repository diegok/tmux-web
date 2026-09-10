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

// ServerStart identifies the generation of the running tmux server, or "" if
// there is none.
//
// Pane ids are only unique within one server's life: they restart at %0 when the
// server does. Anything remembering something per pane across polls -- the
// browser's "this finished and I have not looked yet" memory -- has to key on
// this too, or a remembered %3 is silently applied to an unrelated new pane.
//
// The value is tmux's own #{start_time}, in whole seconds, and it is treated as
// an opaque token rather than a time. Two servers started within the same second
// therefore share a generation; that is a restart so fast it cannot be produced
// by hand, and the consequence is one stale badge, so it is not worth a second
// discriminator that tmux does not offer.
//
// No server is not an error, for the same reason as in Snapshot: a machine where
// tmux has never started is an ordinary cold start, not a fault.
func (c *Client) ServerStart(ctx context.Context) (string, error) {
	out, err := c.Run(ctx, "display-message", "-p", "#{start_time}")
	if err != nil {
		if noServer(err.Error()) {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(out), nil
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

// SelectPane points one session at a pane: the browser tab's own grouped
// session, so clicking a pane in one tab moves neither the other tabs nor the
// terminal the user is sitting in front of.
//
// The plan specified `select-window -t <session>:<paneID>`, and tmux 3.7b
// rejects it -- "can't find window: %3". A window target parses <session>:<win>
// and looks <win> up as a name, index or @id; a pane id is not one of those.
// The bare form `select-window -t %3` does work, but it picks the session
// itself, and with grouped sessions that choice is arbitrary: with two tabs
// open on one base, it moved the wrong one. Hence the extra call to resolve the
// pane's window id, which can then be qualified with the session that must
// move. "=" pins the session name to an exact match, as in Sweep.
//
// list-panes does the resolving rather than display-message, which is the
// obvious command and is unusable here: given a target it cannot find, it
// prints an empty expansion and exits 0, and `display-message -t work:9`
// happily answers about a different window. list-panes errors on all of those.
// It reports the window id once per pane in the window; the lines are identical
// and the first is taken.
//
// Only the current window is per-session. The active pane belongs to the
// window, which grouped sessions share, so that half is visible to every member
// of the group. tmux offers no way to scope it, and it matches what the user
// sees when they select a pane in one of two attached clients.
func (c *Client) SelectPane(ctx context.Context, session, paneID string) error {
	// tmux resolves an empty target to "whatever is current" and exits 0, so an
	// unset pane id would quietly navigate the tab somewhere arbitrary instead
	// of failing. The ids come from the frontend, where "no selection yet" is
	// one bug away from being the empty string.
	if err := ValidatePaneID(paneID); err != nil {
		return fmt.Errorf("select pane: %w", err)
	}
	if session == "" {
		return fmt.Errorf("select pane %s: no session given", paneID)
	}
	out, err := c.Run(ctx, "list-panes", "-t", paneID, "-F", "#{window_id}")
	if err != nil {
		return err
	}
	window, _, _ := strings.Cut(out, "\n")
	if _, err := c.Run(ctx, "select-window", "-t", "="+session+":"+window); err != nil {
		return err
	}
	_, err = c.Run(ctx, "select-pane", "-t", paneID)
	return err
}

// KillSession removes a throwaway session explicitly. destroy-unattached is the
// crash net; this is the normal teardown path.
//
// "=" pins the target to an exact match, for the reason spelled out in Sweep:
// tmux falls back to prefix matching without reporting the ambiguity, so
// `kill-session -t _web-` would kill an unrelated _web-abcd and exit 0. Unlike
// Sweep, this takes a name from its caller rather than from tmux, so the guard
// is reachable and tested.
func (c *Client) KillSession(ctx context.Context, name string) error {
	_, err := c.Run(ctx, "kill-session", "-t", "="+name)
	return err
}

// Capture returns a pane's visible screen.
//
// -p writes it to stdout instead of a buffer, and -J rejoins a line the pane
// wrapped so that a question broken across two rows reads as one.
//
// Deliberately NOT -S -8. A negative -S counts back from the top of the visible
// screen into scrollback, so on a 6-row pane `-S -8` returns 14 lines: the
// screen plus 8 lines of history, which is exactly where a just-answered
// approval box lives. Reporting one of those as a live question is the false
// positive that would train the owner to ignore the badge. The visible screen
// is what is being asked about, and any slicing happens in Go.
//
// The whole screen is returned rather than a tail: the hash of it is what tells
// working from idle, and a redraw at the top of the screen is work too.
func (c *Client) Capture(ctx context.Context, paneID string) (string, error) {
	// tmux resolves an empty target to "whatever is current" and exits 0, so an
	// unvalidated id would silently classify some other pane -- and the state
	// would look plausible while belonging to the wrong row.
	if err := ValidatePaneID(paneID); err != nil {
		return "", fmt.Errorf("capture pane: %w", err)
	}
	return c.Run(ctx, "capture-pane", "-p", "-J", "-t", paneID)
}
