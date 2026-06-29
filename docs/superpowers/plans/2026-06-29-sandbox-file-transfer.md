# Sandbox File Transfer Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let an admin upload a single file into, and download a single file out of, a sandbox over REST and from the console Files tab.

**Architecture:** A raw HTTP handler (`filesMux`) intercepts `/v1/sandboxes/{id}/files`, wired inside `OwnerProxy` exactly like the terminal handler (so cross-node is reverse-proxied for free). It stages the file through a host temp file and calls the existing `Backend().CopyTo`/`CopyFrom` primitives (which shell out to `sbx cp`). The handler is admin-gated by wrapping it in `mw.RequireRole("admin", …)`; the same wrap is added to the terminal (currently authN-only).

**Tech Stack:** Go (net/http, `internal/apiserver`), the `sbx-go-sdk` backend, Nuxt 4 + `@nuxt/ui` v4 console, Vitest.

## Global Constraints

- **Spec:** `docs/superpowers/specs/2026-06-29-sandbox-file-transfer-design.md`. Glossary term **File transfer** in `CONTEXT.md`.
- **Auth:** admin-only **both** directions, via `mw.RequireRole("admin", …)`. Raw handlers bypass the gRPC `authz` interceptor and must self-gate. `401` unauth, `403` non-admin.
- **Default upload dir:** `/home/agent` (const `defaultUploadDir = "/home/agent"`).
- **Upload destination must be a full file path:** relative → `/home/agent/<rel>`; reject empty, `..`, and trailing-slash/bare-dir dests.
- **Size cap:** upload only, default `100 << 20` (100 MiB), config `max_upload_bytes` (0 → default), over cap → `413`.
- **Activity:** upload bumps `BumpActivity`; download does **not**.
- **Audit:** both — `file.upload` / `file.download`, actor = role from `auth.RoleFromContext`, target = resolved path, outcome ok|error.
- **gofmt only the files you touch** (repo is broadly gofmt-dirty but unenforced).
- **Run Go tests** with the bare-repo env override is NOT needed here (no bare-repo tests touched): plain `go test ./internal/apiserver/ ./internal/sandbox/ ./internal/config/` is fine.
- **Web build** after `.vue`/`.ts` changes: `cd web && bash scripts/build.sh` (the Go binary embeds the SPA). Web tests: `cd web && npm test`.

---

### Task 1: Upload — handler, plumbing, wired end-to-end

**Files:**
- Create: `internal/apiserver/files.go`
- Create: `internal/apiserver/files_test.go`
- Modify: `internal/sandbox/fake.go` (add `CopyToFunc` hook)
- Modify: `internal/apiserver/sandboxservice.go` (add `maxUploadBytes` field + `SetMaxUploadBytes`)
- Modify: `internal/apiserver/server.go` (wire `filesMux` with the admin gate)

**Interfaces:**
- Produces:
  - `func filesMux(handler, next http.Handler) http.Handler`
  - `func (s *SandboxService) FilesHandler() http.Handler`
  - `func (s *SandboxService) SetMaxUploadBytes(n int64)`
  - `func resolveUploadDest(rawPath string) (string, error)`
  - `const defaultUploadDir = "/home/agent"`, `const defaultMaxUploadBytes int64 = 100 << 20`
  - Fake field `CopyToFunc func(name, localPath, remotePath string) error`
- Consumes: `Backend().CopyTo`, `mgr.Resolve`, `mgr.BumpActivity`, `s.audit`, `auth.RoleFromContext`.

- [ ] **Step 1: Write the failing test for `resolveUploadDest` (pure logic)**

Create `internal/apiserver/files_test.go`. Imports grow across the steps below
(later tests add `context`, `net/http`, `net/http/httptest`, `os`, `path/filepath`,
`strings`, and `internal/{audit,auth,sandbox,store}`) — run
`goimports -w internal/apiserver/files_test.go` after pasting each test so the
import set matches the code present.

```go
package apiserver

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResolveUploadDest(t *testing.T) {
	cases := []struct {
		in, want string
		err      bool
	}{
		{in: "/etc/app.conf", want: "/etc/app.conf"},      // absolute → verbatim
		{in: "report.txt", want: "/home/agent/report.txt"}, // relative → default dir
		{in: "sub/a.txt", want: "/home/agent/sub/a.txt"},
		{in: "", err: true},               // empty
		{in: "/home/agent/", err: true},   // trailing slash / bare dir
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
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/apiserver/ -run TestResolveUploadDest -v`
Expected: FAIL — `undefined: resolveUploadDest`.

