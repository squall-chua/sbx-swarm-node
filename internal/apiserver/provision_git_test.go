package apiserver

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/squall-chua/sbx-swarm-node/internal/git"
	"github.com/squall-chua/sbx-swarm-node/internal/ids"
	"github.com/squall-chua/sbx-swarm-node/internal/ops"
	"github.com/squall-chua/sbx-swarm-node/internal/sandbox"
	"github.com/squall-chua/sbx-swarm-node/internal/store"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// newTestManager builds a fake-backend Manager with ample capacity (shared by the
// git tests in this package). Mirrors the inline construction in the existing
// newSandboxSvc / provision_test.go.
func newTestManager(t *testing.T) *sandbox.Manager {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "n.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	mgr := sandbox.NewManager("n1", sandbox.NewFake(), st, ids.NewGen("n1"))
	mgr.SetCapacity(sandbox.NewCapacity(4, 1e9, 1e9))
	return mgr
}

// newTestOps builds an ops.Manager (its own store; the tested publish paths don't
// exercise op persistence, so a separate store is fine).
func newTestOps(t *testing.T) *ops.Manager {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "ops.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	return ops.NewManager(st, ids.NewGen("n1"))
}

func newGitWS(t *testing.T) map[string]*git.Workspace {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	base := filepath.Join(root, "base.git")
	cmd := exec.Command("git", "init", "--bare", base)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	ws := git.New(git.Spec{
		Name: "repo", Base: base, Remote: "origin", DefaultBranch: "main", AllowPush: true,
		PreSteps:  [][]string{{"git", "fetch", "--all"}}, // no remote configured => succeeds as a no-op-ish fetch
		Allowlist: []string{"git"},
	})
	return map[string]*git.Workspace{"repo": ws}
}

func TestProvisionLocal_RejectsNonCloneGitBacked(t *testing.T) {
	gitWS := newGitWS(t)
	mgr := newTestManager(t) // helper used elsewhere in this package's tests
	_, err := ProvisionLocal(context.Background(), mgr, gitWS, sandbox.CreateSpec{
		Agent: "shell", Clone: false, Workspaces: []sandbox.WorkspaceMount{{Name: "repo"}},
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestProvisionLocal_RejectsCloneWithNonGitWorkspace(t *testing.T) {
	gitWS := newGitWS(t)
	mgr := newTestManager(t)
	_, err := ProvisionLocal(context.Background(), mgr, gitWS, sandbox.CreateSpec{
		Agent: "shell", Clone: true, Workspaces: []sandbox.WorkspaceMount{{Name: "not-git"}},
	})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestProvisionLocal_CloneRunsPreAndCreates(t *testing.T) {
	gitWS := newGitWS(t)
	mgr := newTestManager(t)
	rec, err := ProvisionLocal(context.Background(), mgr, gitWS, sandbox.CreateSpec{
		Agent: "shell", Clone: true, Branch: "agent/x", Workspaces: []sandbox.WorkspaceMount{{Name: "repo"}},
	})
	require.NoError(t, err)
	require.Equal(t, "agent/x", rec.Spec.Branch)
}
