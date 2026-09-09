# tmux-web v1 Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Ship a single Go binary that serves a browser terminal attached to the local tmux server over TLS, with a sidebar that navigates sessions, windows, and panes.

**Architecture:** Each browser tab gets its own throwaway tmux session grouped onto a real one, created and attached in a single `tmux new-session` invocation running under a PTY. The PTY's bytes are the WebSocket payload, fed to `@wterm/react`. tmux is the only persistence layer — the server holds no terminal state, so reconnect is just a fresh attach. Auth is anchored on `SO_PEERCRED` over a unix socket: a CLI run as the service uid mints single-use enrollment links that a browser redeems for a durable device cookie.

**Tech Stack:** Go 1.26 (`creack/pty`, `coder/websocket`, `caddyserver/certmagic`, `golang.org/x/sys/unix`), Vite + React + TypeScript, Tailwind v4, shadcn/ui, `@wterm/react`. Frontend built to `web/dist` and embedded with `go:embed`.

**Design document:** `docs/plans/2026-09-09-tmux-web-design.md` (revision 3). Read it before starting. Every non-obvious decision here has its rationale there, and several were arrived at by testing tmux rather than reading the manual.

---

## Before you start

**Five things in this plan are counter-intuitive.** They were each verified against tmux 3.7b, and each replaced an earlier approach that looked correct and was not. Do not "simplify" them back:

1. **Session creation is one command, not four.** `tmux new-session -t <base> -s <name> \; set destroy-unattached on \; ...` with no `-d`. Setting `destroy-unattached on` on a detached session destroys it ~8ms later, before any attach lands. Doing it as a second call after spawning the attach only narrows the race.
2. **The snapshot is deduplicated in Go, not filtered in tmux.** A `-f` filter drops panes entirely once the user kills the base session while a tab is open.
3. **App sessions are marked with the `@wterm_web` tmux user option**, never matched by name.
4. **The snapshot does not carry `pane_current_path`.** It is the one field tmux does not sanitize, and putting it last does not contain the damage: a newline in it starts a fresh line whose every field is pane-controlled, forging a row, and a 0x1f in it swallows the next pane's record so a live pane vanishes from the sidebar. `#{q:}` escapes neither byte. Nothing in v1 renders the path; the git panel can query it per pane. Relatedly, sort panes by `#{pane_index}`, never by `pane_id` — after a split-and-kill cycle the ids run `%0 %4 %2 %1` while the layout the user sees runs `0 1 2 3`.
5. **The frontend must not use `WebSocketTransport`'s built-in reconnect.** It buffers sends while disconnected and flushes on reopen; since our reconnect creates a new tmux session, buffered keystrokes would land in the wrong pane.

**Testing philosophy.** The valuable tests here run against a real tmux server on an isolated socket. Do not mock tmux — the whole risk of this project lives in tmux's actual behavior. Pure parsing logic gets table tests; everything else gets an integration test.

**Commit after every task.** Conventional commit prefixes (`feat:`, `test:`, `fix:`, `chore:`).

---

## Phase A — the tmux layer

Nothing in this phase touches HTTP. At the end of it you can enumerate panes and drive tmux sessions correctly, proven against a real server.

### Task 1: Repository scaffolding

**Files:**
- Create: `go.mod`, `.gitignore`, `Makefile`, `cmd/wterm-web/main.go`

**Step 1: Initialize the module**

```bash
go mod init github.com/diegok/tmux-web
```

`go mod init` writes the toolchain's full patch version (e.g. `go 1.26.5`),
which makes the module refuse to build on go1.26.0. Edit `go.mod` down to the
language version:

```
go 1.26
```

**Step 2: Write `.gitignore`**

```
/wterm-web
/web/node_modules
/web/dist
/internal/front/dist
```

**Step 3: Write a placeholder `cmd/wterm-web/main.go`**

```go
package main

import "fmt"

func main() {
	fmt.Println("wterm-web")
}
```

**Step 4: Write `Makefile`**

```make
.PHONY: build test test-go test-web front

build: front
	go build -o wterm-web ./cmd/wterm-web

front:
	cd web && pnpm install && pnpm build

test: test-go

test-go:
	go test ./... -count=1

# No frontend yet. Fail loudly rather than succeed silently: a declared but
# empty test target reports success in CI while running nothing.
test-web:
	@echo "test-web: no frontend yet (see Phase D); wire up web/ tests here" >&2; exit 1
```

`test-web` must have a recipe that fails. A `.PHONY` target with no recipe
prints "Nothing to be done" and exits 0, so CI would report a passing frontend
test suite before the frontend exists. Phase D replaces the recipe with the
real command and adds `test-web` to the `test` target's prerequisites.

**Step 5: Verify it builds**

Run: `go build ./... && go vet ./...`
Expected: no output, exit 0.

**Step 6: Commit**

```bash
git add -A
git commit -m "chore: scaffold go module and makefile"
```

---

### Task 2: Isolated tmux test harness

Every later tmux test depends on this. It must never touch the user's real tmux
server, and it must not inherit the developer's tmux configuration.

Two hazards, both verified on this machine:

1. **A private socket is not a private configuration.** `tmux -L <name>` starts
   a fresh server but still reads `~/.tmux.conf`. The developer's config sets
   `prefix C-a`, `mouse on`, `aggressive-resize on` and `automatic-rename off`.
   Task 6 asserts `mouse = on` after `AttachArgs` sets it -- inherited, that
   assertion passes even if `AttachArgs` never touches mouse. Passing
   `-f /dev/null` restores stock defaults (`prefix C-b`, `mouse off`).

   Note which direction this cuts for window names. The developer's config sets
   `automatic-rename off`, so `-f /dev/null` turns it back **on** and makes
   `#{window_name}` *more* volatile in tests, not less: a window created without
   `-n` is named after its running command and follows it. Hermeticity is still
   right -- the suite must not vary with one machine's dotfiles -- but any test
   asserting a window name must create the window with `-n` to freeze it.
2. **Merged stderr corrupts parsed output.** Tasks 3-5 split tmux output on
   0x1f and index fields positionally, so a diagnostic line merged into stdout
   produces a malformed row and fails three files away from its cause.

**Files:**
- Create: `internal/tmux/testutil/server.go`
- Test: `internal/tmux/testutil/server_test.go`

**Step 1: Write the failing test**

`TryRun` returns an error instead of failing the test, so tests can assert that
a tmux command *fails* and so polling helpers (which have no usable `*testing.T`
inside a closure) can call it. `Run` is the fatal-on-error wrapper. Cleanup can
only be observed from a parent after the subtest that registered it returns,
hence the split into two tests.

```go
package testutil_test

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/diegok/tmux-web/internal/tmux/testutil"
)

func TestServerIsIsolated(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "probe")

	out := srv.Run(t, "list-sessions", "-F", "#{session_name}")
	if !strings.Contains(out, "probe") {
		t.Fatalf("expected probe session, got %q", out)
	}

	// The socket name must be unique per test, never the default server.
	if srv.Socket == "default" || srv.Socket == "" {
		t.Fatalf("refusing to run against socket %q", srv.Socket)
	}

	// The server must not inherit ~/.tmux.conf: a developer config that already
	// sets mouse/aggressive-resize/automatic-rename would make later assertions
	// about those options vacuous. C-b is the tmux default prefix.
	if got := srv.Run(t, "show", "-g", "-v", "prefix"); got != "C-b" {
		t.Fatalf("prefix = %q, want %q: server inherited a user config", got, "C-b")
	}

	// Sanity: the user's real server must not have gained a "probe" session.
	real, _ := exec.Command("tmux", "list-sessions", "-F", "#{session_name}").Output()
	if strings.Contains(string(real), "probe") {
		t.Fatal("test leaked a session into the user's real tmux server")
	}
}

func TestTryRunReportsFailureWithoutFataling(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "probe")

	out, err := srv.TryRun("has-session", "-t", "missing")
	if err == nil {
		t.Fatalf("expected an error for a missing session, got output %q", out)
	}
	// Diagnostics belong in the error; the returned value carries stdout only,
	// so callers that parse it never see a stray stderr line.
	if out != "" {
		t.Errorf("returned value = %q, want empty: stderr must not be merged into it", out)
	}
	if !strings.Contains(err.Error(), "can't find session") {
		t.Errorf("error should carry tmux's stderr, got %v", err)
	}
}

func TestTryRunReturnsStdoutOnSuccess(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "probe")

	out, err := srv.TryRun("list-sessions", "-F", "#{session_name}")
	if err != nil {
		t.Fatalf("list-sessions: %v", err)
	}
	if out != "probe" {
		t.Errorf("out = %q, want %q", out, "probe")
	}
}

// Cleanup runs when a test returns, so it can only be observed from a parent
// once the subtest that registered it has finished.
func TestServerCleanupKillsServerAndRemovesSocket(t *testing.T) {
	var socket, path string

	t.Run("inner", func(t *testing.T) {
		srv := testutil.NewServer(t)
		socket, path = srv.Socket, srv.SocketPath()
		srv.Run(t, "new-session", "-d", "-s", "probe")

		// Anchor the path: if SocketPath were wrong, the parent's "it is gone"
		// assertion below would pass without proving anything.
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("stat %s while the server is running: %v", path, err)
		}
	})

	if out, err := exec.Command("tmux", "-L", socket, "-f", "/dev/null",
		"has-session", "-t", "probe").CombinedOutput(); err == nil {
		t.Errorf("server on %s survived cleanup: %s", socket, out)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("stat %s after cleanup: got %v, want not-exist", path, err)
	}
}
```

**Step 2: Run it to verify it fails**

Run: `go test ./internal/tmux/testutil/ -run TestServer -v`
Expected: FAIL -- package `testutil` does not exist.

Then implement in stages and watch each assertion fail before fixing it: with a
plain `-L` socket the prefix assertion reports `prefix = "C-a"`; with
`CombinedOutput` the failure test reports the returned value carrying tmux's
error text; without the `os.Remove` the cleanup test reports the socket file
still present after the subtest returned.

**Step 3: Implement the harness**

