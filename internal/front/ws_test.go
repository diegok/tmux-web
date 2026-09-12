package front_test

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/diegok/tmux-web/internal/front"
	"github.com/diegok/tmux-web/internal/ptybridge"
	"github.com/diegok/tmux-web/internal/tmux/testutil"
)

// The canonical origin this handler is configured for. Spelled out here rather
// than shared with auth_test.go so the two files can be read on their own.
const wsCanonical = "https://tmux.example.com"

// A tab's throwaway session is named by tmux.NewSessionName, which no test can
// see before it exists; every assertion about "the browser tab's session" finds
// it by this prefix instead.
const wsSessionPrefix = "_web-"

// fastPing makes the keepalive observable inside a test. The production
// defaults are 20s/10s, which no test can wait for.
var fastPing = front.TerminalConfig{PingInterval: 100 * time.Millisecond, PingTimeout: time.Second}

// --- fixture ---------------------------------------------------------------

// wsFixture is a handler in front of a private tmux server holding one real
// session called "work" -- the session a browser tab attaches to.
type wsFixture struct {
	srv *testutil.Server
	ts  *httptest.Server
}

func newWSFixture(t *testing.T, cfg front.TerminalConfig) *wsFixture {
	t.Helper()
	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "work", "-x", "80", "-y", "24")

	cfg.TmuxArgs = srv.Args()
	ts := httptest.NewServer(front.NewTerminalHandler(cfg))
	t.Cleanup(ts.Close)
	return &wsFixture{srv: srv, ts: ts}
}

// defaultWSFixture is the common case: the canonical origin, production ping
// timings, one "work" session.
func defaultWSFixture(t *testing.T) *wsFixture {
	t.Helper()
	return newWSFixture(t, front.TerminalConfig{AllowedOrigin: wsCanonical})
}

// tryDial attempts a handshake. origin is omitted entirely when empty, which is
// what a non-browser client sends -- coder/websocket's own origin check treats
// that as authorized, so it has to be tested against ours.
func (f *wsFixture) tryDial(t *testing.T, origin, query string, opts *websocket.DialOptions) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	if opts == nil {
		opts = &websocket.DialOptions{}
	}
	if opts.HTTPHeader == nil {
		opts.HTTPHeader = http.Header{}
	}
	if origin != "" {
		opts.HTTPHeader.Set("Origin", origin)
	}
	// Cancelled at the end of the test, not on return: the returned connection
	// belongs to the request this context bounds.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return websocket.Dial(ctx, "ws"+strings.TrimPrefix(f.ts.URL, "http")+query, opts)
}

func (f *wsFixture) dial(t *testing.T, origin, query string) *websocket.Conn {
	t.Helper()
	c, _, err := f.tryDial(t, origin, query, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.CloseNow() })
	return c
}

func (f *wsFixture) sessions(t *testing.T) string {
	t.Helper()
	// "work" is unattached but keeps the server alive, so a failure here is
	// never the transient "no server" case and must not be polled through.
	out, err := f.srv.TryRun("list-sessions", "-F", "#{session_name}")
	if err != nil {
		t.Fatalf("list-sessions: %v", err)
	}
	return out
}

// tabSession waits for the browser tab's throwaway session to exist with its
// client attached, and returns its name. Asserting on a session that is still
// starting up is the main source of flake in these tests.
func (f *wsFixture) tabSession(t *testing.T) string {
	t.Helper()
	var name string
	wsWaitFor(t, 10*time.Second, func() bool {
		out, err := f.srv.TryRun("list-sessions", "-F", "#{session_name} #{session_attached}")
		if err != nil {
			return false
		}
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, wsSessionPrefix) && strings.HasSuffix(line, " 1") {
				name, _, _ = strings.Cut(line, " ")
				return true
			}
		}
		return false
	}, "the tab's tmux session never came up attached")
	return name
}

func (f *wsFixture) waitNoTabSession(t *testing.T, why string) {
	t.Helper()
	wsWaitFor(t, 10*time.Second, func() bool {
		return !strings.Contains(f.sessions(t), wsSessionPrefix)
	}, why)
}

func wsWaitFor(t *testing.T, d time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal(msg)
}

