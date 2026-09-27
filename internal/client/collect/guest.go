package collect

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	inventoryv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/inventory/v1"
)

var (
	vmDiskKey  = regexp.MustCompile(`^((ide|sata|scsi|virtio)\d+|efidisk0|tpmstate0|unused\d+)$`)
	ctDiskKey  = regexp.MustCompile(`^(rootfs|mp\d+|unused\d+)$`)
	nicKey     = regexp.MustCompile(`^net\d+$`)
	vmPassKey  = regexp.MustCompile(`^(hostpci|usb)\d+$`)
	ctPassKey  = regexp.MustCompile(`^dev\d+$`)
	nicModels  = []string{"virtio", "e1000", "e1000e", "rtl8139", "vmxnet3", "i82551", "i82557b", "i82559er", "ne2k_isa", "ne2k_pci", "pcnet"}
	macPattern = regexp.MustCompile(`^[0-9A-Fa-f]{2}(:[0-9A-Fa-f]{2}){5}$`)
)

// storageInfo is what readiness needs to know about a storage.
type storageInfo struct {
	typ  string
	pool string
}

// buildGuest converts a guest's list entry and configuration into inventory.
// Only explicitly listed fields are read, so notes, cloud-init credentials,
// and unknown keys are never included.
func buildGuest(typ inventoryv1.GuestType, entry, cfg map[string]any, storages map[string]storageInfo) *inventoryv1.Guest {
	g := &inventoryv1.Guest{
		Vmid:     uint32(num(entry["vmid"])), //nolint:gosec // VM IDs fit in uint32
		Type:     typ,
		Status:   str(entry["status"]),
		Onboot:   boolean(cfg["onboot"]),
		Startup:  str(cfg["startup"]),
		Template: boolean(cfg["template"]) || boolean(entry["template"]),
		Tags:     splitTags(str(cfg["tags"])),
		Lock:     str(cfg["lock"]),
	}
	if typ == inventoryv1.GuestType_GUEST_TYPE_VM {
		g.Name = str(cfg["name"])
		cores, sockets := num(cfg["cores"]), num(cfg["sockets"])
		cores, sockets = max(cores, 1), max(sockets, 1)
		g.Cores = uint32(cores * sockets) //nolint:gosec // small counts
		mem := parseMemoryMB(cfg["memory"])
		if mem == 0 {
			mem = 512 // Proxmox default
		}
		g.MemoryBytes = mem << 20
	} else {
		g.Name = str(cfg["hostname"])
		g.Cores = uint32(num(cfg["cores"])) //nolint:gosec // small counts
		mem := num(cfg["memory"])
		if mem == 0 {
			mem = 512 // Proxmox default
		}
		g.MemoryBytes = mem << 20
	}
	if g.Name == "" {
		g.Name = str(entry["name"])
	}

	keys := make([]string, 0, len(cfg))
	for k := range cfg {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return naturalLess(keys[i], keys[j]) })

	diskKey, passKey := vmDiskKey, vmPassKey
	if typ == inventoryv1.GuestType_GUEST_TYPE_CONTAINER {
		diskKey, passKey = ctDiskKey, ctPassKey
	}
	for _, k := range keys {
		v := str(cfg[k])
		switch {
		case diskKey.MatchString(k):
			if d := buildDisk(typ, k, v, storages, g); d != nil {
				g.Disks = append(g.Disks, d)
			}
		case nicKey.MatchString(k):
			g.Nics = append(g.Nics, buildNic(typ, k, v))
		case passKey.MatchString(k):
			g.Passthrough = true
		}
	}

	g.Ready = true
	for _, d := range g.Disks {
		if d.Readiness == inventoryv1.Readiness_READINESS_NOT_REPLICABLE {
			g.Ready = false
		}
	}
	if g.Passthrough {
		g.ReadinessWarnings = append(g.ReadinessWarnings,
			"uses PCI, USB, or device passthrough; the DR host's hardware may differ")
	}
	return g
}

