package auth_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/diegok/tmux-web/internal/auth"
)

func openStore(t *testing.T) (*auth.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "devices.json")
	s, err := auth.OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	return s, path
}

func TestStoreAddListRevokeRoundTrip(t *testing.T) {
	s, _ := openStore(t)

	if got := s.Devices(); len(got) != 0 {
		t.Fatalf("a fresh store must be empty, got %d devices", len(got))
	}

	tok, err := s.AddDevice("laptop", "Firefox/1.0")
	if err != nil {
		t.Fatalf("AddDevice: %v", err)
	}
	if tok == "" {
		t.Fatal("AddDevice returned an empty device token")
	}

	devices := s.Devices()
	if len(devices) != 1 {
		t.Fatalf("want 1 device, got %d", len(devices))
	}
	d := devices[0]
	if d.Name != "laptop" || d.UserAgent != "Firefox/1.0" {
		t.Fatalf("device did not keep its name and user agent: %+v", d)
	}
	if d.ID == "" {
		t.Fatal("device needs a stable id: revocation must sever live connections by id")
	}
	if d.CreatedAt.IsZero() || d.LastSeen.IsZero() {
		t.Fatalf("timestamps must be set at enrolment: %+v", d)
	}

	if err := s.Revoke(d.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if got := s.Devices(); len(got) != 0 {
		t.Fatalf("revoked device is still listed: %+v", got)
	}
}

func TestStorePersistsAcrossReopen(t *testing.T) {
	s, path := openStore(t)

	if _, err := s.AddDevice("laptop", "Firefox/1.0"); err != nil {
		t.Fatalf("AddDevice: %v", err)
	}
	tok, err := s.AddDevice("phone", "Safari/1.0")
	if err != nil {
		t.Fatalf("AddDevice: %v", err)
	}

	reopened, err := auth.OpenStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	devices := reopened.Devices()
	if len(devices) != 2 {
		t.Fatalf("want 2 devices after reopen, got %d", len(devices))
	}
	if devices[0].Name != "laptop" || devices[1].Name != "phone" {
		t.Fatalf("devices did not survive the reopen in order: %+v", devices)
	}
	if _, ok := reopened.Lookup(tok); !ok {
		t.Fatal("a token minted before the restart no longer authenticates; " +
			"every live browser session would be signed out on daemon restart")
	}
}

func TestStoreNeverPersistsRawTokens(t *testing.T) {
	s, path := openStore(t)

	tok, err := s.AddDevice("laptop", "ua")
	if err != nil {
		t.Fatalf("AddDevice: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read state file: %v", err)
	}
	if bytes.Contains(raw, []byte(tok)) {
		t.Fatal("raw device token was written to disk: anyone who can read the " +
			"state file could authenticate with what they found")
	}
}

func TestStoreConcurrentWritesDoNotLoseUpdates(t *testing.T) {
	s, _ := openStore(t)

	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := s.AddDevice(fmt.Sprint(i), "ua"); err != nil {
				t.Errorf("AddDevice(%d): %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	if got := len(s.Devices()); got != 20 {
		t.Fatalf("want 20 devices, got %d -- atomic rename prevents torn files, "+
			"not lost read-modify-write updates", got)
	}
}

// A truncating write empties the file before refilling it. Rename swaps a
// finished file in one step, so a reader sees the whole old state or the whole
// new one and never a zero-length or half-written file.
//
// This catches a truncating write only when a read lands inside the window --
// about a third of the time, measured. It never fails on a correct store, and
// TestPersistReplacesTheFileRatherThanRewritingIt pins the same property
// deterministically; this one is here because it states the property in the
// terms that actually matter to a reader of the file.
func TestStoreConcurrentReadersNeverSeeAPartialFile(t *testing.T) {
	s, path := openStore(t)

	// Enough devices that the encoded file is far larger than anything a
	// single write could land atomically by luck.
	for i := range 100 {
		if _, err := s.AddDevice(fmt.Sprintf("seed-%d", i), "ua"); err != nil {
			t.Fatalf("AddDevice: %v", err)
		}
	}

	done := make(chan struct{})
	var readerWG sync.WaitGroup
	readerWG.Add(1)
	go func() {
		defer readerWG.Done()
		for reads := 0; ; reads++ {
			select {
			case <-done:
				if reads == 0 {
					t.Error("reader never observed the file")
				}
				return
			default:
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Errorf("read state file: %v", err)
				return
			}
			var state struct {
				Devices []auth.Device `json:"devices"`
			}
			if err := json.Unmarshal(raw, &state); err != nil {
				t.Errorf("state file was unparseable mid-write (%d bytes): %v -- "+
					"writes must go to a temp file and be renamed into place", len(raw), err)
				return
			}
			if len(state.Devices) < 100 {
				t.Errorf("state file held only %d devices mid-write; a reader must "+
					"never see a partially written store", len(state.Devices))
				return
			}
		}
	}()

	// Stop the reader even if a write fails, or a failing store would hang the
	// test until the package timeout instead of reporting.
	func() {
		defer close(done)
		for i := range 50 {
			if _, err := s.AddDevice(fmt.Sprintf("writer-%d", i), "ua"); err != nil {
				t.Errorf("AddDevice: %v", err)
				return
			}
		}
	}()
	readerWG.Wait()
}

func TestRevokeRemovesOnlyTheNamedDevice(t *testing.T) {
	s, _ := openStore(t)

	keepTok, err := s.AddDevice("keep", "ua")
	if err != nil {
		t.Fatalf("AddDevice: %v", err)
	}
	dropTok, err := s.AddDevice("drop", "ua")
	if err != nil {
		t.Fatalf("AddDevice: %v", err)
	}
	drop, ok := s.Lookup(dropTok)
	if !ok {
		t.Fatal("freshly minted token does not authenticate")
	}

	if err := s.Revoke(drop.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	left := s.Devices()
	if len(left) != 1 || left[0].Name != "keep" {
		t.Fatalf("Revoke removed the wrong device: %+v", left)
	}
	if _, ok := s.Lookup(dropTok); ok {
		t.Fatal("a revoked device's token still authenticates")
	}
	if _, ok := s.Lookup(keepTok); !ok {
		t.Fatal("revoking one device invalidated another's token")
	}
}

func TestRevokeUnknownIDIsAnError(t *testing.T) {
	s, _ := openStore(t)
	if _, err := s.AddDevice("laptop", "ua"); err != nil {
		t.Fatalf("AddDevice: %v", err)
	}

	err := s.Revoke("0123456789abcdef")
	if !errors.Is(err, auth.ErrNoSuchDevice) {
		t.Fatalf("want ErrNoSuchDevice for an unknown id, got %v -- reporting "+
			"success would tell an operator a lost device was cut off when it was not", err)
	}
	if got := len(s.Devices()); got != 1 {
		t.Fatalf("a failed revoke changed the store: %d devices", got)
	}
}

func TestRevokeSurvivesReopen(t *testing.T) {
	s, path := openStore(t)

	tok, err := s.AddDevice("laptop", "ua")
	if err != nil {
		t.Fatalf("AddDevice: %v", err)
	}
	d, _ := s.Lookup(tok)
	if err := s.Revoke(d.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	reopened, err := auth.OpenStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, ok := reopened.Lookup(tok); ok {
		t.Fatal("a revoked device came back after a restart: revocation must be persisted")
	}
}

func TestLookupRejectsUnknownAndEmptyTokens(t *testing.T) {
	s, _ := openStore(t)
	tok, err := s.AddDevice("laptop", "ua")
	if err != nil {
		t.Fatalf("AddDevice: %v", err)
	}

	if _, ok := s.Lookup(tok + "x"); ok {
		t.Fatal("a token with an extra character authenticated")
	}
	if _, ok := s.Lookup(""); ok {
		t.Fatal("an empty token authenticated: a request with no cookie must not pass")
	}
	got, ok := s.Lookup(tok)
	if !ok || got.Name != "laptop" {
		t.Fatalf("Lookup did not return the right device: %+v ok=%v", got, ok)
	}
}

func TestAddDeviceMintsDistinctTokensAndIDs(t *testing.T) {
	s, _ := openStore(t)

	tokens := map[string]bool{}
	for i := range 10 {
		tok, err := s.AddDevice(fmt.Sprintf("d%d", i), "ua")
		if err != nil {
			t.Fatalf("AddDevice: %v", err)
		}
		if tokens[tok] {
			t.Fatal("two devices were minted the same token")
		}
		tokens[tok] = true
	}

	ids := map[string]bool{}
	for _, d := range s.Devices() {
		if ids[d.ID] {
			t.Fatalf("two devices share the id %q", d.ID)
		}
		ids[d.ID] = true
		if d.TokenHash == "" {
			t.Fatalf("device %q has no token hash", d.ID)
		}
	}
	if len(ids) != 10 {
		t.Fatalf("want 10 distinct ids, got %d", len(ids))
	}
}

func TestTouchRecordsLastSeenAndPersists(t *testing.T) {
	s, path := openStore(t)

	tok, err := s.AddDevice("laptop", "ua")
	if err != nil {
		t.Fatalf("AddDevice: %v", err)
	}
	d, _ := s.Lookup(tok)

	seen := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	if err := s.Touch(d.ID, seen); err != nil {
		t.Fatalf("Touch: %v", err)
	}

	reopened, err := auth.OpenStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got := reopened.Devices()[0]
	if !got.LastSeen.Equal(seen) {
		t.Fatalf("last-seen not persisted: want %v, got %v", seen, got.LastSeen)
	}
	if !got.CreatedAt.Equal(d.CreatedAt) {
		t.Fatalf("Touch moved created-at: want %v, got %v", d.CreatedAt, got.CreatedAt)
	}

	if err := s.Touch("0123456789abcdef", seen); !errors.Is(err, auth.ErrNoSuchDevice) {
		t.Fatalf("want ErrNoSuchDevice touching an unknown id, got %v", err)
	}
}

func TestOpenStoreMissingFileIsAnEmptyStore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "devices.json")

	s, err := auth.OpenStore(path)
	if err != nil {
		t.Fatalf("a cold start must not be an error: %v", err)
	}
	if got := len(s.Devices()); got != 0 {
		t.Fatalf("want an empty store, got %d devices", got)
	}
	if _, err := s.AddDevice("laptop", "ua"); err != nil {
		t.Fatalf("AddDevice into a fresh state directory: %v", err)
	}
}

func TestOpenStoreEmptyFileIsAnEmptyStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := auth.OpenStore(path)
	if err != nil {
		t.Fatalf("a zero-length state file holds no devices to lose; "+
			"refusing to start over it only wedges the daemon: %v", err)
	}
	if got := len(s.Devices()); got != 0 {
		t.Fatalf("want an empty store, got %d devices", got)
	}
}

func TestOpenStoreCorruptFileFailsWithoutDestroyingIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	corrupt := []byte(`{"devices": [{"id": "abc", "name": "lap`)
	if err := os.WriteFile(path, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := auth.OpenStore(path); err == nil {
		t.Fatal("a truncated state file must be an error, not an empty store: " +
			"starting empty signs out every device and the next enrolment " +
			"overwrites the only recoverable copy")
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, corrupt) {
		t.Fatalf("a failed open rewrote the state file: %q", raw)
	}
}

func TestOpenStoreUnreadableFileIsAnError(t *testing.T) {
	skipIfRoot(t)
	path := filepath.Join(t.TempDir(), "devices.json")
	if err := os.WriteFile(path, []byte(`{"devices":[]}`), 0o000); err != nil {
		t.Fatal(err)
	}

	if _, err := auth.OpenStore(path); err == nil {
		t.Fatal("an unreadable state file must be an error: the devices it holds " +
			"are still enrolled, so proceeding as if there were none is a lie")
	}
}

func TestWriteFailureLeavesTheStoreUnchanged(t *testing.T) {
	skipIfRoot(t)
	dir := t.TempDir()
	s, err := auth.OpenStore(filepath.Join(dir, "devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	tok, err := s.AddDevice("laptop", "ua")
	if err != nil {
		t.Fatalf("AddDevice: %v", err)
	}
	d, _ := s.Lookup(tok)

	if err := os.Chmod(dir, 0o500); err != nil { // no new temp files
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	if _, err := s.AddDevice("phone", "ua"); err == nil {
		t.Fatal("AddDevice reported success though it could not persist")
	}
	if got := s.Devices(); len(got) != 1 || got[0].ID != d.ID {
		t.Fatalf("a device that could not be persisted is live in memory: %+v", got)
	}

	if err := s.Revoke(d.ID); err == nil {
		t.Fatal("Revoke reported success though it could not persist")
	}
	if _, ok := s.Lookup(tok); !ok {
		t.Fatal("a revoke that failed to persist still cut the device off in " +
			"memory; after a restart it would come back, which is worse than failing")
	}
}

func TestStateFileIsNotWorldReadable(t *testing.T) {
	// A nested path so the directory under test is one OpenStore created, not
	// one the test harness made.
	path := filepath.Join(t.TempDir(), "tmux-web", "devices.json")
	s, err := auth.OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddDevice("laptop", "ua"); err != nil {
		t.Fatalf("AddDevice: %v", err)
	}

	fi, statErr := os.Stat(path)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("state file mode is %04o, want 0600", perm)
	}
	di, statErr := os.Stat(filepath.Dir(path))
	if statErr != nil {
		t.Fatal(statErr)
	}
	if perm := di.Mode().Perm(); perm&0o077 != 0 {
		t.Fatalf("state directory mode is %04o, want no group or other access", perm)
	}
}

func TestPersistLeavesNoTempFilesBehind(t *testing.T) {
	s, path := openStore(t)
	for i := range 3 {
		if _, err := s.AddDevice(fmt.Sprint(i), "ua"); err != nil {
			t.Fatalf("AddDevice: %v", err)
		}
	}

	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(path) {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("state directory holds %v, want only the store file", names)
	}
}

func TestDefaultPath(t *testing.T) {
	t.Run("under XDG_STATE_HOME", func(t *testing.T) {
		t.Setenv("XDG_STATE_HOME", "/xdg/state")
		got, err := auth.DefaultPath()
		if err != nil {
			t.Fatal(err)
		}
		if want := "/xdg/state/tmux-web/devices.json"; got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("falls back to ~/.local/state", func(t *testing.T) {
		t.Setenv("XDG_STATE_HOME", "")
		t.Setenv("HOME", "/home/someone")
		got, err := auth.DefaultPath()
		if err != nil {
			t.Fatal(err)
		}
		if want := "/home/someone/.local/state/tmux-web/devices.json"; got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("ignores a relative XDG_STATE_HOME", func(t *testing.T) {
		t.Setenv("XDG_STATE_HOME", "relative/state")
		t.Setenv("HOME", "/home/someone")
		got, err := auth.DefaultPath()
		if err != nil {
			t.Fatal(err)
		}
		if want := "/home/someone/.local/state/tmux-web/devices.json"; got != want {
			t.Fatalf("the basedir spec says a relative XDG_STATE_HOME is ignored; got %q", got)
		}
	})
}

func skipIfRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("running as root: file permissions are not enforced")
	}
}

func TestDevicesAreListedOldestFirst(t *testing.T) {
	s, _ := openStore(t)

	var want []string
	for i := range 8 {
		name := fmt.Sprintf("device-%d", i)
		if _, err := s.AddDevice(name, "ua"); err != nil {
			t.Fatalf("AddDevice: %v", err)
		}
		want = append(want, name)
	}

	var got []string
	for _, d := range s.Devices() {
		got = append(got, d.Name)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("devices listed as %v, want enrolment order %v -- map iteration "+
			"order would reshuffle the CLI listing on every run", got, want)
	}
}

// The rename is the last step, so it is the one that can fail after a temp file
// already exists. Repeated failures must not litter the state directory.
func TestPersistCleansUpAfterAFailedRename(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "devices.json")
	s, err := auth.OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}

	// A directory cannot be replaced by a rename, so persist fails at the very
	// last step, with the temp file already written.
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}

	if _, err := s.AddDevice("laptop", "ua"); err == nil {
		t.Fatal("AddDevice reported success though the rename could not have worked")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("failed write left temp files behind: %v", names)
	}
}

// The deterministic half of the atomic-write property: a rename swaps in a new
// inode, an in-place write keeps the old one. The concurrent-reader test above
// only catches a truncating write when it happens to read inside the window.
func TestPersistReplacesTheFileRatherThanRewritingIt(t *testing.T) {
	s, path := openStore(t)
	if _, err := s.AddDevice("laptop", "ua"); err != nil {
		t.Fatalf("AddDevice: %v", err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.AddDevice("phone", "ua"); err != nil {
		t.Fatalf("AddDevice: %v", err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	if os.SameFile(before, after) {
		t.Fatal("the store file was written in place: writes must go to a temp " +
			"file renamed over it, or a concurrent reader can see it empty")
	}
}
