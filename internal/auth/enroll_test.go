package auth_test

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/diegok/tmux-web/internal/auth"
)

// fakeClock is the seam the enroller's TTL and failure window are measured
// against. Tests jump it rather than sleeping: a ten-minute TTL is not
// something a test can wait out, and a one-millisecond one would only prove the
// scheduler is slow.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newEnroller(t *testing.T) (*auth.Enroller, *auth.Store, *fakeClock) {
	t.Helper()
	s, _ := openStore(t)
	clock := newClock()
	e := auth.NewEnroller(s)
	e.SetClock(clock.Now)
	return e, s, clock
}

func TestMintedTokenRedeemsOnceAndReturnsADeviceToken(t *testing.T) {
	e, s, _ := newEnroller(t)

	tok, err := e.Mint("laptop")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if tok == "" {
		t.Fatal("Mint returned an empty enrollment token")
	}

	devTok, err := e.Redeem(tok, "Firefox/1.0")
	if err != nil {
		t.Fatalf("Redeem: %v", err)
	}
	if devTok == "" {
		t.Fatal("Redeem returned an empty device token")
	}
	if devTok == tok {
		t.Fatal("Redeem handed back the enrollment token: the device session " +
			"must be a fresh credential, not the single-use one it was traded for")
	}

	d, ok := s.Lookup(devTok)
	if !ok {
		t.Fatal("the device token Redeem returned does not authenticate")
	}
	if d.Name != "laptop" {
		t.Fatalf("device name %q, want the name the link was minted for", d.Name)
	}
	if d.UserAgent != "Firefox/1.0" {
		t.Fatalf("device user agent %q: the browser is only known at redemption", d.UserAgent)
	}
}

func TestRedeemingTheSameTokenTwiceFails(t *testing.T) {
	e, s, _ := newEnroller(t)

	tok, err := e.Mint("laptop")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if _, err := e.Redeem(tok, "ua"); err != nil {
		t.Fatalf("first Redeem: %v", err)
	}

	if _, err := e.Redeem(tok, "ua"); !errors.Is(err, auth.ErrInvalidEnrollToken) {
		t.Fatalf("second Redeem error = %v, want ErrInvalidEnrollToken: an "+
			"enrollment link that is pasted twice must only ever mint one device", err)
	}
	if got := len(s.Devices()); got != 1 {
		t.Fatalf("%d devices enrolled from one link, want 1", got)
	}
}

func TestTokenPastItsTTLFails(t *testing.T) {
	e, s, clock := newEnroller(t)

	tok, err := e.Mint("laptop")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	clock.Advance(auth.EnrollTTL)
	if _, err := e.Redeem(tok, "ua"); !errors.Is(err, auth.ErrEnrollTokenExpired) {
		t.Fatalf("Redeem at exactly the TTL = %v, want ErrEnrollTokenExpired", err)
	}
	if got := len(s.Devices()); got != 0 {
		t.Fatalf("an expired link enrolled %d devices", got)
	}
}

func TestTokenJustInsideItsTTLStillRedeems(t *testing.T) {
	e, _, clock := newEnroller(t)

	tok, err := e.Mint("laptop")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	clock.Advance(auth.EnrollTTL - time.Nanosecond)
	if _, err := e.Redeem(tok, "ua"); err != nil {
		t.Fatalf("Redeem one nanosecond before the TTL: %v — the window must be "+
			"the full %v, not something shorter", err, auth.EnrollTTL)
	}
}

func TestExpiredTokenIsRejectedEvenAfterAnotherMint(t *testing.T) {
	// Minting prunes expired entries. That must not be the only thing standing
	// between an expired link and a device: redemption checks the expiry itself.
	e, _, clock := newEnroller(t)

	tok, err := e.Mint("laptop")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	clock.Advance(auth.EnrollTTL)

	if _, err := e.Redeem(tok, "ua"); !errors.Is(err, auth.ErrEnrollTokenExpired) {
		t.Fatalf("Redeem = %v, want ErrEnrollTokenExpired", err)
	}
}

