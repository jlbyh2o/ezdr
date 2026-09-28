package client

import (
	"path/filepath"
	"testing"

	"github.com/jlbyh2o/ezdr/internal/client/failover"
	"github.com/jlbyh2o/ezdr/internal/client/guests"
	clientv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/client/v1"
)

func TestWithoutBreakGlass(t *testing.T) {
	dir := t.TempDir()
	gp := guests.Paths{PVE: dir, Plans: filepath.Join(dir, "plans")}
	if err := guests.Store(gp, nil, []*clientv1.PlanRecovery{{PlanId: "plan1abcdef"}, {PlanId: "plan2abcdef"}}); err != nil {
		t.Fatal(err)
	}
	a := &applier{failover: &failover.Runner{Guests: gp}}
	if err := a.failover.WriteMarker(failover.Marker{PlanID: "plan1abcdef"}); err != nil {
		t.Fatal(err)
	}
	ds := &clientv1.DesiredState{Zrepl: &clientv1.Zrepl{PullJobs: []*clientv1.PullJob{
		{Name: "ezdr_plan1abc_local-zfs_pull"}, {Name: "ezdr_plan2abc_local-zfs_pull"},
	}}}
	// The portal doesn't know yet: the marked plan's job is dropped.
	z := a.withoutBreakGlass(ds)
	if len(z.PullJobs) != 1 || z.PullJobs[0].Name != "ezdr_plan2abc_local-zfs_pull" || len(ds.Zrepl.PullJobs) != 2 {
		t.Errorf("jobs = %v (desired state must not be modified)", z.PullJobs)
	}
	// The portal recorded it: the marker goes.
	ds.FailedOverPlans = []string{"plan1abcdef"}
	a.withoutBreakGlass(ds)
	if ms, _ := a.failover.Markers(); len(ms) != 0 {
		t.Errorf("marker kept after the portal recorded the failover: %v", ms)
	}
}
