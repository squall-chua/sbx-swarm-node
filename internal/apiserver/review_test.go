package apiserver

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	sbxv1 "github.com/squall-chua/sbx-swarm-node/internal/gen/sbxswarm/v1"
	"github.com/squall-chua/sbx-swarm-node/internal/git"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)
import "github.com/stretchr/testify/require"

// reviewSvc builds a service with one github workspace whose REST base points at
// the given fake forge — review is workspace-scoped, so no sandbox is needed.
func reviewSvc(t *testing.T, apiBase string) *SandboxService {
	t.Helper()
	svc := NewSandboxService(newTestManager(t), newTestOps(t))
	svc.SetGit(map[string]*git.Workspace{"repo": git.New(git.Spec{
		Name: "repo", Provider: "github", RemoteURL: "https://github.com/o/r.git",
		APIBaseURL: apiBase, Cred: git.Credential{Token: "tok"}, Allowlist: []string{"git"},
	})})
	return svc
}

func TestReadReview_MapsProto(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":{"repository":{"pullRequest":{
			"headRefOid":"h1","reviewThreads":{"nodes":[
				{"id":"T1","isResolved":false,"path":"x.go","line":5,"comments":{"nodes":[{"id":"c1","author":{"login":"al"},"body":"nit"}]}}
			]},"reviews":{"nodes":[]}}}}}`)
	}))
	defer srv.Close()

	resp, err := reviewSvc(t, srv.URL).ReadReview(context.Background(),
		&sbxv1.ReadReviewRequest{ReviewRef: &sbxv1.ReviewRef{Workspace: "repo", Id: "7"}})
	require.NoError(t, err)
	require.Equal(t, "h1", resp.Review.Head)
	require.Len(t, resp.Review.Threads, 1)
	require.Equal(t, "T1", resp.Review.Threads[0].Id)
	require.Equal(t, int32(5), resp.Review.Threads[0].Line)
	require.Equal(t, "c1", resp.Review.Threads[0].Comments[0].Id)
}

func TestResolveThreads_ReturnsCreatedIDs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":{"addPullRequestReviewThreadReply":{"comment":{"id":"R1"}}}}`)
	}))
	defer srv.Close()

	resp, err := reviewSvc(t, srv.URL).ResolveThreads(context.Background(),
		&sbxv1.ResolveThreadsRequest{ReviewRef: &sbxv1.ReviewRef{Workspace: "repo", Id: "7"},
			Replies: []*sbxv1.ThreadReply{{ThreadId: "T1", Body: "done"}}})
	require.NoError(t, err)
	require.Equal(t, []string{"R1"}, resp.CreatedCommentIds)
}

func TestReadReview_Validation(t *testing.T) {
	svc := reviewSvc(t, "http://unused")

	_, err := svc.ReadReview(context.Background(), &sbxv1.ReadReviewRequest{ReviewRef: &sbxv1.ReviewRef{Workspace: "repo"}})
	require.Equal(t, codes.InvalidArgument, status.Code(err), "missing id")

	_, err = svc.ReadReview(context.Background(), &sbxv1.ReadReviewRequest{ReviewRef: &sbxv1.ReviewRef{Workspace: "nope", Id: "7"}})
	require.Equal(t, codes.FailedPrecondition, status.Code(err), "unknown workspace")
}
