// Package gitprovider runs the host-side PublishWork strategies against a
// registered provider workspace's remote (ADR-0019). Derivation and strategy
// support are decided here; the git transport uses the workspace credential env.
package gitprovider

import (
	"net/url"
	"strings"
)

type Provider string

const (
	GitHub Provider = "github"
	GitLab Provider = "gitlab"
	Gerrit Provider = "gerrit"
	Plain  Provider = "plain"
)

// Derive resolves the provider. Explicit config always wins; otherwise only the
// two obvious public hosts are recognized, everything else is Plain (self-hosted
// GitLab/Gerrit require an explicit provider). See design Q5.
func Derive(remoteURL, explicit string) Provider {
	switch Provider(strings.ToLower(strings.TrimSpace(explicit))) {
	case GitHub, GitLab, Gerrit, Plain:
		return Provider(strings.ToLower(strings.TrimSpace(explicit)))
	}
	host := hostOf(remoteURL)
	switch {
	case host == "github.com":
		return GitHub
	case strings.Contains(host, "gitlab"):
		return GitLab
	default:
		return Plain
	}
}

// hostOf extracts the host from an HTTPS or scp-like SSH URL.
func hostOf(remote string) string {
	remote = strings.TrimSpace(remote)
	if strings.Contains(remote, "://") {
		if u, err := url.Parse(remote); err == nil {
			return strings.ToLower(u.Hostname())
		}
	}
	// scp-like: git@host:path
	if _, after, ok := strings.Cut(remote, "@"); ok {
		if host, _, ok := strings.Cut(after, ":"); ok {
			return strings.ToLower(host)
		}
	}
	return ""
}

var strategySupport = map[Provider]map[string]bool{
	Plain:  {"branch": true, "patch": true},
	GitHub: {"branch": true, "patch": true, "pull_request": true},
	GitLab: {"branch": true, "patch": true, "merge_request": true},
	Gerrit: {"branch": true, "patch": true, "gerrit_change": true},
}

// Supports reports whether this provider can run the given publish strategy.
func (p Provider) Supports(strategy string) bool { return strategySupport[p][strategy] }
