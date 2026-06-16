package apiserver

import (
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/squall-chua/sbx-swarm-node/internal/routing"
	"github.com/stretchr/testify/require"
)

func TestSandboxPathID(t *testing.T) {
	cases := []struct {
		path   string
		want   string
		wantOK bool
	}{
		{"/v1/sandboxes/nB.abc", "nB.abc", true},
		{"/v1/sandboxes/nB.abc/logs", "nB.abc", true},
		{"/v1/sandboxes/nB.abc/stats", "nB.abc", true},
		{"/v1/sandboxes", "", false},
		{"/v1/sandboxes/", "", false},
		{"/v1/events", "", false},
		{"/healthz", "", false},
	}
	for _, c := range cases {
		got, ok := sandboxPathID(c.path)
		require.Equal(t, c.wantOK, ok, c.path)
		require.Equal(t, c.want, got, c.path)
	}
}

// localSentinel is a next-handler that records it was called and writes 599.
func localSentinel(called *bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		*called = true
		w.WriteHeader(599)
	})
}

func TestOwnerProxy_LocalFallsThrough(t *testing.T) {
	tbl := routing.NewTable("nA")
	called := false
	noPin := func(string) (crypto.PublicKey, bool) { return nil, false }
	h := OwnerProxy(tbl, noPin, localSentinel(&called))

	// Local id (owner == self) → next handler.
	req := httptest.NewRequest(http.MethodGet, "/v1/sandboxes/nA.abc", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	require.True(t, called)
	require.Equal(t, 599, rr.Code)
}

func TestOwnerProxy_UnknownOwnerFallsThrough(t *testing.T) {
	tbl := routing.NewTable("nA")
	// nB owner is routable but its addr is unknown → fall through to local 404.
	called := false
	noPin := func(string) (crypto.PublicKey, bool) { return nil, false }
	h := OwnerProxy(tbl, noPin, localSentinel(&called))

	req := httptest.NewRequest(http.MethodGet, "/v1/sandboxes/nB.abc", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	require.True(t, called)
	require.Equal(t, 599, rr.Code)
}

func TestOwnerProxy_NonRoutableFallsThrough(t *testing.T) {
	tbl := routing.NewTable("nA")
	called := false
	noPin := func(string) (crypto.PublicKey, bool) { return nil, false }
	h := OwnerProxy(tbl, noPin, localSentinel(&called))

	// Id without a dot is not self-routing → local.
	req := httptest.NewRequest(http.MethodGet, "/v1/sandboxes/plainid", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	require.True(t, called)
}

func TestOwnerProxy_PinnedForwardToOwner(t *testing.T) {
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer tok", r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()
	addr := strings.TrimPrefix(backend.URL, "https://")

	leafPub := backend.Certificate().PublicKey
	tbl := routing.NewTable("nA")
	tbl.Upsert("nB", addr, false, nil)

	resolver := func(nodeID string) (crypto.PublicKey, bool) {
		if nodeID == "nB" {
			return leafPub, true
		}
		return nil, false
	}
	h := OwnerProxy(tbl, resolver, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("should have proxied")
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/sandboxes/nB.abc", nil)
	req.Header.Set("Authorization", "Bearer tok")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)
	require.Equal(t, "ok", rr.Body.String())
}

func TestOwnerProxy_PinMismatchFailsClosed(t *testing.T) {
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()
	addr := strings.TrimPrefix(backend.URL, "https://")

	wrongPub, _, _ := ed25519.GenerateKey(rand.Reader)
	tbl := routing.NewTable("nA")
	tbl.Upsert("nB", addr, false, nil)
	resolver := func(string) (crypto.PublicKey, bool) { return wrongPub, true }
	h := OwnerProxy(tbl, resolver, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))

	req := httptest.NewRequest(http.MethodGet, "/v1/sandboxes/nB.abc", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	require.Equal(t, http.StatusBadGateway, rr.Code) // pin rejected the channel
}
