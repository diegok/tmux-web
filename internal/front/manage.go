package front

import (
	"context"
	"net/http"
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

// Manager is the tmux management surface the browser drives. *tmux.Client
// satisfies it.
//
// Declared here, at the consumer, and narrow on purpose: it is the complete
// list of what a browser request can make this daemon do to the tmux server.
type Manager interface {
	NewSession(ctx context.Context, name, path string) (string, error)
	NewWindow(ctx context.Context, sessionID, name, fromPane string) (string, error)
	SplitPane(ctx context.Context, paneID, direction string) (string, error)
	RenameSession(ctx context.Context, sessionID, name string) error
	RenameWindow(ctx context.Context, windowID, name string) error
	SetLabel(ctx context.Context, paneID, label string) error
	ToggleZoom(ctx context.Context, paneID string) error
	KillSessionID(ctx context.Context, sessionID string) error
	KillWindow(ctx context.Context, windowID string) error
	KillPane(ctx context.Context, paneID string) error
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
	id, err := s.manage.NewSession(r.Context(), body.Name, body.Path)
	if err != nil {
		writeManageError(w, err)
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
	id, err := s.manage.NewWindow(r.Context(), body.Session, body.Name, body.FromPane)
	if err != nil {
		writeManageError(w, err)
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
	id, err := s.manage.SplitPane(r.Context(), body.Pane, body.Direction)
	if err != nil {
		writeManageError(w, err)
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
	if err := s.manage.RenameSession(r.Context(), r.PathValue("id"), body.Name); err != nil {
		writeManageError(w, err)
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
	if err := s.manage.RenameWindow(r.Context(), r.PathValue("id"), body.Name); err != nil {
		writeManageError(w, err)
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
	if err := s.manage.SetLabel(r.Context(), r.PathValue("id"), body.Label); err != nil {
		writeManageError(w, err)
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
	if err := s.manage.ToggleZoom(r.Context(), r.PathValue("id")); err != nil {
		writeManageError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// -- kill -------------------------------------------------------------------

func (s *server) killSession(w http.ResponseWriter, r *http.Request) {
	if !confirmed(w, r) {
		return
	}
	if err := s.manage.KillSessionID(r.Context(), r.PathValue("id")); err != nil {
		writeManageError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) killWindow(w http.ResponseWriter, r *http.Request) {
	if !confirmed(w, r) {
		return
	}
	if err := s.manage.KillWindow(r.Context(), r.PathValue("id")); err != nil {
		writeManageError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) killPane(w http.ResponseWriter, r *http.Request) {
	if !confirmed(w, r) {
		return
	}
	if err := s.manage.KillPane(r.Context(), r.PathValue("id")); err != nil {
		writeManageError(w, err)
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
func writeManageError(w http.ResponseWriter, err error) {
	writeError(w, http.StatusBadRequest, err.Error())
}
