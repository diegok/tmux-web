package auth_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/diegok/tmux-web/internal/auth"
)

const baseURL = "https://tmux.example.com"

// newAdmin returns a store, an enroller, and the admin handler over them.
func newAdmin(t *testing.T) (*auth.Store, *auth.Enroller, http.Handler) {
	t.Helper()
	st, err := auth.OpenStore(filepath.Join(t.TempDir(), "devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	en := auth.NewEnroller(st)
	return st, en, auth.AdminMux(auth.AdminConfig{Store: st, Enroller: en, BaseURL: baseURL})
}

// serveAdmin listens on a socket inside dir and serves the admin API on it,
// returning the socket path.
func serveAdmin(t *testing.T, dir string, h http.Handler) string {
	t.Helper()
	sock := filepath.Join(dir, "admin.sock")
	ln, err := auth.ListenAdmin(sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return sock
}

// -- the socket itself ------------------------------------------------------

// The socket is the only door to minting credentials, so its own permissions
// are part of the auth chain: the uid check is the second lock, not the first.
func TestListenAdminCreatesAPrivateSocketInAPrivateDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run")
	sock := filepath.Join(dir, "admin.sock")

	ln, err := auth.ListenAdmin(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	fi, err := os.Stat(sock)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSocket == 0 {
		t.Fatalf("%s is not a socket: %v", sock, fi.Mode())
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode %v, want 0600: any other local user could connect", fi.Mode().Perm())
	}

	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm()&0o077 != 0 {
		t.Fatalf("socket directory mode %v: it must not be traversable by other users", di.Mode().Perm())
	}
}

// A daemon killed with SIGKILL leaves its socket file behind. Refusing to start
// afterwards would need a manual rm before every restart.
func TestListenAdminReplacesAStaleSocket(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "admin.sock")

	// A crashed daemon: the socket file outlives the process listening on it.
	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	stale.SetUnlinkOnClose(false)
	if err := stale.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("test setup did not leave a stale socket: %v", err)
	}

	ln, err := auth.ListenAdmin(sock)
	if err != nil {
		t.Fatalf("stale socket must be replaced, not fatal: %v", err)
	}
	defer ln.Close()

	// And the replacement is live, not merely bound.
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
}

// The stale-socket unlink must not steal the socket from a daemon that is still
// running: two daemons would then both hold the runtime dir with only one
// reachable, and `revoke` would land in whichever won the race.
func TestListenAdminRefusesWhenAnotherDaemonIsListening(t *testing.T) {
	dir := t.TempDir()
	_, _, h := newAdmin(t)
	sock := serveAdmin(t, dir, h)

	ln, err := auth.ListenAdmin(sock)
	if err == nil {
		ln.Close()
		t.Fatal("ListenAdmin on a live socket succeeded; it stole the socket from the running daemon")
	}
	if !errors.Is(err, auth.ErrAdminSocketInUse) {
		t.Fatalf("error = %v, want ErrAdminSocketInUse", err)
	}

	// The first daemon is untouched.
	if code, _ := adminGet(t, sock, "/devices"); code != http.StatusOK {
		t.Fatalf("the running daemon stopped answering after a second ListenAdmin: %d", code)
	}
}

// The path is ours by name, but "unlink whatever is there" is how a daemon ends
// up deleting a file it did not create.
func TestListenAdminRefusesToRemoveANonSocket(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "admin.sock")
	if err := os.WriteFile(sock, []byte("not a socket"), 0o600); err != nil {
		t.Fatal(err)
	}

	ln, err := auth.ListenAdmin(sock)
	if err == nil {
		ln.Close()
		t.Fatal("ListenAdmin replaced a regular file at the socket path")
	}
	if got, err := os.ReadFile(sock); err != nil || string(got) != "not a socket" {
		t.Fatalf("the file was destroyed: %q, %v", got, err)
	}
}

// -- peer credentials -------------------------------------------------------

