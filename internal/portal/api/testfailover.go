package api

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	clientv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/client/v1"
	inventoryv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/inventory/v1"
	planv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/plan/v1"
	portalv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/portal/v1"
	"github.com/jlbyh2o/ezdr/internal/plan"
	"github.com/jlbyh2o/ezdr/internal/portal/store"
)

// Test failovers: see docs/design/test-failover.md.

// TestFailoverService runs test failovers.
type TestFailoverService struct{ *Deps }

// Steps of a test, in order. Steps up to testStepCheck set the test up;
// testStepCleanup runs when it ends.
const (
	testStepPrepare = iota
	testStepStart
	testStepCheck
	testStepCleanup
)

var testStepNames = [...]string{
	"Clone the replicas and register the test guests",
	"Start the guests in order",
	"Check the guests",
	"Remove the test guests and clones",
}

const (
	// guestCheckTimeout is how long a guest may take to run (and its guest
	// agent to answer).
	guestCheckTimeout   = 5 * time.Minute
	endedBySetupFailure = "setup failure"
	endedByTimeLimit    = "time limit"
)

// testPoll is how often the runner checks guests and the supervisor checks
// tests; tests shorten it.
var testPoll = 10 * time.Second

// testPlan is what a test needs to know about its plan.
type testPlan struct {
	row         store.Plan
	spec        *planv1.PlanSpec
	primary, dr *inventoryv1.Inventory
	drID        string
	configs     map[uint32]store.GuestConfig
}

// loadTestPlan loads an active or paused plan with its hosts' inventories.
func (d *Deps) loadTestPlan(ctx context.Context, planID string) (*testPlan, error) {
	sp, err := d.Store.PlanByID(ctx, planID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("plan not found"))
	}
	if err != nil {
		return nil, internalError(err)
	}
	if (sp.State != store.PlanActive && sp.State != store.PlanPaused) || sp.AppliedSpec == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("only an active or paused plan has replicas to test"))
	}
	spec := &planv1.PlanSpec{}
	if err := proto.Unmarshal(sp.AppliedSpec, spec); err != nil {
		return nil, internalError(err)
	}
	primary, dr, err := d.loadInventoriesFor(ctx, spec)
	if err != nil {
		return nil, err
	}
	configs, err := d.Store.GuestConfigs(ctx, spec.PrimaryHostId)
	if err != nil {
		return nil, internalError(err)
	}
	return &testPlan{row: sp, spec: spec, primary: primary, dr: dr, drID: spec.DrHostId, configs: configs}, nil
}

// testGuest describes one of a plan's guests for a test.
type testGuest struct {
	option *portalv1.TestGuestOption
	guest  *clientv1.TestGuest
	delay  time.Duration
}

// guests lists the plan's guests in startup order, with their disks'
// replicas and clones.
func (tp *testPlan) guests() []testGuest {
	receive := map[string]string{}
	for _, m := range tp.spec.StorageMappings {
		receive[m.SourceStorage] = m.ReceiveDataset
	}
	inv := map[uint32]*inventoryv1.Guest{}
	for _, g := range tp.primary.GetGuests() {
		inv[g.Vmid] = g
	}
	offset := plan.TestVMIDOffset(tp.spec)
	var out []testGuest
	for _, pg := range tp.spec.Guests {
		testID := pg.Vmid + offset
		o := &portalv1.TestGuestOption{Vmid: pg.Vmid, TestVmid: testID, StartupOrder: pg.StartupOrder}
		tg := &clientv1.TestGuest{Vmid: pg.Vmid, TestVmid: testID}
		g := inv[pg.Vmid]
		if g == nil {
			o.Problem = "the guest no longer exists on the primary"
		} else {
			o.Name, o.MemoryBytes = g.Name, g.MemoryBytes
			o.Type, tg.Type = "qemu", "qemu"
			if g.Type == inventoryv1.GuestType_GUEST_TYPE_CONTAINER {
				o.Type, tg.Type = "lxc", "lxc"
			}
			for _, disk := range g.Disks {
				if disk.Readiness != inventoryv1.Readiness_READINESS_REPLICABLE || disk.ZfsDataset == "" {
					continue
				}
				tg.Disks = append(tg.Disks, &clientv1.TestDisk{
					Volume:  disk.Storage + ":" + disk.Volume,
					Replica: receive[disk.Storage] + "/" + disk.ZfsDataset,
					Clone:   cloneName(disk.Volume, pg.Vmid, testID),
				})
			}
			if c, ok := tp.configs[pg.Vmid]; ok {
				o.ConfigChangedAt = timestamppb.New(c.ChangedAt)
			} else {
				o.Problem = "the DR host doesn't have the guest's configuration yet"
			}
		}
		out = append(out, testGuest{option: o, guest: tg, delay: time.Duration(pg.StartupDelaySeconds) * time.Second})
	}
	slices.SortStableFunc(out, func(a, b testGuest) int {
		return cmp.Or(cmp.Compare(a.option.StartupOrder, b.option.StartupOrder), cmp.Compare(a.option.Vmid, b.option.Vmid))
	})
	return out
}

