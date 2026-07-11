package apiserver

import (
	"context"

	sbxv1 "github.com/squall-chua/sbx-swarm-node/internal/gen/sbxswarm/v1"
	"github.com/squall-chua/sbx-swarm-node/internal/gitprovider"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ReadReview reads a Review's published, unresolved threads for a registered
// workspace — pure forge/Gerrit REST, NO Sandbox (so a self-generated webhook
// can no-op before a VM boots). The node derives the provider from the
// workspace remote (ADR-0024); the Agency never names a git ref.
func (s *SandboxService) ReadReview(ctx context.Context, r *sbxv1.ReadReviewRequest) (*sbxv1.ReadReviewResponse, error) {
	prov, env, err := s.reviewEnv(r.GetReviewRef())
	if err != nil {
		return nil, err
	}
	rv, err := gitprovider.ReadReview(ctx, env, prov, r.GetReviewRef().GetId())
	if err != nil {
		return nil, err
	}
	return &sbxv1.ReadReviewResponse{Review: toReviewProto(rv)}, nil
}

// ResolveThreads posts replies (and optionally resolves) on a Review's threads
// and returns the ids of the comments it created — the Agency folds these into
// loop-protection state (#23-c). Workspace-scoped, NO Sandbox.
func (s *SandboxService) ResolveThreads(ctx context.Context, r *sbxv1.ResolveThreadsRequest) (*sbxv1.ResolveThreadsResponse, error) {
	prov, env, err := s.reviewEnv(r.GetReviewRef())
	if err != nil {
		return nil, err
	}
	replies := make([]gitprovider.Reply, 0, len(r.GetReplies()))
	for _, rp := range r.GetReplies() {
		replies = append(replies, gitprovider.Reply{ThreadID: rp.GetThreadId(), Body: rp.GetBody(), Resolve: rp.GetResolve()})
	}
	ids, err := gitprovider.ResolveThreads(ctx, env, prov, r.GetReviewRef().GetId(), replies)
	if err != nil {
		return nil, err
	}
	return &sbxv1.ResolveThreadsResponse{CreatedCommentIds: ids}, nil
}

// reviewEnv resolves a ReviewRef to its registered workspace and builds the
// gitprovider.Env for a REST review op (no Base/RunEnv — review is pure REST).
func (s *SandboxService) reviewEnv(ref *sbxv1.ReviewRef) (gitprovider.Provider, gitprovider.Env, error) {
	if ref.GetWorkspace() == "" || ref.GetId() == "" {
		return "", gitprovider.Env{}, status.Error(codes.InvalidArgument, "review_ref requires workspace and id")
	}
	ws := s.gitWS[ref.GetWorkspace()]
	if ws == nil {
		return "", gitprovider.Env{}, status.Errorf(codes.FailedPrecondition, "workspace %q is not git-backed", ref.GetWorkspace())
	}
	prov := gitprovider.Derive(ws.RemoteURL(), ws.Provider())
	env := gitprovider.Env{
		RemoteURL: ws.RemoteURL(),
		Cred:      ws.Cred(),
		APIBase:   gitprovider.APIBase(prov, ws.RemoteURL(), ws.APIBaseURL()),
	}
	return prov, env, nil
}

func toReviewProto(rv gitprovider.ReviewData) *sbxv1.Review {
	out := &sbxv1.Review{Head: rv.Head, RequestedChanges: rv.RequestedChanges}
	for _, t := range rv.Threads {
		th := &sbxv1.ReviewThread{Id: t.ID, File: t.File, Line: int32(t.Line)}
		for _, c := range t.Comments {
			th.Comments = append(th.Comments, &sbxv1.ReviewComment{Id: c.ID, Author: c.Author, Body: c.Body})
		}
		out.Threads = append(out.Threads, th)
	}
	return out
}
