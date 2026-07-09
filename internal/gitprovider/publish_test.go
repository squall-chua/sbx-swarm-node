package gitprovider

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/squall-chua/sbx-swarm-node/internal/git"
	"github.com/stretchr/testify/require"
)

func gitCmd(t *testing.T, dir string, args ...string) {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	out, err := c.CombinedOutput()
	require.NoError(t, err, string(out))
}

func TestBranch_PushesToRemote(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	upstream := filepath.Join(root, "up.git")
	base := filepath.Join(root, "base.git")
	work := filepath.Join(root, "work")
	gitCmd(t, root, "init", "--bare", upstream)
	gitCmd(t, root, "clone", upstream, work)
	gitCmd(t, work, "-c", "user.email=a@b.c", "-c", "user.name=a", "commit", "--allow-empty", "-m", "x")
	gitCmd(t, work, "push", "origin", "HEAD:main")
	gitCmd(t, root, "clone", "--mirror", upstream, base) // base mirrors upstream; origin=upstream
	// `clone --mirror` sets remote.origin.mirror=true, which makes git reject ANY
	// refspec push ("--mirror can't be combined with refspecs") — unset it so the
	// base behaves as a normal push-capable remote while still holding all refs.
	gitCmd(t, base, "config", "remote.origin.mirror", "false")

	r := git.NewRunner([]string{"git"})
	res, err := Branch(context.Background(), r, Env{Dir: base, Remote: "origin"}, "main", "feature-x")
	require.NoError(t, err)
	require.Equal(t, "refs/heads/feature-x", res.Ref)
	require.Empty(t, res.DeliveryURL)
	// upstream now has feature-x:
	out, err := exec.Command("git", "--git-dir", upstream, "rev-parse", "refs/heads/feature-x").CombinedOutput()
	require.NoError(t, err, string(out))
}
