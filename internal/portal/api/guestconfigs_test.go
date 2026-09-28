package api

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	clientv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/client/v1"
	planv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/plan/v1"
	portalv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/portal/v1"
	"github.com/jlbyh2o/ezdr/internal/portal/store"
)

func TestGuestConfigRelay(t *testing.T) {
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
	created, err := svc.CreatePlan(ctx, connect.NewRequest(&portalv1.CreatePlanRequest{Spec: sug.Msg.Spec}))
	if err != nil {
		t.Fatal(err)
	}
	id := created.Msg.Plan.Id

	// Drafts ask for nothing.
	if ds, _ := d.desiredState(ctx, primary); len(ds.ReportGuestConfigs) != 0 {
		t.Errorf("draft plan asked for configurations: %v", ds.ReportGuestConfigs)
	}
	if _, err := svc.ActivatePlan(ctx, connect.NewRequest(&portalv1.ActivatePlanRequest{Id: id})); err != nil {
		t.Fatal(err)
	}
	ds, _ := d.desiredState(ctx, primary)
	if len(ds.ReportGuestConfigs) != 1 || ds.ReportGuestConfigs[0] != 101 {
		t.Fatalf("primary asked for %v", ds.ReportGuestConfigs)
	}

	// The primary reports; the DR host's desired state carries it.
	report := func(conf string) {
		t.Helper()
		hctx := context.WithValue(ctx, hostKey{}, store.Host{ID: primary, Hostname: "pve1"})
		if _, err := (ClientService{Deps: d}).ReportGuestConfigs(hctx, connect.NewRequest(&clientv1.ReportGuestConfigsRequest{
			Guests: []*clientv1.GuestConfig{{Vmid: 101, Type: "lxc", Config: conf}}})); err != nil {
			t.Fatal(err)
		}
	}
	report("hostname: web\n")
	drDS, _ := d.desiredState(ctx, dr)
	if len(drDS.PlanGuestConfigs) != 1 || len(drDS.PlanGuestConfigs[0].Guests) != 1 ||
		drDS.PlanGuestConfigs[0].Guests[0].Config != "hostname: web\n" || drDS.PlanGuestConfigs[0].PrimaryHostname != "pve1" {
		t.Fatalf("DR desired state = %v", drDS.PlanGuestConfigs)
	}
	// An unchanged report doesn't change the DR host's desired state.
	gen := drDS.Generation
	report("hostname: web\n")
	if again, _ := d.desiredState(ctx, dr); again.Generation != gen {
		t.Errorf("unchanged report changed the generation: %d -> %d", gen, again.Generation)
	}

	// Pausing keeps the configurations; deactivating removes them.
	if _, err := svc.PausePlan(ctx, connect.NewRequest(&portalv1.PausePlanRequest{Id: id})); err != nil {
		t.Fatal(err)
	}
	if ds, _ := d.desiredState(ctx, dr); len(ds.PlanGuestConfigs) != 1 {
		t.Errorf("paused plan lost its configurations")
	}
	if _, err := svc.DeactivatePlan(ctx, connect.NewRequest(&portalv1.DeactivatePlanRequest{Id: id})); err != nil {
		t.Fatal(err)
	}
	if ds, _ := d.desiredState(ctx, dr); len(ds.PlanGuestConfigs) != 0 {
		t.Errorf("deactivated plan kept its configurations: %v", ds.PlanGuestConfigs)
	}
}
