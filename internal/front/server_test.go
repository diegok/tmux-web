package front_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/diegok/tmux-web/internal/auth"
	"github.com/diegok/tmux-web/internal/front"
	"github.com/diegok/tmux-web/internal/tmux"
)

// The route table is the thing under test here, and almost every test needs the
// same four collaborators behind it: a real store with one enrolled device, a
// real enroller, a real registry, and a snapshot source a test can break on
// cue. Only tmux is faked -- the terminal endpoint's own behavior is ws_test's
// subject, and what this file needs from it is a handler it can watch being
// authorized, registered, and severed.

// fakeSnapshots is a *tmux.Poller that answers what a test tells it to.
type fakeSnapshots struct {
	mu          sync.Mutex
	rows        []tmux.Row
	err         error
	serverStart string
}

func (f *fakeSnapshots) ServerStart() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.serverStart
}

func (f *fakeSnapshots) Latest() []tmux.Row {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rows
}

func (f *fakeSnapshots) Err() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}

func (f *fakeSnapshots) set(rows []tmux.Row, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows, f.err = rows, err
}

func (f *fakeSnapshots) setServerStart(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.serverStart = s
}

// builtSPA is what Vite leaves in internal/front/dist: a shell naming one
// content-hashed bundle.
func builtSPA() fstest.MapFS {
	return fstest.MapFS{
		"index.html":             &fstest.MapFile{Data: []byte("<!doctype html><div id=root></div><script src=/assets/index-abc123.js></script>")},
		"assets/index-abc123.js": &fstest.MapFile{Data: []byte("console.log('spa')")},
		"favicon.svg":            &fstest.MapFile{Data: []byte("<svg/>")},
	}
}

type fixture struct {
	t         *testing.T
	handler   http.Handler
	store     *auth.Store
	storePath string
	enroller  *auth.Enroller
	registry  *front.Registry
	snaps     *fakeSnapshots

	// token authenticates deviceID, the one device enrolled at the start.
	token    string
	deviceID string

	// terminal records what reached the WebSocket endpoint, and blocks there
	// until its request context is cancelled, the way a real terminal does.
	terminalOpen  chan struct{}
	terminalGone  chan struct{}
	terminalCalls atomic.Int64
}

type fixtureOpt func(*front.HandlerConfig)

func withOrigins(origins ...string) fixtureOpt {
	return func(cfg *front.HandlerConfig) {
		// Rebuilt rather than mutated: Auth parses its allowlist once, at
		// startup, and there is no setter.
		a, err := front.NewAuth(cfg.Store.(*auth.Store), origins)
		if err != nil {
			panic(err)
		}
		cfg.Auth = a
	}
}

func withAssets(fsys fstest.MapFS) fixtureOpt {
	return func(cfg *front.HandlerConfig) { cfg.Assets = fsys }
}

func newFixture(t *testing.T, opts ...fixtureOpt) *fixture {
	t.Helper()

	store, token, storePath := enrolledStore(t)
	devices := store.Devices()
	if len(devices) != 1 {
		t.Fatalf("expected one enrolled device, got %d", len(devices))
	}

	f := &fixture{
		t:            t,
		store:        store,
		storePath:    storePath,
		enroller:     auth.NewEnroller(store),
		registry:     front.NewRegistry(),
		snaps:        &fakeSnapshots{},
		token:        token,
		deviceID:     devices[0].ID,
		terminalOpen: make(chan struct{}, 8),
		terminalGone: make(chan struct{}, 8),
	}

	cfg := front.HandlerConfig{
		Auth:      newAuth(t, store),
		Store:     store,
		Enroller:  f.enroller,
		Snapshots: f.snaps,
		Registry:  f.registry,
		Terminal: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			f.terminalCalls.Add(1)
			f.terminalOpen <- struct{}{}
			<-r.Context().Done()
			f.terminalGone <- struct{}{}
		}),
		Assets:  builtSPA(),
		BaseURL: canonicalOrigin,
	}
	for _, opt := range opts {
		opt(&cfg)
	}

	h, err := front.NewHandler(cfg)
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	f.handler = h
	return f
}

// req builds a request. Every test spells out its own credential and Origin,
// because which of the two a route requires is exactly what is being tested.
type reqOpt func(*http.Request)

func authed(token string) reqOpt {
	return func(r *http.Request) { withCookie(r, token) }
}

func origin(o string) reqOpt {
	return func(r *http.Request) { r.Header.Set("Origin", o) }
}

func (f *fixture) do(method, target string, body string, opts ...reqOpt) *httptest.ResponseRecorder {
	f.t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	for _, opt := range opts {
		opt(r)
	}
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, r)
	return rec
}

// ok is the ordinary authenticated request: the device cookie and the app's own
// origin, which is what every real request from the SPA carries.
func (f *fixture) ok(method, target, body string) *httptest.ResponseRecorder {
	f.t.Helper()
	return f.do(method, target, body, authed(f.token), origin(canonicalOrigin))
}

