package apiserver

import (
	"context"
	"crypto"
	"crypto/tls"
	"net/http"
	"strings"
	"time"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"github.com/squall-chua/sbx-swarm-node/internal/auth"
	"github.com/squall-chua/sbx-swarm-node/internal/events"
	sbxv1 "github.com/squall-chua/sbx-swarm-node/internal/gen/sbxswarm/v1"
	"github.com/squall-chua/sbx-swarm-node/internal/obs"
	"github.com/squall-chua/sbx-swarm-node/internal/peer"
	"github.com/squall-chua/sbx-swarm-node/internal/routing"
	"github.com/squall-chua/sbx-swarm-node/web"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/encoding/protojson"
)

// Options configures the server.
type Options struct {
	NodeID, NodeName, Version string
	Keys                      auth.KeyStore
	Signer                    *auth.Signer
	Cert                      tls.Certificate
	Health                    *obs.Health      // optional; health routes mounted if set
	Sandboxes                 *SandboxService  // optional; registered if set
	Events                    *events.Bus      // optional; mounts /v1/events (SSE) under auth if set
	Policy                    *PolicyService   // optional; registered if set
	Forward                   *Forwarder       // optional; mounts unary forwarding interceptor if set
	Routing                   *routing.Table   // optional; used for SSE peer-merge when combined with Peers
	Peers                     *peer.Pool       // optional; used for SSE peer-merge when Routing is set
	Pins                      PinResolver      // optional; resolves per-node TLS pin for OwnerProxy
	Internal                  *InternalService // optional; node->node provision RPC (grpc-only, no gateway)
	NodeSvc                   *NodeService     // optional; if set, used instead of creating a new one (allows pre-wired Cordoner)
	Denylist                  func(nodeID string) bool
	PubKeyFor                 func(nodeID string) ([]byte, bool)
}

