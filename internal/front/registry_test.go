package front_test

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/diegok/tmux-web/internal/front"
)

// closers run on their own goroutines, so every assertion about them is a wait
// with a deadline. A second is far longer than any of these need and short
// enough that a hung test is still a test result.
const settle = time.Second

// notClosed is how long to wait before believing a closer will not run. It is a
// real sleep on the happy path of several tests, so it stays small; a mutant
// that runs the wrong closer runs it immediately, not after a delay.
const notClosed = 50 * time.Millisecond

func waitClosed(t *testing.T, ch <-chan struct{}, msg string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(settle):
		t.Fatal(msg)
	}
}

func mustNotClose(t *testing.T, ch <-chan struct{}, msg string) {
	t.Helper()
	select {
	case <-ch:
		t.Fatal(msg)
	case <-time.After(notClosed):
	}
}

// closeSignal returns a channel and a closer that closes it. The closer is safe
// to call twice so that a registry which wrongly retains and re-runs it is
// caught by a count, not by a panic inside a recover that hides the fault.
func closeSignal() (<-chan struct{}, func(), *atomic.Int32) {
	ch := make(chan struct{})
	var calls atomic.Int32
	var once sync.Once
	return ch, func() {
		calls.Add(1)
		once.Do(func() { close(ch) })
	}, &calls
}

// The plan's test, verbatim: the whole point of the type.
func TestRevokeClosesLiveSessions(t *testing.T) {
	reg := front.NewRegistry()
	closed := make(chan struct{})
	reg.Add("device-1", func() { close(closed) })

	reg.CloseDevice("device-1")

	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("revoking a device must close its live sockets and attach PTYs; " +
			"otherwise a revoked device keeps its shell until it disconnects")
	}
}

// A laptop with three tabs open loses all three, not the first one the map
// happened to yield.
func TestCloseDeviceClosesEveryConnectionOfThatDevice(t *testing.T) {
	reg := front.NewRegistry()

	const conns = 3
	chans := make([]<-chan struct{}, conns)
	for i := range chans {
		ch, closer, _ := closeSignal()
		chans[i] = ch
		if _, ok := reg.Add("device-1", closer); !ok {
			t.Fatalf("Add %d refused a device that was never revoked", i)
		}
	}

	reg.CloseDevice("device-1")

	for i, ch := range chans {
		waitClosed(t, ch, fmt.Sprintf("connection %d of the revoked device survived CloseDevice", i))
	}
}

// Revoking the lost laptop must not sign the owner out of the browser they are
// revoking it from.
func TestCloseDeviceLeavesOtherDevicesAlone(t *testing.T) {
	reg := front.NewRegistry()

	target, targetCloser, _ := closeSignal()
	other, otherCloser, _ := closeSignal()
	reg.Add("device-1", targetCloser)
	reg.Add("device-2", otherCloser)

	reg.CloseDevice("device-1")

	waitClosed(t, target, "CloseDevice did not close the named device's connection")
	mustNotClose(t, other, "CloseDevice closed another device's connection; revoking a lost laptop must not sign every other browser out")
}

// Deregistration must name one connection, on one device. A registry that
// removed the wrong entry, all of a device's entries, or an entry from whatever
// device it happened upon would leave a revoked device with a live shell or
// kill a healthy tab.
func TestRemoveDeregistersOnlyItsOwnConnection(t *testing.T) {
	reg := front.NewRegistry()

	kept, keptCloser, _ := closeSignal()
	reg.Add("device-1", keptCloser)

	// Other devices are connected throughout -- the ordinary state of a daemon
	// with a laptop and a phone enrolled.
	others := make([]<-chan struct{}, 0, 4)
	for i := range cap(others) {
		ch, closer, _ := closeSignal()
		others = append(others, ch)
		reg.Add(fmt.Sprintf("device-other-%d", i), closer)
	}

	// Several tabs open and close again. One removal that silently did nothing
	// -- because it looked under the wrong device -- would leave that tab's
	// closer behind, so the cycle is repeated: a registry that picks an
	// arbitrary device is caught here, not caught one run in five.
	const cycles = 8
	goneChans := make([]<-chan struct{}, 0, cycles)
	goneCalls := make([]*atomic.Int32, 0, cycles)
	for range cycles {
		ch, closer, calls := closeSignal()
		goneChans = append(goneChans, ch)
		goneCalls = append(goneCalls, calls)

		remove, ok := reg.Add("device-1", closer)
		if !ok {
			t.Fatal("Add refused a device that was never revoked")
		}
		remove()
	}

	reg.CloseDevice("device-1")

	waitClosed(t, kept, "removing one connection deregistered another one, so a revoked device kept a live shell")
	for i, ch := range goneChans {
		mustNotClose(t, ch, fmt.Sprintf("deregistered connection %d was still closed on revocation", i))
		if n := goneCalls[i].Load(); n != 0 {
			t.Fatalf("deregistered closer %d ran %d times, want 0", i, n)
		}
	}
	for i, ch := range others {
		mustNotClose(t, ch, fmt.Sprintf("deregistering one device's connection closed device-other-%d's", i))
	}
}

