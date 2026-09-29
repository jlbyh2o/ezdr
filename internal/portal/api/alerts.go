package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	portalv1 "github.com/jlbyh2o/ezdr/internal/gen/ezdr/portal/v1"
	"github.com/jlbyh2o/ezdr/internal/portal/notify"
	"github.com/jlbyh2o/ezdr/internal/portal/store"
	"github.com/jlbyh2o/ezdr/internal/replication"
)

const alertSettingsSecret = "alert_settings" //nolint:gosec // a row name, not a credential

// Alert timing (see docs/design/replication.md, section 8).
const (
	AlertInterval = time.Minute
	reminderAfter = 12 * time.Hour
	offlineAfter  = 5 * time.Minute
	failingGrace  = time.Minute
)

// loadAlertSettings returns the stored settings, including secrets.
func (d *Deps) loadAlertSettings(ctx context.Context) (*portalv1.AlertSettings, error) {
	sealed, err := d.Store.Secret(ctx, alertSettingsSecret)
	if errors.Is(err, store.ErrNotFound) {
		return &portalv1.AlertSettings{Smtp: &portalv1.SmtpSettings{Port: 587, Security: portalv1.SmtpSecurity_SMTP_SECURITY_STARTTLS}}, nil
	}
	if err != nil {
		return nil, err
	}
	b, err := d.Box.Open(sealed)
	if err != nil {
		return nil, err
	}
	s := &portalv1.AlertSettings{}
	return s, proto.Unmarshal(b, s)
}

// redact removes secrets before settings are returned to the browser.
func redact(s *portalv1.AlertSettings) *portalv1.AlertSettings {
	c := proto.CloneOf(s)
	if c.Smtp == nil {
		c.Smtp = &portalv1.SmtpSettings{}
	}
	c.Smtp.HasPassword, c.Smtp.Password = c.Smtp.Password != "", ""
	for _, w := range c.Webhooks {
		w.HasSecret, w.Secret, w.ClearSecret = w.Secret != "", "", false
	}
	return c
}

var smtpSecurity = map[portalv1.SmtpSecurity]string{
	portalv1.SmtpSecurity_SMTP_SECURITY_STARTTLS: notify.SecurityStartTLS,
	portalv1.SmtpSecurity_SMTP_SECURITY_TLS:      notify.SecurityTLS,
	portalv1.SmtpSecurity_SMTP_SECURITY_NONE:     notify.SecurityNone,
}

func smtpChannel(s *portalv1.SmtpSettings) notify.SMTP {
	return notify.SMTP{
		Host: strings.TrimSpace(s.GetHost()), Port: int(s.GetPort()), Security: smtpSecurity[s.GetSecurity()],
		Username: s.GetUsername(), Password: s.GetPassword(), From: s.GetFrom(), Recipients: s.GetRecipients(),
	}
}

// AlertService manages alert channels and lists alerts.
type AlertService struct{ *Deps }

// GetAlertSettings returns the alert channels without secrets.
func (s AlertService) GetAlertSettings(ctx context.Context, _ *connect.Request[portalv1.GetAlertSettingsRequest]) (*connect.Response[portalv1.GetAlertSettingsResponse], error) {
	st, err := s.loadAlertSettings(ctx)
	if err != nil {
		return nil, internalError(err)
	}
	return connect.NewResponse(&portalv1.GetAlertSettingsResponse{Settings: redact(st)}), nil
}

