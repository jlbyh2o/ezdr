package zrepl

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"time"
)

// EnsureCertificate returns this host's zrepl TLS certificate (PEM),
// creating a key pair and long-lived self-signed certificate named name if
// none exists. The certificate is a leaf, not a certificate authority; peers
// trust exactly this certificate.
func EnsureCertificate(p Paths, name string) (string, error) {
	if b, err := os.ReadFile(p.CertFile()); err == nil {
		return string(b), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return "", err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: name},
		DNSNames:              []string{name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(20, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return "", err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(p.CertDir, 0o700); err != nil {
		return "", err
	}
	if err := writeFile(p.KeyFile(), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return "", err
	}
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := writeFile(p.CertFile(), cert, 0o644); err != nil {
		return "", err
	}
	return string(cert), nil
}

// checkCertificate verifies that pemText is a single certificate for name.
func checkCertificate(pemText, name string) error {
	block, rest := pem.Decode([]byte(pemText))
	if block == nil || block.Type != "CERTIFICATE" || hasPEM(rest) {
		return fmt.Errorf("peer %s: not a single PEM certificate", name)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return fmt.Errorf("peer %s: %w", name, err)
	}
	if cert.Subject.CommonName != name {
		return fmt.Errorf("peer certificate is for %q, expected %q", cert.Subject.CommonName, name)
	}
	return nil
}

func hasPEM(b []byte) bool {
	block, _ := pem.Decode(b)
	return block != nil
}

// writeFile writes data atomically with the given permissions. Paths are
// fixed or built from validated peer names.
func writeFile(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil { //nolint:gosec // see above
		return err
	}
	if err := os.Chmod(tmp, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
