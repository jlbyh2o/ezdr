package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	portalv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/portal/v1"
	"github.com/jlbyh2o/ezdr/internal/portal/dns"
	"github.com/jlbyh2o/ezdr/internal/portal/store"
)

// DNS: see docs/design/failover.md, section 6.

const (
	dnsTokenSecret = "dns_cloudflare_token" //nolint:gosec // a row name, not a credential
	// DNSCheckInterval is how often plans' records are checked for drift.
	DNSCheckInterval = 5 * time.Minute
	// maxDNSOnlyTTL is the longest TTL a DNS-only record should have to switch
	// quickly.
	maxDNSOnlyTTL = 300
)

// DNSService manages the DNS provider connection.
type DNSService struct{ *Deps }

// dnsProvider returns the configured provider, or nil if none is.
func (d *Deps) dnsProvider(ctx context.Context) (dns.Provider, error) {
	sealed, err := d.Store.Secret(ctx, dnsTokenSecret)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	token, err := d.Box.Open(sealed)
	if err != nil {
		return nil, err
	}
	if d.NewDNSProvider != nil {
		return d.NewDNSProvider(string(token)), nil
	}
	return &dns.Cloudflare{Token: string(token)}, nil
}

// GetDnsSettings reports whether a Cloudflare token is stored. The token
// itself is never returned.
func (s DNSService) GetDnsSettings(ctx context.Context, _ *connect.Request[portalv1.GetDnsSettingsRequest]) (*connect.Response[portalv1.GetDnsSettingsResponse], error) { //nolint:revive // name from the generated interface
	p, err := s.dnsProvider(ctx)
	if err != nil {
		return nil, internalError(err)
	}
	return connect.NewResponse(&portalv1.GetDnsSettingsResponse{CloudflareTokenSet: p != nil}), nil
}

// UpdateDnsSettings stores or clears the Cloudflare token.
func (s DNSService) UpdateDnsSettings(ctx context.Context, req *connect.Request[portalv1.UpdateDnsSettingsRequest]) (*connect.Response[portalv1.UpdateDnsSettingsResponse], error) { //nolint:revive // name from the generated interface
	token := strings.TrimSpace(req.Msg.CloudflareToken)
	switch {
	case req.Msg.Clear:
		if err := s.Store.DeleteSecret(ctx, dnsTokenSecret); err != nil && !errors.Is(err, store.ErrNotFound) {
			return nil, internalError(err)
		}
		s.audit(ctx, currentUser(ctx).Username, "settings.dns", "settings", "Cloudflare token removed")
	case token != "":
		if len(token) > 200 {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("that doesn't look like a Cloudflare API token"))
		}
		if err := s.Store.PutSecret(ctx, dnsTokenSecret, s.Box.Seal([]byte(token))); err != nil {
			return nil, internalError(err)
		}
		s.audit(ctx, currentUser(ctx).Username, "settings.dns", "settings", "Cloudflare token set")
	}
	return connect.NewResponse(&portalv1.UpdateDnsSettingsResponse{}), nil
}

// TestDnsSettings lists the zones the token can see.
func (s DNSService) TestDnsSettings(ctx context.Context, _ *connect.Request[portalv1.TestDnsSettingsRequest]) (*connect.Response[portalv1.TestDnsSettingsResponse], error) { //nolint:revive // name from the generated interface
	p, err := s.dnsProvider(ctx)
	if err != nil {
		return nil, internalError(err)
	}
	if p == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("no Cloudflare token is stored"))
	}
	zones, err := p.Zones(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, err)
	}
	resp := &portalv1.TestDnsSettingsResponse{}
	for _, z := range zones {
		resp.Zones = append(resp.Zones, z.Name)
	}
	return connect.NewResponse(resp), nil
}

// GetPlanDnsStatus returns the plan's records as last checked.
func (s DNSService) GetPlanDnsStatus(ctx context.Context, req *connect.Request[portalv1.GetPlanDnsStatusRequest]) (*connect.Response[portalv1.GetPlanDnsStatusResponse], error) { //nolint:revive // name from the generated interface
	b, err := s.Store.DNSCheck(ctx, req.Msg.PlanId)
	if errors.Is(err, store.ErrNotFound) {
		return connect.NewResponse(&portalv1.GetPlanDnsStatusResponse{}), nil
	}
	if err != nil {
		return nil, internalError(err)
	}
	st := &portalv1.PlanDnsStatus{}
	if err := proto.Unmarshal(b, st); err != nil {
		return nil, internalError(err)
	}
	return connect.NewResponse(&portalv1.GetPlanDnsStatusResponse{Status: st}), nil
}

