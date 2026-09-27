//go:build webui

// Package web provides the portal's web UI assets.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// Built reports whether the real web UI is embedded in this binary.
const Built = true

// FS returns the built web UI.
func FS() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err)
	}
	return sub
}