- [ ] **Step 3: Create `internal/apiserver/files.go` with the path helper + skeleton**

```go
package apiserver

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path"
	"strings"

	"github.com/squall-chua/sbx-swarm-node/internal/audit"
	"github.com/squall-chua/sbx-swarm-node/internal/auth"
)

const (
	defaultUploadDir            = "/home/agent"
	defaultMaxUploadBytes int64 = 100 << 20 // 100 MiB
)

// resolveUploadDest turns the request path into an absolute container *file* path.
// Relative paths land under defaultUploadDir; bare directories and traversal are
// rejected (a dir dest would make `sbx cp` name the file after our temp file).
func resolveUploadDest(rawPath string) (string, error) {
	p := strings.TrimSpace(rawPath)
	if p == "" {
		return "", errors.New("path is required")
	}
	if strings.HasSuffix(p, "/") {
		return "", errors.New("path must be a file, not a directory")
	}
	if !path.IsAbs(p) {
		p = path.Join(defaultUploadDir, p)
	}
	clean := path.Clean(p)
	if clean != p || strings.Contains(rawPath, "..") {
		return "", errors.New("path must not contain '..'")
	}
	return clean, nil
}

// filesSandboxID returns the {id} from /v1/sandboxes/{id}/files.
func filesSandboxID(p string) (string, bool) {
	const pre = "/v1/sandboxes/"
	if !strings.HasPrefix(p, pre) || !strings.HasSuffix(p, "/files") {
		return "", false
	}
	id := strings.TrimSuffix(strings.TrimPrefix(p, pre), "/files")
	if id == "" || strings.Contains(id, "/") {
		return "", false
	}
	return id, true
}

// filesMux intercepts /v1/sandboxes/{id}/files and serves the file handler; all
// other requests fall through to next. It sits inside OwnerProxy, so a remote
// sandbox's request is already proxied to its owner.
func filesMux(handler, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id, ok := filesSandboxID(r.URL.Path); ok && id != "" {
			handler.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// FilesHandler transfers a single file in or out of a sandbox. Admin enforcement
// is done by wrapping this in RequireRole in server.go.
func (s *SandboxService) FilesHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := filesSandboxID(r.URL.Path)
		if !ok {
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case http.MethodPut:
			s.handleUpload(w, r, id)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
}

func (s *SandboxService) handleUpload(w http.ResponseWriter, r *http.Request, id string) {
	dest, err := resolveUploadDest(r.URL.Query().Get("path"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	name, err := s.mgr.Resolve(r.Context(), id)
	if err != nil {
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}
	cap := s.maxUploadBytes
	if cap <= 0 {
		cap = defaultMaxUploadBytes
	}
	tmp, err := os.CreateTemp("", "sbxup-*")
	if err != nil {
		http.Error(w, "stage temp file", http.StatusInternalServerError)
		return
	}
	defer os.Remove(tmp.Name())
	_, copyErr := io.Copy(tmp, http.MaxBytesReader(w, r.Body, cap))
	_ = tmp.Close()
	var maxErr *http.MaxBytesError
	if errors.As(copyErr, &maxErr) {
		http.Error(w, "file too large", http.StatusRequestEntityTooLarge)
		return
	}
	if copyErr != nil {
		http.Error(w, "read body", http.StatusInternalServerError)
		return
	}
	err = s.mgr.Backend().CopyTo(r.Context(), name, tmp.Name(), dest)
	s.auditFile("file.upload", dest, r, err)
	if err != nil {
		http.Error(w, "copy failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	_ = s.mgr.BumpActivity(r.Context(), id) // upload is Activity
	w.WriteHeader(http.StatusNoContent)
}

// auditFile records a file transfer; actor is the authenticated role.
func (s *SandboxService) auditFile(action, target string, r *http.Request, err error) {
	if s.audit == nil {
		return
	}
	actor, _ := auth.RoleFromContext(r.Context())
	if actor == "" {
		actor = "system"
	}
	outcome := "ok"
	if err != nil {
		outcome = "error"
	}
	_ = s.audit.Record(audit.Entry{Actor: actor, Action: action, Target: target, Outcome: outcome})
}
```

> `io` is used by `handleUpload` (`io.Copy`); all imports above are used in Task 1.

- [ ] **Step 4: Run the path test to verify it passes**

Run: `go test ./internal/apiserver/ -run TestResolveUploadDest -v`
Expected: PASS.

- [ ] **Step 5: Add the `CopyToFunc` hook to the Fake**

