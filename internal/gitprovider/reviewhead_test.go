package gitprovider

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/squall-chua/sbx-swarm-node/internal/git"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestGitHubReviewHead_SameRepoBranch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Contains(t, r.URL.Path, "/repos/o/r/pulls/7")
		_, _ = io.WriteString(w, `{"head":{"ref":"feature-x","repo":{"full_name":"o/r"}}}`)
	}))
	defer srv.Close()

	e := Env{RemoteURL: "https://github.com/o/r.git", APIBase: srv.URL, Cred: git.Credential{Token: "t"}}
	h, err := ResolveReviewHead(context.Background(), e, GitHub, "7")
	require.NoError(t, err)
	require.Equal(t, "feature-x", h.LocalBranch)
	require.Empty(t, h.FetchRef, "same-repo branch already in base via PreSteps")
}

func TestGitHubReviewHead_ForkRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"head":{"ref":"feature-x","repo":{"full_name":"someone-else/r"}}}`)
	}))
	defer srv.Close()

	e := Env{RemoteURL: "https://github.com/o/r.git", APIBase: srv.URL, Cred: git.Credential{Token: "t"}}
	_, err := ResolveReviewHead(context.Background(), e, GitHub, "7")
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	require.Contains(t, err.Error(), "fork")
}

func TestGerritReviewHead_PatchsetFetchRef(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `)]}'
{"current_revision":"sha1","revisions":{"sha1":{"ref":"refs/changes/01/1/3"}}}`)
	}))
	defer srv.Close()

	e := Env{RemoteURL: "ssh://admin@localhost:29418/demo", APIBase: srv.URL, Cred: git.Credential{Token: "pw"}}
	h, err := ResolveReviewHead(context.Background(), e, Gerrit, "1")
	require.NoError(t, err)
	require.Equal(t, "review/1", h.LocalBranch)
	require.Equal(t, "refs/changes/01/1/3", h.FetchRef)
}

// A review-head sandbox's source carries the original Patchset's Change-Id (on an
// ancestor commit; the fix commit on top has none). GerritChange must reuse it so
// the re-push lands a NEW Patchset on the same Change, not a duplicate.
func TestGerritChange_ReusesExistingChangeID(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	const existing = "I1234567890abcdef1234567890abcdef12345678"
	root := t.TempDir()
	upstream := filepath.Join(root, "up.git")
	base := filepath.Join(root, "base.git")
	work := filepath.Join(root, "work")
	gitCmd(t, root, "init", "--bare", upstream)
	gitCmd(t, root, "clone", upstream, work)
	gitCmd(t, work, "-c", "user.email=a@b.c", "-c", "user.name=a", "commit", "--allow-empty", "-m", "main")
	gitCmd(t, work, "push", "origin", "HEAD:main")
	gitCmd(t, work, "checkout", "-b", "review/1")
	// original patchset commit, with a Change-Id trailer
	gitCmd(t, work, "-c", "user.email=a@b.c", "-c", "user.name=a", "commit", "--allow-empty", "-m", "patchset\n\nChange-Id: "+existing)
	// fix commit on top, no trailer
	gitCmd(t, work, "-c", "user.email=a@b.c", "-c", "user.name=a", "commit", "--allow-empty", "-m", "fix")
	gitCmd(t, work, "push", "origin", "HEAD:review/1")
	gitCmd(t, root, "clone", "--mirror", upstream, base)
	gitCmd(t, base, "config", "remote.origin.mirror", "false")

	e := Env{Dir: base, Remote: "origin", RemoteURL: "ssh://gerrit.corp/svc", Actor: "alice"}
	r := git.NewRunner([]string{"git"})
	res, err := GerritChange(context.Background(), r, e, "review/1", "main")
	require.NoError(t, err)
	require.Equal(t, existing, res.ChangeID, "reused, not derived")
	require.NotEqual(t, gerritChangeID("ssh://gerrit.corp/svc", "review/1", "main"), res.ChangeID)

	msg := gitOut(t, base, "log", "-1", "--format=%B", "refs/for/main")
	require.Equal(t, 1, strings.Count(msg, "Change-Id:"), "exactly one Change-Id trailer")
}
