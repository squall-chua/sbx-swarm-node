package gitprovider

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Gerrit review read/reply runs over the Gerrit REST API (HTTP), not the SSH push
// transport the gerrit_change publish uses. Auth is HTTP basic: the username is
// parsed from the SSH remote (ssh://<user>@host/…) and the password is the
// workspace token (an HTTP password from Gerrit Settings → HTTP Credentials).
// The REST base is the workspace APIBaseURL (e.g. http://gerrit:8080).

const gerritPatchsetLevel = "/PATCHSET_LEVEL"

// gerritComment is the subset of Gerrit's CommentInfo the resolver needs.
type gerritComment struct {
	ID         string `json:"id"`
	InReplyTo  string `json:"in_reply_to"`
	Line       int    `json:"line"`
	Message    string `json:"message"`
	Unresolved bool   `json:"unresolved"`
	Updated    string `json:"updated"`
	Author     struct {
		Username string `json:"username"`
		Name     string `json:"name"`
	} `json:"author"`
}

func (g gerritComment) author() string {
	if g.Author.Username != "" {
		return g.Author.Username
	}
	return g.Author.Name
}

// gerritClient talks to the Gerrit REST API with HTTP basic auth and strips the
// XSSI prefix ()]}') Gerrit prepends to every JSON body.
type gerritClient struct {
	http *http.Client
	base string // APIBaseURL, e.g. http://gerrit:8080
	user string
	pass string
}

func newGerritClient(e Env) (*gerritClient, error) {
	if e.APIBase == "" {
		return nil, status.Error(codes.FailedPrecondition, "gerrit review requires the workspace api_base_url (its HTTP endpoint)")
	}
	if e.Cred.Token == "" {
		return nil, status.Error(codes.FailedPrecondition, "gerrit review requires an HTTP password token credential")
	}
	user := userFromRemote(e.RemoteURL)
	if user == "" {
		return nil, status.Error(codes.FailedPrecondition, "gerrit review: cannot derive username from remote_url")
	}
	return &gerritClient{
		http: &http.Client{},
		base: strings.TrimRight(e.APIBase, "/"),
		user: user,
		pass: e.Cred.Token,
	}, nil
}

// do issues an authenticated request under /a/, decoding the XSSI-stripped JSON
// body into out. Errors are scrubbed of the URL (leak bar), same as restClient.
func (c *gerritClient) do(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return status.Errorf(codes.Internal, "marshal gerrit request: %v", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+"/a"+path, rdr)
	if err != nil {
		return status.Error(codes.Internal, "build gerrit request")
	}
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(c.user+":"+c.pass)))
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return status.Errorf(codes.Unavailable, "gerrit request failed: %v", scrubURLErr(err))
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return status.Errorf(statusToCode(resp.StatusCode), "gerrit HTTP %d: %s", resp.StatusCode, forgeMessage(data))
	}
	if out != nil {
		data = stripXSSI(data)
		if len(data) > 0 {
			if err := json.Unmarshal(data, out); err != nil {
				return status.Error(codes.Internal, "decode gerrit response")
			}
		}
	}
	return nil
}

// stripXSSI removes Gerrit's magic ")]}'\n" JSON prefix.
func stripXSSI(b []byte) []byte {
	if bytes.HasPrefix(b, []byte(")]}'")) {
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			return b[i+1:]
		}
	}
	return b
}

// userFromRemote extracts the user from ssh://user@host:port/path or
// user@host:path; "" if none.
func userFromRemote(remote string) string {
	remote = strings.TrimSpace(remote)
	if i := strings.Index(remote, "://"); i >= 0 {
		remote = remote[i+3:]
	}
	at := strings.IndexByte(remote, '@')
	if at < 0 {
		return ""
	}
	return remote[:at]
}

// gerritThread is a grouped, unresolved Gerrit discussion.
type gerritThread struct {
	rootID   string
	path     string
	line     int
	latestID string // newest comment; the one a reply targets
	comments []gerritComment
}

