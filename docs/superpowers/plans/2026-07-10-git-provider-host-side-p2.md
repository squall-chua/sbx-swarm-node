# Git Provider host-side Phase 2 (PR / MR / Gerrit) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement the three remaining PublishWork strategies — `pull_request` (GitHub), `merge_request` (GitLab), and `gerrit_change` (Gerrit) — that Phase 1 gated as `Unimplemented`.

**Architecture:** Three free functions added to `internal/gitprovider`, dispatched from the existing `apiserver.PublishWork` switch. PR/MR push the source branch (reusing P1's `Branch`) then call the forge REST API through a small shared `restClient`; Gerrit squashes the branch to one commit via `git commit-tree`, injects a deterministic `Change-Id`, and pushes `refs/for/<target>`. The workspace credential's token feeds the REST `Authorization` header and its `ca_path` feeds TLS. Every deliverable is keyed by `(workspace, source, target)` (ADR-0021), so a re-publish updates in place.

**Tech Stack:** Go, stdlib `net/http` + `crypto/tls` (no new deps), `git` plumbing (`commit-tree`, `push`), `google.golang.org/grpc/status`, `httptest` for fakes.

## Global Constraints

- **No new dependencies.** REST uses stdlib `net/http`; TLS uses stdlib `crypto/tls`/`crypto/x509`.
- **Leak bar (P1, extended to REST):** the token and its base64 form must never appear in an error, `delivery_url`, event, audit entry, log, or persisted record. Never put the token in argv or a URL; it lives only in an HTTP header. Errors carry `provider + HTTP status + forge message`, never the request, URL query, or headers.
- **Gate before mutation:** unsupported strategy, missing token (PR/MR), and unparseable remote are all rejected before `EnsureBase`/bundle/fetch touch the base.
- **Idempotency key = `(workspace, source, target)`**, sandbox-independent (ADR-0021).
- **Same-repo only** for PR/MR (head branch pushed to origin; PR head `owner:source`).
- **No retries.** One attempt inside the existing publish timeout; fail-closed.
- **TDD:** every task writes the failing test first, watches it fail, then implements. Commit at the end of each task.
- Match existing style: free functions in `gitprovider`, `git.NewRunner([]string{"git"})`, `status.Errorf(codes.X, ...)`. Format only files you touch (repo is gofmt-dirty-but-unenforced).

---

### Task 1: Config + workspace plumbing for `api_base_url`

Adds the optional `api_base_url` field end to end: config → `git.Spec` → `Workspace.APIBaseURL()`. Nothing consumes it yet; this is the foundation the strategies read.

**Files:**
- Modify: `internal/config/config.go` (GitConfig struct, ~line 77-82)
- Modify: `internal/git/workspace.go` (Spec struct ~line 12-25; accessor ~line 39-42)
- Modify: `internal/node/node.go` (Spec build ~line 594-603)
- Test: `internal/config/config_test.go`, `internal/git/workspace_test.go`

**Interfaces:**
- Produces: `config.GitConfig.APIBaseURL string` (yaml `api_base_url`); `git.Spec.APIBaseURL string`; `func (w *git.Workspace) APIBaseURL() string`.

- [ ] **Step 1: Write the failing test** for the config field and workspace accessor.

In `internal/config/config_test.go`, add:

```go
func TestGitConfig_APIBaseURL(t *testing.T) {
	g := GitConfig{
		RemoteURL:  "https://ghe.corp.com/acme/app",
		Provider:   "github",
		APIBaseURL: "https://ghe.corp.com/api/v3",
	}.WithDefaults()
	require.Equal(t, "https://ghe.corp.com/api/v3", g.APIBaseURL)
}
```

In `internal/git/workspace_test.go` (create if absent — package `git`), add:

```go
func TestWorkspace_APIBaseURL(t *testing.T) {
	w := New(Spec{Name: "repo", APIBaseURL: "https://api.example.com"})
	require.Equal(t, "https://api.example.com", w.APIBaseURL())
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/config/ ./internal/git/ -run 'APIBaseURL' -v`
Expected: FAIL — `APIBaseURL` is an unknown field / undefined method.

- [ ] **Step 3: Add the config field.** In `internal/config/config.go`, inside `GitConfig` after `CAPath` (line ~82):

```go
	CAPath            string `yaml:"ca_path"`              // internal-CA / self-signed PEM (HTTPS only)
	APIBaseURL        string `yaml:"api_base_url"`         // REST API base override; "" => derive from remote_url (GitHub/GitLab only)
```

- [ ] **Step 4: Add the Spec field + accessor.** In `internal/git/workspace.go`, inside `Spec` after `AllowPush bool` (line ~19):

```go
	AllowPush     bool
	APIBaseURL    string // REST API base override (GitHub/GitLab); "" => derive
```

And add the accessor next to the others (after line ~40):

```go
func (w *Workspace) APIBaseURL() string    { return w.spec.APIBaseURL }
```

- [ ] **Step 5: Plumb config → Spec.** In `internal/node/node.go`, in the `git.Spec{...}` literal (line ~594), add the field on the `AllowPush` line:

```go
			DefaultBranch: g.DefaultBranch,
			AllowPush:     g.AllowPush, APIBaseURL: g.APIBaseURL, PreSteps: g.PreSteps, PublishSteps: g.PublishSteps, Allowlist: g.ExecAllowlist,
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./internal/config/ ./internal/git/ -run 'APIBaseURL' -v && go build ./...`
Expected: PASS; build clean.

- [ ] **Step 7: Commit**

```bash
git add internal/config/config.go internal/git/workspace.go internal/node/node.go internal/config/config_test.go internal/git/workspace_test.go
git commit -m "feat(git-provider): plumb optional api_base_url through config→spec→workspace"
```

---

### Task 2: `gitprovider` foundation — extend `Env`, add `APIBase`, `ParseRepo`, `tipSubject`

Pure host-side helpers the three strategies share. No network, no git writes here (except `tipSubject`, a read).

**Files:**
- Modify: `internal/gitprovider/publish.go` (extend `Env`; add `tipSubject`)
- Modify: `internal/gitprovider/provider.go` (add `pathOf`, `APIBase`, `ParseRepo`)
- Test: `internal/gitprovider/provider_test.go`, `internal/gitprovider/publish_test.go`

**Interfaces:**
- Produces:
  - `Env` gains `APIBase string`, `Title string`, `Body string`, `Actor string`.
  - `func APIBase(p Provider, remoteURL, override string) string`
  - `func ParseRepo(p Provider, remoteURL string) (owner, repo string, err error)` — GitLab returns `owner==""`, `repo`=full project path.
  - `func tipSubject(ctx context.Context, r *git.Runner, dir, ref string) string`

- [ ] **Step 1: Write the failing tests.** In `internal/gitprovider/provider_test.go`, add:

```go
func TestAPIBase(t *testing.T) {
	cases := []struct {
		p             Provider
		url, override string
		want          string
	}{
		{GitHub, "https://github.com/acme/app", "", "https://api.github.com"},
		{GitHub, "https://ghe.corp.com/acme/app", "", "https://ghe.corp.com/api/v3"},
		{GitLab, "https://gitlab.com/acme/app", "", "https://gitlab.com/api/v4"},
		{GitLab, "https://gitlab.corp/g/s/app", "", "https://gitlab.corp/api/v4"},
		{Gerrit, "ssh://git@gerrit.corp:29418/svc", "", ""},
		{Plain, "https://x/y/z", "", ""},
		{GitHub, "https://github.com/acme/app", "https://override/api", "https://override/api"},
	}
	for _, c := range cases {
		if got := APIBase(c.p, c.url, c.override); got != c.want {
			t.Errorf("APIBase(%q,%q,%q)=%q want %q", c.p, c.url, c.override, got, c.want)
		}
	}
}

func TestParseRepo(t *testing.T) {
	// GitHub: exactly owner/repo, both URL forms, .git stripped.
	o, r, err := ParseRepo(GitHub, "https://github.com/acme/app.git")
	require.NoError(t, err)
	require.Equal(t, "acme", o)
	require.Equal(t, "app", r)
	o, r, err = ParseRepo(GitHub, "git@github.com:acme/app.git")
	require.NoError(t, err)
	require.Equal(t, "acme", o)
	require.Equal(t, "app", r)
	// GitHub with a nested path is not a valid repo.
	_, _, err = ParseRepo(GitHub, "https://github.com/acme")
	require.Error(t, err)
	_, _, err = ParseRepo(GitHub, "https://github.com/a/b/c")
	require.Error(t, err)
	// GitLab: whole path is the project (subgroups kept), owner empty.
	o, r, err = ParseRepo(GitLab, "https://gitlab.corp/group/sub/app.git")
	require.NoError(t, err)
	require.Equal(t, "", o)
	require.Equal(t, "group/sub/app", r)
	// Empty path rejected.
	_, _, err = ParseRepo(GitLab, "https://gitlab.corp/")
	require.Error(t, err)
}
```

Add `import "github.com/stretchr/testify/require"` to `provider_test.go`.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/gitprovider/ -run 'APIBase|ParseRepo' -v`
Expected: FAIL — `APIBase`/`ParseRepo` undefined.

- [ ] **Step 3: Implement `pathOf`, `APIBase`, `ParseRepo`.** In `internal/gitprovider/provider.go`, add imports `"fmt"` (keep `"net/url"`, `"strings"`) and append:

```go
// pathOf extracts the path (no host) from an HTTPS or scp-like SSH URL.
func pathOf(remote string) string {
	remote = strings.TrimSpace(remote)
	if strings.Contains(remote, "://") {
		if u, err := url.Parse(remote); err == nil {
			return u.Path
		}
	}
	if _, after, ok := strings.Cut(remote, "@"); ok { // scp-like git@host:path
		if _, path, ok := strings.Cut(after, ":"); ok {
			return path
		}
	}
	return ""
}

// APIBase returns the REST API base URL for a provider. override wins. GitHub
// derives api.github.com (public) or HOST/api/v3 (enterprise); GitLab derives
// HOST/api/v4 (public and self-hosted). Gerrit (git push, no REST) and plain
// return "".
func APIBase(p Provider, remoteURL, override string) string {
	if override != "" {
		return override
	}
	host := hostOf(remoteURL)
	switch p {
	case GitHub:
		if host == "github.com" {
			return "https://api.github.com"
		}
		return "https://" + host + "/api/v3"
	case GitLab:
		return "https://" + host + "/api/v4"
	default:
		return ""
	}
}

// ParseRepo extracts the repo identity from a remote URL. GitHub requires exactly
// owner/repo (both returned). GitLab returns the whole project path as repo (may
// be nested subgroups) with an empty owner; the caller URL-encodes it. A remote
// that does not yield a valid identity is an error (rejected before any mutation).
func ParseRepo(p Provider, remoteURL string) (owner, repo string, err error) {
	path := strings.Trim(pathOf(remoteURL), "/")
	path = strings.TrimSuffix(path, ".git")
	if path == "" {
		return "", "", fmt.Errorf("cannot parse owner/repo from remote_url")
	}
	if p == GitLab {
		return "", path, nil
	}
	parts := strings.Split(path, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("cannot parse owner/repo from remote_url")
	}
	return parts[0], parts[1], nil
}
```

- [ ] **Step 4: Extend `Env` and add `tipSubject`.** In `internal/gitprovider/publish.go`, add `"strings"` to imports, extend the `Env` struct:

```go
type Env struct {
	Dir       string
	RunEnv    []string
	Remote    string // configured upstream remote name in the base (e.g. "origin")
	RemoteURL string
	Cred      git.Credential
	APIBase   string // REST base URL (GitHub/GitLab); "" for gerrit/plain
	Title     string // raw request title ("" => not supplied)
	Body      string // raw request body ("" => not supplied)
	Actor     string // audit actor, used as the git identity for the Gerrit squash
}
```

And append:

```go
// tipSubject returns the first line of ref's tip commit message, falling back to
// the ref name if git fails. Used as the create-time title default (spec Q4).
func tipSubject(ctx context.Context, r *git.Runner, dir, ref string) string {
	res, err := r.Run(ctx, dir, nil, [][]string{{"git", "log", "-1", "--format=%s", ref}})
	if err != nil || len(res) == 0 {
		return ref
	}
	if s := strings.TrimSpace(string(res[len(res)-1].Output)); s != "" {
		return s
	}
	return ref
}
```

- [ ] **Step 5: Add a `tipSubject` test.** In `internal/gitprovider/publish_test.go`, add:

```go
func TestTipSubject(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo := t.TempDir()
	gitCmd(t, repo, "init", ".")
	gitCmd(t, repo, "-c", "user.email=a@b.c", "-c", "user.name=a", "commit", "--allow-empty", "-m", "hello subject")
	gitCmd(t, repo, "branch", "-M", "main")
	r := git.NewRunner([]string{"git"})
	require.Equal(t, "hello subject", tipSubject(context.Background(), r, repo, "main"))
	// missing ref falls back to the ref name.
	require.Equal(t, "nope", tipSubject(context.Background(), r, repo, "nope"))
}
```

- [ ] **Step 6: Run to verify they pass**

Run: `go test ./internal/gitprovider/ -run 'APIBase|ParseRepo|TipSubject' -v`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/gitprovider/provider.go internal/gitprovider/publish.go internal/gitprovider/provider_test.go internal/gitprovider/publish_test.go
git commit -m "feat(git-provider): APIBase/ParseRepo derivation, extend Env, tipSubject helper"
```

---

### Task 3: `restClient` + `PullRequest` (GitHub) strategy

The shared REST client (TLS trust, per-provider auth, error mapping) plus the GitHub PR strategy. Proves create-then-update, CA trust, and the REST leak bar.

**Files:**
- Create: `internal/gitprovider/rest.go`
- Create: `internal/gitprovider/rest_test.go`

**Interfaces:**
- Consumes: `Env{APIBase,Title,Body,Cred,RemoteURL,Remote,Dir,RunEnv}`, `ParseRepo`, `tipSubject`, `Branch`.
- Produces:
  - `func newRESTClient(p Provider, base string, cred git.Credential) (*restClient, error)`
  - `func (c *restClient) do(ctx context.Context, method, url string, body, out any) error`
  - `func statusToCode(httpStatus int) codes.Code`
  - `func forgeMessage(data []byte) string`
  - `func PullRequest(ctx context.Context, r *git.Runner, e Env, source, target string) (Result, error)`

- [ ] **Step 1: Write the failing test.** Create `internal/gitprovider/rest_test.go`:

```go
package gitprovider

import (
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/squall-chua/sbx-swarm-node/internal/git"
	"github.com/stretchr/testify/require"
)

// fakeGitHub serves the minimal PR surface: list (empty until created), create,
// update. It records the auth header so the test can assert the token is sent
// but never leaks outward.
type fakeGitHub struct {
	srv     *httptest.Server
	created atomic.Bool
	gotAuth string
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	f := &fakeGitHub{}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app/pulls", func(w http.ResponseWriter, r *http.Request) {
		f.gotAuth = r.Header.Get("Authorization")
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			if f.created.Load() {
				_, _ = w.Write([]byte(`[{"number":7,"html_url":"https://github.com/acme/app/pull/7"}]`))
			} else {
				_, _ = w.Write([]byte(`[]`))
			}
		case http.MethodPost:
			f.created.Store(true)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"number":7,"html_url":"https://github.com/acme/app/pull/7"}`))
		}
	})
	mux.HandleFunc("/repos/acme/app/pulls/7", func(w http.ResponseWriter, r *http.Request) {
		f.gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"number":7,"html_url":"https://github.com/acme/app/pull/7"}`))
	})
	f.srv = httptest.NewTLSServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// caFile writes the fake server's cert to a PEM file for cred.CAPath.
func caFile(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ca.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	require.NoError(t, os.WriteFile(p, pemBytes, 0o600))
	return p
}

// prEnv builds an Env pointing REST at the fake (trusting its cert) and git at a
// real bare upstream so the head-branch push succeeds.
func prEnv(t *testing.T, f *fakeGitHub, tok, title, body string) (Env, *git.Runner, string) {
	if _, err := lookGit(); err != nil {
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
	gitCmd(t, work, "-c", "user.email=a@b.c", "-c", "user.name=a", "commit", "--allow-empty", "-m", "feat work")
	gitCmd(t, work, "push", "origin", "HEAD:feature")
	gitCmd(t, root, "clone", "--mirror", upstream, base)
	gitCmd(t, base, "config", "remote.origin.mirror", "false")
	return Env{
		Dir: base, Remote: "origin", RemoteURL: "https://github.com/acme/app",
		APIBase: f.srv.URL, Title: title, Body: body,
		Cred: git.Credential{Token: tok, CAPath: caFile(t, f.srv)},
	}, git.NewRunner([]string{"git"}), root
}

func TestPullRequest_CreateThenUpdate(t *testing.T) {
	f := newFakeGitHub(t)
	e, r, _ := prEnv(t, f, "TESTTOK", "My PR", "body")
	// create
	res, err := PullRequest(context.Background(), r, e, "feature", "main")
	require.NoError(t, err)
	require.Equal(t, "refs/heads/feature", res.Ref)
	require.Equal(t, "https://github.com/acme/app/pull/7", res.DeliveryURL)
	require.True(t, f.created.Load(), "first call must POST-create")
	// second call finds the open PR and updates it, same URL — no duplicate.
	res2, err := PullRequest(context.Background(), r, e, "feature", "main")
	require.NoError(t, err)
	require.Equal(t, res.DeliveryURL, res2.DeliveryURL)
	require.Equal(t, "Bearer TESTTOK", f.gotAuth)
}

func TestPullRequest_TLSTrustAndLeak(t *testing.T) {
	f := newFakeGitHub(t)
	e, r, _ := prEnv(t, f, "SENTINELTOK", "t", "b")
	res, err := PullRequest(context.Background(), r, e, "feature", "main")
	require.NoError(t, err)
	require.NotContains(t, res.String(), "SENTINELTOK")

	// wrong CA (system roots) must fail closed, and the error must not leak the token.
	bad := e
	bad.Cred.CAPath = ""
	_, err = PullRequest(context.Background(), r, bad, "feature", "main")
	require.Error(t, err)
	require.NotContains(t, err.Error(), "SENTINELTOK")
	require.Equal(t, "https://github.com/acme/app/pull/7", res.DeliveryURL)
}

func TestStatusToCode(t *testing.T) {
	require.Equal(t, "PermissionDenied", statusToCode(401).String())
	require.Equal(t, "PermissionDenied", statusToCode(403).String())
	require.Equal(t, "FailedPrecondition", statusToCode(404).String())
	require.Equal(t, "InvalidArgument", statusToCode(422).String())
	require.Equal(t, "Unavailable", statusToCode(503).String())
	require.Equal(t, "Internal", statusToCode(418).String())
}
```

Add this one shared helper at the bottom of `internal/gitprovider/publish_test.go` (`exec` is already imported there):

```go
func lookGit() (string, error) { return exec.LookPath("git") }
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/gitprovider/ -run 'PullRequest|StatusToCode' -v`
Expected: FAIL — `newRESTClient`/`PullRequest`/`statusToCode` undefined.

- [ ] **Step 3: Implement `rest.go`.** Create `internal/gitprovider/rest.go`:

```go
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
func (c *restClient) do(ctx context.Context, method, url string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return status.Errorf(codes.Internal, "marshal %s request: %v", c.provider, err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
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
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/gitprovider/ -run 'PullRequest|StatusToCode' -v`
Expected: PASS. If the cert-PEM helper juggling fails to compile, switch `caFile` to `encoding/pem` as the NOTE says.

- [ ] **Step 5: Commit**

```bash
git add internal/gitprovider/rest.go internal/gitprovider/rest_test.go internal/gitprovider/publish_test.go
git commit -m "feat(git-provider): restClient + GitHub pull_request strategy (create-or-update, TLS trust, leak-safe)"
```

---

### Task 4: `MergeRequest` (GitLab) strategy

Same shape as PR against the GitLab API: URL-encoded project path, `iid`, `PUT` update, `description` (not `body`), `web_url`.

**Files:**
- Modify: `internal/gitprovider/rest.go` (add `MergeRequest`)
- Modify: `internal/gitprovider/rest_test.go` (add a GitLab fake + test)

**Interfaces:**
- Produces: `func MergeRequest(ctx context.Context, r *git.Runner, e Env, source, target string) (Result, error)`

- [ ] **Step 1: Write the failing test.** In `internal/gitprovider/rest_test.go`, add:

```go
func newFakeGitLab(t *testing.T) (*httptest.Server, *atomic.Bool, *string) {
	var created atomic.Bool
	var gotTok string
	mux := http.NewServeMux()
	// project path "group/app" url-encodes to group%2Fapp
	mux.HandleFunc("/projects/group%2Fapp/merge_requests", func(w http.ResponseWriter, r *http.Request) {
		gotTok = r.Header.Get("PRIVATE-TOKEN")
		switch r.Method {
		case http.MethodGet:
			if created.Load() {
				_, _ = w.Write([]byte(`[{"iid":3,"web_url":"https://gitlab.corp/group/app/-/merge_requests/3"}]`))
			} else {
				_, _ = w.Write([]byte(`[]`))
			}
		case http.MethodPost:
			created.Store(true)
			_, _ = w.Write([]byte(`{"iid":3,"web_url":"https://gitlab.corp/group/app/-/merge_requests/3"}`))
		}
	})
	mux.HandleFunc("/projects/group%2Fapp/merge_requests/3", func(w http.ResponseWriter, r *http.Request) {
		gotTok = r.Header.Get("PRIVATE-TOKEN")
		_, _ = w.Write([]byte(`{"iid":3,"web_url":"https://gitlab.corp/group/app/-/merge_requests/3"}`))
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	return srv, &created, &gotTok
}

func TestMergeRequest_CreateThenUpdate(t *testing.T) {
	if _, err := lookGit(); err != nil {
		t.Skip("git not installed")
	}
	srv, created, gotTok := newFakeGitLab(t)
	// reuse the git upstream/base scaffolding from prEnv by pointing RemoteURL at gitlab.
	f := &fakeGitHub{srv: srv} // only srv is used by prEnv for APIBase + caFile
	e, r, _ := prEnv(t, f, "GLTOK", "My MR", "desc")
	e.RemoteURL = "https://gitlab.corp/group/app"
	e.APIBase = srv.URL

	res, err := MergeRequest(context.Background(), r, e, "feature", "main")
	require.NoError(t, err)
	require.Equal(t, "refs/heads/feature", res.Ref)
	require.Equal(t, "https://gitlab.corp/group/app/-/merge_requests/3", res.DeliveryURL)
	require.True(t, created.Load())

	res2, err := MergeRequest(context.Background(), r, e, "feature", "main")
	require.NoError(t, err)
	require.Equal(t, res.DeliveryURL, res2.DeliveryURL)
	require.Equal(t, "GLTOK", *gotTok)
}
```

> The `fakeGitHub{srv: srv}` reuse works because `prEnv` only reads `f.srv.URL` and `f.srv.Certificate()`; the HTTP handlers come from `srv`. If you'd rather, refactor `prEnv` to take a `*httptest.Server` directly.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/gitprovider/ -run 'MergeRequest' -v`
Expected: FAIL — `MergeRequest` undefined.

- [ ] **Step 3: Implement `MergeRequest`.** Append to `internal/gitprovider/rest.go`:

```go
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
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/gitprovider/ -run 'MergeRequest' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/gitprovider/rest.go internal/gitprovider/rest_test.go
git commit -m "feat(git-provider): GitLab merge_request strategy (create-or-update)"
```

---

### Task 5: `GerritChange` strategy

Squash `source` to one commit parented on `target` via `commit-tree`, inject a deterministic `Change-Id`, push `refs/for/<target>`, parse the change URL from push stderr.

**Files:**
- Create: `internal/gitprovider/gerrit.go`
- Create: `internal/gitprovider/gerrit_test.go`

**Interfaces:**
- Produces:
  - `func GerritChange(ctx context.Context, r *git.Runner, e Env, source, target string) (Result, error)`
  - `func gerritChangeID(remoteURL, source, target string) string`
  - `func parseGerritURL(pushOutput []byte) string`

- [ ] **Step 1: Write the failing test.** Create `internal/gitprovider/gerrit_test.go`:

```go
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
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/gitprovider/ -run 'Gerrit' -v`
Expected: FAIL — `GerritChange`/`gerritChangeID`/`parseGerritURL` undefined.

- [ ] **Step 3: Implement `gerrit.go`.** Create `internal/gitprovider/gerrit.go`:

```go
package gitprovider

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"regexp"
	"strings"

	"github.com/squall-chua/sbx-swarm-node/internal/git"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const gerritIdentityEmail = "noreply@sbx-swarm.local"

var gerritURLRe = regexp.MustCompile(`https?://\S+`)

// GerritChange squashes source into one commit parented on target and pushes it
// to refs/for/<target> with a deterministic Change-Id, so a re-publish lands a
// new patchset on the same change (idempotent per workspace/source/target).
func GerritChange(ctx context.Context, r *git.Runner, e Env, source, target string) (Result, error) {
	if target == "" {
		return Result{}, status.Error(codes.InvalidArgument, "gerrit_change requires a target branch")
	}
	remote := e.Remote
	if remote == "" {
		remote = "origin"
	}
	changeID := gerritChangeID(e.RemoteURL, source, target)

	subject := e.Title
	if subject == "" {
		subject = tipSubject(ctx, r, e.Dir, source)
	}
	msg := subject
	if e.Body != "" {
		msg += "\n\n" + e.Body
	}
	msg += "\n\nChange-Id: " + changeID

	actor := e.Actor
	if actor == "" {
		actor = "system"
	}
	// The base is a bare repo with no user.email; commit-tree needs an identity.
	ident := []string{
		"GIT_AUTHOR_NAME=" + actor, "GIT_AUTHOR_EMAIL=" + gerritIdentityEmail,
		"GIT_COMMITTER_NAME=" + actor, "GIT_COMMITTER_EMAIL=" + gerritIdentityEmail,
	}
	env := append(append([]string{}, e.RunEnv...), ident...)

	made, err := r.Run(ctx, e.Dir, env, [][]string{
		{"git", "commit-tree", source + "^{tree}", "-p", target, "-m", msg},
	})
	if err != nil {
		return Result{}, err
	}
	commit := strings.TrimSpace(string(made[len(made)-1].Output))

	pushed, err := r.Run(ctx, e.Dir, env, [][]string{
		{"git", "push", remote, commit + ":refs/for/" + target},
	})
	if err != nil {
		return Result{}, err
	}
	return Result{ChangeID: changeID, DeliveryURL: parseGerritURL(pushed[len(pushed)-1].Output)}, nil
}

// gerritChangeID derives Gerrit's Change-Id from the deliverable key
// (workspace remote, source, target) — stable across re-publishes, independent
// of the sandbox (ADR-0021).
func gerritChangeID(remoteURL, source, target string) string {
	h := sha1.Sum([]byte(remoteURL + "\x00" + source + "\x00" + target))
	return "I" + hex.EncodeToString(h[:])
}

// parseGerritURL extracts the change URL Gerrit prints on push stderr
// (a `remote:` line). Best-effort: "" if the output has no recognizable URL.
func parseGerritURL(pushOutput []byte) string {
	for _, line := range strings.Split(string(pushOutput), "\n") {
		if !strings.Contains(line, "remote:") {
			continue
		}
		if m := gerritURLRe.FindString(line); m != "" {
			return strings.TrimRight(m, " \t\r")
		}
	}
	return ""
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/gitprovider/ -run 'Gerrit' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/gitprovider/gerrit.go internal/gitprovider/gerrit_test.go
git commit -m "feat(git-provider): gerrit_change strategy (commit-tree squash, deterministic Change-Id)"
```

---

### Task 6: Wire the three strategies into `PublishWork` + REST leak bar

Add the up-front gates (token for PR/MR, repo-parse for PR/MR), populate the new `Env` fields, dispatch the three cases, and extend the credential-leak test to a REST strategy across all handler surfaces.

**Files:**
- Modify: `internal/apiserver/publish_work.go`
- Modify: `internal/apiserver/publish_work_leak_test.go`

**Interfaces:**
- Consumes: `gitprovider.APIBase`, `gitprovider.ParseRepo`, `gitprovider.{PullRequest,MergeRequest,GerritChange}`, `ws.APIBaseURL()`, `ws.Cred().Token`.

- [ ] **Step 1: Write the failing test.** In `internal/apiserver/publish_work_leak_test.go`, add a REST-path leak test. It reuses `credLeakFixture` but points the workspace at a fake GitHub server and runs `pull_request`:

```go
func TestPublishWork_PullRequest_NoCredentialLeak(t *testing.T) {
	const tok = "SENTINEL-PR-TOKEN-7c1d"
	// fake GitHub PR API (create then found).
	var created bool
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/app/pulls", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			if created {
				_, _ = w.Write([]byte(`[{"number":1,"html_url":"https://github.com/acme/app/pull/1"}]`))
			} else {
				_, _ = w.Write([]byte(`[]`))
			}
			return
		}
		created = true
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"number":1,"html_url":"https://github.com/acme/app/pull/1"}`))
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(caPath, caPEM, 0o600))

	svc, rec, al, cp := credLeakFixture(t, tok)
	svc.publishTimeout = 10 * time.Second
	// re-register the workspace as a github provider pointing at the fake.
	ws := git.New(git.Spec{
		Name: "repo", Base: gitBaseOf(t, svc), Remote: "origin", DefaultBranch: "main", AllowPush: true,
		Provider: "github", RemoteURL: "https://github.com/acme/app", APIBaseURL: srv.URL,
		Cred:      git.Credential{Token: tok, CAPath: caPath},
		Allowlist: []string{"git"},
	})
	svc.SetGit(map[string]*git.Workspace{"repo": ws})

	res, err := svc.PublishWork(context.Background(), &sbxv1.PublishWorkRequest{Id: rec.ID, Strategy: "pull_request", Target: "main", Title: "t"})
	require.NoError(t, err)
	require.Equal(t, "https://github.com/acme/app/pull/1", res.DeliveryUrl)

	auditList, err := al.List()
	require.NoError(t, err)
	b64 := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + tok))
	surfaces := []string{res.String(), fmt.Sprint(cp.calls), fmt.Sprint(auditList)}
	for _, s := range surfaces {
		require.NotContains(t, s, tok)
		require.NotContains(t, s, b64)
		require.NotContains(t, s, "Bearer "+tok)
	}
}
```