In `internal/sandbox/fake.go`, add to the hooks block (next to `CopyFromFunc`):

```go
	CopyToFunc func(name, localPath, remotePath string) error
```

Replace `func (f *Fake) CopyTo`:

```go
func (f *Fake) CopyTo(_ context.Context, name, localPath, remotePath string) error {
	if _, err := f.Get(context.Background(), name); err != nil {
		return err
	}
	if f.CopyToFunc != nil {
		return f.CopyToFunc(name, localPath, remotePath)
	}
	return nil
}
```

- [ ] **Step 6: Add the `maxUploadBytes` field + setter to SandboxService**

In `internal/apiserver/sandboxservice.go`, add to the struct (after `bundleDir`):

```go
	maxUploadBytes   int64 // 0 → defaultMaxUploadBytes; per-request upload ceiling
```

Add near the other setters (after `SetIdleTimeout`):

```go
// SetMaxUploadBytes sets the per-request file-upload ceiling (0 → default).
func (s *SandboxService) SetMaxUploadBytes(n int64) { s.maxUploadBytes = n }
```

- [ ] **Step 7: Wire `filesMux` with the admin gate in `server.go`**

In `internal/apiserver/server.go`, inside the `if opts.Sandboxes != nil {` block (where `terminalMux` is wired, ~L121), add the files line:

```go
	if opts.Sandboxes != nil {
		v1 = terminalMux(opts.Sandboxes.TerminalHandler(), v1)
		v1 = filesMux(mw.RequireRole("admin", opts.Sandboxes.FilesHandler()), v1)
	}
```

(Terminal admin-gating is Task 3 — leave `terminalMux` as-is here for now.)

- [ ] **Step 8: Write the failing integration test for upload (admin, gate, 413, 400)**

Append to `internal/apiserver/files_test.go`:

```go
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
	fake.CopyToFunc = func(_ , localPath, remotePath string) error {
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
```

> Download audits the same way via `auditFile` ("file.download"); the upload audit
> test above exercises that shared path.

- [ ] **Step 9: Run the upload tests to verify they pass; fix compile (remove the temporary `var _` stubs only after Task 2)**

Run: `go test ./internal/apiserver/ -run 'TestUpload|TestResolveUploadDest' -v`
Expected: PASS for all. If `newSandboxSvc`/`keyMap`/`testSigner` signatures differ, adjust the call (they live in `sandboxservice_test.go` / `rolegate_test.go`).

- [ ] **Step 10: Build + vet, then commit**

Run: `go build ./... && go vet ./internal/apiserver/ ./internal/sandbox/`
Then:

```bash
gofmt -w internal/apiserver/files.go internal/apiserver/files_test.go internal/sandbox/fake.go internal/apiserver/sandboxservice.go internal/apiserver/server.go
git add internal/apiserver/files.go internal/apiserver/files_test.go internal/sandbox/fake.go internal/apiserver/sandboxservice.go internal/apiserver/server.go
git commit -m "feat(files): admin-gated file upload over REST (sbx cp via host temp)"
```

---

### Task 2: Download — stream a file out

**Files:**
- Modify: `internal/apiserver/files.go` (add the GET branch + `handleDownload`; delete the temporary `var _` stubs)
- Modify: `internal/apiserver/files_test.go` (download tests)

**Interfaces:**
- Consumes: `Backend().CopyFrom`, `mgr.Resolve`, `s.auditFile` (from Task 1).
- Produces: `handleDownload`; `GET /v1/sandboxes/{id}/files?path=<abs>` streams the file.

- [ ] **Step 1: Write the failing download tests**

Append to `internal/apiserver/files_test.go`:

```go
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
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/apiserver/ -run TestDownload -v`
Expected: FAIL — `405 method not allowed` (GET not handled yet) / missing `handleDownload`.

- [ ] **Step 3: Implement the GET branch in `files.go`**

In `FilesHandler`'s `switch`, add the `GET` case:

```go
		case http.MethodGet:
			s.handleDownload(w, r, id)
```

Add `handleDownload`:

