# Custom Secret Value Update Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let the node change the value of an existing custom secret, which it cannot do today.

**Architecture:** The sbx daemon refuses a second write to the same `(scope, env)` unless the caller re-supplies the existing placeholder. `SDKBackend.SecretSet` never passes one, so every update fails. The fix reads the placeholder back through the existing `SecretList` call and passes it through. Reusing the placeholder is also what makes a rotation safe: the value the sandbox holds in its env var stays put and only the real secret behind the proxy changes.

**Tech Stack:** Go, sbx-go-sdk v0.1.9, `sbx` v0.37.0 daemon, buf/protoc codegen, testify.

**Spec:** `docs/superpowers/specs/2026-07-28-secret-value-update-design.md`

## Global Constraints

- Branch is `fix/secret-value-update`, already created off `main` @ `7909669`. Do not branch again.
- A secret **value** is write-only. Never store it, never log it, never put it in an error, never put it in an audit entry. A **placeholder** is not a secret — it is visible inside every sandbox and is safe to read, log, and return.
- The repo is gofmt-dirty but does not enforce gofmt. Format only the files you touch. Do not reformat anything else.
- Tests that need a real daemon live behind the `integration` build tag. There is no docker and no `sbx` in CI. Run them by hand: `go test -tags integration ./internal/sandbox/`.
- This host has a running `sbx` v0.37.0 daemon (api `0.24.0`), so the integration tests do run here.
- Editing a `.proto` file means running `buf generate` from the repo root, then `go build ./...`, then committing the regenerated `internal/gen/sbxswarm/v1/*` files (they are git-tracked). `buf generate` needs network access for the remote BSR plugins.
- Ignore gopls "undefined / redeclared / MissingLitField" warnings after codegen. Trust the `go` toolchain.
- `go build` does not compile tests. Run `go vet ./...` to catch test breakage.

## File Structure

| File | Responsibility | Task |
|---|---|---|
| `internal/sandbox/sdkbackend.go` | `SecretSet` — look the placeholder up, then write | 1 |
| `internal/sandbox/sdkbackend_integration_test.go` | Prove a second write lands and keeps the placeholder | 1 |
| `proto/sbxswarm/v1/policy.proto` | Say that `SetSecret` is keyed on `(scope, env)` and replaces the host | 2 |
| `internal/gen/sbxswarm/v1/policy.pb.go` | Regenerated comment, not hand-edited | 2 |
| `internal/apiserver/policyservice.go` | Same note on the Go handler | 2 |

---

### Task 1: SecretSet reuses the existing placeholder

**Files:**
- Modify: `internal/sandbox/sdkbackend.go:528-537`
- Test: `internal/sandbox/sdkbackend_integration_test.go:267-299`

**Interfaces:**
- Consumes: `(*SDKBackend).SecretList(ctx context.Context, scope string) (Secrets, error)` — already exists at `sdkbackend.go:561`. Returns `Secrets{Custom []CustomSecret, Stored []StoredSecret}`, already filtered to the exact scope. Each `CustomSecret` read back has `Host`, `Env` and `Placeholder` set and `Value` empty.
- Consumes: `CustomSecret{Host, Env, Placeholder, Value string}` — already exists at `internal/sandbox/backend.go:133`. No type change needed.
- Produces: no new exported names. `(*SDKBackend).SecretSet(ctx context.Context, scope string, s CustomSecret) error` keeps its signature and gains "create or replace" behaviour.

- [ ] **Step 1: Write the failing test**

Open `internal/sandbox/sdkbackend_integration_test.go`. Find this line inside `TestSDKBackend_SecretRoundTrip` (around line 285):

```go
	require.Empty(t, found.Value, "secret value must be masked on read")
```

Insert this block directly after it:

```go
	// Rotation: a second write to the same (scope, env) must land. The daemon
	// rejects it unless the existing placeholder is re-supplied, which SecretSet
	// looks up for us. The placeholder must NOT change — it is the value the
	// sandbox already holds in its env var, so changing it would break the
	// sandbox rather than rotate the secret.
	firstPlaceholder := found.Placeholder
	require.NotEmpty(t, firstPlaceholder, "daemon did not report a placeholder")

	require.NoError(t,
		b.SecretSet(ctx, scope, CustomSecret{Host: "api.example.com", Env: "API_TOKEN", Value: "rotated"}),
		"second write to the same env must succeed")

	after, err := b.SecretList(ctx, scope)
	require.NoError(t, err)
	var rotated *CustomSecret
	for i := range after.Custom {
		if after.Custom[i].Env == "API_TOKEN" {
			rotated = &after.Custom[i]
		}
	}
	require.NotNil(t, rotated, "secret not listed after rotation")
	require.Equal(t, firstPlaceholder, rotated.Placeholder, "rotation must reuse the placeholder")
	require.Empty(t, rotated.Value, "secret value must stay masked after rotation")
```

