package apiserver

import (
	"io/fs"
	"testing"

	"github.com/squall-chua/sbx-swarm-node/web"
	"github.com/stretchr/testify/require"
)

// dist/ is gitignored except a tracked empty .gitkeep, so a placeholder-only
// tree has no index.html. Like the _nuxt check below, this is only meaningful
// against a real build and skips rather than failing CI on the bare tree.
func TestEmbeddedSPA_HasIndex(t *testing.T) {
	if _, err := fs.Stat(web.FS(), "index.html"); err != nil {
		t.Skip("no index.html in embedded FS (placeholder-only tree; run web/scripts/build.sh for a real build)")
	}
}

// Nuxt emits every JS/CSS asset under dist/_nuxt/. A plain //go:embed dist
// silently drops "_"-prefixed paths, so the binary would serve index.html but
// 404 all assets (blank page). The embed.go directive must be `all:dist`.
// Only meaningful against a real build (the committed placeholder has no
// _nuxt/), so this skips on the placeholder-only tree rather than failing CI.
func TestEmbeddedSPA_IncludesNuxtAssets(t *testing.T) {
	if _, err := fs.Stat(web.FS(), "_nuxt"); err != nil {
		t.Skip("no _nuxt/ in embedded FS (placeholder-only tree; run web/scripts/build.sh for a real build)")
	}
	entries, err := fs.ReadDir(web.FS(), "_nuxt")
	require.NoError(t, err)
	require.NotEmpty(t, entries, "_nuxt/ embedded but empty — embed.go must use `//go:embed all:dist`")
}