```go
// Package testutil starts throwaway tmux servers for tests.
package testutil

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Server is a tmux server on a private socket, killed when the test ends.
type Server struct {
	Socket string
}

// NewServer reserves a private tmux socket for a test and registers its
// cleanup. It does not start the server: tmux does that lazily on the first
// command run against the socket.
func NewServer(t *testing.T) *Server {
	t.Helper()

	// Fail rather than skip: a suite that silently skips every tmux test
	// because tmux is missing looks green while testing nothing.
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Fatalf("tmux not found in PATH: %v", err)
	}

	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	s := &Server{Socket: "wterm-test-" + socketSafe(t.Name()) + "-" + hex.EncodeToString(b)}

	t.Cleanup(func() {
		_, _ = s.TryRun("kill-server")
		// tmux does not unlink the socket on shutdown, so without this a dead
		// socket file would accumulate per test.
		_ = os.Remove(s.SocketPath())
	})
	return s
}

// socketSafe reduces a test name to characters that are safe in a socket name,
// truncated to keep the socket path well inside the unix path length limit.
func socketSafe(name string) string {
	const max = 24
	var b strings.Builder
	for _, r := range name {
		if b.Len() >= max {
			break
		}
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

// SocketPath is the filesystem path of this server's socket. tmux places it in
// $TMUX_TMPDIR/tmux-<uid>, falling back to /tmp.
func (s *Server) SocketPath() string {
	dir := os.Getenv("TMUX_TMPDIR")
	if dir == "" {
		dir = "/tmp"
	}
	return filepath.Join(dir, fmt.Sprintf("tmux-%d", os.Getuid()), s.Socket)
}

// Args prefixes tmux arguments with this server's socket. It also passes
// -f /dev/null: a tmux server started on a private socket still reads
// ~/.tmux.conf, and inheriting the developer's options would make tests that
// assert on those same options pass without testing anything.
func (s *Server) Args(args ...string) []string {
	out := make([]string, 0, 4+len(args))
	out = append(out, "-L", s.Socket, "-f", "/dev/null")
	return append(out, args...)
}

// TryRun executes a tmux command against this server and returns its stdout
// and any error. Callers that parse the output need it free of diagnostics, so
// stderr is kept out of the returned value and reported through the error
// instead. It takes no *testing.T so it can be used from cleanup and polling
// helpers, and so tests can assert that a tmux command fails.
func (s *Server) TryRun(args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.Command("tmux", s.Args(args...)...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimRight(stderr.String(), "\n"); msg != "" {
			return "", fmt.Errorf("tmux %v: %w: %s", args, err, msg)
		}
		return "", fmt.Errorf("tmux %v: %w", args, err)
	}
	return strings.TrimRight(stdout.String(), "\n"), nil
}

// Run executes a tmux command against this server and returns its stdout,
// failing the test if the command does not succeed.
func (s *Server) Run(t *testing.T, args ...string) string {
	t.Helper()
	out, err := s.TryRun(args...)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return out
}
```

**Step 4: Run the test**

Run: `go test ./internal/tmux/testutil/ -v`
Expected: PASS (4 tests, one with an `inner` subtest).

**Step 5: Confirm no leaked servers or sockets**

A leaked server keeps its socket in `argv`, so it is greppable; a dead socket
file is a separate leak, since tmux does not unlink the socket on shutdown.

```bash
pgrep -af -- '[t]mux -L wterm-test' || echo "no leaked servers"
ls "${TMUX_TMPDIR:-/tmp}/tmux-$(id -u)/" 2>/dev/null | grep '^wterm-test-' || echo "no leaked sockets"
```

Expected: `no leaked servers` and `no leaked sockets`.

The bracket in `[t]mux` keeps the pattern from matching the shell that runs it;
do not `echo` the unbracketed pattern in the same command, or that echo becomes
a self-match. Verify the check is not vacuous by starting a server on a
throwaway `wterm-test-` socket first and confirming it is reported.

**Step 6: Commit**

```bash
git add internal/tmux/testutil
git commit -m "test: add isolated tmux server harness"
```

---

### Task 3: Snapshot record parsing

Pure function, no tmux. This is where the untrusted-field hazard is contained — by not carrying the untrusted field. Read "Before you start" item 4 first.

Two properties matter more than the parsing itself. **Lines are independent:** a malformed line is skipped, never merged into a neighbour, so one bad record cannot corrupt a good one. **Loss is counted, not silent:** `dropped` exists so a snapshot quietly shedding panes is observable instead of looking like an empty server.

**Files:**
- Create: `internal/tmux/snapshot.go`
- Test: `internal/tmux/snapshot_test.go`

**Step 1: Write the failing test**

```go
package tmux

import (
	"strings"
	"testing"
)

// rec builds one snapshot record from its fields, joined by the real separator.
// Tests use Sep rather than a private copy of the byte so that a change to the
// separator cannot leave the suite passing against a stale literal.
func rec(fields ...string) string { return strings.Join(fields, Sep) }

// A valid record, as a named baseline the malformed cases can be varied from.
// Field order matches Format: group, pane id, pane index, app marker, window
// index, window name, pane active, command.
func goodRow(paneID, paneIndex, windowIndex string) string {
	return rec("work", paneID, paneIndex, "", windowIndex, "api", "1", "claude")
}

func TestParseRows(t *testing.T) {
	t.Run("one well formed row", func(t *testing.T) {
		got, dropped, err := ParseRows(rec("work", "%3", "0", "", "1", "api", "1", "claude"))
		if err != nil {
			t.Fatal(err)
		}
		if dropped != 0 {
			t.Fatalf("dropped = %d, want 0", dropped)
		}
		if len(got) != 1 {
			t.Fatalf("want 1 row, got %d", len(got))
		}
		r := got[0]
		if r.GroupKey != "work" || r.PaneID != "%3" || r.PaneIndex != 0 || r.AppOwned {
			t.Fatalf("bad row: %+v", r)
		}
		if r.WindowIndex != 1 || r.WindowName != "api" || !r.PaneActive {
			t.Fatalf("bad row: %+v", r)
		}
		if r.Command != "claude" {
			t.Fatalf("bad row: %+v", r)
		}
	})

	// Pins the false side of both booleans: a parser hardcoding either to true
	// passes every other subtest.
	t.Run("app owned row with an inactive pane", func(t *testing.T) {
		got, dropped, err := ParseRows(rec("work", "%3", "2", "1", "0", "w", "0", "zsh"))
		if err != nil || dropped != 0 || len(got) != 1 {
			t.Fatalf("ParseRows = %+v, %d, %v", got, dropped, err)
		}
		if !got[0].AppOwned {
			t.Fatal("expected AppOwned")
		}
		if got[0].PaneActive {
			t.Fatal("expected PaneActive false")
		}
		if got[0].PaneIndex != 2 {
			t.Fatalf("PaneIndex = %d, want 2", got[0].PaneIndex)
		}
	})

	// Every other subtest parses a single record, which lets a parser that only
	// ever returns one row, or one that mixes fields between rows, survive.
	t.Run("several rows are returned in input order", func(t *testing.T) {
		out := strings.Join([]string{
			goodRow("%1", "0", "0"),
			goodRow("%4", "1", "0"),
			goodRow("%2", "2", "3"),
		}, "\n")
		got, dropped, err := ParseRows(out)
		if err != nil || dropped != 0 {
			t.Fatalf("dropped = %d, err = %v", dropped, err)
		}
		if len(got) != 3 {
			t.Fatalf("want 3 rows, got %d: %+v", len(got), got)
		}
		for i, want := range []struct {
			paneID             string
			paneIdx, windowIdx int
		}{
			{"%1", 0, 0},
			{"%4", 1, 0},
			{"%2", 2, 3},
		} {
			if got[i].PaneID != want.paneID || got[i].PaneIndex != want.paneIdx ||
				got[i].WindowIndex != want.windowIdx {
				t.Fatalf("row %d = %+v, want %+v", i, got[i], want)
			}
		}
	})

	// A record split across lines used to be rejoined into the previous row's
	// path. There is no path field any more, so a line that does not parse is
	// dropped -- it must never merge into, or corrupt, a neighbouring row.
	t.Run("malformed lines are dropped and counted", func(t *testing.T) {
		out := strings.Join([]string{
			goodRow("%1", "0", "0"),
			"nonsense", // too few fields
			// Numeric indices, so that only the field count can reject it.
			rec("work", "%7", "0", "", "0", "w", "1", "zsh", "extra"),
			rec("work", "%9", "0", "", "notanint", "w", "1", "zsh"), // bad window index
			rec("work", "%8", "notanint", "", "0", "w", "1", "zsh"), // bad pane index
			goodRow("%2", "1", "0"),
		}, "\n")
		got, dropped, err := ParseRows(out)
		if err != nil {
			t.Fatal(err)
		}
		if dropped != 4 {
			t.Fatalf("dropped = %d, want 4", dropped)
		}
		if len(got) != 2 {
			t.Fatalf("want the 2 good rows, got %d: %+v", len(got), got)
		}
		if got[0].PaneID != "%1" || got[1].PaneID != "%2" {
			t.Fatalf("wrong rows survived: %+v", got)
		}
	})

	// tmux output arrives newline-terminated. Client.Run trims it today, but
	// that contract lives in another file and a control-mode caller would not
	// go through it.
	t.Run("a trailing newline does not produce a phantom row", func(t *testing.T) {
		got, dropped, err := ParseRows(goodRow("%1", "0", "0") + "\n")
		if err != nil || dropped != 0 || len(got) != 1 {
			t.Fatalf("ParseRows = %+v, %d, %v; want 1 row, 0 dropped", got, dropped, err)
		}
	})

	t.Run("empty output yields no rows and drops nothing", func(t *testing.T) {
		got, dropped, err := ParseRows("")
		if err != nil || got != nil || dropped != 0 {
			t.Fatalf("ParseRows(\"\") = %v, %d, %v; want nil, 0, nil", got, dropped, err)
		}
	})

	// The error is always nil today. The return exists because Task 5 calls this
	// where an error is the natural shape; this pins the current contract so a
	// caller that ignores it is not silently wrong later.
	t.Run("error is always nil", func(t *testing.T) {
		for _, in := range []string{"", "nonsense", goodRow("%1", "0", "0"), "\n\n"} {
			if _, _, err := ParseRows(in); err != nil {
				t.Fatalf("ParseRows(%q) returned %v, want nil", in, err)
			}
		}
	})
}

// Format and fieldCount must agree, or every record is dropped at runtime while
// the parser's own tests keep passing.
func TestFormatFieldCount(t *testing.T) {
	if n := strings.Count(Format, Sep); n != fieldCount-1 {
		t.Fatalf("Format has %d separators (%d fields), want %d (%d fields)",
			n, n+1, fieldCount-1, fieldCount)
	}
	if strings.Contains(Format, "pane_current_path") {
		t.Fatal("pane_current_path must not be in the snapshot: an unsanitized " +
			"field lets one pane's output forge or erase another's record")
	}
}
```

**Step 2: Run it to verify it fails**

Run: `go test ./internal/tmux/ -run TestParseRows -v`
Expected: FAIL — `ParseRows` undefined.

**Step 3: Implement**