// wsWrite sends one already-framed message as the binary type the protocol
// requires of both directions.
func wsWrite(t *testing.T, c *websocket.Conn, frame []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageBinary, frame); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// wsReadUntil accumulates decoded data frames until want appears. It also
// asserts the transport-level type of every message, which is the only thing
// that catches a server sending text frames: Decode is happy either way, and so
// is a Go client. A browser is not -- text frames arrive as strings that have
// already been through UTF-8 replacement, which corrupts the escape sequences
// this stream is made of.
func wsReadUntil(t *testing.T, c *websocket.Conn, want string, d time.Duration) {
	t.Helper()
	var got strings.Builder
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		typ, msg, err := c.Read(ctx)
		cancel()
		if err != nil {
			t.Fatalf("read: %v (got %q)", err, got.String())
		}
		if typ != websocket.MessageBinary {
			t.Fatalf("message type = %v, want binary: PTY bytes are not text", typ)
		}
		kind, payload, err := ptybridge.Decode(msg)
		if err != nil {
			t.Fatalf("undecodable frame %v: %v", msg, err)
		}
		if kind != ptybridge.FrameData {
			continue
		}
		got.Write(payload)
		if strings.Contains(got.String(), want) {
			return
		}
	}
	t.Fatalf("never saw %q; got %q", want, got.String())
}

// wsExpectClosed asserts the server closed the socket, with the status it
// closed with. Reading is the only way to observe a close: a write can succeed
// locally against a socket the peer has already given up on.
func wsExpectClosed(t *testing.T, c *websocket.Conn, want websocket.StatusCode) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		_, _, err := c.Read(ctx)
		if err == nil {
			continue // server output racing the close; keep reading
		}
		if got := websocket.CloseStatus(err); got != want {
			t.Fatalf("close status = %v (%v), want %v", got, err, want)
		}
		return
	}
}

// --- origin ----------------------------------------------------------------

// Browsers attach cookies to cross-origin WebSocket handshakes, so Origin is
// the CSRF boundary for this endpoint. Every case here is dialled against a
// fixture whose "work" session really exists, so a rejection can only be the
// origin check: without that, a handler that 400s on a missing session would
// make all of these pass while allowing every origin.
func TestWebSocketRejectsForeignOrigins(t *testing.T) {
	f := defaultWSFixture(t)

	for _, tc := range []struct {
		name, origin string
	}{
		// The one that matters. A service later published on a sibling
		// subdomain is *same-site* with the canonical host -- SameSite is
		// computed on the registrable domain -- so the browser attaches the
		// device cookie. Only an exact origin match stops it.
		{"sibling subdomain", "https://test.example.com"},
		{"registrable domain itself", "https://example.com"},
		// Kills a suffix comparison from the other end.
		{"canonical host as a prefix", "https://tmux.example.com.evil.com"},
		{"canonical host inside a userinfo", "https://tmux.example.com@evil.com"},
		{"unrelated host", "https://evil.com"},
		// A downgraded scheme is a different origin, and the one an attacker
		// on the network can serve.
		{"same host over http", "http://tmux.example.com"},
		// The default port is elided by browsers; a request carrying it is not
		// something the real frontend produces.
		{"explicit default port", "https://tmux.example.com:443"},
		{"non-default port", "https://tmux.example.com:8443"},
		// coder/websocket's own check returns "authorized" for a request with
		// no Origin at all, so this case is ours alone to reject.
		{"no origin header", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, resp, err := f.tryDial(t, tc.origin, "?session=work", nil)
			if err == nil {
				_ = c.CloseNow()
				t.Fatalf("origin %q was accepted", tc.origin)
			}
			// The status matters as much as the failure: it is what
			// distinguishes "wrong origin" from a handler that happens to
			// reject everything, which is what a rejection test is worth
			// nothing without.
			wsWantStatus(t, resp, err, http.StatusForbidden)
			if strings.Contains(f.sessions(t), wsSessionPrefix) {
				t.Fatal("a rejected handshake still started a tmux client")
			}
		})
	}
}

// coder/websocket authorizes any Origin whose host equals the request's Host
// header, regardless of the configured patterns. That is not the boundary this
// daemon wants: it is reachable by IP and by whatever name a reverse proxy
// passes through, and only the configured origin may open a terminal. This is
// the case the explicit check exists for, and the one that kills a mutant which
// deletes it and leans on the library.
func TestWebSocketChecksOriginAgainstConfigNotTheHostHeader(t *testing.T) {
	f := defaultWSFixture(t)

	host := strings.TrimPrefix(f.ts.URL, "http://")
	c, resp, err := f.tryDial(t, "http://"+host, "?session=work", nil)
	if err == nil {
		_ = c.CloseNow()
		t.Fatalf("origin matching the Host header was accepted; only %s may connect", wsCanonical)
	}
	wsWantStatus(t, resp, err, http.StatusForbidden)
}

