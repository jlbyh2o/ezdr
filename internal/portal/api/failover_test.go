package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	clientv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/client/v1"
	portalv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/portal/v1"
	"github.com/jlbyh2o/ezdr/internal/portal/store"
)

// failoverFixture is testFixture plus a fake primary.
func failoverFixture(t *testing.T) (*Deps, context.Context, string, *fakeHost, *fakeHost) {
	t.Helper()
	d, ctx, planID, dr := testFixture(t)
	hctx, cancel := context.WithCancel(ctx)
	p := &fakeHost{d: d, id: "pve1"}
	dr.peer = p
	p.run(hctx, t)
	t.Cleanup(func() {
		cancel()
		p.wait()
	})
	return d, ctx, planID, p, dr
}

func waitFailover(ctx context.Context, t *testing.T, d *Deps, planID string, want portalv1.FailoverState) *portalv1.Failover {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		_, f, err := d.loadFailover(ctx, planID)
		if err == nil && f.State == want {
			if _, busy := runningFailovers.Load(planID); !busy {
				return f
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	_, f, _ := d.loadFailover(ctx, planID)
	t.Fatalf("failover didn't reach %v: %v", want, f)
	return nil
}

func TestPlannedFailover(t *testing.T) {
	d, ctx, planID, primary, dr := failoverFixture(t)
	svc := FailoverService{Deps: d}
	opts, err := svc.GetFailoverOptions(ctx, connect.NewRequest(&portalv1.GetFailoverOptionsRequest{PlanId: planID}))
	if err != nil {
		t.Fatal(err)
	}
	if !opts.Msg.PlannedPossible || opts.Msg.Problem != "" || len(opts.Msg.Guests) != 1 {
		t.Fatalf("options = %v", opts.Msg)
	}
	if _, err := svc.StartFailover(ctx, connect.NewRequest(&portalv1.StartFailoverRequest{PlanId: planID, Planned: true, ConfirmName: "wrong"})); err == nil {
		t.Error("started without the plan's name")
	}
	if _, err := svc.StartFailover(ctx, connect.NewRequest(&portalv1.StartFailoverRequest{PlanId: planID, Planned: true, ConfirmName: "Main"})); err != nil {
		t.Fatal(err)
	}
	f := waitFailover(ctx, t, d, planID, portalv1.FailoverState_FAILOVER_STATE_AWAITING_CONFIRMATION)
	if f.Guests[0].Status != "running" || !strings.HasPrefix(f.Snapshot, "ezdr_") {
		t.Errorf("failover = %v", f)
	}
	if got := strings.Join(primary.did(), "|"); got != "stop [101]|snapshot" {
		t.Errorf("primary actions = %s", got)
	}
	drActs := strings.Join(dr.did(), "|")
	for _, want := range []string{"replicate", "prepare [101]", "start 101", "check"} {
		if !strings.Contains(drActs, want) {
			t.Errorf("DR actions lack %q: %s", want, drActs)
		}
	}
	sp, _ := d.Store.PlanByID(ctx, planID)
	if sp.State != store.PlanFailedOver {
		t.Errorf("plan state = %s", sp.State)
	}
	// Replication stopped, and the primary keeps the guest locked.
	if ds, _ := d.desiredState(ctx, "dr1"); len(ds.GetZrepl().GetPullJobs()) != 0 {
		t.Errorf("DR still pulls: %v", ds.Zrepl)
	}
	if ds, _ := d.desiredState(ctx, "pve1"); len(ds.LockedGuests) != 1 || ds.LockedGuests[0].Vmids[0] != 101 {
		t.Errorf("primary locks = %v", ds.LockedGuests)
	}
	// A failed-over plan can't be changed.
	if _, err := (PlanService{Deps: d}).PausePlan(ctx, connect.NewRequest(&portalv1.PausePlanRequest{Id: planID})); err == nil {
		t.Error("paused a failed-over plan")
	}

	c, err := svc.ConfirmFailover(ctx, connect.NewRequest(&portalv1.ConfirmFailoverRequest{PlanId: planID}))
	if err != nil || c.Msg.Failover.State != portalv1.FailoverState_FAILOVER_STATE_COMPLETED {
		t.Fatalf("confirm = %v, %v", c, err)
	}
}

func TestPlannedFailoverAborts(t *testing.T) {
	d, ctx, planID, primary, dr := failoverFixture(t)
	dr.failReplicate = true
	svc := FailoverService{Deps: d}
	if _, err := svc.StartFailover(ctx, connect.NewRequest(&portalv1.StartFailoverRequest{PlanId: planID, Planned: true, ConfirmName: "Main"})); err != nil {
		t.Fatal(err)
	}
	f := waitFailover(ctx, t, d, planID, portalv1.FailoverState_FAILOVER_STATE_ABORTED)
	if !strings.Contains(f.Error, "zrepl isn't running") || !strings.Contains(f.Error, "started again") {
		t.Errorf("error = %s", f.Error)
	}
	if got := strings.Join(primary.did(), "|"); got != "stop [101]|snapshot|unlock [101]" {
		t.Errorf("primary actions = %s", got)
	}
	if sp, _ := d.Store.PlanByID(ctx, planID); sp.State != store.PlanActive {
		t.Errorf("plan state after abort = %s", sp.State)
	}
}

func TestUnplannedFailover(t *testing.T) {
	d, ctx, planID, primary, _ := failoverFixture(t)
	svc := FailoverService{Deps: d}
	if _, err := svc.StartFailover(ctx, connect.NewRequest(&portalv1.StartFailoverRequest{PlanId: planID, ConfirmName: "Main"})); err != nil {
		t.Fatal(err)
	}
	waitFailover(ctx, t, d, planID, portalv1.FailoverState_FAILOVER_STATE_AWAITING_CONFIRMATION)
	// The primary isn't asked to do anything: its guests get locked through
	// its desired state.
	if got := primary.did(); len(got) != 0 {
		t.Errorf("primary actions = %v", got)
	}
}

func TestBreakGlassReport(t *testing.T) {
	d, ctx, planID, _, _ := failoverFixture(t)
	hctx := context.WithValue(ctx, hostKey{}, store.Host{ID: "dr1", Hostname: "dr1"})
	if _, err := (ClientService{Deps: d}).ReportStatus(hctx, connect.NewRequest(&clientv1.ReportStatusRequest{
		BreakGlass: []*clientv1.BreakGlassFailover{{PlanId: planID, User: "root", At: timestamppb.Now(), Started: []uint32{101}}},
	})); err != nil {
		t.Fatal(err)
	}
	sp, _ := d.Store.PlanByID(ctx, planID)
	if sp.State != store.PlanFailedOver {
		t.Fatalf("plan state = %s", sp.State)
	}
	_, f, err := d.loadFailover(ctx, planID)
	if err != nil || !f.BreakGlass || f.StartedBy != "root@dr1" || f.Guests[0].Status != "running" {
		t.Errorf("failover = %v, %v", f, err)
	}
	if ds, _ := d.desiredState(ctx, "dr1"); len(ds.FailedOverPlans) != 1 || ds.FailedOverPlans[0] != planID {
		t.Errorf("DR failed-over plans = %v", ds.FailedOverPlans)
	}
	// A report from a host that isn't the plan's DR host is ignored.
	other := context.WithValue(ctx, hostKey{}, store.Host{ID: "pve1", Hostname: "pve1"})
	if err := d.recordBreakGlass(other, store.Host{ID: "pve1", Hostname: "pve1"}, &clientv1.BreakGlassFailover{PlanId: planID}); err == nil {
		t.Error("accepted a break-glass report from the primary")
	}
}

func TestListAndGetFailovers(t *testing.T) {
	d, ctx, planID, _, _ := failoverFixture(t)
	svc := FailoverService{Deps: d}
	if _, err := svc.StartFailover(ctx, connect.NewRequest(&portalv1.StartFailoverRequest{PlanId: planID, Planned: true, ConfirmName: "Main"})); err != nil {
		t.Fatal(err)
	}
	waitFailover(ctx, t, d, planID, portalv1.FailoverState_FAILOVER_STATE_AWAITING_CONFIRMATION)
	list, err := svc.ListFailovers(ctx, connect.NewRequest(&portalv1.ListFailoversRequest{PlanId: planID}))
	if err != nil || len(list.Msg.Failovers) != 1 {
		t.Fatalf("list = %v, %v", list, err)
	}
	id := list.Msg.Failovers[0].Id
	got, err := svc.GetFailover(ctx, connect.NewRequest(&portalv1.GetFailoverRequest{PlanId: planID, Id: id}))
	if err != nil || got.Msg.Failover.GetId() != id {
		t.Fatalf("get by ID = %v, %v", got, err)
	}
	if _, err := svc.GetFailover(ctx, connect.NewRequest(&portalv1.GetFailoverRequest{PlanId: "other", Id: id})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("another plan's failover: %v", err)
	}
	if fb, err := svc.ListFailbacks(ctx, connect.NewRequest(&portalv1.ListFailbacksRequest{PlanId: planID})); err != nil || len(fb.Msg.Failbacks) != 0 {
		t.Errorf("failbacks = %v, %v", fb, err)
	}
}