// A device accumulates tabs over days: opened, closed, opened again. A
// registration must not be displaced by a later one, or the tab holding it
// would survive the revocation that was meant to close it -- silently, since
// nothing else ever notices a closer that is simply no longer there.
func TestRegistrationsSurviveTabsComingAndGoing(t *testing.T) {
	reg := front.NewRegistry()

	first, firstCloser, _ := closeSignal()
	removeFirst, _ := reg.Add("device-1", firstCloser)

	long, longCloser, _ := closeSignal()
	reg.Add("device-1", longCloser)

	removeFirst()

	later, laterCloser, _ := closeSignal()
	reg.Add("device-1", laterCloser)

	reg.CloseDevice("device-1")

	waitClosed(t, long, "a connection registered before a churn of others was lost, so revocation left its shell open")
	waitClosed(t, later, "the most recent connection was not closed on revocation")
	mustNotClose(t, first, "a deregistered connection was closed on revocation")
}

// The handler's `defer remove()` fires on every exit path, including ones where
// CloseDevice has already swept the connection away.
func TestRemoveIsSafeToCallTwiceAndAfterCloseDevice(t *testing.T) {
	reg := front.NewRegistry()

	ch, closer, calls := closeSignal()
	remove, _ := reg.Add("device-1", closer)

	reg.CloseDevice("device-1")
	waitClosed(t, ch, "CloseDevice did not close the connection")

	remove()
	remove()

	if n := calls.Load(); n != 1 {
		t.Fatalf("closer ran %d times, want 1", n)
	}
}

// A closer that has run is finished with. Retaining it would re-run teardown on
// a connection that is already gone, and would mean the registry grows for the
// life of the daemon.
func TestCloseDeviceRunsEachCloserOnce(t *testing.T) {
	reg := front.NewRegistry()

	ch, closer, calls := closeSignal()
	reg.Add("device-1", closer)

	reg.CloseDevice("device-1")
	waitClosed(t, ch, "CloseDevice did not close the connection")
	reg.CloseDevice("device-1")

	// Give a second run somewhere to show up before believing there wasn't one.
	time.Sleep(notClosed)
	if n := calls.Load(); n != 1 {
		t.Fatalf("closer ran %d times across two CloseDevice calls, want 1", n)
	}
}

// The real closer ends a WebSocket and a PTY session, and those teardown paths
// call back into the registry to deregister themselves. Re-entering from a
// closer must be safe, and it is safe only because closers run off the lock: a
// closer called inline under it deadlocks the daemon on the first revocation.
func TestCloserMayReenterTheRegistry(t *testing.T) {
	reg := front.NewRegistry()

	done := make(chan struct{})
	var remove func()
	var reenterOK atomic.Bool

	remove, _ = reg.Add("device-1", func() {
		defer close(done)
		remove()                    // the deregistration the handler defers
		reg.CloseDevice("device-1") // a second revocation arriving mid-teardown
		_, ok := reg.Add("device-2", func() {})
		reenterOK.Store(ok)
	})

	// CloseDevice itself must not deadlock either, so it is watched too.
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		reg.CloseDevice("device-1")
	}()

	waitClosed(t, returned, "CloseDevice never returned: the registry deadlocked")
	waitClosed(t, done, "the closer never finished: re-entering the registry from a closer deadlocked, which is what holding the lock across a closer does")
	if !reenterOK.Load() {
		t.Fatal("Add from inside a closer refused a device that was never revoked")
	}
}

// One tab's teardown blowing up must not leave the other tabs of a revoked
// laptop connected.
func TestPanickingCloserDoesNotStopTheOthers(t *testing.T) {
	reg := front.NewRegistry()

	panicked := make(chan struct{})
	reg.Add("device-1", func() {
		close(panicked)
		panic("teardown blew up")
	})
	ch, closer, _ := closeSignal()
	reg.Add("device-1", closer)

	reg.CloseDevice("device-1")

	waitClosed(t, panicked, "the panicking closer never ran")
	waitClosed(t, ch, "a panicking closer stopped the other connections of the revoked device from being closed")
}