// Two Origin headers is not something a browser produces; it means a proxy or
// a smuggling attempt is in the path, and picking one of them is a guess.
func TestWebSocketRejectsDuplicateOriginHeaders(t *testing.T) {
	f := defaultWSFixture(t)

	opts := &websocket.DialOptions{HTTPHeader: http.Header{}}
	opts.HTTPHeader.Add("Origin", wsCanonical)
	opts.HTTPHeader.Add("Origin", "https://evil.com")

	c, resp, err := f.tryDial(t, "", "?session=work", opts)
	if err == nil {
		_ = c.CloseNow()
		t.Fatal("a handshake carrying two Origin headers was accepted")
	}
	wsWantStatus(t, resp, err, http.StatusForbidden)
}

// An unset or unusable AllowedOrigin must reject everything rather than
// everything-goes. A misconfigured daemon that serves no terminals is a bug
// report; one that serves them to any page on the internet is not noticed.
func TestWebSocketWithNoConfiguredOriginRejectsEveryone(t *testing.T) {
	for _, origin := range []string{"", wsCanonical, "https://evil.com"} {
		t.Run("origin="+origin, func(t *testing.T) {
			f := newWSFixture(t, front.TerminalConfig{})
			c, resp, err := f.tryDial(t, origin, "?session=work", nil)
			if err == nil {
				_ = c.CloseNow()
				t.Fatal("a handler with no configured origin accepted a connection")
			}
			wsWantStatus(t, resp, err, http.StatusForbidden)
		})
	}
}

// --- base session ----------------------------------------------------------

// The session to attach to arrives in the query string. Both bad cases are
// rejected during the HTTP handshake rather than by opening a socket that dies
// a moment later: the browser gets a status code it can show, and no tmux
// client is ever forked.
func TestWebSocketRejectsAnUnusableSessionParameter(t *testing.T) {
	f := defaultWSFixture(t)

	for _, tc := range []struct {
		name, query string
		want        int
	}{
		{"absent", "", http.StatusBadRequest},
		// `tmux has-session -t ""` exits 0 -- an empty target resolves to
		// "whatever is current" -- so an empty value must be rejected before
		// tmux ever sees it.
		{"empty", "?session=", http.StatusBadRequest},
		{"no such session", "?session=nope", http.StatusNotFound},
		// tmux target matching falls back to a prefix, so "wor" would
		// otherwise attach to "work".
		{"prefix of a real session", "?session=wor", http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, resp, err := f.tryDial(t, wsCanonical, tc.query, nil)
			if err == nil {
				_ = c.CloseNow()
				t.Fatalf("session=%q was accepted", tc.query)
			}
			wsWantStatus(t, resp, err, tc.want)
			if strings.Contains(f.sessions(t), wsSessionPrefix) {
				t.Fatal("a rejected handshake still started a tmux client")
			}
		})
	}
}

// The attach path after a rename, which is the shape the owner hit: a session
// renamed while it already had a group.
//
// tmux freezes `session_group` at the name the group was created under, and the
// group is created by this app's own throwaway session on the first attach --
// so from the first browser tab onwards, every rename leaves the group key
// naming a session that no longer answers to it. `has-session -t =<group key>`
// then fails, the handshake 404s, and the tab says the session is gone while it
// is sitting right there in the sidebar. Renaming an *ungrouped* session does
// not reproduce it: the group is empty and refills under the new name.
//
// What this pins is the contract the frontend now relies on -- an id is
// addressed as an id, the live name is addressed exactly, and the frozen group
// key is addressed as neither.
func TestWebSocketAttachesToARenamedSessionByIDOrLiveName(t *testing.T) {
	f := defaultWSFixture(t)
	// The precondition: a grouped member, exactly as the first tab leaves behind.
	f.srv.Run(t, "new-session", "-d", "-t", "work", "-s", "_web-old")
	f.srv.Run(t, "rename-session", "-t", "work", "api")

	// The freeze itself, asserted rather than assumed. If a future tmux renames
	// the group with the session, this test would otherwise keep passing while
	// testing nothing, and the whole design decision behind Row.SessionID would
	// be up for revisiting.
	if got := f.srv.Run(t, "list-sessions", "-F", "#{session_name} #{session_group}"); !strings.Contains(got, "api work") {
		t.Fatalf("the group key did not freeze at the pre-rename name: %q", got)
	}
	sid := f.srv.Run(t, "display-message", "-p", "-t", "api:", "#{session_id}")
	if !strings.HasPrefix(sid, "$") {
		t.Fatalf("session id = %q, want $N", sid)
	}

	// The group key is not an address and must not become one by accident: it
	// names no session, and a handler that fell back to a prefix or to the
	// group would attach the tab to whatever matched.
	c, resp, err := f.tryDial(t, wsCanonical, "?session="+url.QueryEscape("work"), nil)
	if err == nil {
		_ = c.CloseNow()
		t.Fatal("the frozen group key was accepted as a session address")
	}
	wsWantStatus(t, resp, err, http.StatusNotFound)

	// The id, which is what the frontend sends.
	byID := f.dial(t, wsCanonical, "?session="+url.QueryEscape(sid))
	tab := f.tabSession(t)
	// Grouped onto the renamed session, not merely connected to something: a
	// tab in the wrong group cannot select the panes the sidebar is showing.
	if got := f.srv.Run(t, "display-message", "-p", "-t", "="+tab+":", "#{session_group}"); got != "work" {
		t.Errorf("the tab joined group %q, want the renamed session's group work", got)
	}
	wsWrite(t, byID, ptybridge.EncodeData([]byte("echo by-id-ok\r")))
	wsReadUntil(t, byID, "by-id-ok", 10*time.Second)
	_ = byID.CloseNow()

	// And the live name, which is what a person types into the query string.
	byName := f.dial(t, wsCanonical, "?session="+url.QueryEscape("api"))
	wsWrite(t, byName, ptybridge.EncodeData([]byte("echo by-name-ok\r")))
	wsReadUntil(t, byName, "by-name-ok", 10*time.Second)
}

