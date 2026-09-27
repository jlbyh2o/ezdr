package collect

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	inventoryv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/inventory/v1"
	"github.com/jlbyh2o/ezdr/internal/inventory"
)

// fakeAPI serves canned JSON by path.
type fakeAPI map[string]string

func (f fakeAPI) Get(_ context.Context, path string, out any) error {
	body, ok := f[path]
	if !ok {
		return fmt.Errorf("GET %s: 403 Forbidden", path)
	}
	return json.Unmarshal([]byte(body), out)
}

const secret = "hunter2-do-not-leak"

var fixture = fakeAPI{
	"nodes":             `[{"node":"pve1"},{"node":"pve2"}]`,
	"nodes/pve1/status": `{"pveversion":"pve-manager/9.2.20/abc","kversion":"Linux 7.0.14-19-pve","cpuinfo":{"model":"Example CPU","cpus":8},"memory":{"total":34359738368}}`,
	"storage": `[
		{"storage":"local-zfs","type":"zfspool","pool":"rpool/data","content":"rootdir,images"},
		{"storage":"local-lvm","type":"lvmthin","content":"images,rootdir"},
		{"storage":"local","type":"dir","content":"iso,vztmpl,backup"},
		{"storage":"other-node","type":"zfspool","pool":"tank","content":"images","nodes":"pve2"}
	]`,
	"nodes/pve1/storage": `[{"storage":"local-zfs","total":1000,"used":400,"avail":600,"active":1}]`,
	"nodes/pve1/qemu": `[
		{"vmid":201,"name":"app","status":"running"},
		{"vmid":202,"name":"legacy","status":"stopped"},
		{"vmid":203,"name":"broken","status":"stopped"}
	]`,
	"nodes/pve1/qemu/201/config": `{
		"name":"app","memory":"current=2048","cores":2,"sockets":2,"onboot":1,"startup":"order=1,up=30",
		"tags":"web;prod","description":"root password is ` + secret + `","cipassword":"` + secret + `",
		"sshkeys":"ssh-ed25519%20` + secret + `",
		"scsi0":"local-zfs:vm-201-disk-0,discard=on,size=32G",
		"scsi10":"local-zfs:vm-201-disk-2,size=1T",
		"scsi2":"local-zfs:vm-201-disk-1,size=512M",
		"efidisk0":"local-zfs:vm-201-disk-3,efitype=4m,size=528K",
		"ide2":"local-zfs:vm-201-cloudinit,media=cdrom",
		"ide0":"local:iso/debian-13.iso,media=cdrom,size=700M",
		"unused0":"local-zfs:vm-201-disk-9",
		"net0":"virtio=BC:24:11:48:39:FB,bridge=vmbr1,tag=20,firewall=1",
		"hostpci0":"0000:01:00.0"
	}`,
	"nodes/pve1/qemu/202/config": `{"name":"legacy","memory":"1024","scsi0":"local-lvm:vm-202-disk-0,size=8G","sata0":"/dev/disk/by-id/ata-EXAMPLE,size=100G","net0":"e1000=BC:24:11:00:00:02,bridge=vmbr0"}`,
	"nodes/pve1/lxc":             `[{"vmid":101,"name":"web","status":"running"}]`,
	"nodes/pve1/lxc/101/config": `{
		"hostname":"web","memory":512,"cores":1,"description":"` + secret + `",
		"rootfs":"local-zfs:subvol-101-disk-0,size=4G",
		"mp0":"local-zfs:subvol-101-disk-1,mp=/data,size=8G",
		"mp1":"/srv/shared,mp=/shared",
		"net0":"name=eth0,bridge=vmbr1,hwaddr=BC:24:11:D3:77:16,ip=192.0.2.11/24,tag=10,type=veth"
	}`,
	"nodes/pve1/disks/zfs": `[{"name":"rpool","health":"ONLINE","size":1000,"alloc":400,"free":600,"frag":3}]`,
	"nodes/pve1/network": `[
		{"iface":"vmbr0","type":"bridge","bridge_ports":"nic0","bridge_vlan_aware":1,"cidr":"192.0.2.10/24","gateway":"192.0.2.1","active":1,"autostart":1,"comments":"` + secret + `"},
		{"iface":"nic0","type":"eth","active":1},
		{"iface":"vmbr0.30","type":"vlan","vlan-id":"30","vlan-raw-device":"vmbr0","active":1},
		{"iface":"bond0","type":"bond","slaves":"nic1 nic2","active":0}
	]`,
}

