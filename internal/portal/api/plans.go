package api

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	inventoryv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/inventory/v1"
	planv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/plan/v1"
	portalv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/portal/v1"
	"github.com/jlbyh2o/ezdr/internal/plan"
	"github.com/jlbyh2o/ezdr/internal/portal/store"
	"github.com/jlbyh2o/ezdr/internal/replication"
)

// PlanService manages DR plans.
type PlanService struct{ *Deps }

// loadHost returns what validation needs about a host, or nil if the host
// doesn't exist.
func (s PlanService) loadHost(ctx context.Context, id string) (*plan.Host, error) {
	if id == "" {
		return nil, nil
	}
	h, err := s.Store.HostByID(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	ph := &plan.Host{ID: h.ID, Hostname: h.Hostname}
	inv, err := s.Store.HostInventory(ctx, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
	case err != nil:
		return nil, err
	default:
		ph.Inventory = &inventoryv1.Inventory{}
		if err := proto.Unmarshal(inv.Data, ph.Inventory); err != nil {
			return nil, err
		}
		ph.ReceivedAt = inv.ReceivedAt
	}
	return ph, nil
}

// validate checks spec; planID is the plan being edited, if any.
func (s PlanService) validate(ctx context.Context, spec *planv1.PlanSpec, planID string) ([]*planv1.Issue, error) {
	primary, err := s.loadHost(ctx, spec.PrimaryHostId)
	if err != nil {
		return nil, err
	}
	dr, err := s.loadHost(ctx, spec.DrHostId)
	if err != nil {
		return nil, err
	}
	others := map[uint32]string{}
	if spec.PrimaryHostId != "" {
		guestPlans, err := s.Store.GuestPlans(ctx, spec.PrimaryHostId)
		if err != nil {
			return nil, err
		}
		names := map[string]string{}
		plans, err := s.Store.ListPlans(ctx)
		if err != nil {
			return nil, err
		}
		for _, p := range plans {
			names[p.ID] = p.Name
		}
		for vmid, id := range guestPlans {
			if id != planID {
				others[vmid] = names[id]
			}
		}
	}
	ports := map[uint32]string{}
	var tunnels []plan.OtherTunnel
	all, err := s.Store.ListPlans(ctx)
	if err != nil {
		return nil, err
	}
	for _, p := range all {
		if p.ID == planID || p.AppliedSpec == nil {
			continue
		}
		applied, err := decodeSpec(p.AppliedSpec)
		if err != nil {
			return nil, err
		}
		if t := applied.GetNetwork().GetTunnel(); t != nil {
			tunnels = append(tunnels, plan.OtherTunnel{Plan: p.Name, PrimaryHostID: applied.PrimaryHostId, DRHostID: applied.DrHostId, Tunnel: t})
		}
		if primary != nil && primary.Inventory != nil && p.PrimaryHostID == spec.PrimaryHostId {
			for _, g := range plan.JobGroups(applied, primary.Inventory) {
				ports[g.Port] = p.Name
			}
		}
	}
	replicas, err := s.otherReplicas(ctx, all, planID, spec.DrHostId)
	if err != nil {
		return nil, err
	}
	excluded := map[uint32]bool{}
	if spec.PrimaryHostId != "" {
		ex, err := s.Store.GuestExclusions(ctx, spec.PrimaryHostId)
		if err != nil {
			return nil, err
		}
		for vmid := range ex {
			excluded[vmid] = true
		}
	}
	failedOver := false
	if planID != "" {
		if sp, err := s.Store.PlanByID(ctx, planID); err == nil && sp.State == store.PlanFailedOver {
			failedOver = true
		} else if running, err := s.failoverRunning(ctx, planID); err == nil && running {
			failedOver = true
		}
	}
	return plan.Validate(spec, plan.Context{Primary: primary, DR: dr, OtherPlans: others, UsedPorts: ports, FailedOver: failedOver, Excluded: excluded,
		OtherTunnels: tunnels, OtherReplicas: replicas, Now: time.Now()}), nil
}

// otherReplicas returns where the plans other than planID with the given DR
// host keep their replicas, from both their saved and applied
// specifications.
func (s PlanService) otherReplicas(ctx context.Context, all []store.Plan, planID, drHostID string) ([]plan.OtherReplicas, error) {
	hosts := map[string]*plan.Host{}
	var out []plan.OtherReplicas
	for _, p := range all {
		if p.ID == planID || p.DRHostID != drHostID || drHostID == "" {
			continue
		}
		h, ok := hosts[p.PrimaryHostID]
		if !ok {
			var err error
			if h, err = s.loadHost(ctx, p.PrimaryHostID); err != nil {
				return nil, err
			}
			hosts[p.PrimaryHostID] = h
		}
		if h == nil || h.Inventory == nil {
			continue
		}
		o := plan.OtherReplicas{Plan: p.Name}
		for _, b := range [][]byte{p.Spec, p.AppliedSpec} {
			if b == nil {
				continue
			}
			sp, err := decodeSpec(b)
			if err != nil {
				return nil, err
			}
			o.Paths = append(o.Paths, plan.ReplicaPaths(sp, h.Inventory)...)
			for _, g := range plan.JobGroups(sp, h.Inventory) {
				o.ReceiveDatasets = append(o.ReceiveDatasets, g.ReceiveDataset)
			}
		}
		out = append(out, o)
	}
	return out, nil
}

// checkSpec enforces what a plan needs before it can be saved at all. Other
// problems are reported as validation issues; drafts may be saved with them.
func checkSpec(spec *planv1.PlanSpec) error {
	if spec == nil {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("plan specification is required"))
	}
	spec.Name = strings.TrimSpace(spec.Name)
	switch {
	case spec.Name == "":
		return connect.NewError(connect.CodeInvalidArgument, errors.New("the plan needs a name"))
	case len(spec.Name) > 100:
		return connect.NewError(connect.CodeInvalidArgument, errors.New("the plan name is too long"))
	case spec.PrimaryHostId == "" || spec.DrHostId == "":
		return connect.NewError(connect.CodeInvalidArgument, errors.New("choose a primary host and a DR host"))
	case spec.PrimaryHostId == spec.DrHostId:
		return connect.NewError(connect.CodeInvalidArgument, errors.New("the primary and DR hosts must be different"))
	}
	seen := map[uint32]bool{}
	for _, g := range spec.Guests {
		if seen[g.Vmid] {
			return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("guest %d is listed twice", g.Vmid))
		}
		seen[g.Vmid] = true
	}
	return nil
}

