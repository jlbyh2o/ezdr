package api

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	clientv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/client/v1"
	inventoryv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/inventory/v1"
	planv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/plan/v1"
	portalv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/portal/v1"
	"github.com/jlbyh2o/ezdr/internal/portal/store"
	"github.com/jlbyh2o/ezdr/internal/replication"
)

// OverviewService summarizes every host and plan for the dashboard.
type OverviewService struct{ *Deps }

// transferStale is how old a replication report may be and still show a
// transfer as running: clients report every few seconds while one runs.
const transferStale = 30 * time.Second

// GetOverview returns the hosts with their guests, the plans with their
// health and transfers, and the operations in progress.
func (s OverviewService) GetOverview(ctx context.Context, _ *connect.Request[portalv1.GetOverviewRequest]) (*connect.Response[portalv1.GetOverviewResponse], error) {
	resp := &portalv1.GetOverviewResponse{}
	hosts, err := s.Store.ListHosts(ctx)
	if err != nil {
		return nil, internalError(err)
	}
	for _, h := range hosts {
		oh, err := s.overviewHost(ctx, h)
		if err != nil {
			return nil, internalError(err)
		}
		resp.Hosts = append(resp.Hosts, oh)
	}

	plans, err := s.Store.ListPlans(ctx)
	if err != nil {
		return nil, internalError(err)
	}
	tests, err := s.Store.ActiveTestRuns(ctx)
	if err != nil {
		return nil, internalError(err)
	}
	testing := map[string]bool{}
	for _, row := range tests {
		t := &portalv1.TestRun{}
		if err := proto.Unmarshal(row.Data, t); err != nil {
			return nil, internalError(err)
		}
		testing[row.PlanID] = t.State == portalv1.TestState_TEST_STATE_RUNNING
		resp.Operations = append(resp.Operations, &portalv1.OverviewOperation{
			Kind: portalv1.OperationKind_OPERATION_KIND_TEST, Id: row.ID, PlanId: row.PlanID, PlanName: t.PlanName,
			State: testStateText(t.State), Step: currentStep(t.Steps), StartedBy: t.StartedBy, StartedAt: t.StartedAt,
		})
	}
	for _, sp := range plans {
		op, err := s.overviewPlan(ctx, sp, testing[sp.ID])
		if err != nil {
			return nil, internalError(err)
		}
		resp.Plans = append(resp.Plans, op)
		ops, err := s.planOperations(ctx, sp)
		if err != nil {
			return nil, internalError(err)
		}
		resp.Operations = append(resp.Operations, ops...)
	}
	for _, o := range resp.Operations {
		if o.PlanName == "" {
			o.PlanName = planName(plans, o.PlanId)
		}
	}
	slices.SortStableFunc(resp.Operations, func(a, b *portalv1.OverviewOperation) int {
		return a.StartedAt.AsTime().Compare(b.StartedAt.AsTime())
	})

	alerts, err := s.Store.FiringAlerts(ctx)
	if err != nil {
		return nil, internalError(err)
	}
	resp.FiringAlerts = uint32(len(alerts)) //nolint:gosec // small counts
	return connect.NewResponse(resp), nil
}

func hasKey[K comparable, V any](m map[K]V, k K) bool {
	_, ok := m[k]
	return ok
}

func planName(plans []store.Plan, id string) string {
	for _, p := range plans {
		if p.ID == id {
			return p.Name
		}
	}
	return ""
}

func (s OverviewService) overviewHost(ctx context.Context, h store.Host) (*portalv1.OverviewHost, error) {
	oh := &portalv1.OverviewHost{Id: h.ID, Hostname: h.Hostname, Online: s.Hub.Online(h.ID), HasInventory: h.HasInventory}
	if !h.LastSeenAt.IsZero() {
		oh.LastSeenAt = timestamppb.New(h.LastSeenAt)
	}
	if !h.HasInventory {
		return oh, nil
	}
	stored, err := s.Store.HostInventory(ctx, h.ID)
	if errors.Is(err, store.ErrNotFound) {
		return oh, nil
	}
	if err != nil {
		return nil, err
	}
	inv := &inventoryv1.Inventory{}
	if err := proto.Unmarshal(stored.Data, inv); err != nil {
		return nil, err
	}
	protected, err := s.Store.GuestPlans(ctx, h.ID)
	if err != nil {
		return nil, err
	}
	excluded, err := s.Store.GuestExclusions(ctx, h.ID)
	if err != nil {
		return nil, err
	}
	for _, g := range inv.Guests {
		oh.Guests = append(oh.Guests, &portalv1.OverviewGuest{
			Vmid: g.Vmid, Name: g.Name, Type: g.Type, Status: g.Status, Lock: g.Lock,
			PlanId: protected[g.Vmid], Template: g.Template, Excluded: hasKey(excluded, g.Vmid),
		})
	}
	slices.SortFunc(oh.Guests, func(a, b *portalv1.OverviewGuest) int { return int(a.Vmid) - int(b.Vmid) })
	return oh, nil
}

