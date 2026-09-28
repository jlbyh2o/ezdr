package testfailover

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jlbyh2o/ezdr/internal/client/guests"
	clientv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/client/v1"
)

// fakeHost records commands and keeps a tiny model of ZFS clones and guest
// states.
type fakeHost struct {
	calls   []string
	clones  map[string]string // dataset -> origin
	running map[string]bool   // "qm 10201" -> running
	agentOK bool
}

func (f *fakeHost) run(_ context.Context, name string, args ...string) ([]byte, error) {
	call := strings.Join(append([]string{name}, args...), " ")
	f.calls = append(f.calls, call)
	last := args[len(args)-1]
	switch {
	case call == "zfs list -Hp -t snapshot -o name,creation -d 1 tank/rep/rpool/vm-201-disk-0":
		return []byte("tank/rep/rpool/vm-201-disk-0@zrepl_1\t100\ntank/rep/rpool/vm-201-disk-0@zrepl_2\t200\ntank/rep/rpool/vm-201-disk-0@manual\t150\n"), nil
	case call == "zfs list -Hp -t snapshot -o name,creation -d 1 tank/rep/rpool/subvol-101-disk-0":
		return []byte("tank/rep/rpool/subvol-101-disk-0@zrepl_2\t200\n"), nil
	case strings.HasPrefix(call, "zfs get -H -o value origin "):
		if o, ok := f.clones[last]; ok {
			return []byte(o + "\n"), nil
		}
		return nil, errors.New("dataset does not exist")
	case strings.HasPrefix(call, "zfs clone "):
		f.clones[last] = args[1]
	case strings.HasPrefix(call, "zfs destroy "):
		delete(f.clones, last)
	case strings.HasPrefix(call, "zfs list -H -o name "):
		return nil, errors.New("dataset does not exist")
	case len(args) == 2 && args[0] == "status":
		if f.running[name+" "+last] {
			return []byte("status: running\n"), nil
		}
		return []byte("status: stopped\n"), nil
	case len(args) == 2 && args[0] == "start":
		f.running[name+" "+last] = true
	case len(args) == 2 && args[0] == "stop":
		f.running[name+" "+last] = false
	case strings.HasSuffix(call, " ping"):
		if !f.agentOK {
			return nil, errors.New("agent not running")
		}
	}
	return nil, nil
}

