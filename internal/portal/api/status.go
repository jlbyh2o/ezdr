package api

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	clientv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/client/v1"
	portalv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/portal/v1"
	"github.com/jlbyh2o/ezdr/internal/portal/store"
	"github.com/jlbyh2o/ezdr/internal/replication"
)

// ReportReplication stores a host's replication status.
func (s ClientService) ReportReplication(ctx context.Context, req *connect.Request[clientv1.ReportReplicationRequest]) (*connect.Response[clientv1.ReportReplicationResponse], error) {
	b, err := proto.Marshal(req.Msg)
	if err != nil {
		return nil, internalError(err)
	}
	if len(b) > 4<<20 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("status is too large"))
	}
	if err := s.Store.PutReplicationStatus(ctx, hostFrom(ctx).ID, b); err != nil {
		return nil, internalError(err)
	}
	return connect.NewResponse(&clientv1.ReportReplicationResponse{}), nil
}

// planHealth computes the health of a plan from its applied specification
// and the DR host's latest status. Drafts and paused plans have no health.
func (d *Deps) planHealth(ctx context.Context, p store.Plan) (*portalv1.GetPlanStatusResponse, error) {
	resp := &portalv1.GetPlanStatusResponse{Health: &portalv1.PlanHealth{State: portalv1.HealthState_HEALTH_STATE_NONE}}
	if p.State != store.PlanActive || p.AppliedSpec == nil {
		if p.State == store.PlanPaused {
			resp.Health.Message = "paused"
		}
		return resp, nil
	}
	spec, err := decodeSpec(p.AppliedSpec)
	if err != nil {
		return nil, err
	}
	rp := replication.Plan{ID: p.ID, Name: p.Name, Spec: spec}
	hosts, err := d.replicationHosts(ctx, []replication.Plan{rp})
	if err != nil {
		return nil, err
	}
	in := replication.HealthInput{
		Plan: rp, Primary: hosts[spec.PrimaryHostId], DR: hosts[spec.DrHostId],
		PrimaryOnline: d.Hub.Online(spec.PrimaryHostId), DROnline: d.Hub.Online(spec.DrHostId), Now: time.Now(),
	}
	b, at, err := d.Store.ReplicationStatus(ctx, spec.DrHostId)
	switch {
	case errors.Is(err, store.ErrNotFound):
	case err != nil:
		return nil, err
	default:
		st := &clientv1.ReportReplicationRequest{}
		if err := proto.Unmarshal(b, st); err != nil {
			return nil, err
		}
		in.DRStatus, in.DRStatusReceivedAt = st, at
	}
	resp.Health, resp.Datasets, resp.Errors = replication.Health(in)
	return resp, nil
}

// GetPlanStatus returns an active plan's replication health.
func (s PlanService) GetPlanStatus(ctx context.Context, req *connect.Request[portalv1.GetPlanStatusRequest]) (*connect.Response[portalv1.GetPlanStatusResponse], error) {
	sp, _, err := s.loadPlan(ctx, req.Msg.Id)
	if err != nil {
		return nil, err
	}
	resp, err := s.planHealth(ctx, sp)
	if err != nil {
		slog.Error("plan health", "plan", sp.ID, "err", err)
		return nil, internalError(err)
	}
	return connect.NewResponse(resp), nil
}
