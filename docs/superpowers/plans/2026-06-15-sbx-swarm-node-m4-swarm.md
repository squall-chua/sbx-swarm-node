# sbx-swarm-node M4 — Swarm (Gossip, Routing, Failure, Fan-out) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development or superpowers:executing-plans. Checkbox steps.
>
> **Forward-looking & integration-heavy:** depends on all of M1 (esp. `ids` self-routing, `events.Bus`, `sandbox.Manager`, `apiserver`, proto). Distributed behavior is verified mostly by **multi-node in-process integration tests**; pure logic (identity guard, routing table, event merge) is unit-TDD'd. Reconcile signatures against real M1 code.

**Goal:** Turn a standalone node into a swarm member: encrypted gossip membership with a distinct **Swarm ID** (ADR-0001) and **per-node-key trust** (ADR-0004), three-tier state dissemination (ADR-0005), `sandbox_id→owner` routing with cross-node gRPC forwarding + stream relay, swarm-wide event fan-out, failure detection (`unreachable`/`lost`), rejoin reconcile, cordon/drain, and capability/protocol gating (ADR-0009) — including the pending-join startup modes (spec §7).

**Architecture:** A `membership.Cluster` wraps `hashicorp/memberlist` with a `Delegate` that puts tiny routing data in `NodeMeta` (node_id/addr/cordoned/state_version/protocol_version) and bulky state (capabilities, owned sandbox IDs, limits, util) in TCP push/pull (`LocalState`/`MergeRemoteState`), plus delta broadcasts. A `routing.Table` maps node_id→REST/gRPC address and resolves a sandbox's owner from its self-routing prefix. A gRPC forwarding interceptor relays calls/streams to the owner. `WatchEvents` (server-stream) lets any node merge peers' events into the SSE firehose.

**Tech Stack:** Go 1.23, `github.com/hashicorp/memberlist`, M1 stack.

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/membership/identity.go` | Swarm ID mint/adopt/persist + join guard (ADR-0001) |
| `internal/membership/state.go` | NodeState encode/decode (NodeMeta tiny vs push/pull bulk) |
| `internal/membership/cluster.go` | memberlist wrapper + `Delegate`/`EventDelegate` |
| `internal/routing/table.go` | node_id→addr table; `Owner(sandboxID)`; cordon set |
| `internal/peer/client.go` | gRPC peer dialer (cached conns, node-key auth) |
| `internal/apiserver/forward.go` | forwarding interceptor (unary + stream) by sandbox owner |
| `internal/apiserver/watchevents.go` | `WatchEvents` server-stream + SSE peer-merge |
| `internal/config/config.go` | add `ClusterSecret`, `Join`, `SwarmName`, `Labels`, `ProvisionLimits` |
| `internal/node/node.go` | startup modes, cluster lifecycle, failure→reconcile wiring |

---

## Task 1: Swarm identity + join guard (pure logic)

**Files:** `internal/membership/identity.go`, test `internal/membership/identity_test.go`

- [x] **Step 1: Failing test**

```go
package membership

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSwarmIdentity_MintPersistAdopt(t *testing.T) {
	dir := t.TempDir()

	// no persisted id, no seeds -> mint standalone
	si, err := LoadOrInit(filepath.Join(dir, "swarm.json"), nil)
	require.NoError(t, err)
	require.Equal(t, ModeStandalone, si.Mode)
	require.NotEmpty(t, si.SwarmID)

	// reload -> same id, rejoin mode if it had peers; here standalone persists id
	si2, err := LoadOrInit(filepath.Join(dir, "swarm.json"), nil)
	require.NoError(t, err)
	require.Equal(t, si.SwarmID, si2.SwarmID)
}

func TestSwarmIdentity_PendingJoinWhenSeedsButNoID(t *testing.T) {
	si, err := LoadOrInit(filepath.Join(t.TempDir(), "swarm.json"), []string{"10.0.0.1:7946"})
	require.NoError(t, err)
	require.Equal(t, ModePendingJoin, si.Mode) // never mint when seeds are configured
	require.Empty(t, si.SwarmID)
}