var planStates = map[string]portalv1.PlanState{
	store.PlanDraft:      portalv1.PlanState_PLAN_STATE_DRAFT,
	store.PlanActive:     portalv1.PlanState_PLAN_STATE_ACTIVE,
	store.PlanPaused:     portalv1.PlanState_PLAN_STATE_PAUSED,
	store.PlanFailedOver: portalv1.PlanState_PLAN_STATE_FAILED_OVER,
}

func decodeSpec(b []byte) (*planv1.PlanSpec, error) {
	spec := &planv1.PlanSpec{}
	return spec, proto.Unmarshal(b, spec)
}

// planMsg converts a stored plan, including its hosts' progress applying it.
func (s PlanService) planMsg(ctx context.Context, p store.Plan) (*portalv1.Plan, error) {
	spec, err := decodeSpec(p.Spec)
	if err != nil {
		return nil, err
	}
	msg := &portalv1.Plan{Id: p.ID, Spec: spec, CreatedBy: p.CreatedBy,
		CreatedAt: ts(p.CreatedAt), UpdatedAt: ts(p.UpdatedAt), State: planStates[p.State]}
	if running, err := s.failoverRunning(ctx, p.ID); err == nil && running {
		msg.State = portalv1.PlanState_PLAN_STATE_FAILING_OVER
	}
	if running, err := s.failbackRunning(ctx, p.ID); err == nil && running {
		msg.State = portalv1.PlanState_PLAN_STATE_FAILING_BACK
	}
	if deleting, err := s.deletionActive(ctx, p.ID); err == nil && deleting {
		msg.State = portalv1.PlanState_PLAN_STATE_DELETING
	}
	if p.AppliedSpec != nil {
		if msg.AppliedSpec, err = decodeSpec(p.AppliedSpec); err != nil {
			return nil, err
		}
		msg.AppliedAt = ts(p.AppliedAt)
		msg.PendingChanges = !proto.Equal(spec, msg.AppliedSpec)
	}
	for _, id := range []string{p.PrimaryHostID, p.DRHostID} {
		h, err := s.Store.HostByID(ctx, id)
		if err != nil {
			continue
		}
		msg.Hosts = append(msg.Hosts, &portalv1.HostApplyStatus{
			HostId: h.ID, Hostname: h.Hostname, Online: s.Hub.Online(h.ID),
			Applied:    h.AppliedGeneration >= h.DesiredGeneration && h.ApplyError == "",
			ApplyError: h.ApplyError, ZreplVersion: h.ZreplVersion, HasCertificate: h.ZreplCertificate != "",
		})
	}
	return msg, nil
}

