package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	clientv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/client/v1"
	inventoryv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/inventory/v1"
	planv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/plan/v1"
	portalv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/portal/v1"
	"github.com/jlbyh2o/ezdr/internal/plan"
	"github.com/jlbyh2o/ezdr/internal/portal/store"
	"github.com/jlbyh2o/ezdr/internal/replication"
)

// Taking over an existing zrepl setup: see docs/design/replication.md,
// section 5.

// preflightTimeout bounds how long the preflight waits for both hosts.
const preflightTimeout = 2 * time.Minute

// RunTakeoverPreflight checks on both hosts that an adopted plan can take over
// its zrepl setup.
func (s PlanService) RunTakeoverPreflight(ctx context.Context, req *connect.Request[portalv1.RunTakeoverPreflightRequest]) (*connect.Response[portalv1.RunTakeoverPreflightResponse], error) {
	sp, spec, err := s.loadPlan(ctx, req.Msg.Id)
	if err != nil {
		return nil, err
	}
	if sp.State != store.PlanDraft || spec.Takeover == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("only a draft plan that adopts a zrepl setup has a takeover preflight"))
	}
	if t, err := s.loadTakeover(ctx, sp.ID); err != nil {
		return nil, internalError(err)
	} else if t.GetState() == portalv1.TakeoverState_TAKEOVER_STATE_RUNNING {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("the takeover is already running"))
	}
	if err := s.requireValid(ctx, spec, sp.ID); err != nil {
		return nil, err
	}
	primary, dr, err := s.loadInventories(ctx, spec.PrimaryHostId, spec.DrHostId)
	if err != nil {
		return nil, err
	}
	var setup plan.ZreplSetup
	for _, c := range plan.ZreplSetups(primary, dr) {
		if c.Source.Name == spec.Takeover.SourceJob && c.Pull.Name == spec.Takeover.PullJob {
			setup = c
		}
	}
	if setup.Source == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("the adopted zrepl jobs weren't found"))
	}

	var priDatasets, drDatasets []string
	for _, d := range replication.TakeoverDatasets(spec, primary) {
		priDatasets = append(priDatasets, d.Primary)
		drDatasets = append(drDatasets, d.Replica)
	}
	var pri, drRes *clientv1.ZreplPreflightResult
	g, gctx := errgroup.WithContext(ctx)
	gctx, cancel := context.WithTimeout(gctx, preflightTimeout)
	defer cancel()
	run := func(hostID, name string, datasets []string, job string, out **clientv1.ZreplPreflightResult) func() error {
		return func() error {
			ack, err := s.Hub.Request(gctx, hostID, &clientv1.Action{Kind: &clientv1.Action_ZreplPreflight{
				ZreplPreflight: &clientv1.ZreplPreflight{Datasets: datasets, ReleaseJob: job}}})
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
	g.Go(run(spec.PrimaryHostId, "primary", priDatasets, setup.Source.Name, &pri))
	g.Go(run(spec.DrHostId, "DR host", drDatasets, setup.Pull.Name, &drRes))
	if err := g.Wait(); err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("preflight: %w", err))
	}

	report := replication.Preflight(spec, primary, dr, setup, pri, drRes)
	report.CheckedAt = timestamppb.Now()
	t := &portalv1.Takeover{State: portalv1.TakeoverState_TAKEOVER_STATE_READY, Preflight: report, UpdatedAt: timestamppb.Now()}
	if len(report.Problems) > 0 {
		t.State = portalv1.TakeoverState_TAKEOVER_STATE_BLOCKED
	}
	if err := s.saveTakeover(ctx, store.Takeover{PlanID: sp.ID}, t); err != nil {
		return nil, internalError(err)
	}
	s.audit(ctx, currentUser(ctx).Username, "plan.takeover_preflight", "plan:"+sp.ID,
		fmt.Sprintf("plan %q: %d dataset(s), %d problem(s)", sp.Name, len(report.Datasets), len(report.Problems)))
	return connect.NewResponse(&portalv1.RunTakeoverPreflightResponse{Takeover: t}), nil
}

