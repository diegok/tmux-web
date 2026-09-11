package front_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/diegok/tmux-web/internal/auth"
	"github.com/diegok/tmux-web/internal/front"
)

// The canonical host the daemon serves, and the sibling a published port would
// later live on. They are the same site -- SameSite would let the sibling
// through -- and different origins, which is the whole point of these tests.
const (
	canonicalOrigin = "https://tmux.example.com"
	siblingOrigin   = "https://test.example.com"
)

// errWriteFailed stands in for a store that cannot persist -- a full disk, a
// read-only filesystem.
var errWriteFailed = errors.New("write failed")

// wireCookieName is spelled out rather than read from front.DeviceCookieName so
// that renaming the constant fails a test instead of silently changing the wire
// format every enrolled browser depends on.
const wireCookieName = "__Host-tmux_web_device"

// spy is the protected handler. It records whether it ran, which is the only
// way to tell a rejection from a handler that happened to write the same code.
type spy struct {
	mu     sync.Mutex
	calls  int
	device auth.Device
	seen   bool
}

func (s *spy) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d, ok := front.DeviceFrom(r.Context())
		s.mu.Lock()
		s.calls++
		s.device, s.seen = d, ok
		s.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
}

func (s *spy) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// fakeStore stands in for auth.Store where a test needs to count Lookup and
// Touch calls or make Touch fail. The round-trip tests use a real store.
type fakeStore struct {
	mu       sync.Mutex
	token    string
	device   auth.Device
	touches  []time.Time
	touchErr error
	lookups  int
}

func (f *fakeStore) Lookup(token string) (auth.Device, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lookups++
	if token == "" || token != f.token {
		return auth.Device{}, false
	}
	return f.device, true
}

func (f *fakeStore) Touch(id string, t time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.touchErr != nil {
		return f.touchErr
	}
	f.touches = append(f.touches, t)
	return nil
}

func (f *fakeStore) touchCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.touches)
}

func (f *fakeStore) lookupCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lookups
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		token:  "device-token",
		device: auth.Device{ID: "dev-1", Name: "laptop"},
	}
}

