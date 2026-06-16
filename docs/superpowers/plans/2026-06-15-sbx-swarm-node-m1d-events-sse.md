# sbx-swarm-node M1d — Event Bus + SSE Firehose Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** An in-process event bus that sandbox/operation transitions publish to, exposed as a **local SSE firehose** at `GET /v1/events` with type/sandbox filters and best-effort `Last-Event-ID` resume — a best-effort notification bus, not a durable log (ADR-0008).

**Architecture:** `events.Bus` assigns each event a per-node monotonic `seq` + id (ADR-0008), keeps a bounded ring buffer for replay, and fans out to filtered subscribers over buffered channels (slow subscribers drop, never block). The `sandbox.Manager` and `ops.Manager` publish through a `Publisher` interface. An SSE handler under the authenticated `/v1` mux streams to browsers (`EventSource` sends the session cookie per ADR-0006). Peer fan-out for swarm-wide events is **out of scope** (Milestone 4).

**Tech Stack:** Go 1.23, `net/http` SSE, `encoding/json`, `github.com/stretchr/testify`. Builds on M1a–M1c.

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/events/event.go` | `Event` value + `Filter` + `Publisher` interface |
| `internal/events/bus.go` | `Bus`: publish, bounded ring buffer, filtered subscribe, replay |
| `internal/apiserver/sse.go` | SSE handler for `GET /v1/events` |
| `internal/sandbox/manager.go` | emit lifecycle events via injected `Publisher` |
| `internal/ops/ops.go` | emit operation events via injected `Publisher` |
| `internal/apiserver/server.go` | mount `/v1/events` under auth when a bus is provided |
| `internal/node/node.go` | construct the bus, inject it into manager/ops/server |

---

## Task 1: Event type + Bus

**Files:**
- Create: `internal/events/event.go`, `internal/events/bus.go`
- Test: `internal/events/bus_test.go`

- [x] **Step 1: Write the failing test**

```go
package events

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBus_PublishAssignsMonotonicIDsAndBuffers(t *testing.T) {
	b := NewBus("node1", 8)

	e1 := b.Publish("sandbox.created", "sb1", map[string]string{"k": "v"})
	e2 := b.Publish("sandbox.stopped", "sb1", nil)
	require.Equal(t, uint64(1), e1.Seq)
	require.Equal(t, uint64(2), e2.Seq)
	require.Equal(t, "node1-1", e1.ID)

	// replay everything after seq 0
	all := b.Replay(Filter{}, 0)
	require.Len(t, all, 2)

	// filter by type
	created := b.Replay(Filter{Types: []string{"sandbox.created"}}, 0)
	require.Len(t, created, 1)
	require.Equal(t, "sandbox.created", created[0].Type)

	// filter by sandbox + since seq
	since := b.Replay(Filter{SandboxID: "sb1"}, 1)
	require.Len(t, since, 1)
	require.Equal(t, uint64(2), since[0].Seq)
}

func TestBus_SubscribeReceivesLiveEvents(t *testing.T) {
	b := NewBus("node1", 8)
	ch, cancel := b.Subscribe(Filter{Types: []string{"sandbox.created"}}, 0)
	defer cancel()

	b.Publish("sandbox.stopped", "sb1", nil) // filtered out
	b.Publish("sandbox.created", "sb2", nil)  // delivered

	got := <-ch
	require.Equal(t, "sandbox.created", got.Type)
	require.Equal(t, "sb2", got.SandboxID)
}
```

- [x] **Step 2: Run test to verify it fails**

Run: `go test ./internal/events/ -v`
Expected: FAIL — `undefined: NewBus`

- [x] **Step 3: Write `event.go`**

```go
// Package events is a best-effort, in-process notification bus (ADR-0008):
// not durable, not a source of truth. Subscribers reconcile against state if
// they need certainty.
package events

import (
	"encoding/json"
	"time"
)