// refused runs a request that must be turned away before it reaches a handler,
// with a deadline. The terminal endpoint blocks for the life of a connection,
// so a middleware that wrongly let one of these through would hang the test
// binary until its timeout -- and a test that fails by hanging tells nobody
// which check stopped working.
func (f *fixture) refused(method, target, body string, opts ...reqOpt) int {
	f.t.Helper()
	code := make(chan int, 1)
	go func() { code <- f.do(method, target, body, opts...).Code }()
	select {
	case c := <-code:
		return c
	case <-time.After(2 * time.Second):
		f.t.Fatalf("%s %s was not refused: it reached a handler that blocks", method, target)
		return 0
	}
}

func decode(t *testing.T, rec *httptest.ResponseRecorder, out any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
}

func cookieFrom(rec *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range (&http.Response{Header: rec.Header()}).Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// ------------------------------------------------------------ the auth matrix

// Every row of the plan's route table, checked as a table: which routes need a
// device cookie, which additionally need an Origin, and which need neither.
//
// This is the test that fails when a route is registered without its
// middleware, which is the single most consequential mistake this file can
// contain -- an unprotected /ws or /api/devices is a shell handed to the
// internet.
func TestEveryRouteEnforcesTheAuthItsRowSpecifies(t *testing.T) {
	cases := []struct {
		name         string
		method       string
		target       string
		body         string
		needsCookie  bool
		needsOrigin  bool
		authorized   int // status when properly credentialed
		unauthorized int
	}{
		{name: "spa shell", method: "GET", target: "/", needsCookie: true, authorized: 200},
		{name: "deep link", method: "GET", target: "/session/work", needsCookie: true, authorized: 200},
		{name: "asset", method: "GET", target: "/assets/index-abc123.js", needsCookie: true, authorized: 200},
		{name: "snapshot", method: "GET", target: "/api/snapshot", needsCookie: true, authorized: 200},
		{name: "devices list", method: "GET", target: "/api/devices", needsCookie: true, authorized: 200},
		{name: "mint", method: "POST", target: "/api/devices", body: `{"name":"phone"}`, needsCookie: true, needsOrigin: true, authorized: 200},
		{name: "revoke", method: "DELETE", target: "/api/devices/nope", needsCookie: true, needsOrigin: true, authorized: 404},
		{name: "terminal", method: "GET", target: "/ws", needsCookie: true, needsOrigin: true, authorized: -1},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)

			// No credential at all.
			want := http.StatusUnauthorized
			if c.needsOrigin {
				// Origin is checked before the cookie, so a mutating request
				// with neither is refused on the origin.
				want = http.StatusForbidden
			}
			if code := f.refused(c.method, c.target, c.body); code != want {
				t.Errorf("uncredentialed %s %s = %d, want %d", c.method, c.target, code, want)
			}

			// A page on a sibling subdomain -- same site, different origin,
			// carrying the victim's cookie.
			if code := f.refused(c.method, c.target, c.body, authed(f.token), origin(siblingOrigin)); code != http.StatusForbidden {
				t.Errorf("%s %s from %s = %d, want 403", c.method, c.target, siblingOrigin, code)
			}

			// The right cookie from the right origin.
			if c.authorized > 0 {
				if rec := f.ok(c.method, c.target, c.body); rec.Code != c.authorized {
					t.Errorf("authorized %s %s = %d (%s), want %d",
						c.method, c.target, rec.Code, rec.Body.String(), c.authorized)
				}
			}
		})
	}
}

// The handshake is a GET, and browsers attach cookies to cross-origin
// WebSocket handshakes, so "GET is safe" would hand a shell to any page the
// user visits. ProtectSocket requires the header rather than merely matching it.
func TestTheTerminalHandshakeRequiresAnOriginEvenThoughItIsAGet(t *testing.T) {
	f := newFixture(t)
	if code := f.refused("GET", "/ws", "", authed(f.token)); code != http.StatusForbidden {
		t.Fatalf("GET /ws with no Origin = %d, want 403", code)
	}
	if f.terminalCalls.Load() != 0 {
		t.Fatal("the terminal handler ran for a request with no Origin")
	}
}

// ---------------------------------------------------------------- enrollment

