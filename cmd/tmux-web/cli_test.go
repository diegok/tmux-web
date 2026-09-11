package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/diegok/tmux-web/internal/auth"
)

// The plan said these subcommands could not be tested without a running
// daemon. They can: AdminMux is an http.Handler and ListenAdmin takes a path,
// so a test stands up a real admin socket in a directory of its own, serves
// the real store and enroller on it, and drives the real command functions
// against it in-process. Nothing here forks a process, and nothing here
// touches the developer's own socket -- every test passes --socket explicitly,
// except the one that exists to check the default is used.

const testBaseURL = "https://tmux.example.com"

// admin is a real admin socket with a real store behind it.
type admin struct {
	store    *auth.Store
	enroller *auth.Enroller
	socket   string
	dir      string
}

// newAdmin serves the real admin API on a socket of its own.
func newAdmin(t *testing.T) *admin {
	t.Helper()
	a := newAdminDir(t)
	a.serve(t, auth.AdminMux(auth.AdminConfig{
		Store:    a.store,
		Enroller: a.enroller,
		BaseURL:  testBaseURL,
	}))
	return a
}

// newAdminDir builds the store and picks a socket path without listening, so a
// test can serve a stub handler on it instead.
func newAdminDir(t *testing.T) *admin {
	t.Helper()
	// Not t.TempDir: a unix socket path is capped at ~108 bytes and test names
	// here are long enough to matter.
	dir, err := os.MkdirTemp("", "wt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	st, err := auth.OpenStore(filepath.Join(dir, "devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	// The same basename AdminSocketPath derives, so a test can point
	// $XDG_RUNTIME_DIR at this directory and have the default path find it.
	return &admin{store: st, enroller: auth.NewEnroller(st), socket: filepath.Join(dir, "tmux-web.sock"), dir: dir}
}

func (a *admin) serve(t *testing.T, h http.Handler) {
	t.Helper()
	ln, err := auth.ListenAdmin(a.socket)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
}

// enroll mints a link and redeems it, returning the enrolled device's id.
func (a *admin) enroll(t *testing.T, name, userAgent string) string {
	t.Helper()
	token, err := a.enroller.Mint(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.enroller.Redeem(token, userAgent); err != nil {
		t.Fatal(err)
	}
	for _, d := range a.store.Devices() {
		if d.Name == name {
			return d.ID
		}
	}
	t.Fatalf("device %q is not in the store after redeeming its link", name)
	return ""
}

func (a *admin) deviceNames(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, d := range a.store.Devices() {
		out = append(out, d.Name)
	}
	return out
}

// result is one CLI invocation.
type result struct {
	code   int
	stdout string
	stderr string
}

func runCLI(args ...string) result {
	var out, errb bytes.Buffer
	code := run(args, &out, &errb)
	return result{code: code, stdout: out.String(), stderr: errb.String()}
}

func (r result) String() string {
	return fmt.Sprintf("exit %d\nstdout: %q\nstderr: %q", r.code, r.stdout, r.stderr)
}

// wants checks the exit code and that stderr explains itself.
func (r result) wantCode(t *testing.T, code int) {
	t.Helper()
	if r.code != code {
		t.Fatalf("exit code = %d, want %d\n%s", r.code, code, r)
	}
}

func (r result) wantStderrContains(t *testing.T, subs ...string) {
	t.Helper()
	for _, s := range subs {
		if !strings.Contains(r.stderr, s) {
			t.Errorf("stderr does not mention %q\n%s", s, r)
		}
	}
}

// -- enroll -----------------------------------------------------------------

// The fragment is the whole point of the link's shape: a token after '#' is
// never sent to a server, so it stays out of access logs and Referer headers,
// and the link scanners messaging apps run do not redeem it on the way past.
// Printing it in the query would be a silent downgrade with no visible symptom
// until a token is spent by a scanner or read out of a log.
func TestEnrollPrintsTheTokenInTheFragment(t *testing.T) {
	a := newAdmin(t)

	r := runCLI("enroll", "--name", "laptop", "--socket", a.socket)
	r.wantCode(t, 0)

	line := strings.TrimSpace(r.stdout)
	u, err := url.Parse(line)
	if err != nil {
		t.Fatalf("stdout is not a URL: %v\n%s", err, r)
	}
	if u.Fragment == "" {
		t.Fatalf("enrollment link carries no fragment: %q", line)
	}
	if u.RawQuery != "" {
		t.Fatalf("enrollment link carries a query string %q; the token must be in the fragment, which is never sent to the server", u.RawQuery)
	}
	if u.Scheme != "https" || u.Host != "tmux.example.com" || u.Path != "/enroll" {
		t.Errorf("link points somewhere unexpected: %q", line)
	}

	// It is the live token, not a decoration: redeeming it enrolls the device.
	if _, err := a.enroller.Redeem(u.Fragment, "Mozilla/5.0"); err != nil {
		t.Fatalf("the fragment is not a redeemable enrollment token: %v", err)
	}
	if got := a.deviceNames(t); len(got) != 1 || got[0] != "laptop" {
		t.Fatalf("devices after redeeming = %v, want [laptop]", got)
	}
}

// The CLI is the last place a human sees the link before pasting it. A daemon
// that handed back a query-string token -- a version skew, a mangled BaseURL --
// must not get it printed anyway.
func TestEnrollRefusesALinkThatWouldLeakItsToken(t *testing.T) {
	for _, tc := range []struct {
		name, url, want string
	}{
		{"query string", testBaseURL + "/enroll?token=abc123", "query"},
		{"no fragment", testBaseURL + "/enroll", "fragment"},
		{"empty fragment", testBaseURL + "/enroll#", "fragment"},
		{"not a url", "::nonsense::", "not a URL"},
		{"no scheme", "/enroll#abc123", "scheme"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newAdminDir(t)
			a.serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(map[string]string{"name": "laptop", "url": tc.url})
			}))

			r := runCLI("enroll", "--name", "laptop", "--socket", a.socket)
			r.wantCode(t, 1)
			r.wantStderrContains(t, tc.want)
			if r.stdout != "" {
				t.Fatalf("printed a link it should have refused: %q", r.stdout)
			}
		})
	}
}

