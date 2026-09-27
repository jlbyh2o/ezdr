package api

import (
	"context"
	"strings"
	"sync"
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

// fakeHost plays an enrolled host: it applies desired state, answers
// takeover actions, and (as a DR host with pull jobs) reports replication.
type fakeHost struct {
	d  *Deps
	id string
	// fullSend makes the DR host report a full send, which fails the
	// takeover's check.
	fullSend bool

	mu      sync.Mutex
	actions []string
	pulls   []string
}

func (f *fakeHost) run(ctx context.Context, t *testing.T) {
	_, outbox, release := f.d.Hub.connect(ctx, f.id)
	go func() {
		defer release()
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case msg := <-outbox:
				if ds := msg.GetDesiredState(); ds != nil {
					f.mu.Lock()
					f.pulls = nil
					for _, j := range ds.GetZrepl().GetPullJobs() {
						f.pulls = append(f.pulls, j.Name)
					}
					f.mu.Unlock()
					if err := f.d.Store.SetHostApplied(ctx, f.id, ds.Generation, ""); err != nil {
						t.Error(err)
					}
				}
				if a := msg.GetAction(); a != nil {
					f.answer(a)
				}
			case <-tick.C:
				f.report(ctx, t)
			}
		}
	}()
}

func (f *fakeHost) answer(a *clientv1.Action) {
	ack := &clientv1.AckActionRequest{ActionId: a.Id, Succeeded: true}
	var name string
	switch k := a.Kind.(type) {
	case *clientv1.Action_ZreplPreflight:
		name = "preflight"
		ack.Preflight = &clientv1.ZreplPreflightResult{ZreplVersion: "v0.7.0", ZreplRunning: true}
		for _, ds := range k.ZreplPreflight.Datasets {
			ack.Preflight.Datasets = append(ack.Preflight.Datasets, &clientv1.DatasetSnapshots{Dataset: ds, Exists: true,
				Snapshots: []*clientv1.SnapshotInfo{{Name: "@zrepl_1", Guid: 7, Createtxg: 1}}})
		}
	case *clientv1.Action_ZreplRemoveJobs:
		name = "remove " + strings.Join(k.ZreplRemoveJobs.Jobs, ",")
	case *clientv1.Action_ZreplRestoreConfig:
		name = "restore"
	case *clientv1.Action_ZreplReleaseJobs:
		name = "release " + strings.Join(k.ZreplReleaseJobs.Jobs, ",")
		ack.Output = []string{"destroy hold"}
	default:
		name = "other"
	}
	f.mu.Lock()
	f.actions = append(f.actions, name)
	f.mu.Unlock()
	f.d.Hub.deliver(f.id, ack)
}

func (f *fakeHost) report(ctx context.Context, t *testing.T) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.pulls) == 0 {
		return
	}
	st := &clientv1.ReportReplicationRequest{}
	for _, name := range f.pulls {
		st.Jobs = append(st.Jobs, &clientv1.JobStatus{Name: name, Type: "pull", State: "done", AttemptStartedAt: timestamppb.Now(),
			Datasets: []*clientv1.DatasetStatus{{Dataset: "rpool/subvol-101-disk-0", State: "done", FullSend: f.fullSend}}})
	}
	b, _ := proto.Marshal(st)
	if err := f.d.Store.PutReplicationStatus(ctx, f.id, b); err != nil {
		t.Error(err)
	}
}

func (f *fakeHost) did() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.actions...)
}