// --- round trip ------------------------------------------------------------

func TestWebSocketRoundTrip(t *testing.T) {
	f := defaultWSFixture(t)
	c := f.dial(t, wsCanonical, "?session=work")
	f.tabSession(t)

	wsWrite(t, c, ptybridge.EncodeData([]byte("echo ws-ok\r")))
	wsReadUntil(t, c, "ws-ok", 10*time.Second)
}

// The browser terminal has no size until it has laid out, so the attach starts
// at a conventional 80x24 and the first resize control frame corrects it.
func TestWebSocketAttachesAtAConventionalSize(t *testing.T) {
	f := defaultWSFixture(t)
	f.dial(t, wsCanonical, "?session=work")
	tab := f.tabSession(t)

	if got := f.srv.Run(t, "list-clients", "-t", "="+tab, "-F", "#{client_width}x#{client_height}"); got != "80x24" {
		t.Fatalf("client size = %s, want 80x24", got)
	}
}

// --- control messages ------------------------------------------------------

// The JSON below is written out literally rather than built from the handler's
// own struct: it is a cross-language contract that the browser transport
// implements independently, so a renamed field has to fail here.
func TestResizeControlMessageResizesTheTmuxClient(t *testing.T) {
	f := defaultWSFixture(t)
	c := f.dial(t, wsCanonical, "?session=work")
	tab := f.tabSession(t)

	wsWrite(t, c, ptybridge.EncodeControl([]byte(`{"type":"resize","cols":100,"rows":37}`)))

	wsWaitFor(t, 10*time.Second, func() bool {
		out, err := f.srv.TryRun("list-clients", "-t", "="+tab, "-F", "#{client_width}x#{client_height}")
		return err == nil && out == "100x37"
	}, "resize never reached the tmux client")
}

// Clicking a pane in the sidebar moves this tab and nothing else -- not the
// other tabs, and not the terminal the user is sitting in front of.
func TestSelectControlMessageMovesOnlyThisTabsSession(t *testing.T) {
	f := defaultWSFixture(t)
	f.srv.Run(t, "new-window", "-t", "=work", "-d")
	panes := strings.Split(f.srv.Run(t, "list-panes", "-s", "-t", "=work", "-F", "#{pane_id}"), "\n")
	if len(panes) != 2 {
		t.Fatalf("want two panes to choose between, got %q", panes)
	}

	c := f.dial(t, wsCanonical, "?session=work")
	tab := f.tabSession(t)
	before := wsCurrentWindow(t, f, "work")

	wsWrite(t, c, ptybridge.EncodeControl([]byte(`{"type":"select","pane":"`+panes[1]+`"}`)))

	wsWaitFor(t, 10*time.Second, func() bool {
		return wsCurrentWindow(t, f, tab) != before
	}, "select never moved the tab's session")
	if now := wsCurrentWindow(t, f, "work"); now != before {
		t.Fatalf("the user's own session moved from window %s to %s", before, now)
	}
}

