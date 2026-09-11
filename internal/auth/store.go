// Package auth holds the device store, enrollment tokens, and the admin socket
// that anchors them. There are no passwords anywhere in it: a browser proves
// nothing on its own, so trust is projected from a local unix socket -- where
// SO_PEERCRED yields an unforgeable uid -- onto a device via a single-use
// enrollment link.
package auth

import (
	"cmp"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

// ErrNoSuchDevice is returned by Revoke and Touch for an id the store does not
// hold. Revoking an unknown id is an error rather than a no-op: `revoke <id>`
// is how an operator responds to a lost laptop, and silently reporting success
// for a mistyped id would leave them believing a device was cut off when it
// still has a shell.
var ErrNoSuchDevice = errors.New("auth: no such device")

// Device is one enrolled browser. TokenHash is the SHA-256 of the device token
// in hex; the token itself is returned once, by AddDevice, and never stored --
// reading this file must not be enough to authenticate.
//
// Device sessions never expire, so ID is the durable handle revocation works
// through, both here and in the registry that closes a revoked device's live
// sockets.
//
// The json tags are the on-disk format, not an API shape: TokenHash must never
// be serialized to a browser. Whatever renders the devices list builds its own
// view of Name, CreatedAt, and LastSeen.
type Device struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	TokenHash string    `json:"token_hash"`
	UserAgent string    `json:"user_agent"`
	CreatedAt time.Time `json:"created_at"`
	LastSeen  time.Time `json:"last_seen"`
}

// Store is the set of enrolled devices, persisted as one JSON file.
//
// Every mutation takes mu for the whole read-modify-write-persist cycle. The
// atomic rename in persist stops a reader seeing a half-written file; it does
// nothing about two writers each rendering the state they read before the other
// landed, which is how concurrent enrolments would silently drop devices.
type Store struct {
	path string

	mu      sync.Mutex
	devices map[string]Device // by ID
}

type stateFile struct {
	Devices []Device `json:"devices"`
}

// OpenStore loads the store at path, creating its directory if needed. A file
// that does not exist yet is an empty store, not an error: that is every cold
// start.
//
// A file that exists but cannot be read or parsed *is* an error, deliberately.
// Starting empty instead would sign out every enrolled device without saying
// so, and the first enrolment afterwards would overwrite the only copy of the
// file an operator might still have recovered. Failing here surfaces the
// problem while the evidence is intact; the recovery path is the documented one
// -- ssh in, look at the file, remove it, run enroll again.
func OpenStore(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("auth: create state directory: %w", err)
	}

	s := &Store{path: path, devices: map[string]Device{}}

	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return s, nil
	case err != nil:
		return nil, fmt.Errorf("auth: read device store: %w", err)
	case len(raw) == 0:
		// A zero-length file is what a crash between create and write leaves
		// behind. It holds no devices to lose, so there is nothing to protect
		// by refusing to start.
		return s, nil
	}

	var state stateFile
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, fmt.Errorf("auth: parse device store %s: %w", path, err)
	}
	for _, d := range state.Devices {
		s.devices[d.ID] = d
	}
	return s, nil
}

// AddDevice enrolls a device and returns its token. The token is returned here
// and nowhere else -- only its hash is kept -- so the caller must hand it
// straight to the browser it belongs to.
func (s *Store) AddDevice(name, userAgent string) (string, error) {
	token, err := randomToken()
	if err != nil {
		return "", err
	}
	id, err := randomID()
	if err != nil {
		return "", err
	}

	now := time.Now().UTC()
	d := Device{
		ID:        id,
		Name:      name,
		TokenHash: hashToken(token),
		UserAgent: userAgent,
		CreatedAt: now,
		LastSeen:  now,
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.devices[id] = d
	if err := s.persist(); err != nil {
		delete(s.devices, id) // memory and disk must not disagree
		return "", err
	}
	return token, nil
}

// Devices lists the enrolled devices, oldest first. The order is the order the
// file and the CLI listing show, so it must not be map iteration order.
func (s *Store) Devices() []Device {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sorted()
}

// Lookup resolves a device token to its device. It reports only whether the
// token matches; it does not record a visit, because it sits on every
// authenticated request and a write per request is not worth a last-seen
// timestamp. Callers that want one call Touch.
//
// The comparison is over SHA-256 digests, not the token, so ConstantTimeCompare
// is not load-bearing here: a timing leak would reveal digest bytes, which are
// useless without a preimage. It costs one call and forecloses the question.
func (s *Store) Lookup(token string) (Device, bool) {
	// An empty token -- a request with no cookie -- needs no special case: its
	// digest is a digest like any other and matches no enrolled device. An
	// explicit guard here was dead code, and mutation testing said so.
	want := []byte(hashToken(token))

	// A scan rather than a second map keyed by hash: one user enrolls a handful
	// of devices, and one map cannot drift out of step with itself.
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.devices {
		if subtle.ConstantTimeCompare(want, []byte(d.TokenHash)) == 1 {
			return d, true
		}
	}
	return Device{}, false
}

// Touch records that a device was seen at t.
func (s *Store) Touch(id string, t time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	d, ok := s.devices[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNoSuchDevice, id)
	}
	prev := d.LastSeen
	d.LastSeen = t.UTC()
	s.devices[id] = d
	if err := s.persist(); err != nil {
		d.LastSeen = prev
		s.devices[id] = d
		return err
	}
	return nil
}

