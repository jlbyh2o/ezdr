package plan

import (
	"sort"

	inventoryv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/inventory/v1"
	planv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/plan/v1"
)

// DefaultZreplPort is the first zrepl port for a plan.
const DefaultZreplPort = 8888

// JobGroup is a set of datasets replicated by one zrepl source/pull job
// pair. zrepl needs one sending job per receiver and sets send options per
// job, so a plan has one group per storage mapping and encryption kind.
type JobGroup struct {
	SourceStorage  string
	ReceiveDataset string
	Encrypted      bool
	// Datasets on the primary, sorted.
	Datasets []string
	// Port is the group's zrepl port: the plan's base port plus its index.
	Port uint32
}

// JobGroups derives a plan's job groups from the primary's inventory: every
// replicable disk of every protected guest, grouped by storage mapping and
// encryption. Groups are ordered by source storage, unencrypted first, which
// keeps their ports stable.
func JobGroups(spec *planv1.PlanSpec, primary *inventoryv1.Inventory) []JobGroup {
	encrypted := map[string]bool{}
	for _, d := range primary.GetZfsDatasets() {
		encrypted[d.Name] = d.Encryption != "" && d.Encryption != "off"
	}
	receive := map[string]string{}
	for _, m := range spec.StorageMappings {
		receive[m.SourceStorage] = m.ReceiveDataset
	}
	guests := guestsByID(primary)

	type key struct {
		storage string
		enc     bool
	}
	sets := map[key]map[string]bool{}
	for _, pg := range spec.Guests {
		g := guests[pg.Vmid]
		if g == nil {
			continue
		}
		for _, d := range g.Disks {
			if d.Readiness != inventoryv1.Readiness_READINESS_REPLICABLE || d.ZfsDataset == "" {
				continue
			}
			if _, mapped := receive[d.Storage]; !mapped {
				continue
			}
			k := key{d.Storage, encrypted[d.ZfsDataset]}
			if sets[k] == nil {
				sets[k] = map[string]bool{}
			}
			sets[k][d.ZfsDataset] = true
		}
	}

	keys := make([]key, 0, len(sets))
	for k := range sets {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].storage != keys[j].storage {
			return keys[i].storage < keys[j].storage
		}
		return !keys[i].enc && keys[j].enc
	})

	base := BasePort(spec)
	groups := make([]JobGroup, 0, len(keys))
	for i, k := range keys {
		groups = append(groups, JobGroup{
			SourceStorage: k.storage, ReceiveDataset: receive[k.storage], Encrypted: k.enc,
			Datasets: sortedKeys(sets[k]), Port: base + uint32(i), //nolint:gosec // few groups
		})
	}
	return groups
}

// BasePort returns the plan's first zrepl port.
func BasePort(spec *planv1.PlanSpec) uint32 {
	var p uint32
	switch n := spec.GetNetwork().GetPath().(type) {
	case *planv1.ReplicationNetwork_Existing:
		p = n.Existing.GetPort()
	case *planv1.ReplicationNetwork_Tunnel:
		p = n.Tunnel.GetPort()
	}
	if p == 0 {
		p = DefaultZreplPort
	}
	return p
}
