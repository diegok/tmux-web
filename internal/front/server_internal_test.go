package front

// These tests are inside the package because what they check is the wiring
// rather than the API: which allowlist the terminal socket ends up with, what
// the startup sweep does with a failure, and which origin an enrollment link
// names. All three are decisions Serve makes before it touches the network, and
// a test that had to start a daemon on a real port to see them is a test that
// would be written once and then skipped.

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/diegok/tmux-web/internal/tmux/testutil"
)

// captureLogs redirects the default logger for the duration of a test.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

type fakeSweeper struct {
	err      error
	called   bool
	deadline bool
}

func (f *fakeSweeper) Sweep(ctx context.Context) error {
	f.called = true
	_, f.deadline = ctx.Deadline()
	return f.err
}

// An uncollectable orphan leaks one tmux session. Refusing to start leaves the
// owner with no way in at all -- and the conditions that produce a sweep error,
// an unreadable socket for instance, persist across restarts, so a fatal sweep
// would be a permanently unstartable daemon.
func TestASweepFailureIsLoggedAndStartupCarriesOn(t *testing.T) {
	logs := captureLogs(t)
	s := &fakeSweeper{err: errors.New("connect: permission denied")}

	sweepOrphans(context.Background(), s)

	if !s.called {
		t.Fatal("the sweep did not run")
	}
	if got := logs.String(); !strings.Contains(got, "permission denied") {
		t.Fatalf("the sweep failure was swallowed: %q", got)
	}
}

// A cold start has no tmux server, Sweep reports that as nil, and a daemon that
// logged a warning every time it started on a fresh machine would train its
// owner to ignore the log.
func TestAColdStartSweepLogsNothing(t *testing.T) {
	logs := captureLogs(t)
	sweepOrphans(context.Background(), &fakeSweeper{})
	if got := logs.String(); got != "" {
		t.Fatalf("a successful sweep logged %q", got)
	}
}

// The sweep runs before the daemon serves anything, so a tmux server that never
// answers must not hold the daemon down.
func TestTheSweepCannotHoldStartupOpenForever(t *testing.T) {
	s := &fakeSweeper{}
	sweepOrphans(context.Background(), s)
	if !s.deadline {
		t.Fatal("the startup sweep was given no deadline")
	}
}

// -- the dev switch ---------------------------------------------------------

func devDaemon(t *testing.T, dev bool) (*daemon, string) {
	t.Helper()
	d, err := newDaemon(Config{
		Host:      "tmux.example.com",
		Dev:       dev,
		Port:      7000,
		StatePath: filepath.Join(t.TempDir(), "devices.json"),
	})
	if err != nil {
		t.Fatalf("newDaemon: %v", err)
	}
	token, err := d.store.AddDevice("laptop", "Go test")
	if err != nil {
		t.Fatal(err)
	}
	return d, token
}

func request(t *testing.T, d *daemon, method, target, token, origin string) int {
	t.Helper()
	r := httptest.NewRequest(method, target, strings.NewReader(`{"name":"x"}`))
	r.Header.Set("Content-Type", "application/json")
	r.AddCookie(&http.Cookie{Name: DeviceCookieName, Value: token})
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	rec := httptest.NewRecorder()
	d.handler.ServeHTTP(rec, r)
	return rec.Code
}

// AllowedOrigins replaces the production origin in dev mode rather than adding
// to it, and this is the test that the daemon's wiring actually passes cfg.Dev
// through rather than deriving the allowlist from the host alone. Adding
// instead of replacing would mean a production daemon accepting requests from a
// page on the developer's laptop, which is a hole rather than a dev switch.
func TestTheDevSwitchReplacesTheProductionOriginRatherThanJoiningIt(t *testing.T) {
	dev, token := devDaemon(t, true)
	if code := request(t, dev, "POST", "/api/devices", token, "https://tmux.example.com"); code != http.StatusForbidden {
		t.Errorf("a --dev daemon accepted the production origin: %d", code)
	}
	for _, o := range []string{"http://localhost:7000", "http://127.0.0.1:7000", "http://[::1]:7000"} {
		if code := request(t, dev, "POST", "/api/devices", token, o); code != http.StatusOK {
			t.Errorf("a --dev daemon refused %s: %d", o, code)
		}
	}

	prod, token := devDaemon(t, false)
	if code := request(t, prod, "POST", "/api/devices", token, "http://localhost:7000"); code != http.StatusForbidden {
		t.Errorf("a production daemon accepted a loopback origin: %d", code)
	}
	if code := request(t, prod, "POST", "/api/devices", token, "https://tmux.example.com"); code != http.StatusOK {
		t.Errorf("a production daemon refused its own origin: %d", code)
	}
}

