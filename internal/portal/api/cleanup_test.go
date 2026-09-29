package api

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	inventoryv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/inventory/v1"
	portalv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/portal/v1"
	"github.com/jlbyh2o/ezdr/internal/portal/store"
)

// waitCleanup waits for the plan's cleanup of a kind to reach a state and
// its runner to stop.
func waitCleanup(ctx context.Context, t *testing.T, d *Deps, kind portalv1.DataCleanupKind, planID string, want portalv1.DataCleanupState) *portalv1.DataCleanup {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		_, c, err := d.loadCleanup(ctx, kind, planID)
		if err == nil && c.State == want {
			if _, busy := runningCleanups.Load(kind.String() + "/" + planID); !busy {
				return c
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	_, c, _ := d.loadCleanup(ctx, kind, planID)
	t.Fatalf("cleanup didn't reach %v: %v", want, c)
	return nil
}

func TestTakeoverCleanup(t *testing.T) {
	d, ctx, id, primary, dr := takeoverFixture(t)
	svc := PlanService{Deps: d}
	kind := portalv1.DataCleanupKind_DATA_CLEANUP_KIND_TAKEOVER
	if _, err := svc.PreviewTakeoverCleanup(ctx, connect.NewRequest(&portalv1.PreviewTakeoverCleanupRequest{PlanId: id})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("preview before the takeover: %v", err)
	}
	if tk := runTakeoverTest(ctx, t, d, id); tk.State != portalv1.TakeoverState_TAKEOVER_STATE_COMPLETED {
		t.Fatalf("takeover = %v", tk)
	}
	// The old jobs also replicated the pool root and a cloud-init volume.
	row, err := d.Store.TakeoverByPlan(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	tk := &portalv1.Takeover{}
	if err := proto.Unmarshal(row.Data, tk); err != nil {
		t.Fatal(err)
	}
	tk.Preflight.DroppedDatasets = []string{"rpool", "rpool/vm-101-cloudinit"}
	if row.Data, err = proto.Marshal(tk); err != nil {
		t.Fatal(err)
	}
	if err := d.Store.PutTakeover(ctx, row); err != nil {
		t.Fatal(err)
	}
	// After the takeover, the primary no longer reports the old job.
	priHost, err := svc.loadHost(ctx, primary.id)
	if err != nil {
		t.Fatal(err)
	}
	priHost.Inventory.Zrepl.Jobs = nil
	data, _ := proto.Marshal(priHost.Inventory)
	if _, err := d.Store.PutInventory(ctx, store.Inventory{HostID: primary.id, Data: data, Hash: []byte("pri2"), CollectedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	drHost, err := svc.loadHost(ctx, dr.id)
	if err != nil {
		t.Fatal(err)
	}
	drHost.Inventory.ZfsDatasets = []*inventoryv1.ZfsDataset{{Name: "tank/replicated"}, {Name: "tank/replicated/rpool"},
		{Name: "tank/replicated/rpool/subvol-101-disk-0"}, {Name: "tank/replicated/rpool/vm-101-cloudinit"}}
	data, _ = proto.Marshal(drHost.Inventory)
	if _, err := d.Store.PutInventory(ctx, store.Inventory{HostID: dr.id, Data: data, Hash: []byte("dr2"), CollectedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	pre, err := svc.PreviewTakeoverCleanup(ctx, connect.NewRequest(&portalv1.PreviewTakeoverCleanupRequest{PlanId: id}))
	if err != nil {
		t.Fatal(err)
	}
	p := pre.Msg.Preview
	if len(p.Problems) != 0 || len(p.Hosts) != 2 || len(p.Hosts[0].Datasets) != 2 || len(p.Hosts[1].Datasets) != 2 ||
		p.Hosts[1].ReclaimBytes != 2<<20 {
		t.Fatalf("preview = %v", p)
	}
	if _, err := svc.StartTakeoverCleanup(ctx, connect.NewRequest(&portalv1.StartTakeoverCleanupRequest{PlanId: id})); err != nil {
		t.Fatal(err)
	}
	c := waitCleanup(ctx, t, d, kind, id, portalv1.DataCleanupState_DATA_CLEANUP_STATE_COMPLETED)
	if c.Steps[0].Status != "done" || c.Steps[1].Status != "done" || c.CompletedAt == nil {
		t.Errorf("cleanup = %v", c)
	}
	if got := primary.did(); got[len(got)-1] != "cleanup rpool:zrepl_,rpool/vm-101-cloudinit:zrepl_ release " {
		t.Errorf("primary actions = %q", got)
	}
	// The pool root's replica holds the other replicas: only its snapshots go.
	if got := dr.did(); got[len(got)-1] != "cleanup tank/replicated/rpool:zrepl_,tank/replicated/rpool/vm-101-cloudinit:destroy release " {
		t.Errorf("DR actions = %q", got)
	}
}

func TestDeletePlanWithData(t *testing.T) {
	d, ctx, id, primary, dr := takeoverFixture(t)
	svc := PlanService{Deps: d}
	kind := portalv1.DataCleanupKind_DATA_CLEANUP_KIND_PLAN
	if tk := runTakeoverTest(ctx, t, d, id); tk.State != portalv1.TakeoverState_TAKEOVER_STATE_COMPLETED {
		t.Fatalf("takeover = %v", tk)
	}
	del := func(name string) error {
		_, err := svc.DeletePlan(ctx, connect.NewRequest(&portalv1.DeletePlanRequest{Id: id, DeleteData: true, ConfirmName: name}))
		return err
	}
	if err := del("Main"); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("delete an active plan: %v", err)
	}
	if _, err := svc.DeactivatePlan(ctx, connect.NewRequest(&portalv1.DeactivatePlanRequest{Id: id})); err != nil {
		t.Fatal(err)
	}
	if err := del("main"); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("wrong confirmation: %v", err)
	}

	// A problem on a host blocks it.
	dr.mu.Lock()
	dr.scanProblem = "used by guest 101"
	dr.mu.Unlock()
	if err := del("Main"); connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "used by guest 101") {
		t.Errorf("delete with a problem: %v", err)
	}
	dr.mu.Lock()
	dr.scanProblem, dr.failDataCleanup = "", 1
	dr.mu.Unlock()

	// A failed step waits for a retry; meanwhile the plan shows as deleting
	// and can't change.
	if err := del("Main"); err != nil {
		t.Fatal(err)
	}
	c := waitCleanup(ctx, t, d, kind, id, portalv1.DataCleanupState_DATA_CLEANUP_STATE_FAILED)
	if c.Steps[0].Status != "done" || c.Steps[1].Status != "failed" || !strings.Contains(c.Error, "zfs destroy failed") {
		t.Errorf("failed cleanup = %v", c)
	}
	got, err := svc.GetPlan(ctx, connect.NewRequest(&portalv1.GetPlanRequest{Id: id}))
	if err != nil || got.Msg.Plan.State != portalv1.PlanState_PLAN_STATE_DELETING {
		t.Fatalf("plan while deleting = %v, %v", got, err)
	}
	if _, err := svc.ActivatePlan(ctx, connect.NewRequest(&portalv1.ActivatePlanRequest{Id: id})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("activate while deleting: %v", err)
	}
	if _, err := svc.RetryDataCleanup(ctx, connect.NewRequest(&portalv1.RetryDataCleanupRequest{PlanId: id, Kind: kind})); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := d.Store.PlanByID(ctx, id); errors.Is(err, store.ErrNotFound) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("plan not deleted")
		}
		time.Sleep(5 * time.Millisecond)
	}
	wantPri := "cleanup rpool/subvol-101-disk-0:zrepl_ release ezdr_" + id[:8] + "_local-zfs"
	if got := primary.did(); got[len(got)-1] != wantPri {
		t.Errorf("primary actions = %q, want last %q", got, wantPri)
	}
	wantDR := "cleanup tank/replicated/rpool/subvol-101-disk-0:destroy release ezdr_" + id[:8] + "_local-zfs_pull"
	if got := dr.did(); got[len(got)-1] != wantDR || got[len(got)-2] != wantDR {
		t.Errorf("DR actions = %q, want the last two %q", got, wantDR)
	}
}

func TestCancelPlanDeletion(t *testing.T) {
	d, ctx, id, _, dr := takeoverFixture(t)
	svc := PlanService{Deps: d}
	kind := portalv1.DataCleanupKind_DATA_CLEANUP_KIND_PLAN
	dr.mu.Lock()
	dr.failDataCleanup = 1
	dr.mu.Unlock()
	if _, err := svc.DeletePlan(ctx, connect.NewRequest(&portalv1.DeletePlanRequest{Id: id, DeleteData: true, ConfirmName: "Main"})); err != nil {
		t.Fatal(err)
	}
	waitCleanup(ctx, t, d, kind, id, portalv1.DataCleanupState_DATA_CLEANUP_STATE_FAILED)
	if _, err := svc.CancelDataCleanup(ctx, connect.NewRequest(&portalv1.CancelDataCleanupRequest{PlanId: id, Kind: kind})); err != nil {
		t.Fatal(err)
	}
	got, err := svc.GetPlan(ctx, connect.NewRequest(&portalv1.GetPlanRequest{Id: id}))
	if err != nil || got.Msg.Plan.State != portalv1.PlanState_PLAN_STATE_DRAFT {
		t.Errorf("plan after canceling = %v, %v", got, err)
	}
}
