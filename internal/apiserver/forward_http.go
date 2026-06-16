package apiserver

import (
	"crypto/tls"
	"net/http"
	"net/http/httputil"
	"strings"

	"github.com/squall-chua/sbx-swarm-node/internal/routing"
)

// OwnerProxy reverse-proxies REST requests for a remote sandbox to its owning
// node. It covers both unary REST (the in-process gateway never traverses the
// gRPC forwarding interceptor) and SSE streams (logs/stats), and naturally
// preserves the caller's Authorization/Cookie/Idempotency-Key headers so the
// owner re-authenticates the request.
//
// Paths handled: /v1/sandboxes/{id} and /v1/sandboxes/{id}/... . The {id} is the
// first path segment after /v1/sandboxes/. When the id is routable (self-routing
// "<node>.<ulid>") and not local, the request is proxied to the owner; otherwise
// it falls through to next (local handler → proper 404 for unknown ids).
func OwnerProxy(tbl *routing.Table, next http.Handler) http.Handler {
	if tbl == nil {
		return next
	}
	// v1 trusted-network only; peer identity/MITM protection pending node-key
	// challenge auth (ADR-0004).
	transport := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := sandboxPathID(r.URL.Path)
		if !ok || !strings.Contains(id, ".") || tbl.IsLocal(id) {
			next.ServeHTTP(w, r)
			return
		}
		owner, found := tbl.Owner(id)
		if !found {
			next.ServeHTTP(w, r)
			return
		}
		addr, ok := tbl.Addr(owner)
		if !ok {
			next.ServeHTTP(w, r) // unknown owner addr → local handler returns 404
			return
		}

		proxy := &httputil.ReverseProxy{
			Director: func(req *http.Request) {
				req.URL.Scheme = "https"
				req.URL.Host = addr
				req.Host = addr
			},
			Transport: transport,
			// Flush SSE frames immediately rather than buffering.
			FlushInterval: -1,
		}
		proxy.ServeHTTP(w, r)
	})
}

// sandboxPathID extracts the {id} segment from /v1/sandboxes/{id} or
// /v1/sandboxes/{id}/<suffix>. Returns false if the path is not under
// /v1/sandboxes/ or has no id segment.
func sandboxPathID(urlPath string) (string, bool) {
	const prefix = "/v1/sandboxes/"
	if !strings.HasPrefix(urlPath, prefix) {
		return "", false
	}
	rest := strings.TrimPrefix(urlPath, prefix)
	if rest == "" {
		return "", false
	}
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rest = rest[:i]
	}
	if rest == "" {
		return "", false
	}
	return rest, true
}