func TestCopyModeControlMessageEntersCopyMode(t *testing.T) {
	f := defaultWSFixture(t)
	c := f.dial(t, wsCanonical, "?session=work")
	f.tabSession(t)
	pane := f.srv.Run(t, "list-panes", "-t", "=work:", "-F", "#{pane_id}")

	wsWrite(t, c, ptybridge.EncodeControl([]byte(`{"type":"copy-mode","pane":"`+pane+`"}`)))

	wsWaitFor(t, 10*time.Second, func() bool {
		out, err := f.srv.TryRun("display-message", "-p", "-t", pane, "#{pane_in_mode}")
		return err == nil && out == "1"
	}, "copy-mode never reached the pane")
}

// The palette offers "enter copy mode" before anything has been clicked, so the
// message has to work with no pane: it means "the pane this tab is looking at".
func TestCopyModeWithNoPaneUsesTheTabsOwnCurrentPane(t *testing.T) {
	f := defaultWSFixture(t)
	c := f.dial(t, wsCanonical, "?session=work")
	f.tabSession(t)
	pane := f.srv.Run(t, "list-panes", "-t", "=work:", "-F", "#{pane_id}")

	wsWrite(t, c, ptybridge.EncodeControl([]byte(`{"type":"copy-mode"}`)))

	wsWaitFor(t, 10*time.Second, func() bool {
		out, err := f.srv.TryRun("display-message", "-p", "-t", pane, "#{pane_in_mode}")
		return err == nil && out == "1"
	}, "copy-mode with no pane never reached the tab's current pane")
}

// `tmux copy-mode -t work` exits 0 and puts the *user's* current pane into copy
// mode. A frontend bug that sent a session name where a pane id belongs would
// otherwise freeze the terminal they are working in, from a machine they are
// not sitting at.
func TestCopyModeRejectsATargetThatIsNotAPaneID(t *testing.T) {
	f := defaultWSFixture(t)
	c := f.dial(t, wsCanonical, "?session=work")
	f.tabSession(t)
	pane := f.srv.Run(t, "list-panes", "-t", "=work:", "-F", "#{pane_id}")

	wsWrite(t, c, ptybridge.EncodeControl([]byte(`{"type":"copy-mode","pane":"work"}`)))

	// The socket survives, so the round trip below proves the message was
	// processed and rejected rather than merely still in flight.
	wsWrite(t, c, ptybridge.EncodeData([]byte("echo after-bad-target\r")))
	wsReadUntil(t, c, "after-bad-target", 10*time.Second)

	if got := f.srv.Run(t, "display-message", "-p", "-t", pane, "#{pane_in_mode}"); got != "0" {
		t.Fatal("a session name used as a pane target put a pane into copy mode")
	}
}

// --- where am I -------------------------------------------------------------

// The question a tab cannot answer for itself.
//
// Its own throwaway session has a current window independent of the user's --
// that is what grouping buys, and what lets a browser and a local terminal look
// at different things -- so nothing the tab can read tells it which pane it
// landed on. Every fixture here parks the user's own session somewhere else, so
// the pane the daemon reports can never be mistaken for the user's.
func TestWhereAnswersWithThePaneThisTabLandedOn(t *testing.T) {
	f := defaultWSFixture(t)
	// Window 0 is split, so "the active pane" is not "the first pane" either.
	f.srv.Run(t, "split-window", "-t", "=work:0", "-d")
	f.srv.Run(t, "new-window", "-t", "=work", "-d")
	panes := strings.Split(f.srv.Run(t, "list-panes", "-t", "=work:0", "-F", "#{pane_id}"), "\n")
	if len(panes) != 2 {
		t.Fatalf("want a split window to land on, got %q", panes)
	}
	// Where the tab will land: `new-session -t` starts its session on the
	// group's FIRST window (measured on 3.7b -- not on the base session's
	// current window, which is what this file used to say), and the active pane
	// within a window belongs to the window, so it is shared with the group.
	f.srv.Run(t, "select-pane", "-t", panes[1])
	want := panes[1]

	// The user, meanwhile, is somewhere else entirely.
	f.srv.Run(t, "select-window", "-t", "=work:1")

	// Asked without waiting for the tab's session to exist, deliberately: the
	// daemon answers the WebSocket handshake before it forks the attach, so a
	// browser really does ask this before there is a session to ask about.
	c := f.dial(t, wsCanonical, "?session=work")
	wsWrite(t, c, ptybridge.EncodeControl([]byte(`{"type":"where"}`)))

	if got := wsReadPane(t, c, 15*time.Second); got != want {
		t.Errorf("the tab was told it is on %q, want %q.\n"+
			"the user's own session is looking at %q and window 0's first pane is %q -- "+
			"either of those is a pane the browser is not showing",
			got, want,
			f.srv.Run(t, "list-panes", "-t", "=work:1", "-F", "#{pane_id}"), panes[0])
	}
}

