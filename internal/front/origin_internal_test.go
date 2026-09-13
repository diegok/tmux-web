package front

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/diegok/tmux-web/internal/auth"
)

// These are the tests for the port a browser has to reach this daemon on.
//
// The bug they were written for: --tls-port moved the listener and nothing
// else, so `tmux-web enroll` printed https://host/enroll#... for a daemon that
// was answering on 8443, and correcting the port by hand did not help either --
// the browser then sent Origin: https://host:8443, which the allowlist refused.
// With a non-default --tls-port, enrolment was impossible.
//
// So each case here walks the whole path rather than comparing two helpers with
// each other: mint a link the way the CLI does, read the origin off the link
// the way a browser does, and redeem it against the real handler. The expected
// link is spelled out as a literal; nothing in the assertions is computed by
// the code under test.

// enrol mints an enrollment link over the admin mux -- the `tmux-web enroll`
// path -- opens it as a browser would, and returns the link and the status the
// daemon answered the redemption with.
func enrol(t *testing.T, cfg Config) (link string, code int) {
	t.Helper()
	if cfg.StatePath == "" {
		cfg.StatePath = filepath.Join(t.TempDir(), "devices.json")
	}
	d, err := newDaemon(cfg)
	if err != nil {
		t.Fatalf("newDaemon: %v", err)
	}

	// Exactly the wiring Serve uses for the admin socket.
	admin := auth.AdminMux(auth.AdminConfig{
		Store:    d.store,
		Enroller: d.enroller,
		BaseURL:  d.baseURL,
	})
	rec := httptest.NewRecorder()
	admin.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/enroll", strings.NewReader(`{"name":"phone"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("minting a link: %d %s", rec.Code, rec.Body.String())
	}
	var minted struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &minted); err != nil {
		t.Fatalf("minted link: %v", err)
	}

	// What the browser does with the link it was given: the address bar decides
	// the Origin header, and url.Host carries the port only when the link does.
	u, err := url.Parse(minted.URL)
	if err != nil {
		t.Fatalf("the minted link does not parse: %v", err)
	}
	origin := u.Scheme + "://" + u.Host

	r := httptest.NewRequest(http.MethodPost, "/api/enroll", strings.NewReader(`{"token":"`+u.Fragment+`"}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", origin)
	redeemed := httptest.NewRecorder()
	d.handler.ServeHTTP(redeemed, r)
	return minted.URL, redeemed.Code
}

// An enrollment link that names an origin the daemon does not allow is a link
// that cannot be redeemed: the page loads, the POST is refused, and the token
// is spent on nothing. So the link and the allowlist are one derivation -- and
// the derivation has to name the port something is actually listening on, which
// is where --tls-port came unstuck.
//
// Every row spells the expected link out. A row that computed it would pass
// whatever the code did, which is how a daemon that printed
// https://host/enroll for a listener on 8443 kept a green suite: the link and
// the allowlist agreed with each other, and both were wrong.
func TestEnrollmentLinksNameTheOriginTheDaemonServesAndAccepts(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
		want string
	}{
		{
			// The reproduction: --self-signed on an unprivileged port.
			name: "self-signed on a non-default port",
			cfg:  Config{Host: "tmux.example.com", SelfSigned: true, TLSPort: 8443, Port: 7000},
			want: "https://tmux.example.com:8443/enroll#",
		},
		{
			// --tls-cert takes the same --tls-port, so it had the same bug.
			name: "an own certificate on a non-default port",
			cfg: Config{
				Host: "tmux.example.com", TLSCert: "/etc/tmux-web/cert.pem",
				TLSKey: "/etc/tmux-web/key.pem", TLSPort: 8443, Port: 7000,
			},
			want: "https://tmux.example.com:8443/enroll#",
		},
		{
			name: "self-signed on the default port",
			cfg:  Config{Host: "tmux.example.com", SelfSigned: true, TLSPort: 443, Port: 7000},
			want: "https://tmux.example.com/enroll#",
		},
		{
			name: "self-signed with no port configured",
			cfg:  Config{Host: "tmux.example.com", SelfSigned: true, Port: 7000},
			want: "https://tmux.example.com/enroll#",
		},
		{
			// certmagic owns its listeners and serves 443; a link naming
			// --tls-port here would name a port nothing answers on. The CLI
			// refuses the combination, but front.Config is reachable without
			// the CLI, so the daemon still has to be right.
			name: "the ACME path ignores --tls-port",
			cfg:  Config{Host: "tmux.example.com", TLSPort: 8443, Port: 7000},
			want: "https://tmux.example.com/enroll#",
		},
		{
			// --dev has its own port and its own allowlist, and serves no
			// certificate at all: --tls-port has nothing to say about it. The
			// host is localhost rather than the configured name because the
			// dev allowlist is the loopback origins.
			name: "--dev keeps its own port",
			cfg:  Config{Host: "tmux.example.com", Dev: true, Port: 7000, TLSPort: 8443},
			want: "http://localhost:7000/enroll#",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			link, code := enrol(t, tc.cfg)
			if !strings.HasPrefix(link, tc.want) {
				t.Errorf("enrollment link = %q, want it to start %q", link, tc.want)
			}
			if code != http.StatusOK {
				t.Errorf("redeeming the link the daemon itself printed = %d, want 200", code)
			}
		})
	}
}

// What the own-certificate listener would bind, without binding it: the
// enrollment link is built from servingPort, and so must the listener be, or a
// person is handed a URL to a port nothing answers on. 443 cannot be bound in a
// test, so the default case is checked here and the reachable case below.
func TestTheOwnCertListenerTakesItsAddressFromTheServingPort(t *testing.T) {
	for _, tc := range []struct {
		cfg  Config
		want string
	}{
		{Config{Host: "tmux.example.com", SelfSigned: true, TLSPort: 8443, Port: 7000}, ":8443"},
		{Config{Host: "tmux.example.com", SelfSigned: true, Port: 7000}, ":443"},
		{Config{Host: "tmux.example.com", TLSCert: "c.pem", TLSKey: "k.pem", TLSPort: 8443}, ":8443"},
	} {
		if got := ownCertServer(tc.cfg, nil).Addr; got != tc.want {
			t.Errorf("listener address for %+v = %q, want %q", tc.cfg, got, tc.want)
		}
	}
}

// The port in the link has to be the port the listener is on. Everything else
// here checks that the link and the allowlist agree with each other; this is the
// one that checks they agree with reality, which is what --tls-port broke.
//
// It binds a port, so it takes one the kernel just handed back as free, and it
// asks the daemon for the port by reading the link rather than by reading the
// config it was given.
func TestTheListenerAnswersOnThePortTheLinkNames(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot bind a loopback port here: %v", err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	probe.Close()

	dir := t.TempDir()
	cfg := Config{
		Host:       "tmux.example.com",
		SelfSigned: true,
		TLSPort:    port,
		Port:       7000,
		StatePath:  filepath.Join(dir, "devices.json"),
	}

	link, code := enrol(t, cfg)
	if code != http.StatusOK {
		t.Fatalf("redeeming the daemon's own link = %d, want 200", code)
	}
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	served := make(chan error, 1)
	go func() {
		served <- serveOwnCert(ctx, cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusTeapot)
		}))
	}()

	// The certificate is this daemon's own, and checking it is the browser's
	// job elsewhere; here the question is only which port answers.
	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}}
	deadline := time.Now().Add(10 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		resp, err := client.Get("https://127.0.0.1:" + u.Port() + "/")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode != http.StatusTeapot {
				t.Fatalf("something else is on port %s: %d", u.Port(), resp.StatusCode)
			}
			return
		}
		last = err
		select {
		case err := <-served:
			t.Fatalf("the daemon stopped serving: %v", err)
		case <-time.After(50 * time.Millisecond):
		}
	}
	t.Fatalf("nothing answered on the port the enrollment link names (%s): %v", u.Port(), last)
}

// 443 must be absent from the allowlist as well as from the link: a browser
// omits the default port when it serializes an origin, and this app compares
// origins byte-exactly, so "https://host:443" would sit there matching nothing
// while looking like it permitted something.
func TestTheDefaultHTTPSPortIsWrittenNowhere(t *testing.T) {
	for _, tlsPort := range []int{0, 443} {
		origins, err := AllowedOrigins(Config{Host: "tmux.example.com", SelfSigned: true, TLSPort: tlsPort})
		if err != nil {
			t.Fatalf("AllowedOrigins: %v", err)
		}
		if !slices.Equal(origins, []string{"https://tmux.example.com"}) {
			t.Errorf("--tls-port %d: allowlist = %v, want exactly [https://tmux.example.com]", tlsPort, origins)
		}
	}
}

// The dev allowlist is three loopback origins on the dev port, and --tls-port
// must not reach any of them.
func TestTheDevAllowlistIgnoresTheTLSPort(t *testing.T) {
	origins, err := AllowedOrigins(Config{Host: "tmux.example.com", Dev: true, Port: 7000, TLSPort: 8443})
	if err != nil {
		t.Fatalf("AllowedOrigins: %v", err)
	}
	want := []string{"http://localhost:7000", "http://127.0.0.1:7000", "http://[::1]:7000"}
	if !slices.Equal(origins, want) {
		t.Errorf("dev allowlist = %v, want %v", origins, want)
	}
}

// The allowlist is exact, and that exactness is the CSRF boundary: an origin
// that names the default port explicitly is a different string, and no browser
// sends it. Accepting it would mean the comparison had been loosened.
func TestAnExplicitDefaultPortIsStillARefusedOrigin(t *testing.T) {
	d, err := newDaemon(Config{
		Host:       "tmux.example.com",
		SelfSigned: true,
		StatePath:  filepath.Join(t.TempDir(), "devices.json"),
	})
	if err != nil {
		t.Fatalf("newDaemon: %v", err)
	}
	token, err := d.store.AddDevice("laptop", "Go test")
	if err != nil {
		t.Fatal(err)
	}
	if code := request(t, d, "POST", "/api/devices", token, "https://tmux.example.com:443"); code != http.StatusForbidden {
		t.Errorf("an origin naming the default port explicitly was accepted: %d", code)
	}
}

// A non-default port is one origin, not two: adding the bare host beside it
// would let a page served on 443 by anything else on the box post here.
func TestANonDefaultPortDoesNotAlsoAdmitTheBareHost(t *testing.T) {
	d, err := newDaemon(Config{
		Host:       "tmux.example.com",
		SelfSigned: true,
		TLSPort:    8443,
		StatePath:  filepath.Join(t.TempDir(), "devices.json"),
	})
	if err != nil {
		t.Fatalf("newDaemon: %v", err)
	}
	token, err := d.store.AddDevice("laptop", "Go test")
	if err != nil {
		t.Fatal(err)
	}
	if code := request(t, d, "POST", "/api/devices", token, "https://tmux.example.com"); code != http.StatusForbidden {
		t.Errorf("a daemon on 8443 accepted an origin with no port: %d", code)
	}
	if code := request(t, d, "POST", "/api/devices", token, "https://tmux.example.com:8443"); code != http.StatusOK {
		t.Errorf("a daemon on 8443 refused its own origin: %d", code)
	}
}

// The terminal socket takes the same allowlist, so it moves with the port too.
// 400 is the pass: it is what the socket answers once the origin check is
// behind it and the session parameter is missing.
func TestTheTerminalSocketFollowsTheServingPort(t *testing.T) {
	d, err := newDaemon(Config{
		Host:       "tmux.example.com",
		SelfSigned: true,
		TLSPort:    8443,
		StatePath:  filepath.Join(t.TempDir(), "devices.json"),
	})
	if err != nil {
		t.Fatalf("newDaemon: %v", err)
	}
	token, err := d.store.AddDevice("laptop", "Go test")
	if err != nil {
		t.Fatal(err)
	}
	if code := request(t, d, "GET", "/ws", token, "https://tmux.example.com:8443"); code != http.StatusBadRequest {
		t.Errorf("the terminal socket refused the daemon's own origin: %d", code)
	}
	if code := request(t, d, "GET", "/ws", token, "https://tmux.example.com"); code != http.StatusForbidden {
		t.Errorf("the terminal socket accepted an origin with no port on a daemon serving 8443: %d", code)
	}
}

// A port that cannot be listened on must stop the daemon at startup, next to
// the rest of the allowlist's fail-closed checks, rather than producing an
// origin no browser can send.
func TestASillyTLSPortIsRefusedAtStartup(t *testing.T) {
	for _, port := range []int{-1, 70000} {
		if _, err := AllowedOrigins(Config{Host: "tmux.example.com", SelfSigned: true, TLSPort: port}); err == nil {
			t.Errorf("--tls-port %d was accepted", port)
		}
	}
}
