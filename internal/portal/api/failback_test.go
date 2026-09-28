package api

import (
	"strings"
	"testing"

	"connectrpc.com/connect"

	portalv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/portal/v1"
	"github.com/jlbyh2o/ezdr/internal/portal/store"
)

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