// cloneName renames a volume for the test VMID: "vm-201-disk-0" becomes
// "vm-10201-disk-0". Linked clones ("base-200-disk-0/vm-201-disk-0") use
// their own volume's name.
func cloneName(volume string, vmid, testID uint32) string {
	if i := strings.LastIndex(volume, "/"); i >= 0 {
		volume = volume[i+1:]
	}
	id := "-" + strconv.FormatUint(uint64(vmid), 10) + "-"
	return strings.Replace(volume, id, "-"+strconv.FormatUint(uint64(testID), 10)+"-", 1)
}

// options asks the DR host for the replicas' snapshots and works out which
// guests can be tested and from which points in time.
func (d *Deps) testOptions(ctx context.Context, tp *testPlan, selected map[uint32]bool) ([]testGuest, *portalv1.GetTestOptionsResponse, error) {
	gs := tp.guests()
	var replicas []string
	for _, g := range gs {
		for _, disk := range g.guest.Disks {
			replicas = append(replicas, disk.Replica)
		}
	}
	ack, err := d.request(ctx, tp.drID, time.Minute, &clientv1.Action{Kind: &clientv1.Action_TestOptions{TestOptions: &clientv1.TestOptions{
		Replicas: replicas, SnapshotPrefix: tp.spec.SnapshotPrefix}}})
	if err != nil {
		return nil, nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("DR host: %w", err))
	}
	snaps := map[string]map[string]*clientv1.TestSnapshot{}
	for _, r := range ack.GetTestOptions().GetReplicas() {
		m := map[string]*clientv1.TestSnapshot{}
		for _, s := range r.Snapshots {
			m[s.Name] = s
		}
		snaps[r.Replica] = m
	}
	resp := &portalv1.GetTestOptionsResponse{MemoryAvailableBytes: ack.GetTestOptions().GetMemoryAvailableBytes()}
	var common map[string]*clientv1.TestSnapshot
	for _, g := range gs {
		resp.Guests = append(resp.Guests, g.option)
		if g.option.Problem == "" && len(g.guest.Disks) == 0 {
			g.option.Problem = "the guest has no replicated disks"
		}
		for _, disk := range g.guest.Disks {
			if g.option.Problem == "" && len(snaps[disk.Replica]) == 0 {
				g.option.Problem = "its disks haven't been replicated yet"
			}
		}
		if g.option.Problem != "" || (selected != nil && !selected[g.option.Vmid]) {
			continue
		}
		for _, disk := range g.guest.Disks {
			if common == nil {
				common = maps.Clone(snaps[disk.Replica])
				continue
			}
			for name := range common {
				if _, ok := snaps[disk.Replica][name]; !ok {
					delete(common, name)
				}
			}
		}
	}
	for _, s := range common {
		resp.PointsInTime = append(resp.PointsInTime, &portalv1.TestPointInTime{Snapshot: s.Name, CreatedAt: s.CreatedAt})
	}
	slices.SortFunc(resp.PointsInTime, func(a, b *portalv1.TestPointInTime) int {
		return b.CreatedAt.AsTime().Compare(a.CreatedAt.AsTime())
	})
	return gs, resp, nil
}

