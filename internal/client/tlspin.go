package client

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"net/http"
	"time"
)

// PublicHTTPClient returns an HTTP client for the portal's public endpoint.
// With a pin, only certificate chains containing the pinned public key are
// accepted; without one, normal certificate verification applies.
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

func verifyPin(cs tls.ConnectionState, pin string) error {
	for _, cert := range cs.PeerCertificates {
		sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
		if base64.StdEncoding.EncodeToString(sum[:]) == pin {
			return nil
		}
	}
	return errors.New("portal certificate does not match the pin in the enrollment token")
}
