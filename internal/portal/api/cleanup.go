package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/proto"

	clientv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/client/v1"
	planv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/plan/v1"
	portalv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/portal/v1"
	"github.com/jlbyh2o/ezdr/internal/portal/store"
	"github.com/jlbyh2o/ezdr/internal/replication"
)

// cleanupTimeout bounds one host's cleanup, including waiting for it while
// it's offline.
const cleanupTimeout = 30 * time.Minute

// Cleanup steps. Both kinds start with the two hosts; deleting a plan's
// data ends by deleting the plan.
const (
	cleanStepPrimary = iota
	cleanStepDR
	cleanStepDeletePlan
)

var cleanupTables = map[portalv1.DataCleanupKind]string{
	portalv1.DataCleanupKind_DATA_CLEANUP_KIND_PLAN:     store.CleanupPlan,
	portalv1.DataCleanupKind_DATA_CLEANUP_KIND_TAKEOVER: store.CleanupTakeover,
}

func cleanupStepNames(kind portalv1.DataCleanupKind) []string {
	if kind == portalv1.DataCleanupKind_DATA_CLEANUP_KIND_PLAN {
		return []string{"Clean up the primary", "Delete the replicas on the DR host", "Delete the plan"}
	}
	return []string{"Clean up the primary", "Clean up the DR host"}
}

// scanCleanup asks both hosts what removing the targets would do and builds
// the preview. Both hosts must be online.
func (d *Deps) scanCleanup(ctx context.Context, spec *planv1.PlanSpec, pri, dr replication.HostCleanup) (*portalv1.DataCleanupPreview, error) {
	preview := &portalv1.DataCleanupPreview{CheckedAt: ts(time.Now())}
	hosts := []struct {
		id, role string
		c        replication.HostCleanup
	}{{spec.PrimaryHostId, "primary", pri}, {spec.DrHostId, "DR host", dr}}
	results := make([]*clientv1.DataScanResult, len(hosts))
	g, gctx := errgroup.WithContext(ctx)
	gctx, cancel := context.WithTimeout(gctx, preflightTimeout)
	defer cancel()
	for i, h := range hosts {
		hc := &portalv1.HostDataCleanup{HostId: h.id, Role: h.role, ReleaseJobs: h.c.ReleaseJobs, Notes: h.c.Notes}
		if host, err := d.Store.HostByID(ctx, h.id); err == nil {
			hc.Hostname = host.Hostname
		}
		preview.Hosts = append(preview.Hosts, hc)
		if len(h.c.Targets) == 0 {
			continue
		}
		if !d.Hub.Online(h.id) {
			preview.Problems = append(preview.Problems, fmt.Sprintf("the %s (%s) is offline", h.role, hc.Hostname))
			continue
		}
		g.Go(func() error {
			ack, err := d.Hub.Request(gctx, h.id, &clientv1.Action{Kind: &clientv1.Action_DataScan{
				DataScan: &clientv1.DataScan{Targets: h.c.Targets, ReleaseJobs: h.c.ReleaseJobs}}})
			switch {
			case err != nil:
				return fmt.Errorf("%s: %w", h.role, err)
			case !ack.Succeeded:
				return fmt.Errorf("%s: %s", h.role, ack.Message)
			}
			results[i] = ack.DataScan
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("scan: %w", err))
	}
	for i, h := range hosts {
		hc := preview.Hosts[i]
		for j, s := range results[i].GetDatasets() {
			t := h.c.Targets[j]
			if !s.Exists {
				continue
			}
			hc.Datasets = append(hc.Datasets, &portalv1.DatasetCleanup{Dataset: s.Dataset, Destroy: t.Destroy, Prefix: t.Prefix,
				EmptyParentsBelow: t.EmptyParentsBelow, ReclaimBytes: s.ReclaimBytes, Snapshots: s.Snapshots,
				Bookmarks: s.Bookmarks, Skipped: s.Skipped, Problems: s.Problems})
			hc.ReclaimBytes += s.ReclaimBytes
			for _, p := range s.Problems {
				preview.Problems = append(preview.Problems, fmt.Sprintf("%s: %s: %s", hc.Hostname, s.Dataset, p))
			}
		}
	}
	preview.Fingerprint = fingerprint(preview)
	return preview, nil
}

// fingerprint identifies what a preview would remove.
func fingerprint(p *portalv1.DataCleanupPreview) string {
	h := sha256.New()
	for _, hc := range p.Hosts {
		fmt.Fprintf(h, "host %q %q\n", hc.HostId, hc.ReleaseJobs)
		for _, ds := range hc.Datasets {
			fmt.Fprintf(h, "dataset %q %v %q %q\n", ds.Dataset, ds.Destroy, ds.Prefix, ds.EmptyParentsBelow)
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// checkFingerprint refuses to start a cleanup that differs from the
// preview the operator reviewed.
func checkFingerprint(p *portalv1.DataCleanupPreview, reviewed string) error {
	if reviewed == "" || reviewed != p.Fingerprint {
		return connect.NewError(connect.CodeFailedPrecondition,
			errors.New("what would be removed changed since the preview; review it again"))
	}
	return nil
}

// empty reports whether a preview has nothing to remove.
func empty(p *portalv1.DataCleanupPreview) bool {
	for _, h := range p.Hosts {
		if len(h.Datasets) > 0 {
			return false
		}
	}
	return true
}

// planDataPreview previews deleting a deactivated plan's data.
func (s PlanService) planDataPreview(ctx context.Context, sp store.Plan, spec *planv1.PlanSpec) (*portalv1.DataCleanupPreview, error) {
	primary, err := s.loadHost(ctx, spec.PrimaryHostId)
	if err != nil {
		return nil, internalError(err)
	}
	dr, err := s.loadHost(ctx, spec.DrHostId)
	if err != nil {
		return nil, internalError(err)
	}
	if primary == nil || dr == nil || primary.Inventory == nil || dr.Inventory == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("both hosts must exist and have reported an inventory"))
	}
	all, err := s.Store.ListPlans(ctx)
	if err != nil {
		return nil, internalError(err)
	}
	others, err := s.otherReplicas(ctx, all, sp.ID, spec.DrHostId)
	if err != nil {
		return nil, internalError(err)
	}
	pri, drc := replication.PlanData(sp.ID, spec, primary.Inventory, dr.Inventory, others)
	return s.scanCleanup(ctx, spec, pri, drc)
}

// takeoverPreview previews cleaning up after a completed takeover.
func (s PlanService) takeoverPreview(ctx context.Context, sp store.Plan, spec *planv1.PlanSpec) (*portalv1.DataCleanupPreview, error) {
	t, err := s.loadTakeover(ctx, sp.ID)
	if err != nil {
		return nil, internalError(err)
	}
	if t.GetState() != portalv1.TakeoverState_TAKEOVER_STATE_COMPLETED {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("the plan has no completed takeover"))
	}
	if err := s.refuseIfFailedOver(ctx, sp.ID); err != nil {
		return nil, err
	}
	primary, err := s.loadHost(ctx, spec.PrimaryHostId)
	if err != nil {
		return nil, internalError(err)
	}
	dr, err := s.loadHost(ctx, spec.DrHostId)
	if err != nil {
		return nil, internalError(err)
	}
	if primary == nil || dr == nil || primary.Inventory == nil || dr.Inventory == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("both hosts must exist and have reported an inventory"))
	}
	pri, drc := replication.TakeoverLeftovers(spec, t.GetPreflight().GetDroppedDatasets(), primary.Inventory, dr.Inventory)
	return s.scanCleanup(ctx, spec, pri, drc)
}