// realStore enrolls one device in an on-disk store and returns it with the
// device token, so the auth path is exercised against the code that will really
// answer it rather than against a fake that agrees with it.
func realStore(t *testing.T) (*auth.Store, string) {
	t.Helper()
	s, err := auth.OpenStore(filepath.Join(t.TempDir(), "devices.json"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	token, err := s.AddDevice("laptop", "Go test")
	if err != nil {
		t.Fatalf("AddDevice: %v", err)
	}
	return s, token
}

func newAuth(t *testing.T, store front.DeviceStore) *front.Auth {
	t.Helper()
	a, err := front.NewAuth(store, []string{canonicalOrigin})
	if err != nil {
		t.Fatalf("NewAuth: %v", err)
	}
	return a
}

func withCookie(r *http.Request, token string) *http.Request {
	r.AddCookie(&http.Cookie{Name: wireCookieName, Value: token})
	return r
}

// ---------------------------------------------------------------- the cookie

func TestSetCookieUsesHostPrefix(t *testing.T) {
	rec := httptest.NewRecorder()
	front.SetDeviceCookie(rec, "tok")
	sc := rec.Result().Cookies()[0]

	if !strings.HasPrefix(sc.Name, "__Host-") {
		t.Fatal("device cookie must use the __Host- prefix so a sibling " +
			"subdomain cannot shadow it with a Domain=.example.com cookie")
	}
	if sc.Domain != "" {
		t.Fatalf("Domain must be empty, got %q", sc.Domain)
	}
	if !sc.Secure || !sc.HttpOnly || sc.Path != "/" {
		t.Fatalf("bad cookie attributes: %+v", sc)
	}
	if sc.Value != "tok" {
		t.Fatalf("value = %q, want the device token", sc.Value)
	}
	if sc.Name != wireCookieName {
		t.Fatalf("name = %q, want %q", sc.Name, wireCookieName)
	}
}

func TestSetCookieEmitsNoDomainAttributeAtAll(t *testing.T) {
	// net/http parses an empty Domain the same whether the attribute is absent
	// or present-and-empty; the browser does not. A __Host- cookie carrying any
	// Domain attribute is rejected outright, which would sign the user out.
	rec := httptest.NewRecorder()
	front.SetDeviceCookie(rec, "tok")
	raw := rec.Header().Get("Set-Cookie")

	if strings.Contains(strings.ToLower(raw), "domain=") {
		t.Fatalf("Set-Cookie carries a Domain attribute: %q", raw)
	}
	for _, want := range []string{"Secure", "HttpOnly", "Path=/"} {
		if !strings.Contains(raw, want) {
			t.Fatalf("Set-Cookie %q is missing %s", raw, want)
		}
	}
}

func TestDeviceCookieIsSameSiteLaxAndPersistent(t *testing.T) {
	rec := httptest.NewRecorder()
	front.SetDeviceCookie(rec, "tok")
	sc := rec.Result().Cookies()[0]

	// Lax is defence in depth only: test.example.com is *same site*, so this
	// attribute does nothing against the published-ports threat. The Origin
	// check is the boundary. It is still asserted so that dropping it is a
	// deliberate act.
	if sc.SameSite != http.SameSiteLaxMode {
		t.Fatalf("SameSite = %v, want Lax", sc.SameSite)
	}
	// Device sessions do not expire -- revocation is the control -- so the
	// cookie must outlive the browser process. A session cookie would sign the
	// owner out on browser restart, and re-enrolling needs an ssh session.
	if sc.MaxAge <= 0 {
		t.Fatalf("MaxAge = %d, want a persistent cookie", sc.MaxAge)
	}
}

func TestClearDeviceCookieMatchesTheAttributesItReplaces(t *testing.T) {
	rec := httptest.NewRecorder()
	front.ClearDeviceCookie(rec)
	sc := rec.Result().Cookies()[0]

	// A deletion only lands if name, path and domain match the cookie being
	// replaced, and a __Host- deletion must itself be Secure and path-scoped or
	// the browser rejects it and the stale credential stays in the jar.
	if sc.Name != wireCookieName || sc.Path != "/" || sc.Domain != "" || !sc.Secure {
		t.Fatalf("deletion cookie does not match the cookie it replaces: %+v", sc)
	}
	if sc.MaxAge >= 0 || sc.Value != "" {
		t.Fatalf("deletion cookie must expire immediately with an empty value: %+v", sc)
	}
}

// ------------------------------------------------------------ authentication

func TestNoCookieIsUnauthorized(t *testing.T) {
	store, _ := realStore(t)
	s := &spy{}
	h := newAuth(t, store).Protect(s.handler())

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if s.count() != 0 {
		t.Fatal("protected handler ran without a device cookie")
	}
}

func TestValidCookieIsAuthorized(t *testing.T) {
	store, token := realStore(t)
	s := &spy{}
	h := newAuth(t, store).Protect(s.handler())

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, withCookie(httptest.NewRequest(http.MethodGet, "/", nil), token))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if s.count() != 1 {
		t.Fatal("protected handler did not run for a valid device cookie")
	}
	if !s.seen || s.device.Name != "laptop" {
		t.Fatalf("handler saw device %+v (found=%v), want the enrolled device", s.device, s.seen)
	}
}

func TestRevokedDeviceIsUnauthorized(t *testing.T) {
	store, token := realStore(t)
	s := &spy{}
	h := newAuth(t, store).Protect(s.handler())

	// Prove the cookie worked before revocation, so a 401 afterwards can only
	// come from the revocation.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, withCookie(httptest.NewRequest(http.MethodGet, "/", nil), token))
	if rec.Code != http.StatusOK {
		t.Fatalf("pre-revocation status = %d, want 200", rec.Code)
	}

	if err := store.Revoke(store.Devices()[0].ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, withCookie(httptest.NewRequest(http.MethodGet, "/", nil), token))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for a revoked device", rec.Code)
	}
	if s.count() != 1 {
		t.Fatal("protected handler ran for a revoked device")
	}
}

func TestUnknownAndEmptyTokensAreUnauthorized(t *testing.T) {
	store, token := realStore(t)
	h := newAuth(t, store).Protect((&spy{}).handler())

	for _, tc := range []struct{ name, token string }{
		{"garbage", "not-a-token"},
		{"empty", ""},
		{"prefix of a real token", token[:len(token)-1]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, withCookie(httptest.NewRequest(http.MethodGet, "/", nil), tc.token))
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
		})
	}
}

