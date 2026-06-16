package apiserver

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/squall-chua/sbx-swarm-node/internal/ids"
	"github.com/squall-chua/sbx-swarm-node/internal/obsd"
	"github.com/squall-chua/sbx-swarm-node/internal/sandbox"
	"github.com/squall-chua/sbx-swarm-node/internal/store"
	"github.com/stretchr/testify/require"
)

// syncRecorder is a minimal thread-safe http.ResponseWriter+Flusher so the
// streaming handler goroutine and the test assertion goroutine don't race on
// the body buffer (httptest.ResponseRecorder is not concurrency-safe).
type syncRecorder struct {
	mu     sync.Mutex
	body   bytes.Buffer
	header http.Header
	code   int
}

func newSyncRecorder() *syncRecorder {
	return &syncRecorder{header: http.Header{}, code: http.StatusOK}
}

func (s *syncRecorder) Header() http.Header { return s.header }
func (s *syncRecorder) WriteHeader(c int)   { s.mu.Lock(); s.code = c; s.mu.Unlock() }
func (s *syncRecorder) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.body.Write(p)
}
func (s *syncRecorder) Flush() {}
func (s *syncRecorder) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.body.String()
}
func (s *syncRecorder) statusCode() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.code
}

func newObserveDeps(t *testing.T) (ObserveDeps, *sandbox.Fake, *sandbox.Manager) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "n.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	f := sandbox.NewFake()
	mgr := sandbox.NewManager("n1", f, st, ids.NewGen("n1"))
	statsC := obsd.NewStatsCollector(f, func(ctx context.Context) ([]string, error) { return nil, nil }, obsd.DefaultProvisionLimit(), 4)
	netC := obsd.NewNetLogCollector(f, mgr.ResolveVMToID)
	return ObserveDeps{Stats: statsC, NetLog: netC, Backend: f, Mgr: mgr}, f, mgr
}

func TestLogsSSEHandler_WritesDataFrame(t *testing.T) {
	deps, _, mgr := newObserveDeps(t)
	ctx := context.Background()
	rec, err := mgr.Create(ctx, sandbox.CreateSpec{Name: "s1"})
	require.NoError(t, err)

	h := LogsSSEHandler(deps)

	// Cancellable request context so the streaming handler returns.
	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/v1/sandboxes/"+rec.ID+"/logs", nil).WithContext(reqCtx)
	rw := newSyncRecorder()

	done := make(chan struct{})
	go func() {
		h.ServeHTTP(rw, req)
		close(done)
	}()

	// The fake emits one line ("log from <backendName>"); wait for the SSE frame.
	require.Eventually(t, func() bool {
		return strings.Contains(rw.String(), "data: log from "+rec.BackendName)
	}, time.Second, 10*time.Millisecond)

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handler did not return after cancel")
	}

	require.Contains(t, rw.Header().Get("Content-Type"), "text/event-stream")
}

func TestLogsSSEHandler_NotFoundForUnknownSandbox(t *testing.T) {
	deps, _, _ := newObserveDeps(t)
	h := LogsSSEHandler(deps)
	req := httptest.NewRequest(http.MethodGet, "/v1/sandboxes/nope/logs", nil)
	rw := newSyncRecorder()
	h.ServeHTTP(rw, req)
	require.Equal(t, http.StatusNotFound, rw.statusCode())
}
