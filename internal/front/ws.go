package front

import (
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/diegok/tmux-web/internal/ptybridge"
	"github.com/diegok/tmux-web/internal/tmux"
)

// Keepalive defaults. Pinging is not a nicety here: on a half-open connection
// -- a closed laptop lid, a NAT that dropped the mapping -- the `tmux attach`
// process stays alive and attached, so destroy-unattached never fires, the
// session is never collected, and the user's next reconnect adds a second
// throwaway session beside the leaked one. On a flaky link those accumulate.
// A ping is the only thing that distinguishes a quiet terminal from a dead peer.
const (
	wsPingInterval = 20 * time.Second
	wsPingTimeout  = 10 * time.Second
)

// wsReadLimit bounds one inbound message. Keystrokes are a few bytes, but a
// paste arrives as a single message, and coder/websocket's 32KB default would
// tear down a terminal for pasting a moderately large blob of text. A megabyte
// is far past anything a person pastes and still nothing next to the PTY buffer
// it feeds.
const wsReadLimit = 1 << 20

// wsControlBuffer bounds the queue of control messages waiting to go out to the
// browser. A tab sends one "where" per socket and the answer is one small
// frame, so this is slack rather than capacity; the send is non-blocking so
// that a browser which has stopped reading cannot park the read goroutine, and
// the ping loop is what eventually collects such a peer.
const wsControlBuffer = 4

// The size the attach starts at. A browser terminal has no dimensions until it
// has laid out, so the first thing the frontend sends is a resize; this is only
// what tmux draws in the meantime, and 80x24 is the conventional answer.
const (
	wsInitialCols = 80
	wsInitialRows = 24
)

// wsTmuxTimeout bounds each tmux command run on behalf of a socket. These run
// on the read goroutine, so a wedged tmux server would otherwise stall the
// tab's keystrokes indefinitely rather than for a few seconds.
const wsTmuxTimeout = 5 * time.Second

// TerminalConfig configures the WebSocket terminal endpoint.
type TerminalConfig struct {
	// TmuxArgs selects the tmux server, e.g. {"-L", "sock"}; nil is the
	// user's default server.
	TmuxArgs []string

	// AllowedOrigin is the one origin permitted to open a terminal, as
	// scheme://host[:port]. Anything else -- including a request with no
	// Origin at all -- is refused. Empty refuses everything.
	AllowedOrigin string

	// AllowedOrigins is the same thing for a deployment that has more than one
	// origin, and is the field the server wires up: it is fed from
	// Auth.Origins() so the handshake asks the same question of the same
	// allowlist the cookie middleware uses, instead of keeping a second copy of
	// the rule that could drift from it.
	//
	// Production has exactly one entry, https://<host>. Dev mode has three --
	// http://localhost:port, http://127.0.0.1:port and http://[::1]:port --
	// because those are three distinct origins to a browser and which one
	// appears depends on what the developer typed. A single-valued field would
	// have made --dev work or not depending on that, which is the kind of
	// difference between dev and production that hides a real bug.
	//
	// The effective allowlist is this field plus AllowedOrigin; each entry is
	// matched exactly.
	AllowedOrigins []string

	// PingInterval and PingTimeout override the keepalive timings. Zero means
	// the defaults above; they exist so tests can observe a keepalive without
	// waiting twenty seconds for one.
	PingInterval time.Duration
	PingTimeout  time.Duration
}

// TerminalHandler serves one browser tab's terminal over a WebSocket: a
// throwaway tmux session grouped onto a real one, attached under a PTY, with
// the PTY's bytes framed onto the socket in both directions.
type TerminalHandler struct {
	cfg TerminalConfig

	// origins is the configured allowlist reduced to scheme://host entries,
	// with anything unreadable as an origin dropped. Compared by equality,
	// never by suffix.
	origins []string

	tm *tmux.Client
}