func TestPeerUIDReportsTheConnectingUID(t *testing.T) {
	dir := t.TempDir()
	_, _, h := newAdmin(t)
	sock := serveAdmin(t, dir, h)

	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	uid, err := auth.PeerUID(c.(*net.UnixConn))
	if err != nil {
		t.Fatal(err)
	}
	if uid != uint32(os.Getuid()) {
		t.Fatalf("PeerUID = %d, want %d", uid, os.Getuid())
	}
}

// A unit test cannot mint a second uid on demand, so a uid comparison alone
// cannot tell "read the peer's credentials" from "report our own". The pid can:
// the peer here is a child process, and its pid is not ours.
func TestPeerCredentialsDescribeThePeerNotThisProcess(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "admin.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperDialsAdminSocket$")
	cmd.Env = append(os.Environ(), "WTERM_TEST_HELPER_SOCK="+sock)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})

	ln.(*net.UnixListener).SetDeadline(time.Now().Add(30 * time.Second))
	c, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	uid, pid, err := auth.PeerCredForTest(c.(*net.UnixConn))
	if err != nil {
		t.Fatal(err)
	}
	if pid != int32(cmd.Process.Pid) {
		t.Fatalf("peer pid = %d, want the child's %d (credentials must come from the connection, "+
			"not from this process)", pid, cmd.Process.Pid)
	}
	if uid != uint32(os.Getuid()) {
		t.Fatalf("peer uid = %d, want %d", uid, os.Getuid())
	}
}

// TestHelperDialsAdminSocket is not a test: it is the child process
// TestPeerCredentialsDescribeThePeerNotThisProcess reads credentials from.
func TestHelperDialsAdminSocket(t *testing.T) {
	sock := os.Getenv("WTERM_TEST_HELPER_SOCK")
	if sock == "" {
		t.Skip("helper process for TestPeerCredentialsDescribeThePeerNotThisProcess")
	}
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	io.Copy(io.Discard, c) // hold the connection open until the parent closes it
}

// -- the uid decision -------------------------------------------------------

func TestPeerAllowedOnlyForTheServiceUID(t *testing.T) {
	const self = 1000
	for _, tc := range []struct {
		peer uint32
		want bool
	}{
		{self, true},
		{self + 1, false},
		{0, false}, // root is another uid, not a superuser exception
		{^uint32(0), false},
	} {
		if got := auth.PeerAllowedForTest(tc.peer, self); got != tc.want {
			t.Errorf("peerAllowed(%d, %d) = %v, want %v", tc.peer, self, got, tc.want)
		}
	}
}

// The rejection has to close the connection and carry on. Returning an error
// from Accept instead would stop http.Serve for good, so one foreign connection
// would take the admin socket down until the next restart.
func TestForeignPeerIsClosedAndTheListenerKeepsServing(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "admin.sock")

	var foreign atomic.Bool
	foreign.Store(true)
	self := uint32(os.Getuid())
	ln, err := auth.ListenAdminForTest(sock, self, func(*net.UnixConn) (uint32, error) {
		if foreign.Load() {
			return self + 1, nil
		}
		return self, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	_, _, h := newAdmin(t)
	srv := &http.Server{Handler: h}
	go srv.Serve(ln)
	defer srv.Close()

	if body := rawRequest(t, sock); body != "" {
		t.Fatalf("a foreign uid was served %q; the connection must be closed unanswered", body)
	}

	foreign.Store(false)
	if !strings.Contains(rawRequest(t, sock), "200") {
		t.Fatal("the listener stopped serving after refusing a foreign connection")
	}
}

// Credentials that cannot be read are not credentials: with no proof of who is
// on the other end there is nothing to authorize.
func TestUnreadablePeerCredentialsAreRefused(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "admin.sock")
	ln, err := auth.ListenAdminForTest(sock, uint32(os.Getuid()),
		func(*net.UnixConn) (uint32, error) { return 0, errors.New("boom") })
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	_, _, h := newAdmin(t)
	srv := &http.Server{Handler: h}
	go srv.Serve(ln)
	defer srv.Close()

	if body := rawRequest(t, sock); body != "" {
		t.Fatalf("served %q despite unreadable peer credentials", body)
	}
}

