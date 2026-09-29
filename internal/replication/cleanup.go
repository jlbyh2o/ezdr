package replication

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	clientv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/client/v1"
	inventoryv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/inventory/v1"
	planv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/plan/v1"
	"github.com/jlbyh2o/ezdr/internal/plan"
)

// HostCleanup is what a cleanup asks one host to remove. See
// docs/design/cleanup.md.
type HostCleanup struct {
	Targets     []*clientv1.DataTarget
	ReleaseJobs []string
	// Notes are datasets left alone, and why.
	Notes []string
}

// guestDisk matches the names Proxmox gives guest disks, capturing the
// VMID.
var guestDisk = regexp.MustCompile(`^(?:vm|subvol)-(\d+)-disk-\d+$`)

// within reports whether dataset a is b or inside it.
func within(a, b string) bool { return a == b || strings.HasPrefix(a, b+"/") }

// PlanData returns what deleting a plan's replicated data removes: on the
// primary, the snapshots and bookmarks with the plan's prefix on its disks;
// on the DR host, its replicas. Besides the replicas of the disks the
// primary reports, replicas of the plan's guests' disks found under its
// receive datasets are included (a disk deleted on the primary leaves its
// replica behind), unless another plan's replicas are there.
func PlanData(planID string, spec *planv1.PlanSpec, primary, dr *inventoryv1.Inventory, others []plan.OtherReplicas) (pri, drc HostCleanup) {
	groups := plan.JobGroups(spec, primary)
	replicas := map[string]string{} // replica -> receive dataset
	for _, g := range groups {
		job := JobName(planID, g)
		pri.ReleaseJobs = append(pri.ReleaseJobs, job)
		drc.ReleaseJobs = append(drc.ReleaseJobs, PullJobName(job))
		for _, d := range g.Datasets {
			pri.Targets = append(pri.Targets, &clientv1.DataTarget{Dataset: d, Prefix: spec.SnapshotPrefix})
			replicas[g.ReceiveDataset+"/"+d] = g.ReceiveDataset
		}
	}

	vmids := map[uint64]bool{}
	for _, g := range spec.Guests {
		vmids[uint64(g.Vmid)] = true
	}
	var receive []string
	for _, m := range spec.StorageMappings {
		if m.ReceiveDataset != "" && !slices.Contains(receive, m.ReceiveDataset) {
			receive = append(receive, m.ReceiveDataset)
		}
	}
	for _, d := range dr.GetZfsDatasets() {
		base := d.Name[strings.LastIndex(d.Name, "/")+1:]
		m := guestDisk.FindStringSubmatch(base)
		if m == nil {
			continue
		}
		if id, _ := strconv.ParseUint(m[1], 10, 32); !vmids[id] {
			continue
		}
		for _, r := range receive {
			if !strings.HasPrefix(d.Name, r+"/") || replicas[d.Name] != "" {
				continue
			}
			if !claimed(d.Name, others) {
				replicas[d.Name] = r
			}
		}
	}
	for _, name := range sortedKeys(replicas) {
		drc.Targets = append(drc.Targets, &clientv1.DataTarget{Dataset: name, Destroy: true, EmptyParentsBelow: replicas[name]})
	}
	return pri, drc
}

// claimed reports whether another plan keeps a replica at or around the
// dataset.
func claimed(dataset string, others []plan.OtherReplicas) bool {
	for _, o := range others {
		for _, p := range o.Paths {
			if within(dataset, p) || within(p, dataset) {
				return true
			}
		}
	}
	return false
}

// TakeoverLeftovers returns what the old zrepl jobs left behind after a
// takeover: on the primary, the snapshots and bookmarks with the plan's
// prefix (the old jobs' prefix) on the datasets they replicated but the
// plan doesn't; on the DR host, those datasets' stale replicas under the
// plan's receive datasets (the old root_fs). A replica with children (such
// as the pool root's, which holds every other replica) only loses its
// snapshots. Datasets a remaining zrepl job on the primary still covers
// are left alone.
func TakeoverLeftovers(spec *planv1.PlanSpec, dropped []string, primary, dr *inventoryv1.Inventory) (pri, drc HostCleanup) {
	exists := map[string]bool{}
	for _, d := range dr.GetZfsDatasets() {
		exists[d.Name] = true
	}
	hasChildren := func(ds string) bool {
		for name := range exists {
			if strings.HasPrefix(name, ds+"/") {
				return true
			}
		}
		return false
	}
	var receive []string
	for _, m := range spec.StorageMappings {
		if m.ReceiveDataset != "" && !slices.Contains(receive, m.ReceiveDataset) {
			receive = append(receive, m.ReceiveDataset)
		}
	}
	for _, ds := range dropped {
		if job := coveringJob(primary, ds); job != "" {
			pri.Notes = append(pri.Notes, fmt.Sprintf("%s: zrepl job %s still replicates it", ds, job))
			continue
		}
		pri.Targets = append(pri.Targets, &clientv1.DataTarget{Dataset: ds, Prefix: spec.SnapshotPrefix})
		for _, r := range receive {
			replica := r + "/" + ds
			switch {
			case !exists[replica]:
			case hasChildren(replica):
				drc.Targets = append(drc.Targets, &clientv1.DataTarget{Dataset: replica, Prefix: spec.SnapshotPrefix})
			default:
				drc.Targets = append(drc.Targets, &clientv1.DataTarget{Dataset: replica, Destroy: true, EmptyParentsBelow: r})
			}
		}
	}
	return pri, drc
}

// coveringJob returns a zrepl job on the host that sends or snapshots the
// dataset, if any.
func coveringJob(inv *inventoryv1.Inventory, dataset string) string {
	for _, j := range inv.GetZrepl().GetJobs() {
		switch j.Type {
		case "source", "push", "snap":
			if plan.FilterIncludes(j.Filesystems, dataset) {
				return j.Name
			}
		}
	}
	return ""
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