// GetTakeover returns a plan's latest preflight and takeover progress.
func (s PlanService) GetTakeover(ctx context.Context, req *connect.Request[portalv1.GetTakeoverRequest]) (*connect.Response[portalv1.GetTakeoverResponse], error) {
	if _, _, err := s.loadPlan(ctx, req.Msg.Id); err != nil {
		return nil, err
	}
	t, err := s.loadTakeover(ctx, req.Msg.Id)
	if err != nil {
		return nil, internalError(err)
	}
	return connect.NewResponse(&portalv1.GetTakeoverResponse{Takeover: t}), nil
}

// loadTakeover returns a plan's takeover, or nil if there is none.
func (d *Deps) loadTakeover(ctx context.Context, planID string) (*portalv1.Takeover, error) {
	st, err := d.Store.TakeoverByPlan(ctx, planID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	t := &portalv1.Takeover{}
	return t, proto.Unmarshal(st.Data, t)
}

// saveTakeover stores t in row (whose other fields are kept as given).
func (d *Deps) saveTakeover(ctx context.Context, row store.Takeover, t *portalv1.Takeover) error {
	t.UpdatedAt = timestamppb.Now()
	data, err := proto.Marshal(t)
	if err != nil {
		return err
	}
	row.Data = data
	return d.Store.PutTakeover(ctx, row)
}

// Steps of a takeover, in order. The runner records the next step, so a
// takeover interrupted by a portal restart resumes there; every step is safe
// to repeat.
const (
	stepUpgradePrimary = iota
	stepUpgradeDR
	stepStopOldPull
	stepReplaceSource
	stepAddPull
	stepVerify
	stepRelease
	stepCount
)

var stepNames = [stepCount]string{
	"Upgrade zrepl on the primary",
	"Upgrade zrepl on the DR host",
	"Remove the old pull job on the DR host",
	"Replace the old source job on the primary",
	"Add EZDR's pull job on the DR host",
	"Check that the first replication is incremental",
	"Release the old jobs' holds and bookmarks",
}

const (
	// preflightMaxAge is how old a preflight may be when the takeover starts.
	preflightMaxAge = 15 * time.Minute
	// applyTimeout bounds waiting for a host to apply desired state.
	applyTimeout = 5 * time.Minute
	// minVerifyTimeout is the shortest wait for the first replication.
	minVerifyTimeout = 10 * time.Minute
)

// takeoverPoll is how often the runner checks hosts' progress; tests shorten
// it.
var takeoverPoll = 2 * time.Second

// StartTakeover starts taking over an adopted plan's zrepl setup.
func (s PlanService) StartTakeover(ctx context.Context, req *connect.Request[portalv1.StartTakeoverRequest]) (*connect.Response[portalv1.StartTakeoverResponse], error) {
	sp, spec, err := s.loadPlan(ctx, req.Msg.Id)
	if err != nil {
		return nil, err
	}
	if sp.State != store.PlanDraft || spec.Takeover == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("only a draft plan that adopts a zrepl setup can take it over"))
	}
	row, err := s.Store.TakeoverByPlan(ctx, sp.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("run the preflight first"))
	}
	if err != nil {
		return nil, internalError(err)
	}
	t := &portalv1.Takeover{}
	if err := proto.Unmarshal(row.Data, t); err != nil {
		return nil, internalError(err)
	}
	checked := t.GetPreflight().GetCheckedAt().AsTime()
	switch {
	case t.State == portalv1.TakeoverState_TAKEOVER_STATE_RUNNING:
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("the takeover is already running"))
	case t.State != portalv1.TakeoverState_TAKEOVER_STATE_READY:
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("the preflight found problems; fix them and check again"))
	case time.Since(checked) > preflightMaxAge || sp.UpdatedAt.After(checked):
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("the preflight is out of date; check again"))
	}
	if err := s.requireValid(ctx, spec, sp.ID); err != nil {
		return nil, err
	}

	t.State, t.Error, t.Steps = portalv1.TakeoverState_TAKEOVER_STATE_RUNNING, "", nil
	for i, name := range stepNames {
		st := &portalv1.TakeoverStep{Name: name, Status: "pending", UpdatedAt: timestamppb.Now()}
		if (i == stepUpgradePrimary && !t.Preflight.UpgradePrimary) || (i == stepUpgradeDR && !t.Preflight.UpgradeDr) {
			st.Status, st.Detail = "skipped", "already on zrepl 0.7"
		}
		t.Steps = append(t.Steps, st)
	}
	row = store.Takeover{PlanID: sp.ID, Backup: strings.ToLower(store.NewID())}
	if err := s.saveTakeover(ctx, row, t); err != nil {
		return nil, internalError(err)
	}
	s.audit(ctx, currentUser(ctx).Username, "plan.takeover_start", "plan:"+sp.ID,
		fmt.Sprintf("plan %q takes over %s and %s", sp.Name, spec.Takeover.SourceJob, spec.Takeover.PullJob))
	// The takeover outlives this request.
	go s.runTakeover(context.WithoutCancel(ctx), sp.ID)
	return connect.NewResponse(&portalv1.StartTakeoverResponse{Takeover: t}), nil
}