```go
package tmux

import (
	"strconv"
	"strings"
)

// Sep is the field separator used in tmux -F format strings. It is a literal
// 0x1f byte: tmux does not expand "\x1f" inside a format string, and a tab is
// unsafe because window names may contain one.
const Sep = "\x1f"

const fieldCount = 8

// Row is one pane as reported by tmux, before deduplication.
//
// The JSON names are the wire contract with the frontend; without the tags Go
// would marshal the exported Go names instead.
type Row struct {
	GroupKey    string `json:"groupKey"`  // session_group, falling back to session_name
	PaneID      string `json:"paneId"`    // e.g. "%3", stable for the pane's lifetime
	PaneIndex   int    `json:"paneIndex"` // position within the window, in layout order
	AppOwned    bool   `json:"appOwned"`  // set from the @wterm_web user option
	WindowIndex int    `json:"windowIndex"`
	WindowName  string `json:"windowName"`
	PaneActive  bool   `json:"paneActive"`
	Command     string `json:"command"`
}

// Format is the -F argument producing rows this package can parse.
//
// pane_current_path is deliberately absent. tmux sanitizes session and window
// names but not the path, so a pane sitting in a directory whose name contains
// a 0x1f or a newline can forge a whole extra record or swallow the following
// pane's -- either way the sidebar shows something other than the truth, and a
// pane that exists can vanish from it. Being the last field does not bound the
// damage: a newline simply starts a fresh line whose eight fields are all
// attacker-controlled. tmux's #{q:} modifier does not escape either byte.
// Nothing in v1 renders the path; the deferred git panel can query it per pane,
// where a single-pane result needs no field splitting to interpret.
//
// pane_current_command is a theoretical residual: it is not known to be
// sanitized either, and two attempts to make tmux report a command containing a
// newline failed, but that is not a proof that it cannot happen.
const Format = "#{?#{session_group},#{session_group},#{session_name}}" + Sep +
	"#{pane_id}" + Sep +
	"#{pane_index}" + Sep +
	"#{@wterm_web}" + Sep +
	"#{window_index}" + Sep +
	"#{window_name}" + Sep +
	"#{pane_active}" + Sep +
	"#{pane_current_command}"

// ParseRows parses raw `tmux list-panes` output into one Row per line.
//
// Any line that is not a well-formed record -- wrong field count, or a
// non-numeric index -- is skipped and counted in dropped. Lines are independent:
// a malformed one never merges into or alters a neighbouring row. dropped is
// returned rather than logged so the caller can surface a snapshot that is
// quietly losing panes instead of it passing unnoticed.
//
// The error is always nil today. It is part of the signature because Snapshot
// calls this in a context where an error is the natural shape.
func ParseRows(out string) (rows []Row, dropped int, err error) {
	out = strings.TrimSuffix(out, "\n")
	if out == "" {
		return nil, 0, nil
	}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Split(line, Sep)
		if len(fields) != fieldCount {
			dropped++
			continue
		}
		// Distinct names: shadowing the named err return here would be
		// harmless today only because it is always nil.
		pidx, perr := strconv.Atoi(fields[2])
		widx, werr := strconv.Atoi(fields[4])
		if perr != nil || werr != nil {
			dropped++
			continue
		}
		rows = append(rows, Row{
			GroupKey:    fields[0],
			PaneID:      fields[1],
			PaneIndex:   pidx,
			AppOwned:    fields[3] == "1",
			WindowIndex: widx,
			WindowName:  fields[5],
			PaneActive:  fields[6] == "1",
			Command:     fields[7],
		})
	}
	return rows, dropped, nil
}
```

**Step 4: Run the tests**

Run: `go test ./internal/tmux/ -v -count=1`
Expected: PASS — seven `TestParseRows` subtests plus `TestFormatFieldCount`.

Note the unfiltered run: `-run TestParseRows` would silently skip
`TestFormatFieldCount`, which is the one test that catches `Format` and
`fieldCount` disagreeing.

**Step 5: Commit**

```bash
git add internal/tmux/snapshot.go internal/tmux/snapshot_test.go
git commit -m "feat: parse tmux pane rows into a typed snapshot"
```

---

### Task 4: Deduplication by group

This is the fix for three separate bugs: duplicate rows, panes vanishing when
the base session dies, and a sidebar that reshuffles on every poll. Read the
design doc's "Sidebar state" section first.

**Files:**
- Modify: `internal/tmux/snapshot.go`
- Test: `internal/tmux/snapshot_test.go`

Unlike Task 3's, the code blocks below are *fragments appended to* files Task 3
created, not whole-file listings. That changes how to verify plan-against-code,
so use the right check for each file:

- `snapshot_test.go` -- Task 3's block stays a strict **prefix**; Task 4 only
  appends.
- `snapshot.go` -- Task 3's block is **not** a prefix, because Task 4 edits the
  import block in the middle of the file. It is byte-equal to the shipped file
  minus exactly two Task 4 edits: `sort` added to the imports, and `Dedupe`
  appended. Reconstruct rather than compare prefixes.
- Task 4's own three fragments (test, import, `Dedupe`) must each be
  byte-**present** in the shipped files.

**Step 1: Write the failing test**

```go
func TestDedupe(t *testing.T) {
	// The app-owned copies carry different display fields, so this asserts
	// which row's payload survived -- not merely how many rows did. Each pane
	// appears with the app copy on either side of the real one, so both
	// directions of the preference are exercised.
	t.Run("grouped sessions collapse to one row per pane", func(t *testing.T) {
		rows := []Row{
			{PaneID: "%0", GroupKey: "work", PaneIndex: 0, WindowName: "api", Command: "zsh", AppOwned: false},
			{PaneID: "%0", GroupKey: "work", PaneIndex: 0, WindowName: "stale", Command: "stale", AppOwned: true},
			{PaneID: "%1", GroupKey: "work", PaneIndex: 1, WindowName: "stale", Command: "stale", AppOwned: true},
			{PaneID: "%1", GroupKey: "work", PaneIndex: 1, WindowName: "api", Command: "vim", AppOwned: false},
		}
		got := Dedupe(rows)
		if len(got) != 2 {
			t.Fatalf("want 2 panes, got %d: %+v", len(got), got)
		}
		for _, r := range got {
			if r.AppOwned {
				t.Fatalf("should prefer the non-app row: %+v", r)
			}
			if r.WindowName != "api" || r.Command == "stale" {
				t.Fatalf("kept the app row's payload: %+v", r)
			}
		}
	})

	// The regression that motivated dedupe over a tmux filter. Kill the base
	// session while two tabs are open and the group survives with only
	// app-owned members -- so every pane still arrives duplicated, and every
	// copy is AppOwned. There is nothing to prefer, and a filter returns
	// nothing at all while the agents are still running.
	//
	// The fixture must contain duplicates. Two distinct panes with one row each
	// prove only that Dedupe does not filter, which no plausible implementation
	// gets wrong.
	t.Run("duplicated app owned rows collapse but survive", func(t *testing.T) {
		rows := []Row{
			{PaneID: "%0", GroupKey: "work", PaneIndex: 0, Command: "claude", AppOwned: true},
			{PaneID: "%1", GroupKey: "work", PaneIndex: 1, Command: "npm", AppOwned: true},
			{PaneID: "%0", GroupKey: "work", PaneIndex: 0, Command: "claude", AppOwned: true},
			{PaneID: "%1", GroupKey: "work", PaneIndex: 1, Command: "npm", AppOwned: true},
		}
		got := Dedupe(rows)
		if len(got) != 2 {
			t.Fatalf("app-owned panes must survive and collapse, got %d: %+v", len(got), got)
		}
		for _, r := range got {
			if !r.AppOwned {
				t.Fatalf("nothing here to prefer: %+v", r)
			}
		}
	})

	// Every field contradicts the others, so an implementation that sorts by
	// PaneID alone -- or that keeps PaneID as the tiebreak within a window --
	// produces a different answer from the correct one. A fixture whose pane
	// ids happen to already be in the intended order tests nothing.
	t.Run("output is ordered by group, then window, then pane index", func(t *testing.T) {
		rows := []Row{
			{PaneID: "%1", GroupKey: "b", WindowIndex: 0, PaneIndex: 0},
			{PaneID: "%9", GroupKey: "a", WindowIndex: 2, PaneIndex: 0},
			{PaneID: "%4", GroupKey: "a", WindowIndex: 0, PaneIndex: 1},
			{PaneID: "%7", GroupKey: "a", WindowIndex: 0, PaneIndex: 0},
		}
		got := Dedupe(rows)
		want := []string{"%7", "%4", "%9", "%1"}
		if len(got) != len(want) {
			t.Fatalf("want %d rows, got %+v", len(want), got)
		}
		for i, w := range want {
			if got[i].PaneID != w {
				t.Fatalf("order = %+v, want %v", got, want)
			}
		}
	})

	// sort.Slice is explicitly not a stable sort and map iteration order is
	// randomised, so determinism has to be proven rather than assumed. Without
	// a total order, one window with four panes produced four different
	// orderings across 200 runs -- a sidebar that reshuffles every 1.5s poll.
	//
	// The fixture is the verified real case: after a split-and-kill cycle the
	// pane ids run %0 %4 %2 %1 while pane_index runs 0 1 2 3, so this also pins
	// that layout order wins over id order.
	t.Run("repeated calls return the same order", func(t *testing.T) {
		rows := []Row{
			{PaneID: "%0", GroupKey: "w", WindowIndex: 0, PaneIndex: 0},
			{PaneID: "%4", GroupKey: "w", WindowIndex: 0, PaneIndex: 1},
			{PaneID: "%2", GroupKey: "w", WindowIndex: 0, PaneIndex: 2},
			{PaneID: "%1", GroupKey: "w", WindowIndex: 0, PaneIndex: 3},
		}
		want := []string{"%0", "%4", "%2", "%1"}
		for run := 0; run < 200; run++ {
			got := Dedupe(rows)
			for i, w := range want {
				if got[i].PaneID != w {
					t.Fatalf("run %d: order = %+v, want %v", run, got, want)
				}
			}
		}
	})

	// Snapshot returns nil when no server is running. If Dedupe returned an
	// empty slice here the API would emit [] in one case and null in the other,
	// and Task 21's sidebar would map over null.
	t.Run("an empty result is nil, not an empty slice", func(t *testing.T) {
		if got := Dedupe(nil); got != nil {
			t.Fatalf("Dedupe(nil) = %+v, want nil", got)
		}
		if got := Dedupe([]Row{}); got != nil {
			t.Fatalf("Dedupe([]Row{}) = %+v, want nil", got)
		}
	})

	// Task 7's Poller hands this slice to concurrent HTTP readers without
	// copying it, which is only safe if it never aliases the poll's input.
	t.Run("the result does not alias the input", func(t *testing.T) {
		rows := []Row{{PaneID: "%0", GroupKey: "w", Command: "zsh"}}
		got := Dedupe(rows)
		got[0].Command = "mutated"
		if rows[0].Command != "zsh" {
			t.Fatal("Dedupe returned rows aliasing its input")
		}
	})
}
```

**Step 2: Run it to verify it fails**

Run: `go test ./internal/tmux/ -run TestDedupe -v`
Expected: FAIL — `Dedupe` undefined.

**Step 3: Implement**

`snapshot.go` already imports `strconv` and `strings`; extend that block rather
than adding a second `import` declaration:

```go
import (
	"sort"
	"strconv"
	"strings"
)

// Dedupe collapses rows to one per pane and puts them in the order the user
// sees on screen.
//
// Grouped sessions share a window list, so `list-panes -a` reports every pane
// once per member of the group: with two browser tabs open on one base session,
// three copies of every pane. A tmux -f filter cannot do this job. Excluding
// app-owned sessions looks equivalent until the user kills the base session
// while a tab is open: the group survives with only app-owned members, so a
// filtered snapshot comes back empty while the agents are still running and
// still visible in the attached tab. Preferring a non-app row keeps the label
// honest; keeping an app-owned row when it is the only one keeps those agents
// in the sidebar.
//
// Panes are keyed by PaneID alone. Pane ids are unique per server, so GroupKey
// adds no discrimination -- only a failure mode, if session_group is ever empty
// for one member of a group and the same pane keys twice.
//
// The returned slice is freshly allocated and never aliases rows. Task 7's
// Poller hands it to concurrent HTTP readers without copying, which is only
// safe because of that. An empty result is nil rather than an empty slice, to
// match Snapshot's no-server path.
func Dedupe(rows []Row) []Row {
	best := make(map[string]Row, len(rows))
	for _, r := range rows {
		if cur, ok := best[r.PaneID]; !ok || (cur.AppOwned && !r.AppOwned) {
			best[r.PaneID] = r
		}
	}
	if len(best) == 0 {
		return nil
	}
	out := make([]Row, 0, len(best))
	for _, r := range best {
		out = append(out, r)
	}
	// Order by what the user sees. PaneIndex, not PaneID: after a split-and-kill
	// cycle the ids run %0 %4 %2 %1 while the layout runs 0 1 2 3, so sorting by
	// id -- lexicographically or numerically -- disagrees with the screen.
	//
	// sort.Slice is explicitly NOT stable and map iteration order is randomised,
	// so this comparator has to be a total order or the output varies run to
	// run. (GroupKey, WindowIndex, PaneIndex) is already one over a correctly
	// deduped set, since pane indices are unique within a window; PaneID is a
	// final tiebreak so that a snapshot violating that assumption degrades to a
	// wrong-but-stable order rather than a sidebar that reshuffles every poll.
	sort.Slice(out, func(i, j int) bool {
		if out[i].GroupKey != out[j].GroupKey {
			return out[i].GroupKey < out[j].GroupKey
		}
		if out[i].WindowIndex != out[j].WindowIndex {
			return out[i].WindowIndex < out[j].WindowIndex
		}
		if out[i].PaneIndex != out[j].PaneIndex {
			return out[i].PaneIndex < out[j].PaneIndex
		}
		return out[i].PaneID < out[j].PaneID
	})
	return out
}
```

