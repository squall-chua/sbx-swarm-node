# Template Lifecycle Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let an operator save a warmed sandbox as a template, delete one, and use
a registry-hosted template anywhere in the swarm.

**Architecture:** Placement learns that a registry-shaped reference travels, so
`fits` stops filtering on it and holding the image becomes a tie-break. Two new
admin RPCs — `SaveTemplate` on `SandboxService`, `RemoveTemplate` on
`NodeService` — sit over SDK calls the node already links. The advertised
template list moves from a boot-time snapshot to a refresh on the existing
ten-second ticker, which is what candidate item 1 asked for.

**Tech Stack:** Go 1.x, buf v1.66 for protobuf codegen, grpc-gateway for REST,
memberlist via `internal/membership`, Nuxt 4 with @nuxt/ui v4 and Vitest for the
console.

**Spec:** `docs/superpowers/specs/2026-07-29-template-lifecycle-design.md`
**ADR:** `docs/adr/0024-a-template-travels-as-a-registry-reference.md` (committed)

## Global Constraints

- **Rebase first.** This plan was written against local `main` @ `1de5541`,
  which is 6 commits behind `origin/main`. Start from `origin/main` and re-check
  every line number below; they were read before that merge.
- **Do not build any transfer of image bytes between nodes.** ADR-0024 rejects
  it with reasons. If a task looks like it needs one, stop and ask.
- The repo is gofmt-dirty but does not enforce it. Format only files you touch.
- `go vet ./...` does not apply the `integration` tag. Also run
  `go vet -tags integration ./internal/...`.
- `TestNode_Gerrit_Publish` is red unless the local Gerrit stack is running (see
  `dev/gerrit/README.md`). Environmental, not yours.
- Codegen is `buf generate` with **remote** plugins (`buf.gen.yaml`), so Task 2
  needs network. `buf` 1.66.0 is on the path.
- Every new RPC must be classified in `internal/apiserver/authz.go`. The drift
  guard `TestAuthz_AllMethodsClassified` fails otherwise, and an unclassified
  method fails closed as mutating.
- Plain, short English in comments and commit messages. One idea per sentence.
- The console test command runs from `web/`. Vitest 4 is in use; mock factories
  that are constructed with `new` need care.

## File Structure

| File | Task | Responsibility |
|---|---|---|
| `internal/scheduler/scheduler.go` | 1 | `pullable` helper, the `fits` rule, the holder tie-break |
| `internal/scheduler/scheduler_test.go` | 1 | Reference shapes, placement, tie-break |
| `docs/adr/0007-*.md` | 1 | One line: the holder tie-break precedes the local one |
| `proto/sbxswarm/v1/sandbox.proto` | 2 | `SaveTemplate` |
| `proto/sbxswarm/v1/node.proto` | 2 | `RemoveTemplate` |
| `internal/gen/sbxswarm/v1/*` | 2 | Generated; never hand-edited |
| `internal/apiserver/authz.go` | 2 | Classify both as mutating |
| `internal/apiserver/forward.go` | 2 | Route both to the owning node |
| `internal/apiserver/forward_test.go` | 2 | The node-control guard still holds |
| `internal/sandbox/backend.go` | 3 | Two interface methods |
| `internal/sandbox/fake.go` | 3 | Fake implementations plus recorders |
| `internal/sandbox/sdkbackend.go` | 3 | SDK implementations |
| `internal/sandbox/fake_test.go` | 3 | Fake behaviour |
| `internal/apiserver/sandboxservice.go` | 4 | `SaveTemplate` handler |
| `internal/apiserver/sandboxservice_test.go` | 4 | Stopped-only rule |
| `internal/apiserver/nodeservice.go` | 5 | `RemoveTemplate` handler |
| `internal/apiserver/nodeservice_test.go` | 5 | Handler behaviour |
| `internal/membership/cluster.go` | 6 | `UpdateLocalTemplates` |
| `internal/membership/cluster_test.go` | 6 | It bumps `StateVersion` |
| `internal/node/node.go` | 6 | Refresh templates on the ticker |
| `web/app/components/drawer/InfoTab.vue` | 7 | Save-as-template control |
| `web/tests/drawer-info.spec.ts` | 7 | The control's gating and call |
| `internal/sandbox/sdkbackend_integration_test.go` | 8 | Live save and remove |
| `CONTEXT.md`, `README.md` | 8 | Term and operator guidance |

---

### Task 1: Placement understands a reference that travels

**Files:**
- Modify: `internal/scheduler/scheduler.go` — `fits` at line 86, the sort at
  lines 61-77
- Modify: `docs/adr/0007-*.md` — one line about the tie-break order
- Test: `internal/scheduler/scheduler_test.go`

**Interfaces:**
- Produces, used by nothing else: `func pullable(ref string) bool` (unexported;
  the rule stays inside the scheduler)

This task ships value on its own and touches no proto.

- [ ] **Step 1: Write the failing tests**

Add to `internal/scheduler/scheduler_test.go`. Read the file first and reuse its
existing candidate helpers if it has any.