// Event is a single domain notification.
type Event struct {
	ID        string          `json:"id"`   // "<node_id>-<seq>"
	Seq       uint64          `json:"seq"`
	TS        time.Time       `json:"ts"`
	Type      string          `json:"type"`
	NodeID    string          `json:"node_id"`
	SandboxID string          `json:"sandbox_id,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

// Filter selects events by type set (empty = all) and/or sandbox id.
type Filter struct {
	Types     []string
	SandboxID string
}

func (f Filter) matches(e Event) bool {
	if f.SandboxID != "" && e.SandboxID != f.SandboxID {
		return false
	}
	if len(f.Types) == 0 {
		return true
	}
	for _, t := range f.Types {
		if t == e.Type {
			return true
		}
	}
	return false
}

// Publisher publishes domain events. The Bus implements it; the sandbox and ops
// managers depend on it (nil-safe via a wrapper).
type Publisher interface {
	Publish(eventType, sandboxID string, payload any) Event
}
```

- [x] **Step 4: Write `bus.go`**

```go
package events

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

type subscription struct {
	filter Filter
	ch     chan Event
}

// Bus is a bounded, best-effort in-process event bus.
type Bus struct {
	nodeID string
	mu     sync.Mutex
	seq    uint64
	ring   []Event
	size   int
	subs   map[int]*subscription
	nextID int
	now    func() time.Time
}

// NewBus returns a bus retaining the last bufSize events for replay.
func NewBus(nodeID string, bufSize int) *Bus {
	if bufSize <= 0 {
		bufSize = 256
	}
	return &Bus{nodeID: nodeID, size: bufSize, subs: map[int]*subscription{}, now: time.Now}
}

// Publish assigns a seq/id/timestamp, buffers the event, and fans it out to
// matching subscribers without blocking (a full subscriber drops the event).
func (b *Bus) Publish(eventType, sandboxID string, payload any) Event {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.seq++
	var raw json.RawMessage
	if payload != nil {
		if enc, err := json.Marshal(payload); err == nil {
			raw = enc
		}
	}
	e := Event{
		ID: fmt.Sprintf("%s-%d", b.nodeID, b.seq), Seq: b.seq, TS: b.now(),
		Type: eventType, NodeID: b.nodeID, SandboxID: sandboxID, Payload: raw,
	}

	b.ring = append(b.ring, e)
	if len(b.ring) > b.size {
		b.ring = b.ring[len(b.ring)-b.size:]
	}

	for _, s := range b.subs {
		if !s.filter.matches(e) {
			continue
		}
		select {
		case s.ch <- e:
		default: // slow subscriber: drop (best-effort, ADR-0008)
		}
	}
	return e
}

// Replay returns buffered events with Seq > sinceSeq matching the filter.
func (b *Bus) Replay(f Filter, sinceSeq uint64) []Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []Event
	for _, e := range b.ring {
		if e.Seq > sinceSeq && f.matches(e) {
			out = append(out, e)
		}
	}
	return out
}

// Subscribe returns a channel of future matching events plus a cancel func.
// Buffered events after sinceSeq are NOT pushed to the channel here; callers
// that want backfill call Replay first (the SSE handler does).
func (b *Bus) Subscribe(f Filter, _ uint64) (<-chan Event, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id := b.nextID
	b.nextID++
	s := &subscription{filter: f, ch: make(chan Event, 64)}
	b.subs[id] = s
	return s.ch, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if _, ok := b.subs[id]; ok {
			delete(b.subs, id)
			close(s.ch)
		}
	}
}
```

- [x] **Step 5: Run test, then commit**

Run: `go test ./internal/events/ -v`
Expected: PASS

```bash
git add internal/events/
git commit -m "feat(events): best-effort in-process event bus (ADR-0008)"
```

---

## Task 2: SSE handler

**Files:**
- Create: `internal/apiserver/sse.go`
- Test: `internal/apiserver/sse_test.go`

- [x] **Step 1: Write the failing test**

```go
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
```

- [x] **Step 2: Run test to verify it fails**

Run: `go test ./internal/apiserver/ -run TestSSE -v`
Expected: FAIL — `undefined: SSEHandler`

- [x] **Step 3: Write the implementation**

```go
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
```

- [x] **Step 4: Run test, then commit**

Run: `go test ./internal/apiserver/ -run TestSSE -v`
Expected: PASS

```bash
git add internal/apiserver/sse.go internal/apiserver/sse_test.go
git commit -m "feat(apiserver): SSE firehose handler with filters + Last-Event-ID"
```

---

## Task 3: Emit lifecycle events from the sandbox Manager

**Files:**
- Modify: `internal/sandbox/manager.go`
- Test: `internal/sandbox/manager_events_test.go`

- [x] **Step 1: Write the failing test**

```go
package sandbox

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/squall-chua/sbx-swarm-node/internal/events"
	"github.com/squall-chua/sbx-swarm-node/internal/ids"
	"github.com/squall-chua/sbx-swarm-node/internal/store"
	"github.com/stretchr/testify/require"
)

func TestManager_EmitsLifecycleEvents(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "n.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	bus := events.NewBus("node1", 32)
	m := NewManager("node1", NewFake(), st, ids.NewGen("node1"))
	m.SetPublisher(bus)

	ctx := context.Background()
	rec, err := m.Create(ctx, CreateSpec{})
	require.NoError(t, err)
	require.NoError(t, m.Stop(ctx, rec.ID))

	got := bus.Replay(events.Filter{}, 0)
	types := []string{}
	for _, e := range got {
		types = append(types, e.Type)
	}
	require.Contains(t, types, "sandbox.created")
	require.Contains(t, types, "sandbox.stopped")
}
```

- [x] **Step 2: Run test to verify it fails**

Run: `go test ./internal/sandbox/ -run TestManager_EmitsLifecycle -v`
Expected: FAIL — `m.SetPublisher undefined`

- [x] **Step 3: Modify `manager.go`**

Add a publisher field + setter + nil-safe emit, and call it on transitions.

Add the import `"github.com/squall-chua/sbx-swarm-node/internal/events"` and to the `Manager` struct:

```go
	pub events.Publisher
```

Add:

```go
// SetPublisher wires an event publisher (optional; nil disables events).
func (m *Manager) SetPublisher(p events.Publisher) { m.pub = p }

func (m *Manager) emit(eventType, sandboxID string, payload any) {
	if m.pub != nil {
		m.pub.Publish(eventType, sandboxID, payload)
	}
}
```

In `Create`, after `m.save(rec)` succeeds and before `return rec, nil`:

```go
	m.emit("sandbox.created", rec.ID, map[string]string{"status": rec.Status})
```

In `lifecycle`, after `m.save(rec)`:

```go
	m.emit("sandbox."+status, rec.ID, nil)
```

Wait — `lifecycle` sets status to "running"/"stopped"; emit `sandbox.running`/`sandbox.stopped`. Adjust so the event name matches the test (`sandbox.stopped`). Since `lifecycle` is called with status `"running"`/`"stopped"`, the emit produces `sandbox.running`/`sandbox.stopped`. Good.

In `Delete`, after `m.store.Delete(...)` succeeds:

```go
	m.emit("sandbox.deleted", id, nil)
```

In `Reconcile`, when marking a record `lost` (after `m.save(rec)`):

```go
		m.emit("sandbox.lost", rec.ID, nil)
```

- [x] **Step 4: Run test, then commit**

Run: `go test ./internal/sandbox/ -v`
Expected: PASS (existing + new)

```bash
git add internal/sandbox/manager.go internal/sandbox/manager_events_test.go
git commit -m "feat(sandbox): emit lifecycle events via Publisher"
```

---

## Task 4: Emit operation events + wire bus into server & node

**Files:**
- Modify: `internal/ops/ops.go`
- Modify: `internal/apiserver/server.go`
- Modify: `internal/node/node.go`
- Test: `internal/node/node_test.go` (extend)

- [x] **Step 1: Emit events from ops (failing test first)**

Append to `internal/ops/ops_test.go`:

```go
import "github.com/squall-chua/sbx-swarm-node/internal/events" // add to existing imports

func TestOps_EmitsStateEvents(t *testing.T) {
	m := newMgr(t)
	bus := events.NewBus("n1", 16)
	m.SetPublisher(bus)

	op, _, err := m.Start(context.Background(), "provision", "")
	require.NoError(t, err)
	m.Run(op.ID, func() (string, error) { return "sb1", nil })

	require.Eventually(t, func() bool {
		for _, e := range bus.Replay(events.Filter{Types: []string{"operation.done"}}, 0) {
			if e.Type == "operation.done" {
				return true
			}
		}
		return false
	}, time.Second, 10*time.Millisecond)
}
```

- [x] **Step 2: Run to verify it fails**

Run: `go test ./internal/ops/ -run TestOps_EmitsStateEvents -v`
Expected: FAIL — `m.SetPublisher undefined`

- [x] **Step 3: Modify `ops.go`**

Add import `"github.com/squall-chua/sbx-swarm-node/internal/events"`, a `pub events.Publisher` field, a `SetPublisher`, and emit in `Run` after each state change:

```go
// SetPublisher wires an event publisher (optional).
func (m *Manager) SetPublisher(p events.Publisher) { m.pub = p }

func (m *Manager) emit(op *Operation) {
	if m.pub != nil {
		m.pub.Publish("operation."+op.State, op.SandboxID, map[string]string{"op_id": op.ID, "type": op.Type})
	}
}
```

In `Run`, after `op.State = "running"; _ = m.put(op)` add `m.emit(op)`, and after the final `_ = m.put(op)` add `m.emit(op)`.

- [x] **Step 4: Mount `/v1/events` in `apiserver.Build`**

In `server.go`, add to `Options`:

```go
	Events *events.Bus // optional; mounts /v1/events (SSE) under auth if set
```

In `Build`, where the REST mux is assembled, before the `/v1/` catch-all add:

```go
	if opts.Events != nil {
		rest.Handle("/v1/events", mw.Authenticate(SSEHandler(opts.Events)))
	}
```

(`/v1/events` must be registered before the broader `/v1/` pattern so Go's mux prefers the exact path; `http.ServeMux` longest-pattern matching handles this, but registering the specific route is clearest.) Add the `events` import.

- [x] **Step 5: Wire the bus in `node.New`**

In `node.go` `New`, after creating `gen`:

```go
	bus := events.NewBus(id.NodeID, 1024)
```

After building `mgr` and `opsM`:

```go
	mgr.SetPublisher(bus)
	opsM.SetPublisher(bus)
```

Add `Events: bus` to `apiserver.Options{...}`. Add the `events` import.

- [x] **Step 6: Extend the node integration test**

Append to `node_test.go`:

```go
func TestNode_SSEEndpointAuthed(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.ListenAddr = "127.0.0.1:0"
	cfg.APIKeys = []config.APIKey{{Key: "adm", Role: "admin"}}

	n, err := New(cfg, obs.NewLogger("error", io.Discard), "test")
	require.NoError(t, err)
	require.NoError(t, n.Start())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = n.Stop(ctx)
	})

	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}

	// unauthenticated SSE -> 401
	resp, err := client.Get("https://" + n.Addr() + "/v1/events")
	require.NoError(t, err)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	resp.Body.Close()
}
```

- [x] **Step 7: Run all tests + manual smoke**

Run: `go test ./...`
Expected: PASS across all packages.

Manual:
```bash
go run ./cmd/sbx-swarm-node --data-dir ./tmp-data --listen-addr 127.0.0.1:8443 &
# with an admin key configured, in one shell:
curl -sk -N -H "Authorization: Bearer adm" https://localhost:8443/v1/events &
# in another, create a sandbox and watch sandbox.created / operation.* events stream
kill %1 %2 2>/dev/null; rm -rf ./tmp-data
```

- [x] **Step 8: Commit**

```bash
git add internal/ops/ops.go internal/ops/ops_test.go internal/apiserver/server.go internal/node/node.go internal/node/node_test.go
git commit -m "feat(events): emit op events; mount authenticated /v1/events SSE"
```

---

## Self-Review

**Spec coverage (M1d slice):**
- In-process event bus, bounded buffer, best-effort, per-node monotonic id (ADR-0008) → Task 1 ✓
- SSE firehose `GET /v1/events` with `types`/`sandbox` filters + `Last-Event-ID` best-effort replay → Tasks 2, 4 ✓
- Events emitted from sandbox lifecycle + operations → Tasks 3, 4 ✓
- Served under auth (cookie via `EventSource`, ADR-0006) → Task 4 ✓
- **Deferred:** per-sandbox stats/logs/network SSE streams (Milestone 2); swarm-wide peer fan-out / `WatchEvents` merge (Milestone 4). This milestone is local-only by design.

**Placeholder scan:** No TBD/TODO; every step has complete code + exact commands. The slow-subscriber drop is an explicit, documented best-effort behavior (ADR-0008), not an unfinished edge case.

**Type consistency:** `events.NewBus(nodeID, size)`→`*Bus` implementing `events.Publisher` (`Publish(type, sandboxID, payload) Event`) plus `Subscribe`/`Replay`; `apiserver.SSEHandler(*events.Bus)`→`http.Handler`; `sandbox.Manager.SetPublisher`/`ops.Manager.SetPublisher` take `events.Publisher`; `apiserver.Options.Events` is mounted in `Build`. Event names (`sandbox.created`/`sandbox.running`/`sandbox.stopped`/`sandbox.deleted`/`sandbox.lost`, `operation.<state>`) are consistent across emitters and tests.

---

## Milestone 1 complete

With M1a–M1d done, the standalone node: boots with a persistent identity + versioned store, serves gRPC + REST + static + SSE on one TLS port behind role-based auth, manages sandboxes (CRUD/exec/agent-run/ports/files) through a swappable backend with idempotent async operations, and streams a local event firehose. Next milestones build on this: **M2** observability (stats/logs/network), **M3** policy/secrets, **M4** swarm (gossip, routing, peer event fan-out), **M5** scheduling, **M6** git workspaces, **M7** TTL/idle reaper, **M8** Nuxt console.