**Step 4: Run the tests**

Run: `go test ./internal/tmux/ -v -count=1`
Expected: PASS.

**Step 5: Commit**

```bash
git add internal/tmux
git commit -m "feat: dedupe pane rows, preferring real sessions over app ones"
```

---

### Task 5: Client.Snapshot against a real tmux server

**Files:**
- Create: `internal/tmux/client.go`
- Test: `internal/tmux/client_integration_test.go`

**Step 1: Write the failing integration test**

```go
package tmux_test

import (
	"context"
	"testing"

	"github.com/diegok/tmux-web/internal/tmux"
	"github.com/diegok/tmux-web/internal/tmux/testutil"
)

func TestSnapshotAgainstRealTmux(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "work", "-x", "80", "-y", "24")
	srv.Run(t, "new-window", "-t", "work")

	// A user session whose name starts with the app's prefix. It must never be
	// hidden or swept: only the @wterm_web option marks an app session.
	srv.Run(t, "new-session", "-d", "-s", "_web-notes")

	// Simulate an open browser tab: a grouped, app-marked session.
	srv.Run(t, "new-session", "-d", "-t", "work", "-s", "_web-sim")
	srv.Run(t, "set", "-t", "_web-sim", "@wterm_web", "1")

	c := tmux.NewClient(srv.Args())
	panes, err := c.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	byPane := map[string]tmux.Row{}
	for _, p := range panes {
		if _, dup := byPane[p.PaneID]; dup {
			t.Fatalf("pane %s reported twice: %+v", p.PaneID, panes)
		}
		byPane[p.PaneID] = p
	}
	if len(byPane) != 3 {
		t.Fatalf("want 3 panes (work x2, _web-notes x1), got %d: %+v", len(byPane), panes)
	}

	var sawNotes bool
	for _, p := range panes {
		if p.GroupKey == "_web-notes" {
			sawNotes = true
		}
	}
	if !sawNotes {
		t.Fatal("a user session named _web-notes must not be hidden")
	}
}

// Regression: killing the base session must not blank the sidebar.
func TestSnapshotSurvivesBaseSessionKill(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "work", "-x", "80", "-y", "24")
	srv.Run(t, "new-window", "-t", "work")
	srv.Run(t, "new-session", "-d", "-t", "work", "-s", "_web-sim")
	srv.Run(t, "set", "-t", "_web-sim", "@wterm_web", "1")

	srv.Run(t, "kill-session", "-t", "work")

	c := tmux.NewClient(srv.Args())
	panes, err := c.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(panes) != 2 {
		t.Fatalf("agents must stay visible after the base session dies, got %+v", panes)
	}
	for _, p := range panes {
		if p.GroupKey != "work" {
			t.Fatalf("panes should still be labelled by group %q: %+v", "work", p)
		}
	}
}
```

**Step 2: Run it to verify it fails**

Run: `go test ./internal/tmux/ -run TestSnapshot -v`
Expected: FAIL — `NewClient` undefined.

**Step 3: Implement**

```go
package tmux

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
)

// Client runs tmux commands against one server.
type Client struct {
	// base is prepended to every invocation, e.g. []string{"-L", "sockname"}.
	// Empty means the user's default server.
	base []string
}

func NewClient(base []string) *Client { return &Client{base: base} }

func (c *Client) command(ctx context.Context, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, "tmux", append(append([]string{}, c.base...), args...)...)
}

// Run executes a tmux command and returns trimmed stdout.
//
// stdout and stderr are captured separately, exactly as in the test harness and
// for the same reason: Snapshot splits this return value on 0x1f and indexes
// fields positionally, so a single diagnostic line merged into it would produce
// a malformed row and fail far from its cause. stderr goes into the error,
// where it is useful, rather than into data that gets parsed.
func (c *Client) Run(ctx context.Context, args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := c.command(ctx, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimRight(stderr.String(), "\n"); msg != "" {
			return "", fmt.Errorf("tmux %s: %w: %s", strings.Join(args, " "), err, msg)
		}
		return "", fmt.Errorf("tmux %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimRight(stdout.String(), "\n"), nil
}

// Snapshot returns one row per pane, deduplicated across session groups.
func (c *Client) Snapshot(ctx context.Context) ([]Row, error) {
	out, err := c.Run(ctx, "list-panes", "-a", "-F", Format)
	if err != nil {
		// No server running is not an error condition for the UI. tmux reports
		// this on stderr, which Run folds into the error message.
		if noServer(err.Error()) {
			return nil, nil
		}
		return nil, err
	}
	rows, dropped, err := ParseRows(out)
	if err != nil {
		return nil, err
	}
	if dropped > 0 {
		// A pane silently missing from the sidebar is the worst failure this
		// project has, so an unparseable row is logged rather than swallowed.
		// It is not promoted to an error: the panes that did parse are still
		// worth showing.
		slog.Warn("tmux snapshot: skipped malformed rows", "dropped", dropped)
	}
	return Dedupe(rows), nil
}

// noServer reports whether a tmux failure means "there is no server on this
// socket" rather than a real fault.
//
// tmux 3.7b says this two different ways, depending on what is left of the
// socket. Once the server has exited but its socket file survives, connect
// fails with ECONNREFUSED and tmux prints "no server running on <path>". When
// the file was never created -- a machine where tmux has not been started since
// boot, and the state of every fresh test server -- connect fails with ENOENT
// and tmux prints "error connecting to <path> (No such file or directory)".
// Both mean the same thing to the sidebar: no panes.
//
// The ENOENT text is matched alongside the prefix rather than the prefix alone,
// because "error connecting to" also covers failures the operator must see --
// a permission error on a socket owned by another user, say, which as an empty
// sidebar would be indistinguishable from a machine with no tmux running.
//
// Matching on message text is fragile: tmux exits 1 for this and for genuine
// errors alike, so there is nothing else to match on. TestSnapshotWithNoServer*
// pin both strings against a real tmux.
func noServer(msg string) bool {
	return strings.Contains(msg, "no server running") ||
		(strings.Contains(msg, "error connecting to") &&
			strings.Contains(msg, "No such file or directory"))
}
```

**Step 4: Pin the no-server contract**

`Snapshot` treats a missing server as an empty UI, not an error, and it detects
that by substring-matching an error message. tmux writes that text to **stderr**
with exit 1, and `Run` folds non-empty stderr into the error -- so this behavior
spans two files and rests on a string match with nothing holding it in place.

Both strings need pinning, because tmux words the same condition two ways and
an earlier revision of this task matched only one of them. A fresh `NewServer`
has never created its socket, so tmux fails to connect with ENOENT and says
"error connecting to <path> (No such file or directory)":

```go
func TestSnapshotWithNoServerIsNotAnError(t *testing.T) {
	rows, err := tmux.NewClient(testutil.NewServer(t).Args()).Snapshot(context.Background())
	if err != nil || rows != nil {
		t.Fatalf("Snapshot on a dead server = %v, %v; want nil, nil", rows, err)
	}
}
```

The other half is the one a long-running deployment actually hits: the user
quits their last session, the server exits, and its socket file stays behind.
Connect then fails with ECONNREFUSED and tmux says "no server running on
<path>" instead. `kill-server` returns before the server has finished exiting,
and a command landing in that window fails with a third message, "server exited
unexpectedly", so poll until it settles rather than asserting on the transient:

```go
// The socket file outlives the server, and tmux then words the same condition
// differently -- "no server running on <path>" instead of the connect error a
// never-started socket gives. Without this the ECONNREFUSED half of noServer is
// unexercised, which is the half a long-running deployment actually hits: the
// user quits their last session while the sidebar is polling.
func TestSnapshotWithNoServerAfterExitIsNotAnError(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "work")
	srv.Run(t, "kill-server")
	if _, err := os.Stat(srv.SocketPath()); err != nil {
		t.Fatalf("socket file must outlive the server, else this duplicates the ENOENT test: %v", err)
	}

	// kill-server returns before the server has finished exiting, and a command
	// that lands in that window fails with "server exited unexpectedly". Poll
	// until it settles rather than asserting on the transient.
	c := tmux.NewClient(srv.Args())
	var err error
	var rows []tmux.Row
	deadline := time.Now().Add(5 * time.Second)
	for {
		rows, err = c.Snapshot(context.Background())
		if err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err != nil || rows != nil {
		t.Fatalf("Snapshot on an exited server = %v, %v; want nil, nil", rows, err)
	}
}
```

Both tests need `os` and `time` in the import block alongside `context` and
`testing`.

**Step 5: Run the tests**

Run: `go test ./internal/tmux/ -v`
Expected: PASS.

**Step 6: Commit**

```bash
git add internal/tmux
git commit -m "feat: snapshot panes from a real tmux server"
```

---

### Task 6: Session lifecycle — the one-shot attach command

The single most important detail in the project. Re-read "Attach model" in the design doc.

**Files:**
- Create: `internal/tmux/session.go`
- Test: `internal/tmux/session_test.go`, `internal/tmux/session_integration_test.go`

**Step 1: Write the failing unit test for the command builder**

`TestNewSessionNameIsUniqueAndPrefixed` is the other half of this file: without
it, `NewSessionName` returning a constant passes the suite, and two tabs opened
together would collide on one session name.

```go
package tmux

import (
	"strings"
	"testing"
)

func TestAttachArgs(t *testing.T) {
	args := AttachArgs("work", "_web-abc")
	joined := strings.Join(args, " ")

	if strings.Contains(joined, "-d") {
		t.Fatal("must not be detached: -d creates a session with zero clients, " +
			"and destroy-unattached then destroys it before any attach lands")
	}
	for _, want := range []string{
		"new-session", "-t", "work", "-s", "_web-abc",
		"destroy-unattached", "status", "mouse", "@wterm_web",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in %q", want, joined)
		}
	}
	// Every `set` must come after new-session so it runs in the attached
	// client's context.
	if strings.Index(joined, "new-session") > strings.Index(joined, "destroy-unattached") {
		t.Fatal("new-session must come first")
	}
}

// Two tabs opened at the same moment must not land on one session: a colliding
// name makes the second new-session fail outright.
func TestNewSessionNameIsUniqueAndPrefixed(t *testing.T) {
	seen := make(map[string]bool)
	for range 100 {
		name := NewSessionName()
		if !strings.HasPrefix(name, "_web-") {
			t.Fatalf("NewSessionName() = %q, want the _web- prefix", name)
		}
		if seen[name] {
			t.Fatalf("NewSessionName() repeated %q", name)
		}
		seen[name] = true
	}
}
```

