package front

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/user"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/caddyserver/certmagic"
	"github.com/diegok/tmux-web/internal/auth"
	"github.com/diegok/tmux-web/internal/tmux"
)

// DefaultPollInterval is how often the shared tmux snapshot is refreshed. One
// fork per interval regardless of how many tabs are connected, and up to one
// interval of lag before a newly spawned agent appears in the sidebar.
const DefaultPollInterval = 1500 * time.Millisecond

// maxJSONBody bounds a request body. Every JSON request this API takes is a
// token, a device name, a tmux name, a pane label or a directory; anything
// larger is a mistake or an attempt to make the daemon allocate.
const maxJSONBody = 64 << 10

// sweepTimeout bounds the startup orphan sweep. It runs before the daemon
// serves anything, so a tmux server that never answers must not hold the
// daemon down: the sweep is housekeeping, and the browser cannot reach a
// process that is still waiting for it.
const sweepTimeout = 10 * time.Second

// shutdownGrace is how long in-flight HTTP requests get to finish once the
// daemon is asked to stop. WebSockets are hijacked connections, which Shutdown
// does not wait for -- the terminal loops end with the process.
const shutdownGrace = 5 * time.Second

// SnapshotSource is the cached tmux snapshot the sidebar polls: *tmux.Poller
// satisfies it. It is an interface so a test can serve a stale snapshot, or a
// failing one, without a tmux server to break on cue.
type SnapshotSource interface {
	// Latest is the most recent *successful* poll. It is nil before the first
	// one, and shared with every other reader, so nothing here may modify it.
	Latest() []tmux.Row
	// Err is how the most recent poll ended. Non-nil with a non-nil Latest
	// means the snapshot is stale, not wrong.
	Err() error
	// ServerStart identifies the generation of the tmux server these rows came
	// from, or "" if there is none. Pane ids restart at %0 when tmux restarts,
	// so the browser keys its per-pane memory on it.
	ServerStart() string
	// PollNow forces a poll and returns once its result has been published, so
	// that a Latest taken after it reflects whatever the caller just did to the
	// tmux tree. The management handlers are the only callers: see settle.
	//
	// In this interface rather than a second one, because it is the same
	// object either way and a snapshot source that cannot be told to catch up
	// is a source these handlers cannot use. A fake that forgets it is a
	// compile error, which is the outcome worth having.
	PollNow(ctx context.Context) error
}

// DeviceAdmin is the part of *auth.Store this layer administers. Lookup lives
// on DeviceStore, which the middleware takes; this is the half the devices API
// needs.
type DeviceAdmin interface {
	Devices() []auth.Device
	Revoke(id string) error
}

// DeviceEnroller mints and redeems enrollment tokens: *auth.Enroller.
type DeviceEnroller interface {
	Mint(name string) (string, error)
	Redeem(token, userAgent string) (string, error)
}

// HandlerConfig is everything the browser-facing mux is built from. It takes
// finished collaborators rather than paths and flags so that a test can drive
// the whole route table -- auth, enrollment, revocation, the SPA -- without
// binding a port, opening a socket, or forking tmux.
type HandlerConfig struct {
	// Auth is the device-cookie middleware and the origin allowlist. Required.
	Auth *Auth

	// Store administers enrolled devices; Enroller mints and redeems links.
	Store    DeviceAdmin
	Enroller DeviceEnroller

	// Snapshots is the cached tmux state GET /api/snapshot serves.
	Snapshots SnapshotSource

	// Registry is the live half of revocation: DELETE /api/devices/{id} closes
	// what the device has open, and GET /ws registers itself in it.
	Registry *Registry

	// Manage drives the management verbs behind /api/sessions, /api/windows
	// and /api/panes. Required: a daemon built without one would answer every
	// context-menu action with a 404 and report nothing at startup.
	Manage Manager

	// Terminal is the WebSocket endpoint, already origin-checking itself.
	Terminal http.Handler

	// Assets is the built SPA, usually DistFS().
	Assets fs.FS

	// BaseURL is the daemon's public origin, e.g. https://tmux.example.com,
	// used to render enrollment links. No trailing slash.
	BaseURL string
}

// server is the assembled route table. It is unexported: NewHandler returns an
// http.Handler because nothing outside needs to reach past the mux.
type server struct {
	auth      *Auth
	store     DeviceAdmin
	enroller  DeviceEnroller
	snapshots SnapshotSource
	registry  *Registry
	manage    Manager
	terminal  http.Handler
	assets    fs.FS
	baseURL   string
	spaBuilt  bool

	// Identity for the sidebar footer, resolved once: neither the uid the
	// daemon runs as nor the hostname changes while it is running.
	osUser   string
	hostname string
}

// localIdentity is the "dev@devbox" the sidebar footer shows. It is
// functional rather than decorative: there is exactly one user -- the uid the
// daemon runs as -- so the only question it answers is which box this tab is
// driving, which starts to matter as soon as there is more than one.
//
// Both halves degrade to a placeholder rather than failing the daemon; a
// missing hostname is not a reason to refuse to serve a terminal.
func localIdentity() (osUser, hostname string) {
	osUser, hostname = "unknown", "unknown"
	if u, err := user.Current(); err == nil && u.Username != "" {
		osUser = u.Username
	}
	if h, err := os.Hostname(); err == nil && h != "" {
		hostname = h
	}
	return osUser, hostname
}

