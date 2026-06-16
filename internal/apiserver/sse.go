package apiserver

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/squall-chua/sbx-swarm-node/internal/events"
	sbxv1 "github.com/squall-chua/sbx-swarm-node/internal/gen/sbxswarm/v1"
	"github.com/squall-chua/sbx-swarm-node/internal/peer"
	"github.com/squall-chua/sbx-swarm-node/internal/routing"
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

// SSEHandlerWithPeers is like SSEHandler but also merges events from peer nodes
// via WatchEvents gRPC streams. When routing or pool are nil it falls back to
// local-only behaviour. The actual node wiring happens in Task 7; this function
// provides the capability.
func SSEHandlerWithPeers(bus *events.Bus, tbl *routing.Table, pool *peer.Pool) http.Handler {
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

		// Backfill local buffer.
		for _, e := range bus.Replay(filter, since) {
			writeSSE(w, e)
		}
		flusher.Flush()

		// Merged output channel: local bus + peer WatchEvents streams.
		ctx := r.Context()
		merged := make(chan events.Event, 128)
		merger := NewMerger(merged)

		localCh, cancel := bus.Subscribe(filter, since)
		defer cancel()
		go merger.Consume(ctx, localCh)

		// Open WatchEvents to each peer (best-effort; ignore dial errors).
		peers := tbl.Peers()
		for _, nodeID := range peers {
			addr, ok := tbl.Addr(nodeID)
			if !ok {
				continue
			}
			conn, err := pool.Conn(addr)
			if err != nil {
				continue
			}
			peerCtx, peerCancel := context.WithCancel(r.Context())
			defer peerCancel()
			stream, err := sbxv1.NewEventServiceClient(conn).WatchEvents(peerCtx, &sbxv1.WatchRequest{
				Types:    filter.Types,
				Sandbox:  filter.SandboxID,
				SinceSeq: since,
			})
			if err != nil {
				continue
			}
			peerCh := make(chan events.Event, 64)
			go merger.Consume(ctx, peerCh)
			go func() {
				defer close(peerCh)
				for {
					msg, err := stream.Recv()
					if err != nil {
						return
					}
					select {
					case peerCh <- events.Event{
						ID:        msg.Id,
						Seq:       msg.Seq,
						Type:      msg.Type,
						NodeID:    msg.NodeId,
						SandboxID: msg.SandboxId,
						Payload:   msg.Payload,
					}:
					case <-peerCtx.Done():
						return
					}
				}
			}()
		}

		for {
			select {
			case <-ctx.Done():
				return
			case e, ok := <-merged:
				if !ok {
					return
				}
				writeSSE(w, e)
				flusher.Flush()
			}
		}
	})
}
