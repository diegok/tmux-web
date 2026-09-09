package auth

import (
	"net"
	"time"
)

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

// ListenAdminForTest is ListenAdmin with the service uid and the peer
// credential source injected. A unit test cannot create a second uid, so this
// is how the refusal path itself -- close the connection, keep serving -- is
// exercised on every machine. TestForeignUIDIsRefusedOverARealSocket covers the
// same path with a genuinely foreign uid where the machine allows it.
func ListenAdminForTest(path string, self uint32, peerUID func(*net.UnixConn) (uint32, error)) (net.Listener, error) {
	return listenAdmin(path, self, peerUID)
}

// PeerAllowedForTest exposes the uid decision, which takes a uid rather than a
// connection precisely so it can be tested without one.
func PeerAllowedForTest(peer, self uint32) bool {
	return peerAllowed(peer, self)
}

// PeerCredForTest returns the peer's uid and pid. Production needs only the
// uid; the pid is what lets a test tell "read the peer's credentials" from
// "report our own" without a second uid to compare against.
func PeerCredForTest(c *net.UnixConn) (uid uint32, pid int32, err error) {
	cred, err := peerCred(c)
	if err != nil {
		return 0, 0, err
	}
	return cred.Uid, cred.Pid, nil
}

// SetRuntimeDirRoot points the /run/user lookup in AdminSocketPath at a
// directory a test controls, returning the previous value to restore.
func SetRuntimeDirRoot(dir string) string {
	prev := runtimeDirRoot
	runtimeDirRoot = dir
	return prev
}
