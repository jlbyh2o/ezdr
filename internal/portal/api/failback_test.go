package api

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	portalv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/portal/v1"
	"github.com/jlbyh2o/ezdr/internal/portal/store"
)

// failedOverFixture is failoverFixture after a planned failover, with both
// fake hosts connected and a fast clock for round snapshot names.
func failedOverFixture(t *testing.T) (*Deps, context.Context, string, *fakeHost, *fakeHost) {
	t.Helper()
	d, ctx, planID, primary, dr := failoverFixture(t)
	svc := FailoverService{Deps: d}
	if _, err := svc.StartFailover(ctx, connect.NewRequest(&portalv1.StartFailoverRequest{PlanId: planID, Planned: true, ConfirmName: "Main"})); err != nil {
		t.Fatal(err)
	}
	waitFailover(ctx, t, d, planID, portalv1.FailoverState_FAILOVER_STATE_AWAITING_CONFIRMATION)
	if _, err := svc.ConfirmFailover(ctx, connect.NewRequest(&portalv1.ConfirmFailoverRequest{PlanId: planID})); err != nil {
		t.Fatal(err)
	}
	var tick atomic.Int64
	old := failbackNow
	failbackNow = func() time.Time { return time.Unix(1_800_000_000+tick.Add(1), 0) }
	t.Cleanup(func() { failbackNow = old })
	primary.mu.Lock()
	primary.actions = nil
	primary.mu.Unlock()
	dr.mu.Lock()
	dr.actions = nil
	dr.mu.Unlock()
	return d, ctx, planID, primary, dr
}