// The real thing: a process running as a genuinely different uid, produced with
// an unprivileged user namespace and this user's subuid range. It skips wherever
// that is unavailable rather than pretending -- but where it runs, it is the
// only test that exercises the rejection end to end.
//
// The socket is deliberately opened to 0666 in a world-traversable directory
// first, so that what refuses the connection is the uid check and not the
// filesystem.
func TestForeignUIDIsRefusedOverARealSocket(t *testing.T) {
	dir := t.TempDir()
	requireForeignUID(t, dir)

	_, _, h := newAdmin(t)
	sock := serveAdmin(t, dir, h)
	if err := os.Chmod(sock, 0o666); err != nil {
		t.Fatal(err)
	}

	// connect() must succeed for both callers -- the socket was just opened to
	// 0666 -- so whatever the foreign caller fails to get, it is the uid check
	// that denied it. A refused connection is closed with the request still
	// unread, which the peer sees as a reset rather than a clean EOF.
	const dial = `
import socket, sys
s = socket.socket(socket.AF_UNIX)
s.connect(sys.argv[1])
try:
    s.sendall(b"GET /devices HTTP/1.0\r\nHost: admin\r\n\r\n")
    data = s.recv(4096)
except (BrokenPipeError, ConnectionResetError):
    data = b""
sys.stdout.write(repr(data))
`
	mine, err := exec.Command(pythonBin, "-c", dial, sock).Output()
	if err != nil {
		t.Fatalf("the service uid must be served: %v", err)
	}
	if !strings.Contains(string(mine), "200 OK") {
		t.Fatalf("the service uid was not served: %s", mine)
	}

	theirs, err := foreignRun(pythonBin, "-c", dial, sock)
	if err != nil {
		t.Fatalf("the foreign process could not connect at all, so nothing was proven: %v: %s", err, theirs)
	}
	if theirs != "b''" {
		t.Fatalf("a foreign uid was served %s; it must get nothing at all", theirs)
	}
}

