package apiserver

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// principal is the authenticated identity attached by the authn interceptor.
type principal struct {
	userRole string // "admin" | "read-only" | "" (none)
	node     bool   // authenticated as a swarm peer via node-key
}

func (p principal) authenticated() bool { return p.userRole != "" || p.node }

// mutatingMethods require an admin USER role. A node-only principal can never
// authorize a mutation by itself; forwarded mutations carry the user's admin
// credential alongside the node-key. Keep in sync with the proto — the drift
// guard test (TestAuthz_AllMethodsClassified) fails if a new method is unlisted.
var mutatingMethods = map[string]bool{
	"/sbxswarm.v1.SandboxService/CreateSandbox": true,
	"/sbxswarm.v1.SandboxService/DeleteSandbox": true,
	"/sbxswarm.v1.SandboxService/StartSandbox":  true,
	"/sbxswarm.v1.SandboxService/StopSandbox":   true,
	"/sbxswarm.v1.SandboxService/Exec":          true,
	"/sbxswarm.v1.SandboxService/AgentRun":      true,
	"/sbxswarm.v1.SandboxService/PublishPort":   true,
	"/sbxswarm.v1.PolicyService/SetPolicy":      true,
	"/sbxswarm.v1.PolicyService/SetSecret":      true,
	"/sbxswarm.v1.PolicyService/DeleteSecret":   true,
	"/sbxswarm.v1.NodeService/Cordon":           true,
	"/sbxswarm.v1.NodeService/Uncordon":         true,
	"/sbxswarm.v1.NodeService/Drain":            true,
}

// internalMethods are node->node RPCs authorized by node identity alone
// (a verified swarm peer). A user principal cannot call them. Admin is enforced
// once at the request's entry node before the async op (ADR-0011).
var internalMethods = map[string]bool{
	"/sbxswarm.v1.InternalService/Provision": true,
}

// readMethods are explicitly read/internal (any authenticated principal).
var readMethods = map[string]bool{
	"/sbxswarm.v1.SandboxService/GetSandbox":    true,
	"/sbxswarm.v1.SandboxService/ListSandboxes": true,
	"/sbxswarm.v1.SandboxService/ListPorts":     true,
	"/sbxswarm.v1.SandboxService/GetStats":      true,
	"/sbxswarm.v1.SandboxService/ListBlocked":   true,
	"/sbxswarm.v1.NodeService/GetNodeInfo":      true,
	"/sbxswarm.v1.PolicyService/ListPolicy":     true,
	"/sbxswarm.v1.PolicyService/ListSecrets":    true,
	"/sbxswarm.v1.EventService/WatchEvents":     true,
}

func classified(fullMethod string) bool {
	return mutatingMethods[fullMethod] || readMethods[fullMethod] || internalMethods[fullMethod]
}

// authorize enforces the 3-bucket policy. Unknown methods fail closed (treated as
// mutating: require admin) so an unclassified new RPC is never silently open.
func authorize(fullMethod string, p principal) error {
	if !p.authenticated() {
		return status.Error(codes.Unauthenticated, "authentication required")
	}
	if internalMethods[fullMethod] {
		if p.node {
			return nil
		}
		return status.Errorf(codes.PermissionDenied, "method %s requires a swarm node", fullMethod)
	}
	if readMethods[fullMethod] {
		return nil
	}
	if p.userRole == "admin" {
		return nil
	}
	return status.Errorf(codes.PermissionDenied, "method %s requires admin", fullMethod)
}

// authzUnaryInterceptor enforces authorize() using the principal placed in ctx by
// the authn interceptor.
func authzUnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if err := authorize(info.FullMethod, principalFromContext(ctx)); err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// authzStreamInterceptor is the streaming counterpart.
func authzStreamInterceptor() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if err := authorize(info.FullMethod, principalFromContext(ss.Context())); err != nil {
			return err
		}
		return handler(srv, ss)
	}
}
