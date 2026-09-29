package notify

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

var sample = Notification{Event: "firing", Severity: "critical", Title: "RPO exceeded for Main",
	Message: "newest replicated snapshot is 20m old", Plan: "Main", Host: "pve2", Time: time.Unix(1790000000, 0)}

func TestWebhook(t *testing.T) {
	var got Notification
	var sig string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		sig = r.Header.Get(SignatureHeader)
		if sig != Sign("s3cret", body) {
			http.Error(w, "bad signature", http.StatusUnauthorized)
			return
		}
		_ = json.Unmarshal(body, &got)
	}))
	defer srv.Close()

	if err := (Webhook{URL: srv.URL, Secret: "s3cret"}).Send(context.Background(), sample); err != nil {
		t.Fatal(err)
	}
	if got.Title != sample.Title || got.Event != "firing" || !strings.HasPrefix(sig, "sha256=") {
		t.Errorf("received %+v, signature %q", got, sig)
	}
	if err := (Webhook{URL: srv.URL, Secret: "wrong"}).Send(context.Background(), sample); err == nil {
		t.Error("expected an error for a rejected webhook")
	}
	if err := ValidateURL("ftp://example.com"); err == nil {
		t.Error("ftp URL accepted")
	}

	// Redirects aren't followed.
	var followed bool
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { followed = true }))
	defer target.Close()
	redirect := httptest.NewServer(http.RedirectHandler(target.URL, http.StatusTemporaryRedirect))
	defer redirect.Close()
	if err := (Webhook{URL: redirect.URL}).Send(context.Background(), sample); err == nil || followed {
		t.Errorf("redirect followed: %v", err)
	}
}

// fakeSMTP accepts one message and returns what it received.
func fakeSMTP(t *testing.T) (addr string, received chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	received = make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close(); _ = ln.Close() }()
		r := bufio.NewReader(conn)
		say := func(s string) { _, _ = conn.Write([]byte(s + "\r\n")) }
		say("220 fake ESMTP")
		var data strings.Builder
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			cmd := strings.ToUpper(strings.TrimSpace(line))
			switch {
			case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
				say("250 fake")
			case strings.HasPrefix(cmd, "MAIL"), strings.HasPrefix(cmd, "RCPT"):
				data.WriteString(strings.TrimSpace(line) + "\n")
				say("250 ok")
			case cmd == "DATA":
				say("354 go ahead")
				for {
					l, err := r.ReadString('\n')
					if err != nil || l == ".\r\n" {
						break
					}
					data.WriteString(l)
				}
				say("250 queued")
			case cmd == "QUIT":
				say("221 bye")
				received <- data.String()
				return
			default:
				say("250 ok")
			}
		}
	}()
	return ln.Addr().String(), received
}

func TestSendEmail(t *testing.T) {
	addr, received := fakeSMTP(t)
	host, port, _ := net.SplitHostPort(addr)
	p, _ := strconv.Atoi(port)
	s := SMTP{Host: host, Port: p, Security: SecurityNone, From: "EZDR <ezdr@example.com>",
		Recipients: []string{"ops@example.com"}}
	n := sample
	n.Title = "RPO exceeded\r\nBcc: attacker@example.com"
	if err := s.SendEmail(context.Background(), n); err != nil {
		t.Fatal(err)
	}
	msg := <-received
	for _, want := range []string{"MAIL FROM:<ezdr@example.com>", "RCPT TO:<ops@example.com>",
		"Subject: [EZDR] CRITICAL: RPO exceeded Bcc: attacker@example.com", "newest replicated snapshot"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "\r\nBcc:") {
		t.Error("header injection through the title")
	}
}

func TestSMTPValidate(t *testing.T) {
	good := SMTP{Host: "smtp.example.com", Port: 587, Security: SecurityStartTLS, From: "a@example.com", Recipients: []string{"b@example.com"}}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*SMTP){
		"host":     func(s *SMTP) { s.Host = "" },
		"port":     func(s *SMTP) { s.Port = 0 },
		"security": func(s *SMTP) { s.Security = "maybe" },
		"from":     func(s *SMTP) { s.From = "not an address" },
		"no rcpt":  func(s *SMTP) { s.Recipients = nil },
		"bad rcpt": func(s *SMTP) { s.Recipients = []string{"x"} },
	} {
		s := good
		change(&s)
		if s.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
