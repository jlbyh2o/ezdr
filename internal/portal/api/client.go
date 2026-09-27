package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"google.golang.org/protobuf/proto"

	"connectrpc.com/connect"

	clientv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/client/v1"
	"github.com/jlbyh2o/ezdr/internal/inventory"
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
	ctx, outbox, release := s.Hub.connect(ctx, h.ID)
	defer release()
	slog.Info("host connected", "host", h.Hostname, "id", h.ID)
	defer slog.Info("host disconnected", "host", h.Hostname, "id", h.ID)

	touch := func() {
		if err := s.Store.TouchHost(ctx, h.ID, req.Msg.ClientVersion); err != nil && ctx.Err() == nil {
			slog.Error("update host last seen", "host", h.ID, "err", err)
		}
	}
	touch()

	ds, err := s.desiredState(ctx, h.ID)
	if err != nil {
		return internalError(err)
	}
	desired := &clientv1.SubscribeResponse{Message: &clientv1.SubscribeResponse_DesiredState{DesiredState: ds}}
	if err := stream.Send(desired); err != nil {
		return err
	}

	ticker := time.NewTicker(HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case msg := <-outbox:
			if err := stream.Send(msg); err != nil {
				return err
			}
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

// ReportStatus records a host's status: what it applied and its zrepl
// certificate. A new certificate is passed on to the host's peers.
func (s ClientService) ReportStatus(ctx context.Context, req *connect.Request[clientv1.ReportStatusRequest]) (*connect.Response[clientv1.ReportStatusResponse], error) {
	h := hostFrom(ctx)
	m := req.Msg
	if err := s.Store.TouchHost(ctx, h.ID, m.ClientVersion); err != nil {
		return nil, internalError(err)
	}
	if len(m.ZreplCertificate) > 16<<10 || len(m.ApplyError) > 4<<10 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("status fields are too large"))
	}
	if err := s.Store.SetHostApplied(ctx, h.ID, m.AppliedGeneration, m.ApplyError); err != nil {
		return nil, internalError(err)
	}
	if m.ApplyError != "" {
		slog.Warn("host failed to apply configuration", "host", h.Hostname, "generation", m.AppliedGeneration, "err", m.ApplyError)
	}
	if len(m.SitePublicKey) == 32 {
		changed, err := s.Store.SetSitePublicKey(ctx, h.ID, m.SitePublicKey)
		if err != nil {
			return nil, internalError(err)
		}
		if changed {
			slog.Info("host site tunnel key updated", "host", h.Hostname)
			s.reconcile(context.WithoutCancel(ctx), s.relatedHosts(ctx, h.ID)...)
		}
	}
	if m.ZreplCertificate != "" {
		changed, err := s.Store.SetHostZrepl(ctx, h.ID, m.ZreplCertificate, m.ZreplVersion)
		if err != nil {
			return nil, internalError(err)
		}
		if changed {
			slog.Info("host zrepl certificate updated", "host", h.Hostname)
			s.reconcile(context.WithoutCancel(ctx), s.relatedHosts(ctx, h.ID)...)
		}
	}
	return connect.NewResponse(&clientv1.ReportStatusResponse{}), nil
}

// ReportInventory stores a host's inventory.
func (s ClientService) ReportInventory(ctx context.Context, req *connect.Request[clientv1.ReportInventoryRequest]) (*connect.Response[clientv1.ReportInventoryResponse], error) {
	h := hostFrom(ctx)
	inv := req.Msg.GetInventory()
	if inv == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("inventory is required"))
	}
	data, err := proto.Marshal(inv)
	if err != nil {
		return nil, internalError(err)
	}
	collected := time.Now()
	if inv.GetCollectedAt() != nil {
		collected = inv.GetCollectedAt().AsTime()
	}
	changed, err := s.Store.PutInventory(ctx, store.Inventory{
		HostID: h.ID, Data: data, Hash: inventory.Hash(inv), CollectedAt: collected,
		GuestCount: len(inv.GetGuests()), GuestsNotReady: inventory.NotReady(inv),
	})
	if err != nil {
		return nil, internalError(err)
	}
	if changed {
		slog.Info("inventory updated", "host", h.Hostname, "guests", len(inv.GetGuests()),
			"not_ready", inventory.NotReady(inv), "warnings", len(inv.GetWarnings()))
		// A primary's disks decide which datasets its plans replicate.
		s.reconcile(context.WithoutCancel(ctx), s.relatedHosts(ctx, h.ID)...)
	}
	return connect.NewResponse(&clientv1.ReportInventoryResponse{}), nil
}

// AckAction records the outcome of an action.
func (s ClientService) AckAction(ctx context.Context, req *connect.Request[clientv1.AckActionRequest]) (*connect.Response[clientv1.AckActionResponse], error) {
	h := hostFrom(ctx)
	m := req.Msg
	if m.Succeeded {
		slog.Info("action completed", "host", h.Hostname, "action", m.ActionId)
	} else {
		slog.Warn("action failed", "host", h.Hostname, "action", m.ActionId, "message", m.Message)
	}
	s.Hub.deliver(h.ID, m)
	return connect.NewResponse(&clientv1.AckActionResponse{}), nil
}
