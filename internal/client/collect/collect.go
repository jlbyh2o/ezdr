// Package collect builds a host's inventory from the local Proxmox VE API and
// ZFS. See docs/design/inventory.md.
package collect

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	inventoryv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/inventory/v1"
)

// API reads from the Proxmox VE API; *pve.Client implements it.
type API interface {
	Get(ctx context.Context, path string, out any) error
}

// Sources are the inputs to Collect. The defaults read the local host.
type Sources struct {
	API API
	// ZFSList returns the output of `zfs list -Hp -t filesystem,volume -o
	// name,type,used,refer,volsize,compression,encryption`.
	ZFSList func(ctx context.Context) ([]byte, error)
	// ZFSVersion returns the loaded ZFS module version.
	ZFSVersion func() string
	Hostname   func() (string, error)
	Now        func() time.Time
}

// LocalSources returns sources for this host using api.
func LocalSources(api API) Sources {
	return Sources{
		API: api,
		ZFSList: func(ctx context.Context) ([]byte, error) {
			ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
			defer cancel()
			return exec.CommandContext(ctx, "zfs", "list", "-Hp", "-t", "filesystem,volume",
				"-o", "name,type,used,refer,volsize,compression,encryption").Output()
		},
		ZFSVersion: func() string {
			b, _ := os.ReadFile("/sys/module/zfs/version")
			return strings.TrimSpace(string(b))
		},
		Hostname: os.Hostname,
		Now:      time.Now,
	}
}

type object = map[string]any

// Collect builds the inventory. Problems with individual items are recorded
// in the inventory's warnings; an error is returned only when nothing useful
// could be collected.
func Collect(ctx context.Context, src Sources) (*inventoryv1.Inventory, error) {
	inv := &inventoryv1.Inventory{CollectedAt: timestamppb.New(src.Now())}
	warn := func(format string, args ...any) {
		inv.Warnings = append(inv.Warnings, fmt.Sprintf(format, args...))
	}

	node, err := nodeName(ctx, src)
	if err != nil {
		return nil, err
	}

	inv.Host = &inventoryv1.HostInfo{Hostname: node, ZfsVersion: src.ZFSVersion()}
	var status object
	if err := src.API.Get(ctx, "nodes/"+node+"/status", &status); err != nil {
		warn("host status: %v", err)
	} else {
		inv.Host.PveVersion = str(status["pveversion"])
		inv.Host.Kernel = str(status["kversion"])
		if ci, ok := status["cpuinfo"].(object); ok {
			inv.Host.CpuModel = str(ci["model"])
			inv.Host.Cpus = uint32(num(ci["cpus"])) //nolint:gosec // small counts
		}
		if mem, ok := status["memory"].(object); ok {
			inv.Host.MemoryBytes = num(mem["total"])
		}
	}

	storages := collectStorage(ctx, src, node, inv, warn)
	collectGuests(ctx, src, node, inv, storages, warn)
	collectZFS(ctx, src, node, inv, warn)
	collectNetwork(ctx, src, node, inv, warn)
	return inv, nil
}

// nodeName finds this host's Proxmox node name.
func nodeName(ctx context.Context, src Sources) (string, error) {
	var nodes []object
	if err := src.API.Get(ctx, "nodes", &nodes); err != nil {
		return "", fmt.Errorf("list nodes: %w", err)
	}
	hostname, _ := src.Hostname()
	short, _, _ := strings.Cut(hostname, ".")
	for _, n := range nodes {
		if str(n["node"]) == short {
			return short, nil
		}
	}
	if len(nodes) == 1 {
		return str(nodes[0]["node"]), nil
	}
	return "", fmt.Errorf("could not find node %q among %d cluster nodes", short, len(nodes))
}

func collectStorage(ctx context.Context, src Sources, node string, inv *inventoryv1.Inventory, warn func(string, ...any)) map[string]storageInfo {
	info := map[string]storageInfo{}
	var configs, statuses []object
	if err := src.API.Get(ctx, "storage", &configs); err != nil {
		warn("storage configuration: %v", err)
		return info
	}
	if err := src.API.Get(ctx, "nodes/"+node+"/storage", &statuses); err != nil {
		warn("storage status: %v", err)
	}
	status := map[string]object{}
	for _, s := range statuses {
		status[str(s["storage"])] = s
	}
	for _, c := range configs {
		id := str(c["storage"])
		if nodes := str(c["nodes"]); nodes != "" && !contains(strings.Split(nodes, ","), node) {
			continue // not available on this node
		}
		st := status[id]
		s := &inventoryv1.Storage{
			Id: id, Type: str(c["type"]), ZfsPool: str(c["pool"]),
			Content:        strings.Split(str(c["content"]), ","),
			TotalBytes:     num(st["total"]),
			UsedBytes:      num(st["used"]),
			AvailableBytes: num(st["avail"]),
			Active:         boolean(st["active"]),
			Shared:         boolean(c["shared"]),
		}
		if len(s.Content) == 1 && s.Content[0] == "" {
			s.Content = nil
		}
		sort.Strings(s.Content)
		inv.Storages = append(inv.Storages, s)
		info[id] = storageInfo{typ: s.Type, pool: s.ZfsPool}
	}
	sort.Slice(inv.Storages, func(i, j int) bool { return inv.Storages[i].Id < inv.Storages[j].Id })
	return info
}

