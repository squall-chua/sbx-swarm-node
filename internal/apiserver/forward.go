package apiserver

import (
	"context"
	"strings"

	sbxv1 "github.com/squall-chua/sbx-swarm-node/internal/gen/sbxswarm/v1"
	"github.com/squall-chua/sbx-swarm-node/internal/peer"
	"github.com/squall-chua/sbx-swarm-node/internal/routing"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// Forwarder routes unary RPCs to the owning node when the sandbox id is remote.
type Forwarder struct {
	tbl  *routing.Table
	pool *peer.Pool
}

// NewForwarder builds the forwarder.
func NewForwarder(tbl *routing.Table, pool *peer.Pool) *Forwarder {
	return &Forwarder{tbl: tbl, pool: pool}
}

// idExtractor pulls the routable id from a request that has one.
type idExtractor interface{ GetId() string }

// UnaryInterceptor relays unary calls whose request carries a remote sandbox id.
func (f *Forwarder) UnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		id, ok := routableID(req)
		if !ok || f.tbl.IsLocal(id) {
			return handler(ctx, req)
		}
		owner, found := f.tbl.Owner(id)
		if !found {
			return handler(ctx, req)
		}
		addr, ok := f.tbl.Addr(owner)
		if !ok {
			return handler(ctx, req) // unknown owner: let local handler return 404
		}
		conn, err := f.pool.Conn(addr, owner)
		if err != nil {
			return nil, err
		}
		out := newReplyFor(info.FullMethod)
		if out == nil {
			return handler(ctx, req) // method not in forward map
		}
		// Promote the caller's incoming metadata (auth, idempotency-key) to the
		// outgoing context so the owner re-authenticates the forwarded call.
		if md, ok := metadata.FromIncomingContext(ctx); ok {
			ctx = metadata.NewOutgoingContext(ctx, md)
		}
		if err := conn.Invoke(ctx, info.FullMethod, req, out); err != nil {
			return nil, err
		}
		return out, nil
	}
}

func routableID(req any) (string, bool) {
	e, ok := req.(idExtractor)
	if !ok {
		return "", false
	}
	id := e.GetId()
	if !strings.Contains(id, ".") {
		return "", false
	}
	return id, true
}

// newReplyFor returns a freshly-allocated proto reply for the given full method,
// or nil if the method is not forwardable.
func newReplyFor(fullMethod string) any {
	switch fullMethod {
	case "/sbxswarm.v1.SandboxService/GetSandbox":
		return new(sbxv1.Sandbox)
	case "/sbxswarm.v1.SandboxService/DeleteSandbox":
		return new(sbxv1.Operation)
	case "/sbxswarm.v1.SandboxService/StartSandbox":
		return new(sbxv1.Sandbox)
	case "/sbxswarm.v1.SandboxService/StopSandbox":
		return new(sbxv1.Sandbox)
	case "/sbxswarm.v1.SandboxService/Exec":
		return new(sbxv1.ExecResponse)
	case "/sbxswarm.v1.SandboxService/AgentRun":
		return new(sbxv1.Operation)
	case "/sbxswarm.v1.SandboxService/PublishPort":
		return new(sbxv1.Port)
	case "/sbxswarm.v1.SandboxService/ListPorts":
		return new(sbxv1.ListPortsResponse)
	case "/sbxswarm.v1.SandboxService/GetStats":
		return new(sbxv1.Stats)
	case "/sbxswarm.v1.SandboxService/ListBlocked":
		return new(sbxv1.ListBlockedResponse)
	case "/sbxswarm.v1.SandboxService/PublishSandbox":
		return new(sbxv1.Operation)
	case "/sbxswarm.v1.SandboxService/KeepAlive":
		return new(sbxv1.Sandbox)
	default:
		return nil
	}
}