// NewHandler builds the browser-facing route table.
//
// The auth column of the plan's route table is expressed here and nowhere else,
// so it can be read in one screen:
//
//	GET    /                    device cookie          SPA shell
//	GET    /assets/             device cookie          hashed SPA bundles
//	GET    /enroll              none                   reads location.hash
//	POST   /api/enroll          none, rate limited     redeem -> set cookie
//	GET    /api/snapshot        device cookie          cached poller output
//	GET    /api/user            device cookie          footer identity
//	GET    /api/devices         device cookie          list
//	POST   /api/devices         device cookie + Origin mint a link
//	DELETE /api/devices/{id}    device cookie + Origin revoke
//	GET    /ws                  device cookie + Origin terminal
//	POST   /api/sessions        device cookie + Origin create
//	POST   /api/windows         device cookie + Origin create
//	POST   /api/panes           device cookie + Origin split
//	PATCH  /api/sessions/{id}   device cookie + Origin rename
//	PATCH  /api/windows/{id}    device cookie + Origin rename
//	PATCH  /api/panes/{id}      device cookie + Origin label
//	POST   /api/panes/{id}/zoom device cookie + Origin toggle zoom
//	DELETE /api/sessions/{id}   device cookie + Origin kill, confirmed
//	DELETE /api/windows/{id}    device cookie + Origin kill, confirmed
//	DELETE /api/panes/{id}      device cookie + Origin kill, confirmed
//
// Protect supplies the Origin requirement for the mutating routes: a present
// Origin must match on every method, and an absent one is tolerated only on
// GET and HEAD. ProtectSocket additionally requires the header, because a
// WebSocket handshake is a GET that browsers attach cookies to.
func NewHandler(cfg HandlerConfig) (http.Handler, error) {
	switch {
	case cfg.Auth == nil:
		return nil, errors.New("front: no auth middleware")
	case cfg.Store == nil:
		return nil, errors.New("front: no device store")
	case cfg.Enroller == nil:
		return nil, errors.New("front: no enroller")
	case cfg.Snapshots == nil:
		return nil, errors.New("front: no snapshot source")
	case cfg.Registry == nil:
		// Without it, revoking a device would leave its shell open, which is
		// the one failure the whole revocation design exists to prevent.
		return nil, errors.New("front: no connection registry")
	case cfg.Manage == nil:
		// Silent otherwise: the routes would simply not exist, and the owner
		// would discover it one context-menu action at a time.
		return nil, errors.New("front: no tmux manager; the management routes cannot be served")
	case cfg.BaseURL == "":
		return nil, errors.New("front: no base URL; enrollment links cannot be rendered")
	}

	assets := cfg.Assets
	if assets == nil {
		assets = emptyFS{}
	}
	s := &server{
		auth:      cfg.Auth,
		store:     cfg.Store,
		enroller:  cfg.Enroller,
		snapshots: cfg.Snapshots,
		registry:  cfg.Registry,
		manage:    cfg.Manage,
		terminal:  cfg.Terminal,
		assets:    assets,
		baseURL:   strings.TrimRight(cfg.BaseURL, "/"),
		spaBuilt:  spaAvailable(assets),
	}
	s.osUser, s.hostname = localIdentity()
	if !s.spaBuilt {
		// Once, at startup, rather than on every request: a daemon serving the
		// placeholder is running from a binary built without `make front`, and
		// that is worth exactly one line in the log.
		slog.Warn("front: this binary has no built frontend; serving a placeholder page (run `make front`)")
	}

	mux := http.NewServeMux()

	// NO ROUTE HERE EVER INSTALLS AN AGENT INTEGRATION, and that is a rule
	// about capability rather than about authentication.
	//
	// Writing executable code into somebody's repository is not a thing a
	// network request should be able to do however well authenticated it is,
	// and this daemon is reachable from a phone. The Origin middleware below is
	// the boundary for tmux operations; installing is not a tmux operation. It
	// lives in cmd/tmux-web (see install.go), in package main, which nothing
	// can import -- so the direct half of this rule is enforced by the compiler
	// and the other half, a reimplementation inside this package, is a grep in
	// cmd/tmux-web/install_test.go. If you were about to add the route, read
	// that test before you delete it.
	mux.Handle("GET /enroll", http.HandlerFunc(s.enrollPage))
	mux.Handle("POST /api/enroll", http.HandlerFunc(s.redeem))

	mux.Handle("GET /api/snapshot", cfg.Auth.Protect(http.HandlerFunc(s.snapshot)))
	mux.Handle("GET /api/user", cfg.Auth.Protect(http.HandlerFunc(s.identity)))
	mux.Handle("GET /api/devices", cfg.Auth.Protect(http.HandlerFunc(s.listDevices)))
	mux.Handle("POST /api/devices", cfg.Auth.Protect(http.HandlerFunc(s.mintDevice)))
	mux.Handle("DELETE /api/devices/{id}", cfg.Auth.Protect(http.HandlerFunc(s.revokeDevice)))

	// Management. Every one is a mutating verb, so every one carries the
	// Origin requirement as well as the cookie -- see manage.go. {id} is a
	// tmux id and arrives percent-encoded, because a pane id contains "%".
	mux.Handle("POST /api/sessions", cfg.Auth.Protect(http.HandlerFunc(s.createSession)))
	mux.Handle("POST /api/windows", cfg.Auth.Protect(http.HandlerFunc(s.createWindow)))
	mux.Handle("POST /api/panes", cfg.Auth.Protect(http.HandlerFunc(s.createPane)))
	mux.Handle("PATCH /api/sessions/{id}", cfg.Auth.Protect(http.HandlerFunc(s.renameSession)))
	mux.Handle("PATCH /api/windows/{id}", cfg.Auth.Protect(http.HandlerFunc(s.renameWindow)))
	mux.Handle("PATCH /api/panes/{id}", cfg.Auth.Protect(http.HandlerFunc(s.labelPane)))
	mux.Handle("POST /api/panes/{id}/zoom", cfg.Auth.Protect(http.HandlerFunc(s.zoomPane)))
	mux.Handle("DELETE /api/sessions/{id}", cfg.Auth.Protect(http.HandlerFunc(s.killSession)))
	mux.Handle("DELETE /api/windows/{id}", cfg.Auth.Protect(http.HandlerFunc(s.killWindow)))
	mux.Handle("DELETE /api/panes/{id}", cfg.Auth.Protect(http.HandlerFunc(s.killPane)))

	if s.terminal != nil {
		mux.Handle("GET /ws", cfg.Auth.ProtectSocket(s.trackDevice(s.terminal)))
	}

	// An honest 404 for anything else under /api/. Without this the SPA
	// fallback below would answer GET /api/anything with index.html and a 200,
	// so a frontend calling a route this daemon does not have would parse the
	// shell as JSON and report something incoherent instead of "no such
	// endpoint". ServeMux prefers the more specific patterns above, so this
	// catches only what nothing else claimed.
	mux.Handle("/api/", http.NotFoundHandler())

	// Static assets are content-addressed by Vite and must 404 when missing:
	// falling back to index.html here would answer a stale bundle request with
	// HTML, and the browser would report a syntax error in a .js file rather
	// than a missing one.
	mux.Handle("GET /assets/", cfg.Auth.Protect(http.HandlerFunc(s.asset)))

	// Everything else is the SPA shell, so a deep link the user bookmarked
	// still loads the app.
	//
	// Registered without a method, and getOnly supplies the restriction that
	// "GET /" would have expressed. ServeMux refuses to hold "GET /" alongside
	// "/api/" at all -- one has the more general path and the other the
	// narrower method set, so neither is more specific and the mux panics at
	// registration rather than guessing. Doing the method check in a wrapper
	// keeps the honest /api/ 404 above and puts the 405 in front of the auth
	// middleware, so a POST to a path that does not exist is answered as a
	// method problem rather than as a credential one.
	mux.Handle("/", getOnly(cfg.Auth.Protect(http.HandlerFunc(s.spa))))

	return securityHeaders(mux), nil
}

