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
	return plan.Validate(spec, plan.Context{Primary: primary, DR: dr, OtherPlans: others, Now: time.Now()}), nil
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

func planMsg(p store.Plan) (*portalv1.Plan, error) {
	spec := &planv1.PlanSpec{}
	if err := proto.Unmarshal(p.Spec, spec); err != nil {
		return nil, err
	}
	return &portalv1.Plan{Id: p.ID, Spec: spec, CreatedBy: p.CreatedBy,
		CreatedAt: ts(p.CreatedAt), UpdatedAt: ts(p.UpdatedAt)}, nil
}

// save stores spec as plan id ("" creates a new plan) and returns the stored
// plan with its validation issues.
func (s PlanService) save(ctx context.Context, id string, spec *planv1.PlanSpec) (*portalv1.Plan, []*planv1.Issue, error) {
	if err := checkSpec(spec); err != nil {
		return nil, nil, err
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
	msg, err := planMsg(sp)
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
	p, err := planMsg(sp)
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
		p, err := planMsg(sp)
		if err != nil {
			return nil, internalError(err)
		}
		issues, err := s.validate(ctx, p.Spec, p.Id)
		if err != nil {
			return nil, internalError(err)
		}
		errs, warns := plan.Counts(issues)
		resp.Plans = append(resp.Plans, &portalv1.PlanSummary{
			Id: p.Id, Name: p.Spec.Name,
			PrimaryHostname: names[p.Spec.PrimaryHostId], DrHostname: names[p.Spec.DrHostId],
			GuestCount:      uint32(len(p.Spec.Guests)), //nolint:gosec // small counts
			IntervalSeconds: p.Spec.IntervalSeconds,
			ErrorCount:      uint32(errs),  //nolint:gosec // small counts
			WarningCount:    uint32(warns), //nolint:gosec // small counts
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
