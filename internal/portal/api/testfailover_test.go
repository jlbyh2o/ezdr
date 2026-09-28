package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	planv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/plan/v1"
	portalv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/portal/v1"
	"github.com/jlbyh2o/ezdr/internal/portal/store"
)

// testFixture activates a plan protecting container 101 and reports its
// configuration, with a fake DR host.
func testFixture(t *testing.T) (*Deps, context.Context, string, *fakeHost) {
	t.Helper()
	oldPoll := testPoll
	testPoll = 5 * time.Millisecond
	t.Cleanup(func() { testPoll = oldPoll })

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
	sug.Msg.Spec.TestBridge = "vmbr99"
	created, err := svc.CreatePlan(ctx, connect.NewRequest(&portalv1.CreatePlanRequest{Spec: sug.Msg.Spec}))
	if err != nil {
		t.Fatal(err)
	}
	id := created.Msg.Plan.Id
	if _, err := svc.ActivatePlan(ctx, connect.NewRequest(&portalv1.ActivatePlanRequest{Id: id})); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Store.PutGuestConfigs(ctx, primary, []store.GuestConfig{{VMID: 101, Type: "lxc", Config: "hostname: web\n"}}); err != nil {
		t.Fatal(err)
	}
	hctx, cancel := context.WithCancel(ctx)
	h := &fakeHost{d: d, id: dr}
	h.run(hctx, t)
	t.Cleanup(func() {
		cancel()
		h.wait()
	})
	return d, ctx, id, h
}

// waitTest waits until the test reaches a state and its runner has stopped.
func waitTest(ctx context.Context, t *testing.T, d *Deps, id string, want portalv1.TestState) *portalv1.TestRun {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		_, tr, err := d.loadTest(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if _, busy := runningTests.Load(id); !busy && tr.State == want {
			return tr
		}
		time.Sleep(5 * time.Millisecond)
	}
	_, tr, _ := d.loadTest(ctx, id)
	t.Fatalf("test didn't reach %v: %v", want, tr)
	return nil
}

func TestTestFailover(t *testing.T) {
	d, ctx, planID, dr := testFixture(t)
	svc := TestFailoverService{Deps: d}

	opts, err := svc.GetTestOptions(ctx, connect.NewRequest(&portalv1.GetTestOptionsRequest{PlanId: planID}))
	if err != nil {
		t.Fatal(err)
	}
	g := opts.Msg.Guests
	if len(g) != 1 || g[0].TestVmid != 10101 || g[0].Problem != "" || len(opts.Msg.PointsInTime) != 2 ||
		opts.Msg.PointsInTime[0].Snapshot != "zrepl_2" || opts.Msg.MemoryAvailableBytes != 1<<30 {
		t.Fatalf("options = %v", opts.Msg)
	}

	if _, err := svc.StartTest(ctx, connect.NewRequest(&portalv1.StartTestRequest{PlanId: planID, Vmids: []uint32{101}, Snapshot: "nope"})); err == nil {
		t.Error("started from a snapshot the guests don't have")
	}
	started, err := svc.StartTest(ctx, connect.NewRequest(&portalv1.StartTestRequest{PlanId: planID, Vmids: []uint32{101}, Snapshot: "zrepl_1"}))
	if err != nil {
		t.Fatal(err)
	}
	id := started.Msg.Test.Id
	// One test at a time per plan.
	if _, err := svc.StartTest(ctx, connect.NewRequest(&portalv1.StartTestRequest{PlanId: planID, Vmids: []uint32{101}, Snapshot: "zrepl_1"})); err == nil {
		t.Error("started a second test")
	}
	tr := waitTest(ctx, t, d, id, portalv1.TestState_TEST_STATE_RUNNING)
	if tr.Guests[0].Status != "running" || tr.Snapshot != "zrepl_1" || len(tr.Notes) != 1 {
		t.Errorf("running test = %v", tr)
	}

	before := tr.Deadline.AsTime()
	ext, err := svc.ExtendTest(ctx, connect.NewRequest(&portalv1.ExtendTestRequest{Id: id}))
	if err != nil || !ext.Msg.Test.Deadline.AsTime().After(before) {
		t.Errorf("extend = %v, %v", ext, err)
	}
	if _, err := svc.SetTestVerdict(ctx, connect.NewRequest(&portalv1.SetTestVerdictRequest{Id: id,
		Verdict: portalv1.TestVerdict_TEST_VERDICT_PASSED, Notes: " web answered "})); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.EndTest(ctx, connect.NewRequest(&portalv1.EndTestRequest{Id: id})); err != nil {
		t.Fatal(err)
	}
	tr = waitTest(ctx, t, d, id, portalv1.TestState_TEST_STATE_ENDED)
	if tr.EndedBy != "admin" || tr.Verdict != portalv1.TestVerdict_TEST_VERDICT_PASSED || tr.VerdictNotes != "web answered" || tr.EndedAt == nil {
		t.Errorf("ended test = %v", tr)
	}
	if got := strings.Join(dr.did(), "|"); got != "test options|test options|test options|test prepare zrepl_1|test start 10101|test check|test cleanup" {
		t.Errorf("DR actions = %s", got)
	}
	list, _ := svc.ListTests(ctx, connect.NewRequest(&portalv1.ListTestsRequest{PlanId: planID}))
	if len(list.Msg.Tests) != 1 {
		t.Errorf("history = %v", list.Msg.Tests)
	}
}

func TestTestFailoverTimeLimit(t *testing.T) {
	d, ctx, planID, _ := testFixture(t)
	svc := TestFailoverService{Deps: d}
	started, err := svc.StartTest(ctx, connect.NewRequest(&portalv1.StartTestRequest{PlanId: planID, Vmids: []uint32{101}, Snapshot: "zrepl_2"}))
	if err != nil {
		t.Fatal(err)
	}
	id := started.Msg.Test.Id
	waitTest(ctx, t, d, id, portalv1.TestState_TEST_STATE_RUNNING)
	if _, err := d.updateTest(ctx, id, func(_ *store.TestRun, tr *portalv1.TestRun) error {
		tr.Deadline = timestamppb.New(time.Now().Add(-time.Second))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	go d.runTest(ctx, id)
	if tr := waitTest(ctx, t, d, id, portalv1.TestState_TEST_STATE_ENDED); tr.EndedBy != endedByTimeLimit {
		t.Errorf("ended by %q", tr.EndedBy)
	}
}

func TestTestFailoverSetupFailure(t *testing.T) {
	d, ctx, planID, dr := testFixture(t)
	dr.failPrepare = true
	svc := TestFailoverService{Deps: d}
	started, err := svc.StartTest(ctx, connect.NewRequest(&portalv1.StartTestRequest{PlanId: planID, Vmids: []uint32{101}, Snapshot: "zrepl_2"}))
	if err != nil {
		t.Fatal(err)
	}
	tr := waitTest(ctx, t, d, started.Msg.Test.Id, portalv1.TestState_TEST_STATE_FAILED)
	if !strings.Contains(tr.Error, "clone failed") || !strings.HasSuffix(strings.Join(dr.did(), "|"), "test cleanup") {
		t.Errorf("failed test = %v, actions %v", tr, dr.did())
	}
	// A failed test frees the plan for the next one.
	if _, err := svc.StartTest(ctx, connect.NewRequest(&portalv1.StartTestRequest{PlanId: planID, Vmids: []uint32{101}, Snapshot: "zrepl_2"})); err != nil {
		t.Errorf("starting again after a failure: %v", err)
	}
}
