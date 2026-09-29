package guestconfig

import (
	"cmp"
	"strings"
	"testing"
)

const vmConf = `#Application server
#owned by ops
agent: 1
boot: order=scsi0
ciuser: ezdr
hostpci0: 0000:01:00.0
ide0: local:iso/debian.iso,media=cdrom
ide1: nfs-isos:iso/tools.iso,media=cdrom
ide2: local-zfs:vm-201-cloudinit,media=cdrom
lock: backup
memory: 1024
name: app-vm
net0: virtio=BC:24:11:48:39:FB,bridge=vmbr1,tag=20
onboot: 1
scsi0: local-zfs:vm-201-disk-0,discard=on,size=4G
efidisk0: local-zfs:vm-201-disk-1,efitype=4m,size=1M
smbios1: uuid=0eaeb7c4-b95a-4737-985d-71d881b33d33
tags: web;prod
unused0: local-zfs:vm-201-disk-9
vmgenid: eb9d7918-6f53-4149-8656-7c829f72d19c
`

func TestRewriteVM(t *testing.T) {
	res, err := Rewrite(vmConf, Mapping{
		Type: "qemu",
		Volumes: map[string]string{
			"local-zfs:vm-201-disk-0": "ezdr-test-tank:vm-10201-disk-0",
			"local-zfs:vm-201-disk-1": "ezdr-test-tank:vm-10201-disk-1",
		},
		Bridge: "vmbr99", Storages: []string{"local", "tank"}, Name: "test-app-vm", Tag: "ezdr-test",
		Note: "EZDR test failover", VMGenID: "0a1e5c64-6f21-44fe-b69b-7fcbd91fd01d",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `#EZDR test failover
#
#Application server
#owned by ops
agent: 1
boot: order=scsi0
ciuser: ezdr
efidisk0: ezdr-test-tank:vm-10201-disk-1,efitype=4m,size=1M
ide0: local:iso/debian.iso,media=cdrom
ide1: none,media=cdrom
memory: 1024
name: test-app-vm
net0: virtio=BC:24:11:48:39:FB,bridge=vmbr99,tag=20
onboot: 0
scsi0: ezdr-test-tank:vm-10201-disk-0,discard=on,size=4G
smbios1: uuid=0eaeb7c4-b95a-4737-985d-71d881b33d33
tags: web;prod;ezdr-test
vmgenid: 0a1e5c64-6f21-44fe-b69b-7fcbd91fd01d
`
	if res.Config != want {
		t.Errorf("config:\n%s\nwant:\n%s", res.Config, want)
	}
	if res.CloudInit != "ide2" {
		t.Errorf("cloud-init = %q", res.CloudInit)
	}
	if got := strings.Join(res.Removed, "|"); got != "hostpci0: 0000:01:00.0|ide1: nfs-isos:iso/tools.iso,media=cdrom" {
		t.Errorf("removed = %s", got)
	}
}

func TestRewriteContainer(t *testing.T) {
	conf := "arch: amd64\nhostname: web-test\nmp0: /srv/data,mp=/data\nnet0: name=eth0,bridge=vmbr1,hwaddr=BC:24:11:D3:77:16,ip=192.168.10.11/24,tag=10,type=veth\n" +
		"rootfs: local-zfs:subvol-101-disk-0,size=4G\nunprivileged: 1\n"
	res, err := Rewrite(conf, Mapping{
		Type:    "lxc",
		Volumes: map[string]string{"local-zfs:subvol-101-disk-0": "ezdr-test-tank:subvol-10101-disk-0"},
		Bridge:  "vmbr99", Name: "ignored", Tag: "ezdr-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "arch: amd64\nhostname: web-test\nnet0: name=eth0,bridge=vmbr99,hwaddr=BC:24:11:D3:77:16,ip=192.168.10.11/24,tag=10,type=veth\n" +
		"onboot: 0\nrootfs: ezdr-test-tank:subvol-10101-disk-0,size=4G\ntags: ezdr-test\nunprivileged: 1\n"
	if res.Config != want {
		t.Errorf("config:\n%s\nwant:\n%s", res.Config, want)
	}
	if len(res.Removed) != 1 || !strings.HasPrefix(res.Removed[0], "mp0:") {
		t.Errorf("removed = %v", res.Removed)
	}
}

func TestRewriteErrors(t *testing.T) {
	base := Mapping{Type: "qemu", VMGenID: "0a1e5c64-6f21-44fe-b69b-7fcbd91fd01d", Bridge: "vmbr99"}
	// A disk that wasn't replicated.
	if _, err := Rewrite("scsi0: local-lvm:vm-201-disk-0,size=4G\n", base); err == nil || !strings.Contains(err.Error(), "wasn't replicated") {
		t.Errorf("unreplicated disk: %v", err)
	}
	// A mapped volume the configuration doesn't have (the configuration
	// changed since the disks were chosen).
	m := base
	m.Volumes = map[string]string{"local-zfs:vm-201-disk-3": "x:vm-10201-disk-3"}
	if _, err := Rewrite("name: a\n", m); err == nil || !strings.Contains(err.Error(), "isn't in the configuration") {
		t.Errorf("missing volume: %v", err)
	}
	m = base
	m.VMGenID = "1"
	if _, err := Rewrite("name: a\n", m); err == nil {
		t.Error("accepted an invalid generation ID")
	}
	if _, err := Rewrite("garbage\n", base); err == nil {
		t.Error("accepted an unreadable line")
	}
	// Values set from elsewhere can't add lines or options.
	for _, bad := range []Mapping{
		{Bridge: "vmbr0\nargs: -x"}, {Bridge: "vmbr0,firewall=0"}, {Bridges: map[string]string{"vmbr1": "a b"}},
		{Tag: "t\nargs: -x"}, {Name: "a\nargs: -x"},
	} {
		m := base
		m.Bridge, m.Bridges, m.Tag, m.Name = cmp.Or(bad.Bridge, base.Bridge), bad.Bridges, bad.Tag, bad.Name
		if _, err := Rewrite("name: a\n", m); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	m = base
	m.Note = "from pve1\nargs: -x\r"
	res, err := Rewrite("name: a\n", m)
	if err != nil || res.Config != "#from pve1 args: -x\nname: a\nonboot: 0\nvmgenid: "+base.VMGenID+"\n" {
		t.Errorf("note not kept on one line: %q, %v", res.Config, err)
	}
}

func TestRewriteForFailover(t *testing.T) {
	conf := "name: app\nnet0: virtio=BC:24:11:48:39:FB,bridge=vmbr1,tag=20\nnet1: virtio=BC:24:11:48:39:FC,bridge=vmbr5\n" +
		"onboot: 1\nscsi0: local-zfs:vm-201-disk-0,size=4G\n"
	m := Mapping{
		Type: "qemu", Volumes: map[string]string{"local-zfs:vm-201-disk-0": "ezdr-plan-local-zfs:vm-201-disk-0"},
		Bridges: map[string]string{"vmbr1": "vmbr2", "vmbr5": "vmbr6"}, KeepOnboot: true, Tag: "ezdr-failover",
		VMGenID: "0a1e5c64-6f21-44fe-b69b-7fcbd91fd01d",
	}
	res, err := Rewrite(conf, m)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"name: app\n", "bridge=vmbr2,tag=20", "bridge=vmbr6", "onboot: 1\n", "tags: ezdr-failover",
		"scsi0: ezdr-plan-local-zfs:vm-201-disk-0,size=4G"} {
		if !strings.Contains(res.Config, want) {
			t.Errorf("missing %q in:\n%s", want, res.Config)
		}
	}
	if strings.Count(res.Config, "onboot") != 1 {
		t.Errorf("onboot duplicated:\n%s", res.Config)
	}
	delete(m.Bridges, "vmbr5")
	if _, err := Rewrite(conf, m); err == nil || !strings.Contains(err.Error(), `bridge "vmbr5" isn't mapped`) {
		t.Errorf("unmapped bridge: %v", err)
	}
}

func TestDisks(t *testing.T) {
	got := Disks("qemu", vmConf)
	if len(got) != 2 || got[0].Volume != "local-zfs:vm-201-disk-0" || got[0].Size != "4G" || got[1].Key != "efidisk0" {
		t.Errorf("VM disks = %v", got)
	}
	ct := Disks("lxc", "rootfs: local-zfs:subvol-101-disk-0,size=4G\nmp0: /srv,mp=/data\nmp1: local-zfs:subvol-101-disk-1,mp=/x,size=8G\n")
	if len(ct) != 2 || ct[1].Volume != "local-zfs:subvol-101-disk-1" || ct[1].Size != "8G" {
		t.Errorf("container volumes = %v", ct)
	}
}