```go
func (s *SandboxService) handleDownload(w http.ResponseWriter, r *http.Request, id string) {
	p := strings.TrimSpace(r.URL.Query().Get("path"))
	if !path.IsAbs(p) {
		http.Error(w, "path must be an absolute container path", http.StatusBadRequest)
		return
	}
	name, err := s.mgr.Resolve(r.Context(), id)
	if err != nil {
		http.Error(w, "sandbox not found", http.StatusNotFound)
		return
	}
	tmp, err := os.CreateTemp("", "sbxdl-*")
	if err != nil {
		http.Error(w, "stage temp file", http.StatusInternalServerError)
		return
	}
	tmpName := tmp.Name()
	_ = tmp.Close()
	defer os.Remove(tmpName)

	err = s.mgr.Backend().CopyFrom(r.Context(), name, p, tmpName)
	s.auditFile("file.download", p, r, err)
	if err != nil {
		http.Error(w, "copy failed: "+err.Error(), http.StatusNotFound)
		return
	}
	f, err := os.Open(tmpName)
	if err != nil {
		http.Error(w, "open staged file", http.StatusInternalServerError)
		return
	}
	defer f.Close()
	if fi, err := f.Stat(); err == nil && !fi.Mode().IsRegular() {
		http.Error(w, "not a regular file", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+path.Base(p)+`"`)
	_, _ = io.Copy(w, f)
}
```

- [ ] **Step 4: Run download + upload tests; verify all pass**

Run: `go test ./internal/apiserver/ -run 'TestUpload|TestDownload|TestResolveUploadDest' -v`
Expected: PASS.

- [ ] **Step 5: Build + commit**

Run: `go build ./... && go vet ./internal/apiserver/`
Then:

```bash
gofmt -w internal/apiserver/files.go internal/apiserver/files_test.go
git add internal/apiserver/files.go internal/apiserver/files_test.go
git commit -m "feat(files): admin-gated file download (stream via Content-Disposition)"
```

---

### Task 3: Config knob + close the terminal authZ gap

**Files:**
- Modify: `internal/config/config.go` (add `MaxUploadBytes`)
- Modify: `internal/node/node.go` (`SetMaxUploadBytes`)
- Modify: `internal/apiserver/server.go` (admin-gate the terminal)
- Modify: `internal/apiserver/files_test.go` (terminal-gate regression test)

**Interfaces:**
- Consumes: `cfg.MaxUploadBytes`, `mw.RequireRole`, `opts.Sandboxes.TerminalHandler()`.
- Produces: `Config.MaxUploadBytes int64`; terminal route returns 403 for non-admin.

- [ ] **Step 1: Write the failing terminal-gate test (through the real built server)**

Append to `internal/apiserver/files_test.go` (uses the `startRoleGateServer` harness +
`crypto/tls` already present in `rolegate_test.go`; add `crypto/tls` + `net/http` to
this file's imports via goimports):

```go
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
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/apiserver/ -run TestTerminal_ReadOnlyForbidden -v`
Expected: FAIL — today the terminal is authN-only, so a read-only key reaches the
WebSocket handler and gets a non-403 status (e.g. 426/400 from the failed upgrade),
not 403.

- [ ] **Step 3: Admin-gate the terminal in `server.go`**

Change the terminal wiring line:

```go
		v1 = terminalMux(mw.RequireRole("admin", opts.Sandboxes.TerminalHandler()), v1)
```

- [ ] **Step 4: Add the `max_upload_bytes` config field**

In `internal/config/config.go`, add next to `IdleTimeout` (~L43):

```go
	MaxUploadBytes          int64             `yaml:"max_upload_bytes"` // 0 → 100 MiB; per-request file-upload ceiling
```

- [ ] **Step 5: Wire the config into the service**

In `internal/node/node.go`, after `sandboxes.SetIdleTimeout(...)` (~L107):

```go
	sandboxes.SetMaxUploadBytes(cfg.MaxUploadBytes)