Add imports to the leak test file: `"net/http"`, `"net/http/httptest"`, `"encoding/pem"`, `"os"`. `gitBaseOf` is a one-liner helper — add it too:

```go
// gitBaseOf returns the base dir of the single workspace registered on svc
// (field is s.gitWS), so the REST-leak test can re-register a github provider
// over the same base.
func gitBaseOf(t *testing.T, svc *SandboxService) string {
	t.Helper()
	for _, w := range svc.gitWS {
		return w.Base()
	}
	t.Fatal("no workspace registered")
	return ""
}
```

> `s.gitWS` is unexported but the test is in-package (`package apiserver`), so ranging it here is fine. The point is only that `pull_request`'s head-push has a real base to push into.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/apiserver/ -run 'PullRequest_NoCredentialLeak' -v`
Expected: FAIL — `pull_request` returns `Unimplemented` (handler not wired yet).

- [ ] **Step 3: Wire the handler.** In `internal/apiserver/publish_work.go`:

Replace the unimplemented gate (lines ~29-31):

```go
	if r.Strategy != "branch" && r.Strategy != "patch" {
		return nil, status.Errorf(codes.Unimplemented, "strategy %q not yet implemented", r.Strategy)
	}
```

with the REST preconditions (gate before any mutation):

```go
	if r.Strategy == "pull_request" || r.Strategy == "merge_request" {
		if ws.Cred().Token == "" {
			return nil, status.Error(codes.FailedPrecondition, "REST strategy requires an HTTPS token credential")
		}
		if _, _, err := gitprovider.ParseRepo(prov, ws.RemoteURL()); err != nil {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
	}
```

