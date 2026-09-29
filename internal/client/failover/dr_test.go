package failover

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

type drFake struct {
	fake
	datasets map[string]bool
	// snapshots are the snapshots of a dataset, as zfs lists them.
	snapshots map[string]string
}

func (f *drFake) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	call := strings.Join(append([]string{name}, args...), " ")
	switch {
	case strings.HasPrefix(call, "zfs list -H -o name "):
		f.calls = append(f.calls, call)
		if !f.datasets[args[len(args)-1]] {
			return nil, errors.New("dataset does not exist")
		}
		return nil, nil
	case strings.HasPrefix(call, "zfs list -H -t snapshot -o name -d 1 "):
		f.calls = append(f.calls, call)
		return []byte(f.snapshots[args[len(args)-1]]), nil
	case strings.HasPrefix(call, "zfs receive -A "):
		f.calls = append(f.calls, call)
		return nil, errors.New("exit status 1: 'x' does not have any resumable receive state to abort")
	}
	return f.fake.run(ctx, name, args...)
}

func drRunner(t *testing.T) (*Runner, *drFake) {
	t.Helper()
	r, _ := testRunner(t)
	f := &drFake{fake: fake{running: map[string]bool{}}, datasets: map[string]bool{
		"tank/ezdr/pve1/rpool/data/vm-201-disk-0":     true,
		"tank/ezdr/pve1/rpool/data/subvol-101-disk-0": true,
	}}
	r.Run, r.Node = f.run, func() (string, error) { return "dr1", nil }
	dir := t.TempDir()
	r.Guests = guests.Paths{PVE: r.PVE, Plans: filepath.Join(dir, "plans")}
	write(t, filepath.Join(r.PVE, "storage.cfg"), "dir: local\n\tpath /var/lib/vz\n")
	write(t, filepath.Join(r.PVE, ".vmlist"), `{"ids":{"100":{}}}`)
	at := timestamppb.New(time.Unix(1, 0))
	if err := guests.Store(r.Guests, []*clientv1.PlanGuestConfigs{{PlanId: "plan1", PlanName: "Main", PrimaryHostname: "pve1",
		Guests: []*clientv1.GuestConfig{
			{Vmid: 201, Type: "qemu", ChangedAt: at, Config: "ide2: local-zfs:vm-201-cloudinit,media=cdrom\nname: app\n" +
				"net0: virtio=BC:24:11:48:39:FB,bridge=vmbr1,tag=20\nonboot: 1\nscsi0: local-zfs:vm-201-disk-0,size=4G\n"},
			{Vmid: 101, Type: "lxc", ChangedAt: at, Config: "hostname: web\nnet0: name=eth0,bridge=vmbr1,type=veth\n" +
				"rootfs: local-zfs:subvol-101-disk-0,size=4G\n"},
		}}}, []*clientv1.PlanRecovery{{PlanId: "plan1", PlanName: "Main", PrimaryHostname: "pve1",
		Storages: []*clientv1.RecoveryStorage{{SourceStorage: "local-zfs", SourceDataset: "rpool/data",
			ReceiveDataset: "tank/ezdr/pve1", StorageId: "ezdr-plan1-local-zfs"}},
		Bridges: []*clientv1.RecoveryBridge{{SourceBridge: "vmbr1", TargetBridge: "vmbr2"}},
	}}); err != nil {
		t.Fatal(err)
	}
	return r, f
}

func TestPrepareOnDR(t *testing.T) {
	ctx := context.Background()
	r, f := drRunner(t)
	if _, err := r.Prepare(ctx, "plan1", []uint32{201, 101}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"pvesm add zfspool ezdr-plan1-local-zfs --pool tank/ezdr/pve1/rpool/data --content images,rootdir --sparse 1 --nodes dr1",
		"zfs set readonly=off tank/ezdr/pve1/rpool/data/vm-201-disk-0",
		"zfs set refquota=4G tank/ezdr/pve1/rpool/data/subvol-101-disk-0",
		"qm set 201 --ide2 ezdr-plan1-local-zfs:cloudinit", // no replica of the drive: a fresh one
	} {
		found := false
		for _, c := range f.calls {
			found = found || c == want
		}
		if !found {
			t.Errorf("missing %q in:\n%s", want, strings.Join(f.calls, "\n"))
		}
	}
	vm, _ := os.ReadFile(r.configPath("qemu", 201))
	for _, want := range []string{"ezdr-failover-plan1", "name: app\n", "bridge=vmbr2,tag=20", "onboot: 1\n",
		"scsi0: ezdr-plan1-local-zfs:vm-201-disk-0,size=4G", "tags: ezdr-failover"} {
		if !strings.Contains(string(vm), want) {
			t.Errorf("VM config lacks %q:\n%s", want, vm)
		}
	}

	// Repeating skips the registered guests (qm set added the cloud-init
	// drive, which the fake doesn't do).
	write(t, filepath.Join(r.PVE, ".vmlist"), `{"ids":{"201":{},"101":{}}}`)
	write(t, r.configPath("qemu", 201), string(vm)+"ide2: ezdr-plan1-local-zfs:vm-201-cloudinit,media=cdrom\n")
	f.calls = nil
	if _, err := r.Prepare(ctx, "plan1", []uint32{201, 101}); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "qm set") || strings.HasPrefix(c, "zfs set") {
			t.Errorf("repeat changed things: %s", c)
		}
	}
	if err := r.StartGuest(ctx, "plan1", 201); err != nil {
		t.Fatal(err)
	}
	if err := r.StartGuest(ctx, "other", 201); err == nil {
		t.Error("started a guest of another plan")
	}
}