```

- [ ] **Step 6: Add a config-parse test**

In `internal/config/config_test.go`, add:

```go
func TestConfig_MaxUploadBytes(t *testing.T) {
	var c Config
	require.Equal(t, int64(0), c.MaxUploadBytes) // unset → 0 (handler defaults to 100 MiB)
	c.MaxUploadBytes = 5 << 20
	require.Equal(t, int64(5<<20), c.MaxUploadBytes)
}
```

(If `config_test.go` lacks the `require` import or `Config` is built differently, mirror an existing test in that file.)

- [ ] **Step 7: Run tests, build, commit**

Run: `go test ./internal/apiserver/ ./internal/config/ -run 'Terminal|MaxUpload' -v && go build ./...`
Expected: PASS.

```bash
gofmt -w internal/apiserver/server.go internal/config/config.go internal/node/node.go internal/apiserver/files_test.go internal/config/config_test.go
git add internal/apiserver/server.go internal/config/config.go internal/node/node.go internal/apiserver/files_test.go internal/config/config_test.go
git commit -m "feat(files): max_upload_bytes knob; admin-gate the terminal (close authN-only gap)"
```

---

### Task 4: Console Files tab

**Files:**
- Modify: `web/app/composables/useApi.ts` (add `upload` + `downloadUrl`)
- Modify: `web/app/components/drawer/FilesTab.vue` (replace the placeholder)
- Create: `web/tests/drawer-files.spec.ts`

**Interfaces:**
- Consumes: `useApi()`, `useSession().isAdmin`.
- Produces: `Api.upload(path, body: Blob): Promise<void>`, `Api.downloadUrl(path): string`.

- [ ] **Step 1: Add `upload` + `downloadUrl` to the API client**

In `web/app/composables/useApi.ts`, extend the `Api` type:

```ts
export type Api = {
  get: (path: string) => Promise<any>
  post: (path: string, body?: unknown, headers?: Record<string, string>) => Promise<any>
  put: (path: string, body?: unknown) => Promise<any>
  del: (path: string) => Promise<any>
  upload: (path: string, body: Blob) => Promise<void>
  downloadUrl: (path: string) => string
}
```

Inside `createApi`, after `req` is defined, add the two methods to the returned object:

```ts
  const root = base.replace(/\/$/, '')
  return {
    get: (p) => req('GET', p),
    post: (p, b, h) => req('POST', p, b, h),
    put: (p, b) => req('PUT', p, b),
    del: (p) => req('DELETE', p),
    downloadUrl: (p) => root + p,
    upload: async (p, body) => {
      const res = await fetchImpl(root + p, {
        method: 'PUT',
        headers: { 'X-CSRF-Token': readCookie('sbx_csrf') }, // raw body: no Content-Type
        credentials: 'include',
        body,
      })
      if (res.status === 401) { onAuthLost(); throw new Error('unauthorized') }
      if (!res.ok) {
        let msg = `PUT ${p} -> ${res.status}`
        try { const m = (await res.json())?.message; if (m) msg = String(m) } catch { /* keep generic */ }
        throw new Error(msg)
      }
    },
  }
```

- [ ] **Step 2: Write the failing FilesTab test**

Create `web/tests/drawer-files.spec.ts`:

```ts
// @vitest-environment nuxt
import { ref } from 'vue'
import { describe, it, expect, vi } from 'vitest'
import { mountSuspended } from '@nuxt/test-utils/runtime'
import FilesTab from '../app/components/drawer/FilesTab.vue'

const upload = vi.fn(async () => {})
const downloadUrl = vi.fn((p: string) => 'https://node' + p)
vi.mock('../app/composables/useApi', () => ({
  useApi: () => ({ upload, downloadUrl, get: vi.fn(), post: vi.fn() }),
}))
vi.mock('../app/composables/useSession', () => ({ useSession: () => ({ isAdmin: ref(true) }) }))

describe('FilesTab', () => {
  it('upload PUTs the chosen file to the default /home/agent path', async () => {
    const w = await mountSuspended(FilesTab, { props: { sandbox: { id: 'n1.s1' } } })
    const file = new File(['hi'], 'report.txt', { type: 'text/plain' })
    const input = w.find('input[type="file"]').element as HTMLInputElement
    Object.defineProperty(input, 'files', { value: [file] })
    await w.find('input[type="file"]').trigger('change')
    await w.find('[data-test="upload"]').trigger('click')
    expect(upload).toHaveBeenCalledWith('/v1/sandboxes/n1.s1/files?path=%2Fhome%2Fagent%2Freport.txt', file)
  })

  it('download builds the file URL from the path field', async () => {
    const w = await mountSuspended(FilesTab, { props: { sandbox: { id: 'n1.s1' } } })
    await w.find('[data-test="dl-path"]').setValue('/home/agent/out.txt')
    await w.find('[data-test="download"]').trigger('click')
    expect(downloadUrl).toHaveBeenCalledWith('/v1/sandboxes/n1.s1/files?path=%2Fhome%2Fagent%2Fout.txt')
  })
})
```

- [ ] **Step 3: Run to verify failure**

Run: `cd web && npm test -- drawer-files`
Expected: FAIL — FilesTab still renders the "coming soon" placeholder (no inputs/buttons).

- [ ] **Step 4: Implement `FilesTab.vue`**

Replace `web/app/components/drawer/FilesTab.vue`:

```vue
<script setup lang="ts">
const props = defineProps<{ sandbox: { id: string } }>()

