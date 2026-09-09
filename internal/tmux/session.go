package tmux

import (
	"context"
	"crypto/rand"
	"encoding/hex"
)

// AppOption is the tmux user option marking a session as app-created.
// Sessions are identified by this, never by name: a user may legitimately have
// a session called "_web-notes", and it must be neither hidden nor swept.
const AppOption = "@wterm_web"

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
func (c *Client) Sweep(ctx context.Context) error {
	out, err := c.Run(ctx, "list-sessions", "-F",
		"#{session_name}"+Sep+"#{"+AppOption+"}"+Sep+"#{session_attached}")
	if err != nil {
		return nil // no server, nothing to sweep
	}
	for _, line := range splitLines(out) {
		f := splitSep(line)
		if len(f) != 3 {
			continue
		}
		name, app, attached := f[0], f[1], f[2]
		if app == "1" && attached == "0" {
			_, _ = c.Run(ctx, "kill-session", "-t", name)
		}
	}
	return nil
}
