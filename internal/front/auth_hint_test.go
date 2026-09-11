package front_test

import (
	"net/http"
	"strings"
	"testing"
)

// With no passwords and no sign-in form, an unenrolled browser has no way to
// discover that the fix is a command on the box. The 401 body is the only
// place that can say so.
func TestUnenrolledBrowserIsToldHowToEnroll(t *testing.T) {
	f := newFixture(t)

	rec := f.do("GET", "/", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET / unauthenticated = %d, want 401", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "tmux-web enroll") {
		t.Errorf("the 401 a new owner sees must name the enroll command, got %q", body)
	}
}

// The hint is for a person reading a page. Machine callers get the terse form,
// and the SPA renders its own message rather than parsing prose.
func TestApiAndSocketRefusalsStayTerse(t *testing.T) {
	f := newFixture(t)

	for _, target := range []string{"/api/snapshot", "/api/devices"} {
		rec := f.do("GET", target, "")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("GET %s = %d, want 401", target, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "tmux-web enroll") {
			t.Errorf("%s should not answer a machine caller with prose: %q", target, rec.Body.String())
		}
	}
}

// An unauthenticated reader learns the command and nothing else: not whether
// any device is enrolled, not whether theirs was revoked, not the socket path.
func TestTheHintDoesNotDescribeTheSystem(t *testing.T) {
	f := newFixture(t)
	body := f.do("GET", "/", "").Body.String()

	for _, leak := range []string{".sock", "XDG_RUNTIME_DIR", "devices.json", "revoked"} {
		if strings.Contains(body, leak) {
			t.Errorf("the unauthenticated 401 leaks %q: %s", leak, body)
		}
	}
}