// -- the SPA ----------------------------------------------------------------

// spa serves the app shell, and any static file at the top level (favicon.svg),
// falling back to index.html so that client-side routes survive a reload.
func (s *server) spa(w http.ResponseWriter, r *http.Request) {
	if name, ok := assetName(r.URL.Path); ok && name != "index.html" {
		if s.serveFile(w, r, name) {
			return
		}
	}
	if !s.spaBuilt {
		s.placeholder(w)
		return
	}
	// no-store rather than an ETag: the shell names hashed bundles, and a
	// cached shell after a deploy loads bundles that are no longer there.
	w.Header().Set("Cache-Control", "no-store")
	if !s.serveFile(w, r, "index.html") {
		http.Error(w, "not found", http.StatusNotFound)
	}
}

// asset serves one built bundle, or 404s.
func (s *server) asset(w http.ResponseWriter, r *http.Request) {
	name, ok := assetName(r.URL.Path)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	// Vite puts a content hash in every asset name, so a URL that resolves
	// today resolves to the same bytes forever. private, not public: these
	// responses already carry Vary: Cookie from Protect, and there is no shared
	// cache in the deployment that should be holding them.
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	if !s.serveFile(w, r, name) {
		http.Error(w, "not found", http.StatusNotFound)
	}
}

// serveFile writes one file from the embedded FS, reporting whether it existed.
// A directory is not a file: FileServer would render an index of it, which for
// an embedded build tree is a listing of internals nobody asked for.
func (s *server) serveFile(w http.ResponseWriter, r *http.Request, name string) bool {
	f, err := s.assets.Open(name)
	if err != nil {
		return false
	}
	st, err := f.Stat()
	f.Close()
	if err != nil || st.IsDir() {
		return false
	}
	http.ServeFileFS(w, r, s.assets, name)
	return true
}

// assetName turns a request path into an fs.FS name, or reports that it is not
// one. fs.ValidPath is the check that matters: it rejects "..", a leading
// slash, and an empty element, so nothing here can escape the embedded tree.
func assetName(p string) (string, bool) {
	name := strings.TrimPrefix(path.Clean("/"+p), "/")
	if name == "" || !fs.ValidPath(name) {
		return "", false
	}
	return name, true
}

// placeholderPage is what a binary built without the frontend serves at /. It
// is a runtime condition rather than a build failure -- see embed.go -- so it
// has to say so somewhere a person will look.
const placeholderPage = `<!doctype html>
<meta charset="utf-8">
<title>tmux-web</title>
<style>body{font:16px/1.5 system-ui,sans-serif;margin:4rem auto;max-width:36rem;padding:0 1rem}code{background:#eee;padding:.1em .3em;border-radius:3px}</style>
<h1>tmux-web</h1>
<p>This binary was built without the frontend, so there is no app to serve.</p>
<p>Build it with <code>make front</code> (or <code>cd web &amp;&amp; pnpm build</code>) and rebuild.</p>
<p>The API and the terminal socket are running normally.</p>
`

func (s *server) placeholder(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	io.WriteString(w, placeholderPage)
}

// -- enrollment -------------------------------------------------------------