// UpdateAlertSettings replaces the alert channels. Empty passwords and
// secrets keep the stored ones.
func (s AlertService) UpdateAlertSettings(ctx context.Context, req *connect.Request[portalv1.UpdateAlertSettingsRequest]) (*connect.Response[portalv1.UpdateAlertSettingsResponse], error) {
	in := req.Msg.GetSettings()
	if in == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("settings are required"))
	}
	old, err := s.loadAlertSettings(ctx)
	if err != nil {
		return nil, internalError(err)
	}
	next := proto.CloneOf(in)
	if next.Smtp == nil {
		next.Smtp = &portalv1.SmtpSettings{}
	}
	// The stored password is kept only for the same server and account, so
	// it can't be sent to another one.
	if o := old.GetSmtp(); next.Smtp.Password == "" && next.Smtp.Host == o.GetHost() && next.Smtp.Port == o.GetPort() &&
		next.Smtp.Security == o.GetSecurity() && next.Smtp.Username == o.GetUsername() {
		next.Smtp.Password = o.GetPassword()
	}
	if next.Smtp.Enabled {
		if err := smtpChannel(next.Smtp).Validate(); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
	}
	oldSecrets := map[string]string{}
	for _, w := range old.GetWebhooks() {
		oldSecrets[w.Url] = w.Secret
	}
	if len(next.Webhooks) > 10 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("at most 10 webhooks"))
	}
	for _, w := range next.Webhooks {
		w.Url = strings.TrimSpace(w.Url)
		if err := notify.ValidateURL(w.Url); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		switch {
		case w.ClearSecret:
			w.Secret = ""
		case w.Secret == "":
			w.Secret = oldSecrets[w.Url]
		}
		w.ClearSecret, w.HasSecret = false, false
	}
	next.Smtp.HasPassword = false
	b, err := proto.Marshal(next)
	if err != nil {
		return nil, internalError(err)
	}
	if err := s.Store.PutSecret(ctx, alertSettingsSecret, s.Box.Seal(b)); err != nil {
		return nil, internalError(err)
	}
	s.audit(ctx, currentUser(ctx).Username, "alerts.settings", "", fmt.Sprintf("email %s, %d webhook(s)",
		map[bool]string{true: "enabled", false: "disabled"}[next.Smtp.Enabled], len(next.Webhooks)))
	return connect.NewResponse(&portalv1.UpdateAlertSettingsResponse{Settings: redact(next)}), nil
}

// notifyAll sends n to every configured channel and returns one result line
// per channel.
func (d *Deps) notifyAll(ctx context.Context, n notify.Notification) []string {
	st, err := d.loadAlertSettings(ctx)
	if err != nil {
		slog.Error("load alert settings", "err", err)
		return []string{"could not load alert settings"}
	}
	n.PortalURL = d.PublicURL.String()
	var results []string
	if st.GetSmtp().GetEnabled() {
		if err := smtpChannel(st.Smtp).SendEmail(ctx, n); err != nil {
			results = append(results, "email: failed: "+err.Error())
		} else {
			results = append(results, "email: sent to "+strings.Join(st.Smtp.Recipients, ", "))
		}
	}
	for _, w := range st.GetWebhooks() {
		if err := (notify.Webhook{URL: w.Url, Secret: w.Secret}).Send(ctx, n); err != nil {
			results = append(results, "webhook "+w.Url+": failed: "+err.Error())
		} else {
			results = append(results, "webhook "+w.Url+": sent")
		}
	}
	for _, r := range results {
		if strings.Contains(r, ": failed: ") {
			slog.Warn("alert notification failed", "result", r)
		}
	}
	return results
}

// SendTestAlert sends a test notification to every channel.
func (s AlertService) SendTestAlert(ctx context.Context, _ *connect.Request[portalv1.SendTestAlertRequest]) (*connect.Response[portalv1.SendTestAlertResponse], error) {
	results := s.notifyAll(ctx, notify.Notification{
		Event: "test", Severity: "info", Title: "Test notification",
		Message: "This is a test notification from EZDR. Alert delivery is working.", Time: time.Now(),
	})
	if len(results) == 0 {
		results = []string{"no channels are configured"}
	}
	s.audit(ctx, currentUser(ctx).Username, "alerts.test", "", strings.Join(results, "; "))
	return connect.NewResponse(&portalv1.SendTestAlertResponse{Results: results}), nil
}

var alertSeverities = map[string]portalv1.AlertSeverity{
	"warning":  portalv1.AlertSeverity_ALERT_SEVERITY_WARNING,
	"critical": portalv1.AlertSeverity_ALERT_SEVERITY_CRITICAL,
}