// Expired links are never redeemable, but they must not pile up either: this
// daemon runs for months and the owner mints a link every time they pick up a
// new browser.
func TestMintingDiscardsExpiredTokens(t *testing.T) {
	e, _, clock := newEnroller(t)

	for i := 0; i < 3; i++ {
		if _, err := e.Mint("laptop"); err != nil {
			t.Fatalf("Mint: %v", err)
		}
	}
	if got := e.PendingCount(); got != 3 {
		t.Fatalf("holding %d pending tokens, want 3", got)
	}

	clock.Advance(auth.EnrollTTL)
	if _, err := e.Mint("laptop"); err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if got := e.PendingCount(); got != 1 {
		t.Fatalf("holding %d pending tokens after three of them expired, want 1", got)
	}
}

func TestUnknownTokenFails(t *testing.T) {
	e, s, _ := newEnroller(t)

	if _, err := e.Redeem("", "ua"); !errors.Is(err, auth.ErrInvalidEnrollToken) {
		t.Fatalf("Redeem of the empty token = %v, want ErrInvalidEnrollToken", err)
	}
	if _, err := e.Redeem("bm90LWEtdG9rZW4", "ua"); !errors.Is(err, auth.ErrInvalidEnrollToken) {
		t.Fatalf("Redeem of an unminted token = %v, want ErrInvalidEnrollToken", err)
	}
	if got := len(s.Devices()); got != 0 {
		t.Fatalf("%d devices enrolled without a valid link", got)
	}
}

// The behavioural half of the constant-time requirement: whatever the
// comparison is, it must be over the whole token. A prefix, a suffix, or a
// single flipped character is not a match.
//
// The five near misses stay under MaxRedeemFailures, so the limiter does not
// change the error they get back.
func TestRedeemRejectsNearMissesOfAValidToken(t *testing.T) {
	e, _, _ := newEnroller(t)

	tok, err := e.Mint("laptop")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	flipped := []byte(tok)
	if flipped[len(flipped)-1] == 'A' {
		flipped[len(flipped)-1] = 'B'
	} else {
		flipped[len(flipped)-1] = 'A'
	}

	for name, bad := range map[string]string{
		"prefix":      tok[:len(tok)-1],
		"suffix":      tok[1:],
		"extended":    tok + "A",
		"last byte":   string(flipped),
		"first chars": tok[:8],
	} {
		if _, err := e.Redeem(bad, "ua"); !errors.Is(err, auth.ErrInvalidEnrollToken) {
			t.Fatalf("Redeem of a token differing by %s = %v, want ErrInvalidEnrollToken", name, err)
		}
	}

	if _, err := e.Redeem(tok, "ua"); err != nil {
		t.Fatalf("the real token stopped working after near misses: %v", err)
	}
}

func TestMintedTokensAreDistinctAnd32Bytes(t *testing.T) {
	e, _, _ := newEnroller(t)

	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		tok, err := e.Mint("laptop")
		if err != nil {
			t.Fatalf("Mint: %v", err)
		}
		if seen[tok] {
			t.Fatalf("Mint repeated a token: %q", tok)
		}
		seen[tok] = true

		raw, err := base64.RawURLEncoding.DecodeString(tok)
		if err != nil {
			t.Fatalf("token %q is not unpadded base64url, so it cannot ride in a "+
				"URL fragment unescaped: %v", tok, err)
		}
		if len(raw) != 32 {
			t.Fatalf("token carries %d bytes of entropy, want 32", len(raw))
		}
	}
}

func TestRepeatedFailuresAreRateLimited(t *testing.T) {
	e, _, _ := newEnroller(t)

	for i := 0; i < auth.MaxRedeemFailures-1; i++ {
		if _, err := e.Redeem("wrong", "ua"); !errors.Is(err, auth.ErrInvalidEnrollToken) {
			t.Fatalf("failure %d = %v, want ErrInvalidEnrollToken", i+1, err)
		}
	}
	if _, err := e.Redeem("wrong", "ua"); !errors.Is(err, auth.ErrTooManyRedeemAttempts) {
		t.Fatalf("failure %d = %v, want ErrTooManyRedeemAttempts", auth.MaxRedeemFailures, err)
	}
	if _, err := e.Redeem("wrong", "ua"); !errors.Is(err, auth.ErrTooManyRedeemAttempts) {
		t.Fatalf("a failure past the limit = %v, want ErrTooManyRedeemAttempts", err)
	}
}

