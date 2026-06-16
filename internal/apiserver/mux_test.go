package apiserver

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMultiplex_RoutesByProtocolAndContentType(t *testing.T) {
	grpcH := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Header().Set("X-Route", "grpc") })
	otherH := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Header().Set("X-Route", "other") })

	mux := Multiplex(grpcH, otherH)

	// HTTP/2 + application/grpc -> grpc
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/svc/Method", nil)
	req.ProtoMajor = 2
	req.Header.Set("Content-Type", "application/grpc")
	mux.ServeHTTP(rec, req)
	require.Equal(t, "grpc", rec.Header().Get("X-Route"))

	// plain GET -> other
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/node", nil)
	mux.ServeHTTP(rec, req)
	require.Equal(t, "other", rec.Header().Get("X-Route"))
}
