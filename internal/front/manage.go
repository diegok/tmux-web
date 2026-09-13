package front

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// The management endpoints: create, rename, split, label, zoom and kill,
// addressed by the tmux id of the thing being acted on.
//
// Three rules run through all of them.
//
// Every route is registered behind cfg.Auth.Protect, so every one requires the
// device cookie and an Origin that matches exactly. That is the CSRF boundary
// and the only security control here: SameSite is computed on the registrable
// domain, so a service published on a sibling subdomain is same-site and its
// pages' requests would otherwise arrive carrying the owner's cookie.
//
// Every DELETE requires {"confirm": true}. It mirrors the UI's two-step dialog
// rather than replacing it -- an anti-footgun, so that a request which never
// passed through the dialog cannot destroy a window -- and it is explicitly
// *not* the security control. A page that can forge a request can forge a
// field in it.
//
// Ids arrive percent-encoded, because a pane id contains "%": the browser
// sends encodeURIComponent("%3") -> "%253" and r.PathValue hands back "%3". The
// un-encoded form is not something this code has to defend against, since "%3"
// is an invalid escape and net/http refuses the request line before the mux is
// consulted -- but the frontend has to know, which is why the tests pin the
// encoded spelling.
//
// Nothing here validates an id or a name. The verbs in internal/tmux do, per
// kind, and a second copy of those rules in this package is a copy that
// eventually disagrees with the first.
//
// And every one of them runs its tmux command under a deadline; see
// manageTimeout.

// manageTimeout bounds the tmux command one management request makes.
//
// These run on the request goroutine against the same server registry.go warns
// about -- a tmux call "can take seconds if the server is wedged" -- and an
// unbounded one holds the request until the browser gives up on it, which
// leaves the owner with a dialog that never closes and the daemon holding a
// request nobody is waiting for. Five seconds is what the socket path already
// allows one tmux command (wsTmuxTimeout) against the same wedged server, and
// tighter than the poll's, because the owner is sitting in front of this one.
//
// A var rather than a const only so a test can shorten it: waiting a real one
// out costs five seconds, and nothing in production assigns this.
var manageTimeout = 5 * time.Second

// manageCtx bounds one management request's tmux work.
//
// Derived from the request's own context, so a browser that goes away still
// cancels the call -- the deadline only puts a ceiling on how long the daemon
// waits when the browser is still there.
func manageCtx(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), manageTimeout)
}

// settle forces the poll the browser is about to ask for, and is why "the
// sidebar refreshes immediately" is now true of the daemon and not only of the
// frontend.
//
// The browser already re-fetches /api/snapshot the moment a management request
// comes back. That endpoint serves Poller.Latest(), which is up to a poll
// interval old, so the re-fetch was answered from a tree read BEFORE the verb
// ran. A killed row lingering is the mild half; the sharp half is a split,
// whose new pane id is missing from the tree the browser just asked for -- and
// a tab pointed at a pane the daemon has never heard of is worse than a stale
// row.
//
// CALLED ON THE FAILURE PATH TOO, which is the case the frontend's comment
// actually described: the ordinary management failure is an id that was alive
// when the last poll produced it and is not any more, so the row the owner just
// tried to act on is precisely the one that should disappear along with the
// error. It costs a poll on a path that is rare, and nothing at all when the
// failure was the daemon's own deadline -- ctx is already done, so this
// returns at once.
//
// BEFORE THE RESPONSE IS WRITTEN, without exception. Afterwards would be a
// race with the browser's re-fetch that the browser usually loses, which is the
// same bug with a smaller window and no way to see it.
//
// The error is dropped, deliberately. There is nothing to tell the owner: the
// verb itself already succeeded or failed on its own terms, and a poll that
// could not be forced leaves the sidebar exactly as stale as it was before any
// of this existed. Turning it into a 500 would fail a request that worked.
func (s *server) settle(ctx context.Context) {
	_ = s.snapshots.PollNow(ctx)
}

