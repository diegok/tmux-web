package front

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// selfCertLifetime is deliberately long. A regenerated certificate is a fresh
// browser warning on every device, so the renewal is the expensive event here,
// not the expiry.
const selfCertLifetime = 10 * 365 * 24 * time.Hour

// SelfSignedCert returns paths to a certificate and key for host, generating
// them on first use and reusing them afterwards.
//
// Reuse is the whole point: browsers remember an accepted exception per
// certificate, so regenerating on every start would ask again on every device
// every time the daemon restarts. The pair is written next to the device store,
// with the key 0600.
//
// This exists for an invented internal hostname -- devbox.ss, something on a
// VPN -- where ACME cannot work because no public CA will ever issue for a name
// it cannot verify. It is not a downgrade from ACME for a real hostname; it is
// the only option for a name that only your network knows.
func SelfSignedCert(dir, host string) (certPath, keyPath string, err error) {
	certPath = filepath.Join(dir, "self-signed.crt")
	keyPath = filepath.Join(dir, "self-signed.key")

	if reusable(certPath, keyPath, host) {
		return certPath, keyPath, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", err
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return "", "", err
	}

	now := time.Now()
	tmpl := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host, Organization: []string{"wterm-web"}},
		// Backdated an hour so a client whose clock runs slow does not reject a
		// certificate that was valid the moment it was written.
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(selfCertLifetime),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	// A modern browser ignores CommonName entirely and matches on SAN, so the
	// name has to appear here or every request fails with a name mismatch.
	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{host}
	}

	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return "", "", err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return "", "", err
	}

	if err := writePEM(certPath, "CERTIFICATE", der, 0o644); err != nil {
		return "", "", err
	}
	if err := writePEM(keyPath, "EC PRIVATE KEY", keyDER, 0o600); err != nil {
		return "", "", err
	}
	return certPath, keyPath, nil
}

// reusable reports whether an existing pair can be kept.
//
// A certificate for the wrong host, or one close to expiry, must be replaced --
// but anything else is kept, because replacing it costs a browser warning on
// every enrolled device.
func reusable(certPath, keyPath, host string) bool {
	if _, err := os.Stat(keyPath); err != nil {
		return false
	}
	pemBytes, err := os.ReadFile(certPath)
	if err != nil {
		return false
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return false
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return false
	}
	if time.Now().After(cert.NotAfter.Add(-30 * 24 * time.Hour)) {
		return false
	}
	return cert.VerifyHostname(host) == nil
}

func writePEM(path, blockType string, der []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := pem.Encode(f, &pem.Block{Type: blockType, Bytes: der}); err != nil {
		return err
	}
	return f.Close()
}

// CertFingerprint is the SHA-256 of the certificate, formatted the way browsers
// show it.
//
// Printed at startup so the exception you accept in the browser can be checked
// against the one the daemon actually served, rather than accepted blind. That
// check is the only thing standing between a self-signed setup and someone
// else's certificate.
func CertFingerprint(certPath string) (string, error) {
	pemBytes, err := os.ReadFile(certPath)
	if err != nil {
		return "", err
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return "", fmt.Errorf("front: %s is not PEM", certPath)
	}
	sum := sha256.Sum256(block.Bytes)
	hexed := strings.ToUpper(hex.EncodeToString(sum[:]))
	var out strings.Builder
	for i := 0; i < len(hexed); i += 2 {
		if i > 0 {
			out.WriteByte(':')
		}
		out.WriteString(hexed[i : i+2])
	}
	return out.String(), nil
}
