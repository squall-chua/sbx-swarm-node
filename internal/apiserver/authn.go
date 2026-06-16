package apiserver

import (
	"context"
	"strings"
	"time"

	"github.com/squall-chua/sbx-swarm-node/internal/auth"
	"github.com/squall-chua/sbx-swarm-node/internal/nodekey"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type principalCtxKey struct{}

func principalFromContext(ctx context.Context) principal {
	p, _ := ctx.Value(principalCtxKey{}).(principal)
	return p
}

// authnDeps are the authenticator's collaborators. PubKeyFor/Denied are nil in
// standalone mode (no peers).
type authnDeps struct {
	Signer      *auth.Signer
	Keys        auth.KeyStore
	LocalNodeID string
	PubKeyFor   func(nodeID string) ([]byte, bool) // gossiped peer pubkeys
	Denied      func(nodeID string) bool
	Skew        time.Duration // 0 -> default 30s
}

type authenticator struct{ d authnDeps }

func newAuthenticator(d authnDeps) *authenticator {
	if d.Skew == 0 {
		d.Skew = 30 * time.Second
	}
	return &authenticator{d: d}
}

// authenticate resolves a principal in strict order: x-sbx-authz (signed role),
// authorization (api key), node-key. Returns Unauthenticated if none verify.
func (a *authenticator) authenticate(ctx context.Context) (principal, error) {
	md, _ := metadata.FromIncomingContext(ctx)

	if v := first(md, "x-sbx-authz"); v != "" {
		if role, err := a.d.Signer.Verify(v); err == nil && isUserRole(role) {
			return principal{userRole: role}, nil
		}
	}
	if v := first(md, "authorization"); v != "" {
		if role, ok := a.d.Keys.RoleForKey(strings.TrimPrefix(v, "Bearer ")); ok {
			return principal{userRole: role}, nil
		}
	}
	if v := first(md, nodekey.MetadataKey); v != "" && a.d.PubKeyFor != nil {
		if _, err := nodekey.Verify(v, a.d.LocalNodeID, a.d.PubKeyFor, time.Now(), a.d.Skew, a.d.Denied); err == nil {
			return principal{node: true}, nil
		}
	}
	return principal{}, status.Error(codes.Unauthenticated, "authentication required")
}

// isUserRole reports whether role is a real principal role (guards against
// non-role signed tokens like the CSRF cookie value being replayed as auth).
func isUserRole(role string) bool { return role == "admin" || role == "read-only" }

func first(md metadata.MD, key string) string {
	if v := md.Get(key); len(v) > 0 {
		return v[0]
	}
	return ""
}

func (a *authenticator) unaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		p, err := a.authenticate(ctx)
		if err != nil {
			return nil, err
		}
		return handler(context.WithValue(ctx, principalCtxKey{}, p), req)
	}
}

func (a *authenticator) streamInterceptor() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		p, err := a.authenticate(ss.Context())
		if err != nil {
			return err
		}
		return handler(srv, &ctxStream{ServerStream: ss, ctx: context.WithValue(ss.Context(), principalCtxKey{}, p)})
	}
}

// ctxStream overrides Context() so downstream interceptors/handlers see the principal.
type ctxStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *ctxStream) Context() context.Context { return s.ctx }
