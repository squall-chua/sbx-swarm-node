// Package imageref parses and normalizes Docker image references, the way
// both the scheduler and the fake backend need to.
package imageref

import "strings"

// Pullable reports whether a template reference names a registry, so any node's
// daemon can fetch it. This is Docker's own rule: the first path component is a
// registry host when it contains a "." or a ":", or is exactly "localhost".
//
// "org/img:1" is deliberately NOT pullable here. Docker reads it as a Docker Hub
// image, but it is ambiguous with a locally saved two-part tag, so the rule errs
// toward refusing placement instead of assuming a pull (ADR-0024). Write
// "docker.io/org/img:1" to make it travel.
func Pullable(ref string) bool {
	i := strings.Index(ref, "/")
	if i < 0 {
		return false
	}
	host := ref[:i]
	return host == "localhost" || strings.ContainsAny(host, ".:")
}

// Canonical returns the reference the way the daemon reports it in its image list.
// The daemon canonicalizes an unqualified repository the way Docker does, so a
// template saved as "myimage:v1" is listed back as "docker.io/library/myimage:v1"
// (proven live in TestSDKBackend_SaveRemoveTemplate). A node advertises what the
// daemon lists, so the request has to be canonicalized before it is matched.
//
// Only a reference that names no registry is registry-qualified. Anything
// pullable is already qualified and is left alone there.
//
// Docker also defaults a missing tag to "latest", so a reference with no tag on
// its last path segment gets one appended. The tag check looks only at the last
// path segment, so a registry host's port colon (as in "localhost:5000/img")
// is never mistaken for a tag.
func Canonical(ref string) string {
	out := ref
	if !Pullable(ref) {
		if strings.Contains(ref, "/") {
			out = "docker.io/" + ref
		} else {
			out = "docker.io/library/" + ref
		}
	}
	last := out
	if i := strings.LastIndex(out, "/"); i >= 0 {
		last = out[i+1:]
	}
	if !strings.Contains(last, ":") {
		out += ":latest"
	}
	return out
}

// SplitRepoTag splits a ref into repository and tag at the LAST colon, but
// only when that colon falls in the last path segment. This keeps a registry
// host's port (e.g. "localhost:5000/img") from being mistaken for a tag,
// while still splitting "localhost:5000/img:1" as repository "localhost:5000/img"
// and tag "1". A digest reference (e.g. "myimage@sha256:abc") has no tag, so
// an "@" in the last path segment is left alone.
func SplitRepoTag(ref string) (repo, tag string) {
	prefix, last := "", ref
	if i := strings.LastIndexByte(ref, '/'); i >= 0 {
		prefix, last = ref[:i+1], ref[i+1:]
	}
	if strings.Contains(last, "@") {
		return ref, ""
	}
	if i := strings.LastIndexByte(last, ':'); i >= 0 {
		return prefix + last[:i], last[i+1:]
	}
	return ref, ""
}
