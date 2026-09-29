package api

import (
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	clientv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/client/v1"
	planv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/plan/v1"
	portalv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/portal/v1"
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
			Datasets:         []*clientv1.DatasetStatus{{Dataset: "rpool/subvol-101-disk-0", BytesExpected: 100, BytesReplicated: 40}}}
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

	report(true)
	if tr := get().Plans[0].Transfer; tr != nil {
		t.Errorf("finished transfer still shown: %v", tr)
	}
}
