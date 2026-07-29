# Design — save a template, and let one travel

Date: 2026-07-29
Base: `main` @ `2ff3093` (local; `origin/main` is 6 commits ahead of the shared
ancestor, so re-probe after rebasing)
Probed against: `sbx` v0.37.0 (api `0.24.0`), sbx-go-sdk v0.1.9

This is the scoping result for candidate **item 7, template lifecycle**, and it
absorbs candidate **item 1, template re-gossip**, which cannot ship alone.

## The problem

The node can only *list* templates. `template.Load`, `template.Remove` and
`sandbox.SaveTemplate` are all unused, and the scheduler will only place a
sandbox on a node that already holds the requested image
(`internal/scheduler/scheduler.go:95`). Nothing in the node ever creates a
template, so `CONTEXT.md` is right that the swarm does not move them.

Item 1 sits underneath: templates are read once at boot (`node.go:191`) and never
re-advertised. It buys nothing on its own, and becomes required the moment a node
can create a template at runtime.

## What the probe changed

The handoff framed distribution as the hard part — bytes on the wire, who
initiates, what happens mid-transfer. One probe against the live daemon shrank
it:

```
$ sbx create -t ghcr.io/nonexistent-org-xyz/nope:1 --name probe-pull-xyz shell /tmp
ERROR: request failed: 403 Forbidden: pull failed for image "ghcr.io/nonexistent-org-xyz/nope:1"
```

**The daemon pulls a remote image reference.** No sandbox was created; the run
failed at the pull. `-t/--template` is documented as "Container image to use for
the sandbox", and templates *are* images — `template.List` is
`GET /docker/images`.

So a template held in a registry is already available on every node, with no
transfer code at all. What blocks it is our own scheduler, which refuses any node
that does not already hold the exact reference.

Two more facts worth recording, both contradicting the handoff's assumptions:

- `sbx template save SANDBOX TAG --output FILE` **does** export a tar, and
  `sbx template load FILE` imports it. The SDK's `sandbox.SaveTemplate` omits
  `--output`, but `client.Runner()` and `Capture` are exported, so a node could
  shell out itself with no SDK bump. That is the raw material for a byte-moving
  feature, if one is ever wanted.
- The only gRPC dialers in the repo are the loopback to self and the peer pool,
  and the only streaming RPC is `WatchEvents`. A multi-gigabyte transfer would
  need a path that does not exist yet.

## The decision

Build the two small features. Do not move bytes.

1. Teach placement that a registry-shaped reference travels.
2. Let an operator save a warmed sandbox as a template, and delete one.
3. Re-advertise templates, which item 1 asked for and these two now require.

## Change 1 — placement understands a reference that travels

### The rule

Add a helper that decides whether a reference names a registry, using Docker's
own rule: the reference has a `/`, and the first path component contains a `.` or
a `:`, or is exactly `localhost`.

- `ghcr.io/org/img:1` — registry. Any node can fetch it.
- `localhost:5000/img:1` — registry. Docker's rule treats `localhost` as a host.
- `myimage:v1` — bare. It exists only where it was saved.
- `org/img:1` — **treated as bare**, even though Docker would read it as a Docker
  Hub image. The shorthand is ambiguous with a locally saved two-part tag, so the
  rule errs toward refusing placement rather than assuming a pull. Write
  `docker.io/org/img:1` to make it travel. This belongs in the README.

In `fits`, the template constraint applies only to bare references:

```go
if req.Template != "" && !pullable(req.Template) && !c.Templates[req.Template] {
	return false
}
```

Two lines and a helper. No proto change, no new request field, and the caller
does not have to know anything the reference does not already say.

### Why not the alternatives

- **Drop the filter entirely.** One line deleted, but a typo in a local tag then
  places the sandbox somewhere and fails at create, instead of being refused up
  front with a clear reason.
- **An explicit `pull` flag on the create request.** A proto change, a console
  change, and a decision pushed onto the caller that the reference already
  encodes.

### The known weakness

It is a heuristic. A private registry reachable from only some nodes is assumed
reachable from all. The failure is visible and local — a create error on the
chosen node, naming the pull — not a silent bad placement. Accepted.

### The tie-break

A registry reference makes every node eligible, so an unloaded peer holding
nothing can beat the node that already has the image, and the sandbox waits for a
pull that was avoidable.

Add "already holds the image" as the **first** tie-break, ahead of the entry-node
preference (`scheduler.go:70-77`). `Candidate.Templates` is already there, so it
is one comparison in the existing chain. ADR-0007 fixed that ordering, so it
gains a line.

This only applies when scores tie exactly. A busier holder still loses on score
and the pull still happens elsewhere. That is the right trade: score reflects
real load, and a pull is a one-off cost.

## Change 2 — save a warmed sandbox as a template

### The RPC

`SaveTemplate(sandbox_id, tag)` on `SandboxService`. Taking the sandbox id means
the existing sandbox-id forwarding carries the call to the owning node with no
new routing code.

It **requires the sandbox to be stopped** and returns `FailedPrecondition`
otherwise. The daemon refuses to snapshot a running sandbox, and the CLI prompts,
which fails on a non-interactive stdin — so a stop must happen either way. The
caller does it, visibly.

