package client

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testBootGuard(t *testing.T) bootGuard {
	t.Helper()
	dir := t.TempDir()
	return bootGuard{dropIn: filepath.Join(dir, "pve-guests.service.d", "ezdr-boot-guard.conf"),
		state: filepath.Join(dir, "lib", "boot-guard.json"), ready: filepath.Join(dir, "run", "boot-ready")}
}

func TestBootGuardDropIn(t *testing.T) {
	g := testBootGuard(t)
	if changed, err := g.install("/usr/bin/ezdr"); err != nil || !changed {
		t.Fatalf("install: %v, %v", changed, err)
	}
	b, _ := os.ReadFile(g.dropIn)
	if !strings.Contains(string(b), "ExecStartPre=-/usr/bin/ezdr boot-guard\n") {
		t.Errorf("drop-in:\n%s", b)
	}
	if changed, err := g.install("/usr/bin/ezdr"); err != nil || changed {
		t.Errorf("second install: %v, %v", changed, err)
	}
	if removed, err := g.remove(); err != nil || !removed {
		t.Errorf("remove: %v, %v", removed, err)
	}
	if _, err := os.Stat(filepath.Dir(g.dropIn)); !os.IsNotExist(err) {
		t.Error("empty drop-in directory left behind")
	}
	if removed, err := g.remove(); err != nil || removed {
		t.Errorf("second remove: %v, %v", removed, err)
	}
}

func TestBootGuardWait(t *testing.T) {
	g := testBootGuard(t)
	wait := func() (time.Duration, string) {
		var out strings.Builder
		start := time.Now()
		g.wait(context.Background(), &out, 200*time.Millisecond, 10*time.Millisecond)
		return time.Since(start), out.String()
	}
	// Hosts that aren't a plan's primary (or haven't heard yet) don't wait.
	if d, _ := wait(); d > 100*time.Millisecond {
		t.Errorf("no state: waited %s", d)
	}
	if err := g.setPrimary(false); err != nil {
		t.Fatal(err)
	}
	if d, _ := wait(); d > 100*time.Millisecond {
		t.Errorf("not a primary: waited %s", d)
	}
	// A primary waits for this boot's locks, up to the timeout.
	if err := g.setPrimary(true); err != nil {
		t.Fatal(err)
	}
	if d, out := wait(); d < 200*time.Millisecond || !strings.Contains(out, "starting guests as usual") {
		t.Errorf("timeout: waited %s, %q", d, out)
	}
	if err := g.markReady(); err != nil {
		t.Fatal(err)
	}
	if d, out := wait(); d > 100*time.Millisecond || !strings.Contains(out, "applied the portal's locks") {
		t.Errorf("ready: waited %s, %q", d, out)
	}
}