// Build constructs the primary handler (gRPC + REST multiplex), the bare REST
// handler (SPA + /v1 + SSE + terminal WS, no gRPC surface), and the gRPC server.
// The caller serves the primary handler over the pinned TLS port; the REST
// handler can additionally be served on a browser-facing console port with a
// browser-compatible cert. The caller stops grpcSrv on shutdown.
func Build(opts Options) (http.Handler, http.Handler, *grpc.Server, error) {
	node := opts.NodeSvc
	if node == nil {
		node = NewNodeService(opts.NodeID, opts.NodeName, opts.Version)
	}

	// Authn + authz always-on (closes the native-gRPC port even standalone).
	authn := newAuthenticator(authnDeps{
		Signer: opts.Signer, Keys: opts.Keys, LocalNodeID: opts.NodeID,
		PubKeyFor: opts.PubKeyFor, Denied: opts.Denylist,
	})
	unary := []grpc.UnaryServerInterceptor{authn.unaryInterceptor(), authzUnaryInterceptor()}
	if opts.Forward != nil {
		unary = append(unary, opts.Forward.UnaryInterceptor())
	}
	grpcSrv := grpc.NewServer(
		grpc.ChainUnaryInterceptor(unary...),
		grpc.ChainStreamInterceptor(authn.streamInterceptor(), authzStreamInterceptor()),
	)

	sbxv1.RegisterNodeServiceServer(grpcSrv, node)
	if opts.Sandboxes != nil {
		sbxv1.RegisterSandboxServiceServer(grpcSrv, opts.Sandboxes)
	}
	if opts.Policy != nil {
		sbxv1.RegisterPolicyServiceServer(grpcSrv, opts.Policy)
	}
	if opts.Events != nil {
		sbxv1.RegisterEventServiceServer(grpcSrv, NewEventService(opts.Events))
	}
	if opts.Internal != nil {
		sbxv1.RegisterInternalServiceServer(grpcSrv, opts.Internal)
	}

	// Loopback: the gateway dials the local gRPC server so REST traverses the
	// interceptor chain. Bridge the HTTP-authenticated role across the wire as a
	// signed x-sbx-authz token.
	loopConn, _, err := loopback(grpcSrv)
	if err != nil {
		return nil, nil, nil, err
	}
	gw := runtime.NewServeMux(
		runtime.WithMarshalerOption(runtime.MIMEWildcard, &runtime.JSONPb{
			MarshalOptions:   protojson.MarshalOptions{UseProtoNames: true, EmitUnpopulated: true},
			UnmarshalOptions: protojson.UnmarshalOptions{DiscardUnknown: true},
		}),
		runtime.WithIncomingHeaderMatcher(incomingHeaderMatcher),
		runtime.WithMetadata(roleAnnotator(opts.Signer)),
	)
	if err := sbxv1.RegisterNodeServiceHandler(context.Background(), gw, loopConn); err != nil {
		return nil, nil, nil, err
	}
	if opts.Sandboxes != nil {
		if err := sbxv1.RegisterSandboxServiceHandler(context.Background(), gw, loopConn); err != nil {
			return nil, nil, nil, err
		}
	}
	if opts.Policy != nil {
		if err := sbxv1.RegisterPolicyServiceHandler(context.Background(), gw, loopConn); err != nil {
			return nil, nil, nil, err
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
	if opts.Sandboxes != nil {
		v1 = terminalMux(mw.RequireRole("admin", opts.Sandboxes.TerminalHandler()), v1)
		v1 = filesMux(mw.RequireRole("admin", opts.Sandboxes.FilesHandler()), v1)
	}
	// In cluster mode, reverse-proxy REST/SSE requests for remote sandboxes to
	// their owning node (the in-process gateway never traverses the gRPC
	// forwarding interceptor, so REST/SSE need an HTTP-layer proxy). The proxy
	// runs after authentication (the caller is authenticated at this node) but
	// before the local handlers; local ids fall through unchanged. Nil-safe.
	if opts.Routing != nil {
		pins := opts.Pins
		if pins == nil {
			pins = func(string) (crypto.PublicKey, bool) { return nil, false }
		}
		v1 = OwnerProxy(opts.Routing, pins, v1)
	}

	rest := http.NewServeMux()
	rest.Handle("/v1/auth/session", sessionHandler(opts.Keys, opts.Signer)) // unauthenticated exchange
	if opts.Events != nil {
		var sseH http.Handler
		if opts.Routing != nil && opts.Peers != nil {
			sseH = SSEHandlerWithPeers(opts.Events, opts.Routing, opts.Peers)
		} else {
			sseH = SSEHandler(opts.Events)
		}
		rest.Handle("/v1/events", mw.Authenticate(sseH)) // authed SSE firehose
	}
	rest.Handle("/v1/", mw.Authenticate(v1))             // everything else under /v1 is authed
	rest.Handle("/", http.FileServer(http.FS(web.FS()))) // SPA fallback
	if opts.Health != nil {
		rest.Handle("/healthz", opts.Health.Handler())
		rest.Handle("/readyz", opts.Health.Handler())
		rest.Handle("/metrics", opts.Health.Handler())
	}

	return Multiplex(grpcSrv, rest), rest, grpcSrv, nil
}

// roleAnnotator injects the HTTP-authenticated role across the loopback as a
// signed x-sbx-authz token (cookie callers have no Authorization header).
func roleAnnotator(signer *auth.Signer) func(context.Context, *http.Request) metadata.MD {
	return func(_ context.Context, r *http.Request) metadata.MD {
		role, ok := auth.RoleFromContext(r.Context())
		if !ok {
			return nil
		}
		return metadata.Pairs("x-sbx-authz", signer.Mint(role, time.Now().Add(30*time.Second)))
	}
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
		secure := r.TLS != nil // false when the console is served over plain HTTP; browsers drop Secure cookies on cleartext non-localhost
		tok := signer.Mint(role, time.Now().Add(12*time.Hour))
		http.SetCookie(w, &http.Cookie{Name: auth.SessionCookie, Value: tok, Path: "/", HttpOnly: true, Secure: secure, SameSite: http.SameSiteStrictMode})
		csrf := signer.Mint("csrf", time.Now().Add(12*time.Hour)) // opaque random-ish value
		http.SetCookie(w, &http.Cookie{Name: auth.CSRFCookie, Value: csrf, Path: "/", Secure: secure, SameSite: http.SameSiteStrictMode})
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