// "where" is not "where did I land": it is asked after the tab has replayed the
// pane it remembered, and it has to describe the tab as it is by then.
func TestWhereFollowsASelectRatherThanReportingTheLanding(t *testing.T) {
	f := defaultWSFixture(t)
	f.srv.Run(t, "new-window", "-t", "=work", "-d")
	moved := f.srv.Run(t, "list-panes", "-t", "=work:1", "-F", "#{pane_id}")
	landed := f.srv.Run(t, "list-panes", "-t", "=work:0", "-F", "#{pane_id}")

	c := f.dial(t, wsCanonical, "?session=work")
	f.tabSession(t)

	// One loop reads these in order, so the answer is taken after the select.
	wsWrite(t, c, ptybridge.EncodeControl([]byte(`{"type":"select","pane":"`+moved+`"}`)))
	wsWrite(t, c, ptybridge.EncodeControl([]byte(`{"type":"where"}`)))

	if got := wsReadPane(t, c, 15*time.Second); got != moved {
		t.Errorf("the tab was told it is on %q, want %q (it selected that); it landed on %q",
			got, moved, landed)
	}
}

// The case that makes one question enough for both.
//
// A tab reloads and replays the pane it remembered, but that pane died while it
// was away. The select fails, tmux leaves the tab where it attached, and the
// answer has to be that pane -- not the dead one, and not nothing. Without it
// the tab goes on naming a corpse over a terminal showing something else.
func TestWhereReportsRealityAfterASelectThatCouldNotBeCarriedOut(t *testing.T) {
	f := defaultWSFixture(t)
	f.srv.Run(t, "new-window", "-t", "=work", "-d")
	// The user is on window 1; the tab lands on window 0, the group's first.
	f.srv.Run(t, "select-window", "-t", "=work:1")
	alive := f.srv.Run(t, "list-panes", "-t", "=work:0", "-F", "#{pane_id}")

	c := f.dial(t, wsCanonical, "?session=work")
	f.tabSession(t)

	wsWrite(t, c, ptybridge.EncodeControl([]byte(`{"type":"select","pane":"%9999"}`)))
	wsWrite(t, c, ptybridge.EncodeControl([]byte(`{"type":"where"}`)))

	if got := wsReadPane(t, c, 15*time.Second); got != alive {
		t.Errorf("after a select that failed the tab was told it is on %q, want %q", got, alive)
	}
}

// A message this daemon understands but cannot carry out is not a reason to
// destroy a terminal. The sidebar is up to 1.5s stale, so selecting a pane that
// just died is an ordinary race, not a broken client.
func TestControlMessagesThatCannotBeCarriedOutDoNotCloseTheSocket(t *testing.T) {
	for _, tc := range []struct{ name, json string }{
		{"unknown type", `{"type":"wat"}`},
		{"pane that no longer exists", `{"type":"select","pane":"%9999"}`},
		{"pane id that is not one", `{"type":"select","pane":"work"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := defaultWSFixture(t)
			c := f.dial(t, wsCanonical, "?session=work")
			f.tabSession(t)

			wsWrite(t, c, ptybridge.EncodeControl([]byte(tc.json)))

			wsWrite(t, c, ptybridge.EncodeData([]byte("echo survived\r")))
			wsReadUntil(t, c, "survived", 10*time.Second)
		})
	}
}

// A dimension a pty cannot express is ignored, not applied. cols and rows cross
// the wire as JSON numbers and reach a pty as uint16s, so 99999 silently
// becomes 34463 without the guard, and 0 is not a terminal at all. The echoed
// round trip in the middle is the ordering barrier: control messages and
// keystrokes are processed by the same loop in the order they arrive, so output
// from the echo proves the resize was already handled.
func TestOutOfRangeResizesAreIgnoredRatherThanApplied(t *testing.T) {
	for _, tc := range []struct{ name, json string }{
		{"zero", `{"type":"resize","cols":0,"rows":0}`},
		{"negative", `{"type":"resize","cols":-1,"rows":-1}`},
		{"past what a uint16 holds", `{"type":"resize","cols":99999,"rows":40}`},
		{"no dimensions at all", `{"type":"resize"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := defaultWSFixture(t)
			c := f.dial(t, wsCanonical, "?session=work")
			tab := f.tabSession(t)

			wsWrite(t, c, ptybridge.EncodeControl([]byte(tc.json)))
			wsWrite(t, c, ptybridge.EncodeData([]byte("echo survived\r")))
			wsReadUntil(t, c, "survived", 10*time.Second)

			got := f.srv.Run(t, "list-clients", "-t", "="+tab, "-F", "#{client_width}x#{client_height}")
			if got != "80x24" {
				t.Fatalf("client size = %s after an out-of-range resize, want the untouched 80x24", got)
			}
		})
	}
}

