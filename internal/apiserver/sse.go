package apiserver

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/squall-chua/sbx-swarm-node/internal/events"
)

// SSEHandler streams the local event firehose as text/event-stream.
// Query params: types (csv), sandbox. Last-Event-ID header drives best-effort
// replay from the bus buffer (ADR-0008).
func SSEHandler(bus *events.Bus) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}

		filter := events.Filter{SandboxID: r.URL.Query().Get("sandbox")}
		if t := r.URL.Query().Get("types"); t != "" {
			filter.Types = strings.Split(t, ",")
		}
		since := parseSinceSeq(r.Header.Get("Last-Event-ID"))

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)

		// Backfill from the buffer first (best-effort).
		for _, e := range bus.Replay(filter, since) {
			writeSSE(w, e)
		}
		flusher.Flush()

		ch, cancel := bus.Subscribe(filter, since)
		defer cancel()

		ctx := r.Context()
		for {
			select {
			case <-ctx.Done():
				return
			case e, ok := <-ch:
				if !ok {
					return
				}
				writeSSE(w, e)
				flusher.Flush()
			}
		}
	})
}

func writeSSE(w http.ResponseWriter, e events.Event) {
	fmt.Fprintf(w, "id: %s\n", e.ID)
	fmt.Fprintf(w, "event: %s\n", e.Type)
	fmt.Fprintf(w, "data: %s\n\n", e.Payload) // payload is JSON (may be empty)
}

func parseSinceSeq(lastID string) uint64 {
	if lastID == "" {
		return 0
	}
	i := strings.LastIndexByte(lastID, '-')
	if i < 0 || i == len(lastID)-1 {
		return 0
	}
	n, err := strconv.ParseUint(lastID[i+1:], 10, 64)
	if err != nil {
		return 0
	}
	return n
}
