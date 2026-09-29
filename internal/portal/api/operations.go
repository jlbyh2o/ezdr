package api

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	portalv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/portal/v1"
	"github.com/jlbyh2o/ezdr/internal/portal/store"
)

// loadOperation decodes one stored failover or failback into msg. It must
// belong to planID, when given.
func loadOperation(ctx context.Context, byID func(context.Context, string) (store.FailoverRow, error), id, planID string, msg proto.Message) error {
	row, err := byID(ctx, id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && planID != "" && row.PlanID != planID) {
		return connect.NewError(connect.CodeNotFound, errors.New("not found"))
	}
	if err != nil {
		return internalError(err)
	}
	if err := proto.Unmarshal(row.Data, msg); err != nil {
		return internalError(err)
	}
	return nil
}

// ListFailovers returns a plan's failovers, newest first.
func (s FailoverService) ListFailovers(ctx context.Context, req *connect.Request[portalv1.ListFailoversRequest]) (*connect.Response[portalv1.ListFailoversResponse], error) {
	rows, err := s.Store.Failovers(ctx, req.Msg.PlanId)
	if err != nil {
		return nil, internalError(err)
	}
	resp := &portalv1.ListFailoversResponse{}
	for _, row := range rows {
		f := &portalv1.Failover{}
		if err := proto.Unmarshal(row.Data, f); err != nil {
			return nil, internalError(err)
		}
		resp.Failovers = append(resp.Failovers, f)
	}
	return connect.NewResponse(resp), nil
}

// ListFailbacks returns a plan's failbacks, newest first.
func (s FailoverService) ListFailbacks(ctx context.Context, req *connect.Request[portalv1.ListFailbacksRequest]) (*connect.Response[portalv1.ListFailbacksResponse], error) {
	rows, err := s.Store.Failbacks(ctx, req.Msg.PlanId)
	if err != nil {
		return nil, internalError(err)
	}
	resp := &portalv1.ListFailbacksResponse{}
	for _, row := range rows {
		fb := &portalv1.Failback{}
		if err := proto.Unmarshal(row.Data, fb); err != nil {
			return nil, internalError(err)
		}
		resp.Failbacks = append(resp.Failbacks, fb)
	}
	return connect.NewResponse(resp), nil
}