func TestRateLimitLapsesAfterItsWindow(t *testing.T) {
	e, _, clock := newEnroller(t)

	for i := 0; i < auth.MaxRedeemFailures; i++ {
		_, _ = e.Redeem("wrong", "ua")
	}
	if _, err := e.Redeem("wrong", "ua"); !errors.Is(err, auth.ErrTooManyRedeemAttempts) {
		t.Fatalf("the limiter did not trip: %v", err)
	}

	clock.Advance(auth.RedeemFailureWindow)
	if _, err := e.Redeem("wrong", "ua"); !errors.Is(err, auth.ErrInvalidEnrollToken) {
		t.Fatalf("after the window the limiter must lapse, got %v: a limit that "+
			"never expires is a permanent lockout anyone can trigger", err)
	}
}

// The decisive property of a global limiter: it must shed a flood without ever
// standing between the owner and their own link. Anything else lets a stranger
// who knows the URL lock the owner out of the only remote enrolment path.
func TestRateLimitNeverBlocksAValidToken(t *testing.T) {
	e, s, _ := newEnroller(t)

	tok, err := e.Mint("laptop")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	for i := 0; i < auth.MaxRedeemFailures*3; i++ {
		_, _ = e.Redeem("wrong", "ua")
	}

	if _, err := e.Redeem(tok, "ua"); err != nil {
		t.Fatalf("a valid link was refused while the limiter was tripped: %v", err)
	}
	if got := len(s.Devices()); got != 1 {
		t.Fatalf("%d devices enrolled, want 1", got)
	}
}

func TestConcurrentRedemptionsEnrollExactlyOneDevice(t *testing.T) {
	e, s, _ := newEnroller(t)

	tok, err := e.Mint("laptop")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	var wins int
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := e.Redeem(tok, "ua"); err == nil {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if wins != 1 {
		t.Fatalf("%d concurrent redemptions succeeded, want exactly 1", wins)
	}
	if got := len(s.Devices()); got != 1 {
		t.Fatalf("%d devices enrolled, want 1", got)
	}
}

// A redemption that could not be persisted has not happened, so it must not
// burn the link: the owner is standing at a browser with a page that failed,
// and the fix is a reload, not another ssh session.
func TestAFailedEnrolmentDoesNotConsumeTheToken(t *testing.T) {
	skipIfRoot(t)
	dir := t.TempDir()
	s, err := auth.OpenStore(filepath.Join(dir, "devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	e := auth.NewEnroller(s)

	tok, err := e.Mint("laptop")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Redeem(tok, "ua"); err == nil {
		t.Fatal("Redeem reported success though the device could not be persisted")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	if _, err := e.Redeem(tok, "ua"); err != nil {
		t.Fatalf("the link was consumed by an enrolment that never happened: %v", err)
	}
}

// Two properties no behaviour can observe: which comparison primitive runs, and
// which generator the bytes came from. A test that swapped ConstantTimeCompare
// for == or crypto/rand for math/rand would still pass every test above, so
// this reads the source instead. It is a lint, not a proof -- but it is the
// only thing that stops either property from being refactored away in silence.
func TestEnrollSourceUsesConstantTimeCompareAndCryptoRand(t *testing.T) {
	src, err := os.ReadFile("enroll.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)

	if !strings.Contains(text, "subtle.ConstantTimeCompare") {
		t.Error("enroll.go no longer compares with subtle.ConstantTimeCompare: " +
			"unlike the device store, the value compared here is the live token " +
			"itself, not a digest of it")
	}
	if !strings.Contains(text, `"crypto/rand"`) {
		t.Error("enroll.go does not import crypto/rand")
	}
	if strings.Contains(text, `"math/rand"`) || strings.Contains(text, `"math/rand/v2"`) {
		t.Error("enroll.go imports math/rand: enrollment tokens must be unguessable")
	}
}
