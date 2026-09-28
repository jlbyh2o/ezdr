package replication

import (
	"fmt"
	"slices"
	"strings"

	clientv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/client/v1"
	inventoryv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/inventory/v1"
	planv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/plan/v1"
	portalv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/portal/v1"
	"github.com/jlbyh2o/ezdr/internal/plan"
)

// FailbackPreflight works out what failing a failed-over plan back would do
// (docs/design/failback.md, section 3) from both hosts' snapshots and
// inventories: each dataset's common snapshot, how much the DR host copies
// back, what the primary discards, and the guests' configuration changes
// made at the DR site. storagePrefix is the prefix of the Proxmox storages
// the failover added on the DR host.
func FailbackPreflight(spec *planv1.PlanSpec, primaryInv, drInv *inventoryv1.Inventory, storagePrefix string,
	pri, dr *clientv1.ZreplPreflightResult) *portalv1.FailbackPreflight {
	r := &portalv1.FailbackPreflight{}
	problem := func(format string, args ...any) { r.Problems = append(r.Problems, fmt.Sprintf(format, args...)) }
	warn := func(format string, args ...any) { r.Warnings = append(r.Warnings, fmt.Sprintf(format, args...)) }
	change := func(format string, args ...any) {
		r.ConfigChanges = append(r.ConfigChanges, fmt.Sprintf(format, args...))
	}

	byName := func(res *clientv1.ZreplPreflightResult) map[string]*clientv1.DatasetSnapshots {
		m := map[string]*clientv1.DatasetSnapshots{}
		for _, d := range res.GetDatasets() {
			m[d.Dataset] = d
		}
		return m
	}
	priSets, drSets := byName(pri), byName(dr)
	for _, td := range TakeoverDatasets(spec, primaryInv) {
		p, q := priSets[td.Primary], drSets[td.Replica]
		if p == nil || q == nil {
			problem("%s wasn't checked; run the preflight again", td.Primary)
			continue
		}
		switch {
		case !p.Exists:
			problem("%s no longer exists on the primary; failing back into a new dataset isn't supported", td.Primary)
			continue
		case !q.Exists:
			problem("the replica %s doesn't exist on the DR host", td.Replica)
			continue
		}
		ds := &portalv1.FailbackDataset{Dataset: td.Primary, Replica: td.Replica}
		r.Datasets = append(r.Datasets, ds)
		common, priIdx, drIdx := failbackCommon(p, q)
		if common == "" {
			problem("%s has no snapshot in common with its replica, so only a full copy could fail it back (not supported)", td.Primary)
			continue
		}
		ds.CommonSnapshot = common
		ds.CopyBytes = q.WrittenBytes
		for _, s := range q.Snapshots[drIdx+1:] {
			ds.CopyBytes += s.WrittenBytes
		}
		ds.DivergedBytes = p.WrittenBytes
		for _, s := range p.Snapshots[priIdx+1:] {
			if strings.HasPrefix(s.Name, "@") {
				ds.DiscardedSnapshots = append(ds.DiscardedSnapshots, s.Name)
				ds.DivergedBytes += s.WrittenBytes
			}
		}
		if len(ds.DiscardedSnapshots) > 0 || ds.DivergedBytes > 0 {
			r.Diverged = true
		}
	}

	guests := func(inv *inventoryv1.Inventory) map[uint32]*inventoryv1.Guest {
		m := map[uint32]*inventoryv1.Guest{}
		for _, g := range inv.GetGuests() {
			m[g.Vmid] = g
		}
		return m
	}
	priGuests, drGuests := guests(primaryInv), guests(drInv)
	inPlan := map[uint32]bool{}
	for _, pg := range spec.Guests {
		inPlan[pg.Vmid] = true
		p, q := priGuests[pg.Vmid], drGuests[pg.Vmid]
		switch {
		case p == nil:
			problem("guest %d no longer exists on the primary; failing back to a rebuilt primary isn't supported", pg.Vmid)
		case q == nil:
			warn("guest %d isn't registered on the DR host; its disks are still copied back from the replicas", pg.Vmid)
		default:
			compareGuests(p, q, problem, change)
		}
	}
	for _, g := range drInv.GetGuests() {
		if inPlan[g.Vmid] {
			continue
		}
		for _, d := range g.Disks {
			if storagePrefix != "" && strings.HasPrefix(d.Storage, storagePrefix) {
				problem("guest %d on the DR host isn't in the plan but has a disk on the plan's storage %s; move or remove that disk first: the failback removes the storage",
					g.Vmid, d.Storage)
				break
			}
		}
	}
	return r
}

