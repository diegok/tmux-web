package front

import "time"

// SetClock replaces the clock the last-seen throttle measures against.
func (a *Auth) SetClock(now func() time.Time) {
	a.now = now
}
