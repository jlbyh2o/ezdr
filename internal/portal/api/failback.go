package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	clientv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/client/v1"
	planv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/plan/v1"
	portalv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/portal/v1"
	"github.com/jlbyh2o/ezdr/internal/plan"
	"github.com/jlbyh2o/ezdr/internal/portal/store"
	"github.com/jlbyh2o/ezdr/internal/replication"
)

// Failback: see docs/design/failback.md.

// RunFailbackPreflight checks what failing the plan back would do.
func (s FailoverService) RunFailbackPreflight(ctx context.Context, req *connect.Request[portalv1.RunFailbackPreflightRequest]) (*connect.Response[portalv1.RunFailbackPreflightResponse], error) {
	sp, spec, err := s.loadFailedOverPlan(ctx, req.Msg.PlanId)
	if err != nil {
		return nil, err
	}
	r, err := s.failbackPreflight(ctx, sp, spec)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&portalv1.RunFailbackPreflightResponse{Preflight: r}), nil
}

// loadFailedOverPlan loads a failed-over plan and its applied spec.
func (d *Deps) loadFailedOverPlan(ctx context.Context, planID string) (store.Plan, *planv1.PlanSpec, error) {
	sp, err := d.Store.PlanByID(ctx, planID)
	if errors.Is(err, store.ErrNotFound) {
		return sp, nil, connect.NewError(connect.CodeNotFound, errors.New("plan not found"))
	}
	if err != nil {
		return sp, nil, internalError(err)
	}
	if sp.State != store.PlanFailedOver || sp.AppliedSpec == nil {
		return sp, nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("only a failed-over plan can fail back"))
	}
	spec, err := decodeSpec(sp.AppliedSpec)
	if err != nil {
		return sp, nil, internalError(err)
	}
	return sp, spec, nil
}

// failbackPreflight lists the plan's datasets' snapshots on both hosts and
// works out what failing back would do.
func (d *Deps) failbackPreflight(ctx context.Context, sp store.Plan, spec *planv1.PlanSpec) (*portalv1.FailbackPreflight, error) {
	for _, h := range []struct{ id, name string }{{spec.PrimaryHostId, "primary"}, {spec.DrHostId, "DR host"}} {
		if !d.Hub.Online(h.id) {
			return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("the %s isn't connected", h.name))
		}
	}
	primary, dr, err := d.loadInventoriesFor(ctx, spec)
	if err != nil {
		return nil, err
	}
	var priDatasets, drDatasets []string
	for _, ds := range replication.TakeoverDatasets(spec, primary) {
		priDatasets = append(priDatasets, ds.Primary)
		drDatasets = append(drDatasets, ds.Replica)
	}
	var pri, drRes *clientv1.ZreplPreflightResult
	g, gctx := errgroup.WithContext(ctx)
	gctx, cancel := context.WithTimeout(gctx, preflightTimeout)
	defer cancel()
	run := func(hostID, name string, datasets []string, out **clientv1.ZreplPreflightResult) func() error {
		return func() error {
			ack, err := d.Hub.Request(gctx, hostID, &clientv1.Action{Kind: &clientv1.Action_ZreplPreflight{
				ZreplPreflight: &clientv1.ZreplPreflight{Datasets: datasets}}})
			switch {
			case err != nil:
				return fmt.Errorf("%s: %w", name, err)
			case !ack.Succeeded:
				return fmt.Errorf("%s: %s", name, ack.Message)
			}
			*out = ack.Preflight
			return nil
		}
	}
	g.Go(run(spec.PrimaryHostId, "primary", priDatasets, &pri))
	g.Go(run(spec.DrHostId, "DR host", drDatasets, &drRes))
	if err := g.Wait(); err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("preflight: %w", err))
	}
	r := replication.FailbackPreflight(spec, primary, dr, RecoveryStorageID(sp.ID, ""), pri, drRes)
	r.CheckedAt = timestamppb.Now()
	return r, nil
}

