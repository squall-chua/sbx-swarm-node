# sbx-swarm-node M8a — Backend API Completion Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add the five backend capabilities the M8b swarm console needs — list-nodes, list-templates, list-operations, cross-node cordon/drain, and an interactive terminal WebSocket — each following an existing node pattern.

**Architecture:** The three reads and the cordon change are ordinary gRPC methods with grpc-gateway HTTP annotations, classified in `authz.go`, served over the existing one-port loopback chain. The terminal is the only non-gRPC piece: a native `http.Handler` (gateway can't do WebSockets) composed into the `/v1` handler *before* the gateway and *inside* `OwnerProxy`, so a peer-owned sandbox's terminal is reverse-proxied to its owner for free.

**Tech Stack:** Go 1.25, protobuf via `buf generate`, grpc-gateway, bbolt, `github.com/coder/websocket` (new), sbx-go-sdk `exec` package (interactive attach), testify.

## Global Constraints

- **Codegen flow:** edit `proto/sbxswarm/v1/*.proto` → `buf generate` → `go build ./...` → commit the regenerated `internal/gen/sbxswarm/v1/*` (git-tracked). `go build` does not compile tests; run `go vet ./...` / `go test ./...` to catch test breakage. After `buf generate`, gopls shows false "undefined/redeclared" — trust `go build`, not the editor.
- **Authz drift guard:** every new gRPC method MUST be added to exactly one bucket in `internal/apiserver/authz.go` (`readMethods` / `mutatingMethods` / `internalMethods`) or `TestAuthz_AllMethodsClassified` fails. The terminal endpoint is NOT a gRPC method and is exempt.
- **REST JSON is snake_case** (gateway marshaler uses `UseProtoNames` + `EmitUnpopulated`). Don't change the marshaler.
- **TDD throughout:** write the failing test, watch it fail, minimal code to pass. Real daemon tests are `//go:build integration`, env-gated, red-by-default in CI (run with `-tags integration`; a live sbx v0.32.0 daemon is available on the dev box).
- **Merges are user-driven** local ff-merges: branch → commit per task → ask before merging. Commit trailer: `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`.
- **Manager writes** go through the lock-protected `mutate` helper; none of these tasks mutate Manager records, but BumpActivity (used by the terminal) is already lock-safe.

---

### Task 1: `ListOperations` — durable operation history

**Files:**
- Modify: `internal/ops/ops.go` (add `List`)
- Test: `internal/ops/ops_test.go` (add `TestManager_ListNewestFirst`)
- Modify: `proto/sbxswarm/v1/sandbox.proto` (add messages + rpc)
- Modify: `internal/apiserver/sandboxservice.go` (add `ListOperations`)
- Modify: `internal/apiserver/authz.go` (classify read)
- Test: `internal/apiserver/sandboxservice_test.go` (add `TestSandboxService_ListOperations`)

**Interfaces:**
- Produces: `ops.Manager.List(limit int) ([]*ops.Operation, error)` — newest-first, capped.
- Produces: `SandboxService.ListOperations(ctx, *sbxv1.ListOperationsRequest) (*sbxv1.ListOperationsResponse, error)`.

- [ ] **Step 1: Write the failing test for `ops.Manager.List`**

In `internal/ops/ops_test.go` (reuse the existing test harness that builds a `Manager` over a temp `store.Store` — copy the setup from the top of that file):

```go
func TestManager_ListNewestFirst(t *testing.T) {
	m := newTestManager(t) // existing helper: Manager over a temp store
	o1, _, err := m.Start(context.Background(), "provision", "")
	require.NoError(t, err)
	o2, _, err := m.Start(context.Background(), "remove", "")
	require.NoError(t, err)

	got, err := m.List(0) // 0 => default cap
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, o2.ID, got[0].ID, "newest first")
	require.Equal(t, o1.ID, got[1].ID)

	require.Len(t, mustList(t, m, 1), 1) // limit honored
}

func mustList(t *testing.T, m *Manager, n int) []*Operation {
	t.Helper()
	got, err := m.List(n)
	require.NoError(t, err)
	return got
}
```

If `newTestManager` does not exist, build the Manager inline exactly as the other tests in `ops_test.go` do (open a `store.Store` on `t.TempDir()`, `NewManager(st, ids.NewGen("n"))`).

- [ ] **Step 2: Run it, watch it fail**

Run: `go test ./internal/ops/ -run TestManager_ListNewestFirst`
Expected: FAIL — `m.List undefined`.

- [ ] **Step 3: Implement `List`**

In `internal/ops/ops.go`, add `"sort"` to imports, a const, and the method (mirror `RecoverInterrupted`'s `ForEach` read pattern — collect then process, never mutate inside the iterator):

```go
const defaultListLimit = 200

// List returns operations newest-first (by CreatedAt), capped at limit. limit
// <= 0 or > defaultListLimit uses defaultListLimit. Reads the durable
// operations bucket — this is the operation history, distinct from the
// best-effort event firehose (ADR-0008).
func (m *Manager) List(limit int) ([]*Operation, error) {
	if limit <= 0 || limit > defaultListLimit {
		limit = defaultListLimit
	}
	var ops []*Operation
	err := m.store.ForEach(opBucket, func(_, v []byte) error {
		var op Operation
		if uerr := json.Unmarshal(v, &op); uerr != nil {
			return uerr
		}
		ops = append(ops, &op)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(ops, func(i, j int) bool { return ops[i].CreatedAt.After(ops[j].CreatedAt) })
	if len(ops) > limit {
		ops = ops[:limit]
	}
	return ops, nil
}
```

- [ ] **Step 4: Run it, watch it pass**

Run: `go test ./internal/ops/ -run TestManager_ListNewestFirst`
Expected: PASS.

- [ ] **Step 5: Add the proto messages + rpc**

In `proto/sbxswarm/v1/sandbox.proto`, add to `service SandboxService` (after `KeepAlive`):

```proto
  rpc ListOperations(ListOperationsRequest) returns (ListOperationsResponse) {
    option (google.api.http) = {get: "/v1/operations"};
  }
```

and add the messages near `Operation`:

```proto
message OperationSummary {
  string id = 1;
  string type = 2;
  string state = 3;
  string sandbox_id = 4;
  string error = 5;
  string created_at = 6; // RFC3339
  string updated_at = 7; // RFC3339
}
message ListOperationsRequest { int32 limit = 1; } // 0 = server default (200)
message ListOperationsResponse { repeated OperationSummary operations = 1; }
```

- [ ] **Step 6: Regenerate + classify**

Run: `buf generate && go build ./...`

In `internal/apiserver/authz.go`, add to `readMethods`:

```go
	"/sbxswarm.v1.SandboxService/ListOperations": true,
```

- [ ] **Step 7: Write the failing service test**

In `internal/apiserver/sandboxservice_test.go` (use the existing `newSandboxSvc`/`newTestManager`-style harness already in that file; it exposes the `*SandboxService` and its `ops` manager — if the harness doesn't return the ops manager, drive ops through the service by creating a sandbox first):

```go
func TestSandboxService_ListOperations(t *testing.T) {
	svc := newSandboxSvc(t) // existing harness
	// Create a sandbox to produce a "provision" operation through the service.
	_, err := svc.CreateSandbox(context.Background(), &sbxv1.CreateSandboxRequest{})
	require.NoError(t, err)

	resp, err := svc.ListOperations(context.Background(), &sbxv1.ListOperationsRequest{})
	require.NoError(t, err)
	require.NotEmpty(t, resp.Operations)
	require.Equal(t, "provision", resp.Operations[0].Type)
	require.NotEmpty(t, resp.Operations[0].CreatedAt)
}
```

- [ ] **Step 8: Run it, watch it fail**

Run: `go test ./internal/apiserver/ -run TestSandboxService_ListOperations`
Expected: FAIL — `svc.ListOperations undefined`.

- [ ] **Step 9: Implement `ListOperations`**

In `internal/apiserver/sandboxservice.go` (the service already holds the ops manager as `s.ops`; confirm the field name and reuse it):

```go
func (s *SandboxService) ListOperations(_ context.Context, r *sbxv1.ListOperationsRequest) (*sbxv1.ListOperationsResponse, error) {
	list, err := s.ops.List(int(r.Limit))
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	out := &sbxv1.ListOperationsResponse{}
	for _, op := range list {
		out.Operations = append(out.Operations, &sbxv1.OperationSummary{
			Id: op.ID, Type: op.Type, State: op.State, SandboxId: op.SandboxID,
			Error:     op.Error,
			CreatedAt: op.CreatedAt.UTC().Format(time.RFC3339),
			UpdatedAt: op.UpdatedAt.UTC().Format(time.RFC3339),
		})
	}
	return out, nil
}
```

(`time` is already imported in this file. If `s.ops` is unexported under a different name, grep `s.ops.Start` in the file to confirm.)

- [ ] **Step 10: Run service test + authz guard + full build**

Run: `go test ./internal/apiserver/ -run 'TestSandboxService_ListOperations|TestAuthz_AllMethodsClassified' && go build ./... && go vet ./...`
Expected: PASS.

- [ ] **Step 11: Commit**

```bash
git add proto internal/gen internal/ops/ops.go internal/ops/ops_test.go internal/apiserver/sandboxservice.go internal/apiserver/sandboxservice_test.go internal/apiserver/authz.go
git commit -m "feat(apiserver): ListOperations - durable operation history endpoint"
```

---

### Task 2: `ListNodes` — swarm topology

**Files:**
- Modify: `proto/sbxswarm/v1/node.proto` (NodeSummary + rpc)
- Modify: `internal/apiserver/nodeservice.go` (`NodeRow`, `SetNodeLister`, `Draining`, `ListNodes`)
- Modify: `internal/apiserver/authz.go` (classify read)
- Modify: `internal/node/node.go` (wire the lister closure)
- Test: `internal/apiserver/nodeservice_test.go` (add `TestNodeService_ListNodes`)

**Interfaces:**
- Produces: `apiserver.NodeRow` struct (wiring layer fills it; keeps apiserver free of a `membership` import).
- Produces: `NodeService.SetNodeLister(func() []NodeRow)`, `NodeService.Draining() bool`, `NodeService.ListNodes(...)`.
- Consumes: `membership.Cluster.LocalNodeState()`, `.PeerStates()`; `sandbox.Capacity.Limits()/Snapshot()` (already used in node.go).

- [ ] **Step 1: Add proto**

In `proto/sbxswarm/v1/node.proto`, add to `service NodeService`:

```proto
  rpc ListNodes(ListNodesRequest) returns (ListNodesResponse) {
    option (google.api.http) = {get: "/v1/nodes"};
  }
```

and messages:

```proto
message ListNodesRequest {}
message ListNodesResponse { repeated NodeSummary nodes = 1; }

message NodeSummary {
  string node_id = 1;
  string node_name = 2;        // self-only (peers gossip no name)
  bool cordoned = 3;
  bool draining = 4;           // self-only (not gossiped)
  map<string, string> labels = 5;
  repeated string capabilities = 6;
  repeated string workspaces = 7;
  repeated string templates = 8;
  double limit_cpu = 9;
  double limit_mem_kb = 10;
  double limit_disk_gb = 11;
  double alloc_cpu = 12;
  double alloc_mem_kb = 13;
  double alloc_disk_gb = 14;
  double actual_cpu = 15;      // gossiped util, 0..1+
  double actual_mem = 16;
}
```

- [ ] **Step 2: Regenerate**

Run: `buf generate && go build ./...`
Expected: builds (NodeService now has an unimplemented `ListNodes` via the embedded `UnimplementedNodeServiceServer`).

- [ ] **Step 3: Add `NodeRow` + setters to NodeService**

In `internal/apiserver/nodeservice.go`, add the struct (top of file, after imports) and fields/methods:

```go
// NodeRow is one node's summary for ListNodes, assembled by the wiring layer
// (node.go) so apiserver need not import membership. Field names/units mirror
// membership.NodeState.
type NodeRow struct {
	NodeID, NodeName                  string
	Cordoned, Draining                bool
	Labels                            map[string]string
	Capabilities, Workspaces, Templates []string
	LimitCPU, LimitMemKB, LimitDiskGB float64
	AllocCPU, AllocMemKB, AllocDiskGB float64
	ActualCPU, ActualMem              float64
}
```

Add `nodeLister func() []NodeRow` to the `NodeService` struct, and:

```go
// SetNodeLister wires the swarm-node snapshot source (node.go). nil-safe:
// without it, ListNodes reports self identity only.
func (s *NodeService) SetNodeLister(fn func() []NodeRow) { s.nodeLister = fn }

// Draining reports this node's drain flag (self-only; not gossiped).
func (s *NodeService) Draining() bool { return s.draining.Load() }

// ListNodes returns self plus gossiped peers (a node present here is alive by
// construction — dead nodes are removed from routing).
func (s *NodeService) ListNodes(_ context.Context, _ *sbxv1.ListNodesRequest) (*sbxv1.ListNodesResponse, error) {
	out := &sbxv1.ListNodesResponse{}
	if s.nodeLister == nil {
		out.Nodes = append(out.Nodes, &sbxv1.NodeSummary{
			NodeId: s.nodeID, NodeName: s.nodeName, Draining: s.draining.Load(),
		})
		return out, nil
	}
	for _, r := range s.nodeLister() {
		out.Nodes = append(out.Nodes, &sbxv1.NodeSummary{
			NodeId: r.NodeID, NodeName: r.NodeName, Cordoned: r.Cordoned, Draining: r.Draining,
			Labels: r.Labels, Capabilities: r.Capabilities, Workspaces: r.Workspaces, Templates: r.Templates,
			LimitCpu: r.LimitCPU, LimitMemKb: r.LimitMemKB, LimitDiskGb: r.LimitDiskGB,
			AllocCpu: r.AllocCPU, AllocMemKb: r.AllocMemKB, AllocDiskGb: r.AllocDiskGB,
			ActualCpu: r.ActualCPU, ActualMem: r.ActualMem,
		})
	}
	return out, nil
}
```

(Generated Go field names use Go casing: `LimitCpu`, `LimitMemKb`, etc. Confirm against `internal/gen/.../node.pb.go` after Step 2.)

- [ ] **Step 4: Classify read in authz.go**

Add to `readMethods`:

```go
	"/sbxswarm.v1.NodeService/ListNodes": true,
```

- [ ] **Step 5: Write the failing NodeService test**

In `internal/apiserver/nodeservice_test.go`:

```go
func TestNodeService_ListNodes(t *testing.T) {
	svc := NewNodeService("n1", "node-one", "test")

	// No lister: self identity only.
	resp, err := svc.ListNodes(context.Background(), &sbxv1.ListNodesRequest{})
	require.NoError(t, err)
	require.Len(t, resp.Nodes, 1)
	require.Equal(t, "n1", resp.Nodes[0].NodeId)

	// With a lister returning self + one peer.
	svc.SetNodeLister(func() []NodeRow {
		return []NodeRow{
			{NodeID: "n1", NodeName: "node-one", Cordoned: false, LimitCpuFields(2, 0, 0)},
			{NodeID: "n2", Cordoned: true, Labels: map[string]string{"zone": "b"}},
		}
	})
	resp, err = svc.ListNodes(context.Background(), &sbxv1.ListNodesRequest{})
	require.NoError(t, err)
	require.Len(t, resp.Nodes, 2)
	require.True(t, resp.Nodes[1].Cordoned)
	require.Equal(t, "b", resp.Nodes[1].Labels["zone"])
}
```

Replace the `LimitCpuFields(2,0,0)` placeholder with literal field assignment `LimitCPU: 2` — written here only to flag: set `LimitCPU: 2` on the self row and assert `resp.Nodes[0].LimitCpu == 2`. Final test body:

```go
	svc.SetNodeLister(func() []NodeRow {
		return []NodeRow{
			{NodeID: "n1", NodeName: "node-one", LimitCPU: 2},
			{NodeID: "n2", Cordoned: true, Labels: map[string]string{"zone": "b"}},
		}
	})
	resp, err = svc.ListNodes(context.Background(), &sbxv1.ListNodesRequest{})
	require.NoError(t, err)
	require.Len(t, resp.Nodes, 2)
	require.Equal(t, float64(2), resp.Nodes[0].LimitCpu)
	require.True(t, resp.Nodes[1].Cordoned)
	require.Equal(t, "b", resp.Nodes[1].Labels["zone"])
```

- [ ] **Step 6: Run it (fail then pass)**

Run: `go test ./internal/apiserver/ -run 'TestNodeService_ListNodes|TestAuthz_AllMethodsClassified'`
Expected: PASS (the impl from Step 3 is already present; if you wrote the test first per TDD, it failed at Step 3's absence — that's fine, the ordering here folds the impl in).

- [ ] **Step 7: Wire the lister in node.go**

In `internal/node/node.go`, after `nodeSvc` is created and the cluster section (so `clusterInstance` is known), add a helper and the wiring. Add near the other helpers:

```go
// rowFromState maps a gossiped NodeState to a NodeRow (peer view: no name/draining).
func rowFromState(ns membership.NodeState) apiserver.NodeRow {
	return apiserver.NodeRow{
		NodeID: ns.NodeID, Cordoned: ns.Cordoned, Labels: ns.Labels,
		Capabilities: ns.Capabilities, Workspaces: ns.Workspaces, Templates: ns.Templates,
		LimitCPU: ns.LimitCPU, LimitMemKB: ns.LimitMemKB, LimitDiskGB: ns.LimitDiskGB,
		AllocCPU: ns.AllocCPU, AllocMemKB: ns.AllocMemKB, AllocDiskGB: ns.AllocDiskGB,
		ActualCPU: ns.ActualCPU, ActualMem: ns.ActualMem,
	}
}
```

Then where the other `nodeSvc.Set*` calls are (after the cluster block), add:

```go
	nodeSvc.SetNodeLister(func() []apiserver.NodeRow {
		// Self row, live (capacity + current templates + drain/cordon state).
		lc, lm, ld := capt.Limits()
		ac, am, ad := capt.Snapshot()
		tmpls, _ := mgr.Backend().ListTemplates(context.Background())
		self := apiserver.NodeRow{
			NodeID: id.NodeID, NodeName: cfg.NodeName, Draining: nodeSvc.Draining(),
			Cordoned:     clusterInstance != nil && clusterInstance.LocalNodeState().Cordoned,
			Labels:       cfg.Labels,
			Capabilities: []string{"clone", "stats", "exec"},
			Workspaces:   workspaceNames(cfg.Workspaces),
			Templates:    tmpls,
			LimitCPU:     lc, LimitMemKB: lm, LimitDiskGB: ld,
			AllocCPU: ac, AllocMemKB: am, AllocDiskGB: ad,
		}
		rows := []apiserver.NodeRow{self}
		if clusterInstance != nil {
			for _, ns := range clusterInstance.PeerStates() {
				rows = append(rows, rowFromState(ns))
			}
		}
		return rows
	})
```

(Confirm `capt`, `mgr`, `id`, `cfg`, `clusterInstance`, `workspaceNames` are all in scope at that point — they are, per the existing `localNS`/`buildCandidates` wiring.)

- [ ] **Step 8: Build, vet, test**

Run: `go build ./... && go vet ./... && go test ./internal/apiserver/ ./internal/node/`
Expected: PASS.

- [ ] **Step 9: Commit**

```bash
git add proto internal/gen internal/apiserver/nodeservice.go internal/apiserver/nodeservice_test.go internal/apiserver/authz.go internal/node/node.go
git commit -m "feat(apiserver): ListNodes - self + gossiped peers topology endpoint"
```

---

### Task 3: `ListTemplates` — local node's rich template catalog

> The swarm-wide *union of names* is already free via `ListNodes` (Task 2). This adds the **local** node's rich metadata (repo/tag/id/agent/created), which requires a new additive backend method because the existing `Backend.ListTemplates()` is names-only (it feeds gossip). Cost: one interface method + Fake + SDK impl.

**Files:**
- Modify: `internal/sandbox/backend.go` (add `TemplateInfo`, `ListTemplateInfo` to `Backend`)
- Modify: `internal/sandbox/fake.go` (implement `ListTemplateInfo`)
- Modify: `internal/sandbox/sdkbackend.go` (implement `ListTemplateInfo`)
- Modify: `proto/sbxswarm/v1/node.proto` (TemplateInfo + rpc)
- Modify: `internal/apiserver/nodeservice.go` (`SetTemplateLister`, `ListTemplates`)
- Modify: `internal/apiserver/authz.go` (classify read)
- Modify: `internal/node/node.go` (wire template lister to `mgr.Backend()`)
- Test: `internal/sandbox/fake_test.go` (or existing fake test file) + `internal/apiserver/nodeservice_test.go` + integration in `internal/sandbox/sdkbackend_integration_test.go`

**Interfaces:**
- Produces: `sandbox.TemplateInfo{Repository, Tag, ID, Agent, CreatedAt string}`.
- Produces: `Backend.ListTemplateInfo(ctx) ([]TemplateInfo, error)`.
- Produces: `NodeService.SetTemplateLister(func(context.Context) ([]TemplateRow, error))`, `NodeService.ListTemplates(...)` where `TemplateRow` mirrors `TemplateInfo` (apiserver-local, no sandbox import needed since apiserver already imports sandbox — reuse `sandbox.TemplateInfo` directly).

- [ ] **Step 1: Add `TemplateInfo` + interface method (failing build)**

In `internal/sandbox/backend.go`, add the struct (near `CreateSpec`) and the interface method (in `Backend`, after `ListTemplates`):

```go
// TemplateInfo is a template image with operator-facing metadata.
type TemplateInfo struct {
	Repository string
	Tag        string
	ID         string
	Agent      string
	CreatedAt  string
}
```

```go
	// ListTemplateInfo returns the local daemon's templates with metadata.
	ListTemplateInfo(ctx context.Context) ([]TemplateInfo, error)
```

- [ ] **Step 2: Run build, watch Fake/SDKBackend fail to satisfy the interface**

Run: `go build ./internal/sandbox/`
Expected: FAIL — `*Fake` and `*SDKBackend` do not implement `Backend` (missing `ListTemplateInfo`).

- [ ] **Step 3: Implement on Fake**

In `internal/sandbox/fake.go`:

```go
// ListTemplateInfo returns a canned template so tests need no daemon.
func (b *Fake) ListTemplateInfo(_ context.Context) ([]TemplateInfo, error) {
	return []TemplateInfo{{Repository: "fake/base", Tag: "latest", ID: "img-fake", Agent: "shell"}}, nil
}
```

- [ ] **Step 4: Implement on SDKBackend**

In `internal/sandbox/sdkbackend.go` (the SDK's `template.Image` has `Agent, CreatedAt, ID, Repository, Tag`):

```go
// ListTemplateInfo returns the daemon's templates with metadata.
func (b *SDKBackend) ListTemplateInfo(ctx context.Context) ([]TemplateInfo, error) {
	imgs, err := sdktemplate.List(ctx, b.cl)
	if err != nil {
		return nil, err
	}
	out := make([]TemplateInfo, 0, len(imgs))
	for _, im := range imgs {
		out = append(out, TemplateInfo{
			Repository: im.Repository, Tag: im.Tag, ID: im.ID, Agent: im.Agent, CreatedAt: im.CreatedAt,
		})
	}
	return out, nil
}
```

- [ ] **Step 5: Build + a Fake unit test**

Run: `go build ./internal/sandbox/`
Then add to the fake test file (e.g. `internal/sandbox/fake_test.go`):

```go
func TestFake_ListTemplateInfo(t *testing.T) {
	got, err := NewFake().ListTemplateInfo(context.Background())
	require.NoError(t, err)
	require.NotEmpty(t, got)
	require.Equal(t, "fake/base", got[0].Repository)
}
```

Run: `go test ./internal/sandbox/ -run TestFake_ListTemplateInfo`
Expected: PASS.

- [ ] **Step 6: Add proto + regenerate + classify**

In `proto/sbxswarm/v1/node.proto`, add to the service:

```proto
  rpc ListTemplates(ListTemplatesRequest) returns (ListTemplatesResponse) {
    option (google.api.http) = {get: "/v1/templates"};
  }
```

messages:

```proto
message ListTemplatesRequest {}
message ListTemplatesResponse { repeated TemplateInfo templates = 1; }
message TemplateInfo {
  string repository = 1;
  string tag = 2;
  string id = 3;
  string agent = 4;
  string created_at = 5;
}
```

Run: `buf generate && go build ./...`
Add to `readMethods` in `authz.go`: `"/sbxswarm.v1.NodeService/ListTemplates": true,`

- [ ] **Step 7: NodeService.ListTemplates + setter**

In `internal/apiserver/nodeservice.go`, add a field `templateLister func(context.Context) ([]sandbox.TemplateInfo, error)` (add `"github.com/squall-chua/sbx-swarm-node/internal/sandbox"` import) and:

```go
// SetTemplateLister wires the local backend's template source (node.go).
func (s *NodeService) SetTemplateLister(fn func(context.Context) ([]sandbox.TemplateInfo, error)) {
	s.templateLister = fn
}

// ListTemplates returns the local node's templates with metadata.
func (s *NodeService) ListTemplates(ctx context.Context, _ *sbxv1.ListTemplatesRequest) (*sbxv1.ListTemplatesResponse, error) {
	out := &sbxv1.ListTemplatesResponse{}
	if s.templateLister == nil {
		return out, nil
	}
	infos, err := s.templateLister(ctx)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	for _, t := range infos {
		out.Templates = append(out.Templates, &sbxv1.TemplateInfo{
			Repository: t.Repository, Tag: t.Tag, Id: t.ID, Agent: t.Agent, CreatedAt: t.CreatedAt,
		})
	}
	return out, nil
}
```

- [ ] **Step 8: Failing service test**

In `internal/apiserver/nodeservice_test.go`:

```go
func TestNodeService_ListTemplates(t *testing.T) {
	svc := NewNodeService("n1", "node-one", "test")
	svc.SetTemplateLister(func(context.Context) ([]sandbox.TemplateInfo, error) {
		return []sandbox.TemplateInfo{{Repository: "r", Tag: "t", ID: "i"}}, nil
	})
	resp, err := svc.ListTemplates(context.Background(), &sbxv1.ListTemplatesRequest{})
	require.NoError(t, err)
	require.Len(t, resp.Templates, 1)
	require.Equal(t, "r", resp.Templates[0].Repository)
	require.Equal(t, "i", resp.Templates[0].Id)
}
```

- [ ] **Step 9: Wire in node.go**

Add next to `SetNodeLister`:

```go
	nodeSvc.SetTemplateLister(mgr.Backend().ListTemplateInfo)
```

- [ ] **Step 10: Build, test, integration**

Run: `go build ./... && go vet ./... && go test ./internal/sandbox/ ./internal/apiserver/`
Add an integration assertion in `internal/sandbox/sdkbackend_integration_test.go`:

```go
func TestSDKBackend_ListTemplateInfo(t *testing.T) {
	infos, err := dial(t, noWorkspaces).ListTemplateInfo(context.Background())
	require.NoError(t, err)
	require.NotEmpty(t, infos, "daemon should hold at least the shell-docker template")
	require.NotEmpty(t, infos[0].Repository)
}
```

Run (live daemon): `go test -tags integration ./internal/sandbox/ -run TestSDKBackend_ListTemplateInfo`
Expected: PASS.

- [ ] **Step 11: Commit**

```bash
git add proto internal/gen internal/sandbox internal/apiserver/nodeservice.go internal/apiserver/nodeservice_test.go internal/apiserver/authz.go internal/node/node.go
git commit -m "feat: ListTemplates - local node's rich template catalog"
```

---

### Task 4: Cross-node cordon / drain (forward-to-peer, ADR-0018)

**Files:**
- Modify: `proto/sbxswarm/v1/node.proto` (`node_id` on Cordon/Drain requests)
- Modify: `internal/apiserver/forward.go` (route node-targeted RPCs by `node_id`)
- Modify: `internal/node/node.go` (`NewForwarder` gets self id)
- Test: `internal/apiserver/forward_test.go` (add node-routing test)

**Interfaces:**
- Consumes: `routing.Table.Addr(nodeID)`, `peer.Pool.Conn(addr, nodeID)` (existing).
- Produces: `NewForwarder(tbl, pool, self string)` (signature change) + node-id routing for `Cordon`/`Uncordon`/`Drain`.

- [ ] **Step 1: Add `node_id` to proto + regenerate**

In `proto/sbxswarm/v1/node.proto`:

```proto
message CordonRequest { string node_id = 1; } // empty = self
message DrainRequest { string node_id = 1; }  // empty = self
```

Run: `buf generate && go build ./...`
(No NodeService handler change: an empty `node_id` keeps today's self behavior; non-empty is handled by the forwarder before the handler runs.)

- [ ] **Step 2: Write the failing forwarder test**

In `internal/apiserver/forward_test.go` (model on the existing forwarder tests there — they already stand up a `routing.Table` and a `peer.Pool`/stub). Assert that a `CordonRequest{NodeId: <peer>}` is routed to that peer's address, and an empty id is handled locally:

```go
func TestForwarder_RoutesCordonByNodeID(t *testing.T) {
	tbl := routing.NewTable("self")
	tbl.Upsert("peer2", "127.0.0.1:65501", false, nil) // unreachable addr; we assert dial is attempted

	fwd := NewForwarder(tbl, peer.NewPool(nil), "self")
	interceptor := fwd.UnaryInterceptor()

	called := false
	handler := func(ctx context.Context, req any) (any, error) { called = true; return &sbxv1.NodeInfo{}, nil }

	// Empty node_id -> handled locally.
	_, err := interceptor(context.Background(), &sbxv1.CordonRequest{}, &grpc.UnaryServerInfo{FullMethod: "/sbxswarm.v1.NodeService/Cordon"}, handler)
	require.NoError(t, err)
	require.True(t, called, "empty node_id must run locally")

	// Peer node_id -> forwarded (dial fails to the bogus addr => error, NOT local handling).
	called = false
	_, err = interceptor(context.Background(), &sbxv1.CordonRequest{NodeId: "peer2"}, &grpc.UnaryServerInfo{FullMethod: "/sbxswarm.v1.NodeService/Cordon"}, handler)
	require.Error(t, err, "forwarding to an unreachable peer should error")
	require.False(t, called, "peer-targeted cordon must not run the local handler")
}
```

(Adjust `routing.NewTable`/`peer.NewPool` constructors to match the real signatures in those packages — grep them; the existing forwarder tests show the exact calls.)

- [ ] **Step 3: Run it, watch it fail**

Run: `go test ./internal/apiserver/ -run TestForwarder_RoutesCordonByNodeID`
Expected: FAIL — `NewForwarder` takes 2 args / node routing not implemented.

- [ ] **Step 4: Implement node routing in forward.go**

Add `self` to the struct and constructor, a node-id extractor, and a branch in `UnaryInterceptor` before the sandbox-id branch:

```go
type Forwarder struct {
	tbl  *routing.Table
	pool *peer.Pool
	self string
}

func NewForwarder(tbl *routing.Table, pool *peer.Pool, self string) *Forwarder {
	return &Forwarder{tbl: tbl, pool: pool, self: self}
}
```

In `UnaryInterceptor`, at the top of the returned func (before `routableID`):

```go
		if nodeID, ok := routableNode(req); ok && nodeID != f.self {
			addr, ok := f.tbl.Addr(nodeID)
			if !ok {
				return handler(ctx, req) // unknown node: let local handler answer
			}
			conn, err := f.pool.Conn(addr, nodeID)
			if err != nil {
				return nil, err
			}
			out := newReplyFor(info.FullMethod)
			if out == nil {
				return handler(ctx, req)
			}
			if md, ok := metadata.FromIncomingContext(ctx); ok {
				ctx = metadata.NewOutgoingContext(ctx, md)
			}
			if err := conn.Invoke(ctx, info.FullMethod, req, out); err != nil {
				return nil, err
			}
			return out, nil
		}
```

Add the extractor and the node reply types:

```go
type nodeIDExtractor interface{ GetNodeId() string }

// routableNode pulls a target node id from a node-control request (Cordon/Drain).
func routableNode(req any) (string, bool) {
	e, ok := req.(nodeIDExtractor)
	if !ok {
		return "", false
	}
	id := e.GetNodeId()
	if id == "" {
		return "", false
	}
	return id, true
}
```

Extend `newReplyFor` with the node methods:

```go
	case "/sbxswarm.v1.NodeService/Cordon",
		"/sbxswarm.v1.NodeService/Uncordon",
		"/sbxswarm.v1.NodeService/Drain":
		return new(sbxv1.NodeInfo)
```

> Caveat to note in the commit: `RevokeNodeRequest` and `GetNodeInfoRequest` also have/await a `node_id`-shaped getter? No — only `CordonRequest`/`DrainRequest` get `node_id` here, and `routableNode` only matches a non-empty `GetNodeId()`, so `RevokeNode` (`GetNodeId` on `RevokeNodeRequest`) would also match. Guard by method: only apply node routing when `info.FullMethod` is one of the three cordon/drain methods. Wrap the new branch in:
> `if isNodeControlMethod(info.FullMethod) { ... }` where
> `func isNodeControlMethod(m string) bool { return m == ".../Cordon" || m == ".../Uncordon" || m == ".../Drain" }`
> (full method strings). This prevents RevokeNode from being misrouted.

- [ ] **Step 5: Update `NewForwarder` call in node.go**

`internal/node/node.go` line ~177: `fwd := apiserver.NewForwarder(tbl, pool, id.NodeID)`.

- [ ] **Step 6: Run tests + build**

Run: `go build ./... && go vet ./... && go test ./internal/apiserver/ -run 'TestForwarder|TestAuthz_AllMethodsClassified'`
Expected: PASS. (Cordon/Uncordon/Drain are already in `mutatingMethods` — no authz change.)

- [ ] **Step 7: Commit**

```bash
git add proto internal/gen internal/apiserver/forward.go internal/apiserver/forward_test.go internal/node/node.go
git commit -m "feat(apiserver): cross-node cordon/drain forwards by node_id (ADR-0018)"
```

---

### Task 5: Terminal WebSocket (ADR-0017)

**Files:**
- Modify: `go.mod` / `go.sum` (`github.com/coder/websocket`)
- Modify: `internal/sandbox/backend.go` (`Session` interface + `ExecInteractive` on `Backend`)
- Modify: `internal/sandbox/fake.go` (echo `Session`)
- Modify: `internal/sandbox/sdkbackend.go` (`ExecInteractive` via SDK attach)
- Create: `internal/apiserver/terminal.go` (handler + bridge + path mux)
- Modify: `internal/apiserver/server.go` (compose terminal mux into `v1` before the gateway)
- Test: `internal/sandbox/fake_test.go`, `internal/apiserver/terminal_test.go`, integration in `internal/sandbox/sdkbackend_integration_test.go`

**Interfaces:**
- Produces: `sandbox.Session` interface: `Stdin() io.Writer`, `Stdout() io.Reader`, `Resize(ctx, cols, rows int) error`, `Wait(ctx) (int, error)`, `Close() error`. (`*sdkexec.AttachSession` satisfies this structurally.)
- Produces: `Backend.ExecInteractive(ctx, name string, cmd []string, tty bool) (Session, error)`.
- Produces: `(s *SandboxService) TerminalHandler() http.Handler`; `apiserver.terminalMux(next http.Handler, term http.Handler) http.Handler`.

- [ ] **Step 1: Add the dependency**

Run: `go get github.com/coder/websocket@latest && go mod tidy`

- [ ] **Step 2: Add `Session` + `ExecInteractive` to the interface (failing build)**

In `internal/sandbox/backend.go` (add `"io"` import if absent):

```go
// Session is a live interactive exec attached to a running sandbox (Terminal
// session). *sdkexec.AttachSession satisfies it structurally.
type Session interface {
	Stdin() io.Writer
	Stdout() io.Reader
	Resize(ctx context.Context, cols, rows int) error
	Wait(ctx context.Context) (int, error)
	Close() error
}
```

Add to `Backend`:

```go
	// ExecInteractive opens a Terminal session (TTY when tty=true).
	ExecInteractive(ctx context.Context, name string, cmd []string, tty bool) (Session, error)
```

Run: `go build ./internal/sandbox/` → FAIL (Fake, SDKBackend missing the method).

- [ ] **Step 3: Implement Fake echo session**

In `internal/sandbox/fake.go` (add `"io"`, `"sync"` if needed):

```go
type fakeSession struct {
	r      *io.PipeReader
	w      *io.PipeWriter
	closed chan struct{}
	once   sync.Once
}

func (s *fakeSession) Stdin() io.Writer                          { return s.w } // echo: Stdin -> Stdout
func (s *fakeSession) Stdout() io.Reader                         { return s.r }
func (s *fakeSession) Resize(context.Context, int, int) error    { return nil }
func (s *fakeSession) Wait(ctx context.Context) (int, error) {
	select {
	case <-s.closed:
		return 0, nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}
func (s *fakeSession) Close() error {
	s.once.Do(func() { close(s.closed); _ = s.w.Close(); _ = s.r.Close() })
	return nil
}

// ExecInteractive returns an echo session (bytes written to Stdin appear on Stdout).
func (b *Fake) ExecInteractive(_ context.Context, _ string, _ []string, _ bool) (Session, error) {
	pr, pw := io.Pipe()
	return &fakeSession{r: pr, w: pw, closed: make(chan struct{})}, nil
}
```

- [ ] **Step 4: Implement SDKBackend.ExecInteractive**

In `internal/sandbox/sdkbackend.go`:

```go
// ExecInteractive opens a Terminal session via the SDK's hijacking attach.
func (b *SDKBackend) ExecInteractive(ctx context.Context, name string, cmd []string, tty bool) (Session, error) {
	sb, err := b.handle(ctx, name)
	if err != nil {
		return nil, err
	}
	popts := []sdkexec.ProcessOption{sdkexec.WithAutoStart()}
	if tty {
		popts = append(popts, sdkexec.WithTTY())
	}
	return sdkexec.ExecInteractive(ctx, sb, cmd, popts...) // *AttachSession satisfies Session
}
```

Run: `go build ./internal/sandbox/` → PASS.

- [ ] **Step 5: Fake session unit test**

```go
func TestFake_ExecInteractiveEchoes(t *testing.T) {
	sess, err := NewFake().ExecInteractive(context.Background(), "sb", []string{"/bin/sh"}, true)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sess.Close() })

	go func() { _, _ = sess.Stdin().Write([]byte("ping")) }()
	buf := make([]byte, 4)
	_, err = io.ReadFull(sess.Stdout(), buf)
	require.NoError(t, err)
	require.Equal(t, "ping", string(buf))
}
```

Run: `go test ./internal/sandbox/ -run TestFake_ExecInteractiveEchoes` → PASS.

- [ ] **Step 6: Write the terminal handler + path mux**

Create `internal/apiserver/terminal.go`:

```go
package apiserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/coder/websocket"
	"github.com/squall-chua/sbx-swarm-node/internal/sandbox"
)

// terminalMux intercepts /v1/sandboxes/{id}/terminal and serves the WebSocket;
// all other requests fall through to next (the gateway). It sits inside
// OwnerProxy, so a remote sandbox's upgrade is already proxied to its owner.
func terminalMux(term, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id, ok := terminalSandboxID(r.URL.Path); ok && id != "" {
			term.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// terminalSandboxID returns the {id} from /v1/sandboxes/{id}/terminal.
func terminalSandboxID(p string) (string, bool) {
	const pre = "/v1/sandboxes/"
	if !strings.HasPrefix(p, pre) || !strings.HasSuffix(p, "/terminal") {
		return "", false
	}
	id := strings.TrimSuffix(strings.TrimPrefix(p, pre), "/terminal")
	if id == "" || strings.Contains(id, "/") {
		return "", false
	}
	return id, true
}

// TerminalHandler upgrades to a WebSocket and bridges it to a Terminal session.
// Auth is enforced upstream by the cookie/bearer middleware; websocket.Accept
// enforces the same-origin Origin check by default (ADR-0017).
func (s *SandboxService) TerminalHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := terminalSandboxID(r.URL.Path)
		if !ok {
			http.NotFound(w, r)
			return
		}
		name, err := s.mgr.Resolve(r.Context(), id)
		if err != nil {
			http.Error(w, "sandbox not found", http.StatusNotFound)
			return
		}
		c, err := websocket.Accept(w, r, nil) // nil opts => same-origin enforced (ADR-0017)
		if err != nil {
			return // Accept already wrote the response (e.g. 403 on bad Origin)
		}
		defer c.CloseNow()

		_ = s.mgr.BumpActivity(r.Context(), id) // a Terminal session is Activity
		sess, err := s.mgr.Backend().ExecInteractive(r.Context(), name, []string{"/bin/sh"}, true)
		if err != nil {
			_ = c.Close(websocket.StatusInternalError, "exec failed")
			return
		}
		defer sess.Close()
		bridgeTerminal(r.Context(), c, sess)
	})
}

type resizeMsg struct {
	Type string `json:"type"`
	Cols int    `json:"cols"`
	Rows int    `json:"rows"`
}

// bridgeTerminal copies sess.Stdout -> ws (binary) and ws -> sess.Stdin, parsing
// text control frames as resize requests. Returns when either side ends.
func bridgeTerminal(ctx context.Context, c *websocket.Conn, sess sandbox.Session) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// session stdout -> websocket
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := sess.Stdout().Read(buf)
			if n > 0 {
				if werr := c.Write(ctx, websocket.MessageBinary, buf[:n]); werr != nil {
					cancel()
					return
				}
			}
			if err != nil {
				cancel()
				return
			}
		}
	}()

	// session exit -> close
	go func() {
		_, _ = sess.Wait(ctx)
		_ = c.Close(websocket.StatusNormalClosure, "session ended")
		cancel()
	}()

	// websocket -> session stdin (and resize control frames)
	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		if typ == websocket.MessageText {
			var m resizeMsg
			if json.Unmarshal(data, &m) == nil && m.Type == "resize" {
				_ = sess.Resize(ctx, m.Cols, m.Rows)
			}
			continue
		}
		if _, werr := sess.Stdin().Write(data); werr != nil {
			return
		}
	}
}

var _ = errors.Is // keep errors import if unused after edits; remove if vet complains
```

> Remove the trailing `var _ = errors.Is` and the `errors` import if `go vet` flags them unused — it's a guard for editors, not required.

- [ ] **Step 7: Compose into the server before the gateway**

In `internal/apiserver/server.go`, in `Build`, after the `observeStreamMux` line and **before** the `OwnerProxy` wrap:

```go
	if opts.Sandboxes != nil {
		v1 = terminalMux(opts.Sandboxes.TerminalHandler(), v1)
	}
```

(Order matters: terminal must be inside OwnerProxy so remote terminals forward, and before the gateway so the gateway never sees the WS path. `mw.Authenticate` still wraps the whole `/v1/`, so the upgrade is authed.)

- [ ] **Step 8: Write the handler test (Fake-backed, real WS client)**

Create `internal/apiserver/terminal_test.go`:

```go
package apiserver

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"
)

func TestTerminalHandler_EchoesOverWebSocket(t *testing.T) {
	svc := newSandboxSvc(t) // existing harness; Fake backend
	// Create a sandbox so Resolve succeeds.
	rec := mustCreateSandbox(t, svc) // helper in this package's tests; or create via svc + poll
	srv := httptest.NewServer(terminalMux(svc.TerminalHandler(), http.NotFoundHandler()))
	t.Cleanup(srv.Close)

	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/v1/sandboxes/" + rec.ID + "/terminal"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, url, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close(websocket.StatusNormalClosure, "") })

	require.NoError(t, c.Write(ctx, websocket.MessageBinary, []byte("hello")))
	typ, data, err := c.Read(ctx)
	require.NoError(t, err)
	require.Equal(t, websocket.MessageBinary, typ)
	require.Equal(t, "hello", string(data))
}
```

(Use the sandbox-creation helper the apiserver tests already provide; if none returns a record id, create via `svc.CreateSandbox` then read the id from `svc.mgr.List`. `websocket.Dial` to a `127.0.0.1` httptest server passes the default same-origin check because there is no browser `Origin` header — confirming the handler accepts non-browser clients.)

- [ ] **Step 9: Run it (fail → pass), build, vet**

Run: `go test ./internal/apiserver/ -run TestTerminalHandler_EchoesOverWebSocket && go build ./... && go vet ./...`
Expected: PASS.

- [ ] **Step 10: Integration test against the live daemon**

In `internal/sandbox/sdkbackend_integration_test.go`:

```go
func TestSDKBackend_ExecInteractive(t *testing.T) {
	ctx := context.Background()
	b, ws := backendWS(t)
	sb := mkSandbox(t, b, CreateSpec{Name: "it-terminal", CPUs: 1, MemoryBytes: 1 << 30, Workspaces: ws})

	sess, err := b.ExecInteractive(ctx, sb.Name, []string{"/bin/sh"}, true)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sess.Close() })

	_, err = sess.Stdin().Write([]byte("echo it-terminal-ok; exit\n"))
	require.NoError(t, err)

	out := make([]byte, 0, 256)
	buf := make([]byte, 256)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		n, rerr := sess.Stdout().Read(buf)
		out = append(out, buf[:n]...)
		if strings.Contains(string(out), "it-terminal-ok") {
			return
		}
		if rerr != nil {
			break
		}
	}
	t.Fatalf("did not see terminal echo; got: %q", out)
}
```

Run (live daemon): `go test -tags integration ./internal/sandbox/ -run TestSDKBackend_ExecInteractive`
Expected: PASS.

- [ ] **Step 11: Commit**

```bash
git add go.mod go.sum internal/sandbox internal/apiserver/terminal.go internal/apiserver/terminal_test.go internal/apiserver/server.go
git commit -m "feat(apiserver): interactive Terminal session over WebSocket (ADR-0017)"
```

---

## Self-Review

**Spec coverage** (against `docs/superpowers/specs/2026-06-23-m8a-backend-api-design.md`):
- §1 `ListNodes` → Task 2 (drops `reachable`; cordoned-all; draining/name self-only). ✓
- §2 `ListTemplates` → Task 3 (local rich; union-of-names already via Task 2). ✓
- §3 `ListOperations` → Task 1 (durable bucket; newest-first; limit). ✓
- §4 cross-node cordon/drain → Task 4 (`node_id`; forward-to-peer; ADR-0018; method-guarded so RevokeNode isn't misrouted). ✓
- §5 terminal WS → Task 5 (native handler inside OwnerProxy + before gateway; `ExecInteractive` + Fake echo + SDK attach; coder/websocket Accept = Origin check per ADR-0017; resize control frame). ✓
- Cross-cutting: every gRPC method classified (Tasks 1–3 add reads; cordon/drain already mutating); `buf generate` flow per task; TDD; env-gated integration for templates + terminal. ✓

**Placeholder scan:** The only deliberate placeholders are flagged inline with the exact fix: the Task 2 test's `LimitCpuFields(...)` is immediately replaced with the literal final body; the Task 5 test's `mustCreateSandbox` defers to the existing apiserver sandbox-creation helper. The `var _ = errors.Is` guard is called out for removal. No `TODO`/`TBD` remain.

**Type consistency:** `NodeRow` fields (Go casing `LimitCPU`) map to generated proto getters (`LimitCpu`); `TemplateInfo` is the single source struct reused across backend + apiserver; `Session` interface methods match `*sdkexec.AttachSession` exactly (verified: `Stdin() io.Writer`, `Stdout() io.Reader`, `Resize(ctx,cols,rows)`, `Wait(ctx)(int,error)`, `Close()`); `NewForwarder` signature change is propagated to its sole caller in `node.go`.

**Open confirmations for the implementer** (cheap, code-checkable, not blockers): exact name of the apiserver sandbox-test harness (`newSandboxSvc`) and the ops field (`s.ops`); `routing.NewTable`/`peer.NewPool` constructor signatures in Task 4's test; generated Go getter casing after each `buf generate`.