// A peer that is not speaking this protocol is a different matter: there is
// nothing to recover to, and silently ignoring it would leave a tab whose
// keystrokes go nowhere with nothing in the log. Closing says so, and
// reconnecting costs nothing because tmux redraws on attach.
func TestFramesThatAreNotThisProtocolCloseTheSocket(t *testing.T) {
	t.Run("text message", func(t *testing.T) {
		f := defaultWSFixture(t)
		c := f.dial(t, wsCanonical, "?session=work")
		f.tabSession(t)

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := c.Write(ctx, websocket.MessageText, ptybridge.EncodeData([]byte("hi"))); err != nil {
			t.Fatalf("write: %v", err)
		}
		wsExpectClosed(t, c, websocket.StatusUnsupportedData)
	})

	for _, tc := range []struct {
		name  string
		frame []byte
	}{
		{"empty message", []byte{}},
		{"unknown frame kind", []byte{0x02, 'x'}},
		{"control frame that is not json", append([]byte{ptybridge.FrameControl}, []byte("not json")...)},
		{"control frame with a wrongly typed field", append([]byte{ptybridge.FrameControl}, []byte(`{"type":"resize","cols":"wide"}`)...)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := defaultWSFixture(t)
			c := f.dial(t, wsCanonical, "?session=work")
			f.tabSession(t)

			wsWrite(t, c, tc.frame)
			wsExpectClosed(t, c, websocket.StatusUnsupportedData)
		})
	}
}

// --- lifetime --------------------------------------------------------------

// The tab's tmux client and its session are owned by the socket. Leaving them
// behind is how a daemon accumulates sessions the user never opened.
func TestClosingTheSocketEndsTheTmuxSession(t *testing.T) {
	f := defaultWSFixture(t)
	c := f.dial(t, wsCanonical, "?session=work")
	f.tabSession(t)

	if err := c.Close(websocket.StatusNormalClosure, ""); err != nil {
		t.Fatalf("close: %v", err)
	}
	f.waitNoTabSession(t, "the tab's tmux session outlived its socket")
}

// The realistic disconnect: a laptop that goes to sleep, a process that is
// killed. There is no close frame, only a TCP FIN.
func TestAVanishedClientEndsTheTmuxSession(t *testing.T) {
	f := defaultWSFixture(t)
	d := &wsRecordingDialer{}
	c, _, err := f.tryDial(t, wsCanonical, "?session=work", d.dialOptions())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.CloseNow() }()
	f.tabSession(t)

	d.closeAll(t)
	f.waitNoTabSession(t, "the tab's tmux session outlived a vanished client")
}

// The tmux base session going away -- the user typing `exit`, or killing it --
// ends the socket too, with a status that tells the browser this was not a
// network fault and reconnecting is pointless.
func TestTheSocketClosesCleanlyWhenTheSessionEnds(t *testing.T) {
	f := defaultWSFixture(t)
	c := f.dial(t, wsCanonical, "?session=work")
	tab := f.tabSession(t)

	f.srv.Run(t, "kill-session", "-t", "="+tab)
	wsExpectClosed(t, c, websocket.StatusNormalClosure)
}

// Keepalive is what collects a half-open connection. The client below completes
// the handshake and then never reads, so it never answers a ping, while its TCP
// connection stays open and unclosed -- exactly a closed laptop lid or a NAT
// that dropped the mapping. Without pings the `tmux attach` stays attached,
// destroy-unattached never fires, and the next reconnect adds a second session
// beside the leaked one.
func TestKeepaliveCollectsAHalfOpenConnection(t *testing.T) {
	cfg := fastPing
	cfg.AllowedOrigin = wsCanonical
	f := newWSFixture(t, cfg)

	c := f.dial(t, wsCanonical, "?session=work")
	f.tabSession(t)

	// Deliberately no Read: coder/websocket answers pings from inside Read,
	// so a client that never reads is indistinguishable from a dead one.
	f.waitNoTabSession(t, "a client that stopped answering pings kept its tmux session alive")

	// The socket was never closed from this side, which is what makes the
	// server's ping the only thing that could have detected the loss.
	_ = c
}

