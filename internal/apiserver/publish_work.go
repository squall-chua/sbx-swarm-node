package apiserver

import (
	"context"

	sbxv1 "github.com/squall-chua/sbx-swarm-node/internal/gen/sbxswarm/v1"
	"github.com/squall-chua/sbx-swarm-node/internal/git"
	"github.com/squall-chua/sbx-swarm-node/internal/gitprovider"
	"github.com/squall-chua/sbx-swarm-node/internal/sandbox"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// PublishWork synchronously publishes the sandbox's source branch via the chosen
// strategy against its registered provider workspace (ADR-0019). Source branch is
// the sandbox's own HEAD (recorded branch on detached HEAD), never caller-supplied.
func (s *SandboxService) PublishWork(ctx context.Context, r *sbxv1.PublishWorkRequest) (*sbxv1.PublishResult, error) {
	rec, ws, err := s.gitTarget(ctx, r.Id)
	if err != nil {
		return nil, err
	}
	prov := gitprovider.Derive(ws.RemoteURL(), ws.Provider())
	if !prov.Supports(r.Strategy) {
		return nil, status.Errorf(codes.InvalidArgument, "%s on %s: unsupported (set provider explicitly if self-hosted)", r.Strategy, prov)
	}
	if r.Strategy != "patch" && !ws.AllowPush() {
		return nil, status.Error(codes.FailedPrecondition, "workspace does not allow push")
	}
	if r.Strategy == "pull_request" || r.Strategy == "merge_request" {
		if ws.Cred().Token == "" {
			return nil, status.Error(codes.FailedPrecondition, "REST strategy requires an HTTPS token credential")
		}
		if _, _, err := gitprovider.ParseRepo(prov, ws.RemoteURL()); err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
	}

	to := s.publishTimeout
	if to <= 0 {
		to = defaultPublishTimeout
	}
	pubCtx, cancel := context.WithTimeout(ctx, to)
	defer cancel()

	source, err := s.sourceBranch(pubCtx, rec)
	if err != nil {
		return nil, err
	}

	// Ensure the node-managed mirror base exists (ADR-0020) before we touch it.
	// No-op for an operator-prepared base or a legacy workspace with no remote_url.
	if err := ws.EnsureBase(pubCtx); err != nil {
		return nil, status.Errorf(codes.Internal, "publish-work ensure base: %v", err)
	}

	// Bundle the source branch out of the LIVE sandbox into the base under lock,
	// then run the strategy from the base.
	bundlePath, cleanup, err := s.bundleBranches(pubCtx, rec.BackendName, []string{source})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "publish-work bundle: %v", err)
	}
	defer cleanup()

	runEnv, err := ws.Cred().Env(ws.RemoteURL())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "publish-work credential: %v", err)
	}
	unlock, err := ws.FetchFromBundle(pubCtx, source, bundlePath) // adds source to the base
	if err != nil {
		return nil, status.Errorf(codes.Internal, "publish-work fetch: %v", err)
	}
	defer unlock() // hold the workspace lock across the strategy push — fetch+push atomic
	actor := principalFromContext(ctx).userRole
	if actor == "" {
		actor = "system"
	}
	env := gitprovider.Env{
		Dir: ws.Base(), RunEnv: runEnv, Remote: ws.RemoteName(),
		RemoteURL: ws.RemoteURL(), Cred: ws.Cred(),
		APIBase: gitprovider.APIBase(prov, ws.RemoteURL(), ws.APIBaseURL()),
		Title:   r.Title, Body: r.Body, Actor: actor,
	}
	runner := git.NewRunner([]string{"git"})

	var res gitprovider.Result
	switch r.Strategy {
	case "branch":
		res, err = gitprovider.Branch(pubCtx, runner, env, source, r.Target)
	case "patch":
		res, err = gitprovider.Patch(pubCtx, runner, env, source, r.Target)
	case "pull_request":
		res, err = gitprovider.PullRequest(pubCtx, runner, env, source, r.Target)
	case "merge_request":
		res, err = gitprovider.MergeRequest(pubCtx, runner, env, source, r.Target)
	case "gerrit_change":
		res, err = gitprovider.GerritChange(pubCtx, runner, env, source, r.Target)
	default:
		return nil, status.Errorf(codes.Unimplemented, "strategy %q not yet implemented", r.Strategy)
	}
	s.auditPublish(ws.Name(), source, actor, err)
	if err != nil {
		s.emit("sandbox.publish_failed", r.Id, map[string]string{"branch": source, "strategy": r.Strategy})
		return nil, status.Errorf(codes.Internal, "publish-work: %v", err)
	}
	s.emit("sandbox.published", r.Id, map[string]string{"branch": source, "strategy": r.Strategy})
	return &sbxv1.PublishResult{Ref: res.Ref, DeliveryUrl: res.DeliveryURL, ChangeId: res.ChangeID, Patch: res.Patch}, nil
}

// sourceBranch resolves the publish source: live HEAD when it is a real branch,
// else the recorded branch (detached-HEAD fallback). Never caller-supplied.
func (s *SandboxService) sourceBranch(ctx context.Context, rec *sandbox.Record) (string, error) {
	if b, err := s.agentHeadBranch(ctx, rec.BackendName); err == nil && b != "" {
		return b, nil
	}
	if rec.Spec.Branch != "" {
		return rec.Spec.Branch, nil
	}
	return "", status.Error(codes.FailedPrecondition, "no source branch: detached HEAD and no recorded branch")
}
