package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// adminSocketName is the socket's basename in whichever runtime directory
// AdminSocketPath settles on. The daemon and the CLI must agree on it without
// talking to each other, so it is a constant rather than a flag default.
const adminSocketName = "tmux-web.sock"

// AdminBaseURL is the URL the admin API answers on. A unix socket has no host,
// but Go's HTTP client insists on one; the client from AdminClient ignores it
// and dials the socket instead.
const AdminBaseURL = "http://admin"

// ErrAdminSocketInUse means another tmux-web is already listening on the admin
// socket. It is distinct so `serve` can say "already running" rather than
// reporting a bind failure the operator would try to fix by deleting the file.
var ErrAdminSocketInUse = errors.New("auth: admin socket already in use")

// adminDialTimeout bounds the liveness probe in ListenAdmin. A unix connect to
// a socket with no listener fails immediately; the timeout only covers a
// listener whose backlog is full, which is a live daemon by definition.
const adminDialTimeout = 2 * time.Second

// runtimeDirRoot is where the per-user runtime directories live when
// $XDG_RUNTIME_DIR is not set. It is a variable only so a test can point it at
// a directory it controls.
var runtimeDirRoot = "/run/user"

// AdminSocketPath is where the daemon listens and the CLI dials.
//
// $XDG_RUNTIME_DIR is the answer whenever there is one: it is per-user, mode
// 0700, and cleared on logout, which is exactly the lifetime of a socket that
// grants a shell. It is absent in cron jobs, some containers, and ssh sessions
// without a login manager, so:
//
//  1. $XDG_RUNTIME_DIR when it is absolute (the spec says to ignore a relative
//     value),
//  2. /run/user/<uid> when it exists and we own it -- on any systemd machine
//     that is the same directory the daemon got as $XDG_RUNTIME_DIR, so a CLI
//     run from cron still finds the socket the service is listening on,
//  3. the state directory the device store lives in, under $XDG_STATE_HOME or
//     ~/.local/state.
//
// Deliberately never /tmp. It is world-writable, so a predictable path in it
// can be squatted by another local user before the daemon starts -- turning a
// missing socket into a denial of service, or into a socket someone else owns
// for a CLI that has no way to tell. Every fallback here is a directory only
// this user can create entries in.
func AdminSocketPath() (string, error) {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); filepath.IsAbs(dir) {
		return filepath.Join(dir, adminSocketName), nil
	}

	uid := os.Getuid()
	if dir := filepath.Join(runtimeDirRoot, strconv.Itoa(uid)); ownedDir(dir, uid) {
		return filepath.Join(dir, adminSocketName), nil
	}

	state, err := DefaultPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(state), adminSocketName), nil
}

// ownedDir reports whether path is a directory belonging to uid. Ownership is
// the point: /run/user/<uid> is created by the login manager, and a path that
// happens to exist but belongs to someone else is not a runtime directory of
// ours to put a credential-minting socket in.
func ownedDir(path string, uid int) bool {
	fi, err := os.Stat(path)
	if err != nil || !fi.IsDir() {
		return false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && st.Uid == uint32(uid)
}

// ListenAdmin listens on the admin socket at path, refusing every connection
// whose peer uid is not this process's own.
//
// The mode is 0600 and the directory is created 0700, but neither is the
// authorization: a mode is advisory in the sense that it only stops other users
// from *reaching* the socket, and the window between bind and chmod is
// unavoidable. SO_PEERCRED is the authorization, and the permissions are the
// layer that keeps a foreign process from getting as far as being refused.
func ListenAdmin(path string) (net.Listener, error) {
	return listenAdmin(path, uint32(os.Getuid()), PeerUID)
}

// listenAdmin is ListenAdmin with the service uid and the credential source as
// parameters. Tests substitute a peer uid because a unit test cannot create a
// second one; nothing in production calls it with anything else.
func listenAdmin(path string, self uint32, peerUID func(*net.UnixConn) (uint32, error)) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("auth: create admin socket directory: %w", err)
	}
	if err := clearStaleSocket(path); err != nil {
		return nil, err
	}

	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("auth: listen on admin socket %s: %w", path, err)
	}
	// Not defence in depth against a peer we would refuse anyway: it is what
	// keeps another local user from opening connections at all.
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, fmt.Errorf("auth: restrict admin socket %s: %w", path, err)
	}

	unixLn, ok := ln.(*net.UnixListener)
	if !ok { // unreachable: net.Listen("unix", ...) returns *net.UnixListener
		ln.Close()
		return nil, fmt.Errorf("auth: admin socket is a %T, not a unix listener", ln)
	}
	return &peerListener{UnixListener: unixLn, self: self, peerUID: peerUID}, nil
}

