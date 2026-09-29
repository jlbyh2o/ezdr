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

func TestPlans(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	alloc := seqAlloc(netip.MustParseAddr("100.64.42.2"))
	var hosts []Host
	for _, id := range []string{"p1", "d1"} {
		if _, err := s.CreateToken(ctx, Token{ID: "t" + id, SecretHash: []byte("s"), CreatedBy: "admin",
			ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
		h, err := s.EnrollHost(ctx, "t"+id, []byte("s"), newHost(id, "m"+id), alloc)
		if err != nil {
			t.Fatal(err)
		}
		hosts = append(hosts, h)
	}
	primary, dr := hosts[0].ID, hosts[1].ID

	a, err := s.SavePlan(ctx, Plan{Name: "A", Spec: []byte("a"), PrimaryHostID: primary, DRHostID: dr, VMIDs: []uint32{101, 102}, CreatedBy: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == "" || len(a.VMIDs) != 2 {
		t.Fatalf("plan = %+v", a)
	}
	// Same name (any case) is a conflict.
	if _, err := s.SavePlan(ctx, Plan{Name: "a", Spec: []byte("x"), PrimaryHostID: primary, DRHostID: dr}); !errors.Is(err, ErrConflict) {
		t.Errorf("duplicate name: err = %v", err)
	}
	// A guest can be in only one plan.
	if _, err := s.SavePlan(ctx, Plan{Name: "B", Spec: []byte("b"), PrimaryHostID: primary, DRHostID: dr, VMIDs: []uint32{102, 103}}); !errors.Is(err, ErrGuestInOtherPlan) {
		t.Errorf("guest in two plans: err = %v", err)
	}
	b, err := s.SavePlan(ctx, Plan{Name: "B", Spec: []byte("b"), PrimaryHostID: primary, DRHostID: dr, VMIDs: []uint32{103}})
	if err != nil {
		t.Fatal(err)
	}
	// Updating a plan replaces its guests; freed guests can move plans.
	a.VMIDs = []uint32{101}
	if _, err := s.SavePlan(ctx, a); err != nil {
		t.Fatal(err)
	}
	b.VMIDs = []uint32{102, 103}
	if _, err := s.SavePlan(ctx, b); err != nil {
		t.Fatalf("moving a freed guest: %v", err)
	}
	gp, _ := s.GuestPlans(ctx, primary)
	if gp[101] != a.ID || gp[102] != b.ID || gp[103] != b.ID {
		t.Errorf("guest plans = %v", gp)
	}
	if _, err := s.SavePlan(ctx, Plan{ID: "missing", Name: "C", Spec: []byte("c"), PrimaryHostID: primary, DRHostID: dr}); !errors.Is(err, ErrNotFound) {
		t.Errorf("updating a missing plan: err = %v", err)
	}
	if _, err := s.SavePlan(ctx, Plan{Name: "C", Spec: []byte("c"), PrimaryHostID: "nope", DRHostID: dr}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown host: err = %v", err)
	}

	hs, _ := s.ListHosts(ctx)
	for _, h := range hs {
		if h.PlanCount != 2 {
			t.Errorf("host %s plan count = %d, want 2", h.ID, h.PlanCount)
		}
	}
	plans, _ := s.ListPlans(ctx)
	if len(plans) != 2 || plans[0].Name != "A" {
		t.Errorf("plans = %+v", plans)
	}

	// Removing a host removes its plans and their guests.
	if err := s.DeleteHost(ctx, primary); err != nil {
		t.Fatal(err)
	}
	if plans, _ := s.ListPlans(ctx); len(plans) != 0 {
		t.Errorf("plans not removed with host: %+v", plans)
	}
	if gp, _ := s.GuestPlans(ctx, primary); len(gp) != 0 {
		t.Errorf("plan guests not removed: %v", gp)
	}
}

func TestPlanStateAndHostZrepl(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	alloc := seqAlloc(netip.MustParseAddr("100.64.42.2"))
	var ids []string
	for _, id := range []string{"p1", "d1"} {
		if _, err := s.CreateToken(ctx, Token{ID: "t" + id, SecretHash: []byte("s"), CreatedBy: "admin",
			ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
		h, err := s.EnrollHost(ctx, "t"+id, []byte("s"), newHost(id, "m"+id), alloc)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, h.ID)
	}
	p, err := s.SavePlan(ctx, Plan{Name: "A", Spec: []byte("a"), PrimaryHostID: ids[0], DRHostID: ids[1]})
	if err != nil {
		t.Fatal(err)
	}
	if p.State != PlanDraft || p.AppliedSpec != nil {
		t.Fatalf("new plan = %+v", p)
	}
	if err := s.SetPlanState(ctx, p.ID, PlanActive, []byte("applied")); err != nil {
		t.Fatal(err)
	}
	// Saving edits keeps the state and applied specification.
	p.Spec = []byte("edited")
	p, _ = s.SavePlan(ctx, p)
	if p.State != PlanActive || string(p.AppliedSpec) != "applied" || p.AppliedAt.IsZero() || string(p.Spec) != "edited" {
		t.Fatalf("active plan after edit = %+v", p)
	}
	if err := s.SetPlanState(ctx, p.ID, PlanDraft, nil); err != nil {
		t.Fatal(err)
	}
	if p, _ = s.PlanByID(ctx, p.ID); p.AppliedSpec != nil || !p.AppliedAt.IsZero() {
		t.Errorf("deactivated plan = %+v", p)
	}

	h := ids[0]
	if changed, _ := s.SetHostZrepl(ctx, h, "CERT", "v0.7.0"); !changed {
		t.Error("first certificate not reported as changed")
	}
	if changed, _ := s.SetHostZrepl(ctx, h, "CERT", "v0.7.1"); changed {
		t.Error("same certificate reported as changed")
	}
	g1, _ := s.SetDesiredHash(ctx, h, []byte("h1"))
	g2, _ := s.SetDesiredHash(ctx, h, []byte("h1"))
	g3, _ := s.SetDesiredHash(ctx, h, []byte("h2"))
	if g1 != 2 || g2 != 2 || g3 != 3 {
		t.Errorf("generations = %d %d %d, want 2 2 3", g1, g2, g3)
	}
	if err := s.SetHostApplied(ctx, h, 3, "boom"); err != nil {
		t.Fatal(err)
	}
	host, _ := s.HostByID(ctx, h)
	if host.ZreplCertificate != "CERT" || host.ZreplVersion != "v0.7.1" || host.DesiredGeneration != 3 ||
		host.AppliedGeneration != 3 || host.ApplyError != "boom" {
		t.Errorf("host zrepl state = %+v", host)
	}
}

func TestAlerts(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Now()
	s.now = func() time.Time { return now }

	a, err := s.FireAlert(ctx, Alert{Key: "rpo:p1", Severity: "critical", Title: "RPO exceeded", Message: "m1", PlanID: "p1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.FireAlert(ctx, Alert{Key: "rpo:p1", Severity: "critical", Title: "x", Message: "x"}); !errors.Is(err, ErrConflict) {
		t.Errorf("second firing alert with the same key: err = %v", err)
	}
	now = now.Add(time.Hour)
	if err := s.UpdateAlert(ctx, a.ID, "m2", true); err != nil {
		t.Fatal(err)
	}
	firing, _ := s.FiringAlerts(ctx)
	if len(firing) != 1 || firing[0].Message != "m2" || !firing[0].LastNotifiedAt.After(firing[0].FiredAt) {
		t.Fatalf("firing = %+v", firing)
	}
	if err := s.ResolveAlert(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	// Once resolved, the same condition can fire again.
	if _, err := s.FireAlert(ctx, Alert{Key: "rpo:p1", Severity: "critical", Title: "RPO exceeded", Message: "m3"}); err != nil {
		t.Fatalf("refiring after resolve: %v", err)
	}
	all, _ := s.ListAlerts(ctx, 10)
	if len(all) != 2 || !all[0].ResolvedAt.IsZero() || all[1].ResolvedAt.IsZero() {
		t.Errorf("list = %+v (firing first)", all)
	}
}

func TestSiteTunnelState(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	alloc := seqAlloc(netip.MustParseAddr("100.64.42.2"))
	var ids []string
	for _, id := range []string{"a1", "b2", "c3"} {
		if _, err := s.CreateToken(ctx, Token{ID: "t" + id, SecretHash: []byte("s"), CreatedBy: "admin",
			ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
		h, err := s.EnrollHost(ctx, "t"+id, []byte("s"), newHost(id, "m"+id), alloc)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, h.ID)
	}
	prefix := netip.MustParsePrefix("100.64.43.0/30") // room for two hosts
	a, err := s.AllocateSiteAddress(ctx, ids[0], prefix)
	if err != nil || a != netip.MustParseAddr("100.64.43.1") {
		t.Fatalf("first = %v, %v", a, err)
	}
	if again, _ := s.AllocateSiteAddress(ctx, ids[0], prefix); again != a {
		t.Errorf("address not stable: %v", again)
	}
	if b, _ := s.AllocateSiteAddress(ctx, ids[1], prefix); b != netip.MustParseAddr("100.64.43.2") {
		t.Errorf("second = %v", b)
	}
	if _, err := s.AllocateSiteAddress(ctx, ids[2], prefix); err == nil {
		t.Error("allocated beyond the range")
	}
	if changed, _ := s.SetSitePublicKey(ctx, ids[0], []byte("k1")); !changed {
		t.Error("new key not reported as changed")
	}
	if changed, _ := s.SetSitePublicKey(ctx, ids[0], []byte("k1")); changed {
		t.Error("same key reported as changed")
	}
	h, _ := s.HostByID(ctx, ids[0])
	if h.SiteAddress != a || string(h.SitePublicKey) != "k1" {
		t.Errorf("host site state = %v %q", h.SiteAddress, h.SitePublicKey)
	}
}

func TestGuestExclusions(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if _, err := s.CreateToken(ctx, Token{ID: "t", SecretHash: []byte("s"), CreatedBy: "admin", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	h, err := s.EnrollHost(ctx, "t", []byte("s"), newHost("h1", "m1"), seqAlloc(netip.MustParseAddr("100.64.42.2")))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetGuestExcluded(ctx, h.ID, 130, true, "admin"); err != nil {
		t.Fatal(err)
	}
	// Marking it again keeps the first record.
	if err := s.SetGuestExcluded(ctx, h.ID, 130, true, "someone"); err != nil {
		t.Fatal(err)
	}
	got, err := s.GuestExclusions(ctx, h.ID)
	if err != nil || len(got) != 1 || got[130].By != "admin" || got[130].At.IsZero() {
		t.Fatalf("exclusions = %v, %v", got, err)
	}
	if err := s.SetGuestExcluded(ctx, "nope", 1, true, "admin"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown host: %v", err)
	}
	if err := s.SetGuestExcluded(ctx, h.ID, 130, false, "admin"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GuestExclusions(ctx, h.ID); len(got) != 0 {
		t.Errorf("exclusions after clearing = %v", got)
	}
}
