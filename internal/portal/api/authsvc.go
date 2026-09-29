package api

import (
	"context"
	"errors"
	"strings"
	"time"

	"connectrpc.com/connect"

	portalv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/portal/v1"
	"github.com/jlbyh2o/ezdr/internal/portal/auth"
	"github.com/jlbyh2o/ezdr/internal/portal/store"
)

// AuthService signs users in and out.
type AuthService struct{ *Deps }

func userMsg(u store.User) *portalv1.User {
	return &portalv1.User{Id: u.ID, Username: u.Username}
}

// Login checks the password and starts the TOTP step.
func (s AuthService) Login(ctx context.Context, req *connect.Request[portalv1.LoginRequest]) (*connect.Response[portalv1.LoginResponse], error) {
	s.init()
	username := strings.TrimSpace(req.Msg.Username)
	source := SourceAddress(ctx).String()
	failureKey := strings.ToLower(username) + "\x00" + source
	if !s.loginByIP.Allow(source) || !s.loginFailures.Ready(failureKey) {
		return nil, rateLimited()
	}
	if validateUsername(username) != nil {
		// No such user can exist; don't store the name.
		s.loginFailures.Allow(failureKey)
		s.audit(ctx, "", "auth.login_failed", "", "invalid username")
		return nil, connect.NewError(connect.CodeUnauthenticated, errBadCredentials)
	}

	u, err := s.Store.UserByUsername(ctx, username)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, internalError(err)
	}
	if !auth.VerifyPassword(req.Msg.Password, u.PasswordHash) {
		s.loginFailures.Allow(failureKey)
		s.audit(ctx, username, "auth.login_failed", "", "wrong username or password")
		return nil, connect.NewError(connect.CodeUnauthenticated, errBadCredentials)
	}

	if !s.totpFailures.Ready(u.ID) {
		return nil, rateLimited()
	}

	ch := auth.Challenge{UserID: u.ID}
	resp := &portalv1.LoginResponse{}
	if u.TOTPSecret == nil {
		key, err := auth.NewTOTPKey("EZDR ("+s.PublicURL.Hostname()+")", u.Username)
		if err != nil {
			return nil, internalError(err)
		}
		ch.PendingSecret = key.Secret()
		resp.TotpSetup = &portalv1.TotpSetup{Secret: key.Secret(), Url: key.URL()}
	}
	resp.ChallengeId = s.challenges.Create(ch)
	return connect.NewResponse(resp), nil
}

// VerifyTotp completes sign-in with a TOTP or recovery code.
func (s AuthService) VerifyTotp(ctx context.Context, req *connect.Request[portalv1.VerifyTotpRequest]) (*connect.Response[portalv1.VerifyTotpResponse], error) {
	s.init()
	if !s.loginByIP.Allow(SourceAddress(ctx).String()) {
		return nil, rateLimited()
	}
	ch, ok := s.challenges.Attempt(req.Msg.ChallengeId)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, errBadCode)
	}
	if !s.totpFailures.Ready(ch.UserID) {
		return nil, rateLimited()
	}
	u, err := s.Store.UserByID(ctx, ch.UserID)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, errBadCode)
	}

	resp := &portalv1.VerifyTotpResponse{User: userMsg(u)}
	now := time.Now()
	code := req.Msg.Code

	switch {
	case ch.PendingSecret != "":
		// First-time setup: a valid code proves the authenticator app works.
		if !s.replayGuard.Validate(u.ID, ch.PendingSecret, code, now) {
			return nil, s.totpFailed(ctx, u)
		}
		codes, hashes := auth.NewRecoveryCodes()
		// Another sign-in may have set up TOTP since this one started.
		if err := s.Store.SetTOTP(ctx, u.ID, s.Box.Seal([]byte(ch.PendingSecret)), hashes); errors.Is(err, store.ErrNotFound) {
			return nil, connect.NewError(connect.CodeUnauthenticated, errBadCode)
		} else if err != nil {
			return nil, internalError(err)
		}
		resp.RecoveryCodes = codes
		s.audit(ctx, u.Username, "auth.totp_enabled", "user:"+u.ID, "")
	case auth.LooksLikeRecoveryCode(code):
		ok, err := s.Store.UseRecoveryCode(ctx, u.ID, auth.HashRecoveryCode(code))
		if err != nil {
			return nil, internalError(err)
		}
		if !ok {
			return nil, s.totpFailed(ctx, u)
		}
		s.audit(ctx, u.Username, "auth.recovery_code_used", "user:"+u.ID, "")
	default:
		secret, err := s.Box.Open(u.TOTPSecret)
		if err != nil {
			return nil, internalError(err)
		}
		if !s.replayGuard.Validate(u.ID, string(secret), code, now) {
			return nil, s.totpFailed(ctx, u)
		}
	}

	s.challenges.Delete(req.Msg.ChallengeId)
	id, hash := auth.NewSessionID()
	if err := s.Store.CreateSession(ctx, hash, u.ID, now.Add(auth.SessionTTL)); err != nil {
		return nil, internalError(err)
	}
	s.audit(ctx, u.Username, "auth.login", "user:"+u.ID, "")
	res := connect.NewResponse(resp)
	res.Header().Add("Set-Cookie", auth.SessionCookieValue(id, s.SecureCookies))
	return res, nil
}

func (s AuthService) totpFailed(ctx context.Context, u store.User) error {
	s.totpFailures.Allow(u.ID)
	s.audit(ctx, u.Username, "auth.login_failed", "user:"+u.ID, "wrong TOTP or recovery code")
	return connect.NewError(connect.CodeUnauthenticated, errBadCode)
}

// Logout ends the current session.
func (s AuthService) Logout(ctx context.Context, req *connect.Request[portalv1.LogoutRequest]) (*connect.Response[portalv1.LogoutResponse], error) {
	if c := cookieFromHeader(req.Header().Get("Cookie")); c != "" {
		if err := s.Store.DeleteSession(ctx, auth.HashSessionID(c)); err != nil {
			return nil, internalError(err)
		}
	}
	if u, ok := auth.UserFrom(ctx); ok {
		s.audit(ctx, u.Username, "auth.logout", "user:"+u.ID, "")
	}
	res := connect.NewResponse(&portalv1.LogoutResponse{})
	res.Header().Add("Set-Cookie", auth.ClearSessionCookieValue(s.SecureCookies))
	return res, nil
}

// GetCurrentUser returns the signed-in user, or no user when signed out.
// Signed out is a normal state for the UI, so it is not an error.
func (s AuthService) GetCurrentUser(ctx context.Context, _ *connect.Request[portalv1.GetCurrentUserRequest]) (*connect.Response[portalv1.GetCurrentUserResponse], error) {
	resp := &portalv1.GetCurrentUserResponse{}
	if u, ok := auth.UserFrom(ctx); ok {
		resp.User = userMsg(u)
	}
	return connect.NewResponse(resp), nil
}

func cookieFromHeader(header string) string {
	for _, part := range strings.Split(header, ";") {
		name, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if ok && name == auth.SessionCookie {
			return value
		}
	}
	return ""
}