**Step 2: Run it, expect FAIL**

Run: `go test ./internal/tmux/ -run TestAttachArgs -v`
Expected: FAIL — `AttachArgs` undefined.

**Step 3: Implement**

```go
package tmux

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
)

// AppOption is the tmux user option marking a session as app-created.
// Sessions are identified by this, never by name: a user may legitimately have
// a session called "_web-notes", and it must be neither hidden nor swept.
const AppOption = "@wterm_web"

// NewSessionName returns a unique name for a throwaway session.
func NewSessionName() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "_web-" + hex.EncodeToString(b)
}

// AttachArgs builds the single command that creates, attaches, and configures a
// throwaway session grouped onto base.
//
// This is deliberately one invocation. `destroy-unattached on` is not an
// on-detach hook: it is a "zero clients => destroy" invariant that fires the
// moment it becomes true. Setting it on a session created with -d destroys that
// session ~8ms later, before an attach can land. Setting it in a second call
// after spawning the attach only narrows the race, and there is no non-guessy
// way to observe the client coming up from outside. Without -d, new-session
// creates and attaches in one client, and the chained `set` commands run
// afterwards in that client's context.
func AttachArgs(base, name string) []string {
	return []string{
		"new-session", "-t", base, "-s", name,
		";", "set", "destroy-unattached", "on",
		";", "set", "status", "off",
		";", "set", "mouse", "on",
		";", "set", AppOption, "1",
	}
}

// Sweep kills app-created sessions with no attached clients. It runs at startup
// to collect sessions orphaned by a crash, since destroy-unattached cannot fire
// if the daemon died mid-attach.
//
// The listing and the kills are not atomic: a session reported with zero
// clients could in principle gain one before its kill lands, closing a live
// browser tab. That is tolerable only because the sole caller is startup, when
// no app session is attaching. A periodic sweep would need a different design.
//
// A failure that is not "no server" is returned rather than swallowed. Startup
// logs it and carries on -- an uncollectable orphan must not stop the daemon
// serving -- but it has to be visible, because the conditions that produce one
// (an unreadable socket, say) persist across every restart and leak a session
// each time.
func (c *Client) Sweep(ctx context.Context) error {
	out, err := c.Run(ctx, "list-sessions", "-F",
		"#{session_name}"+Sep+"#{"+AppOption+"}"+Sep+"#{session_attached}")
	if err != nil {
		if noServer(err.Error()) {
			return nil // nothing to sweep
		}
		return err
	}
	var errs []error
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(line, Sep)
		if len(f) != 3 {
			continue
		}
		name, app, attached := f[0], f[1], f[2]
		if app != "1" || attached != "0" {
			continue
		}
		// "=" makes the target an exact match. Sweep always passes a name it
		// just read, so a bare name is correct today -- but tmux target
		// matching falls back to a prefix, and `kill-session -t _web-` will
		// silently kill _web-abcd and exit 0. Pinning exactness here means a
		// future caller passing a partial name gets an error instead of
		// destroying a user's session, which is the failure @wterm_web exists
		// to prevent. No test pins this: Sweep never passes a partial name, and
		// tmux prefers an exact match over a longer prefix, so "=" and a bare
		// name behave identically for every input reachable today.
		if _, err := c.Run(ctx, "kill-session", "-t", "="+name); err != nil {
			// The session going away between the listing and the kill is the
			// expected race, not a fault. Anything else is worth surfacing.
			// Matched on message text for the same reason as noServer: tmux
			// exits 1 for every failure alike.
			if !strings.Contains(err.Error(), "can't find session") {
				errs = append(errs, err)
			}
		}
	}
	// Nil when errs is empty, so the common path returns no error.
	return errors.Join(errs...)
}
```

**Step 4: Run the unit test**

Run: `go test ./internal/tmux/ -run TestAttachArgs -v`
Expected: PASS.

**Step 5: Write the failing integration test**

Note the shell quoting: `exec.Command` passes `;` as a literal argument, which is exactly what tmux wants — no shell involved.

`TestSweepSparesAttachedAppSessions` exists because mutation testing found the
gap: with only `TestSweepSparesUserSessions`, dropping the `session_attached`
check from `Sweep` passed the suite, and a daemon restart would then kill every
live tab's session. It is the counterpart to the name-prefix case — `Sweep` has
two independent conditions and each needs its own test.

`TestSweepPropagatesRealErrors` needs a tmux failure that is *not* "no server",
which the harness by construction cannot produce — its sockets work. It is the
one tmux invocation in the package that does not go through `testutil.Server`:
a read-only `list-sessions` against an explicit `-S` path under `t.TempDir()`
that exceeds the unix `sun_path` limit. A socket in an unreadable directory is
the more realistic case but does not fail for root, so it would pass vacuously
in a container.

```go
package tmux_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/diegok/tmux-web/internal/tmux"
	"github.com/diegok/tmux-web/internal/tmux/testutil"
)

func TestAttachLifecycle(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "work", "-x", "80", "-y", "24")

	name, f, cmd := startAttached(t, srv, "work")

	// These assertions are only meaningful because the harness starts tmux
	// with -f /dev/null: the developer's ~/.tmux.conf already sets mouse on
	// globally, which would satisfy the mouse check on its own.
	for _, tc := range []struct{ opt, want string }{
		{"destroy-unattached", "on"},
		{"status", "off"},
		{"mouse", "on"},
		{tmux.AppOption, "1"},
	} {
		got := srv.Run(t, "show", "-t", name, "-v", tc.opt)
		if got != tc.want {
			t.Errorf("%s = %q, want %q", tc.opt, got, tc.want)
		}
	}

	// Closing the PTY detaches the client; destroy-unattached must collect it.
	_ = f.Close()
	_ = cmd.Process.Kill()
	waitFor(t, 3*time.Second, func() bool {
		// The unattached "work" session keeps the server alive, so an error
		// here is never transient: it means the harness broke. Polling through
		// it and then reporting "not reaped" would name the wrong cause.
		// t.Fatalf is safe in this closure because waitFor calls cond() on the
		// test goroutine.
		out, err := srv.TryRun("list-sessions", "-F", "#{session_name}")
		if err != nil {
			t.Fatalf("list-sessions while waiting for reap: %v", err)
		}
		return !strings.Contains(out, name)
	}, "session was not reaped on detach")
}

// Sweep runs at startup, but the daemon can be restarted while a browser tab is
// attached. Collecting orphans must not kill a live session out from under it.
func TestSweepSparesAttachedAppSessions(t *testing.T) {
	srv := testutil.NewServer(t)
	// "work" is not app-owned, so the sweep spares it and it keeps the server
	// alive -- which makes the list-sessions below a real assertion rather than
	// an error whose message would name the wrong cause.
	srv.Run(t, "new-session", "-d", "-s", "work", "-x", "80", "-y", "24")
	live, _, _ := startAttached(t, srv, "work")

	srv.Run(t, "new-session", "-d", "-t", "work", "-s", "_web-orphan")
	srv.Run(t, "set", "-t", "_web-orphan", tmux.AppOption, "1")

	if err := tmux.NewClient(srv.Args()).Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}

	out := srv.Run(t, "list-sessions", "-F", "#{session_name}")
	if !strings.Contains(out, live) {
		t.Fatalf("sweep killed an attached app session, closing a live tab: %q", out)
	}
	if strings.Contains(out, "_web-orphan") {
		t.Fatalf("sweep failed to collect an app-owned orphan: %q", out)
	}
}

func TestSweepSparesUserSessions(t *testing.T) {
	srv := testutil.NewServer(t)
	// An unrelated session, purely to keep the server alive if the sweep kills
	// everything it should have spared. Without it the last session's death
	// takes the server with it, and the assertions below fail with "no server
	// running" -- naming the harness instead of the bug.
	srv.Run(t, "new-session", "-d", "-s", "keepalive")
	srv.Run(t, "new-session", "-d", "-s", "_web-notes") // user's own, unmarked
	srv.Run(t, "new-session", "-d", "-s", "_web-orphan")
	srv.Run(t, "set", "-t", "_web-orphan", tmux.AppOption, "1")

	if err := tmux.NewClient(srv.Args()).Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}

	out := srv.Run(t, "list-sessions", "-F", "#{session_name}")
	if !strings.Contains(out, "_web-notes") {
		t.Fatal("sweep killed a user session that merely shares the name prefix")
	}
	if strings.Contains(out, "_web-orphan") {
		t.Fatal("sweep failed to collect an app-owned orphan")
	}
}

// startAttached runs the real one-shot attach command under a PTY and returns
// once the session reports a client. Teardown is registered with t.Cleanup
// rather than deferred in the caller, so that a t.Fatal anywhere in the test
// still collects the client: a surviving `tmux attach` holds the session open
// and defeats destroy-unattached.
func TestSweepWithNoServerIsNotAnError(t *testing.T) {
	// NewServer reserves a socket without starting tmux, so this is the
	// never-started case: nothing to sweep is not a failure.
	if err := tmux.NewClient(testutil.NewServer(t).Args()).Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep with no server = %v, want nil", err)
	}
}

// A tmux failure that is not "no server" must reach the caller. Startup logs it
// and carries on, but silently reporting success would hide, for instance, a
// socket the daemon cannot read -- where every orphan survives and every
// restart leaks another.
//
// The socket path is deliberately over the unix sun_path limit: it fails
// identically for any uid, whereas the realistic case -- a socket whose
// directory denies access, which tmux reports as "(Permission denied)" -- does
// not fail at all for root. This is the one tmux invocation in the package that
// does not go through testutil.Server, because the harness by construction
// hands out sockets that work. It is read-only and names an explicit -S path
// under t.TempDir(), so it cannot reach any real server.
func TestSweepPropagatesRealErrors(t *testing.T) {
	socket := filepath.Join(t.TempDir(), strings.Repeat("x", 200))
	err := tmux.NewClient([]string{"-S", socket, "-f", "/dev/null"}).Sweep(context.Background())
	if err == nil {
		t.Fatal("Sweep on an unusable socket = nil, want an error")
	}
	if !strings.Contains(err.Error(), "File name too long") {
		t.Fatalf("Sweep error = %v, want the underlying tmux failure", err)
	}
}

func startAttached(t *testing.T, srv *testutil.Server, base string) (string, *os.File, *exec.Cmd) {
	t.Helper()
	name := tmux.NewSessionName()
	cmd := exec.Command("tmux", append(srv.Args(), tmux.AttachArgs(base, name)...)...)
	f, err := pty.Start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = f.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	waitFor(t, 3*time.Second, func() bool {
		// Check the error: a failed command yields empty output, and a
		// Contains check on empty output would make this wait vacuous.
		out, err := srv.TryRun("list-sessions", "-F", "#{session_name} #{session_attached}")
		return err == nil && strings.Contains(out, name+" 1")
	}, "session never came up attached")
	return name, f, cmd
}

func waitFor(t *testing.T, d time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal(msg)
}
```

**Step 6: Add the pty dependency and run**

```bash
go get github.com/creack/pty
go test ./internal/tmux/ -v
```
Expected: PASS. If `TestAttachLifecycle` fails at "session never came up attached", you have reintroduced `-d` or split the command — re-read Step 3's comment.

**Step 7: Commit**

```bash
git add internal/tmux go.mod go.sum
git commit -m "feat: one-shot tmux session create-attach-configure"
```

---

