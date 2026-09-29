package api

import (
	"context"
	"fmt"
	"strconv"
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
	// dropOnce names an action (as recorded in actions) that makes the host
	// disconnect without acknowledging it, the first time only.
	dropOnce string
	// failRestore fails this many restore actions; failRelease fails
	// releases.
	failRestore int
	failRelease bool
	// failPrepare fails test preparation.
	failPrepare bool
	// finalSnapshot is reported as replicated once replication is
	// triggered; failReplicate fails that instead.
	finalSnapshot string
	failReplicate bool
	// sendBytes are the sizes failback sends report, one per send (then
	// 1 KiB); failSend fails that send (counting from 1) and failCleanup
	// that many cleanups. written makes preflights report changes since
	// the newest snapshot (divergence, on a primary).
	sendBytes   []uint64
	sends       int
	failSend    int
	failCleanup int
	written     uint64
	// failDataCleanup fails that many data cleanups; scanProblem is
	// reported for every dataset a data scan looks at.
	failDataCleanup int
	scanProblem     string
	// peer is the other host: a DR host's replication brings the peer's
	// latest snapshot.
	peer *fakeHost

	mu              sync.Mutex
	pendingSnapshot string
	actions         []string
	pulls           []string
	stopped         chan struct{}
}

// run serves the host until ctx is done; wait returns once it has stopped.
// It returns once the host is connected, so tests don't race its first
// connection.
func (f *fakeHost) run(ctx context.Context, t *testing.T) {
	f.stopped = make(chan struct{})
	go func() {
		defer close(f.stopped)
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		for ctx.Err() == nil {
			_, outbox, release := f.d.Hub.connect(ctx, f.id)
			f.serve(ctx, t, outbox, tick.C)
			release()
		}
	}()
	for deadline := time.Now().Add(5 * time.Second); !f.d.Hub.Online(f.id); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("fake host %s didn't connect", f.id)
		}
	}
}

// serve handles one connection until ctx is done or the host drops it.
func (f *fakeHost) serve(ctx context.Context, t *testing.T, outbox <-chan *clientv1.SubscribeResponse, tick <-chan time.Time) {
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
				if err := f.d.Store.SetHostApplied(ctx, f.id, ds.Generation, ""); err != nil && ctx.Err() == nil {
					t.Error(err)
				}
			}
			if a := msg.GetAction(); a != nil && !f.answer(a) {
				return // dropped: reconnect
			}
		case <-tick:
			f.report(ctx, t)
		}
	}
}