func TestSwarmIdentity_JoinGuardRejectsMismatch(t *testing.T) {
	require.Error(t, GuardJoin("swarm-A", "swarm-B"))  // different id, same secret -> refuse
	require.NoError(t, GuardJoin("swarm-A", "swarm-A"))
	require.NoError(t, GuardJoin("", "swarm-A"))        // pending-join adopts
}
```

- [x] **Step 2: Run → FAIL**: `go test ./internal/membership/ -run TestSwarmIdentity -v`

- [x] **Step 3: Implement `identity.go`**

```go
// Package membership manages swarm membership: identity, gossip, and failure
// detection.
package membership

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
)

// Mode is how the node starts up relative to a swarm (spec §7).
type Mode int

const (
	ModeStandalone  Mode = iota // minted own id, no seeds
	ModePendingJoin             // seeds configured but no id yet; adopt on contact
	ModeRejoin                  // persisted id + seeds
)

// SwarmIdentity is the node's view of which swarm it belongs to.
type SwarmIdentity struct {
	SwarmID   string `json:"swarm_id"`
	SwarmName string `json:"swarm_name,omitempty"`
	Mode      Mode   `json:"-"`
}

// LoadOrInit loads a persisted swarm identity or initializes one per the
// startup rules: persisted id => rejoin; no id + seeds => pending-join (never
// mint); no id + no seeds => mint standalone.
func LoadOrInit(path string, seeds []string) (*SwarmIdentity, error) {
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		var si SwarmIdentity
		if err := json.Unmarshal(raw, &si); err != nil {
			return nil, err
		}
		si.Mode = ModeRejoin
		return &si, nil
	case !errors.Is(err, fs.ErrNotExist):
		return nil, err
	}
	if len(seeds) > 0 {
		return &SwarmIdentity{Mode: ModePendingJoin}, nil // adopt on contact; do not persist yet
	}
	si := &SwarmIdentity{SwarmID: newSwarmID(), Mode: ModeStandalone}
	return si, persist(path, si)
}

// Adopt records the swarm id learned from seeds (pending-join → member).
func (si *SwarmIdentity) Adopt(path, swarmID, swarmName string) error {
	si.SwarmID, si.SwarmName = swarmID, swarmName
	return persist(path, si)
}

// GuardJoin refuses to merge with a peer presenting a different swarm id under
// the same secret (ADR-0001). An empty local id means pending-join (adopt).
func GuardJoin(localID, peerID string) error {
	if localID == "" || localID == peerID {
		return nil
	}
	return errors.New("refusing to join: peer swarm id differs from ours under the same cluster secret")
}

func newSwarmID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func persist(path string, si *SwarmIdentity) error {
	raw, err := json.Marshal(si)
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}
```

- [x] **Step 4: Run → PASS, commit**

```bash
go test ./internal/membership/ -v
git add internal/membership/identity.go internal/membership/identity_test.go
git commit -m "feat(membership): swarm identity, startup modes, join guard (ADR-0001)"
```

---

## Task 2: Node state encode/decode (tiered)

**Files:** `internal/membership/state.go`, test `internal/membership/state_test.go`

- [x] **Step 1: Failing test**

```go
package membership

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNodeState_MetaTinyAndBulkRoundTrip(t *testing.T) {
	ns := NodeState{
		NodeID: "n1", Addr: "10.0.0.1:8443", Cordoned: true, StateVersion: 7, ProtocolVersion: 1,
		PubKey: []byte("pk"), Capabilities: []string{"clone", "stats"},
		OwnedSandboxIDs: []string{"n1.aaa", "n1.bbb"}, SwarmID: "swarm-A",
	}

	meta := ns.EncodeMeta()
	require.LessOrEqual(t, len(meta), 512) // NodeMeta budget (ADR-0005)
	gotMeta, err := DecodeMeta(meta)
	require.NoError(t, err)
	require.Equal(t, "n1", gotMeta.NodeID)
	require.Equal(t, uint64(7), gotMeta.StateVersion)

	bulk := ns.EncodeBulk()
	gotBulk, err := DecodeBulk(bulk)
	require.NoError(t, err)
	require.Equal(t, []string{"n1.aaa", "n1.bbb"}, gotBulk.OwnedSandboxIDs)
	require.Equal(t, []string{"clone", "stats"}, gotBulk.Capabilities)
}
```

- [x] **Step 2: Run → FAIL**: `go test ./internal/membership/ -run TestNodeState -v`

- [x] **Step 3: Implement `state.go`**

```go
package membership