// requireForeignUID makes dir reachable and writable by another uid and returns
// a directory inside it that such a process can create entries in, skipping the
// test when this machine cannot run a process as a uid that is not ours.
//
// The precondition is established without the code under test: a foreign
// process creates a file, and the kernel's own record of its owner is what says
// the uid really was foreign. Believing PeerUID here would make the test agree
// with whatever PeerUID reports.
func requireForeignUID(t *testing.T, dir string) string {
	t.Helper()
	for _, bin := range []string{pythonBin, "/usr/bin/unshare", "/usr/bin/setpriv"} {
		if _, err := os.Stat(bin); err != nil {
			t.Skipf("%s is required to run a process as another uid", bin)
		}
	}
	for _, d := range []string{filepath.Dir(dir), dir} {
		if err := os.Chmod(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writable := filepath.Join(dir, "w")
	if err := os.Mkdir(writable, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(writable, 0o777); err != nil { // Mkdir applied the umask
		t.Fatal(err)
	}

	probe := filepath.Join(writable, "probe")
	if out, err := foreignRun(pythonBin, "-c", "import sys; open(sys.argv[1], 'w').close()", probe); err != nil {
		t.Skipf("cannot run a process as another uid here: %v: %s", err, out)
	}
	fi, err := os.Stat(probe)
	if err != nil {
		t.Skipf("the foreign process could not reach %s: %v", dir, err)
	}
	if uid := fi.Sys().(*syscall.Stat_t).Uid; uid == uint32(os.Getuid()) {
		t.Skipf("user namespace mapping produced our own uid (%d); no foreign uid available", uid)
	}
	return writable
}

// pythonBin is the absolute path on purpose: the foreign uid cannot read a
// version-manager shim under this user's home.
const pythonBin = "/usr/bin/python3"

// foreignRun runs argv as a uid that is not this one, using an unprivileged
// user namespace: --map-auto maps this user's subuid range, and setpriv steps
// into the first id of it.
func foreignRun(argv ...string) (string, error) {
	args := append([]string{"--map-auto", "--map-root-user", "--",
		"setpriv", "--reuid=1", "--regid=1", "--clear-groups", "--"}, argv...)
	cmd := exec.Command("/usr/bin/unshare", args...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=/nonexistent"}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return errb.String(), err
	}
	return out.String(), nil
}

// -- where the socket lives -------------------------------------------------

func TestAdminSocketPathPrefersTheRuntimeDir(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/4242")

	got, err := auth.AdminSocketPath()
	if err != nil {
		t.Fatal(err)
	}
	if want := "/run/user/4242/wterm-web.sock"; got != want {
		t.Fatalf("AdminSocketPath = %q, want %q", got, want)
	}
}

// The basedir spec says a relative $XDG_RUNTIME_DIR must be ignored, and a
// relative path here would put the socket wherever the process happens to have
// been started -- a different place for the daemon than for the CLI.
func TestAdminSocketPathIgnoresARelativeRuntimeDir(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "relative/run")
	root := t.TempDir()
	defer auth.SetRuntimeDirRoot(auth.SetRuntimeDirRoot(root))
	mine := filepath.Join(root, strconv.Itoa(os.Getuid()))
	if err := os.Mkdir(mine, 0o700); err != nil {
		t.Fatal(err)
	}

	got, err := auth.AdminSocketPath()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(mine, "wterm-web.sock"); got != want {
		t.Fatalf("AdminSocketPath = %q, want %q", got, want)
	}
}

// Without a runtime directory the socket goes next to the device store, under
// the state dir -- never /tmp, where another local user could create the path
// first and either deny the daemon its socket or answer the CLI in its place.
func TestAdminSocketPathFallsBackToThePrivateStateDir(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "")
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	defer auth.SetRuntimeDirRoot(auth.SetRuntimeDirRoot(filepath.Join(t.TempDir(), "absent")))

	got, err := auth.AdminSocketPath()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(state, "wterm-web", "wterm-web.sock"); got != want {
		t.Fatalf("AdminSocketPath = %q, want %q", got, want)
	}

	// And with no XDG variable at all it follows HOME, not a shared directory.
	t.Setenv("XDG_STATE_HOME", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	got, err = auth.AdminSocketPath()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".local", "state", "wterm-web", "wterm-web.sock"); got != want {
		t.Fatalf("AdminSocketPath = %q, want %q", got, want)
	}
}

// A runtime directory belonging to someone else, or that is not a directory at
// all, is not one to put a credential-minting socket in.
func TestAdminSocketPathSkipsAnUnusableRuntimeDir(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "")
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)

	root := t.TempDir()
	defer auth.SetRuntimeDirRoot(auth.SetRuntimeDirRoot(root))
	// Not a directory: the name exists, so a bare existence check would take it.
	if err := os.WriteFile(filepath.Join(root, strconv.Itoa(os.Getuid())), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := auth.AdminSocketPath()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(state, "wterm-web", "wterm-web.sock"); got != want {
		t.Fatalf("AdminSocketPath = %q, want %q", got, want)
	}
}

