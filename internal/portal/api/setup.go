package api

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"connectrpc.com/connect"

	portalv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/portal/v1"
	"github.com/jlbyh2o/ezdr/internal/portal/auth"
	"github.com/jlbyh2o/ezdr/internal/portal/store"
)

var usernamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

func validateUsername(u string) error {
	if !usernamePattern.MatchString(u) {
		return errors.New("username must be 1-64 characters: letters, digits, '.', '_', or '-'")
	}
	return nil
}

// SetupService creates the first administrator.
type SetupService struct{ *Deps }

// GetSetupStatus reports whether the portal still needs its first user.
func (s SetupService) GetSetupStatus(ctx context.Context, _ *connect.Request[portalv1.GetSetupStatusRequest]) (*connect.Response[portalv1.GetSetupStatusResponse], error) {
	n, err := s.Store.CountUsers(ctx)
	if err != nil {
		return nil, internalError(err)
	}
	return connect.NewResponse(&portalv1.GetSetupStatusResponse{SetupRequired: n == 0}), nil
}

// CompleteSetup creates the first administrator, given the setup code.
func (s SetupService) CompleteSetup(ctx context.Context, req *connect.Request[portalv1.CompleteSetupRequest]) (*connect.Response[portalv1.CompleteSetupResponse], error) {
	s.init()
	if !s.setupByIP.Allow(SourceAddress(ctx).String()) {
		return nil, rateLimited()
	}
	m := req.Msg
	if !s.Setup.matches(strings.TrimSpace(strings.ToLower(m.SetupCode))) {
		s.audit(ctx, "", "setup.failed", "", "invalid setup code")
		return nil, connect.NewError(connect.CodePermissionDenied, errBadCode)
	}
	if err := validateUsername(m.Username); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if err := auth.ValidatePassword(m.Password); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	u, err := s.Store.CreateFirstUser(ctx, m.Username, auth.HashPassword(m.Password))
	if errors.Is(err, store.ErrSetupComplete) {
		s.Setup.invalidate()
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}
	if err != nil {
		return nil, internalError(err)
	}
	s.Setup.invalidate()
	s.audit(ctx, u.Username, "setup.complete", "user:"+u.ID, fmt.Sprintf("created administrator %q", u.Username))
	return connect.NewResponse(&portalv1.CompleteSetupResponse{}), nil
}
