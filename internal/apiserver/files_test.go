package apiserver

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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

// fakeContainerFS simulates the sandbox filesystem for the exec-based transfer:
// it interprets the mkdir / truncate / base64-append / dd-read / stat / mv / rm
// commands the copy helpers issue, so tests assert real byte round-trips. Fail
// injection (failAppends / shortReads) exercises the retry paths.
type fakeContainerFS struct {
	mu          sync.Mutex
	files       map[string][]byte
	execCmds    [][]string
	appendCalls int
	failAppends int // the first N base64-append execs return a non-zero exit
	readCalls   int
	shortReads  int // the first N dd-read execs return a byte-short result
}

// ranExec reports whether the given argv was executed in the sandbox.
func (fs *fakeContainerFS) ranExec(want ...string) bool {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	for _, c := range fs.execCmds {
		if len(c) == len(want) {
			match := true
			for i := range c {
				if c[i] != want[i] {
					match = false
					break
				}
			}
			if match {
				return true
			}
		}
	}
	return false
}

func newFakeContainerFS() *fakeContainerFS { return &fakeContainerFS{files: map[string][]byte{}} }

// put pre-seeds a container file (used by download tests; the in-container source
// is always whole — only the transfer to the host can arrive short).
func (fs *fakeContainerFS) put(p string, b []byte) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.files[p] = append([]byte(nil), b...)
}

func (fs *fakeContainerFS) wire(f *sandbox.Fake) {
	f.ExecFunc = func(_ string, cmd []string) (sandbox.ExecResult, error) {
		fs.mu.Lock()
		defer fs.mu.Unlock()
		fs.execCmds = append(fs.execCmds, cmd)
		switch {
		case cmd[0] == "mkdir":
			return sandbox.ExecResult{}, nil
		case cmd[0] == "stat": // stat -c %s <path>
			b, ok := fs.files[cmd[len(cmd)-1]]
			if !ok {
				return sandbox.ExecResult{ExitCode: 1, Stderr: []byte("stat: No such file")}, nil
			}
			return sandbox.ExecResult{Stdout: []byte(strconv.Itoa(len(b)) + "\n")}, nil
		case cmd[0] == "mv": // mv -f <src> <dst>
			src, dst := cmd[len(cmd)-2], cmd[len(cmd)-1]
			fs.files[dst] = fs.files[src]
			delete(fs.files, src)
			return sandbox.ExecResult{}, nil
		case cmd[0] == "rm": // rm -f <path>
			delete(fs.files, cmd[len(cmd)-1])
			return sandbox.ExecResult{}, nil
		case cmd[0] == "sh" && strings.HasPrefix(cmd[2], ": >"): // truncate: $0=cmd[3]
			fs.files[cmd[3]] = []byte{}
			return sandbox.ExecResult{}, nil
		case cmd[0] == "sh" && strings.Contains(cmd[2], "base64 -d"): // append: $0=dest, $1..=b64
			fs.appendCalls++
			if fs.appendCalls <= fs.failAppends {
				return sandbox.ExecResult{ExitCode: 1, Stderr: []byte("injected append failure")}, nil
			}
			dest := cmd[3]
			for _, a := range cmd[4:] {
				dec, err := base64.StdEncoding.DecodeString(a)
				if err != nil {
					return sandbox.ExecResult{ExitCode: 1, Stderr: []byte("base64: invalid input")}, nil
				}
				fs.files[dest] = append(fs.files[dest], dec...)
			}
			return sandbox.ExecResult{}, nil
		case cmd[0] == "sh" && strings.HasPrefix(cmd[2], "dd "): // read block: $0=path, $1=block
			fs.readCalls++
			data := fs.files[cmd[3]]
			block, _ := strconv.Atoi(cmd[4])
			start := block * execChunkRaw
			if start >= len(data) {
				return sandbox.ExecResult{}, nil // past EOF → empty stdout
			}
			end := start + execChunkRaw
			if end > len(data) {
				end = len(data)
			}
			chunk := data[start:end]
			if fs.readCalls <= fs.shortReads && len(chunk) > 0 {
				chunk = chunk[:len(chunk)-1] // exec-stdout truncated
			}
			return sandbox.ExecResult{Stdout: []byte(base64.StdEncoding.EncodeToString(chunk))}, nil
		}
		return sandbox.ExecResult{}, nil
	}
}

func (fs *fakeContainerFS) get(p string) ([]byte, bool) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	b, ok := fs.files[p]
	return b, ok
}

// anyTemp reports whether any staged ".part" temp survived the copy.
func (fs *fakeContainerFS) anyTemp() bool {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	for k := range fs.files {
		if strings.Contains(k, ".part") {
			return true
		}
	}
	return false
}

func TestUpload_AdminWritesFileAndBumpsActivity(t *testing.T) {
	h, fake, svc, id := filesTestServer(t)
	fs := newFakeContainerFS()
	fs.wire(fake)
	before, _ := svc.mgr.Get(context.Background(), id)

	req := httptest.NewRequest(http.MethodPut, "/v1/sandboxes/"+id+"/files?path=report.txt", strings.NewReader("hello"))
	req.Header.Set("Authorization", "Bearer adm")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	require.Equal(t, http.StatusNoContent, rec.Code)
	got, ok := fs.get("/home/agent/report.txt")
	require.True(t, ok, "file landed at the destination")
	require.Equal(t, "hello", string(got))
	after, _ := svc.mgr.Get(context.Background(), id)
	require.True(t, after.LastActivity.After(before.LastActivity), "upload bumps Activity")
}