func TestOnlyThePrefixedCookieNameAuthenticates(t *testing.T) {
	// A cookie the browser would accept without the prefix -- one a sibling
	// subdomain could have set with Domain=.example.com -- must not be read as
	// a credential.
	store, token := realStore(t)
	h := newAuth(t, store).Protect((&spy{}).handler())

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(&http.Cookie{Name: "tmux_web_device", Value: token})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for an unprefixed cookie name", rec.Code)
	}
}

func TestProtectedResponsesVaryOnCookieAndOrigin(t *testing.T) {
	store, _ := realStore(t)
	h := newAuth(t, store).Protect((&spy{}).handler())

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	vary := strings.ToLower(strings.Join(rec.Header().Values("Vary"), ","))
	for _, want := range []string{"cookie", "origin"} {
		if !strings.Contains(vary, want) {
			t.Fatalf("Vary = %q, want it to include %q", vary, want)
		}
	}
}

// -------------------------------------------------------------- the boundary

func TestPostFromSiblingSubdomainIsForbidden(t *testing.T) {
	store, token := realStore(t)
	s := &spy{}
	h := newAuth(t, store).Protect(s.handler())

	r := withCookie(httptest.NewRequest(http.MethodPost, "/api/devices", nil), token)
	r.Header.Set("Origin", siblingOrigin)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: test.example.com is same-site, so the "+
			"browser attaches the device cookie and only the Origin check stops it", rec.Code)
	}
	if s.count() != 0 {
		t.Fatal("a cross-origin POST reached the handler")
	}
}

func TestPostWithNoOriginIsForbidden(t *testing.T) {
	store, token := realStore(t)
	s := &spy{}
	h := newAuth(t, store).Protect(s.handler())

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, withCookie(httptest.NewRequest(http.MethodPost, "/api/devices", nil), token))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a mutating request with no Origin", rec.Code)
	}
	if s.count() != 0 {
		t.Fatal("an origin-less POST reached the handler")
	}
}

func TestPostFromTheCanonicalOriginSucceeds(t *testing.T) {
	// Without this the Origin check could reject everything and still pass the
	// tests above.
	store, token := realStore(t)
	s := &spy{}
	h := newAuth(t, store).Protect(s.handler())

	r := withCookie(httptest.NewRequest(http.MethodPost, "/api/devices", nil), token)
	r.Header.Set("Origin", canonicalOrigin)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)

	if rec.Code != http.StatusOK || s.count() != 1 {
		t.Fatalf("status = %d, handler calls = %d, want 200 and 1", rec.Code, s.count())
	}
}

func TestGetWithNoOriginIsAllowed(t *testing.T) {
	// Ordinary navigation -- a bookmark, a typed URL, a same-origin fetch --
	// carries no Origin header. Rejecting it would break the app.
	store, token := realStore(t)
	s := &spy{}
	h := newAuth(t, store).Protect(s.handler())

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, withCookie(httptest.NewRequest(http.MethodGet, "/", nil), token))

	if rec.Code != http.StatusOK || s.count() != 1 {
		t.Fatalf("status = %d, handler calls = %d, want 200 and 1", rec.Code, s.count())
	}
}

func TestGetFromAForeignOriginIsForbidden(t *testing.T) {
	// A GET that carries an Origin was made by script, not by navigation. Our
	// own pages send either no Origin or the canonical one, so rejecting a
	// foreign Origin on GET costs nothing and closes read-shaped requests --
	// including the WebSocket handshake, which is a GET.
	store, token := realStore(t)
	s := &spy{}
	h := newAuth(t, store).Protect(s.handler())

	r := withCookie(httptest.NewRequest(http.MethodGet, "/api/snapshot", nil), token)
	r.Header.Set("Origin", siblingOrigin)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if s.count() != 0 {
		t.Fatal("a cross-origin GET reached the handler")
	}
}

