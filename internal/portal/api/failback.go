package api

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"
	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/types/known/timestamppb"

	clientv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/client/v1"
	planv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/plan/v1"
	portalv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/portal/v1"
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