// PreviewPlanDataDeletion asks both hosts what deleting a plan's data would
// remove.
func (s PlanService) PreviewPlanDataDeletion(ctx context.Context, req *connect.Request[portalv1.PreviewPlanDataDeletionRequest]) (*connect.Response[portalv1.PreviewPlanDataDeletionResponse], error) {
	sp, spec, err := s.loadPlan(ctx, req.Msg.PlanId)
	if err != nil {
		return nil, err
	}
	if sp.State != store.PlanDraft {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("deactivate the plan first"))
	}
	p, err := s.planDataPreview(ctx, sp, spec)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&portalv1.PreviewPlanDataDeletionResponse{Preview: p}), nil
}

// PreviewTakeoverCleanup asks both hosts what cleaning up after the
// takeover's old jobs would remove.
func (s PlanService) PreviewTakeoverCleanup(ctx context.Context, req *connect.Request[portalv1.PreviewTakeoverCleanupRequest]) (*connect.Response[portalv1.PreviewTakeoverCleanupResponse], error) {
	sp, spec, err := s.loadPlan(ctx, req.Msg.PlanId)
	if err != nil {
		return nil, err
	}
	p, err := s.takeoverPreview(ctx, sp, spec)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&portalv1.PreviewTakeoverCleanupResponse{Preview: p}), nil
}