// RetryTakeoverRollback runs a failed takeover's rollback again.
func (s PlanService) RetryTakeoverRollback(ctx context.Context, req *connect.Request[portalv1.RetryTakeoverRollbackRequest]) (*connect.Response[portalv1.RetryTakeoverRollbackResponse], error) {
	sp, _, err := s.loadPlan(ctx, req.Msg.Id)
	if err != nil {
		return nil, err
	}
	row, err := s.Store.TakeoverByPlan(ctx, sp.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("the plan has no takeover"))
	}
	if err != nil {
		return nil, internalError(err)
	}
	t := &portalv1.Takeover{}
	if err := proto.Unmarshal(row.Data, t); err != nil {
		return nil, internalError(err)
	}
	if t.State != portalv1.TakeoverState_TAKEOVER_STATE_FAILED || !t.RollingBack || sp.State != store.PlanDraft {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("only a takeover whose rollback failed can be rolled back again"))
	}
	t.State = portalv1.TakeoverState_TAKEOVER_STATE_RUNNING
	if err := s.saveTakeover(ctx, row, t); err != nil {
		return nil, internalError(err)
	}
	s.audit(ctx, currentUser(ctx).Username, "plan.takeover_rollback_retry", "plan:"+sp.ID, fmt.Sprintf("plan %q", sp.Name))
	go s.runTakeover(context.WithoutCancel(ctx), sp.ID)
	return connect.NewResponse(&portalv1.RetryTakeoverRollbackResponse{Takeover: t}), nil
}

// ResumeTakeovers restarts takeovers that were running when the portal
// stopped.
func (d *Deps) ResumeTakeovers(ctx context.Context) {
	rows, err := d.Store.ListTakeovers(ctx)
	if err != nil {
		slog.Error("list takeovers", "err", err)
		return
	}
	for _, row := range rows {
		t := &portalv1.Takeover{}
		if proto.Unmarshal(row.Data, t) == nil && t.State == portalv1.TakeoverState_TAKEOVER_STATE_RUNNING {
			slog.Info("resuming takeover", "plan", row.PlanID, "step", row.NextStep)
			go d.runTakeover(ctx, row.PlanID)
		}
	}
}

var runningTakeovers sync.Map // plan ID -> struct{}

// takeoverRunning reports whether a plan's takeover is running, so the plan
// can't be changed meanwhile.
func (d *Deps) takeoverRunning(ctx context.Context, planID string) (bool, error) {
	t, err := d.loadTakeover(ctx, planID)
	return t.GetState() == portalv1.TakeoverState_TAKEOVER_STATE_RUNNING, err
}

