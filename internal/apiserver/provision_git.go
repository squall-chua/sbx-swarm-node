package apiserver

import (
	"context"

	"github.com/squall-chua/sbx-swarm-node/internal/git"
	"github.com/squall-chua/sbx-swarm-node/internal/sandbox"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ProvisionLocal enforces the clone <=> git-backed bijection (ADR-0015), runs the
// PRE pipeline under the workspace lock spanning Create, then admits+creates. It
// runs on the OWNER node (the only place that knows which workspaces are
// git-backed — that is node-local config, not gossiped). Shared by the local
// attempt (attemptFor) and the forwarded InternalService.Provision.
func ProvisionLocal(ctx context.Context, mgr *sandbox.Manager, gitWS map[string]*git.Workspace, spec sandbox.CreateSpec) (*sandbox.Record, error) {
	var gw *git.Workspace
	gitRefs := 0
	for _, w := range spec.Workspaces {
		if g, ok := gitWS[w.Name]; ok {
			gitRefs++
			gw = g
		}
	}
	if spec.Clone {
		if len(spec.Workspaces) != 1 || gw == nil {
			return nil, status.Error(codes.InvalidArgument, "clone mode requires exactly one git-backed workspace")
		}
		unlock, err := gw.PreLock(ctx, spec.Branch)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "git pre: %v", err)
		}
		defer unlock() // hold the lock across Create (clone-sourcing)
	} else if gitRefs > 0 {
		return nil, status.Error(codes.InvalidArgument, "git-backed workspace requires clone mode")
	}
	return mgr.AdmitAndCreate(ctx, spec)
}