func TestOriginMustMatchExactlyNotBySuffix(t *testing.T) {
	store, token := realStore(t)
	s := &spy{}
	h := newAuth(t, store).Protect(s.handler())

	for _, origin := range []string{
		siblingOrigin,                       // the published-port attack
		"https://example.com",               // the registrable domain itself
		"https://tmux.example.com.evil.net", // suffix-of-us as a prefix of them
		"https://eviltmux.example.com",      // no dot boundary
		"http://tmux.example.com",           // wrong scheme
		"https://tmux.example.com:8443",     // wrong port
		"https://tmux.example.com/",         // not an origin serialization
		"null",                              // sandboxed iframe, file://, some redirects
		"*",
		"",
	} {
		t.Run(origin, func(t *testing.T) {
			r := withCookie(httptest.NewRequest(http.MethodPost, "/api/devices", nil), token)
			if origin != "" {
				r.Header.Set("Origin", origin)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, r)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("Origin %q got %d, want 403", origin, rec.Code)
			}
		})
	}
	if s.count() != 0 {
		t.Fatal("a rejected origin reached the handler")
	}
}

func TestOriginIsCheckedAgainstConfigNotTheHostHeader(t *testing.T) {
	// r.Host is whatever the client wrote. Deriving the allowlist from it would
	// let the attacker's page satisfy the check by talking to itself.
	store, token := realStore(t)
	s := &spy{}
	h := newAuth(t, store).Protect(s.handler())

	r := withCookie(httptest.NewRequest(http.MethodPost, "/api/devices", nil), token)
	r.Host = "test.example.com"
	r.Header.Set("Origin", siblingOrigin)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)

	if rec.Code != http.StatusForbidden || s.count() != 0 {
		t.Fatalf("status = %d, handler calls = %d: the allowlist must come from "+
			"configuration, never from the request", rec.Code, s.count())
	}
}

func TestDuplicateOriginHeadersAreRejected(t *testing.T) {
	store, token := realStore(t)
	h := newAuth(t, store).Protect((&spy{}).handler())

	r := withCookie(httptest.NewRequest(http.MethodPost, "/api/devices", nil), token)
	r.Header.Add("Origin", canonicalOrigin)
	r.Header.Add("Origin", siblingOrigin)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: a request with two Origins has no "+
			"single origin to check", rec.Code)
	}
}

func TestOnlyGetAndHeadMayOmitOrigin(t *testing.T) {
	store, token := realStore(t)
	h := newAuth(t, store).Protect((&spy{}).handler())

	allowed := map[string]bool{http.MethodGet: true, http.MethodHead: true}
	for _, method := range []string{
		http.MethodGet, http.MethodHead,
		http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete,
		// OPTIONS and TRACE are "safe" in RFC 9110 but this app answers neither,
		// and an extension method is unknowable -- both fail closed.
		http.MethodOptions, http.MethodTrace, "PROPFIND",
	} {
		t.Run(method, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, withCookie(httptest.NewRequest(method, "/api/devices", nil), token))

			want := http.StatusForbidden
			if allowed[method] {
				want = http.StatusOK
			}
			if rec.Code != want {
				t.Fatalf("%s with no Origin got %d, want %d", method, rec.Code, want)
			}
		})
	}
}

func TestOriginIsCheckedBeforeTheCookie(t *testing.T) {
	// A forged-origin request is refused on the request alone: it never reaches
	// the store, so it cannot time a lookup or move a last-seen timestamp.
	fake := newFakeStore()
	h := newAuth(t, fake).Protect((&spy{}).handler())

	r := withCookie(httptest.NewRequest(http.MethodPost, "/api/devices", nil), fake.token)
	r.Header.Set("Origin", siblingOrigin)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if fake.lookupCount() != 0 {
		t.Fatalf("store was consulted %d times for a rejected origin", fake.lookupCount())
	}
	if fake.touchCount() != 0 {
		t.Fatal("a rejected request moved last-seen")
	}
}

// ------------------------------------------------- the websocket handshake

func TestProtectSocketRequiresAnOriginEvenThoughItIsAGet(t *testing.T) {
	// The handshake is a GET, and browsers attach cookies to cross-origin WS
	// handshakes. Treating GET as safe here would hand a shell to any page.
	store, token := realStore(t)

	for _, tc := range []struct {
		name   string
		origin string
		want   int
	}{
		{"no origin", "", http.StatusForbidden},
		{"sibling subdomain", siblingOrigin, http.StatusForbidden},
		{"canonical", canonicalOrigin, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &spy{}
			h := newAuth(t, store).ProtectSocket(s.handler())

			r := withCookie(httptest.NewRequest(http.MethodGet, "/ws?session=work", nil), token)
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, r)

			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
			if want := map[bool]int{true: 1, false: 0}[tc.want == http.StatusOK]; s.count() != want {
				t.Fatalf("handler calls = %d, want %d", s.count(), want)
			}
		})
	}
}