// CheckPlanDns checks the plan's records now.
func (s DNSService) CheckPlanDns(ctx context.Context, req *connect.Request[portalv1.CheckPlanDnsRequest]) (*connect.Response[portalv1.CheckPlanDnsResponse], error) { //nolint:revive // name from the generated interface
	st, err := s.checkPlanDNS(ctx, req.Msg.PlanId)
	if err != nil {
		return nil, internalError(err)
	}
	return connect.NewResponse(&portalv1.CheckPlanDnsResponse{Status: st}), nil
}

// expectedDNS returns each record's expected value for the plan's state,
// keyed by name and type. While failed over, only records the failover
// switched are expected to hold their failover values; skipped records
// aren't checked (the operator switches them by hand).
func (d *Deps) expectedDNS(ctx context.Context, sp store.Plan) (map[string]string, []*portalv1.DnsRecordSwitch, error) {
	spec, err := decodeSpec(sp.AppliedSpec)
	if err != nil {
		return nil, nil, err
	}
	records := dnsRecords(spec)
	expected := map[string]string{}
	switch sp.State {
	case store.PlanActive, store.PlanPaused:
		for _, r := range records {
			expected[dnsKey(r.Name, r.Type)] = r.ProductionValue
		}
	case store.PlanFailedOver:
		_, f, err := d.loadFailover(ctx, sp.ID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return nil, nil, err
		}
		for _, r := range f.GetDnsRecords() {
			if r.Status == "switched" {
				expected[dnsKey(r.Name, r.Type)] = r.FailoverValue
			}
		}
	}
	return expected, records, nil
}

func dnsKey(name, typ string) string { return dns.Normalize(name) + "/" + strings.ToUpper(typ) }

// checkPlanDNS reads the plan's records from the provider, compares them
// with the values expected for the plan's state, and stores the result.
func (d *Deps) checkPlanDNS(ctx context.Context, planID string) (*portalv1.PlanDnsStatus, error) {
	sp, err := d.Store.PlanByID(ctx, planID)
	if err != nil {
		return nil, err
	}
	st := &portalv1.PlanDnsStatus{CheckedAt: timestamppb.Now()}
	if sp.AppliedSpec == nil {
		return st, nil
	}
	expected, records, err := d.expectedDNS(ctx, sp)
	if err != nil {
		return nil, err
	}
	if len(records) == 0 {
		return st, d.saveDNSCheck(ctx, planID, st)
	}
	p, err := d.dnsProvider(ctx)
	if err != nil {
		return nil, err
	}
	var zones []dns.Zone
	if p == nil {
		st.Error = "no DNS provider is connected"
	} else if zones, err = p.Zones(ctx); err != nil {
		st.Error = err.Error()
	}
	for _, r := range records {
		rs := &portalv1.DnsRecordStatus{Vmid: r.Vmid, Name: r.Name, Type: r.Type, Expected: expected[dnsKey(r.Name, r.Type)]}
		st.Records = append(st.Records, rs)
		if st.Error != "" {
			continue
		}
		z, ok := dns.ZoneFor(zones, r.Name)
		if !ok {
			rs.Problems = append(rs.Problems, "no zone the Cloudflare token can see contains this name")
			continue
		}
		rs.Zone = z.Name
		rec, err := p.Find(ctx, z, r.Name, r.Type)
		switch {
		case err != nil:
			rs.Problems = append(rs.Problems, err.Error())
			continue
		case rec == nil:
			rs.Problems = append(rs.Problems, "the record doesn't exist in Cloudflare")
			continue
		}
		rs.Current, rs.Ttl, rs.Proxied = rec.Content, uint32(max(rec.TTL, 0)), rec.Proxied //nolint:gosec // TTLs fit
		// Cloudflare's TTL 1 means automatic (300 seconds).
		if !rec.Proxied && rec.TTL > maxDNSOnlyTTL {
			rs.Problems = append(rs.Problems, fmt.Sprintf("the TTL is %d seconds; resolvers may keep the old value that long after a switch (300 or less is recommended)", rec.TTL))
		}
		if rs.Expected != "" && !dns.SameValue(rec.Content, rs.Expected) {
			rs.Drift = true
			rs.Problems = append(rs.Problems, fmt.Sprintf("the record is %s, but %s is expected for the plan's state", rec.Content, rs.Expected))
		}
	}
	return st, d.saveDNSCheck(ctx, planID, st)
}