func TestTheEnrollPageIsServedWithoutAnyCredential(t *testing.T) {
	f := newFixture(t)
	rec := f.do("GET", "/enroll", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /enroll = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	// The token travels in the fragment, so the page is the thing that reads
	// it. If this stops being true the enrollment link silently stops working.
	if !strings.Contains(body, "location.hash") {
		t.Error("the enroll page does not read location.hash")
	}
	if !strings.Contains(body, "/api/enroll") {
		t.Error("the enroll page does not post to /api/enroll")
	}
	// The token is a single-use bearer credential sitting in the address bar.
	// The page drops it from the URL and from history before doing anything
	// else, so it is not left for the next person to borrow the laptop and a
	// reload cannot look like it might work. Asserted on the source of the page
	// because the Go suite has no DOM; the end-to-end test drives the behavior.
	if !strings.Contains(body, "history.replaceState") {
		t.Error("the enroll page leaves the token in the address bar and in history")
	}
}

// http.FileServer would render an index of the embedded build tree here, which
// is a listing of internals nobody asked for and a map of every bundle name.
func TestTheAssetDirectoryIsNotBrowsable(t *testing.T) {
	f := newFixture(t)
	rec := f.ok("GET", "/assets/", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /assets/ = %d, want 404", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "index-abc123.js") {
		t.Fatalf("GET /assets/ listed the build tree: %s", rec.Body.String())
	}
}

// The page holds a bearer token in its URL, so it is the one page where an
// injected script would be worth writing.
func TestTheEnrollPageIsLockedDownByCSP(t *testing.T) {
	f := newFixture(t)
	rec := f.do("GET", "/enroll", "")
	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "nonce-") {
		t.Fatalf("enroll CSP = %q", csp)
	}
	// A nonce that repeats is not a nonce.
	second := f.do("GET", "/enroll", "").Header().Get("Content-Security-Policy")
	if csp == second {
		t.Fatal("the CSP nonce is reused across requests")
	}
}

func TestRedeemingALinkSetsTheDeviceCookieAndAuthenticates(t *testing.T) {
	f := newFixture(t)
	token, err := f.enroller.Mint("phone")
	if err != nil {
		t.Fatal(err)
	}

	rec := f.do("POST", "/api/enroll", `{"token":"`+token+`"}`, origin(canonicalOrigin))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/enroll = %d (%s), want 200", rec.Code, rec.Body.String())
	}
	c := cookieFrom(rec, wireCookieName)
	if c == nil {
		t.Fatal("no device cookie was set")
	}
	if !c.Secure || !c.HttpOnly || c.Path != "/" || c.Domain != "" {
		t.Fatalf("device cookie = %+v", c)
	}

	// The cookie is a credential, not a receipt: it must authenticate.
	if rec := f.do("GET", "/api/snapshot", "", authed(c.Value)); rec.Code != http.StatusOK {
		t.Fatalf("the freshly enrolled cookie was refused: %d", rec.Code)
	}
	// And it enrolled a device that the list can now show.
	if got := len(f.store.Devices()); got != 2 {
		t.Fatalf("devices after enrolling = %d, want 2", got)
	}
}

func TestRedeemRecordsTheBrowserThatRedeemed(t *testing.T) {
	f := newFixture(t)
	token, _ := f.enroller.Mint("phone")
	f.do("POST", "/api/enroll", `{"token":"`+token+`"}`, origin(canonicalOrigin), func(r *http.Request) {
		r.Header.Set("User-Agent", "Mozilla/5.0 (phone)")
	})
	for _, d := range f.store.Devices() {
		if d.Name == "phone" && d.UserAgent != "Mozilla/5.0 (phone)" {
			t.Fatalf("user agent = %q", d.UserAgent)
		}
	}
}

// A foreign page must not be able to spend a token it somehow learned, and --
// more importantly -- a refusal must not burn the link. The owner's own
// browser has to still be able to use it.
func TestRedeemingFromAForeignOriginIsRefusedAndDoesNotBurnTheLink(t *testing.T) {
	f := newFixture(t)
	token, _ := f.enroller.Mint("phone")

	for _, o := range []reqOpt{origin(siblingOrigin), func(*http.Request) {}} {
		if rec := f.do("POST", "/api/enroll", `{"token":"`+token+`"}`, o); rec.Code != http.StatusForbidden {
			t.Fatalf("refused redemption = %d, want 403", rec.Code)
		}
	}
	if rec := f.do("POST", "/api/enroll", `{"token":"`+token+`"}`, origin(canonicalOrigin)); rec.Code != http.StatusOK {
		t.Fatalf("the link stopped working after a refused attempt: %d", rec.Code)
	}
}

func TestAnAlreadyRedeemedLinkIsRefusedWithSomethingActionable(t *testing.T) {
	f := newFixture(t)
	token, _ := f.enroller.Mint("phone")
	f.do("POST", "/api/enroll", `{"token":"`+token+`"}`, origin(canonicalOrigin))

	rec := f.do("POST", "/api/enroll", `{"token":"`+token+`"}`, origin(canonicalOrigin))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("replayed token = %d, want 400", rec.Code)
	}
	var body struct{ Error string }
	decode(t, rec, &body)
	if !strings.Contains(body.Error, "enroll") {
		t.Fatalf("error = %q, want it to say what to do", body.Error)
	}
	if cookieFrom(rec, wireCookieName) != nil {
		t.Fatal("a refused redemption set a cookie")
	}
}