// save stores spec as plan id ("" creates a new plan) and returns the stored
// plan with its validation issues.
func (s PlanService) save(ctx context.Context, id string, spec *planv1.PlanSpec) (*portalv1.Plan, []*planv1.Issue, error) {
	if err := checkSpec(spec); err != nil {
		return nil, nil, err
	}
	if id != "" {
		if running, err := s.takeoverRunning(ctx, id); err != nil {
			return nil, nil, internalError(err)
		} else if running {
			return nil, nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("the plan can't change while its takeover runs"))
		}
		if err := s.refuseIfFailedOver(ctx, id); err != nil {
			return nil, nil, err
		}
	}
	data, err := proto.Marshal(spec)
	if err != nil {
		return nil, nil, internalError(err)
	}
	vmids := make([]uint32, 0, len(spec.Guests))
	for _, g := range spec.Guests {
		vmids = append(vmids, g.Vmid)
	}
	sp, err := s.Store.SavePlan(ctx, store.Plan{
		ID: id, Name: spec.Name, Spec: data, PrimaryHostID: spec.PrimaryHostId, DRHostID: spec.DrHostId,
		VMIDs: vmids, CreatedBy: currentUser(ctx).Username,
	})
	switch {
	case errors.Is(err, store.ErrConflict):
		return nil, nil, connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("a plan named %q already exists", spec.Name))
	case errors.Is(err, store.ErrGuestInOtherPlan):
		return nil, nil, connect.NewError(connect.CodeFailedPrecondition, err)
	case errors.Is(err, store.ErrNotFound):
		return nil, nil, connect.NewError(connect.CodeNotFound, errors.New("plan or host not found"))
	case err != nil:
		return nil, nil, internalError(err)
	}
	msg, err := s.planMsg(ctx, sp)
	if err != nil {
		return nil, nil, internalError(err)
	}
	issues, err := s.validate(ctx, spec, sp.ID)
	if err != nil {
		return nil, nil, internalError(err)
	}
	return msg, issues, nil
}

// CreatePlan creates a plan.
func (s PlanService) CreatePlan(ctx context.Context, req *connect.Request[portalv1.CreatePlanRequest]) (*connect.Response[portalv1.CreatePlanResponse], error) {
	p, issues, err := s.save(ctx, "", req.Msg.Spec)
	if err != nil {
		return nil, err
	}
	s.audit(ctx, currentUser(ctx).Username, "plan.create", "plan:"+p.Id, fmt.Sprintf("created plan %q", p.Spec.Name))
	return connect.NewResponse(&portalv1.CreatePlanResponse{Plan: p, Issues: issues}), nil
}