// stdout carries the answer and nothing else, so the link can be piped into a
// QR encoder or a clipboard without a --quiet flag. The expiry note is real
// information, but it is commentary.
func TestEnrollPutsOnlyTheLinkOnStdout(t *testing.T) {
	a := newAdmin(t)

	r := runCLI("enroll", "--name", "phone", "--socket", a.socket)
	r.wantCode(t, 0)

	if n := strings.Count(r.stdout, "\n"); n != 1 {
		t.Fatalf("stdout has %d lines, want exactly one (the link)\n%s", n, r)
	}
	if !strings.HasPrefix(r.stdout, testBaseURL+"/enroll#") {
		t.Fatalf("stdout is not the bare link\n%s", r)
	}
	r.wantStderrContains(t, "single use", "10m", "phone")
	if strings.Contains(r.stderr, "/enroll#") {
		t.Errorf("the token is repeated on stderr; one copy is enough\n%s", r)
	}
}

func TestEnrollRequiresAName(t *testing.T) {
	a := newAdmin(t)

	for _, args := range [][]string{
		{"enroll", "--socket", a.socket},
		{"enroll", "--name", "   ", "--socket", a.socket},
	} {
		r := runCLI(args...)
		r.wantCode(t, 2)
		r.wantStderrContains(t, "--name")
		if r.stdout != "" {
			t.Errorf("wrote to stdout for a bad command line: %q", r.stdout)
		}
	}
}

// A bare word after `enroll` is someone forgetting --name. Accepting it
// silently would mint a device called "" -- or, once the daemon rejects that,
// report a confusing error about a name they did type.
func TestEnrollRejectsAPositionalName(t *testing.T) {
	a := newAdmin(t)

	r := runCLI("enroll", "laptop", "--socket", a.socket)
	r.wantCode(t, 2)
	r.wantStderrContains(t, "--name laptop")
}