// Every connection starts three goroutines and a tmux client. If closing one
// side does not close the others, a daemon that has served a few hundred tabs
// is holding a few hundred parked goroutines and, worse, their tmux sessions.
func TestNothingIsLeftRunningAfterClientsDisappear(t *testing.T) {
	cfg := fastPing
	cfg.AllowedOrigin = wsCanonical
	f := newWSFixture(t, cfg)

	cycle := func(t *testing.T, abrupt bool) {
		t.Helper()
		d := &wsRecordingDialer{}
		c, _, err := f.tryDial(t, wsCanonical, "?session=work", d.dialOptions())
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		f.tabSession(t)
		if abrupt {
			d.closeAll(t) // no close frame: the client vanished
		} else {
			_ = c.Close(websocket.StatusNormalClosure, "")
		}
		f.waitNoTabSession(t, "a tmux session outlived its socket")
		_ = c.CloseNow()
	}

	// One full cycle first, so every lazily created goroutine in the http
	// server, the transport and the tmux harness already exists at baseline.
	cycle(t, true)
	wsSettle(t)
	baseline := runtime.NumGoroutine()

	const cycles = 5
	for i := range cycles {
		cycle(t, i%2 == 0)
	}

	// Three per connection would be +15 here; the tolerance covers only the
	// http server's own churn.
	wsWaitFor(t, 10*time.Second, func() bool {
		return runtime.NumGoroutine() <= baseline+3
	}, "goroutines were left running after every client disappeared")
}

// --- helpers ---------------------------------------------------------------

// wsWantStatus pins the HTTP status a refused handshake was refused with.
func wsWantStatus(t *testing.T, resp *http.Response, err error, want int) {
	t.Helper()
	if resp == nil {
		t.Fatalf("no HTTP response to report a status: %v", err)
	}
	if resp.StatusCode != want {
		t.Fatalf("status = %d, want %d (%v)", resp.StatusCode, want, err)
	}
}

// wsReadPane waits for the daemon's answer to "where": a control frame naming
// the pane this tab is on. Data frames are skipped -- the attach paints a shell
// prompt over the same socket -- and any other control message is a protocol
// the browser does not implement, so it fails rather than being skipped.
func wsReadPane(t *testing.T, c *websocket.Conn, d time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		typ, msg, err := c.Read(ctx)
		cancel()
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if typ != websocket.MessageBinary {
			t.Fatalf("message type = %v, want binary", typ)
		}
		kind, payload, err := ptybridge.Decode(msg)
		if err != nil {
			t.Fatalf("undecodable frame %v: %v", msg, err)
		}
		if kind != ptybridge.FrameControl {
			continue
		}
		var got struct {
			Type string `json:"type"`
			Pane string `json:"pane"`
		}
		if err := json.Unmarshal(payload, &got); err != nil {
			t.Fatalf("control message is not JSON: %q", payload)
		}
		// Pinned literally, in both this suite and the TypeScript one: these
		// two strings are the whole contract, and a rename on one side only
		// leaves a tab that never learns where it is.
		if got.Type != "pane" {
			t.Fatalf("control message type = %q, want \"pane\": %q", got.Type, payload)
		}
		return got.Pane
	}
	t.Fatal("the daemon never said which pane this tab is on")
	return ""
}

func wsCurrentWindow(t *testing.T, f *wsFixture, session string) string {
	t.Helper()
	// The trailing ":" makes this a session target; a bare -t is read as a
	// pane target, and display-message prints an empty line and exits 0 for a
	// target it cannot resolve.
	out := f.srv.Run(t, "display-message", "-p", "-t", "="+session+":", "#{window_id}")
	if out == "" {
		t.Fatalf("no current window for session %s", session)
	}
	return out
}

func wsSettle(t *testing.T) {
	t.Helper()
	for range 5 {
		runtime.GC()
		time.Sleep(50 * time.Millisecond)
	}
}

// wsRecordingDialer hands out the TCP connections a websocket client dials, so
// a test can destroy one without the close handshake a real disconnect never
// performs.
type wsRecordingDialer struct {
	mu    sync.Mutex
	conns []net.Conn
}

func (d *wsRecordingDialer) dialOptions() *websocket.DialOptions {
	return &websocket.DialOptions{HTTPClient: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			c, err := (&net.Dialer{}).DialContext(ctx, network, addr)
			if err == nil {
				d.mu.Lock()
				d.conns = append(d.conns, c)
				d.mu.Unlock()
			}
			return c, err
		},
	}}}
}

func (d *wsRecordingDialer) closeAll(t *testing.T) {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.conns) == 0 {
		t.Fatal("no connection was recorded: the test is not closing what it thinks")
	}
	for _, c := range d.conns {
		_ = c.Close()
	}
}