// Ending a session kills tmux, waits for it and then talks to the server under
// a five-second timeout. A wedged one must not hold up the operator's revoke
// request or the other connections being severed.
func TestBlockedCloserWedgesNeitherTheCallerNorTheOthers(t *testing.T) {
	reg := front.NewRegistry()

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	blocking := make(chan struct{})
	reg.Add("device-1", func() {
		close(blocking)
		<-release
	})
	ch, closer, _ := closeSignal()
	reg.Add("device-1", closer)

	returned := make(chan struct{})
	go func() {
		defer close(returned)
		reg.CloseDevice("device-1")
	}()

	waitClosed(t, blocking, "the blocking closer never ran")
	waitClosed(t, returned, "CloseDevice blocked on a closer; a wedged tmux server would hang the revoke request")
	waitClosed(t, ch, "one blocked closer stopped another connection of the revoked device from being closed")
}

// The window the plan's ordering leaves open: the store drops the device, the
// sweep runs, and only then does a connection that authenticated just before
// the removal register itself. Device sessions never expire, so that connection
// would keep its shell forever.
func TestAddAfterCloseDeviceIsRefusedAndClosedAnyway(t *testing.T) {
	reg := front.NewRegistry()

	reg.CloseDevice("device-1")

	ch, closer, _ := closeSignal()
	remove, ok := reg.Add("device-1", closer)
	if ok {
		t.Fatal("Add accepted a registration for an already-revoked device; that connection would survive its own revocation")
	}
	waitClosed(t, ch, "Add did not close the closer of an already-revoked device, so a caller that ignores ok keeps its shell")
	if remove == nil {
		t.Fatal("Add returned a nil deregistration; the caller's deferred remove would panic")
	}
	remove()

	// The refusal is per device, not global.
	other, otherCloser, _ := closeSignal()
	if _, ok := reg.Add("device-2", otherCloser); !ok {
		t.Fatal("revoking one device refused registrations for another")
	}
	mustNotClose(t, other, "registering a connection for a live device closed it immediately")
}

func TestCloseDeviceForAnUnknownDeviceIsANoOp(t *testing.T) {
	reg := front.NewRegistry()

	ch, closer, _ := closeSignal()
	reg.Add("device-1", closer)

	reg.CloseDevice("no-such-device")

	mustNotClose(t, ch, "revoking an unknown device closed a live device's connection")
}

// Connections come and go on request goroutines while revocation runs on
// another. Run under -race: this is the test that fails if the mutex goes.
func TestConcurrentAddRemoveAndClose(t *testing.T) {
	reg := front.NewRegistry()

	const (
		devices = 4
		workers = 16
		rounds  = 50
	)
	deviceIDs := []string{"device-0", "device-1", "device-2", "device-3"}

	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range rounds {
				id := deviceIDs[(w+i)%devices]
				remove, ok := reg.Add(id, func() {})
				if ok {
					remove()
				}
			}
		}()
	}
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range rounds {
				reg.CloseDevice(deviceIDs[(w+i)%devices])
			}
		}()
	}
	wg.Wait()

	// Every device has been revoked by now, so nothing may still be registered
	// and every new registration is refused.
	for _, id := range deviceIDs {
		if _, ok := reg.Add(id, func() {}); ok {
			t.Fatalf("%s accepted a registration after being revoked", id)
		}
	}
}

// Live is what stops the daemon capturing panes for nobody. Every state on the
// sidebar depends on it answering true while a tab is open and false once the
// last one goes, so both edges are pinned -- and the middle, because a registry
// that reported true for one device's tab while another device's was open would
// look correct in a single-device test.
func TestLiveFollowsTheOpenConnections(t *testing.T) {
	r := front.NewRegistry()
	if r.Live() {
		t.Fatal("Live() on an empty registry = true, want false")
	}

	removeA, _ := r.Add("dev-a", func() {})
	if !r.Live() {
		t.Fatal("Live() with one connection = false, want true")
	}
	removeB1, _ := r.Add("dev-b", func() {})
	removeB2, _ := r.Add("dev-b", func() {})

	// Two tabs on one device: closing one leaves the device connected.
	removeB1()
	if !r.Live() {
		t.Fatal("Live() after one of a device's two tabs closed = false, want true")
	}
	removeA()
	if !r.Live() {
		t.Fatal("Live() with dev-b still connected = false, want true")
	}
	removeB2()
	if r.Live() {
		t.Fatal("Live() after the last connection closed = true, want false")
	}

	// Revocation is the other way a connection ends, and it takes the device's
	// whole entry rather than removing one connection.
	r.Add("dev-c", func() {})
	if !r.Live() {
		t.Fatal("Live() after a fresh Add = false, want true")
	}
	r.CloseDevice("dev-c")
	if r.Live() {
		t.Fatal("Live() after the only device was revoked = true, want false")
	}
}
