package portal

import (
	"bytes"
	"context"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"connectrpc.com/connect"
	"github.com/pquerna/otp/totp"

	enrollv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/enroll/v1"
	"github.com/jlbyh2o/ezdr/internal/gen/ezdr/enroll/v1/enrollv1connect"
	portalv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/portal/v1"
	"github.com/jlbyh2o/ezdr/internal/gen/ezdr/portal/v1/portalv1connect"
	"github.com/jlbyh2o/ezdr/internal/portal/api"
	"github.com/jlbyh2o/ezdr/internal/portal/store"
	"github.com/jlbyh2o/ezdr/internal/token"
)

type fakePeers struct {
	mu    sync.Mutex
	peers map[string]netip.Addr
}

func (f *fakePeers) PublicKey() []byte { return bytes.Repeat([]byte{9}, 32) }

func (f *fakePeers) AddPeer(k []byte, a netip.Addr) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.peers[string(k)] = a
	return nil
}

func (f *fakePeers) RemovePeer(k []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.peers, string(k))
	return nil
}

func TestPortalFlow(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "ezdr.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	box, _ := store.NewSecretBox(bytes.Repeat([]byte{1}, 32))
	peers := &fakePeers{peers: map[string]netip.Addr{}}
	publicURL, _ := url.Parse("https://portal.example.com")
	d := &api.Deps{
		Store: st, Box: box, Peers: peers, Hub: api.NewHub(), PublicURL: publicURL,
		WireGuardEndpoint: "portal.example.com:51820", TunnelPrefix: DefaultTunnelPrefix,
		PortalTunnelAddr: netip.MustParseAddr("100.64.42.1"), ClientAPIURL: "http://100.64.42.1:8080",
		Setup: api.NewSetupCode(),
	}
	srv := httptest.NewServer(PublicHandler(d, fstest.MapFS{"index.html": {Data: []byte("ui")}}))
	defer srv.Close()

	jar, _ := cookiejar.New(nil)
	hc := &http.Client{Jar: jar}
	setup := portalv1connect.NewSetupServiceClient(hc, srv.URL)
	authc := portalv1connect.NewAuthServiceClient(hc, srv.URL)
	tokens := portalv1connect.NewTokenServiceClient(hc, srv.URL)
	hosts := portalv1connect.NewHostServiceClient(hc, srv.URL)
	enroll := enrollv1connect.NewEnrollmentServiceClient(http.DefaultClient, srv.URL)

	wantCode := func(t *testing.T, err error, code connect.Code) {
		t.Helper()
		if connect.CodeOf(err) != code {
			t.Fatalf("err = %v, want code %v", err, code)
		}
	}

	// Signed-out users cannot call protected APIs.
	_, err = hosts.ListHosts(ctx, connect.NewRequest(&portalv1.ListHostsRequest{}))
	wantCode(t, err, connect.CodeUnauthenticated)

	// First-run setup requires the setup code.
	st1, _ := setup.GetSetupStatus(ctx, connect.NewRequest(&portalv1.GetSetupStatusRequest{}))
	if !st1.Msg.SetupRequired {
		t.Fatal("setup should be required")
	}
	_, err = setup.CompleteSetup(ctx, connect.NewRequest(&portalv1.CompleteSetupRequest{
		SetupCode: "wrong", Username: "admin", Password: "a long enough password"}))
	wantCode(t, err, connect.CodePermissionDenied)
	_, err = setup.CompleteSetup(ctx, connect.NewRequest(&portalv1.CompleteSetupRequest{
		SetupCode: d.Setup.String(), Username: "admin", Password: "a long enough password"}))
	if err != nil {
		t.Fatal(err)
	}
	if d.Setup.String() != "" {
		t.Error("setup code still valid after use")
	}

	// Wrong password.
	_, err = authc.Login(ctx, connect.NewRequest(&portalv1.LoginRequest{Username: "admin", Password: "nope"}))
	wantCode(t, err, connect.CodeUnauthenticated)

	// First sign-in sets up TOTP.
	login, err := authc.Login(ctx, connect.NewRequest(&portalv1.LoginRequest{Username: "admin", Password: "a long enough password"}))
	if err != nil {
		t.Fatal(err)
	}
	setupInfo := login.Msg.TotpSetup
	if setupInfo == nil || !strings.HasPrefix(setupInfo.Url, "otpauth://") {
		t.Fatalf("expected TOTP setup, got %+v", login.Msg)
	}
	code, _ := totp.GenerateCode(setupInfo.Secret, time.Now())
	verified, err := authc.VerifyTotp(ctx, connect.NewRequest(&portalv1.VerifyTotpRequest{
		ChallengeId: login.Msg.ChallengeId, Code: code}))
	if err != nil {
		t.Fatal(err)
	}
	if len(verified.Msg.RecoveryCodes) != 10 {
		t.Fatalf("got %d recovery codes", len(verified.Msg.RecoveryCodes))
	}
	if !strings.Contains(verified.Header().Get("Set-Cookie"), "HttpOnly") {
		t.Error("session cookie is not HttpOnly")
	}

	me, err := authc.GetCurrentUser(ctx, connect.NewRequest(&portalv1.GetCurrentUserRequest{}))
	if err != nil || me.Msg.User.Username != "admin" {
		t.Fatalf("GetCurrentUser: %v, %v", me, err)
	}

	// Cross-origin requests are rejected even with a valid session.
	req, _ := http.NewRequest(http.MethodPost, srv.URL+portalv1connect.HostServiceListHostsProcedure, strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://evil.example.com")
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-origin status = %d, want 403", resp.StatusCode)
	}

	// Create a token and enroll a host with it.
	created, err := tokens.CreateToken(ctx, connect.NewRequest(&portalv1.CreateTokenRequest{Description: "lab"}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(created.Msg.InstallCommand, created.Msg.TokenString) ||
		!strings.HasPrefix(created.Msg.EnrollCommand, "ezdr enroll ezdr1_") {
		t.Fatalf("unexpected commands: %+v", created.Msg)
	}
	tok, err := token.Decode(created.Msg.TokenString)
	if err != nil {
		t.Fatal(err)
	}
	if tok.PortalURL != "https://portal.example.com" {
		t.Errorf("token URL = %s", tok.PortalURL)
	}

	check, err := enroll.CheckToken(ctx, connect.NewRequest(&enrollv1.CheckTokenRequest{TokenId: tok.ID, Secret: tok.Secret}))
	if err != nil || check.Msg.TunnelPrefix != "100.64.42.0/28" {
		t.Fatalf("CheckToken: %v, %v", check, err)
	}
	enrollReq := &enrollv1.EnrollRequest{
		TokenId: tok.ID, Secret: tok.Secret, WireguardPublicKey: bytes.Repeat([]byte{5}, 32),
		Facts: &enrollv1.HostFacts{Hostname: "pve1", MachineId: "m1", PveVersion: "pve-manager/9.2", ClientVersion: "dev"},
	}
	enrolled, err := enroll.Enroll(ctx, connect.NewRequest(enrollReq))
	if err != nil {
		t.Fatal(err)
	}
	if enrolled.Msg.TunnelAddress != "100.64.42.2" || enrolled.Msg.PortalTunnelAddress != "100.64.42.1" {
		t.Errorf("addresses = %s / %s", enrolled.Msg.TunnelAddress, enrolled.Msg.PortalTunnelAddress)
	}
	if peers.peers[string(enrollReq.WireguardPublicKey)] != netip.MustParseAddr("100.64.42.2") {
		t.Error("WireGuard peer not added")
	}

	// The token cannot be used again.
	_, err = enroll.Enroll(ctx, connect.NewRequest(enrollReq))
	wantCode(t, err, connect.CodePermissionDenied)

	list, err := hosts.ListHosts(ctx, connect.NewRequest(&portalv1.ListHostsRequest{}))
	if err != nil || len(list.Msg.Hosts) != 1 || list.Msg.Hosts[0].Online {
		t.Fatalf("ListHosts: %+v, %v", list, err)
	}

	// Removing the host removes its peer.
	if _, err := hosts.DeleteHost(ctx, connect.NewRequest(&portalv1.DeleteHostRequest{Id: enrolled.Msg.HostId})); err != nil {
		t.Fatal(err)
	}
	if len(peers.peers) != 0 {
		t.Error("WireGuard peer not removed")
	}

	// Signing out ends the session.
	if _, err := authc.Logout(ctx, connect.NewRequest(&portalv1.LogoutRequest{})); err != nil {
		t.Fatal(err)
	}
	_, err = hosts.ListHosts(ctx, connect.NewRequest(&portalv1.ListHostsRequest{}))
	wantCode(t, err, connect.CodeUnauthenticated)

	// The audit log recorded the important events.
	events, _ := st.ListAuditEvents(ctx, 100)
	seen := map[string]bool{}
	for _, e := range events {
		seen[e.Action] = true
	}
	for _, a := range []string{"setup.complete", "auth.totp_enabled", "auth.login", "auth.login_failed",
		"token.create", "host.enroll", "host.delete", "auth.logout"} {
		if !seen[a] {
			t.Errorf("audit log missing %q", a)
		}
	}
}