// NewTerminalHandler builds the handler. It cannot fail: an AllowedOrigin that
// is unset or unreadable leaves the handler refusing every connection, which is
// the safe direction and is logged. Returning an error instead would push a
// decision onto every caller for a case that is already fail-closed.
func NewTerminalHandler(cfg TerminalConfig) *TerminalHandler {
	var origins []string
	for _, o := range append([]string{cfg.AllowedOrigin}, cfg.AllowedOrigins...) {
		if n := wsNormalizeOrigin(o); n != "" && !slices.Contains(origins, n) {
			origins = append(origins, n)
		}
	}
	if len(origins) == 0 {
		slog.Warn("terminal handler has no usable allowed origin; it will refuse every connection",
			"allowed_origin", cfg.AllowedOrigin, "allowed_origins", cfg.AllowedOrigins)
	}
	return &TerminalHandler{cfg: cfg, origins: origins, tm: tmux.NewClient(cfg.TmuxArgs)}
}

func (h *TerminalHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Origin is the CSRF boundary for this endpoint, and it is checked before
	// anything else happens. Browsers attach cookies to cross-origin WebSocket
	// handshakes, so without this any page the user visits could open a socket
	// onto their shell. SameSite does not help: it is computed on the
	// registrable domain, so a service later published on test.example.com is
	// same-site with tmux.example.com and its cookies ride along.
	if !h.allowsOrigin(r) {
		http.Error(w, "forbidden origin", http.StatusForbidden)
		return
	}

	base := r.URL.Query().Get("session")
	if base == "" {
		// Not a default: tmux resolves an empty target to "whatever is
		// current" and exits 0, so an absent parameter would silently attach
		// the tab to an arbitrary session. There is no session the daemon
		// could pick that would be right, and the frontend always knows which
		// one it wants.
		http.Error(w, "missing session parameter", http.StatusBadRequest)
		return
	}

	// One value, resolved once, used for both the existence check and the
	// attach -- so the session this handler said yes to cannot be a different
	// session from the one it grouped the tab onto.
	target := wsSessionTarget(base)

	// Checked before the upgrade so that "there is no such session" arrives as
	// an HTTP status the browser can act on, rather than as a socket that opens
	// and dies a moment later with the reason painted into the terminal.
	ctx, cancel := context.WithTimeout(r.Context(), wsTmuxTimeout)
	_, err := h.tm.Run(ctx, "has-session", "-t", target)
	cancel()
	if err != nil {
		slog.Info("terminal refused: no such tmux session", "session", base, "target", target, "err", err)
		http.Error(w, "no such session", http.StatusNotFound)
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// The library's own origin check is turned off because allowsOrigin
		// above has already run and is strictly stricter. Layering the two
		// would be worse than it looks: coder/websocket authorizes any request
		// whose Origin host matches the Host header, and treats a request with
		// no Origin at all as authorized, so it is not the boundary this
		// endpoint needs -- and standing behind allowsOrigin it would mask a
		// bug in it. With it off, every origin rule this handler has is one its
		// own tests can reach; a suffix comparison slipped into allowsOrigin
		// fails TestWebSocketRejectsForeignOrigins instead of being quietly
		// caught by the library.
		InsecureSkipVerify: true,
	})
	if err != nil {
		return // Accept has already written the response
	}
	h.serve(r.Context(), conn, target)
}

