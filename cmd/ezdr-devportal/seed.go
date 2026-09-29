package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	inventoryv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/inventory/v1"
	planv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/plan/v1"
	portalv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/portal/v1"
	"github.com/jlbyh2o/ezdr/internal/portal/api"
	"github.com/jlbyh2o/ezdr/internal/portal/auth"
	"github.com/jlbyh2o/ezdr/internal/portal/store"
)

const seedUser = "admin"

// A fake site: hosts, their guests, and plans in many states.

type fakeGuest struct {
	vmid   uint32
	name   string
	vm     bool
	diskGB uint64
}

type fakeHost struct {
	name    string
	cidr    string
	dr      bool
	offline bool
	guests  []fakeGuest
}

var hosts = []fakeHost{
	{name: "pve-hq-1", cidr: "192.0.2.11/24", guests: []fakeGuest{
		{100, "web-01", false, 16}, {101, "web-02", false, 16}, {102, "api", true, 64},
		{110, "postgres", true, 200}, {111, "redis", false, 8}, {120, "mail", true, 120},
		{130, "dev-sandbox", true, 80}, {131, "build-runner", false, 40},
	}},
	{name: "pve-hq-2", cidr: "192.0.2.12/24", guests: []fakeGuest{
		{200, "erp", true, 300}, {201, "files", false, 500}, {202, "print", false, 8},
		{210, "monitoring", false, 32}, {211, "wiki", false, 16},
	}},
	{name: "pve-branch", cidr: "198.51.100.11/24", guests: []fakeGuest{
		{300, "pos", true, 60}, {301, "branch-files", false, 120}, {302, "dc01", true, 60},
	}},
	{name: "dr-colo", cidr: "203.0.113.21/24", dr: true},
	{name: "dr-branch", cidr: "203.0.113.22/24", dr: true},
	{name: "pve-lab", cidr: "192.0.2.50/24", offline: true, guests: []fakeGuest{
		{900, "experiments", true, 40},
	}},
}

type fakePlan struct {
	name, primary, dr string
	vmids             []uint32
	port              uint32
	state             string // "draft", "active", "paused", "failed-over", "testing", "lagging"
}

var plans = []fakePlan{
	{"Web and API", "pve-hq-1", "dr-colo", []uint32{100, 101, 102}, 8888, "active"},
	{"Databases", "pve-hq-1", "dr-colo", []uint32{110, 111}, 8890, "lagging"},
	{"Mail", "pve-hq-1", "dr-colo", []uint32{120}, 8892, "draft"},
	{"ERP", "pve-hq-2", "dr-colo", []uint32{200, 201, 202}, 8888, "testing"},
	{"Monitoring", "pve-hq-2", "dr-colo", []uint32{210}, 8890, "paused"},
	{"Branch office", "pve-branch", "dr-branch", []uint32{300, 301, 302}, 8888, "failed-over"},
}

func inventory(h fakeHost) *inventoryv1.Inventory {
	prefix := netip.MustParsePrefix(h.cidr)
	inv := &inventoryv1.Inventory{
		Host: &inventoryv1.HostInfo{Hostname: h.name, PveVersion: "9.2.20", Kernel: "6.14.8-2-pve", ZfsVersion: "2.3.4",
			CpuModel: "AMD EPYC 9124 16-Core Processor", Cpus: 32, MemoryBytes: 256 << 30},
		Interfaces: []*inventoryv1.NetworkInterface{
			{Name: "nic0", Type: "eth", Active: true, Autostart: true},
			{Name: "vmbr0", Type: "bridge", Active: true, Autostart: true, BridgePorts: []string{"nic0"},
				VlanAware: true, Cidr: h.cidr, Gateway: prefix.Masked().Addr().Next().String()},
		},
		Zrepl: &inventoryv1.Zrepl{Version: "v0.7.0", Running: true},
	}
	pool, storage := "rpool", "local-zfs"
	if h.dr {
		pool, storage = "tank", "tank"
		inv.Interfaces = append(inv.Interfaces, &inventoryv1.NetworkInterface{Name: "vmbr99", Type: "bridge", Active: true, Autostart: true})
	}
	dataset := pool + "/data"
	if h.dr {
		dataset = pool
	}
	inv.Storages = []*inventoryv1.Storage{{Id: storage, Type: "zfspool", Content: []string{"images", "rootdir"}, ZfsPool: dataset,
		TotalBytes: 3840 << 30, UsedBytes: 1210 << 30, AvailableBytes: 2630 << 30, Active: true}}
	inv.ZfsPools = []*inventoryv1.ZfsPool{{Name: pool, Health: "ONLINE", SizeBytes: 3840 << 30, AllocatedBytes: 1210 << 30,
		FreeBytes: 2630 << 30, FragmentationPercent: 12}}
	for _, g := range h.guests {
		guest := &inventoryv1.Guest{Vmid: g.vmid, Name: g.name, Status: "running", Cores: 4, MemoryBytes: 8 << 30,
			Onboot: true, Ready: true}
		vol, key := fmt.Sprintf("subvol-%d-disk-0", g.vmid), "rootfs"
		guest.Type = inventoryv1.GuestType_GUEST_TYPE_CONTAINER
		if g.vm {
			vol, key = fmt.Sprintf("vm-%d-disk-0", g.vmid), "scsi0"
			guest.Type = inventoryv1.GuestType_GUEST_TYPE_VM
		}
		guest.Disks = []*inventoryv1.Disk{{Key: key, Storage: storage, Volume: vol, SizeBytes: g.diskGB << 30,
			ZfsDataset: dataset + "/" + vol, Readiness: inventoryv1.Readiness_READINESS_REPLICABLE}}
		guest.Nics = []*inventoryv1.Nic{{Key: "net0", Bridge: "vmbr0", Model: "virtio",
			Mac: fmt.Sprintf("BC:24:11:00:%02X:%02X", g.vmid>>8, g.vmid&0xff)}}
		inv.Guests = append(inv.Guests, guest)
	}
	return inv
}

