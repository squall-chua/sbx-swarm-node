package apiserver

import (
	"net/http"
	"strings"
)

// Multiplex returns a handler that routes HTTP/2 application/grpc requests to
// grpcH and everything else to otherH (gateway/auth/static). Serve it over TLS
// (ALPN h2) so gRPC clients negotiate HTTP/2.
func Multiplex(grpcH, otherH http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor == 2 && strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
			grpcH.ServeHTTP(w, r)
			return
		}
		otherH.ServeHTTP(w, r)
	})
}
