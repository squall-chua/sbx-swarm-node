package apiserver

import (
	"context"

	"github.com/squall-chua/sbx-swarm-node/internal/git"
	"github.com/squall-chua/sbx-swarm-node/internal/gitprovider"
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
		// Create the node-managed mirror base on first use (ADR-0020) so the PRE
		// fetch below has a base to freshen. No-op for an operator-prepared base or
		// a legacy workspace with no remote_url.
		if err := gw.EnsureBase(ctx); err != nil {
			return nil, status.Errorf(codes.Internal, "git ensure base: %v", err)
		}
		// Review-head checkout: resolve the Review's head branch and (Gerrit)
		// fetch its Patchset into the base, then check that out instead of Branch.
		if spec.ReviewRef != nil {
			branch, err := resolveReviewHead(ctx, gw, spec.ReviewRef.ID)
			if err != nil {
				return nil, err
			}
			spec.Branch = branch
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

// resolveReviewHead resolves a Review head to the local branch the clone checks
// out (and PublishWork later pushes in place), fetching the head into the base
// when PreSteps do not (a Gerrit Patchset). Pure REST derivation (ADR-0024).
func resolveReviewHead(ctx context.Context, gw *git.Workspace, reviewID string) (string, error) {
	prov := gitprovider.Derive(gw.RemoteURL(), gw.Provider())
	env := gitprovider.Env{
		RemoteURL: gw.RemoteURL(), Cred: gw.Cred(),
		APIBase: gitprovider.APIBase(prov, gw.RemoteURL(), gw.APIBaseURL()),
	}
	head, err := gitprovider.ResolveReviewHead(ctx, env, prov, reviewID)
	if err != nil {
		return "", err
	}
	if head.FetchRef != "" {
		if err := gw.FetchRef(ctx, head.FetchRef, head.LocalBranch); err != nil {
			return "", status.Errorf(codes.Internal, "fetch review head: %v", err)
		}
	}
	return head.LocalBranch, nil
}
