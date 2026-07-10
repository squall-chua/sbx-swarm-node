// Package gitprovider runs the host-side PublishWork strategies against a
// registered provider workspace's remote (ADR-0019). Derivation and strategy
// support are decided here; the git transport uses the workspace credential env.
package gitprovider

import (
	"fmt"
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

// splitRemote parses an HTTPS or scp-like SSH remote into its host and path.
// A "://" URL that parses wins; otherwise the scp-like git@host:path form is
// tried. Either component is "" when the remote yields neither.
func splitRemote(remote string) (host, path string) {
	remote = strings.TrimSpace(remote)
	if strings.Contains(remote, "://") {
		if u, err := url.Parse(remote); err == nil {
			return u.Hostname(), u.Path
		}
	}
	if _, after, ok := strings.Cut(remote, "@"); ok { // scp-like git@host:path
		if h, p, ok := strings.Cut(after, ":"); ok {
			return h, p
		}
	}
	return "", ""
}

// hostOf extracts the lowercased host from an HTTPS or scp-like SSH URL.
func hostOf(remote string) string {
	host, _ := splitRemote(remote)
	return strings.ToLower(host)
}

var strategySupport = map[Provider]map[string]bool{
	Plain:  {"branch": true, "patch": true},
	GitHub: {"branch": true, "patch": true, "pull_request": true},
	GitLab: {"branch": true, "patch": true, "merge_request": true},
	Gerrit: {"branch": true, "patch": true, "gerrit_change": true},
}

// Supports reports whether this provider can run the given publish strategy.
func (p Provider) Supports(strategy string) bool { return strategySupport[p][strategy] }

// pathOf extracts the path (no host) from an HTTPS or scp-like SSH URL.
func pathOf(remote string) string {
	_, path := splitRemote(remote)
	return path
}

// APIBase returns the REST API base URL for a provider. override wins. GitHub
// derives api.github.com (public) or HOST/api/v3 (enterprise); GitLab derives
// HOST/api/v4 (public and self-hosted). Gerrit (git push, no REST) and plain
// return "".
func APIBase(p Provider, remoteURL, override string) string {
	if override != "" {
		return override
	}
	host := hostOf(remoteURL)
	switch p {
	case GitHub:
		if host == "github.com" {
			return "https://api.github.com"
		}
		return "https://" + host + "/api/v3"
	case GitLab:
		return "https://" + host + "/api/v4"
	default:
		return ""
	}
}

// ParseRepo extracts the repo identity from a remote URL. GitHub requires exactly
// owner/repo (both returned). GitLab returns the whole project path as repo (may
// be nested subgroups) with an empty owner; the caller URL-encodes it. A remote
// that does not yield a valid identity is an error (rejected before any mutation).
func ParseRepo(p Provider, remoteURL string) (owner, repo string, err error) {
	path := strings.Trim(pathOf(remoteURL), "/")
	path = strings.TrimSuffix(path, ".git")
	if path == "" {
		return "", "", fmt.Errorf("cannot parse owner/repo from remote_url")
	}
	if p == GitLab {
		return "", path, nil
	}
	parts := strings.Split(path, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("cannot parse owner/repo from remote_url")
	}
	return parts[0], parts[1], nil
}
