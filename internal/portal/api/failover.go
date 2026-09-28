package api

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	clientv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/client/v1"
	planv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/plan/v1"
	portalv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/portal/v1"
	"github.com/jlbyh2o/ezdr/internal/plan"
	"github.com/jlbyh2o/ezdr/internal/portal/store"
	"github.com/jlbyh2o/ezdr/internal/replication"
)

// Failover: see docs/design/failover.md.

// FailoverService fails plans over.
type FailoverService struct{ *Deps }

// Steps of a failover, in order. Steps before foStepCommit can be undone;
// from foStepCommit on the plan is failed over.
const (
	foStepEndTest = iota
	foStepStopPrimary
	foStepFinalSync
	foStepCommit
	foStepPrepare
	foStepStart
	foStepCheck
	foStepConfirm
	foStepCount
)

var foStepNames = [foStepCount]string{
	"End a running test failover",
	"Shut down and lock the guests on the primary",
	"Take a final snapshot and replicate it",
	"Stop replication",
	"Prepare the replicas and register the guests",
	"Start the guests in order",
	"Check the guests",
	"Verify the guests and switch DNS",
}

// finalSyncTimeout bounds the final replication of a planned failover.
const finalSyncTimeout = 30 * time.Minute

// GetFailoverOptions returns what a failover of the plan would involve.
func (s FailoverService) GetFailoverOptions(ctx context.Context, req *connect.Request[portalv1.GetFailoverOptionsRequest]) (*connect.Response[portalv1.GetFailoverOptionsResponse], error) {
	tp, err := s.loadTestPlan(ctx, req.Msg.PlanId)
	if err != nil {
		return nil, err
	}
	resp := &portalv1.GetFailoverOptionsResponse{
		PrimaryOnline: s.Hub.Online(tp.spec.PrimaryHostId), DrOnline: s.Hub.Online(tp.drID),
		DnsRecords: dnsRecords(tp.spec),
	}
	resp.PlannedPossible = resp.PrimaryOnline && tp.row.State == store.PlanActive
	if !resp.DrOnline {
		resp.Problem = "the DR host isn't connected"
		return connect.NewResponse(resp), nil
	}
	_, opts, err := s.testOptions(ctx, tp, nil)
	if err != nil {
		return nil, err
	}
	resp.Guests = opts.Guests
	for _, g := range opts.Guests {
		if g.Problem != "" {
			resp.Problem = fmt.Sprintf("guest %d: %s", g.Vmid, g.Problem)
		}
	}
	if len(opts.PointsInTime) > 0 {
		resp.NewestSnapshotAt = opts.PointsInTime[0].CreatedAt
	}
	return connect.NewResponse(resp), nil
}

func dnsRecords(spec *planv1.PlanSpec) []*portalv1.DnsRecordSwitch {
	var out []*portalv1.DnsRecordSwitch
	for _, g := range spec.Guests {
		for _, r := range g.DnsRecords {
			out = append(out, &portalv1.DnsRecordSwitch{Vmid: g.Vmid, Name: r.Name, Type: strings.TrimPrefix(r.Type.String(), "DNS_RECORD_TYPE_"),
				ProductionValue: r.ProductionValue, FailoverValue: r.FailoverValue, Status: "pending"})
		}
	}
	return out
}

