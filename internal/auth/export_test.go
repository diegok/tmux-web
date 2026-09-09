package auth

import "time"

// SetClock replaces the clock the enroller measures its TTL and failure window
// against. It exists only in the test build: a ten-minute expiry is not
// something a test can wait out, and the production path should not carry a
// knob whose only caller is a test.
func (e *Enroller) SetClock(now func() time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.now = now
}

// PendingCount reports how many minted, unredeemed enrollment tokens the
// enroller holds. Test-only: nothing in production wants the number, but the
// housekeeping that keeps it from growing for the life of the daemon is
// otherwise invisible to a test.
func (e *Enroller) PendingCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.pending)
}
