package apiserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/squall-chua/sbx-swarm-node/internal/sandbox"
)

// pathID extracts the {id} segment from /v1/sandboxes/{id}/<suffix>, returning
// false if the path doesn't match that shape.
func pathID(urlPath, suffix string) (string, bool) {
	const prefix = "/v1/sandboxes/"
	if !strings.HasPrefix(urlPath, prefix) || !strings.HasSuffix(urlPath, "/"+suffix) {
		return "", false
	}
	id := strings.TrimSuffix(strings.TrimPrefix(urlPath, prefix), "/"+suffix)
	if id == "" || strings.Contains(id, "/") {
		return "", false
	}
	return id, true
}

// LogsSSEHandler streams a sandbox's logs as text/event-stream. It resolves the
// sandbox id to a backend name and drains Backend.Logs lines into SSE frames.
func LogsSSEHandler(o ObserveDeps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(r.URL.Path, "logs")
		if !ok {
			http.NotFound(w, r)
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		name, err := o.Mgr.Resolve(r.Context(), id)
		if err != nil {
			http.Error(w, "sandbox not found", http.StatusNotFound)
			return
		}

		ctx := r.Context()
		lines := make(chan sandbox.LogLine, 16)
		if err := o.Backend.Logs(ctx, name, r.URL.Query().Get("path"), lines); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		for {
			select {
			case <-ctx.Done():
				return
			case ll, ok := <-lines:
				if !ok {
					return
				}
				if ll.Err != nil {
					return // stream end/error
				}
				fmt.Fprintf(w, "event: log\ndata: %s\n\n", ll.Line)
				flusher.Flush()
			}
		}
	})
}

// StatsSSEHandler streams a sandbox's cached usage as text/event-stream on a
// ticker. It resolves the sandbox id to a backend name and emits Stats.Latest.
func StatsSSEHandler(o ObserveDeps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := pathID(r.URL.Path, "stats")
		if !ok {
			http.NotFound(w, r)
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		name, err := o.Mgr.Resolve(r.Context(), id)
		if err != nil {
			http.Error(w, "sandbox not found", http.StatusNotFound)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)

		ctx := r.Context()
		emit := func() {
			u, ok := o.Stats.Latest(name)
			if !ok {
				return
			}
			b, _ := json.Marshal(map[string]any{
				"cores": u.Cores, "cpu_percent": u.CPUPercent,
				"mem_total_kb": u.MemTotalKB, "mem_used_kb": u.MemUsedKB,
			})
			fmt.Fprintf(w, "event: stats\ndata: %s\n\n", b)
			flusher.Flush()
		}
		emit() // immediate first frame from cache
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				emit()
			}
		}
	})
}

// observeStreamMux routes the SSE log/stats endpoints. text/event-stream
// requests for .../logs and .../stats are served here; everything else falls
// through to the gateway (so the unary JSON GetStats still works).
func observeStreamMux(o ObserveDeps, fallback http.Handler) http.Handler {
	logs := LogsSSEHandler(o)
	stats := StatsSSEHandler(o)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := pathID(r.URL.Path, "logs"); ok {
			logs.ServeHTTP(w, r)
			return
		}
		if _, ok := pathID(r.URL.Path, "stats"); ok && wantsEventStream(r) {
			stats.ServeHTTP(w, r)
			return
		}
		fallback.ServeHTTP(w, r)
	})
}

func wantsEventStream(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "text/event-stream")
}