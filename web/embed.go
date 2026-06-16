// Package web embeds the built SPA assets served by the node.
package web

import (
	"embed"
	"io/fs"
)

//go:embed dist
var dist embed.FS

// FS returns the embedded SPA file system rooted at the dist directory.
func FS() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err) // dist is embedded at build time; this cannot fail
	}
	return sub
}
