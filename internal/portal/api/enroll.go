package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"regexp"

	"connectrpc.com/connect"

	enrollv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/enroll/v1"
	"github.com/jlbyh2o/ezdr/internal/portal/store"
	"github.com/jlbyh2o/ezdr/internal/token"
)

// persistentKeepalive keeps NAT mappings open between client and portal.
const persistentKeepalive = 25

var (
	errInvalidToken = errors.New("invalid, expired, revoked, or already used enrollment token")
	errNoAddresses  = errors.New("no free tunnel addresses; configure a larger EZDR_TUNNEL_PREFIX")
)

// EnrollmentService is the public enrollment API.
type EnrollmentService struct{ *Deps }

// CheckToken validates a token without using it.
func (s EnrollmentService) CheckToken(ctx context.Context, req *connect.Request[enrollv1.CheckTokenRequest]) (*connect.Response[enrollv1.CheckTokenResponse], error) {
	s.init()
	if !s.enrollByIP.Allow(SourceAddress(ctx).String()) {
		return nil, rateLimited()
	}
	err := s.Store.CheckToken(ctx, req.Msg.TokenId, token.HashSecret(req.Msg.Secret))
	if errors.Is(err, store.ErrTokenInvalid) {
		return nil, connect.NewError(connect.CodePermissionDenied, errInvalidToken)
	}
	if err != nil {
		return nil, internalError(err)
	}
	return connect.NewResponse(&enrollv1.CheckTokenResponse{
		TunnelPrefix:      s.TunnelPrefix.String(),
		WireguardEndpoint: s.WireGuardEndpoint,
	}), nil
}

func validateFacts(f *enrollv1.HostFacts) error {
	if f == nil || f.Hostname == "" || f.MachineId == "" {
		return errors.New("hostname and machine ID are required")
	}
	for name, v := range map[string]string{
		"hostname": f.Hostname, "machine ID": f.MachineId,
		"Proxmox VE version": f.PveVersion, "client version": f.ClientVersion,
	} {
		if len(v) > 255 {
			return fmt.Errorf("%s is too long", name)
		}
	}
	// The hostname appears in guest configurations and zrepl settings on
	// other hosts.
	if !hostnamePattern.MatchString(f.Hostname) {
		return fmt.Errorf("hostname %q isn't a valid host name", f.Hostname)
	}
	return nil
}

var hostnamePattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*$`)

// Enroll uses a token and registers the host.
func (s EnrollmentService) Enroll(ctx context.Context, req *connect.Request[enrollv1.EnrollRequest]) (*connect.Response[enrollv1.EnrollResponse], error) {
	s.init()
	if !s.enrollByIP.Allow(SourceAddress(ctx).String()) {
		return nil, rateLimited()
	}
	m := req.Msg
	if len(m.WireguardPublicKey) != 32 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("WireGuard public key must be 32 bytes"))
	}
	if err := validateFacts(m.Facts); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}

	h := store.Host{
		ID:                 store.NewID(),
		Hostname:           m.Facts.Hostname,
		MachineID:          m.Facts.MachineId,
		PVEVersion:         m.Facts.PveVersion,
		ClientVersion:      m.Facts.ClientVersion,
		WireGuardPublicKey: m.WireguardPublicKey,
	}
	h, err := s.Store.EnrollHost(ctx, m.TokenId, token.HashSecret(m.Secret), h,
		addressAllocator(s.TunnelPrefix, s.PortalTunnelAddr))
	switch {
	case errors.Is(err, store.ErrTokenInvalid):
		s.audit(ctx, "", "host.enroll_failed", "token:"+m.TokenId, "invalid token")
		return nil, connect.NewError(connect.CodePermissionDenied, errInvalidToken)
	case errors.Is(err, store.ErrConflict):
		return nil, connect.NewError(connect.CodeAlreadyExists, errors.New("this WireGuard key is already enrolled"))
	case errors.Is(err, errNoAddresses):
		return nil, connect.NewError(connect.CodeResourceExhausted, err)
	case err != nil:
		return nil, internalError(err)
	}

	if err := s.Peers.AddPeer(h.WireGuardPublicKey, h.TunnelAddress); err != nil {
		if derr := s.Store.DeleteHost(ctx, h.ID); derr != nil {
			slog.Error("remove host after failed peer setup", "host", h.ID, "err", derr)
		}
		return nil, internalError(fmt.Errorf("add WireGuard peer: %w", err))
	}
	s.audit(ctx, "", "host.enroll", "host:"+h.ID,
		fmt.Sprintf("enrolled %q as %s with token %s", h.Hostname, h.TunnelAddress, m.TokenId))

	return connect.NewResponse(&enrollv1.EnrollResponse{
		HostId:                     h.ID,
		TunnelAddress:              h.TunnelAddress.String(),
		TunnelPrefix:               s.TunnelPrefix.String(),
		PortalTunnelAddress:        s.PortalTunnelAddr.String(),
		PortalWireguardPublicKey:   s.Peers.PublicKey(),
		WireguardEndpoint:          s.WireGuardEndpoint,
		ClientApiUrl:               s.ClientAPIURL,
		PersistentKeepaliveSeconds: persistentKeepalive,
	}), nil
}

// addressAllocator returns the first free host address in prefix, skipping
// the network address, the portal's address, and the broadcast address.
func addressAllocator(prefix netip.Prefix, portal netip.Addr) store.AddressAllocator {
	return func(used []netip.Addr) (netip.Addr, error) {
		taken := map[netip.Addr]bool{portal: true}
		for _, u := range used {
			taken[u] = true
		}
		for a := prefix.Addr().Next(); prefix.Contains(a); a = a.Next() {
			if !prefix.Contains(a.Next()) {
				break // broadcast address
			}
			if !taken[a] {
				return a, nil
			}
		}
		return netip.Addr{}, errNoAddresses
	}
}