// StartFailover starts a failover of the plan.
func (s FailoverService) StartFailover(ctx context.Context, req *connect.Request[portalv1.StartFailoverRequest]) (*connect.Response[portalv1.StartFailoverResponse], error) {
	tp, err := s.loadTestPlan(ctx, req.Msg.PlanId)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.Msg.ConfirmName) != tp.row.Name {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("type the plan's name to confirm the failover"))
	}
	if !s.Hub.Online(tp.drID) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("the DR host isn't connected"))
	}
	if req.Msg.Planned && (tp.row.State != store.PlanActive || !s.Hub.Online(tp.spec.PrimaryHostId)) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("a planned failover needs an active plan and a connected primary"))
	}
	if running, err := s.takeoverRunning(ctx, tp.row.ID); err != nil || running {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("the plan's takeover is running"))
	}

	id := strings.ToLower(store.NewID())
	f := &portalv1.Failover{Id: id, PlanId: tp.row.ID, Planned: req.Msg.Planned, State: portalv1.FailoverState_FAILOVER_STATE_RUNNING,
		StartedBy: currentUser(ctx).Username, StartedAt: timestamppb.Now(), DnsRecords: dnsRecords(tp.spec)}
	for i, name := range foStepNames {
		st := &portalv1.TakeoverStep{Name: name, Status: "pending", UpdatedAt: timestamppb.Now()}
		if !req.Msg.Planned && (i == foStepStopPrimary || i == foStepFinalSync) {
			st.Status, st.Detail = "skipped", "unplanned failover"
		}
		f.Steps = append(f.Steps, st)
	}
	for _, g := range tp.guests() {
		f.Guests = append(f.Guests, &portalv1.TestRunGuest{Vmid: g.option.Vmid, TestVmid: g.option.Vmid, Name: g.option.Name,
			Type: g.option.Type, Status: "pending"})
	}
	data, _ := proto.Marshal(f)
	err = s.Store.CreateFailover(ctx, store.FailoverRow{ID: id, PlanID: tp.row.ID, Active: true, Data: data, StartedAt: time.Now()})
	if errors.Is(err, store.ErrFailoverActive) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	if err != nil {
		return nil, internalError(err)
	}
	kind := "unplanned"
	if req.Msg.Planned {
		kind = "planned"
	}
	s.audit(ctx, currentUser(ctx).Username, "plan.failover_start", "plan:"+tp.row.ID, fmt.Sprintf("%s failover of plan %q", kind, tp.row.Name))
	go s.runFailover(context.WithoutCancel(ctx), id, tp.row.ID)
	return connect.NewResponse(&portalv1.StartFailoverResponse{Failover: f}), nil
}

// GetFailover returns the plan's latest failover.
func (s FailoverService) GetFailover(ctx context.Context, req *connect.Request[portalv1.GetFailoverRequest]) (*connect.Response[portalv1.GetFailoverResponse], error) {
	_, f, err := s.loadFailover(ctx, req.Msg.PlanId)
	if errors.Is(err, store.ErrNotFound) {
		return connect.NewResponse(&portalv1.GetFailoverResponse{}), nil
	}
	if err != nil {
		return nil, internalError(err)
	}
	return connect.NewResponse(&portalv1.GetFailoverResponse{Failover: f}), nil
}