// UpdatePlan replaces a plan's specification.
func (s PlanService) UpdatePlan(ctx context.Context, req *connect.Request[portalv1.UpdatePlanRequest]) (*connect.Response[portalv1.UpdatePlanResponse], error) {
	if req.Msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("plan ID is required"))
	}
	p, issues, err := s.save(ctx, req.Msg.Id, req.Msg.Spec)
	if err != nil {
		return nil, err
	}
	s.audit(ctx, currentUser(ctx).Username, "plan.update", "plan:"+p.Id, fmt.Sprintf("updated plan %q", p.Spec.Name))
	return connect.NewResponse(&portalv1.UpdatePlanResponse{Plan: p, Issues: issues}), nil
}

// GetPlan returns a plan with its validation issues.
func (s PlanService) GetPlan(ctx context.Context, req *connect.Request[portalv1.GetPlanRequest]) (*connect.Response[portalv1.GetPlanResponse], error) {
	sp, err := s.Store.PlanByID(ctx, req.Msg.Id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("plan not found"))
	}
	if err != nil {
		return nil, internalError(err)
	}
	p, err := s.planMsg(ctx, sp)
	if err != nil {
		return nil, internalError(err)
	}
	issues, err := s.validate(ctx, p.Spec, p.Id)
	if err != nil {
		return nil, internalError(err)
	}
	return connect.NewResponse(&portalv1.GetPlanResponse{Plan: p, Issues: issues}), nil
}

// ListPlans returns plan summaries with validation counts.
func (s PlanService) ListPlans(ctx context.Context, _ *connect.Request[portalv1.ListPlansRequest]) (*connect.Response[portalv1.ListPlansResponse], error) {
	plans, err := s.Store.ListPlans(ctx)
	if err != nil {
		return nil, internalError(err)
	}
	hosts, err := s.Store.ListHosts(ctx)
	if err != nil {
		return nil, internalError(err)
	}
	names := map[string]string{}
	for _, h := range hosts {
		names[h.ID] = h.Hostname
	}
	resp := &portalv1.ListPlansResponse{}
	for _, sp := range plans {
		p, err := s.planMsg(ctx, sp)
		if err != nil {
			return nil, internalError(err)
		}
		issues, err := s.validate(ctx, p.Spec, p.Id)
		if err != nil {
			return nil, internalError(err)
		}
		errs, warns := plan.Counts(issues)
		health, err := s.planHealth(ctx, sp)
		if err != nil {
			return nil, internalError(err)
		}
		resp.Plans = append(resp.Plans, &portalv1.PlanSummary{
			Health: health.Health,
			Id:     p.Id, Name: p.Spec.Name,
			PrimaryHostname: names[p.Spec.PrimaryHostId], DrHostname: names[p.Spec.DrHostId],
			GuestCount:      uint32(len(p.Spec.Guests)), //nolint:gosec // small counts
			IntervalSeconds: p.Spec.IntervalSeconds,
			ErrorCount:      uint32(errs),  //nolint:gosec // small counts
			WarningCount:    uint32(warns), //nolint:gosec // small counts
			State:           p.State, PendingChanges: p.PendingChanges,
		})
	}
	return connect.NewResponse(resp), nil
}

