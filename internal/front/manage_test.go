package front_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/diegok/tmux-web/internal/front"
	"github.com/diegok/tmux-web/internal/tmux"
	"github.com/diegok/tmux-web/internal/tmux/testutil"
)

// The management endpoints, tested against a real tmux server on a throwaway
// socket. Nothing here is mocked: the point of most of these tests is what
// tmux actually answers -- which id shape it accepts, which message it emits
// for a stale target, and whether the object is still there afterwards -- and
// a fake would only ever confirm what its author believed.

// manageFixture is the ordinary route-table fixture with its manager pointed at
// a private tmux server the test can also drive directly.
type manageFixture struct {
	*fixture
	srv *testutil.Server
}

func newManageFixture(t *testing.T) *manageFixture {
	t.Helper()
	srv := testutil.NewServer(t)
	f := newFixture(t, withManager(tmux.NewClient(srv.Args())))
	return &manageFixture{fixture: f, srv: srv}
}

func withManager(m front.Manager) fixtureOpt {
	return func(cfg *front.HandlerConfig) { cfg.Manage = m }
}

// pathID encodes an id for a URL path segment the way the browser's
// encodeURIComponent does. It matters only for pane ids -- "%" is the one
// sigil that is not legal raw in a path -- but every id goes through it, so a
// caller cannot forget which kind it is holding.
func pathID(id string) string { return url.PathEscape(id) }

// seed starts a session on the private server and returns its session, window
// and pane ids.
func (f *manageFixture) seed(t *testing.T, name string) (sessionID, windowID, paneID string) {
	t.Helper()
	out := f.srv.Run(t, "new-session", "-d", "-s", name, "-P", "-F",
		"#{session_id}\t#{window_id}\t#{pane_id}")
	parts := strings.Split(out, "\t")
	if len(parts) != 3 {
		t.Fatalf("seed %q: unexpected new-session output %q", name, out)
	}
	return parts[0], parts[1], parts[2]
}

// ------------------------------------------------------- the middleware

// Every management route is a mutating verb behind the device cookie *and* the
// exact-Origin check. This is the test that fails when a route is registered
// without cfg.Auth.Protect -- which on this API means a page on a sibling
// subdomain can kill the owner's windows while carrying their cookie.
func TestEveryManagementRouteIsBehindTheCookieAndTheOrigin(t *testing.T) {
	// One fixture, seeded once, so the table can use ids that really exist:
	// a route that let an uncredentialed request through would then destroy
	// something, and the "still there" assertions below would notice.
	for _, c := range []struct {
		name   string
		method string
		target string
		body   string
	}{
		{"create session", "POST", "/api/sessions", `{"name":"made-by-a-stranger"}`},
		{"create window", "POST", "/api/windows", `{"session":"$0"}`},
		{"split pane", "POST", "/api/panes", `{"pane":"%0","direction":"right"}`},
		{"rename session", "PATCH", "/api/sessions/%240", `{"name":"renamed-by-a-stranger"}`},
		{"rename window", "PATCH", "/api/windows/%400", `{"name":"renamed-by-a-stranger"}`},
		{"label pane", "PATCH", "/api/panes/%250", `{"label":"set-by-a-stranger"}`},
		{"zoom pane", "POST", "/api/panes/%250/zoom", ``},
		{"kill session", "DELETE", "/api/sessions/%240", `{"confirm":true}`},
		{"kill window", "DELETE", "/api/windows/%400", `{"confirm":true}`},
		{"kill pane", "DELETE", "/api/panes/%250", `{"confirm":true}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newManageFixture(t)
			f.seed(t, "victim")

			// Neither credential. Origin is checked first, so this is a 403.
			if code := f.refused(c.method, c.target, c.body); code != http.StatusForbidden {
				t.Errorf("uncredentialed %s %s = %d, want 403", c.method, c.target, code)
			}
			// A page on a sibling subdomain: same site, different origin,
			// carrying the victim's cookie.
			if code := f.refused(c.method, c.target, c.body, authed(f.token), origin(siblingOrigin)); code != http.StatusForbidden {
				t.Errorf("%s %s from %s = %d, want 403", c.method, c.target, siblingOrigin, code)
			}
			// The right origin without a cookie.
			if code := f.refused(c.method, c.target, c.body, origin(canonicalOrigin)); code != http.StatusUnauthorized {
				t.Errorf("unauthenticated %s %s = %d, want 401", c.method, c.target, code)
			}

			// Nothing above reached tmux: the session, its window and its
			// pane are all still there, and no stranger's session was made.
			if out := f.srv.Run(t, "list-sessions", "-F", "#{session_name}"); out != "victim" {
				t.Errorf("after three refused requests the sessions are %q, want %q", out, "victim")
			}
			if n := len(strings.Split(f.srv.Run(t, "list-panes", "-a", "-F", "#{pane_id}"), "\n")); n != 1 {
				t.Errorf("after three refused requests there are %d panes, want 1", n)
			}

			// And the properly credentialed request does reach a handler --
			// otherwise the three refusals above would also pass against a
			// route that does not exist at all.
			if rec := f.ok(c.method, c.target, c.body); rec.Code == http.StatusForbidden ||
				rec.Code == http.StatusUnauthorized || rec.Code == http.StatusNotFound ||
				rec.Code == http.StatusMethodNotAllowed {
				t.Errorf("credentialed %s %s = %d (%s), want it to reach the handler",
					c.method, c.target, rec.Code, rec.Body.String())
			}
		})
	}
}

// ------------------------------------------------------- percent-encoding

// A pane id contains "%", which is not legal raw in a URL path. The browser
// must send encodeURIComponent("%3") -> "%253", and r.PathValue decodes it
// back. Both halves are pinned as literals here, because Task 12 writes the
// frontend against exactly these strings.
func TestPaneIdsAreAddressedPercentEncoded(t *testing.T) {
	f := newManageFixture(t)
	_, _, pane := f.seed(t, "encoded")
	if pane != "%0" {
		t.Fatalf("expected the first pane of a fresh server to be %%0, got %q", pane)
	}

	rec := f.ok("PATCH", "/api/panes/%250", `{"label":"decoded"}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("PATCH /api/panes/%%250 = %d (%s), want 204", rec.Code, rec.Body.String())
	}
	// The label landed on %0, so the handler decoded "%250" to "%0" rather
	// than passing the escaped text to tmux.
	if got := f.srv.Run(t, "show", "-p", "-t", "%0", "-qv", tmux.LabelOption); got != "decoded" {
		t.Errorf("label on %%0 is %q, want %q", got, "decoded")
	}
}

// The un-encoded form never reaches this daemon's mux at all: "%3" is an
// invalid percent-escape, so net/http answers 400 while parsing the request
// line and no handler is ever chosen. That is the whole reason the frontend
// has to encode, so it is asserted against the real route table over a real
// HTTP conversation rather than against the parser in isolation -- and with
// full credentials and a confirmation, so that the only thing standing between
// this request and a dead pane is the encoding rule.
func TestAnUnencodedPaneIdIsRejectedBeforeRouting(t *testing.T) {
	f := newManageFixture(t)
	_, _, pane := f.seed(t, "survivor")

	resp := f.wire(t, "DELETE /api/panes/"+pane+" HTTP/1.1")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("DELETE /api/panes/%s = %d, want 400", pane, resp.StatusCode)
	}
	if _, err := f.srv.TryRun("display-message", "-p", "-t", pane, "#{pane_id}"); err != nil {
		t.Fatalf("the un-encoded request killed %s: %v", pane, err)
	}

	// The encoded spelling of the same id, over the same wire, does reach the
	// handler -- so the 400 above is the escape and not the credentials.
	if resp := f.wire(t, "DELETE /api/panes/"+url.PathEscape(pane)+" HTTP/1.1"); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE /api/panes/%s = %d, want 204", url.PathEscape(pane), resp.StatusCode)
	}
	if _, err := f.srv.TryRun("display-message", "-p", "-t", pane, "#{pane_id}"); err == nil {
		t.Errorf("the encoded request did not kill %s", pane)
	}
}