```go
func TestPullable(t *testing.T) {
	cases := map[string]bool{
		"ghcr.io/org/img:1":     true,  // registry host: has a dot
		"localhost:5000/img:1":  true,  // registry host: localhost
		"registry:5000/img:1":   true,  // registry host: has a colon
		"myimage:v1":            false, // bare tag: only where it was saved
		"org/img:1":             false, // Docker Hub shorthand, deliberately bare
		"alpine":                false,
	}
	for ref, want := range cases {
		require.Equal(t, want, pullable(ref), ref)
	}
}

func TestSchedule_RegistryTemplatePlacesOnANodeWithoutIt(t *testing.T) {
	c := Candidate{NodeID: "n1", LimitCPU: 8, LimitMem: 8 << 20, LimitDisk: 100}
	// c.Templates is empty: this node holds nothing.
	got, err := Schedule(Request{Template: "ghcr.io/org/img:1", CPU: 1}, []Candidate{c})
	require.NoError(t, err)
	require.Equal(t, []string{"n1"}, got)
}

func TestSchedule_BareTemplateStillFiltered(t *testing.T) {
	c := Candidate{NodeID: "n1", LimitCPU: 8, LimitMem: 8 << 20, LimitDisk: 100}
	_, err := Schedule(Request{Template: "myimage:v1", CPU: 1}, []Candidate{c})
	require.ErrorIs(t, err, ErrNoEligibleNode)
}

func TestSchedule_HolderBeatsTheEntryNodeOnATie(t *testing.T) {
	// Two identical, unloaded nodes. n2 holds the image; n1 is the entry node.
	mk := func(id string, holds bool) Candidate {
		c := Candidate{NodeID: id, LimitCPU: 8, LimitMem: 8 << 20, LimitDisk: 100}
		if holds {
			c.Templates = map[string]bool{"ghcr.io/org/img:1": true}
		}
		return c
	}
	req := Request{Template: "ghcr.io/org/img:1", CPU: 1, Local: "n1"}
	got, err := Schedule(req, []Candidate{mk("n1", false), mk("n2", true)})
	require.NoError(t, err)
	require.Equal(t, "n2", got[0], "a node holding the image must win over the entry node")
}
```

Copy the resource field names and units from the existing tests in that file
rather than trusting the values above; `LimitMem` is in KB elsewhere in the
codebase.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/scheduler/ -run 'TestPullable|TestSchedule_' -v`

Expected: FAIL to compile, `undefined: pullable`.

- [ ] **Step 3: Add the helper and the rule**

In `internal/scheduler/scheduler.go`, add `"strings"` to the imports and:

```go
// pullable reports whether a template reference names a registry, so any node's
// daemon can fetch it. This is Docker's own rule: the first path component is a
// registry host when it contains a "." or a ":", or is exactly "localhost".
//
// "org/img:1" is deliberately NOT pullable here. Docker reads it as a Docker Hub
// image, but it is ambiguous with a locally saved two-part tag, so the rule errs
// toward refusing placement instead of assuming a pull (ADR-0024). Write
// "docker.io/org/img:1" to make it travel.
func pullable(ref string) bool {
	i := strings.Index(ref, "/")
	if i < 0 {
		return false
	}
	host := ref[:i]
	return host == "localhost" || strings.ContainsAny(host, ".:")
}
```

Then change the template constraint in `fits` (line 95):

```go
	if req.Template != "" && !pullable(req.Template) && !c.Templates[req.Template] {
		return false
	}
```

- [ ] **Step 4: Add the tie-break**

In `Schedule`'s sort, immediately after the score comparison and **before** the
`req.Local` comparison (line 70):

```go
		// A node that already holds the image beats one that would have to pull
		// it. Only reached on an exact score tie, so real load still wins first.
		if req.Template != "" {
			if hi, hj := ok[i].Templates[req.Template], ok[j].Templates[req.Template]; hi != hj {
				return hi
			}
		}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/scheduler/ -v`

Expected: PASS, including the tests that were already there.

- [ ] **Step 6: Amend ADR-0007**

Find the ADR that fixes the tie-break order (`docs/adr/0007-*.md`) and add one
sentence to it: on a score tie, a node that already holds the requested template
is preferred before the entry node, because avoiding a multi-gigabyte pull is
worth more than staying local, and both only apply when the score is level.

Do not rewrite the rest of it.

- [ ] **Step 7: Verify and commit**

```bash
go build ./... && go vet ./... && go test ./internal/scheduler/ ./internal/coordinator/
git add internal/scheduler/scheduler.go internal/scheduler/scheduler_test.go docs/adr/0007-*.md
git commit -m "feat(scheduler): let a registry template place on any node

The daemon pulls a remote image reference, so a node does not need to
hold one already. A bare tag keeps the strict filter, because it only
exists where it was saved.

On a score tie, a node that holds the image now wins before the entry
node (ADR-0024, amends ADR-0007)."
```

---

### Task 2: The two RPCs exist and route

**Files:**
- Modify: `proto/sbxswarm/v1/sandbox.proto` — after `UnpublishPort` (line 44)
- Modify: `proto/sbxswarm/v1/node.proto` — after `ListTemplates` (line 35)
- Modify: `internal/apiserver/authz.go` — `mutatingMethods` (line 23)
- Modify: `internal/apiserver/forward.go` — `isNodeControlMethod` (line 36),
  `newReplyFor` (line 129)
- Test: `internal/apiserver/forward_test.go`, and the existing authz drift guard

**Interfaces:**
- Produces, used by Tasks 4, 5 and 7:
  - `sbxv1.SaveTemplateRequest{Id string, Tag string}` → returns `sbxv1.Empty`
  - `sbxv1.RemoveTemplateRequest{NodeId string, Ref string}` → returns
    `sbxv1.ListTemplatesResponse`
  - REST: `POST /v1/sandboxes/{id}/template` and `POST /v1/templates/remove`

The handlers are **not** written here. After this task both RPCs answer
`Unimplemented`, which is what the generated `UnimplementedXServer` gives us.
The deliverable is the API surface: generated, classified, and routed.

- [ ] **Step 1: Add the sandbox RPC**

In `proto/sbxswarm/v1/sandbox.proto`, inside `service SandboxService`, after
`UnpublishPort`:

```proto
  rpc SaveTemplate(SaveTemplateRequest) returns (Empty) {
    option (google.api.http) = {post: "/v1/sandboxes/{id}/template" body: "*"};
  }