// failbackCommon returns the replica's newest snapshot that the primary also
// has as a snapshot (a bookmark can't be received into), named as on the
// primary, with its index in each list. The name is empty if there's none.
func failbackCommon(primary, replica *clientv1.DatasetSnapshots) (name string, priIdx, drIdx int) {
	for i := len(replica.Snapshots) - 1; i >= 0; i-- {
		s := replica.Snapshots[i]
		if !strings.HasPrefix(s.Name, "@") {
			continue
		}
		j := slices.IndexFunc(primary.Snapshots, func(p *clientv1.SnapshotInfo) bool {
			return p.Guid == s.Guid && strings.HasPrefix(p.Name, "@")
		})
		if j >= 0 {
			return primary.Snapshots[j].Name, j, i
		}
	}
	return "", 0, 0
}

// compareGuests reports what changed in a guest's configuration at the DR
// site (q) compared with the primary's (p). Disks added at the DR site block
// the failback: their data would be lost.
func compareGuests(p, q *inventoryv1.Guest, problem, change func(string, ...any)) {
	priDisks := map[string]*inventoryv1.Disk{}
	for _, d := range p.Disks {
		priDisks[d.Key] = d
	}
	drDisks := map[string]bool{}
	for _, d := range q.Disks {
		drDisks[d.Key] = true
		pd := priDisks[d.Key]
		switch {
		case pd == nil:
			problem("guest %d has a disk added at the DR site (%s): failing back would lose its data; move the data to another disk and remove it first",
				q.Vmid, d.Key)
		case d.SizeBytes > pd.SizeBytes && pd.SizeBytes > 0:
			change("guest %d: disk %s was resized at the DR site (%s to %s); check its size on the primary after failing back",
				q.Vmid, d.Key, plan.FormatBytes(pd.SizeBytes), plan.FormatBytes(d.SizeBytes))
		}
	}
	for _, d := range p.Disks {
		if !drDisks[d.Key] {
			change("guest %d: disk %s was removed at the DR site; the primary keeps it", q.Vmid, d.Key)
		}
	}
	if p.Cores != q.Cores {
		change("guest %d: cores changed at the DR site (%d to %d)", q.Vmid, p.Cores, q.Cores)
	}
	if p.MemoryBytes != q.MemoryBytes {
		change("guest %d: memory changed at the DR site (%s to %s)", q.Vmid, plan.FormatBytes(p.MemoryBytes), plan.FormatBytes(q.MemoryBytes))
	}
	priNics := map[string]*inventoryv1.Nic{}
	for _, n := range p.Nics {
		priNics[n.Key] = n
	}
	drNics := map[string]bool{}
	for _, n := range q.Nics {
		drNics[n.Key] = true
		switch pn := priNics[n.Key]; {
		case pn == nil:
			change("guest %d: network device %s was added at the DR site", q.Vmid, n.Key)
		case !strings.EqualFold(pn.Mac, n.Mac) || pn.VlanTag != n.VlanTag || pn.Model != n.Model:
			change("guest %d: network device %s changed at the DR site", q.Vmid, n.Key)
		}
	}
	for _, n := range p.Nics {
		if !drNics[n.Key] {
			change("guest %d: network device %s was removed at the DR site", q.Vmid, n.Key)
		}
	}
}
