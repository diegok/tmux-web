package tmux

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
)

// AppOption is the tmux user option marking a session as app-created.
// Sessions are identified by this, never by name: a user may legitimately have
// a session called "_web-notes", and it must be neither hidden nor swept.
const AppOption = "@tmux_web_owned"

// NewSessionName returns a unique name for a throwaway session.
func NewSessionName() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "_web-" + hex.EncodeToString(b)
}

// AttachArgs builds the single command that creates, attaches, and configures a
// throwaway session grouped onto base.
//
// This is deliberately one invocation. `destroy-unattached on` is not an
// on-detach hook: it is a "zero clients => destroy" invariant that fires the
// moment it becomes true. Setting it on a session created with -d destroys that
// session ~8ms later, before an attach can land. Setting it in a second call
// after spawning the attach only narrows the race, and there is no non-guessy
// way to observe the client coming up from outside. Without -d, new-session
// creates and attaches in one client, and the chained `set` commands run
// afterwards in that client's context.
func AttachArgs(base, name string) []string {
	return []string{
		"new-session", "-t", base, "-s", name,
		";", "set", "destroy-unattached", "on",
		";", "set", "status", "off",
		";", "set", "mouse", "on",
		";", "set", AppOption, "1",
	}
}

// Sweep kills app-created sessions with no attached clients. It runs at startup
// to collect sessions orphaned by a crash, since destroy-unattached cannot fire
// if the daemon died mid-attach.
//
// The listing and the kills are not atomic: a session reported with zero
// clients could in principle gain one before its kill lands, closing a live
// browser tab. That is tolerable only because the sole caller is startup, when
// no app session is attaching. A periodic sweep would need a different design.
//
// A failure that is not "no server" is returned rather than swallowed. Startup
// logs it and carries on -- an uncollectable orphan must not stop the daemon
// serving -- but it has to be visible, because the conditions that produce one
// (an unreadable socket, say) persist across every restart and leak a session
// each time.
func (c *Client) Sweep(ctx context.Context) error {
	out, err := c.Run(ctx, "list-sessions", "-F",
		"#{session_name}"+Sep+"#{"+AppOption+"}"+Sep+"#{session_attached}")
	if err != nil {
		if noServer(err.Error()) {
			return nil // nothing to sweep
		}
		return err
	}
	var errs []error
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(line, Sep)
		if len(f) != 3 {
			continue
		}
		name, app, attached := f[0], f[1], f[2]
		if app != "1" || attached != "0" {
			continue
		}
		// "=" makes the target an exact match. Sweep always passes a name it
		// just read, so a bare name is correct today -- but tmux target
		// matching falls back to a prefix, and `kill-session -t _web-` will
		// silently kill _web-abcd and exit 0. Pinning exactness here means a
		// future caller passing a partial name gets an error instead of
		// destroying a user's session, which is the failure @tmux_web_owned exists
		// to prevent. No test pins this: Sweep never passes a partial name, and
		// tmux prefers an exact match over a longer prefix, so "=" and a bare
		// name behave identically for every input reachable today.
		if _, err := c.Run(ctx, "kill-session", "-t", "="+name); err != nil {
			// The session going away between the listing and the kill is the
			// expected race, not a fault. Anything else is worth surfacing.
			// Matched on message text for the same reason as noServer: tmux
			// exits 1 for every failure alike.
			if !strings.Contains(err.Error(), "can't find session") {
				errs = append(errs, err)
			}
		}
	}
	// Nil when errs is empty, so the common path returns no error.
	return errors.Join(errs...)
}

// TermName is the TERM the browser's tmux client runs under, and the key that
// scopes hyperlink support to it.
//
// Deliberately not xterm-256color. tmux only sends OSC 8 hyperlinks to a client
// whose terminal advertises the "hyperlinks" feature, and that feature is
// matched by TERM name against a *server-wide* option -- so sharing a TERM with
// the user's own local clients would turn hyperlinks on for their terminal too.
// A name only this client uses keeps the change scoped to the browser.
const TermName = "tmux-web-256color"

// hyperlinkFeature is the terminal-features entry that lets OSC 8 through.
const hyperlinkFeature = TermName + ":hyperlinks"

// EnableHyperlinks tells the tmux server that TermName clients can render OSC 8
// links, so a URL emitted by gh, delta or eza arrives at the browser as a real
// anchor instead of being stripped.
//
// Without it tmux sends zero OSC 8 sequences to the web client: the link is in
// the grid (capture-pane -e shows it) and dropped on the way out. Measured.
//
// Appended, never assigned: a bare `set -s` would replace the server's whole
// terminal-features list, discarding the entries tmux ships for xterm, screen
// and rxvt. And appended only once -- `set -sa` does not deduplicate, so
// calling this per attach would grow the list by one entry per browser tab.
func (c *Client) EnableHyperlinks(ctx context.Context) error {
	out, err := c.Run(ctx, "show", "-s", "terminal-features")
	if err != nil {
		if noServer(err.Error()) {
			// Nothing to configure yet. Open calls this again once a server
			// exists, so a cold start still gets links on its first attach.
			return nil
		}
		return err
	}
	if strings.Contains(out, hyperlinkFeature) {
		return nil
	}
	// The leading comma is what makes this an append to the option's own list
	// rather than a new array entry that happens to parse.
	_, err = c.Run(ctx, "set", "-sa", "terminal-features", ","+hyperlinkFeature)
	return err
}