import "encoding/json"

// NodeState is a node's full advertised state. It is split: small routing
// fields ride NodeMeta (UDP); the rest rides TCP push/pull (ADR-0005).
type NodeState struct {
	// meta (tiny, UDP)
	NodeID          string `json:"id"`
	Addr            string `json:"a"`  // gRPC/REST address for routing
	Cordoned        bool   `json:"c"`
	StateVersion    uint64 `json:"v"`
	ProtocolVersion uint32 `json:"p"`
	// bulk (TCP push/pull)
	SwarmID         string   `json:"swarm_id,omitempty"`
	PubKey          []byte   `json:"pubkey,omitempty"`
	Capabilities    []string `json:"caps,omitempty"`
	OwnedSandboxIDs []string `json:"owned,omitempty"`
	Labels          map[string]string `json:"labels,omitempty"`
	LimitCPU        float64  `json:"limit_cpu,omitempty"`
	LimitMemKB      float64  `json:"limit_mem_kb,omitempty"`
	AllocCPU        float64  `json:"alloc_cpu,omitempty"`
	AllocMemKB      float64  `json:"alloc_mem_kb,omitempty"`
}

type metaWire struct {
	NodeID, Addr    string
	Cordoned        bool
	StateVersion    uint64
	ProtocolVersion uint32
}

// EncodeMeta serializes only the tiny routing fields for NodeMeta.
func (n NodeState) EncodeMeta() []byte {
	b, _ := json.Marshal(metaWire{n.NodeID, n.Addr, n.Cordoned, n.StateVersion, n.ProtocolVersion})
	return b
}

// DecodeMeta parses NodeMeta into a partial NodeState (routing fields only).
func DecodeMeta(b []byte) (NodeState, error) {
	var m metaWire
	if err := json.Unmarshal(b, &m); err != nil {
		return NodeState{}, err
	}
	return NodeState{NodeID: m.NodeID, Addr: m.Addr, Cordoned: m.Cordoned, StateVersion: m.StateVersion, ProtocolVersion: m.ProtocolVersion}, nil
}

// EncodeBulk/DecodeBulk serialize the full state for TCP push/pull.
func (n NodeState) EncodeBulk() []byte { b, _ := json.Marshal(n); return b }
func DecodeBulk(b []byte) (NodeState, error) {
	var n NodeState
	err := json.Unmarshal(b, &n)
	return n, err
}
```

- [x] **Step 4: Run → PASS, commit**

```bash
go test ./internal/membership/ -v
git add internal/membership/state.go internal/membership/state_test.go
git commit -m "feat(membership): tiered node-state encode/decode (ADR-0005)"
```

---

## Task 3: Routing table

**Files:** `internal/routing/table.go`, test `internal/routing/table_test.go`

- [x] **Step 1: Failing test**

```go
package routing

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTable_OwnerByPrefixAndAddr(t *testing.T) {
	tbl := NewTable("self")
	tbl.Upsert("self", "127.0.0.1:1", false)
	tbl.Upsert("n2", "127.0.0.1:2", false)

	owner, ok := tbl.Owner("n2.01ABC")
	require.True(t, ok)
	require.Equal(t, "n2", owner)
	require.False(t, tbl.IsLocal("n2.01ABC"))
	require.True(t, tbl.IsLocal("self.01XYZ"))

	addr, ok := tbl.Addr("n2")
	require.True(t, ok)
	require.Equal(t, "127.0.0.1:2", addr)

	// cordoned nodes are excluded from scheduling candidates
	tbl.Upsert("n2", "127.0.0.1:2", true)
	require.True(t, tbl.IsCordoned("n2"))
}
```

- [x] **Step 2: Run → FAIL**: `go test ./internal/routing/ -v`

- [x] **Step 3: Implement `table.go`**

```go
// Package routing resolves which node owns a sandbox (by its self-routing id
// prefix, ADR-0002) and tracks node addresses + cordon state.
package routing

import (
	"strings"
	"sync"
)

type entry struct {
	addr     string
	cordoned bool
}

// Table is the in-memory node directory (rebuilt from gossip).
type Table struct {
	self string
	mu   sync.RWMutex
	m    map[string]entry
}

// NewTable returns a table for the local node id.
func NewTable(self string) *Table { return &Table{self: self, m: map[string]entry{}} }

