package imageref

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPullable(t *testing.T) {
	cases := map[string]bool{
		"ghcr.io/org/img:1":    true,  // registry host: has a dot
		"localhost:5000/img:1": true,  // registry host: localhost
		"registry:5000/img:1":  true,  // registry host: has a colon
		"myimage:v1":           false, // bare tag: only where it was saved
		"org/img:1":            false, // Docker Hub shorthand, deliberately bare
		"alpine":               false,
	}
	for ref, want := range cases {
		require.Equal(t, want, Pullable(ref), ref)
	}
}

func TestCanonical(t *testing.T) {
	cases := map[string]string{
		"myimage:v1":           "docker.io/library/myimage:v1",     // bare tag, as the daemon reports it
		"org/img:1":            "docker.io/org/img:1",              // Docker Hub shorthand
		"ghcr.io/org/img:1":    "ghcr.io/org/img:1",                // already qualified: unchanged
		"localhost:5000/img:1": "localhost:5000/img:1",             // already qualified: unchanged
		"myimage":              "docker.io/library/myimage:latest", // no tag: Docker defaults to latest
		"localhost:5000/img":   "localhost:5000/img:latest",        // registry port colon is not a tag
	}
	for in, want := range cases {
		require.Equal(t, want, Canonical(in), in)
	}
}

// TestSplitRepoTag covers directly what was previously only exercised
// indirectly through Fake.ListTemplateInfo (sandbox package).
func TestSplitRepoTag(t *testing.T) {
	cases := []struct {
		ref       string
		repo, tag string
	}{
		{"myimage:v1", "myimage", "v1"},
		{"localhost:5000/img:1", "localhost:5000/img", "1"}, // port colon is not the tag split
		{"myimage@sha256:abc", "myimage@sha256:abc", ""},    // digest has no tag
		{"myimage", "myimage", ""},                          // no tag at all
	}
	for _, c := range cases {
		repo, tag := SplitRepoTag(c.ref)
		require.Equal(t, c.repo, repo, c.ref)
		require.Equal(t, c.tag, tag, c.ref)
	}
}
