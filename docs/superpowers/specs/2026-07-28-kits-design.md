# Design: kit support

- **Date:** 2026-07-28
- **Targets:** `sbx` / `sandboxd` v0.37.0 (daemon API `0.24.0`), sbx-go-sdk v0.1.9
- **Companion decision:** [ADR 0022](../../adr/0022-kit-refs-are-node-config.md)
- **Vocabulary:** the **Kit** entry in `CONTEXT.md`

## Background

`sbx kit` is new in sbx v0.37.0. A kit is a declarative YAML artifact — `spec.yaml` plus an
optional `files/` directory — that contributes configuration to a sandbox: environment
variables, egress caps, install commands, and agent context. sbx-go-sdk v0.1.9 wraps the whole
command, so nothing in this design needs new SDK work.

A kit has two kinds. A `mixin` extends a sandbox. A `sandbox` kit supplies the base image
instead. Upstream marks the whole command EXPERIMENTAL: "this command may change or be removed
in future releases."

This design was reached by grilling a first draft. Where the final answer differs from the
obvious one, the reason is recorded, because the obvious answer looked fine.

## The hazard, and why it does not apply here

The daemon records a sandbox's kit list verbatim and re-resolves the **whole** list on every
later `kit add`, resolving a relative path against the **daemon's** working directory rather
than the caller's. The add reports success, so the damage stays invisible until an unrelated
later add fails with `re-resolve original kit 0 (...): path does not exist` — after which that
sandbox can take no kit at all. Verified upstream against sbx v0.37.0.

Three properties of this design remove the hazard rather than guard against it:

1. A kit reference never arrives in a request. It lives in node config (ADR 0022).
2. The node never calls `AddKit`, so there is no later add for the daemon to re-resolve.
3. The SDK makes a local reference absolute when it builds the argument vector, and only when
   the reference stats. An OCI reference passes through untouched.

## Decisions

| Question | Answer |
|---|---|
| Who supplies a kit reference? | Node config only. Never a request (ADR 0022). |
| Who chooses which kits a sandbox gets? | The caller, by **name**, per sandbox. |
| Which kinds are supported? | `mixin` only. |
| Is a kit added to a live sandbox? | No. `WithKit` at create; never `AddKit`. |
| Do `Pack` / `Push` / `Pull` belong here? | No. They are authoring operations. |
| Does the console get a kit catalog page? | No. A create-time multi-select is enough. |

## Config

```yaml
kits:
  - name: my-tools           # the name callers use
    ref: /opt/kits/my-tools  # local directory, ZIP, or OCI reference
```

`KitConfig{Name, Ref}`. `Validate` rejects an empty name, an empty reference, or a duplicate
name. Nothing else: the CLI is the authority on what a reference is.

## Admission at boot

The node runs one `kit.Inspect` per configured kit at boot. The inspections run concurrently under
one bounded context, so N slow references cost one timeout rather than N. They must finish before
boot continues: the gossiped `NodeState` is built once at boot and nothing re-gossips it, so an
advertisement assembled after that point would never reach the swarm.

There are two outcomes, and they are deliberately different:

- **`Inspect` succeeded and the manifest is bad** — `kind != "mixin"`, or `resources` or
  `runOptions` is non-empty → **rejected**. The kit is dropped from both the resolver and the
  advertisement, and the reason is logged at error level. Such a kit will never fix itself, so
  the swarm should not believe in it.
- **`Inspect` failed at all** — unreachable registry, missing path, or a typo → **advertised
  anyway**, logged at warn. If the kit really is broken, create fails on this node carrying the
  CLI's own message.

The node cannot tell a typo'd reference from a registry that is briefly down, so it treats both
as temporary. A typo therefore surfaces as a failed create rather than as a missing kit. This is
preferred over silently shrinking a node's advertised capacity because of a blip.

The gate is a pure function, `admit(kit.Info) error`, so it is table-testable with no daemon.
The caller decides what an `Inspect` *error* means.

The `resources` / `runOptions` check is the reason this gate exists at all. The SDK documents
those fields as meaningful only for `kind: sandbox` kits and empty for a mixin, which is an
upstream expectation and not a promise. If upstream ever honours them on a mixin, a kit could
hand a sandbox more CPU or memory than the node admitted — and "no over-admit" has been this
repo's line since M5. `resources` is a `json.RawMessage` and `runOptions` is a `[]string`, so the
check is a length test either way: no schema, no unmarshalling.

`backend: fake` has no SDK client, so it admits every configured kit unchecked. Kits are a
real-daemon feature; the fake exists for tests and daemonless nodes.

## Wire