// A /run/user/<uid> that exists but belongs to another user is not a runtime
// directory of ours. Ownership is what says so; existence does not.
func TestAdminSocketPathSkipsARuntimeDirOwnedByAnotherUser(t *testing.T) {
	dir := t.TempDir()
	root := requireForeignUID(t, dir)

	mine := strconv.Itoa(os.Getuid())
	if out, err := foreignRun("/usr/bin/mkdir", filepath.Join(root, mine)); err != nil {
		t.Skipf("could not create a directory owned by another uid: %v: %s", err, out)
	}
	fi, err := os.Stat(filepath.Join(root, mine))
	if err != nil || fi.Sys().(*syscall.Stat_t).Uid == uint32(os.Getuid()) {
		t.Skipf("the runtime directory is not owned by another uid: %v", err)
	}

	t.Setenv("XDG_RUNTIME_DIR", "")
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	defer auth.SetRuntimeDirRoot(auth.SetRuntimeDirRoot(root))

	got, err := auth.AdminSocketPath()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(state, "wterm-web", "wterm-web.sock"); got != want {
		t.Fatalf("AdminSocketPath = %q, want %q: another user's directory must not hold our socket", got, want)
	}
}

// -- the admin API ----------------------------------------------------------

func TestMintedLinkCarriesARedeemableTokenInItsFragment(t *testing.T) {
	st, en, h := newAdmin(t)

	code, body := adminDo(t, h, http.MethodPost, "/enroll", `{"name":"laptop"}`)
	if code != http.StatusOK {
		t.Fatalf("POST /enroll = %d: %s", code, body)
	}
	var got struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	if got.Name != "laptop" {
		t.Errorf("name = %q, want laptop", got.Name)
	}

	prefix := baseURL + "/enroll#"
	if !strings.HasPrefix(got.URL, prefix) {
		t.Fatalf("url = %q, want the token in the fragment of %s (a token in the path or query "+
			"would reach the server's logs and any Referer)", got.URL, prefix)
	}
	token := strings.TrimPrefix(got.URL, prefix)

	deviceToken, err := en.Redeem(token, "Mozilla/5.0")
	if err != nil {
		t.Fatalf("the minted token did not redeem: %v", err)
	}
	d, ok := st.Lookup(deviceToken)
	if !ok || d.Name != "laptop" {
		t.Fatalf("enrolled device = %+v, %v", d, ok)
	}
}

func TestMintRequiresANameAndBurnsNoTokenWithoutOne(t *testing.T) {
	_, en, h := newAdmin(t)

	for _, body := range []string{`{}`, `{"name":""}`, `{"name":"   "}`} {
		if code, got := adminDo(t, h, http.MethodPost, "/enroll", body); code != http.StatusBadRequest {
			t.Errorf("POST /enroll %s = %d, want 400: %s", body, code, got)
		}
	}
	if n := en.PendingCount(); n != 0 {
		t.Fatalf("%d tokens minted for rejected requests; a refused mint must not leave a live link", n)
	}
}