// answer acknowledges an action; it returns false if the host drops the
// connection instead.
func (f *fakeHost) answer(a *clientv1.Action) bool {
	ack := &clientv1.AckActionRequest{ActionId: a.Id, Succeeded: true}
	var name string
	switch k := a.Kind.(type) {
	case *clientv1.Action_ZreplPreflight:
		name = "preflight"
		f.mu.Lock()
		written := f.written
		f.mu.Unlock()
		ack.Preflight = &clientv1.ZreplPreflightResult{ZreplVersion: "v0.7.0", ZreplRunning: true}
		for _, ds := range k.ZreplPreflight.Datasets {
			ack.Preflight.Datasets = append(ack.Preflight.Datasets, &clientv1.DatasetSnapshots{Dataset: ds, Exists: true,
				WrittenBytes: written, Snapshots: []*clientv1.SnapshotInfo{{Name: "@zrepl_1", Guid: 7, Createtxg: 1}}})
		}
	case *clientv1.Action_ZreplRemoveJobs:
		name = "remove " + strings.Join(k.ZreplRemoveJobs.Jobs, ",")
	case *clientv1.Action_ZreplRestoreConfig:
		name = "restore"
		f.mu.Lock()
		if f.failRestore > 0 {
			f.failRestore--
			ack.Succeeded, ack.Message = false, "restore failed"
		}
		f.mu.Unlock()
	case *clientv1.Action_ZreplReleaseJobs:
		name = "release " + strings.Join(k.ZreplReleaseJobs.Jobs, ",")
		ack.Output = []string{"destroy hold"}
		if f.failRelease {
			ack.Succeeded, ack.Message = false, "release failed"
		}
	case *clientv1.Action_TestOptions:
		name = "test options"
		ack.TestOptions = &clientv1.TestOptionsResult{MemoryAvailableBytes: 1 << 30}
		f.mu.Lock()
		final := f.finalSnapshot
		f.mu.Unlock()
		for _, r := range k.TestOptions.Replicas {
			snaps := []*clientv1.TestSnapshot{{Name: "zrepl_2", CreatedAt: timestamppb.Now()}, {Name: "zrepl_1", CreatedAt: timestamppb.New(time.Unix(1, 0))}}
			if final != "" {
				snaps = append([]*clientv1.TestSnapshot{{Name: final, CreatedAt: timestamppb.Now()}}, snaps...)
			}
			ack.TestOptions.Replicas = append(ack.TestOptions.Replicas, &clientv1.ReplicaSnapshots{Replica: r, Exists: true, Snapshots: snaps})
		}
	case *clientv1.Action_FailoverStopGuests:
		name = "stop " + fmt.Sprint(k.FailoverStopGuests.Vmids)
	case *clientv1.Action_FailoverSnapshot:
		name = "snapshot"
		f.mu.Lock()
		f.pendingSnapshot = k.FailoverSnapshot.Snapshot
		f.mu.Unlock()
	case *clientv1.Action_FailoverReplicate:
		name = "replicate"
		if f.failReplicate {
			ack.Succeeded, ack.Message = false, "zrepl isn't running"
		} else if f.peer != nil {
			f.peer.mu.Lock()
			snap := f.peer.pendingSnapshot
			f.peer.mu.Unlock()
			f.mu.Lock()
			f.finalSnapshot = snap
			f.mu.Unlock()
		}
	case *clientv1.Action_FailoverPrepare:
		name = "prepare " + fmt.Sprint(k.FailoverPrepare.Vmids)
	case *clientv1.Action_FailoverStartGuest:
		name = "start " + strconv.FormatUint(uint64(k.FailoverStartGuest.Vmid), 10)
	case *clientv1.Action_FailoverCheckGuest:
		name = "check"
		ack.GuestCheck = &clientv1.TestGuestCheck{Running: true}
	case *clientv1.Action_FailoverUnlockGuests:
		name = "unlock " + fmt.Sprint(k.FailoverUnlockGuests.Vmids)
	case *clientv1.Action_FailbackReceive:
		var parts []string
		for _, t := range k.FailbackReceive.Datasets {
			parts = append(parts, fmt.Sprintf("%s@%s rollback=%v", t.Dataset, t.FromSnapshot, t.Rollback))
		}
		name = "receive " + strings.Join(parts, ",")
	case *clientv1.Action_FailbackSend:
		name = "send"
		f.mu.Lock()
		f.sends++
		n := uint64(1024)
		if len(f.sendBytes) > 0 {
			n, f.sendBytes = f.sendBytes[0], f.sendBytes[1:]
		}
		if f.sends == f.failSend {
			ack.Succeeded, ack.Message = false, "zfs send failed"
		}
		f.mu.Unlock()
		ack.Transfer = &clientv1.FailbackTransfer{}
		for _, s := range k.FailbackSend.Datasets {
			ack.Transfer.Datasets = append(ack.Transfer.Datasets, &clientv1.DatasetTransfer{Dataset: s.Dataset, Bytes: n})
		}
	case *clientv1.Action_FailbackCleanup:
		name = "cleanup " + k.FailbackCleanup.Snapshot
		f.mu.Lock()
		if f.failCleanup > 0 {
			f.failCleanup--
			ack.Succeeded, ack.Message = false, "pvesm failed"
		}
		f.mu.Unlock()
	case *clientv1.Action_FailbackCheckGuest:
		name = "check primary"
		ack.GuestCheck = &clientv1.TestGuestCheck{Running: true}
	case *clientv1.Action_TestPrepare:
		name = "test prepare " + k.TestPrepare.Snapshot
		if f.failPrepare {
			ack.Succeeded, ack.Message = false, "clone failed"
		}
		ack.Output = []string{"guest 101: removed dev0"}
	case *clientv1.Action_TestStartGuest:
		name = "test start " + strconv.FormatUint(uint64(k.TestStartGuest.TestVmid), 10)
	case *clientv1.Action_TestCheckGuest:
		name = "test check"
		ack.GuestCheck = &clientv1.TestGuestCheck{Running: true}
	case *clientv1.Action_TestCleanup:
		name = "test cleanup"
		ack.Output = []string{"destroyed test guest", "destroyed clone"}
	case *clientv1.Action_DataScan:
		name = "scan"
		ack.DataScan = &clientv1.DataScanResult{}
		f.mu.Lock()
		for _, t := range k.DataScan.Targets {
			sc := &clientv1.DatasetScan{Dataset: t.Dataset, Exists: true, ReclaimBytes: 1 << 20, Snapshots: 3}
			if f.scanProblem != "" {
				sc.Problems = []string{f.scanProblem}
			}
			ack.DataScan.Datasets = append(ack.DataScan.Datasets, sc)
		}
		f.mu.Unlock()
	case *clientv1.Action_DataCleanup:
		var ts []string
		for _, t := range k.DataCleanup.Targets {
			if t.Destroy {
				ts = append(ts, t.Dataset+":destroy")
			} else {
				ts = append(ts, t.Dataset+":"+t.Prefix)
			}
		}
		name = "cleanup " + strings.Join(ts, ",") + " release " + strings.Join(k.DataCleanup.ReleaseJobs, ",")
		ack.Output = []string{"destroyed something"}
		f.mu.Lock()
		if f.failDataCleanup > 0 {
			f.failDataCleanup--
			ack.Succeeded, ack.Message = false, "zfs destroy failed"
		}
		f.mu.Unlock()
	default:
		name = "other"
	}
	f.mu.Lock()
	f.actions = append(f.actions, name)
	drop := f.dropOnce != "" && f.dropOnce == name
	if drop {
		f.dropOnce = ""
	}
	f.mu.Unlock()
	if drop {
		return false
	}
	f.d.Hub.deliver(f.id, ack)
	return true
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
	if err := f.d.Store.PutReplicationStatus(ctx, f.id, b); err != nil && ctx.Err() == nil {
		t.Error(err)
	}
}