func TestPrepareRefusesTakenIDs(t *testing.T) {
	r, _ := drRunner(t)
	write(t, filepath.Join(r.PVE, ".vmlist"), `{"ids":{"201":{}}}`)
	write(t, r.configPath("qemu", 201), "name: somebody-else\n")
	if _, err := r.Prepare(context.Background(), "plan1", []uint32{201}); err == nil || !strings.Contains(err.Error(), "ID in use") {
		t.Errorf("err = %v", err)
	}
}

func TestSnapshotAndReplicate(t *testing.T) {
	r, f := drRunner(t)
	if err := r.Snapshot(context.Background(), []string{"rpool/data/vm-201-disk-0"}, "zrepl_20260928_031500_000"); err != nil {
		t.Fatal(err)
	}
	if err := r.Replicate(context.Background(), []string{"ezdr_plan1_local-zfs_pull"}); err != nil {
		t.Fatal(err)
	}
	calls := strings.Join(f.calls, "\n")
	if !strings.Contains(calls, "zfs snapshot rpool/data/vm-201-disk-0@zrepl_20260928_031500_000") ||
		!strings.Contains(calls, "zrepl signal wakeup ezdr_plan1_local-zfs_pull") {
		t.Errorf("calls:\n%s", calls)
	}
	if err := r.Replicate(context.Background(), []string{"not-ezdr; rm"}); err == nil {
		t.Error("accepted an invalid job")
	}
}

// A replica of the cloud-init drive (for example, from an earlier
// hand-written setup) is reused, including by a retry after an attempt that
// stopped before the drive.
func TestPrepareReusesCloudInitReplica(t *testing.T) {
	ctx := context.Background()
	r, f := drRunner(t)
	f.datasets["tank/ezdr/pve1/rpool/data/vm-201-cloudinit"] = true
	write(t, filepath.Join(r.PVE, ".vmlist"), `{"ids":{"201":{}}}`)
	write(t, r.configPath("qemu", 201), "#ezdr-failover-plan1\nname: app\ntags: ezdr-failover\nscsi0: ezdr-plan1-local-zfs:vm-201-disk-0\n")
	if _, err := r.Prepare(ctx, "plan1", []uint32{201}); err != nil {
		t.Fatal(err)
	}
	calls := strings.Join(f.calls, "\n")
	if !strings.Contains(calls, "zfs set readonly=off tank/ezdr/pve1/rpool/data/vm-201-cloudinit") ||
		!strings.Contains(calls, "qm set 201 --ide2 ezdr-plan1-local-zfs:vm-201-cloudinit,media=cdrom") {
		t.Errorf("calls:\n%s", calls)
	}
}

func TestBreakGlassMarkersAndJobs(t *testing.T) {
	ctx := context.Background()
	r, f := drRunner(t)
	jobs := filepath.Join(t.TempDir(), "ezdr.yml")
	write(t, jobs, "jobs:\n- name: ezdr_plan1_local-zfs_pull\n  type: pull\n- name: ezdr_other12_local-zfs_pull\n  type: pull\n")
	n, err := r.RemovePlanJobs(ctx, jobs, "plan1")
	if err != nil || n != 1 {
		t.Fatalf("removed %d, %v", n, err)
	}
	b, _ := os.ReadFile(jobs) //nolint:gosec // test file
	if strings.Contains(string(b), "ezdr_plan1_") || !strings.Contains(string(b), "ezdr_other12_") {
		t.Errorf("jobs file:\n%s", b)
	}
	if !strings.Contains(strings.Join(f.calls, "\n"), "systemctl restart zrepl") {
		t.Error("zrepl not restarted")
	}

	if err := r.WriteMarker(Marker{PlanID: "plan1", PlanName: "Main", User: "root", Started: []uint32{201}}); err != nil {
		t.Fatal(err)
	}
	ms, err := r.Markers()
	if err != nil || len(ms) != 1 || ms[0].Started[0] != 201 {
		t.Fatalf("markers = %v, %v", ms, err)
	}
	if err := r.RemoveMarker("plan1"); err != nil {
		t.Fatal(err)
	}
	if ms, _ := r.Markers(); len(ms) != 0 {
		t.Errorf("marker kept: %v", ms)
	}
}

func TestCloudInitDrive(t *testing.T) {
	for conf, want := range map[string]string{
		"scsi0: local-zfs:vm-201-disk-0\nide2: local-zfs:vm-201-cloudinit,media=cdrom\n": "ide2",
		"ide2: local:iso/debian.iso,media=cdrom\n":                                       "",
		// The key becomes a qm set option: nothing but a drive key.
		"args: local-zfs:vm-201-cloudinit,media=cdrom\n": "",
	} {
		if key, _ := cloudInitDrive(conf); key != want {
			t.Errorf("cloudInitDrive(%q) = %q, want %q", conf, key, want)
		}
	}
}
