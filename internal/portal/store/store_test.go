package store

import (
	"bytes"
	"context"
	"errors"
	"net/netip"
	"path/filepath"
	"testing"
	"time"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "ezdr.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestMigrateIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ezdr.db")
	for range 2 {
		s, err := Open(context.Background(), path)
		if err != nil {
			t.Fatal(err)
		}
		_ = s.Close()
	}
}

func TestCreateFirstUser(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	u, err := s.CreateFirstUser(ctx, "Admin", "hash")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateFirstUser(ctx, "other", "hash"); !errors.Is(err, ErrSetupComplete) {
		t.Fatalf("second CreateFirstUser: err = %v, want ErrSetupComplete", err)
	}
	got, err := s.UserByUsername(ctx, "admin")
	if err != nil {
		t.Fatalf("lookup is not case-insensitive: %v", err)
	}
	if got.ID != u.ID {
		t.Errorf("ID = %q, want %q", got.ID, u.ID)
	}
}

func TestRecoveryCodesAreSingleUse(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	u, _ := s.CreateFirstUser(ctx, "admin", "hash")
	if err := s.SetTOTP(ctx, u.ID, []byte("sealed"), [][]byte{[]byte("a"), []byte("b")}); err != nil {
		t.Fatal(err)
	}
	ok, err := s.UseRecoveryCode(ctx, u.ID, []byte("a"))
	if err != nil || !ok {
		t.Fatalf("first use: ok=%v err=%v", ok, err)
	}
	if ok, _ := s.UseRecoveryCode(ctx, u.ID, []byte("a")); ok {
		t.Error("recovery code was accepted twice")
	}
	if ok, _ := s.UseRecoveryCode(ctx, u.ID, []byte("nope")); ok {
		t.Error("unknown recovery code was accepted")
	}
}

func TestSessionExpiry(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	u, _ := s.CreateFirstUser(ctx, "admin", "hash")
	now := time.Now()
	s.now = func() time.Time { return now }
	if err := s.CreateSession(ctx, []byte("sid"), u.ID, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SessionUser(ctx, []byte("sid")); err != nil {
		t.Fatalf("valid session: %v", err)
	}
	s.now = func() time.Time { return now.Add(2 * time.Hour) }
	if _, err := s.SessionUser(ctx, []byte("sid")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired session: err = %v, want ErrNotFound", err)
	}
}

func seqAlloc(first netip.Addr) AddressAllocator {
	return func(used []netip.Addr) (netip.Addr, error) {
		a := first
		for {
			taken := false
			for _, u := range used {
				if u == a {
					taken = true
				}
			}
			if !taken {
				return a, nil
			}
			a = a.Next()
		}
	}
}

func newHost(id, machineID string) Host {
	return Host{ID: id, Hostname: id, MachineID: machineID, PVEVersion: "pve", ClientVersion: "dev",
		WireGuardPublicKey: bytes.Repeat([]byte(id[:1]), 32)}
}

func TestTokenLifecycle(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now()
	s.now = func() time.Time { return now }
	alloc := seqAlloc(netip.MustParseAddr("100.64.42.2"))

	mk := func(id string, ttl time.Duration) {
		t.Helper()
		if _, err := s.CreateToken(ctx, Token{ID: id, SecretHash: []byte("secret-" + id),
			CreatedBy: "admin", ExpiresAt: now.Add(ttl)}); err != nil {
			t.Fatal(err)
		}
	}
	mk("good", time.Hour)
	mk("expired", -time.Minute)
	mk("revoked", time.Hour)
	if err := s.RevokeToken(ctx, "revoked"); err != nil {
		t.Fatal(err)
	}

	if err := s.CheckToken(ctx, "good", []byte("secret-good")); err != nil {
		t.Errorf("CheckToken(good): %v", err)
	}
	for _, tc := range []struct{ id, secret string }{
		{"good", "wrong"}, {"expired", "secret-expired"}, {"revoked", "secret-revoked"}, {"missing", "x"},
	} {
		if err := s.CheckToken(ctx, tc.id, []byte(tc.secret)); !errors.Is(err, ErrTokenInvalid) {
			t.Errorf("CheckToken(%s): err = %v, want ErrTokenInvalid", tc.id, err)
		}
	}

	h, err := s.EnrollHost(ctx, "good", []byte("secret-good"), newHost("a1", "m1"), alloc)
	if err != nil {
		t.Fatal(err)
	}
	if h.TunnelAddress != netip.MustParseAddr("100.64.42.2") {
		t.Errorf("address = %v", h.TunnelAddress)
	}
	if _, err := s.EnrollHost(ctx, "good", []byte("secret-good"), newHost("b2", "m2"), alloc); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("token reuse: err = %v, want ErrTokenInvalid", err)
	}
	if err := s.RevokeToken(ctx, "good"); !errors.Is(err, ErrNotFound) {
		t.Errorf("revoking a used token: err = %v, want ErrNotFound", err)
	}

	tokens, _ := s.ListTokens(ctx)
	for _, tk := range tokens {
		if tk.ID == "good" && (tk.UsedAt.IsZero() || tk.HostID != "a1") {
			t.Errorf("used token not recorded: %+v", tk)
		}
	}
}

