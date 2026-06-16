package obs

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
)

func TestHealth_Endpoints(t *testing.T) {
	reg := prometheus.NewRegistry()
	RegisterBuildInfo(reg, "test")
	h := NewHealth(reg)
	srv := httptest.NewServer(h.Handler())
	t.Cleanup(srv.Close)

	// healthz always ok
	resp, err := http.Get(srv.URL + "/healthz")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	// readyz 503 until ready
	resp, err = http.Get(srv.URL + "/readyz")
	require.NoError(t, err)
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	resp.Body.Close()

	h.SetReady(true)
	resp, err = http.Get(srv.URL + "/readyz")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	// metrics exposes our build_info
	resp, err = http.Get(srv.URL + "/metrics")
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.Contains(t, string(body), "sbx_build_info")
}