func waitFailback(ctx context.Context, t *testing.T, d *Deps, planID string, want portalv1.FailbackState) *portalv1.Failback {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		_, fb, err := d.loadFailback(ctx, planID)
		if err == nil && fb.State == want {
			if _, busy := runningFailbacks.Load(planID); !busy {
				return fb
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	_, fb, _ := d.loadFailback(ctx, planID)
	t.Fatalf("failback didn't reach %v: %v", want, fb)
	return nil
}

func TestFailbackPreflight(t *testing.T) {
	d, ctx, planID, primary, dr := failoverFixture(t)
	svc := FailoverService{Deps: d}
	req := connect.NewRequest(&portalv1.RunFailbackPreflightRequest{PlanId: planID})
	if _, err := svc.RunFailbackPreflight(ctx, req); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("preflight of an active plan: %v", err)
	}

	sp, _ := d.Store.PlanByID(ctx, planID)
	if err := d.Store.SetPlanState(ctx, planID, store.PlanFailedOver, sp.AppliedSpec); err != nil {
		t.Fatal(err)
	}
	// The fake hosts connect in the background.
	for deadline := time.Now().Add(5 * time.Second); !d.Hub.Online("pve1") || !d.Hub.Online("dr1"); {
		if time.Now().After(deadline) {
			t.Fatal("the fake hosts didn't connect")
		}
		time.Sleep(5 * time.Millisecond)
	}
	res, err := svc.RunFailbackPreflight(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	r := res.Msg.Preflight
	if len(r.Problems) != 0 || len(r.Datasets) != 1 || r.Datasets[0].CommonSnapshot != "@zrepl_1" || r.Diverged || r.CheckedAt == nil {
		t.Errorf("preflight = %v", r)
	}
	if got := strings.Join(primary.did(), "|") + "|" + strings.Join(dr.did(), "|"); got != "preflight|preflight" {
		t.Errorf("actions = %s", got)
	}
}

func TestFailback(t *testing.T) {
	d, ctx, planID, primary, dr := failedOverFixture(t)
	dr.mu.Lock()
	dr.sendBytes = []uint64{1 << 30, 300 << 20, 1 << 20, 4096}
	dr.mu.Unlock()
	svc := FailoverService{Deps: d}
	if _, err := svc.StartFailback(ctx, connect.NewRequest(&portalv1.StartFailbackRequest{PlanId: planID, ConfirmName: "wrong"})); err == nil {
		t.Error("started without the plan's name")
	}
	if _, err := svc.StartFailback(ctx, connect.NewRequest(&portalv1.StartFailbackRequest{PlanId: planID, ConfirmName: "Main"})); err != nil {
		t.Fatal(err)
	}
	fb := waitFailback(ctx, t, d, planID, portalv1.FailbackState_FAILBACK_STATE_AWAITING_CONFIRMATION)

	// Rounds until one copies less than 256 MiB, then the final copy.
	if len(fb.Rounds) != 4 || !fb.Rounds[3].Final || fb.Rounds[2].Final || fb.Rounds[2].Bytes != 1<<20 {
		t.Fatalf("rounds = %v", fb.Rounds)
	}
	final := fb.Rounds[3].Snapshot
	pri := strings.Join(primary.did(), "|")
	for _, want := range []string{
		"receive rpool/subvol-101-disk-0@zrepl_1 rollback=false", // from the common snapshot
		"receive rpool/subvol-101-disk-0@" + fb.Rounds[0].Snapshot + " rollback=true",
		"unlock [101]", "check primary",
	} {
		if !strings.Contains(pri, want) {
			t.Errorf("primary actions lack %q: %s", want, pri)
		}
	}
	drActs := strings.Join(dr.did(), "|")
	for _, want := range []string{"snapshot|send", "stop [101]", "cleanup " + final} {
		if !strings.Contains(drActs, want) {
			t.Errorf("DR actions lack %q: %s", want, drActs)
		}
	}
	if strings.Contains(drActs, "unlock") {
		t.Errorf("DR guests restarted: %s", drActs)
	}
	if fb.Guests[0].Status != "running" {
		t.Errorf("guests = %v", fb.Guests)
	}

	// Replication resumed; the primary no longer locks the guest.
	if sp, _ := d.Store.PlanByID(ctx, planID); sp.State != store.PlanActive {
		t.Errorf("plan state = %s", sp.State)
	}
	if ds, _ := d.desiredState(ctx, "dr1"); len(ds.GetZrepl().GetPullJobs()) != 1 || len(ds.FailedOverPlans) != 0 {
		t.Errorf("DR desired state = %v", ds)
	}
	if ds, _ := d.desiredState(ctx, "pve1"); len(ds.LockedGuests) != 0 {
		t.Errorf("primary locks = %v", ds.LockedGuests)
	}
	// Once failed back, the old failover can't be retried, and the plan
	// can't change until the failback is confirmed.
	if _, err := svc.RetryFailover(ctx, connect.NewRequest(&portalv1.RetryFailoverRequest{PlanId: planID})); err == nil {
		t.Error("retried the failover after failing back")
	}
	if _, err := (PlanService{Deps: d}).PausePlan(ctx, connect.NewRequest(&portalv1.PausePlanRequest{Id: planID})); err == nil {
		t.Error("paused a plan waiting for the failback's confirmation")
	}
	if p, _ := (PlanService{Deps: d}).GetPlan(ctx, connect.NewRequest(&portalv1.GetPlanRequest{Id: planID})); p.Msg.Plan.State != portalv1.PlanState_PLAN_STATE_FAILING_BACK {
		t.Errorf("plan state = %v", p.Msg.Plan.State)
	}

	c, err := svc.ConfirmFailback(ctx, connect.NewRequest(&portalv1.ConfirmFailbackRequest{PlanId: planID}))
	if err != nil || c.Msg.Failback.State != portalv1.FailbackState_FAILBACK_STATE_COMPLETED {
		t.Fatalf("confirm = %v, %v", c, err)
	}
}

func TestFailbackAborts(t *testing.T) {
	d, ctx, planID, primary, dr := failedOverFixture(t)
	dr.mu.Lock()
	dr.failSend = 2 // the final copy
	dr.mu.Unlock()
	svc := FailoverService{Deps: d}
	if _, err := svc.StartFailback(ctx, connect.NewRequest(&portalv1.StartFailbackRequest{PlanId: planID, ConfirmName: "Main"})); err != nil {
		t.Fatal(err)
	}
	fb := waitFailback(ctx, t, d, planID, portalv1.FailbackState_FAILBACK_STATE_ABORTED)
	if !strings.Contains(fb.Error, "zfs send failed") || !strings.Contains(fb.Error, "started again on the DR host") {
		t.Errorf("error = %s", fb.Error)
	}
	if got := strings.Join(dr.did(), "|"); !strings.HasSuffix(got, "stop [101]|snapshot|send|unlock [101]") {
		t.Errorf("DR actions = %s", got)
	}
	if strings.Contains(strings.Join(primary.did(), "|"), "unlock") {
		t.Errorf("primary guests unlocked: %v", primary.did())
	}
	if sp, _ := d.Store.PlanByID(ctx, planID); sp.State != store.PlanFailedOver {
		t.Errorf("plan state after abort = %s", sp.State)
	}

	// Another attempt runs.
	if _, err := svc.StartFailback(ctx, connect.NewRequest(&portalv1.StartFailbackRequest{PlanId: planID, ConfirmName: "Main"})); err != nil {
		t.Fatal(err)
	}
	waitFailback(ctx, t, d, planID, portalv1.FailbackState_FAILBACK_STATE_AWAITING_CONFIRMATION)
}

func TestFailbackRetry(t *testing.T) {
	d, ctx, planID, _, dr := failedOverFixture(t)
	dr.mu.Lock()
	dr.failCleanup = 1
	dr.mu.Unlock()
	svc := FailoverService{Deps: d}
	if _, err := svc.StartFailback(ctx, connect.NewRequest(&portalv1.StartFailbackRequest{PlanId: planID, ConfirmName: "Main"})); err != nil {
		t.Fatal(err)
	}
	fb := waitFailback(ctx, t, d, planID, portalv1.FailbackState_FAILBACK_STATE_FAILED)
	if _, err := svc.StartFailback(ctx, connect.NewRequest(&portalv1.StartFailbackRequest{PlanId: planID, ConfirmName: "Main"})); err == nil {
		t.Error("started another failback instead of retrying")
	}
	if !strings.Contains(fb.Error, "pvesm failed") || strings.Contains(strings.Join(dr.did(), "|"), "unlock") {
		t.Errorf("failback = %v; DR actions %v", fb, dr.did())
	}
	// The plan can't change while the failback waits for a retry.
	if _, err := (PlanService{Deps: d}).PausePlan(ctx, connect.NewRequest(&portalv1.PausePlanRequest{Id: planID})); err == nil {
		t.Error("paused a plan with a failed failback")
	}
	if _, err := svc.RetryFailback(ctx, connect.NewRequest(&portalv1.RetryFailbackRequest{PlanId: planID})); err != nil {
		t.Fatal(err)
	}
	waitFailback(ctx, t, d, planID, portalv1.FailbackState_FAILBACK_STATE_AWAITING_CONFIRMATION)
}

func TestFailbackDiverged(t *testing.T) {
	d, ctx, planID, primary, _ := failedOverFixture(t)
	primary.mu.Lock()
	primary.written = 4096
	primary.mu.Unlock()
	svc := FailoverService{Deps: d}
	_, err := svc.StartFailback(ctx, connect.NewRequest(&portalv1.StartFailbackRequest{PlanId: planID, ConfirmName: "Main"}))
	if err == nil || !strings.Contains(err.Error(), "confirm discarding") {
		t.Fatalf("err = %v", err)
	}
	if _, err := svc.StartFailback(ctx, connect.NewRequest(&portalv1.StartFailbackRequest{PlanId: planID, ConfirmName: "Main",
		DiscardDiverged: true})); err != nil {
		t.Fatal(err)
	}
	waitFailback(ctx, t, d, planID, portalv1.FailbackState_FAILBACK_STATE_AWAITING_CONFIRMATION)
	if got := strings.Join(primary.did(), "|"); !strings.Contains(got, "receive rpool/subvol-101-disk-0@zrepl_1 rollback=true") {
		t.Errorf("primary actions = %s", got)
	}
}