// -- devices ----------------------------------------------------------------

func TestDevicesListsWhatIsEnrolled(t *testing.T) {
	a := newAdmin(t)
	laptop := a.enroll(t, "laptop", "Mozilla/5.0 (X11; Linux x86_64) Firefox/128.0")
	phone := a.enroll(t, "phone", "Mozilla/5.0 (iPhone) Safari/605.1")

	r := runCLI("devices", "--socket", a.socket)
	r.wantCode(t, 0)

	for _, want := range []string{"laptop", "phone", laptop, phone, "Firefox", "ID", "NAME", "LAST SEEN"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("devices output does not mention %q\n%s", want, r)
		}
	}
	// The listing exists so the owner can pick an id for revoke, so the id has
	// to survive whole rather than be truncated into an ambiguous prefix.
	for _, id := range []string{laptop, phone} {
		if !strings.Contains(r.stdout, id) {
			t.Errorf("device id %q is not printed in full\n%s", id, r)
		}
	}
}

// A token hash is the one thing in the store that must never be printed. The
// admin API already keeps it off the wire; this asserts the CLI does not find
// some other way to surface it.
func TestDevicesPrintsNoCredentialMaterial(t *testing.T) {
	a := newAdmin(t)
	a.enroll(t, "laptop", "Firefox")

	r := runCLI("devices", "--socket", a.socket)
	r.wantCode(t, 0)
	for _, d := range a.store.Devices() {
		if d.TokenHash != "" && strings.Contains(r.stdout, d.TokenHash) {
			t.Fatalf("the devices table printed a token hash\n%s", r)
		}
	}
}

// An empty table with a header row reads as a broken command, and a note on
// stdout would land in whatever the output was piped into.
func TestDevicesSaysSoWhenThereAreNone(t *testing.T) {
	a := newAdmin(t)

	r := runCLI("devices", "--socket", a.socket)
	r.wantCode(t, 0)
	if r.stdout != "" {
		t.Errorf("stdout should be empty when there are no devices, got %q", r.stdout)
	}
	r.wantStderrContains(t, "no devices", "enroll")
}

// The user-agent is written by whichever browser redeemed a link. A leaked
// link redeemed by someone else puts a string of their choosing into this
// table, and ANSI escapes in it could clear the screen or hide the row above --
// which is to say, hide the device the owner came here to find.
func TestDeviceTableNeutralisesTerminalEscapes(t *testing.T) {
	var buf bytes.Buffer
	writeDeviceTable(&buf, []deviceRow{{
		ID:        "abc123",
		Name:      "laptop",
		UserAgent: "Mozilla\x1b[2J\x1b[1;1H evil \r\n more",
		CreatedAt: time.Now(),
		LastSeen:  time.Now(),
	}}, time.Now())

	out := buf.String()
	for _, bad := range []string{"\x1b", "\r"} {
		if strings.Contains(out, bad) {
			t.Errorf("control character %q reached the terminal: %q", bad, out)
		}
	}
	if lines := strings.Count(strings.TrimRight(out, "\n"), "\n"); lines != 1 {
		t.Errorf("a newline in a user-agent broke the table into %d extra lines: %q", lines, out)
	}
}

// Times are relative because the question this table answers is comparative --
// which of these is still in use -- and nobody subtracts RFC 3339 timestamps in
// their head. Past a month a relative age stops carrying information.
func TestHumanAge(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		when time.Time
		want string
	}{
		{now.Add(-10 * time.Second), "just now"},
		{now.Add(-90 * time.Second), "1m ago"},
		{now.Add(-3 * time.Hour), "3h ago"},
		{now.Add(-72 * time.Hour), "3d ago"},
		{now.Add(-400 * 24 * time.Hour), "2025-08-05"},
		{time.Time{}, "never"},
		{now.Add(48 * time.Hour), "2026-09-11 (future)"},
	} {
		if got := humanAge(tc.when, now); got != tc.want {
			t.Errorf("humanAge(%v) = %q, want %q", tc.when, got, tc.want)
		}
	}
}