// wsSessionTarget turns the ?session= value into a tmux target.
//
// The parameter carries two different things, and tmux has to be told which.
//
//   - A session id, "$4", which is what the app sends. It has to, because the
//     alternative it used to send was the session *group* key, and tmux freezes
//     `session_group` at the name the group was created under. This app creates
//     that group itself on the first attach, so from the first browser tab
//     onwards a rename leaves the key naming nothing: `has-session -t =work3`
//     answers "can't find session: work3" while the session is alive as `api`,
//     the handshake 404s, and the tab tells the user their session is gone
//     while its panes are listed in the sidebar beside the message. An id
//     answers for the session's whole life, whatever it is renamed to.
//   - A name, which is the only thing a person typing `?session=work` by hand
//     could write. That is a documented escape hatch -- a tab pinned this way
//     is never moved by the resolver -- so it stays supported.
//
// A name gets the "=" exact-match prefix and an id does not. The "=" is
// load-bearing for a name: tmux target matching otherwise falls back to a
// prefix, so ?session=wor would attach to "work". It is not merely unnecessary
// on an id but meaningless -- tmux resolves "$4" as an id before it considers
// names, with or without the prefix (probed on 3.7b: with a session actually
// named "$0" beside a session whose id is $0, both "$0" and "=$0" resolved to
// the id). Leaving it on would have worked by that accident; saying which kind
// of thing arrived is what makes the two cases visible and testable.
//
// tmux.ValidateSessionID is the discriminator rather than a local "$" test:
// that rule already exists once in internal/tmux, and it is strict about what
// follows the sigil, so a session someone named "$x" is still addressed as the
// name it is.
func wsSessionTarget(s string) string {
	if tmux.ValidateSessionID(s) == nil {
		return s
	}
	return "=" + s
}

// allowsOrigin reports whether the request came from the one configured origin.
//
// Exact match on scheme, host and port. Never a suffix match: comparing
// suffixes is what lets test.example.com pass as tmux.example.com, which is the
// precise attack this endpoint has to survive. Never a host-only match either:
// a downgraded scheme is a different origin and is the one an attacker on the
// network can serve.
//
// A request with no Origin header is refused. Browsers always send one on a
// WebSocket handshake, so the only clients this turns away are non-browser ones
// -- which have no business here, and for which coder/websocket's own check
// returns "authorized".
func (h *TerminalHandler) allowsOrigin(r *http.Request) bool {
	if len(h.origins) == 0 {
		return false // unconfigured means closed, not open
	}
	// Exactly one header. Two Origin headers is not something a browser
	// produces; it is a sign of a proxy or a smuggling attempt, and picking one
	// of them is a guess.
	got := r.Header.Values("Origin")
	if len(got) != 1 {
		return false
	}
	return slices.Contains(h.origins, wsNormalizeOrigin(got[0]))
}

// wsNormalizeOrigin reduces an origin to lowercase scheme://host, or "" if the
// string is not one. Both sides of the comparison go through it so that a
// configured "https://Tmux.Example.com" matches a browser's lowercase header.
//
// Anything carrying more than an origin -- userinfo, a query, a fragment, a
// path beyond "/" -- yields "". A header like https://tmux.example.com@evil.com
// is safe either way, since url.Parse reads the host as evil.com, but refusing
// to interpret a malformed origin at all is one less thing to reason about.
func wsNormalizeOrigin(s string) string {
	u, err := url.Parse(s)
	if err != nil || u.Scheme == "" || u.Host == "" || u.User != nil ||
		u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return ""
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host)
}

// wsExit is how one loop tells the handler why the connection is over, and
// whether the peer is still there to be told about it.
type wsExit struct {
	status   websocket.StatusCode
	reason   string
	graceful bool
}