// wire serves one credentialed, confirmed request over an in-memory
// connection, so that the request *line* is what is under test. httptest
// cannot express this: httptest.NewRequest parses the target with url.Parse
// and panics on an invalid escape, which is the very thing being asserted.
func (f *manageFixture) wire(t *testing.T, requestLine string) *http.Response {
	t.Helper()
	server, client := net.Pipe()
	go (&http.Server{Handler: f.handler}).Serve(&oneConn{c: server})
	t.Cleanup(func() { client.Close() })

	// A deadline on both halves: a bug that left the server waiting for more
	// of the request would otherwise hang the test binary rather than fail.
	if err := client.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}
	const body = `{"confirm":true}`
	req := requestLine + "\r\n" +
		"Host: tmux.example.com\r\n" +
		"Origin: " + canonicalOrigin + "\r\n" +
		"Cookie: " + front.DeviceCookieName + "=" + f.token + "\r\n" +
		"Content-Type: application/json\r\n" +
		"Content-Length: " + strconv.Itoa(len(body)) + "\r\n" +
		"Connection: close\r\n\r\n" + body
	if _, err := io.WriteString(client, req); err != nil {
		t.Fatalf("writing %q: %v", requestLine, err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(client), nil)
	if err != nil {
		t.Fatalf("reading the response to %q: %v", requestLine, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// oneConn is a listener that hands over a single in-memory connection. Nothing
// binds a port: a test that opened a real socket would be a test that can fail
// because of what else is running on the machine.
type oneConn struct {
	c    net.Conn
	done bool
}

func (l *oneConn) Addr() net.Addr { return dummyAddr{} }
func (l *oneConn) Close() error   { return nil }
func (l *oneConn) Accept() (net.Conn, error) {
	if l.done {
		return nil, errors.New("no more connections")
	}
	l.done = true
	return l.c, nil
}

type dummyAddr struct{}

func (dummyAddr) Network() string { return "pipe" }
func (dummyAddr) String() string  { return "pipe" }

// ------------------------------------------------------- the create verbs

func TestCreateVerbsReportTheIdTheyMade(t *testing.T) {
	f := newManageFixture(t)
	session, _, pane := f.seed(t, "base")

	var made struct{ ID string }

	rec := f.ok("POST", "/api/sessions", `{"name":"fresh"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /api/sessions = %d (%s), want 201", rec.Code, rec.Body.String())
	}
	decode(t, rec, &made)
	if err := tmux.ValidateSessionID(made.ID); err != nil {
		t.Errorf("POST /api/sessions returned %q: %v", made.ID, err)
	}
	if got := f.srv.Run(t, "display-message", "-p", "-t", made.ID, "#{session_name}"); got != "fresh" {
		t.Errorf("session %s is named %q, want %q", made.ID, got, "fresh")
	}

	rec = f.ok("POST", "/api/windows", `{"session":"`+session+`","name":"api"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /api/windows = %d (%s), want 201", rec.Code, rec.Body.String())
	}
	decode(t, rec, &made)
	if err := tmux.ValidateWindowID(made.ID); err != nil {
		t.Errorf("POST /api/windows returned %q: %v", made.ID, err)
	}
	if got := f.srv.Run(t, "display-message", "-p", "-t", made.ID, "#{window_name}"); got != "api" {
		t.Errorf("window %s is named %q, want %q", made.ID, got, "api")
	}

	rec = f.ok("POST", "/api/panes", `{"pane":"`+pane+`","direction":"right"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /api/panes = %d (%s), want 201", rec.Code, rec.Body.String())
	}
	decode(t, rec, &made)
	if err := tmux.ValidatePaneID(made.ID); err != nil {
		t.Errorf("POST /api/panes returned %q: %v", made.ID, err)
	}
	if made.ID == pane {
		t.Errorf("split returned the id of the pane it split, %q", pane)
	}
	if got := f.srv.Run(t, "display-message", "-p", "-t", made.ID, "#{window_id}"); got != f.srv.Run(t, "display-message", "-p", "-t", pane, "#{window_id}") {
		t.Errorf("the new pane %s is not in the window it was split from", made.ID)
	}
}

// ------------------------------------------------------- rename, label, zoom

func TestRenameLabelAndZoomActOnTheAddressedObject(t *testing.T) {
	f := newManageFixture(t)
	session, window, pane := f.seed(t, "before")

	if rec := f.ok("PATCH", "/api/sessions/"+pathID(session), `{"name":"after"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("PATCH session = %d (%s), want 204", rec.Code, rec.Body.String())
	}
	if got := f.srv.Run(t, "display-message", "-p", "-t", session, "#{session_name}"); got != "after" {
		t.Errorf("session name is %q, want %q", got, "after")
	}

	if rec := f.ok("PATCH", "/api/windows/"+pathID(window), `{"name":"named"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("PATCH window = %d (%s), want 204", rec.Code, rec.Body.String())
	}
	if got := f.srv.Run(t, "display-message", "-p", "-t", window, "#{window_name}"); got != "named" {
		t.Errorf("window name is %q, want %q", got, "named")
	}

	if rec := f.ok("PATCH", "/api/panes/"+pathID(pane), `{"label":"agent"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("PATCH pane = %d (%s), want 204", rec.Code, rec.Body.String())
	}
	if got := f.srv.Run(t, "show", "-p", "-t", pane, "-qv", tmux.LabelOption); got != "agent" {
		t.Errorf("label is %q, want %q", got, "agent")
	}
	// An empty label clears it, which is what the design's `"" clears` means.
	if rec := f.ok("PATCH", "/api/panes/"+pathID(pane), `{"label":""}`); rec.Code != http.StatusNoContent {
		t.Fatalf("PATCH pane with an empty label = %d (%s), want 204", rec.Code, rec.Body.String())
	}
	if got := f.srv.Run(t, "show", "-p", "-t", pane, "-qv", tmux.LabelOption); got != "" {
		t.Errorf("label after clearing is %q, want empty", got)
	}

	// Zoom needs a second pane: measured on tmux 3.7b, `resize-pane -Z` on a
	// window holding one pane exits 0 and does nothing, so a single-pane
	// fixture would report "not zoomed" no matter what the handler did.
	f.srv.Run(t, "split-window", "-t", pane)

	// Zoom is a toggle, so it is asserted in both directions: a handler that
	// zoomed unconditionally would pass a one-way test.
	if rec := f.ok("POST", "/api/panes/"+pathID(pane)+"/zoom", ``); rec.Code != http.StatusNoContent {
		t.Fatalf("POST zoom = %d (%s), want 204", rec.Code, rec.Body.String())
	}
	if got := f.srv.Run(t, "display-message", "-p", "-t", pane, "#{window_zoomed_flag}"); got != "1" {
		t.Errorf("window_zoomed_flag after one zoom is %q, want 1", got)
	}
	if rec := f.ok("POST", "/api/panes/"+pathID(pane)+"/zoom", ``); rec.Code != http.StatusNoContent {
		t.Fatalf("POST unzoom = %d (%s), want 204", rec.Code, rec.Body.String())
	}
	if got := f.srv.Run(t, "display-message", "-p", "-t", pane, "#{window_zoomed_flag}"); got != "0" {
		t.Errorf("window_zoomed_flag after two zooms is %q, want 0", got)
	}
}

// ------------------------------------------------------- confirm on delete

// Every DELETE requires {"confirm": true}. This mirrors the UI's two-step
// dialog rather than replacing it: it is an anti-footgun, not a security
// control -- the Origin check above is the boundary.
//
// The status code is the smaller half of each assertion. What matters is that
// the object is still alive afterwards, because a handler that answered 400
// and killed anyway would satisfy a code-only test.
func TestEveryDeleteRefusesWithoutConfirmation(t *testing.T) {
	// Each of these is a body that is *not* a confirmation. The last two are
	// what a confirm check written as "did the body parse" or "is the field
	// present" would wave through.
	refusals := map[string]string{
		"no body at all":    ``,
		"an empty object":   `{}`,
		"confirm false":     `{"confirm":false}`,
		"confirm as a word": `{"confirm":"true"}`,
		"some other field":  `{"confirmed":true}`,
	}
	// The positive control, carrying an unrelated extra field so that the
	// check is on the value of confirm and not on the shape of the body.
	const confirmed = `{"confirm":true,"and":"an extra field"}`

	for _, kind := range []string{"sessions", "windows", "panes"} {
		t.Run(kind, func(t *testing.T) {
			for name, body := range refusals {
				t.Run(name, func(t *testing.T) {
					f := newManageFixture(t)
					session, window, pane := f.seed(t, "alive")
					id := map[string]string{"sessions": session, "windows": window, "panes": pane}[kind]

					rec := f.ok("DELETE", "/api/"+kind+"/"+pathID(id), body)
					if rec.Code != http.StatusBadRequest {
						t.Errorf("DELETE /api/%s with %s = %d (%s), want 400",
							kind, name, rec.Code, rec.Body.String())
					}
					// The real assertion: nothing died.
					if _, err := f.srv.TryRun("display-message", "-p", "-t", id, "#{pane_id}"); err != nil {
						t.Errorf("DELETE /api/%s with %s destroyed %s: %v", kind, name, id, err)
					}
				})
			}

			// A confirmed delete does go through, so the refusals above are
			// not just a handler that refuses everything.
			f := newManageFixture(t)
			session, window, pane := f.seed(t, "doomed")
			id := map[string]string{"sessions": session, "windows": window, "panes": pane}[kind]
			if rec := f.ok("DELETE", "/api/"+kind+"/"+pathID(id), confirmed); rec.Code != http.StatusNoContent {
				t.Fatalf("confirmed DELETE /api/%s = %d (%s), want 204", kind, rec.Code, rec.Body.String())
			}
			if _, err := f.srv.TryRun("display-message", "-p", "-t", id, "#{pane_id}"); err == nil {
				t.Errorf("confirmed DELETE /api/%s left %s alive", kind, id)
			}
		})
	}
}

// ------------------------------------------------------- failures

// A stale id is the ordinary case, not a server fault: the sidebar is up to
// one poll interval out of date, so the browser routinely addresses something
// that has just died. It must come back as tmux's own words -- which the toast
// shows -- with a 4xx, never a 500.
//
// The expected messages are per verb because tmux's wording is not uniform:
// kill-pane says "can't find pane" where set-option says "no such pane". Each
// string here was measured against the command that emits it.
func TestAStaleIdComesBackAsTmuxsOwnMessage(t *testing.T) {
	for _, c := range []struct {
		name   string
		method string
		target string
		body   string
		want   string
	}{
		{"new window", "POST", "/api/windows", `{"session":"$99"}`, "can't find session: $99"},
		{"split pane", "POST", "/api/panes", `{"pane":"%99","direction":"down"}`, "can't find pane: %99"},
		{"rename session", "PATCH", "/api/sessions/%2499", `{"name":"x"}`, "can't find session: $99"},
		{"rename window", "PATCH", "/api/windows/%4099", `{"name":"x"}`, "can't find window: @99"},
		{"label pane", "PATCH", "/api/panes/%2599", `{"label":"x"}`, "no such pane: %99"},
		{"zoom pane", "POST", "/api/panes/%2599/zoom", ``, "can't find pane: %99"},
		{"kill session", "DELETE", "/api/sessions/%2499", `{"confirm":true}`, "can't find session: $99"},
		{"kill window", "DELETE", "/api/windows/%4099", `{"confirm":true}`, "can't find window: @99"},
		{"kill pane", "DELETE", "/api/panes/%2599", `{"confirm":true}`, "can't find pane: %99"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newManageFixture(t)
			f.seed(t, "present")

			rec := f.ok(c.method, c.target, c.body)
			if rec.Code/100 != 4 {
				t.Fatalf("%s %s = %d (%s), want a 4xx", c.method, c.target, rec.Code, rec.Body.String())
			}
			var e struct{ Error string }
			decode(t, rec, &e)
			if !strings.Contains(e.Error, c.want) {
				t.Errorf("%s %s answered %q, want it to contain tmux's %q",
					c.method, c.target, e.Error, c.want)
			}
		})
	}
}

// The wrong *kind* of id is worse than a stale one, because tmux resolves it
// rather than refusing it: `kill-window -t %0` kills the window containing
// pane 0, successfully. The verbs validate the sigil per kind; this is the
// endpoint-level proof that the browser cannot reach past that.
func TestAnIdOfTheWrongKindIsRefused(t *testing.T) {
	f := newManageFixture(t)
	session, window, pane := f.seed(t, "intact")

	// A pane id where a window id belongs.
	rec := f.ok("DELETE", "/api/windows/"+pathID(pane), `{"confirm":true}`)
	if rec.Code/100 != 4 {
		t.Errorf("DELETE /api/windows/%s = %d (%s), want a 4xx", pane, rec.Code, rec.Body.String())
	}
	if _, err := f.srv.TryRun("display-message", "-p", "-t", window, "#{window_id}"); err != nil {
		t.Errorf("a pane id sent to the window route killed window %s: %v", window, err)
	}

	// An empty id is not a route this mux has, so it must not fall through to
	// a handler that would let tmux read it as "whatever is current".
	if rec := f.ok("DELETE", "/api/sessions/", `{"confirm":true}`); rec.Code != http.StatusNotFound {
		t.Errorf("DELETE /api/sessions/ = %d (%s), want 404", rec.Code, rec.Body.String())
	}
	if _, err := f.srv.TryRun("display-message", "-p", "-t", session, "#{session_id}"); err != nil {
		t.Errorf("an empty id killed session %s: %v", session, err)
	}
}

// The daemon refuses to kill its own sessions on the direct path, and the
// refusal has to survive the trip through HTTP: it is the app's, and killing
// one drops a live tab's socket for no reason the owner could understand.
func TestKillingAnAppSessionIsRefused(t *testing.T) {
	f := newManageFixture(t)
	session, _, _ := f.seed(t, "app-owned")
	f.srv.Run(t, "set", "-t", session, tmux.AppOption, "1")

	rec := f.ok("DELETE", "/api/sessions/"+pathID(session), `{"confirm":true}`)
	if rec.Code/100 != 4 {
		t.Fatalf("DELETE of an app session = %d (%s), want a 4xx", rec.Code, rec.Body.String())
	}
	if _, err := f.srv.TryRun("display-message", "-p", "-t", session, "#{session_id}"); err != nil {
		t.Errorf("the app session %s was killed anyway: %v", session, err)
	}
}

// A name the browser is not allowed to use is refused before it reaches a tmux
// command line. The validator is Task 7's and its own tests cover the rules;
// what is checked here is that the endpoint asks it at all.
func TestABadNameIsRefusedRatherThanCreated(t *testing.T) {
	f := newManageFixture(t)

	// A leading "-" is read as a flag in a positional slot, and `new-session
	// -s -x` succeeds -- creating a session that can never be renamed.
	rec := f.ok("POST", "/api/sessions", `{"name":"-x"}`)
	if rec.Code/100 != 4 {
		t.Fatalf("POST /api/sessions with a name of %q = %d (%s), want a 4xx", "-x", rec.Code, rec.Body.String())
	}
	if out, err := f.srv.TryRun("list-sessions", "-F", "#{session_name}"); err == nil && out != "" {
		t.Errorf("a refused name still created sessions: %q", out)
	}
}

// A body that is not JSON, or is missing, is the browser's mistake and not the
// server's.
func TestAMalformedBodyIsABadRequest(t *testing.T) {
	f := newManageFixture(t)
	f.seed(t, "here")

	for _, c := range []struct{ method, target, body string }{
		{"POST", "/api/sessions", `not json`},
		{"POST", "/api/sessions", ``},
		{"POST", "/api/windows", `{`},
		{"POST", "/api/panes", `{"pane":"%0","direction":"sideways"}`},
		{"PATCH", "/api/panes/%250", `[]`},
	} {
		rec := f.ok(c.method, c.target, c.body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s %s with body %q = %d (%s), want 400",
				c.method, c.target, c.body, rec.Code, rec.Body.String())
		}
	}
}

// ------------------------------------------------------- the optional inputs

// The direction actually reaches tmux, and means what the browser thinks it
// means. Without this, a handler that passed a constant would create a pane
// every time and look correct.
//
// tmux's own flags are the other way round -- -h splits the *line*
// horizontally and puts the new pane on the right -- which is exactly why the
// mapping is worth an assertion rather than a reading.
func TestSplitDirectionDecidesWhereTheNewPaneLands(t *testing.T) {
	for _, c := range []struct{ direction, want string }{
		{"right", "left"}, // the new pane starts part-way across the window
		{"down", "top"},   // and part-way down it
	} {
		t.Run(c.direction, func(t *testing.T) {
			f := newManageFixture(t)
			_, _, pane := f.seed(t, "split")

			rec := f.ok("POST", "/api/panes", `{"pane":"`+pane+`","direction":"`+c.direction+`"}`)
			if rec.Code != http.StatusCreated {
				t.Fatalf("split %s = %d (%s), want 201", c.direction, rec.Code, rec.Body.String())
			}
			var made struct{ ID string }
			decode(t, rec, &made)

			moved := f.srv.Run(t, "display-message", "-p", "-t", made.ID, "#{pane_"+c.want+"}")
			if moved == "0" {
				t.Errorf("split %q put the new pane at pane_%s=0; it went the other way",
					c.direction, c.want)
			}
			// And the other axis did not move, so "right" is not just "any
			// split at all".
			other := map[string]string{"left": "top", "top": "left"}[c.want]
			if got := f.srv.Run(t, "display-message", "-p", "-t", made.ID, "#{pane_"+other+"}"); got != "0" {
				t.Errorf("split %q moved the new pane on both axes (pane_%s=%s)", c.direction, other, got)
			}
		})
	}
}

// The optional path on session create is the one path that crosses the wire,
// typed by the owner in the dialog. It has to reach tmux -- and a directory
// that does not exist has to be reported, because `new-session -c /gone` exits
// 0 and starts the shell in $HOME instead.
func TestTheOptionalPathOnSessionCreateIsUsedAndChecked(t *testing.T) {
	f := newManageFixture(t)

	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	rec := f.ok("POST", "/api/sessions", `{"name":"rooted","path":`+quote(dir)+`}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /api/sessions with a path = %d (%s), want 201", rec.Code, rec.Body.String())
	}
	var made struct{ ID string }
	decode(t, rec, &made)
	if got := f.srv.Run(t, "display-message", "-p", "-t", made.ID, "#{pane_current_path}"); got != dir {
		t.Errorf("the session opened in %q, want %q", got, dir)
	}

	// A path that is not there is reported rather than silently ignored.
	rec = f.ok("POST", "/api/sessions", `{"name":"lost","path":"/no/such/directory/here"}`)
	if rec.Code/100 != 4 {
		t.Fatalf("POST /api/sessions with a missing path = %d (%s), want a 4xx", rec.Code, rec.Body.String())
	}
	if out := f.srv.Run(t, "list-sessions", "-F", "#{session_name}"); strings.Contains(out, "lost") {
		t.Errorf("a session was created for a path that does not exist: %q", out)
	}
}

// fromPane is a pane *id*, and the daemon resolves that pane's working
// directory itself: paths never cross the wire in this direction, which is
// what keeps them out of the snapshot. Without the id reaching NewWindow the
// new window would open in the daemon's own directory.
func TestNewWindowInheritsTheDirectoryOfThePaneItCameFrom(t *testing.T) {
	f := newManageFixture(t)

	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	out := f.srv.Run(t, "new-session", "-d", "-s", "rooted", "-c", dir, "-P", "-F",
		"#{session_id}\t#{pane_id}")
	session, pane, _ := strings.Cut(out, "\t")

	rec := f.ok("POST", "/api/windows", `{"session":"`+session+`","fromPane":"`+pane+`"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /api/windows with fromPane = %d (%s), want 201", rec.Code, rec.Body.String())
	}
	var made struct{ ID string }
	decode(t, rec, &made)
	if got := f.srv.Run(t, "display-message", "-p", "-t", made.ID, "#{pane_current_path}"); got != dir {
		t.Errorf("the new window opened in %q, want the source pane's %q", got, dir)
	}
}

// quote renders a string as a JSON string, for building a request body around
// a path a test does not choose.
func quote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// ------------------------------------------------------- the deadline
//
// A management verb runs a tmux command on the request goroutine. Against a
// wedged server that command can take seconds -- registry.go says so out loud
// about the same server -- and with nothing bounding it the browser's fetch is
// what eventually gives up, leaving the daemon still holding a request nobody
// is waiting for and the owner with a dialog that never closes.

// deadlineManager is a Manager that records the deadline every verb was given,
// and -- when told to block -- never answers, the way a wedged tmux does not.
type deadlineManager struct {
	mu    sync.Mutex
	calls []managerCall
	block bool
}

type managerCall struct {
	verb string
	// bounded is whether the verb was given a deadline at all, and left is how
	// long it had. Both, because the failure being pinned here is the absence
	// of one: a zero `left` from a missing deadline would otherwise be
	// indistinguishable from one that had already expired.
	bounded bool
	left    time.Duration
}

func (m *deadlineManager) seen(ctx context.Context, verb string) error {
	deadline, ok := ctx.Deadline()
	m.mu.Lock()
	m.calls = append(m.calls, managerCall{verb: verb, bounded: ok, left: time.Until(deadline)})
	blocking := m.block
	m.mu.Unlock()
	if blocking {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

func (m *deadlineManager) seenCalls() []managerCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]managerCall{}, m.calls...)
}

func (m *deadlineManager) NewSession(ctx context.Context, _, _ string) (string, error) {
	return "$9", m.seen(ctx, "create session")
}

func (m *deadlineManager) NewWindow(ctx context.Context, _, _, _ string) (string, error) {
	return "@9", m.seen(ctx, "create window")
}

func (m *deadlineManager) SplitPane(ctx context.Context, _, _ string) (string, error) {
	return "%9", m.seen(ctx, "split pane")
}

func (m *deadlineManager) RenameSession(ctx context.Context, _, _ string) error {
	return m.seen(ctx, "rename session")
}

func (m *deadlineManager) RenameWindow(ctx context.Context, _, _ string) error {
	return m.seen(ctx, "rename window")
}

func (m *deadlineManager) SetLabel(ctx context.Context, _, _ string) error {
	return m.seen(ctx, "label pane")
}

func (m *deadlineManager) ToggleZoom(ctx context.Context, _ string) error {
	return m.seen(ctx, "zoom pane")
}

func (m *deadlineManager) KillSessionID(ctx context.Context, _ string) error {
	return m.seen(ctx, "kill session")
}

func (m *deadlineManager) KillWindow(ctx context.Context, _ string) error {
	return m.seen(ctx, "kill window")
}

func (m *deadlineManager) KillPane(ctx context.Context, _ string) error {
	return m.seen(ctx, "kill pane")
}

// manageRoutes is every management route, in the shape a request takes.
var manageRoutes = []struct {
	verb   string
	method string
	target string
	body   string
}{
	{"create session", "POST", "/api/sessions", `{"name":"work"}`},
	{"create window", "POST", "/api/windows", `{"session":"$0"}`},
	{"split pane", "POST", "/api/panes", `{"pane":"%0","direction":"right"}`},
	{"rename session", "PATCH", "/api/sessions/%240", `{"name":"renamed"}`},
	{"rename window", "PATCH", "/api/windows/%400", `{"name":"renamed"}`},
	{"label pane", "PATCH", "/api/panes/%250", `{"label":"a label"}`},
	{"zoom pane", "POST", "/api/panes/%250/zoom", ``},
	{"kill session", "DELETE", "/api/sessions/%240", `{"confirm":true}`},
	{"kill window", "DELETE", "/api/windows/%400", `{"confirm":true}`},
	{"kill pane", "DELETE", "/api/panes/%250", `{"confirm":true}`},
}

// Every one of them, because the bound is per handler: the one route that
// forgets it is the one that hangs, and it would hang exactly as rarely as
// tmux wedges.
func TestEveryManagementVerbRunsUnderADeadline(t *testing.T) {
	for _, c := range manageRoutes {
		t.Run(c.verb, func(t *testing.T) {
			m := &deadlineManager{}
			f := newFixture(t, withManager(m))

			rec := f.ok(c.method, c.target, c.body)
			if rec.Code >= 400 {
				t.Fatalf("%s %s = %d (%s)", c.method, c.target, rec.Code, rec.Body.String())
			}

			calls := m.seenCalls()
			if len(calls) != 1 {
				t.Fatalf("%s reached the manager %d times, want 1: %+v", c.verb, len(calls), calls)
			}
			if !calls[0].bounded {
				t.Fatalf("%s ran on the bare request context: a wedged tmux would hold this request "+
					"until the browser gave up on it", c.verb)
			}
			// 5s, minus however long the request took to get here. A tighter
			// window than the poll's because the owner is waiting on this one.
			if calls[0].left < 4*time.Second || calls[0].left > 5*time.Second {
				t.Errorf("%s had %v left on its deadline, want ~5s", c.verb, calls[0].left)
			}
		})
	}
}

// What a wedged tmux looks like to the owner: an answer, saying what happened,
// instead of a dialog that spins until the browser gives up.
//
// 504 rather than the 400 every other management failure gets. That rule is
// about not guessing at tmux's message text -- see writeManageError -- and this
// case needs no guess: the daemon's own deadline expired, which it knows
// without reading a word tmux said. 400 would tell the owner their request was
// wrong, and it was not.
func TestAWedgedManagementVerbSaysSoInsteadOfHanging(t *testing.T) {
	defer front.SetManageTimeout(60 * time.Millisecond)()

	m := &deadlineManager{block: true}
	f := newFixture(t, withManager(m))

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- f.ok("DELETE", "/api/panes/%250", `{"confirm":true}`) }()

	var rec *httptest.ResponseRecorder
	select {
	case rec = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a management request against a tmux that never answered never came back")
	}

	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("a timed-out kill = %d (%s), want 504", rec.Code, rec.Body.String())
	}
	var body struct {
		Error string `json:"error"`
	}
	decode(t, rec, &body)
	if !strings.Contains(body.Error, "tmux did not answer within 60ms") {
		t.Errorf("the owner is told %q; it has to say tmux did not answer, and for how long", body.Error)
	}
}

// The ordinary refusal must not be swept into the timeout answer: a stale id is
// the common case, it is the owner's own request that was wrong, and tmux's
// sentence about it is what the toast prints.
func TestARefusedManagementVerbIsStillA400(t *testing.T) {
	f := newManageFixture(t)
	f.seed(t, "victim")

	rec := f.ok("DELETE", "/api/panes/"+pathID("%99"), `{"confirm":true}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("killing a stale pane id = %d (%s), want 400", rec.Code, rec.Body.String())
	}
	var body struct {
		Error string `json:"error"`
	}
	decode(t, rec, &body)
	if strings.Contains(body.Error, "did not answer") || !strings.Contains(body.Error, "%99") {
		t.Errorf("the owner is told %q, want tmux's own words about the id they named", body.Error)
	}
}

// ------------------------------------------------------- the forced poll
//
// "Nothing is retried ... and the sidebar refreshes immediately -- so a row
// that no longer exists disappears along with the error rather than waiting out
// a poll" (web/src/lib/manage.ts), and the v2 design says the same. Only half
// of it was true: the browser did re-fetch /api/snapshot, and /api/snapshot
// served Poller.Latest(), which nothing re-polled. The re-fetch was answered
// from a tree read up to an interval BEFORE the verb ran.

// serveInto is f.ok with the recorder supplied by the caller, so a test can
// watch the response being built rather than only read it afterwards.
func (f *fixture) serveInto(rec *httptest.ResponseRecorder, method, target, body string) {
	f.t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	authed(f.token)(r)
	origin(canonicalOrigin)(r)
	f.handler.ServeHTTP(rec, r)
}

// Every verb, and BEFORE it answers -- both halves, over the whole route table.
//
// Every verb, because a poll forced by nine handlers out of ten is a promise
// broken by exactly the one nobody thought about. Before it answers, because
// the browser re-fetches the moment the response lands: a poll forced
// afterwards is the same staleness with a smaller window and no way to see it.
//
// The sharp case is the split. A killed row lingering for an interval is mild;
// a create returns an id the very next snapshot does not contain, and a tab
// pointed at a pane the daemon has never heard of is worse than a stale row.
func TestEveryManagementVerbForcesAPollBeforeItAnswers(t *testing.T) {
	for _, c := range manageRoutes {
		t.Run(c.verb, func(t *testing.T) {
			m := &deadlineManager{}
			f := newFixture(t, withManager(m))

			rec := httptest.NewRecorder()
			// httptest.NewRecorder starts at 200 with an empty body, and no
			// management verb answers 200 -- they are 201 or 204 -- so an
			// untouched recorder is distinguishable from every answer any of
			// them gives.
			codeAtPoll, bodyAtPoll, verbsAtPoll := 0, -1, -1
			f.snaps.setOnPoll(func() {
				codeAtPoll, bodyAtPoll = rec.Code, rec.Body.Len()
				verbsAtPoll = len(m.seenCalls())
			})

			f.serveInto(rec, c.method, c.target, c.body)
			if rec.Code >= 400 {
				t.Fatalf("%s %s = %d (%s)", c.method, c.target, rec.Code, rec.Body.String())
			}
			if n := f.snaps.pollCount(); n != 1 {
				t.Fatalf("%s forced %d polls, want exactly 1: without one the browser's "+
					"re-fetch is answered from the tree as it was before the verb ran", c.verb, n)
			}
			if bodyAtPoll != 0 || codeAtPoll != http.StatusOK {
				t.Errorf("%s had already answered %d with %d bytes when it forced the poll; "+
					"the refresh has to land before the response the browser reacts to",
					c.verb, codeAtPoll, bodyAtPoll)
			}
			// AFTER the tmux command and before the answer, which is the only
			// window that helps: a poll forced first reads the tree as it was
			// before the verb changed it, which is the staleness being fixed,
			// dressed up as a fix.
			if verbsAtPoll != 1 {
				t.Errorf("%s had reached tmux %d times when it forced the poll, want 1: "+
					"a poll taken before the verb ran cannot show what the verb did", c.verb, verbsAtPoll)
			}
		})
	}
}

// The failure path forces one too, and this is the case the frontend's comment
// actually describes: the ordinary management failure is an id that was alive
// when the poll that produced it ran and is not any more, so the row the owner
// just tried to act on is precisely the one that has to disappear along with
// the toast.
func TestAFailedManagementVerbStillForcesThePoll(t *testing.T) {
	f := newManageFixture(t)
	f.seed(t, "victim")

	rec := f.ok("DELETE", "/api/panes/"+pathID("%99"), `{"confirm":true}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("killing a stale pane id = %d (%s), want 400", rec.Code, rec.Body.String())
	}
	if n := f.snaps.pollCount(); n != 1 {
		t.Fatalf("a failed kill forced %d polls, want 1: the stale row is exactly what "+
			"has to go away with the error", n)
	}
}

// A request turned away before it reaches a handler forces nothing. The poll
// costs a tmux fork and a capture per agent pane, and an unauthenticated or
// unconfirmed request has changed nothing for it to catch up with -- so this is
// what keeps the forced poll from being reachable without the device cookie.
func TestARequestThatNeverReachedTmuxForcesNoPoll(t *testing.T) {
	for _, c := range []struct {
		name, method, target, body string
		opts                       func(f *fixture) []reqOpt
	}{
		{"no device cookie", "DELETE", "/api/panes/%250", `{"confirm":true}`,
			func(*fixture) []reqOpt { return []reqOpt{origin(canonicalOrigin)} }},
		{"a delete with no confirmation", "DELETE", "/api/panes/%250", `{}`,
			func(f *fixture) []reqOpt { return []reqOpt{authed(f.token), origin(canonicalOrigin)} }},
		{"a body that is not JSON", "PATCH", "/api/panes/%250", `{`,
			func(f *fixture) []reqOpt { return []reqOpt{authed(f.token), origin(canonicalOrigin)} }},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t, withManager(&deadlineManager{}))
			if rec := f.do(c.method, c.target, c.body, c.opts(f)...); rec.Code < 400 {
				t.Fatalf("%s = %d (%s), want a refusal", c.name, rec.Code, rec.Body.String())
			}
			if n := f.snaps.pollCount(); n != 0 {
				t.Errorf("%s forced %d polls; a request that never reached tmux has "+
					"nothing for a poll to catch up with", c.name, n)
			}
		})
	}
}
