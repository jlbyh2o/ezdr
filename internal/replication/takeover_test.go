package replication

import (
	"strings"
	"testing"

	clientv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/client/v1"
	inventoryv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/inventory/v1"
	"github.com/jlbyh2o/ezdr/internal/plan"
)

func snaps(pairs ...any) []*clientv1.SnapshotInfo {
	var out []*clientv1.SnapshotInfo
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, &clientv1.SnapshotInfo{Name: pairs[i].(string), Guid: pairs[i+1].(uint64), Createtxg: uint64(len(out))})
	}
	return out
}

func takeoverFixture() (Plan, *inventoryv1.Inventory, *inventoryv1.Inventory, plan.ZreplSetup) {
	p, hosts := fixture()
	pInv, dInv := hosts["p1"].Inventory, hosts["d1"].Inventory
	pInv.ZfsDatasets = append(pInv.ZfsDatasets, &inventoryv1.ZfsDataset{Name: "rpool"})
	src := &inventoryv1.ZreplJob{Name: "old_source", Type: "source", ListenAddress: ":8888",
		Filesystems: []*inventoryv1.ZreplFilter{{Pattern: "rpool<", Include: true}}}
	pull := &inventoryv1.ZreplJob{Name: "old_pull", Type: "pull", ConnectAddress: "192.0.2.12:8888", RootFs: "tank/replicated"}
	pInv.Zrepl = &inventoryv1.Zrepl{Jobs: []*inventoryv1.ZreplJob{src, {Name: "hourly_snap", Type: "snap"}}}
	dInv.Zrepl = &inventoryv1.Zrepl{Jobs: []*inventoryv1.ZreplJob{pull}}
	return p, pInv, dInv, plan.ZreplSetup{Source: src, Pull: pull}
}

func TestPreflight(t *testing.T) {
	p, pInv, dInv, setup := takeoverFixture()
	pri := &clientv1.ZreplPreflightResult{ZreplVersion: "v0.6.1", ZreplRunning: true, ReleasePreview: []string{"would destroy a"},
		Datasets: []*clientv1.DatasetSnapshots{
			// Only a bookmark is left of the newest common snapshot.
			{Dataset: "rpool/subvol-101-disk-0", Exists: true, Snapshots: snaps("@zrepl_1", uint64(1), "#zrepl_CURSOR_2", uint64(2), "@zrepl_3", uint64(3))},
			{Dataset: "rpool/subvol-102-disk-0", Exists: true, ReferencedBytes: 5000, Snapshots: snaps("@zrepl_1", uint64(11))},
		}}
	dr := &clientv1.ZreplPreflightResult{ZreplVersion: "v0.7.0", ZreplRunning: true, Datasets: []*clientv1.DatasetSnapshots{
		{Dataset: "tank/replicated/rpool/subvol-101-disk-0", Exists: true, Snapshots: snaps("@zrepl_1", uint64(1), "@zrepl_2", uint64(2))},
		{Dataset: "tank/replicated/rpool/subvol-102-disk-0"},
	}}
	r := Preflight(p.Spec, pInv, dInv, setup, pri, dr)
	if len(r.Problems) != 0 {
		t.Fatalf("problems: %v", r.Problems)
	}
	if len(r.Datasets) != 2 || r.Datasets[0].CommonSnapshot != "#zrepl_CURSOR_2" || r.Datasets[0].FullSend ||
		!r.Datasets[1].FullSend || r.Datasets[1].FullSendBytes != 5000 {
		t.Errorf("datasets = %v", r.Datasets)
	}
	if !r.UpgradePrimary || r.UpgradeDr || len(r.PrimaryReleases) != 1 {
		t.Errorf("upgrade/releases = %v", r)
	}
	if len(r.DroppedDatasets) != 1 || r.DroppedDatasets[0] != "rpool" {
		t.Errorf("dropped = %v", r.DroppedDatasets)
	}
	if len(r.OtherJobs) != 1 || r.OtherJobs[0] != "primary: hourly_snap (snap)" ||
		!strings.Contains(strings.Join(r.Warnings, "\n"), "run on zrepl 0.7 after the upgrade") {
		t.Errorf("other jobs = %v, warnings = %v", r.OtherJobs, r.Warnings)
	}
}

func TestPreflightProblems(t *testing.T) {
	p, pInv, dInv, setup := takeoverFixture()
	// Another hand-written job listens on the plan's port.
	pInv.Zrepl.Jobs[1] = &inventoryv1.ZreplJob{Name: "other_source", Type: "source", ListenAddress: "192.0.2.12:8888"}
	pri := &clientv1.ZreplPreflightResult{ZreplVersion: "v0.7.0", Datasets: []*clientv1.DatasetSnapshots{
		{Dataset: "rpool/subvol-101-disk-0", Exists: true, Snapshots: snaps("@zrepl_1", uint64(1))},
		{Dataset: "rpool/subvol-102-disk-0", Exists: true, Snapshots: snaps("@zrepl_5", uint64(5))},
	}}
	dr := &clientv1.ZreplPreflightResult{Datasets: []*clientv1.DatasetSnapshots{
		// No common snapshot.
		{Dataset: "tank/replicated/rpool/subvol-101-disk-0", Exists: true, Snapshots: snaps("@other", uint64(9))},
		// Diverged: a newer snapshot the primary doesn't have.
		{Dataset: "tank/replicated/rpool/subvol-102-disk-0", Exists: true, Snapshots: snaps("@zrepl_5", uint64(5), "@manual", uint64(6))},
	}}
	got := strings.Join(Preflight(p.Spec, pInv, dInv, setup, pri, dr).Problems, "\n")
	for _, want := range []string{
		"zrepl isn't installed on the DR host",
		"tank/replicated/rpool/subvol-101-disk-0 has no snapshot in common",
		"subvol-102-disk-0 has a snapshot the primary doesn't (@manual)",
		"other_source on the primary also listens on port 8888",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing problem %q in:\n%s", want, got)
		}
	}
}
