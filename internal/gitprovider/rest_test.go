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