// enrollTemplate is the one page this daemon serves that is not the SPA.
//
// It is written in Go rather than as a React route because it is the only page
// that must work with no device cookie and no assumptions: it is what a browser
// loads when it has never talked to this daemon before, and the fewer bytes
// between the fragment and the POST that redeems it, the fewer ways there are
// to lose a single-use credential. It is also what the end-to-end test drives.
//
// The nonce is not decoration. This page holds a bearer token in
// location.hash, so it is the one page where an injected script would be worth
// writing; a CSP of default-src 'none' with a per-request nonce means the only
// code that can run is the code below.
var enrollTemplate = template.Must(template.New("enroll").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Enrolling &middot; tmux-web</title>
<style nonce="{{.Nonce}}">
body{font:16px/1.5 system-ui,sans-serif;margin:4rem auto;max-width:32rem;padding:0 1rem}
h1{font-size:1.25rem}
#msg{color:#444}
#msg.bad{color:#a00}
code{background:#eee;padding:.1em .3em;border-radius:3px}
</style>
</head>
<body>
<h1>tmux-web</h1>
<p id="msg">Enrolling this browser&hellip;</p>
<script nonce="{{.Nonce}}">
(function () {
  var msg = document.getElementById('msg');
  function fail(text) { msg.textContent = text; msg.className = 'bad'; }

  var token = location.hash.slice(1);
  // Drop the token from the address bar and from history before anything else.
  // It is a single-use credential: it must not sit in a URL the next person to
  // borrow this laptop can read out of the history, and a reload must not look
  // like it could work.
  if (location.hash) { history.replaceState(null, '', location.pathname); }
  if (!token) {
    fail('This enrollment link has no token in it. Run tmux-web enroll again and open the whole link, including the part after the #.');
    return;
  }

  fetch('/api/enroll', {
    method: 'POST',
    credentials: 'same-origin',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ token: token })
  }).then(function (r) {
    return r.json().catch(function () { return {}; }).then(function (body) {
      if (r.ok) { location.replace('/'); return; }
      fail(body.error || ('Enrollment failed (' + r.status + ').'));
    });
  }).catch(function () {
    fail('Could not reach the server. Check the connection and open the link again.');
  });
})();
</script>
</body>
</html>
`))

func (s *server) enrollPage(w http.ResponseWriter, r *http.Request) {
	nonce, err := randomNonce()
	if err != nil {
		slog.Error("front: cannot generate a CSP nonce", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// no-store: the page is trivial to re-fetch and a cached copy is one more
	// place a token could be replayed from.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; script-src 'nonce-"+nonce+"'; style-src 'nonce-"+nonce+
			"'; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	enrollTemplate.Execute(w, struct{ Nonce string }{nonce})
}

// redeem trades an enrollment token for the device cookie.
//
// It is the one route that takes no cookie -- a browser being enrolled has none
// by definition -- so the token is the whole credential, and everything that
// makes it safe lives in the enroller: 256 bits of entropy, ten minutes, single
// use, constant-time comparison, and a global limiter that turns a flood into
// one distinguishable error.
//
// The Origin check stays, and is required rather than merely matched. The
// design's rule is that every mutating request is checked against the exact
// origin allowlist, with no exemptions to reason about; a browser always sends
// Origin on a cross-origin *and* a same-origin POST, so this costs the enroll
// page nothing. A non-browser client redeeming by hand must send the header,
// which is the same deal every other mutating route offers.
func (s *server) redeem(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("Vary", "Origin")
	if got := r.Header.Values("Origin"); len(got) != 1 || !s.auth.AllowsOrigin(got[0]) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	var req struct {
		Token string `json:"token"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "that request was not the JSON this endpoint takes")
		return
	}
	if req.Token == "" {
		writeError(w, http.StatusBadRequest, "this enrollment link carried no token")
		return
	}

	token, err := s.enroller.Redeem(req.Token, r.UserAgent())
	switch {
	case errors.Is(err, auth.ErrInvalidEnrollToken):
		// 400 rather than 401: there is no credential to re-present and no
		// WWW-Authenticate to offer. The page turns this into "run enroll
		// again", which is the only thing that helps.
		writeError(w, http.StatusBadRequest, "this enrollment link is not valid; it may already have been used. Run tmux-web enroll again.")
		return
	case errors.Is(err, auth.ErrEnrollTokenExpired):
		writeError(w, http.StatusBadRequest, "this enrollment link has expired. Run tmux-web enroll again.")
		return
	case errors.Is(err, auth.ErrTooManyRedeemAttempts):
		// The limiter is checked after the token comparison, so this never
		// refuses a real link -- it is what a flood of guesses looks like from
		// here, and it is worth logging as such.
		slog.Warn("front: enrollment attempts are being rate limited")
		w.Header().Set("Retry-After", strconv.Itoa(int(auth.RedeemFailureWindow.Seconds())))
		writeError(w, http.StatusTooManyRequests, "too many enrollment attempts; try again shortly")
		return
	case err != nil:
		// The link is still live in this case -- the enroller does not burn a
		// token the store failed to persist -- so the page can be reloaded.
		slog.Error("front: enrollment failed", "err", err)
		writeError(w, http.StatusInternalServerError, "the daemon could not save this device; see its log")
		return
	}

	SetDeviceCookie(w, token)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// -- snapshot ---------------------------------------------------------------

// snapshotResponse is the sidebar's wire format. Panes is always an array,
// never null: a frontend that has to handle both is one `.map` away from a
// blank screen.
type snapshotResponse struct {
	Panes []tmux.Row `json:"panes"`
	// ServerStart is the tmux server's generation. The browser keys its per-pane
	// "finished and not yet looked at" memory on it, because pane ids restart at
	// %0 when the tmux server does -- so a remembered %3 would otherwise be
	// applied to an unrelated new pane. It is "" when no tmux server is running,
	// which is the same case as an empty Panes.
	ServerStart string `json:"serverStart"`
	// Stale means the most recent poll failed and these rows are from before
	// it. The sidebar is expected to keep rendering them and say so quietly.
	Stale bool   `json:"stale,omitempty"`
	Error string `json:"error,omitempty"`
}