func TestDisplayTruncatesWithoutSplittingRunes(t *testing.T) {
	if got := display("ααααα", 3); got != "ααα…" {
		t.Errorf("display truncation = %q, want %q", got, "ααα…")
	}
	if got := display("", 10); got != "-" {
		t.Errorf("display of an empty value = %q, want a placeholder", got)
	}
	if got := display("ok", 10); got != "ok" {
		t.Errorf("display mangled a normal value: %q", got)
	}
}

// -- revoke -----------------------------------------------------------------

// revoke is how someone responds to a lost laptop, so it has to act on the id
// it was given and only that one.
func TestRevokeRemovesTheDeviceItWasGiven(t *testing.T) {
	a := newAdmin(t)
	laptop := a.enroll(t, "laptop", "Firefox")
	a.enroll(t, "phone", "Safari")

	r := runCLI("revoke", laptop, "--socket", a.socket)
	r.wantCode(t, 0)
	r.wantStderrContains(t, "revoked", laptop)

	got := a.deviceNames(t)
	if len(got) != 1 || got[0] != "phone" {
		t.Fatalf("devices after revoking the laptop = %v, want [phone]", got)
	}
}

// Revoking an id the store does not hold must fail loudly: reporting success
// would leave the owner believing a lost device was cut off when it still has
// a shell.
func TestRevokeUnknownIDFails(t *testing.T) {
	a := newAdmin(t)
	a.enroll(t, "laptop", "Firefox")

	r := runCLI("revoke", "nosuchdevice", "--socket", a.socket)
	r.wantCode(t, 1)
	r.wantStderrContains(t, "nosuchdevice")
	if names := a.deviceNames(t); len(names) != 1 {
		t.Fatalf("a failed revoke changed the store: %v", names)
	}
}

func TestRevokeNeedsExactlyOneID(t *testing.T) {
	a := newAdmin(t)
	laptop := a.enroll(t, "laptop", "Firefox")
	phone := a.enroll(t, "phone", "Safari")

	for _, args := range [][]string{
		{"revoke", "--socket", a.socket},
		// Two ids must not revoke the first and drop the second in silence:
		// the owner would be told one device was cut off while the other kept
		// its shell.
		{"revoke", laptop, phone, "--socket", a.socket},
	} {
		r := runCLI(args...)
		r.wantCode(t, 2)
		if names := a.deviceNames(t); len(names) != 2 {
			t.Fatalf("%v changed the store: %v", args, names)
		}
	}
}

// An id is put in a URL path. A slash in it must not steer the request at a
// different route.
func TestRevokeDoesNotLetAnIDEscapeItsRoute(t *testing.T) {
	a := newAdmin(t)
	a.enroll(t, "laptop", "Firefox")

	r := runCLI("revoke", "../devices", "--socket", a.socket)
	if r.code == 0 {
		t.Fatalf("revoking %q reported success\n%s", "../devices", r)
	}
	if names := a.deviceNames(t); len(names) != 1 {
		t.Fatalf("a traversal-shaped id changed the store: %v", names)
	}
}

// -- reaching the daemon ----------------------------------------------------