// StartTakeoverCleanup cleans up after the takeover's old jobs.
func (s PlanService) StartTakeoverCleanup(ctx context.Context, req *connect.Request[portalv1.StartTakeoverCleanupRequest]) (*connect.Response[portalv1.StartTakeoverCleanupResponse], error) {
	sp, spec, err := s.loadPlan(ctx, req.Msg.PlanId)
	if err != nil {
		return nil, err
	}
	p, err := s.takeoverPreview(ctx, sp, spec)
	if err != nil {
		return nil, err
	}
	if err := checkFingerprint(p, req.Msg.PreviewFingerprint); err != nil {
		return nil, err
	}
	c, err := s.startCleanup(ctx, sp, portalv1.DataCleanupKind_DATA_CLEANUP_KIND_TAKEOVER, p)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&portalv1.StartTakeoverCleanupResponse{Cleanup: c}), nil
}

// startCleanup stores a new cleanup from its preview and starts the runner.
func (s PlanService) startCleanup(ctx context.Context, sp store.Plan, kind portalv1.DataCleanupKind, p *portalv1.DataCleanupPreview) (*portalv1.DataCleanup, error) {
	if len(p.Problems) > 0 {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New(strings.Join(p.Problems, "; ")))
	}
	if kind == portalv1.DataCleanupKind_DATA_CLEANUP_KIND_TAKEOVER && empty(p) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("there is nothing to clean up"))
	}
	now := time.Now()
	c := &portalv1.DataCleanup{Id: store.NewID(), PlanId: sp.ID, PlanName: sp.Name, Kind: kind,
		State: portalv1.DataCleanupState_DATA_CLEANUP_STATE_RUNNING, StartedBy: currentUser(ctx).Username,
		StartedAt: ts(now), Preview: p}
	for _, name := range cleanupStepNames(kind) {
		c.Steps = append(c.Steps, &portalv1.TakeoverStep{Name: name, Status: "pending", UpdatedAt: ts(now)})
	}
	data, err := proto.Marshal(c)
	if err != nil {
		return nil, internalError(err)
	}
	err = s.Store.CreateCleanup(ctx, cleanupTables[kind], store.FailoverRow{ID: c.Id, PlanID: sp.ID, Active: true, Data: data, StartedAt: now})
	if errors.Is(err, store.ErrCleanupActive) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	if err != nil {
		return nil, internalError(err)
	}
	action, what := "plan.takeover_cleanup_start", fmt.Sprintf("plan %q: cleaning up after the old zrepl jobs", sp.Name)
	if kind == portalv1.DataCleanupKind_DATA_CLEANUP_KIND_PLAN {
		action, what = "plan.delete_data_start", fmt.Sprintf("plan %q: deleting its replicated data, then the plan", sp.Name)
	}
	s.audit(ctx, c.StartedBy, action, "plan:"+sp.ID, what)
	go s.runCleanup(context.WithoutCancel(ctx), kind, c.Id, sp.ID)
	return c, nil
}

func (d *Deps) loadCleanup(ctx context.Context, kind portalv1.DataCleanupKind, planID string) (store.FailoverRow, *portalv1.DataCleanup, error) {
	table, ok := cleanupTables[kind]
	if !ok {
		return store.FailoverRow{}, nil, connect.NewError(connect.CodeInvalidArgument, errors.New("unknown cleanup kind"))
	}
	row, err := d.Store.LatestCleanup(ctx, table, planID)
	if err != nil {
		return row, nil, err
	}
	c := &portalv1.DataCleanup{}
	if err := proto.Unmarshal(row.Data, c); err != nil {
		return row, nil, err
	}
	return row, c, nil
}

var cleanupLocks sync.Map // kind/plan ID -> *sync.Mutex