// The limiter exists so that a flood of guesses is one distinguishable thing in
// the log and one distinguishable status on the wire, rather than an
// indistinguishable stream of "invalid token".
func TestAFloodOfBadTokensBecomesA429(t *testing.T) {
	f := newFixture(t)
	var last int
	for range auth.MaxRedeemFailures + 1 {
		last = f.do("POST", "/api/enroll", `{"token":"nope"}`, origin(canonicalOrigin)).Code
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("status after %d bad tokens = %d, want 429", auth.MaxRedeemFailures+1, last)
	}
}

func TestRedeemRejectsGarbageWithoutCountingItAsACredential(t *testing.T) {
	f := newFixture(t)
	for _, body := range []string{`not json`, `{}`, `{"token":""}`} {
		if rec := f.do("POST", "/api/enroll", body, origin(canonicalOrigin)); rec.Code != http.StatusBadRequest {
			t.Errorf("POST /api/enroll %s = %d, want 400", body, rec.Code)
		}
	}
}

// ------------------------------------------------------------------ snapshot

func TestSnapshotServesTheCachedPoll(t *testing.T) {
	f := newFixture(t)
	f.snaps.set([]tmux.Row{{GroupKey: "work", PaneID: "%1", WindowName: "api", Command: "vim"}}, nil)

	rec := f.ok("GET", "/api/snapshot", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/snapshot = %d", rec.Code)
	}
	var body struct {
		Panes []tmux.Row `json:"panes"`
		Stale bool       `json:"stale"`
	}
	decode(t, rec, &body)
	if len(body.Panes) != 1 || body.Panes[0].PaneID != "%1" {
		t.Fatalf("panes = %+v", body.Panes)
	}
	if body.Stale {
		t.Fatal("a good snapshot was reported as stale")
	}
}

// Each browser keeps its own "this pane finished and I have not looked yet"
// memory, keyed ${serverStart}:${paneId}. Pane ids restart at %0 when the tmux
// server restarts, so without the generation on the response a stale seen["%3"]
// silently suppresses the badge on an unrelated new pane.
func TestSnapshotCarriesTheTmuxServerGeneration(t *testing.T) {
	f := newFixture(t)
	f.snaps.setServerStart("1789038099")
	f.snaps.set([]tmux.Row{{GroupKey: "work", PaneID: "%1"}}, nil)

	var body struct {
		ServerStart string `json:"serverStart"`
	}
	decode(t, f.ok("GET", "/api/snapshot", ""), &body)
	if body.ServerStart != "1789038099" {
		t.Fatalf("serverStart = %q, want the value the poller read this poll", body.ServerStart)
	}
}

// The design's rule: a transient tmux fault must not blank a sidebar that was
// correct 1.5s ago. tmux prints "server exited unexpectedly" for a few
// milliseconds while a server restarts, and a sidebar that emptied itself for
// one interval and refilled on the next would be worse than one that is stale.
func TestATransientTmuxFaultDoesNotBlankTheSidebar(t *testing.T) {
	f := newFixture(t)
	rows := []tmux.Row{{GroupKey: "work", PaneID: "%1"}}
	f.snaps.set(rows, errors.New("server exited unexpectedly"))

	rec := f.ok("GET", "/api/snapshot", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("stale snapshot = %d, want 200", rec.Code)
	}
	var body struct {
		Panes []tmux.Row `json:"panes"`
		Stale bool       `json:"stale"`
		Error string     `json:"error"`
	}
	decode(t, rec, &body)
	if len(body.Panes) != 1 {
		t.Fatalf("a failed poll blanked the sidebar: %+v", body.Panes)
	}
	if !body.Stale || body.Error == "" {
		t.Fatalf("staleness was served silently: %+v", body)
	}
}

// The one case where an empty array would be a lie rather than an answer.
func TestASnapshotThatNeverSucceededIsAnError(t *testing.T) {
	f := newFixture(t)
	f.snaps.set(nil, errors.New("error connecting to /tmp/x (Permission denied)"))
	if rec := f.ok("GET", "/api/snapshot", ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET /api/snapshot = %d, want 503", rec.Code)
	}
}