// Manager is the tmux management surface the browser drives. *tmux.Client
// satisfies it.
//
// Declared here, at the consumer, and narrow on purpose: it is the complete
// list of what a browser request can make this daemon do to the tmux server.
type Manager interface {
	NewSession(ctx context.Context, name, path string) (string, error)
	NewWindow(ctx context.Context, sessionID, name, fromPane string) (string, error)
	// ResumeAgent is a second window verb rather than a flag on the first, and
	// the difference is the point: NewWindow opens a shell, this one runs the
	// named agent's own resume. `agent` is a key into a fixed table in
	// internal/tmux, so this interface -- the complete list of what a browser
	// request can make the daemon do -- still contains no "run this string".
	ResumeAgent(ctx context.Context, sessionID, fromPane, agent string) (string, error)
	SplitPane(ctx context.Context, paneID, direction string) (string, error)
	RenameSession(ctx context.Context, sessionID, name string) error
	RenameWindow(ctx context.Context, windowID, name string) error
	SetLabel(ctx context.Context, paneID, label string) error
	ToggleZoom(ctx context.Context, paneID string) error
	KillSessionID(ctx context.Context, sessionID string) error
	KillWindow(ctx context.Context, windowID string) error
	KillPane(ctx context.Context, paneID string) error
	// CaptureRange is the one read on this interface: the capture panel's
	// bounded scrollback, GET /api/panes/{id}/capture. It is here rather than
	// on a second interface for the reason SnapshotSource.PollNow is on that
	// one -- it is the same object either way, and a fake that forgets it is a
	// compile error, which is the outcome worth having.
	CaptureRange(ctx context.Context, paneID string, lines int) (text string, truncated bool, err error)
}

// -- create -----------------------------------------------------------------

