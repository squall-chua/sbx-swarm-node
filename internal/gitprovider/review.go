package gitprovider

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ReviewData is the node's read of a Review: PUBLISHED, UNRESOLVED threads only
// (drafts and resolved threads are filtered). Maps 1:1 to the Review proto.
type ReviewData struct {
	Head             string
	Threads          []Thread
	RequestedChanges []string
}

// Thread is one unresolved discussion. Comments is ordered oldest-first.
type Thread struct {
	ID       string
	File     string // "" = review-level / general comment
	Line     int
	Comments []Comment
}

// Comment is one comment in a thread. ID is the Agency's loop-protection unit.
type Comment struct {
	ID     string
	Author string
	Body   string
}

// Reply replies to one thread; Resolve marks it resolved (opt-in).
type Reply struct {
	ThreadID string
	Body     string
	Resolve  bool
}

// ReadReview reads a Review's published, unresolved threads. Provider is derived
// by the caller from the workspace remote (ADR-0024); reviewID is the PR number /
// Gerrit change number as a string.
func ReadReview(ctx context.Context, e Env, prov Provider, reviewID string) (ReviewData, error) {
	switch prov {
	case GitHub:
		return githubReadReview(ctx, e, reviewID)
	case Gerrit:
		return gerritReadReview(ctx, e, reviewID)
	default:
		// ponytail: gitlab review is unimplemented — no config/infra/test target here.
		// Add a discussions-API impl when a GitLab workspace needs it.
		return ReviewData{}, status.Errorf(codes.Unimplemented, "review read not supported for provider %q", prov)
	}
}

// ResolveThreads posts each reply (and resolves the thread when Reply.Resolve is
// set) and returns the ids of the comments it created — the Agency folds these
// into loop-protection state so the bot's own reply never re-triggers (#23-c).
func ResolveThreads(ctx context.Context, e Env, prov Provider, reviewID string, replies []Reply) ([]string, error) {
	switch prov {
	case GitHub:
		return githubResolveThreads(ctx, e, replies)
	case Gerrit:
		return gerritResolveThreads(ctx, e, reviewID, replies)
	default:
		return nil, status.Errorf(codes.Unimplemented, "review resolve not supported for provider %q", prov)
	}
}

// -------- GitHub (GraphQL) --------
//
// PR review-thread resolved state (isResolved) and resolving a thread
// (resolveReviewThread) exist ONLY in GitHub's GraphQL API — REST has neither.
// So the whole GitHub review path is GraphQL: one POST /graphql per operation.

// githubGraphQLURL derives the GraphQL endpoint from the v3 REST base:
// api.github.com -> api.github.com/graphql; HOST/api/v3 -> HOST/api/graphql.
func githubGraphQLURL(apiBase string) string {
	if strings.HasSuffix(apiBase, "/api/v3") {
		return strings.TrimSuffix(apiBase, "/api/v3") + "/api/graphql"
	}
	return strings.TrimRight(apiBase, "/") + "/graphql"
}

// graphql POSTs a query to GitHub's GraphQL endpoint and decodes response.data
// into out. A GraphQL body carries errors with HTTP 200, so the errors array is
// checked explicitly (c.do only maps the HTTP status).
func (c *restClient) graphql(ctx context.Context, url, query string, vars map[string]any, out any) error {
	var env struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := c.do(ctx, http.MethodPost, url, map[string]any{"query": query, "variables": vars}, &env); err != nil {
		return err
	}
	if len(env.Errors) > 0 {
		return status.Errorf(codes.Internal, "github graphql: %s", env.Errors[0].Message)
	}
	if out != nil && len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, out); err != nil {
			return status.Error(codes.Internal, "decode github graphql data")
		}
	}
	return nil
}

const githubReadQuery = `query($o:String!,$n:String!,$num:Int!){repository(owner:$o,name:$n){pullRequest(number:$num){headRefOid reviewThreads(first:100){nodes{id isResolved path line comments(first:100){nodes{id author{login} body}}}} reviews(first:100,states:[CHANGES_REQUESTED]){nodes{body}}}}}`