// ListAlerts lists firing alerts, then recent resolved ones.
func (s AlertService) ListAlerts(ctx context.Context, req *connect.Request[portalv1.ListAlertsRequest]) (*connect.Response[portalv1.ListAlertsResponse], error) {
	limit := int(req.Msg.Limit)
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	alerts, err := s.Store.ListAlerts(ctx, limit)
	if err != nil {
		return nil, internalError(err)
	}
	resp := &portalv1.ListAlertsResponse{}
	for _, a := range alerts {
		resp.Alerts = append(resp.Alerts, &portalv1.Alert{
			Key: a.Key, Severity: alertSeverities[a.Severity], Title: a.Title, Message: a.Message,
			PlanId: a.PlanID, HostId: a.HostID, FiredAt: ts(a.FiredAt), ResolvedAt: ts(a.ResolvedAt),
		})
	}
	return connect.NewResponse(resp), nil
}

// condition is an alert condition that currently holds.
type condition struct {
	store.Alert
	plan, host string // names, for notifications
}

// AlertEngine evaluates alert conditions and sends notifications.
type AlertEngine struct {
	*Deps
	mu           sync.Mutex
	failingSince map[string]time.Time // plan ID -> when it started failing
}

// NewAlertEngine returns an engine for d.
func NewAlertEngine(d *Deps) *AlertEngine {
	return &AlertEngine{Deps: d, failingSince: map[string]time.Time{}}
}

// Run evaluates alerts every minute until ctx is canceled.
func (e *AlertEngine) Run(ctx context.Context) {
	ticker := time.NewTicker(AlertInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := e.Evaluate(ctx, time.Now()); err != nil && ctx.Err() == nil {
				slog.Error("evaluate alerts", "err", err)
			}
		}
	}
}

