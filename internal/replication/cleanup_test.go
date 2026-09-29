package replication

import (
	"strings"
	"testing"

	clientv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/client/v1"
	inventoryv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/inventory/v1"
	planv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/plan/v1"
	"github.com/jlbyh2o/ezdr/internal/plan"
)

func targets(ts []*clientv1.DataTarget) string {
	var out []string
	for _, t := range ts {
		s := t.Dataset + ":" + t.Prefix
		if t.Destroy {
			s = t.Dataset + ":destroy<" + t.EmptyParentsBelow
		}
		out = append(out, s)
	}
	return strings.Join(out, " ")
}

func TestPlanData(t *testing.T) {
	spec := &planv1.PlanSpec{SnapshotPrefix: "ezdr_", Guests: []*planv1.PlanGuest{{Vmid: 101}, {Vmid: 102}},
		StorageMappings: []*planv1.StorageMapping{{SourceStorage: "local-zfs", ReceiveDataset: "tank/ezdr/pve1"}}}
	primary := &inventoryv1.Inventory{Guests: []*inventoryv1.Guest{{Vmid: 101, Disks: []*inventoryv1.Disk{{Storage: "local-zfs",
		ZfsDataset: "rpool/data/vm-101-disk-0", Readiness: inventoryv1.Readiness_READINESS_REPLICABLE}}}}}
	dr := &inventoryv1.Inventory{ZfsDatasets: []*inventoryv1.ZfsDataset{
		{Name: "tank/ezdr/pve1/rpool/data/vm-101-disk-0"},
		// Guest 102's disk was deleted on the primary; its replica remains.
		{Name: "tank/ezdr/pve1/rpool/data/vm-102-disk-1"},
		// Not the plan's guests, or another plan's replica.
		{Name: "tank/ezdr/pve1/rpool/data/vm-103-disk-0"},
		{Name: "tank/ezdr/pve1/rpool/data/subvol-102-disk-0"},
		{Name: "tank/other/rpool/data/vm-101-disk-0"},
	}}
	others := []plan.OtherReplicas{{Plan: "Other", Paths: []string{"tank/ezdr/pve1/rpool/data/subvol-102-disk-0"}}}
	pri, drc := PlanData("abcdefghij", spec, primary, dr, others)
	if got := targets(pri.Targets); got != "rpool/data/vm-101-disk-0:ezdr_" {
		t.Errorf("primary targets = %s", got)
	}
	if got := strings.Join(pri.ReleaseJobs, " ") + " | " + strings.Join(drc.ReleaseJobs, " "); got != "ezdr_abcdefgh_local-zfs | ezdr_abcdefgh_local-zfs_pull" {
		t.Errorf("release jobs = %s", got)
	}
	if got := targets(drc.Targets); got != "tank/ezdr/pve1/rpool/data/vm-101-disk-0:destroy<tank/ezdr/pve1 tank/ezdr/pve1/rpool/data/vm-102-disk-1:destroy<tank/ezdr/pve1" {
		t.Errorf("DR targets = %s", got)
	}
}

func TestTakeoverLeftovers(t *testing.T) {
	spec := &planv1.PlanSpec{SnapshotPrefix: "zrepl_",
		StorageMappings: []*planv1.StorageMapping{{SourceStorage: "local-zfs", ReceiveDataset: "tank/replicated"}}}
	primary := &inventoryv1.Inventory{Zrepl: &inventoryv1.Zrepl{Jobs: []*inventoryv1.ZreplJob{{Name: "backup", Type: "push",
		Filesystems: []*inventoryv1.ZreplFilter{{Pattern: "rpool/keep<", Include: true}}}}}}
	dr := &inventoryv1.Inventory{ZfsDatasets: []*inventoryv1.ZfsDataset{
		{Name: "tank/replicated/rpool"}, {Name: "tank/replicated/rpool/vm-101-disk-0"}, {Name: "tank/replicated/rpool/vm-101-cloudinit"},
	}}
	pri, drc := TakeoverLeftovers(spec, []string{"rpool", "rpool/vm-101-cloudinit", "rpool/keep/data", "rpool/gone"}, primary, dr)
	if got := targets(pri.Targets); got != "rpool:zrepl_ rpool/vm-101-cloudinit:zrepl_ rpool/gone:zrepl_" {
		t.Errorf("primary targets = %s", got)
	}
	if len(pri.Notes) != 1 || pri.Notes[0] != "rpool/keep/data: zrepl job backup still replicates it" {
		t.Errorf("notes = %q", pri.Notes)
	}
	if got := targets(drc.Targets); got != "tank/replicated/rpool:zrepl_ tank/replicated/rpool/vm-101-cloudinit:destroy<tank/replicated" {
		t.Errorf("DR targets = %s", got)
	}
}
