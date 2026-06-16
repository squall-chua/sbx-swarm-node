package apiserver

import (
	"crypto"
	"crypto/tls"
	"net/http"
	"net/http/httputil"
	"strings"

	"github.com/squall-chua/sbx-swarm-node/internal/routing"
	"github.com/squall-chua/sbx-swarm-node/internal/tlsutil"
)

// PinResolver returns the expected peer public key for a node id (TLS pin).
type PinResolver func(nodeID string) (crypto.PublicKey, bool)

// OwnerProxy reverse-proxies REST/SSE for a remote sandbox to its owning node,
// pinning the owner's TLS leaf cert to its gossiped pubkey (ADR-0004). Fail-closed
// (502) when no pin is known.
func OwnerProxy(tbl *routing.Table, pins PinResolver, next http.Handler) http.Handler {
	if tbl == nil {
		return next
	}
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
			next.ServeHTTP(w, r)
			return
		}
		pin, ok := pins(owner)
		if !ok {
			http.Error(w, "no TLS pin known for owner node", http.StatusBadGateway)
			return
		}
		transport := &http.Transport{TLSClientConfig: &tls.Config{
			InsecureSkipVerify:    true, //nolint:gosec // pin is enforced below
			VerifyPeerCertificate: tlsutil.PinnedVerify(pin),
		}}
		proxy := &httputil.ReverseProxy{
			Director: func(req *http.Request) {
				req.URL.Scheme = "https"
				req.URL.Host = addr
				req.Host = addr
			},
			Transport:     transport,
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