// The terminal socket checks the handshake itself, inside the upgrade, and it
// has to ask the same question of the same allowlist -- a second copy of the
// rule is a second thing to keep in step with --dev, and the failure mode is a
// dev machine where the sidebar works and the terminal does not.
//
// 400 is the pass here: it is what the socket answers once the origin check is
// behind it and the session parameter is missing, and it means no tmux was
// forked to get the answer.
func TestTheTerminalSocketSharesTheOriginAllowlist(t *testing.T) {
	dev, token := devDaemon(t, true)
	for _, o := range []string{"http://localhost:7000", "http://127.0.0.1:7000", "http://[::1]:7000"} {
		if code := request(t, dev, "GET", "/ws", token, o); code != http.StatusBadRequest {
			t.Errorf("the terminal socket refused %s: %d", o, code)
		}
	}
	if code := request(t, dev, "GET", "/ws", token, "https://tmux.example.com"); code != http.StatusForbidden {
		t.Errorf("the terminal socket accepted the production origin in dev: %d", code)
	}

	prod, token := devDaemon(t, false)
	if code := request(t, prod, "GET", "/ws", token, "https://tmux.example.com"); code != http.StatusBadRequest {
		t.Errorf("the terminal socket refused its own origin: %d", code)
	}
	if code := request(t, prod, "GET", "/ws", token, "http://127.0.0.1:7000"); code != http.StatusForbidden {
		t.Errorf("the terminal socket accepted a loopback origin in production: %d", code)
	}
}

// An enrollment link that names an origin the daemon does not allow is a link
// that cannot be redeemed: the page loads, the POST is refused, and the token
// is spent on nothing. The two must be derived together.
func TestEnrollmentLinksNameAnOriginTheDaemonAccepts(t *testing.T) {
	for _, dev := range []bool{false, true} {
		cfg := Config{Host: "tmux.example.com", Dev: dev, Port: 7000}
		origins, err := AllowedOrigins(cfg.Host, cfg.Dev, cfg.Port)
		if err != nil {
			t.Fatal(err)
		}
		base := baseURL(cfg)
		if !slices.Contains(origins, base) {
			t.Errorf("dev=%v: enrollment links name %s, which is not in %v", dev, base, origins)
		}
	}
}

// -- the admin wrapper ------------------------------------------------------

func TestOnlyADeviceRevocationTriggersTheSweep(t *testing.T) {
	cases := []struct {
		method, path string
		id           string
		want         bool
	}{
		{"DELETE", "/devices/dev-1", "dev-1", true},
		{"DELETE", "/devices/dev-1/", "dev-1", true},
		{"DELETE", "/devices/", "", false},
		{"DELETE", "/devices", "", false},
		{"DELETE", "/devices/dev-1/extra", "", false},
		{"GET", "/devices/dev-1", "", false},
		{"POST", "/enroll", "", false},
	}
	for _, c := range cases {
		r := httptest.NewRequest(c.method, c.path, nil)
		id, ok := revokedDeviceID(r)
		if ok != c.want || (ok && id != c.id) {
			t.Errorf("revokedDeviceID(%s %s) = %q, %v; want %q, %v", c.method, c.path, id, ok, c.id, c.want)
		}
	}
}

// -- assorted ---------------------------------------------------------------

func TestAssetNameRefusesToEscapeTheEmbeddedTree(t *testing.T) {
	for _, p := range []string{"/", "/../go.mod", "/assets/../../secret", "//", "/."} {
		name, ok := assetName(p)
		if ok && (strings.Contains(name, "..") || name == "") {
			t.Errorf("assetName(%q) = %q, %v", p, name, ok)
		}
	}
	if name, ok := assetName("/assets/index-abc.js"); !ok || name != "assets/index-abc.js" {
		t.Errorf("assetName(/assets/index-abc.js) = %q, %v", name, ok)
	}
}

func TestPollIntervalDefaults(t *testing.T) {
	d, err := newDaemon(Config{Host: "tmux.example.com", StatePath: filepath.Join(t.TempDir(), "d.json")})
	if err != nil {
		t.Fatal(err)
	}
	if d.poller == nil {
		t.Fatal("no poller")
	}
	if DefaultPollInterval != 1500*time.Millisecond {
		t.Fatalf("DefaultPollInterval = %v; the sidebar polls at 1.5s and the design costs it at one fork per interval", DefaultPollInterval)
	}
}

// -- the poller's second job ------------------------------------------------