// snapshot serves the cached poll.
//
// A failed poll does not blank the sidebar. The poller keeps the last good
// snapshot precisely because tmux prints "server exited unexpectedly" for a few
// milliseconds while a server restarts, and a sidebar that emptied itself for
// one interval and refilled on the next would be worse than one that was 1.5s
// stale. So: rows plus a stale flag, and a 503 only when there has never been a
// snapshot to serve -- which is the case where an empty array would be a lie
// rather than an answer.
//
// No tmux server at all is not a failure. Snapshot reports it as an empty
// result with no error, so a machine where tmux has never started answers 200
// with zero panes, and the frontend offers to create a session.
func (s *server) snapshot(w http.ResponseWriter, _ *http.Request) {
	rows, err := s.snapshots.Latest(), s.snapshots.Err()
	if err != nil && len(rows) == 0 {
		writeError(w, http.StatusServiceUnavailable, "cannot read the tmux server: "+err.Error())
		return
	}
	body := snapshotResponse{Panes: rows, ServerStart: s.snapshots.ServerStart()}
	if body.Panes == nil {
		body.Panes = []tmux.Row{}
	}
	if err != nil {
		body.Stale = true
		body.Error = err.Error()
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, body)
}

// -- devices ----------------------------------------------------------------

// deviceJSON is what this API says about a device.
//
// It is a separate type from auth.Device deliberately, and the separation is
// the control: Device carries TokenHash, and a handler that serialized the
// store's type directly would publish credential material to the browser the
// first time someone added a field. The admin socket keeps its own copy of this
// type for the same reason.
type deviceJSON struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	UserAgent string    `json:"user_agent"`
	CreatedAt time.Time `json:"created_at"`
	LastSeen  time.Time `json:"last_seen"`
	// Current marks the device making this request, so the UI can label it and
	// warn before signing itself out.
	Current bool `json:"current,omitempty"`
}

// identity serves the sidebar footer: which box, and which enrolled device is
// reading. The device half comes from the request rather than the store, so it
// is always the caller's own.
func (s *server) identity(w http.ResponseWriter, r *http.Request) {
	me, _ := DeviceFrom(r.Context())
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"user":     s.osUser,
		"host":     s.hostname,
		"device":   me.Name,
		"deviceId": me.ID,
	})
}

func (s *server) listDevices(w http.ResponseWriter, r *http.Request) {
	me, _ := DeviceFrom(r.Context())
	devices := s.store.Devices()
	out := make([]deviceJSON, 0, len(devices))
	for _, d := range devices {
		out = append(out, deviceJSON{
			ID:        d.ID,
			Name:      d.Name,
			UserAgent: d.UserAgent,
			CreatedAt: d.CreatedAt,
			LastSeen:  d.LastSeen,
			Current:   d.ID == me.ID,
		})
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"devices": out})
}

// mintDevice issues an enrollment link, the same operation the CLI performs
// over the admin socket. Doing it from an enrolled browser is what makes
// "enroll my phone from my laptop" possible without an ssh session.
func (s *server) mintDevice(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "that request was not the JSON this endpoint takes")
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeError(w, http.StatusBadRequest, "a device needs a name you will recognise in the list")
		return
	}

	token, err := s.enroller.Mint(name)
	if err != nil {
		slog.Error("front: minting an enrollment link failed", "err", err)
		writeError(w, http.StatusInternalServerError, "could not mint an enrollment link")
		return
	}
	// The token goes in the fragment, never the query: a fragment is not sent
	// to a server, so it stays out of access logs, out of Referer headers, and
	// out of the link scanners messaging apps run over a pasted URL.
	writeJSON(w, http.StatusOK, map[string]string{
		"name": name,
		"url":  s.baseURL + "/enroll#" + token,
	})
}

// revokeDevice removes a device and severs what it has open.
//
// Both halves are required. Removing the device from the store only stops it
// authenticating the *next* request; a browser holding a WebSocket keeps its
// shell until it happens to disconnect, which for a terminal is days. The
// registry is what turns revocation into something that reaches the lost
// laptop, and CloseDevice also fences a connection that authenticated a moment
// before the removal and would otherwise register just after this sweep.
func (s *server) revokeDevice(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	err := s.store.Revoke(id)
	switch {
	case errors.Is(err, auth.ErrNoSuchDevice):
		// Not a no-op: revoking a mistyped id must not report success, or the
		// owner believes a device was cut off while it still has a shell.
		writeError(w, http.StatusNotFound, "no such device")
		return
	case err != nil:
		slog.Error("front: revoking a device failed", "device", id, "err", err)
		writeError(w, http.StatusInternalServerError, "could not revoke that device")
		return
	}

	s.registry.CloseDevice(id)

	// Signing out is revoking yourself, which the design insists on: a
	// cookie-only logout would leave a live, fully privileged credential in the
	// store that no UI can still identify. Clearing the cookie as well stops
	// the browser presenting a credential that is already dead.
	if me, ok := DeviceFrom(r.Context()); ok && me.ID == id {
		ClearDeviceCookie(w)
	}
	w.WriteHeader(http.StatusNoContent)
}

// -- the terminal socket ----------------------------------------------------