// A missing socket is the most likely failure this CLI has, and "dial unix:
// no such file or directory" tells the owner nothing about what to do next.
// Exiting 0 would be worse still: `devices` would print an empty list and read
// as "nothing is enrolled".
func TestCommandsFailLoudlyWhenTheDaemonIsNotRunning(t *testing.T) {
	dir, err := os.MkdirTemp("", "wt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	missing := filepath.Join(dir, "tmux-web.sock")

	for _, args := range [][]string{
		{"devices", "--socket", missing},
		{"enroll", "--name", "laptop", "--socket", missing},
		{"revoke", "someid", "--socket", missing},
	} {
		r := runCLI(args...)
		r.wantCode(t, 1)
		if r.stdout != "" {
			t.Errorf("%v wrote to stdout while failing: %q", args, r.stdout)
		}
		// The path, so the owner can look; what to run, so they can fix it;
		// and the override, because a daemon under a different
		// $XDG_RUNTIME_DIR is the case where the path is simply wrong.
		r.wantStderrContains(t, missing, "not appear to be running", "tmux-web serve", "--socket")
	}
}

// A socket file with nothing behind it is a daemon that died. It is a
// different situation from a missing file -- nothing needs cleaning up by
// hand, ListenAdmin does that on the next start -- so it gets its own
// sentence.
func TestAStaleSocketIsReportedAsADeadDaemon(t *testing.T) {
	a := newAdminDir(t)

	// What a SIGKILL leaves behind: the file is there, nobody is accepting.
	ln, err := auth.ListenAdmin(a.socket)
	if err != nil {
		t.Fatal(err)
	}
	ul, ok := ln.(interface{ SetUnlinkOnClose(bool) })
	if !ok {
		t.Fatalf("admin listener is a %T, which cannot be made to leave its socket file behind", ln)
	}
	ul.SetUnlinkOnClose(false)
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(a.socket); err != nil {
		t.Fatalf("the socket file did not survive Close, so this is the missing-file case: %v", err)
	}

	r := runCLI("devices", "--socket", a.socket)
	r.wantCode(t, 1)
	r.wantStderrContains(t, a.socket, "not running", "tmux-web serve")
}

// The daemon's own error text is what reaches the owner: they are the only
// caller, on a socket only they can open, so there is nobody to keep it from.
func TestTheDaemonsErrorIsWhatIsPrinted(t *testing.T) {
	a := newAdminDir(t)
	a.serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "auth: the disk is full"})
	}))

	r := runCLI("devices", "--socket", a.socket)
	r.wantCode(t, 1)
	r.wantStderrContains(t, "the disk is full")
}

// --socket is the escape hatch for a daemon whose $XDG_RUNTIME_DIR differs
// from the shell's. It is only an escape hatch if it wins.
func TestSocketFlagOverridesTheDefaultPath(t *testing.T) {
	byDefault := newAdmin(t)
	explicit := newAdmin(t)
	t.Setenv("XDG_RUNTIME_DIR", byDefault.dir)

	// Without the flag, the default path is what is used.
	runCLI("enroll", "--name", "default").wantCode(t, 0)
	if got := len(byDefault.store.Devices()); got != 0 {
		t.Fatalf("minting a link should not enrol anything yet, store has %d", got)
	}
	r := runCLI("devices", "--socket", explicit.socket)
	r.wantCode(t, 0)

	// And with it, the flag is.
	explicit.enroll(t, "chosen", "Firefox")
	r = runCLI("devices", "--socket", explicit.socket)
	r.wantCode(t, 0)
	if !strings.Contains(r.stdout, "chosen") {
		t.Fatalf("--socket did not reach the socket it named\n%s", r)
	}
	r = runCLI("devices")
	r.wantCode(t, 0)
	if strings.Contains(r.stdout, "chosen") {
		t.Fatalf("the default path reached the socket --socket named\n%s", r)
	}
}

// -- serve ------------------------------------------------------------------

// serve's server is Task 17. Its command line is not, and the flags the manual
// verification steps use are pinned here so they are not invented twice.
func TestServeParsesItsFlags(t *testing.T) {
	var got serveConfig
	restore := stubServe(t, func(cfg serveConfig, _, _ io.Writer) error {
		got = cfg
		return nil
	})
	defer restore()

	r := runCLI("serve", "--host", "tmux.example.com", "--dev", "--port", "9000", "--socket", "/tmp/x.sock")
	r.wantCode(t, 0)

	// TLSPort carries the flag's default rather than a zero, so that `serve -h`
	// can show it. The daemon treats 0 as 443 too, for programmatic callers.
	want := serveConfig{
		Host: "tmux.example.com", Dev: true, Port: 9000,
		Socket: "/tmp/x.sock", TLSPort: 443,
	}
	if got != want {
		t.Fatalf("serve config = %+v, want %+v", got, want)
	}
}

