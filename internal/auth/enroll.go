package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"
	"time"
)

// EnrollTTL is how long a minted enrollment link stays redeemable. It is short
// because the link is a bearer credential travelling through whatever channel
// the owner used to get it onto the other machine -- a paste buffer, a chat
// window to themselves, a photographed terminal. Ten minutes is long enough to
// walk to the laptop and short enough that a link left in a message thread is
// dead by the time anyone reads it. Losing one costs another `enroll`.
const EnrollTTL = 10 * time.Minute

// MaxRedeemFailures and RedeemFailureWindow bound how many failed redemptions
// are answered normally before the enroller starts reporting
// ErrTooManyRedeemAttempts instead. See Enroller for what that is and is not
// defending.
const (
	MaxRedeemFailures   = 10
	RedeemFailureWindow = time.Minute
)

var (
	// ErrInvalidEnrollToken is a token that was never minted, or was already
	// redeemed. The two cases are deliberately indistinguishable: a redeemed
	// token is gone, and nothing about it is worth telling a caller who did not
	// hold it.
	ErrInvalidEnrollToken = errors.New("auth: invalid enrollment token")

	// ErrEnrollTokenExpired is a token that was minted but has aged out. It is
	// distinct from ErrInvalidEnrollToken so the page can say "this link
	// expired, run enroll again" rather than "no". Only someone holding the
	// token can see the difference, and they minted it.
	ErrEnrollTokenExpired = errors.New("auth: enrollment token expired")

	// ErrTooManyRedeemAttempts means failures are arriving faster than
	// RedeemFailureWindow allows. It never applies to a valid token.
	ErrTooManyRedeemAttempts = errors.New("auth: too many enrollment attempts")
)

// Enroller mints single-use enrollment tokens and trades them for device
// sessions. It is the hinge of the auth chain: a token is minted through the
// admin socket, where SO_PEERCRED proves the caller is the service user, and
// redeemed over HTTP, where nothing is proven at all. The token is the whole of
// what carries that local proof to a remote browser.
//
// Pending tokens live only in memory and deliberately do not survive a restart.
// Persisting them would put a live credential on disk to save the owner from
// re-running a command that takes a second.
//
// # What the rate limit is for
//
// Not guessing. A token is 256 bits; an attacker who could try a billion a
// second for the ten minutes one exists would still be about seventy orders of
// magnitude short. The entropy, the TTL, and single use are the defence, and
// the limiter must not be described as one.
//
// The limiter is global rather than per-source because there is no honest
// source identity here: the daemon sits behind a reverse proxy, so every
// request arrives from the proxy, and X-Forwarded-For is whatever the client
// wrote unless the proxy is configured to rewrite it -- which is not something
// this design can assume. Bucketing by a forgeable header would only let an
// attacker pick which bucket to fill.
//
// What it does buy: a flood of unauthenticated POSTs at /enroll becomes one
// distinguishable error the HTTP layer can turn into a 429 and an operator can
// see in a log, instead of a uniform stream of "invalid token".
//
// Crucially the limiter is checked *after* the token comparison, so a tripped
// limiter never refuses a real link. Failing closed would mean any stranger who
// learns the URL can hold the endpoint shut with garbage and keep the owner
// from enrolling -- turning a control against a non-threat into a real outage
// on the one path that recovers from having no devices at all. The cost of that
// ordering is that the limiter caps nothing an attacker does; with 256 bits of
// entropy there is nothing there to cap.
type Enroller struct {
	store *Store

	mu       sync.Mutex
	pending  []pendingEnroll
	failures []time.Time // most recent first, at most MaxRedeemFailures
	now      func() time.Time
}

// pendingEnroll is one minted, unredeemed link. The token is held raw: there is
// no file to protect here, and hashing it would make the comparison a digest
// comparison, which is exactly the case where constant time stops meaning
// anything.
type pendingEnroll struct {
	token     []byte
	name      string
	expiresAt time.Time
}

// NewEnroller returns an enroller that mints device sessions into store.
func NewEnroller(store *Store) *Enroller {
	return &Enroller{store: store, now: time.Now}
}

// Mint issues an enrollment token for a device to be called name. The token is
// returned once and held only in memory; the caller renders it into the
// fragment of an enrollment URL, which is why it is unpadded base64url.
func (e *Enroller) Mint(name string) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("auth: generate enrollment token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(b)

	e.mu.Lock()
	defer e.mu.Unlock()

	now := e.now()
	e.dropExpired(now)
	e.pending = append(e.pending, pendingEnroll{
		token:     []byte(token),
		name:      name,
		expiresAt: now.Add(EnrollTTL),
	})
	return token, nil
}

// Redeem trades an enrollment token for a device session, returning the device
// token. userAgent is recorded against the device: the browser is the one thing
// about it that is only knowable here.
//
// A token is consumed only by a redemption that actually enrolled a device. If
// the store could not persist, the link stays live and the page can be
// reloaded.
func (e *Enroller) Redeem(token, userAgent string) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	now := e.now()

	i := e.find(token)
	if i < 0 {
		return "", e.recordFailure(now, ErrInvalidEnrollToken)
	}
	p := e.pending[i]
	if !now.Before(p.expiresAt) {
		e.remove(i)
		return "", e.recordFailure(now, ErrEnrollTokenExpired)
	}

	deviceToken, err := e.store.AddDevice(p.name, userAgent)
	if err != nil {
		// Not a credential failure: the caller held a good link and the disk
		// let them down. It neither burns the token nor counts against the
		// limiter.
		return "", err
	}
	e.remove(i)
	return deviceToken, nil
}

// find returns the index of the pending entry matching token, or -1.
//
// Unlike the digest comparison in Store.Lookup, constant time is load-bearing
// here: the value on both sides is the live credential, so bytes leaked by an
// early exit are bytes of the token itself rather than of a hash nobody can
// invert. The loop therefore runs to the end instead of breaking on a match --
// there are at most a handful of entries, and stopping early would put the
// timing back that ConstantTimeCompare took out.
func (e *Enroller) find(token string) int {
	got := []byte(token)
	found := -1
	for i := range e.pending {
		if subtle.ConstantTimeCompare(got, e.pending[i].token) == 1 {
			found = i
		}
	}
	return found
}

// remove deletes one pending entry. Callers must hold mu.
func (e *Enroller) remove(i int) {
	e.pending = append(e.pending[:i], e.pending[i+1:]...)
}

// dropExpired discards pending entries that have aged out, so a daemon whose
// owner keeps minting links they never open does not accumulate them. It is
// housekeeping, not enforcement: Redeem checks the expiry itself, because a
// token whose entry is still in the slice must not be redeemable just because
// nothing has minted since. Callers must hold mu.
func (e *Enroller) dropExpired(now time.Time) {
	kept := e.pending[:0]
	for _, p := range e.pending {
		if now.Before(p.expiresAt) {
			kept = append(kept, p)
		}
	}
	e.pending = kept
}

// recordFailure counts one failed redemption and returns either err or
// ErrTooManyRedeemAttempts. Callers must hold mu.
func (e *Enroller) recordFailure(now time.Time, err error) error {
	cutoff := now.Add(-RedeemFailureWindow)
	kept := e.failures[:0]
	for _, t := range e.failures {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	e.failures = append(kept, now)

	if len(e.failures) >= MaxRedeemFailures {
		// Bounded: a flood must not grow the slice it is being measured by.
		e.failures = e.failures[len(e.failures)-MaxRedeemFailures:]
		return ErrTooManyRedeemAttempts
	}
	return err
}
