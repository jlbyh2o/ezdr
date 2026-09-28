package replication

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	clientv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/client/v1"
	inventoryv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/inventory/v1"
)

// failbackFixture returns a failed-over plan's inventories: the DR host runs
// copies of the primary's guests from the plan's storage.
func failbackFixture() (Plan, *inventoryv1.Inventory, *inventoryv1.Inventory) {
	p, hosts := fixture()
	pInv := hosts["p1"].Inventory
	for _, g := range pInv.Guests {
		g.Cores, g.MemoryBytes = 2, 2<<30
		g.Disks[0].SizeBytes = 8 << 30
		g.Nics = []*inventoryv1.Nic{{Key: "net0", Mac: "BC:24:11:00:00:01", Model: "veth"}}
	}
	dInv := &inventoryv1.Inventory{}
	for _, g := range pInv.Guests {
		c := proto.Clone(g).(*inventoryv1.Guest)
		c.Disks[0].Storage = "ezdr-abcdefgh-local-zfs"
		dInv.Guests = append(dInv.Guests, c)
	}
	return p, pInv, dInv
}

func withWritten(s []*clientv1.SnapshotInfo, written ...uint64) []*clientv1.SnapshotInfo {
	for i, w := range written {
		s[i].WrittenBytes = w
	}
	return s
}

func TestFailbackPreflight(t *testing.T) {
	p, pInv, dInv := failbackFixture()
	pri := &clientv1.ZreplPreflightResult{Datasets: []*clientv1.DatasetSnapshots{
		// A planned failover: the primary stopped at the final snapshot.
		{Dataset: "rpool/subvol-101-disk-0", Exists: true, Snapshots: snaps("@zrepl_1", uint64(1), "#zrepl_CURSOR_2", uint64(2), "@zrepl_2", uint64(2))},
		// An unplanned failover: the primary took another snapshot and kept
		// writing after the DR host's newest one.
		{Dataset: "rpool/subvol-102-disk-0", Exists: true, WrittenBytes: 100,
			Snapshots: withWritten(snaps("@zrepl_1", uint64(11), "@zrepl_2", uint64(12)), 0, 1000)},
	}}
	dr := &clientv1.ZreplPreflightResult{Datasets: []*clientv1.DatasetSnapshots{
		{Dataset: "tank/replicated/rpool/subvol-101-disk-0", Exists: true, WrittenBytes: 5000,
			Snapshots: snaps("@zrepl_1", uint64(1), "@zrepl_2", uint64(2))},
		{Dataset: "tank/replicated/rpool/subvol-102-disk-0", Exists: true, WrittenBytes: 7,
			Snapshots: withWritten(snaps("@zrepl_1", uint64(11), "@failback_1", uint64(99)), 0, 300)},
	}}
	r := FailbackPreflight(p.Spec, pInv, dInv, "ezdr-abcdefgh-", pri, dr)
	if len(r.Problems) != 0 || len(r.Warnings) != 0 || len(r.ConfigChanges) != 0 {
		t.Fatalf("problems %v, warnings %v, changes %v", r.Problems, r.Warnings, r.ConfigChanges)
	}
	if len(r.Datasets) != 2 || !r.Diverged {
		t.Fatalf("preflight = %v", r)
	}
	d := r.Datasets[0]
	if d.CommonSnapshot != "@zrepl_2" || d.CopyBytes != 5000 || d.DivergedBytes != 0 || len(d.DiscardedSnapshots) != 0 ||
		d.Replica != "tank/replicated/rpool/subvol-101-disk-0" {
		t.Errorf("dataset 101 = %v", d)
	}
	d = r.Datasets[1]
	if d.CommonSnapshot != "@zrepl_1" || d.CopyBytes != 307 || d.DivergedBytes != 1100 ||
		len(d.DiscardedSnapshots) != 1 || d.DiscardedSnapshots[0] != "@zrepl_2" {
		t.Errorf("dataset 102 = %v", d)
	}
}

func TestFailbackPreflightProblems(t *testing.T) {
	p, pInv, dInv := failbackFixture()
	// A disk added at the DR site, other changes, and a guest outside the
	// plan on the plan's storage.
	g := dInv.Guests[0]
	g.Disks = append(g.Disks, &inventoryv1.Disk{Key: "mp0", Storage: "ezdr-abcdefgh-local-zfs"})
	g.Disks[0].SizeBytes = 16 << 30
	g.Cores, g.MemoryBytes = 4, 4<<30
	g.Nics = append(g.Nics, &inventoryv1.Nic{Key: "net1"})
	dInv.Guests[1].Nics[0].Mac = "BC:24:11:00:00:99"
	dInv.Guests = append(dInv.Guests, &inventoryv1.Guest{Vmid: 900, Disks: []*inventoryv1.Disk{{Key: "rootfs", Storage: "ezdr-abcdefgh-local-zfs"}}})
	pri := &clientv1.ZreplPreflightResult{Datasets: []*clientv1.DatasetSnapshots{
		// Only a bookmark is left of the replica's newest snapshot.
		{Dataset: "rpool/subvol-101-disk-0", Exists: true, Snapshots: snaps("#zrepl_1", uint64(1))},
		{Dataset: "rpool/subvol-102-disk-0"},
	}}
	dr := &clientv1.ZreplPreflightResult{Datasets: []*clientv1.DatasetSnapshots{
		{Dataset: "tank/replicated/rpool/subvol-101-disk-0", Exists: true, Snapshots: snaps("@zrepl_1", uint64(1))},
		{Dataset: "tank/replicated/rpool/subvol-102-disk-0", Exists: true},
	}}
	r := FailbackPreflight(p.Spec, pInv, dInv, "ezdr-abcdefgh-", pri, dr)
	for _, c := range []struct {
		name string
		got  []string
		want []string
	}{
		{"problems", r.Problems, []string{
			"rpool/subvol-101-disk-0 has no snapshot in common",
			"rpool/subvol-102-disk-0 no longer exists on the primary",
			"guest 101 has a disk added at the DR site (mp0)",
		}},
		{"changes", r.ConfigChanges, []string{
			"guest 101: disk rootfs was resized at the DR site (8.0 GiB to 16.0 GiB)",
			"guest 101: cores changed at the DR site (2 to 4)",
			"guest 101: memory changed at the DR site (2.0 GiB to 4.0 GiB)",
			"guest 101: network device net1 was added",
			"guest 102: network device net0 changed",
		}},
		{"warnings", r.Warnings, []string{"guest 900 on the DR host isn't in the plan"}},
	} {
		all := strings.Join(c.got, "\n")
		for _, w := range c.want {
			if !strings.Contains(all, w) {
				t.Errorf("missing %s %q in:\n%s", c.name, w, all)
			}
		}
		if len(c.got) != len(c.want) {
			t.Errorf("%s = %d, want %d:\n%s", c.name, len(c.got), len(c.want), all)
		}
	}
}

func TestFailbackAddresses(t *testing.T) {
	p, hosts := fixture()
	listen, connect, freebind, err := FailbackAddresses(p, hosts["p1"], hosts["d1"])
	if err != nil || listen != ":8888" || connect != "192.0.2.12:8888" || freebind {
		t.Errorf("addresses = %q, %q, %v, %v", listen, connect, freebind, err)
	}
	p.Spec.Network = nil
	if _, _, _, err := FailbackAddresses(p, hosts["p1"], hosts["d1"]); err == nil {
		t.Error("no error without a network")
	}
}
