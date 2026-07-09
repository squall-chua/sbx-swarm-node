package apiserver

import (
	"context"
	"testing"
	"time"

	sbxv1 "github.com/squall-chua/sbx-swarm-node/internal/gen/sbxswarm/v1"
	"github.com/squall-chua/sbx-swarm-node/internal/git"
	"github.com/squall-chua/sbx-swarm-node/internal/sandbox"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestPublishWork_Branch(t *testing.T) {
	svc, rec, upstream, _ := gitPublishFixture(t)
	svc.publishTimeout = 10 * time.Second

	res, err := svc.PublishWork(context.Background(), &sbxv1.PublishWorkRequest{Id: rec.ID, Strategy: "branch", Target: "feature-x"})
	require.NoError(t, err)
	require.Equal(t, "refs/heads/feature-x", res.Ref)
	require.Empty(t, res.DeliveryUrl)
	require.True(t, upstreamHasBranch(t, upstream, "feature-x"))
}

// mismatchSvc builds a minimal clone-mode sandbox with a single git-backed
// workspace registered under the given provider/remote, without any real git base
// or git binary — the provider-mismatch rejection fires in PublishWork before any
// git/exec work happens.
func mismatchSvc(t *testing.T, wsName, provider, remoteURL string) (*SandboxService, string) {
	t.Helper()
	mgr := newTestManager(t)
	rec, err := mgr.AdmitAndCreate(context.Background(), sandbox.CreateSpec{
		Agent: "shell", Clone: true, Branch: "main",
		Workspaces: []sandbox.WorkspaceMount{{Name: wsName}},
	})
	require.NoError(t, err)
	svc := NewSandboxService(mgr, newTestOps(t))
	svc.SetGit(map[string]*git.Workspace{wsName: git.New(git.Spec{
		Name: wsName, Provider: provider, RemoteURL: remoteURL, AllowPush: true, Allowlist: []string{"git"},
	})})
	return svc, rec.ID
}

func TestPublishWork_ProviderMismatch(t *testing.T) {
	ghSvc, ghID := mismatchSvc(t, "gh-repo", "github", "https://github.com/acme/app")
	_, err := ghSvc.PublishWork(context.Background(), &sbxv1.PublishWorkRequest{Id: ghID, Strategy: "gerrit_change", Target: "main"})
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, err.Error(), "unsupported")

	plainSvc, plainID := mismatchSvc(t, "plain-repo", "", "")
	_, err = plainSvc.PublishWork(context.Background(), &sbxv1.PublishWorkRequest{Id: plainID, Strategy: "pull_request", Target: "main"})
	require.Error(t, err)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}