// Each mode is a different answer to "how does a browser get a trusted
// connection", so asking for two is a contradiction rather than a preference
// the daemon should resolve on its own.
func TestServeRefusesContradictoryTLSModes(t *testing.T) {
	restore := stubServe(t, func(serveConfig, io.Writer, io.Writer) error {
		t.Fatal("the daemon must not start with contradictory TLS flags")
		return nil
	})
	defer restore()

	for _, args := range [][]string{
		{"serve", "--host", "h", "--dev", "--self-signed"},
		{"serve", "--host", "h", "--dev", "--tls-cert", "c", "--tls-key", "k"},
		{"serve", "--host", "h", "--self-signed", "--tls-cert", "c", "--tls-key", "k"},
	} {
		r := runCLI(args...)
		if r.code == 0 {
			t.Errorf("%v was accepted, want a usage error", args[1:])
		}
	}
}

// A certificate without its key, or the reverse, is a misconfiguration that
// would otherwise surface as a TLS failure at first connection.
func TestServeRequiresCertAndKeyTogether(t *testing.T) {
	restore := stubServe(t, func(serveConfig, io.Writer, io.Writer) error {
		t.Fatal("the daemon must not start with half a keypair")
		return nil
	})
	defer restore()

	for _, args := range [][]string{
		{"serve", "--host", "h", "--tls-cert", "only-cert.pem"},
		{"serve", "--host", "h", "--tls-key", "only-key.pem"},
	} {
		if r := runCLI(args...); r.code == 0 {
			t.Errorf("%v was accepted, want a usage error", args[1:])
		}
	}
}

func TestServeParsesTLSFlags(t *testing.T) {
	var got serveConfig
	restore := stubServe(t, func(cfg serveConfig, _, _ io.Writer) error {
		got = cfg
		return nil
	})
	defer restore()

	runCLI("serve", "--host", "devbox.ss", "--self-signed", "--tls-port", "8443").wantCode(t, 0)
	if !got.SelfSigned || got.TLSPort != 8443 || got.Dev {
		t.Fatalf("serve config = %+v", got)
	}

	runCLI("serve", "--host", "devbox.ss", "--tls-cert", "c.pem", "--tls-key", "k.pem").wantCode(t, 0)
	if got.TLSCert != "c.pem" || got.TLSKey != "k.pem" || got.SelfSigned {
		t.Fatalf("serve config = %+v", got)
	}
}

// --host is what the origin allowlist and the certificate are both derived
// from, in dev as well as in production. A daemon that guessed its own public
// name would guess wrong exactly once.
func TestServeRequiresAHost(t *testing.T) {
	called := false
	restore := stubServe(t, func(serveConfig, io.Writer, io.Writer) error {
		called = true
		return nil
	})
	defer restore()

	r := runCLI("serve", "--dev")
	r.wantCode(t, 2)
	r.wantStderrContains(t, "--host")
	if called {
		t.Fatal("serve started without a host")
	}
}

// A daemon that could not start is a failed operation, not a usage error: exit
// 1, with the reason on stderr. The most likely reason by far is a device store
// that will not parse, which is deliberately fatal -- starting empty would sign
// out every enrolled device.
func TestServeReportsWhyTheDaemonCouldNotStart(t *testing.T) {
	restore := stubServe(t, func(serveConfig, io.Writer, io.Writer) error {
		return errors.New("the device store is corrupt")
	})
	defer restore()

	r := runCLI("serve", "--host", "tmux.example.com")
	r.wantCode(t, 1)
	r.wantStderrContains(t, "the device store is corrupt")
}

// The seam is wired to the real daemon rather than to a placeholder. Nothing
// here starts it -- that would bind ports and ask a CA for a certificate --
// so this checks the wiring by identity.
func TestServeRunsTheRealDaemon(t *testing.T) {
	if reflect.ValueOf(serveRun).Pointer() != reflect.ValueOf(runDaemon).Pointer() {
		t.Fatal("serve is not wired to the daemon")
	}
}

