package apiserver

import (
	"context"
	"crypto/tls"
	"net/http"
	"strings"
	"time"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/squall-chua/sbx-swarm-node/internal/auth"
	"github.com/squall-chua/sbx-swarm-node/internal/events"
	sbxv1 "github.com/squall-chua/sbx-swarm-node/internal/gen/sbxswarm/v1"
	"github.com/squall-chua/sbx-swarm-node/internal/obs"
	"github.com/squall-chua/sbx-swarm-node/web"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
)

// Options configures the server.
type Options struct {
	NodeID, NodeName, Version string
	Keys                      auth.KeyStore
	Signer                    *auth.Signer
	Cert                      tls.Certificate
	Health                    *obs.Health     // optional; health routes mounted if set
	Sandboxes                 *SandboxService // optional; registered if set
	Events                    *events.Bus     // optional; mounts /v1/events (SSE) under auth if set
	Policy                    *PolicyService  // optional; registered if set
}

// Build constructs the one-port handler and the gRPC server. The caller serves
// the handler over TLS (ALPN h2) and stops grpcSrv on shutdown.
func Build(opts Options) (http.Handler, *grpc.Server, error) {
	node := NewNodeService(opts.NodeID, opts.NodeName, opts.Version)

	grpcSrv := grpc.NewServer()
	sbxv1.RegisterNodeServiceServer(grpcSrv, node)

	// In-process gateway: the gateway calls the service directly via a local
	// handler registration (no loopback gRPC connection). Use proto field names
	// (snake_case) in the JSON so the REST contract matches the proto.
	gw := runtime.NewServeMux(
		runtime.WithMarshalerOption(runtime.MIMEWildcard, &runtime.JSONPb{
			MarshalOptions:   protojson.MarshalOptions{UseProtoNames: true, EmitUnpopulated: true},
			UnmarshalOptions: protojson.UnmarshalOptions{DiscardUnknown: true},
		}),
		runtime.WithIncomingHeaderMatcher(incomingHeaderMatcher),
	)
	if err := sbxv1.RegisterNodeServiceHandlerServer(context.Background(), gw, node); err != nil {
		return nil, nil, err
	}

	if opts.Sandboxes != nil {
		sbxv1.RegisterSandboxServiceServer(grpcSrv, opts.Sandboxes)
		if err := sbxv1.RegisterSandboxServiceHandlerServer(context.Background(), gw, opts.Sandboxes); err != nil {
			return nil, nil, err
		}
	}

	if opts.Policy != nil {
		sbxv1.RegisterPolicyServiceServer(grpcSrv, opts.Policy)
		if err := sbxv1.RegisterPolicyServiceHandlerServer(context.Background(), gw, opts.Policy); err != nil {
			return nil, nil, err
		}
	}

	mw := auth.New(opts.Keys, opts.Signer)

	// The /v1 subtree is authed. SSE log/stats handlers (when the observe
	// collectors are wired) intercept .../logs and text/event-stream .../stats;
	// all other /v1 traffic (including unary JSON GetStats) falls through to gw.
	var v1 http.Handler = gw
	if opts.Sandboxes != nil && opts.Sandboxes.observeStreamReady() {
		v1 = observeStreamMux(opts.Sandboxes.obs, gw)
	}

	rest := http.NewServeMux()
	rest.Handle("/v1/auth/session", sessionHandler(opts.Keys, opts.Signer)) // unauthenticated exchange
	if opts.Events != nil {
		rest.Handle("/v1/events", mw.Authenticate(SSEHandler(opts.Events))) // authed SSE firehose
	}
	rest.Handle("/v1/", mw.Authenticate(v1))             // everything else under /v1 is authed
	rest.Handle("/", http.FileServer(http.FS(web.FS()))) // SPA fallback
	if opts.Health != nil {
		rest.Handle("/healthz", opts.Health.Handler())
		rest.Handle("/readyz", opts.Health.Handler())
		rest.Handle("/metrics", opts.Health.Handler())
	}

	return Multiplex(grpcSrv, rest), grpcSrv, nil
}

// incomingHeaderMatcher forwards the Idempotency-Key REST header into gRPC
// metadata (the default matcher drops non-permanent headers), so REST clients
// get the same idempotent behavior as native gRPC callers.
func incomingHeaderMatcher(key string) (string, bool) {
	if strings.EqualFold(key, "Idempotency-Key") {
		return "idempotency-key", true
	}
	return runtime.DefaultHeaderMatcher(key)
}

// sessionHandler exchanges a valid API key (Authorization: Bearer <key>) for an
// httpOnly session cookie + a readable CSRF cookie.
func sessionHandler(keys auth.KeyStore, signer *auth.Signer) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		key := r.Header.Get("Authorization")
		key = trimBearer(key)
		role, ok := keys.RoleForKey(key)
		if !ok {
			http.Error(w, "invalid key", http.StatusUnauthorized)
			return
		}
		tok := signer.Mint(role, time.Now().Add(12*time.Hour))
		http.SetCookie(w, &http.Cookie{Name: auth.SessionCookie, Value: tok, Path: "/", HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode})
		csrf := signer.Mint("csrf", time.Now().Add(12*time.Hour)) // opaque random-ish value
		http.SetCookie(w, &http.Cookie{Name: auth.CSRFCookie, Value: csrf, Path: "/", Secure: true, SameSite: http.SameSiteStrictMode})
		w.WriteHeader(http.StatusNoContent)
	})
}

func trimBearer(h string) string {
	const p = "Bearer "
	if len(h) > len(p) && h[:len(p)] == p {
		return h[len(p):]
	}
	return h
}
