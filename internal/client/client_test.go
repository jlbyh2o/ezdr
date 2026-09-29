package client

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPVEMajor(t *testing.T) {
	major, err := PVEMajor("pve-manager/9.2.20/49318c671b82f31e (running kernel: 7.0.14-19-pve)")
	if err != nil || major != 9 {
		t.Fatalf("PVEMajor = %d, %v", major, err)
	}
	if _, err := PVEMajor("something else"); err == nil {
		t.Error("unrecognized output accepted")
	}
}

func TestFirstOverlap(t *testing.T) {
	p := netip.MustParsePrefix("100.64.42.0/28")
	ok := []netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/24"),
		netip.MustParsePrefix("0.0.0.0/0"), // default routes are ignored
		netip.MustParsePrefix("100.64.43.0/24"),
	}
	if err := firstOverlap(p, ok); err != nil {
		t.Errorf("unexpected overlap: %v", err)
	}
	for _, bad := range []string{"100.64.0.0/10", "100.64.42.5/32", "100.64.42.0/24"} {
		if err := firstOverlap(p, []netip.Prefix{netip.MustParsePrefix(bad)}); err == nil {
			t.Errorf("%s: overlap not detected", bad)
		}
	}
}

func TestVerifyPin(t *testing.T) {
	ca := newTestCert(t, nil, "EZDR test CA", true)
	leaf := newTestCert(t, ca, "portal.example.com", false)
	other := newTestCert(t, nil, "portal.example.com", false)
	pinOf := func(c *testCert) string {
		sum := sha256.Sum256(c.cert.RawSubjectPublicKeyInfo)
		return base64.StdEncoding.EncodeToString(sum[:])
	}
	state := func(name string, chain ...*testCert) tls.ConnectionState {
		cs := tls.ConnectionState{ServerName: name}
		for _, c := range chain {
			cs.PeerCertificates = append(cs.PeerCertificates, c.cert)
		}
		return cs
	}
	for _, tc := range []struct {
		name string
		cs   tls.ConnectionState
		pin  string
		ok   bool
	}{
		{"pinned leaf", state("portal.example.com", other), pinOf(other), true},
		{"wrong pin", state("portal.example.com", other), "AAAA", false},
		{"pinned CA", state("portal.example.com", leaf, ca), pinOf(ca), true},
		// Anyone can send a copy of the pinned certificate after their own.
		{"pinned leaf after another", state("portal.example.com", other, leaf), pinOf(leaf), false},
		{"pinned CA after another", state("portal.example.com", other, ca), pinOf(ca), false},
		{"pinned CA, other name", state("evil.example.com", leaf, ca), pinOf(ca), false},
	} {
		if err := verifyPin(tc.cs, tc.pin); (err == nil) != tc.ok {
			t.Errorf("%s: verifyPin = %v", tc.name, err)
		}
	}
}

type testCert struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

// newTestCert creates a certificate for name, signed by parent (self-signed if
// nil).
func newTestCert(t *testing.T, parent *testCert, name string, isCA bool) *testCert {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, BasicConstraintsValid: true, IsCA: isCA}
	if !isCA {
		tmpl.DNSNames, tmpl.ExtKeyUsage = []string{name}, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	}
	signer, signerKey := tmpl, key
	if parent != nil {
		signer, signerKey = parent.cert, parent.key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, signer, &key.PublicKey, signerKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &testCert{cert: cert, key: key}
}

func TestConfigRoundTrip(t *testing.T) {
	dir := t.TempDir()
	in := Config{
		HostID: "h1", PortalURL: "https://portal.example.com",
		TunnelAddress:            netip.MustParseAddr("100.64.42.2"),
		TunnelPrefix:             netip.MustParsePrefix("100.64.42.0/28"),
		PortalTunnelAddress:      netip.MustParseAddr("100.64.42.1"),
		PortalWireGuardPublicKey: make([]byte, 32), WireGuardEndpoint: "portal.example.com:51820",
	}
	if err := in.Save(dir); err != nil {
		t.Fatal(err)
	}
	out, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if out.TunnelAddress != in.TunnelAddress || out.TunnelPrefix != in.TunnelPrefix || out.HostID != "h1" {
		t.Fatalf("round trip mismatch: %+v", out)
	}
	if _, err := LoadConfig(t.TempDir()); !errors.Is(err, ErrNotEnrolled) {
		t.Errorf("missing config: err = %v, want ErrNotEnrolled", err)
	}
}

func TestRootOnly(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "ezdr")
	if err := os.WriteFile(bin, nil, 0o755); err != nil { //nolint:gosec // test binary
		t.Fatal(err)
	}
	if os.Getuid() != 0 {
		if err := rootOnly(bin); err == nil {
			t.Error("accepted a binary another user owns")
		}
	}
	if _, err := os.Stat("/usr/bin/env"); err == nil {
		if err := rootOnly("/usr/bin/env"); err != nil {
			t.Errorf("/usr/bin/env: %v", err)
		}
	}
}

func TestPrintable(t *testing.T) {
	var b strings.Builder
	fmt.Fprintf(printable{&b}, "plan %s\n", "Main\x1b]0;evil\x07\u009b2J…")
	if got := b.String(); got != "plan Main]0;evil2J…\n" {
		t.Errorf("printed %q", got)
	}
}