func (s *server) createSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
		// Optional, and the one path that crosses the wire: the owner types it
		// in the dialog. NewSession stats it, because `new-session -c /gone`
		// exits 0 and starts the shell in $HOME instead.
		Path string `json:"path"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	ctx, cancel := manageCtx(r)
	defer cancel()
	id, err := s.manage.NewSession(ctx, body.Name, body.Path)
	s.settle(ctx)
	if err != nil {
		writeManageError(ctx, w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": id})
}

func (s *server) createWindow(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Session string `json:"session"`
		// Optional: an empty name leaves tmux to apply its automatic one,
		// which is what an empty name field in the dialog means.
		Name string `json:"name"`
		// Optional, and a pane *id*, never a path: the daemon resolves
		// #{pane_current_path} itself so that working directories stay off
		// the wire.
		FromPane string `json:"fromPane"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	ctx, cancel := manageCtx(r)
	defer cancel()
	id, err := s.manage.NewWindow(ctx, body.Session, body.Name, body.FromPane)
	s.settle(ctx)
	if err != nil {
		writeManageError(ctx, w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": id})
}

// resumeAgent opens a window in a pane's directory running that agent's own
// resume command.
//
// Everything that makes this safe is one field: `agent` is a NAME, looked up in
// a fixed table in internal/tmux, and the argv never comes from here. There is
// deliberately no field for a command and none for a path -- the working
// directory is resolved daemon-side from the pane id, exactly as createWindow's
// fromPane is, so a browser holding a stale `Row.Path` cannot aim a resume at a
// directory the pane has left.
//
// Nothing is installed and nothing executable is written: the agent is already
// on the machine and reads its own history, which is also why this works for
// sessions that predate any of this existing. See the rule at the top of the
// route table in server.go for the line that must not be crossed.
func (s *server) resumeAgent(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Session string `json:"session"`
		// A pane *id*: the pane whose directory the agent resumes in. Required
		// here, unlike on a new window -- "resume here" with no "here" would
		// resume in the daemon's own directory and offer the wrong history.
		FromPane string `json:"fromPane"`
		// The agent's name, as pane_current_command spells it. A key, not a
		// command: anything that is not in internal/tmux's table is refused
		// before tmux is spoken to.
		Agent string `json:"agent"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	ctx, cancel := manageCtx(r)
	defer cancel()
	id, err := s.manage.ResumeAgent(ctx, body.Session, body.FromPane, body.Agent)
	s.settle(ctx)
	if err != nil {
		writeManageError(ctx, w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": id})
}

func (s *server) createPane(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Pane      string `json:"pane"`
		Direction string `json:"direction"` // "right" or "down"
	}
	if !readJSON(w, r, &body) {
		return
	}
	ctx, cancel := manageCtx(r)
	defer cancel()
	id, err := s.manage.SplitPane(ctx, body.Pane, body.Direction)
	s.settle(ctx)
	if err != nil {
		writeManageError(ctx, w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": id})
}

// -- rename, label, zoom ----------------------------------------------------

func (s *server) renameSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	ctx, cancel := manageCtx(r)
	defer cancel()
	err := s.manage.RenameSession(ctx, r.PathValue("id"), body.Name)
	s.settle(ctx)
	if err != nil {
		writeManageError(ctx, w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) renameWindow(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	ctx, cancel := manageCtx(r)
	defer cancel()
	err := s.manage.RenameWindow(ctx, r.PathValue("id"), body.Name)
	s.settle(ctx)
	if err != nil {
		writeManageError(ctx, w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// labelPane sets a pane's label. An empty label clears it, which is the
// dialog's "erase this" and not a malformed request.
func (s *server) labelPane(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Label string `json:"label"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	ctx, cancel := manageCtx(r)
	defer cancel()
	err := s.manage.SetLabel(ctx, r.PathValue("id"), body.Label)
	s.settle(ctx)
	if err != nil {
		writeManageError(ctx, w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// zoomPane toggles the zoom of the window containing a pane.
//
// No body, so none is read: a zoom is idempotent in the only sense that
// matters -- it is its own undo -- and there is nothing for a confirmation to
// protect.
func (s *server) zoomPane(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := manageCtx(r)
	defer cancel()
	err := s.manage.ToggleZoom(ctx, r.PathValue("id"))
	s.settle(ctx)
	if err != nil {
		writeManageError(ctx, w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// -- kill -------------------------------------------------------------------

func (s *server) killSession(w http.ResponseWriter, r *http.Request) {
	if !confirmed(w, r) {
		return
	}
	ctx, cancel := manageCtx(r)
	defer cancel()
	err := s.manage.KillSessionID(ctx, r.PathValue("id"))
	s.settle(ctx)
	if err != nil {
		writeManageError(ctx, w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) killWindow(w http.ResponseWriter, r *http.Request) {
	if !confirmed(w, r) {
		return
	}
	ctx, cancel := manageCtx(r)
	defer cancel()
	err := s.manage.KillWindow(ctx, r.PathValue("id"))
	s.settle(ctx)
	if err != nil {
		writeManageError(ctx, w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) killPane(w http.ResponseWriter, r *http.Request) {
	if !confirmed(w, r) {
		return
	}
	ctx, cancel := manageCtx(r)
	defer cancel()
	err := s.manage.KillPane(ctx, r.PathValue("id"))
	s.settle(ctx)
	if err != nil {
		writeManageError(ctx, w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// confirmed reports whether a delete carries {"confirm": true}, answering the
// request itself when it does not.
//
// The check is on the *value*, not on the presence of the field or on whether
// the body parsed: `{}` and `{"confirm": false}` are both refusals, and a
// delete that went through on any well-formed body would be no check at all.
func confirmed(w http.ResponseWriter, r *http.Request) bool {
	var body struct {
		Confirm bool `json:"confirm"`
	}
	if !readJSON(w, r, &body) {
		return false
	}
	if !body.Confirm {
		writeError(w, http.StatusBadRequest, `this request destroys something: send {"confirm": true} to carry it out`)
		return false
	}
	return true
}

// readJSON decodes a request body, answering the request itself and reporting
// false when it cannot.
func readJSON(w http.ResponseWriter, r *http.Request, out any) bool {
	if err := decodeJSON(w, r, out); err != nil {
		writeError(w, http.StatusBadRequest, "could not read the request: "+err.Error())
		return false
	}
	return true
}

// writeManageError answers a failed management verb.
//
// Always a 400, and never a 500, for two reasons. Every failure reachable here
// is about what the request named: an id of the wrong shape, a name tmux could
// not address afterwards, a directory that does not exist, or -- the ordinary
// case -- an id that was alive when the poll that produced it ran 1.5 seconds
// ago and is not any more. And telling those apart from a genuinely broken
// tmux would mean matching on its message text, which is exactly what must not
// be relied on: kill-pane says "can't find pane: %99" where set-option says
// "no such pane: %99" for the same stale id.
//
// tmux's own words are passed through rather than replaced. There is one user,
// they own the machine, and "can't find pane: %7" in a toast tells them what
// happened; a laundered "management failed" does not.
//
// The one exception is the daemon's own deadline running out, which is a 504.
// It does not break the rule above, it is the reason for it: telling a timeout
// apart from a refusal takes no guess at tmux's message text, because the
// daemon is the one that gave up -- and it must be told apart, since a 400
// says the owner's request was wrong when it was not, and the words tmux is
// killed with ("signal: killed") say nothing about what happened. The context
// is what is asked rather than the error, because an exec killed by its
// context wraps no context error at all: measured on go1.26, cmd.Run returns a
// bare *ExitError and errors.Is(err, context.DeadlineExceeded) is false.
//
// A browser that went away mid-request cancels the same context, and that is
// deliberately NOT a timeout: it reports itself as cancelled, and this answers
// it the ordinary way -- into a response nobody reads.
func writeManageError(ctx context.Context, w http.ResponseWriter, err error) {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		writeError(w, http.StatusGatewayTimeout, fmt.Sprintf("tmux did not answer within %s", manageTimeout))
		return
	}
	writeError(w, http.StatusBadRequest, err.Error())
}