func stubServe(t *testing.T, fn func(serveConfig, io.Writer, io.Writer) error) func() {
	t.Helper()
	prev := serveRun
	serveRun = fn
	return func() { serveRun = prev }
}

// -- dispatch ---------------------------------------------------------------

func TestUnknownAndMissingCommands(t *testing.T) {
	for _, args := range [][]string{nil, {"revoked"}, {"--name", "laptop"}} {
		r := runCLI(args...)
		r.wantCode(t, 2)
		r.wantStderrContains(t, "Usage:")
		if r.stdout != "" {
			t.Errorf("%v wrote usage to stdout; a mistake is not an answer: %q", args, r.stdout)
		}
	}
}

func TestHelpIsAnAnswerNotAnError(t *testing.T) {
	for _, args := range [][]string{{"help"}, {"--help"}, {"-h"}} {
		r := runCLI(args...)
		r.wantCode(t, 0)
		if !strings.Contains(r.stdout, "Usage:") {
			t.Errorf("%v did not print usage on stdout\n%s", args, r)
		}
		for _, cmd := range []string{"serve", "enroll", "devices", "revoke"} {
			if !strings.Contains(r.stdout, cmd) {
				t.Errorf("usage does not list %q", cmd)
			}
		}
	}
}

func TestSubcommandHelpExitsZero(t *testing.T) {
	for _, args := range [][]string{{"enroll", "-h"}, {"devices", "-h"}, {"revoke", "-h"}, {"serve", "-h"}} {
		r := runCLI(args...)
		r.wantCode(t, 0)
		r.wantStderrContains(t, "Usage:", "socket")
	}
}

func TestUnknownFlagIsAUsageError(t *testing.T) {
	r := runCLI("devices", "--all")
	r.wantCode(t, 2)
}

// TestMain points $XDG_RUNTIME_DIR at a directory of this run's own, so a
// test that forgets --socket reaches nothing rather than the developer's live
// daemon -- where it could mint a credential or revoke their own browser. The
// safety of this suite must not rest on every test remembering a flag.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "wtroot")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Setenv("XDG_RUNTIME_DIR", dir)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// -- command line shape -----------------------------------------------------

// The stdlib flag package stops parsing at the first non-flag word, which made
// "revoke <id> --socket /path" read as three device ids. That is the order a
// person types -- the id is the subject, the flag an afterthought -- so the
// parser permutes instead of refusing.
func TestFlagsMayFollowAPositionalArgument(t *testing.T) {
	a := newAdmin(t)
	laptop := a.enroll(t, "laptop", "Firefox")

	r := runCLI("revoke", laptop, "--socket", a.socket)
	r.wantCode(t, 0)
	if names := a.deviceNames(t); len(names) != 0 {
		t.Fatalf("device survived a revoke with a trailing flag: %v", names)
	}
}

// "--" is the only way an operand that looks like a flag can get through, so
// permuting must not swallow it.
func TestDoubleDashEndsFlagParsing(t *testing.T) {
	a := newAdmin(t)
	a.enroll(t, "laptop", "Firefox")

	// Exit 1 (the daemon has no such device), not 2 (a flag we do not define).
	r := runCLI("revoke", "--socket", a.socket, "--", "-not-a-flag")
	r.wantCode(t, 1)
	r.wantStderrContains(t, "-not-a-flag")
	if names := a.deviceNames(t); len(names) != 1 {
		t.Fatalf("the store changed: %v", names)
	}
}

// The admin API never redirects, so a 3xx means a request landed somewhere
// unintended. Following it would turn a DELETE into a GET -- a revoke that
// reports success having read a list instead of removing a device.
func TestARedirectIsAFailureNotAHint(t *testing.T) {
	a := newAdminDir(t)
	a.serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			http.Redirect(w, r, "/devices", http.StatusMovedPermanently)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"devices": []deviceRow{}})
	}))

	r := runCLI("revoke", "someid", "--socket", a.socket)
	r.wantCode(t, 1)
}