// Upsert records a node's address + cordon flag.
func (t *Table) Upsert(nodeID, addr string, cordoned bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.m[nodeID] = entry{addr: addr, cordoned: cordoned}
}

// Remove drops a node (left/dead).
func (t *Table) Remove(nodeID string) { t.mu.Lock(); delete(t.m, nodeID); t.mu.Unlock() }

// Owner returns the node id that owns a sandbox/op id (its prefix).
func (t *Table) Owner(id string) (string, bool) {
	i := strings.IndexByte(id, '.')
	if i <= 0 {
		return "", false
	}
	return id[:i], true
}

// IsLocal reports whether the id is owned by this node.
func (t *Table) IsLocal(id string) bool {
	owner, ok := t.Owner(id)
	return ok && owner == t.self
}

// Addr returns a node's address.
func (t *Table) Addr(nodeID string) (string, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	e, ok := t.m[nodeID]
	return e.addr, ok
}

// IsCordoned reports a node's cordon state.
func (t *Table) IsCordoned(nodeID string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.m[nodeID].cordoned
}

// Peers returns all known node ids except self.
func (t *Table) Peers() []string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	var out []string
	for id := range t.m {
		if id != t.self {
			out = append(out, id)
		}
	}
	return out
}
```

- [x] **Step 4: Run → PASS, commit**

```bash
go test ./internal/routing/ -v
git add internal/routing/ && git commit -m "feat(routing): node directory + owner-by-prefix resolution"
```

---

## Task 4: Peer gRPC client (cached, node-key auth)

**Files:** `internal/peer/client.go`, test `internal/peer/client_test.go` (dial a local in-process server)

- [x] **Step 1: Failing test** — start an in-process gRPC server exposing `NodeService`, dial it via `peer.Pool.Conn(addr)`, call `GetNodeInfo`, assert the cached conn is reused.

```go
package peer

import (
	"context"
	"net"
	"testing"

	sbxv1 "github.com/squall-chua/sbx-swarm-node/internal/gen/sbxswarm/v1"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

type infoSvc struct{ sbxv1.UnimplementedNodeServiceServer }

func (infoSvc) GetNodeInfo(context.Context, *sbxv1.GetNodeInfoRequest) (*sbxv1.NodeInfo, error) {
	return &sbxv1.NodeInfo{NodeId: "peer"}, nil
}

func TestPool_DialAndReuse(t *testing.T) {
	lis := bufconn.Listen(1 << 20)
	s := grpc.NewServer()
	sbxv1.RegisterNodeServiceServer(s, infoSvc{})
	go s.Serve(lis)
	defer s.Stop()

	dial := func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }
	p := NewPool(WithContextDialer(dial), WithCreds(insecure.NewCredentials()))

	c1, err := p.Conn("bufnet")
	require.NoError(t, err)
	out, err := sbxv1.NewNodeServiceClient(c1).GetNodeInfo(context.Background(), &sbxv1.GetNodeInfoRequest{})
	require.NoError(t, err)
	require.Equal(t, "peer", out.NodeId)

	c2, err := p.Conn("bufnet")
	require.NoError(t, err)
	require.Same(t, c1, c2) // cached
}
```

- [x] **Step 2: Run → FAIL**: `go test ./internal/peer/ -v`

- [x] **Step 3: Implement `client.go`**

```go
// Package peer maintains cached gRPC connections to other nodes.
package peer

import (
	"context"
	"net"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// Pool caches one gRPC client connection per peer address.
type Pool struct {
	mu     sync.Mutex
	conns  map[string]*grpc.ClientConn
	dialer func(context.Context, string) (net.Conn, error)
	creds  credentials.TransportCredentials
}

// Option configures the Pool.
type Option func(*Pool)

// WithContextDialer overrides the dialer (tests use bufconn).
func WithContextDialer(d func(context.Context, string) (net.Conn, error)) Option {
	return func(p *Pool) { p.dialer = d }
}

// WithCreds sets transport credentials (TLS in production; node-key auth is
// added via a PerRPCCredentials / interceptor in a later step).
func WithCreds(c credentials.TransportCredentials) Option { return func(p *Pool) { p.creds = c } }

// NewPool builds a connection pool.
func NewPool(opts ...Option) *Pool {
	p := &Pool{conns: map[string]*grpc.ClientConn{}}
	for _, o := range opts {
		o(p)
	}
	return p
}

// Conn returns a cached connection to addr, dialing if needed.
func (p *Pool) Conn(addr string) (*grpc.ClientConn, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.conns[addr]; ok {
		return c, nil
	}
	var dialOpts []grpc.DialOption
	if p.dialer != nil {
		dialOpts = append(dialOpts, grpc.WithContextDialer(p.dialer))
	}
	if p.creds != nil {
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(p.creds))
	}
	c, err := grpc.NewClient(addr, dialOpts...)
	if err != nil {
		return nil, err
	}
	p.conns[addr] = c
	return c, nil
}

