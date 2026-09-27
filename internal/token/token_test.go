package token

import (
	"bytes"
	"strings"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	in := Token{PortalURL: "https://portal.example.com", ID: "abc", Secret: NewSecret(), TLSPin: "pin"}
	s := in.Encode()
	if !strings.HasPrefix(s, Prefix) {
		t.Fatalf("missing prefix: %q", s)
	}
	out, err := Decode("  " + s + "\n")
	if err != nil {
		t.Fatal(err)
	}
	if out.PortalURL != in.PortalURL || out.ID != in.ID || !bytes.Equal(out.Secret, in.Secret) || out.TLSPin != in.TLSPin {
		t.Fatalf("round trip mismatch: %+v != %+v", out, in)
	}
}

func TestDecodeRejects(t *testing.T) {
	good := Token{PortalURL: "https://portal.example.com", ID: "abc", Secret: NewSecret()}
	httpURL := good
	httpURL.PortalURL = "http://portal.example.com"
	short := good
	short.Secret = []byte("short")
	noID := good
	noID.ID = ""

	for name, s := range map[string]string{
		"wrong prefix": "ezdr2_abc",
		"bad base64":   Prefix + "!!!",
		"bad json":     Prefix + "bm90IGpzb24",
		"plain http":   httpURL.Encode(),
		"short secret": short.Encode(),
		"missing id":   noID.Encode(),
	} {
		if _, err := Decode(s); err == nil {
			t.Errorf("%s: Decode succeeded", name)
		}
	}
}
