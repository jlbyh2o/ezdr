package dns

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeCloudflare serves the parts of Cloudflare's API that EZDR uses.
type fakeCloudflare struct {
	mu      sync.Mutex
	records map[string]*cfRecord // record ID -> record
}

func (f *fakeCloudflare) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	reply := func(result any, pages int) {
		b, _ := json.Marshal(result)
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "result": json.RawMessage(b),
			"result_info": map[string]int{"page": 1, "total_pages": pages}})
	}
	if r.Header.Get("Authorization") != "Bearer good" {
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "errors": []map[string]any{{"code": 10000, "message": "Authentication error"}}})
		return
	}
	switch {
	case r.URL.Path == "/zones":
		reply([]map[string]string{{"id": "z1", "name": "example.com"}, {"id": "z2", "name": "shop.example.com"}}, 1)
	case strings.HasSuffix(r.URL.Path, "/dns_records") && r.Method == http.MethodGet:
		var out []*cfRecord
		for _, rec := range f.records {
			if rec.Name == r.URL.Query().Get("name") && rec.Type == r.URL.Query().Get("type") {
				out = append(out, rec)
			}
		}
		reply(out, 1)
	case strings.Contains(r.URL.Path, "/dns_records/") && r.Method == http.MethodPatch:
		id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.records[id].Content = body["content"]
		reply(f.records[id], 1)
	default:
		http.NotFound(w, r)
	}
}

func TestCloudflare(t *testing.T) {
	f := &fakeCloudflare{records: map[string]*cfRecord{
		"r1": {ID: "r1", Name: "app.shop.example.com", Type: "A", Content: "203.0.113.10", TTL: 300},
		"r2": {ID: "r2", Name: "example.com", Type: "TXT", Content: `"v=spf1 ip4:203.0.113.10 -all"`, TTL: 1, Proxied: false},
	}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	ctx := context.Background()
	c := &Cloudflare{Token: "good", BaseURL: srv.URL}

	zones, err := c.Zones(ctx)
	if err != nil || len(zones) != 2 {
		t.Fatalf("zones = %v, %v", zones, err)
	}
	z, ok := ZoneFor(zones, "App.Shop.Example.com.")
	if !ok || z.ID != "z2" {
		t.Errorf("zone for app.shop = %v", z)
	}
	if _, ok := ZoneFor(zones, "example.org"); ok {
		t.Error("found a zone for another domain")
	}

	rec, err := c.Find(ctx, z, "app.shop.example.com", "a")
	if err != nil || rec == nil || rec.Content != "203.0.113.10" {
		t.Fatalf("find = %v, %v", rec, err)
	}
	if err := c.Update(ctx, z, *rec, "198.51.100.10"); err != nil {
		t.Fatal(err)
	}
	if rec, _ = c.Find(ctx, z, "app.shop.example.com", "A"); rec.Content != "198.51.100.10" {
		t.Errorf("after update: %v", rec)
	}
	if missing, err := c.Find(ctx, z, "nope.shop.example.com", "A"); missing != nil || err != nil {
		t.Errorf("missing record = %v, %v", missing, err)
	}
	txt, _ := c.Find(ctx, Zone{ID: "z1", Name: "example.com"}, "example.com", "TXT")
	if !SameValue(txt.Content, "v=spf1 ip4:203.0.113.10 -all") {
		t.Errorf("TXT values should compare without quotes: %q", txt.Content)
	}

	bad := &Cloudflare{Token: "bad", BaseURL: srv.URL}
	if _, err := bad.Zones(ctx); err == nil || !strings.Contains(err.Error(), "Authentication error (10000)") {
		t.Errorf("bad token: %v", err)
	}
}