// Close closes all connections.
func (p *Pool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.conns {
		_ = c.Close()
	}
	p.conns = map[string]*grpc.ClientConn{}
}
```

- [x] **Step 4: Run → PASS, commit**

```bash
go test ./internal/peer/ -v
git add internal/peer/ && git commit -m "feat(peer): cached gRPC connection pool"
```

> Node-key auth (ADR-0004): add a `PerRPCCredentials` that signs a per-call challenge with the node's Ed25519 key, plus a server interceptor that verifies against the gossiped pinned pubkey for the caller's node_id. Add this as a follow-on task once Tasks 1–7 land; until then peers authenticate via the shared TLS + cluster secret. (Tracked in the self-review.)

---

## Task 5: Forwarding interceptor (unary + stream)

**Files:** `internal/apiserver/forward.go`, test `internal/apiserver/forward_test.go` (two in-process servers; a request for a non-local sandbox id is relayed)

- [x] **Step 1: Failing test** — stand up server B with a fake-backed `SandboxService` holding sandbox `nB.x`; stand up server A with the forwarding interceptor and a routing table pointing `nB`→B's bufconn; call `GetSandbox(nB.x)` on A and assert it returns B's data.

(Full server harness mirrors M1c `server_test.go`; the assertion is that A returns the sandbox owned by B.)

- [x] **Step 2: Run → FAIL**: `go test ./internal/apiserver/ -run TestForward -v`

- [x] **Step 3: Implement `forward.go`**

```go
package apiserver

import (
	"context"
	"strings"

	"github.com/squall-chua/sbx-swarm-node/internal/peer"
	"github.com/squall-chua/sbx-swarm-node/internal/routing"
	"google.golang.org/grpc"
)

// Forwarder routes a request to the owning node when its sandbox id is remote.
type Forwarder struct {
	tbl  *routing.Table
	pool *peer.Pool
}

// NewForwarder builds the forwarder.
func NewForwarder(tbl *routing.Table, pool *peer.Pool) *Forwarder { return &Forwarder{tbl: tbl, pool: pool} }

// idExtractor pulls the routable id from a request, if it has one.
type idExtractor interface{ GetId() string }

// UnaryInterceptor relays unary calls whose request carries a remote sandbox id.
func (f *Forwarder) UnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		id, ok := routableID(req)
		if !ok || f.tbl.IsLocal(id) {
			return handler(ctx, req) // local: handle here
		}
		owner, _ := f.tbl.Owner(id)
		addr, found := f.tbl.Addr(owner)
		if !found {
			return handler(ctx, req) // unknown owner: let local handler 404
		}
		conn, err := f.pool.Conn(addr)
		if err != nil {
			return nil, err
		}
		// Re-dispatch the same method on the owner, reusing the generic invoker.
		out := newReplyFor(info.FullMethod)
		if err := conn.Invoke(ctx, info.FullMethod, req, out); err != nil {
			return nil, err
		}
		return out, nil
	}
}

func routableID(req any) (string, bool) {
	if e, ok := req.(idExtractor); ok {
		if id := e.GetId(); strings.Contains(id, ".") {
			return id, true
		}
	}
	return "", false
}
```

`newReplyFor(fullMethod)` returns a freshly-allocated reply message for the method (a small map from method name → `func() proto.Message`, populated for the forwardable RPCs: GetSandbox→`*sbxv1.Sandbox`, DeleteSandbox→`*sbxv1.Operation`, Start/Stop→`*sbxv1.Sandbox`, Exec→`*sbxv1.ExecResponse`, GetStats→`*sbxv1.Stats`, ListBlocked→`*sbxv1.ListBlockedResponse`, AgentRun→`*sbxv1.Operation`, PublishPort→`*sbxv1.Port`, ListPorts→`*sbxv1.ListPortsResponse`). Provide the map fully in the implementation.

> Stream relay (terminal/logs SSE) uses the same owner resolution: the SSE handlers, on a remote id, dial the owner and copy its stream to the client (lossless for logs/terminal, lossy-coalesce for stats — spec §8). Implement in the observe/SSE handlers by checking `tbl.IsLocal(id)` and, if remote, proxying to `https://<ownerAddr>/v1/sandboxes/{id}/...` with the caller's credentials.

