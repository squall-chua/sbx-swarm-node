package gitprovider

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"

	"github.com/squall-chua/sbx-swarm-node/internal/git"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// restClient talks to a forge REST API using the workspace credential. The token
// is sent only as a request header (never in argv or a URL), and CA trust comes
// from cred.CAPath. Errors are scrubbed of the request/URL/headers (leak bar).
type restClient struct {
	http     *http.Client
	cred     git.Credential
	provider Provider
}

func newRESTClient(p Provider, base string, cred git.Credential) (*restClient, error) {
	if cred.Token == "" {
		return nil, status.Error(codes.FailedPrecondition, "REST strategy requires an HTTPS token credential")
	}
	tc := &tls.Config{MinVersion: tls.VersionTLS12}
	if cred.CAPath != "" {
		pem, err := os.ReadFile(cred.CAPath)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "read ca_path: %v", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, status.Error(codes.Internal, "ca_path: no certificates found")
		}
		tc.RootCAs = pool
	}
	_ = base // base is passed per-request by the caller
	return &restClient{
		http:     &http.Client{Transport: &http.Transport{TLSClientConfig: tc}},
		cred:     cred,
		provider: p,
	}, nil
}

// do issues method url with provider auth, decoding a 2xx JSON body into out.
// A non-2xx response maps to a gRPC status; the token never appears in any error.
func (c *restClient) do(ctx context.Context, method, reqURL string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return status.Errorf(codes.Internal, "marshal %s request: %v", c.provider, err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, reqURL, rdr)
	if err != nil {
		return status.Errorf(codes.Internal, "build %s request", c.provider)
	}
	switch c.provider {
	case GitHub:
		req.Header.Set("Authorization", "Bearer "+c.cred.Token)
		req.Header.Set("Accept", "application/vnd.github+json")
	case GitLab:
		req.Header.Set("PRIVATE-TOKEN", c.cred.Token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// A transport error (*url.Error) embeds the full URL; scrub to host-only.
		return status.Errorf(codes.Unavailable, "%s request failed: %v", c.provider, scrubURLErr(err))
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return status.Errorf(statusToCode(resp.StatusCode), "%s HTTP %d: %s", c.provider, resp.StatusCode, forgeMessage(data))
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return status.Errorf(codes.Internal, "decode %s response", c.provider)
		}
	}
	return nil
}

// scrubURLErr strips the URL (which could carry query params) from a transport
// error, keeping only the underlying cause — never the token (token is a header).
func scrubURLErr(err error) error {
	if ue, ok := err.(*url.Error); ok {
		return ue.Err
	}
	return err
}

func statusToCode(httpStatus int) codes.Code {
	switch {
	case httpStatus == 401 || httpStatus == 403:
		return codes.PermissionDenied
	case httpStatus == 404:
		return codes.FailedPrecondition
	case httpStatus == 422:
		return codes.InvalidArgument
	case httpStatus >= 500:
		return codes.Unavailable
	default:
		return codes.Internal
	}
}

// forgeMessage extracts the human message from a GitHub/GitLab error body
// (never contains the request token — it is the forge's own response).
func forgeMessage(data []byte) string {
	var m struct {
		Message string `json:"message"`
		Error   string `json:"error"`
	}
	if json.Unmarshal(data, &m) == nil {
		if m.Message != "" {
			return m.Message
		}
		if m.Error != "" {
			return m.Error
		}
	}
	if len(data) > 200 {
		data = data[:200]
	}
	return string(data)
}

