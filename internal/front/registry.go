package front

import (
	"log/slog"
	"sync"
)

// Registry is the live half of revocation.
//
// Removing a device from the store only stops it authenticating the *next*
// request. A browser that already holds a WebSocket keeps its shell until it
// happens to disconnect, which for a terminal is days. Device sessions never
// expire here, so revocation is the only control there is, and a control that
// leaves the lost laptop's shell open is not one. The registry is what makes
// revoking a device also close what that device has open right now.
//
// It holds one closer per live connection, keyed by device. The WebSocket
// handler registers on connect and deregisters on disconnect; revoke and sign
// out call CloseDevice after removing the device from the store.
//
// A Registry is safe for concurrent use, and its zero value is not usable: use
// NewRegistry.
type Registry struct {
	mu sync.Mutex

	// next is the handle for the next registration. It is global rather than
	// per-device and never reused, so a deregistration handed out before a
	// CloseDevice can never name a connection registered after it.
	next uint64

	// live maps a device to its open connections. A device with no open
	// connections holds no entry -- see remove -- so this does not accumulate
	// a map header per device that ever connected.
	live map[string]map[uint64]func()

	// revoked remembers devices CloseDevice has been called for, which closes
	// the window between the store removal and the sweep: see Add.
	//
	// It is never pruned. Its size is bounded by the number of devices revoked
	// in this process's lifetime -- a handful of short strings, gone on restart
	// -- and pruning would mean deciding when a revocation stops applying,
	// which is never: device ids are 8 random bytes and are not reused, so a
	// re-enrolled browser is a different id and is unaffected.
	revoked map[string]struct{}
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{
		live:    make(map[string]map[uint64]func()),
		revoked: make(map[string]struct{}),
	}
}

// Add registers closer as the way to sever one live connection belonging to
// deviceID, and returns the deregistration for it.
//
// The caller must call remove when the connection ends normally -- `defer
// remove()` at the top of the handler is the intended shape. Without it a
// long-lived daemon would retain one dead closer per browser tab ever opened,
// and a later revocation would run all of them.
//
// remove is idempotent and safe to call from anywhere, including from inside
// closer itself, and after CloseDevice has already run.
//
// ok is false if deviceID has already been revoked. Nothing is registered in
// that case and closer is run, on the same terms as CloseDevice runs one. That
// is the point of the second return: without it there is a window between the
// store dropping the device and CloseDevice sweeping the registry, and a
// connection that authenticated just before the removal but registers just
// after the sweep would survive its own revocation forever. The window is not
// theoretical -- opening a session spawns tmux between those two moments -- so
// it is closed here rather than left to the caller's ordering. A caller that
// ignores ok still gets its connection closed; a caller that checks it can
// refuse the connection instead of opening one and immediately tearing it down.
func (r *Registry) Add(deviceID string, closer func()) (remove func(), ok bool) {
	r.mu.Lock()
	if _, gone := r.revoked[deviceID]; gone {
		r.mu.Unlock()
		// Outside the lock, like every other call into a closer.
		fire(closer)
		return func() {}, false
	}

	id := r.next
	r.next++
	conns := r.live[deviceID]
	if conns == nil {
		conns = make(map[uint64]func())
		r.live[deviceID] = conns
	}
	conns[id] = closer
	r.mu.Unlock()

	return func() { r.remove(deviceID, id) }, true
}

// remove deregisters one connection. The handle is unique for the life of the
// Registry, so this can only ever remove the connection it was issued for.
func (r *Registry) remove(deviceID string, id uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	conns, ok := r.live[deviceID]
	if !ok {
		// Already swept by CloseDevice, or already removed.
		return
	}
	delete(conns, id)
	if len(conns) == 0 {
		delete(r.live, deviceID)
	}
}

// CloseDevice severs every live connection belonging to deviceID, and makes any
// connection registered later for that device severed too. Callers revoke the
// device in the store first and call this second; see Add for why the leftover
// window is closed here rather than by ordering alone.
//
// It is a no-op for a device with nothing open, which is the ordinary case: a
// device is usually revoked from another browser while it has no socket.
//
// Each closer runs on its own goroutine, so CloseDevice does not block on them
// and one closer cannot prevent another running. That matters concretely: a
// closer ends a tmux attach, which kills a process, waits for it and then talks
// to the tmux server under a timeout, so it can take seconds if the server is
// wedged. The operator revoking a lost laptop gets an answer immediately, and
// the guarantee they need is that the shell *will* be closed, not that it was
// closed before their HTTP response.
//
// A closer that panics is logged and does not take down the daemon or the other
// closers with it.
func (r *Registry) CloseDevice(deviceID string) {
	r.mu.Lock()
	conns := r.live[deviceID]
	delete(r.live, deviceID)
	r.revoked[deviceID] = struct{}{}
	r.mu.Unlock()

	// conns is unreachable from the Registry now -- Add never revives a revoked
	// device -- so ranging it without the lock is safe.
	//
	// What actually makes re-entrancy safe is fire running each closer on its
	// own goroutine: a closer ends a connection whose teardown calls its own
	// remove, and that would deadlock if it ran inline under this lock.
	// Releasing first is belt and braces -- it costs nothing and stops the
	// safety of every closer resting on that one detail of fire.
	for _, closer := range conns {
		fire(closer)
	}
}

// fire runs one closer off the caller's goroutine, containing a panic in it.
func fire(closer func()) {
	go func() {
		defer func() {
			if v := recover(); v != nil {
				// Swallowed rather than repanicked: one broken teardown must
				// not take the whole terminal server down, and the connection
				// this was meant to sever is already unreachable from the
				// registry, so there is nothing to retry.
				slog.Error("front: closing a revoked device's connection panicked", "panic", v)
			}
		}()
		closer()
	}()
}