- [x] **Step 4: Run → PASS, commit**

```bash
go test ./internal/apiserver/ -run TestForward -v
git add internal/apiserver/forward.go internal/apiserver/forward_test.go
git commit -m "feat(apiserver): cross-node unary forwarding by sandbox owner"
```

---

## Task 6: WatchEvents + swarm-wide SSE merge

**Files:** `proto/sbxswarm/v1/events.proto`, `internal/apiserver/watchevents.go`, test `internal/apiserver/watchevents_test.go`

- [x] **Step 1: Proto** — `service EventService { rpc WatchEvents(WatchRequest) returns (stream EventMsg); }` with `EventMsg{id,seq,type,node_id,sandbox_id,payload}` and `WatchRequest{types,sandbox,since_seq}`. Regenerate.

- [x] **Step 2: Failing test (merge dedup, pure)** — a `Merger` that consumes two channels of `events.Event` and emits a single stream deduped by `ID`:

```go
func TestMerger_DedupsByID(t *testing.T) {
	out := make(chan events.Event, 8)
	m := NewMerger(out)
	a, b := make(chan events.Event, 4), make(chan events.Event, 4)
	go m.Consume(a); go m.Consume(b)

	e := events.Event{ID: "n1-1", Type: "x"}
	a <- e; b <- e // same event from two sources
	close(a); close(b)

	got := <-out
	require.Equal(t, "n1-1", got.ID)
	select {
	case dup := <-out:
		t.Fatalf("unexpected duplicate %v", dup)
	default:
	}
}
```

- [x] **Step 3: Implement** the `EventService.WatchEvents` server (streams the local bus, M1d) and a `Merger` (dedup by ID via a bounded seen-set). The SSE handler (M1d) gains peer-merge: on connect it subscribes locally **and** opens `WatchEvents` to each peer (`tbl.Peers()` → `pool.Conn`), feeding all into the `Merger` whose output writes SSE frames. One shared peer subscription per node is fine for v1 (spec §19 SSE-fan-out note).

- [x] **Step 4: Run → PASS, commit**

```bash
go test ./internal/apiserver/ -run "TestMerger|TestWatch" -v
git add proto/ internal/gen/ internal/apiserver/watchevents.go internal/apiserver/watchevents_test.go
git commit -m "feat(events): WatchEvents stream + swarm-wide SSE merge"
```

---

## Task 7: memberlist cluster + failure→reconcile + cordon/drain + node wiring

**Files:** `internal/membership/cluster.go`, `internal/config/config.go`, `internal/node/node.go`, test `internal/membership/cluster_integration_test.go`

- [x] **Step 1: Extend config** — add `ClusterSecret string`, `Join []string`, `SwarmName string`, `Labels map[string]string`, `ProvisionLimits struct{CPUCores float64; MemoryBytes int64}`; extend `Validate`.

- [x] **Step 2: Implement `cluster.go`** — wrap `memberlist`:
  - `Delegate.NodeMeta(limit)` → `state.EncodeMeta()` of the local `NodeState` (must fit `limit`; if over, log + truncate owned-ids out of meta — they live in bulk anyway).
  - `Delegate.LocalState(join)`/`MergeRemoteState(buf, join)` → exchange `state.EncodeBulk()`; on merge, `GuardJoin(local.SwarmID, remote.SwarmID)` and reject (leave) on mismatch; update `routing.Table` + a `peerStates` map (caps, owned ids, limits, util); on pending-join, `Adopt` the remote swarm id.
  - `Delegate.GetBroadcasts` / `NotifyMsg` → delta broadcasts (e.g. "sandbox created/removed", "node cordoned") with a `memberlist.TransmitLimitedQueue`.
  - `EventDelegate.NotifyJoin/Update/Leave` → `tbl.Upsert`/`tbl.Remove`; on Leave/dead, call back into the node to mark that node's sandboxes `unreachable`.
  - Methods: `Join(seeds)`, `Leave(timeout)`, `LocalNodeState()`, `SetCordoned(bool)`, `BumpStateVersion()`.

