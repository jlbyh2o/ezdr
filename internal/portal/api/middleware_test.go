package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"connectrpc.com/connect"
)

func TestSourceAddress(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("172.16.0.0/12")}
	var got netip.Addr
	h := WithSourceAddress(trusted, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = SourceAddress(r.Context())
	}))

	for _, tc := range []struct {
		name, remote, xff, want string
	}{
		{"direct client", "203.0.113.5:1234", "", "203.0.113.5"},
		{"untrusted peer cannot spoof", "203.0.113.5:1234", "198.51.100.1", "203.0.113.5"},
		{"trusted proxy", "172.18.0.2:1234", "198.51.100.1", "198.51.100.1"},
		{"rightmost untrusted hop wins", "172.18.0.2:1234", "10.9.9.9, 198.51.100.1", "198.51.100.1"},
		{"chained trusted proxies", "172.18.0.2:1234", "198.51.100.1, 172.18.0.3", "198.51.100.1"},
	} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = tc.remote
		if tc.xff != "" {
			req.Header.Set("X-Forwarded-For", tc.xff)
		}
		h.ServeHTTP(httptest.NewRecorder(), req)
		if got.String() != tc.want {
			t.Errorf("%s: source = %s, want %s", tc.name, got, tc.want)
		}
	}
}

// specConn is a streaming connection that only knows its procedure.
type specConn struct {
	connect.StreamingHandlerConn
	procedure string
}

func (c specConn) Spec() connect.Spec { return connect.Spec{Procedure: c.procedure} }

func TestRequireUserStreaming(t *testing.T) {
	var called bool
	h := RequireUser("/public").WrapStreamingHandler(func(context.Context, connect.StreamingHandlerConn) error {
		called = true
		return nil
	})
	if err := h(context.Background(), specConn{procedure: "/private"}); connect.CodeOf(err) != connect.CodeUnauthenticated || called {
		t.Errorf("signed-out stream: %v, called %v", err, called)
	}
	if err := h(context.Background(), specConn{procedure: "/public"}); err != nil || !called {
		t.Errorf("public stream: %v, called %v", err, called)
	}
}