func (f *fakeHost) wait() { <-f.stopped }

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
	p, h := &fakeHost{d: d, id: primary}, &fakeHost{d: d, id: dr}
	p.run(hctx, t)
	h.run(hctx, t)
	t.Cleanup(func() {
		cancel()
		p.wait()
		h.wait()
	})
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
		if _, busy := runningTakeovers.Load(id); !busy && got.Msg.Takeover.State != portalv1.TakeoverState_TAKEOVER_STATE_RUNNING {
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

func TestTakeoverResendsAfterDisconnect(t *testing.T) {
	d, ctx, id, primary, dr := takeoverFixture(t)
	// The DR host loses its connection while removing the old job, so its
	// acknowledgement never arrives; the portal sends the action again.
	dr.dropOnce = "remove old_pull"
	tk := runTakeoverTest(ctx, t, d, id)
	if tk.State != portalv1.TakeoverState_TAKEOVER_STATE_COMPLETED {
		t.Fatalf("takeover = %v", tk)
	}
	if got := strings.Join(dr.did(), "|"); got != "preflight|remove old_pull|remove old_pull|release old_pull" {
		t.Errorf("DR actions = %s", got)
	}
	_ = primary
}

func TestTakeoverRollbackRetry(t *testing.T) {
	d, ctx, id, primary, dr := takeoverFixture(t)
	dr.fullSend = true
	primary.failRestore = 1
	tk := runTakeoverTest(ctx, t, d, id)
	if tk.State != portalv1.TakeoverState_TAKEOVER_STATE_FAILED || !tk.RollingBack ||
		!strings.Contains(tk.Steps[len(tk.Steps)-1].Detail, "restore failed") {
		t.Fatalf("takeover = %v", tk)
	}
	svc := PlanService{Deps: d}
	// Nothing to retry for the preflight or a new start meanwhile.
	if _, err := svc.StartTakeover(ctx, connect.NewRequest(&portalv1.StartTakeoverRequest{Id: id})); err == nil {
		t.Error("started a new takeover while the last one's rollback failed")
	}
	if _, err := svc.RetryTakeoverRollback(ctx, connect.NewRequest(&portalv1.RetryTakeoverRollbackRequest{Id: id})); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		got, _ := svc.GetTakeover(ctx, connect.NewRequest(&portalv1.GetTakeoverRequest{Id: id}))
		if _, busy := runningTakeovers.Load(id); !busy {
			if tk = got.Msg.Takeover; tk.State != portalv1.TakeoverState_TAKEOVER_STATE_RUNNING {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if tk.State != portalv1.TakeoverState_TAKEOVER_STATE_ROLLED_BACK || !strings.Contains(tk.Error, "sent in full") {
		t.Fatalf("after retry: %v", tk)
	}
	if got := strings.Join(primary.did(), "|"); got != "preflight|remove old_source|restore|restore" {
		t.Errorf("primary actions = %s", got)
	}
	// Only a failed rollback can be retried.
	if _, err := svc.RetryTakeoverRollback(ctx, connect.NewRequest(&portalv1.RetryTakeoverRollbackRequest{Id: id})); err == nil {
		t.Error("retried a finished rollback")
	}
}

func TestTakeoverReleaseFailure(t *testing.T) {
	d, ctx, id, primary, _ := takeoverFixture(t)
	primary.failRelease = true
	tk := runTakeoverTest(ctx, t, d, id)
	// Replication already runs under EZDR, so the takeover completes with a
	// note instead of rolling back.
	if tk.State != portalv1.TakeoverState_TAKEOVER_STATE_COMPLETED || !strings.Contains(tk.Error, "release-all --job") {
		t.Fatalf("takeover = %v", tk)
	}
	sp, _, _ := PlanService{Deps: d}.loadPlan(ctx, id)
	if sp.State != store.PlanActive {
		t.Errorf("plan state = %s", sp.State)
	}
}
