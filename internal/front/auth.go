// Package front is the HTTP layer: the device cookie, the CSRF boundary, the
// terminal socket, and the embedded SPA.
//
// Two decisions in this file are load-bearing and the rest of the app depends
// on them holding.
//
// The device cookie is named __Host-wterm_device. The prefix is not decoration.
// A host-only cookie is already unreadable by a service later published on a
// sibling subdomain such as test.example.com; the prefix additionally stops
// that sibling from *setting* a Domain=.example.com cookie of the same name
// that shadows the real one, because browsers reject a __Host- cookie carrying
// a Domain attribute. The shadowing attack is therefore closed at the browser
// rather than guarded against in parsing code here.
//
// Exact-Origin checking -- not SameSite -- is the CSRF boundary. SameSite is
// computed on the registrable domain, not on the host, so test.example.com and
// tmux.example.com are the *same site*: a service under test, which is
// untrusted code by definition, can serve a page whose same-site POSTs to this
// app carry the device cookie. CORS stops that page reading the response; it
// does not stop the request executing. Origin is scheme + host + port, so
// https://test.example.com does not match https://tmux.example.com, and an
// exact comparison against a configured allowlist is what closes it.
package front

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/diegok/tmux-web/internal/auth"
)

// DeviceCookieName is the wire name of the device cookie. Changing it signs out
// every enrolled browser, which is why the tests pin the literal string.
const DeviceCookieName = "__Host-wterm_device"

// deviceCookieMaxAge is how long the browser keeps the cookie. Device sessions
// themselves never expire -- revocation is the control -- so the cookie only
// needs to outlive the browser process: a session cookie would sign the owner
// out on every browser restart, and re-enrolling costs them an ssh session.
//
// 400 days rather than something rounder because Chrome caps cookie lifetime at
// 400 days and silently truncates anything longer; claiming ten years would
// just be a lie told to the code reader.
const deviceCookieMaxAge = 400 * 24 * 60 * 60

// TouchInterval is how stale a device's last-seen timestamp may get. Touch
// rewrites and fsyncs the whole device file, so it must not sit on every
// request; five minutes bounds the write rate at one per active device per five
// minutes and bounds the error in the devices list by the same amount. The
// list exists so the owner can recognise a device, not to audit its traffic.
const TouchInterval = 5 * time.Minute

// DeviceStore is the part of *auth.Store this layer uses. It is declared here,
// at the consumer, so a test can count writes and make them fail.
type DeviceStore interface {
	// Lookup resolves a device token. It performs no write.
	Lookup(token string) (auth.Device, bool)
	// Touch records that a device was seen. Its failure is not fatal here.
	Touch(id string, t time.Time) error
}

// Auth turns a device token in a cookie into an authenticated request, and
// refuses any request whose Origin is not on its allowlist.
//
// The allowlist comes from configuration and never from the request: r.Host,
// X-Forwarded-Host and the rest are written by whoever is calling, so deriving
// the expected origin from them would let an attacker's page satisfy the check
// by talking to itself.
type Auth struct {
	store   DeviceStore
	origins []string

	now func() time.Time

	// mu guards touched, a per-device record of when last-seen was last
	// written. It is bounded by the number of enrolled devices -- a handful --
	// and an entry for a revoked device is a few dozen bytes that go away on
	// restart, so it is never pruned.
	mu      sync.Mutex
	touched map[string]time.Time
}

// NewAuth builds the middleware. origins are exact origin serializations,
// scheme://host[:port], as a browser would send them; use AllowedOrigins to
// derive them from the configured host.
//
// They are parsed here, once, at startup and against trusted input. Nothing
// parses the Origin header at request time: URL parsing is where this class of
// bug lives, and an exact string comparison has nowhere to hide one.
func NewAuth(store DeviceStore, origins []string) (*Auth, error) {
	if store == nil {
		return nil, errors.New("front: nil device store")
	}
	if len(origins) == 0 {
		// An empty allowlist would match nothing, which fails closed, but it
		// fails closed silently and at the worst moment. Refuse to start.
		return nil, errors.New("front: empty origin allowlist")
	}
	for _, o := range origins {
		if err := validOrigin(o); err != nil {
			return nil, err
		}
	}
	return &Auth{
		store:   store,
		origins: slices.Clone(origins),
		now:     time.Now,
		touched: make(map[string]time.Time),
	}, nil
}