Leave the rest of the test alone. The global-list check and the closing
`b.SecretRemove(ctx, scope, "api.example.com")` still work, because a rotation
keeps the same host.

- [ ] **Step 2: Run the test and check it fails**

Run:

```bash
go test -tags integration ./internal/sandbox/ -run TestSDKBackend_SecretRoundTrip -v
```

Expected: FAIL on the `"second write to the same env must succeed"` assertion.
The daemon error text will look like:

```
custom secret env "API_TOKEN" already exists in scope "it-secret"
with placeholder "sbx-cs-..."
```

If it fails for any other reason, stop and work out why before going on. A pass
at this step means the test is not exercising the bug.

- [ ] **Step 3: Write the fix**

In `internal/sandbox/sdkbackend.go`, replace the whole `SecretSet` function:

```go
func (b *SDKBackend) SecretSet(ctx context.Context, scope string, s CustomSecret) error {
	// The daemon rejects a second write to the same (scope, env) unless the caller
	// re-supplies the existing placeholder, so an update needs a read first. Reusing
	// it is also what makes rotation safe: the sandbox env value stays put and only
	// the real secret behind the proxy changes.
	//
	// ponytail: on a read failure, fall through and let SetCustom report the real
	// error. That is today's behaviour, so no new failure mode.
	if cur, err := b.SecretList(ctx, scope); err == nil {
		for _, c := range cur.Custom {
			if c.Env == s.Env {
				s.Placeholder = c.Placeholder
				break
			}
		}
	}
	err := sdksecret.SetCustom(ctx, b.cl, scope, sdksecret.CustomSecret{
		Host:        s.Host,
		Env:         s.Env,
		Value:       s.Value, // passed to the CLI; never stored or logged here
		Placeholder: s.Placeholder,
	})
	// The underlying sbx CLI echoes the full "--value <key>" argv in its error;
	// scrub the raw value so it never reaches logs or the caller.
	return scrubSecretValue(err, s.Value)
}
```

Four things to notice, so you do not "tidy" them away:

- The lookup keys on `Env`, not `Host`. The daemon's uniqueness rule is per
  `(scope, env)`, and a write may legitimately change the host.
- The lookup is unconditional, on purpose. Do not add a "skip the read if
  `s.Placeholder` is already set" guard: no node caller sets that field on a
  write, the proto has no field for it, and you cannot tell a create from an
  update without reading first. This was ruled on before implementation.
- The `SecretList` error is swallowed on purpose. See the comment. Do not turn
  it into a returned error, and do not add logging for it. This was also ruled
  on before implementation.
- On a create the loop finds nothing, `s.Placeholder` stays `""`, and the SDK
  omits the `--placeholder` flag. That is the correct create path.

- [ ] **Step 4: Run the test and check it passes**

Run:

```bash
go test -tags integration ./internal/sandbox/ -run TestSDKBackend_SecretRoundTrip -v
```

Expected: PASS.

- [ ] **Step 5: Check nothing else broke**

Run:

```bash
gofmt -l internal/sandbox/sdkbackend.go internal/sandbox/sdkbackend_integration_test.go
go build ./... && go vet ./... && go test ./...
```

Expected: `gofmt -l` prints nothing. Build, vet and the normal test suite all
pass. The normal suite runs against the in-memory `Fake` backend and does not
touch this code path, so nothing there should change.

- [ ] **Step 6: Commit**

```bash
git add internal/sandbox/sdkbackend.go internal/sandbox/sdkbackend_integration_test.go
git commit -m "$(cat <<'EOF'
fix(secret): let a custom secret's value be updated

- The daemon needs the existing placeholder on a second write to the
  same scope and env. SecretSet never passed one, so every update
  failed and secret rotation did not work.
- SecretSet now reads the placeholder back through SecretList and
  passes it through.
- Reusing the placeholder keeps the sandbox env value unchanged, so
  only the real secret behind the proxy rotates.
- Extends the secret round-trip integration test with a second write.
EOF
)"
```

---

### Task 2: Write the create-or-replace rule into the API docs