// serve runs the connection until one of its three loops ends, then tears the
// other two, the tmux client and the socket down together.
//
// target is a tmux target -- wsSessionTarget's output, an id or an exactly
// matched name -- and not the raw query parameter. The attach is addressed with
// the same string the existence check passed, so `new-session -t` cannot
// prefix-match its way onto a different session than the one this handler
// approved.
func (h *TerminalHandler) serve(ctx context.Context, conn *websocket.Conn, target string) {
	conn.SetReadLimit(wsReadLimit)

	// context.Background, not the request context, and deliberately so. The
	// session's lifetime is owned by Close; ptybridge.Open documents that it
	// does not wire its context to the process, and handing it one that is
	// cancelled when this handler returns would invite exactly that bug back.
	sess, err := ptybridge.Open(context.Background(), ptybridge.Config{
		TmuxArgs: h.cfg.TmuxArgs,
		Base:     target,
		Cols:     wsInitialCols,
		Rows:     wsInitialRows,
	})
	if err != nil {
		slog.Error("terminal: cannot start tmux client", "session", target, "err", err)
		_ = conn.Close(websocket.StatusInternalError, "cannot start tmux client")
		return
	}

	// One cancellation ends all three loops: it aborts a parked Read, Write or
	// Ping -- coder/websocket closes the connection on a context expiry -- and
	// it is the other arm of the write loop's select.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Where a control message bound for the browser is handed to the one
	// goroutine allowed to write. The read loop must not write to the socket
	// itself: coder/websocket supports one concurrent writer, and the write
	// loop below is it.
	ctrl := make(chan []byte, wsControlBuffer)

	// Buffered for all three, so a loop that loses the race to report still
	// returns instead of parking on the send forever.
	done := make(chan wsExit, 3)
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); done <- h.readLoop(ctx, conn, sess, ctrl) }()
	go func() { defer wg.Done(); done <- wsWriteLoop(ctx, conn, sess.Output(), ctrl) }()
	go func() { defer wg.Done(); done <- h.pingLoop(ctx, conn) }()

	exit := <-done

	// The close frame goes out before the cancellation, and it has to: cancel
	// aborts the parked Read, and aborting a read closes the connection, so a
	// frame written afterwards would never leave. Writing it first also means
	// the still-parked reader is what consumes the peer's reply, which is what
	// keeps this from waiting out the library's five-second handshake timeout.
	if exit.graceful {
		_ = conn.Close(exit.status, exit.reason)
	}

	cancel()
	// Nothing this handler started outlives it. Without the wait, a client that
	// vanished would leave three parked goroutines and, through the session
	// they hold, a tmux client per abandoned tab.
	wg.Wait()
	sess.Close()
	_ = conn.CloseNow()
}

// readLoop carries the browser's keystrokes and control messages. It is the
// only caller of conn.Read, which is the one method on a Conn that is not safe
// to call concurrently.
func (h *TerminalHandler) readLoop(ctx context.Context, conn *websocket.Conn, sess *ptybridge.Session, ctrl chan<- []byte) wsExit {
	for {
		typ, msg, err := conn.Read(ctx)
		if err != nil {
			// The peer closed, vanished, or we are shutting down. There is
			// nobody left to send a status to.
			return wsExit{}
		}
		// Binary only. Text frames would force UTF-8 validation on a stream
		// that is not text -- a browser hands them over as strings that have
		// already had invalid bytes replaced, which corrupts escape sequences
		// irrecoverably.
		if typ != websocket.MessageBinary {
			return wsExit{websocket.StatusUnsupportedData, "binary frames only", true}
		}
		kind, payload, err := ptybridge.Decode(msg)
		if err != nil {
			return wsExit{websocket.StatusUnsupportedData, "malformed frame", true}
		}
		switch kind {
		case ptybridge.FrameData:
			if _, err := sess.Write(payload); err != nil {
				return wsExit{} // the PTY is gone; the write loop is ending too
			}
		case ptybridge.FrameControl:
			if exit, fatal := h.control(ctx, sess, payload, ctrl); fatal {
				return exit
			}
		}
	}
}

// wsControlMessage is the JSON inside a control frame. The field names are a
// cross-language contract: the browser transport encodes this independently, so
// renaming one silently breaks the frontend.
//
// Cols and Rows are int rather than uint16 so that an absurd value is a
// semantic problem this handler can ignore, not a decoding error that would
// tear the socket down. A ResizeObserver firing before layout is a plausible
// source of nonsense dimensions, and losing a terminal over one would be
// absurd.
type wsControlMessage struct {
	Type string `json:"type"`
	Cols int    `json:"cols"`
	Rows int    `json:"rows"`
	Pane string `json:"pane"`
}

// wsPaneMessage is the one message this daemon sends to the browser, in answer
// to a "where": the pane the tab is looking at, as a tmux pane id.
//
// It is the only way a tab can know that. Its own throwaway session has a
// current window of its own -- that is what a grouped session is for -- and
// nothing in the snapshot names it: the rows are deduplicated to one per pane
// with the user's own session preferred, so a tab reading `window_active` from
// there would be reading the *local terminal's* current window. See
// tmux.Client.CurrentPane.
//
// The field names are the same cross-language contract as wsControlMessage's;
// `parsePaneMessage` in web/src/components/Terminal.tsx is the other half.
type wsPaneMessage struct {
	Type string `json:"type"`
	Pane string `json:"pane"`
}

