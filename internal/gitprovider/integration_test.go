//go:build integration

package gitprovider

import (
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/squall-chua/sbx-swarm-node/internal/git"
	"github.com/stretchr/testify/require"
)

// TestPullRequest_Live opens/updates a real GitHub PR. Run with:
//
//	GITPROVIDER_GH_REMOTE=https://github.com/you/repo \
//	GITPROVIDER_GH_TOKEN=ghp_xxx \
//	go test -tags integration ./internal/gitprovider/ -run Live -v
func TestPullRequest_Live(t *testing.T) {
	remote := os.Getenv("GITPROVIDER_GH_REMOTE")
	tok := os.Getenv("GITPROVIDER_GH_TOKEN")
	if remote == "" || tok == "" {
		t.Skip("set GITPROVIDER_GH_REMOTE + GITPROVIDER_GH_TOKEN to run")
	}
	base := liveBase(t, remote, tok)
	e := Env{
		Dir: base, Remote: "origin", RemoteURL: remote,
		APIBase: APIBase(GitHub, remote, ""),
		Title:   "sbx-swarm P2 smoke", Body: "automated",
		Cred: git.Credential{Token: tok},
	}
	r := git.NewRunner([]string{"git"})
	source := "sbx-swarm-p2-smoke"
	makeBranch(t, base, source)
	res, err := PullRequest(context.Background(), r, e, source, "main")
	require.NoError(t, err)
	require.NotEmpty(t, res.DeliveryURL)
	t.Logf("PR: %s", res.DeliveryURL)
	// second call updates in place — same URL.
	res2, err := PullRequest(context.Background(), r, e, source, "main")
	require.NoError(t, err)
	require.Equal(t, res.DeliveryURL, res2.DeliveryURL)
}

func liveBase(t *testing.T, remote, tok string) string {
	t.Helper()
	base := filepath.Join(t.TempDir(), "base.git")
	env := append(os.Environ(),
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http."+remote+".extraheader",
		"GIT_CONFIG_VALUE_0=Authorization: Basic "+basicAuth(tok),
	)
	c := exec.Command("git", "clone", "--mirror", remote, base)
	c.Env = env
	out, err := c.CombinedOutput()
	require.NoError(t, err, string(out))
	c = exec.Command("git", "-C", base, "config", "remote.origin.mirror", "false")
	require.NoError(t, c.Run())
	return base
}

// basicAuth returns the base64 form of the GitHub token-as-basic-auth
// credential ("x-access-token:<token>"), matching git.Credential.Env's
// http.extraheader formula so the mirror clone authenticates identically.
func basicAuth(tok string) string {
	return base64.StdEncoding.EncodeToString([]byte("x-access-token:" + tok))
}

// makeBranch (force-)creates the local branch name in the bare base repo,
// pointing it at main. Used to give PullRequest a source ref to push.
func makeBranch(t *testing.T, dir, name string) {
	t.Helper()
	c := exec.Command("git", "-C", dir, "branch", "-f", name, "main")
	out, err := c.CombinedOutput()
	require.NoError(t, err, string(out))
}