// DeletePlan removes a plan.
func (s PlanService) DeletePlan(ctx context.Context, req *connect.Request[portalv1.DeletePlanRequest]) (*connect.Response[portalv1.DeletePlanResponse], error) {
	sp, err := s.Store.PlanByID(ctx, req.Msg.Id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("plan not found"))
	}
	if err != nil {
		return nil, internalError(err)
	}
	if sp.State != store.PlanDraft {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("deactivate the plan before deleting it"))
	}
	if running, err := s.takeoverRunning(ctx, sp.ID); err != nil {
		return nil, internalError(err)
	} else if running {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("the plan can't be deleted while its takeover runs"))
	}
	if runs, err := s.Store.TestRunsByPlan(ctx, sp.ID, 1); err != nil {
		return nil, internalError(err)
	} else if len(runs) > 0 && runs[0].Active {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("end the plan's test failover before deleting it"))
	}
	if err := s.refuseIfFailedOver(ctx, sp.ID); err != nil {
		return nil, err
	}
	if _, c, err := s.loadCleanup(ctx, portalv1.DataCleanupKind_DATA_CLEANUP_KIND_TAKEOVER, sp.ID); err == nil &&
		c.State == portalv1.DataCleanupState_DATA_CLEANUP_STATE_RUNNING {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("wait for the takeover cleanup to finish"))
	}
	if req.Msg.DeleteData {
		if strings.TrimSpace(req.Msg.ConfirmName) != sp.Name {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("type the plan's name to confirm"))
		}
		spec, err := decodeSpec(sp.Spec)
		if err != nil {
			return nil, internalError(err)
		}
		p, err := s.planDataPreview(ctx, sp, spec)
		if err != nil {
			return nil, err
		}
		if _, err := s.startCleanup(ctx, sp, portalv1.DataCleanupKind_DATA_CLEANUP_KIND_PLAN, p); err != nil {
			return nil, err
		}
		return connect.NewResponse(&portalv1.DeletePlanResponse{}), nil
	}
	if err := s.Store.DeletePlan(ctx, sp.ID); err != nil {
		return nil, internalError(err)
	}
	s.audit(ctx, currentUser(ctx).Username, "plan.delete", "plan:"+sp.ID, fmt.Sprintf("deleted plan %q", sp.Name))
	return connect.NewResponse(&portalv1.DeletePlanResponse{}), nil
}

// ValidatePlan checks a specification without saving it.
func (s PlanService) ValidatePlan(ctx context.Context, req *connect.Request[portalv1.ValidatePlanRequest]) (*connect.Response[portalv1.ValidatePlanResponse], error) {
	if req.Msg.Spec == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("plan specification is required"))
	}
	issues, err := s.validate(ctx, req.Msg.Spec, req.Msg.PlanId)
	if err != nil {
		return nil, internalError(err)
	}
	return connect.NewResponse(&portalv1.ValidatePlanResponse{Issues: issues}), nil
}

// SuggestPlan fills in missing mappings, startup order, and defaults.
func (s PlanService) SuggestPlan(ctx context.Context, req *connect.Request[portalv1.SuggestPlanRequest]) (*connect.Response[portalv1.SuggestPlanResponse], error) {
	spec := req.Msg.Spec
	if spec == nil {
		spec = &planv1.PlanSpec{}
	}
	primary, err := s.loadHost(ctx, spec.PrimaryHostId)
	if err != nil {
		return nil, internalError(err)
	}
	dr, err := s.loadHost(ctx, spec.DrHostId)
	if err != nil {
		return nil, internalError(err)
	}
	var pInv, dInv *inventoryv1.Inventory
	if primary != nil {
		pInv = primary.Inventory
	}
	if dr != nil {
		dInv = dr.Inventory
	}
	return connect.NewResponse(&portalv1.SuggestPlanResponse{Spec: plan.Suggest(spec, pInv, dInv)}), nil
}

// ListZreplSetups lists hand-written zrepl setups between two hosts.
func (s PlanService) ListZreplSetups(ctx context.Context, req *connect.Request[portalv1.ListZreplSetupsRequest]) (*connect.Response[portalv1.ListZreplSetupsResponse], error) {
	primary, dr, err := s.loadInventories(ctx, req.Msg.PrimaryHostId, req.Msg.DrHostId)
	if err != nil {
		return nil, err
	}
	resp := &portalv1.ListZreplSetupsResponse{}
	for _, c := range plan.ZreplSetups(primary, dr) {
		resp.Setups = append(resp.Setups, &portalv1.ZreplSetup{SourceJob: c.Source, PullJob: c.Pull})
	}
	return connect.NewResponse(resp), nil
}