func (d *Deps) saveDNSCheck(ctx context.Context, planID string, st *portalv1.PlanDnsStatus) error {
	b, err := proto.Marshal(st)
	if err != nil {
		return err
	}
	return d.Store.PutDNSCheck(ctx, planID, b)
}

// RunDNSChecker checks every plan's records for drift until ctx is canceled.
func (d *Deps) RunDNSChecker(ctx context.Context) {
	ticker := time.NewTicker(DNSCheckInterval)
	defer ticker.Stop()
	for {
		plans, err := d.Store.ListPlans(ctx)
		if err != nil && ctx.Err() == nil {
			slog.Error("dns check: list plans", "err", err)
		}
		for _, p := range plans {
			if p.State == store.PlanDraft {
				continue
			}
			if _, err := d.checkPlanDNS(ctx, p.ID); err != nil && ctx.Err() == nil {
				slog.Warn("dns check", "plan", p.Name, "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// dnsUpdater applies fn to the DNS records of a failover or failback and
// saves them.
type dnsUpdater func(fn func(records []*portalv1.DnsRecordSwitch))

// switchRecords sets each pending record of the plan's latest failover to
// its failover (or production) value and reads it back. It returns a
// summary.
func (d *Deps) switchRecords(ctx context.Context, planID string, toFailover bool, update dnsUpdater) string {
	p, err := d.dnsProvider(ctx)
	if err != nil || p == nil {
		why := "no DNS provider is connected"
		if err != nil {
			why = err.Error()
		}
		update(func(records []*portalv1.DnsRecordSwitch) {
			for _, r := range records {
				if r.Status == "pending" || r.Status == "failed" {
					r.Status, r.Detail = "failed", why
				}
			}
		})
		return "DNS not switched: " + why
	}
	zones, zerr := p.Zones(ctx)
	switched, failed := 0, 0
	update(func(records []*portalv1.DnsRecordSwitch) {
		for _, r := range records {
			if r.Status != "pending" && r.Status != "failed" {
				continue
			}
			want := r.ProductionValue
			if toFailover {
				want = r.FailoverValue
			}
			if err := switchRecord(ctx, p, zones, zerr, r, want); err != nil {
				r.Status, r.Detail = "failed", err.Error()
				failed++
				continue
			}
			r.Status, r.Detail = "switched", "now "+want
			switched++
		}
	})
	if _, err := d.checkPlanDNS(ctx, planID); err != nil {
		slog.Warn("dns check after switch", "plan", planID, "err", err)
	}
	if failed > 0 {
		return fmt.Sprintf("switched %d DNS record(s), %d failed", switched, failed)
	}
	return fmt.Sprintf("switched %d DNS record(s)", switched)
}

// switchRecord updates one record and reads it back.
func switchRecord(ctx context.Context, p dns.Provider, zones []dns.Zone, zonesErr error, r *portalv1.DnsRecordSwitch, want string) error {
	if zonesErr != nil {
		return zonesErr
	}
	z, ok := dns.ZoneFor(zones, r.Name)
	if !ok {
		return errors.New("no zone the Cloudflare token can see contains this name")
	}
	rec, err := p.Find(ctx, z, r.Name, r.Type)
	if err != nil {
		return err
	}
	if rec == nil {
		return errors.New("the record doesn't exist in Cloudflare")
	}
	if !dns.SameValue(rec.Content, want) {
		if err := p.Update(ctx, z, *rec, want); err != nil {
			return err
		}
	}
	back, err := p.Find(ctx, z, r.Name, r.Type)
	if err != nil {
		return fmt.Errorf("reading it back: %w", err)
	}
	if back == nil || !dns.SameValue(back.Content, want) {
		return errors.New("the record didn't change")
	}
	return nil
}