// Steps of a failback, in order. Steps before fbStepCleanup can be undone
// (the guests are started again on the DR host); from fbStepCleanup on, the
// failback is committed and failed steps are retried.
const (
	fbStepCopy = iota
	fbStepStopDR
	fbStepFinalCopy
	fbStepCleanup
	fbStepResume
	fbStepStart
	fbStepCheck
	fbStepConfirm
	fbStepCount
)

var fbStepNames = [fbStepCount]string{
	"Copy changes while the guests run on the DR host",
	"Shut down the guests on the DR host",
	"Copy the final changes",
	"Remove the guests and storages from the DR host",
	"Resume replication",
	"Start the guests on the primary in order",
	"Check the guests",
	"Verify the guests and switch DNS back",
}

const (
	// Copy rounds end once one copies less than failbackSmallRound, or
	// after failbackMaxRounds: the final copy, with the guests stopped,
	// should be small.
	failbackSmallRound = 256 << 20
	failbackMaxRounds  = 5
	// failbackRoundTimeout bounds one round's transfer, and
	// failbackConnectTimeout how long the primary waits for the DR host.
	failbackRoundTimeout   = 24 * time.Hour
	failbackConnectTimeout = 3 * time.Minute
)

// StartFailback starts failing the plan back to its primary.
func (s FailoverService) StartFailback(ctx context.Context, req *connect.Request[portalv1.StartFailbackRequest]) (*connect.Response[portalv1.StartFailbackResponse], error) {
	sp, spec, err := s.loadFailedOverPlan(ctx, req.Msg.PlanId)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.Msg.ConfirmName) != sp.Name {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("type the plan's name to confirm the failback"))
	}
	if running, err := s.failbackRunning(ctx, sp.ID); err != nil || running {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("the plan's last failback hasn't finished"))
	}
	if _, f, err := s.loadFailover(ctx, sp.ID); err == nil &&
		(f.State == portalv1.FailoverState_FAILOVER_STATE_RUNNING || f.State == portalv1.FailoverState_FAILOVER_STATE_AWAITING_CONFIRMATION) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("finish the failover first (it's running or waiting for confirmation)"))
	}
	pf, err := s.failbackPreflight(ctx, sp, spec)
	if err != nil {
		return nil, err
	}
	if len(pf.Problems) > 0 {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("the preflight found problems: %s", strings.Join(pf.Problems, "; ")))
	}
	if pf.Diverged && !req.Msg.DiscardDiverged {
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			errors.New("the primary has changes newer than the replicas; confirm discarding them"))
	}

	primary, _, err := s.loadInventoriesFor(ctx, spec)
	if err != nil {
		return nil, err
	}
	id := strings.ToLower(store.NewID())
	fb := &portalv1.Failback{Id: id, PlanId: sp.ID, State: portalv1.FailbackState_FAILBACK_STATE_RUNNING,
		StartedBy: currentUser(ctx).Username, StartedAt: timestamppb.Now(), Preflight: pf, DiscardDiverged: pf.Diverged,
		DnsRecords: dnsRecords(spec), Synced: map[string]string{}}
	for _, name := range fbStepNames {
		fb.Steps = append(fb.Steps, &portalv1.TakeoverStep{Name: name, Status: "pending", UpdatedAt: timestamppb.Now()})
	}
	for _, g := range startupOrder(spec) {
		fg := &portalv1.TestRunGuest{Vmid: g.Vmid, TestVmid: g.Vmid, Status: "pending"}
		for _, ig := range primary.GetGuests() {
			if ig.Vmid == g.Vmid {
				fg.Name, fg.Type = ig.Name, strings.ToLower(strings.TrimPrefix(ig.Type.String(), "GUEST_TYPE_"))
			}
		}
		fb.Guests = append(fb.Guests, fg)
	}
	data, _ := proto.Marshal(fb)
	err = s.Store.CreateFailback(ctx, store.FailoverRow{ID: id, PlanID: sp.ID, Active: true, Data: data, StartedAt: time.Now()})
	if errors.Is(err, store.ErrFailbackActive) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	if err != nil {
		return nil, internalError(err)
	}
	detail := fmt.Sprintf("failback of plan %q", sp.Name)
	if pf.Diverged {
		detail += "; the primary's diverged data is discarded"
	}
	s.audit(ctx, currentUser(ctx).Username, "plan.failback_start", "plan:"+sp.ID, detail)
	go s.runFailback(context.WithoutCancel(ctx), id, sp.ID)
	return connect.NewResponse(&portalv1.StartFailbackResponse{Failback: fb}), nil
}

