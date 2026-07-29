# Small Gaps Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close three small places where the node reports something untrue, or fails to report something it knows.

**Architecture:** Three independent changes with no shared code and no ordering
between them: a proto comment, a warn-log in the SDK backend's `Create`, and a
console confirmation dialog with a glossary entry. Nothing changes an API shape,
a stored record, or a wire format.

**Tech Stack:** Go 1.x, protobuf via `buf`, Nuxt 4 + `@nuxt/ui` v4 console, Vitest.

**Spec:** `docs/superpowers/specs/2026-07-29-small-gaps-design.md`

## Global Constraints

- Base is `main` @ `46c6286` or later. `feat/kits` (PR #11) **must** already be
  merged; this plan reads code that only exists after it.
- The repo is gofmt-dirty but does not enforce it. Format only the files you
  touch. Do not reformat neighbouring code.
- `go vet ./...` does **not** apply the `integration` build tag and cannot
  compile integration test files. Also run `go vet -tags integration ./internal/...`.
- Tests needing a real daemon live behind the `integration` build tag. Do not add
  any in this plan.
- `TestNode_Gerrit_Publish` is red unless the local Gerrit stack is running (see
  `dev/gerrit/README.md`). That failure is environmental, not yours.
- Console tests run from the `web/` directory, not the repo root.
- Plain, short English in comments and commit messages. One idea per sentence.
- Do not touch `internal/sandbox/manager.go:172-183`. That is the kits port-record
  fix (`675c846`) and it is deliberate.

## File Structure

| File | Task | Responsibility |
|---|---|---|
| `proto/sbxswarm/v1/sandbox.proto` | 1 | Say what `Sandbox.ports` really holds |
| `internal/gen/sbxswarm/v1/sandbox.pb.go` | 1 | Regenerated; comment only |
| `internal/sandbox/sdkbackend.go` | 2 | Warn when the daemon denied a mount |
| `web/app/components/drawer/SecretsTab.vue` | 3 | Confirm a destructive secret re-point |
| `web/tests/drawer-secrets.spec.ts` | 3 | Prove the confirm blocks, and rotation does not prompt |
| `CONTEXT.md` | 3 | Define **Custom secret**, name rotate and re-point |

---

### Task 1: Document what `Sandbox.ports` actually holds

**Why:** `Record.Ports` is written only by `manager.go` after a create whose spec
carried kits, and `PublishPort`/`UnpublishPort` never update it. So field 4 is a
create-time snapshot for kit sandboxes, empty for everything else, and stale
after any publish. Nothing in the contract says so.

**Files:**
- Modify: `proto/sbxswarm/v1/sandbox.proto:104`
- Modify (generated, do not hand-edit): `internal/gen/sbxswarm/v1/sandbox.pb.go`

**Interfaces:**
- Consumes: nothing.
- Produces: nothing. Comment only — no symbol, type, or behaviour changes.

- [ ] **Step 1: Read the current field**

Run: `sed -n '100,106p' proto/sbxswarm/v1/sandbox.proto`

Expected to contain exactly this line:

```proto
  repeated Port ports = 4;
```

If it does not, stop. The base is wrong or someone has already edited it.

- [ ] **Step 2: Add the comment**

Replace that one line in `proto/sbxswarm/v1/sandbox.proto` with:

```proto
  // Ports known when the sandbox was created. Set only for a create that used
  // kits, because a kit can publish ports the node never asked for; empty for
  // every other sandbox. PublishPort and UnpublishPort do not update it, so it
  // goes stale. ListPorts reads the backend live — use that when the answer
  // must be current.
  repeated Port ports = 4;
```

Leave the field number, type, and name untouched. Do not add `[deprecated = true]`.
The field carries real data for kit sandboxes.

- [ ] **Step 3: Regenerate**

Run from the repo root: `buf generate`

This needs network access; the plugins in `buf.gen.yaml` are remote.

- [ ] **Step 4: Confirm the diff is comment-only**

Run: `git diff --stat`

Expected: exactly two files, `proto/sbxswarm/v1/sandbox.proto` and
`internal/gen/sbxswarm/v1/sandbox.pb.go`.

Run: `git diff internal/gen/sbxswarm/v1/sandbox.pb.go`

Expected: added `//` comment lines only. If any non-comment line changed, stop —
the generator version drifted and that is a separate problem.

- [ ] **Step 5: Verify the build**

Run each and expect no output:

```bash
go build ./...
go vet ./...
go vet -tags integration ./internal/...
```

- [ ] **Step 6: Run the Go tests**

Run: `go test ./...`

Expected: PASS, except `TestNode_Gerrit_Publish` if the Gerrit stack is down.

- [ ] **Step 7: Commit**

```bash
git add proto/sbxswarm/v1/sandbox.proto internal/gen/sbxswarm/v1/sandbox.pb.go
git commit -m "docs(proto): say what Sandbox.ports really holds

The field is a create-time snapshot, set only when the create used kits,
and it is never updated by PublishPort or UnpublishPort. ListPorts is the
live source. Comment only; no behaviour change."
```

---

### Task 2: Warn when the daemon denied a workspace mount

**Why:** The daemon can refuse a workspace mount by policy and still start the
sandbox. The agent comes up looking healthy with its workspace missing, and
nothing says so.

**Files:**
- Modify: `internal/sandbox/sdkbackend.go:219-223` (inside `Create`)

**Interfaces:**
- Consumes: `(*Sandbox).MountPolicyDenied() bool` and `(*Sandbox).Name() string`
  from `github.com/squall-chua/sbx-go-sdk/sandbox`; `(*SDKBackend).logger() *slog.Logger`
  from `internal/sandbox/sdkbackend.go:67`.
- Produces: nothing. No new symbol.

**No test, deliberately.** Do not add one, and do not build a harness to make one
possible:

- `internal/sandbox/sdkbackend_test.go` covers only pure helpers
  (`TestLoggerNeverNil`, `TestScrubSecretValue`, `TestWorkspaceArg`,
  `TestDedupePorts`). There is no fake-daemon harness, so `Create` cannot be
  reached from a unit test.
- The integration test cannot force a denial. The daemon exposes no local mount
  policy control: not in `sbx settings list`, not a `sbx create` flag, and not
  part of `sbx policy`, which is network rules only.
- The change is one `if` that logs. There is no branch to get wrong.

- [ ] **Step 1: Read the insert point**

Run: `sed -n '215,225p' internal/sandbox/sdkbackend.go`

Expected to end with:

```go
	sb, err := sdksandbox.Create(ctx, b.cl, opts...)
	if err != nil {
		return BackendSandbox{}, err
	}
	return BackendSandbox{Name: sb.Name(), Status: sb.State()}, nil
}
```

If the `return BackendSandbox{...}` line is not directly after the error check,
stop and re-read the function before editing.

- [ ] **Step 2: Insert the warning**

In `internal/sandbox/sdkbackend.go`, between the closing brace of that error
check and the `return BackendSandbox{...}` line, insert:

```go
	// The daemon can refuse a workspace mount by policy and still start the
	// sandbox, so the agent comes up silently without its workspace. It reports
	// one bool for the whole sandbox, so we cannot name which mount was denied.
	if sb.MountPolicyDenied() {
		b.logger().Warn("daemon denied a workspace mount by policy; sandbox is running without it",
			"sandbox", sb.Name())
	}
```

The handle is already hydrated: the SDK's `Create` ends with
`return Get(ctx, c, d.name)` (`sandbox/lifecycle.go:62`), a REST fetch of
`SandboxInfo`, which carries `mount_policy_denied`. No extra daemon call is made.

Use `b.logger()`, never `b.log` directly. `b.log` can be nil and `.Warn` on a nil
`*slog.Logger` panics; `logger()` guards that and `TestLoggerNeverNil` covers it.

- [ ] **Step 3: Verify it compiles**

Run each and expect no output:

```bash
go build ./...
go vet ./...
go vet -tags integration ./internal/...
```

- [ ] **Step 4: Confirm nothing else moved**

Run: `git diff --stat`

Expected: one file, `internal/sandbox/sdkbackend.go`, with roughly 7 lines added
and 0 removed. If lines were removed, you reformatted neighbouring code — undo it.

- [ ] **Step 5: Run the Go tests**

Run: `go test ./...`

Expected: PASS, except `TestNode_Gerrit_Publish` if the Gerrit stack is down.

- [ ] **Step 6: Commit**

```bash
git add internal/sandbox/sdkbackend.go
git commit -m "feat(sandbox): warn when the daemon denied a workspace mount

A denied mount still starts the sandbox, so the agent runs without its
workspace and nothing says so. The daemon reports one bool, so the log
names the sandbox but not the mount."
```

---

### Task 3: Confirm before a secret re-point destroys a credential, and name the terms

**Why:** `SDKBackend.SecretSet` (`internal/sandbox/sdkbackend.go:634`) matches on
`c.Env == s.Env`. Host is not part of the key. That gives two very different
outcomes from one console action:

- **Rotate** — same env, same host. The placeholder is reused. The value inside
  the sandbox does not change; only the credential behind the proxy moves. Safe,
  and it is the intended path.
- **Re-point** — same env, different host. The old host's credential is destroyed
  and cannot be recovered, because values are write-only.

Today the only signal for a re-point is a warn-log the operator never sees. The
confirm fires **only** on the re-point. Prompting on rotation would put a dialog
on the common safe path, which teaches people to click through it.

**Files:**
- Modify: `web/app/components/drawer/SecretsTab.vue:46-47` (inside `doAdd`)
- Modify: `web/tests/drawer-secrets.spec.ts`
- Modify: `CONTEXT.md:110` (insert a new term after the `Workspace credential` block)

**Interfaces:**
- Consumes: the component's existing `secrets` ref, typed
  `Ref<{ custom: { host: string; env: string; placeholder?: string }[]; stored: ... }>`,
  and the existing `addHost` / `addEnv` refs. All already defined in the file.
- Produces: nothing exported.

- [ ] **Step 1: Write the two tests**

The first is the failing test that drives the change. The second passes already,
on purpose: it pins the behaviour you must **not** break, which is that a
rotation never prompts. Write both now so the guard cannot over-fire unnoticed.

Add both to the `describe('SecretsTab', ...)` block in
`web/tests/drawer-secrets.spec.ts`, after the existing two tests:

```ts
  it('confirms a re-point to a different host, and cancelling blocks the PUT', async () => {
    get.mockResolvedValueOnce({ custom: [{ host: 'old.example.com', env: 'API_KEY' }], stored: [] })
    const confirmFn = vi.fn(() => false)
    vi.stubGlobal('confirm', confirmFn)
    const w = await mountSuspended(SecretsTab, { props: { id: 'n1.s1' } })
    await w.find('[data-test="secret-host"]').setValue('new.example.com')
    await w.find('[data-test="secret-env"]').setValue('API_KEY')
    await w.find('[data-test="secret-value"]').setValue('s3cr3t')
    await w.find('[data-test="secret-add"]').trigger('click')
    expect(confirmFn).toHaveBeenCalled()
    expect(put).not.toHaveBeenCalled()
    vi.unstubAllGlobals()
  })

  it('does not prompt on a rotation (same env, same host)', async () => {
    get.mockResolvedValueOnce({ custom: [{ host: 'api.example.com', env: 'API_KEY' }], stored: [] })
    const confirmFn = vi.fn(() => false)
    vi.stubGlobal('confirm', confirmFn)
    const w = await mountSuspended(SecretsTab, { props: { id: 'n1.s1' } })
    await w.find('[data-test="secret-host"]').setValue('api.example.com')
    await w.find('[data-test="secret-env"]').setValue('API_KEY')
    await w.find('[data-test="secret-value"]').setValue('rotated')
    await w.find('[data-test="secret-add"]').trigger('click')
    expect(confirmFn).not.toHaveBeenCalled()
    expect(put).toHaveBeenCalled()
    vi.unstubAllGlobals()
  })
```

Two notes on why these are written this way:

- `get` is the module-level mock at line 8. `mockResolvedValueOnce` seeds the
  `onMounted(fetchSecrets)` call for that one mount, so the component starts with
  an existing custom secret.
- Use `vi.stubGlobal('confirm', ...)`, not `vi.spyOn(window, 'confirm')`. The
  component calls the bare global `confirm(...)`, matching the existing
  stored-secret delete at line 90, and `stubGlobal` intercepts that whether or not
  the test environment defines `window.confirm`.

- [ ] **Step 2: Run the tests and check which one fails**

Run from the `web/` directory:

```bash
npx vitest run tests/drawer-secrets.spec.ts
```

Expected: **3 passed, 1 failed.**

- The re-point test FAILS on `expect(put).not.toHaveBeenCalled()`. There is no
  guard yet, so the PUT goes out despite the cancel. This is the red test.
- The rotation test PASSES. Nothing prompts yet, so "does not prompt" is trivially
  true. It only earns its keep after Step 3, where it catches a guard that fires
  on the safe path.

If the re-point test passes here, stop. Either the guard already exists or the
mock is not seeding `secrets.custom`.

- [ ] **Step 3: Add the guard**

In `web/app/components/drawer/SecretsTab.vue`, `doAdd` currently starts:

```ts
async function doAdd() {
  if (!canAdd.value) return
  addLoading.value = true
```

Insert between the `canAdd` guard and `addLoading.value = true`:

```ts
  // Replace is keyed on env alone, so the same env under a different host
  // destroys the old host's credential, and values are write-only so it cannot
  // be recovered. Same host is a rotation: the placeholder is reused and the
  // value inside the sandbox does not change, so it needs no prompt.
  const env = addEnv.value.trim()
  const host = addHost.value.trim()
  const clash = secrets.value.custom.find(c => c.env === env && c.host !== host)
  if (clash && !confirm(
    `${env} is already set for ${clash.host}.\n\n`
    + `Saving it for ${host} destroys the ${clash.host} credential. It cannot be recovered.\n\n`
    + `Continue?`)) return
```

Leave the rest of `doAdd` alone. It already trims `addHost` and `addEnv` when it
builds the request body; do not refactor it to reuse the new consts.

- [ ] **Step 4: Run the tests to verify they pass**

Run from the `web/` directory:

```bash
npx vitest run tests/drawer-secrets.spec.ts
```

Expected: 4 passed.

- [ ] **Step 5: Run the whole console suite**

Run from the `web/` directory:

```bash
npx vitest run
```

Expected: all files pass. Nothing else mounts `SecretsTab`, so no other spec
should move.

- [ ] **Step 6: Add the glossary term**

`CONTEXT.md:110` ends the **Workspace credential** entry with:

```
_Avoid_: token (bare), secret (that is the Sandbox-injected kind), key
```

That points at a term the glossary never defines. Insert a new entry directly
after that line and its following blank line, before `**Publish**:`

```markdown
**Custom secret**:
The Sandbox-injected secret, keyed by host and env. The node never holds the value: the daemon issues a
placeholder that stands in for it inside the Sandbox, and the real credential lives behind the egress proxy.
Values are write-only — never stored, returned, or logged by the node. Two operations share one call and
behave very differently. _Rotate_ (same env, same host) reuses the placeholder, so the value inside the
Sandbox does not change and only the credential behind the proxy moves. _Re-point_ (same env, different
host) destroys the old host's credential, unrecoverably.
_Avoid_: credential (that is the Workspace kind), env var, token
```

Keep the file's existing shape: `**Term**:`, description lines wrapped at about
110 characters, then one `_Avoid_:` line. Do not add implementation detail;
`CONTEXT.md` is a glossary and nothing else.

- [ ] **Step 7: Confirm the diff is only what you meant**

Run: `git diff --stat`

Expected: exactly three files —
`web/app/components/drawer/SecretsTab.vue`, `web/tests/drawer-secrets.spec.ts`,
and `CONTEXT.md`. No `web/dist` files: do not rebuild the SPA in this task.

- [ ] **Step 8: Commit**

```bash
git add web/app/components/drawer/SecretsTab.vue web/tests/drawer-secrets.spec.ts CONTEXT.md
git commit -m "feat(web): confirm before a custom secret re-point

Replace is keyed on env alone, so saving an existing env under a new host
destroys the old host's credential and it cannot be recovered. Prompt only
for that case; a rotation on the same host stays silent.

Also define Custom secret in CONTEXT.md, which the Workspace credential
entry already pointed at but nothing defined."
```

---

## Done when

- [ ] `go build ./...`, `go vet ./...` and `go vet -tags integration ./internal/...` are all clean
- [ ] `go test ./...` passes, Gerrit aside
- [ ] `npx vitest run` from `web/` passes, including the two new cases
- [ ] `git diff --stat main` shows six files and no others
- [ ] `internal/sandbox/manager.go` is untouched

## Explicitly out of scope

These were decided in the spec. Do not add them, and do not treat them as
oversights:

- Deleting `Record.Ports` or deprecating proto field 4. Kits writes that field.
- Dropping the `len(spec.Kits) > 0` guard in `manager.go`. Only a kit publishes
  ports during create, so it would be a wasted daemon call on every other create.
- Updating `rec.Ports` from `PublishPort` / `UnpublishPort`.
- A test for the mount-denied warning, or a fake-daemon harness to enable one.
- Per-sandbox governance profile, mapping secret errors to `AlreadyExists`, the
  `SetSecret` read-then-write race, and the multi-host entry collapse. All four
  are closed as do-nothing with reasons in the spec.
- Surfacing daemon settings, and changing how `admitKits` handles a kit the
  daemon permanently refuses. Both closed as do-nothing in the spec on
  2026-07-29. Do not "improve" `internal/sandbox/kits.go` while working on
  Task 2; that file is not in this plan.