// seed creates the administrator, the hosts, and the plans (as drafts).
func seed(ctx context.Context, d *api.Deps) error {
	u, err := d.Store.CreateFirstUser(ctx, seedUser, auth.HashPassword("dev-password-1234"))
	if err != nil {
		return err
	}
	ctx = auth.WithUser(ctx, u)

	next := portalTunnelFirstHost()
	alloc := func([]netip.Addr) (netip.Addr, error) {
		a := next
		next = next.Next()
		return a, nil
	}
	ids := map[string]string{}
	for _, h := range hosts {
		tok := "seed-" + h.name
		if _, err := d.Store.CreateToken(ctx, store.Token{ID: tok, SecretHash: []byte("s"), CreatedBy: seedUser,
			ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
			return err
		}
		key := sha256.Sum256([]byte(h.name))
		host, err := d.Store.EnrollHost(ctx, tok, []byte("s"), store.Host{ID: store.NewID(), Hostname: h.name, MachineID: h.name,
			PVEVersion: "9.2.20", WireGuardPublicKey: key[:]}, alloc)
		if err != nil {
			return err
		}
		ids[h.name] = host.ID
		inv := inventory(h)
		data, _ := proto.Marshal(inv)
		sum := sha256.Sum256(data)
		if _, err := d.Store.PutInventory(ctx, store.Inventory{HostID: host.ID, Data: data, Hash: sum[:], CollectedAt: time.Now(),
			GuestCount: len(inv.Guests)}); err != nil {
			return err
		}
		if _, err := d.Store.SetHostZrepl(ctx, host.ID, "-----BEGIN CERTIFICATE-----\nfake "+h.name+"\n-----END CERTIFICATE-----\n", "v0.7.0"); err != nil {
			return err
		}
	}

	svc := api.PlanService{Deps: d}
	for _, p := range plans {
		spec := &planv1.PlanSpec{Name: p.name, PrimaryHostId: ids[p.primary], DrHostId: ids[p.dr]}
		for _, id := range p.vmids {
			spec.Guests = append(spec.Guests, &planv1.PlanGuest{Vmid: id})
		}
		sug, err := svc.SuggestPlan(ctx, connect.NewRequest(&portalv1.SuggestPlanRequest{Spec: spec}))
		if err != nil {
			return fmt.Errorf("suggest %s: %w", p.name, err)
		}
		spec = sug.Msg.Spec
		spec.IntervalSeconds = 300
		spec.Network = &planv1.ReplicationNetwork{Path: &planv1.ReplicationNetwork_Existing{Existing: &planv1.ExistingNetwork{
			PrimaryAddress: netip.MustParsePrefix(hostByName(p.primary).cidr).Addr().String(), Port: p.port}}}
		for i, g := range spec.Guests {
			g.StartupOrder = int32(i + 1)
		}
		created, err := svc.CreatePlan(ctx, connect.NewRequest(&portalv1.CreatePlanRequest{Spec: spec}))
		if err != nil {
			return fmt.Errorf("create %s: %w", p.name, err)
		}
		for _, is := range created.Msg.Issues {
			if is.Severity == planv1.Severity_SEVERITY_ERROR {
				return fmt.Errorf("plan %s: %s", p.name, is.Message)
			}
		}
	}
	slog.Info("seeded the dev portal", "hosts", len(hosts), "plans", len(plans))
	return nil
}

func portalTunnelFirstHost() netip.Addr {
	return netip.MustParseAddr("100.64.42.2")
}

func hostByName(name string) fakeHost {
	for _, h := range hosts {
		if h.name == name {
			return h
		}
	}
	return fakeHost{}
}

// startHosts runs a simulated client for every host except offline ones,
// and stalls the lagging plans.
func startHosts(ctx context.Context, d *api.Deps, sim *api.Sim) error {
	stored, err := d.Store.ListPlans(ctx)
	if err != nil {
		return err
	}
	for _, sp := range stored {
		for _, p := range plans {
			if p.name == sp.Name && p.state == "lagging" {
				sim.Stall(sp.ID, 42*time.Minute)
			}
		}
	}
	list, err := d.Store.ListHosts(ctx)
	if err != nil {
		return err
	}
	for _, h := range list {
		if !hostByName(h.Hostname).offline {
			sim.Run(ctx, h.ID)
		}
	}
	return nil
}

// scenario brings a freshly seeded portal into its states through the real
// services: it activates plans, pauses one, fails one over, and starts a
// test failover.
func scenario(ctx context.Context, d *api.Deps) error {
	u, err := d.Store.UserByUsername(ctx, seedUser)
	if err != nil {
		return err
	}
	ctx = auth.WithUser(ctx, u)
	svc := api.PlanService{Deps: d}
	byName := map[string]string{}
	stored, err := d.Store.ListPlans(ctx)
	if err != nil {
		return err
	}
	for _, sp := range stored {
		byName[sp.Name] = sp.ID
	}
	// Give the simulated hosts a moment to connect.
	if err := wait(ctx, 2*time.Second); err != nil {
		return err
	}
	for _, p := range plans {
		if p.state == "draft" {
			continue
		}
		if _, err := svc.ActivatePlan(ctx, connect.NewRequest(&portalv1.ActivatePlanRequest{Id: byName[p.name]})); err != nil {
			return fmt.Errorf("activate %s: %w", p.name, err)
		}
		if p.state == "paused" {
			if _, err := svc.PausePlan(ctx, connect.NewRequest(&portalv1.PausePlanRequest{Id: byName[p.name]})); err != nil {
				return fmt.Errorf("pause %s: %w", p.name, err)
			}
		}
	}
	slog.Info("scenario: plans activated; waiting for replication before failing over and testing")
	if err := wait(ctx, 20*time.Second); err != nil {
		return err
	}

	fo := api.FailoverService{Deps: d}
	tests := api.TestFailoverService{Deps: d}
	for _, p := range plans {
		id := byName[p.name]
		switch p.state {
		case "failed-over":
			if _, err := fo.StartFailover(ctx, connect.NewRequest(&portalv1.StartFailoverRequest{PlanId: id, Planned: true, ConfirmName: p.name})); err != nil {
				return fmt.Errorf("fail over %s: %w", p.name, err)
			}
			go func() {
				if err := confirmFailover(ctx, fo, id); err != nil && ctx.Err() == nil {
					slog.Error("scenario: confirm failover", "plan", p.name, "err", err)
				}
			}()
		case "testing":
			opts, err := tests.GetTestOptions(ctx, connect.NewRequest(&portalv1.GetTestOptionsRequest{PlanId: id}))
			if err != nil {
				return fmt.Errorf("test options %s: %w", p.name, err)
			}
			if len(opts.Msg.PointsInTime) == 0 {
				return fmt.Errorf("test %s: no point in time to test", p.name)
			}
			if _, err := tests.StartTest(ctx, connect.NewRequest(&portalv1.StartTestRequest{PlanId: id, Vmids: p.vmids,
				Snapshot: opts.Msg.PointsInTime[0].Snapshot})); err != nil {
				return fmt.Errorf("test %s: %w", p.name, err)
			}
		}
	}
	return nil
}

func confirmFailover(ctx context.Context, fo api.FailoverService, planID string) error {
	for {
		res, err := fo.GetFailover(ctx, connect.NewRequest(&portalv1.GetFailoverRequest{PlanId: planID}))
		if err != nil {
			return err
		}
		switch res.Msg.Failover.GetState() {
		case portalv1.FailoverState_FAILOVER_STATE_AWAITING_CONFIRMATION:
			_, err := fo.ConfirmFailover(ctx, connect.NewRequest(&portalv1.ConfirmFailoverRequest{PlanId: planID}))
			if err == nil {
				slog.Info("scenario: failover confirmed")
			}
			return err
		case portalv1.FailoverState_FAILOVER_STATE_ABORTED:
			return errors.New("failover aborted: " + res.Msg.Failover.Error)
		}
		if err := wait(ctx, 2*time.Second); err != nil {
			return err
		}
	}
}

func wait(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}