// GetFailback returns the plan's latest failback.
func (s FailoverService) GetFailback(ctx context.Context, req *connect.Request[portalv1.GetFailbackRequest]) (*connect.Response[portalv1.GetFailbackResponse], error) {
	if req.Msg.Id != "" {
		fb := &portalv1.Failback{}
		if err := loadOperation(ctx, s.Store.FailbackByID, req.Msg.Id, req.Msg.PlanId, fb); err != nil {
			return nil, err
		}
		return connect.NewResponse(&portalv1.GetFailbackResponse{Failback: fb}), nil
	}
	_, fb, err := s.loadFailback(ctx, req.Msg.PlanId)
	if errors.Is(err, store.ErrNotFound) {
		return connect.NewResponse(&portalv1.GetFailbackResponse{}), nil
	}
	if err != nil {
		return nil, internalError(err)
	}
	return connect.NewResponse(&portalv1.GetFailbackResponse{Failback: fb}), nil
}

// ConfirmFailback records the operator's verification, switches DNS back
// if asked, and finishes the failback.
func (s FailoverService) ConfirmFailback(ctx context.Context, req *connect.Request[portalv1.ConfirmFailbackRequest]) (*connect.Response[portalv1.ConfirmFailbackResponse], error) {
	user := currentUser(ctx).Username
	fb, err := s.updateFailback(ctx, req.Msg.PlanId, func(_ *store.FailoverRow, fb *portalv1.Failback) error {
		if fb.State != portalv1.FailbackState_FAILBACK_STATE_AWAITING_CONFIRMATION {
			return connect.NewError(connect.CodeFailedPrecondition, errors.New("the failback isn't waiting for confirmation"))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	detail := "verified by " + user + "; DNS not switched back"
	if req.Msg.SwitchDns {
		detail = "verified by " + user + "; " + s.switchRecords(ctx, fb.PlanId, false, func(fn func([]*portalv1.DnsRecordSwitch)) {
			_, _ = s.updateFailback(ctx, fb.PlanId, func(_ *store.FailoverRow, fb *portalv1.Failback) error {
				fn(fb.DnsRecords)
				return nil
			})
		})
	}
	fb, err = s.updateFailback(ctx, req.Msg.PlanId, func(row *store.FailoverRow, fb *portalv1.Failback) error {
		for _, r := range fb.DnsRecords {
			if r.Status == "pending" {
				r.Status = "skipped"
			}
		}
		setFbStep(fb, fbStepConfirm, "done", detail)
		fb.State, fb.CompletedAt = portalv1.FailbackState_FAILBACK_STATE_COMPLETED, timestamppb.Now()
		row.NextStep = fbStepCount
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.audit(ctx, user, "plan.failback_confirm", "plan:"+fb.PlanId, detail)
	return connect.NewResponse(&portalv1.ConfirmFailbackResponse{Failback: fb}), nil
}

// RetryFailback resumes a committed failback whose step failed.
func (s FailoverService) RetryFailback(ctx context.Context, req *connect.Request[portalv1.RetryFailbackRequest]) (*connect.Response[portalv1.RetryFailbackResponse], error) {
	fb, err := s.updateFailback(ctx, req.Msg.PlanId, func(row *store.FailoverRow, fb *portalv1.Failback) error {
		if fb.State != portalv1.FailbackState_FAILBACK_STATE_FAILED {
			return connect.NewError(connect.CodeFailedPrecondition, errors.New("only a failed failback can be retried"))
		}
		fb.State, fb.Error = portalv1.FailbackState_FAILBACK_STATE_RUNNING, ""
		row.Active = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.audit(ctx, currentUser(ctx).Username, "plan.failback_retry", "plan:"+fb.PlanId, "")
	go s.runFailback(context.WithoutCancel(ctx), fb.Id, fb.PlanId)
	return connect.NewResponse(&portalv1.RetryFailbackResponse{Failback: fb}), nil
}

func (d *Deps) loadFailback(ctx context.Context, planID string) (store.FailoverRow, *portalv1.Failback, error) {
	row, err := d.Store.LatestFailback(ctx, planID)
	if err != nil {
		return row, nil, err
	}
	fb := &portalv1.Failback{}
	return row, fb, proto.Unmarshal(row.Data, fb)
}

var failbackLocks sync.Map // plan ID -> *sync.Mutex

// updateFailback loads the plan's latest failback, applies fn, and saves it.
func (d *Deps) updateFailback(ctx context.Context, planID string, fn func(row *store.FailoverRow, fb *portalv1.Failback) error) (*portalv1.Failback, error) {
	mu, _ := failbackLocks.LoadOrStore(planID, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()
	row, fb, err := d.loadFailback(ctx, planID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("the plan has no failback"))
	}
	if err != nil {
		return nil, internalError(err)
	}
	if err := fn(&row, fb); err != nil {
		return nil, err
	}
	if row.Data, err = proto.Marshal(fb); err != nil {
		return nil, internalError(err)
	}
	row.Active = fb.State == portalv1.FailbackState_FAILBACK_STATE_RUNNING
	if err := d.Store.UpdateFailback(ctx, row); err != nil {
		return nil, internalError(err)
	}
	return fb, nil
}

func setFbStep(fb *portalv1.Failback, i int, status, detail string) {
	fb.Steps[i].Status, fb.Steps[i].Detail, fb.Steps[i].UpdatedAt = status, detail, timestamppb.Now()
}

// failbackRunning reports whether a plan's failback hasn't finished: it's
// running, waiting for confirmation, or failed after committing (waiting
// for a retry). The plan can't be changed meanwhile.
func (d *Deps) failbackRunning(ctx context.Context, planID string) (bool, error) {
	_, fb, err := d.loadFailback(ctx, planID)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	switch fb.GetState() {
	case portalv1.FailbackState_FAILBACK_STATE_RUNNING, portalv1.FailbackState_FAILBACK_STATE_AWAITING_CONFIRMATION,
		portalv1.FailbackState_FAILBACK_STATE_FAILED:
		return true, err
	}
	return false, err
}

// ResumeFailbacks restarts failbacks that were running when the portal
// stopped.
func (d *Deps) ResumeFailbacks(ctx context.Context) {
	rows, err := d.Store.ActiveFailbacks(ctx)
	if err != nil {
		slog.Error("list failbacks", "err", err)
		return
	}
	for _, row := range rows {
		slog.Info("resuming failback", "plan", row.PlanID, "step", row.NextStep)
		go d.runFailback(ctx, row.ID, row.PlanID)
	}
}

var runningFailbacks sync.Map // plan ID -> struct{}

// runFailback runs a failback's remaining steps.
func (d *Deps) runFailback(ctx context.Context, id, planID string) {
	if _, busy := runningFailbacks.LoadOrStore(planID, struct{}{}); busy {
		return
	}
	defer runningFailbacks.Delete(planID)
	for ctx.Err() == nil {
		row, fb, err := d.loadFailback(ctx, planID)
		if err != nil || fb.Id != id || fb.State != portalv1.FailbackState_FAILBACK_STATE_RUNNING {
			return
		}
		step := row.NextStep
		if step >= fbStepConfirm {
			_, _ = d.updateFailback(ctx, planID, func(_ *store.FailoverRow, fb *portalv1.Failback) error {
				setFbStep(fb, fbStepConfirm, "running", "waiting for the operator to verify the guests")
				fb.State = portalv1.FailbackState_FAILBACK_STATE_AWAITING_CONFIRMATION
				return nil
			})
			return
		}
		_, _ = d.updateFailback(ctx, planID, func(_ *store.FailoverRow, fb *portalv1.Failback) error {
			setFbStep(fb, step, "running", "")
			return nil
		})
		slog.Info("failback step", "plan", planID, "step", fbStepNames[step])
		detail, err := d.failbackStep(ctx, planID, step)
		if ctx.Err() != nil {
			return // the portal is stopping; the step reruns when it starts again
		}
		if err != nil {
			slog.Error("failback step failed", "plan", planID, "step", fbStepNames[step], "err", err)
			if step < fbStepCleanup {
				d.abortFailback(ctx, planID, step, err)
				return
			}
			_, _ = d.updateFailback(ctx, planID, func(_ *store.FailoverRow, fb *portalv1.Failback) error {
				setFbStep(fb, step, "failed", strings.TrimSpace(detail+" "+err.Error()))
				fb.State, fb.Error = portalv1.FailbackState_FAILBACK_STATE_FAILED, fbStepNames[step]+": "+err.Error()
				return nil
			})
			d.audit(ctx, "system", "plan.failback_failed", "plan:"+planID, fbStepNames[step]+": "+err.Error())
			return
		}
		_, _ = d.updateFailback(ctx, planID, func(row *store.FailoverRow, fb *portalv1.Failback) error {
			setFbStep(fb, step, "done", detail)
			row.NextStep = step + 1
			return nil
		})
	}
}

// failbackStep runs one step and returns a description of what it did.
func (d *Deps) failbackStep(ctx context.Context, planID string, step int) (string, error) {
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
	case fbStepCopy:
		for {
			_, fb, err := d.loadFailback(ctx, planID)
			if err != nil {
				return "", err
			}
			if n := len(fb.Rounds); n > 0 {
				last := fb.Rounds[n-1]
				if last.Bytes < failbackSmallRound || n >= failbackMaxRounds {
					return fmt.Sprintf("%d round(s); the last copied %s", n, plan.FormatBytes(last.Bytes)), nil
				}
			}
			if _, err := d.failbackRound(ctx, sp, spec, false); err != nil {
				return "", err
			}
		}

	case fbStepStopDR:
		reverse := slices.Clone(vmids)
		slices.Reverse(reverse)
		timeout := plan.ShutdownTimeoutSeconds(spec)
		ack, err := d.request(ctx, spec.DrHostId, time.Duration(len(vmids)+1)*time.Duration(timeout+60)*time.Second,
			&clientv1.Action{Kind: &clientv1.Action_FailoverStopGuests{FailoverStopGuests: &clientv1.FailoverStopGuests{
				PlanId: planID, Vmids: reverse, ShutdownTimeoutSeconds: timeout}}})
		return strings.Join(ack.GetOutput(), "; "), err

	case fbStepFinalCopy:
		n, err := d.failbackRound(ctx, sp, spec, true)
		return "copied " + plan.FormatBytes(n), err

	case fbStepCleanup:
		snapshot, err := d.finalFailbackSnapshot(ctx, planID)
		if err != nil {
			return "", err
		}
		ack, err := d.request(ctx, spec.DrHostId, 15*time.Minute, &clientv1.Action{Kind: &clientv1.Action_FailbackCleanup{
			FailbackCleanup: &clientv1.FailbackCleanup{PlanId: planID, Vmids: vmids, Snapshot: snapshot}}})
		if len(ack.GetOutput()) > 0 {
			_, _ = d.updateFailback(ctx, planID, func(_ *store.FailoverRow, fb *portalv1.Failback) error {
				fb.Notes = append(fb.Notes, ack.GetOutput()...)
				return nil
			})
		}
		return fmt.Sprintf("%d guest(s) removed; replicas read-only at %s", len(vmids), snapshot), err

	case fbStepResume:
		snapshot, err := d.finalFailbackSnapshot(ctx, planID)
		if err != nil {
			return "", err
		}
		if sp.State != store.PlanActive {
			if err := d.Store.SetPlanState(ctx, planID, store.PlanActive, sp.AppliedSpec); err != nil {
				return "", err
			}
			d.audit(ctx, "system", "plan.failed_back", "plan:"+planID, fmt.Sprintf("plan %q failed back; replication resumes", sp.Name))
		}
		d.reconcile(ctx, spec.PrimaryHostId, spec.DrHostId)
		for _, h := range []struct{ id, name string }{{spec.PrimaryHostId, "primary"}, {spec.DrHostId, "DR host"}} {
			if err := d.waitApplied(ctx, h.id); err != nil {
				return "", fmt.Errorf("%s: %w", h.name, err)
			}
		}
		return "the plan is active again; replication resumes from " + snapshot, nil

	case fbStepStart:
		for i, g := range ordered {
			_, fb, err := d.loadFailback(ctx, planID)
			if err != nil {
				return "", err
			}
			if fb.Guests[guestIndex(fb.Guests, g.Vmid)].Status != "pending" {
				continue
			}
			_, err = d.request(ctx, spec.PrimaryHostId, 5*time.Minute, &clientv1.Action{Kind: &clientv1.Action_FailoverUnlockGuests{
				FailoverUnlockGuests: &clientv1.FailoverUnlockGuests{Vmids: []uint32{g.Vmid}, Start: true}}})
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			_, _ = d.updateFailback(ctx, planID, func(_ *store.FailoverRow, fb *portalv1.Failback) error {
				fg := fb.Guests[guestIndex(fb.Guests, g.Vmid)]
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

	case fbStepCheck:
		return d.checkGuests(ctx, spec.PrimaryHostId,
			func() ([]*portalv1.TestRunGuest, error) {
				_, fb, err := d.loadFailback(ctx, planID)
				return fb.GetGuests(), err
			},
			func(vmid uint32) *clientv1.Action {
				return &clientv1.Action{Kind: &clientv1.Action_FailbackCheckGuest{FailbackCheckGuest: &clientv1.FailbackCheckGuest{Vmid: vmid}}}
			},
			func(vmid uint32, fn func(*portalv1.TestRunGuest)) {
				_, _ = d.updateFailback(ctx, planID, func(_ *store.FailoverRow, fb *portalv1.Failback) error {
					fn(fb.Guests[guestIndex(fb.Guests, vmid)])
					return nil
				})
			})
	}
	return "", fmt.Errorf("unknown step %d", step)
}

// finalFailbackSnapshot returns the snapshot of the failback's final copy.
func (d *Deps) finalFailbackSnapshot(ctx context.Context, planID string) (string, error) {
	_, fb, err := d.loadFailback(ctx, planID)
	if err != nil {
		return "", err
	}
	for _, r := range slices.Backward(fb.Rounds) {
		if r.Final {
			return r.Snapshot, nil
		}
	}
	return "", errors.New("the final copy hasn't run")
}

// failbackRound snapshots the replicas on the DR host and copies the
// changes since the previous round (or the common snapshot) to the primary.
// It returns the bytes sent.
func (d *Deps) failbackRound(ctx context.Context, sp store.Plan, spec *planv1.PlanSpec, final bool) (uint64, error) {
	_, fb, err := d.loadFailback(ctx, sp.ID)
	if err != nil {
		return 0, err
	}
	snapshot, repeat := fb.PendingSnapshot, fb.PendingSnapshot != ""
	if !repeat {
		// Names have a resolution of a second (like zrepl's), so a quick
		// round waits for a new one.
		for snapshot == "" || failbackSnapshotUsed(fb, snapshot) {
			if snapshot != "" {
				if err := sleep(ctx, 200*time.Millisecond); err != nil {
					return 0, err
				}
			}
			snapshot = spec.SnapshotPrefix + failbackNow().UTC().Format("20060102_150405_000")
		}
		if _, err := d.updateFailback(ctx, sp.ID, func(_ *store.FailoverRow, fb *portalv1.Failback) error {
			fb.PendingSnapshot = snapshot
			return nil
		}); err != nil {
			return 0, err
		}
	}
	rp := replication.Plan{ID: sp.ID, Name: sp.Name, Spec: spec}
	hosts, err := d.replicationHosts(ctx, []replication.Plan{rp})
	if err != nil {
		return 0, err
	}
	primary, dr := hosts[spec.PrimaryHostId], hosts[spec.DrHostId]
	if primary == nil || dr == nil {
		return 0, errors.New("a host or its inventory is missing")
	}
	listen, connectAddr, freebind, err := replication.FailbackAddresses(rp, primary, dr)
	if err != nil {
		return 0, err
	}
	encrypted := map[string]bool{}
	for _, g := range plan.JobGroups(spec, primary.Inventory) {
		for _, ds := range g.Datasets {
			encrypted[ds] = g.Encrypted
		}
	}
	var datasets, replicas []string
	for _, ds := range fb.Preflight.GetDatasets() {
		datasets, replicas = append(datasets, ds.Dataset), append(replicas, ds.Replica)
	}

	if _, err := d.request(ctx, spec.DrHostId, 5*time.Minute, &clientv1.Action{Kind: &clientv1.Action_FailoverSnapshot{
		FailoverSnapshot: &clientv1.FailoverSnapshot{Datasets: replicas, Snapshot: snapshot}}}); err != nil {
		return 0, fmt.Errorf("DR host: %w", err)
	}
	// A repeated round may have copied some datasets already.
	have := map[string]bool{}
	if repeat {
		ack, err := d.request(ctx, spec.PrimaryHostId, preflightTimeout, &clientv1.Action{Kind: &clientv1.Action_ZreplPreflight{
			ZreplPreflight: &clientv1.ZreplPreflight{Datasets: datasets}}})
		if err != nil {
			return 0, fmt.Errorf("primary: %w", err)
		}
		for _, ds := range ack.GetPreflight().GetDatasets() {
			have[ds.Dataset] = slices.ContainsFunc(ds.Snapshots, func(s *clientv1.SnapshotInfo) bool { return s.Name == "@"+snapshot })
		}
	}
	recv := &clientv1.FailbackReceive{PlanId: sp.ID, ListenAddress: listen, ListenFreebind: freebind,
		Peer:                  &clientv1.Peer{Name: replication.PeerName(dr.ID), CertificatePem: dr.Certificate},
		ConnectTimeoutSeconds: uint32(failbackConnectTimeout / time.Second)}
	send := &clientv1.FailbackSend{PlanId: sp.ID, Address: connectAddr,
		Peer: &clientv1.Peer{Name: replication.PeerName(primary.ID), CertificatePem: primary.Certificate}}
	for _, ds := range fb.Preflight.GetDatasets() {
		if have[ds.Dataset] {
			continue
		}
		// The first round starts from the common snapshot, rolling diverged
		// datasets back once the operator confirmed it; later rounds roll
		// back anything written since the previous round.
		from, rollback := fb.Synced[ds.Dataset], true
		if from == "" {
			from, rollback = strings.TrimPrefix(ds.CommonSnapshot, "@"), fb.DiscardDiverged
		}
		recv.Datasets = append(recv.Datasets, &clientv1.FailbackTarget{Dataset: ds.Dataset, FromSnapshot: from, Rollback: rollback})
		send.Datasets = append(send.Datasets, &clientv1.FailbackSource{Replica: ds.Replica, Dataset: ds.Dataset,
			FromSnapshot: from, ToSnapshot: snapshot, Raw: encrypted[ds.Dataset]})
	}

	var bytes uint64
	if len(send.Datasets) > 0 {
		var recvErr error
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, recvErr = d.request(ctx, spec.PrimaryHostId, failbackRoundTimeout, &clientv1.Action{Kind: &clientv1.Action_FailbackReceive{FailbackReceive: recv}})
		}()
		ack, sendErr := d.request(ctx, spec.DrHostId, failbackRoundTimeout, &clientv1.Action{Kind: &clientv1.Action_FailbackSend{FailbackSend: send}})
		<-done
		switch {
		case recvErr != nil && sendErr != nil:
			return 0, fmt.Errorf("primary: %w; DR host: %w", recvErr, sendErr)
		case recvErr != nil:
			return 0, fmt.Errorf("primary: %w", recvErr)
		case sendErr != nil:
			return 0, fmt.Errorf("DR host: %w", sendErr)
		}
		for _, t := range ack.GetTransfer().GetDatasets() {
			bytes += t.Bytes
		}
	}
	_, err = d.updateFailback(ctx, sp.ID, func(_ *store.FailoverRow, fb *portalv1.Failback) error {
		if fb.Synced == nil {
			fb.Synced = map[string]string{}
		}
		for _, ds := range datasets {
			fb.Synced[ds] = snapshot
		}
		fb.Rounds = append(fb.Rounds, &portalv1.FailbackRound{Snapshot: snapshot, Bytes: bytes, CompletedAt: timestamppb.Now(), Final: final})
		fb.PendingSnapshot = ""
		return nil
	})
	return bytes, err
}

// failbackNow is the clock for round snapshot names (replaced in tests).
var failbackNow = time.Now

// failbackSnapshotUsed reports whether a failback already uses a snapshot
// name: a round's, or a common snapshot a round starts from.
func failbackSnapshotUsed(fb *portalv1.Failback, name string) bool {
	for _, r := range fb.Rounds {
		if r.Snapshot == name {
			return true
		}
	}
	for _, ds := range fb.Preflight.GetDatasets() {
		if ds.CommonSnapshot == "@"+name {
			return true
		}
	}
	return false
}

// abortFailback undoes a failback that failed before the DR host's cleanup:
// the guests there are unlocked and started again. The plan stays failed
// over; what was copied to the primary is superseded by the next attempt.
func (d *Deps) abortFailback(ctx context.Context, planID string, step int, cause error) {
	detail := "the guests still run on the DR host"
	if step >= fbStepStopDR {
		sp, err := d.Store.PlanByID(ctx, planID)
		if err == nil {
			if spec, err := decodeSpec(sp.AppliedSpec); err == nil {
				var vmids []uint32
				for _, g := range startupOrder(spec) {
					vmids = append(vmids, g.Vmid)
				}
				ack, err := d.request(ctx, spec.DrHostId, 15*time.Minute, &clientv1.Action{Kind: &clientv1.Action_FailoverUnlockGuests{
					FailoverUnlockGuests: &clientv1.FailoverUnlockGuests{Vmids: vmids, Start: true}}})
				if err != nil {
					detail = "starting the guests on the DR host again failed: " + err.Error()
				} else {
					detail = "the guests were started again on the DR host: " + strings.Join(ack.GetOutput(), "; ")
				}
			}
		}
	}
	_, _ = d.updateFailback(ctx, planID, func(_ *store.FailoverRow, fb *portalv1.Failback) error {
		setFbStep(fb, step, "failed", cause.Error())
		fb.State, fb.Error = portalv1.FailbackState_FAILBACK_STATE_ABORTED, fbStepNames[step]+": "+cause.Error()+"; "+detail
		return nil
	})
	d.audit(ctx, "system", "plan.failback_aborted", "plan:"+planID, cause.Error()+"; "+detail)
}
