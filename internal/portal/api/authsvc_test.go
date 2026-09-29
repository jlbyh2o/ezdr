package api

import (
	"context"
	"net/netip"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"

	portalv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/portal/v1"
	"github.com/jlbyh2o/ezdr/internal/portal/auth"
	"github.com/jlbyh2o/ezdr/internal/portal/store"
)

func TestLoginLimits(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "ezdr.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.CreateFirstUser(ctx, "admin", auth.HashPassword("a long enough password")); err != nil {
		t.Fatal(err)
	}
	publicURL, _ := url.Parse("https://portal.example.com")
	d := &Deps{Store: st, PublicURL: publicURL}
	s := AuthService{Deps: d}
	from := func(addr string) context.Context {
		return context.WithValue(ctx, sourceKey{}, netip.MustParseAddr(addr))
	}
	login := func(addr, username, password string) error {
		_, err := s.Login(from(addr), connect.NewRequest(&portalv1.LoginRequest{Username: username, Password: password}))
		return err
	}

	// Failed passwords from one address are limited there...
	var limited bool
	for range 8 {
		if connect.CodeOf(login("192.0.2.66", "admin", "wrong")) == connect.CodeResourceExhausted {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("repeated failures weren't limited")
	}
	// ...but don't lock the user out elsewhere.
	if err := login("198.51.100.7", "admin", "a long enough password"); err != nil {
		t.Fatalf("sign-in from another address: %v", err)
	}

	// A name no user can have isn't looked up or stored.
	long := strings.Repeat("x", 10000)
	if connect.CodeOf(login("203.0.113.9", long, "wrong")) != connect.CodeUnauthenticated {
		t.Error("invalid username not rejected")
	}
	events, err := d.Store.ListAuditEvents(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if strings.Contains(e.Actor, long) || strings.Contains(e.Detail, long) {
			t.Error("invalid username stored in the audit log")
		}
	}
}