func TestAllowsOriginIsExportedForHandlersThatCheckThemselves(t *testing.T) {
	// Task 10's terminal handler does its own check inside the upgrade. It must
	// be able to ask the same question and get the same answer.
	a := newAuth(t, newFakeStore())

	if !a.AllowsOrigin(canonicalOrigin) {
		t.Fatal("canonical origin rejected")
	}
	if a.AllowsOrigin(siblingOrigin) {
		t.Fatal("sibling subdomain accepted")
	}
	if got := a.Origins(); len(got) != 1 || got[0] != canonicalOrigin {
		t.Fatalf("Origins() = %v", got)
	}
}

func TestOriginsIsACopy(t *testing.T) {
	a := newAuth(t, newFakeStore())
	a.Origins()[0] = siblingOrigin
	if a.AllowsOrigin(siblingOrigin) {
		t.Fatal("the allowlist was mutated through the slice Origins returned")
	}
}

// -------------------------------------------------------------- last-seen

func TestLastSeenIsRecordedButThrottled(t *testing.T) {
	fake := newFakeStore()
	a := newAuth(t, fake)
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	a.SetClock(func() time.Time { return now })
	h := a.Protect((&spy{}).handler())

	get := func() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, withCookie(httptest.NewRequest(http.MethodGet, "/", nil), fake.token))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
	}

	for i := 0; i < 20; i++ {
		get()
	}
	if fake.touchCount() != 1 {
		t.Fatalf("touches = %d after 20 requests, want 1: Touch rewrites and "+
			"fsyncs the whole device file, so it must not sit on every request",
			fake.touchCount())
	}

	now = now.Add(front.TouchInterval - time.Second)
	get()
	if fake.touchCount() != 1 {
		t.Fatalf("touches = %d just inside the interval, want 1", fake.touchCount())
	}

	now = now.Add(2 * time.Second)
	get()
	if fake.touchCount() != 2 {
		t.Fatalf("touches = %d past the interval, want 2", fake.touchCount())
	}
	if !fake.touches[1].Equal(now) {
		t.Fatalf("recorded %v, want the time of the request %v", fake.touches[1], now)
	}
}

func TestLastSeenIsNotRecordedForRejectedRequests(t *testing.T) {
	fake := newFakeStore()
	h := newAuth(t, fake).Protect((&spy{}).handler())

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, withCookie(httptest.NewRequest(http.MethodGet, "/", nil), "wrong-token"))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if fake.touchCount() != 0 {
		t.Fatal("an unauthenticated request moved a last-seen timestamp")
	}
}

func TestAFailedTouchDoesNotFailTheRequest(t *testing.T) {
	// last-seen is decoration. A full disk must not sign the owner out of the
	// tool they would use to notice.
	fake := newFakeStore()
	fake.touchErr = errWriteFailed
	s := &spy{}
	h := newAuth(t, fake).Protect(s.handler())

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, withCookie(httptest.NewRequest(http.MethodGet, "/", nil), fake.token))

	if rec.Code != http.StatusOK || s.count() != 1 {
		t.Fatalf("status = %d, handler calls = %d, want 200 and 1", rec.Code, s.count())
	}
}

func TestConcurrentRequestsTouchOnce(t *testing.T) {
	fake := newFakeStore()
	a := newAuth(t, fake)
	now := time.Now()
	a.SetClock(func() time.Time { return now })
	h := a.Protect((&spy{}).handler())

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, withCookie(httptest.NewRequest(http.MethodGet, "/", nil), fake.token))
		}()
	}
	wg.Wait()

	if fake.touchCount() != 1 {
		t.Fatalf("touches = %d, want 1", fake.touchCount())
	}
}

// ----------------------------------------------------------- configuration