const api = useApi()
const session = useSession()
const toast = useToast()

const file = ref<File | null>(null)
const dest = ref('')
const uploading = ref(false)
const dlPath = ref('/home/agent/')

// Default to /home/agent/<filename> unless the operator typed an absolute path.
function resolvedDest(): string {
  const d = dest.value.trim()
  const name = file.value?.name ?? ''
  if (d) return d.startsWith('/') ? d : `/home/agent/${d}`
  return `/home/agent/${name}`
}
function filesUrl(p: string): string {
  return `/v1/sandboxes/${props.sandbox.id}/files?path=${encodeURIComponent(p)}`
}
function onPick(e: Event) {
  file.value = (e.target as HTMLInputElement).files?.[0] ?? null
}
async function doUpload() {
  if (!file.value) return
  uploading.value = true
  try {
    await api.upload(filesUrl(resolvedDest()), file.value)
    toast.add({ title: `Uploaded to ${resolvedDest()}`, color: 'success', icon: 'i-lucide-check-circle' })
  } catch (e: any) {
    toast.add({ title: 'Upload failed', description: e?.message, color: 'error', icon: 'i-lucide-alert-circle' })
  } finally {
    uploading.value = false
  }
}
function doDownload() {
  const p = dlPath.value.trim()
  if (!p.startsWith('/')) {
    toast.add({ title: 'Path must be absolute', color: 'error' })
    return
  }
  const a = document.createElement('a')
  a.href = api.downloadUrl(filesUrl(p))
  a.download = p.split('/').pop() || 'download'
  document.body.appendChild(a)
  a.click()
  a.remove()
}
</script>

<template>
  <div class="flex flex-col gap-6 pt-4">
    <template v-if="session.isAdmin.value">
      <!-- Upload -->
      <div class="flex flex-col gap-2">
        <p class="text-xs font-semibold text-muted uppercase tracking-wide">Upload</p>
        <input type="file" aria-label="File to upload" @change="onPick">
        <UInput v-model="dest" size="sm" placeholder="destination (default: /home/agent/<filename>)" aria-label="Destination path" />
        <UButton
          label="Upload"
          icon="i-lucide-upload"
          size="sm"
          color="primary"
          :loading="uploading"
          :disabled="!file"
          data-test="upload"
          @click="doUpload"
        />
      </div>

      <USeparator />

      <!-- Download -->
      <div class="flex flex-col gap-2">
        <p class="text-xs font-semibold text-muted uppercase tracking-wide">Download</p>
        <UInput v-model="dlPath" size="sm" placeholder="/absolute/container/path" aria-label="Download path" data-test="dl-path" />
        <UButton
          label="Download"
          icon="i-lucide-download"
          size="sm"
          color="primary"
          :disabled="!dlPath.startsWith('/') || dlPath.endsWith('/')"
          data-test="download"
          @click="doDownload"
        />
      </div>
    </template>

    <UAlert
      v-else
      color="neutral"
      variant="subtle"
      icon="i-lucide-lock"
      title="Admin only"
      description="File transfer requires admin access."
    />
  </div>
</template>
```

- [ ] **Step 5: Run the FilesTab test to verify it passes**

Run: `cd web && npm test -- drawer-files`
Expected: PASS (both cases). `useToast` is a Nuxt UI auto-import provided by the nuxt
test env. If mounting errors on toast, mirror the toast handling in an existing drawer
spec that mounts a toasting component (e.g. `tests/drawer-secrets.spec.ts`).

- [ ] **Step 6: Run the whole web suite + build the SPA**

Run: `cd web && npm test && bash scripts/build.sh`
Expected: all specs pass; build succeeds (the Go binary embeds `web/dist`).

- [ ] **Step 7: Commit**

```bash
git add web/app/composables/useApi.ts web/app/components/drawer/FilesTab.vue web/tests/drawer-files.spec.ts web/dist
git commit -m "feat(web): Files tab — admin upload/download (raw PUT + browser GET)"
```

---

## Final verification

- [ ] `go test ./... 2>&1 | grep -E 'FAIL|panic'` is empty (run with the bare-repo override only if git suites are touched; they are not here).
- [ ] `go build ./... && go vet ./internal/...` clean.
- [ ] `cd web && npm test` green; `bash scripts/build.sh` succeeds.
- [ ] Manual smoke (optional, live daemon): start the node `backend:sdk`; from the console Files tab upload a small file to the default path, then download it back; confirm a read-only key gets 403 on both `/files` and `/terminal`.