```

And with the other messages, beside `UnpublishPortRequest` (line 237):

```proto
// SaveTemplateRequest snapshots a STOPPED sandbox as a reusable template image.
message SaveTemplateRequest {
  string id = 1;
  string tag = 2; // e.g. "myimage:v1" (local) or "ghcr.io/org/img:1"
}
```

`Empty` is already imported into this file from `policy.proto`.

- [ ] **Step 2: Add the node RPC**

In `proto/sbxswarm/v1/node.proto`, inside `service NodeService`, after
`ListTemplates`:

```proto
  rpc RemoveTemplate(RemoveTemplateRequest) returns (ListTemplatesResponse) {
    option (google.api.http) = {post: "/v1/templates/remove" body: "*"};
  }
```

And beside `ListTemplatesRequest` (line 72):

```proto
// RemoveTemplateRequest deletes one template image from a node's image store.
message RemoveTemplateRequest {
  string node_id = 1; // empty = self
  string ref = 2;     // repository:tag, as listed by ListTemplates
}
```

It returns `ListTemplatesResponse` on purpose: the caller gets the remaining
templates, and no new message type is needed.

- [ ] **Step 3: Generate**

```bash
buf generate
git status --short internal/gen/
```

Expected: `internal/gen/sbxswarm/v1/*.pb.go`, `*_grpc.pb.go` and `*.pb.gw.go`
modified. Never hand-edit those files. If `buf generate` cannot reach its remote
plugins, stop and say so — do not hand-write generated code.

- [ ] **Step 4: Classify both methods**

In `internal/apiserver/authz.go`, add to `mutatingMethods`:

```go
	"/sbxswarm.v1.SandboxService/SaveTemplate":   true,
	"/sbxswarm.v1.NodeService/RemoveTemplate":    true,
```

Run: `go test ./internal/apiserver/ -run TestAuthz -v`

Expected: PASS. Before this step the drift guard fails, which is the point of it.

- [ ] **Step 5: Route both to the owning node**

`SaveTemplate` carries a sandbox id, so the existing sandbox-id forwarding picks
it up automatically — **but only if `newReplyFor` knows the method**. Without
that entry the interceptor falls through to the local handler, which would answer
about a sandbox it does not own. In `newReplyFor` (line 129):

```go
	case "/sbxswarm.v1.SandboxService/SaveTemplate":
		return new(sbxv1.Empty)
```

`RemoveTemplate` carries a node id, so it joins the node-control list. In
`isNodeControlMethod` (line 36):

```go
	return m == "/sbxswarm.v1.NodeService/Cordon" ||
		m == "/sbxswarm.v1.NodeService/Uncordon" ||
		m == "/sbxswarm.v1.NodeService/Drain" ||
		m == "/sbxswarm.v1.NodeService/RemoveTemplate"
```

and in `newReplyFor`, beside the other node-control replies:

```go
	case "/sbxswarm.v1.NodeService/RemoveTemplate":
		return new(sbxv1.ListTemplatesResponse)
```

The comment above `isNodeControlMethod` explains that the guard exists to stop
`RevokeNode` being misrouted. Update it to say the list is now four methods, and
leave `RevokeNode` out.

- [ ] **Step 6: Test the routing**

Add to `internal/apiserver/forward_test.go`, matching how the existing
node-control tests build a `Forwarder`:

```go
func TestForward_RemoveTemplateIsNodeControl(t *testing.T) {
	require.True(t, isNodeControlMethod("/sbxswarm.v1.NodeService/RemoveTemplate"))
	require.False(t, isNodeControlMethod("/sbxswarm.v1.NodeService/RevokeNode"))
}

func TestForward_SaveTemplateHasAReply(t *testing.T) {
	require.NotNil(t, newReplyFor("/sbxswarm.v1.SandboxService/SaveTemplate"))
	require.NotNil(t, newReplyFor("/sbxswarm.v1.NodeService/RemoveTemplate"))
}
```

- [ ] **Step 7: Verify and commit**

```bash
go build ./... && go vet ./... && go test ./internal/apiserver/
git add proto/ internal/gen/ internal/apiserver/authz.go internal/apiserver/forward.go internal/apiserver/forward_test.go
git commit -m "feat(proto): add SaveTemplate and RemoveTemplate

Both are admin-only mutations. SaveTemplate rides the existing sandbox-id
forwarding; RemoveTemplate joins the node-control list beside Cordon.

Handlers come next: both answer Unimplemented for now."
```

---

### Task 3: The backend can save and remove a template

**Files:**
- Modify: `internal/sandbox/backend.go` — the `Backend` interface, beside
  `ListTemplates` (line 178)
- Modify: `internal/sandbox/fake.go` — beside `ListTemplates` (line 254)
- Modify: `internal/sandbox/sdkbackend.go` — beside `ListTemplates` (line 726)
- Test: `internal/sandbox/fake_test.go`

**Interfaces:**
- Produces, used by Tasks 4 and 5:
  - `SaveTemplate(ctx context.Context, name, tag string) error`
  - `RemoveTemplate(ctx context.Context, ref string) error`
  - On the fake: `(*Fake).SavedTemplates() []string` returning `"name=>tag"`
    pairs in call order, for handler tests.

- [ ] **Step 1: Write the failing test**

Add to `internal/sandbox/fake_test.go`:

```go
func TestFake_SaveAndRemoveTemplate(t *testing.T) {
	f := NewFake()
	require.NoError(t, f.SaveTemplate(context.Background(), "sb-1", "myimage:v1"))
	require.Equal(t, []string{"sb-1=>myimage:v1"}, f.SavedTemplates())

	// A saved template is listed, then removable.
	got, err := f.ListTemplates(context.Background())
	require.NoError(t, err)
	require.Contains(t, got, "myimage:v1")

	require.NoError(t, f.RemoveTemplate(context.Background(), "myimage:v1"))
	got, err = f.ListTemplates(context.Background())
	require.NoError(t, err)
	require.NotContains(t, got, "myimage:v1")
}
```

Check the fake's constructor name in the file before relying on `NewFake()`.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/sandbox/ -run TestFake_SaveAndRemoveTemplate -v`

Expected: FAIL to compile, `f.SaveTemplate undefined`.

- [ ] **Step 3: Extend the interface**

In `internal/sandbox/backend.go`, beside `ListTemplates`:

```go
	// SaveTemplate snapshots a sandbox as a reusable template image. The daemon
	// refuses to snapshot a running sandbox, so the caller must stop it first.
	SaveTemplate(ctx context.Context, name, tag string) error

	// RemoveTemplate deletes one template image from this node's image store.
	RemoveTemplate(ctx context.Context, ref string) error
```

- [ ] **Step 4: Implement on the fake**

In `internal/sandbox/fake.go`, beside `ListTemplates`. Follow the file's existing
locking style:

```go
// SaveTemplate records the call and adds the tag to the advertised templates.
func (f *Fake) SaveTemplate(_ context.Context, name, tag string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.savedTemplates = append(f.savedTemplates, name+"=>"+tag)
	f.templates = append(f.templates, tag)
	return nil
}

// SavedTemplates returns the SaveTemplate calls in order (tests).
func (f *Fake) SavedTemplates() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.savedTemplates...)
}

// RemoveTemplate drops the ref from the advertised templates. Removing one that
// is not there is not an error, matching the daemon.
func (f *Fake) RemoveTemplate(_ context.Context, ref string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.templates[:0]
	for _, t := range f.templates {
		if t != ref {
			out = append(out, t)
		}
	}
	f.templates = out
	return nil
}
```

Add the `savedTemplates []string` field to the `Fake` struct.

- [ ] **Step 5: Implement on the SDK backend**

In `internal/sandbox/sdkbackend.go`, beside `ListTemplates`. Use the existing
`b.handle(ctx, name)` helper (line 141), which is how `Stop` gets its sandbox:

```go
// SaveTemplate snapshots the sandbox as a template image. The SDK shells out to
// `sbx template save NAME TAG`; the daemon refuses a running sandbox, and the
// CLI's prompt fails on a non-interactive stdin, so the caller stops it first.
func (b *SDKBackend) SaveTemplate(ctx context.Context, name, tag string) error {
	sb, err := b.handle(ctx, name)
	if err != nil {
		return err
	}
	return sb.SaveTemplate(ctx, tag)
}

// RemoveTemplate deletes a template image by ref (REST DELETE on the daemon).
func (b *SDKBackend) RemoveTemplate(ctx context.Context, ref string) error {
	return sdktemplate.Remove(ctx, b.cl, ref)
}
```

`sdktemplate` is already imported in this file for `List`.

- [ ] **Step 6: Run the tests**

```bash
go build ./...
go test ./internal/sandbox/
```

Expected: PASS. If another fake or stub in the repo implements `Backend`, the
compiler will name it; add the two methods there too rather than changing the
interface.

- [ ] **Step 7: Commit**

```bash
git add internal/sandbox/backend.go internal/sandbox/fake.go internal/sandbox/sdkbackend.go internal/sandbox/fake_test.go
git commit -m "feat(sandbox): backend can save and remove a template

SaveTemplate shells out through the SDK, which is why the sandbox has to
be stopped first. RemoveTemplate is a daemon REST call."
```

---

### Task 4: The SaveTemplate handler

**Files:**
- Modify: `internal/apiserver/sandboxservice.go` — beside `StopSandbox` (line 348)
- Test: `internal/apiserver/sandboxservice_test.go`

**Interfaces:**
- Consumes: `Backend.SaveTemplate` from Task 3, `sbxv1.SaveTemplateRequest` from
  Task 2.
- Produces: nothing later tasks call directly. The console (Task 7) uses the REST
  route.

- [ ] **Step 1: Write the failing tests**

Add to `internal/apiserver/sandboxservice_test.go`, matching how the file builds
a service over the fake backend:

```go
func TestSandboxService_SaveTemplateRequiresAStoppedSandbox(t *testing.T) {
	// A running sandbox is refused, and the backend is never called.
	_, err := svc.SaveTemplate(context.Background(), &sbxv1.SaveTemplateRequest{
		Id: rec.ID, Tag: "myimage:v1",
	})
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
	require.Empty(t, fake.SavedTemplates())
}

func TestSandboxService_SaveTemplateSavesAStoppedSandbox(t *testing.T) {
	// ... stop the sandbox first ...
	_, err := svc.SaveTemplate(context.Background(), &sbxv1.SaveTemplateRequest{
		Id: rec.ID, Tag: "myimage:v1",
	})
	require.NoError(t, err)
	require.Equal(t, []string{rec.BackendName + "=>myimage:v1"}, fake.SavedTemplates())
}

func TestSandboxService_SaveTemplateNeedsATag(t *testing.T) {
	_, err := svc.SaveTemplate(context.Background(), &sbxv1.SaveTemplateRequest{Id: rec.ID})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}
```

Note the second assertion: the backend is called with the **backend name**, not
the routing id. That is what every other backend call in this file uses.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/apiserver/ -run TestSandboxService_SaveTemplate -v`

Expected: FAIL, `Unimplemented` from the generated server, because Task 2 added
the RPC without a handler.

- [ ] **Step 3: Write the handler**

In `internal/apiserver/sandboxservice.go`, beside `StopSandbox`:

```go
// SaveTemplate snapshots a stopped sandbox as a reusable template image.
//
// The sandbox must already be stopped. The daemon refuses to snapshot a running
// one, and the CLI prompts, which fails on a non-interactive stdin — so a stop
// has to happen either way. The caller does it, visibly: stopping on their
// behalf would destroy a running sandbox for anyone who mistypes an id.
func (s *SandboxService) SaveTemplate(ctx context.Context, r *sbxv1.SaveTemplateRequest) (*sbxv1.Empty, error) {
	if r.GetTag() == "" {
		return nil, status.Error(codes.InvalidArgument, "tag is required")
	}
	rec, err := s.mgr.Get(ctx, r.GetId())
	if errors.Is(err, sandbox.ErrNotFound) {
		return nil, status.Error(codes.NotFound, "sandbox not found")
	}
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if rec.Status == "running" {
		return nil, status.Error(codes.FailedPrecondition, "stop the sandbox before saving it as a template")
	}
	if err := s.mgr.Backend().SaveTemplate(ctx, rec.BackendName, r.GetTag()); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &sbxv1.Empty{}, nil
}
```

Check how neighbouring handlers resolve a sandbox — several use
`s.mgr.Resolve(ctx, id)` to get the backend name. If `Resolve` is the house
style, use it and drop the manual `Get`, but keep the status check, which
`Resolve` does not do.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/apiserver/ -run TestSandboxService_SaveTemplate -v`

Expected: PASS.

- [ ] **Step 5: Verify and commit**

```bash
go build ./... && go vet ./... && go test ./internal/apiserver/
git add internal/apiserver/sandboxservice.go internal/apiserver/sandboxservice_test.go
git commit -m "feat(apiserver): save a stopped sandbox as a template

A running sandbox is refused with FailedPrecondition. The daemon cannot
snapshot one, and stopping it for the caller would lose a sandbox to a
mistyped id."
```

---

### Task 5: The RemoveTemplate handler

**Files:**
- Modify: `internal/apiserver/nodeservice.go` — beside `ListTemplates`
- Test: `internal/apiserver/nodeservice_test.go`

**Interfaces:**
- Consumes: `Backend.RemoveTemplate` from Task 3, `sbxv1.RemoveTemplateRequest`
  from Task 2.
- Produces: nothing.

`NodeService` reaches templates through the lister wired by
`SetTemplateLister` (`node.go:238`). Removal needs the backend itself, so add a
second optional hook rather than widening the lister.

- [ ] **Step 1: Write the failing test**

Add to `internal/apiserver/nodeservice_test.go`, modelled on the existing
`TestNodeService_ListTemplates` (line 73):

```go
func TestNodeService_RemoveTemplate(t *testing.T) {
	f := sandbox.NewFake()
	require.NoError(t, f.SaveTemplate(context.Background(), "sb-1", "myimage:v1"))

	s := NewNodeService("n1", "node-1", "test")
	s.SetTemplateLister(f.ListTemplateInfo)
	s.SetTemplateRemover(f.RemoveTemplate)

	resp, err := s.RemoveTemplate(context.Background(), &sbxv1.RemoveTemplateRequest{Ref: "myimage:v1"})
	require.NoError(t, err)
	for _, tm := range resp.Templates {
		require.NotEqual(t, "myimage:v1", tm.Repository+":"+tm.Tag)
	}
}

func TestNodeService_RemoveTemplateNeedsARef(t *testing.T) {
	s := NewNodeService("n1", "node-1", "test")
	_, err := s.RemoveTemplate(context.Background(), &sbxv1.RemoveTemplateRequest{})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestNodeService_RemoveTemplateWithoutABackend(t *testing.T) {
	s := NewNodeService("n1", "node-1", "test") // no remover wired
	_, err := s.RemoveTemplate(context.Background(), &sbxv1.RemoveTemplateRequest{Ref: "myimage:v1"})
	require.Equal(t, codes.Unavailable, status.Code(err))
}
```

The fake's `ListTemplateInfo` returns a canned list (`fake.go:262`), so the first
test's assertion is "the removed ref is not present", not an exact list match.
If that canned list makes the assertion vacuous, extend the fake to return its
real `templates` slice — that is a fair change and Task 3 already touched it.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/apiserver/ -run TestNodeService_RemoveTemplate -v`

Expected: FAIL to compile, `s.SetTemplateRemover undefined`.

- [ ] **Step 3: Write the hook and the handler**

In `internal/apiserver/nodeservice.go`, add the field beside the other optional
collaborators and the setter beside `SetTemplateLister`:

```go
// SetTemplateRemover wires template deletion (node.go). Optional: without it,
// RemoveTemplate answers Unavailable rather than pretending to delete.
func (s *NodeService) SetTemplateRemover(fn func(ctx context.Context, ref string) error) {
	s.removeTemplate = fn
}
```

And the handler beside `ListTemplates`:

```go
// RemoveTemplate deletes one template image from this node's image store and
// returns what is left. Cross-node calls arrive here already forwarded by
// node_id (ADR-0018), so this only ever removes locally.
func (s *NodeService) RemoveTemplate(ctx context.Context, r *sbxv1.RemoveTemplateRequest) (*sbxv1.ListTemplatesResponse, error) {
	if r.GetRef() == "" {
		return nil, status.Error(codes.InvalidArgument, "ref is required")
	}
	if s.removeTemplate == nil {
		return nil, status.Error(codes.Unavailable, "no sandbox backend on this node")
	}
	if err := s.removeTemplate(ctx, r.GetRef()); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return s.ListTemplates(ctx, &sbxv1.ListTemplatesRequest{})
}
```

- [ ] **Step 4: Wire it in the node**

In `internal/node/node.go`, beside `nodeSvc.SetTemplateLister(...)` (line 238):

```go
	nodeSvc.SetTemplateRemover(mgr.Backend().RemoveTemplate)
```

- [ ] **Step 5: Run the tests and the build**

```bash
go build ./... && go vet ./... && go test ./internal/apiserver/ ./internal/node/
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/apiserver/nodeservice.go internal/apiserver/nodeservice_test.go internal/node/node.go
git commit -m "feat(apiserver): delete a template from a node

RemoveTemplate returns the remaining templates, so a caller sees the
result without a second request. Cross-node calls are forwarded by
node_id before they reach the handler."
```

---

### Task 6: Templates stop being a boot-time fact

**Files:**
- Modify: `internal/membership/cluster.go` — beside `UpdateLocalLoad` (line 198)
- Modify: `internal/node/node.go` — the ten-second ticker (lines 119-146)
- Test: `internal/membership/cluster_test.go`

**Interfaces:**
- Produces: `func (c *Cluster) UpdateLocalTemplates(tmpls []string)`

This closes candidate item 1. Without it, a template saved by Task 4 is invisible
to every peer until the node restarts.

- [ ] **Step 1: Write the failing test**

Add to `internal/membership/cluster_test.go`, matching how its existing tests
build a `Cluster` without starting memberlist:

```go
func TestCluster_UpdateLocalTemplates(t *testing.T) {
	c := // ... build as the UpdateLocalLoad test does ...
	before := c.LocalNodeState().StateVersion

	c.UpdateLocalTemplates([]string{"myimage:v1"})

	ls := c.LocalNodeState()
	require.Equal(t, []string{"myimage:v1"}, ls.Templates)
	require.Greater(t, ls.StateVersion, before, "peers only re-read a bumped state")
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/membership/ -run TestCluster_UpdateLocalTemplates -v`

Expected: FAIL to compile, `c.UpdateLocalTemplates undefined`.

- [ ] **Step 3: Write it**

In `internal/membership/cluster.go`, beside `UpdateLocalLoad`:

```go
// UpdateLocalTemplates re-advertises the templates this node holds. Templates
// are a bulk gossip field, so this rides the push/pull rather than the meta:
// peers see a new template within about half a minute, which is fine for a
// placement input.
func (c *Cluster) UpdateLocalTemplates(tmpls []string) {
	c.mu.Lock()
	c.local.Templates = tmpls
	c.local.StateVersion++
	c.mu.Unlock()
}
```

Copy the locking shape from `UpdateLocalLoad` exactly, including whether it calls
`UpdateNode`. `UpdateLocalLoad` does not, because bulk fields do not ride the
meta — do the same here.

- [ ] **Step 4: Refresh on the ticker**

In `internal/node/node.go`, inside the existing ten-second ticker, in the block
that already runs when clustered (line 141):

```go
		if clusterInstance != nil {
			rc, rm, rd := mgr.Capacity().Snapshot()
			clusterInstance.UpdateLocalLoad(rc, rm, rd, au.CPU, au.Mem)
			// Re-advertise templates: they used to be a boot-time snapshot, so a
			// template saved at runtime was invisible to peers until a restart.
			// Polling also catches images added or removed outside our own RPCs.
			if tmpls, terr := mgr.Backend().ListTemplates(nctx); terr == nil {
				clusterInstance.UpdateLocalTemplates(tmpls)
			}
		}
```

A failed list is skipped silently, on purpose: it is one tick of a poller that
runs every ten seconds, and the previous value stays advertised.

**The ticker body itself has no unit test, and that is a real gap.** It is an
anonymous closure inside `node.New`, so a test cannot drive one tick without
booting a node and waiting ten seconds. Two honest options, in order of
preference:

1. Lift the clustered block into a small named function in `internal/node` —
   something like `refreshLocalState(cl, mgr, capt, statsC, nctx)` — and test
   that directly with a fake backend and a cluster. This is a few lines of
   extraction and makes the whole block testable, not only the template part.
2. If the surrounding state makes that extraction ugly, leave the closure alone
   and say so in a comment on `UpdateLocalTemplates`, noting that the wiring is
   covered by the two-node manual check in "Done when" and nothing else.

Pick one and state which in the commit message. Do not silently leave it untested
and unmentioned.

- [ ] **Step 5: Run the tests**

```bash
go build ./... && go vet ./... && go test ./internal/membership/ ./internal/node/
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/membership/cluster.go internal/membership/cluster_test.go internal/node/node.go
git commit -m "feat(membership): re-advertise templates while running

Templates were read once at boot, so a template saved at runtime stayed
invisible to peers until a restart. The existing ten-second ticker now
refreshes them, which also catches changes made outside our own RPCs."
```

---

### Task 7: The console can save a template

**Files:**
- Modify: `web/app/components/drawer/InfoTab.vue` — the Actions row (line 283)
- Test: `web/tests/drawer-info.spec.ts`

**Interfaces:**
- Consumes: `POST /v1/sandboxes/{id}/template` with `{ "tag": "..." }` from
  Task 2, and the `api` composable already injected in this component.

Save only. There is no delete control by decision — deleting a multi-gigabyte
image is rarer and more dangerous, and the API is enough.

- [ ] **Step 1: Write the failing test**

Add to `web/tests/drawer-info.spec.ts`, matching how that file mounts `InfoTab`
and stubs `api`:

```ts
it('disables save-as-template while the sandbox is running', async () => {
  const wrapper = mountInfoTab({ status: 'running' })
  expect(wrapper.get('[data-test="save-template"]').attributes('disabled')).toBeDefined()
})

it('posts the tag when saving a stopped sandbox', async () => {
  const post = vi.fn().mockResolvedValue({})
  const wrapper = mountInfoTab({ status: 'stopped' }, { post })
  await wrapper.get('[data-test="save-template"]').trigger('click')
  await wrapper.get('[data-test="save-template-tag"]').setValue('myimage:v1')
  await wrapper.get('[data-test="save-template-confirm"]').trigger('click')
  expect(post).toHaveBeenCalledWith('/v1/sandboxes/sb-1/template', { tag: 'myimage:v1' })
})
```

Use the file's own mount helper and stub names; the two above are illustrative of
shape, not of the helper's real name. Read the neighbouring delete-confirmation
test first — this control needs the same modal pattern.

- [ ] **Step 2: Run the tests to verify they fail**

From `web/`: `npm test -- drawer-info`

Expected: FAIL, no element matching `[data-test="save-template"]`.

- [ ] **Step 3: Add the control**

In the Actions row of `InfoTab.vue`, after the Stop button, a button that opens a
small modal asking for a tag. Mirror the existing delete-confirmation modal
(around line 337) rather than inventing a new pattern:

```vue
          <UButton
            data-test="save-template"
            label="Save as Template"
            icon="i-lucide-camera"
            size="sm"
            color="neutral"
            variant="outline"
            :loading="actionLoading === 'save-template'"
            :disabled="isRunning"
            @click="saveTemplateOpen = true"
          />
```

`:disabled="isRunning"` matches the server rule: the API refuses a running
sandbox with `FailedPrecondition`, so the button must not offer it. Add a hint on
the disabled state — "stop the sandbox first" — in the same style the file uses
for other gated controls.

The handler, beside `doAction`:

```ts
async function doSaveTemplate() {
  saveTemplateOpen.value = false
  actionLoading.value = 'save-template'
  try {
    await api.post(`/v1/sandboxes/${props.sandbox.id}/template`, { tag: templateTag.value })
    toast.add({ title: `Saved template ${templateTag.value}`, color: 'success', icon: 'i-lucide-check-circle' })
  } catch (e: any) {
    toast.add({ title: 'Failed to save template', description: e?.message, color: 'error' })
  } finally {
    actionLoading.value = null
  }
}
```

with `const saveTemplateOpen = ref(false)` and `const templateTag = ref('')`
beside the existing `deleteConfirmOpen`.

- [ ] **Step 4: Run the tests to verify they pass**

From `web/`: `npm test -- drawer-info`

Expected: PASS. Then run the whole console suite: `npm test`.

- [ ] **Step 5: Verify the embed still builds**

```bash
make web && go build ./...
```

Expected: no errors. The SPA is embedded, so a broken build here breaks the
binary.

- [ ] **Step 6: Commit**

```bash
git add web/app/components/drawer/InfoTab.vue web/tests/drawer-info.spec.ts web/dist
git commit -m "feat(web): save a stopped sandbox as a template

The button is disabled while the sandbox runs, matching the API, which
refuses a running one. No delete control: removing a multi-gigabyte image
stays an API call for now."
```

Check whether `web/dist` is tracked in this repo before adding it; if it is
generated at build time and ignored, drop it from the `git add`.

---

### Task 8: Prove it against a live daemon, and write it down

**Files:**
- Modify: `internal/sandbox/sdkbackend_integration_test.go`
- Modify: `CONTEXT.md` — the **Template** entry (line 172)
- Modify: `README.md` — beside the placement rules

**Interfaces:**
- Consumes: everything above.
- Produces: nothing.

- [ ] **Step 1: Write the integration test**

Add to `internal/sandbox/sdkbackend_integration_test.go`, which is behind
`//go:build integration`. The file's helpers are `backendWS(t)` (a backend plus a
writable workspace, which the daemon requires on create) and `mkSandbox(t, b,
spec)` (creates and schedules removal). Tests in this file are deliberately not
parallel.

```go
// TestSDKBackend_SaveRemoveTemplate proves the save/remove pair against a live
// daemon. The daemon refuses to snapshot a running sandbox, so this stops first
// — the same rule the SaveTemplate handler enforces.
func TestSDKBackend_SaveRemoveTemplate(t *testing.T) {
	ctx := context.Background()
	b, ws := backendWS(t)
	tag := "it-tmpl:v1"

	sb := mkSandbox(t, b, CreateSpec{Name: "it-save-tmpl", CPUs: 1, MemoryBytes: 1 << 30, Workspaces: ws})

	// A saved image outlives the sandbox, so remove it even if the test fails.
	t.Cleanup(func() { _ = b.RemoveTemplate(context.Background(), tag) })

	require.NoError(t, b.Stop(ctx, sb.Name), "the daemon cannot snapshot a running sandbox")
	require.NoError(t, b.SaveTemplate(ctx, sb.Name, tag))

	got, err := b.ListTemplates(ctx)
	require.NoError(t, err)
	require.Contains(t, got, tag, "a saved template must be listed by its repository:tag")

	require.NoError(t, b.RemoveTemplate(ctx, tag))
	got, err = b.ListTemplates(ctx)
	require.NoError(t, err)
	require.NotContains(t, got, tag)
}
```

One thing to check while running it, and to record in a comment either way: what
`ListTemplates` returns for the saved tag. `SDKBackend.ListTemplates` builds
`repository + ":" + tag` from the daemon's image list, and there is a standing
`ponytail:` note at `sdkbackend.go:723` saying that format is assumed and unproven
against a live daemon. This test is the first thing that proves or disproves it.
If the real format differs, fix the assertion **and** the note — do not fix the
test to match a bug.

- [ ] **Step 2: Run it**

```bash
go vet -tags integration ./internal/...
go test -tags integration ./internal/sandbox/ -run TestSDKBackend_SaveRemoveTemplate -v
```

Expected: PASS against the live daemon (`sbx` v0.37.0 on this host). If no daemon
is available, the env gate skips it — say so plainly rather than claiming it
passed.

- [ ] **Step 3: Update the glossary**

`CONTEXT.md`'s **Template** entry (line 172) currently ends "The swarm does not
move templates between nodes (v1)." Rewrite the entry so it says all of:

- A template is a reusable sandbox base image, saved on a node or pulled from a
  registry.
- A node advertises the templates it holds, and re-advertises while running.
- A registry-hosted reference is usable on every Node and is the supported way to
  share one; a bare tag exists only on the Node that saved it.
- Holding the image is a tie-break at placement, not a requirement, for a
  reference that travels.
- The swarm never moves image bytes between Nodes (ADR-0024).

Keep the file's shape: `**Term**:`, lines wrapped at about 110 characters, one
`_Avoid_:` line. It is a glossary — no implementation detail.

- [ ] **Step 4: Update the README**

Beside the placement rules, state the operator-facing rule in plain words: to use
one template across the swarm, push it to a registry and request it by its full
reference, for example `ghcr.io/org/img:1`. A bare tag like `myimage:v1` only
places on the node that holds it. Note the sharp edge: `org/img:1` is treated as
a bare tag, so write `docker.io/org/img:1` if you mean Docker Hub.

- [ ] **Step 5: Verify and commit**

```bash
go build ./... && go vet ./... && go vet -tags integration ./internal/...
go test ./...
```

Expected: PASS, Gerrit aside.

```bash
git add internal/sandbox/sdkbackend_integration_test.go CONTEXT.md README.md
git commit -m "test(sandbox): live save and remove, and document templates

- An env-gated integration test creates, stops, saves, lists and removes
  a template against a real daemon.
- CONTEXT.md says a registry reference is how a template is shared, and
  that the swarm never moves image bytes.
- README gives operators the rule, including that org/img:1 is treated as
  a bare tag."
```

---

## Done when

- [ ] `go build ./...`, `go vet ./...` and `go vet -tags integration ./internal/...` are clean
- [ ] `go test ./...` passes, Gerrit aside
- [ ] `npm test` in `web/` passes
- [ ] A registry-shaped template places on a node that does not hold it; a bare
      tag still does not
- [ ] Saving a running sandbox as a template is refused with `FailedPrecondition`
- [ ] A template saved on one node appears in another node's `GET /v1/nodes`
      within about a minute
- [ ] `git diff --stat origin/main` shows only the files in the File Structure
      table, plus `internal/gen/`
- [ ] Manual, two nodes: save a template on node A, confirm node B sees it, then
      request a registry-hosted template and confirm it places on a node that
      does not hold it

## Explicitly out of scope

Decided in the spec and ADR-0024. Do not add these, and do not treat them as
oversights:

- Moving image bytes between nodes, in any form, including `--output` tars.
- A console delete control, and a console template catalogue page.
- Template disk accounting.
- Treating a bare tag as pullable, or any config switch that would.
- `template.Load`, which only matters if bytes ever move.