func TestEnrollAllocatesAndFlagsDuplicates(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	alloc := seqAlloc(netip.MustParseAddr("100.64.42.2"))
	for i, id := range []string{"a1", "b2"} {
		tok := "t" + id
		if _, err := s.CreateToken(ctx, Token{ID: tok, SecretHash: []byte("s"), CreatedBy: "admin",
			ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
		h, err := s.EnrollHost(ctx, tok, []byte("s"), newHost(id, "same-machine"), alloc)
		if err != nil {
			t.Fatal(err)
		}
		want := netip.MustParseAddr("100.64.42.2")
		for range i {
			want = want.Next()
		}
		if h.TunnelAddress != want {
			t.Errorf("host %s address = %v, want %v", id, h.TunnelAddress, want)
		}
	}
	hosts, err := s.ListHosts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 2 || !hosts[0].DuplicateMachineID || !hosts[1].DuplicateMachineID {
		t.Fatalf("duplicate machine IDs not flagged: %+v", hosts)
	}
	got, err := s.HostByTunnelAddress(ctx, netip.MustParseAddr("100.64.42.3"))
	if err != nil || got.ID != "b2" {
		t.Fatalf("HostByTunnelAddress: %+v, %v", got, err)
	}
	if err := s.DeleteHost(ctx, "a1"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteHost(ctx, "a1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete: err = %v, want ErrNotFound", err)
	}
}

func TestAuditOrder(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	for _, a := range []string{"first", "second"} {
		if err := s.AddAuditEvent(ctx, AuditEvent{Actor: "admin", Action: a}); err != nil {
			t.Fatal(err)
		}
	}
	events, err := s.ListAuditEvents(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Action != "second" {
		t.Fatalf("events = %+v, want newest first", events)
	}
}

func TestSecretBox(t *testing.T) {
	box, err := NewSecretBox(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	sealed := box.Seal([]byte("hello"))
	got, err := box.Open(sealed)
	if err != nil || string(got) != "hello" {
		t.Fatalf("Open = %q, %v", got, err)
	}
	sealed[len(sealed)-1] ^= 1
	if _, err := box.Open(sealed); err == nil {
		t.Error("tampered value decrypted without error")
	}
	if _, err := NewSecretBox([]byte("short")); err == nil {
		t.Error("short key accepted")
	}
}

func TestSecrets(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if _, err := s.Secret(ctx, "k"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing secret: err = %v", err)
	}
	for _, v := range []string{"v1", "v2"} {
		if err := s.PutSecret(ctx, "k", []byte(v)); err != nil {
			t.Fatal(err)
		}
	}
	if v, _ := s.Secret(ctx, "k"); string(v) != "v2" {
		t.Errorf("secret = %q, want v2", v)
	}
}

func TestInventory(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if _, err := s.CreateToken(ctx, Token{ID: "t", SecretHash: []byte("s"), CreatedBy: "admin",
		ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	h, err := s.EnrollHost(ctx, "t", []byte("s"), newHost("a1", "m1"), seqAlloc(netip.MustParseAddr("100.64.42.2")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.HostInventory(ctx, h.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no inventory yet: err = %v", err)
	}

	now := time.Now()
	s.now = func() time.Time { return now }
	put := func(hash string, guests int) bool {
		t.Helper()
		changed, err := s.PutInventory(ctx, Inventory{HostID: h.ID, Data: []byte("inv-" + hash), Hash: []byte(hash),
			CollectedAt: s.now(), GuestCount: guests, GuestsNotReady: 1})
		if err != nil {
			t.Fatal(err)
		}
		return changed
	}
	if !put("h1", 3) {
		t.Error("first inventory not reported as changed")
	}
	first := now
	now = now.Add(time.Minute)
	if put("h1", 3) {
		t.Error("identical inventory reported as changed")
	}
	inv, err := s.HostInventory(ctx, h.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !inv.ChangedAt.Equal(first.Truncate(time.Millisecond)) || !inv.ReceivedAt.After(inv.ChangedAt) {
		t.Errorf("changed_at = %v, received_at = %v", inv.ChangedAt, inv.ReceivedAt)
	}
	now = now.Add(time.Minute)
	if !put("h2", 4) {
		t.Error("new inventory not reported as changed")
	}

	hosts, _ := s.ListHosts(ctx)
	if !hosts[0].HasInventory || hosts[0].GuestCount != 4 || hosts[0].GuestsNotReady != 1 {
		t.Errorf("host counts = %+v", hosts[0])
	}
	if err := s.DeleteHost(ctx, h.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.HostInventory(ctx, h.ID); !errors.Is(err, ErrNotFound) {
		t.Error("inventory not removed with host")
	}
}
