package apiserver

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/squall-chua/sbx-swarm-node/internal/events"
	"github.com/stretchr/testify/require"
)

func TestSSE_StreamsLiveEvents(t *testing.T) {
	bus := events.NewBus("node1", 16)
	srv := httptest.NewServer(SSEHandler(bus))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/events", nil)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))

	// publish after the client is connected
	go func() {
		time.Sleep(50 * time.Millisecond)
		bus.Publish("sandbox.created", "sb9", nil)
	}()

	r := bufio.NewReader(resp.Body)
	var sawID, sawEvent bool
	deadline := time.After(2 * time.Second)
	for !(sawID && sawEvent) {
		select {
		case <-deadline:
			t.Fatal("did not receive SSE event in time")
		default:
		}
		line, err := r.ReadString('\n')
		require.NoError(t, err)
		if strings.HasPrefix(line, "id: node1-") {
			sawID = true
		}
		if strings.HasPrefix(line, "event: sandbox.created") {
			sawEvent = true
		}
	}
}
