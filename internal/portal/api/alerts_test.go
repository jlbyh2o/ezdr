package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	clientv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/client/v1"
	planv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/plan/v1"
	portalv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/portal/v1"
	"github.com/jlbyh2o/ezdr/internal/portal/notify"
)

func TestAlertSettingsKeepSecrets(t *testing.T) {
	d, ctx, _, _ := planTestDeps(t)
	svc := AlertService{Deps: d}
	_, err := svc.UpdateAlertSettings(ctx, connect.NewRequest(&portalv1.UpdateAlertSettingsRequest{Settings: &portalv1.AlertSettings{
		Smtp: &portalv1.SmtpSettings{Enabled: true, Host: "smtp.example.com", Port: 587, Security: portalv1.SmtpSecurity_SMTP_SECURITY_STARTTLS,
			Username: "u", Password: "p@ss", From: "ezdr@example.com", Recipients: []string{"ops@example.com"}},
		Webhooks: []*portalv1.Webhook{{Url: "https://hooks.example.com/a", Secret: "s1"}},
	}}))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := svc.GetAlertSettings(ctx, connect.NewRequest(&portalv1.GetAlertSettingsRequest{}))
	st := got.Msg.Settings
	if st.Smtp.Password != "" || !st.Smtp.HasPassword || st.Webhooks[0].Secret != "" || !st.Webhooks[0].HasSecret {
		t.Fatalf("secrets returned or lost: %v", st)
	}
	// Saving what the browser got back (no secrets) keeps the stored ones.
	if _, err := svc.UpdateAlertSettings(ctx, connect.NewRequest(&portalv1.UpdateAlertSettingsRequest{Settings: st})); err != nil {
		t.Fatal(err)
	}
	stored, _ := d.loadAlertSettings(ctx)
	if stored.Smtp.Password != "p@ss" || stored.Webhooks[0].Secret != "s1" {
		t.Errorf("stored secrets = %q %q", stored.Smtp.Password, stored.Webhooks[0].Secret)
	}
	// Another server doesn't get the stored password.
	moved := proto.CloneOf(st)
	moved.Smtp.Host = "smtp.other.example.com"
	_, _ = svc.UpdateAlertSettings(ctx, connect.NewRequest(&portalv1.UpdateAlertSettingsRequest{Settings: moved}))
	if stored, _ = d.loadAlertSettings(ctx); stored.Smtp.Host == moved.Smtp.Host && stored.Smtp.Password != "" {
		t.Error("stored password kept for another server")
	}
	st.Webhooks[0].ClearSecret = true
	_, _ = svc.UpdateAlertSettings(ctx, connect.NewRequest(&portalv1.UpdateAlertSettingsRequest{Settings: st}))
	if stored, _ = d.loadAlertSettings(ctx); stored.Webhooks[0].Secret != "" {
		t.Error("secret not cleared")
	}
	bad := proto.CloneOf(st)
	bad.Webhooks[0].Url = "ftp://x"
	if _, err := svc.UpdateAlertSettings(ctx, connect.NewRequest(&portalv1.UpdateAlertSettingsRequest{Settings: bad})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("invalid webhook accepted: %v", err)
	}
}

type webhookSink struct {
	mu     sync.Mutex
	events []notify.Notification
}

func (w *webhookSink) handler(_ http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	var n notify.Notification
	_ = json.Unmarshal(b, &n)
	w.mu.Lock()
	w.events = append(w.events, n)
	w.mu.Unlock()
}

func (w *webhookSink) take() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []string
	for _, e := range w.events {
		out = append(out, e.Event+" "+e.Title)
	}
	w.events = nil
	return out
}

