package gitprovider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/squall-chua/sbx-swarm-node/internal/git"
	"github.com/stretchr/testify/require"
)

func TestUserFromRemote(t *testing.T) {
	require.Equal(t, "admin", userFromRemote("ssh://admin@localhost:29418/demo"))
	require.Equal(t, "git", userFromRemote("git@github.com:o/r.git"))
	require.Equal(t, "", userFromRemote("https://github.com/o/r.git"))
}

func TestStripXSSI(t *testing.T) {
	require.Equal(t, `{"a":1}`, string(stripXSSI([]byte(")]}'\n{\"a\":1}"))))
	require.Equal(t, `{"a":1}`, string(stripXSSI([]byte(`{"a":1}`)))) // no prefix
}

// --- GitHub (GraphQL) ---

func TestGitHubReadReview_UnresolvedOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/graphql", r.URL.Path)
		require.Equal(t, "Bearer tok", r.Header.Get("Authorization"))
		_, _ = io.WriteString(w, `{"data":{"repository":{"pullRequest":{
			"headRefOid":"abc123",
			"reviewThreads":{"nodes":[
				{"id":"T_resolved","isResolved":true,"path":"a.go","line":1,"comments":{"nodes":[{"id":"c0","author":{"login":"human"},"body":"done"}]}},
				{"id":"T_open","isResolved":false,"path":"b.go","line":42,"comments":{"nodes":[{"id":"c1","author":{"login":"human"},"body":"fix this"}]}}
			]},
			"reviews":{"nodes":[{"body":"please address"},{"body":""}]}
		}}}}`)
	}))
	defer srv.Close()

	e := Env{RemoteURL: "https://github.com/o/r.git", APIBase: srv.URL, Cred: git.Credential{Token: "tok"}}
	rv, err := ReadReview(context.Background(), e, GitHub, "7")
	require.NoError(t, err)
	require.Equal(t, "abc123", rv.Head)
	require.Len(t, rv.Threads, 1, "only the unresolved thread")
	require.Equal(t, "T_open", rv.Threads[0].ID)
	require.Equal(t, "b.go", rv.Threads[0].File)
	require.Equal(t, 42, rv.Threads[0].Line)
	require.Equal(t, "c1", rv.Threads[0].Comments[0].ID)
	require.Equal(t, []string{"please address"}, rv.RequestedChanges)
}

func TestGitHubResolveThreads_ReplyAndResolve(t *testing.T) {
	var mutations []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Query string `json:"query"`
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		switch {
		case strings.Contains(body.Query, "addPullRequestReviewThreadReply"):
			mutations = append(mutations, "reply")
			_, _ = io.WriteString(w, `{"data":{"addPullRequestReviewThreadReply":{"comment":{"id":"REPLY_1"}}}}`)
		case strings.Contains(body.Query, "resolveReviewThread"):
			mutations = append(mutations, "resolve")
			_, _ = io.WriteString(w, `{"data":{"resolveReviewThread":{"thread":{"id":"T_open"}}}}`)
		default:
			t.Fatalf("unexpected query: %s", body.Query)
		}
	}))
	defer srv.Close()

	e := Env{RemoteURL: "https://github.com/o/r.git", APIBase: srv.URL, Cred: git.Credential{Token: "tok"}}
	ids, err := ResolveThreads(context.Background(), e, GitHub, "7", []Reply{
		{ThreadID: "T_open", Body: "fixed in a1b2c3", Resolve: true},
	})
	require.NoError(t, err)
	require.Equal(t, []string{"REPLY_1"}, ids)
	require.Equal(t, []string{"reply", "resolve"}, mutations)
}

func TestGitHubResolveThreads_ReplyOnly_NoResolve(t *testing.T) {
	var mutations []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(string(b), "resolveReviewThread") {
			mutations = append(mutations, "resolve")
		} else {
			mutations = append(mutations, "reply")
		}
		_, _ = io.WriteString(w, `{"data":{"addPullRequestReviewThreadReply":{"comment":{"id":"REPLY_2"}}}}`)
	}))
	defer srv.Close()

	e := Env{RemoteURL: "https://github.com/o/r.git", APIBase: srv.URL, Cred: git.Credential{Token: "tok"}}
	ids, err := ResolveThreads(context.Background(), e, GitHub, "7", []Reply{{ThreadID: "T_open", Body: "noted"}})
	require.NoError(t, err)
	require.Equal(t, []string{"REPLY_2"}, ids)
	require.Equal(t, []string{"reply"}, mutations, "no resolve when Resolve=false")
}