func TestNewAuthRejectsAnUnusableAllowlist(t *testing.T) {
	for _, tc := range []struct {
		name    string
		origins []string
	}{
		{"none", nil},
		{"empty string", []string{""}},
		{"bare host", []string{"tmux.example.com"}},
		{"trailing slash", []string{"https://tmux.example.com/"}},
		{"with a path", []string{"https://tmux.example.com/app"}},
		{"wildcard", []string{"*"}},
		{"userinfo", []string{"https://u:p@tmux.example.com"}},
		{"wrong scheme", []string{"ftp://tmux.example.com"}},
		{"one good one bad", []string{canonicalOrigin, "*"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := front.NewAuth(newFakeStore(), tc.origins); err == nil {
				t.Fatalf("NewAuth accepted %q; a malformed entry can never match "+
					"a browser origin, so it is a silently broken allowlist", tc.origins)
			}
		})
	}
}

func TestNewAuthRejectsANilStore(t *testing.T) {
	if _, err := front.NewAuth(nil, []string{canonicalOrigin}); err == nil {
		t.Fatal("NewAuth accepted a nil store")
	}
}

func TestAllowedOriginsInProductionIsExactlyTheOneHost(t *testing.T) {
	got, err := front.AllowedOrigins("tmux.example.com", false, 7000)
	if err != nil {
		t.Fatalf("AllowedOrigins: %v", err)
	}
	if len(got) != 1 || got[0] != canonicalOrigin {
		t.Fatalf("AllowedOrigins = %v, want exactly [%q]", got, canonicalOrigin)
	}
}

func TestDevOriginsReplaceProductionRatherThanJoiningIt(t *testing.T) {
	got, err := front.AllowedOrigins("tmux.example.com", true, 7000)
	if err != nil {
		t.Fatalf("AllowedOrigins: %v", err)
	}
	for _, o := range got {
		if o == canonicalOrigin {
			t.Fatal("--dev must not leave the production origin in the allowlist: " +
				"a developer's machine would then accept requests from it over plain http")
		}
		if !strings.HasPrefix(o, "http://") {
			t.Fatalf("dev origin %q is not plain http on loopback", o)
		}
	}
	want := []string{"http://localhost:7000", "http://127.0.0.1:7000", "http://[::1]:7000"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("AllowedOrigins = %v, want %v", got, want)
	}
}

func TestAllowedOriginsRejectsNonsense(t *testing.T) {
	for _, tc := range []struct {
		name string
		host string
		dev  bool
		port int
	}{
		{"no host", "", false, 0},
		{"host with a scheme", "https://tmux.example.com", false, 0},
		{"host with a port", "tmux.example.com:443", false, 0},
		{"host with a path", "tmux.example.com/app", false, 0},
		{"dev with no port", "tmux.example.com", true, 0},
		{"dev with a silly port", "tmux.example.com", true, 70000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := front.AllowedOrigins(tc.host, tc.dev, tc.port); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestAllowedOriginsLowercasesTheHost(t *testing.T) {
	// Browsers serialize origins with a lowercase host, and the comparison is
	// byte-exact, so a capitalised --host would reject every real request.
	got, err := front.AllowedOrigins("TMUX.Example.COM", false, 0)
	if err != nil {
		t.Fatalf("AllowedOrigins: %v", err)
	}
	if len(got) != 1 || got[0] != canonicalOrigin {
		t.Fatalf("AllowedOrigins = %v, want [%q]", got, canonicalOrigin)
	}
}

func TestTheWholeChainFromEnrolmentToARequest(t *testing.T) {
	// What Task 17 will wire together: redeem an enrollment token, set the
	// cookie from the response, and use it on a protected route.
	store, _ := realStore(t)
	enroller := auth.NewEnroller(store)

	enrollToken, err := enroller.Mint("phone")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	deviceToken, err := enroller.Redeem(enrollToken, "Go test")
	if err != nil {
		t.Fatalf("Redeem: %v", err)
	}

	rec := httptest.NewRecorder()
	front.SetDeviceCookie(rec, deviceToken)

	s := &spy{}
	h := newAuth(t, store).Protect(s.handler())
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, c := range rec.Result().Cookies() {
		r.AddCookie(c)
	}

	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, r)
	if rec2.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec2.Code)
	}
	if s.device.Name != "phone" {
		t.Fatalf("handler saw %q, want the device just enrolled", s.device.Name)
	}
}