// wsPaneType is the "type" of that message. Named because the Go test and the
// TypeScript one both pin the literal, and a rename that changed only one of
// them would leave a tab that never learns where it is.
const wsPaneType = "pane"

// control applies one control message.
//
// The split between fatal and not is deliberate. A payload that is not this
// protocol at all -- unparseable JSON -- means the two ends disagree about the
// wire format, there is nothing to recover to, and ignoring it would leave a
// tab whose resizes quietly stop working with nothing in the log. That closes
// the socket, loudly, and reconnecting costs the user nothing because tmux
// redraws on attach.
//
// A well-formed message this daemon cannot carry out is the opposite case. The
// sidebar is up to 1.5s stale, so selecting a pane that has just died is an
// ordinary race rather than a broken client, and destroying a working terminal
// over it would be hostile. Those are logged and the socket carries on.
func (h *TerminalHandler) control(ctx context.Context, sess *ptybridge.Session, payload []byte, ctrl chan<- []byte) (wsExit, bool) {
	var m wsControlMessage
	if err := json.Unmarshal(payload, &m); err != nil {
		return wsExit{websocket.StatusUnsupportedData, "malformed control message", true}, true
	}

	ctx, cancel := context.WithTimeout(ctx, wsTmuxTimeout)
	defer cancel()

	switch m.Type {
	case "resize":
		// A pty winsize is two uint16s, and a zero one is not a terminal.
		//
		// Both bounds and both axes have to be checked. An earlier version read
		// `m.Cols < 0 && m.Cols > math.MaxUint16`, which no value satisfies, so
		// the guard never fired: cols:-1 reached uint16(-1) == 65535 and
		// resized the pty to 65535x65535, which wedges the client.
		if m.Cols <= 0 || m.Rows <= 0 || m.Cols > math.MaxUint16 || m.Rows > math.MaxUint16 {
			slog.Warn("terminal: ignoring out-of-range resize", "cols", m.Cols, "rows", m.Rows)
			break
		}
		if err := sess.Resize(uint16(m.Cols), uint16(m.Rows)); err != nil {
			slog.Warn("terminal: resize failed", "err", err)
		}
	case "select":
		// Navigates this tab's own session only, so clicking a pane here moves
		// neither the other tabs nor the terminal the user is sitting at.
		if err := sess.SelectPane(ctx, m.Pane); err != nil {
			slog.Warn("terminal: select pane failed", "pane", m.Pane, "err", err)
		}
	case "copy-mode":
		if err := h.copyMode(ctx, sess, m.Pane); err != nil {
			slog.Warn("terminal: copy-mode failed", "pane", m.Pane, "err", err)
		}
	case "where":
		// "Which pane did I land on?", asked once per socket, immediately
		// after the tab has replayed the pane it remembered. Answering *after*
		// that select is what makes one answer cover both cases: the remembered
		// pane when it is still alive, and the pane tmux actually left the tab
		// on when the select failed because that pane had died.
		//
		// A failure is logged and dropped, like every other message this daemon
		// cannot carry out. The tab is then no worse off than it was before
		// this message existed.
		pane, err := sess.CurrentPane(ctx)
		if err != nil {
			slog.Warn("terminal: cannot say which pane this tab is on", "err", err)
			break
		}
		msg, err := json.Marshal(wsPaneMessage{Type: wsPaneType, Pane: pane})
		if err != nil {
			slog.Warn("terminal: cannot encode the pane message", "pane", pane, "err", err)
			break
		}
		select {
		case ctrl <- msg:
		default:
			// A peer that is not draining. Dropping is right: the write loop
			// is already behind, and the ping loop collects a peer that has
			// stopped reading altogether.
			slog.Warn("terminal: dropping the pane message, the browser is not reading", "pane", pane)
		}
	default:
		slog.Warn("terminal: ignoring unknown control message", "type", m.Type)
	}
	return wsExit{}, false
}