func collectGuests(ctx context.Context, src Sources, node string, inv *inventoryv1.Inventory, storages map[string]storageInfo, warn func(string, ...any)) {
	for _, kind := range []struct {
		path  string
		typ   inventoryv1.GuestType
		label string
	}{
		{"qemu", inventoryv1.GuestType_GUEST_TYPE_VM, "VM"},
		{"lxc", inventoryv1.GuestType_GUEST_TYPE_CONTAINER, "container"},
	} {
		var list []object
		if err := src.API.Get(ctx, "nodes/"+node+"/"+kind.path, &list); err != nil {
			warn("list %ss: %v", kind.label, err)
			continue
		}
		for _, entry := range list {
			vmid := str(entry["vmid"])
			var cfg object
			if err := src.API.Get(ctx, "nodes/"+node+"/"+kind.path+"/"+vmid+"/config", &cfg); err != nil {
				warn("%s %s configuration: %v", kind.label, vmid, err)
				continue
			}
			inv.Guests = append(inv.Guests, buildGuest(kind.typ, entry, cfg, storages))
		}
	}
	sort.Slice(inv.Guests, func(i, j int) bool { return inv.Guests[i].Vmid < inv.Guests[j].Vmid })
}

func collectZFS(ctx context.Context, src Sources, node string, inv *inventoryv1.Inventory, warn func(string, ...any)) {
	var pools []object
	if err := src.API.Get(ctx, "nodes/"+node+"/disks/zfs", &pools); err != nil {
		warn("ZFS pools: %v", err)
	}
	for _, p := range pools {
		inv.ZfsPools = append(inv.ZfsPools, &inventoryv1.ZfsPool{
			Name: str(p["name"]), Health: str(p["health"]),
			SizeBytes: num(p["size"]), AllocatedBytes: num(p["alloc"]), FreeBytes: num(p["free"]),
			FragmentationPercent: uint32(num(p["frag"])), //nolint:gosec // percentage
		})
	}
	sort.Slice(inv.ZfsPools, func(i, j int) bool { return inv.ZfsPools[i].Name < inv.ZfsPools[j].Name })

	out, err := src.ZFSList(ctx)
	if err != nil {
		warn("ZFS datasets: %v", err)
		return
	}
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		f := strings.Split(sc.Text(), "\t")
		if len(f) != 7 {
			continue
		}
		inv.ZfsDatasets = append(inv.ZfsDatasets, &inventoryv1.ZfsDataset{
			Name: f[0], Type: f[1], UsedBytes: num(f[2]), ReferencedBytes: num(f[3]),
			VolumeSizeBytes: num(f[4]), Compression: f[5], Encryption: f[6],
		})
	}
	sort.Slice(inv.ZfsDatasets, func(i, j int) bool { return inv.ZfsDatasets[i].Name < inv.ZfsDatasets[j].Name })
}

func collectNetwork(ctx context.Context, src Sources, node string, inv *inventoryv1.Inventory, warn func(string, ...any)) {
	var ifaces []object
	if err := src.API.Get(ctx, "nodes/"+node+"/network", &ifaces); err != nil {
		warn("network: %v", err)
		return
	}
	for _, i := range ifaces {
		if str(i["type"]) == "loopback" {
			continue
		}
		inv.Interfaces = append(inv.Interfaces, &inventoryv1.NetworkInterface{
			Name: str(i["iface"]), Type: str(i["type"]),
			Active: boolean(i["active"]), Autostart: boolean(i["autostart"]),
			BridgePorts:   strings.Fields(str(i["bridge_ports"])),
			VlanAware:     boolean(i["bridge_vlan_aware"]),
			VlanId:        uint32(num(i["vlan-id"])), //nolint:gosec // VLAN IDs fit in uint32
			VlanRawDevice: str(i["vlan-raw-device"]),
			BondMembers:   strings.Fields(str(i["slaves"])),
			Cidr:          str(i["cidr"]),
			Gateway:       str(i["gateway"]),
		})
	}
	sort.Slice(inv.Interfaces, func(i, j int) bool { return inv.Interfaces[i].Name < inv.Interfaces[j].Name })
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if strings.TrimSpace(x) == s {
			return true
		}
	}
	return false
}