// AdoptZreplSetup fills in a specification from an existing zrepl setup.
func (s PlanService) AdoptZreplSetup(ctx context.Context, req *connect.Request[portalv1.AdoptZreplSetupRequest]) (*connect.Response[portalv1.AdoptZreplSetupResponse], error) {
	spec := req.Msg.Spec
	if spec == nil {
		spec = &planv1.PlanSpec{}
	}
	primary, dr, err := s.loadInventories(ctx, spec.PrimaryHostId, spec.DrHostId)
	if err != nil {
		return nil, err
	}
	adopted, notes, err := plan.Adopt(spec, primary, dr, req.Msg.SourceJob, req.Msg.PullJob)
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	return connect.NewResponse(&portalv1.AdoptZreplSetupResponse{Spec: adopted, Notes: notes}), nil
}

// loadInventories returns both hosts' inventories, which must exist.
func (s PlanService) loadInventories(ctx context.Context, primaryID, drID string) (primary, dr *inventoryv1.Inventory, err error) {
	for _, h := range []struct {
		id  string
		out **inventoryv1.Inventory
	}{{primaryID, &primary}, {drID, &dr}} {
		host, err := s.loadHost(ctx, h.id)
		if err != nil {
			return nil, nil, internalError(err)
		}
		if host == nil || host.Inventory == nil {
			return nil, nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("choose both hosts first; each must have reported its inventory"))
		}
		*h.out = host.Inventory
	}
	return primary, dr, nil
}

// loadPlan returns a stored plan and its decoded editing specification.
func (s PlanService) loadPlan(ctx context.Context, id string) (store.Plan, *planv1.PlanSpec, error) {
	sp, err := s.Store.PlanByID(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return store.Plan{}, nil, connect.NewError(connect.CodeNotFound, errors.New("plan not found"))
	}
	if err != nil {
		return store.Plan{}, nil, internalError(err)
	}
	spec, err := decodeSpec(sp.Spec)
	if err != nil {
		return store.Plan{}, nil, internalError(err)
	}
	return sp, spec, nil
}

// requireValid refuses specifications with validation errors.
func (s PlanService) requireValid(ctx context.Context, spec *planv1.PlanSpec, id string) error {
	issues, err := s.validate(ctx, spec, id)
	if err != nil {
		return internalError(err)
	}
	if errs, _ := plan.Counts(issues); errs > 0 {
		return connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("the plan has %d validation error(s); fix them first", errs))
	}
	return nil
}

// transition changes a plan's state, pushes new desired state to every host
// affected, and returns the updated plan.
func (s PlanService) transition(ctx context.Context, sp store.Plan, state string, applied *planv1.PlanSpec, action string) (*portalv1.Plan, error) {
	hosts := []string{sp.PrimaryHostID, sp.DRHostID}
	if old, err := decodeSpec(sp.AppliedSpec); err == nil && sp.AppliedSpec != nil {
		hosts = append(hosts, old.PrimaryHostId, old.DrHostId)
	}
	var data []byte
	if applied != nil {
		var err error
		if data, err = proto.Marshal(applied); err != nil {
			return nil, internalError(err)
		}
	}
	if err := s.Store.SetPlanState(ctx, sp.ID, state, data); err != nil {
		return nil, internalError(err)
	}
	s.audit(ctx, currentUser(ctx).Username, action, "plan:"+sp.ID, fmt.Sprintf("plan %q", sp.Name))
	s.reconcile(context.WithoutCancel(ctx), hosts...)
	updated, err := s.Store.PlanByID(ctx, sp.ID)
	if err != nil {
		return nil, internalError(err)
	}
	msg, err := s.planMsg(ctx, updated)
	if err != nil {
		return nil, internalError(err)
	}
	return msg, nil
}