const zfsList = "rpool/data\tfilesystem\t4000\t100\t-\ton\toff\n" +
	"rpool/data/vm-201-disk-0\tvolume\t3000\t2000\t34359738368\ton\taes-256-gcm\n"

func sources(api API) Sources {
	return Sources{
		API:        api,
		ZFSList:    func(context.Context) ([]byte, error) { return []byte(zfsList), nil },
		ZFSVersion: func() string { return "2.3.4-pve1" },
		Hostname:   func() (string, error) { return "pve1.example.com", nil },
		Now:        func() time.Time { return time.Unix(1000, 0) },
	}
}

func guest(t *testing.T, inv *inventoryv1.Inventory, vmid uint32) *inventoryv1.Guest {
	t.Helper()
	for _, g := range inv.Guests {
		if g.Vmid == vmid {
			return g
		}
	}
	t.Fatalf("guest %d not found", vmid)
	return nil
}

func disk(t *testing.T, g *inventoryv1.Guest, key string) *inventoryv1.Disk {
	t.Helper()
	for _, d := range g.Disks {
		if d.Key == key {
			return d
		}
	}
	t.Fatalf("guest %d: disk %s not found", g.Vmid, key)
	return nil
}

func TestCollect(t *testing.T) {
	inv, err := Collect(context.Background(), sources(fixture))
	if err != nil {
		t.Fatal(err)
	}

	if h := inv.Host; h.Hostname != "pve1" || h.Cpus != 8 || h.MemoryBytes != 32<<30 || h.ZfsVersion != "2.3.4-pve1" {
		t.Errorf("host = %+v", h)
	}

	// Storage on other nodes is excluded.
	if len(inv.Storages) != 3 {
		t.Errorf("got %d storages, want 3", len(inv.Storages))
	}

	// VM 203's configuration can't be read: it's a warning, not a failure.
	if len(inv.Guests) != 3 || len(inv.Warnings) != 1 {
		t.Fatalf("guests = %d, warnings = %v", len(inv.Guests), inv.Warnings)
	}

	vm := guest(t, inv, 201)
	if vm.Name != "app" || vm.Cores != 4 || vm.MemoryBytes != 2048<<20 || !vm.Onboot || len(vm.Tags) != 2 {
		t.Errorf("VM 201 = %+v", vm)
	}
	if d := disk(t, vm, "scsi0"); d.Readiness != inventoryv1.Readiness_READINESS_REPLICABLE ||
		d.ZfsDataset != "rpool/data/vm-201-disk-0" || d.SizeBytes != 32<<30 {
		t.Errorf("scsi0 = %+v", d)
	}
	if d := disk(t, vm, "efidisk0"); d.Readiness != inventoryv1.Readiness_READINESS_REPLICABLE {
		t.Errorf("efidisk0 = %+v", d)
	}
	for _, k := range []string{"ide2", "ide0", "unused0"} {
		if d := disk(t, vm, k); d.Readiness != inventoryv1.Readiness_READINESS_NOT_NEEDED {
			t.Errorf("%s = %+v, want not needed", k, d)
		}
	}
	// Natural ordering: scsi2 before scsi10.
	var keys []string
	for _, d := range vm.Disks {
		keys = append(keys, d.Key)
	}
	if fmt.Sprint(keys) != "[efidisk0 ide0 ide2 scsi0 scsi2 scsi10 unused0]" {
		t.Errorf("disk order = %v", keys)
	}
	if !vm.Ready || !vm.Passthrough || len(vm.ReadinessWarnings) != 2 {
		t.Errorf("VM 201 ready=%v passthrough=%v warnings=%v", vm.Ready, vm.Passthrough, vm.ReadinessWarnings)
	}
	if n := vm.Nics[0]; n.Model != "virtio" || n.Mac != "BC:24:11:48:39:FB" || n.Bridge != "vmbr1" || n.VlanTag != 20 || !n.Firewall {
		t.Errorf("VM NIC = %+v", n)
	}

	legacy := guest(t, inv, 202)
	if legacy.Ready {
		t.Error("VM 202 on LVM should not be ready")
	}
	if d := disk(t, legacy, "scsi0"); d.Readiness != inventoryv1.Readiness_READINESS_NOT_REPLICABLE || d.Reason == "" {
		t.Errorf("LVM disk = %+v", d)
	}
	if d := disk(t, legacy, "sata0"); d.Reason != "passthrough device" {
		t.Errorf("passthrough disk = %+v", d)
	}

	ct := guest(t, inv, 101)
	if ct.Type != inventoryv1.GuestType_GUEST_TYPE_CONTAINER || ct.Name != "web" || ct.Ready {
		t.Errorf("CT 101 = %+v", ct)
	}
	if d := disk(t, ct, "mp1"); d.Reason != "bind mount of a host path" {
		t.Errorf("bind mount = %+v", d)
	}
	if n := ct.Nics[0]; n.Mac != "BC:24:11:D3:77:16" || n.VlanTag != 10 {
		t.Errorf("CT NIC = %+v", n)
	}

	if len(inv.ZfsDatasets) != 2 || inv.ZfsDatasets[1].Encryption != "aes-256-gcm" || inv.ZfsDatasets[1].VolumeSizeBytes != 32<<30 {
		t.Errorf("datasets = %+v", inv.ZfsDatasets)
	}
	if len(inv.Interfaces) != 4 {
		t.Fatalf("interfaces = %+v", inv.Interfaces)
	}
	byName := map[string]*inventoryv1.NetworkInterface{}
	for _, i := range inv.Interfaces {
		byName[i.Name] = i
	}
	if i := byName["vmbr0"]; !i.VlanAware || i.Gateway != "192.0.2.1" || i.BridgePorts[0] != "nic0" {
		t.Errorf("vmbr0 = %+v", i)
	}
	if i := byName["vmbr0.30"]; i.VlanId != 30 || i.VlanRawDevice != "vmbr0" {
		t.Errorf("vlan = %+v", i)
	}
	if i := byName["bond0"]; len(i.BondMembers) != 2 {
		t.Errorf("bond = %+v", i)
	}
}

