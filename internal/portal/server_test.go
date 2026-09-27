package portal

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/jlbyh2o/ezdr/internal/portal/api"
)

func NewHandler(ui fstest.MapFS) http.Handler {
	return PublicHandler(&api.Deps{}, ui)
}

func testUI() fstest.MapFS {
	return fstest.MapFS{
		"index.html":    &fstest.MapFile{Data: []byte("index")},
		"assets/app.js": &fstest.MapFile{Data: []byte("app")},
	}
}

func get(h http.Handler, target string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

func TestHealth(t *testing.T) {
	rec := get(NewHandler(testUI()), "/api/health")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got["status"] != "ok" {
		t.Errorf("status field = %q, want %q", got["status"], "ok")
	}
}

func TestUnknownAPIRouteIsNotFound(t *testing.T) {
	rec := get(NewHandler(testUI()), "/api/nope")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestStaticAndSPAFallback(t *testing.T) {
	h := NewHandler(testUI())
	tests := []struct {
		path, want string
	}{
		{"/", "index"},
		{"/assets/app.js", "app"},
		{"/plans/42", "index"},
	}
	for _, tt := range tests {
		rec := get(h, tt.path)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", tt.path, rec.Code)
			continue
		}
		if got := rec.Body.String(); !strings.Contains(got, tt.want) {
			t.Errorf("%s: body = %q, want %q", tt.path, got, tt.want)
		}
	}
}
