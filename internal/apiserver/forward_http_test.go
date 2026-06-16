package apiserver

import (
	"io"
	"net/http"
	"net/http/httptest"
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
	h := OwnerProxy(tbl, localSentinel(&called))

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
	h := OwnerProxy(tbl, localSentinel(&called))

	req := httptest.NewRequest(http.MethodGet, "/v1/sandboxes/nB.abc", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	require.True(t, called)
	require.Equal(t, 599, rr.Code)
}

func TestOwnerProxy_NonRoutableFallsThrough(t *testing.T) {
	tbl := routing.NewTable("nA")
	called := false
	h := OwnerProxy(tbl, localSentinel(&called))

	// Id without a dot is not self-routing → local.
	req := httptest.NewRequest(http.MethodGet, "/v1/sandboxes/plainid", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	require.True(t, called)
}

func TestOwnerProxy_RemoteProxies(t *testing.T) {
	// Owner backend (TLS) echoes a marker so we can prove the proxy reached it.
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/sandboxes/nB.abc/logs", r.URL.Path)
		require.Equal(t, "Bearer tok", r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "from-owner")
	}))
	defer backend.Close()

	// backend.Listener.Addr() gives host:port; strip scheme.
	ownerAddr := backend.Listener.Addr().String()

	tbl := routing.NewTable("nA")
	tbl.Upsert("nB", ownerAddr, false, nil)

	called := false
	h := OwnerProxy(tbl, localSentinel(&called))

	req := httptest.NewRequest(http.MethodGet, "/v1/sandboxes/nB.abc/logs", nil)
	req.Header.Set("Authorization", "Bearer tok")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	require.False(t, called, "should NOT fall through to local for a remote sandbox")
	require.Equal(t, http.StatusOK, rr.Code)
	require.Equal(t, "from-owner", rr.Body.String())
}