// Notes, cloud-init credentials, and interface comments must never be sent.
func TestCollectRedacts(t *testing.T) {
	inv, err := Collect(context.Background(), sources(fixture))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := proto.Marshal(inv)
	if bytes.Contains(b, []byte(secret)) {
		t.Fatal("inventory contains redacted content")
	}
}

func TestCollectIsStable(t *testing.T) {
	a, _ := Collect(context.Background(), sources(fixture))
	src := sources(fixture)
	src.Now = func() time.Time { return time.Unix(2000, 0) }
	b, _ := Collect(context.Background(), src)
	if !bytes.Equal(inventory.Hash(a), inventory.Hash(b)) {
		t.Error("identical hosts produced different hashes")
	}
}

func TestCollectFailsWithoutNodes(t *testing.T) {
	if _, err := Collect(context.Background(), sources(fakeAPI{})); err == nil {
		t.Fatal("expected an error when nodes can't be listed")
	}
	src := sources(fixture)
	src.ZFSList = func(context.Context) ([]byte, error) { return nil, errors.New("zfs failed") }
	inv, err := Collect(context.Background(), src)
	if err != nil || len(inv.Warnings) != 2 {
		t.Fatalf("zfs failure should be a warning: %v, %v", err, inv.GetWarnings())
	}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]uint64{"4G": 4 << 30, "512M": 512 << 20, "528K": 528 << 10, "1T": 1 << 40, "1.5G": 3 << 29, "": 0, "x": 0} {
		if got := parseSize(in); got != want {
			t.Errorf("parseSize(%q) = %d, want %d", in, got, want)
		}
	}
}