// conditions returns the alert conditions that hold now.
func (e *AlertEngine) conditions(ctx context.Context, now time.Time) ([]condition, error) {
	plans, err := e.Store.ListPlans(ctx)
	if err != nil {
		return nil, err
	}
	hosts, err := e.Store.ListHosts(ctx)
	if err != nil {
		return nil, err
	}
	hostByID := map[string]store.Host{}
	for _, h := range hosts {
		hostByID[h.ID] = h
	}

	var out []condition
	watched := map[string]bool{}
	e.mu.Lock()
	defer e.mu.Unlock()
	failing := map[string]bool{}
	for _, p := range plans {
		if p.State == store.PlanDraft {
			continue
		}
		watched[p.PrimaryHostID], watched[p.DRHostID] = true, true
		if p.State != store.PlanActive {
			continue
		}
		st, err := e.planHealth(ctx, p)
		if err != nil {
			return nil, err
		}
		h := st.Health
		spec, _ := decodeSpec(p.AppliedSpec)
		threshold := replication.RPOAlertThreshold(spec)
		if h.LastReplicationAt != nil && time.Duration(h.RpoAgeSeconds)*time.Second > threshold { //nolint:gosec // seconds fit
			out = append(out, condition{plan: p.Name, Alert: store.Alert{
				Key: "rpo:" + p.ID, Severity: "critical", PlanID: p.ID, HostID: p.DRHostID,
				Title:   "RPO exceeded for plan " + p.Name,
				Message: fmt.Sprintf("The newest replicated snapshot is %s old; the alert threshold is %s.", (time.Duration(h.RpoAgeSeconds) * time.Second).String(), threshold), //nolint:gosec // seconds fit
			}})
		}
		if h.State == portalv1.HealthState_HEALTH_STATE_FAILING {
			failing[p.ID] = true
			since, ok := e.failingSince[p.ID]
			if !ok {
				since = now
				e.failingSince[p.ID] = now
			}
			// At least two replication runs: one snapshot interval plus a
			// minute of grace.
			if now.Sub(since) >= time.Duration(spec.IntervalSeconds)*time.Second+failingGrace {
				out = append(out, condition{plan: p.Name, Alert: store.Alert{
					Key: "failing:" + p.ID, Severity: "critical", PlanID: p.ID, HostID: p.DRHostID,
					Title:   "Replication failing for plan " + p.Name,
					Message: strings.Join(uniqueLines(append([]string{h.Message}, st.Errors...)), "\n"),
				}})
			}
		}
	}
	for id := range e.failingSince {
		if !failing[id] {
			delete(e.failingSince, id)
		}
	}

	// DNS drift, from the latest check of each plan's records.
	for _, p := range plans {
		if p.State == store.PlanDraft {
			continue
		}
		b, err := e.Store.DNSCheck(ctx, p.ID)
		if err != nil {
			continue
		}
		st := &portalv1.PlanDnsStatus{}
		if proto.Unmarshal(b, st) != nil {
			continue
		}
		var lines []string
		for _, r := range st.Records {
			if r.Drift || slices.Contains(r.Problems, "the record doesn't exist in Cloudflare") {
				lines = append(lines, fmt.Sprintf("%s %s: %s", r.Name, r.Type, strings.Join(r.Problems, "; ")))
			}
		}
		if len(lines) > 0 {
			out = append(out, condition{plan: p.Name, Alert: store.Alert{
				Key: "dns:" + p.ID, Severity: "warning", PlanID: p.ID,
				Title:   "DNS records don't match plan " + p.Name,
				Message: strings.Join(lines, "\n"),
			}})
		}
	}

	for id := range watched {
		h, ok := hostByID[id]
		if !ok {
			continue
		}
		if !e.Hub.Online(id) && now.Sub(h.LastSeenAt) > offlineAfter {
			out = append(out, condition{host: h.Hostname, Alert: store.Alert{
				Key: "offline:" + id, Severity: "critical", HostID: id,
				Title:   "Host " + h.Hostname + " is offline",
				Message: fmt.Sprintf("%s hasn't connected to the portal since %s.", h.Hostname, h.LastSeenAt.Format(time.RFC1123)),
			}})
		}
		if h.ApplyError != "" {
			out = append(out, condition{host: h.Hostname, Alert: store.Alert{
				Key: "apply:" + id, Severity: "critical", HostID: id,
				Title:   "Host " + h.Hostname + " couldn't apply its replication configuration",
				Message: h.ApplyError,
			}})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// Evaluate compares current conditions with firing alerts: new conditions
// fire, ongoing ones are reminded every 12 hours, and cleared ones resolve.
func (e *AlertEngine) Evaluate(ctx context.Context, now time.Time) error {
	conds, err := e.conditions(ctx, now)
	if err != nil {
		return err
	}
	firing, err := e.Store.FiringAlerts(ctx)
	if err != nil {
		return err
	}
	byKey := map[string]store.Alert{}
	for _, a := range firing {
		byKey[a.Key] = a
	}
	holding := map[string]bool{}
	send := func(event string, a store.Alert, c condition) {
		e.notifyAll(ctx, notify.Notification{Event: event, Severity: a.Severity, Title: a.Title,
			Message: a.Message, Plan: c.plan, Host: c.host, Time: now})
	}
	for _, c := range conds {
		holding[c.Key] = true
		if a, ok := byKey[c.Key]; ok {
			remind := now.Sub(a.LastNotifiedAt) >= reminderAfter
			if err := e.Store.UpdateAlert(ctx, a.ID, c.Message, remind); err != nil {
				return err
			}
			if remind {
				a.Message = c.Message
				send("reminder", a, c)
			}
			continue
		}
		a, err := e.Store.FireAlert(ctx, c.Alert)
		if errors.Is(err, store.ErrConflict) {
			continue
		}
		if err != nil {
			return err
		}
		slog.Warn("alert firing", "key", a.Key, "title", a.Title)
		send("firing", a, c)
	}
	for _, a := range firing {
		if holding[a.Key] {
			continue
		}
		if err := e.Store.ResolveAlert(ctx, a.ID); err != nil {
			return err
		}
		slog.Info("alert resolved", "key", a.Key)
		send("resolved", a, condition{})
	}
	return nil
}

// uniqueLines drops repeated lines, keeping the first occurrence.
func uniqueLines(lines []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, l := range lines {
		if !seen[l] {
			seen[l] = true
			out = append(out, l)
		}
	}
	return out
}