// trackDevice registers a live terminal connection against the device that
// opened it, so revoking the device closes it.
//
// The closer is the cancellation of the request context, which is what the
// terminal handler runs its read, write and ping loops on: cancelling it aborts
// the parked Read, ends the write and ping loops, and tears down the tmux
// attach with them. That keeps the registry's contract -- "closing this ends
// the connection" -- without the registry or this middleware knowing anything
// about WebSockets.
//
// A device revoked between the cookie check and this registration is refused
// here rather than connected and immediately closed: Add reports it, having
// already run the closer.
func (s *server) trackDevice(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d, ok := DeviceFrom(r.Context())
		if !ok {
			// Unreachable behind Protect; a mux edit that put this route in
			// front of the middleware would otherwise open an unattributed
			// shell, and that is not a failure to discover in production.
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()

		remove, live := s.registry.Add(d.ID, cancel)
		defer remove()
		if !live {
			http.Error(w, "this device has been revoked", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// -- the admin socket -------------------------------------------------------

// SeverRevokedDevices wraps the admin API so that a revocation performed over
// the unix socket also closes that device's live connections.
//
// The admin mux cannot do this itself: it lives in internal/auth, which knows
// nothing about HTTP connections, and handing it a callback would put a
// pointer to the front layer inside the package that must stay reachable from
// the CLI alone. Wrapping is the smaller coupling -- and revocation from the
// CLI is the important case, because "I lost my laptop, ssh in and cut it off"
// is exactly when the browser cannot be used to do it.
//
// Only a 204 triggers the sweep: a 404 revoked nothing.
func SeverRevokedDevices(next http.Handler, reg *Registry) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := revokedDeviceID(r)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		if rec.status == http.StatusNoContent {
			reg.CloseDevice(id)
		}
	})
}

// revokedDeviceID reports the device a request is revoking, if it is one.
// Matched on the request rather than on a route so the wrapper needs no second
// copy of the admin mux's patterns.
func revokedDeviceID(r *http.Request) (string, bool) {
	if r.Method != http.MethodDelete {
		return "", false
	}
	id, ok := strings.CutPrefix(path.Clean(r.URL.Path), "/devices/")
	if !ok || id == "" || strings.Contains(id, "/") {
		return "", false
	}
	return id, true
}

// statusRecorder remembers the status a handler wrote. It deliberately does not
// implement Flush, Hijack or Push: the admin API answers small JSON bodies over
// a unix socket, and a wrapper that claimed capabilities it does not forward
// would be a trap for whatever is added to that API next.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

// -- the daemon -------------------------------------------------------------

// Config is what `tmux-web serve` runs with.
type Config struct {
	// Host is the public hostname browsers reach the daemon on. It is what the
	// certificate is issued for and what the origin allowlist is derived from,
	// and it is required even in dev mode -- a daemon that guessed its own
	// public name would guess wrong exactly once.
	Host string

	// Dev serves plain HTTP on loopback instead of getting a certificate, so
	// the whole stack is testable without ACME. It changes *which* origins are
	// allowed, never whether the check happens.
	Dev  bool
	Port int

	// StatePath is the device store file; empty means auth.DefaultPath.
	StatePath string

	// AdminSocket is the unix socket the CLI talks to; empty means
	// auth.AdminSocketPath.
	AdminSocket string

	// TmuxArgs selects the tmux server, e.g. {"-L", "sock"}. nil is the user's
	// default server.
	TmuxArgs []string

	// PollInterval overrides DefaultPollInterval.
	PollInterval time.Duration

	// TLSCert and TLSKey serve an existing certificate instead of getting one
	// from ACME. This is the path for an internal CA or a mkcert-issued pair,
	// which browsers accept without a warning because the CA is already
	// trusted -- the only way to run on an invented hostname and still get a
	// clean padlock.
	TLSCert, TLSKey string

	// SelfSigned generates a certificate for Host on first run and reuses it
	// afterwards. For a name no public CA can ever validate, such as an
	// invented internal one. Browsers warn once per device until the exception
	// is accepted.
	SelfSigned bool

	// TLSPort overrides 443, for running unprivileged.
	TLSPort int
}

// Serve runs the daemon until ctx is cancelled.
//
// The order of the startup sequence is the interesting part:
//
//  1. Derive the origin allowlist. It fails closed and it fails first, because
//     every other component takes it.
//  2. Open the device store. A store that exists but cannot be parsed is fatal:
//     starting empty would silently sign out every enrolled device, and the
//     first enrollment afterwards would overwrite the only copy an operator
//     could still have recovered.
//  3. Sweep orphaned app sessions, before anything can attach. Its failure is
//     logged and startup continues -- an uncollectable orphan must not stop the
//     daemon serving -- and it returns nil when there is no tmux server at all,
//     so an ordinary cold start logs nothing.
//  4. Start the poller, whose first poll is synchronous, so the first tab to
//     connect does not see an empty sidebar.
//  5. Listen on the admin socket. Failure is fatal: ErrAdminSocketInUse means
//     another daemon is already running, and two daemons writing one device
//     file is how devices get lost.
//  6. Serve.
func Serve(ctx context.Context, cfg Config) error {
	d, err := newDaemon(cfg)
	if err != nil {
		return err
	}

	// Before the poller and before anything can attach: a sweep that ran later
	// could kill a session a browser had just created.
	sweepOrphans(ctx, d.tmux)

	// Start polls once synchronously, so the first tab to connect does not see
	// an empty sidebar, and then follows ctx.
	d.poller.Start(ctx)

	socket := cfg.AdminSocket
	if socket == "" {
		if socket, err = auth.AdminSocketPath(); err != nil {
			return err
		}
	}
	// Fatal, and ErrAdminSocketInUse is why: two daemons writing one device
	// file is how devices get lost.
	ln, err := auth.ListenAdmin(socket)
	if err != nil {
		return err
	}
	admin := &http.Server{
		Handler: SeverRevokedDevices(auth.AdminMux(auth.AdminConfig{
			Store:    d.store,
			Enroller: d.enroller,
			BaseURL:  d.baseURL,
		}), d.registry),
		ReadHeaderTimeout: 10 * time.Second,
	}
	defer admin.Close()
	go func() {
		if err := admin.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("front: the admin socket stopped serving", "socket", socket, "err", err)
		}
	}()
	slog.Info("tmux-web serving", "url", d.baseURL, "state", d.statePath, "socket", socket)

	switch {
	case cfg.Dev:
		return serveDev(ctx, cfg, d.handler)
	case cfg.TLSCert != "" || cfg.SelfSigned:
		return serveOwnCert(ctx, cfg, d.handler)
	default:
		return serveTLS(ctx, cfg, d.handler)
	}
}

