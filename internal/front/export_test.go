package front

import "time"

// SetClock replaces the clock the last-seen throttle measures against.
func (a *Auth) SetClock(now func() time.Time) {
	a.now = now
}

// SetManageTimeout shortens the bound on a management request's tmux call and
// returns the restore. A test that waited the real one out would cost five
// seconds to prove one line.
func SetManageTimeout(d time.Duration) func() {
	prev := manageTimeout
	manageTimeout = d
	return func() { manageTimeout = prev }
}
