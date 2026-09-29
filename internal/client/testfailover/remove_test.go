package testfailover

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRemoveAll(t *testing.T) {
	pve := t.TempDir()
	write := func(path, data string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(pve, "storage.cfg"), "zfspool: local-zfs\n\tpool rpool/data\n\nzfspool: ezdr-test-tank\n\tpool tank/ezdr-test\n\tsparse 1\n")
	write(filepath.Join(pve, "qemu-server", "10201.conf"), "#EZDR test copy (ezdr-test-id-abc)\nscsi0: ezdr-test-tank:vm-10201-disk-0,size=4G\ntags: ezdr-test\n")
	write(filepath.Join(pve, "qemu-server", "300.conf"), "#my VM\nscsi0: local-zfs:vm-300-disk-0\n")
	write(filepath.Join(pve, "lxc", "101.conf"), "hostname: web\nrootfs: local-zfs:subvol-101-disk-0\n")

	var calls []string
	clones := "tank/ezdr-test\t-\ntank/ezdr-test/vm-10201-disk-0\ttank/r/vm-201-disk-0@zrepl_1\n"
	r := &Runner{PVE: pve, Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
		call := name + " " + strings.Join(args, " ")
		calls = append(calls, call)
		switch call {
		case "zfs list -H -o name -t filesystem,volume -r tank/ezdr-test":
			return []byte("tank/ezdr-test\ntank/ezdr-test/vm-10201-disk-0\n"), nil
		case "zfs list -H -o name,origin -t filesystem,volume -r tank/ezdr-test":
			return []byte(clones), nil
		case "qm status 10201":
			return []byte("status: running\n"), nil
		}
		return nil, nil
	}}
	l, err := r.FindLeftovers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if l.Empty() || len(l.Guests) != 1 || l.Guests[0] != (Leftover{TestID: "abc", Type: "qemu", VMID: 10201}) ||
		len(l.Clones) != 1 || l.Storages["ezdr-test-tank"] != "tank/ezdr-test" {
		t.Fatalf("leftovers = %+v", l)
	}
	done, err := r.RemoveAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"qm stop 10201", "qm destroy 10201 --purge 1 --destroy-unreferenced-disks 1",
		"zfs destroy tank/ezdr-test/vm-10201-disk-0", "pvesm remove ezdr-test-tank", "zfs destroy tank/ezdr-test"}
	got := strings.Join(calls, "\n")
	for _, w := range want {
		if !strings.Contains(got, w+"\n") && !strings.HasSuffix(got, w) {
			t.Errorf("missing %q in:\n%s", w, got)
		}
	}
	if strings.Contains(got, " 300") || strings.Contains(got, " 101") || len(done) != 4 {
		t.Errorf("done = %q, calls:\n%s", done, got)
	}

	// A dataset in the test storage that isn't a clone is left alone.
	calls = nil
	clones = "tank/ezdr-test\t-\ntank/ezdr-test/mine\t-\n"
	if _, err := r.RemoveAll(context.Background()); err == nil || !strings.Contains(err.Error(), "isn't a clone") {
		t.Errorf("non-clone: %v", err)
	}
}