// runTakeover runs a takeover's remaining steps until ctx is canceled. A
// failure before the release step rolls both hosts back to the old setup.
func (d *Deps) runTakeover(ctx context.Context, planID string) {
	if _, busy := runningTakeovers.LoadOrStore(planID, struct{}{}); busy {
		return
	}
	defer runningTakeovers.Delete(planID)
	fail := func(err error) { slog.Error("takeover", "plan", planID, "err", err) }

	for {
		row, err := d.Store.TakeoverByPlan(ctx, planID)
		if err != nil {
			fail(err)
			return
		}
		t := &portalv1.Takeover{}
		if err := proto.Unmarshal(row.Data, t); err != nil {
			fail(err)
			return
		}
		sp, err := d.Store.PlanByID(ctx, planID)
		if err != nil {
			fail(err)
			return
		}
		spec, err := decodeSpec(sp.Spec)
		if err != nil {
			fail(err)
			return
		}
		if t.State != portalv1.TakeoverState_TAKEOVER_STATE_RUNNING {
			return
		}
		if t.RollingBack {
			// Interrupted (or retried) rollback.
			d.rollbackTakeover(ctx, row, sp, spec, t, nil)
			return
		}
		if row.NextStep >= stepCount {
			d.completeTakeover(ctx, row, sp, spec, t)
			return
		}
		i := row.NextStep
		step := t.Steps[i]
		if step.Status == "skipped" {
			row.NextStep++
			if err := d.saveTakeover(ctx, row, t); err != nil {
				fail(err)
				return
			}
			continue
		}
		step.Status, step.Detail, step.UpdatedAt = "running", "", timestamppb.Now()
		if err := d.saveTakeover(ctx, row, t); err != nil {
			fail(err)
			return
		}
		slog.Info("takeover step", "plan", sp.Name, "step", step.Name)
		detail, err := d.takeoverStep(ctx, i, &row, spec, t)
		if ctx.Err() != nil {
			return // the portal is stopping; the step reruns when it starts again
		}
		step.Detail, step.UpdatedAt = detail, timestamppb.Now()
		if err != nil {
			step.Status = "failed"
			step.Detail = strings.TrimSpace(detail + " " + err.Error())
			slog.Error("takeover step failed", "plan", sp.Name, "step", step.Name, "err", err)
			if i == stepRelease {
				// Replication already runs under EZDR; the old holds can be
				// released by hand.
				t.Error = fmt.Sprintf("releasing the old jobs' holds and bookmarks failed (%v); run `zrepl zfs-abstraction release-all --job <job>` on each host",
					err)
				d.completeTakeover(ctx, row, sp, spec, t)
				return
			}
			d.rollbackTakeover(ctx, row, sp, spec, t, fmt.Errorf("%s: %w", step.Name, err))
			return
		}
		step.Status = "done"
		row.NextStep++
		if err := d.saveTakeover(ctx, row, t); err != nil {
			fail(err)
			return
		}
	}
}

// takeoverStep runs one step and returns a description of what it did.
func (d *Deps) takeoverStep(ctx context.Context, i int, row *store.Takeover, spec *planv1.PlanSpec, t *portalv1.Takeover) (string, error) {
	old := spec.Takeover
	switch i {
	case stepUpgradePrimary, stepUpgradeDR:
		host := spec.PrimaryHostId
		if i == stepUpgradeDR {
			host = spec.DrHostId
		}
		ack, err := d.request(ctx, host, 15*time.Minute, &clientv1.Action{Kind: &clientv1.Action_ZreplUpgrade{ZreplUpgrade: &clientv1.ZreplUpgrade{}}})
		return strings.Join(ack.GetOutput(), "; "), err

	case stepStopOldPull:
		_, err := d.request(ctx, spec.DrHostId, applyTimeout, &clientv1.Action{Kind: &clientv1.Action_ZreplRemoveJobs{
			ZreplRemoveJobs: &clientv1.ZreplRemoveJobs{Jobs: []string{old.PullJob}, Backup: row.Backup}}})
		return "removed " + old.PullJob, err

	case stepReplaceSource:
		if _, err := d.request(ctx, spec.PrimaryHostId, applyTimeout, &clientv1.Action{Kind: &clientv1.Action_ZreplRemoveJobs{
			ZreplRemoveJobs: &clientv1.ZreplRemoveJobs{Jobs: []string{old.SourceJob}, Backup: row.Backup}}}); err != nil {
			return "", err
		}
		row.PrimaryJobs = true
		if err := d.saveTakeover(ctx, *row, t); err != nil {
			return "", err
		}
		return "removed " + old.SourceJob + "; added EZDR's source job", d.waitApplied(ctx, spec.PrimaryHostId)

	case stepAddPull:
		row.DRJobs = true
		if err := d.saveTakeover(ctx, *row, t); err != nil {
			return "", err
		}
		return "added EZDR's pull job", d.waitApplied(ctx, spec.DrHostId)

	case stepVerify:
		return d.verifyTakeover(ctx, row.PlanID, spec, t)

	case stepRelease:
		var n int
		for _, h := range []struct{ host, job string }{{spec.PrimaryHostId, old.SourceJob}, {spec.DrHostId, old.PullJob}} {
			ack, err := d.request(ctx, h.host, applyTimeout, &clientv1.Action{Kind: &clientv1.Action_ZreplReleaseJobs{
				ZreplReleaseJobs: &clientv1.ZreplReleaseJobs{Jobs: []string{h.job}}}})
			n += len(ack.GetOutput())
			if err != nil {
				return fmt.Sprintf("released %d hold(s) and bookmark(s)", n), err
			}
		}
		return fmt.Sprintf("released %d hold(s) and bookmark(s)", n), nil
	}
	return "", fmt.Errorf("unknown step %d", i)
}

