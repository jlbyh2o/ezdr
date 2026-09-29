// Package api implements the portal's ConnectRPC services.
package api

import (
	"context"
	"crypto/subtle"
	"log/slog"
	"net/netip"
	"net/url"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jlbyh2o/ezdr/internal/portal/auth"
	"github.com/jlbyh2o/ezdr/internal/portal/dns"
	"github.com/jlbyh2o/ezdr/internal/portal/store"
)

// PeerManager adds and removes WireGuard peers on the portal's tunnel.
type PeerManager interface {
	PublicKey() []byte
	AddPeer(publicKey []byte, addr netip.Addr) error
	RemovePeer(publicKey []byte) error
}

// Deps holds everything the services need.
type Deps struct {
	Store *store.Store
	Box   *store.SecretBox
	Peers PeerManager
	Hub   *Hub

	PublicURL         *url.URL
	TLSPin            string
	SecureCookies     bool
	WireGuardEndpoint string
	TunnelPrefix      netip.Prefix
	SiteTunnelPrefix  netip.Prefix
	PortalTunnelAddr  netip.Addr
	ClientAPIURL      string

	Setup *SetupCode

	// NewDNSProvider makes the DNS provider from its token; tests replace it.
	// Nil means Cloudflare.
	NewDNSProvider func(token string) dns.Provider

	challenges  *auth.Challenges
	replayGuard *auth.ReplayGuard
	// Rate limits for unauthenticated endpoints.
	loginByIP *auth.RateLimiter
	// Failed passwords per username and source address: failures from one
	// address can't lock the user out elsewhere.
	loginFailures *auth.RateLimiter
	// Failed TOTP or recovery codes per user (only possible with the
	// password).
	totpFailures  *auth.RateLimiter
	enrollByIP    *auth.RateLimiter
	setupByIP     *auth.RateLimiter
	initLimitOnce sync.Once
}

func (d *Deps) init() {
	d.initLimitOnce.Do(func() {
		d.challenges = auth.NewChallenges()
		d.replayGuard = auth.NewReplayGuard()
		d.loginByIP = auth.NewRateLimiter(6*time.Second, 10)
		d.loginFailures = auth.NewRateLimiter(time.Minute, 5)
		d.totpFailures = auth.NewRateLimiter(5*time.Minute, 10)
		d.enrollByIP = auth.NewRateLimiter(6*time.Second, 10)
		d.setupByIP = auth.NewRateLimiter(time.Minute, 5)
	})
}

// audit records an audit event, logging (not failing) on error.
func (d *Deps) audit(ctx context.Context, actor, action, target, detail string) {
	e := store.AuditEvent{
		Actor: actor, Action: action, Target: target,
		SourceAddress: SourceAddress(ctx).String(), Detail: detail,
	}
	if err := d.Store.AddAuditEvent(ctx, e); err != nil {
		slog.Error("write audit event", "action", action, "err", err)
	}
}

// SetupCode is the one-time code required to create the first administrator.
type SetupCode struct {
	mu   sync.Mutex
	code string
}

// NewSetupCode returns a setup code holder with a fresh random code.
func NewSetupCode() *SetupCode {
	return &SetupCode{code: auth.NewSetupCodeString()}
}

// String returns the current code, or "" once it has been used.
func (s *SetupCode) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.code
}

func (s *SetupCode) matches(code string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.code != "" && subtle.ConstantTimeCompare([]byte(s.code), []byte(code)) == 1
}

func (s *SetupCode) invalidate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.code = ""
}

func ts(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}

func internalError(err error) error {
	slog.Error("internal error", "err", err)
	return connect.NewError(connect.CodeInternal, nil)
}

func rateLimited() error {
	return connect.NewError(connect.CodeResourceExhausted, errTooManyAttempts)
}
