package gitprovider

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/squall-chua/sbx-swarm-node/internal/git"
	"github.com/stretchr/testify/require"
)

func TestGerritChangeID_DeterministicAndSandboxIndependent(t *testing.T) {
	a := gerritChangeID("https://gerrit/x", "feature", "main")
	b := gerritChangeID("https://gerrit/x", "feature", "main")
	require.Equal(t, a, b, "same key => same Change-Id")
	require.True(t, strings.HasPrefix(a, "I"))
	require.NotEqual(t, a, gerritChangeID("https://gerrit/x", "feature", "release"), "target is part of the key")
	require.NotEqual(t, a, gerritChangeID("https://gerrit/y", "feature", "main"), "workspace is part of the key")
}

func TestParseGerritURL(t *testing.T) {
	out := []byte("remote: Processing changes: new: 1\nremote:   https://gerrit.corp/c/svc/+/1234 my subject\nremote: \nTo ssh://gerrit\n")
	require.Equal(t, "https://gerrit.corp/c/svc/+/1234", parseGerritURL(out))
	require.Equal(t, "", parseGerritURL([]byte("no url here")))
}

// A bare repo accepts refs/for/*. A multi-commit source must collapse to ONE
// pushed commit whose sole parent is the target tip and whose message carries
// the derived Change-Id.
func TestGerritChange_SquashesMultiCommit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	upstream := filepath.Join(root, "up.git")
	base := filepath.Join(root, "base.git")
	work := filepath.Join(root, "work")
	gitCmd(t, root, "init", "--bare", upstream)
	gitCmd(t, root, "clone", upstream, work)
	gitCmd(t, work, "-c", "user.email=a@b.c", "-c", "user.name=a", "commit", "--allow-empty", "-m", "main")
	gitCmd(t, work, "push", "origin", "HEAD:main")
	gitCmd(t, work, "checkout", "-b", "feature")
	gitCmd(t, work, "-c", "user.email=a@b.c", "-c", "user.name=a", "commit", "--allow-empty", "-m", "c1")
	gitCmd(t, work, "-c", "user.email=a@b.c", "-c", "user.name=a", "commit", "--allow-empty", "-m", "c2")
	gitCmd(t, work, "push", "origin", "HEAD:feature")
	gitCmd(t, root, "clone", "--mirror", upstream, base)
	gitCmd(t, base, "config", "remote.origin.mirror", "false")

	e := Env{
		Dir: base, Remote: "origin", RemoteURL: "ssh://gerrit.corp/svc", Actor: "alice",
	}
	r := git.NewRunner([]string{"git"})
	res, err := GerritChange(context.Background(), r, e, "feature", "main")
	require.NoError(t, err)
	require.Equal(t, gerritChangeID("ssh://gerrit.corp/svc", "feature", "main"), res.ChangeID)

	// The pushed ref has exactly one parent (the target tip) and the Change-Id.
	ref := gitOut(t, base, "rev-parse", "refs/for/main")
	parents := strings.Fields(gitOut(t, base, "rev-list", "--parents", "-n", "1", ref))
	require.Len(t, parents, 2, "squashed commit has exactly one parent") // self + 1 parent
	mainTip := gitOut(t, base, "rev-parse", "refs/heads/main")
	require.Equal(t, mainTip, parents[1])
	msg := gitOut(t, base, "log", "-1", "--format=%B", ref)
	require.Contains(t, msg, "Change-Id: "+res.ChangeID)
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	out, err := c.CombinedOutput()
	require.NoError(t, err, string(out))
	return strings.TrimSpace(string(out))
}
