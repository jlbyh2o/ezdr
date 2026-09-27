// Package notify sends alert notifications by email and webhook.
package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Notification is one alert event.
type Notification struct {
	// Event is "firing", "resolved", "reminder", or "test".
	Event    string    `json:"event"`
	Severity string    `json:"severity"`
	Title    string    `json:"title"`
	Message  string    `json:"message"`
	Plan     string    `json:"plan,omitempty"`
	Host     string    `json:"host,omitempty"`
	Time     time.Time `json:"time"`
	// PortalURL links back to the portal.
	PortalURL string `json:"portal_url,omitempty"`
}

// Security modes for SMTP.
const (
	SecurityStartTLS = "starttls"
	SecurityTLS      = "tls"
	SecurityNone     = "none"
)

// SMTP is an email channel.
type SMTP struct {
	Host       string
	Port       int
	Security   string
	Username   string
	Password   string
	From       string
	Recipients []string
	// TLSConfig is for tests; nil uses the system roots.
	TLSConfig *tls.Config
}

// Validate checks the channel's settings.
func (s SMTP) Validate() error {
	if s.Host == "" || strings.ContainsAny(s.Host, " /:") {
		return errors.New("enter the SMTP server's host name")
	}
	if s.Port < 1 || s.Port > 65535 {
		return errors.New("enter a valid SMTP port")
	}
	switch s.Security {
	case SecurityStartTLS, SecurityTLS, SecurityNone:
	default:
		return errors.New("choose SMTP security")
	}
	if _, err := mail.ParseAddress(s.From); err != nil {
		return fmt.Errorf("invalid sender address: %w", err)
	}
	if len(s.Recipients) == 0 {
		return errors.New("add at least one recipient")
	}
	for _, r := range s.Recipients {
		if _, err := mail.ParseAddress(r); err != nil {
			return fmt.Errorf("invalid recipient %q", r)
		}
	}
	return nil
}

// oneLine removes line breaks, so values can't inject headers.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// Subject is the email subject for a notification.
func Subject(n Notification) string {
	prefix := map[string]string{"resolved": "[EZDR] Resolved: ", "reminder": "[EZDR] Still firing: ", "test": "[EZDR] "}[n.Event]
	if prefix == "" {
		prefix = "[EZDR] " + strings.ToUpper(n.Severity) + ": "
	}
	return oneLine(prefix + n.Title)
}

// SendEmail sends n to the channel's recipients.
func (s SMTP) SendEmail(ctx context.Context, n Notification) error {
	if err := s.Validate(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	addr := net.JoinHostPort(s.Host, strconv.Itoa(s.Port))
	tlsConfig := s.TLSConfig
	if tlsConfig == nil {
		tlsConfig = &tls.Config{ServerName: s.Host, MinVersion: tls.VersionTLS12}
	}

	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if s.Security == SecurityTLS {
		conn = tls.Client(conn, tlsConfig)
	}
	c, err := smtp.NewClient(conn, s.Host)
	if err != nil {
		_ = conn.Close()
		return err
	}
	defer func() { _ = c.Close() }()
	if s.Security == SecurityStartTLS {
		if err := c.StartTLS(tlsConfig); err != nil {
			return fmt.Errorf("STARTTLS: %w", err)
		}
	}
	if s.Username != "" {
		if err := c.Auth(smtp.PlainAuth("", s.Username, s.Password, s.Host)); err != nil {
			return fmt.Errorf("authenticate: %w", err)
		}
	}
	from, _ := mail.ParseAddress(s.From)
	if err := c.Mail(from.Address); err != nil {
		return err
	}
	var to []string
	for _, r := range s.Recipients {
		a, _ := mail.ParseAddress(r)
		if err := c.Rcpt(a.Address); err != nil {
			return fmt.Errorf("recipient %s: %w", a.Address, err)
		}
		to = append(to, a.String())
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	var body bytes.Buffer
	fmt.Fprintf(&body, "From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nMIME-Version: 1.0\r\n"+
		"Content-Type: text/plain; charset=utf-8\r\n\r\n",
		from.String(), strings.Join(to, ", "), Subject(n), n.Time.Format(time.RFC1123Z))
	fmt.Fprintf(&body, "%s\r\n\r\n", strings.ReplaceAll(n.Message, "\n", "\r\n"))
	if n.Plan != "" {
		fmt.Fprintf(&body, "Plan: %s\r\n", oneLine(n.Plan))
	}
	if n.Host != "" {
		fmt.Fprintf(&body, "Host: %s\r\n", oneLine(n.Host))
	}
	fmt.Fprintf(&body, "Time: %s\r\n", n.Time.UTC().Format(time.RFC3339))
	if n.PortalURL != "" {
		fmt.Fprintf(&body, "\r\n%s\r\n", n.PortalURL)
	}
	if _, err := w.Write(body.Bytes()); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

// Webhook is a webhook channel.
type Webhook struct {
	URL    string
	Secret string
	Client *http.Client
}

// ValidateURL checks a webhook URL.
func ValidateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return fmt.Errorf("invalid webhook URL %q", raw)
	}
	return nil
}

// SignatureHeader carries the HMAC-SHA256 of the request body, as
// "sha256=<hex>", when the webhook has a secret.
const SignatureHeader = "X-EZDR-Signature"

// Sign returns the signature header value for body.
func Sign(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// Send posts n as JSON.
func (w Webhook) Send(ctx context.Context, n Notification) error {
	if err := ValidateURL(w.URL); err != nil {
		return err
	}
	body, err := json.Marshal(n)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "EZDR")
	if w.Secret != "" {
		req.Header.Set(SignatureHeader, Sign(w.Secret, body))
	}
	client := w.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("webhook returned %s", resp.Status)
	}
	return nil
}
