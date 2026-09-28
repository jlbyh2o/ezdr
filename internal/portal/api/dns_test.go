package api

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	planv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/plan/v1"
	portalv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/portal/v1"
	"github.com/jlbyh2o/ezdr/internal/portal/dns"
	"github.com/jlbyh2o/ezdr/internal/portal/store"
)

type fakeDNS struct {
	mu      sync.Mutex
	records map[string]*dns.Record // name/type -> record
}

func (f *fakeDNS) Zones(context.Context) ([]dns.Zone, error) {
	return []dns.Zone{{ID: "z1", Name: "example.com"}}, nil
}

func (f *fakeDNS) Find(_ context.Context, _ dns.Zone, name, typ string) (*dns.Record, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.records[dnsKey(name, typ)]; ok {
		c := *r
		return &c, nil
	}
	return nil, nil
}

func (f *fakeDNS) Update(_ context.Context, _ dns.Zone, r dns.Record, content string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records[dnsKey(r.Name, r.Type)].Content = content
	return nil
}

// withDNS gives the fixture's plan a DNS record and a connected provider.
func withDNS(ctx context.Context, t *testing.T, d *Deps, planID string, ttl int) *fakeDNS {
	t.Helper()
	sp, err := d.Store.PlanByID(ctx, planID)
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := decodeSpec(sp.AppliedSpec)
	spec.Guests[0].DnsRecords = []*planv1.DnsRecord{{Name: "web.example.com", Type: planv1.DnsRecordType_DNS_RECORD_TYPE_A,
		ProductionValue: "203.0.113.10", FailoverValue: "198.51.100.10"}}
	b, _ := proto.Marshal(spec)
	if err := d.Store.SetPlanState(ctx, planID, store.PlanActive, b); err != nil {
		t.Fatal(err)
	}
	f := &fakeDNS{records: map[string]*dns.Record{
		"web.example.com/A": {ID: "r1", Name: "web.example.com", Type: "A", Content: "203.0.113.10", TTL: ttl},
	}}
	d.NewDNSProvider = func(string) dns.Provider { return f }
	if err := d.Store.PutSecret(ctx, dnsTokenSecret, d.Box.Seal([]byte("token"))); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestDNSCheck(t *testing.T) {
	d, ctx, planID, _ := testFixture(t)
	f := withDNS(ctx, t, d, planID, 3600)
	st, err := d.checkPlanDNS(ctx, planID)
	if err != nil {
		t.Fatal(err)
	}
	r := st.Records[0]
	if r.Drift || r.Current != "203.0.113.10" || r.Zone != "example.com" || len(r.Problems) != 1 || !strings.Contains(r.Problems[0], "TTL is 3600") {
		t.Errorf("record = %v", r)
	}
	// A manual edit is drift, and raises an alert.
	f.records["web.example.com/A"].Content = "192.0.2.99"
	if st, _ = d.checkPlanDNS(ctx, planID); !st.Records[0].Drift {
		t.Errorf("drift not detected: %v", st.Records[0])
	}
	conds, err := NewAlertEngine(d).conditions(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range conds {
		found = found || c.Key == "dns:"+planID
	}
	if !found {
		t.Errorf("no DNS alert in %v", conds)
	}
}

func TestFailoverSwitchesDNS(t *testing.T) {
	d, ctx, planID, _, _ := failoverFixture(t)
	f := withDNS(ctx, t, d, planID, 300)
	svc := FailoverService{Deps: d}
	if _, err := svc.StartFailover(ctx, connect.NewRequest(&portalv1.StartFailoverRequest{PlanId: planID, Planned: true, ConfirmName: "Main"})); err != nil {
		t.Fatal(err)
	}
	waitFailover(ctx, t, d, planID, portalv1.FailoverState_FAILOVER_STATE_AWAITING_CONFIRMATION)
	c, err := svc.ConfirmFailover(ctx, connect.NewRequest(&portalv1.ConfirmFailoverRequest{PlanId: planID, SwitchDns: true}))
	if err != nil {
		t.Fatal(err)
	}
	fo := c.Msg.Failover
	if fo.DnsRecords[0].Status != "switched" || f.records["web.example.com/A"].Content != "198.51.100.10" ||
		!strings.Contains(fo.Steps[foStepConfirm].Detail, "switched 1 DNS record") {
		t.Errorf("failover = %v", fo)
	}
	// Failed over and switched: the failover value is now expected.
	st, _ := d.checkPlanDNS(ctx, planID)
	if st.Records[0].Expected != "198.51.100.10" || st.Records[0].Drift {
		t.Errorf("check after failover = %v", st.Records[0])
	}
	f.records["web.example.com/A"].Content = "203.0.113.10"
	if st, _ := d.checkPlanDNS(ctx, planID); !st.Records[0].Drift {
		t.Error("switching back by hand wasn't reported as drift")
	}
}
