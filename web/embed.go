// Package web embeds the built SPA assets served by the node.
package web

import (
	"embed"
	"io/fs"
)

// all: includes files whose names start with "_" or "." — Nuxt emits all its
// JS/CSS under dist/_nuxt/, which a plain //go:embed dist would silently drop.
//
//go:embed all:dist
var dist embed.FS

// FS returns the embedded SPA file system rooted at the dist directory.
func FS() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err) // dist is embedded at build time; this cannot fail
	}
	return sub
}