// updateCleanup loads the plan's latest cleanup of a kind, applies fn, and
// saves it, holding a per-plan lock.
func (d *Deps) updateCleanup(ctx context.Context, kind portalv1.DataCleanupKind, planID string, fn func(row *store.FailoverRow, c *portalv1.DataCleanup) error) (*portalv1.DataCleanup, error) {
	mu, _ := cleanupLocks.LoadOrStore(kind.String()+"/"+planID, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()
	row, c, err := d.loadCleanup(ctx, kind, planID)
	if err != nil {
		return nil, err
	}
	if err := fn(&row, c); err != nil {
		return nil, err
	}
	row.Active = c.State == portalv1.DataCleanupState_DATA_CLEANUP_STATE_RUNNING
	if row.Data, err = proto.Marshal(c); err != nil {
		return nil, err
	}
	return c, d.Store.UpdateCleanup(ctx, cleanupTables[kind], row)
}

// deletionActive reports whether the plan's data is being deleted (running,
// or failed and waiting for a retry or cancellation).
func (d *Deps) deletionActive(ctx context.Context, planID string) (bool, error) {
	_, c, err := d.loadCleanup(ctx, portalv1.DataCleanupKind_DATA_CLEANUP_KIND_PLAN, planID)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return c.State == portalv1.DataCleanupState_DATA_CLEANUP_STATE_RUNNING ||
		c.State == portalv1.DataCleanupState_DATA_CLEANUP_STATE_FAILED, nil
}

var runningCleanups sync.Map // kind/plan ID -> struct{}

// runCleanup runs a cleanup's remaining steps.
func (d *Deps) runCleanup(ctx context.Context, kind portalv1.DataCleanupKind, id, planID string) {
	key := kind.String() + "/" + planID
	if _, busy := runningCleanups.LoadOrStore(key, struct{}{}); busy {
		return
	}
	defer runningCleanups.Delete(key)
	for {
		row, c, err := d.loadCleanup(ctx, kind, planID)
		if err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				slog.Error("cleanup: load", "plan", planID, "err", err)
			}
			return
		}
		if c.Id != id || c.State != portalv1.DataCleanupState_DATA_CLEANUP_STATE_RUNNING {
			return
		}
		step := row.NextStep
		if step >= len(c.Steps) {
			_, err := d.updateCleanup(ctx, kind, planID, func(_ *store.FailoverRow, c *portalv1.DataCleanup) error {
				c.State, c.CompletedAt = portalv1.DataCleanupState_DATA_CLEANUP_STATE_COMPLETED, ts(time.Now())
				return nil
			})
			if err != nil {
				slog.Error("cleanup: complete", "plan", planID, "err", err)
			}
			d.audit(ctx, "system", "plan.cleanup_completed", "plan:"+planID, fmt.Sprintf("plan %q: cleanup completed", c.PlanName))
			return
		}
		if step == cleanStepDeletePlan {
			// The plan's records, this cleanup's included, go with it.
			if err := d.Store.DeletePlan(ctx, planID); err != nil {
				d.failCleanup(ctx, kind, planID, step, err)
				return
			}
			d.audit(ctx, "system", "plan.delete", "plan:"+planID, fmt.Sprintf("deleted plan %q after deleting its data", c.PlanName))
			return
		}
		if _, err := d.updateCleanup(ctx, kind, planID, func(_ *store.FailoverRow, c *portalv1.DataCleanup) error {
			setCleanupStep(c, step, "running", "")
			return nil
		}); err != nil {
			slog.Error("cleanup: save", "plan", planID, "err", err)
			return
		}
		status, detail, err := d.cleanupHost(ctx, c.Preview.GetHosts()[step])
		if ctx.Err() != nil {
			return // the step runs again after a restart
		}
		if err != nil {
			d.failCleanup(ctx, kind, planID, step, err)
			return
		}
		if status == "done" {
			d.audit(ctx, "system", "plan.cleanup_host", "plan:"+planID,
				fmt.Sprintf("plan %q, %s: %s", c.PlanName, c.Preview.GetHosts()[step].Hostname, detail))
		}
		if _, err := d.updateCleanup(ctx, kind, planID, func(row *store.FailoverRow, c *portalv1.DataCleanup) error {
			setCleanupStep(c, step, status, detail)
			row.NextStep = step + 1
			return nil
		}); err != nil {
			slog.Error("cleanup: save", "plan", planID, "err", err)
			return
		}
	}
}

// cleanupHost asks a host to remove what its preview listed.
func (d *Deps) cleanupHost(ctx context.Context, h *portalv1.HostDataCleanup) (string, string, error) {
	if len(h.Datasets) == 0 {
		return "skipped", "nothing to remove", nil
	}
	var targets []*clientv1.DataTarget
	for _, ds := range h.Datasets {
		targets = append(targets, &clientv1.DataTarget{Dataset: ds.Dataset, Destroy: ds.Destroy, Prefix: ds.Prefix,
			EmptyParentsBelow: ds.EmptyParentsBelow})
	}
	ack, err := d.request(ctx, h.HostId, cleanupTimeout, &clientv1.Action{Kind: &clientv1.Action_DataCleanup{
		DataCleanup: &clientv1.DataCleanup{Targets: targets, ReleaseJobs: h.ReleaseJobs}}})
	if err != nil {
		return "", "", fmt.Errorf("%s: %w", h.Hostname, err)
	}
	return "done", strings.Join(ack.GetOutput(), "; "), nil
}