Stopping on the caller's behalf was considered: it is about five more lines
reusing the graceful stop path, which also publishes git work on the way down.
Rejected because an API caller who mistypes an id would lose a running sandbox.
Stop-save-restart was rejected outright: the save can succeed and the restart
fail, leaving the sandbox down with nobody owning the problem.

Admin-gated, so it joins the authz map and its drift-guard test
(`TestAuthz_AllMethodsClassified`).

### The backend

The `Backend` interface gains `SaveTemplate(ctx, name, tag string) error`, over
the SDK's `sandbox.SaveTemplate`, plus a fake. Note what the SDK method is: a
shell-out to `sbx template save NAME TAG`, not a REST call.

## Change 3 — delete a template

`RemoveTemplate(node_id, ref)` on `NodeService`, routed the way `Cordon` already
is (ADR-0018), over the SDK's `template.Remove`. Admin-gated. The `Backend`
interface gains `RemoveTemplate(ctx, ref string) error`.

A save RPC with no delete RPC is a product that only fills disks: a golden image
is measured in gigabytes, and the only other way to remove one is a shell on the
host. Shipping the pair roughly doubles a small task rather than adding a new
kind of work.

## Change 4 — templates stop being a boot-time fact

Today `localNS.Templates` is built once (`node.go:191`) and never updated. Add to
the existing ten-second ticker (`node.go:119-146`), which already calls
`mgr.List` and `UpdateLocalLoad` when clustered:

- `mgr.Backend().ListTemplates(nctx)`
- `clusterInstance.UpdateLocalTemplates(tmpls)`, modelled exactly on
  `UpdateLocalLoad` (`cluster.go:198`) — set the field, bump `StateVersion`.

`Templates` is a **bulk** gossip field, so it rides the thirty-second push/pull
rather than the meta. Peers learn within about half a minute. That is fine for a
placement input; nothing depends on it being instant.

Polling also catches changes we did not make — an `sbx template rm` on the host,
or an image pulled by a create — which a notifier wired to our own RPCs would
miss. That is why this is a ticker refresh and not a `SetOwnedIDsNotifier`-style
hook.

This closes candidate item 1.

## Change 5 — the console

A save button on a **stopped** sandbox, asking for a tag. That is the moment the
feature exists for: someone warms a sandbox by hand and wants to keep it.

No delete control. Deleting a multi-gigabyte image is rarer and more dangerous,
and the API is enough until somebody asks.

## Change 6 — documentation

`CONTEXT.md`'s **Template** entry ends "The swarm does not move templates between
nodes (v1)." That stays true and is now incomplete. It must also say that a
registry-hosted reference is usable on every node, and that this is the supported
way to share one. Nodes advertise the templates they hold, and holding one is now
a tie-break rather than a hard requirement for a reference that travels.

`README.md` says the same thing for operators, next to the placement rules.

## ADR

One new ADR: **a template travels as a registry reference; the swarm does not
move image bytes.**

It meets all three bars. Surprising — the reasonable expectation of a swarm is
that it distributes what it schedules on. A real trade-off — the alternative,
exporting a tar and streaming it peer to peer, is available and was rejected for
size and for the transfer path it would need. Hard to reverse — the placement
rule now depends on the shape of the reference, and callers will write references
that assume it.

## Rejected alternatives

### Moving image bytes between nodes

The handoff's "much bigger" option, and still bigger than it looks even though
the export exists. It needs a streaming transfer path the node does not have,
disk headroom for a multi-gigabyte tar on both ends, a decision about who
initiates and what happens mid-transfer, and a story for a half-received image.
A registry solves the same problem with infrastructure that already exists and
that operators already run.

If it is ever wanted, the raw material is recorded above.

### A template catalogue page in the console

The workspaces catalogue exists and this would mirror it. Out of scope: nothing
here creates enough templates to need browsing, and `ListTemplates` already
backs the nodes view.

### Template disk accounting

Saved images consume disk that nothing measures. Real, and it belongs with
per-sandbox disk enforcement, which is blocked upstream. Not this item.

## How to verify

- A unit test on the registry-reference helper, covering `ghcr.io/org/img:1`,
  `localhost:5000/img:1`, `myimage:v1`, and a bare name with no tag.
- A scheduler test: a registry reference places on a node that holds nothing; a
  bare reference does not.
- A scheduler test: on a score tie, the holder wins over the entry node.
- A `SaveTemplate` test: a running sandbox is refused with `FailedPrecondition`;
  a stopped one calls the backend with the right name and tag.
- A `RemoveTemplate` test over the fake backend.
- A test that the ticker refresh reaches `localNS.Templates`.
- `go build ./...`, `go vet ./...`, `go vet -tags integration ./internal/...`,
  `go test ./...` green, `TestNode_Gerrit_Publish` aside.
- Integration, behind the `integration` tag and a live daemon: create a sandbox,
  stop it, save it as a template, confirm it appears in `ListTemplates`, then
  remove it and confirm it is gone.
- Manual, two nodes: save a template on node A and confirm node B sees it in
  `GET /v1/nodes` within about a minute. Then request a registry-hosted template
  and confirm it places on a node that does not hold it.

## Out of scope

- Moving image bytes between nodes.
- A console delete control, and a console template catalogue.
- Template disk accounting.
- Anything that would let a bare tag be treated as pullable.
