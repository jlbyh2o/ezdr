package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	clientv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/client/v1"
	inventoryv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/inventory/v1"
	portalv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/portal/v1"
	"github.com/jlbyh2o/ezdr/internal/portal/auth"
	"github.com/jlbyh2o/ezdr/internal/portal/store"
	"github.com/jlbyh2o/ezdr/internal/token"
)

// Token lifetimes.
const (
	DefaultTokenTTL = time.Hour
	MaxTokenTTL     = 7 * 24 * time.Hour
)

// InstallScriptURL is where the one-command installer is published.
const InstallScriptURL = "https://github.com/jlbyh2o/ezdr/releases/latest/download/install.sh"

func currentUser(ctx context.Context) store.User {
	u, _ := auth.UserFrom(ctx)
	return u
}

// TokenService manages enrollment tokens.
type TokenService struct{ *Deps }

func tokenMsg(t store.Token) *portalv1.EnrollmentToken {
	return &portalv1.EnrollmentToken{
		Id: t.ID, Description: t.Description, CreatedBy: t.CreatedBy,
		CreatedAt: ts(t.CreatedAt), ExpiresAt: ts(t.ExpiresAt), UsedAt: ts(t.UsedAt),
		RevokedAt: ts(t.RevokedAt), HostId: t.HostID,
	}
}

// CreateToken creates an enrollment token and the commands that use it.
func (s TokenService) CreateToken(ctx context.Context, req *connect.Request[portalv1.CreateTokenRequest]) (*connect.Response[portalv1.CreateTokenResponse], error) {
	if s.PublicURL.Scheme != "https" {
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			errors.New("enrollment requires HTTPS: set EZDR_PUBLIC_URL to an https:// URL"))
	}
	ttl := DefaultTokenTTL
	if req.Msg.TtlSeconds > 0 {
		ttl = time.Duration(req.Msg.TtlSeconds) * time.Second
	}
	if ttl > MaxTokenTTL {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("token lifetime cannot exceed %s", MaxTokenTTL))
	}
	if len(req.Msg.Description) > 200 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("description is too long"))
	}

	u := currentUser(ctx)
	secret := token.NewSecret()
	t, err := s.Store.CreateToken(ctx, store.Token{
		ID: store.NewID(), SecretHash: token.HashSecret(secret), Description: req.Msg.Description,
		CreatedBy: u.Username, ExpiresAt: time.Now().Add(ttl),
	})
	if err != nil {
		return nil, internalError(err)
	}
	str := token.Token{PortalURL: s.PublicURL.String(), ID: t.ID, Secret: secret, TLSPin: s.TLSPin}.Encode()
	s.audit(ctx, u.Username, "token.create", "token:"+t.ID, fmt.Sprintf("expires %s", t.ExpiresAt.UTC().Format(time.RFC3339)))

	return connect.NewResponse(&portalv1.CreateTokenResponse{
		Token:          tokenMsg(t),
		TokenString:    str,
		InstallCommand: "curl -fsSL " + InstallScriptURL + " | sh -s -- " + str,
		EnrollCommand:  "ezdr enroll " + str,
	}), nil
}

// ListTokens lists enrollment tokens, newest first.
func (s TokenService) ListTokens(ctx context.Context, _ *connect.Request[portalv1.ListTokensRequest]) (*connect.Response[portalv1.ListTokensResponse], error) {
	tokens, err := s.Store.ListTokens(ctx)
	if err != nil {
		return nil, internalError(err)
	}
	resp := &portalv1.ListTokensResponse{}
	for _, t := range tokens {
		resp.Tokens = append(resp.Tokens, tokenMsg(t))
	}
	return connect.NewResponse(resp), nil
}