// --- Gerrit (REST) ---

const gerritCommentsJSON = `)]}'
{
  "file.go": [
    {"id":"root_open","line":10,"message":"fix here","unresolved":false,"updated":"2026-07-11 10:00:00.000000000","author":{"username":"human"}},
    {"id":"reply_open","in_reply_to":"root_open","line":10,"message":"still broken","unresolved":true,"updated":"2026-07-11 11:00:00.000000000","author":{"username":"human"}},
    {"id":"root_done","line":20,"message":"nit","unresolved":false,"updated":"2026-07-11 09:00:00.000000000","author":{"username":"human"}}
  ],
  "/PATCHSET_LEVEL": [
    {"id":"ps1","message":"overall looks off","unresolved":true,"updated":"2026-07-11 08:00:00.000000000","author":{"username":"human"}}
  ]
}`

func TestGerritReadReview_ThreadsAndFiltering(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.True(t, strings.HasPrefix(r.URL.Path, "/a/"), "authenticated path")
		require.NotEmpty(t, r.Header.Get("Authorization"))
		switch {
		case strings.HasSuffix(r.URL.Path, "/comments"):
			_, _ = io.WriteString(w, gerritCommentsJSON)
		default: // change detail
			_, _ = io.WriteString(w, ")]}'\n{\"current_revision\":\"deadbeef\"}")
		}
	}))
	defer srv.Close()

	e := Env{RemoteURL: "ssh://admin@localhost:29418/demo", APIBase: srv.URL, Cred: git.Credential{Token: "httppw"}}
	rv, err := ReadReview(context.Background(), e, Gerrit, "1")
	require.NoError(t, err)
	require.Equal(t, "deadbeef", rv.Head)
	require.Len(t, rv.Threads, 2, "file.go open thread + patchset-level open; root_done is resolved")

	byID := map[string]Thread{}
	for _, th := range rv.Threads {
		byID[th.ID] = th
	}
	open := byID["root_open"]
	require.Equal(t, "file.go", open.File)
	require.Equal(t, 10, open.Line)
	require.Len(t, open.Comments, 2, "root + reply grouped")

	ps := byID["ps1"]
	require.Equal(t, "", ps.File, "patchset-level maps to review-level")
}

func TestGerritResolveThreads_ReplyResolveAndCreatedIDs(t *testing.T) {
	var gets int
	var postBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			gets++
			if gets == 1 {
				_, _ = io.WriteString(w, gerritCommentsJSON) // pre-snapshot
			} else {
				// post-snapshot: the reply comment now exists
				_, _ = io.WriteString(w, `)]}'
{"file.go":[
  {"id":"root_open","line":10,"message":"fix here","unresolved":false,"updated":"2026-07-11 10:00:00.000000000","author":{"username":"human"}},
  {"id":"reply_open","in_reply_to":"root_open","line":10,"message":"still broken","unresolved":true,"updated":"2026-07-11 11:00:00.000000000","author":{"username":"human"}},
  {"id":"root_done","line":20,"message":"nit","unresolved":false,"updated":"2026-07-11 09:00:00.000000000","author":{"username":"human"}},
  {"id":"bot_reply","in_reply_to":"reply_open","line":10,"message":"fixed","unresolved":false,"updated":"2026-07-11 12:00:00.000000000","author":{"username":"bot"}}
]}`)
			}
		case http.MethodPost:
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &postBody)
			_, _ = io.WriteString(w, ")]}'\n{}")
		}
	}))
	defer srv.Close()

	e := Env{RemoteURL: "ssh://admin@localhost:29418/demo", APIBase: srv.URL, Cred: git.Credential{Token: "httppw"}}
	ids, err := ResolveThreads(context.Background(), e, Gerrit, "1", []Reply{
		{ThreadID: "root_open", Body: "fixed", Resolve: true},
	})
	require.NoError(t, err)
	require.Equal(t, []string{"bot_reply"}, ids, "new comment id discovered by set-diff")

	// the reply targets the latest comment in the thread and clears unresolved
	comments := postBody["comments"].(map[string]any)
	inputs := comments["file.go"].([]any)
	ci := inputs[0].(map[string]any)
	require.Equal(t, "reply_open", ci["in_reply_to"])
	require.Equal(t, false, ci["unresolved"])
	require.Equal(t, "fixed", ci["message"])
}
