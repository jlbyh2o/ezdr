package plan

import (
	"sort"
	"strconv"
	"strings"

	"google.golang.org/protobuf/proto"

	inventoryv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/inventory/v1"
	planv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/plan/v1"
)

// Suggest returns a copy of spec with defaults and missing mappings, startup
// settings, and receive datasets filled in from the hosts' inventories.
// Anything already set is kept. Either inventory may be nil.
func Suggest(spec *planv1.PlanSpec, primary, dr *inventoryv1.Inventory) *planv1.PlanSpec {
	s := proto.CloneOf(spec)
	if s.IntervalSeconds == 0 {
		s.IntervalSeconds = DefaultIntervalSeconds
	}
	if len(s.PrimaryRetention) == 0 {
		s.PrimaryRetention = DefaultPrimaryRetention()
	}
	if len(s.DrRetention) == 0 {
		s.DrRetention = DefaultDRRetention()
	}
	if s.SnapshotPrefix == "" {
		s.SnapshotPrefix = DefaultSnapshotPrefix
	}
	if primary == nil {
		return s
	}

	guests := guestsByID(primary)
	startupAssigned := false
	for _, pg := range s.Guests {
		g := guests[pg.Vmid]
		if g == nil || pg.StartupOrder != 0 || pg.StartupDelaySeconds != 0 {
			continue
		}
		pg.StartupOrder, pg.StartupDelaySeconds = parseStartup(g.Startup)
		startupAssigned = true
	}
	if startupAssigned {
		placeUnordered(s.Guests)
	}

	usedStorage, usedBridges := used(s, guests)
	if dr == nil {
		return s
	}

	mappedStorage := map[string]*planv1.StorageMapping{}
	for _, m := range s.StorageMappings {
		mappedStorage[m.SourceStorage] = m
	}
	for _, src := range usedStorage {
		m := mappedStorage[src]
		if m == nil {
			m = &planv1.StorageMapping{SourceStorage: src, TargetStorage: suggestStorage(src, dr)}
			s.StorageMappings = append(s.StorageMappings, m)
		}
		if m.ReceiveDataset == "" && m.TargetStorage != "" {
			if st := findStorage(dr, m.TargetStorage); st != nil && st.ZfsPool != "" {
				m.ReceiveDataset = st.ZfsPool + "/ezdr/" + primary.GetHost().GetHostname()
			}
		}
	}

	mappedBridges := map[string]bool{}
	targets := map[string]bool{}
	for _, m := range s.NetworkMappings {
		mappedBridges[m.SourceBridge] = true
		targets[m.TargetBridge] = true
	}
	for _, src := range usedBridges {
		if !mappedBridges[src] {
			t := suggestBridge(src, dr)
			s.NetworkMappings = append(s.NetworkMappings, &planv1.NetworkMapping{SourceBridge: src, TargetBridge: t})
			targets[t] = true
		}
	}
	if s.TestBridge == "" {
		var candidates []string
		for _, i := range dr.Interfaces {
			if i.Type == "bridge" && len(i.BridgePorts) == 0 && !targets[i.Name] {
				candidates = append(candidates, i.Name)
			}
		}
		if len(candidates) == 1 {
			s.TestBridge = candidates[0]
		}
	}
	return s
}

func guestsByID(inv *inventoryv1.Inventory) map[uint32]*inventoryv1.Guest {
	m := map[uint32]*inventoryv1.Guest{}
	for _, g := range inv.GetGuests() {
		m[g.Vmid] = g
	}
	return m
}

// used returns the source storages (of replicable disks) and bridges used by
// the plan's guests, sorted.
func used(s *planv1.PlanSpec, guests map[uint32]*inventoryv1.Guest) (storage, bridges []string) {
	st, br := map[string]bool{}, map[string]bool{}
	for _, pg := range s.Guests {
		g := guests[pg.Vmid]
		if g == nil {
			continue
		}
		for _, d := range g.Disks {
			if d.Readiness == inventoryv1.Readiness_READINESS_REPLICABLE {
				st[d.Storage] = true
			}
		}
		for _, n := range g.Nics {
			if n.Bridge != "" {
				br[n.Bridge] = true
			}
		}
	}
	return sortedKeys(st), sortedKeys(br)
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func findStorage(inv *inventoryv1.Inventory, id string) *inventoryv1.Storage {
	for _, s := range inv.GetStorages() {
		if s.Id == id {
			return s
		}
	}
	return nil
}

func findInterface(inv *inventoryv1.Inventory, name string) *inventoryv1.NetworkInterface {
	for _, i := range inv.GetInterfaces() {
		if i.Name == name {
			return i
		}
	}
	return nil
}

func suggestStorage(src string, dr *inventoryv1.Inventory) string {
	var zfs []string
	for _, s := range dr.Storages {
		if s.Type == "zfspool" {
			if s.Id == src {
				return s.Id
			}
			zfs = append(zfs, s.Id)
		}
	}
	if len(zfs) == 1 {
		return zfs[0]
	}
	return ""
}

func suggestBridge(src string, dr *inventoryv1.Inventory) string {
	if i := findInterface(dr, src); i != nil && i.Type == "bridge" {
		return src
	}
	var internal, aware []string
	for _, i := range dr.Interfaces {
		if i.Type != "bridge" || !i.VlanAware {
			continue
		}
		aware = append(aware, i.Name)
		if len(i.BridgePorts) == 0 {
			internal = append(internal, i.Name)
		}
	}
	switch {
	case len(internal) == 1:
		return internal[0]
	case len(aware) == 1:
		return aware[0]
	}
	return ""
}

// parseStartup reads Proxmox's "order=N,up=S" startup setting.
func parseStartup(s string) (order int32, delay uint32) {
	for _, part := range strings.Split(s, ",") {
		k, v, _ := strings.Cut(part, "=")
		n, err := strconv.ParseUint(v, 10, 31)
		if err != nil {
			continue
		}
		switch k {
		case "order":
			order = int32(n) //nolint:gosec // bounded by ParseUint
		case "up":
			delay = uint32(n)
		}
	}
	return order, delay
}

// placeUnordered gives guests without a startup order an order after all
// others, in VMID order.
func placeUnordered(guests []*planv1.PlanGuest) {
	var maxOrder int32
	var unordered []*planv1.PlanGuest
	for _, g := range guests {
		if g.StartupOrder == 0 {
			unordered = append(unordered, g)
		} else if g.StartupOrder > maxOrder {
			maxOrder = g.StartupOrder
		}
	}
	sort.Slice(unordered, func(i, j int) bool { return unordered[i].Vmid < unordered[j].Vmid })
	for i, g := range unordered {
		g.StartupOrder = maxOrder + int32(i) + 1 //nolint:gosec // small counts
	}
}