// Agent state exists only if newDaemon actually hands the poller a way to
// capture panes and a way to ask whether anyone is connected. Both are one line
// each, neither has any other caller, and a poller built without them classifies
// nothing while every test in tmux and front stays green -- so the wiring is
// checked here, through the real daemon, against a real tmux.
//
// The agent pane is a copy of cat named "claude", so this needs no TUI
// installed and still goes through the real tmux.Agents list.
func TestDaemonPollerClassifiesAgentPanesWhileAClientIsConnected(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "work", "-x", "40", "-y", "10",
		testutil.FakeAgent(t, "claude"))

	d, err := newDaemon(Config{
		Host:         "tmux.example.com",
		Dev:          true,
		Port:         7000,
		StatePath:    filepath.Join(t.TempDir(), "devices.json"),
		TmuxArgs:     srv.Args(),
		PollInterval: 25 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("newDaemon: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.poller.Start(ctx)

	agentState := func() string {
		for _, r := range d.poller.Latest() {
			if r.Command == "claude" {
				return r.AgentState
			}
		}
		return "<no agent pane>"
	}

	// Nobody is connected, so nothing is captured and nothing is classified.
	// Given a moment, in case a first poll could sneak a state in.
	time.Sleep(100 * time.Millisecond)
	if got := agentState(); got != "" {
		t.Fatalf("agentState = %q with no browser connected, want empty", got)
	}

	// A tab opens: the registry is what the poller asks, so registering one
	// connection is enough to turn capturing on.
	remove, ok := d.registry.Add("laptop", func() {})
	if !ok {
		t.Fatal("registry refused a fresh registration")
	}
	defer remove()

	deadline := time.Now().Add(5 * time.Second)
	for agentState() == "" {
		if time.Now().After(deadline) {
			t.Fatal("no state was ever computed for an agent pane with a client connected")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// ... and it goes away again with the last tab, rather than freezing at
	// whatever it last was.
	remove()
	deadline = time.Now().Add(5 * time.Second)
	for agentState() != "" {
		if time.Now().After(deadline) {
			t.Fatalf("agentState = %q after the last client left, want empty", agentState())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A sidebar click costs one tmux fork only if the terminal handler was actually
// given the poller's window cache. Nothing else observes the wiring: the click
// lands on the right pane either way -- tmux.Client.SelectPane reads the id when
// it has no hint -- so every behavioural test in front, ptybridge and tmux stays
// green with this one line deleted, and the daemon quietly pays three forks per
// click again.
func TestDaemonGivesTheTerminalThePollersWindowCache(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "work", "-x", "40", "-y", "10")
	srv.Run(t, "new-window", "-t", "work")
	pane := srv.Run(t, "list-panes", "-t", "=work:1", "-F", "#{pane_id}")
	window := srv.Run(t, "list-panes", "-t", "=work:1", "-F", "#{window_id}")

	d, err := newDaemon(Config{
		Host:         "tmux.example.com",
		Dev:          true,
		Port:         7000,
		StatePath:    filepath.Join(t.TempDir(), "devices.json"),
		TmuxArgs:     srv.Args(),
		PollInterval: 25 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("newDaemon: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.poller.Start(ctx)

	deadline := time.Now().Add(5 * time.Second)
	for len(d.poller.Latest()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the poller never produced a snapshot")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if got := d.terminal.windowFor(pane); got != window {
		t.Errorf("the terminal handler answers %q for pane %s, want %q -- it is not reading the poller",
			got, pane, window)
	}
	if got := d.terminal.windowFor("%9999"); got != "" {
		t.Errorf("the terminal handler answers %q for a pane no poll ever saw, want \"\"", got)
	}
}

// The cache's second return value is the whole answer when the first one is not
// empty. A poller answers ("", false) for a pane it never saw, so a handler that
// ignored ok would look correct against one -- but the field is a func, and any
// cache that answers "here is a value, but no" must be believed on the "no".
// The value it did offer would otherwise become a tmux target.
func TestTerminalWindowForBelievesTheMiss(t *testing.T) {
	for _, tc := range []struct {
		name   string
		window string
		ok     bool
		want   string
	}{
		{"a hit", "@7", true, "@7"},
		{"a miss carrying a value anyway", "@7", false, ""},
		{"a hit with no value", "", true, ""},
		{"a miss", "", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := NewTerminalHandler(TerminalConfig{
				AllowedOrigin: "https://tmux.example.com",
				WindowFor:     func(string) (string, bool) { return tc.window, tc.ok },
			})
			if got := h.windowFor("%1"); got != tc.want {
				t.Errorf("windowFor = %q, want %q", got, tc.want)
			}
		})
	}
	// No cache at all is a miss, not a panic: a handler built without a poller
	// behind it still has to serve terminals.
	h := NewTerminalHandler(TerminalConfig{AllowedOrigin: "https://tmux.example.com"})
	if got := h.windowFor("%1"); got != "" {
		t.Errorf("windowFor with no cache = %q, want \"\"", got)
	}
}