### Task 7: Navigation and cached snapshot polling

**Files:**
- Modify: `internal/tmux/client.go`, `internal/tmux/client_integration_test.go`
- Create: `internal/tmux/poller.go`
- Test: `internal/tmux/poller_test.go`, `internal/tmux/poller_integration_test.go`

**Step 1: Write the failing tests**

The poller's own tests are pure -- `NewPollerFunc` takes a function, so they need
no tmux -- but `NewPoller` then has nothing pinning its wiring, and a poller that
never reaches tmux would serve an empty sidebar forever. That one test
(`TestNewPollerCachesRealSnapshots`) runs against a real server and also proves
the cache advances on its own: it opens a window after `Start` and waits for the
pane to appear.

```go
func TestPollerSharesOnePollAcrossReaders(t *testing.T) {
	var calls int32
	p := tmux.NewPollerFunc(50*time.Millisecond, func(context.Context) ([]tmux.Row, error) {
		atomic.AddInt32(&calls, 1)
		return []tmux.Row{{PaneID: "%0"}}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx)

	// Ten concurrent readers inside one interval must not cause ten polls.
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = p.Latest() }()
	}
	wg.Wait()

	if n := atomic.LoadInt32(&calls); n > 2 {
		t.Fatalf("want at most 2 polls, got %d -- readers are each forking tmux", n)
	}
}
```

Four more pure tests, each of which a mutant survives without:
`TestPollerPollsOnceBeforeStartReturns` (the first tab must not wait an interval
for its sidebar), `TestPollerRefreshesOnTheTicker`,
`TestPollerStopsOnContextCancel`, and the one that pins the decision below:

```go
// A transient tmux failure -- "server exited unexpectedly" is emitted for a few
// milliseconds while a server shuts down -- must not blank a sidebar that was
// correct an interval ago. The error is still visible so a persistent failure
// can be surfaced rather than served as a snapshot that is stale forever.
func TestPollerKeepsLastGoodSnapshotOnError(t *testing.T) {
	boom := errors.New("server exited unexpectedly")

	var mu sync.Mutex
	mode := "ok"
	p := tmux.NewPollerFunc(time.Millisecond, func(context.Context) ([]tmux.Row, error) {
		mu.Lock()
		defer mu.Unlock()
		switch mode {
		case "ok":
			return []tmux.Row{{PaneID: "%7"}}, nil
		case "fail":
			return nil, boom
		default:
			return []tmux.Row{{PaneID: "%9"}}, nil
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx)

	if rows := p.Latest(); len(rows) != 1 || rows[0].PaneID != "%7" {
		t.Fatalf("Latest() = %+v before any failure, want the first good snapshot", rows)
	}

	mu.Lock()
	mode = "fail"
	mu.Unlock()
	waitFor(t, 3*time.Second, func() bool { return p.Err() != nil }, "a failed poll never surfaced an error")

	if rows := p.Latest(); len(rows) != 1 || rows[0].PaneID != "%7" {
		t.Fatalf("Latest() = %+v after a failed poll, want the last good snapshot kept", rows)
	}
	if err := p.Err(); !errors.Is(err, boom) {
		t.Fatalf("Err() = %v, want the failure that the poll returned", err)
	}

	// Recovery: a later success must both replace the rows and clear the error,
	// or the UI keeps an error banner up forever.
	mu.Lock()
	mode = "recovered"
	mu.Unlock()
	waitFor(t, 3*time.Second, func() bool {
		rows := p.Latest()
		return p.Err() == nil && len(rows) == 1 && rows[0].PaneID == "%9"
	}, "the poller never recovered after a transient failure")
}
```

**Step 2: Run it, expect FAIL**

Run: `go test ./internal/tmux/ -run TestPoller -v`

**Step 3: Implement `poller.go`**

```go
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
```

**Decided: a failed poll keeps the last good snapshot.**

`noServer` (Task 5) matches two of tmux's three failure texts. The third,
`server exited unexpectedly`, appears for a few milliseconds while a server is
shutting down. Task 5 leaves it a hard error, which is correct for a one-shot
command but wrong for a poller: when the user closes their last tmux session,
one poll can land in that window and raise an error banner that the next poll
silently clears.

The fix is in the poller, not in `noServer`: `refresh` updates `err` on every
poll but only replaces `latest` on a successful one. A transient tmux fault
never blanks a sidebar that was correct 1.5s ago, and it costs no further
guessing at tmux's error strings -- which is the fragile part of `noServer` and
the part Task 5 got wrong the first time. `Err()` exists so a persistent failure
is still visible: serving a stale snapshot forever without saying so is its own
bug, and Task 17's `/api/snapshot` handler is the caller that will surface it.

**Step 4: Add navigation helpers to `client.go`**

```go
// SelectPane points one session at a pane: the browser tab's own grouped
// session, so clicking a pane in one tab moves neither the other tabs nor the
// terminal the user is sitting in front of.
//
// The plan specified `select-window -t <session>:<paneID>`, and tmux 3.7b
// rejects it -- "can't find window: %3". A window target parses <session>:<win>
// and looks <win> up as a name, index or @id; a pane id is not one of those.
// The bare form `select-window -t %3` does work, but it picks the session
// itself, and with grouped sessions that choice is arbitrary: with two tabs
// open on one base, it moved the wrong one. Hence the extra call to resolve the
// pane's window id, which can then be qualified with the session that must
// move. "=" pins the session name to an exact match, as in Sweep.
//
// list-panes does the resolving rather than display-message, which is the
// obvious command and is unusable here: given a target it cannot find, it
// prints an empty expansion and exits 0, and `display-message -t work:9`
// happily answers about a different window. list-panes errors on all of those.
// It reports the window id once per pane in the window; the lines are identical
// and the first is taken.
//
// Only the current window is per-session. The active pane belongs to the
// window, which grouped sessions share, so that half is visible to every member
// of the group. tmux offers no way to scope it, and it matches what the user
// sees when they select a pane in one of two attached clients.
func (c *Client) SelectPane(ctx context.Context, session, paneID string) error {
	// tmux resolves an empty target to "whatever is current" and exits 0, so an
	// unset pane id would quietly navigate the tab somewhere arbitrary instead
	// of failing. The ids come from the frontend, where "no selection yet" is
	// one bug away from being the empty string.
	if !isPaneID(paneID) {
		return fmt.Errorf("select pane: %q is not a tmux pane id", paneID)
	}
	if session == "" {
		return fmt.Errorf("select pane %s: no session given", paneID)
	}
	out, err := c.Run(ctx, "list-panes", "-t", paneID, "-F", "#{window_id}")
	if err != nil {
		return err
	}
	window, _, _ := strings.Cut(out, "\n")
	if _, err := c.Run(ctx, "select-window", "-t", "="+session+":"+window); err != nil {
		return err
	}
	_, err = c.Run(ctx, "select-pane", "-t", paneID)
	return err
}

// isPaneID reports whether s is a tmux pane id, e.g. "%3".
func isPaneID(s string) bool {
	if len(s) < 2 || s[0] != '%' {
		return false
	}
	for _, r := range s[1:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// KillSession removes a throwaway session explicitly. destroy-unattached is the
// crash net; this is the normal teardown path.
//
// "=" pins the target to an exact match, for the reason spelled out in Sweep:
// tmux falls back to prefix matching without reporting the ambiguity, so
// `kill-session -t _web-` would kill an unrelated _web-abcd and exit 0. Unlike
// Sweep, this takes a name from its caller rather than from tmux, so the guard
// is reachable and tested.
func (c *Client) KillSession(ctx context.Context, name string) error {
	_, err := c.Run(ctx, "kill-session", "-t", "="+name)
	return err
}
```

The `select-window -t <session>:<paneID>` form this plan carried until now does
not work: tmux 3.7b answers `can't find window: %3`. It was checked against a
real server before shipping, which is the only reason it did not reach the UI.
`TestSelectPaneMovesOnlyTheGivenSession` pins the corrected sequence with two
grouped sessions plus the base one, and fails on the old code.

`KillSession` gained the same `=` exact-match prefix as `Sweep`, and here it is
tested rather than merely argued: `Sweep` only ever passes a name it read back
from tmux, but `KillSession` takes one from its caller, so a partial name is a
reachable input and `kill-session -t _web-` would silently kill `_web-abcd`.

**Step 5: Run all tmux tests**

Run: `go test ./internal/tmux/... -v -count=1 -race`
Expected: PASS. `-race` because `Latest` hands one slice to concurrent readers.

**Step 6: Commit**

```bash
git add internal/tmux
git commit -m "feat: shared snapshot poller and pane navigation"
```

---

## Phase B — the PTY bridge

### Task 8: Frame codec

One socket carries both PTY bytes and control JSON behind a one-byte prefix. `WebSocketTransport` imposes no protocol, so we are free to define this — but see Task 17: we replace that class on the client side.

**Files:**
- Create: `internal/ptybridge/frame.go`
- Test: `internal/ptybridge/frame_test.go`

**Step 1: Write the failing test**

```go
package ptybridge

import (
	"bytes"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	t.Run("data frame", func(t *testing.T) {
		enc := EncodeData([]byte("hello"))
		kind, payload, err := Decode(enc)
		if err != nil || kind != FrameData || !bytes.Equal(payload, []byte("hello")) {
			t.Fatalf("kind=%v payload=%q err=%v", kind, payload, err)
		}
	})

	t.Run("control frame", func(t *testing.T) {
		enc := EncodeControl([]byte(`{"type":"resize"}`))
		kind, payload, err := Decode(enc)
		if err != nil || kind != FrameControl {
			t.Fatalf("kind=%v err=%v", kind, err)
		}
		if string(payload) != `{"type":"resize"}` {
			t.Fatalf("payload=%q", payload)
		}
	})

	t.Run("empty frame is an error, not a panic", func(t *testing.T) {
		if _, _, err := Decode(nil); err == nil {
			t.Fatal("want error for empty frame")
		}
	})

	t.Run("unknown kind is rejected", func(t *testing.T) {
		if _, _, err := Decode([]byte{0xEE, 'x'}); err == nil {
			t.Fatal("want error for unknown frame kind")
		}
	})
}
```

**Step 2: Run it, expect FAIL**

Run: `go test ./internal/ptybridge/ -v`

**Step 3: Implement**

```go
// Package ptybridge pumps bytes between a tmux attach PTY and a WebSocket.
package ptybridge

import "errors"

// Frame kinds. One socket carries both streams so that auth, reconnect, and
// keepalive exist in one place rather than two.
const (
	FrameData    byte = 0x00 // raw PTY bytes, both directions
	FrameControl byte = 0x01 // JSON control message
)

var errBadFrame = errors.New("ptybridge: malformed frame")

func EncodeData(b []byte) []byte    { return append([]byte{FrameData}, b...) }
func EncodeControl(b []byte) []byte { return append([]byte{FrameControl}, b...) }

func Decode(frame []byte) (kind byte, payload []byte, err error) {
	if len(frame) == 0 {
		return 0, nil, errBadFrame
	}
	switch frame[0] {
	case FrameData, FrameControl:
		return frame[0], frame[1:], nil
	default:
		return 0, nil, errBadFrame
	}
}
```

**Step 4: Run, expect PASS. Commit**

```bash
go test ./internal/ptybridge/ -v
git add internal/ptybridge
git commit -m "feat: websocket frame codec for data and control"
```

---

### Task 9: The session bridge