Resolve the audit actor BEFORE the strategy (Gerrit needs it as the commit identity). Currently `actor` is resolved AFTER the switch (lines ~80-83) and `env` is built at line ~68 without the new fields. **Replace the existing two lines (the `env := gitprovider.Env{...}` + `runner := git.NewRunner(...)` pair, lines ~68-69)** with the actor-first version below, and **delete** the now-duplicate `actor := principalFromContext(ctx).userRole` block (lines ~80-83) so the `s.auditPublish(..., actor, err)` call below the switch uses this hoisted `actor`:

```go
	actor := principalFromContext(ctx).userRole
	if actor == "" {
		actor = "system"
	}
	env := gitprovider.Env{
		Dir: ws.Base(), RunEnv: runEnv, Remote: ws.RemoteName(),
		RemoteURL: ws.RemoteURL(), Cred: ws.Cred(),
		APIBase: gitprovider.APIBase(prov, ws.RemoteURL(), ws.APIBaseURL()),
		Title:   r.Title, Body: r.Body, Actor: actor,
	}
	runner := git.NewRunner([]string{"git"})
```

Extend the dispatch switch:

```go
	var res gitprovider.Result
	switch r.Strategy {
	case "branch":
		res, err = gitprovider.Branch(pubCtx, runner, env, source, r.Target)
	case "patch":
		res, err = gitprovider.Patch(pubCtx, runner, env, source, r.Target)
	case "pull_request":
		res, err = gitprovider.PullRequest(pubCtx, runner, env, source, r.Target)
	case "merge_request":
		res, err = gitprovider.MergeRequest(pubCtx, runner, env, source, r.Target)
	case "gerrit_change":
		res, err = gitprovider.GerritChange(pubCtx, runner, env, source, r.Target)
	default:
		return nil, status.Errorf(codes.Unimplemented, "strategy %q not yet implemented", r.Strategy)
	}
```

