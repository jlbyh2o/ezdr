//go:build !webui

// Package web provides the portal's web UI assets.
//
// Without the webui build tag, a placeholder page is served instead of the
// real UI, so the Go code can be built and tested without a Node.js toolchain.
// Build the UI with `make web` and the portal with `make build` to embed it.
package web

import (
	"io/fs"
	"testing/fstest"
)

// Built reports whether the real web UI is embedded in this binary.
const Built = false

const placeholder = `<!doctype html>
<html lang="en">
  <head><meta charset="UTF-8" /><title>EZDR</title></head>
  <body>
    <h1>EZDR</h1>
    <p>This portal binary was built without the web UI. Run <code>make build</code> to include it.</p>
  </body>
</html>
`

// FS returns a placeholder file system containing only index.html.
func FS() fs.FS {
	return fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte(placeholder)},
	}
}
