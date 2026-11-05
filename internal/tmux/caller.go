package tmux

import (
	"context"
	"fmt"
)

// Snapshotter is the slice of Client that CallerSession needs, so a test can
// count calls without a tmux server.
type Snapshotter interface {
	Snapshot(context.Context) ([]Row, error)
}

// CallerSession returns the id of the session paneID belongs to.
//
// Every open browser tab adds a grouped _web-* session, so the pane is reported
// once per group member. The _web-* answer is the wrong one: those sessions are
// destroy-unattached, so a window created in one dies with the tab. Dedupe
// already prefers the row the owner keeps; this is that preference for one id.
//
// A pane found only in an app-owned session is still answered, not refused:
// when the base session is gone that is the only truth there is, and deciding
// whether it is good enough is the caller's job.
//
// The id is validated first so a malformed one costs no tmux fork.
func CallerSession(ctx context.Context, c Snapshotter, paneID string) (string, error) {
	if err := ValidatePaneID(paneID); err != nil {
		return "", err
	}
	rows, err := c.Snapshot(ctx)
	if err != nil {
		return "", err
	}
	for _, r := range Dedupe(rows) {
		if r.PaneID == paneID {
			return r.SessionID, nil
		}
	}
	return "", fmt.Errorf("pane %s is not in any session", paneID)
}
