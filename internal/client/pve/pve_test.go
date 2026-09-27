package pve

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClientCertificateMatch(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "PVEAPIToken=ezdr@pve!inventory=secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"data":{"version":"9.2.20"}}`))
	}))
	defer srv.Close()
	good := srv.Certificate().Raw

	var v struct {
		Version string `json:"version"`
	}
	c := newClient(srv.URL+"/", "ezdr@pve!inventory=secret", [][]byte{[]byte("other"), good})
	if err := c.Get(context.Background(), "version", &v); err != nil || v.Version != "9.2.20" {
		t.Fatalf("Get = %+v, %v", v, err)
	}

	wrong := newClient(srv.URL+"/", "ezdr@pve!inventory=secret", [][]byte{[]byte("other")})
	if err := wrong.Get(context.Background(), "version", &v); err == nil || !strings.Contains(err.Error(), "unexpected certificate") {
		t.Fatalf("unexpected certificate accepted: %v", err)
	}

	badToken := newClient(srv.URL+"/", "ezdr@pve!inventory=nope", [][]byte{good})
	if err := badToken.Get(context.Background(), "version", &v); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("bad token: err = %v", err)
	}
}
