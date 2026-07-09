package git

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func gitCmd(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
}

// build: upstream (bare) <- work (clone, has main) ; base = bare clone of upstream ;
// "sandbox" stood in by a second clone with branch agent/x registered as a
// remote on base. PreLock fetches refs into base; Publish fetches agent/x from
// the sandbox remote and pushes it upstream.
func TestWorkspace_PreLockAndPublish(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	upstream := filepath.Join(root, "upstream.git")
	work := filepath.Join(root, "work")
	base := filepath.Join(root, "base.git")
	sbx := filepath.Join(root, "sbx")

	gitCmd(t, root, "init", "--bare", upstream)
	gitCmd(t, root, "clone", upstream, work)
	gitCmd(t, work, "-c", "user.email=a@b.c", "-c", "user.name=t", "commit", "--allow-empty", "-m", "init")
	gitCmd(t, work, "push", "origin", "HEAD:main")
	gitCmd(t, root, "clone", "--bare", upstream, base)

	// Stand in for the in-container clone: a repo with branch agent/x, exposed to
	// the base as a remote named "sandbox-fake" (sbx wires this as sandbox-<name>).
	gitCmd(t, root, "clone", upstream, sbx)
	gitCmd(t, sbx, "checkout", "-b", "agent/x")
	gitCmd(t, sbx, "-c", "user.email=a@b.c", "-c", "user.name=t", "commit", "--allow-empty", "-m", "agent work")
	gitCmd(t, base, "remote", "add", "sandbox-fake", sbx)

	ws := New(Spec{
		Name: "repo", Base: base, Remote: "origin", DefaultBranch: "main", AllowPush: true,
		PreSteps:     [][]string{{"git", "fetch", "{remote}", "+refs/heads/*:refs/heads/*"}},
		PublishSteps: [][]string{{"git", "fetch", "{sandbox_remote}", "+refs/heads/{branch}:refs/heads/{branch}"}, {"git", "push", "{remote}", "{branch}"}},
		Allowlist:    []string{"git"},
	})

	unlock, err := ws.PreLock(context.Background(), "agent/x")
	require.NoError(t, err)
	unlock()

	require.NoError(t, ws.Publish(context.Background(), "agent/x", "sandbox-fake"))

	// upstream now has agent/x
	cmd := exec.Command("git", "branch", "--list", "agent/x")
	cmd.Dir = upstream
	out, _ := cmd.CombinedOutput()
	require.Contains(t, string(out), "agent/x")
}

func TestWorkspace_EnsureBase_ClonesMirror(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	upstream := filepath.Join(root, "upstream.git")
	work := filepath.Join(root, "work")
	gitCmd(t, root, "init", "--bare", upstream)
	gitCmd(t, root, "clone", upstream, work)
	gitCmd(t, work, "-c", "user.email=a@b.c", "-c", "user.name=a", "commit", "--allow-empty", "-m", "init")
	gitCmd(t, work, "push", "origin", "HEAD:main")

	base := filepath.Join(root, "acme.git")
	w := New(Spec{
		Name: "acme", Base: base, RemoteURL: upstream, DefaultBranch: "main",
		Allowlist: []string{"git"},
	})
	require.NoError(t, w.EnsureBase(context.Background()))
	// base now exists as a mirror and carries refs/heads/main:
	out, err := exec.Command("git", "--git-dir", base, "rev-parse", "refs/heads/main").CombinedOutput()
	require.NoError(t, err, string(out))
}

// TestWorkspace_EnsureBase_AllowsRefspecPush guards against a mirror base
// rejecting refspec pushes (git errors on "git push origin src:dst" when
// remote.origin.mirror=true). EnsureBase must clear the mirror flag after
// cloning so downstream Publish/Branch pushes work.
func TestWorkspace_EnsureBase_AllowsRefspecPush(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	upstream := filepath.Join(root, "upstream.git")
	work := filepath.Join(root, "work")
	gitCmd(t, root, "init", "--bare", upstream)
	gitCmd(t, root, "clone", upstream, work)
	gitCmd(t, work, "-c", "user.email=a@b.c", "-c", "user.name=a", "commit", "--allow-empty", "-m", "init")
	gitCmd(t, work, "push", "origin", "HEAD:main")

	base := filepath.Join(root, "acme.git")
	w := New(Spec{
		Name: "acme", Base: base, RemoteURL: upstream, DefaultBranch: "main",
		Allowlist: []string{"git"},
	})
	require.NoError(t, w.EnsureBase(context.Background()))

	// Refspec push under a new ref name must succeed against upstream.
	out, err := exec.Command("git", "--git-dir", base, "push", "origin", "refs/heads/main:refs/heads/pushed-branch").CombinedOutput()
	require.NoError(t, err, string(out))

	out, err = exec.Command("git", "--git-dir", upstream, "rev-parse", "refs/heads/pushed-branch").CombinedOutput()
	require.NoError(t, err, string(out))
}