func (s OverviewService) overviewPlan(ctx context.Context, sp store.Plan, testing bool) (*portalv1.OverviewPlan, error) {
	p, err := PlanService(s).planMsg(ctx, sp)
	if err != nil {
		return nil, err
	}
	// Active plans run their applied specification.
	spec := p.Spec
	if p.AppliedSpec != nil && sp.State != store.PlanDraft {
		spec = p.AppliedSpec
	}
	op := &portalv1.OverviewPlan{
		Id: sp.ID, Name: sp.Name, State: p.State,
		PrimaryHostId: spec.PrimaryHostId, DrHostId: spec.DrHostId,
		Testing: testing, PendingChanges: p.PendingChanges,
	}
	guests := slices.Clone(spec.Guests)
	slices.SortStableFunc(guests, func(a, b *planv1.PlanGuest) int { return int(a.StartupOrder) - int(b.StartupOrder) })
	for _, g := range guests {
		op.Vmids = append(op.Vmids, g.Vmid)
	}
	health, err := s.planHealth(ctx, sp)
	if err != nil {
		return nil, err
	}
	op.Health = health.Health
	if runs, err := s.Store.TestRunsByPlan(ctx, sp.ID, 1); err != nil {
		return nil, err
	} else if len(runs) > 0 {
		op.LastTestAt = timestamppb.New(runs[0].StartedAt)
	}

	switch p.State {
	case portalv1.PlanState_PLAN_STATE_ACTIVE:
		op.Transfer, err = s.replicationTransfer(ctx, sp.ID, spec.DrHostId)
	case portalv1.PlanState_PLAN_STATE_FAILING_BACK:
		op.Transfer, err = s.failbackTransfer(ctx, sp.ID)
	}
	if err != nil {
		return nil, err
	}
	if len(health.Datasets) > 0 {
		op.Guests = s.guestReplication(ctx, spec, health.Datasets, op.Transfer != nil)
	}
	return op, nil
}

// guestReplication sums up the replication of each guest's disks, found
// through the primary's inventory.
func (s OverviewService) guestReplication(ctx context.Context, spec *planv1.PlanSpec, datasets []*portalv1.DatasetHealth, transferring bool) []*portalv1.OverviewGuestReplication {
	stored, err := s.Store.HostInventory(ctx, spec.PrimaryHostId)
	if err != nil {
		return nil
	}
	inv := &inventoryv1.Inventory{}
	if err := proto.Unmarshal(stored.Data, inv); err != nil {
		return nil
	}
	owner := map[string]uint32{}
	for _, g := range inv.Guests {
		for _, d := range g.Disks {
			if d.ZfsDataset != "" {
				owner[d.ZfsDataset] = g.Vmid
			}
		}
	}
	byVMID := map[uint32]*portalv1.OverviewGuestReplication{}
	var out []*portalv1.OverviewGuestReplication
	for _, pg := range spec.Guests {
		r := &portalv1.OverviewGuestReplication{Vmid: pg.Vmid}
		byVMID[pg.Vmid] = r
		out = append(out, r)
	}
	missing := map[uint32]bool{}
	for _, ds := range datasets {
		vmid := owner[ds.Dataset]
		r := byVMID[vmid]
		if r == nil {
			continue
		}
		r.Disks++
		if ds.Error != "" {
			r.Errors = append(r.Errors, ds.Error)
		}
		// A disk not replicated yet means the guest isn't either.
		if ds.LatestSnapshotAt == nil {
			missing[vmid] = true
		} else if r.LastReplicatedAt == nil || ds.LatestSnapshotAt.AsTime().Before(r.LastReplicatedAt.AsTime()) {
			r.LastReplicatedAt, r.LatestSnapshot = ds.LatestSnapshotAt, ds.LatestSnapshot
		}
		if transferring && (ds.State == "stepping" || ds.BytesReplicated < ds.BytesExpected) {
			r.Transferring = true
			r.BytesExpected += ds.BytesExpected
			r.BytesDone += ds.BytesReplicated
		}
	}
	for _, r := range out {
		if missing[r.Vmid] {
			r.LastReplicatedAt, r.LatestSnapshot = nil, ""
		}
	}
	return out
}

// replicationTransfer reports a running replication of the plan's pull jobs,
// from the DR host's latest report.
func (s OverviewService) replicationTransfer(ctx context.Context, planID, drHostID string) (*portalv1.OverviewTransfer, error) {
	b, at, err := s.Store.ReplicationStatus(ctx, drHostID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && time.Since(at) > transferStale) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	st := &clientv1.ReportReplicationRequest{}
	if err := proto.Unmarshal(b, st); err != nil {
		return nil, err
	}
	prefix := planJobPrefix(planID)
	var t *portalv1.OverviewTransfer
	for _, js := range st.Jobs {
		if !strings.HasPrefix(js.Name, prefix) || !replication.Transferring(js) {
			continue
		}
		if t == nil {
			t = &portalv1.OverviewTransfer{Direction: portalv1.OverviewTransfer_DIRECTION_TO_DR, StartedAt: js.AttemptStartedAt}
		}
		for _, ds := range js.Datasets {
			t.BytesExpected += ds.BytesExpected
			t.BytesDone += ds.BytesReplicated
		}
	}
	return t, nil
}