**Files:**
- Modify: `proto/sbxswarm/v1/policy.proto:113-114`
- Modify: `internal/apiserver/policyservice.go:152-153`
- Regenerate: `internal/gen/sbxswarm/v1/policy.pb.go` (via `buf generate`; do not hand-edit)

**Interfaces:**
- Consumes: nothing from Task 1. This task only changes comments and the file they generate into.
- Produces: no code. No message field is added, removed or renumbered, so the wire format does not change.

Why this task exists: a reader of `SetSecret(scope, host, env, value)` could
reasonably guess that a second call **adds** a host. It does not. Verified
against the live daemon: writing `--host b.example.com` over an entry holding
`a.example.com` left only `b.example.com`. The entry is keyed on `(scope, env)`
and the host is replaced. The REST binding is already `PUT`, which agrees.

- [ ] **Step 1: Update the proto comment**

In `proto/sbxswarm/v1/policy.proto`, replace this comment:

```proto
// SetSecretRequest sets a custom secret. value is write-only: not stored, not
// returned, not logged.
```

with:

```proto
// SetSecretRequest creates or replaces a custom secret. The entry is keyed on
// (scope, env): a second call for the same env replaces that entry, including
// its host, rather than adding a host. The placeholder is preserved across a
// replace, so a running sandbox keeps the env value it already holds and only
// the real secret behind the proxy changes.
//
// value is write-only: not stored, not returned, not logged.
```

- [ ] **Step 2: Regenerate and build**

Run from the repo root:

```bash
buf generate && go build ./...
```

Expected: both succeed. `git status` should show only
`internal/gen/sbxswarm/v1/policy.pb.go` changed under `internal/gen/`.

If `buf generate` touches other files under `internal/gen/`, that is unrelated
codegen drift. Stop and ask before committing it.

- [ ] **Step 3: Check the comment landed in the generated Go**

Run:

```bash
grep -n "creates or replaces a custom secret" internal/gen/sbxswarm/v1/policy.pb.go
```

Expected: one match, above the `SetSecretRequest` struct.

- [ ] **Step 4: Update the Go handler comment**

In `internal/apiserver/policyservice.go`, replace this comment:

```go
// SetSecret stores a custom proxy-injected secret. The value is passed through
// to the backend only and is never stored, logged, or returned (spec §11).
```

with:

```go
// SetSecret creates or replaces a custom proxy-injected secret. The entry is
// keyed on (scope, env), so a second call for the same env replaces that entry
// including its host — it does not add a host. The backend reuses the existing
// placeholder on a replace, which is what makes a rotation invisible inside a
// running sandbox.
//
// The value is passed through to the backend only and is never stored, logged,
// or returned (spec §11).
```

- [ ] **Step 5: Check the build and tests still pass**

Run:

```bash
gofmt -l internal/apiserver/policyservice.go
go build ./... && go vet ./... && go test ./...
```

Expected: `gofmt -l` prints nothing, and build, vet and tests all pass. No
behaviour changed in this task, so any failure here means something in Step 2
went wrong.

- [ ] **Step 6: Commit**

```bash
git add proto/sbxswarm/v1/policy.proto internal/gen/sbxswarm/v1/policy.pb.go internal/apiserver/policyservice.go
git commit -m "$(cat <<'EOF'
docs(secret): state that SetSecret creates or replaces

- A second SetSecret for the same env replaces that entry, host and
  all. It does not add a host. Verified against sbx v0.37.0.
- The entry is keyed on scope and env, not on host.
- Says so in the proto comment and the Go handler comment, and
  regenerates policy.pb.go.
- No field or wire change.
EOF
)"
```

---

## Done when

- `go test -tags integration ./internal/sandbox/ -run TestSDKBackend_SecretRoundTrip` passes on a host with a live `sbx` daemon.
- `go build ./... && go vet ./... && go test ./...` all pass.
- The branch holds two commits on top of `main` @ `7909669`, plus the design doc commit `777002b`.

## Not in this plan

Called out so nobody adds them mid-flight. Each is a separate decision.

- No proto field for a caller-supplied placeholder. The lookup covers every caller the node has.
- No model of the daemon's collision rule in the `Fake` backend.
- No `findPlaceholder` helper with its own unit test. The risk here is the daemon contract, not a four-line loop.
- No audit change. A create and a replace both record `secret.set` with the host as the target.
- Nothing about the `SecretSet` keys-on-env versus `SecretRemove` keys-on-host asymmetry. Both work, and a caller reads the list before deleting, so it always holds the current host.
- Nothing from the spec's "Out of scope" section (sandbox SSH, disk enforcement, template re-advertisement over gossip).