// request sends an action and waits for a successful acknowledgement. It
// keeps retrying while the host is offline (for example, right after a portal
// restart), until the timeout.
func (d *Deps) request(ctx context.Context, hostID string, timeout time.Duration, a *clientv1.Action) (*clientv1.AckActionRequest, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		ack, err := d.Hub.Request(ctx, hostID, a)
		switch {
		case errors.Is(err, ErrHostOffline):
			select {
			case <-ctx.Done():
				return nil, errors.New("the host stayed offline")
			case <-time.After(takeoverPoll):
				continue
			}
		case errors.Is(err, context.DeadlineExceeded):
			return nil, fmt.Errorf("no answer from the host within %s", timeout)
		case err != nil:
			return nil, err
		case !ack.Succeeded:
			return ack, errors.New(ack.Message)
		}
		return ack, nil
	}
}

// waitApplied pushes a host's desired state and waits until it's applied.
func (d *Deps) waitApplied(ctx context.Context, hostID string) error {
	ds, err := d.desiredState(ctx, hostID)
	if err != nil {
		return err
	}
	d.Hub.Send(hostID, &clientv1.SubscribeResponse{Message: &clientv1.SubscribeResponse_DesiredState{DesiredState: ds}})
	deadline := time.Now().Add(applyTimeout)
	for time.Now().Before(deadline) {
		h, err := d.Store.HostByID(ctx, hostID)
		if err != nil {
			return err
		}
		if h.AppliedGeneration >= ds.Generation {
			if h.ApplyError != "" {
				return errors.New(h.ApplyError)
			}
			return nil
		}
		if err := sleep(ctx, takeoverPoll); err != nil {
			return err
		}
	}
	return fmt.Errorf("the host didn't apply its configuration within %s", applyTimeout)
}

// verifyTakeover waits for EZDR's pull job to replicate every dataset that
// preflight found a common snapshot for, incrementally, in an attempt that
// started after the pull job was added.
func (d *Deps) verifyTakeover(ctx context.Context, planID string, spec *planv1.PlanSpec, t *portalv1.Takeover) (string, error) {
	primary, _, err := d.loadInventoriesFor(ctx, spec)
	if err != nil {
		return "", err
	}
	pulls := map[string]bool{}
	for _, g := range plan.JobGroups(spec, primary) {
		pulls[replication.PullJobName(replication.JobName(planID, g))] = true
	}
	want := map[string]bool{}
	for _, ds := range t.Preflight.Datasets {
		if !ds.FullSend {
			want[ds.Dataset] = true
		}
	}
	since := t.Steps[stepAddPull].UpdatedAt.AsTime()
	timeout := max(minVerifyTimeout, 3*time.Duration(spec.IntervalSeconds)*time.Second)
	deadline := time.Now().Add(timeout)
	last := "waiting for the first replication"
	for time.Now().Before(deadline) {
		done, msg, err := d.checkFirstReplication(ctx, spec.DrHostId, pulls, want, since)
		if err != nil {
			return "", err
		}
		if done {
			return msg, nil
		}
		last = msg
		if err := sleep(ctx, takeoverPoll); err != nil {
			return "", err
		}
	}
	return "", fmt.Errorf("no incremental replication within %s (%s)", timeout.Round(time.Minute), last)
}