// RevokeToken revokes an unused token.
func (s TokenService) RevokeToken(ctx context.Context, req *connect.Request[portalv1.RevokeTokenRequest]) (*connect.Response[portalv1.RevokeTokenResponse], error) {
	err := s.Store.RevokeToken(ctx, req.Msg.Id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("no unused token with that ID"))
	}
	if err != nil {
		return nil, internalError(err)
	}
	s.audit(ctx, currentUser(ctx).Username, "token.revoke", "token:"+req.Msg.Id, "")
	return connect.NewResponse(&portalv1.RevokeTokenResponse{}), nil
}

// HostService lists and removes hosts.
type HostService struct{ *Deps }

// ListHosts lists enrolled hosts with their online status.
func (s HostService) ListHosts(ctx context.Context, _ *connect.Request[portalv1.ListHostsRequest]) (*connect.Response[portalv1.ListHostsResponse], error) {
	hosts, err := s.Store.ListHosts(ctx)
	if err != nil {
		return nil, internalError(err)
	}
	resp := &portalv1.ListHostsResponse{}
	for _, h := range hosts {
		resp.Hosts = append(resp.Hosts, s.hostMsg(h))
	}
	return connect.NewResponse(resp), nil
}

func (s HostService) hostMsg(h store.Host) *portalv1.Host {
	return &portalv1.Host{
		Id: h.ID, Hostname: h.Hostname, MachineId: h.MachineID, PveVersion: h.PVEVersion,
		ClientVersion: h.ClientVersion, TunnelAddress: h.TunnelAddress.String(),
		EnrolledAt: ts(h.EnrolledAt), LastSeenAt: ts(h.LastSeenAt),
		Online: s.Hub.Online(h.ID), DuplicateMachineId: h.DuplicateMachineID,
		HasInventory: h.HasInventory, GuestCount: uint32(h.GuestCount), //nolint:gosec // small counts
		GuestsNotReady: uint32(h.GuestsNotReady), //nolint:gosec // small counts
		PlanCount:      uint32(h.PlanCount),      //nolint:gosec // small counts
	}
}

// GetHostInventory returns a host and its latest inventory.
func (s HostService) GetHostInventory(ctx context.Context, req *connect.Request[portalv1.GetHostInventoryRequest]) (*connect.Response[portalv1.GetHostInventoryResponse], error) {
	h, err := s.Store.HostByID(ctx, req.Msg.HostId)
	if errors.Is(err, store.ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("host not found"))
	}
	if err != nil {
		return nil, internalError(err)
	}
	resp := &portalv1.GetHostInventoryResponse{Host: s.hostMsg(h)}
	inv, err := s.Store.HostInventory(ctx, h.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
	case err != nil:
		return nil, internalError(err)
	default:
		resp.Inventory = &inventoryv1.Inventory{}
		if err := proto.Unmarshal(inv.Data, resp.Inventory); err != nil {
			return nil, internalError(err)
		}
		resp.ChangedAt, resp.ReceivedAt = ts(inv.ChangedAt), ts(inv.ReceivedAt)
	}

	excluded, err := s.Store.GuestExclusions(ctx, h.ID)
	if err != nil {
		return nil, internalError(err)
	}
	if len(excluded) > 0 {
		resp.ExcludedGuests = map[uint32]*portalv1.GuestExclusion{}
		for vmid, e := range excluded {
			resp.ExcludedGuests[vmid] = &portalv1.GuestExclusion{By: e.By, At: ts(e.At)}
		}
	}

	guestPlans, err := s.Store.GuestPlans(ctx, h.ID)
	if err != nil {
		return nil, internalError(err)
	}
	if len(guestPlans) > 0 {
		plans, err := s.Store.ListPlans(ctx)
		if err != nil {
			return nil, internalError(err)
		}
		names := map[string]string{}
		for _, p := range plans {
			names[p.ID] = p.Name
		}
		resp.GuestPlans = map[uint32]*portalv1.PlanRef{}
		for vmid, id := range guestPlans {
			resp.GuestPlans[vmid] = &portalv1.PlanRef{Id: id, Name: names[id]}
		}
	}
	return connect.NewResponse(resp), nil
}

