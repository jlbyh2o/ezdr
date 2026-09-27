package api

import (
	"context"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	clientv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/client/v1"
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