// The URL is built from the daemon's own configured host: the CLI cannot know
// it. Minting a token we then cannot render into a link would burn a live
// credential for nothing, so the check comes first.
func TestMintWithoutAConfiguredBaseURLFailsBeforeMinting(t *testing.T) {
	st, err := auth.OpenStore(filepath.Join(t.TempDir(), "devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	en := auth.NewEnroller(st)
	h := auth.AdminMux(auth.AdminConfig{Store: st, Enroller: en})

	code, body := adminDo(t, h, http.MethodPost, "/enroll", `{"name":"laptop"}`)
	if code != http.StatusInternalServerError {
		t.Fatalf("POST /enroll = %d, want 500: %s", code, body)
	}
	if n := en.PendingCount(); n != 0 {
		t.Fatalf("%d tokens minted; a mint that cannot produce a link must not leave one live", n)
	}
}

func TestDevicesListsEnrolledDevicesWithoutTheirTokenHashes(t *testing.T) {
	st, _, h := newAdmin(t)
	token, err := st.AddDevice("laptop", "Mozilla/5.0")
	if err != nil {
		t.Fatal(err)
	}
	d := st.Devices()[0]

	code, body := adminDo(t, h, http.MethodGet, "/devices", "")
	if code != http.StatusOK {
		t.Fatalf("GET /devices = %d: %s", code, body)
	}
	for _, want := range []string{d.ID, "laptop", "Mozilla/5.0"} {
		if !strings.Contains(body, want) {
			t.Errorf("GET /devices omitted %q: %s", want, body)
		}
	}
	if strings.Contains(body, d.TokenHash) || strings.Contains(body, "token_hash") || strings.Contains(body, token) {
		t.Fatalf("the devices listing must carry no credential material: %s", body)
	}
}

func TestRevokeRemovesTheDevice(t *testing.T) {
	st, _, h := newAdmin(t)
	token, err := st.AddDevice("laptop", "Mozilla/5.0")
	if err != nil {
		t.Fatal(err)
	}
	id := st.Devices()[0].ID

	code, body := adminDo(t, h, http.MethodDelete, "/devices/"+id, "")
	if code != http.StatusNoContent {
		t.Fatalf("DELETE /devices/%s = %d: %s", id, code, body)
	}
	if _, ok := st.Lookup(token); ok {
		t.Fatal("the revoked device's token still authenticates")
	}
}

// Revoking a mistyped id must not report success: that is how an operator ends
// up believing a lost laptop was cut off.
func TestRevokeUnknownDeviceIsNotFound(t *testing.T) {
	_, _, h := newAdmin(t)
	if code, body := adminDo(t, h, http.MethodDelete, "/devices/nope", ""); code != http.StatusNotFound {
		t.Fatalf("DELETE /devices/nope = %d, want 404: %s", code, body)
	}
}

func TestWrongMethodsAreRefused(t *testing.T) {
	_, _, h := newAdmin(t)
	for _, tc := range [][2]string{
		{http.MethodGet, "/enroll"},
		{http.MethodPost, "/devices"},
		{http.MethodDelete, "/devices"},
	} {
		if code, body := adminDo(t, h, tc[0], tc[1], ""); code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s = %d, want 405: %s", tc[0], tc[1], code, body)
		}
	}
}

// The CLI's whole path: dial the socket, mint a link, redeem it, list what
// appeared. This is what Task 14's subcommands do.
func TestAdminClientDrivesTheAPIOverTheSocket(t *testing.T) {
	st, en, h := newAdmin(t)
	sock := serveAdmin(t, t.TempDir(), h)

	client := auth.AdminClient(sock)
	resp, err := client.Post(auth.AdminBaseURL+"/enroll", "application/json",
		strings.NewReader(`{"name":"laptop"}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /enroll = %d: %s", resp.StatusCode, body)
	}
	var minted struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(body, &minted); err != nil {
		t.Fatal(err)
	}

	_, frag, _ := strings.Cut(minted.URL, "#")
	if _, err := en.Redeem(frag, "Mozilla/5.0"); err != nil {
		t.Fatalf("redeem: %v", err)
	}

	code, list := adminGet(t, sock, "/devices")
	if code != http.StatusOK {
		t.Fatalf("GET /devices = %d: %s", code, list)
	}
	if !strings.Contains(list, st.Devices()[0].ID) {
		t.Fatalf("the enrolled device is missing from %s", list)
	}
}

// -- helpers ----------------------------------------------------------------

// adminDo drives the handler directly, without a socket.
func adminDo(t *testing.T, h http.Handler, method, path, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(method, "http://admin"+path, strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	res := rec.Result()
	defer res.Body.Close()
	out, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, string(out)
}

// adminGet runs one GET over a real admin socket.
func adminGet(t *testing.T, sock, path string) (int, string) {
	t.Helper()
	resp, err := auth.AdminClient(sock).Get(auth.AdminBaseURL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}

// rawRequest sends one HTTP/1.0 request over the socket and returns whatever
// came back, "" if the connection was closed unanswered.
func rawRequest(t *testing.T, sock string) string {
	t.Helper()
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := fmt.Fprint(c, "GET /devices HTTP/1.0\r\nHost: admin\r\n\r\n"); err != nil {
		// A refused connection may be closed before the write lands.
		return ""
	}
	out, _ := io.ReadAll(c)
	return string(out)
}