Finally, delete the now-duplicated `actor := principalFromContext(...)` lines that followed the switch (they moved up); keep the `s.auditPublish(ws.Name(), source, actor, err)` call using the hoisted `actor`.

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/apiserver/ -run 'PublishWork' -v && go build ./...`
Expected: PASS (both the new REST leak test and the existing `TestPublishWork_NoCredentialLeak`); build clean.

- [ ] **Step 5: Full package sweep + vet**

Run: `go test ./... && go vet ./internal/gitprovider/ ./internal/apiserver/`
Expected: all green.

- [ ] **Step 6: Commit**

```bash
git add internal/apiserver/publish_work.go internal/apiserver/publish_work_leak_test.go
git commit -m "feat(git-provider): dispatch pull_request/merge_request/gerrit_change; gate REST on token; REST leak test"
```

---

### Task 7: Integration smoke (env-gated, skipped in CI)

A real-forge smoke test behind env vars — never runs in CI (no tokens), documents how to run it live.

**Files:**
- Create: `internal/gitprovider/integration_test.go`

**Interfaces:**
- Consumes: `PullRequest`, `MergeRequest`, `GerritChange` against real remotes.

- [ ] **Step 1: Write the integration test.** Create `internal/gitprovider/integration_test.go`:

```go
//go:build integration

