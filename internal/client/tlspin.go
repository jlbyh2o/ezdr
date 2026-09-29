package client

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// PublicHTTPClient returns an HTTP client for the portal's public endpoint.
// With a pin, only the pinned certificate, or a chain that verifies up to
// it, is accepted; without one, normal certificate verification applies.
func PublicHTTPClient(pin string) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	if pin != "" {
		// Chain verification is replaced by the pin check below, which lets
		// self-signed portals be trusted without a certificate authority.
		tr.TLSClientConfig.InsecureSkipVerify = true //nolint:gosec // verified by VerifyConnection
		tr.TLSClientConfig.VerifyConnection = func(cs tls.ConnectionState) error {
			return verifyPin(cs, pin)
		}
	}
	return &http.Client{Transport: tr, Timeout: 30 * time.Second}
}

var errPinMismatch = errors.New("portal certificate does not match the pin in the enrollment token")

// verifyPin accepts a connection whose leaf certificate has the pinned
// public key, or whose leaf verifies (for the server's name) up to a
// presented certificate with the pinned key. The handshake only proves
// that the server holds the leaf's key: any other certificate it sends
// could be copied from elsewhere, so a pin on one counts only through a
// verified chain.
func verifyPin(cs tls.ConnectionState, pin string) error {
	certs := cs.PeerCertificates
	for i, cert := range certs {
		sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
		if base64.StdEncoding.EncodeToString(sum[:]) != pin {
			continue
		}
		if i == 0 {
			return nil
		}
		roots, intermediates := x509.NewCertPool(), x509.NewCertPool()
		roots.AddCert(cert)
		for _, c := range certs[1:i] {
			intermediates.AddCert(c)
		}
		if _, err := certs[0].Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates, DNSName: cs.ServerName}); err != nil {
			return fmt.Errorf("%w: %w", errPinMismatch, err)
		}
		return nil
	}
	return errPinMismatch
}