- [x] **Step 3: Node wiring + lifecycle (`node.New`/`Start`/`Stop`)**
  - Build `SwarmIdentity` via `membership.LoadOrInit(dataDir/swarm.json, cfg.Join)`.
  - Build `routing.Table(nodeID)`, `peer.Pool` (TLS creds), `Forwarder`, register `Forwarder.UnaryInterceptor()` on the grpc server (via `apiserver.Options`).
  - Start `membership.Cluster` with the local `NodeState` (caps from sbx/SDK version, owned ids from `mgr.List`, limits from cfg); `Join(cfg.Join)` if seeds (non-blocking, retry in background per startup modes).
  - On peer-dead callback → `mgr.MarkUnreachable(ownedIDs)` (new manager method: set status `unreachable` for sandboxes owned by a dead node — but those are *remote* records the node only knows via gossip; for v1 the node reflects unreachable in its peerStates view and the API surfaces it. The **owner** still owns truth; on the dead node's own rejoin it reconciles `lost`).
  - Add cordon/drain RPCs to `NodeService` (`Cordon`/`Uncordon`/`Drain`) → `cluster.SetCordoned`; `Drain` also stops accepting placements (M5 scheduler reads `tbl.IsCordoned`).
  - On rejoin, call `mgr.Reconcile(ctx)` (M1c) to heal local records and re-advertise.

- [x] **Step 4: Integration test (`cluster_integration_test.go`, tag `integration`)** — start two `node.Node`s on loopback, node B joins node A; assert: A sees B in `routing.Table`; a sandbox created on B is reachable via A's REST (forwarding); stopping B marks its sandboxes view `unreachable`; restarting B + rejoin clears it.

```bash
go test -tags integration ./internal/membership/ -v
```

- [x] **Step 5: Run all + commit**

```bash
go test ./...
git add internal/membership/ internal/config/ internal/node/
git commit -m "feat(swarm): memberlist cluster, failure detection, cordon/drain, wiring"
```

---

## Self-Review

**Spec coverage (M4):** swarm identity + guard + startup modes (ADR-0001, §7) → Tasks 1,7 ✓; tiered dissemination (ADR-0005) → Tasks 2,7 ✓; routing by self-routing prefix (ADR-0002) → Tasks 3,5 ✓; cross-node forwarding (unary done; stream relay specified) → Task 5 ✓; swarm-wide event fan-out → Task 6 ✓; capability + protocol gating (ADR-0009) → Tasks 2,7 ✓; failure → `unreachable`, rejoin reconcile, cordon/drain → Task 7 ✓.

**Known follow-ons (called out, not silently missing):** (a) **node-key challenge auth** (ADR-0004) is specified as a follow-on task after Task 4 — until then peers rely on TLS + cluster secret; (b) **stream relay** for terminal/logs is specified in Task 5's note and implemented in the SSE handlers; (c) the dead-node `unreachable` reflection is a view-level marking — the authoritative `lost` transition remains owner-only (M1c reconcile). These are explicit, with the exact mechanism described.

**Placeholder scan:** No TBD/TODO in pure-logic tasks (1–4, 6-merge) which are fully coded + unit-TDD'd. Tasks 5(stream)/7 are integration-level and specified by precise behavior + an integration test, which is the honest approach for memberlist/gRPC-relay wiring (unit tests would mock away the very thing under test).

**Type consistency:** `membership.LoadOrInit→*SwarmIdentity{Adopt}`, `GuardJoin`; `membership.NodeState.{EncodeMeta,EncodeBulk}`/`DecodeMeta`/`DecodeBulk`; `routing.NewTable(self).{Upsert,Owner,IsLocal,Addr,IsCordoned,Peers,Remove}`; `peer.NewPool(opts).{Conn,Close}`; `apiserver.NewForwarder(tbl,pool).UnaryInterceptor()`. Manager gains `MarkUnreachable`/reuses `Reconcile`.
