package client

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"net/netip"
	"testing"
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
	cert := &x509.Certificate{RawSubjectPublicKeyInfo: []byte("spki")}
	sum := sha256.Sum256([]byte("spki"))
	pin := base64.StdEncoding.EncodeToString(sum[:])
	cs := tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}
	if err := verifyPin(cs, pin); err != nil {
		t.Errorf("matching pin rejected: %v", err)
	}
	if err := verifyPin(cs, "AAAA"); err == nil {
		t.Error("wrong pin accepted")
	}
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