func githubReadReview(ctx context.Context, e Env, reviewID string) (ReviewData, error) {
	owner, repo, err := ParseRepo(GitHub, e.RemoteURL)
	if err != nil {
		return ReviewData{}, status.Error(codes.InvalidArgument, err.Error())
	}
	num, err := strconv.Atoi(strings.TrimSpace(reviewID))
	if err != nil {
		return ReviewData{}, status.Errorf(codes.InvalidArgument, "github review id must be a PR number, got %q", reviewID)
	}
	c, err := newRESTClient(GitHub, e.Cred)
	if err != nil {
		return ReviewData{}, err
	}
	var resp struct {
		Repository struct {
			PullRequest struct {
				HeadRefOid    string `json:"headRefOid"`
				ReviewThreads struct {
					Nodes []struct {
						ID         string `json:"id"`
						IsResolved bool   `json:"isResolved"`
						Path       string `json:"path"`
						Line       int    `json:"line"`
						Comments   struct {
							Nodes []struct {
								ID     string `json:"id"`
								Author struct {
									Login string `json:"login"`
								} `json:"author"`
								Body string `json:"body"`
							} `json:"nodes"`
						} `json:"comments"`
					} `json:"nodes"`
				} `json:"reviewThreads"`
				Reviews struct {
					Nodes []struct {
						Body string `json:"body"`
					} `json:"nodes"`
				} `json:"reviews"`
			} `json:"pullRequest"`
		} `json:"repository"`
	}
	if err := c.graphql(ctx, githubGraphQLURL(e.APIBase), githubReadQuery, map[string]any{"o": owner, "n": repo, "num": num}, &resp); err != nil {
		return ReviewData{}, err
	}
	pr := resp.Repository.PullRequest
	out := ReviewData{Head: pr.HeadRefOid}
	for _, t := range pr.ReviewThreads.Nodes {
		if t.IsResolved {
			continue // published + UNRESOLVED only
		}
		th := Thread{ID: t.ID, File: t.Path, Line: t.Line}
		for _, cm := range t.Comments.Nodes {
			th.Comments = append(th.Comments, Comment{ID: cm.ID, Author: cm.Author.Login, Body: cm.Body})
		}
		out.Threads = append(out.Threads, th)
	}
	for _, r := range pr.Reviews.Nodes {
		if b := strings.TrimSpace(r.Body); b != "" {
			out.RequestedChanges = append(out.RequestedChanges, b)
		}
	}
	return out, nil
}

const githubReplyMutation = `mutation($t:ID!,$b:String!){addPullRequestReviewThreadReply(input:{pullRequestReviewThreadId:$t,body:$b}){comment{id}}}`
const githubResolveMutation = `mutation($t:ID!){resolveReviewThread(input:{threadId:$t}){thread{id}}}`

func githubResolveThreads(ctx context.Context, e Env, replies []Reply) ([]string, error) {
	c, err := newRESTClient(GitHub, e.Cred)
	if err != nil {
		return nil, err
	}
	url := githubGraphQLURL(e.APIBase)
	var created []string
	for _, r := range replies {
		if r.Body != "" {
			var rep struct {
				AddPullRequestReviewThreadReply struct {
					Comment struct {
						ID string `json:"id"`
					} `json:"comment"`
				} `json:"addPullRequestReviewThreadReply"`
			}
			if err := c.graphql(ctx, url, githubReplyMutation, map[string]any{"t": r.ThreadID, "b": r.Body}, &rep); err != nil {
				return created, err
			}
			if id := rep.AddPullRequestReviewThreadReply.Comment.ID; id != "" {
				created = append(created, id)
			}
		}
		if r.Resolve {
			if err := c.graphql(ctx, url, githubResolveMutation, map[string]any{"t": r.ThreadID}, nil); err != nil {
				return created, err
			}
		}
	}
	return created, nil
}
