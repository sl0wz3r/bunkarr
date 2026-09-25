// Package web embeds the built single-page app (web/dist, produced by `make web`) into the
// Bunkarr binary. In a Go-only build dist holds just .gitkeep and the server says the UI is not
// built (the API still works).
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// FS returns the SPA files rooted at dist.
func FS() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err) // unreachable: "dist" is always embedded
	}
	return sub
}
