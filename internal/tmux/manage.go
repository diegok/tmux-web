package tmux

import (
	"context"
	"fmt"
	"os"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The management verbs the browser can drive: create, rename, split, kill,
// zoom, label.
//
// Two rules run through all of them, and both exist because tmux's response to
// a bad request is usually success on the wrong object rather than an error:
//
//   - Targets are ids (%N, @N, $N), validated per kind before they reach a
//     command line. A stale id from a poll 1.5s old then targets nothing. An
//     empty one would target "whatever is current", and the wrong *kind* of id
//     resolves rather than failing -- `kill-window -t %3` kills the window
//     containing pane 3, successfully.
//   - Every name the browser supplies is validated. That is four verbs, not
//     one: NewSession, NewWindow, RenameSession and RenameWindow.
//
// Working directories are resolved here, from the pane tmux already knows
// about, and never taken from the browser. The one path that does come from the
// browser -- NewSession's optional path, typed by the owner -- is stat'd first,
// because `new-session -c /gone` and `split-window -c /gone` both exit 0 and
// quietly start the shell somewhere else.

// LabelOption is the per-pane tmux user option holding a pane's label.
//
// A pane label cannot be the tmux pane title: a zsh prompt and Claude Code both
// rewrite that constantly. A user option is durable, dies with the pane, needs
// no server-side state, and is readable from the same format string -- see
// Format, which reads it through this same constant so the two cannot drift.
const LabelOption = "@wterm_label"

// MaxLabel bounds a pane label in runes.
//
// Runes rather than bytes, and for the same reason as MaxSessionName: the cap
// is a column budget for a sidebar row, and a byte cap would refuse a readable
// label in any language that is not English. The number matches MaxSessionName
// because the rows are the same width.
const MaxLabel = MaxSessionName

// Split directions, as the browser sends them. Mapped here rather than letting
// the frontend send "-h": tmux's flags are about which way the *split line*
// runs, which is the opposite of how anyone describes where the new pane goes.
const (
	SplitRight = "right"
	SplitDown  = "down"
)

// NewSession creates a detached session and returns its id.
//
// path is optional. When given it is stat'd first: tmux exits 0 for a `-c` that
// does not exist and starts the shell elsewhere, so a typo in the dialog would
// otherwise produce a session silently rooted in the wrong place. Relative
// paths resolve against this process's working directory, which is also what
// tmux would use, since the tmux client here is a child of this process.
func (c *Client) NewSession(ctx context.Context, name, path string) (string, error) {
	if err := ValidateSessionName(name); err != nil {
		return "", fmt.Errorf("new session: %w", err)
	}
	args := []string{"new-session", "-d", "-s", name, "-P", "-F", "#{session_id}"}
	if path != "" {
		if err := checkDir(path); err != nil {
			return "", fmt.Errorf("new session %q: %w", name, err)
		}
		args = append(args, "-c", path)
	}
	return c.Run(ctx, args...)
}

// NewWindow creates a window in a session and returns its id.
//
// name is optional: an empty one leaves tmux to apply its own automatic name,
// which is what the dialog's empty name field means. fromPane is optional too
// and is the pane whose working directory the new window inherits -- resolved
// here from #{pane_current_path}, never sent by the browser.
func (c *Client) NewWindow(ctx context.Context, sessionID, name, fromPane string) (string, error) {
	if err := ValidateSessionID(sessionID); err != nil {
		return "", fmt.Errorf("new window: %w", err)
	}
	if name != "" {
		if err := ValidateWindowName(name); err != nil {
			return "", fmt.Errorf("new window: %w", err)
		}
	}
	args := []string{"new-window", "-t", sessionID, "-P", "-F", "#{window_id}"}
	if fromPane != "" {
		dir, err := c.panePath(ctx, fromPane)
		if err != nil {
			return "", fmt.Errorf("new window: %w", err)
		}
		args = append(args, "-c", dir)
	}
	if name != "" {
		args = append(args, "-n", name)
	}
	return c.Run(ctx, args...)
}

// SplitPane splits a pane and returns the new pane's id. The new pane opens in
// the source pane's working directory.
func (c *Client) SplitPane(ctx context.Context, paneID, direction string) (string, error) {
	if err := ValidatePaneID(paneID); err != nil {
		return "", fmt.Errorf("split pane: %w", err)
	}
	var flag string
	switch direction {
	case SplitRight:
		flag = "-h"
	case SplitDown:
		flag = "-v"
	default:
		return "", fmt.Errorf("split pane %s: unknown direction %q (want %q or %q)",
			paneID, direction, SplitRight, SplitDown)
	}
	dir, err := c.panePath(ctx, paneID)
	if err != nil {
		return "", fmt.Errorf("split pane: %w", err)
	}
	return c.Run(ctx, "split-window", flag, "-t", paneID, "-c", dir, "-P", "-F", "#{pane_id}")
}

// RenameSession renames the session with this id.
//
// By id, not by name. tmux keeps the *pre-rename* name in session_group
// forever, so the only name the sidebar holds for a grouped session is stale
// the moment it is renamed once -- and a grouped session is what every open
// browser tab creates.
func (c *Client) RenameSession(ctx context.Context, sessionID, name string) error {
	if err := ValidateSessionID(sessionID); err != nil {
		return fmt.Errorf("rename session: %w", err)
	}
	if err := ValidateSessionName(name); err != nil {
		return fmt.Errorf("rename session %s: %w", sessionID, err)
	}
	// "--" ends the flags, and here it is load-bearing in a way it is not for
	// an option value: verified that `rename-session -t $0 -z` answers
	// "command rename-session: unknown flag -z", because a new name sits in a
	// positional slot that tmux still scans. No test pins it, and none can --
	// ValidateSessionName refuses a leading "-" first, so nothing reachable
	// gets this far. It is the second lock, kept because the first one is a
	// validator someone may one day relax.
	_, err := c.Run(ctx, "rename-session", "-t", sessionID, "--", name)
	return err
}

// RenameWindow renames the window with this id.
func (c *Client) RenameWindow(ctx context.Context, windowID, name string) error {
	if err := ValidateWindowID(windowID); err != nil {
		return fmt.Errorf("rename window: %w", err)
	}
	if err := ValidateWindowName(name); err != nil {
		return fmt.Errorf("rename window %s: %w", windowID, err)
	}
	// "--" for the same unpinnable reason as in RenameSession.
	_, err := c.Run(ctx, "rename-window", "-t", windowID, "--", name)
	return err
}

// SetLabel sets a pane's label, or clears it when label is blank.
//
// Validation here is not cosmetic. tmux sanitises pane *titles* through its own
// OSC parser, but it does not touch user option values: a label containing a
// 0x1f adds a field to the snapshot record and a newline splits the record in
// two, ParseRows drops what it cannot parse, and **the pane disappears from the
// sidebar** -- the worst failure this project has. Rejecting on write is
// necessary but not sufficient (anything with access to the socket can set the
// option out of band), which is why ParseRows also tolerates it; the residual
// is one row missing until the option is cleared.
//
// Unlike a name, a label is never a target, so the rules that keep names
// addressable do not apply: a label may start with "-", contain ":" or ".",
// and be any length up to MaxLabel.
func (c *Client) SetLabel(ctx context.Context, paneID, label string) error {
	if err := ValidatePaneID(paneID); err != nil {
		return fmt.Errorf("set label: %w", err)
	}
	// A label of spaces is an invisible row title, indistinguishable from an
	// unset one, so it clears rather than storing whitespace. Unsetting instead
	// of storing "" keeps the option absent, which is what a cleared label is.
	if strings.TrimSpace(label) == "" {
		// No -q. Unsetting an option that was never set is already quiet, and
		// -q would additionally swallow "no such pane" -- so clearing a label
		// on a pane that has since died would look like it worked.
		_, err := c.Run(ctx, "set", "-p", "-u", "-t", paneID, LabelOption)
		return err
	}
	if err := validateLabel(label); err != nil {
		return fmt.Errorf("set label on %s: %w", paneID, err)
	}
	// No "--". Unlike a new name, an option value is the second positional
	// argument and tmux never re-scans it for flags: verified that a label of
	// "-x", "-p", "-t" or "--" is stored verbatim. A flag terminator here would
	// be code no test could ever fail on.
	_, err := c.Run(ctx, "set", "-p", "-t", paneID, LabelOption, label)
	return err
}

// validateLabel rejects what would break the snapshot record or the row.
func validateLabel(label string) error {
	// encoding/json rewrites invalid UTF-8 to U+FFFD without erroring, so a
	// label that survived this would be shown as something tmux does not hold.
	if !utf8.ValidString(label) {
		return fmt.Errorf("invalid label %q: not valid UTF-8", label)
	}
	if n := utf8.RuneCountInString(label); n > MaxLabel {
		return fmt.Errorf("invalid label: %d characters, limit is %d", n, MaxLabel)
	}
	// unicode.IsControl, not a byte range: it is what catches the C1 controls
	// (U+0080-U+009F) that tmux's own byte-oriented check lets through.
	for _, r := range label {
		if unicode.IsControl(r) {
			return fmt.Errorf("invalid label %q: contains a control character (%U)", label, r)
		}
	}
	return nil
}

// ToggleZoom zooms a pane's window, or un-zooms it if it is already zoomed.
//
// Zoom is a window property, so this is visible to every client viewing the
// window, the user's own terminal included. That is v1's shared-property family
// and is intended: the browser is another client, not a private view.
func (c *Client) ToggleZoom(ctx context.Context, paneID string) error {
	if err := ValidatePaneID(paneID); err != nil {
		return fmt.Errorf("toggle zoom: %w", err)
	}
	_, err := c.Run(ctx, "resize-pane", "-Z", "-t", paneID)
	return err
}

// KillSessionID kills the session with this id.
//
// Distinct from KillSession, which takes a *name* and tears down one browser
// tab's own throwaway session. This is the user-driven verb, and it refuses an
// app session: those belong to the daemon, and killing one drops a live tab's
// socket for no reason the owner could understand.
//
// The refusal guards the direct path only. Killing a base session's last window
// destroys the whole group, app members included -- the sanctioned window and
// pane verbs reach the same end. That is the dialog's job to say ("closes the
// session and disconnects this tab"), not this function's to block: the owner
// asked for it.
func (c *Client) KillSessionID(ctx context.Context, sessionID string) error {
	if err := ValidateSessionID(sessionID); err != nil {
		return fmt.Errorf("kill session: %w", err)
	}
	// -q so that an unset option is empty rather than "invalid option", and so
	// that a stale id is empty rather than an error here -- the kill below then
	// fails with tmux's own "can't find session", which is the message the
	// toast wants.
	app, err := c.Run(ctx, "show", "-t", sessionID, "-qv", AppOption)
	if err != nil {
		return err
	}
	if app == "1" {
		return fmt.Errorf("kill session %s: refusing to kill a %s session, it belongs to the app",
			sessionID, AppOption)
	}
	_, err = c.Run(ctx, "kill-session", "-t", sessionID)
	return err
}

// KillWindow kills the window with this id, and every pane in it.
func (c *Client) KillWindow(ctx context.Context, windowID string) error {
	if err := ValidateWindowID(windowID); err != nil {
		return fmt.Errorf("kill window: %w", err)
	}
	_, err := c.Run(ctx, "kill-window", "-t", windowID)
	return err
}

// KillPane kills the pane with this id.
func (c *Client) KillPane(ctx context.Context, paneID string) error {
	if err := ValidatePaneID(paneID); err != nil {
		return fmt.Errorf("kill pane: %w", err)
	}
	_, err := c.Run(ctx, "kill-pane", "-t", paneID)
	return err
}

// panePath returns the working directory of one pane, checked.
//
// The filter is load-bearing: `list-panes -t %3` lists every pane in the window
// *containing* %3, so without it the answer could be a neighbouring pane's
// directory. display-message is the obvious alternative and is unusable for the
// same reason as in SelectPane -- given a target it cannot find it prints an
// empty expansion and exits 0, so a stale id would silently become "".
func (c *Client) panePath(ctx context.Context, paneID string) (string, error) {
	if err := ValidatePaneID(paneID); err != nil {
		return "", err
	}
	// paneID is "%" plus digits by now, so it cannot break the filter syntax.
	out, err := c.Run(ctx, "list-panes", "-t", paneID,
		"-f", "#{==:#{pane_id},"+paneID+"}", "-F", "#{pane_current_path}")
	if err != nil {
		return "", err
	}
	if out == "" {
		return "", fmt.Errorf("pane %s reports no working directory", paneID)
	}
	if err := checkDir(out); err != nil {
		return "", fmt.Errorf("pane %s: %w", paneID, err)
	}
	return out, nil
}

// checkDir refuses a working directory tmux would accept and then ignore.
//
// `split-window -c /gone` and `new-session -c /gone` both exit 0 and start the
// shell somewhere else entirely, so without this the owner gets a pane in the
// wrong directory and no indication that anything went wrong. On Linux a pane
// whose directory has been deleted reports it as "<path> (deleted)", which
// fails this check for the same reason and with the same message.
func checkDir(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("working directory %q: %w", path, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("working directory %q is not a directory", path)
	}
	return nil
}