// copyMode puts a pane into tmux's copy-mode, which is where this app's
// scrollback lives.
//
// An empty pane means "the pane this tab is looking at": the palette offers
// "enter copy mode" before anything has been clicked, and the tab's own session
// is the one thing that always answers that question.
//
// A non-empty pane must actually be a pane id. `tmux copy-mode -t work` exits 0
// and puts the *user's* current pane into copy-mode, so a frontend that sent a
// session name where a pane id belongs would freeze the terminal they are
// sitting in front of, from a machine they are not. tmux.Client.SelectPane
// guards the same way for the same reason.
func (h *TerminalHandler) copyMode(ctx context.Context, sess *ptybridge.Session, pane string) error {
	// The trailing ":" is what makes this a session target; a bare -t is read
	// as a pane target, and "=name" is not a pane.
	target := "=" + sess.SessionName() + ":"
	if pane != "" {
		// tmux.ValidatePaneID, not a local copy: this rule already exists once
		// in internal/tmux and a second spelling of it here is the one that
		// eventually drifts.
		if err := tmux.ValidatePaneID(pane); err != nil {
			return err
		}
		target = pane
	}
	_, err := h.tm.Run(ctx, "copy-mode", "-t", target)
	return err
}

// wsWriteLoop carries PTY output to the browser, one message per PTY read.
//
// Nothing is buffered here on purpose. The bridge's output channel is already
// bounded and closes the session rather than blocking when it fills, which is
// what stops a wedged browser tab from stalling a tmux client that shares the
// server with the user's own local session. Adding a queue in front of this
// write would defeat that: the backpressure has to reach the bridge.
//
// There is no per-write deadline either. A write that cannot make progress is a
// peer that is not draining, and the ping loop is what notices that -- its own
// control frame queues behind this one and times out. Two overlapping timeouts
// would only make it harder to say which one collected a connection.
//
// ctrl carries control messages for the browser -- today only the answer to a
// "where". They go out through this loop rather than from the read goroutine
// that produced them because coder/websocket supports one writer at a time, and
// two goroutines writing frames onto the same socket is a corrupted stream
// rather than a race that shows up as a test failure.
func wsWriteLoop(ctx context.Context, conn *websocket.Conn, out <-chan []byte, ctrl <-chan []byte) wsExit {
	for {
		select {
		case <-ctx.Done():
			return wsExit{}
		case m := <-ctrl:
			if err := conn.Write(ctx, websocket.MessageBinary, ptybridge.EncodeControl(m)); err != nil {
				return wsExit{}
			}
		case b, ok := <-out:
			if !ok {
				// The session ended: the user typed exit, the base session was
				// killed, or the bridge closed itself. Say so, so the browser
				// can tell this from a network fault it should reconnect after.
				return wsExit{websocket.StatusNormalClosure, "session ended", true}
			}
			if err := conn.Write(ctx, websocket.MessageBinary, ptybridge.EncodeData(b)); err != nil {
				return wsExit{}
			}
		}
	}
}

// pingLoop is the only thing that can tell a quiet terminal from a dead peer.
// See wsPingInterval for why that distinction is load-bearing rather than
// hygienic.
func (h *TerminalHandler) pingLoop(ctx context.Context, conn *websocket.Conn) wsExit {
	interval, timeout := h.cfg.PingInterval, h.cfg.PingTimeout
	if interval <= 0 {
		interval = wsPingInterval
	}
	if timeout <= 0 {
		timeout = wsPingTimeout
	}

	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return wsExit{}
		case <-t.C:
			pctx, cancel := context.WithTimeout(ctx, timeout)
			err := conn.Ping(pctx)
			cancel()
			if err != nil {
				// Not graceful: a close handshake with a peer that just failed
				// to answer a ping would only wait out its own timeout.
				return wsExit{}
			}
		}
	}
}
