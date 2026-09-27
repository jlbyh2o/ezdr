package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"connectrpc.com/connect"

	clientv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/client/v1"
	"github.com/jlbyh2o/ezdr/internal/portal/store"
)

// HeartbeatInterval is how often the portal sends heartbeats on the command
// stream.
const HeartbeatInterval = 15 * time.Second

type hostKey struct{}

func hostFrom(ctx context.Context) store.Host {
	h, _ := ctx.Value(hostKey{}).(store.Host)
	return h
}

// WithTunnelHost identifies the calling host by its tunnel source address.
// It must only wrap handlers served inside the WireGuard tunnel, where
// WireGuard guarantees the source address belongs to the peer's key.
func WithTunnelHost(s *store.Store, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h, err := s.HostByTunnelAddress(r.Context(), remoteAddr(r))
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "unknown host", http.StatusForbidden)
			return
		}
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), hostKey{}, h)))
	})
}

// ClientService is the API used by enrolled hosts.
type ClientService struct{ *Deps }

// Subscribe streams desired state and heartbeats to a host.
func (s ClientService) Subscribe(ctx context.Context, req *connect.Request[clientv1.SubscribeRequest], stream *connect.ServerStream[clientv1.SubscribeResponse]) error {
	h := hostFrom(ctx)
	ctx, release := s.Hub.connect(ctx, h.ID)
	defer release()
	slog.Info("host connected", "host", h.Hostname, "id", h.ID)
	defer slog.Info("host disconnected", "host", h.Hostname, "id", h.ID)

	touch := func() {
		if err := s.Store.TouchHost(ctx, h.ID, req.Msg.ClientVersion); err != nil && ctx.Err() == nil {
			slog.Error("update host last seen", "host", h.ID, "err", err)
		}
	}
	touch()

	// Phase 1 has no configuration to apply yet; later phases fill this in.
	desired := &clientv1.SubscribeResponse{Message: &clientv1.SubscribeResponse_DesiredState{
		DesiredState: &clientv1.DesiredState{Generation: 1},
	}}
	if err := stream.Send(desired); err != nil {
		return err
	}

	ticker := time.NewTicker(HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			heartbeat := &clientv1.SubscribeResponse{Message: &clientv1.SubscribeResponse_Heartbeat{
				Heartbeat: &clientv1.Heartbeat{},
			}}
			if err := stream.Send(heartbeat); err != nil {
				return err
			}
			touch()
		}
	}
}

// ReportStatus records a host's status.
func (s ClientService) ReportStatus(ctx context.Context, req *connect.Request[clientv1.ReportStatusRequest]) (*connect.Response[clientv1.ReportStatusResponse], error) {
	if err := s.Store.TouchHost(ctx, hostFrom(ctx).ID, req.Msg.ClientVersion); err != nil {
		return nil, internalError(err)
	}
	return connect.NewResponse(&clientv1.ReportStatusResponse{}), nil
}
