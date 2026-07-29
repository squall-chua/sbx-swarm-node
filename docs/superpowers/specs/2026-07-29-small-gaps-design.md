# Design — close three small gaps, and close five items as do-nothing

Date: 2026-07-29
Base: `main` @ `46c6286` (first drafted at `1036807`, revised after `feat/kits` merged)
Probed against: `sbx` v0.37.0 (api `0.24.0`), sbx-go-sdk v0.1.9

## The problem

Three places where the node reports something that is not true, or fails to
report something that is. None of them is large. They share one theme: the node
is quiet where it should speak.

This design also closes five candidate items as **do nothing**. The reasoning is
written down here so a later session does not reopen them.

## Sequencing — `feat/kits` has landed

This design was first drafted at `1036807`, when `feat/kits` was unmerged and
conflicted with five of the files here. That gate is gone: kits merged as PR #11
at `46c6286`.

The merge invalidated the original change 1. See the section below. Changes 2, 3
and 4 were re-checked against the merged tree and are unaffected, except that
the console `doAdd` now uses `api.put`, not POST.

## Change 1 — say what the record's ports actually are

### What changed under us

The first draft of this design proposed deleting `Record.Ports`, on the evidence
that it was never written anywhere. That was true at `1036807`. It is false now.

`feat/kits` commit `675c846`, *"fix(sandbox): record ports after a create that
used kits"*, added a write at `internal/sandbox/manager.go:172-183`: after a
create whose spec carries kits, the manager reads the backend's ports once and
stores them on the record. A kit can publish ports the node never asked for, so
without this the `Sandbox` message would show nothing for them.

That fix is correct and stays. Deleting the field would revert it.

### What is left wrong

The field is now populated inconsistently:

- **kit sandbox** — a create-time snapshot of the ports.
- **non-kit sandbox** — always empty, even after ports are published.
- **either kind** — `PublishPort` and `UnpublishPort`
  (`internal/apiserver/sandboxservice.go:511-531`) write only to the backend and
  never touch the record, so the snapshot goes stale.

A consumer cannot tell "this sandbox has no ports" from "we did not record any".

### Why not just drop the `len(spec.Kits) > 0` guard

Considered and rejected. Only a kit can publish a port *during* create. For
every other sandbox that read would return empty and be a wasted daemon call on
every create. The guard is right.

### Why not keep the record fresh instead

Updating `rec.Ports` inside `PublishPort` and `UnpublishPort` would deliver what
the kits comment set out to do. It was rejected for this batch: it puts a store
write into the port write path of two RPCs, which is more than a "small gap"
warrants, and `ListPorts` already answers the live question correctly.

Reopen it if a consumer ever needs the `Sandbox` message alone to be accurate.

### The change

`proto/sbxswarm/v1/sandbox.proto` — add a comment above field 4 stating that it
holds the ports known when the sandbox was created, that it is populated only
for a create that used kits, that it is not updated by `PublishPort` or
`UnpublishPort`, and that `ListPorts` is the live source.

No behaviour change. No Go change. Nothing is deprecated, because the field now
carries real data for kit sandboxes.

### How to verify

- `buf generate` produces only the comment change
- `go build ./... && go vet ./...`, plus `go vet -tags integration ./internal/...`
- `go test ./...` stays green

## Change 2 — warn when the daemon denied a workspace mount

### What is wrong

If the daemon refuses a workspace mount by policy, the sandbox still comes up.
It looks healthy. The workspace is simply not there, and nothing says so.

### The change

`internal/sandbox/sdkbackend.go`, in `Create`, immediately after the
`sdksandbox.Create` error check: if `sb.MountPolicyDenied()` is true, log a
warning through `b.logger()` naming the sandbox.

The message names the sandbox but **cannot name the workspace**. The daemon
returns a single bool, not a list.

### Why this is safe and free

