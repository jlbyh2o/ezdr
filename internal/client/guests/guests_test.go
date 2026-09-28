package guests

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	clientv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/client/v1"
)

func testPaths(t *testing.T) Paths {
	dir := t.TempDir()
	p := Paths{PVE: filepath.Join(dir, "pve"), Plans: filepath.Join(dir, "plans")}
	for _, d := range []string{"qemu-server", "lxc"} {
		if err := os.MkdirAll(filepath.Join(p.PVE, d), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

func TestRead(t *testing.T) {
	p := testPaths(t)
	vm := "boot: order=scsi0\nname: app\nscsi0: local-zfs:vm-201-disk-0,size=4G\n\n[PENDING]\nmemory: 2048\n\n[before-upgrade]\nname: old\n"
	if err := os.WriteFile(filepath.Join(p.PVE, "qemu-server", "201.conf"), []byte(vm), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.PVE, "lxc", "101.conf"), []byte("hostname: web\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	g, err := Read(p, 201)
	if err != nil {
		t.Fatal(err)
	}
	if g.Type != "qemu" || g.Config != "boot: order=scsi0\nname: app\nscsi0: local-zfs:vm-201-disk-0,size=4G\n" {
		t.Errorf("VM = %q %q", g.Type, g.Config)
	}
	all, errs := ReadAll(p, []uint32{101, 999})
	if len(all) != 1 || all[0].Type != "lxc" || len(errs) != 1 {
		t.Errorf("ReadAll = %v, %v", all, errs)
	}
}

func TestStoreAndLoad(t *testing.T) {
	p := testPaths(t)
	at := timestamppb.New(time.Unix(1000, 0))
	plans := []*clientv1.PlanGuestConfigs{{PlanId: "abc", PlanName: "Main", PrimaryHostname: "pve1", Guests: []*clientv1.GuestConfig{
		{Vmid: 201, Type: "qemu", Config: "name: app\n", ChangedAt: at},
		{Vmid: 101, Type: "lxc", Config: "hostname: web\n", ChangedAt: at},
	}}}
	if err := Store(p, plans, nil); err != nil {
		t.Fatal(err)
	}
	got, err := Load(p, "abc")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[201].Config != "name: app\n" || got[201].Type != "qemu" || got[101].PrimaryHostname != "pve1" ||
		!got[101].ChangedAt.Equal(at.AsTime()) {
		t.Errorf("Load = %+v", got)
	}

	// A guest leaves the plan, and another plan disappears entirely.
	if err := Store(p, []*clientv1.PlanGuestConfigs{{PlanId: "other"}}, nil); err != nil {
		t.Fatal(err)
	}
	plans[0].Guests = plans[0].Guests[:1]
	if err := Store(p, plans, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := Load(p, "abc"); len(got) != 1 {
		t.Errorf("after removing a guest: %v", got)
	}
	if _, err := os.Stat(filepath.Join(p.Plans, "other")); !os.IsNotExist(err) {
		t.Error("a plan that's no longer listed was kept")
	}
	if err := Store(p, []*clientv1.PlanGuestConfigs{{PlanId: "../x"}}, nil); err == nil {
		t.Error("accepted an invalid plan ID")
	}
}

func TestRecovery(t *testing.T) {
	p := testPaths(t)
	r := &clientv1.PlanRecovery{PlanId: "abc", PlanName: "Main", Storages: []*clientv1.RecoveryStorage{
		{SourceStorage: "local-zfs", SourceDataset: "rpool/data", ReceiveDataset: "tank/ezdr/pve1", StorageId: "ezdr-abc-local-zfs"}}}
	if err := Store(p, nil, []*clientv1.PlanRecovery{r}); err != nil {
		t.Fatal(err)
	}
	got, err := LoadRecovery(p)
	if err != nil || len(got) != 1 || got[0].PlanName != "Main" || got[0].Storages[0].StorageId != "ezdr-abc-local-zfs" {
		t.Errorf("LoadRecovery = %v, %v", got, err)
	}
	// A plan that's no longer listed loses its recovery information too.
	if err := Store(p, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := LoadRecovery(p); len(got) != 0 {
		t.Errorf("recovery kept: %v", got)
	}
}
