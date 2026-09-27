package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"strings"
	"time"

	"github.com/jlbyh2o/ezdr/internal/portal/store"
)

// SessionCookie is the name of the session cookie.
const SessionCookie = "ezdr_session"

// SessionTTL is the absolute lifetime of a session.
const SessionTTL = 12 * time.Hour

// NewSessionID returns a random session ID and the hash stored in the
// database.
func NewSessionID() (id string, hash []byte) {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	id = base64.RawURLEncoding.EncodeToString(b)
	return id, HashSessionID(id)
}

// HashSessionID hashes a session ID for storage and lookup.
func HashSessionID(id string) []byte {
	h := sha256.Sum256([]byte(id))
	return h[:]
}

// SessionCookieValue builds the Set-Cookie value for a session. secure is
// false only when the portal is served over plain HTTP for local development.
func SessionCookieValue(id string, secure bool) string {
	c := &http.Cookie{ //nolint:gosec // Secure is set from configuration
		Name: SessionCookie, Value: id, Path: "/", MaxAge: int(SessionTTL.Seconds()),
		HttpOnly: true, Secure: secure, SameSite: http.SameSiteStrictMode,
	}
	return c.String()
}

// ClearSessionCookieValue builds a Set-Cookie value that removes the session.
func ClearSessionCookieValue(secure bool) string {
	c := &http.Cookie{ //nolint:gosec // Secure is set from configuration
		Name: SessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: secure, SameSite: http.SameSiteStrictMode,
	}
	return c.String()
}

type userKey struct{}

// WithUser returns a context carrying the signed-in user.
func WithUser(ctx context.Context, u store.User) context.Context {
	return context.WithValue(ctx, userKey{}, u)
}

// UserFrom returns the signed-in user, if any.
func UserFrom(ctx context.Context) (store.User, bool) {
	u, ok := ctx.Value(userKey{}).(store.User)
	return u, ok
}

// NewSetupCodeString returns a random, easy-to-type setup code such as
// "abcd-efgh-ijkl-mnop".
func NewSetupCodeString() string {
	raw := strings.ToLower(rand.Text()[:16])
	return raw[0:4] + "-" + raw[4:8] + "-" + raw[8:12] + "-" + raw[12:16]
}