func buildDisk(typ inventoryv1.GuestType, key, value string, storages map[string]storageInfo, g *inventoryv1.Guest) *inventoryv1.Disk {
	p := propertyString(value)
	file := p[""]
	d := &inventoryv1.Disk{Key: key, SizeBytes: parseSize(p["size"])}

	switch {
	case p["media"] == "cdrom":
		storage, volume, _ := strings.Cut(file, ":")
		d.Readiness = inventoryv1.Readiness_READINESS_NOT_NEEDED
		switch {
		case strings.Contains(volume, "cloudinit"):
			d.Storage, d.Volume, d.Reason = storage, volume, "cloud-init drive; regenerated from the guest's configuration"
		case file == "none" || file == "cdrom" || file == "":
			d.Volume, d.Reason = file, "CD/DVD drive"
		default:
			d.Storage, d.Volume, d.Reason = storage, volume, "CD/DVD media"
			g.ReadinessWarnings = append(g.ReadinessWarnings,
				fmt.Sprintf("%s uses %s; it must also exist at the DR site", key, file))
		}
		return d
	case strings.HasPrefix(file, "/"):
		d.Volume = file
		d.Readiness = inventoryv1.Readiness_READINESS_NOT_REPLICABLE
		if typ == inventoryv1.GuestType_GUEST_TYPE_CONTAINER {
			d.Reason = "bind mount of a host path"
		} else {
			d.Reason = "passthrough device"
		}
		return d
	}

	storage, volume, ok := strings.Cut(file, ":")
	if !ok {
		d.Volume = file
		d.Readiness = inventoryv1.Readiness_READINESS_NOT_REPLICABLE
		d.Reason = "unrecognized disk"
		return d
	}
	d.Storage, d.Volume = storage, volume

	if strings.HasPrefix(key, "unused") {
		d.Readiness = inventoryv1.Readiness_READINESS_NOT_NEEDED
		d.Reason = "detached disk"
		return d
	}
	info, known := storages[storage]
	switch {
	case !known:
		d.Readiness = inventoryv1.Readiness_READINESS_NOT_REPLICABLE
		d.Reason = fmt.Sprintf("unknown storage %q", storage)
	case info.typ == "zfspool":
		d.Readiness = inventoryv1.Readiness_READINESS_REPLICABLE
		d.ZfsDataset = info.pool + "/" + volume
	default:
		d.Readiness = inventoryv1.Readiness_READINESS_NOT_REPLICABLE
		d.Reason = fmt.Sprintf("on %s storage %q; only ZFS storage can be replicated", info.typ, storage)
	}
	return d
}

func buildNic(typ inventoryv1.GuestType, key, value string) *inventoryv1.Nic {
	p := propertyString(value)
	n := &inventoryv1.Nic{
		Key:      key,
		Bridge:   p["bridge"],
		VlanTag:  uint32(num(p["tag"])), //nolint:gosec // VLAN IDs fit in uint32
		Firewall: boolean(p["firewall"]),
	}
	if typ == inventoryv1.GuestType_GUEST_TYPE_CONTAINER {
		n.Mac, n.Model = p["hwaddr"], p["type"]
		return n
	}
	// VM NICs start with "<model>=<MAC>", such as "virtio=BC:24:11:48:39:FB".
	for _, m := range nicModels {
		if mac, ok := p[m]; ok {
			n.Model = m
			if macPattern.MatchString(mac) {
				n.Mac = mac
			}
		}
	}
	if n.Model == "" {
		n.Model = p["model"]
		n.Mac = p["macaddr"]
	}
	return n
}

// naturalLess orders "scsi2" before "scsi10".
func naturalLess(a, b string) bool {
	pa, na := splitNum(a)
	pb, nb := splitNum(b)
	if pa != pb {
		return pa < pb
	}
	return na < nb
}

func splitNum(s string) (string, int) {
	i := len(s)
	for i > 0 && s[i-1] >= '0' && s[i-1] <= '9' {
		i--
	}
	n, _ := strconv.Atoi(s[i:])
	return s[:i], n
}
