package portal

import "testing"

func TestSelfSignedIsStable(t *testing.T) {
	dir := t.TempDir()
	_, pin1, err := LoadOrCreateSelfSigned(dir, "portal.example.com")
	if err != nil {
		t.Fatal(err)
	}
	_, pin2, err := LoadOrCreateSelfSigned(dir, "portal.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if pin1 == "" || pin1 != pin2 {
		t.Fatalf("pin changed between loads: %q != %q", pin1, pin2)
	}
}
