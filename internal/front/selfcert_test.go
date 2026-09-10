package front_test

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"github.com/diegok/tmux-web/internal/front"
)

func parseCert(t *testing.T, path string) *x509.Certificate {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		t.Fatalf("%s is not PEM", path)
	}
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestSelfSignedCertMatchesTheHost(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath, err := front.SelfSignedCert(dir, "devbox.ss")
	if err != nil {
		t.Fatal(err)
	}

	cert := parseCert(t, certPath)
	// Browsers ignore CommonName entirely, so the name has to be a SAN or
	// every request fails with a name mismatch.
	if err := cert.VerifyHostname("devbox.ss"); err != nil {
		t.Fatalf("certificate does not match its own host: %v", err)
	}
	if err := cert.VerifyHostname("someone-else.ss"); err == nil {
		t.Fatal("certificate matches a host it was not issued for")
	}

	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("private key mode %v, want 0600", info.Mode().Perm())
	}
}

// The reason reuse matters is not disk space: browsers remember an accepted
// exception per certificate, so regenerating would re-warn on every enrolled
// device every time the daemon restarts.
func TestSelfSignedCertIsReusedAcrossRestarts(t *testing.T) {
	dir := t.TempDir()
	certPath, _, err := front.SelfSignedCert(dir, "devbox.ss")
	if err != nil {
		t.Fatal(err)
	}
	first := parseCert(t, certPath).SerialNumber.String()

	for i := 0; i < 3; i++ {
		if _, _, err := front.SelfSignedCert(dir, "devbox.ss"); err != nil {
			t.Fatal(err)
		}
	}
	if got := parseCert(t, certPath).SerialNumber.String(); got != first {
		t.Fatalf("certificate was regenerated (%s -> %s); every device would warn again", first, got)
	}
}

func TestSelfSignedCertIsReplacedWhenTheHostChanges(t *testing.T) {
	dir := t.TempDir()
	certPath, _, err := front.SelfSignedCert(dir, "devbox.ss")
	if err != nil {
		t.Fatal(err)
	}
	first := parseCert(t, certPath).SerialNumber.String()

	if _, _, err := front.SelfSignedCert(dir, "other.ss"); err != nil {
		t.Fatal(err)
	}
	cert := parseCert(t, certPath)
	if cert.SerialNumber.String() == first {
		t.Fatal("kept a certificate issued for a different host: TLS would fail on every request")
	}
	if err := cert.VerifyHostname("other.ss"); err != nil {
		t.Fatalf("replacement does not match the new host: %v", err)
	}
}

func TestSelfSignedCertReplacesUnusableFiles(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath, err := front.SelfSignedCert(dir, "devbox.ss")
	if err != nil {
		t.Fatal(err)
	}

	// A truncated or corrupt certificate must not be served forever.
	if err := os.WriteFile(certPath, []byte("not a certificate"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := front.SelfSignedCert(dir, "devbox.ss"); err != nil {
		t.Fatal(err)
	}
	if err := parseCert(t, certPath).VerifyHostname("devbox.ss"); err != nil {
		t.Fatalf("corrupt certificate was not replaced: %v", err)
	}

	// A missing key is equally unusable, even with a valid certificate.
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	if _, _, err := front.SelfSignedCert(dir, "devbox.ss"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("key was not regenerated: %v", err)
	}
}

// The fingerprint is what lets the exception accepted in a browser be checked
// against the certificate the daemon actually served.
func TestCertFingerprintMatchesTheBrowserFormat(t *testing.T) {
	dir := t.TempDir()
	certPath, _, err := front.SelfSignedCert(dir, "devbox.ss")
	if err != nil {
		t.Fatal(err)
	}
	fp, err := front.CertFingerprint(certPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(fp) != 95 { // 32 bytes as AA:BB:...
		t.Fatalf("fingerprint %q is not 32 colon-separated bytes", fp)
	}
	if _, err := front.CertFingerprint(filepath.Join(dir, "nope.crt")); err == nil {
		t.Fatal("missing file should be an error, not an empty fingerprint")
	}
}