- `repeated string kits = 16;` on `CreateSandboxRequest` — **names**, never references.
  `ProvisionRequest` wraps `CreateSandboxRequest`, so the cross-node provision path carries kits
  with no extra plumbing.
- `CreateSpec.Kits []string`. `Record.Spec` already persists the whole create spec, so storage,
  display, and re-provision replay come free — no new record field.
- `repeated string kits = 18;` on `NodeSummary`.
- `repeated string kits = 15;` on `Sandbox`, for display.
- `membership.NodeState.Kits []string` in the **bulk** half (ADR 0005). The value is static,
  set at boot, and carried by Join, so it needs none of the `UpdateNode` care that a
  runtime-mutable bulk field needs.

An unknown kit name never surfaces as `InvalidArgument` on the create call, because `CreateSandbox`
is async: it returns an `Operation`, and a placement failure lands in `op.Error`. Usually the
scheduler filters out every node that doesn't advertise the kit, so placement fails with
`ErrNoEligibleNode` before any node is even tried. If a candidate's advertisement is stale — see
"An unknown kit is a NACK, not a hard error" below — the failure instead surfaces as
`ErrNoCapacity` once every candidate has NACKed. An unknown workspace has none of this: it is
still rejected synchronously as `InvalidArgument`, and that comparison was never changed.

## Backend

A `KitResolver func(name string) (ref string, ok bool)` built from the admitted set in
`buildBackend`, beside the existing `workspaceResolver`. `SDKBackend.Create` gains one loop
appending `sdksandbox.WithKit(ref)`. The Fake records the names so tests can assert them.

## Placement

`Candidate.Kits map[string]bool`, plus a filter beside the template check in `scheduler.go`. A
node not advertising a requested kit is not a candidate. This mirrors `Workspaces` and
`Templates` exactly.

A kit is **not** modelled as a Capability. Three sibling gossip fields already exist for this
shape — `Workspaces`, `GitWorkspaces`, `Templates` — and `CONTEXT.md` defines a Capability as a
node feature flag. Riding on the existing capability filter as `kit:<name>` strings would have
saved about twenty lines at the cost of making one term mean two things, and the console would
then have to strip prefixes out of a column it also renders raw.

## Ports

After a create whose spec named kits, the node calls `Ports()` once and stores the result into
`Record.Ports`.

Without this the node reports two different answers. `ListPorts` reads the backend live, while
the `Sandbox` message's `ports` field is built from `rec.Ports`. Today nothing can publish a
port without the node recording it, so the two always agree; a kit declaring `publishedPorts`
would be the first thing to break that.

Two related visibility questions, for the record:

- A kit's `caps.network` is held by the daemon as policy, and `PolicyList` reads the daemon
  live, so it is visible.
- Whether a kit's `credentials` appear in `SecretList` is **unverified**. The integration test
  settles it rather than this design asserting it.

## Console

A kit multi-select in `ProvisionModal`, fed by the same distinct-across-all-nodes computation
the existing template and workspace selects use. Kits are shown on the sandbox drawer.

The union across nodes is the right source, not the connected node's own list: the scheduler
filters candidates anyway, so offering only the connected node's kits would hide kits the swarm
can honour.

## Adding a kit after boot

Edit config and restart the node. There is no config reload in the node, and a restart is safe —
reconcile-on-boot exists for exactly this.

This matches how templates, workspaces, and git workspaces already behave. `ListTemplates` is
read once, before the gossiped `NodeState` is built, and nothing re-gossips it; the self row
re-reads templates live for the console, but peers keep the boot-time list until the node
restarts.

If a runtime refresh is ever wanted, propagation is the easy part: `Kits` is a bulk field, and
memberlist's `DefaultLANConfig` push/pulls about every 30 seconds, so a change spreads on its
own. `pushPullPeers` only buys prompt propagation (ADR 0013). The hard part is that a config
edit needs a config reload, which the node does not have.

## Out of scope

- **`AddKit`.** It refuses any kit declaring `credentials`, `publishedPorts`, `volumes`,
  `commands.startup` or `commands.initFiles` — nearly everything worth adding. It recreates the
  container and auto-starts a stopped sandbox, which would flip status, `LastActivity`, and the
  idle reaper behind the node's back. High risk, little gain.
- **`Pack` / `Push` / `Pull`.** Authoring operations. They belong where the kit is written.
- **An Inspect or Validate RPC.** The configured name is enough for the console.
- **A kit catalog page.**
- **`kind: sandbox` kits.** A sandbox kit supplies the base image, which would make the
  scheduler's template constraint a lie.
