package front_test

import (
	"net/http"
	"os"
	"os/user"
	"testing"
)

// The sidebar footer answers "which box am I driving", so the identity route
// must report the uid the daemon actually runs as -- a value no browser could
// have supplied -- and must name the caller's own device rather than any
// enrolled one.
func TestIdentityReportsTheServiceUserAndTheCallersDevice(t *testing.T) {
	f := newFixture(t)

	rec := f.ok("GET", "/api/user", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/user = %d (%s), want 200", rec.Code, rec.Body.String())
	}
	var got struct {
		User     string `json:"user"`
		Host     string `json:"host"`
		Device   string `json:"device"`
		DeviceID string `json:"deviceId"`
	}
	decode(t, rec, &got)

	wantUser := "unknown"
	if u, err := user.Current(); err == nil && u.Username != "" {
		wantUser = u.Username
	}
	wantHost := "unknown"
	if h, err := os.Hostname(); err == nil && h != "" {
		wantHost = h
	}
	if got.User != wantUser || got.Host != wantHost {
		t.Errorf("identity = %q@%q, want %q@%q", got.User, got.Host, wantUser, wantHost)
	}

	// The device half comes from the request context, so it is the caller's.
	devices := f.store.Devices()
	if len(devices) == 0 {
		t.Fatal("fixture has no enrolled device")
	}
	if got.DeviceID != devices[0].ID || got.Device != devices[0].Name {
		t.Errorf("device = %q/%q, want %q/%q",
			got.Device, got.DeviceID, devices[0].Name, devices[0].ID)
	}
}

func TestIdentityRequiresADevice(t *testing.T) {
	f := newFixture(t)
	if code := f.refused("GET", "/api/user", ""); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET /api/user = %d, want 401", code)
	}
}
