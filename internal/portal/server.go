// Package portal implements the EZDR portal's HTTP server.
package portal

import (
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"path"
	"strings"

	"github.com/jlbyh2o/ezdr/internal/version"
)

// NewHandler returns the portal's root HTTP handler. The ui file system must
// contain an index.html at its root.
func NewHandler(ui fs.FS) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", handleHealth)
	mux.Handle("/api/", http.NotFoundHandler())
	mux.Handle("/", spaHandler(ui))
	return mux
}

func handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status":  "ok",
		"version": version.Version,
	})
}

// spaHandler serves static files from ui, falling back to index.html for
// unknown paths so client-side routes work on reload.
func spaHandler(ui fs.FS) http.Handler {
	files := http.FileServerFS(ui)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
