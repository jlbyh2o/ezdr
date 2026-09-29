package failover

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCleanup(t *testing.T) {
	ctx := context.Background()
	r, f := drRunner(t)
	if _, err := r.Prepare(ctx, "plan1", []uint32{201, 101}); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(r.PVE, "storage.cfg"), "dir: local\n\tpath /var/lib/vz\n\nzfspool: ezdr-plan1-local-zfs\n\tpool tank/ezdr/pve1/rpool/data\n")

	// A running guest isn't removed.
	f.running["qm 201"] = true
	if _, err := r.Cleanup(ctx, "plan1", []uint32{201, 101}, "zrepl_9"); err == nil || !strings.Contains(err.Error(), "still running") {
		t.Fatalf("err = %v", err)
	}
	f.running["qm 201"] = false

	// Another guest on the plan's storage keeps it.
	write(t, r.configPath("qemu", 300), "scsi0: ezdr-plan1-local-zfs:vm-300-disk-0,size=1G\n")
	f.calls = nil
	notes, err := r.Cleanup(ctx, "plan1", []uint32{201, 101}, "zrepl_9")
	if err != nil {
		t.Fatal(err)
	}
	for _, typ := range []string{"qemu", "lxc"} {
		for _, id := range []uint32{201, 101} {
			if _, err := os.Stat(r.configPath(typ, id)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("%s %d still registered", typ, id)
			}
		}
	}
	if got := strings.Join(notes, "|"); !strings.Contains(got, "kept storage ezdr-plan1-local-zfs: guest(s) 300 use it") {
		t.Errorf("notes = %s", got)
	}
	calls := strings.Join(f.calls, "\n")
	for _, want := range []string{
		"zfs rollback tank/ezdr/pve1/rpool/data/vm-201-disk-0@zrepl_9",
		"zfs set readonly=on tank/ezdr/pve1/rpool/data/vm-201-disk-0",
		"zfs set readonly=on tank/ezdr/pve1/rpool/data/subvol-101-disk-0",
	} {
		if !strings.Contains(calls, want) {
			t.Errorf("missing %q in:\n%s", want, calls)
		}
	}
	for _, bad := range []string{"destroy", "pvesm remove"} {
		if strings.Contains(calls, bad) {
			t.Errorf("unexpected %q in:\n%s", bad, calls)
		}
	}

	// Once the storage is free, a repeat removes it.
	if err := os.Remove(r.configPath("qemu", 300)); err != nil {
		t.Fatal(err)
	}
	f.calls = nil
	if _, err := r.Cleanup(ctx, "plan1", []uint32{201, 101}, "zrepl_9"); err != nil {
		t.Fatal(err)
	}
	if calls := strings.Join(f.calls, "\n"); !strings.Contains(calls, "pvesm remove ezdr-plan1-local-zfs") {
		t.Errorf("calls = %s", calls)
	}
}

func TestCleanupRefusesOtherGuests(t *testing.T) {
	r, _ := drRunner(t)
	// Guest 201 exists but wasn't registered by the plan's failover.
	write(t, r.configPath("qemu", 201), "name: other\nscsi0: local:vm-201-disk-0\n")
	if _, err := r.Cleanup(context.Background(), "plan1", []uint32{201}, "zrepl_9"); err == nil ||
		!strings.Contains(err.Error(), "wasn't registered by this plan's failover") {
		t.Errorf("err = %v", err)
	}
	if _, err := os.Stat(r.configPath("qemu", 201)); err != nil {
		t.Errorf("removed someone else's guest: %v", err)
	}
}

func TestCleanupRemovesCreatedCloudInit(t *testing.T) {
	ctx := context.Background()
	r, f := drRunner(t)
	if _, err := r.Prepare(ctx, "plan1", []uint32{201}); err != nil {
		t.Fatal(err)
	}
	// The failover created a fresh drive: it goes.
	drive := "tank/ezdr/pve1/rpool/data/vm-201-cloudinit"
	f.datasets[drive] = true
	f.calls = nil
	notes, err := r.Cleanup(ctx, "plan1", []uint32{201}, "zrepl_9")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(f.calls, "\n"), "zfs destroy "+drive) ||
		!strings.Contains(strings.Join(notes, "|"), "removed guest 201's cloud-init drive "+drive) {
		t.Errorf("calls = %q, notes = %q", f.calls, notes)
	}

	// A reused replica of the drive (with snapshots) is kept.
	f.snapshots = map[string]string{drive: drive + "@zrepl_1\n"}
	f.calls = nil
	if _, err := r.Cleanup(ctx, "plan1", []uint32{201}, "zrepl_9"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(f.calls, "\n"), "destroy") {
		t.Errorf("destroyed a reused replica: %q", f.calls)
	}
}