// No tmux server at all is not a fault: Snapshot reports it as an empty result
// with no error, and the frontend offers to create a session. The array must be
// [] rather than null, or a frontend that maps over it breaks on an ordinary
// cold start.
func TestNoTmuxServerIsAnEmptyArrayNotNull(t *testing.T) {
	f := newFixture(t)
	f.snaps.set(nil, nil)
	rec := f.ok("GET", "/api/snapshot", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/snapshot = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"panes":[]`) {
		t.Fatalf("body = %s, want an empty array", rec.Body.String())
	}
}

// ------------------------------------------------------------------- devices

// auth.Device carries the token hash. Serializing the store's type straight to
// the browser would publish credential material, which is why this API has its
// own view type -- and why the check is on the bytes rather than on the struct.
func TestTheDevicesListNeverCarriesCredentialMaterial(t *testing.T) {
	f := newFixture(t)
	rec := f.ok("GET", "/api/devices", "")
	body := rec.Body.String()
	if strings.Contains(body, "token_hash") || strings.Contains(body, "TokenHash") {
		t.Fatalf("the devices list carries a token hash: %s", body)
	}
	// The hash itself, in case a field is renamed rather than removed.
	raw, err := os.ReadFile(f.storePath)
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		Devices []struct {
			TokenHash string `json:"token_hash"`
		} `json:"devices"`
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	if len(state.Devices) == 0 || state.Devices[0].TokenHash == "" {
		t.Fatal("the store did not hold a token hash to look for")
	}
	if strings.Contains(body, state.Devices[0].TokenHash) {
		t.Fatalf("the devices list leaked the stored token hash: %s", body)
	}
	if !strings.Contains(body, f.deviceID) {
		t.Fatalf("the devices list did not mention the enrolled device: %s", body)
	}
}

func TestTheDevicesListMarksTheDeviceMakingTheRequest(t *testing.T) {
	f := newFixture(t)
	rec := f.ok("GET", "/api/devices", "")
	var body struct {
		Devices []struct {
			ID      string `json:"id"`
			Current bool   `json:"current"`
		} `json:"devices"`
	}
	decode(t, rec, &body)
	if len(body.Devices) != 1 || !body.Devices[0].Current {
		t.Fatalf("devices = %+v, want the caller marked current", body.Devices)
	}
}

func TestMintingFromTheBrowserProducesARedeemableLink(t *testing.T) {
	f := newFixture(t)
	rec := f.ok("POST", "/api/devices", `{"name":"phone"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/devices = %d (%s)", rec.Code, rec.Body.String())
	}
	var body struct{ Name, URL string }
	decode(t, rec, &body)

	u, err := url.Parse(body.URL)
	if err != nil {
		t.Fatal(err)
	}
	// The token belongs after '#', where it is never sent to a server: out of
	// access logs, out of Referer headers, and out of the link scanners
	// messaging apps run over a pasted URL.
	if u.RawQuery != "" || u.Fragment == "" || u.Path != "/enroll" {
		t.Fatalf("enrollment link = %q", body.URL)
	}
	if rec := f.do("POST", "/api/enroll", `{"token":"`+u.Fragment+`"}`, origin(canonicalOrigin)); rec.Code != http.StatusOK {
		t.Fatalf("the minted link was not redeemable: %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestMintingNeedsAName(t *testing.T) {
	f := newFixture(t)
	if rec := f.ok("POST", "/api/devices", `{"name":"  "}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("POST /api/devices with no name = %d, want 400", rec.Code)
	}
}

// Removing a device from the store only stops it authenticating the *next*
// request. A browser that already holds a WebSocket keeps its shell until it
// happens to disconnect, which for a terminal is days -- so revocation has to
// reach the live connection, or it is not revocation.
func TestRevokingADeviceSeversItsLiveConnections(t *testing.T) {
	f := newFixture(t)
	closed := make(chan struct{})
	remove, ok := f.registry.Add(f.deviceID, func() { close(closed) })
	if !ok {
		t.Fatal("registration was refused")
	}
	defer remove()

	if rec := f.ok("DELETE", "/api/devices/"+f.deviceID, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d (%s), want 204", rec.Code, rec.Body.String())
	}
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("revoking the device did not close its live connection")
	}
	// And the credential itself is gone.
	if rec := f.do("GET", "/api/snapshot", "", authed(f.token)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a revoked device still authenticates: %d", rec.Code)
	}
}

func TestRevokingAnUnknownDeviceIsANotFoundAndClosesNothing(t *testing.T) {
	f := newFixture(t)
	closed := make(chan struct{})
	remove, _ := f.registry.Add(f.deviceID, func() { close(closed) })
	defer remove()

	if rec := f.ok("DELETE", "/api/devices/nosuchdevice", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("DELETE unknown = %d, want 404", rec.Code)
	}
	select {
	case <-closed:
		t.Fatal("a failed revocation closed a live connection")
	case <-time.After(50 * time.Millisecond):
	}
}

// Signing out revokes this device rather than only clearing the cookie: a
// cookie-only logout leaves a live, fully privileged credential in the store
// that no UI can still identify. Clearing the cookie as well stops the browser
// presenting something already dead.
func TestSigningYourselfOutAlsoClearsTheCookie(t *testing.T) {
	f := newFixture(t)
	rec := f.ok("DELETE", "/api/devices/"+f.deviceID, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("self-revoke = %d", rec.Code)
	}
	c := cookieFrom(rec, wireCookieName)
	if c == nil || c.MaxAge >= 0 || c.Value != "" {
		t.Fatalf("signing out did not clear the cookie: %+v", c)
	}
}

func TestRevokingSomeoneElseLeavesYourOwnCookieAlone(t *testing.T) {
	f := newFixture(t)
	token, _ := f.enroller.Mint("phone")
	f.do("POST", "/api/enroll", `{"token":"`+token+`"}`, origin(canonicalOrigin))
	var other string
	for _, d := range f.store.Devices() {
		if d.ID != f.deviceID {
			other = d.ID
		}
	}

	rec := f.ok("DELETE", "/api/devices/"+other, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("revoke = %d", rec.Code)
	}
	if c := cookieFrom(rec, wireCookieName); c != nil {
		t.Fatalf("revoking another device cleared this one's cookie: %+v", c)
	}
}

// ------------------------------------------------------- the terminal socket

// The registry is how a revocation reaches a live shell, so the socket has to
// be in it. The closer registered here is the cancellation of the request
// context the terminal loops run on.
func TestATerminalConnectionIsRegisteredAndSeveredByRevocation(t *testing.T) {
	f := newFixture(t)

	done := make(chan int, 1)
	go func() {
		rec := f.do("GET", "/ws", "", authed(f.token), origin(canonicalOrigin))
		done <- rec.Code
	}()

	select {
	case <-f.terminalOpen:
	case <-time.After(2 * time.Second):
		t.Fatal("the terminal handler never ran")
	}

	f.registry.CloseDevice(f.deviceID)

	select {
	case <-f.terminalGone:
	case <-time.After(2 * time.Second):
		t.Fatal("revoking the device did not end the terminal connection")
	}
	<-done
}

// The window between the store dropping a device and the registry sweep is not
// theoretical -- opening a session forks tmux between those two moments -- so a
// connection that authenticated just before a revocation must be refused rather
// than opened and immediately torn down.
func TestATerminalForAnAlreadyRevokedDeviceIsRefused(t *testing.T) {
	f := newFixture(t)
	f.registry.CloseDevice(f.deviceID)

	if code := f.refused("GET", "/ws", "", authed(f.token), origin(canonicalOrigin)); code != http.StatusUnauthorized {
		t.Fatalf("GET /ws for a revoked device = %d, want 401", code)
	}
	if f.terminalCalls.Load() != 0 {
		t.Fatal("a revoked device reached the terminal handler")
	}
}

// A connection that ends normally must not stay in the registry: a long-lived
// daemon would otherwise hold one dead closer per browser tab ever opened, and
// a later revocation would run all of them.
func TestAClosedTerminalDeregistersItself(t *testing.T) {
	f := newFixture(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		r := httptest.NewRequest("GET", "/ws", nil)
		withCookie(r, f.token)
		r.Header.Set("Origin", canonicalOrigin)
		f.handler.ServeHTTP(httptest.NewRecorder(), r.WithContext(ctx))
		close(done)
	}()
	<-f.terminalOpen
	cancel()
	<-done

	// Nothing is left registered: CloseDevice would otherwise run the stale
	// closer, which is observable only by there being nothing to observe.
	closed := make(chan struct{})
	remove, ok := f.registry.Add(f.deviceID, func() { close(closed) })
	if !ok {
		t.Fatal("the device was left marked revoked")
	}
	remove()
	f.registry.CloseDevice(f.deviceID)
	select {
	case <-closed:
		t.Fatal("a deregistered connection was closed by a later revocation")
	case <-time.After(50 * time.Millisecond):
	}
}

// ------------------------------------------------------------ the admin socket

// Revoking over the unix socket is the important case: "I lost my laptop, ssh
// in and cut it off" is exactly when the browser cannot be used to do it. The
// admin mux cannot sever connections itself -- it lives in a package that knows
// nothing about them -- so the front layer wraps it.
func TestRevokingOverTheAdminSocketAlsoSeversLiveConnections(t *testing.T) {
	reg := front.NewRegistry()
	closed := make(chan struct{})
	remove, _ := reg.Add("dev-1", func() { close(closed) })
	defer remove()

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	h := front.SeverRevokedDevices(inner, reg)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("DELETE", "/devices/dev-1", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d", rec.Code)
	}
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("an admin revocation left the device's connection open")
	}
}

// A 404 revoked nothing, and must sever nothing.
func TestAFailedAdminRevocationSeversNothing(t *testing.T) {
	reg := front.NewRegistry()
	closed := make(chan struct{})
	remove, _ := reg.Add("dev-1", func() { close(closed) })
	defer remove()

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	front.SeverRevokedDevices(inner, reg).ServeHTTP(
		httptest.NewRecorder(), httptest.NewRequest("DELETE", "/devices/dev-1", nil))

	select {
	case <-closed:
		t.Fatal("a 404 severed a live connection")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestTheAdminWrapperPassesEverythingElseThrough(t *testing.T) {
	reg := front.NewRegistry()
	closed := make(chan struct{})
	remove, _ := reg.Add("dev-1", func() { close(closed) })
	defer remove()

	var seen string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Method + " " + r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	})
	h := front.SeverRevokedDevices(inner, reg)

	for _, r := range []*http.Request{
		httptest.NewRequest("GET", "/devices", nil),
		httptest.NewRequest("POST", "/enroll", nil),
		httptest.NewRequest("DELETE", "/devices", nil),
	} {
		h.ServeHTTP(httptest.NewRecorder(), r)
		if seen != r.Method+" "+r.URL.Path {
			t.Fatalf("%s %s did not reach the admin API", r.Method, r.URL.Path)
		}
	}
	select {
	case <-closed:
		t.Fatal("a request that revoked nothing severed a connection")
	case <-time.After(50 * time.Millisecond):
	}
}

// ----------------------------------------------------------------- the SPA

// A React app deep link must load the app, not a 404 -- the user bookmarked
// /session/work and the router resolves it in the browser.
func TestUnknownPathsFallBackToTheAppShell(t *testing.T) {
	f := newFixture(t)
	rec := f.ok("GET", "/session/work/pane/3", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "id=root") {
		t.Fatalf("deep link = %d %q", rec.Code, rec.Body.String())
	}
}

// ...but the fallback must not swallow the API. Answering an unknown /api/ route
// with the shell and a 200 makes a frontend calling a route this daemon does not
// have parse HTML as JSON and report something incoherent.
func TestTheFallbackDoesNotSwallowUnknownAPIRoutes(t *testing.T) {
	f := newFixture(t)
	for _, target := range []string{"/api/nope", "/api/", "/api/devices/extra/deep"} {
		rec := f.ok("GET", target, "")
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d (%s), want 404", target, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "id=root") {
			t.Errorf("GET %s answered with the app shell", target)
		}
	}
}

// A missing bundle must 404 rather than fall back: HTML served as application/
// javascript makes the browser report a syntax error in a file that is simply
// not there.
func TestAMissingAssetIs404NotTheAppShell(t *testing.T) {
	f := newFixture(t)
	rec := f.ok("GET", "/assets/index-deadbeef.js", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing asset = %d, want 404", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "id=root") {
		t.Fatal("a missing asset was answered with the app shell")
	}
}

func TestBuiltAssetsAreServedWithTheirOwnContentType(t *testing.T) {
	f := newFixture(t)
	rec := f.ok("GET", "/assets/index-abc123.js", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("asset = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Fatalf("content type = %q", ct)
	}
	if rec.Body.String() != "console.log('spa')" {
		t.Fatalf("body = %q", rec.Body.String())
	}
}

// Files Vite copies from public/ sit at the top level, and the fallback must
// not turn them into the shell.
func TestTopLevelStaticFilesAreServedRatherThanTheShell(t *testing.T) {
	f := newFixture(t)
	rec := f.ok("GET", "/favicon.svg", "")
	if rec.Code != http.StatusOK || rec.Body.String() != "<svg/>" {
		t.Fatalf("favicon = %d %q", rec.Code, rec.Body.String())
	}
}

// A binary built from a clean clone has no frontend in it. That is a runtime
// condition, not a build failure, so the daemon runs and says what is missing.
func TestABinaryBuiltWithoutTheFrontendSaysSo(t *testing.T) {
	f := newFixture(t, withAssets(fstest.MapFS{}))
	rec := f.ok("GET", "/", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / with no frontend = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "make front") {
		t.Fatalf("body = %q, want it to say how to build the frontend", rec.Body.String())
	}
	// The API still works: that is the point of not making it fatal.
	if rec := f.ok("GET", "/api/snapshot", ""); rec.Code != http.StatusOK {
		t.Fatalf("the API stopped working without a frontend: %d", rec.Code)
	}
}

// The embedded FS is the real one the daemon serves. It holds a built SPA only
// after `make front`, so this asserts the shape rather than the contents.
func TestTheEmbeddedDistIsUsable(t *testing.T) {
	if _, err := front.DistFS().Open("."); err != nil {
		t.Fatalf("the embedded dist is unusable: %v", err)
	}
}

// ------------------------------------------------------------- the dev switch

// Dev mode changes *which* origins are allowed, never whether the check
// happens. If the loopback origins were added to production's rather than
// replacing them, a page served over plain HTTP on the developer's machine
// would be able to drive the production daemon -- and a --dev daemon would
// accept requests claiming to come from the public host.
func TestDevOriginsReplaceProductionRatherThanBeingAddedToIt(t *testing.T) {
	dev, err := front.AllowedOrigins("tmux.example.com", true, 7000)
	if err != nil {
		t.Fatal(err)
	}
	f := newFixture(t, withOrigins(dev...))

	if rec := f.do("POST", "/api/devices", `{"name":"x"}`, authed(f.token), origin(canonicalOrigin)); rec.Code != http.StatusForbidden {
		t.Fatalf("a --dev daemon accepted the production origin: %d", rec.Code)
	}
	for _, o := range []string{"http://localhost:7000", "http://127.0.0.1:7000", "http://[::1]:7000"} {
		if rec := f.do("POST", "/api/devices", `{"name":"x"}`, authed(f.token), origin(o)); rec.Code != http.StatusOK {
			t.Fatalf("a --dev daemon refused %s: %d", o, rec.Code)
		}
	}
}

func TestProductionRefusesLoopbackOrigins(t *testing.T) {
	f := newFixture(t) // the production allowlist: exactly https://tmux.example.com
	for _, o := range []string{"http://localhost:7000", "http://127.0.0.1:7000", "http://tmux.example.com"} {
		if rec := f.do("POST", "/api/devices", `{"name":"x"}`, authed(f.token), origin(o)); rec.Code != http.StatusForbidden {
			t.Fatalf("production accepted %s: %d", o, rec.Code)
		}
	}
}

// ------------------------------------------------------------------ assembly

func TestNewHandlerRefusesToBuildSomethingIncomplete(t *testing.T) {
	store, _ := realStore(t)
	full := func() front.HandlerConfig {
		return front.HandlerConfig{
			Auth:      newAuth(t, store),
			Store:     store,
			Enroller:  auth.NewEnroller(store),
			Snapshots: &fakeSnapshots{},
			Registry:  front.NewRegistry(),
			BaseURL:   canonicalOrigin,
		}
	}
	cases := map[string]func(*front.HandlerConfig){
		"no auth":     func(c *front.HandlerConfig) { c.Auth = nil },
		"no store":    func(c *front.HandlerConfig) { c.Store = nil },
		"no enroller": func(c *front.HandlerConfig) { c.Enroller = nil },
		"no snapshot": func(c *front.HandlerConfig) { c.Snapshots = nil },
		// A missing registry would mean revocation that never reaches a live
		// shell -- the failure the whole design exists to prevent, and one
		// that nothing at runtime would report.
		"no registry": func(c *front.HandlerConfig) { c.Registry = nil },
		"no base url": func(c *front.HandlerConfig) { c.BaseURL = "" },
	}
	for name, break_ := range cases {
		cfg := full()
		break_(&cfg)
		if _, err := front.NewHandler(cfg); err == nil {
			t.Errorf("NewHandler with %s returned no error", name)
		}
	}
}

// A store that exists but will not parse is fatal. Starting empty would sign
// out every enrolled device without saying so, and the first enrollment
// afterwards would overwrite the only copy an operator could still recover.
func TestServeRefusesToStartOnADeviceStoreItCannotRead(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "devices.json")
	if err := os.WriteFile(state, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "admin.sock")

	err := front.Serve(context.Background(), front.Config{
		Host:        "tmux.example.com",
		StatePath:   state,
		AdminSocket: socket,
	})
	if err == nil {
		t.Fatal("Serve started with an unreadable device store")
	}
	if !strings.Contains(err.Error(), state) {
		t.Fatalf("error = %v, want it to name the file", err)
	}
	// Nothing was started, and nothing overwrote the evidence.
	if _, statErr := os.Stat(socket); statErr == nil {
		t.Fatal("Serve bound the admin socket before reading the store")
	}
	raw, _ := os.ReadFile(state)
	if string(raw) != "{not json" {
		t.Fatalf("the unreadable store was rewritten: %q", raw)
	}
}

func TestServeRefusesAnUnusableHost(t *testing.T) {
	for _, host := range []string{"", "https://tmux.example.com", "tmux.example.com:8443"} {
		err := front.Serve(context.Background(), front.Config{Host: host, StatePath: filepath.Join(t.TempDir(), "d.json")})
		if err == nil {
			t.Errorf("Serve accepted --host %q", host)
		}
	}
}

// ------------------------------------------------------------------- headers

// The app is a terminal: a page that can frame it can clickjack a shell.
func TestEveryResponseCarriesTheSecurityHeaders(t *testing.T) {
	f := newFixture(t)
	for _, rec := range []*httptest.ResponseRecorder{
		f.ok("GET", "/", ""),
		f.do("GET", "/enroll", ""),
		f.ok("GET", "/api/snapshot", ""),
		f.do("GET", "/api/snapshot", ""), // a 401 is a response too
	} {
		if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("X-Content-Type-Options = %q", got)
		}
		if got := rec.Header().Get("X-Frame-Options"); got != "DENY" {
			t.Errorf("X-Frame-Options = %q", got)
		}
		if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") {
			t.Errorf("CSP = %q", csp)
		}
	}
}

// Authenticated JSON must not be cached: the API is per-credential and the
// devices list changes under the user.
func TestApiResponsesAreNotCached(t *testing.T) {
	f := newFixture(t)
	for _, target := range []string{"/api/snapshot", "/api/devices"} {
		if got := f.ok("GET", target, "").Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s Cache-Control = %q, want no-store", target, got)
		}
	}
}

// -- fixture plumbing --------------------------------------------------------

// enrolledStore is realStore with the path kept: one test has to read the file
// to find the token hash the browser must never be shown.
func enrolledStore(t *testing.T) (*auth.Store, string, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "devices.json")
	s, err := auth.OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	token, err := s.AddDevice("laptop", "Go test")
	if err != nil {
		t.Fatalf("AddDevice: %v", err)
	}
	return s, token, path
}