The SDK's `Create` ends with `return Get(ctx, c, d.name)`
(`sandbox/lifecycle.go:62`), a REST fetch of `SandboxInfo`, and that struct
carries `mount_policy_denied`. The handle is already hydrated at the line where
we read it. No extra daemon call.

`Create` is the only place this belongs. Mount denial is a create-time fact that
never changes, so checking it on every `Get` would only produce noise.

### Why there is no test

This is deliberate, not an oversight.

- `internal/sandbox/sdkbackend_test.go` covers only pure helpers
  (`TestLoggerNeverNil`, `TestScrubSecretValue`, `TestWorkspaceArg`,
  `TestDedupePorts`). There is no fake-daemon harness, so `SDKBackend.Create`
  cannot be reached from a unit test.
- The env-gated integration test cannot force a denial. The daemon exposes no
  local control for mount policy: it is not in `sbx settings list`, not a
  `sbx create` flag, and not part of `sbx policy`, which is network rules only.
- The change is a single `if` that logs. There is no branch to get wrong.

Building a fake-daemon harness for one log line would cost far more than the
thing it tests. If such a harness ever appears for another reason, add the test
then.

### A known limit

Because there is no local mount policy control, this warning most likely only
ever fires under a remote governance policy, which we do not run. It is cheap
insurance that costs nothing when it never fires.

## Change 3 — confirm before a custom secret re-point destroys a credential

### What is wrong

Adding a custom secret in the console for an env that already exists is
destructive, and the only signal today is a warn-log the operator never sees.

### The distinction that matters

`SDKBackend.SecretSet` (`internal/sandbox/sdkbackend.go:556-569`) matches on
`c.Env == s.Env`. Host is not part of the key. That produces two very different
outcomes:

- **Rotate** — same env, same host. The existing placeholder is reused. The
  value inside the sandbox does not change; only the real credential behind the
  proxy moves. This is safe and it is the intended path.
- **Re-point** — same env, different host. The old host's credential is
  destroyed and cannot be recovered, because values are write-only. There is
  already a `logger().Warn` for exactly this at line 563.

### The change

`web/app/components/drawer/SecretsTab.vue`, in `doAdd`: before the `api.put`, if
`secrets.custom` holds an entry with the same `env` under a **different** host,
ask for confirmation. Name the old host and say the old credential cannot be
recovered.

Rotation goes through silently.

This uses the same native `confirm()` the stored-secret delete already uses at
line 90, so it adds no new pattern.

### Why not confirm on any existing env

That fires on rotation, which is the common and safe case. A dialog that appears
on the normal path is a dialog people learn to click through, which costs us the
one case we actually care about.

### How to verify

In `web/tests/drawer-secrets.spec.ts`:

- `window.confirm` stubbed to return false, adding an env that exists under a
  different host: no `api.put` is made.
- Adding an env that exists under the same host: no prompt, and the `api.put`
  goes out.

## Change 4 — define the term the glossary already points at

`CONTEXT.md:110` defines **Workspace credential** and steers the reader away
with `_Avoid_: ... secret (that is the Sandbox-injected kind)`. It points at a
term the glossary never defines.

Add a **Custom secret** entry: the sandbox-injected secret, keyed by host and
env, with a daemon-issued placeholder standing in for the value inside the
sandbox. The value itself is write-only and is never stored or returned by the
node.

Name both operations in that entry, because the difference between them is the
whole reason change 3 exists:

- **Rotate** — same env and host, placeholder preserved, sandbox-visible value
  unchanged.
- **Re-point** — same env, different host. The old host's credential is
  destroyed and unrecoverable.

Today that distinction lives only in a code comment at `sdkbackend.go:559-564`.

### Why no ADR

Nothing here is hard to reverse, which is the bar. The proto comment and this
spec are enough. The env-only replace key is the daemon's rule, not our
decision, so an ADR would only be recording someone else's choice.

## Closed as do nothing

### Item 10 — per-sandbox governance profile

There are no profiles to select. `sbx policy profile ls` returns
"No policy profiles found", and `sbx policy profile --help` says profiles are
"provided by remote governance policies", which we do not run.