- **Cross-node reference-equality checking.** A kit name is an operator promise, exactly as a
  Workspace name already is. Comparing references would prove a string matched, not that the
  content did: the same OCI tag can be re-pushed, and two nodes' copies of the same path can
  differ. A check that cannot prove the thing is worse than a documented promise.

## Risks

1. **Upstream is EXPERIMENTAL** and says the command may be removed. The exit is deleting
   config and one proto field — though a stored record naming a removed kit would then fail
   re-provision, the same way a removed workspace already does.
2. **`kit.allowedSources` is a per-node daemon setting** (on the development host,
   `["docker.io/"]`). A git or OCI reference forbidden on one node only makes that node fail
   creates, which reads like a flaky node rather than a settings gap. The boot warning is the
   place an operator finds out.
3. **A kit name can mean different things on different nodes.** Accepted, documented, and the
   same exposure Workspace names already carry.
4. **An unknown kit is a NACK, not a hard error.** Stale gossip is possible: a peer restarts with
   a kit dropped from its config after the entry node's candidate snapshot already listed it as
   an advertiser. `sandbox.ErrUnknownKit` is a sentinel, wrapped at both production sites —
   `attemptFor`'s local branch in `internal/node/node.go` and `InternalService.Provision` in
   `internal/apiserver/provision.go` — and both treat it as a NACK (`Accepted: false, Reason:
   "unknown kit"`), so the coordinator moves on to the next candidate instead of aborting
   placement. This follows the precedent M5 set converting the self-cordon and dial-failure
   cases to NACKs. This was a deliberate later change: unknown-*workspace* behaviour was left a
   hard error in the same pass, which is a separate call.

## Tests

Unit:

- Config validation: empty name, empty reference, duplicate name.
- `admit()` as a table test: mixin passes; `kind: sandbox` rejected; non-empty `resources`
  rejected; non-empty `runOptions` rejected.
- An `Inspect` failure leaves the kit advertised (the transient case).
- Resolver: a known name maps to its reference; an unknown name is an error.
- Create accepts a configured kit name and rejects an unknown one.
- `Record.Ports` holds a kit-published port after create, and is persisted.
- Scheduler: a node not advertising a requested kit is filtered out — copy
  `TestSchedule_CapabilityAndTemplateFilter`.
- `authz.go` needs no change: no new method, and `CreateSandbox` is already classified. The
  `TestAuthz_AllMethodsClassified` gate stays green.

Integration, `//go:build integration` and env-gated, against a live daemon:

- Create with a real mixin kit; assert `sb.Kits()` holds the absolute reference, and read the
  kit's environment variable from inside the sandbox — proof it was applied, not merely accepted.
- Assert a `kind: sandbox` kit is refused and never advertised, against real daemon output.

Two things the live test does **not** settle, and the plan says so rather than implying coverage:
the `credentials`-in-`SecretList` question needs a fixture declaring a real credential, and proving
the ports agreement live needs a kit declaring `publishedPorts`, which would fight over host ports.
The ports agreement is covered by unit tests instead.

## Two sibling items, declined

Both came from the same review of sbx-go-sdk v0.1.9 that produced this design. They are recorded
here so the reasoning is not lost and the questions are not reopened from scratch.

**`WithPublish` at create — do nothing.** There is no create-then-publish flow to make atomic.
`CreateSpec` carries no ports, and `PublishPort` is a separate RPC that works. Adopting
`WithPublish` would mean ports in the create request, the proto, the internal provision path,
the Fake, and the console, for no behaviour change. An interrupted create-then-publish is fixed
by retrying the publish, which already works.

**`secret.Import` / `SetToken` / `SetRegistry` and `skillstore.Import` — do nothing.** Two of
these read the **node host's** ambient state: `secret.Import` reads the node's own environment
into the daemon keychain, and `skillstore.Import` copies the operator's `~/.agents/skills`,
`~/.claude/skills` and siblings into a store mounted into sandboxes. Exposing either through the
node API would turn an API call into a harvest of the operator's credentials and files.

`SetRegistry` was the one angle worth checking, and it is closed: the node never pulls a
template. The scheduler only places a sandbox on a node that already holds the template image,
so no pull path exists and no registry credential is needed.

One small asymmetry stays, and is harmless: the node can list and delete `type: registry`
stored secrets but cannot create them. Revisit only if kits start arriving over OCI in a way
that needs registry auth, or if the node ever gains a template-pull path.

One note for whoever next touches secrets: the SDK states that `SetCustom` — the call the node
already uses — may share the non-interactive overwrite-prompt bug its siblings guard against,
and calls that unverified.