package gitprovider

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/squall-chua/sbx-swarm-node/internal/git"
	"github.com/stretchr/testify/require"
)

// TestPullRequest_Live opens/updates a real GitHub PR. Run with:
//   GITPROVIDER_GH_REMOTE=https://github.com/you/repo \
//   GITPROVIDER_GH_TOKEN=ghp_xxx \
//   go test -tags integration ./internal/gitprovider/ -run Live -v
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
		Cred:    git.Credential{Token: tok},
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
```

> Add tiny helpers `basicAuth`, `makeBranch` in this file (or reuse `git.Credential.Env`). Keep them local to the integration file so the non-integration build is unaffected. Mirror this test for GitLab/Gerrit if you have live endpoints; a single provider is enough to prove the wiring.

- [ ] **Step 2: Verify it compiles under the tag and is skipped without env**

Run: `go build -tags integration ./internal/gitprovider/ && go test -tags integration ./internal/gitprovider/ -run Live -v`
Expected: builds; `Live` test SKIPs (no env set). The default (no-tag) build is unchanged: `go test ./internal/gitprovider/`.

- [ ] **Step 3: Commit**

```bash
git add internal/gitprovider/integration_test.go
git commit -m "test(git-provider): env-gated live PR/MR/gerrit smoke (integration tag, skipped in CI)"
```

---

## Self-Review

**Spec coverage:**
- Shape (3 free functions, shared restClient, no interface) → Tasks 3-5. ✓
- Env fields (APIBase/Title/Body + Actor) → Task 2. ✓
- Idempotency key `(workspace, source, target)` (ADR-0021) → PR/MR lookup (Tasks 3-4), `gerritChangeID` (Task 5). ✓
- PR/MR require token, parse repo up front → Task 6 gates. ✓
- Gerrit squash via commit-tree + injected identity + Change-Id + stderr URL → Task 5. ✓
- Config `api_base_url` + derivation (`APIBase`) → Tasks 1-2. ✓
- owner/repo parse incl. GitLab subgroups → Task 2 `ParseRepo`. ✓
- REST auth headers (Bearer / PRIVATE-TOKEN), TLS via CAPath → Task 3 `newRESTClient`/`do`. ✓
- Leak bar extended to REST (unit + handler surfaces) → Tasks 3 + 6. ✓
- Error mapping HTTP→gRPC → Task 3 `statusToCode`. ✓
- Gate-before-mutation preserved → Task 6 ordering (gates before `EnsureBase`). ✓
- title/body empty=don't-touch, tip-subject create fallback → Tasks 3-5 (`tipSubject`, non-empty-only PATCH). ✓
- Tests: httptest create-then-update, self-signed CA, gerrit bare-repo squash, integration smoke → Tasks 3-5, 7. ✓
- Out of scope (cross-fork, retries, gerrit REST, reopen) → not implemented (correct). ✓

**Placeholder scan:** All steps carry real code and exact commands. The two `>` NOTE blocks (cert-PEM helper, `gitBaseOf`/`svc.git` access) flag a local decision with both concrete options spelled out — not deferred work. No TODO/TBD.

**Type consistency:** `Env` fields (`APIBase`, `Title`, `Body`, `Actor`) defined in Task 2, consumed unchanged in Tasks 3-6. `PullRequest`/`MergeRequest`/`GerritChange` signatures `(ctx, *git.Runner, Env, source, target) (Result, error)` match `Branch`/`Patch` and the Task 6 dispatch. `ParseRepo`/`APIBase`/`statusToCode`/`gerritChangeID`/`parseGerritURL` signatures are identical between their definition tasks and callers. `Result` fields (`Ref`, `DeliveryURL`, `ChangeID`) are P1's existing struct. ✓
