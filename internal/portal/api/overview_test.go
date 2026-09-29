package api

import (
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	clientv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/client/v1"
	inventoryv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/inventory/v1"
	planv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/plan/v1"
	portalv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/portal/v1"
	"github.com/jlbyh2o/ezdr/internal/portal/store"
)

func TestOverview(t *testing.T) {
	d, ctx, primary, dr := planTestDeps(t)
	svc := PlanService{Deps: d}
	for _, id := range []string{primary, dr} {
		if _, err := d.Store.SetHostZrepl(ctx, id, "CERT-"+id, "v0.7.0"); err != nil {
			t.Fatal(err)
		}
	}
	sug, _ := svc.SuggestPlan(ctx, connect.NewRequest(&portalv1.SuggestPlanRequest{Spec: &planv1.PlanSpec{
		Name: "Main", PrimaryHostId: primary, DrHostId: dr, Guests: []*planv1.PlanGuest{{Vmid: 101}},
	}}))
	sug.Msg.Spec.NetworkMappings[0].TargetBridge = "vmbr0"
	created, err := svc.CreatePlan(ctx, connect.NewRequest(&portalv1.CreatePlanRequest{Spec: sug.Msg.Spec}))
	if err != nil {
		t.Fatal(err)
	}
	id := created.Msg.Plan.Id
	if _, err := svc.ActivatePlan(ctx, connect.NewRequest(&portalv1.ActivatePlanRequest{Id: id})); err != nil {
		t.Fatal(err)
	}

	report := func(finished bool) {
		t.Helper()
		js := &clientv1.JobStatus{Name: planJobPrefix(id) + "local-zfs_pull", Type: "pull", State: "fan-out-filesystems",
			AttemptStartedAt: timestamppb.New(time.Now().Add(-time.Minute)),
			Datasets: []*clientv1.DatasetStatus{{Dataset: "rpool/subvol-101-disk-0", State: "stepping", BytesExpected: 100, BytesReplicated: 40,
				LatestSnapshot: "zrepl_1", LatestSnapshotAt: timestamppb.New(time.Now().Add(-5 * time.Minute))}}}
		if finished {
			js.State, js.AttemptFinishedAt = "done", timestamppb.Now()
		}
		b, _ := proto.Marshal(&clientv1.ReportReplicationRequest{Jobs: []*clientv1.JobStatus{js}})
		if err := d.Store.PutReplicationStatus(ctx, dr, b); err != nil {
			t.Fatal(err)
		}
	}
	get := func() *portalv1.GetOverviewResponse {
		t.Helper()
		res, err := OverviewService{Deps: d}.GetOverview(ctx, connect.NewRequest(&portalv1.GetOverviewRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		return res.Msg
	}

	report(false)
	ov := get()
	if len(ov.Hosts) != 2 || len(ov.Plans) != 1 {
		t.Fatalf("overview = %v", ov)
	}
	for _, h := range ov.Hosts {
		if h.Id != primary {
			continue
		}
		if len(h.Guests) != 2 || h.Guests[0].PlanId != id || h.Guests[1].PlanId != "" {
			t.Errorf("primary guests = %v", h.Guests)
		}
	}
	p := ov.Plans[0]
	if p.State != portalv1.PlanState_PLAN_STATE_ACTIVE || len(p.Vmids) != 1 || p.Vmids[0] != 101 {
		t.Errorf("plan = %v", p)
	}
	if tr := p.Transfer; tr.GetDirection() != portalv1.OverviewTransfer_DIRECTION_TO_DR || tr.BytesExpected != 100 || tr.BytesDone != 40 {
		t.Errorf("transfer = %v", tr)
	}

	if len(p.Guests) != 1 {
		t.Fatalf("guest replication = %v", p.Guests)
	}
	if g := p.Guests[0]; g.Vmid != 101 || g.Disks != 1 || !g.Transferring || g.BytesDone != 40 || g.LatestSnapshot != "zrepl_1" {
		t.Errorf("guest replication = %v", g)
	}

	report(true)
	p = get().Plans[0]
	if p.Transfer != nil {
		t.Errorf("finished transfer still shown: %v", p.Transfer)
	}
	if g := p.Guests[0]; g.Transferring || g.LastReplicatedAt == nil {
		t.Errorf("guest replication after the transfer = %v", g)
	}
}

func TestOverviewMarksTestCopies(t *testing.T) {
	d, ctx, planID, dr := testFixture(t)
	started, err := TestFailoverService{Deps: d}.StartTest(ctx, connect.NewRequest(&portalv1.StartTestRequest{
		PlanId: planID, Vmids: []uint32{101}, Snapshot: "zrepl_1"}))
	if err != nil {
		t.Fatal(err)
	}
	waitTest(ctx, t, d, started.Msg.Test.Id, portalv1.TestState_TEST_STATE_RUNNING)
	// The DR host reports the test copy, and a guest of its own.
	inv := &inventoryv1.Inventory{Host: &inventoryv1.HostInfo{Hostname: "dr1"}, Guests: []*inventoryv1.Guest{
		{Vmid: 10101, Name: "web", Status: "running"}, {Vmid: 500, Name: "local"}}}
	data, _ := proto.Marshal(inv)
	if _, err := d.Store.PutInventory(ctx, store.Inventory{HostID: dr.id, Data: data, Hash: []byte("t"), CollectedAt: time.Now(), GuestCount: 2}); err != nil {
		t.Fatal(err)
	}
	res, err := OverviewService{Deps: d}.GetOverview(ctx, connect.NewRequest(&portalv1.GetOverviewRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range res.Msg.Hosts {
		if h.Id != dr.id {
			continue
		}
		for _, g := range h.Guests {
			isCopy := g.TestPlanId == planID && g.TestId == started.Msg.Test.Id
			if (g.Vmid == 10101) != isCopy {
				t.Errorf("guest %d: test plan %q, test %q", g.Vmid, g.TestPlanId, g.TestId)
			}
		}
	}
}