// checkFirstReplication inspects the DR host's latest status report.
func (d *Deps) checkFirstReplication(ctx context.Context, drID string, pulls, want map[string]bool, since time.Time) (bool, string, error) {
	b, _, err := d.Store.ReplicationStatus(ctx, drID)
	if errors.Is(err, store.ErrNotFound) {
		return false, "waiting for the DR host's status", nil
	}
	if err != nil {
		return false, "", err
	}
	st := &clientv1.ReportReplicationRequest{}
	if err := proto.Unmarshal(b, st); err != nil {
		return false, "", err
	}
	replicated := map[string]bool{}
	var problems []string
	for _, j := range st.Jobs {
		if !pulls[j.Name] {
			continue
		}
		if j.AttemptStartedAt == nil || j.AttemptStartedAt.AsTime().Before(since) {
			return false, "waiting for the first replication", nil
		}
		problems = append(problems, j.Errors...)
		for _, ds := range j.Datasets {
			if !want[ds.Dataset] {
				continue
			}
			switch {
			case ds.FullSend:
				return false, "", fmt.Errorf("%s was sent in full, not incrementally", ds.Dataset)
			case ds.State == "done":
				replicated[ds.Dataset] = true
			case ds.Error != "":
				problems = append(problems, ds.Dataset+": "+ds.Error)
			}
		}
	}
	if len(replicated) == len(want) {
		return true, fmt.Sprintf("%d dataset(s) replicated incrementally", len(want)), nil
	}
	msg := fmt.Sprintf("%d of %d dataset(s) replicated", len(replicated), len(want))
	if len(problems) > 0 {
		msg += ": " + strings.Join(problems, "; ")
	}
	return false, msg, nil
}

// rollbackTakeover removes the plan's jobs from both hosts and restores their
// original main configurations. cause is the failure that started it, or nil
// when an interrupted or failed rollback runs again. Every part is safe to
// repeat.
func (d *Deps) rollbackTakeover(ctx context.Context, row store.Takeover, sp store.Plan, spec *planv1.PlanSpec, t *portalv1.Takeover, cause error) {
	if cause != nil {
		t.Error = cause.Error()
	}
	t.State, t.RollingBack = portalv1.TakeoverState_TAKEOVER_STATE_RUNNING, true
	step := &portalv1.TakeoverStep{Name: "Restore the old setup on both hosts", Status: "running", UpdatedAt: timestamppb.Now()}
	if n := len(t.Steps); n > stepCount && t.Steps[n-1].Status == "running" {
		step = t.Steps[n-1] // resume the interrupted attempt
	} else {
		t.Steps = append(t.Steps, step)
	}
	row.PrimaryJobs, row.DRJobs = false, false
	var errs []string
	if err := d.saveTakeover(ctx, row, t); err != nil {
		errs = append(errs, err.Error())
	}
	for _, h := range []struct{ name, id string }{{"primary", spec.PrimaryHostId}, {"DR host", spec.DrHostId}} {
		if err := d.waitApplied(ctx, h.id); err != nil {
			errs = append(errs, fmt.Sprintf("%s: removing EZDR's jobs: %v", h.name, err))
			continue
		}
		if _, err := d.request(ctx, h.id, applyTimeout, &clientv1.Action{Kind: &clientv1.Action_ZreplRestoreConfig{
			ZreplRestoreConfig: &clientv1.ZreplRestoreConfig{Backup: row.Backup}}}); err != nil {
			errs = append(errs, fmt.Sprintf("%s: restoring zrepl.yml: %v", h.name, err))
		}
	}
	if ctx.Err() != nil {
		return // the portal is stopping; the rollback resumes when it starts again
	}
	step.UpdatedAt = timestamppb.Now()
	if len(errs) == 0 {
		step.Status, step.Detail = "done", "the old jobs run again; the plan stays a draft"
		t.State = portalv1.TakeoverState_TAKEOVER_STATE_ROLLED_BACK
	} else {
		step.Status, step.Detail = "failed", strings.Join(errs, "; ")
		t.State = portalv1.TakeoverState_TAKEOVER_STATE_FAILED
	}
	if err := d.saveTakeover(ctx, row, t); err != nil {
		slog.Error("save takeover", "plan", sp.ID, "err", err)
	}
	d.audit(ctx, "system", "plan.takeover_rollback", "plan:"+sp.ID, fmt.Sprintf("plan %q: %s; %s", sp.Name, t.Error, step.Detail))
}