// Revoke removes one device. Its token stops authenticating immediately;
// severing that device's live sockets is the caller's job, and the reason
// revocation is by id.
func (s *Store) Revoke(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	d, ok := s.devices[id]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNoSuchDevice, id)
	}
	delete(s.devices, id)
	if err := s.persist(); err != nil {
		s.devices[id] = d // memory and disk must not disagree
		return err
	}
	return nil
}

// sorted returns the devices oldest first, id breaking ties. Callers must hold
// mu. Device is a value type with no reference fields, so the slice is a copy
// the caller may keep.
func (s *Store) sorted() []Device {
	out := make([]Device, 0, len(s.devices))
	for _, d := range s.devices {
		out = append(out, d)
	}
	slices.SortFunc(out, func(a, b Device) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return cmp.Compare(a.ID, b.ID)
	})
	return out
}

// persist writes the store to a temp file in the same directory and renames it
// over the real one. Callers must hold mu.
//
// The rename is what makes a concurrent reader -- another process running
// `devices`, or this daemon restarting -- see either the whole old file or the
// whole new one. A plain truncating write exposes a window where the file is
// empty or half-written, and a reader landing in it either loses every device
// or fails to parse.
//
// The temp file is synced before the rename, so a crash cannot leave a renamed
// file whose contents were never flushed. The directory is deliberately not
// synced: a crash that loses the rename leaves the previous consistent state,
// which costs at most one re-enrolment, and that is not worth an fsync on the
// directory of every write.
func (s *Store) persist() error {
	data, err := json.MarshalIndent(stateFile{Devices: s.sorted()}, "", "  ")
	if err != nil {
		return fmt.Errorf("auth: encode device store: %w", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(s.path)
	f, err := os.CreateTemp(dir, ".devices-*.json")
	if err != nil {
		return fmt.Errorf("auth: create temp state file: %w", err)
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op once the rename has succeeded

	if err := writeAndSync(f, data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("auth: close temp state file: %w", err)
	}
	// CreateTemp makes the file 0600 already; being explicit keeps that true if
	// it ever changes. Device names and user agents are not secrets, but they
	// are nobody else's business either.
	if err := os.Chmod(tmp, 0o600); err != nil {
		return fmt.Errorf("auth: chmod temp state file: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("auth: replace device store: %w", err)
	}
	return nil
}

func writeAndSync(f *os.File, data []byte) error {
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("auth: write temp state file: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("auth: sync temp state file: %w", err)
	}
	return nil
}

// DefaultPath is where the daemon and the CLI both expect the store, following
// the XDG basedir spec: $XDG_STATE_HOME, or ~/.local/state when it is unset or
// relative (the spec says a relative value must be ignored).
func DefaultPath() (string, error) {
	base := os.Getenv("XDG_STATE_HOME")
	if !filepath.IsAbs(base) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("auth: locate state directory: %w", err)
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "tmux-web", "devices.json"), nil
}

// randomToken mints a 32-byte device token, base64url without padding, so it
// survives a URL fragment and a cookie value unescaped.
func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("auth: generate device token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// randomID mints a device id: 8 random bytes as hex. It is not a secret -- it
// appears in URLs and in `devices` output, and is typed back into
// `revoke <id>` -- so it trades length for something a human can read off a
// terminal and retype. 64 bits is far past a birthday collision for a store
// that holds a handful of rows, so AddDevice does not check for one.
func randomID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("auth: generate device id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