func TestAlertEngine(t *testing.T) {
	d, ctx, primary, dr := planTestDeps(t)
	sink := &webhookSink{}
	srv := httptest.NewServer(http.HandlerFunc(sink.handler))
	defer srv.Close()
	if _, err := (AlertService{Deps: d}).UpdateAlertSettings(ctx, connect.NewRequest(&portalv1.UpdateAlertSettingsRequest{
		Settings: &portalv1.AlertSettings{Webhooks: []*portalv1.Webhook{{Url: srv.URL}}}})); err != nil {
		t.Fatal(err)
	}

	// An active plan with a 5-minute interval (15-minute RPO threshold).
	for _, id := range []string{primary, dr} {
		_, _ = d.Store.SetHostZrepl(ctx, id, "CERT-"+id, "v0.7.0")
		_ = d.Store.TouchHost(ctx, id, "dev")
	}
	svc := PlanService{Deps: d}
	sug, _ := svc.SuggestPlan(ctx, connect.NewRequest(&portalv1.SuggestPlanRequest{Spec: &planv1.PlanSpec{
		Name: "Main", PrimaryHostId: primary, DrHostId: dr, Guests: []*planv1.PlanGuest{{Vmid: 101}}, IntervalSeconds: 300,
	}}))
	created, err := svc.CreatePlan(ctx, connect.NewRequest(&portalv1.CreatePlanRequest{Spec: sug.Msg.Spec}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ActivatePlan(ctx, connect.NewRequest(&portalv1.ActivatePlanRequest{Id: created.Msg.Plan.Id})); err != nil {
		t.Fatal(err)
	}
	// Both hosts connected.
	_, _, relP := d.Hub.connect(ctx, primary)
	defer relP()
	_, _, relD := d.Hub.connect(ctx, dr)
	defer relD()

	report := func(snapAge time.Duration) {
		t.Helper()
		pl, _ := d.Store.PlanByID(ctx, created.Msg.Plan.Id)
		spec, _ := decodeSpec(pl.AppliedSpec)
		job := "ezdr_" + pl.ID[:8] + "_local-zfs_pull"
		st := &clientv1.ReportReplicationRequest{Jobs: []*clientv1.JobStatus{{Name: job, Type: "pull", State: "done",
			Datasets: []*clientv1.DatasetStatus{{Dataset: "rpool/subvol-101-disk-0", LatestSnapshot: spec.SnapshotPrefix + "1",
				LatestSnapshotAt: timestamppb.New(time.Now().Add(-snapAge))}}}}}
		b, _ := proto.Marshal(st)
		if err := d.Store.PutReplicationStatus(ctx, dr, b); err != nil {
			t.Fatal(err)
		}
	}
	e := NewAlertEngine(d)

	report(2 * time.Minute)
	if err := e.Evaluate(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := sink.take(); len(got) != 0 {
		t.Fatalf("healthy plan alerted: %v", got)
	}

	report(20 * time.Minute)
	_ = e.Evaluate(ctx, time.Now())
	if got := sink.take(); len(got) != 1 || got[0] != "firing RPO exceeded for plan Main" {
		t.Fatalf("RPO alert = %v", got)
	}
	// Still exceeded a minute later: no repeat.
	_ = e.Evaluate(ctx, time.Now().Add(time.Minute))
	if got := sink.take(); len(got) != 0 {
		t.Fatalf("repeated alert: %v", got)
	}
	// After 12 hours: a reminder.
	_ = e.Evaluate(ctx, time.Now().Add(13*time.Hour))
	if got := sink.take(); len(got) != 1 || got[0] != "reminder RPO exceeded for plan Main" {
		t.Fatalf("reminder = %v", got)
	}

	report(time.Minute)
	_ = e.Evaluate(ctx, time.Now())
	if got := sink.take(); len(got) != 1 || got[0] != "resolved RPO exceeded for plan Main" {
		t.Fatalf("resolution = %v", got)
	}

	// The DR host disconnects and hasn't been seen for 10 minutes.
	relD()
	later := time.Now().Add(10 * time.Minute)
	_ = e.Evaluate(ctx, later)
	got := sink.take()
	found := false
	for _, g := range got {
		if g == "firing Host dr1 is offline" {
			found = true
		}
	}
	if !found {
		t.Fatalf("offline alert missing: %v", got)
	}

	alerts, _ := (AlertService{Deps: d}).ListAlerts(ctx, connect.NewRequest(&portalv1.ListAlertsRequest{}))
	if len(alerts.Msg.Alerts) < 2 || alerts.Msg.Alerts[0].ResolvedAt != nil {
		t.Errorf("alert list = %v", alerts.Msg.Alerts)
	}
}