// GetTestOptions lists what a test of the plan could include.
func (s TestFailoverService) GetTestOptions(ctx context.Context, req *connect.Request[portalv1.GetTestOptionsRequest]) (*connect.Response[portalv1.GetTestOptionsResponse], error) {
	tp, err := s.loadTestPlan(ctx, req.Msg.PlanId)
	if err != nil {
		return nil, err
	}
	_, resp, err := s.testOptions(ctx, tp, nil)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

// StartTest starts a test failover.
func (s TestFailoverService) StartTest(ctx context.Context, req *connect.Request[portalv1.StartTestRequest]) (*connect.Response[portalv1.StartTestResponse], error) {
	tp, err := s.loadTestPlan(ctx, req.Msg.PlanId)
	if err != nil {
		return nil, err
	}
	if tp.spec.TestBridge == "" {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("the plan has no test failover bridge"))
	}
	if runs, err := s.Store.TestRunsByPlan(ctx, tp.row.ID, 1); err != nil {
		return nil, internalError(err)
	} else if len(runs) > 0 && runs[0].Active {
		return nil, connect.NewError(connect.CodeFailedPrecondition, store.ErrTestActive)
	}
	selected := map[uint32]bool{}
	for _, id := range req.Msg.Vmids {
		selected[id] = true
	}
	if len(selected) == 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("choose at least one guest"))
	}
	gs, opts, err := s.testOptions(ctx, tp, selected)
	if err != nil {
		return nil, err
	}
	var point *portalv1.TestPointInTime
	for _, p := range opts.PointsInTime {
		if p.Snapshot == req.Msg.Snapshot {
			point = p
		}
	}
	if point == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("not every selected guest has snapshot %q", req.Msg.Snapshot))
	}

	id := strings.ToLower(store.NewID())
	now := time.Now()
	t := &portalv1.TestRun{
		Id: id, PlanId: tp.row.ID, PlanName: tp.row.Name, State: portalv1.TestState_TEST_STATE_STARTING,
		Snapshot: point.Snapshot, SnapshotAt: point.CreatedAt, StartedBy: currentUser(ctx).Username,
		StartedAt: timestamppb.New(now),
		Deadline:  timestamppb.New(now.Add(time.Duration(plan.TestTimeLimitSeconds(tp.spec)) * time.Second)),
	}
	prep := &clientv1.TestPrepare{PlanId: tp.row.ID, PlanName: tp.row.Name, TestId: id, Snapshot: point.Snapshot, Bridge: tp.spec.TestBridge}
	for _, g := range gs {
		if !selected[g.option.Vmid] {
			continue
		}
		if g.option.Problem != "" {
			return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("guest %d can't be tested: %s", g.option.Vmid, g.option.Problem))
		}
		delete(selected, g.option.Vmid)
		prep.Guests = append(prep.Guests, g.guest)
		t.Guests = append(t.Guests, &portalv1.TestRunGuest{Vmid: g.option.Vmid, TestVmid: g.option.TestVmid, Name: g.option.Name,
			Type: g.option.Type, Status: "pending"})
	}
	if len(selected) > 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("a selected guest isn't in the plan"))
	}
	for _, name := range testStepNames[:testStepCleanup] {
		t.Steps = append(t.Steps, &portalv1.TakeoverStep{Name: name, Status: "pending", UpdatedAt: timestamppb.Now()})
	}
	data, _ := proto.Marshal(t)
	pdata, _ := proto.Marshal(prep)
	err = s.Store.CreateTestRun(ctx, store.TestRun{ID: id, PlanID: tp.row.ID, Active: true, Data: data, Prepare: pdata, StartedAt: now})
	if errors.Is(err, store.ErrTestActive) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	if err != nil {
		return nil, internalError(err)
	}
	s.audit(ctx, currentUser(ctx).Username, "plan.test_start", "plan:"+tp.row.ID,
		fmt.Sprintf("plan %q: test %s of %d guest(s) from %s", tp.row.Name, id, len(t.Guests), point.Snapshot))
	go s.runTest(context.WithoutCancel(ctx), id)
	return connect.NewResponse(&portalv1.StartTestResponse{Test: t}), nil
}

