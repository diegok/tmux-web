package front

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/diegok/tmux-web/internal/tmux"
)

// The capture panel's one read: GET /api/panes/{id}/capture.
//
// It is a GET, deliberately. It changes nothing, v1's exact-Origin rule is
// scoped to state-changing requests, and a cross-origin page cannot read a
// GET's body without CORS -- which is not enabled. Protect still applies: the
// device cookie is required, and an Origin that is present must match, so the
// only thing this route relaxes against the management verbs is the demand
// that an Origin be there at all.
//
// It shares manage.go's machinery rather than growing its own: manageCtx for
// the deadline (a wedged tmux server must not hold the request open) and
// writeManageError for the failure (tmux's own words to the owner, 504 for the
// daemon's own deadline). What it does NOT share is settle -- a read changes
// nothing, and forcing a poll for it would put a fork on the panel's Recapture
// button.
//
// Nothing here validates the pane id, for the reason manage.go gives: the
// verbs in internal/tmux validate per kind, and a second copy of those rules
// in this package is a copy that eventually disagrees. CaptureRange runs
// ValidatePaneID before it forks, so an id of the wrong shape is a 400 with no
// tmux invocation at all -- which is what TestCaptureOfAPaneThatIsNotThere
// asserts on the argv log.
//
// `lines` is the exception, and it is not a copy of anything: it is a query
// parameter that becomes an element of an argv, and it is validated here
// BEFORE tmux is reached. captureRangeArgs clamps as well. The two are about
// different things -- this one rejects a bad request, that one keeps the
// method safe to call from anywhere -- and the second is not redundancy to be
// removed.

// defaultCaptureLines is how far back the panel looks when the caller does not
// say.
//
// A GUESS -- design open question 5. Sized against a measured ~5 KB per 1000
// lines at 80 columns, which puts the default comfortably inside
// MaxCaptureBytes for an ordinary pane. The real question is how far back a
// person actually scrolls to answer an agent, and nobody has measured it.
const defaultCaptureLines = 1000

// captureResponse is what the panel reads. Every field is always present: a
// panel that had to tell "not truncated" from "the daemon did not say" would
// have to guess, and omitempty would make it do exactly that.
type captureResponse struct {
	PaneID string `json:"paneId"`
	Text   string `json:"text"`
	// Lines is the depth actually used, after the clamp -- not the depth that
	// was asked for. The panel says "the last N lines" with it.
	Lines     int  `json:"lines"`
	Truncated bool `json:"truncated"`
	// CapturedAt is unix MILLISECONDS on the daemon's clock, which is the
	// clock the rest of the wire uses (Row.FinishedAt). The panel ages it
	// against the browser's, so the two are already assumed to be close; what
	// must not happen is a units slip, which reads as a capture taken in 1970.
	CapturedAt int64 `json:"capturedAt"`
}

// capturePane answers one bounded capture of one pane's scrollback.
func (s *server) capturePane(w http.ResponseWriter, r *http.Request) {
	lines, ok := captureDepth(w, r)
	if !ok {
		return
	}
	ctx, cancel := manageCtx(r)
	defer cancel()

	// Percent-decoded by the mux: the browser sends encodeURIComponent("%3")
	// -> "%253" and PathValue hands back "%3". The un-encoded form never gets
	// here -- "%3" is an invalid escape and net/http answers 400 while parsing
	// the request line.
	paneID := r.PathValue("id")
	text, truncated, err := s.manage.CaptureRange(ctx, paneID, lines)
	if err != nil {
		writeManageError(ctx, w, err)
		return
	}
	writeJSON(w, http.StatusOK, captureResponse{
		PaneID:     paneID,
		Text:       text,
		Lines:      lines,
		Truncated:  truncated,
		CapturedAt: time.Now().UnixMilli(),
	})
}

// captureDepth reads and validates ?lines, answering the request itself and
// reporting false when it cannot.
//
// A value that is not a whole number is refused rather than defaulted, because
// this one ends up inside a tmux argument list and the rule this daemon keeps
// everywhere is that nothing unvalidated does. "1e3" is the case that makes
// the point: a strconv.Atoi whose error was ignored would send tmux a 0, and
// something like "abc" defaulted silently would leave the panel showing a
// depth nobody asked for and no way to find out why.
//
// A depth PAST the maximum is clamped rather than refused. It is the friendlier
// answer for the one caller who could send it -- a panel offering a "whole
// history" control -- and it matches what CaptureRange does for a caller
// inside the daemon. The clamped value is what the response reports, so the
// panel never claims a depth the daemon did not ask for.
//
// An absent parameter and an empty one are the same request: neither names a
// depth. Both get the default.
func captureDepth(w http.ResponseWriter, r *http.Request) (int, bool) {
	raw := r.URL.Query().Get("lines")
	if raw == "" {
		return defaultCaptureLines, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		// Out of range lands here too, and is not clamped: a number this
		// daemon could not read is not a number it can guess the intent of.
		writeError(w, http.StatusBadRequest, fmt.Sprintf(
			"lines must be a whole number between 1 and %d; got %q", tmux.MaxCaptureLines, raw))
		return 0, false
	}
	if n > tmux.MaxCaptureLines {
		n = tmux.MaxCaptureLines
	}
	return n, true
}