// takeoverFixture creates an adopted plan between hosts running a
// hand-written setup.
func takeoverFixture(t *testing.T) (*Deps, context.Context, string, *fakeHost, *fakeHost) {
	t.Helper()
	old := takeoverPoll
	takeoverPoll = 5 * time.Millisecond
	t.Cleanup(func() { takeoverPoll = old })

	d, ctx, primary, dr := planTestDeps(t)
	svc := PlanService{Deps: d}
	for id, jobs := range map[string][]*inventoryv1.ZreplJob{
		primary: {{Name: "old_source", Type: "source", ListenAddress: "192.0.2.10:8888", SnapshottingType: "periodic",
			SnapshotPrefix: "zrepl_", SnapshotIntervalSeconds: 300,
			Filesystems: []*inventoryv1.ZreplFilter{{Pattern: "rpool<", Include: true}}}},
		dr: {{Name: "old_pull", Type: "pull", ConnectAddress: "192.0.2.10:8888", RootFs: "tank/replicated", IntervalSeconds: 300}},
	} {
		ph, err := svc.loadHost(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		inv := ph.Inventory
		inv.Zrepl = &inventoryv1.Zrepl{Version: "v0.7.0", Running: true, Jobs: jobs}
		data, _ := proto.Marshal(inv)
		if _, err := d.Store.PutInventory(ctx, store.Inventory{HostID: id, Data: data, Hash: []byte(id + "z"), CollectedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
		if _, err := d.Store.SetHostZrepl(ctx, id, "CERT-"+id, "v0.7.0"); err != nil {
			t.Fatal(err)
		}
	}
	sug, _ := svc.SuggestPlan(ctx, connect.NewRequest(&portalv1.SuggestPlanRequest{Spec: &planv1.PlanSpec{
		Name: "Main", PrimaryHostId: primary, DrHostId: dr}}))
	adopted, err := svc.AdoptZreplSetup(ctx, connect.NewRequest(&portalv1.AdoptZreplSetupRequest{
		Spec: sug.Msg.Spec, SourceJob: "old_source", PullJob: "old_pull"}))
	if err != nil {
		t.Fatal(err)
	}
	spec := adopted.Msg.Spec
	spec.NetworkMappings[0].TargetBridge = "vmbr0"
	spec.TestBridge = "vmbr99"
	created, err := svc.CreatePlan(ctx, connect.NewRequest(&portalv1.CreatePlanRequest{Spec: spec}))
	if err != nil {
		t.Fatal(err)
	}
	if errs, _ := countIssues(created.Msg.Issues); errs > 0 {
		t.Fatalf("adopted plan has errors: %v", created.Msg.Issues)
	}

	hctx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	p, h := &fakeHost{d: d, id: primary}, &fakeHost{d: d, id: dr}
	p.run(hctx, t)
	h.run(hctx, t)
	return d, ctx, created.Msg.Plan.Id, p, h
}

func countIssues(is []*planv1.Issue) (errs, warns int) {
	for _, i := range is {
		if i.Severity == planv1.Severity_SEVERITY_ERROR {
			errs++
		} else {
			warns++
		}
	}
	return errs, warns
}

// runTakeoverTest runs the preflight and the takeover, and waits for it to
// finish.
func runTakeoverTest(ctx context.Context, t *testing.T, d *Deps, id string) *portalv1.Takeover {
	t.Helper()
	svc := PlanService{Deps: d}
	pre, err := svc.RunTakeoverPreflight(ctx, connect.NewRequest(&portalv1.RunTakeoverPreflightRequest{Id: id}))
	if err != nil {
		t.Fatal(err)
	}
	if pre.Msg.Takeover.State != portalv1.TakeoverState_TAKEOVER_STATE_READY {
		t.Fatalf("preflight = %v", pre.Msg.Takeover)
	}
	if ds := pre.Msg.Takeover.Preflight.Datasets; len(ds) != 1 || ds[0].CommonSnapshot != "@zrepl_1" {
		t.Fatalf("preflight datasets = %v", ds)
	}
	if _, err := svc.StartTakeover(ctx, connect.NewRequest(&portalv1.StartTakeoverRequest{Id: id})); err != nil {
		t.Fatal(err)
	}
	// Plans can't change while their takeover runs.
	if _, err := svc.DeletePlan(ctx, connect.NewRequest(&portalv1.DeletePlanRequest{Id: id})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Errorf("delete during takeover: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		got, err := svc.GetTakeover(ctx, connect.NewRequest(&portalv1.GetTakeoverRequest{Id: id}))
		if err != nil {
			t.Fatal(err)
		}
		if got.Msg.Takeover.State != portalv1.TakeoverState_TAKEOVER_STATE_RUNNING {
			return got.Msg.Takeover
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("takeover didn't finish")
	return nil
}

func TestTakeover(t *testing.T) {
	d, ctx, id, primary, dr := takeoverFixture(t)
	tk := runTakeoverTest(ctx, t, d, id)
	if tk.State != portalv1.TakeoverState_TAKEOVER_STATE_COMPLETED || tk.Error != "" {
		t.Fatalf("takeover = %v", tk)
	}
	for _, s := range tk.Steps {
		if s.Status != "done" && s.Status != "skipped" {
			t.Errorf("step %q: %s %s", s.Name, s.Status, s.Detail)
		}
	}
	if got := strings.Join(dr.did(), "|"); got != "preflight|remove old_pull|release old_pull" {
		t.Errorf("DR actions = %s", got)
	}
	if got := strings.Join(primary.did(), "|"); got != "preflight|remove old_source|release old_source" {
		t.Errorf("primary actions = %s", got)
	}
	sp, spec, err := PlanService{Deps: d}.loadPlan(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if sp.State != store.PlanActive || spec.Takeover != nil {
		t.Errorf("plan after takeover: state %s, takeover %v", sp.State, spec.Takeover)
	}
	// The active plan keeps its jobs on the DR host.
	ds, err := d.desiredState(ctx, dr.id)
	if err != nil || len(ds.GetZrepl().GetPullJobs()) != 1 {
		t.Errorf("DR desired state after takeover = %v, %v", ds, err)
	}
}

func TestTakeoverRollsBack(t *testing.T) {
	d, ctx, id, primary, dr := takeoverFixture(t)
	dr.fullSend = true
	tk := runTakeoverTest(ctx, t, d, id)
	if tk.State != portalv1.TakeoverState_TAKEOVER_STATE_ROLLED_BACK || !strings.Contains(tk.Error, "sent in full") {
		t.Fatalf("takeover = %v", tk)
	}
	if got := strings.Join(dr.did(), "|"); got != "preflight|remove old_pull|restore" {
		t.Errorf("DR actions = %s", got)
	}
	if got := strings.Join(primary.did(), "|"); got != "preflight|remove old_source|restore" {
		t.Errorf("primary actions = %s", got)
	}
	sp, _, _ := PlanService{Deps: d}.loadPlan(ctx, id)
	if sp.State != store.PlanDraft {
		t.Errorf("plan state after rollback = %s", sp.State)
	}
	for _, h := range []string{primary.id, dr.id} {
		if ds, _ := d.desiredState(ctx, h); len(ds.GetZrepl().GetPullJobs())+len(ds.GetZrepl().GetSourceJobs()) != 0 {
			t.Errorf("host %s still has the plan's jobs: %v", h, ds)
		}
	}
}
