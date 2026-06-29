package apiserver

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/squall-chua/sbx-swarm-node/internal/audit"
	"github.com/squall-chua/sbx-swarm-node/internal/auth"
	"github.com/squall-chua/sbx-swarm-node/internal/sandbox"
	"github.com/squall-chua/sbx-swarm-node/internal/store"
	"github.com/stretchr/testify/require"
)

func TestResolveUploadDest(t *testing.T) {
	cases := []struct {
		in, want string
		err      bool
	}{
		{in: "/etc/app.conf", want: "/etc/app.conf"},       // absolute → verbatim
		{in: "report.txt", want: "/home/agent/report.txt"}, // relative → default dir
		{in: "sub/a.txt", want: "/home/agent/sub/a.txt"},
		{in: "", err: true},                 // empty
		{in: "/home/agent/", err: true},     // trailing slash / bare dir
		{in: "../../etc/passwd", err: true}, // traversal
		{in: "a/../b", err: true},
	}
	for _, c := range cases {
		got, err := resolveUploadDest(c.in)
		if c.err {
			require.Error(t, err, c.in)
			continue
		}
		require.NoError(t, err, c.in)
		require.Equal(t, c.want, got)
	}
}

// filesTestServer wires Authenticate → RequireRole(admin) → FilesHandler over the
// fake backend, returning the handler plus the fake and service for assertions.
// keyMap{} and testSigner() are shared helpers from rolegate_test.go.
func filesTestServer(t *testing.T) (http.Handler, *sandbox.Fake, *SandboxService, string) {
	t.Helper()
	svc := newSandboxSvc(t) // helper in sandboxservice_test.go: fake-backed service
	fake := svc.mgr.Backend().(*sandbox.Fake)
	rec, err := svc.mgr.Create(context.Background(), sandbox.CreateSpec{})
	require.NoError(t, err)
	mw := auth.New(keyMap{"adm": "admin", "ro": "read-only"}, testSigner())
	h := mw.Authenticate(mw.RequireRole("admin", svc.FilesHandler()))
	return h, fake, svc, rec.ID
}

func TestUpload_AdminWritesFileAndBumpsActivity(t *testing.T) {
	h, fake, svc, id := filesTestServer(t)
	var gotLocal, gotRemote string
	fake.CopyToFunc = func(_, localPath, remotePath string) error {
		b, _ := os.ReadFile(localPath)
		require.Equal(t, "hello", string(b)) // body was staged to the temp file
		gotLocal, gotRemote = localPath, remotePath
		return nil
	}
	before, _ := svc.mgr.Get(context.Background(), id)

	req := httptest.NewRequest(http.MethodPut, "/v1/sandboxes/"+id+"/files?path=report.txt", strings.NewReader("hello"))
	req.Header.Set("Authorization", "Bearer adm")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Equal(t, "/home/agent/report.txt", gotRemote)
	require.NotEmpty(t, gotLocal)
	after, _ := svc.mgr.Get(context.Background(), id)
	require.True(t, after.LastActivity.After(before.LastActivity), "upload bumps Activity")
}

func TestUpload_ReadOnlyForbidden(t *testing.T) {
	h, _, _, id := filesTestServer(t)
	req := httptest.NewRequest(http.MethodPut, "/v1/sandboxes/"+id+"/files?path=x.txt", strings.NewReader("x"))
	req.Header.Set("Authorization", "Bearer ro")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusForbidden, rec.Code)
}

func TestUpload_BadPath400(t *testing.T) {
	h, _, _, id := filesTestServer(t)
	req := httptest.NewRequest(http.MethodPut, "/v1/sandboxes/"+id+"/files?path=/home/agent/", strings.NewReader("x"))
	req.Header.Set("Authorization", "Bearer adm")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestUpload_TooLarge413(t *testing.T) {
	h, _, svc, id := filesTestServer(t)
	svc.SetMaxUploadBytes(4)
	req := httptest.NewRequest(http.MethodPut, "/v1/sandboxes/"+id+"/files?path=x.txt", strings.NewReader("way too big"))
	req.Header.Set("Authorization", "Bearer adm")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
}

func TestUpload_AuditsActor(t *testing.T) {
	svc := newSandboxSvc(t)
	st, err := store.Open(filepath.Join(t.TempDir(), "audit.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	svc.SetAudit(audit.New(st, func() int64 { return 1 }))
	fake := svc.mgr.Backend().(*sandbox.Fake)
	fake.CopyToFunc = func(_, _, _ string) error { return nil }
	rec, err := svc.mgr.Create(context.Background(), sandbox.CreateSpec{})
	require.NoError(t, err)
	mw := auth.New(keyMap{"adm": "admin"}, testSigner())
	h := mw.Authenticate(mw.RequireRole("admin", svc.FilesHandler()))

	req := httptest.NewRequest(http.MethodPut, "/v1/sandboxes/"+rec.ID+"/files?path=a.txt", strings.NewReader("x"))
	req.Header.Set("Authorization", "Bearer adm")
	h.ServeHTTP(httptest.NewRecorder(), req)

	entries, err := svc.audit.List()
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "file.upload", entries[0].Action)
	require.Equal(t, "admin", entries[0].Actor)
	require.Equal(t, "/home/agent/a.txt", entries[0].Target)
}

func TestDownload_AdminStreamsFileNoActivity(t *testing.T) {
	h, fake, svc, id := filesTestServer(t)
	fake.CopyFromFunc = func(_, remotePath, localPath string) error {
		require.Equal(t, "/home/agent/out.txt", remotePath)
		return os.WriteFile(localPath, []byte("payload"), 0o600) // simulate sbx cp
	}
	before, _ := svc.mgr.Get(context.Background(), id)

	req := httptest.NewRequest(http.MethodGet, "/v1/sandboxes/"+id+"/files?path=/home/agent/out.txt", nil)
	req.Header.Set("Authorization", "Bearer adm")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "payload", rec.Body.String())
	require.Contains(t, rec.Header().Get("Content-Disposition"), `filename="out.txt"`)
	after, _ := svc.mgr.Get(context.Background(), id)
	require.Equal(t, before.LastActivity, after.LastActivity, "download does NOT bump Activity")
}

func TestDownload_RelativePath400(t *testing.T) {
	h, _, _, id := filesTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/sandboxes/"+id+"/files?path=out.txt", nil)
	req.Header.Set("Authorization", "Bearer adm")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestDownload_ReadOnlyForbidden(t *testing.T) {
	h, _, _, id := filesTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/sandboxes/"+id+"/files?path=/x", nil)
	req.Header.Set("Authorization", "Bearer ro")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusForbidden, rec.Code)
}

func TestTerminal_ReadOnlyForbidden(t *testing.T) {
	addr, cleanup := startRoleGateServer(t)
	defer cleanup()
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	// RequireRole runs before the sandbox is resolved, so any id works; a read-only
	// key must be rejected with 403 before the WebSocket upgrade is attempted.
	req, _ := http.NewRequest(http.MethodGet, "https://"+addr+"/v1/sandboxes/n1.x/terminal", nil)
	req.Header.Set("Authorization", "Bearer ro")
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusForbidden, resp.StatusCode, "read-only must not open a terminal")
}