// GetTest returns a test.
func (s TestFailoverService) GetTest(ctx context.Context, req *connect.Request[portalv1.GetTestRequest]) (*connect.Response[portalv1.GetTestResponse], error) {
	_, t, err := s.loadTest(ctx, req.Msg.Id)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&portalv1.GetTestResponse{Test: t}), nil
}

// ListTests returns a plan's recent tests.
func (s TestFailoverService) ListTests(ctx context.Context, req *connect.Request[portalv1.ListTestsRequest]) (*connect.Response[portalv1.ListTestsResponse], error) {
	rows, err := s.Store.TestRunsByPlan(ctx, req.Msg.PlanId, 50)
	if err != nil {
		return nil, internalError(err)
	}
	resp := &portalv1.ListTestsResponse{}
	for _, row := range rows {
		t := &portalv1.TestRun{}
		if err := proto.Unmarshal(row.Data, t); err != nil {
			return nil, internalError(err)
		}
		resp.Tests = append(resp.Tests, t)
	}
	return connect.NewResponse(resp), nil
}

// ExtendTest moves a running test's deadline by the plan's time limit.
func (s TestFailoverService) ExtendTest(ctx context.Context, req *connect.Request[portalv1.ExtendTestRequest]) (*connect.Response[portalv1.ExtendTestResponse], error) {
	t, err := s.updateTest(ctx, req.Msg.Id, func(_ *store.TestRun, t *portalv1.TestRun) error {
		if t.State != portalv1.TestState_TEST_STATE_STARTING && t.State != portalv1.TestState_TEST_STATE_RUNNING {
			return connect.NewError(connect.CodeFailedPrecondition, errors.New("the test isn't running"))
		}
		limit := time.Duration(plan.DefaultTestTimeLimit) * time.Second
		if tp, err := s.loadTestPlan(ctx, t.PlanId); err == nil {
			limit = time.Duration(plan.TestTimeLimitSeconds(tp.spec)) * time.Second
		}
		t.Deadline = timestamppb.New(t.Deadline.AsTime().Add(limit))
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.audit(ctx, currentUser(ctx).Username, "plan.test_extend", "plan:"+t.PlanId,
		fmt.Sprintf("test %s now ends at %s", t.Id, t.Deadline.AsTime().UTC().Format(time.RFC3339)))
	return connect.NewResponse(&portalv1.ExtendTestResponse{Test: t}), nil
}

// EndTest ends a test: its guests and clones are removed.
func (s TestFailoverService) EndTest(ctx context.Context, req *connect.Request[portalv1.EndTestRequest]) (*connect.Response[portalv1.EndTestResponse], error) {
	user := currentUser(ctx).Username
	t, err := s.updateTest(ctx, req.Msg.Id, func(_ *store.TestRun, t *portalv1.TestRun) error {
		if t.State != portalv1.TestState_TEST_STATE_STARTING && t.State != portalv1.TestState_TEST_STATE_RUNNING {
			return connect.NewError(connect.CodeFailedPrecondition, errors.New("the test isn't running"))
		}
		t.State, t.EndedBy = portalv1.TestState_TEST_STATE_ENDING, user
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.audit(ctx, user, "plan.test_end", "plan:"+t.PlanId, fmt.Sprintf("test %s", t.Id))
	go s.runTest(context.WithoutCancel(ctx), t.Id)
	return connect.NewResponse(&portalv1.EndTestResponse{Test: t}), nil
}

// SetTestVerdict records whether the test passed.
func (s TestFailoverService) SetTestVerdict(ctx context.Context, req *connect.Request[portalv1.SetTestVerdictRequest]) (*connect.Response[portalv1.SetTestVerdictResponse], error) {
	if len(req.Msg.Notes) > 4000 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("the notes are too long"))
	}
	user := currentUser(ctx).Username
	t, err := s.updateTest(ctx, req.Msg.Id, func(_ *store.TestRun, t *portalv1.TestRun) error {
		t.Verdict, t.VerdictNotes, t.VerdictBy, t.VerdictAt = req.Msg.Verdict, strings.TrimSpace(req.Msg.Notes), user, timestamppb.Now()
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.audit(ctx, user, "plan.test_verdict", "plan:"+t.PlanId, fmt.Sprintf("test %s: %s", t.Id, t.Verdict))
	return connect.NewResponse(&portalv1.SetTestVerdictResponse{Test: t}), nil
}

func (d *Deps) loadTest(ctx context.Context, id string) (store.TestRun, *portalv1.TestRun, error) {
	row, err := d.Store.TestRunByID(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return row, nil, connect.NewError(connect.CodeNotFound, errors.New("test not found"))
	}
	if err != nil {
		return row, nil, internalError(err)
	}
	t := &portalv1.TestRun{}
	if err := proto.Unmarshal(row.Data, t); err != nil {
		return row, nil, internalError(err)
	}
	return row, t, nil
}

var testLocks sync.Map // test ID -> *sync.Mutex

// updateTest loads a test, applies fn, and saves it, so the runner and
// requests never overwrite each other's changes.
func (d *Deps) updateTest(ctx context.Context, id string, fn func(row *store.TestRun, t *portalv1.TestRun) error) (*portalv1.TestRun, error) {
	mu, _ := testLocks.LoadOrStore(id, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()
	row, t, err := d.loadTest(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := fn(&row, t); err != nil {
		return nil, err
	}
	if row.Data, err = proto.Marshal(t); err != nil {
		return nil, internalError(err)
	}
	row.Active = t.State == portalv1.TestState_TEST_STATE_STARTING || t.State == portalv1.TestState_TEST_STATE_RUNNING ||
		t.State == portalv1.TestState_TEST_STATE_ENDING
	if err := d.Store.UpdateTestRun(ctx, row); err != nil {
		return nil, internalError(err)
	}
	return t, nil
}

var runningTests sync.Map // test ID -> struct{}

// RunTestSupervisor keeps active tests moving until ctx is canceled: it
// resumes them after a portal restart, ends them at their deadline, and
// retries cleanups that failed.
func (d *Deps) RunTestSupervisor(ctx context.Context) {
	ticker := time.NewTicker(3 * testPoll)
	defer ticker.Stop()
	for {
		rows, err := d.Store.ActiveTestRuns(ctx)
		if err != nil && ctx.Err() == nil {
			slog.Error("list active tests", "err", err)
		}
		for _, row := range rows {
			go d.runTest(ctx, row.ID)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// runTest advances a test as far as it can go now.
func (d *Deps) runTest(ctx context.Context, id string) {
	if _, busy := runningTests.LoadOrStore(id, struct{}{}); busy {
		return
	}
	defer runningTests.Delete(id)
	for ctx.Err() == nil {
		row, t, err := d.loadTest(ctx, id)
		if err != nil {
			slog.Error("test", "test", id, "err", err)
			return
		}
		prep := &clientv1.TestPrepare{}
		if err := proto.Unmarshal(row.Prepare, prep); err != nil {
			slog.Error("test", "test", id, "err", err)
			return
		}
		switch t.State {
		case portalv1.TestState_TEST_STATE_STARTING:
			if !d.testSetupStep(ctx, row, t, prep) {
				return
			}
		case portalv1.TestState_TEST_STATE_RUNNING:
			if time.Now().Before(t.Deadline.AsTime()) {
				return
			}
			if _, err := d.updateTest(ctx, id, func(_ *store.TestRun, t *portalv1.TestRun) error {
				if t.State == portalv1.TestState_TEST_STATE_RUNNING {
					t.State, t.EndedBy = portalv1.TestState_TEST_STATE_ENDING, endedByTimeLimit
				}
				return nil
			}); err != nil {
				return
			}
			d.audit(ctx, "system", "plan.test_end", "plan:"+t.PlanId, fmt.Sprintf("test %s reached its time limit", id))
		case portalv1.TestState_TEST_STATE_ENDING:
			d.testCleanup(ctx, t, prep)
			return
		default:
			return
		}
	}
}

// setStep records a step's status.
func setStep(t *portalv1.TestRun, i int, status, detail string) {
	for len(t.Steps) <= i {
		t.Steps = append(t.Steps, &portalv1.TakeoverStep{Name: testStepNames[len(t.Steps)], Status: "pending"})
	}
	t.Steps[i].Status, t.Steps[i].Detail, t.Steps[i].UpdatedAt = status, detail, timestamppb.Now()
}

// testSetupStep runs the next setup step. It reports whether to continue.
func (d *Deps) testSetupStep(ctx context.Context, row store.TestRun, t *portalv1.TestRun, prep *clientv1.TestPrepare) bool {
	id := t.Id
	update := func(fn func(row *store.TestRun, t *portalv1.TestRun)) bool {
		_, err := d.updateTest(ctx, id, func(r *store.TestRun, t *portalv1.TestRun) error {
			if t.State != portalv1.TestState_TEST_STATE_STARTING {
				return errEnded
			}
			fn(r, t)
			return nil
		})
		if err != nil && !errors.Is(err, errEnded) {
			slog.Error("save test", "test", id, "err", err)
		}
		return err == nil
	}
	step := row.NextStep
	if !update(func(_ *store.TestRun, t *portalv1.TestRun) { setStep(t, step, "running", "") }) {
		return true // ended meanwhile: the loop moves on to cleanup
	}

	switch step {
	case testStepPrepare:
		ack, err := d.request(ctx, prepDRHost(ctx, d, t.PlanId), 15*time.Minute,
			&clientv1.Action{Kind: &clientv1.Action_TestPrepare{TestPrepare: prep}})
		if ctx.Err() != nil {
			return false
		}
		if err != nil {
			_, _ = d.updateTest(ctx, id, func(_ *store.TestRun, t *portalv1.TestRun) error {
				setStep(t, step, "failed", err.Error())
				t.Error, t.State, t.EndedBy = "setting up: "+err.Error(), portalv1.TestState_TEST_STATE_ENDING, endedBySetupFailure
				return nil
			})
			return true
		}
		return update(func(r *store.TestRun, t *portalv1.TestRun) {
			t.Notes = ack.GetOutput()
			setStep(t, step, "done", fmt.Sprintf("%d guest(s) registered from %s", len(prep.Guests), t.Snapshot))
			r.NextStep = testStepStart
		})

	case testStepStart:
		dr := prepDRHost(ctx, d, t.PlanId)
		delays := d.startupDelays(ctx, t.PlanId)
		for i, g := range prep.Guests {
			if t.Guests[i].Status != "pending" {
				continue
			}
			err := func() error {
				_, err := d.request(ctx, dr, 5*time.Minute, &clientv1.Action{Kind: &clientv1.Action_TestStartGuest{
					TestStartGuest: &clientv1.TestStartGuest{TestId: id, TestVmid: g.TestVmid, Type: g.Type}}})
				return err
			}()
			if ctx.Err() != nil {
				return false
			}
			if !update(func(_ *store.TestRun, t *portalv1.TestRun) {
				if err != nil {
					t.Guests[i].Status, t.Guests[i].Detail = "failed", "start: "+err.Error()
				} else {
					t.Guests[i].Status = "starting"
				}
			}) {
				return true
			}
			if err == nil && i < len(prep.Guests)-1 && delays[g.Vmid] > 0 {
				if sleep(ctx, delays[g.Vmid]) != nil {
					return false
				}
			}
		}
		return update(func(r *store.TestRun, t *portalv1.TestRun) {
			setStep(t, step, "done", "")
			r.NextStep = testStepCheck
		})

	case testStepCheck:
		dr := prepDRHost(ctx, d, t.PlanId)
		deadline := time.Now().Add(guestCheckTimeout)
		for {
			pending := 0
			for i, g := range prep.Guests {
				if t.Guests[i].Status != "starting" {
					continue
				}
				ack, err := d.request(ctx, dr, time.Minute, &clientv1.Action{Kind: &clientv1.Action_TestCheckGuest{
					TestCheckGuest: &clientv1.TestCheckGuest{TestId: id, TestVmid: g.TestVmid, Type: g.Type}}})
				c := ack.GetGuestCheck()
				ok := err == nil && c.GetRunning() && (!c.GetAgentEnabled() || c.GetAgentOk())
				timedOut := time.Now().After(deadline)
				if !ok && !timedOut {
					pending++
					continue
				}
				if !update(func(_ *store.TestRun, t *portalv1.TestRun) {
					tg := t.Guests[i]
					tg.AgentEnabled, tg.AgentOk = c.GetAgentEnabled(), c.GetAgentOk()
					switch {
					case ok:
						tg.Status, tg.Detail = "running", ""
					case err != nil:
						tg.Status, tg.Detail = "failed", "check: "+err.Error()
					case !c.GetRunning():
						tg.Status, tg.Detail = "failed", "not running"
					default:
						tg.Status, tg.Detail = "failed", "the QEMU guest agent didn't answer"
					}
				}) {
					return true
				}
				t.Guests[i].Status = "checked"
			}
			if pending == 0 {
				break
			}
			if sleep(ctx, testPoll) != nil {
				return false
			}
		}
		return update(func(r *store.TestRun, t *portalv1.TestRun) {
			running := 0
			for _, g := range t.Guests {
				if g.Status == "running" {
					running++
				}
			}
			setStep(t, step, "done", fmt.Sprintf("%d of %d guest(s) running", running, len(t.Guests)))
			t.State = portalv1.TestState_TEST_STATE_RUNNING
			r.NextStep = testStepCleanup
		})
	}
	return false
}

var errEnded = errors.New("the test isn't starting any more")

// testCleanup removes the test's guests and clones. On failure the test
// stays ending, and the supervisor tries again.
func (d *Deps) testCleanup(ctx context.Context, t *portalv1.TestRun, prep *clientv1.TestPrepare) {
	id := t.Id
	_, _ = d.updateTest(ctx, id, func(_ *store.TestRun, t *portalv1.TestRun) error {
		setStep(t, testStepCleanup, "running", "")
		return nil
	})
	ack, err := d.request(ctx, prepDRHost(ctx, d, t.PlanId), 10*time.Minute, &clientv1.Action{Kind: &clientv1.Action_TestCleanup{
		TestCleanup: &clientv1.TestCleanup{TestId: id, Guests: prep.Guests}}})
	if ctx.Err() != nil {
		return
	}
	_, _ = d.updateTest(ctx, id, func(_ *store.TestRun, t *portalv1.TestRun) error {
		if err != nil {
			setStep(t, testStepCleanup, "failed", err.Error()+" (retrying)")
			return nil
		}
		setStep(t, testStepCleanup, "done", fmt.Sprintf("%d guest(s) and clone(s) removed", len(ack.GetOutput())))
		t.EndedAt = timestamppb.Now()
		t.State = portalv1.TestState_TEST_STATE_ENDED
		if t.EndedBy == endedBySetupFailure {
			t.State = portalv1.TestState_TEST_STATE_FAILED
		}
		for _, g := range t.Guests {
			if g.Status == "starting" || g.Status == "pending" {
				g.Status = "stopped"
			}
		}
		return nil
	})
	if err != nil {
		slog.Warn("test cleanup failed; will retry", "test", id, "err", err)
		return
	}
	d.audit(ctx, "system", "plan.test_ended", "plan:"+t.PlanId, fmt.Sprintf("test %s cleaned up", id))
}

// prepDRHost returns the plan's DR host.
func prepDRHost(ctx context.Context, d *Deps, planID string) string {
	sp, err := d.Store.PlanByID(ctx, planID)
	if err != nil {
		return ""
	}
	return sp.DRHostID
}

// startupDelays returns each guest's startup delay from the plan.
func (d *Deps) startupDelays(ctx context.Context, planID string) map[uint32]time.Duration {
	out := map[uint32]time.Duration{}
	sp, err := d.Store.PlanByID(ctx, planID)
	if err != nil {
		return out
	}
	spec, err := decodeSpec(sp.Spec)
	if err != nil {
		return out
	}
	for _, g := range spec.Guests {
		out[g.Vmid] = time.Duration(g.StartupDelaySeconds) * time.Second
	}
	return out
}