// SetGuestExcluded records or clears the choice not to protect a guest.
func (s HostService) SetGuestExcluded(ctx context.Context, req *connect.Request[portalv1.SetGuestExcludedRequest]) (*connect.Response[portalv1.SetGuestExcludedResponse], error) {
	u, _ := auth.UserFrom(ctx)
	m := req.Msg
	if m.Vmid == 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("choose a guest"))
	}
	err := s.Store.SetGuestExcluded(ctx, m.HostId, m.Vmid, m.Excluded, u.Username)
	if errors.Is(err, store.ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("host not found"))
	}
	if err != nil {
		return nil, internalError(err)
	}
	action := "guest.protect"
	if m.Excluded {
		action = "guest.unprotect"
	}
	s.audit(ctx, u.Username, action, fmt.Sprintf("host:%s/guest:%d", m.HostId, m.Vmid), "")
	return connect.NewResponse(&portalv1.SetGuestExcludedResponse{}), nil
}

// RefreshInventory asks an online host to report its inventory now.
func (s HostService) RefreshInventory(ctx context.Context, req *connect.Request[portalv1.RefreshInventoryRequest]) (*connect.Response[portalv1.RefreshInventoryResponse], error) {
	if _, err := s.Store.HostByID(ctx, req.Msg.HostId); errors.Is(err, store.ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("host not found"))
	} else if err != nil {
		return nil, internalError(err)
	}
	action := &clientv1.SubscribeResponse{Message: &clientv1.SubscribeResponse_Action{Action: &clientv1.Action{
		Id:   store.NewID(),
		Kind: &clientv1.Action_RefreshInventory{RefreshInventory: &clientv1.RefreshInventory{}},
	}}}
	if !s.Hub.Send(req.Msg.HostId, action) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("host is offline"))
	}
	return connect.NewResponse(&portalv1.RefreshInventoryResponse{}), nil
}

// DeleteHost removes a host and its WireGuard peer, cutting it off at once.
func (s HostService) DeleteHost(ctx context.Context, req *connect.Request[portalv1.DeleteHostRequest]) (*connect.Response[portalv1.DeleteHostResponse], error) {
	h, err := s.Store.HostByID(ctx, req.Msg.Id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("host not found"))
	}
	if err != nil {
		return nil, internalError(err)
	}
	// Remove the peer first: once it is gone the host can no longer reach
	// the client API, even if the database update below fails.
	if err := s.Peers.RemovePeer(h.WireGuardPublicKey); err != nil {
		return nil, internalError(fmt.Errorf("remove WireGuard peer: %w", err))
	}
	s.Hub.Disconnect(h.ID)
	if err := s.Store.DeleteHost(ctx, h.ID); err != nil {
		return nil, internalError(err)
	}
	s.audit(ctx, currentUser(ctx).Username, "host.delete", "host:"+h.ID,
		fmt.Sprintf("removed %q (%s)", h.Hostname, h.TunnelAddress))
	slog.Info("host removed", "host", h.Hostname, "id", h.ID)
	return connect.NewResponse(&portalv1.DeleteHostResponse{}), nil
}

// AuditService lists audit log entries.
type AuditService struct{ *Deps }

// ListAuditEvents returns recent audit events, newest first.
func (s AuditService) ListAuditEvents(ctx context.Context, req *connect.Request[portalv1.ListAuditEventsRequest]) (*connect.Response[portalv1.ListAuditEventsResponse], error) {
	limit := int(req.Msg.Limit)
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	events, err := s.Store.ListAuditEvents(ctx, limit)
	if err != nil {
		return nil, internalError(err)
	}
	resp := &portalv1.ListAuditEventsResponse{}
	for _, e := range events {
		resp.Events = append(resp.Events, &portalv1.AuditEvent{
			Time: ts(e.Time), Actor: e.Actor, Action: e.Action, Target: e.Target,
			SourceAddress: e.SourceAddress, Detail: e.Detail,
		})
	}
	return connect.NewResponse(resp), nil
}