// daemon is everything Serve assembles before it touches the network.
//
// The split is what makes the wiring testable. Whether --dev replaces the
// origin allowlist rather than adding to it, whether the terminal socket is
// checked against the same allowlist as the cookie middleware, and which origin
// enrollment links name are all decisions made here -- and all of them would
// otherwise only be observable by starting a daemon on a real port.
type daemon struct {
	store     *auth.Store
	enroller  *auth.Enroller
	registry  *Registry
	poller    *tmux.Poller
	tmux      *tmux.Client
	terminal  *TerminalHandler
	handler   http.Handler
	baseURL   string
	statePath string
}

func newDaemon(cfg Config) (*daemon, error) {
	// First, and it fails closed: every other component takes the allowlist.
	origins, err := AllowedOrigins(cfg.Host, cfg.Dev, cfg.Port)
	if err != nil {
		return nil, err
	}

	statePath := cfg.StatePath
	if statePath == "" {
		if statePath, err = auth.DefaultPath(); err != nil {
			return nil, err
		}
	}
	// Fatal on a file that exists but will not parse. Starting empty would
	// silently sign out every enrolled device, and the first enrollment
	// afterwards would overwrite the only copy an operator could still have
	// recovered.
	store, err := auth.OpenStore(statePath)
	if err != nil {
		return nil, fmt.Errorf("%w\n  the file is not being touched: inspect it, and remove it only if you accept re-enrolling every device", err)
	}
	authn, err := NewAuth(store, origins)
	if err != nil {
		return nil, err
	}

	interval := cfg.PollInterval
	if interval <= 0 {
		interval = DefaultPollInterval
	}
	tm := tmux.NewClient(cfg.TmuxArgs)
	// Built before the daemon literal because the poller needs it: agent panes
	// are captured only while a browser is holding a terminal socket, and the
	// registry is the only thing that knows whether one is.
	registry := NewRegistry()
	d := &daemon{
		store:    store,
		enroller: auth.NewEnroller(store),
		registry: registry,
		poller: tmux.NewPollerWith(tmux.Options{
			Interval: interval,
			// One fork per poll, carrying the rows, every pane's report, every
			// pane's working directory and the server's generation. No
			// ServerStart beside it: that is the fork the batch removed, and
			// NewPollerWith refuses both.
			Poll:      tm.Poll,
			Capture:   tm.Capture,
			Connected: registry.Live,
		}),
		tmux:      tm,
		baseURL:   baseURL(cfg),
		statePath: statePath,
	}
	// The poll already reads every pane's working directory, so a split or a
	// new window opens in the right place without forking tmux to ask where
	// that is -- and those are keystroke-initiated, which is where the second
	// fork was felt. Wired after the poller exists and before anything is
	// served; the cached answer is still stat'd at the point of use.
	tm.UsePathCache(d.poller.PathFor)

	// Held on the daemon as well as handed to the router: what it was wired
	// with -- the origin allowlist, the poller's window cache -- is a decision
	// made here, and a field is the only way a test can see it without starting
	// a daemon on a real port. See the type's own comment.
	d.terminal = NewTerminalHandler(TerminalConfig{
		TmuxArgs: cfg.TmuxArgs,
		// The same snapshot that drew the sidebar row answers "which
		// window is that pane in", so clicking it forks tmux once
		// instead of three times. Wired here for the same reason as
		// UsePathCache above, and read the same way: as a hint.
		WindowFor: d.poller.WindowFor,
		// Fed from the middleware's own allowlist rather than rebuilt, so
		// the handshake asks the same question of the same list. A second
		// copy of the rule is a second thing to keep in step with --dev.
		AllowedOrigins: authn.Origins(),
	})

	d.handler, err = NewHandler(HandlerConfig{
		Auth:      authn,
		Store:     store,
		Enroller:  d.enroller,
		Snapshots: d.poller,
		Registry:  d.registry,
		Manage:    tm,
		Terminal:  d.terminal,
		Assets:    DistFS(),
		BaseURL:   d.baseURL,
	})
	if err != nil {
		return nil, err
	}
	return d, nil
}

// sweepOrphans collects app sessions left behind by a daemon that was killed.
//
// Its error is logged, never returned. Failing to collect an orphan leaks one
// tmux session; refusing to start leaves the owner with no way in at all, and
// the conditions that produce a sweep error -- an unreadable socket, say --
// persist across restarts, so a fatal sweep would be a permanently unstartable
// daemon.
func sweepOrphans(ctx context.Context, tm interface {
	Sweep(context.Context) error
}) {
	ctx, cancel := context.WithTimeout(ctx, sweepTimeout)
	defer cancel()
	if err := tm.Sweep(ctx); err != nil {
		slog.Warn("front: could not collect orphaned tmux sessions; carrying on", "err", err)
	}
}

// baseURL is the origin this daemon renders into enrollment links.
func baseURL(cfg Config) string {
	if !cfg.Dev {
		return "https://" + strings.ToLower(strings.TrimSpace(cfg.Host))
	}
	// localhost rather than the configured host: in dev the allowlist is the
	// loopback origins, so a link naming the public host would be refused by
	// the daemon that printed it.
	return "http://localhost:" + strconv.Itoa(cfg.Port)
}

