package failover

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fake struct {
	calls   []string
	running map[string]bool
}

func (f *fake) run(_ context.Context, name string, args ...string) ([]byte, error) {
	call := strings.Join(append([]string{name}, args...), " ")
	f.calls = append(f.calls, call)
	if args[0] == "status" {
		if f.running[name+" "+args[1]] {
			return []byte("status: running\n"), nil
		}
		return []byte("status: stopped\n"), nil
	}
	return nil, nil
}

func testRunner(t *testing.T) (*Runner, *fake) {
	t.Helper()
	pve := t.TempDir()
	for _, d := range []string{"qemu-server", "lxc"} {
		if err := os.MkdirAll(filepath.Join(pve, d), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	f := &fake{running: map[string]bool{}}
	return &Runner{PVE: pve, Run: f.run}, f
}

func write(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestStopAndLock(t *testing.T) {
	r, f := testRunner(t)
	write(t, r.configPath("qemu", 201), "name: app\nonboot: 1\ntags: web\n")
	write(t, r.configPath("lxc", 101), "hostname: web\n")
	write(t, r.configPath("lxc", 102), "hostname: db\nlock: migrate\ntags: ezdr-failed-over\n")
	f.running["qm 201"] = true

	events, err := r.StopAndLock(context.Background(), []uint32{201, 101, 102, 999}, 300)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"qm status 201",
		"qm shutdown 201 --timeout 300 --forceStop 1 --skiplock 1",
		"qm set 201 --onboot 0 --tags web;ezdr-failed-over --skiplock 1",
		"qm set 201 --lock migrate --skiplock 1",
		"pct status 101",
		"pct set 101 --onboot 0 --tags ezdr-failed-over",
		"pct set 101 --lock migrate",
	}
	if strings.Join(f.calls, "\n") != strings.Join(want, "\n") {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(f.calls, "\n"), strings.Join(want, "\n"))
	}
	if strings.Join(events, "|") != "stopped guest 201|locked guest 201|locked guest 101" {
		t.Errorf("events = %v", events)
	}
}