// completeTakeover makes the plan active with the adoption cleared: its jobs
// are already running, so the hosts' desired state doesn't change.
func (d *Deps) completeTakeover(ctx context.Context, row store.Takeover, sp store.Plan, spec *planv1.PlanSpec, t *portalv1.Takeover) {
	spec.Takeover = nil
	data, err := proto.Marshal(spec)
	if err == nil {
		sp.Spec = data
		_, err = d.Store.SavePlan(ctx, sp)
	}
	if err == nil {
		// Active first, then drop the takeover's own inclusion of the jobs,
		// so they're never missing from the desired state.
		err = d.Store.SetPlanState(ctx, sp.ID, store.PlanActive, data)
	}
	if err != nil {
		slog.Error("complete takeover", "plan", sp.ID, "err", err)
		t.State, t.Error = portalv1.TakeoverState_TAKEOVER_STATE_FAILED, "activating the plan after the takeover failed: "+err.Error()
	} else if t.State == portalv1.TakeoverState_TAKEOVER_STATE_RUNNING {
		t.State = portalv1.TakeoverState_TAKEOVER_STATE_COMPLETED
	}
	row.PrimaryJobs, row.DRJobs = false, false
	if err := d.saveTakeover(ctx, row, t); err != nil {
		slog.Error("save takeover", "plan", sp.ID, "err", err)
	}
	d.audit(ctx, "system", "plan.takeover_complete", "plan:"+sp.ID, fmt.Sprintf("plan %q is active; %s", sp.Name, t.Error))
	d.reconcile(ctx, spec.PrimaryHostId, spec.DrHostId)
}

// takeoverPlans returns draft plans whose takeover has added their jobs to
// hostID, with their editing specifications.
func (d *Deps) takeoverPlans(ctx context.Context, hostID string) ([]replication.Plan, error) {
	rows, err := d.Store.ListTakeovers(ctx)
	if err != nil {
		return nil, err
	}
	var out []replication.Plan
	for _, row := range rows {
		if !row.PrimaryJobs && !row.DRJobs {
			continue
		}
		sp, err := d.Store.PlanByID(ctx, row.PlanID)
		if err != nil {
			return nil, err
		}
		if sp.State != store.PlanDraft {
			continue // active plans are included anyway
		}
		spec, err := decodeSpec(sp.Spec)
		if err != nil {
			return nil, err
		}
		if (spec.PrimaryHostId == hostID && row.PrimaryJobs) || (spec.DrHostId == hostID && row.DRJobs) {
			out = append(out, replication.Plan{ID: sp.ID, Name: sp.Name, Spec: spec})
		}
	}
	return out, nil
}

// loadInventoriesFor returns a plan's hosts' inventories.
func (d *Deps) loadInventoriesFor(ctx context.Context, spec *planv1.PlanSpec) (primary, dr *inventoryv1.Inventory, err error) {
	return PlanService{Deps: d}.loadInventories(ctx, spec.PrimaryHostId, spec.DrHostId)
}

// sleep waits for d or until ctx is done.
func sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}