// PreviewPlanChanges lists what applying the editing specification would
// change on each host.
func (s PlanService) PreviewPlanChanges(ctx context.Context, req *connect.Request[portalv1.PreviewPlanChangesRequest]) (*connect.Response[portalv1.PreviewPlanChangesResponse], error) {
	sp, spec, err := s.loadPlan(ctx, req.Msg.Id)
	if err != nil {
		return nil, err
	}
	before, err := s.activePlans(ctx, nil)
	if err != nil {
		return nil, internalError(err)
	}
	after, err := s.activePlans(ctx, &replication.Plan{ID: sp.ID, Name: sp.Name, Spec: spec})
	if err != nil {
		return nil, internalError(err)
	}
	resp := &portalv1.PreviewPlanChangesResponse{}
	for _, id := range []string{spec.PrimaryHostId, spec.DrHostId} {
		h, err := s.Store.HostByID(ctx, id)
		if err != nil {
			continue
		}
		bs, _, err := s.desiredConfig(ctx, id, before, nil)
		if err != nil {
			return nil, internalError(err)
		}
		as, problems, err := s.desiredConfig(ctx, id, after, nil)
		if err != nil {
			return nil, internalError(err)
		}
		b, a := bs.Zrepl, as.Zrepl
		var changes []string
		if h.ZreplVersion == "" && len(a.SourceJobs)+len(a.PullJobs) > 0 {
			changes = append(changes, "install zrepl 0.7 from zrepl's official apt repository and hold the package")
		}
		if len(a.SourceJobs)+len(a.PullJobs) > 0 && len(b.SourceJobs)+len(b.PullJobs) == 0 {
			changes = append(changes, "include EZDR's job file from /etc/zrepl/zrepl.yml, if it doesn't already (backing up the current file)")
		}
		changes = append(changes, replication.TunnelChanges(bs.SiteTunnel, as.SiteTunnel)...)
		changes = append(changes, replication.Changes(b, a)...)
		changes = append(changes, problems...)
		if len(changes) == 0 {
			changes = []string{"no changes"}
		}
		resp.Hosts = append(resp.Hosts, &portalv1.HostChanges{HostId: id, Hostname: h.Hostname, Changes: changes})
	}
	if resp.Issues, err = s.validate(ctx, spec, sp.ID); err != nil {
		return nil, internalError(err)
	}
	return connect.NewResponse(resp), nil
}

// ActivatePlan applies a valid draft to its hosts.
func (s PlanService) ActivatePlan(ctx context.Context, req *connect.Request[portalv1.ActivatePlanRequest]) (*connect.Response[portalv1.ActivatePlanResponse], error) {
	sp, spec, err := s.loadPlan(ctx, req.Msg.Id)
	if err != nil {
		return nil, err
	}
	if sp.State != store.PlanDraft {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("the plan is already active or paused"))
	}
	if spec.Takeover != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("the plan adopts an existing zrepl setup; take it over instead"))
	}
	if err := s.refuseIfFailedOver(ctx, sp.ID); err != nil {
		return nil, err
	}
	if err := s.requireValid(ctx, spec, sp.ID); err != nil {
		return nil, err
	}
	p, err := s.transition(ctx, sp, store.PlanActive, spec, "plan.activate")
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&portalv1.ActivatePlanResponse{Plan: p}), nil
}

// ApplyPlanChanges applies an active or paused plan's pending changes.
func (s PlanService) ApplyPlanChanges(ctx context.Context, req *connect.Request[portalv1.ApplyPlanChangesRequest]) (*connect.Response[portalv1.ApplyPlanChangesResponse], error) {
	sp, spec, err := s.loadPlan(ctx, req.Msg.Id)
	if err != nil {
		return nil, err
	}
	if sp.State == store.PlanDraft {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("activate the plan instead"))
	}
	if err := s.refuseIfFailedOver(ctx, sp.ID); err != nil {
		return nil, err
	}
	if spec.Takeover != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("an existing zrepl setup can only be adopted by a draft plan"))
	}
	if err := s.requireValid(ctx, spec, sp.ID); err != nil {
		return nil, err
	}
	p, err := s.transition(ctx, sp, sp.State, spec, "plan.apply")
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&portalv1.ApplyPlanChangesResponse{Plan: p}), nil
}