// validOrigin rejects anything a browser would never send, because such an
// entry can only ever be a misconfiguration: it would sit in the allowlist
// matching nothing while looking like it permits something.
func validOrigin(o string) error {
	u, err := url.Parse(o)
	if err != nil {
		return fmt.Errorf("front: %q is not an origin: %w", o, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("front: origin %q must be http or https", o)
	}
	if u.Host == "" || u.User != nil || u.Opaque != "" ||
		u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("front: origin %q must be exactly scheme://host[:port]", o)
	}
	// Re-serialize and compare: this catches a trailing slash, a stray "?" and
	// anything else url.Parse tolerated but a browser would not produce.
	if want := u.Scheme + "://" + u.Host; want != o {
		return fmt.Errorf("front: origin %q is not in a browser's serialization (%q)", o, want)
	}
	return nil
}

// AllowsOrigin reports whether an Origin header value is the app's own origin.
//
// The comparison is byte-exact. Browsers serialize an origin with a lowercase
// scheme and host and omit the default port, so there is nothing to normalize;
// every looser comparison -- suffix, prefix, "ends with the registrable domain"
// -- admits exactly the sibling-subdomain page this check exists to refuse.
//
// It is exported because the WebSocket handler checks the handshake itself,
// inside the upgrade, and must ask the same question of the same allowlist.
func (a *Auth) AllowsOrigin(origin string) bool {
	return slices.Contains(a.origins, origin)
}

// Origins returns a copy of the allowlist.
func (a *Auth) Origins() []string { return slices.Clone(a.origins) }

// Protect requires a valid device cookie, and an Origin that matches on every
// request that could change something. See originOK for what "could change
// something" means.
func (a *Auth) Protect(next http.Handler) http.Handler {
	return a.protect(next, false)
}

// ProtectSocket is Protect for the WebSocket handshake, which additionally must
// carry an Origin.
//
// The handshake is a GET, and browsers attach cookies to cross-origin WebSocket
// handshakes, so a page on test.example.com can open a socket to this app and
// have it authenticated. Letting it through because "GET is safe" would hand
// that page a shell -- the single most state-changing thing here. Browsers
// always send Origin on a handshake, so requiring it costs a real client
// nothing and removes the exemption entirely.
func (a *Auth) ProtectSocket(next http.Handler) http.Handler {
	return a.protect(next, true)
}

func (a *Auth) protect(next http.Handler, originRequired bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// These responses are per-credential and per-origin. There is no shared
		// cache in the deployment, but a 401 cached for an authenticated user
		// is a bad way to find out that assumption changed.
		w.Header().Add("Vary", "Cookie")
		w.Header().Add("Vary", "Origin")

		// Origin first: a forged-origin request is refused on the request
		// alone, so it never reaches the store, cannot time a lookup, and
		// cannot move a last-seen timestamp.
		if !a.originOK(r, originRequired) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		// The first cookie of that name is the only one considered, and there
		// is deliberately no scan for duplicates: a second __Host-wterm_device
		// would have to be set by a page on this exact host, and HttpOnly stops
		// script there overwriting it. The shadowing attack a sibling could
		// otherwise mount is refused by the browser, not sorted out here.
		c, err := r.Cookie(DeviceCookieName)
		if err != nil {
			// No cookie, or a malformed one. Both are "not signed in".
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		// Lookup compares digests in constant time and knows nothing of revoked
		// devices because Revoke removes them: a revoked token simply stops
		// matching. Severing its live sockets is the registry's job.
		d, ok := a.store.Lookup(c.Value)
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		a.touch(d.ID)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), deviceContextKey{}, d)))
	})
}

// originOK applies the CSRF boundary.
//
// A present Origin must match exactly, whatever the method. Our own pages send
// either no Origin or ours, so refusing a foreign Origin on a GET breaks
// nothing and closes the read-shaped requests -- the WebSocket handshake above
// all -- that a "GET is safe" rule would wave through.
//
// An absent Origin is tolerated only on GET and HEAD, because ordinary
// navigation (a bookmark, a typed URL, a same-origin fetch) carries none.
// Everything else fails closed, including OPTIONS and TRACE, which RFC 9110
// calls safe: this app answers neither, so nothing is lost by refusing them,
// and an extension method's semantics are unknowable from here. The cost is
// that a non-browser client -- curl, a script -- must send an Origin header to
// POST. That is the deliberate trade: there is no way to tell that client from
// a browser being driven by someone else's page.
func (a *Auth) originOK(r *http.Request, originRequired bool) bool {
	got := r.Header.Values("Origin")
	switch len(got) {
	case 0:
		if originRequired {
			return false
		}
		return r.Method == http.MethodGet || r.Method == http.MethodHead
	case 1:
		return a.AllowsOrigin(got[0])
	default:
		// A request with two Origin headers has no single origin to check.
		// Browsers do not produce it; something in the path that folds them
		// together might, and a check that picked one of them would be
		// picking which attacker to believe.
		return false
	}
}