// clearStaleSocket removes the socket file left behind by a daemon that died
// without unlinking it -- a SIGKILL, an OOM kill, a power cut -- so a restart
// does not need a manual rm.
//
// It refuses two cases instead. A socket something is still listening on
// belongs to a running daemon: unlinking it would leave two daemons up, only
// one of them reachable, and `revoke` landing on whichever won. And anything
// that is not a socket is not ours to delete, symlinks included: Lstat rather
// than Stat means a symlink planted at the path is refused rather than
// followed.
func clearStaleSocket(path string) error {
	fi, err := os.Lstat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil
	case err != nil:
		return fmt.Errorf("auth: inspect admin socket %s: %w", path, err)
	case fi.Mode()&os.ModeSocket == 0:
		return fmt.Errorf("auth: %s exists and is not a socket (mode %v); refusing to remove it", path, fi.Mode())
	}

	c, err := net.DialTimeout("unix", path, adminDialTimeout)
	if err == nil {
		c.Close()
		return fmt.Errorf("%w: %s", ErrAdminSocketInUse, path)
	}
	// ECONNREFUSED is the dead-daemon answer: the file is there, nobody is
	// behind it. ENOENT is a socket that vanished under us. Anything else --
	// a permission error, a timeout against a full backlog -- is not evidence
	// that the socket is dead, so it must not be deleted on the strength of it.
	if !errors.Is(err, syscall.ECONNREFUSED) && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("auth: probe admin socket %s: %w", path, err)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("auth: remove stale admin socket %s: %w", path, err)
	}
	return nil
}

// peerListener refuses connections from any uid but self.
//
// The check belongs here, per connection, and not in HTTP middleware. The
// credential is a property of the socket, not of a request: with keep-alive one
// connection carries many requests, so a per-request check would either re-read
// the same answer or -- worse -- read it from a request context that some
// handler or future route forgot to thread through. Refusing at accept means a
// foreign process never gets to send a byte, and no handler can be added later
// that skips the check.
type peerListener struct {
	*net.UnixListener
	self    uint32
	peerUID func(*net.UnixConn) (uint32, error)
}

// Accept returns the next connection from the service uid, closing and logging
// anything else.
//
// A refusal must not surface as an Accept error: http.Serve treats a
// non-temporary Accept error as fatal and stops serving, so one connection from
// another user would take the admin socket down until the daemon restarts.
func (l *peerListener) Accept() (net.Conn, error) {
	for {
		c, err := l.AcceptUnix()
		if err != nil {
			return nil, err
		}

		uid, uerr := l.peerUID(c)
		switch {
		case uerr != nil:
			// No proof of who is on the other end is no authorization.
			slog.Warn("refused admin connection: peer credentials unavailable", "error", uerr)
		case !peerAllowed(uid, l.self):
			slog.Warn("refused admin connection from another user", "peer_uid", uid, "service_uid", l.self)
		default:
			return c, nil
		}
		c.Close()
	}
}

// peerAllowed reports whether a peer uid may use the admin socket. Only the uid
// the daemon runs as may: the socket's whole claim is "you are the service
// user", and root is a different uid, not an exception -- root can reach this
// shell by a dozen shorter routes and does not need one wired through here.
func peerAllowed(peer, self uint32) bool {
	return peer == self
}

// PeerUID returns the uid of the process on the other end of c, as the kernel
// reports it. It is not a claim the peer makes, so there is nothing for it to
// forge; the value is recorded at connect time, so a peer that later execs a
// setuid binary does not change it.
func PeerUID(c *net.UnixConn) (uint32, error) {
	cred, err := peerCred(c)
	if err != nil {
		return 0, err
	}
	return cred.Uid, nil
}

