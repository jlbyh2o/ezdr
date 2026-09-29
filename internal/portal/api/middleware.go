package api

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"

	"connectrpc.com/connect"

	"github.com/jlbyh2o/ezdr/internal/portal/auth"
	"github.com/jlbyh2o/ezdr/internal/portal/store"
)

type sourceKey struct{}

// SourceAddress returns the request's client address as determined by
// WithSourceAddress.
func SourceAddress(ctx context.Context) netip.Addr {
	a, _ := ctx.Value(sourceKey{}).(netip.Addr)
	return a
}

func remoteAddr(r *http.Request) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	a, _ := netip.ParseAddr(host)
	return a.Unmap()
}

// WithSourceAddress records the client's address. X-Forwarded-For is honored
// only when the direct peer is a trusted proxy, and then the rightmost
// untrusted address in the header is used.
func WithSourceAddress(trusted []netip.Prefix, next http.Handler) http.Handler {
	isTrusted := func(a netip.Addr) bool {
		for _, p := range trusted {
			if p.Contains(a) {
				return true
			}
		}
		return false
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		addr := remoteAddr(r)
		if isTrusted(addr) {
			hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
			for i := len(hops) - 1; i >= 0; i-- {
				hop, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
				if err != nil {
					break
				}
				addr = hop.Unmap()
				if !isTrusted(addr) {
					break
				}
			}
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), sourceKey{}, addr)))
	})
}

// WithSession loads the signed-in user from the session cookie, if any.
func WithSession(s *store.Store, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(auth.SessionCookie); err == nil && c.Value != "" {
			u, err := s.SessionUser(r.Context(), auth.HashSessionID(c.Value))
			if err == nil {
				r = r.WithContext(auth.WithUser(r.Context(), u))
			} else if !errors.Is(err, store.ErrNotFound) {
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// WithSameOrigin rejects cross-origin requests. Browsers send Origin on
// cross-site POST requests; together with SameSite=Strict cookies and
// Connect's content-type requirements this prevents cross-site request
// forgery.
func WithSameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" {
			u, err := url.Parse(origin)
			if err != nil || !strings.EqualFold(u.Host, r.Host) {
				http.Error(w, "cross-origin request rejected", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// RequireUser is a Connect interceptor that rejects calls without a signed-in
// user, except for the listed public procedures.
func RequireUser(public ...string) connect.Interceptor {
	allowed := make(map[string]bool, len(public))
	for _, p := range public {
		allowed[p] = true
	}
	return requireUser{allowed: allowed}
}

type requireUser struct{ allowed map[string]bool }

func (r requireUser) check(ctx context.Context, procedure string) error {
	if _, ok := auth.UserFrom(ctx); !ok && !r.allowed[procedure] {
		return connect.NewError(connect.CodeUnauthenticated, errNotSignedIn)
	}
	return nil
}

func (r requireUser) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if err := r.check(ctx, req.Spec().Procedure); err != nil {
			return nil, err
		}
		return next(ctx, req)
	}
}

func (r requireUser) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (r requireUser) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		if err := r.check(ctx, conn.Spec().Procedure); err != nil {
			return err
		}
		return next(ctx, conn)
	}
}
