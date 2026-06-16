package apiserver

import (
	"testing"

	sbxv1 "github.com/squall-chua/sbx-swarm-node/internal/gen/sbxswarm/v1"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestAuthorize_MutationRequiresAdmin(t *testing.T) {
	// read-only user cannot mutate
	err := authorize("/sbxswarm.v1.SandboxService/CreateSandbox", principal{userRole: "read-only"})
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	// admin user can mutate
	require.NoError(t, authorize("/sbxswarm.v1.SandboxService/CreateSandbox", principal{userRole: "admin"}))
	// node-only principal cannot mutate
	err = authorize("/sbxswarm.v1.PolicyService/SetSecret", principal{node: true})
	require.Equal(t, codes.PermissionDenied, status.Code(err))
}

func TestAuthorize_ReadsAndWatchAllowAnyAuthenticated(t *testing.T) {
	require.NoError(t, authorize("/sbxswarm.v1.SandboxService/ListSandboxes", principal{userRole: "read-only"}))
	require.NoError(t, authorize("/sbxswarm.v1.EventService/WatchEvents", principal{node: true}))
	// unauthenticated principal is rejected even for reads
	err := authorize("/sbxswarm.v1.SandboxService/ListSandboxes", principal{})
	require.Equal(t, codes.Unauthenticated, status.Code(err))
}

// Drift guard: every registered method must be classified.
func TestAuthz_AllMethodsClassified(t *testing.T) {
	descs := []grpc.ServiceDesc{
		sbxv1.SandboxService_ServiceDesc,
		sbxv1.NodeService_ServiceDesc,
		sbxv1.PolicyService_ServiceDesc,
		sbxv1.EventService_ServiceDesc,
	}
	for _, d := range descs {
		names := []string{}
		for _, m := range d.Methods {
			names = append(names, m.MethodName)
		}
		for _, s := range d.Streams {
			names = append(names, s.StreamName)
		}
		for _, n := range names {
			full := "/" + d.ServiceName + "/" + n
			require.True(t, classified(full), "method %s is not classified (add to mutating or reads)", full)
		}
	}
}
