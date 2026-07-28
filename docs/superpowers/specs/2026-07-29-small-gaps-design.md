# Design — close three small gaps, and close four items as do-nothing

Date: 2026-07-29
Base: `main` @ `1036807`
Probed against: `sbx` v0.37.0 (api `0.24.0`), sbx-go-sdk v0.1.9

## The problem

Three places where the node reports something that is not true, or fails to
report something that is. None of them is large. They share one theme: the node
is quiet where it should speak.

This design also closes four candidate items as **do nothing**. The reasoning is
written down here so a later session does not reopen them.

## Sequencing — build after `feat/kits` lands

**Do not start the code until `feat/kits` is merged.**

`feat/kits` is unmerged, 17 ahead and 19 behind `main`, and already has a known
conflict in `internal/sandbox/sdkbackend.go`. It touches five of the files this
batch needs:

| File | Needed by | Touched by `feat/kits` |
|---|---|---|
| `proto/sbxswarm/v1/sandbox.proto` | change 1 | yes |
| `internal/gen/sbxswarm/v1/sandbox.pb.go` | change 1 (regenerated) | yes |
| `internal/apiserver/sandboxservice.go` | change 1 | yes |
| `internal/sandbox/sdkbackend.go` | change 2 | yes, and it already conflicts here |
| `CONTEXT.md` | change 4 | yes |

`feat/kits` also inserts its kit loop directly above
`sb, err := sdksandbox.Create(...)`, about four lines from where change 2 goes.
That is close enough to create a second conflict in a file that already has one.
A regenerated `sandbox.pb.go` conflict is worse still, because it cannot be
resolved by hand in any pleasant way.

Only `internal/sandbox/record.go`,
`web/app/components/drawer/SecretsTab.vue` and
`web/tests/drawer-secrets.spec.ts` are clear of `feat/kits`.

## Change 1 — stop pretending the sandbox record holds ports

### What is wrong

`Record.Ports` is **never written**. Not once, anywhere in the repo.

It was born dead in `9dd123b` (2026-06-16). That commit added the field, added
the loop in `toProto` that reads it, and added `ListPorts` that reads the
backend live. Nothing was ever added to fill the record. So field 4 of the
`Sandbox` message is always an empty list.

`web/app/components/drawer/InfoTab.vue:51` seeds its port list from that field,
then immediately refetches from `ListPorts` on mount, so the console never
noticed.

### Why not fill it instead

Filling it would mean one daemon call per sandbox on every `ListSandboxes`.
`ListPorts` already reads the backend live and is the honest answer. The record
should not mirror live backend state.

### Why the proto field is deprecated, not removed

`internal/apiserver/server.go:92` sets `EmitUnpopulated: true`, so the REST JSON
emits `"ports": []` on every sandbox response today. Removing the field would
make that key vanish, not stay empty.

`README.md:272` records that the Agency is allowed to drift in version against
this node, so `/v1/` is a published contract with an outside reader, not only
the embedded console. Nothing can be *using* the values, because there are none,
but a client doing `res.ports.length` would go from `0` to a crash.

Deprecating costs one annotation, removes every line of dead Go, and leaves the
JSON byte-identical.

### The change

- `internal/sandbox/record.go` — delete the `Ports []PublishedPort` field.
- `internal/apiserver/sandboxservice.go` — in `toProto`, delete the `ports` loop
  and the `Ports: ports` field.
- `proto/sbxswarm/v1/sandbox.proto` — mark field 4 `[deprecated = true]` and add
  a comment: the field is always empty, and `ListPorts` is the live source.
  Regenerate.

`PublishedPort` stays. The backend interface still uses it.

Old store files carrying a `ports` key are ignored on load. The values there
were never meaningful.

### How to verify

- `go build ./... && go vet ./...`, plus `go vet -tags integration ./internal/...`
- `buf generate` produces no unexpected diff
- `go test ./...`
- The REST response for a sandbox still contains `"ports": []`

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

`web/app/components/drawer/SecretsTab.vue`, in `doAdd`: before posting, if
`secrets.custom` holds an entry with the same `env` under a **different** host,
ask for confirmation. Name the old host and say the old credential cannot be
recovered.

Rotation posts silently.

This uses the same native `confirm()` the stored-secret delete already uses at
line 90, so it adds no new pattern.

### Why not confirm on any existing env

That fires on rotation, which is the common and safe case. A dialog that appears
on the normal path is a dialog people learn to click through, which costs us the
one case we actually care about.

### How to verify

In `web/tests/drawer-secrets.spec.ts`:

- `window.confirm` stubbed to return false, adding an env that exists under a
  different host: no POST is made.
- Adding an env that exists under the same host: no prompt, and the POST goes
  out.

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

### Secret — a multi-host entry collapsing on an env-keyed replace

An entry covering several hosts collapses to one when replaced by env. The node
never creates such an entry, though `sbx` can.

## Corrections to earlier notes

Three premises carried into this session were wrong, and the probes are recorded
above:

1. `Sandbox.ports` and `ListPorts` do **not** agree today. The record side is
   permanently empty.
2. The node cannot set a node-wide default profile. It has no profile support of
   any kind.
3. `MountPolicyDenied` is cheaper than expected, because `Get` and `List`
   already fetch the struct that carries it.
