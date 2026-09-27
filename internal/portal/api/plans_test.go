package api

import (
	"bytes"
	"context"
	"net/netip"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	inventoryv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/inventory/v1"
	planv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/plan/v1"
	portalv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/portal/v1"
	"github.com/jlbyh2o/ezdr/internal/portal/auth"
	"github.com/jlbyh2o/ezdr/internal/portal/store"
)

func planTestDeps(t *testing.T) (*Deps, context.Context, string, string) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "ezdr.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	u, _ := st.CreateFirstUser(ctx, "admin", "hash")
	ctx = auth.WithUser(ctx, u)

	next := netip.MustParseAddr("100.64.42.2")
	alloc := func([]netip.Addr) (netip.Addr, error) {
		a := next
		next = next.Next()
		return a, nil
	}
	enroll := func(name string, inv *inventoryv1.Inventory) string {
		if _, err := st.CreateToken(ctx, store.Token{ID: "t" + name, SecretHash: []byte("s"), CreatedBy: "admin",
			ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
		h, err := st.EnrollHost(ctx, "t"+name, []byte("s"), store.Host{ID: name, Hostname: name, MachineID: name,
			WireGuardPublicKey: bytes.Repeat([]byte(name[:1]), 32)}, alloc)
		if err != nil {
			t.Fatal(err)
		}
		data, _ := proto.Marshal(inv)
		if _, err := st.PutInventory(ctx, store.Inventory{HostID: h.ID, Data: data, Hash: []byte(name), CollectedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
		return h.ID
	}
	primary := enroll("pve1", &inventoryv1.Inventory{
		Host: &inventoryv1.HostInfo{Hostname: "pve1"},
		Guests: []*inventoryv1.Guest{
			{Vmid: 101, Name: "web", Ready: true, Disks: []*inventoryv1.Disk{{Key: "rootfs", Storage: "local-zfs",
				ZfsDataset: "rpool/subvol-101-disk-0", Readiness: inventoryv1.Readiness_READINESS_REPLICABLE}},
				Nics: []*inventoryv1.Nic{{Key: "net0", Bridge: "vmbr0"}}},
			{Vmid: 102, Name: "db", Ready: true},
		},
		Storages:   []*inventoryv1.Storage{{Id: "local-zfs", Type: "zfspool", ZfsPool: "rpool"}},
		Interfaces: []*inventoryv1.NetworkInterface{{Name: "vmbr0", Type: "bridge", Cidr: "192.0.2.10/24", Gateway: "192.0.2.1"}},
	})
	dr := enroll("dr1", &inventoryv1.Inventory{
		Host:       &inventoryv1.HostInfo{Hostname: "dr1"},
		Storages:   []*inventoryv1.Storage{{Id: "tank", Type: "zfspool", ZfsPool: "tank"}},
		ZfsPools:   []*inventoryv1.ZfsPool{{Name: "tank", FreeBytes: 1 << 40}},
		Interfaces: []*inventoryv1.NetworkInterface{{Name: "vmbr0", Type: "bridge", BridgePorts: []string{"nic0"}}, {Name: "vmbr99", Type: "bridge"}},
	})
	box, _ := store.NewSecretBox(bytes.Repeat([]byte{3}, 32))
	publicURL, _ := url.Parse("https://portal.example.com")
	return &Deps{Store: st, Box: box, Hub: NewHub(), PublicURL: publicURL,
		SiteTunnelPrefix: netip.MustParsePrefix("100.64.43.0/28")}, ctx, primary, dr
}

func TestPlanServiceFlow(t *testing.T) {
	d, ctx, primary, dr := planTestDeps(t)
	svc := PlanService{Deps: d}

	suggested, err := svc.SuggestPlan(ctx, connect.NewRequest(&portalv1.SuggestPlanRequest{Spec: &planv1.PlanSpec{
		Name: "Main", PrimaryHostId: primary, DrHostId: dr, Guests: []*planv1.PlanGuest{{Vmid: 101}},
	}}))
	if err != nil {
		t.Fatal(err)
	}
	spec := suggested.Msg.Spec
	if spec.StorageMappings[0].TargetStorage != "tank" || spec.NetworkMappings[0].TargetBridge != "vmbr0" || spec.TestBridge != "vmbr99" {
		t.Fatalf("suggestion = %v", spec)
	}

	created, err := svc.CreatePlan(ctx, connect.NewRequest(&portalv1.CreatePlanRequest{Spec: spec}))
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range created.Msg.Issues {
		if i.Severity == planv1.Severity_SEVERITY_ERROR {
			t.Errorf("unexpected error: %s", i.Message)
		}
	}
	id := created.Msg.Plan.Id

	// Another plan can't take guest 101, and names are unique.
	other := &planv1.PlanSpec{Name: "Other", PrimaryHostId: primary, DrHostId: dr, Guests: []*planv1.PlanGuest{{Vmid: 101}}}
	if _, err := svc.CreatePlan(ctx, connect.NewRequest(&portalv1.CreatePlanRequest{Spec: other})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("guest in two plans: err = %v", err)
	}
	other.Name, other.Guests = "main", nil
	if _, err := svc.CreatePlan(ctx, connect.NewRequest(&portalv1.CreatePlanRequest{Spec: other})); connect.CodeOf(err) != connect.CodeAlreadyExists {
		t.Errorf("duplicate name: err = %v", err)
	}
	// Validation (without saving) of a second plan reports the conflict.
	other.Name, other.Guests = "Other", []*planv1.PlanGuest{{Vmid: 101}}
	v, err := svc.ValidatePlan(ctx, connect.NewRequest(&portalv1.ValidatePlanRequest{Spec: other}))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, i := range v.Msg.Issues {
		if i.Severity == planv1.Severity_SEVERITY_ERROR && i.Vmid == 101 {
			found = true
		}
	}
	if !found {
		t.Error("ValidatePlan did not report the guest conflict")
	}

	// Drafts with validation errors can still be saved.
	spec.Guests = append(spec.Guests, &planv1.PlanGuest{Vmid: 999})
	updated, err := svc.UpdatePlan(ctx, connect.NewRequest(&portalv1.UpdatePlanRequest{Id: id, Spec: spec}))
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.Msg.Plan.Spec.Guests) != 2 {
		t.Errorf("guests = %v", updated.Msg.Plan.Spec.Guests)
	}

	list, err := svc.ListPlans(ctx, connect.NewRequest(&portalv1.ListPlansRequest{}))
	if err != nil || len(list.Msg.Plans) != 1 || list.Msg.Plans[0].ErrorCount == 0 || list.Msg.Plans[0].PrimaryHostname != "pve1" {
		t.Fatalf("ListPlans = %v, %v", list, err)
	}

	inv, err := HostService{Deps: d}.GetHostInventory(ctx, connect.NewRequest(&portalv1.GetHostInventoryRequest{HostId: primary}))
	if err != nil || inv.Msg.GuestPlans[101].GetName() != "Main" || inv.Msg.Host.PlanCount != 1 {
		t.Fatalf("host inventory plans = %v, count %d, %v", inv.Msg.GetGuestPlans(), inv.Msg.GetHost().GetPlanCount(), err)
	}

	if _, err := svc.DeletePlan(ctx, connect.NewRequest(&portalv1.DeletePlanRequest{Id: id})); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GetPlan(ctx, connect.NewRequest(&portalv1.GetPlanRequest{Id: id})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("deleted plan: err = %v", err)
	}
}

func TestCheckSpec(t *testing.T) {
	for name, spec := range map[string]*planv1.PlanSpec{
		"nil":        nil,
		"no name":    {PrimaryHostId: "a", DrHostId: "b"},
		"no hosts":   {Name: "x"},
		"same hosts": {Name: "x", PrimaryHostId: "a", DrHostId: "a"},
		"dup guest":  {Name: "x", PrimaryHostId: "a", DrHostId: "b", Guests: []*planv1.PlanGuest{{Vmid: 1}, {Vmid: 1}}},
	} {
		if err := checkSpec(spec); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestPlanLifecycle(t *testing.T) {
	d, ctx, primary, dr := planTestDeps(t)
	svc := PlanService{Deps: d}
	for _, id := range []string{primary, dr} {
		if _, err := d.Store.SetHostZrepl(ctx, id, "CERT-"+id, "v0.7.0"); err != nil {
			t.Fatal(err)
		}
	}
	// Watch what the DR host is sent.
	_, drOutbox, release := d.Hub.connect(ctx, dr)
	defer release()

	sug, _ := svc.SuggestPlan(ctx, connect.NewRequest(&portalv1.SuggestPlanRequest{Spec: &planv1.PlanSpec{
		Name: "Main", PrimaryHostId: primary, DrHostId: dr, Guests: []*planv1.PlanGuest{{Vmid: 101}},
	}}))
	created, err := svc.CreatePlan(ctx, connect.NewRequest(&portalv1.CreatePlanRequest{Spec: sug.Msg.Spec}))
	if err != nil {
		t.Fatal(err)
	}
	id := created.Msg.Plan.Id
	if created.Msg.Plan.State != portalv1.PlanState_PLAN_STATE_DRAFT {
		t.Fatalf("state = %v", created.Msg.Plan.State)
	}

	preview, err := svc.PreviewPlanChanges(ctx, connect.NewRequest(&portalv1.PreviewPlanChangesRequest{Id: id}))
	if err != nil {
		t.Fatal(err)
	}
	var drChanges []string
	for _, h := range preview.Msg.Hosts {
		if h.HostId == dr {
			drChanges = h.Changes
		}
	}
	if len(drChanges) == 0 || !strings.Contains(strings.Join(drChanges, "\n"), "pull from 192.0.2.10:8888") {
		t.Fatalf("DR preview = %v", drChanges)
	}

	act, err := svc.ActivatePlan(ctx, connect.NewRequest(&portalv1.ActivatePlanRequest{Id: id}))
	if err != nil {
		t.Fatal(err)
	}
	if act.Msg.Plan.State != portalv1.PlanState_PLAN_STATE_ACTIVE || act.Msg.Plan.PendingChanges {
		t.Fatalf("activated plan = %v", act.Msg.Plan)
	}
	msg := <-drOutbox
	ds := msg.GetDesiredState()
	if ds == nil || len(ds.Zrepl.PullJobs) != 1 || ds.Zrepl.PullJobs[0].Peer.CertificatePem != "CERT-"+primary {
		t.Fatalf("DR desired state = %v", msg)
	}
	gen := ds.Generation

	// Active plans can't be deleted, and edits become pending changes.
	if _, err := svc.DeletePlan(ctx, connect.NewRequest(&portalv1.DeletePlanRequest{Id: id})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("deleting an active plan: err = %v", err)
	}
	spec := act.Msg.Plan.Spec
	spec.IntervalSeconds = 300
	up, err := svc.UpdatePlan(ctx, connect.NewRequest(&portalv1.UpdatePlanRequest{Id: id, Spec: spec}))
	if err != nil || !up.Msg.Plan.PendingChanges || up.Msg.Plan.AppliedSpec.IntervalSeconds == 300 {
		t.Fatalf("pending changes not tracked: %v, %v", up.Msg.GetPlan(), err)
	}
	disc, err := svc.DiscardPlanChanges(ctx, connect.NewRequest(&portalv1.DiscardPlanChangesRequest{Id: id}))
	if err != nil || disc.Msg.Plan.PendingChanges || disc.Msg.Plan.Spec.IntervalSeconds == 300 {
		t.Fatalf("discard: %v, %v", disc.Msg.GetPlan(), err)
	}
	spec.IntervalSeconds = 300
	if _, err := svc.UpdatePlan(ctx, connect.NewRequest(&portalv1.UpdatePlanRequest{Id: id, Spec: spec})); err != nil {
		t.Fatal(err)
	}
	applied, err := svc.ApplyPlanChanges(ctx, connect.NewRequest(&portalv1.ApplyPlanChangesRequest{Id: id}))
	if err != nil || applied.Msg.Plan.PendingChanges || applied.Msg.Plan.AppliedSpec.IntervalSeconds != 300 {
		t.Fatalf("apply: %v, %v", applied.Msg.GetPlan(), err)
	}
	if next := (<-drOutbox).GetDesiredState(); next.Generation <= gen || next.Zrepl.PullJobs[0].IntervalSeconds != 300 {
		t.Fatalf("applied change not pushed: %v", next)
	}

	// Pausing removes the jobs; resuming restores them.
	if _, err := svc.PausePlan(ctx, connect.NewRequest(&portalv1.PausePlanRequest{Id: id})); err != nil {
		t.Fatal(err)
	}
	if paused := (<-drOutbox).GetDesiredState(); len(paused.Zrepl.PullJobs) != 0 {
		t.Fatalf("paused plan still has jobs: %v", paused)
	}
	if _, err := svc.ResumePlan(ctx, connect.NewRequest(&portalv1.ResumePlanRequest{Id: id})); err != nil {
		t.Fatal(err)
	}
	if resumed := (<-drOutbox).GetDesiredState(); len(resumed.Zrepl.PullJobs) != 1 {
		t.Fatalf("resumed plan has no jobs: %v", resumed)
	}

	// Deactivating returns to draft; then the plan can be deleted.
	deact, err := svc.DeactivatePlan(ctx, connect.NewRequest(&portalv1.DeactivatePlanRequest{Id: id}))
	if err != nil || deact.Msg.Plan.State != portalv1.PlanState_PLAN_STATE_DRAFT || deact.Msg.Plan.AppliedSpec != nil {
		t.Fatalf("deactivate: %v, %v", deact.Msg.GetPlan(), err)
	}
	if gone := (<-drOutbox).GetDesiredState(); len(gone.Zrepl.PullJobs) != 0 {
		t.Fatalf("deactivated plan still has jobs: %v", gone)
	}
	if _, err := svc.DeletePlan(ctx, connect.NewRequest(&portalv1.DeletePlanRequest{Id: id})); err != nil {
		t.Errorf("deleting a draft: %v", err)
	}
}

func TestActivateRequiresValidPlan(t *testing.T) {
	d, ctx, primary, dr := planTestDeps(t)
	svc := PlanService{Deps: d}
	created, err := svc.CreatePlan(ctx, connect.NewRequest(&portalv1.CreatePlanRequest{Spec: &planv1.PlanSpec{
		Name: "Incomplete", PrimaryHostId: primary, DrHostId: dr, Guests: []*planv1.PlanGuest{{Vmid: 101}},
	}}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.ActivatePlan(ctx, connect.NewRequest(&portalv1.ActivatePlanRequest{Id: created.Msg.Plan.Id}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("activating an invalid plan: err = %v", err)
	}
}

func TestTunnelPlan(t *testing.T) {
	d, ctx, primary, dr := planTestDeps(t)
	svc := PlanService{Deps: d}
	for _, id := range []string{primary, dr} {
		_, _ = d.Store.SetHostZrepl(ctx, id, "CERT-"+id, "v0.7.0")
		_, _ = d.Store.SetSitePublicKey(ctx, id, bytes.Repeat([]byte(id[:1]), 32))
	}
	sug, _ := svc.SuggestPlan(ctx, connect.NewRequest(&portalv1.SuggestPlanRequest{Spec: &planv1.PlanSpec{
		Name: "Tunnel", PrimaryHostId: primary, DrHostId: dr, Guests: []*planv1.PlanGuest{{Vmid: 101}},
	}}))
	spec := sug.Msg.Spec
	spec.Network = &planv1.ReplicationNetwork{Path: &planv1.ReplicationNetwork_Tunnel{Tunnel: &planv1.EzdrTunnel{
		Listener: planv1.EzdrTunnel_LISTENER_DR, Endpoint: "dr.example.com:51821", ListenPort: 51821, Port: 8888}}}
	created, err := svc.CreatePlan(ctx, connect.NewRequest(&portalv1.CreatePlanRequest{Spec: spec}))
	if err != nil {
		t.Fatal(err)
	}

	preview, err := svc.PreviewPlanChanges(ctx, connect.NewRequest(&portalv1.PreviewPlanChangesRequest{Id: created.Msg.Plan.Id}))
	if err != nil {
		t.Fatal(err)
	}
	joined := ""
	for _, h := range preview.Msg.Hosts {
		joined += strings.Join(h.Changes, "\n") + "\n"
	}
	for _, want := range []string{"create WireGuard interface ezdr1", "accept 100.64.43.", "connect to 100.64.43.", "dr.example.com:51821"} {
		if !strings.Contains(joined, want) {
			t.Errorf("preview lacks %q:\n%s", want, joined)
		}
	}

	_, drOutbox, release := d.Hub.connect(ctx, dr)
	defer release()
	if _, err := svc.ActivatePlan(ctx, connect.NewRequest(&portalv1.ActivatePlanRequest{Id: created.Msg.Plan.Id})); err != nil {
		t.Fatal(err)
	}
	ds := (<-drOutbox).GetDesiredState()
	st := ds.GetSiteTunnel()
	if st == nil || st.ListenPort != 51821 || len(st.Peers) != 1 || st.Peers[0].Endpoint != "" {
		t.Fatalf("DR site tunnel = %v", st)
	}
	pHost, _ := d.Store.HostByID(ctx, primary)
	if pj := ds.Zrepl.PullJobs[0]; pj.Address != pHost.SiteAddress.String()+":8888" {
		t.Errorf("pull address = %s, primary tunnel address %s", pj.Address, pHost.SiteAddress)
	}
	pds, err := d.desiredState(ctx, primary)
	if err != nil {
		t.Fatal(err)
	}
	if pt := pds.SiteTunnel; pt.ListenPort != 0 || pt.Peers[0].Endpoint != "dr.example.com:51821" || pt.Peers[0].PersistentKeepaliveSeconds == 0 {
		t.Errorf("primary site tunnel = %v", pt)
	}
	if sj := pds.Zrepl.SourceJobs[0]; !sj.ListenFreebind || sj.ListenAddress != pHost.SiteAddress.String()+":8888" {
		t.Errorf("source job = %v", sj)
	}
}