// peerCred reads the peer's credentials off the connection.
func peerCred(c *net.UnixConn) (*unix.Ucred, error) {
	raw, err := c.SyscallConn()
	if err != nil {
		return nil, fmt.Errorf("auth: admin connection file descriptor: %w", err)
	}
	var cred *unix.Ucred
	var cerr error
	if err := raw.Control(func(fd uintptr) {
		cred, cerr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return nil, fmt.Errorf("auth: read admin peer credentials: %w", err)
	}
	if cerr != nil {
		return nil, fmt.Errorf("auth: read admin peer credentials: %w", cerr)
	}
	return cred, nil
}

// AdminClient returns an HTTP client that reaches the admin socket at path.
// Request URLs are AdminBaseURL plus the route; the host in them is ignored.
func AdminClient(path string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", path)
			},
		},
	}
}

// AdminConfig is what the admin API operates on.
type AdminConfig struct {
	Store    *Store
	Enroller *Enroller

	// BaseURL is the daemon's public origin, e.g. https://tmux.example.com. It
	// lives here because the enrollment link is rendered where the host is
	// known: the CLI is handed a finished URL rather than a token and a guess.
	BaseURL string
}

// AdminMux is the API served on the admin socket. Everything reaching it has
// already proved, through SO_PEERCRED, that it runs as the service user, so
// there is no per-request auth here and no CSRF boundary: a browser cannot open
// a unix socket, and every caller is the owner.
//
// It speaks HTTP because the transport is the only thing unusual about it --
// http.Serve takes any net.Listener -- and a hand-rolled line protocol would
// mean reinventing framing, methods, status codes, and error shapes for a
// socket that carries a handful of requests a week. The same routes are what
// Task 17 exposes to the browser behind a device cookie.
func AdminMux(cfg AdminConfig) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /enroll", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
			writeAdminError(w, http.StatusBadRequest, fmt.Errorf("auth: parse enroll request: %w", err))
			return
		}
		name := strings.TrimSpace(req.Name)
		if name == "" {
			writeAdminError(w, http.StatusBadRequest, errors.New("auth: enroll requires a device name"))
			return
		}
		// Before minting, not after: a token that cannot be rendered into a
		// link is a live credential nobody can use and nobody can see.
		base := strings.TrimRight(cfg.BaseURL, "/")
		if base == "" {
			writeAdminError(w, http.StatusInternalServerError,
				errors.New("auth: the daemon has no public URL configured, so no enrollment link can be built"))
			return
		}

		token, err := cfg.Enroller.Mint(name)
		if err != nil {
			writeAdminError(w, http.StatusInternalServerError, err)
			return
		}
		// The token goes in the fragment: it is never sent to the server, so it
		// stays out of access logs and Referer headers, and the link scanners
		// messaging apps run do not redeem it on the way past.
		writeAdminJSON(w, http.StatusOK, map[string]string{
			"name": name,
			"url":  base + "/enroll#" + token,
		})
	})

	mux.HandleFunc("GET /devices", func(w http.ResponseWriter, r *http.Request) {
		devices := cfg.Store.Devices()
		out := make([]deviceView, 0, len(devices))
		for _, d := range devices {
			out = append(out, deviceView{
				ID:        d.ID,
				Name:      d.Name,
				UserAgent: d.UserAgent,
				CreatedAt: d.CreatedAt,
				LastSeen:  d.LastSeen,
			})
		}
		writeAdminJSON(w, http.StatusOK, map[string]any{"devices": out})
	})

	// Revoking here removes the device's credential. Severing its live
	// WebSockets and attach PTYs is the registry's job (Task 16), and this
	// handler is one of the two callers that must do it.
	mux.HandleFunc("DELETE /devices/{id}", func(w http.ResponseWriter, r *http.Request) {
		err := cfg.Store.Revoke(r.PathValue("id"))
		switch {
		case errors.Is(err, ErrNoSuchDevice):
			writeAdminError(w, http.StatusNotFound, err)
		case err != nil:
			writeAdminError(w, http.StatusInternalServerError, err)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	})
	return mux
}

// deviceView is what the admin API says about a device. It is a separate type
// from Device on purpose: Device carries TokenHash, and a struct that is
// serialized straight out of the store is one field away from publishing
// credential material.
type deviceView struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	UserAgent string    `json:"user_agent"`
	CreatedAt time.Time `json:"created_at"`
	LastSeen  time.Time `json:"last_seen"`
}

func writeAdminJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(body)
}

// writeAdminError reports err to the caller. The caller is the owner, on a
// socket only they can open, so the real error goes over the wire: there is
// nobody here to keep it from.
func writeAdminError(w http.ResponseWriter, code int, err error) {
	writeAdminJSON(w, code, map[string]string{"error": err.Error()})
}