// touch records the device as seen, at most once per TouchInterval.
//
// The store write happens outside the lock so one fsync does not serialize
// every request, and the marker is set before releasing it so a burst of
// concurrent requests produces exactly one write.
func (a *Auth) touch(id string) {
	// time.Now carries a monotonic reading and Sub prefers it, so a wall-clock
	// jump cannot wedge the throttle open or shut.
	now := a.now()

	a.mu.Lock()
	if last, ok := a.touched[id]; ok && now.Sub(last) < TouchInterval {
		a.mu.Unlock()
		return
	}
	a.touched[id] = now
	a.mu.Unlock()

	if err := a.store.Touch(id, now); err != nil {
		// last-seen is decoration. A full disk must not sign the owner out of
		// the tool they would use to notice. Drop the marker so the next
		// request retries rather than waiting out the interval.
		slog.Warn("front: recording device last-seen failed", "device", id, "error", err)
		a.mu.Lock()
		delete(a.touched, id)
		a.mu.Unlock()
	}
}

type deviceContextKey struct{}

// DeviceFrom returns the device that authenticated the request. Handlers behind
// Protect can rely on it; anything else must check ok.
func DeviceFrom(ctx context.Context) (auth.Device, bool) {
	d, ok := ctx.Value(deviceContextKey{}).(auth.Device)
	return d, ok
}

// SetDeviceCookie writes the device cookie. Every attribute here is required:
// __Host- is rejected by the browser without Secure and Path=/, and rejected
// outright if a Domain is present -- which is exactly the property that stops a
// sibling subdomain shadowing it.
func SetDeviceCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     DeviceCookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   deviceCookieMaxAge,
		Secure:   true,
		HttpOnly: true,
		// Defence in depth only. test.example.com is same-site, so Lax does
		// nothing against the published-ports threat; the Origin check is the
		// boundary. It is kept because it still costs nothing against a
		// genuinely cross-site attacker.
		SameSite: http.SameSiteLaxMode,
		// No Domain. Never Domain=.example.com: that cookie would be readable
		// by whatever is later published on a sibling subdomain, handing a
		// shell to any app under test.
	})
}

// ClearDeviceCookie deletes the device cookie. Sign-out revokes the device in
// the store -- that is the real control -- and this stops the browser
// presenting a credential that is already dead.
//
// The attributes must match the cookie being replaced or the deletion lands as
// a different cookie, and a __Host- deletion missing Secure or Path=/ is
// rejected by the browser outright, leaving the stale cookie in the jar.
func ClearDeviceCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     DeviceCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

// AllowedOrigins derives the exact-origin allowlist from the configured host.
//
// In production that is exactly one entry, https://<host>: v1 serves one
// hostname on a single-name certificate, and one entry is the smallest
// allowlist that can work.
//
// Under --dev it is instead the loopback origins on devPort, and deliberately
// *instead*: a development machine serving plain HTTP must not also accept
// requests claiming to come from the production host, and production must never
// accept localhost. Dev mode changes which origins are allowed, never whether
// the check happens -- that is the difference between a dev switch and a hole.
func AllowedOrigins(host string, dev bool, devPort int) ([]string, error) {
	// Lowercased rather than rejected: DNS is case-insensitive, browsers
	// serialize the host lowercase, and the comparison is byte-exact, so a
	// capitalised --host would otherwise reject every real request.
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return nil, errors.New("front: no host configured; the origin allowlist cannot be derived")
	}
	if strings.ContainsAny(host, ":/\\?#@ \t") {
		return nil, fmt.Errorf("front: --host %q must be a bare hostname, with no scheme, port or path", host)
	}
	if !dev {
		return []string{"https://" + host}, nil
	}

	if devPort < 1 || devPort > 65535 {
		return nil, fmt.Errorf("front: --dev needs a listen port, got %d", devPort)
	}
	// Browsers omit the default port when serializing an origin.
	port := ":" + strconv.Itoa(devPort)
	if devPort == 80 {
		port = ""
	}
	// Three distinct origins, each matched exactly: "localhost" and "127.0.0.1"
	// are different origins to a browser, and which one appears depends on what
	// the developer typed.
	return []string{
		"http://localhost" + port,
		"http://127.0.0.1" + port,
		"http://[::1]" + port,
	}, nil
}