func (f *fakeHost) did(prefix string) bool {
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func setup(t *testing.T) (*Runner, *fakeHost, *clientv1.TestPrepare) {
	t.Helper()
	dir := t.TempDir()
	pve := filepath.Join(dir, "pve")
	for _, d := range []string{"qemu-server", "lxc"} {
		if err := os.MkdirAll(filepath.Join(pve, d), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path, data string) {
		if err := os.WriteFile(filepath.Join(pve, path), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("storage.cfg", "dir: local\n\tpath /var/lib/vz\n\nzfspool: tank\n\tpool tank\n")
	write(".vmlist", `{"version":1,"ids":{"100":{"node":"dr1","type":"qemu"}}}`)
	write("meminfo", "MemTotal: 8000000 kB\nMemAvailable:    4000000 kB\n")
	gp := guests.Paths{PVE: pve, Plans: filepath.Join(dir, "plans")}
	at := timestamppb.New(time.Unix(1, 0))
	if err := guests.Store(gp, []*clientv1.PlanGuestConfigs{{PlanId: "plan1", PlanName: "Main", Guests: []*clientv1.GuestConfig{
		{Vmid: 201, Type: "qemu", ChangedAt: at, Config: "agent: 1\nide2: local-zfs:vm-201-cloudinit,media=cdrom\nname: app\n" +
			"net0: virtio=BC:24:11:48:39:FB,bridge=vmbr1,tag=20\nscsi0: local-zfs:vm-201-disk-0,size=4G\n"},
		{Vmid: 101, Type: "lxc", ChangedAt: at, Config: "hostname: web\nnet0: name=eth0,bridge=vmbr1,type=veth\nrootfs: local-zfs:subvol-101-disk-0,size=4G\n"},
	}}}); err != nil {
		t.Fatal(err)
	}
	f := &fakeHost{clones: map[string]string{}, running: map[string]bool{}, agentOK: true}
	r := &Runner{PVE: pve, Guests: gp, MemInfo: filepath.Join(pve, "meminfo"), Run: f.run, Node: func() (string, error) { return "dr1", nil }}
	p := &clientv1.TestPrepare{PlanId: "plan1", PlanName: "Main", TestId: "t1", Snapshot: "zrepl_2", Bridge: "vmbr99", Guests: []*clientv1.TestGuest{
		{Vmid: 201, TestVmid: 10201, Type: "qemu", Disks: []*clientv1.TestDisk{
			{Volume: "local-zfs:vm-201-disk-0", Replica: "tank/rep/rpool/vm-201-disk-0", Clone: "vm-10201-disk-0"}}},
		{Vmid: 101, TestVmid: 10101, Type: "lxc", Disks: []*clientv1.TestDisk{
			{Volume: "local-zfs:subvol-101-disk-0", Replica: "tank/rep/rpool/subvol-101-disk-0", Clone: "subvol-10101-disk-0"}}},
	}}
	return r, f, p
}

func TestOptions(t *testing.T) {
	r, _, _ := setup(t)
	res, err := r.Options(context.Background(), []string{"tank/rep/rpool/vm-201-disk-0", "tank/rep/rpool/subvol-101-disk-0"}, "zrepl_")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Snapshots) != 1 || res.Snapshots[0].Name != "zrepl_2" || res.MemoryAvailableBytes != 4000000<<10 {
		t.Errorf("options = %v", res)
	}
}

func TestPrepareStartCheckCleanup(t *testing.T) {
	ctx := context.Background()
	r, f, p := setup(t)
	if _, err := r.Prepare(ctx, p); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"zfs create tank/ezdr-test",
		"pvesm add zfspool ezdr-test-tank --pool tank/ezdr-test --content images,rootdir --sparse 1 --nodes dr1",
		"zfs clone tank/rep/rpool/vm-201-disk-0@zrepl_2 tank/ezdr-test/vm-10201-disk-0",
		"qm set 10201 --ide2 ezdr-test-tank:cloudinit",
	} {
		if !f.did(want) {
			t.Errorf("missing %q in %v", want, f.calls)
		}
	}
	vm, _ := os.ReadFile(r.configPath("qemu", 10201))
	for _, want := range []string{"#Test failover of guest 201 from plan Main. EZDR test ID: t1", "name: test-app",
		"scsi0: ezdr-test-tank:vm-10201-disk-0,size=4G", "bridge=vmbr99", "tags: ezdr-test"} {
		if !strings.Contains(string(vm), want) {
			t.Errorf("VM config lacks %q:\n%s", want, vm)
		}
	}

	// Preparing again (for example, after a lost acknowledgement) changes
	// nothing: the guests are registered and the clones exist.
	if err := os.WriteFile(filepath.Join(r.PVE, ".vmlist"), []byte(`{"ids":{"10201":{},"10101":{}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	f.calls = nil
	if _, err := r.Prepare(ctx, p); err != nil {
		t.Fatal(err)
	}
	if f.did("zfs clone") || f.did("qm set") {
		t.Errorf("repeat changed things: %v", f.calls)
	}

	if err := r.StartGuest(ctx, "t1", "qemu", 10201); err != nil {
		t.Fatal(err)
	}
	c, err := r.CheckGuest(ctx, "t1", "qemu", 10201)
	if err != nil || !c.Running || !c.AgentEnabled || !c.AgentOk {
		t.Errorf("check = %v, %v", c, err)
	}
	// Another test's ID doesn't match these guests.
	if err := r.StartGuest(ctx, "t2", "qemu", 10201); err == nil {
		t.Error("started a guest of another test")
	}

	done, err := r.Cleanup(ctx, &clientv1.TestCleanup{TestId: "t1", Guests: p.Guests})
	if err != nil {
		t.Fatal(err)
	}
	if !f.did("qm stop 10201") || !f.did("qm destroy 10201 --purge 1") || !f.did("pct destroy 10101 --purge 1") || len(done) != 4 {
		t.Errorf("cleanup = %v, calls %v", done, f.calls)
	}
}

func TestRefusesOtherGuests(t *testing.T) {
	ctx := context.Background()
	r, f, p := setup(t)
	// A real guest already has the test ID.
	if err := os.WriteFile(filepath.Join(r.PVE, ".vmlist"), []byte(`{"ids":{"10201":{}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.configPath("qemu", 10201), []byte("name: important\nscsi0: tank:vm-10201-disk-0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Prepare(ctx, p); err == nil || !strings.Contains(err.Error(), "already in use") {
		t.Errorf("prepare over a real guest: %v", err)
	}
	if _, err := r.Cleanup(ctx, &clientv1.TestCleanup{TestId: "t1", Guests: p.Guests[:1]}); err == nil {
		t.Error("cleanup accepted a guest that isn't a test guest")
	}
	// Even a guest carrying the test's marker and tag is refused if a disk
	// is outside the test storage.
	if err := os.WriteFile(r.configPath("qemu", 10201), []byte("#EZDR test ID: t1\ntags: ezdr-test\nscsi0: tank:vm-10201-disk-0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Cleanup(ctx, &clientv1.TestCleanup{TestId: "t1", Guests: p.Guests[:1]}); err == nil || !strings.Contains(err.Error(), "outside the test storage") {
		t.Errorf("cleanup of a guest with a real disk: %v", err)
	}
	if f.did("qm destroy") || f.did("qm stop") {
		t.Errorf("touched a guest that isn't ours: %v", f.calls)
	}
	// A dataset at a clone's name that isn't a clone of the replica is kept.
	f.clones["tank/ezdr-test/subvol-10101-disk-0"] = "tank/other@x"
	if _, err := r.Cleanup(ctx, &clientv1.TestCleanup{TestId: "t1", Guests: p.Guests[1:]}); err == nil {
		t.Error("destroyed a dataset that isn't the test's clone")
	}
}
