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
		case http.MethodGet:
			s.handleDownload(w, r, id)
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
	err = copyFileToSandbox(r.Context(), s.mgr.Backend(), name, tmp.Name(), dest)
	s.auditFile("file.upload", dest, r, err)
	if err != nil {
		http.Error(w, "copy failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	_ = s.mgr.BumpActivity(r.Context(), id) // upload is Activity
	w.WriteHeader(http.StatusNoContent)
}

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
	fi, err := f.Stat()
	if err != nil {
		http.Error(w, "stat staged file", http.StatusInternalServerError)
		return
	}
	if !fi.Mode().IsRegular() {
		http.Error(w, "not a regular file", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+path.Base(p)+`"`)
	_, _ = io.Copy(w, f)
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