// ConfirmFailover records the operator's verification and finishes the
// failover.
func (s FailoverService) ConfirmFailover(ctx context.Context, req *connect.Request[portalv1.ConfirmFailoverRequest]) (*connect.Response[portalv1.ConfirmFailoverResponse], error) {
	user := currentUser(ctx).Username
	f, err := s.updateFailover(ctx, req.Msg.PlanId, func(_ *store.FailoverRow, f *portalv1.Failover) error {
		if f.State != portalv1.FailoverState_FAILOVER_STATE_AWAITING_CONFIRMATION {
			return connect.NewError(connect.CodeFailedPrecondition, errors.New("the failover isn't waiting for confirmation"))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	detail := "verified by " + user + "; DNS not switched"
	if req.Msg.SwitchDns {
		detail = "verified by " + user + "; " + s.switchDNS(ctx, f.PlanId)
	} else {
		_, _ = s.updateFailover(ctx, req.Msg.PlanId, func(_ *store.FailoverRow, f *portalv1.Failover) error {
			for _, r := range f.DnsRecords {
				if r.Status == "pending" {
					r.Status = "skipped"
				}
			}
			return nil
		})
	}
	f, err = s.updateFailover(ctx, req.Msg.PlanId, func(row *store.FailoverRow, f *portalv1.Failover) error {
		setFoStep(f, foStepConfirm, "done", detail)
		f.State, f.CompletedAt = portalv1.FailoverState_FAILOVER_STATE_COMPLETED, timestamppb.Now()
		row.NextStep = foStepCount
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.audit(ctx, user, "plan.failover_confirm", "plan:"+f.PlanId, detail)
	return connect.NewResponse(&portalv1.ConfirmFailoverResponse{Failover: f}), nil
}

// switchDNS switches the plan's records to their failover values.
func (d *Deps) switchDNS(ctx context.Context, planID string) string {
	return d.switchRecords(ctx, planID, true)
}

// RetryFailover resumes a failover whose step failed after replication
// stopped.
func (s FailoverService) RetryFailover(ctx context.Context, req *connect.Request[portalv1.RetryFailoverRequest]) (*connect.Response[portalv1.RetryFailoverResponse], error) {
	f, err := s.updateFailover(ctx, req.Msg.PlanId, func(row *store.FailoverRow, f *portalv1.Failover) error {
		if f.State != portalv1.FailoverState_FAILOVER_STATE_FAILED {
			return connect.NewError(connect.CodeFailedPrecondition, errors.New("only a failed failover can be retried"))
		}
		f.State, f.Error = portalv1.FailoverState_FAILOVER_STATE_RUNNING, ""
		row.Active = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.audit(ctx, currentUser(ctx).Username, "plan.failover_retry", "plan:"+f.PlanId, "")
	go s.runFailover(context.WithoutCancel(ctx), f.Id, f.PlanId)
	return connect.NewResponse(&portalv1.RetryFailoverResponse{Failover: f}), nil
}

func (d *Deps) loadFailover(ctx context.Context, planID string) (store.FailoverRow, *portalv1.Failover, error) {
	row, err := d.Store.LatestFailover(ctx, planID)
	if err != nil {
		return row, nil, err
	}
	f := &portalv1.Failover{}
	return row, f, proto.Unmarshal(row.Data, f)
}

var failoverLocks sync.Map // plan ID -> *sync.Mutex

// updateFailover loads the plan's latest failover, applies fn, and saves it.
func (d *Deps) updateFailover(ctx context.Context, planID string, fn func(row *store.FailoverRow, f *portalv1.Failover) error) (*portalv1.Failover, error) {
	mu, _ := failoverLocks.LoadOrStore(planID, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()
	row, f, err := d.loadFailover(ctx, planID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("the plan has no failover"))
	}
	if err != nil {
		return nil, internalError(err)
	}
	if err := fn(&row, f); err != nil {
		return nil, err
	}
	if row.Data, err = proto.Marshal(f); err != nil {
		return nil, internalError(err)
	}
	row.Active = f.State == portalv1.FailoverState_FAILOVER_STATE_RUNNING
	if err := d.Store.UpdateFailover(ctx, row); err != nil {
		return nil, internalError(err)
	}
	return f, nil
}

func setFoStep(f *portalv1.Failover, i int, status, detail string) {
	f.Steps[i].Status, f.Steps[i].Detail, f.Steps[i].UpdatedAt = status, detail, timestamppb.Now()
}

// failoverRunning reports whether a plan's failover is running, so the plan
// can't be changed meanwhile.
func (d *Deps) failoverRunning(ctx context.Context, planID string) (bool, error) {
	_, f, err := d.loadFailover(ctx, planID)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	return f.GetState() == portalv1.FailoverState_FAILOVER_STATE_RUNNING, err
}

// ResumeFailovers restarts failovers that were running when the portal
// stopped.
func (d *Deps) ResumeFailovers(ctx context.Context) {
	rows, err := d.Store.ActiveFailovers(ctx)
	if err != nil {
		slog.Error("list failovers", "err", err)
		return
	}
	for _, row := range rows {
		slog.Info("resuming failover", "plan", row.PlanID, "step", row.NextStep)
		go d.runFailover(ctx, row.ID, row.PlanID)
	}
}

var runningFailovers sync.Map // plan ID -> struct{}

// runFailover runs a failover's remaining steps.
func (d *Deps) runFailover(ctx context.Context, id, planID string) {
	if _, busy := runningFailovers.LoadOrStore(planID, struct{}{}); busy {
		return
	}
	defer runningFailovers.Delete(planID)
	for ctx.Err() == nil {
		row, f, err := d.loadFailover(ctx, planID)
		if err != nil || f.Id != id || f.State != portalv1.FailoverState_FAILOVER_STATE_RUNNING {
			return
		}
		step := row.NextStep
		if step >= foStepConfirm {
			_, _ = d.updateFailover(ctx, planID, func(_ *store.FailoverRow, f *portalv1.Failover) error {
				setFoStep(f, foStepConfirm, "running", "waiting for the operator to verify the guests")
				f.State = portalv1.FailoverState_FAILOVER_STATE_AWAITING_CONFIRMATION
				return nil
			})
			return
		}
		if f.Steps[step].Status == "skipped" {
			_, _ = d.updateFailover(ctx, planID, func(row *store.FailoverRow, _ *portalv1.Failover) error {
				row.NextStep++
				return nil
			})
			continue
		}
		_, _ = d.updateFailover(ctx, planID, func(_ *store.FailoverRow, f *portalv1.Failover) error {
			setFoStep(f, step, "running", "")
			return nil
		})
		slog.Info("failover step", "plan", planID, "step", foStepNames[step])
		detail, err := d.failoverStep(ctx, planID, step, f)
		if ctx.Err() != nil {
			return // the portal is stopping; the step reruns when it starts again
		}
		if err != nil {
			slog.Error("failover step failed", "plan", planID, "step", foStepNames[step], "err", err)
			if step < foStepCommit {
				d.abortFailover(ctx, planID, step, err)
				return
			}
			_, _ = d.updateFailover(ctx, planID, func(_ *store.FailoverRow, f *portalv1.Failover) error {
				setFoStep(f, step, "failed", strings.TrimSpace(detail+" "+err.Error()))
				f.State, f.Error = portalv1.FailoverState_FAILOVER_STATE_FAILED, foStepNames[step]+": "+err.Error()
				return nil
			})
			d.audit(ctx, "system", "plan.failover_failed", "plan:"+planID, foStepNames[step]+": "+err.Error())
			return
		}
		_, _ = d.updateFailover(ctx, planID, func(row *store.FailoverRow, f *portalv1.Failover) error {
			setFoStep(f, step, "done", detail)
			row.NextStep = step + 1
			return nil
		})
	}
}

// failoverStep runs one step and returns a description of what it did.
func (d *Deps) failoverStep(ctx context.Context, planID string, step int, f *portalv1.Failover) (string, error) {
	sp, err := d.Store.PlanByID(ctx, planID)
	if err != nil {
		return "", err
	}
	spec, err := decodeSpec(sp.AppliedSpec)
	if err != nil {
		return "", err
	}
	ordered := startupOrder(spec)
	vmids := make([]uint32, 0, len(ordered))
	for _, g := range ordered {
		vmids = append(vmids, g.Vmid)
	}

	switch step {
	case foStepEndTest:
		return d.endTestForFailover(ctx, planID)

	case foStepStopPrimary:
		reverse := slices.Clone(vmids)
		slices.Reverse(reverse)
		timeout := plan.ShutdownTimeoutSeconds(spec)
		ack, err := d.request(ctx, spec.PrimaryHostId, time.Duration(len(vmids)+1)*time.Duration(timeout+60)*time.Second,
			&clientv1.Action{Kind: &clientv1.Action_FailoverStopGuests{FailoverStopGuests: &clientv1.FailoverStopGuests{
				PlanId: planID, Vmids: reverse, ShutdownTimeoutSeconds: timeout}}})
		return strings.Join(ack.GetOutput(), "; "), err

	case foStepFinalSync:
		return d.finalSync(ctx, planID, spec, f)

	case foStepCommit:
		if sp.State != store.PlanFailedOver {
			if err := d.Store.SetPlanState(ctx, planID, store.PlanFailedOver, sp.AppliedSpec); err != nil {
				return "", err
			}
			d.audit(ctx, "system", "plan.failed_over", "plan:"+planID, fmt.Sprintf("plan %q is failed over", sp.Name))
		}
		d.reconcile(ctx, spec.PrimaryHostId, spec.DrHostId)
		if err := d.waitApplied(ctx, spec.DrHostId); err != nil {
			return "", fmt.Errorf("DR host: %w", err)
		}
		return "the plan's zrepl jobs were removed from the DR host", nil

	case foStepPrepare:
		ack, err := d.request(ctx, spec.DrHostId, 15*time.Minute, &clientv1.Action{Kind: &clientv1.Action_FailoverPrepare{
			FailoverPrepare: &clientv1.FailoverPrepare{PlanId: planID, Vmids: vmids}}})
		if err == nil && len(ack.GetOutput()) > 0 {
			_, _ = d.updateFailover(ctx, planID, func(_ *store.FailoverRow, f *portalv1.Failover) error {
				f.Notes = ack.GetOutput()
				return nil
			})
		}
		return fmt.Sprintf("%d guest(s) registered", len(vmids)), err

	case foStepStart:
		for i, g := range ordered {
			if f.Guests[guestIndex(f, g.Vmid)].Status != "pending" {
				continue
			}
			_, err := d.request(ctx, spec.DrHostId, 5*time.Minute, &clientv1.Action{Kind: &clientv1.Action_FailoverStartGuest{
				FailoverStartGuest: &clientv1.FailoverStartGuest{PlanId: planID, Vmid: g.Vmid}}})
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			_, _ = d.updateFailover(ctx, planID, func(_ *store.FailoverRow, f *portalv1.Failover) error {
				fg := f.Guests[guestIndex(f, g.Vmid)]
				if err != nil {
					fg.Status, fg.Detail = "failed", "start: "+err.Error()
				} else {
					fg.Status = "starting"
				}
				return nil
			})
			if err == nil && i < len(ordered)-1 && g.StartupDelaySeconds > 0 {
				if err := sleep(ctx, time.Duration(g.StartupDelaySeconds)*time.Second); err != nil {
					return "", err
				}
			}
		}
		return "", nil

	case foStepCheck:
		return d.checkFailoverGuests(ctx, planID, spec)
	}
	return "", fmt.Errorf("unknown step %d", step)
}

func guestIndex(f *portalv1.Failover, vmid uint32) int {
	return slices.IndexFunc(f.Guests, func(g *portalv1.TestRunGuest) bool { return g.Vmid == vmid })
}

// startupOrder returns the plan's guests in startup order.
func startupOrder(spec *planv1.PlanSpec) []*planv1.PlanGuest {
	gs := slices.Clone(spec.Guests)
	slices.SortStableFunc(gs, func(a, b *planv1.PlanGuest) int {
		return cmp.Or(cmp.Compare(a.StartupOrder, b.StartupOrder), cmp.Compare(a.Vmid, b.Vmid))
	})
	return gs
}

// endTestForFailover ends the plan's running test, if any, and waits until
// it's cleaned up: its clones depend on the replicas' snapshots.
func (d *Deps) endTestForFailover(ctx context.Context, planID string) (string, error) {
	runs, err := d.Store.TestRunsByPlan(ctx, planID, 1)
	if err != nil || len(runs) == 0 || !runs[0].Active {
		return "no test was running", err
	}
	id := runs[0].ID
	if _, err := d.updateTest(ctx, id, func(_ *store.TestRun, t *portalv1.TestRun) error {
		if t.State == portalv1.TestState_TEST_STATE_STARTING || t.State == portalv1.TestState_TEST_STATE_RUNNING {
			t.State, t.EndedBy = portalv1.TestState_TEST_STATE_ENDING, "failover"
		}
		return nil
	}); err != nil {
		return "", err
	}
	go d.runTest(ctx, id)
	deadline := time.Now().Add(15 * time.Minute)
	for time.Now().Before(deadline) {
		row, err := d.Store.TestRunByID(ctx, id)
		if err != nil {
			return "", err
		}
		if !row.Active {
			return "ended test " + id, nil
		}
		if err := sleep(ctx, testPoll); err != nil {
			return "", err
		}
	}
	return "", errors.New("the running test failover didn't end within 15 minutes")
}

// finalSync takes a final snapshot on the primary, starts the DR host's pull
// jobs, and waits until every replica has it.
func (d *Deps) finalSync(ctx context.Context, planID string, spec *planv1.PlanSpec, f *portalv1.Failover) (string, error) {
	primary, _, err := d.loadInventoriesFor(ctx, spec)
	if err != nil {
		return "", err
	}
	snapshot := f.Snapshot
	if snapshot == "" {
		snapshot = spec.SnapshotPrefix + time.Now().UTC().Format("20060102_150405_000")
		if _, err := d.updateFailover(ctx, planID, func(_ *store.FailoverRow, f *portalv1.Failover) error {
			f.Snapshot = snapshot
			return nil
		}); err != nil {
			return "", err
		}
	}
	var datasets, replicas, jobs []string
	for _, g := range plan.JobGroups(spec, primary) {
		jobs = append(jobs, replication.PullJobName(replication.JobName(planID, g)))
		for _, ds := range g.Datasets {
			datasets = append(datasets, ds)
			replicas = append(replicas, g.ReceiveDataset+"/"+ds)
		}
	}
	if _, err := d.request(ctx, spec.PrimaryHostId, 5*time.Minute, &clientv1.Action{Kind: &clientv1.Action_FailoverSnapshot{
		FailoverSnapshot: &clientv1.FailoverSnapshot{Datasets: datasets, Snapshot: snapshot}}}); err != nil {
		return "", fmt.Errorf("primary: %w", err)
	}
	deadline := time.Now().Add(finalSyncTimeout)
	for time.Now().Before(deadline) {
		if _, err := d.request(ctx, spec.DrHostId, time.Minute, &clientv1.Action{Kind: &clientv1.Action_FailoverReplicate{
			FailoverReplicate: &clientv1.FailoverReplicate{PullJobs: jobs}}}); err != nil {
			return "", fmt.Errorf("DR host: %w", err)
		}
		if err := sleep(ctx, 3*testPoll); err != nil {
			return "", err
		}
		ack, err := d.request(ctx, spec.DrHostId, time.Minute, &clientv1.Action{Kind: &clientv1.Action_TestOptions{
			TestOptions: &clientv1.TestOptions{Replicas: replicas, SnapshotPrefix: spec.SnapshotPrefix}}})
		if err != nil {
			return "", fmt.Errorf("DR host: %w", err)
		}
		missing := 0
		for _, r := range ack.GetTestOptions().GetReplicas() {
			if !slices.ContainsFunc(r.Snapshots, func(s *clientv1.TestSnapshot) bool { return s.Name == snapshot }) {
				missing++
			}
		}
		if missing == 0 {
			return fmt.Sprintf("%d dataset(s) replicated up to %s", len(replicas), snapshot), nil
		}
	}
	return "", fmt.Errorf("the final snapshot didn't reach the DR host within %s", finalSyncTimeout)
}

// checkFailoverGuests waits until each started guest runs (and its guest
// agent answers, when enabled), or the check times out.
func (d *Deps) checkFailoverGuests(ctx context.Context, planID string, spec *planv1.PlanSpec) (string, error) {
	deadline := time.Now().Add(guestCheckTimeout)
	for {
		_, f, err := d.loadFailover(ctx, planID)
		if err != nil {
			return "", err
		}
		pending := 0
		for _, fg := range f.Guests {
			if fg.Status != "starting" {
				continue
			}
			ack, err := d.request(ctx, spec.DrHostId, time.Minute, &clientv1.Action{Kind: &clientv1.Action_FailoverCheckGuest{
				FailoverCheckGuest: &clientv1.FailoverCheckGuest{PlanId: planID, Vmid: fg.Vmid}}})
			c := ack.GetGuestCheck()
			ok := err == nil && c.GetRunning() && (!c.GetAgentEnabled() || c.GetAgentOk())
			if !ok && time.Now().Before(deadline) {
				pending++
				continue
			}
			vmid := fg.Vmid
			_, _ = d.updateFailover(ctx, planID, func(_ *store.FailoverRow, f *portalv1.Failover) error {
				g := f.Guests[guestIndex(f, vmid)]
				g.AgentEnabled, g.AgentOk = c.GetAgentEnabled(), c.GetAgentOk()
				switch {
				case ok:
					g.Status, g.Detail = "running", ""
				case err != nil:
					g.Status, g.Detail = "failed", "check: "+err.Error()
				case !c.GetRunning():
					g.Status, g.Detail = "failed", "not running"
				default:
					g.Status, g.Detail = "failed", "the QEMU guest agent didn't answer"
				}
				return nil
			})
		}
		if pending == 0 {
			break
		}
		if err := sleep(ctx, testPoll); err != nil {
			return "", err
		}
	}
	_, f, err := d.loadFailover(ctx, planID)
	if err != nil {
		return "", err
	}
	running := 0
	for _, g := range f.Guests {
		if g.Status == "running" {
			running++
		}
	}
	return fmt.Sprintf("%d of %d guest(s) running", running, len(f.Guests)), nil
}

// abortFailover undoes a planned failover that failed before replication
// stopped: the primary's guests are unlocked and started again.
func (d *Deps) abortFailover(ctx context.Context, planID string, step int, cause error) {
	detail := "the plan is unchanged"
	sp, err := d.Store.PlanByID(ctx, planID)
	if err == nil {
		spec, derr := decodeSpec(sp.AppliedSpec)
		_, f, ferr := d.loadFailover(ctx, planID)
		if derr == nil && ferr == nil && f.Planned && step >= foStepStopPrimary {
			var vmids []uint32
			for _, g := range startupOrder(spec) {
				vmids = append(vmids, g.Vmid)
			}
			ack, err := d.request(ctx, spec.PrimaryHostId, 15*time.Minute, &clientv1.Action{Kind: &clientv1.Action_FailoverUnlockGuests{
				FailoverUnlockGuests: &clientv1.FailoverUnlockGuests{Vmids: vmids, Start: true}}})
			if err != nil {
				detail = "restarting the guests on the primary failed: " + err.Error()
			} else {
				detail = "the primary's guests were started again: " + strings.Join(ack.GetOutput(), "; ")
			}
		}
	}
	_, _ = d.updateFailover(ctx, planID, func(_ *store.FailoverRow, f *portalv1.Failover) error {
		setFoStep(f, step, "failed", cause.Error())
		f.State, f.Error = portalv1.FailoverState_FAILOVER_STATE_ABORTED, foStepNames[step]+": "+cause.Error()+"; "+detail
		return nil
	})
	d.audit(ctx, "system", "plan.failover_aborted", "plan:"+planID, cause.Error()+"; "+detail)
}

// recordBreakGlass records a failover run on a DR host's command line: the
// plan becomes failed over (which locks the primary's guests and stops
// replication), with a failover entry and an audit record.
func (d *Deps) recordBreakGlass(ctx context.Context, h store.Host, bg *clientv1.BreakGlassFailover) error {
	sp, err := d.Store.PlanByID(ctx, bg.PlanId)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if sp.DRHostID != h.ID || sp.AppliedSpec == nil {
		return fmt.Errorf("host %s isn't the plan's DR host", h.Hostname)
	}
	if sp.State != store.PlanFailedOver {
		spec, err := decodeSpec(sp.AppliedSpec)
		if err != nil {
			return err
		}
		f := &portalv1.Failover{Id: strings.ToLower(store.NewID()), PlanId: sp.ID, State: portalv1.FailoverState_FAILOVER_STATE_COMPLETED,
			StartedBy: bg.User + "@" + h.Hostname, StartedAt: bg.At, CompletedAt: timestamppb.Now(), BreakGlass: true,
			DnsRecords: dnsRecords(spec)}
		for _, r := range f.DnsRecords {
			r.Status, r.Detail = "skipped", "switch by hand (break-glass failover)"
		}
		for _, g := range startupOrder(spec) {
			status := "failed"
			if slices.Contains(bg.Started, g.Vmid) {
				status = "running"
			}
			f.Guests = append(f.Guests, &portalv1.TestRunGuest{Vmid: g.Vmid, TestVmid: g.Vmid, Status: status})
		}
		for _, name := range foStepNames {
			f.Steps = append(f.Steps, &portalv1.TakeoverStep{Name: name, Status: "skipped", Detail: "run on the DR host's command line"})
		}
		data, _ := proto.Marshal(f)
		if err := d.Store.CreateFailover(ctx, store.FailoverRow{ID: f.Id, PlanID: sp.ID, Data: data, NextStep: foStepCount,
			StartedAt: bg.At.AsTime()}); err != nil && !errors.Is(err, store.ErrFailoverActive) {
			return err
		}
		if err := d.Store.SetPlanState(ctx, sp.ID, store.PlanFailedOver, sp.AppliedSpec); err != nil {
			return err
		}
		d.audit(ctx, bg.User+"@"+h.Hostname, "plan.failover_break_glass", "plan:"+sp.ID,
			fmt.Sprintf("break-glass failover of plan %q on %s at %s; %d guest(s) started",
				sp.Name, h.Hostname, bg.At.AsTime().UTC().Format(time.RFC3339), len(bg.Started)))
	}
	// The DR host's desired state now lists the plan as failed over, so it
	// removes its marker; the primary's locks its guests.
	d.reconcile(context.WithoutCancel(ctx), sp.PrimaryHostID, sp.DRHostID)
	go func() {
		if _, err := d.checkPlanDNS(context.WithoutCancel(ctx), sp.ID); err != nil {
			slog.Warn("dns check after break-glass failover", "plan", sp.ID, "err", err)
		}
	}()
	return nil
}