**Files:**
- Create: `internal/ptybridge/session.go`
- Test: `internal/ptybridge/session_integration_test.go`

**Step 1: Write the failing integration test**

```go
package ptybridge_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/diegok/tmux-web/internal/ptybridge"
	"github.com/diegok/tmux-web/internal/tmux"
	"github.com/diegok/tmux-web/internal/tmux/testutil"
)

func TestSessionEchoesTypedInput(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "work", "-x", "80", "-y", "24")

	s, err := ptybridge.Open(context.Background(), ptybridge.Config{
		TmuxArgs: srv.Args(),
		Base:     "work",
		Cols:     80,
		Rows:     24,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// Give tmux time to paint, then type.
	time.Sleep(500 * time.Millisecond)
	if _, err := s.Write([]byte("echo hello-bridge\r")); err != nil {
		t.Fatal(err)
	}

	var buf strings.Builder
	deadline := time.After(5 * time.Second)
	for {
		select {
		case b, ok := <-s.Output():
			if !ok {
				t.Fatal("output closed early")
			}
			buf.Write(b)
			if strings.Contains(buf.String(), "hello-bridge") {
				return
			}
		case <-deadline:
			t.Fatalf("never saw echoed output; got %q", buf.String())
		}
	}
}

func TestSessionKillsItsTmuxSessionOnClose(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "work", "-x", "80", "-y", "24")

	s, err := ptybridge.Open(context.Background(), ptybridge.Config{
		TmuxArgs: srv.Args(), Base: "work", Cols: 80, Rows: 24,
	})
	if err != nil {
		t.Fatal(err)
	}
	name := s.SessionName()
	time.Sleep(500 * time.Millisecond)
	s.Close()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		out := srv.Run(t, "list-sessions", "-F", "#{session_name}")
		if !strings.Contains(out, name) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("throwaway session outlived the bridge")
}
```

**Step 2: Run it, expect FAIL**

**Step 3: Implement `session.go`**

```go
package ptybridge

import (
	"context"
	"os"
	"os/exec"
	"sync"

	"github.com/creack/pty"
	"github.com/diegok/tmux-web/internal/tmux"
)

// outputBuffer bounds how much PTY output may queue for a slow client.
const outputBuffer = 256

type Config struct {
	TmuxArgs []string // server selection, e.g. {"-L", "sock"}; nil for default
	Base     string   // the real session to group onto
	Cols     uint16
	Rows     uint16
}

// Session is one browser tab's tmux client.
type Session struct {
	name string
	cmd  *exec.Cmd
	pty  *os.File
	out  chan []byte
	tm   *tmux.Client

	closeOnce sync.Once
}

func Open(ctx context.Context, cfg Config) (*Session, error) {
	name := tmux.NewSessionName()
	args := append(append([]string{}, cfg.TmuxArgs...), tmux.AttachArgs(cfg.Base, name)...)

	cmd := exec.Command("tmux", args...)
	// TERM must match what wterm emulates. tmux advertises tmux-256color to
	// programs inside it, which is expected and separate from this.
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")

	f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: cfg.Cols, Rows: cfg.Rows})
	if err != nil {
		return nil, err
	}

	s := &Session{
		name: name,
		cmd:  cmd,
		pty:  f,
		out:  make(chan []byte, outputBuffer),
		tm:   tmux.NewClient(cfg.TmuxArgs),
	}
	go s.pump()
	return s, nil
}

func (s *Session) SessionName() string { return s.name }
func (s *Session) Output() <-chan []byte { return s.out }

// pump reads the PTY into the output channel.
//
// If the channel is full the session is closed rather than blocking. Blocking
// here would stall a tmux client that shares the server with the user's local
// session, so a wedged browser tab could freeze the real terminal. Dropping
// bytes is equally unacceptable: a truncated escape sequence corrupts the
// terminal permanently. Closing is safe because tmux redraws on reattach.
func (s *Session) pump() {
	defer close(s.out)
	buf := make([]byte, 32*1024)
	for {
		n, err := s.pty.Read(buf)
		if n > 0 {
			b := make([]byte, n)
			copy(b, buf[:n])
			select {
			case s.out <- b:
			default:
				s.Close()
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (s *Session) Write(b []byte) (int, error) { return s.pty.Write(b) }

func (s *Session) Resize(cols, rows uint16) error {
	return pty.Setsize(s.pty, &pty.Winsize{Cols: cols, Rows: rows})
}

// SelectPane navigates this tab's own session only.
func (s *Session) SelectPane(ctx context.Context, paneID string) error {
	return s.tm.SelectPane(ctx, s.name, paneID)
}

func (s *Session) Close() {
	s.closeOnce.Do(func() {
		_ = s.pty.Close()
		if s.cmd.Process != nil {
			_ = s.cmd.Process.Kill()
		}
		_ = s.tm.KillSession(context.Background(), s.name)
	})
}
```

**Step 4: Run the tests**

Run: `go test ./internal/ptybridge/ -v -count=1`
Expected: PASS.

**Step 5: Commit**

```bash
git add internal/ptybridge
git commit -m "feat: pty bridge for a browser tab's tmux client"
```

---

### Task 10: WebSocket handler with keepalive

**Files:**
- Create: `internal/front/ws.go`
- Test: `internal/front/ws_test.go`

**Step 1: Add the dependency**

```bash
go get github.com/coder/websocket
```

**Step 2: Write the failing test**

```go
package front_test

// Connects a real websocket to the handler, types a command, and asserts the
// echoed output arrives as a data frame.
func TestWebSocketRoundTrip(t *testing.T) {
	srv := testutil.NewServer(t)
	srv.Run(t, "new-session", "-d", "-s", "work", "-x", "80", "-y", "24")

	h := front.NewTerminalHandler(front.TerminalConfig{
		TmuxArgs:      srv.Args(),
		AllowedOrigin: "http://example.test",
	})
	ts := httptest.NewServer(h)
	defer ts.Close()

	c, _, err := websocket.Dial(context.Background(),
		"ws"+strings.TrimPrefix(ts.URL, "http")+"?session=work",
		&websocket.DialOptions{HTTPHeader: http.Header{"Origin": {"http://example.test"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(websocket.StatusNormalClosure, "")

	time.Sleep(500 * time.Millisecond)
	if err := c.Write(context.Background(), websocket.MessageBinary,
		ptybridge.EncodeData([]byte("echo ws-ok\r"))); err != nil {
		t.Fatal(err)
	}

	var got strings.Builder
	for i := 0; i < 200; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, msg, err := c.Read(ctx)
		cancel()
		if err != nil {
			t.Fatalf("read: %v (got %q)", err, got.String())
		}
		kind, payload, err := ptybridge.Decode(msg)
		if err != nil || kind != ptybridge.FrameData {
			continue
		}
		got.Write(payload)
		if strings.Contains(got.String(), "ws-ok") {
			return
		}
	}
	t.Fatalf("never saw output: %q", got.String())
}

func TestWebSocketRejectsForeignOrigin(t *testing.T) {
	h := front.NewTerminalHandler(front.TerminalConfig{AllowedOrigin: "https://tmux.example.com"})
	ts := httptest.NewServer(h)
	defer ts.Close()

	// A service published on a sibling subdomain is same-site, so the browser
	// would attach the device cookie. Origin is the boundary that stops it.
	_, _, err := websocket.Dial(context.Background(),
		"ws"+strings.TrimPrefix(ts.URL, "http")+"?session=work",
		&websocket.DialOptions{HTTPHeader: http.Header{"Origin": {"https://test.example.com"}}})
	if err == nil {
		t.Fatal("sibling subdomain origin must be rejected")
	}
}
```

**Step 3: Run it, expect FAIL**

**Step 4: Implement `ws.go`**

Key points for the implementer:

- `websocket.Accept` with `OriginPatterns` left unset rejects cross-origin by
  default, but do the check explicitly against the configured host so the
  behavior is obvious and testable.
- Read loop and write loop are separate goroutines; closing either closes both.
- `c.Ping(ctx)` every 20 seconds with a 10-second deadline. On failure, close.
  This is what collects a half-open connection: without it the `tmux attach`
  process stays alive and attached, `destroy-unattached` never fires, and
  reconnect spawns a second session while the first leaks.
- Control frames decode into `struct { Type string; Cols, Rows uint16; Pane string }`
  handling `resize`, `select`, and `copy-mode` (which sends `copy-mode` via
  `tmux copy-mode -t <pane>`).

**Step 5: Run tests, then commit**

```bash
go test ./internal/front/ -v -count=1
git add internal/front go.mod go.sum
git commit -m "feat: websocket terminal handler with origin check and keepalive"
```

---

## Phase C — auth

### Task 11: Device store

**Files:**
- Create: `internal/auth/store.go`
- Test: `internal/auth/store_test.go`

**Step 1: Write the failing test**

Cover: add/list/revoke round trip; persistence across reopen; only the hash is
persisted (assert the raw token never appears in the file bytes); concurrent
writers do not lose updates.

```go
func TestStoreNeverPersistsRawTokens(t *testing.T) {
	dir := t.TempDir()
	s, _ := auth.OpenStore(filepath.Join(dir, "devices.json"))
	tok, _ := s.AddDevice("laptop", "ua")

	raw, _ := os.ReadFile(filepath.Join(dir, "devices.json"))
	if bytes.Contains(raw, []byte(tok)) {
		t.Fatal("raw device token was written to disk")
	}
}

func TestStoreConcurrentWritesDoNotLoseUpdates(t *testing.T) {
	dir := t.TempDir()
	s, _ := auth.OpenStore(filepath.Join(dir, "devices.json"))

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); _, _ = s.AddDevice(fmt.Sprint(i), "ua") }(i)
	}
	wg.Wait()

	if got := len(s.Devices()); got != 20 {
		t.Fatalf("want 20 devices, got %d — atomic rename prevents torn files, "+
			"not lost read-modify-write updates", got)
	}
}
```

**Step 2–4:** Implement with a `sync.Mutex` guarding an in-memory map, persisted
by write-to-temp + `os.Rename`. Store `sha256` of the token. Fields: `ID`,
`Name`, `TokenHash`, `UserAgent`, `CreatedAt`, `LastSeen`.

**As built.** `OpenStore(path)`, `AddDevice(name, ua) (token, error)`,
`Devices()`, `Lookup(token) (Device, bool)`, `Touch(id, t)`, `Revoke(id)`, plus
`DefaultPath()` for `$XDG_STATE_HOME/wterm-web/devices.json` (a relative
`XDG_STATE_HOME` is ignored, per the basedir spec). `Lookup` and `Touch` are
here rather than in task 15 because the store is what holds the hash, and
because `LastSeen` is otherwise a field nothing can ever update. They are split
so the request path does not write a file per request.

Decisions the tests pin, each of which a mutant would otherwise get away with:

- **A corrupt or unreadable state file fails `OpenStore`**; a missing or
  zero-length one is an empty store. Starting empty over a file that is merely
  unparseable would sign out every enrolled device without saying so, and the
  next enrolment would overwrite the only copy an operator could still recover.
  The daemon should report that error and exit, not proceed (task 17).
- **A failed persist rolls back the in-memory change**, so a revoke that could
  not be written does not cut a device off until the next restart brings it
  back.
- **`Revoke` on an unknown id is `ErrNoSuchDevice`**, not a silent success:
  `revoke <id>` is the response to a lost laptop.
- **`Devices()` is sorted oldest-first**, not map order, so the CLI listing does
  not reshuffle between runs.