// PullRequest pushes source to origin, then create-or-updates the open GitHub PR
// for (head=owner:source, base=target). Idempotent per (workspace, source, target).
func PullRequest(ctx context.Context, r *git.Runner, e Env, source, target string) (Result, error) {
	owner, repo, err := ParseRepo(GitHub, e.RemoteURL)
	if err != nil {
		return Result{}, status.Error(codes.InvalidArgument, err.Error())
	}
	if target == "" {
		return Result{}, status.Error(codes.InvalidArgument, "pull_request requires a target branch")
	}
	if _, err := Branch(ctx, r, e, source, source); err != nil { // push head to origin
		return Result{}, err
	}
	c, err := newRESTClient(GitHub, e.APIBase, e.Cred)
	if err != nil {
		return Result{}, err
	}
	q := url.Values{"head": {owner + ":" + source}, "base": {target}, "state": {"open"}}
	listURL := fmt.Sprintf("%s/repos/%s/%s/pulls?%s", e.APIBase, owner, repo, q.Encode())
	var found []struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	if err := c.do(ctx, http.MethodGet, listURL, nil, &found); err != nil {
		return Result{}, err
	}
	var out struct {
		HTMLURL string `json:"html_url"`
	}
	if len(found) > 0 {
		patch := map[string]string{}
		if e.Title != "" {
			patch["title"] = e.Title
		}
		if e.Body != "" {
			patch["body"] = e.Body
		}
		if len(patch) == 0 {
			return Result{Ref: "refs/heads/" + source, DeliveryURL: found[0].HTMLURL}, nil
		}
		updURL := fmt.Sprintf("%s/repos/%s/%s/pulls/%d", e.APIBase, owner, repo, found[0].Number)
		if err := c.do(ctx, http.MethodPatch, updURL, patch, &out); err != nil {
			return Result{}, err
		}
		return Result{Ref: "refs/heads/" + source, DeliveryURL: out.HTMLURL}, nil
	}
	title := e.Title
	if title == "" {
		title = tipSubject(ctx, r, e.Dir, source)
	}
	create := map[string]string{"title": title, "head": source, "base": target}
	if e.Body != "" {
		create["body"] = e.Body
	}
	if err := c.do(ctx, http.MethodPost, fmt.Sprintf("%s/repos/%s/%s/pulls", e.APIBase, owner, repo), create, &out); err != nil {
		return Result{}, err
	}
	return Result{Ref: "refs/heads/" + source, DeliveryURL: out.HTMLURL}, nil
}

// MergeRequest pushes source to origin, then create-or-updates the open GitLab MR
// for (source_branch, target_branch). Idempotent per (workspace, source, target).
func MergeRequest(ctx context.Context, r *git.Runner, e Env, source, target string) (Result, error) {
	_, project, err := ParseRepo(GitLab, e.RemoteURL)
	if err != nil {
		return Result{}, status.Error(codes.InvalidArgument, err.Error())
	}
	if target == "" {
		return Result{}, status.Error(codes.InvalidArgument, "merge_request requires a target branch")
	}
	if _, err := Branch(ctx, r, e, source, source); err != nil {
		return Result{}, err
	}
	c, err := newRESTClient(GitLab, e.APIBase, e.Cred)
	if err != nil {
		return Result{}, err
	}
	proj := url.PathEscape(project) // group/app -> group%2Fapp
	q := url.Values{"source_branch": {source}, "target_branch": {target}, "state": {"opened"}}
	listURL := fmt.Sprintf("%s/projects/%s/merge_requests?%s", e.APIBase, proj, q.Encode())
	var found []struct {
		IID    int    `json:"iid"`
		WebURL string `json:"web_url"`
	}
	if err := c.do(ctx, http.MethodGet, listURL, nil, &found); err != nil {
		return Result{}, err
	}
	var out struct {
		WebURL string `json:"web_url"`
	}
	if len(found) > 0 {
		patch := map[string]string{}
		if e.Title != "" {
			patch["title"] = e.Title
		}
		if e.Body != "" {
			patch["description"] = e.Body
		}
		if len(patch) == 0 {
			return Result{Ref: "refs/heads/" + source, DeliveryURL: found[0].WebURL}, nil
		}
		updURL := fmt.Sprintf("%s/projects/%s/merge_requests/%d", e.APIBase, proj, found[0].IID)
		if err := c.do(ctx, http.MethodPut, updURL, patch, &out); err != nil {
			return Result{}, err
		}
		return Result{Ref: "refs/heads/" + source, DeliveryURL: out.WebURL}, nil
	}
	title := e.Title
	if title == "" {
		title = tipSubject(ctx, r, e.Dir, source)
	}
	create := map[string]string{"source_branch": source, "target_branch": target, "title": title}
	if e.Body != "" {
		create["description"] = e.Body
	}
	if err := c.do(ctx, http.MethodPost, fmt.Sprintf("%s/projects/%s/merge_requests", e.APIBase, proj), create, &out); err != nil {
		return Result{}, err
	}
	return Result{Ref: "refs/heads/" + source, DeliveryURL: out.WebURL}, nil
}