// serveDev serves plain HTTP on loopback.
//
// Loopback and not 0.0.0.0: dev mode drops TLS, and a device cookie travelling
// in clear text across a network is a credential handed to whoever is on it.
// Binding both 127.0.0.1 and [::1] is not belt and braces -- "localhost"
// resolves to either depending on the machine, and a browser that picks the
// other one gets a connection refused.
func serveDev(ctx context.Context, cfg Config, handler http.Handler) error {
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	var lns []net.Listener
	for _, host := range []string{"127.0.0.1", "[::1]"} {
		ln, err := net.Listen("tcp", net.JoinHostPort(strings.Trim(host, "[]"), strconv.Itoa(cfg.Port)))
		if err != nil {
			// A machine with IPv6 disabled is ordinary; a machine that cannot
			// bind either address is not, and is reported below.
			slog.Debug("front: cannot listen", "host", host, "err", err)
			continue
		}
		lns = append(lns, ln)
	}
	if len(lns) == 0 {
		return fmt.Errorf("front: cannot listen on loopback port %d: is another daemon already running?", cfg.Port)
	}

	errc := make(chan error, len(lns))
	for _, ln := range lns {
		go func() { errc <- srv.Serve(ln) }()
	}

	select {
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		srv.Shutdown(shutdown)
		return nil
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// serveTLS gets a certificate for exactly one name and serves on :443, with
// :80 answering the ACME HTTP-01 challenge and redirecting everything else.
//
// A single-name certificate, not a wildcard. v1 serves one hostname, and a
// wildcard means DNS-01, which means zone-editing credentials sitting on the
// box for a feature that is deferred. certmagic makes adding the wildcard cheap
// later, when the published-ports feature actually needs it.
//
// certmagic.HTTPS's own documentation warns that it is unsuitable for
// "very long-lived connections" because of the timeouts it sets on its
// http.Server -- which sounds fatal for a terminal socket and is not. Its
// ReadTimeout applies to reading the handshake request; net/http clears the
// connection's deadlines when the WebSocket handler hijacks it
// (conn.hijackLocked calls SetDeadline(time.Time{})), so nothing on that
// server can time out a terminal that is merely quiet. The keepalive ping in
// the socket handler is what collects a dead peer, as it must be.
func serveTLS(ctx context.Context, cfg Config, handler http.Handler) error {
	errc := make(chan error, 1)
	go func() { errc <- certmagic.HTTPS([]string{cfg.Host}, handler) }()
	select {
	case <-ctx.Done():
		// certmagic.HTTPS owns its listeners and offers no shutdown, so the
		// process exiting is what closes them. Nothing here holds data that a
		// graceful stop would protect: tmux is the persistence layer and the
		// device file is fsynced on every write.
		return nil
	case err := <-errc:
		return err
	}
}

// serveOwnCert serves a certificate this machine already has, rather than one
// a public CA issued.
//
// Two cases, one code path. With --tls-cert the pair comes from an internal CA
// or mkcert and browsers trust it silently. With --self-signed the daemon
// generates its own and every device gets one warning to accept.
//
// This exists because ACME cannot help with an invented hostname: no public CA
// will issue for a name it has no way to validate. On a VPN, that is most
// names worth using.
//
// The fingerprint is logged so the exception accepted in the browser can be
// compared against what the daemon actually served. Without that check a
// self-signed setup trusts whatever certificate arrives, which is the whole
// thing a certificate was supposed to prevent.
func serveOwnCert(ctx context.Context, cfg Config, handler http.Handler) error {
	certPath, keyPath := cfg.TLSCert, cfg.TLSKey

	if certPath == "" {
		dir := filepath.Dir(cfg.StatePath)
		if cfg.StatePath == "" {
			p, err := auth.DefaultPath()
			if err != nil {
				return err
			}
			dir = filepath.Dir(p)
		}
		var err error
		certPath, keyPath, err = SelfSignedCert(dir, cfg.Host)
		if err != nil {
			return fmt.Errorf("front: generating a self-signed certificate: %w", err)
		}
		if fp, err := CertFingerprint(certPath); err == nil {
			slog.Warn("serving a self-signed certificate; browsers will warn once per device",
				"host", cfg.Host, "sha256", fp, "cert", certPath)
		}
	}

	port := cfg.TLSPort
	if port == 0 {
		port = 443
	}
	srv := &http.Server{
		Addr:    net.JoinHostPort("", strconv.Itoa(port)),
		Handler: handler,
		// No ReadTimeout or WriteTimeout on purpose: they would apply to the
		// terminal socket. net/http clears deadlines when a handler hijacks the
		// connection, but only after they have been set, and a WriteTimeout set
		// here would still bound the handshake in ways a quiet terminal trips.
		// The socket's own keepalive ping is what collects a dead peer.
		ReadHeaderTimeout: 20 * time.Second,
	}

	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServeTLS(certPath, keyPath) }()
	select {
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdown)
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// -- shared plumbing --------------------------------------------------------

// securityHeaders applies what every response needs regardless of route.
//
// frame-ancestors is the one that matters: this app is a terminal, and a page
// that can frame it can clickjack a shell. nosniff stops a bundle being
// re-interpreted as something executable, and no-referrer keeps the enrollment
// URL out of the Referer header of anything the app later loads.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		if h.Get("Content-Security-Policy") == "" {
			h.Set("Content-Security-Policy", "frame-ancestors 'none'")
		}
		next.ServeHTTP(w, r)
	})
}

// getOnly refuses anything but GET and HEAD. The SPA fallback answers unknown
// paths, and "unknown path" must not mean "this daemon will consider your POST".
func getOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// randomNonce returns a CSP nonce.
func randomNonce() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawStdEncoding.EncodeToString(b), nil
}

func decodeJSON(w http.ResponseWriter, r *http.Request, out any) error {
	return json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBody)).Decode(out)
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(body)
}

// writeError answers with a message meant for the owner's eyes.
//
// There is exactly one user here and they own the machine, so an error that
// says what went wrong is a feature rather than a leak -- with one boundary
// kept: the enrollment errors above say nothing that distinguishes a token that
// was never minted from one already redeemed, because only the holder of a good
// link is entitled to that difference.
func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
