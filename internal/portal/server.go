// Package portal implements the EZDR portal server.
package portal

import (
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"path"
	"strings"

	"connectrpc.com/connect"

	"github.com/jlbyh2o/ezdr/internal/gen/ezdr/client/v1/clientv1connect"
	"github.com/jlbyh2o/ezdr/internal/gen/ezdr/enroll/v1/enrollv1connect"
	"github.com/jlbyh2o/ezdr/internal/gen/ezdr/portal/v1/portalv1connect"
	"github.com/jlbyh2o/ezdr/internal/portal/api"
	"github.com/jlbyh2o/ezdr/internal/version"
)

// publicProcedures can be called without signing in.
var publicProcedures = []string{
	portalv1connect.SetupServiceGetSetupStatusProcedure,
	portalv1connect.SetupServiceCompleteSetupProcedure,
	portalv1connect.AuthServiceLoginProcedure,
	portalv1connect.AuthServiceVerifyTotpProcedure,
	portalv1connect.AuthServiceLogoutProcedure,
	portalv1connect.AuthServiceGetCurrentUserProcedure,
}

// PublicHandler serves the web UI, the user API, and the enrollment API.
func PublicHandler(d *api.Deps, ui fs.FS) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", handleHealth)
	mux.Handle("/api/", http.NotFoundHandler())

	// Limits apply after decompression, and requests are read before the
	// sign-in check.
	mux.Handle(enrollv1connect.NewEnrollmentServiceHandler(api.EnrollmentService{Deps: d},
		connect.WithReadMaxBytes(maxEnrollRequest)))

	userAPI := connect.WithHandlerOptions(connect.WithInterceptors(api.RequireUser(publicProcedures...)),
		connect.WithReadMaxBytes(maxUserRequest))
	for _, register := range []func() (string, http.Handler){
		func() (string, http.Handler) {
			return portalv1connect.NewSetupServiceHandler(api.SetupService{Deps: d}, userAPI)
		},
		func() (string, http.Handler) {
			return portalv1connect.NewAuthServiceHandler(api.AuthService{Deps: d}, userAPI)
		},
		func() (string, http.Handler) {
			return portalv1connect.NewTokenServiceHandler(api.TokenService{Deps: d}, userAPI)
		},
		func() (string, http.Handler) {
			return portalv1connect.NewHostServiceHandler(api.HostService{Deps: d}, userAPI)
		},
		func() (string, http.Handler) {
			return portalv1connect.NewAuditServiceHandler(api.AuditService{Deps: d}, userAPI)
		},
		func() (string, http.Handler) {
			return portalv1connect.NewPlanServiceHandler(api.PlanService{Deps: d}, userAPI)
		},
		func() (string, http.Handler) {
			return portalv1connect.NewAlertServiceHandler(api.AlertService{Deps: d}, userAPI)
		},
		func() (string, http.Handler) {
			return portalv1connect.NewTestFailoverServiceHandler(api.TestFailoverService{Deps: d}, userAPI)
		},
		func() (string, http.Handler) {
			return portalv1connect.NewFailoverServiceHandler(api.FailoverService{Deps: d}, userAPI)
		},
		func() (string, http.Handler) {
			return portalv1connect.NewDnsServiceHandler(api.DNSService{Deps: d}, userAPI)
		},
		func() (string, http.Handler) {
			return portalv1connect.NewOverviewServiceHandler(api.OverviewService{Deps: d}, userAPI)
		},
	} {
		p, h := register()
		mux.Handle(p, api.WithSameOrigin(api.WithSession(d.Store, h)))
	}

	mux.Handle("/", spaHandler(ui))
	return withSecurityHeaders(withBodyLimit(mux))
}

const (
	maxEnrollRequest = 64 << 10
	// Plans with many guests are the largest requests.
	maxUserRequest = 1 << 20
)

// withBodyLimit caps request bodies as sent (compressed or not).
func withBodyLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxUserRequest)
		next.ServeHTTP(w, r)
	})
}

// TunnelHandler serves the client API. It must only be served on the tunnel
// listener.
func TunnelHandler(d *api.Deps) http.Handler {
	mux := http.NewServeMux()
	mux.Handle(clientv1connect.NewClientServiceHandler(api.ClientService{Deps: d},
		connect.WithReadMaxBytes(16<<20))) // inventories of large hosts
	return api.WithTunnelHost(d.Store, mux)
}

func handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status":  "ok",
		"version": version.Version,
	})
}

func withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy",
			"default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; "+
				"frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		next.ServeHTTP(w, r)
	})
}

// spaHandler serves static files from ui, falling back to index.html for
// unknown paths so client-side routes work on reload.
func spaHandler(ui fs.FS) http.Handler {
	files := http.FileServerFS(ui)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only page loads fall back to the UI; anything else (such as a POST
		// to an unknown API path) is a plain 404.
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.NotFound(w, r)
			return
		}
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name == "" {
			name = "."
		}
		if _, err := fs.Stat(ui, name); errors.Is(err, fs.ErrNotExist) {
			http.ServeFileFS(w, r, ui, "index.html")
			return
		}
		files.ServeHTTP(w, r)
	})
}