The node has no profile support at all today. `internal/config/config.go` has no
policy fields, and `PolicyProfiles` on the `Backend` interface has zero callers,
so it is dead as well. It is left in place; deleting it was considered and
deliberately not taken on in this batch.

Reopen if a remote governance policy is ever put in front of these nodes.

### Secret — `AlreadyExists` instead of a 500

`internal/apiserver/policyservice.go` flattens backend errors to
`codes.Internal`. This cannot be fixed the way it was first described.

Secret and policy **mutations** are shell-outs (`r.Capture`), not REST. There is
no HTTP 409 and no gRPC status underneath, so a `status.FromError` passthrough
would match nothing. The only signal is the SDK's own error text, which reads
`"...already exists in this scope..."`. String-matching that to choose a status
code is more fragile than the 500 we have, and the message already reaches the
caller inside the 500.

Reopen if these paths ever move from shell-out to REST.

### Secret — the `SetSecret` read-then-write race

Two concurrent `SetSecret` calls for the same env can both read the same
placeholder, both write, and lose one update. Accepted: these writes are
admin-driven and rare.

### Item 9 — daemon settings

`settings.List/Get/Set/Unset` are unused by the node. Scoped on 2026-07-29 and
closed, because its stated justification does not survive a probe.

The argument was that the kits work must reason about `kit.allowedSources` but
cannot see it. Kits can see it, in the clearest form there is, at the moment it
matters. Probed on the exact code path the SDK uses:

- `sbx kit inspect --json <forbidden-ref>` exits **1** and writes the whole
  explanation to **stderr**. It names `kit.allowedSources`, prints the current
  allowlist, and gives the exact `sbx settings set` command that fixes it.
- `kit.Inspect` (SDK `kit/kit.go:87-90`) returns that `*CLIError` unwrapped.
- `CLIError.Error()` (SDK `internal/cli/cli.go:34`) formats `Stderr` into the
  message.
- `internal/sandbox/kits.go:101` already logs it as `"err", err`.

So the boot warning already carries the daemon's full diagnosis. Reading the
setting ourselves would duplicate it, less well.

What remains is per-node comparison: settings are per-node, and a swarm operator
cannot shell into every node to compare them. That is a real but thin case, and
nothing concrete is asking for it. Roughly 15 files and ~250 lines by this
repo's own calibration.

Writing daemon settings through the node API is a separate trust decision and
was not scoped.

Reopen when someone actually needs to compare settings across nodes.

### A related finding, also closed

`admitKits` (`internal/sandbox/kits.go:78-113`) cannot tell a *transient* inspect
failure (timeout, network blip) from a *permanent* refusal (the allowlist). It
advertises the kit either way. A permanently forbidden kit is therefore
advertised for the life of the process, and every create routed to it fails.

Left alone on purpose. The transient case is why "advertise anyway" was chosen,
and there is no clean signal separating the two: `client.ErrKitRejected` is
produced only by `Validate`, never by `Inspect`, so telling them apart means
matching on the daemon's prose. The boot warning is loud and fully diagnostic,
which is a better trade than a brittle string match.

### Secret — a multi-host entry collapsing on an env-keyed replace

An entry covering several hosts collapses to one when replaced by env. The node
never creates such an entry, though `sbx` can.

## Corrections to earlier notes

Three premises carried into this session were wrong, and the probes are recorded
above:

1. `Sandbox.ports` and `ListPorts` do **not** agree. At `1036807` the record
   side was permanently empty; since `675c846` it holds a create-time snapshot
   for kit sandboxes only, and goes stale after a publish either way.
2. The node cannot set a node-wide default profile. It has no profile support of
   any kind.
3. `MountPolicyDenied` is cheaper than expected, because `Get` and `List`
   already fetch the struct that carries it.

A fourth, from this session's own first draft: a premise verified against `main`
expires the moment another branch lands. Re-probe after every merge, not once.