func TestCopyFileToSandbox_RetriesUntilVerified(t *testing.T) {
	svc := newSandboxSvc(t)
	fake := svc.mgr.Backend().(*sandbox.Fake)
	fs := newFakeContainerFS()
	fs.failAppends = 2 // first two append attempts fail; the third whole stream is whole
	fs.wire(fake)
	rec, err := svc.mgr.Create(context.Background(), sandbox.CreateSpec{})
	require.NoError(t, err)
	name, err := svc.mgr.Resolve(context.Background(), rec.ID)
	require.NoError(t, err)
	local := filepath.Join(t.TempDir(), "src")
	require.NoError(t, os.WriteFile(local, []byte("important-bytes"), 0o600))

	err = copyFileToSandbox(context.Background(), fake, name, local, "/home/agent/doc.pdf")
	require.NoError(t, err)
	require.Equal(t, 3, fs.appendCalls, "retried the whole stream past the failed appends")
	got, ok := fs.get("/home/agent/doc.pdf")
	require.True(t, ok)
	require.Equal(t, "important-bytes", string(got), "re-truncate means no double-append")
	require.False(t, fs.anyTemp(), "the temp is renamed into place, not left behind")
}

func TestCopyFileToSandbox_CreatesDestinationDir(t *testing.T) {
	svc := newSandboxSvc(t)
	fake := svc.mgr.Backend().(*sandbox.Fake)
	fs := newFakeContainerFS()
	fs.wire(fake)
	rec, err := svc.mgr.Create(context.Background(), sandbox.CreateSpec{})
	require.NoError(t, err)
	name, err := svc.mgr.Resolve(context.Background(), rec.ID)
	require.NoError(t, err)
	local := filepath.Join(t.TempDir(), "src")
	require.NoError(t, os.WriteFile(local, []byte("data"), 0o600))

	err = copyFileToSandbox(context.Background(), fake, name, local, "/home/agent/uploads/deep/report.pdf")
	require.NoError(t, err)
	require.True(t, fs.ranExec("mkdir", "-p", "/home/agent/uploads/deep"),
		"destination folder is created before copy (cp into a missing dir fails)")
}

func TestCopyFileToSandbox_FailsAfterMaxTries(t *testing.T) {
	svc := newSandboxSvc(t)
	fake := svc.mgr.Backend().(*sandbox.Fake)
	fs := newFakeContainerFS()
	fs.failAppends = 1000 // every append attempt fails
	fs.wire(fake)
	rec, err := svc.mgr.Create(context.Background(), sandbox.CreateSpec{})
	require.NoError(t, err)
	name, err := svc.mgr.Resolve(context.Background(), rec.ID)
	require.NoError(t, err)
	local := filepath.Join(t.TempDir(), "src")
	require.NoError(t, os.WriteFile(local, []byte("data"), 0o600))

	err = copyFileToSandbox(context.Background(), fake, name, local, "/home/agent/doc.pdf")
	require.Error(t, err)
	require.Equal(t, transferTries, fs.appendCalls)
	_, ok := fs.get("/home/agent/doc.pdf")
	require.False(t, ok, "destination is never written on persistent failure")
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
	newFakeContainerFS().wire(fake)
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
	fs := newFakeContainerFS()
	fs.put("/home/agent/out.txt", []byte("payload"))
	fs.wire(fake)
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

func TestCopyFileFromSandbox_RetriesUntilVerified(t *testing.T) {
	svc := newSandboxSvc(t)
	fake := svc.mgr.Backend().(*sandbox.Fake)
	fs := newFakeContainerFS()
	fs.put("/home/agent/out.bin", []byte("important-bytes"))
	fs.shortReads = 2 // first two reads of the block arrive short; the third is whole
	fs.wire(fake)
	rec, err := svc.mgr.Create(context.Background(), sandbox.CreateSpec{})
	require.NoError(t, err)
	name, err := svc.mgr.Resolve(context.Background(), rec.ID)
	require.NoError(t, err)
	local := filepath.Join(t.TempDir(), "dl")

	err = copyFileFromSandbox(context.Background(), fake, name, "/home/agent/out.bin", local)
	require.NoError(t, err)
	require.Equal(t, 3, fs.readCalls, "retried past the short reads")
	got, err := os.ReadFile(local)
	require.NoError(t, err)
	require.Equal(t, "important-bytes", string(got))
}

func TestCopyFileFromSandbox_FailsAfterMaxTries(t *testing.T) {
	svc := newSandboxSvc(t)
	fake := svc.mgr.Backend().(*sandbox.Fake)
	fs := newFakeContainerFS()
	fs.put("/home/agent/out.bin", []byte("data"))
	fs.shortReads = 1000 // every read arrives short
	fs.wire(fake)
	rec, err := svc.mgr.Create(context.Background(), sandbox.CreateSpec{})
	require.NoError(t, err)
	name, err := svc.mgr.Resolve(context.Background(), rec.ID)
	require.NoError(t, err)
	local := filepath.Join(t.TempDir(), "dl")

	err = copyFileFromSandbox(context.Background(), fake, name, "/home/agent/out.bin", local)
	require.Error(t, err)
	require.Equal(t, transferTries, fs.readCalls)
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