// DiscardPlanChanges reverts the editing specification to the applied one.
func (s PlanService) DiscardPlanChanges(ctx context.Context, req *connect.Request[portalv1.DiscardPlanChangesRequest]) (*connect.Response[portalv1.DiscardPlanChangesResponse], error) {
	sp, _, err := s.loadPlan(ctx, req.Msg.Id)
	if err != nil {
		return nil, err
	}
	if sp.AppliedSpec == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("a draft has no applied settings to revert to"))
	}
	applied, err := decodeSpec(sp.AppliedSpec)
	if err != nil {
		return nil, internalError(err)
	}
	p, _, err := s.save(ctx, sp.ID, applied)
	if err != nil {
		return nil, err
	}
	s.audit(ctx, currentUser(ctx).Username, "plan.discard", "plan:"+sp.ID, fmt.Sprintf("plan %q", sp.Name))
	return connect.NewResponse(&portalv1.DiscardPlanChangesResponse{Plan: p}), nil
}

// PausePlan removes an active plan's jobs from its hosts, keeping its data.
func (s PlanService) PausePlan(ctx context.Context, req *connect.Request[portalv1.PausePlanRequest]) (*connect.Response[portalv1.PausePlanResponse], error) {
	sp, _, err := s.loadPlan(ctx, req.Msg.Id)
	if err != nil {
		return nil, err
	}
	if sp.State != store.PlanActive {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("only active plans can be paused"))
	}
	if err := s.refuseIfFailedOver(ctx, sp.ID); err != nil {
		return nil, err
	}
	applied, err := decodeSpec(sp.AppliedSpec)
	if err != nil {
		return nil, internalError(err)
	}
	p, err := s.transition(ctx, sp, store.PlanPaused, applied, "plan.pause")
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&portalv1.PausePlanResponse{Plan: p}), nil
}

// ResumePlan restores a paused plan's jobs on its hosts.
func (s PlanService) ResumePlan(ctx context.Context, req *connect.Request[portalv1.ResumePlanRequest]) (*connect.Response[portalv1.ResumePlanResponse], error) {
	sp, _, err := s.loadPlan(ctx, req.Msg.Id)
	if err != nil {
		return nil, err
	}
	if sp.State != store.PlanPaused {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("only paused plans can be resumed"))
	}
	applied, err := decodeSpec(sp.AppliedSpec)
	if err != nil {
		return nil, internalError(err)
	}
	if err := s.requireValid(ctx, applied, sp.ID); err != nil {
		return nil, err
	}
	p, err := s.transition(ctx, sp, store.PlanActive, applied, "plan.resume")
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&portalv1.ResumePlanResponse{Plan: p}), nil
}

// DeactivatePlan removes the plan's jobs and returns it to draft. Replicas
// and snapshots are kept.
func (s PlanService) DeactivatePlan(ctx context.Context, req *connect.Request[portalv1.DeactivatePlanRequest]) (*connect.Response[portalv1.DeactivatePlanResponse], error) {
	sp, _, err := s.loadPlan(ctx, req.Msg.Id)
	if err != nil {
		return nil, err
	}
	if sp.State == store.PlanDraft {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("the plan is already a draft"))
	}
	if err := s.refuseIfFailedOver(ctx, sp.ID); err != nil {
		return nil, err
	}
	p, err := s.transition(ctx, sp, store.PlanDraft, nil, "plan.deactivate")
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&portalv1.DeactivatePlanResponse{Plan: p}), nil
}

// refuseIfFailedOver refuses changes to a plan that's failing over or failed
// over: its guests run on the DR host, and failback is the way back.
func (s PlanService) refuseIfFailedOver(ctx context.Context, id string) error {
	sp, err := s.Store.PlanByID(ctx, id)
	if err != nil {
		return nil //nolint:nilerr // callers report missing plans themselves
	}
	if sp.State == store.PlanFailedOver {
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("the plan is failed over; fail it back first"))
	}
	if running, err := s.failoverRunning(ctx, id); err != nil {
		return internalError(err)
	} else if running {
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("the plan's failover is running"))
	}
	if running, err := s.failbackRunning(ctx, id); err != nil {
		return internalError(err)
	} else if running {
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("the plan's failback hasn't finished"))
	}
	if deleting, err := s.deletionActive(ctx, id); err != nil {
		return internalError(err)
	} else if deleting {
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("the plan's data is being deleted"))
	}
	return nil
}