func (d *Deps) failCleanup(ctx context.Context, kind portalv1.DataCleanupKind, planID string, step int, cause error) {
	c, err := d.updateCleanup(ctx, kind, planID, func(_ *store.FailoverRow, c *portalv1.DataCleanup) error {
		setCleanupStep(c, step, "failed", cause.Error())
		c.State, c.Error = portalv1.DataCleanupState_DATA_CLEANUP_STATE_FAILED, cause.Error()
		return nil
	})
	if err != nil {
		slog.Error("cleanup: save", "plan", planID, "err", err)
		return
	}
	d.audit(ctx, "system", "plan.cleanup_failed", "plan:"+planID, fmt.Sprintf("plan %q: %s", c.PlanName, cause))
}

func setCleanupStep(c *portalv1.DataCleanup, i int, status, detail string) {
	c.Steps[i].Status, c.Steps[i].Detail, c.Steps[i].UpdatedAt = status, detail, ts(time.Now())
}

// ResumeCleanups restarts the runners of cleanups a portal restart
// interrupted.
func (d *Deps) ResumeCleanups(ctx context.Context) {
	for kind, table := range cleanupTables {
		rows, err := d.Store.ActiveCleanups(ctx, table)
		if err != nil {
			slog.Error("resume cleanups", "err", err)
			continue
		}
		for _, row := range rows {
			go d.runCleanup(ctx, kind, row.ID, row.PlanID)
		}
	}
}

// GetDataCleanup returns the plan's latest cleanup of a kind.
func (s PlanService) GetDataCleanup(ctx context.Context, req *connect.Request[portalv1.GetDataCleanupRequest]) (*connect.Response[portalv1.GetDataCleanupResponse], error) {
	_, c, err := s.loadCleanup(ctx, req.Msg.Kind, req.Msg.PlanId)
	if errors.Is(err, store.ErrNotFound) {
		return connect.NewResponse(&portalv1.GetDataCleanupResponse{}), nil
	}
	if err != nil {
		var ce *connect.Error
		if errors.As(err, &ce) {
			return nil, err
		}
		return nil, internalError(err)
	}
	return connect.NewResponse(&portalv1.GetDataCleanupResponse{Cleanup: c}), nil
}

// RetryDataCleanup runs a failed cleanup's failed step again.
func (s PlanService) RetryDataCleanup(ctx context.Context, req *connect.Request[portalv1.RetryDataCleanupRequest]) (*connect.Response[portalv1.RetryDataCleanupResponse], error) {
	c, err := s.updateCleanup(ctx, req.Msg.Kind, req.Msg.PlanId, func(_ *store.FailoverRow, c *portalv1.DataCleanup) error {
		if c.State != portalv1.DataCleanupState_DATA_CLEANUP_STATE_FAILED {
			return connect.NewError(connect.CodeFailedPrecondition, errors.New("only a failed cleanup can be retried"))
		}
		c.State, c.Error = portalv1.DataCleanupState_DATA_CLEANUP_STATE_RUNNING, ""
		return nil
	})
	if err != nil {
		return nil, cleanupError(err)
	}
	s.audit(ctx, currentUser(ctx).Username, "plan.cleanup_retry", "plan:"+req.Msg.PlanId, fmt.Sprintf("plan %q: cleanup retried", c.PlanName))
	go s.runCleanup(context.WithoutCancel(ctx), req.Msg.Kind, c.Id, req.Msg.PlanId)
	return connect.NewResponse(&portalv1.RetryDataCleanupResponse{Cleanup: c}), nil
}

// CancelDataCleanup gives up on a failed cleanup.
func (s PlanService) CancelDataCleanup(ctx context.Context, req *connect.Request[portalv1.CancelDataCleanupRequest]) (*connect.Response[portalv1.CancelDataCleanupResponse], error) {
	c, err := s.updateCleanup(ctx, req.Msg.Kind, req.Msg.PlanId, func(_ *store.FailoverRow, c *portalv1.DataCleanup) error {
		if c.State != portalv1.DataCleanupState_DATA_CLEANUP_STATE_FAILED {
			return connect.NewError(connect.CodeFailedPrecondition, errors.New("only a failed cleanup can be canceled"))
		}
		c.State, c.CompletedAt = portalv1.DataCleanupState_DATA_CLEANUP_STATE_CANCELED, ts(time.Now())
		return nil
	})
	if err != nil {
		return nil, cleanupError(err)
	}
	s.audit(ctx, currentUser(ctx).Username, "plan.cleanup_cancel", "plan:"+req.Msg.PlanId, fmt.Sprintf("plan %q: cleanup canceled", c.PlanName))
	return connect.NewResponse(&portalv1.CancelDataCleanupResponse{Cleanup: c}), nil
}

func cleanupError(err error) error {
	var ce *connect.Error
	switch {
	case errors.As(err, &ce):
		return err
	case errors.Is(err, store.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, errors.New("the plan has no cleanup"))
	default:
		return internalError(err)
	}
}