- Atomicity is pinned twice: a concurrent reader that must never parse a partial
  file, plus an `os.SameFile` check that the store file is *replaced* rather
  than rewritten in place. The reader test alone catches a truncating write only
  about a third of the time.

`crypto/subtle.ConstantTimeCompare` is used in `Lookup`, but it is **not**
load-bearing and should not be described as the defence: the values compared are
SHA-256 digests, not the token, so a timing leak would reveal digest bytes that
are useless without a preimage.

**Step 5: Commit**

```bash
git commit -m "feat: device store with hashed tokens and serialized writes"
```

---

### Task 12: Enrollment tokens

**Files:**
- Create: `internal/auth/enroll.go`
- Test: `internal/auth/enroll_test.go`

Tests to write first:

- A minted token redeems once and returns a device token.
- The same token redeemed twice fails.
- A token past its TTL fails.
- An unknown token fails.
- Comparison uses `subtle.ConstantTimeCompare`.
- Redemption is rate limited: N failures from one source start failing fast.

Implementation: 32 bytes from `crypto/rand`, base64url, held in memory with an
expiry (10 minutes). Enrollment tokens do not need to survive a restart — losing
them just means running `enroll` again.

```bash
git commit -m "feat: single-use enrollment tokens"
```

---

### Task 13: Admin socket with SO_PEERCRED

**Files:**
- Create: `internal/auth/adminsock.go`
- Test: `internal/auth/adminsock_test.go`

**Step 1: Write the failing test**

```go
func TestAdminSocketAcceptsOwnUID(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "admin.sock")
	ln, err := auth.ListenAdmin(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	fi, err := os.Stat(sock)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode %v, want 0600", fi.Mode().Perm())
	}

	go http.Serve(ln, auth.AdminMux(...))

	// A connection from this process is the service uid and must be accepted.
	// Testing rejection requires a second uid, which a unit test cannot create;
	// assert the uid extraction instead.
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
```

**Step 3: Implement**

```go
func PeerUID(c *net.UnixConn) (uint32, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred *unix.Ucred
	var cerr error
	err = raw.Control(func(fd uintptr) {
		cred, cerr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	})
	if err != nil {
		return 0, err
	}
	if cerr != nil {
		return 0, cerr
	}
	return cred.Uid, nil
}
```

`ListenAdmin` unlinks a stale socket, listens, then `os.Chmod(sock, 0o600)`.
The serving wrapper rejects any connection whose `PeerUID` differs from
`os.Getuid()`.

```bash
go get golang.org/x/sys/unix
git commit -m "feat: admin unix socket authenticated by SO_PEERCRED"
```

---

### Task 14: CLI subcommands

**Files:**
- Modify: `cmd/wterm-web/main.go`
- Create: `cmd/wterm-web/cli.go`

Subcommands: `serve`, `enroll --name`, `devices`, `revoke <id>`. All but `serve`
talk to the admin socket. `enroll` prints the full URL with the token in the
fragment.

Manual verification (no automated test — it needs a running daemon):

```bash
./wterm-web serve --host tmux.example.com --dev &
./wterm-web enroll --name laptop
# expect: https://tmux.example.com/enroll#<43 chars>
./wterm-web devices
```

```bash
git commit -m "feat: enroll, devices, and revoke subcommands"
```

---

### Task 15: HTTP auth middleware

**Files:**
- Create: `internal/front/auth.go`
- Test: `internal/front/auth_test.go`

Tests first:

- No cookie on a protected route → 401.
- Valid `__Host-wterm_device` cookie → 200.
- Revoked device's cookie → 401.
- `POST` with `Origin: https://test.example.com` → 403 even with a valid cookie.
- `POST` with no `Origin` → 403.
- `GET` with no `Origin` → allowed (navigations have no Origin).
- The cookie is set with `Secure`, `HttpOnly`, `Path=/`, and **no** `Domain`.

```go
func TestSetCookieUsesHostPrefix(t *testing.T) {
	rec := httptest.NewRecorder()
	front.SetDeviceCookie(rec, "tok")
	sc := rec.Result().Cookies()[0]

	if !strings.HasPrefix(sc.Name, "__Host-") {
		t.Fatal("device cookie must use the __Host- prefix so a sibling " +
			"subdomain cannot shadow it with a Domain=.example.com cookie")
	}
	if sc.Domain != "" {
		t.Fatalf("Domain must be empty, got %q", sc.Domain)
	}
	if !sc.Secure || !sc.HttpOnly || sc.Path != "/" {
		t.Fatalf("bad cookie attributes: %+v", sc)
	}
}
```

```bash
git commit -m "feat: device cookie auth with exact-origin csrf boundary"
```

---

### Task 16: Revocation severs live connections

**Files:**
- Create: `internal/front/registry.go`
- Test: `internal/front/registry_test.go`

**Step 1: Write the failing test**

```go
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
```

**Step 3:** A `map[deviceID][]func()` under a mutex. The WebSocket handler
registers a closer on connect and deregisters on disconnect. `revoke` and
`sign out` call `CloseDevice` after removing the device from the store.

```bash
git commit -m "feat: revocation closes live sockets and attach ptys"
```

---

### Task 17: HTTP mux, enrollment page, and TLS

**Files:**
- Create: `internal/front/server.go`, `internal/front/embed.go`
- Modify: `cmd/wterm-web/main.go`

Routes:

| Route | Auth | Purpose |
| --- | --- | --- |
| `GET /` | device cookie | SPA shell |
| `GET /enroll` | none | page that reads `location.hash` |
| `POST /api/enroll` | none, rate limited | redeem token → set cookie |
| `GET /api/snapshot` | device cookie | cached poller output |
| `GET /api/devices` | device cookie | list |
| `POST /api/devices` | device cookie + Origin | mint a link |
| `DELETE /api/devices/{id}` | device cookie + Origin | revoke |
| `GET /ws` | device cookie + Origin | terminal |

TLS: `certmagic.HTTPS([]string{cfg.Host}, mux)` — a **single-name** certificate.
v1 serves one hostname, and a DNS-01 wildcard would put zone-editing credentials
on the box for a feature that is deferred. A `--dev` flag serves plain HTTP on
localhost so the whole stack is testable without certificates.

At startup: `tmux.Client.Sweep` before serving. Its error is logged and
startup continues -- failing to collect orphans must not stop the daemon
serving. It returns nil when there is no tmux server at all, so the ordinary
cold-start case logs nothing.

```bash
git commit -m "feat: http server, enrollment flow, and single-name tls"
```

---

## Phase D — frontend

### Task 18: Scaffold

```bash
cd web
pnpm create vite . --template react-ts
pnpm add -D tailwindcss @tailwindcss/vite
pnpm dlx shadcn@latest init
pnpm dlx shadcn@latest add sidebar command dialog button badge sheet \
    scroll-area tooltip separator dropdown-menu sonner
pnpm add @wterm/react qrcode
```

Configure Vite `build.outDir` to `../internal/front/dist` so `go:embed` picks it
up. Verify `pnpm build` then `go build ./...` succeeds.

```bash
git commit -m "chore: scaffold vite react shadcn frontend"
```

---

### Task 19: Transport with frame prefix and no auto-reconnect

**Files:**
- Create: `web/src/lib/transport.ts`
- Test: `web/src/lib/transport.test.ts`

**This replaces `@wterm/core`'s `WebSocketTransport` deliberately.** That class
is a raw pipe with no room for a control channel, and — more importantly — its
built-in reconnect buffers `send()` while disconnected and flushes on reopen.
Our reconnect creates a *new* tmux session, so buffered keystrokes would be
delivered into whatever pane the new session lands on. Reconnection is driven by
us, at a layer that knows to restore position first.

Write tests (vitest, mocking `WebSocket`) asserting:

- Outgoing data is prefixed `0x00`, control JSON `0x01`.
- Incoming `0x00` frames reach the terminal sink; `0x01` frames reach the
  control handler.
- Nothing is buffered across a close — pending writes are dropped, not queued.

```bash
git commit -m "feat: framed websocket transport without unsafe send buffering"
```

---

### Task 20: Terminal component

**Files:**
- Create: `web/src/components/Terminal.tsx`

`useTerminal` from `@wterm/react`, wired to the transport. `ResizeObserver`
debounced 150ms → control frame `{type:"resize",cols,rows}`. Store the current
`session:window.pane` in `sessionStorage`; on reconnect, send a `select` control
frame before enabling input, falling back to the group's active window if the
remembered pane is gone.

```bash
git commit -m "feat: terminal component with debounced resize and position restore"
```

---

### Task 21: Sidebar

**Files:**
- Create: `web/src/components/AppSidebar.tsx`, `web/src/lib/useSnapshot.ts`

Poll `GET /api/snapshot` every 1.5s. Group rows by `groupKey` → `windowIndex`.
Render `SidebarGroup` / `SidebarMenu` / `SidebarMenuSub`, with a `Badge` showing
`command`. Panes render as sub-items only when a window has more than one.
Clicking sends a `select` control frame.

```bash
git commit -m "feat: sidebar tree from tmux snapshot"
```

---

### Task 22: Footer, devices dialog, palette

**Files:**
- Create: `web/src/components/UserMenu.tsx`, `DevicesDialog.tsx`, `Palette.tsx`

Footer shows `user@host` and the device name. The devices dialog lists devices
with last-seen, mints links (rendering a QR code beside the copyable URL, since
enrolling a phone is the realistic path), and revokes. `Ctrl+Alt+K` opens the
palette in the capture phase, ahead of wterm's key handler — but the header
button is the primary affordance, because `Ctrl+Alt+K` is AltGr+K on several
European keyboard layouts. Include an explicit "enter copy mode" action.

```bash
git commit -m "feat: user menu, devices dialog with qr, and command palette"
```

---

### Task 23: End-to-end

**Files:**
- Create: `e2e/terminal.spec.ts`, `playwright.config.ts`

Build the binary, start it with `--dev`, mint a token over the admin socket,
visit the enroll URL, then assert on the DOM. wterm renders rows as DOM nodes,
so this is an ordinary text assertion rather than canvas pixel-poking.

```ts
test('types into a real tmux pane', async ({ page }) => {
  await page.goto(`http://localhost:7000/enroll#${token}`);
  await expect(page.locator('[data-wterm-row]')).toBeVisible();
  await page.keyboard.type('echo e2e-ok\n');
  await expect(page.locator('[data-wterm-screen]')).toContainText('e2e-ok');
});
```

```bash
git commit -m "test: end-to-end enrollment and terminal round trip"
```

---

## Definition of done

- `go test ./... -count=1` passes, including every tmux integration test.
- `pnpm test` and `pnpm build` pass.
- `make build` produces a single binary that runs with no runtime dependencies
  beyond `tmux` itself.
- Manual check, in order:
  1. `./wterm-web serve --host <host>` with no prior state.
  2. `./wterm-web enroll --name laptop`, open the URL on another machine.
  3. Sidebar lists real sessions; clicking a pane switches to it.
  4. Kill the network, restore it: the terminal reconnects to the *same* pane.
  5. Kill the base session locally while the tab is open: agents stay visible.
  6. Create a session named `_web-notes`; restart the daemon; it still exists.
  7. Revoke the device from a second browser: the first one's terminal drops.

## Explicitly out of scope

Publishing local ports, the gated-route token exchange, the git context panel,
wildcard certificates, the audit log, and idle session expiry. The design
document records why each is deferred and what hooks exist for it.