// gerritThreads groups the change's published comments into threads and returns
// only those whose latest comment is unresolved. Comments come keyed by file
// path; threads are reply-chains within a path.
func gerritThreads(byPath map[string][]gerritComment) []gerritThread {
	var out []gerritThread
	paths := make([]string, 0, len(byPath))
	for p := range byPath {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, path := range paths {
		comments := byPath[path]
		byID := make(map[string]gerritComment, len(comments))
		for _, c := range comments {
			byID[c.ID] = c
		}
		groups := map[string][]gerritComment{}
		for _, c := range comments {
			groups[gerritRoot(c, byID)] = append(groups[gerritRoot(c, byID)], c)
		}
		for root, group := range groups {
			sort.Slice(group, func(i, j int) bool { return group[i].Updated < group[j].Updated })
			latest := group[len(group)-1]
			if !latest.Unresolved {
				continue // resolved thread
			}
			out = append(out, gerritThread{
				rootID:   root,
				path:     path,
				line:     byID[root].Line,
				latestID: latest.ID,
				comments: group,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].rootID < out[j].rootID })
	return out
}

// gerritRoot walks in_reply_to to the thread root id.
func gerritRoot(c gerritComment, byID map[string]gerritComment) string {
	seen := map[string]bool{}
	for c.InReplyTo != "" && !seen[c.ID] {
		seen[c.ID] = true
		parent, ok := byID[c.InReplyTo]
		if !ok {
			break
		}
		c = parent
	}
	return c.ID
}

func gerritReadReview(ctx context.Context, e Env, changeID string) (ReviewData, error) {
	c, err := newGerritClient(e)
	if err != nil {
		return ReviewData{}, err
	}
	var byPath map[string][]gerritComment
	if err := c.do(ctx, http.MethodGet, "/changes/"+changeID+"/comments", nil, &byPath); err != nil {
		return ReviewData{}, err
	}
	var change struct {
		CurrentRevision string `json:"current_revision"`
	}
	// Head is informational; a failure here shouldn't fail the read.
	_ = c.do(ctx, http.MethodGet, "/changes/"+changeID+"?o=CURRENT_REVISION", nil, &change)

	out := ReviewData{Head: change.CurrentRevision}
	for _, t := range gerritThreads(byPath) {
		file := t.path
		if file == gerritPatchsetLevel {
			file = "" // review-level
		}
		th := Thread{ID: t.rootID, File: file, Line: t.line}
		for _, cm := range t.comments {
			th.Comments = append(th.Comments, Comment{ID: cm.ID, Author: cm.author(), Body: cm.Message})
		}
		out.Threads = append(out.Threads, th)
	}
	return out, nil
}

// gerritCommentInput is one CommentInput in a SetReview call.
type gerritCommentInput struct {
	InReplyTo  string `json:"in_reply_to,omitempty"`
	Line       int    `json:"line,omitempty"`
	Message    string `json:"message"`
	Unresolved bool   `json:"unresolved"`
}

func gerritResolveThreads(ctx context.Context, e Env, changeID string, replies []Reply) ([]string, error) {
	c, err := newGerritClient(e)
	if err != nil {
		return nil, err
	}
	var byPath map[string][]gerritComment
	if err := c.do(ctx, http.MethodGet, "/changes/"+changeID+"/comments", nil, &byPath); err != nil {
		return nil, err
	}
	// Locate each reply's thread so we know path/line and the comment to reply to.
	threads := map[string]gerritThread{}
	for _, t := range gerritThreads(byPath) {
		threads[t.rootID] = t
	}
	pre := gerritCommentIDs(byPath)

	comments := map[string][]gerritCommentInput{}
	for _, r := range replies {
		if r.Body == "" {
			continue
		}
		t, ok := threads[r.ThreadID]
		if !ok {
			return nil, status.Errorf(codes.NotFound, "gerrit thread %q not found (already resolved?)", r.ThreadID)
		}
		ci := gerritCommentInput{InReplyTo: t.latestID, Line: t.line, Message: r.Body, Unresolved: !r.Resolve}
		if t.path == gerritPatchsetLevel {
			ci.Line = 0
		}
		comments[t.path] = append(comments[t.path], ci)
	}
	if len(comments) == 0 {
		return nil, nil
	}
	if err := c.do(ctx, http.MethodPost, "/changes/"+changeID+"/revisions/current/review",
		map[string]any{"comments": comments}, nil); err != nil {
		return nil, err
	}

	// created_comment_ids: re-read and diff. Best-effort — a concurrent human
	// comment in the same window would be misattributed.
	// ponytail: set-diff, not txn; acceptable per design §7 (best-effort).
	var after map[string][]gerritComment
	if err := c.do(ctx, http.MethodGet, "/changes/"+changeID+"/comments", nil, &after); err != nil {
		return nil, nil // reply landed; id discovery is best-effort
	}
	var created []string
	for id := range gerritCommentIDs(after) {
		if !pre[id] {
			created = append(created, id)
		}
	}
	sort.Strings(created)
	return created, nil
}

func gerritCommentIDs(byPath map[string][]gerritComment) map[string]bool {
	ids := map[string]bool{}
	for _, comments := range byPath {
		for _, c := range comments {
			ids[c.ID] = true
		}
	}
	return ids
}
