package tmux

import (
	"context"
	"sync"
	"time"
)

// Poller refreshes a snapshot on a ticker and hands the cached value to any
// number of readers. One tmux fork per interval regardless of how many browser
// tabs are connected.
type Poller struct {
	interval time.Duration
	fn       func(context.Context) ([]Row, error)

	mu     sync.RWMutex
	latest []Row
	err    error
}

func NewPollerFunc(interval time.Duration, fn func(context.Context) ([]Row, error)) *Poller {
	return &Poller{interval: interval, fn: fn}
}

func NewPoller(interval time.Duration, c *Client) *Poller {
	return NewPollerFunc(interval, c.Snapshot)
}

// Start polls once synchronously -- so the first tab to connect does not see an
// empty sidebar -- then keeps polling until ctx is cancelled.
func (p *Poller) Start(ctx context.Context) {
	p.refresh(ctx)
	go func() {
		t := time.NewTicker(p.interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				p.refresh(ctx)
			}
		}
	}()
}

// refresh replaces the cached snapshot, but only when the poll succeeded.
//
// A failed poll updates err and leaves the rows alone. tmux prints "server
// exited unexpectedly" for a few milliseconds while a server shuts down, and
// noServer deliberately does not match it -- so without this, one poll landing
// in that window would blank the sidebar and the next would silently fix it.
// Keeping the last good snapshot covers that and any other transient fault
// without having to enumerate tmux's error strings, which is exactly the
// fragile thing noServer already has to do.
//
// The stale snapshot is not served silently: err stays set until a poll
// succeeds, so a caller can tell a momentary hiccup from a server that has been
// unreachable for a minute.
func (p *Poller) refresh(ctx context.Context) {
	rows, err := p.fn(ctx)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.err = err
	if err != nil {
		return
	}
	p.latest = rows
}

// Latest returns the most recent successful snapshot without forking tmux.
//
// The slice is shared with every other reader and must not be modified.
// Snapshot's rows come from Dedupe, which allocates a fresh slice per call and
// never aliases its input, so a refresh replaces this value rather than writing
// through it.
func (p *Poller) Latest() []Row {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.latest
}

// Err reports how the most recent poll ended: nil if it succeeded, otherwise
// the failure, in which case Latest is serving the snapshot from before it.
//
// Latest and Err take the lock separately, so a caller reading both across a
// refresh can pair rows with the other poll's error. That is deliberate -- the
// pair only ever straddles one interval, and neither value is ever wrong on its
// own -- and a combined accessor can be added when a caller needs one.
func (p *Poller) Err() error {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.err
}