// planJobPrefix is the start of every zrepl job name of a plan (see
// replication.JobName).
func planJobPrefix(planID string) string {
	if len(planID) > 8 {
		planID = planID[:8]
	}
	return "ezdr_" + planID + "_"
}

// failbackTransfer reports a failback that is copying data back.
func (s OverviewService) failbackTransfer(ctx context.Context, planID string) (*portalv1.OverviewTransfer, error) {
	_, fb, err := s.loadFailback(ctx, planID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if fb.State != portalv1.FailbackState_FAILBACK_STATE_RUNNING || !strings.HasPrefix(currentStep(fb.Steps), "Copy") {
		return nil, nil
	}
	t := &portalv1.OverviewTransfer{Direction: portalv1.OverviewTransfer_DIRECTION_TO_PRIMARY, StartedAt: fb.StartedAt}
	for _, r := range fb.Rounds {
		t.BytesDone += r.Bytes
	}
	return t, nil
}

// planOperations returns the plan's takeover, failover, or failback while it
// runs or awaits confirmation.
func (s OverviewService) planOperations(ctx context.Context, sp store.Plan) ([]*portalv1.OverviewOperation, error) {
	var ops []*portalv1.OverviewOperation
	if t, err := s.loadTakeover(ctx, sp.ID); err != nil {
		return nil, err
	} else if t != nil && t.State == portalv1.TakeoverState_TAKEOVER_STATE_RUNNING {
		state := "running"
		if t.RollingBack {
			state = "rolling back"
		}
		ops = append(ops, &portalv1.OverviewOperation{Kind: portalv1.OperationKind_OPERATION_KIND_TAKEOVER, PlanId: sp.ID,
			State: state, Step: currentStep(t.Steps), StartedAt: firstStepTime(t.Steps, t.UpdatedAt)})
	}

	_, fo, err := s.loadFailover(ctx, sp.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
	case err != nil:
		return nil, err
	case fo.State == portalv1.FailoverState_FAILOVER_STATE_RUNNING || fo.State == portalv1.FailoverState_FAILOVER_STATE_AWAITING_CONFIRMATION:
		awaiting := fo.State == portalv1.FailoverState_FAILOVER_STATE_AWAITING_CONFIRMATION
		ops = append(ops, &portalv1.OverviewOperation{Kind: portalv1.OperationKind_OPERATION_KIND_FAILOVER, Id: fo.Id, PlanId: sp.ID,
			State: runState(awaiting), Step: currentStep(fo.Steps), StartedBy: fo.StartedBy, StartedAt: fo.StartedAt,
			AwaitingConfirmation: awaiting})
	}

	_, fb, err := s.loadFailback(ctx, sp.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
	case err != nil:
		return nil, err
	case fb.State == portalv1.FailbackState_FAILBACK_STATE_RUNNING || fb.State == portalv1.FailbackState_FAILBACK_STATE_AWAITING_CONFIRMATION:
		awaiting := fb.State == portalv1.FailbackState_FAILBACK_STATE_AWAITING_CONFIRMATION
		ops = append(ops, &portalv1.OverviewOperation{Kind: portalv1.OperationKind_OPERATION_KIND_FAILBACK, Id: fb.Id, PlanId: sp.ID,
			State: runState(awaiting), Step: currentStep(fb.Steps), StartedBy: fb.StartedBy, StartedAt: fb.StartedAt,
			AwaitingConfirmation: awaiting})
	}
	return ops, nil
}

func runState(awaiting bool) string {
	if awaiting {
		return "awaiting confirmation"
	}
	return "running"
}

func testStateText(s portalv1.TestState) string {
	switch s {
	case portalv1.TestState_TEST_STATE_STARTING:
		return "starting"
	case portalv1.TestState_TEST_STATE_ENDING:
		return "ending"
	default:
		return "running"
	}
}

// currentStep returns the name of the step running now, or else the first
// pending one.
func currentStep(steps []*portalv1.TakeoverStep) string {
	for _, st := range steps {
		if st.Status == "running" {
			return st.Name
		}
	}
	for _, st := range steps {
		if st.Status == "pending" {
			return st.Name
		}
	}
	return ""
}

func firstStepTime(steps []*portalv1.TakeoverStep, fallback *timestamppb.Timestamp) *timestamppb.Timestamp {
	for _, st := range steps {
		if st.UpdatedAt != nil && st.Status != "pending" {
			return st.UpdatedAt
		}
	}
	return fallback
}
