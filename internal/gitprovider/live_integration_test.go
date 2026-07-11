//go:build integration

package gitprovider

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/squall-chua/sbx-swarm-node/internal/git"
	"github.com/stretchr/testify/require"
)

// Live review smoke against a REAL forge. Env-gated (integration tag) and
// self-skips without credentials, so CI stays red-by-default free.
//
// GitHub (drives ReadReview -> ResolveThreads -> ReadReview loop protection):
//
//	GITHUB_TOKEN=$(gh auth token) \
//	GH_REVIEW_REPO=squall-chua/test GH_REVIEW_PR=2 \
//	go test -tags integration ./internal/gitprovider/ -run TestLive_GitHubReview -v
//
// Gerrit (needs a local Gerrit HTTP password + a change with an unresolved comment):
//
//	GERRIT_API_BASE=http://localhost:8080 GERRIT_REMOTE=ssh://admin@localhost:29418/demo \
//	GERRIT_HTTP_PASSWORD=<pw> GERRIT_CHANGE=1 \
//	go test -tags integration ./internal/gitprovider/ -run TestLive_GerritReview -v
func TestLive_GitHubReview(t *testing.T) {
	token := os.Getenv("GITHUB_TOKEN")
	repo := os.Getenv("GH_REVIEW_REPO") // owner/name
	pr := os.Getenv("GH_REVIEW_PR")
	if token == "" || repo == "" || pr == "" {
		t.Skip("set GITHUB_TOKEN, GH_REVIEW_REPO, GH_REVIEW_PR")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	e := Env{
		RemoteURL: "https://github.com/" + repo + ".git",
		APIBase:   "https://api.github.com",
		Cred:      git.Credential{Token: token},
	}

	// 1. read: at least one unresolved thread exists (the reviewer's comment).
	rv, err := ReadReview(ctx, e, GitHub, pr)
	require.NoError(t, err)
	t.Logf("read: head=%s threads=%d requested=%v", rv.Head, len(rv.Threads), rv.RequestedChanges)
	require.NotEmpty(t, rv.Threads, "expected at least one unresolved review thread")
	thread := rv.Threads[0]
	require.NotEmpty(t, thread.ID)
	require.NotEmpty(t, thread.Comments)

	// 2. reply WITHOUT resolving: returns the created comment id (loop-protection unit).
	ids, err := ResolveThreads(ctx, e, GitHub, pr, []Reply{{ThreadID: thread.ID, Body: "Acknowledged — pushing a fix."}})
	require.NoError(t, err)
	require.Len(t, ids, 1, "one created comment id")
	t.Logf("reply created comment id: %s", ids[0])

	// 3. re-read: reply-only leaves the thread unresolved, and our reply is now a
	//    comment in it whose id we already recorded — the Agency skips on that.
	rv2, err := ReadReview(ctx, e, GitHub, pr)
	require.NoError(t, err)
	var found Thread
	for _, th := range rv2.Threads {
		if th.ID == thread.ID {
			found = th
		}
	}
	require.NotEmpty(t, found.ID, "reply-only keeps the thread unresolved")
	var sawReply bool
	for _, c := range found.Comments {
		if c.ID == ids[0] {
			sawReply = true
		}
	}
	require.True(t, sawReply, "our reply comment id appears in the re-read thread")

	// 4. resolve it: thread disappears from the unresolved read.
	_, err = ResolveThreads(ctx, e, GitHub, pr, []Reply{{ThreadID: thread.ID, Body: "Resolved.", Resolve: true}})
	require.NoError(t, err)
	rv3, err := ReadReview(ctx, e, GitHub, pr)
	require.NoError(t, err)
	for _, th := range rv3.Threads {
		require.NotEqual(t, thread.ID, th.ID, "resolved thread no longer unresolved")
	}
}

func TestLive_GerritReview(t *testing.T) {
	base := os.Getenv("GERRIT_API_BASE")
	remote := os.Getenv("GERRIT_REMOTE")
	pw := os.Getenv("GERRIT_HTTP_PASSWORD")
	change := os.Getenv("GERRIT_CHANGE")
	if base == "" || remote == "" || pw == "" || change == "" {
		t.Skip("set GERRIT_API_BASE, GERRIT_REMOTE, GERRIT_HTTP_PASSWORD, GERRIT_CHANGE")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	e := Env{RemoteURL: remote, APIBase: base, Cred: git.Credential{Token: pw}}

	rv, err := ReadReview(ctx, e, Gerrit, change)
	require.NoError(t, err)
	t.Logf("gerrit read: head=%s threads=%d", rv.Head, len(rv.Threads))
	require.NotEmpty(t, rv.Threads, "expected an unresolved comment on the change")
	thread := rv.Threads[0]

	ids, err := ResolveThreads(ctx, e, Gerrit, change, []Reply{{ThreadID: thread.ID, Body: "Acknowledged.", Resolve: true}})
	require.NoError(t, err)
	t.Logf("gerrit created comment ids: %v", ids)
	require.NotEmpty(t, ids, "created comment id discovered")

	rv2, err := ReadReview(ctx, e, Gerrit, change)
	require.NoError(t, err)
	for _, th := range rv2.Threads {
		require.NotEqual(t, thread.ID, th.ID, "resolved thread no longer unresolved")
	}

	// review-head resolves to the current patchset fetch ref.
	head, err := ResolveReviewHead(ctx, e, Gerrit, change)
	require.NoError(t, err)
	require.Equal(t, "review/"+change, head.LocalBranch)
	require.Contains(t, head.FetchRef, "refs/changes/")
	t.Logf("gerrit review head: branch=%s fetch=%s", head.LocalBranch, head.FetchRef)
}
